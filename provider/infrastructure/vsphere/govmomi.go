package vsphere

import (
	"context"
	"errors"
	"net"
	"net/url"
	"strings"
	"time"

	"github.com/vmware/govmomi/find"
	"github.com/vmware/govmomi/object"
	"github.com/vmware/govmomi/property"
	"github.com/vmware/govmomi/session"
	"github.com/vmware/govmomi/view"
	"github.com/vmware/govmomi/vim25"
	"github.com/vmware/govmomi/vim25/mo"
	"github.com/vmware/govmomi/vim25/soap"
	"github.com/vmware/govmomi/vim25/types"
)

// ipWaitTimeout bounds the wait for the guest to report an IPv4 address.
const ipWaitTimeout = 10 * time.Minute

// ipPoll is how often the guest address is polled.
const ipPoll = 5 * time.Second

// govmomiClient is the vCenter-backed Client, talking to vCenter with the
// official Go SDK github.com/vmware/govmomi.
type govmomiClient struct {
	vim *vim25.Client
	cfg Config
}

// NewClient opens a govmomi session to the vCenter named by cfg and returns a
// Client. It is called lazily by the lazy client on the first API use.
func NewClient(cfg Config) (Client, error) {
	if cfg.Server == "" {
		return nil, errorf("no vcenter server: set vsphere.server or the %s variable", VarServer)
	}
	u, err := soap.ParseURL(cfg.Server)
	if err != nil {
		return nil, errorf("parsing vcenter server %q: %w", cfg.Server, err)
	}
	if u == nil {
		return nil, errorf("vcenter server %q resolved to no URL", cfg.Server)
	}
	u.User = url.UserPassword(cfg.Username, cfg.Password)

	ctx := context.Background()
	soapClient := soap.NewClient(u, cfg.Insecure)
	if cfg.Thumbprint != "" {
		soapClient.SetThumbprint(u.Host, cfg.Thumbprint)
	}
	vimClient, err := vim25.NewClient(ctx, soapClient)
	if err != nil {
		return nil, errorf("connecting to vcenter %q: %w", u.Host, err)
	}
	if err := session.NewManager(vimClient).Login(ctx, u.User); err != nil {
		return nil, errorf("vcenter login to %q: %w", u.Host, err)
	}
	return &govmomiClient{vim: vimClient, cfg: cfg}, nil
}

// finderDC returns a finder scoped to the configured datacenter (or the only
// datacenter when none is configured) and that datacenter.
func (c *govmomiClient) finderDC(ctx context.Context) (*find.Finder, *object.Datacenter, error) {
	f := find.NewFinder(c.vim, true)
	dc, err := f.DatacenterOrDefault(ctx, c.cfg.Datacenter)
	if err != nil {
		return nil, nil, errorf("finding datacenter %q: %w", c.cfg.Datacenter, err)
	}
	f.SetDatacenter(dc)
	return f, dc, nil
}

// properties reads the named properties of a VM into dst.
func (c *govmomiClient) properties(ctx context.Context, vm *object.VirtualMachine, props []string, dst *mo.VirtualMachine) error {
	return property.DefaultCollector(c.vim).RetrieveOne(ctx, vm.Reference(), props, dst)
}

// vmView builds the provider's VM view from a VM's properties.
func vmView(name string, mvm *mo.VirtualMachine) *VM {
	v := &VM{Name: name}
	if mvm.Config != nil {
		v.UUID = mvm.Config.Uuid
		v.Annotation = mvm.Config.Annotation
	}
	if mvm.Summary.Config.Annotation != "" && v.Annotation == "" {
		v.Annotation = mvm.Summary.Config.Annotation
	}
	v.PoweredOn = mvm.Runtime.PowerState == types.VirtualMachinePowerStatePoweredOn
	for _, ip := range guestIPv4s(mvm) {
		v.Addresses = append(v.Addresses, ip)
	}
	return v
}

// guestIPv4s returns the guest's IPv4 addresses from VMware Tools.
func guestIPv4s(mvm *mo.VirtualMachine) []string {
	var out []string
	if mvm.Guest == nil {
		return out
	}
	for _, nic := range mvm.Guest.Net {
		for _, ip := range nic.IpAddress {
			if p := net.ParseIP(ip); p != nil && p.To4() != nil && !p.IsLoopback() && !p.IsLinkLocalUnicast() {
				out = append(out, ip)
			}
		}
	}
	return out
}

func (c *govmomiClient) FindVM(ctx context.Context, name string) (*VM, error) {
	f, _, err := c.finderDC(ctx)
	if err != nil {
		return nil, err
	}
	vm, err := f.VirtualMachine(ctx, name)
	if err != nil {
		var nf *find.NotFoundError
		if errors.As(err, &nf) {
			return nil, ErrVMNotFound
		}
		return nil, errorf("finding VM %q: %w", name, err)
	}
	var mvm mo.VirtualMachine
	if err := c.properties(ctx, vm, []string{"config", "summary", "runtime", "guest"}, &mvm); err != nil {
		return nil, errorf("reading VM %q: %w", name, err)
	}
	return vmView(name, &mvm), nil
}

func (c *govmomiClient) CloneVM(ctx context.Context, in CloneInput) (*VM, error) {
	f := find.NewFinder(c.vim, true)
	dcName := in.Datacenter
	if dcName == "" {
		dcName = c.cfg.Datacenter
	}
	dc, err := f.DatacenterOrDefault(ctx, dcName)
	if err != nil {
		return nil, errorf("finding datacenter %q: %w", dcName, err)
	}
	f.SetDatacenter(dc)

	tmpl, err := f.VirtualMachine(ctx, in.Template)
	if err != nil {
		return nil, errorf("finding template %q: %w", in.Template, err)
	}

	folder, err := c.cloneFolder(ctx, f, dc, in.Folder)
	if err != nil {
		return nil, err
	}
	pool, err := c.clonePool(ctx, f, in.ResourcePool)
	if err != nil {
		return nil, err
	}
	poolRef := pool.Reference()
	relocate := types.VirtualMachineRelocateSpec{Pool: &poolRef}
	if in.Datastore != "" {
		ds, err := f.Datastore(ctx, in.Datastore)
		if err != nil {
			return nil, errorf("finding datastore %q: %w", in.Datastore, err)
		}
		dsRef := ds.Reference()
		relocate.Datastore = &dsRef
	}

	cloneSpec := types.VirtualMachineCloneSpec{
		Location: relocate,
		PowerOn:  false,
		Template: false,
		Config: &types.VirtualMachineConfigSpec{
			NumCPUs:    in.NumCPUs,
			MemoryMB:   in.MemoryMiB,
			Annotation: in.Annotation,
		},
	}
	if in.CloneMode == CloneModeLinked {
		relocate.DiskMoveType = string(types.VirtualMachineRelocateDiskMoveOptionsMoveAllDiskBackingsAndAllowSharing)
		cloneSpec.Location = relocate
		if snap, err := templateSnapshotRef(ctx, c, tmpl); err == nil && snap != nil {
			cloneSpec.Snapshot = snap
		}
	}
	if in.Network != "" {
		change, err := c.networkDeviceChange(ctx, f, tmpl, in.Network)
		if err != nil {
			return nil, err
		}
		if change != nil {
			cloneSpec.Config.DeviceChange = append(cloneSpec.Config.DeviceChange, change)
		}
	}

	task, err := tmpl.Clone(ctx, folder, in.Name, cloneSpec)
	if err != nil {
		return nil, errorf("cloning template %q into %q: %w", in.Template, in.Name, err)
	}
	info, err := task.WaitForResult(ctx, nil)
	if err != nil {
		return nil, errorf("waiting for clone of %q: %w", in.Name, err)
	}
	ref, ok := info.Result.(types.ManagedObjectReference)
	if !ok {
		return nil, errorf("clone of %q returned no VM reference", in.Name)
	}
	vm := object.NewVirtualMachine(c.vim, ref)

	if in.DiskGiB > 0 {
		if err := c.growDisk(ctx, vm, int64(in.DiskGiB)<<30); err != nil {
			return nil, err
		}
	}

	var mvm mo.VirtualMachine
	if err := c.properties(ctx, vm, []string{"config", "summary", "runtime", "guest"}, &mvm); err != nil {
		return nil, errorf("reading cloned VM %q: %w", in.Name, err)
	}
	return vmView(in.Name, &mvm), nil
}

// cloneFolder resolves the VM folder the clone is created in.
func (c *govmomiClient) cloneFolder(ctx context.Context, f *find.Finder, dc *object.Datacenter, name string) (*object.Folder, error) {
	if name != "" {
		folder, err := f.Folder(ctx, name)
		if err != nil {
			return nil, errorf("finding folder %q: %w", name, err)
		}
		return folder, nil
	}
	folders, err := dc.Folders(ctx)
	if err != nil {
		return nil, errorf("resolving datacenter folders: %w", err)
	}
	return folders.VmFolder, nil
}

// clonePool resolves the resource pool the clone runs in.
func (c *govmomiClient) clonePool(ctx context.Context, f *find.Finder, name string) (*object.ResourcePool, error) {
	if name != "" {
		pool, err := f.ResourcePool(ctx, name)
		if err != nil {
			return nil, errorf("finding resource pool %q: %w", name, err)
		}
		return pool, nil
	}
	pool, err := f.DefaultResourcePool(ctx)
	if err != nil {
		return nil, errorf("resolving default resource pool: %w", err)
	}
	return pool, nil
}

// networkDeviceChange builds the device edit that re-points the template's
// first ethernet NIC at network. It returns nil when the template has no NIC.
func (c *govmomiClient) networkDeviceChange(ctx context.Context, f *find.Finder, tmpl *object.VirtualMachine, network string) (types.BaseVirtualDeviceConfigSpec, error) {
	netRef, err := f.Network(ctx, network)
	if err != nil {
		return nil, errorf("finding network %q: %w", network, err)
	}
	backing, err := netRef.EthernetCardBackingInfo(ctx)
	if err != nil {
		return nil, errorf("resolving network backing for %q: %w", network, err)
	}
	devices, err := tmpl.Device(ctx)
	if err != nil {
		return nil, errorf("reading template devices: %w", err)
	}
	nics := devices.SelectByType((*types.VirtualEthernetCard)(nil))
	if len(nics) == 0 {
		return nil, nil
	}
	nic := nics[0]
	nic.GetVirtualDevice().Backing = backing
	return &types.VirtualDeviceConfigSpec{
		Operation: types.VirtualDeviceConfigSpecOperationEdit,
		Device:    nic,
	}, nil
}

// templateSnapshotRef returns the template's current snapshot, for a linked
// clone, or nil when it has none.
func templateSnapshotRef(ctx context.Context, c *govmomiClient, tmpl *object.VirtualMachine) (*types.ManagedObjectReference, error) {
	var mvm mo.VirtualMachine
	if err := c.properties(ctx, tmpl, []string{"snapshot"}, &mvm); err != nil {
		return nil, err
	}
	if mvm.Snapshot == nil || mvm.Snapshot.CurrentSnapshot == nil {
		return nil, nil
	}
	return mvm.Snapshot.CurrentSnapshot, nil
}

// growDisk grows the VM's first virtual disk to at least sizeBytes.
func (c *govmomiClient) growDisk(ctx context.Context, vm *object.VirtualMachine, sizeBytes int64) error {
	devices, err := vm.Device(ctx)
	if err != nil {
		return errorf("reading cloned VM devices: %w", err)
	}
	disks := devices.SelectByType((*types.VirtualDisk)(nil))
	if len(disks) == 0 {
		return errorf("cloned VM has no virtual disk to grow")
	}
	disk, ok := disks[0].(*types.VirtualDisk)
	if !ok {
		return errorf("unexpected disk device type")
	}
	want := sizeBytes / 1024
	if disk.CapacityInKB >= want {
		return nil
	}
	disk.CapacityInKB = want
	spec := types.VirtualMachineConfigSpec{DeviceChange: []types.BaseVirtualDeviceConfigSpec{
		&types.VirtualDeviceConfigSpec{Operation: types.VirtualDeviceConfigSpecOperationEdit, Device: disk},
	}}
	task, err := vm.Reconfigure(ctx, spec)
	if err != nil {
		return errorf("growing disk: %w", err)
	}
	if err := task.Wait(ctx); err != nil {
		return errorf("growing disk: %w", err)
	}
	return nil
}

func (c *govmomiClient) SetGuestinfo(ctx context.Context, name string, data map[string]string) error {
	f, _, err := c.finderDC(ctx)
	if err != nil {
		return err
	}
	vm, err := f.VirtualMachine(ctx, name)
	if err != nil {
		return errorf("finding VM %q: %w", name, err)
	}
	opts := make([]types.BaseOptionValue, 0, len(data))
	for k, v := range data {
		opts = append(opts, &types.OptionValue{Key: k, Value: v})
	}
	task, err := vm.Reconfigure(ctx, types.VirtualMachineConfigSpec{ExtraConfig: opts})
	if err != nil {
		return errorf("setting guestinfo on %q: %w", name, err)
	}
	if err := task.Wait(ctx); err != nil {
		return errorf("setting guestinfo on %q: %w", name, err)
	}
	return nil
}

func (c *govmomiClient) PowerOn(ctx context.Context, name string) error {
	f, _, err := c.finderDC(ctx)
	if err != nil {
		return err
	}
	vm, err := f.VirtualMachine(ctx, name)
	if err != nil {
		return errorf("finding VM %q: %w", name, err)
	}
	state, err := vm.PowerState(ctx)
	if err != nil {
		return errorf("reading power state of %q: %w", name, err)
	}
	if state == types.VirtualMachinePowerStatePoweredOn {
		return nil
	}
	task, err := vm.PowerOn(ctx)
	if err != nil {
		return errorf("powering on %q: %w", name, err)
	}
	if err := task.Wait(ctx); err != nil {
		return errorf("powering on %q: %w", name, err)
	}
	return nil
}

func (c *govmomiClient) WaitForIP(ctx context.Context, name string) ([]string, error) {
	f, _, err := c.finderDC(ctx)
	if err != nil {
		return nil, err
	}
	vm, err := f.VirtualMachine(ctx, name)
	if err != nil {
		return nil, errorf("finding VM %q: %w", name, err)
	}
	deadline := time.Now().Add(ipWaitTimeout)
	for {
		var mvm mo.VirtualMachine
		if err := c.properties(ctx, vm, []string{"guest"}, &mvm); err == nil {
			if ips := guestIPv4s(&mvm); len(ips) > 0 {
				return ips, nil
			}
		}
		if time.Now().After(deadline) {
			return nil, errorf("timed out waiting for %q to report an IPv4 address", name)
		}
		select {
		case <-ctx.Done():
			return nil, ctx.Err()
		case <-time.After(ipPoll):
		}
	}
}

func (c *govmomiClient) DestroyVM(ctx context.Context, name string) error {
	f, _, err := c.finderDC(ctx)
	if err != nil {
		return err
	}
	vm, err := f.VirtualMachine(ctx, name)
	if err != nil {
		var nf *find.NotFoundError
		if errors.As(err, &nf) {
			return nil
		}
		return errorf("finding VM %q: %w", name, err)
	}
	state, err := vm.PowerState(ctx)
	if err == nil && state == types.VirtualMachinePowerStatePoweredOn {
		task, perr := vm.PowerOff(ctx)
		if perr == nil {
			perr = task.Wait(ctx)
		}
		if perr != nil {
			return errorf("powering off %q: %w", name, perr)
		}
	}
	task, err := vm.Destroy(ctx)
	if err != nil {
		return errorf("destroying %q: %w", name, err)
	}
	if err := task.Wait(ctx); err != nil {
		return errorf("destroying %q: %w", name, err)
	}
	return nil
}

func (c *govmomiClient) ListVMs(ctx context.Context, prefix string) ([]*VM, error) {
	f, _, err := c.finderDC(ctx)
	if err != nil {
		return nil, err
	}
	vms, err := f.VirtualMachineList(ctx, "*")
	if err != nil {
		var nf *find.NotFoundError
		if errors.As(err, &nf) {
			return nil, nil
		}
		return nil, errorf("listing VMs: %w", err)
	}
	var out []*VM
	for _, vm := range vms {
		name := vm.Name()
		if prefix != "" && !strings.HasPrefix(name, prefix) {
			continue
		}
		var mvm mo.VirtualMachine
		if err := c.properties(ctx, vm, []string{"config", "summary", "runtime", "guest"}, &mvm); err != nil {
			continue
		}
		out = append(out, vmView(name, &mvm))
	}
	return out, nil
}

func (c *govmomiClient) TemplateExists(ctx context.Context, name string) (bool, error) {
	f, _, err := c.finderDC(ctx)
	if err != nil {
		return false, err
	}
	vm, err := f.VirtualMachine(ctx, name)
	if err != nil {
		var nf *find.NotFoundError
		if errors.As(err, &nf) {
			return false, nil
		}
		return false, errorf("finding template %q: %w", name, err)
	}
	var mvm mo.VirtualMachine
	if err := c.properties(ctx, vm, []string{"summary.config.template"}, &mvm); err != nil {
		return false, errorf("reading template %q: %w", name, err)
	}
	return mvm.Summary.Config.Template, nil
}

// listNames retrieves the names of managed entities of the given types under
// the datacenter (or the root folder when no datacenter resolves).
func (c *govmomiClient) listNames(ctx context.Context, kinds []string) ([]string, error) {
	root := c.vim.ServiceContent.RootFolder
	if _, dc, err := c.finderDC(ctx); err == nil && dc != nil {
		root = dc.Reference()
	}
	m := view.NewManager(c.vim)
	v, err := m.CreateContainerView(ctx, root, kinds, true)
	if err != nil {
		return nil, errorf("creating container view: %w", err)
	}
	defer func() { _ = v.Destroy(ctx) }()
	var ents []mo.ManagedEntity
	if err := v.Retrieve(ctx, kinds, []string{"name"}, &ents); err != nil {
		return nil, errorf("retrieving %v: %w", kinds, err)
	}
	out := make([]string, 0, len(ents))
	for _, e := range ents {
		out = append(out, e.Name)
	}
	return out, nil
}

func (c *govmomiClient) ListNetworks(ctx context.Context) ([]string, error) {
	return c.listNames(ctx, []string{"Network", "DistributedVirtualPortgroup", "OpaqueNetwork"})
}

func (c *govmomiClient) ListDatastores(ctx context.Context) ([]string, error) {
	return c.listNames(ctx, []string{"Datastore"})
}

func (c *govmomiClient) ListTemplates(ctx context.Context) ([]string, error) {
	root := c.vim.ServiceContent.RootFolder
	if _, dc, err := c.finderDC(ctx); err == nil && dc != nil {
		root = dc.Reference()
	}
	m := view.NewManager(c.vim)
	v, err := m.CreateContainerView(ctx, root, []string{"VirtualMachine"}, true)
	if err != nil {
		return nil, errorf("creating container view: %w", err)
	}
	defer func() { _ = v.Destroy(ctx) }()
	var vms []mo.VirtualMachine
	if err := v.Retrieve(ctx, []string{"VirtualMachine"}, []string{"name", "summary.config.template"}, &vms); err != nil {
		return nil, errorf("retrieving templates: %w", err)
	}
	var out []string
	for _, vm := range vms {
		if vm.Summary.Config.Template {
			out = append(out, vm.Name)
		}
	}
	return out, nil
}

func (c *govmomiClient) Close() error {
	ctx := context.Background()
	return session.NewManager(c.vim).Logout(ctx)
}
