package proxmox

import (
	"bytes"
	"context"
	"crypto/rand"
	"errors"
	"fmt"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"time"

	gp "github.com/luthermonson/go-proxmox"
	"sigs.k8s.io/yaml"

	"github.com/klotho-k8s/klotho/pkg/cloudinit"
	"github.com/klotho-k8s/klotho/pkg/cluster"
)

// ISODevice is the drive the NoCloud ISO is attached to. The Cluster API
// Proxmox provider attaches its own cloud-init ISO to the same device and
// detaches it from there once the machine is up, so the adopted machine
// cleans up after this provider's ISO the same way.
const ISODevice = "ide0"

// ProviderIDPrefix is the scheme of a Cluster API Proxmox provider ID,
// followed by the VM's SMBIOS UUID.
const ProviderIDPrefix = "proxmox://"

var uuidPattern = regexp.MustCompile(`^[0-9a-fA-F]{8}-[0-9a-fA-F]{4}-[0-9a-fA-F]{4}-[0-9a-fA-F]{4}-[0-9a-fA-F]{12}$`)

// cloudInitDrive matches a Proxmox-generated cloud-init drive, which would
// present a second "cidata" volume next to this provider's ISO.
var cloudInitDrive = regexp.MustCompile(`^(ide|sata|scsi)\d+$`)

// Launch clones the template into the bootstrap VM, attaches a NoCloud ISO
// carrying userData (the user-data document, not an ISO), starts the VM and
// waits for the guest agent to report an IPv4 address.
//
// The ISO's meta-data sets instance-id to the VM's SMBIOS UUID and
// local-hostname to the VM name, matching what the Cluster API Proxmox
// provider writes, so "proxmox://{{ ds.meta_data.instance_id }}" in the
// kubelet configuration yields the provider ID the adopted machine carries.
// When the cloned config has no SMBIOS UUID, Launch sets a random one.
//
// The returned node's ID is "proxmox://<uuid>" and its Name is the VM name;
// the VMID is not returned and is looked up again by name and tag (see the
// package documentation). On any failure after the clone, the VM and the ISO
// are removed on a best-effort basis and the original error is returned.
func (p *Provider) Launch(ctx context.Context, spec *cluster.Spec, userData []byte) (*cluster.Node, error) {
	if len(userData) == 0 {
		return nil, errorf("empty user-data")
	}
	t, err := p.locate(ctx, spec)
	if err != nil {
		return nil, err
	}
	for _, v := range t.VMs {
		if v.Name == t.VMName && resHasTag(v, t.Tag) {
			return nil, errorf("bootstrap VM %s already exists as VM %d on node %s; delete the cluster first",
				t.VMName, v.VMID, v.Node)
		}
	}

	vmid, err := p.clone(ctx, t)
	if err != nil {
		return nil, err
	}
	l := &launch{p: p, t: t, vmid: vmid}
	node, err := l.run(ctx, userData)
	if err != nil {
		if cerr := l.cleanup(ctx); cerr != nil {
			return nil, errors.Join(errorf("launching %s (VM %d): %w", t.VMName, vmid, err), cerr)
		}
		return nil, errorf("launching %s (VM %d): %w", t.VMName, vmid, err)
	}
	return node, nil
}

// clone creates the VM and waits for the clone task. With a VMID range it
// tries each free ID in order; otherwise it asks for the next free ID and
// retries a few times if another client takes it first.
func (p *Provider) clone(ctx context.Context, t *target) (int, error) {
	opts := gp.VirtualMachineCloneOptions{Name: t.VMName, Full: gp.IntOrBool(t.FullClone), Pool: t.Pool}
	if t.Node != t.Template.Node {
		opts.Target = t.Node
	}
	try := func(id int) error {
		o := opts
		upid, err := p.cloneVM(ctx, t.Template.Node, int(t.Template.VMID), id, &o)
		if err != nil {
			return err
		}
		return p.wait(ctx, upid)
	}

	if lo, hi := t.VMIDRange[0], t.VMIDRange[1]; lo > 0 {
		used := map[int]bool{}
		for _, v := range t.VMs {
			used[int(v.VMID)] = true
		}
		for id := lo; id <= hi; id++ {
			if used[id] {
				continue
			}
			err := try(id)
			if errors.Is(err, errVMExists) {
				continue
			}
			if err != nil {
				return 0, errorf("cloning template %d into VM %d: %w", t.Template.VMID, id, err)
			}
			return id, nil
		}
		return 0, errorf("no free VMID in range [%d, %d]", lo, hi)
	}

	var err error
	for range 3 {
		var id int
		if id, err = p.nextID(ctx); err != nil {
			return 0, err
		}
		err = try(id)
		if errors.Is(err, errVMExists) {
			continue
		}
		if err != nil {
			return 0, errorf("cloning template %d into VM %d: %w", t.Template.VMID, id, err)
		}
		return id, nil
	}
	return 0, errorf("cloning template %d: %w", t.Template.VMID, err)
}

// launch is the state of one Launch after the clone, for cleanup.
type launch struct {
	p    *Provider
	t    *target
	vmid int
	// isoVolID is set once the ISO upload has been attempted.
	isoVolID string
}

func (l *launch) run(ctx context.Context, userData []byte) (*cluster.Node, error) {
	p, t := l.p, l.t
	cfg, err := p.readVMConfig(ctx, t.Node, l.vmid)
	if err != nil {
		return nil, err
	}

	uuid, smbios, err := smbiosUUID(cfg)
	if err != nil {
		return nil, err
	}
	if smbios != "" {
		if err := p.setVMConfig(ctx, t.Node, l.vmid, map[string]string{"smbios1": smbios}); err != nil {
			return nil, fmt.Errorf("setting SMBIOS UUID: %w", err)
		}
	}

	iso, err := buildISO(uuid, t.VMName, userData)
	if err != nil {
		return nil, err
	}
	if err := l.upload(ctx, iso); err != nil {
		return nil, err
	}

	if err := p.setVMConfig(ctx, t.Node, l.vmid, vmConfig(t.Resolved, cfg)); err != nil {
		return nil, fmt.Errorf("configuring VM: %w", err)
	}
	if err := l.growBootDisk(ctx, cfg); err != nil {
		return nil, err
	}
	upid, err := p.startVM(ctx, t.Node, l.vmid)
	if err != nil {
		return nil, fmt.Errorf("starting VM: %w", err)
	}
	if err := l.p.wait(ctx, upid); err != nil {
		return nil, fmt.Errorf("starting VM: %w", err)
	}
	addrs, err := l.p.waitForAddress(ctx, t.Node, l.vmid, t.AgentTimeout)
	if err != nil {
		return nil, err
	}
	return &cluster.Node{ID: ProviderIDPrefix + uuid, Name: t.VMName, Addresses: addrs}, nil
}

// upload replaces any leftover ISO of the same name and uploads iso.
func (l *launch) upload(ctx context.Context, iso []byte) error {
	p, t := l.p, l.t
	volid := t.ISOStorage + ":iso/" + t.ISOName
	vols, err := p.listISOs(ctx, t.Node, t.ISOStorage)
	if err != nil {
		return fmt.Errorf("listing ISOs on %s: %w", t.ISOStorage, err)
	}
	for _, v := range vols {
		if v.Volid == volid {
			upid, err := p.deleteVolume(ctx, t.Node, t.ISOStorage, volid)
			if err == nil {
				err = p.wait(ctx, upid)
			}
			if err != nil {
				return fmt.Errorf("removing stale %s: %w", volid, err)
			}
		}
	}
	l.isoVolID = volid
	upid, err := p.uploadISO(ctx, t.Node, t.ISOStorage, t.ISOName, bytes.NewReader(iso), int64(len(iso)))
	if err != nil {
		return fmt.Errorf("uploading %s: %w", volid, err)
	}
	if err := l.p.wait(ctx, upid); err != nil {
		return fmt.Errorf("uploading %s: %w", volid, err)
	}
	return nil
}

// cleanup removes the VM and the ISO. It runs on a context detached from
// ctx's cancellation, bounded by the provider's cleanup timeout, so an
// interrupted Launch still cleans up.
func (l *launch) cleanup(ctx context.Context) error {
	ctx, cancel := context.WithTimeout(context.WithoutCancel(ctx), l.p.cleanupTimeout)
	defer cancel()
	p, t := l.p, l.t
	var errs []error
	if err := p.destroyVM(ctx, t.Node, l.vmid); err != nil {
		errs = append(errs, fmt.Errorf("cleanup: VM %d: %w", l.vmid, err))
	}
	if l.isoVolID != "" {
		upid, err := p.deleteVolume(ctx, t.Node, t.ISOStorage, l.isoVolID)
		if err == nil {
			err = p.wait(ctx, upid)
		}
		if err != nil {
			errs = append(errs, fmt.Errorf("cleanup: %s: %w", l.isoVolID, err))
		}
	}
	return errors.Join(errs...)
}

// destroyVM stops the VM if it is running and deletes it with purge. A VM
// that no longer exists is not an error.
func (p *Provider) destroyVM(ctx context.Context, node string, vmid int) error {
	st, err := p.vmStatus(ctx, node, vmid)
	if errors.Is(err, errVMNotFound) {
		return nil
	}
	if err != nil {
		return err
	}
	if st.Status == "running" {
		upid, err := p.stopVM(ctx, node, vmid)
		if err == nil {
			err = p.wait(ctx, upid)
		}
		if err != nil {
			return fmt.Errorf("stopping: %w", err)
		}
	}
	upid, err := p.deleteVM(ctx, node, vmid, true)
	if err == nil {
		err = p.wait(ctx, upid)
	}
	if err != nil {
		return fmt.Errorf("deleting: %w", err)
	}
	return nil
}

// waitForAddress polls the guest agent until it reports a usable IPv4
// address or timeout passes.
func (p *Provider) waitForAddress(ctx context.Context, node string, vmid int, timeout time.Duration) ([]string, error) {
	pctx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()
	var last error
	for {
		ifaces, err := p.agentInterfaces(pctx, node, vmid)
		switch {
		case err == nil:
			if ips := guestIPv4Addresses(ifaces); len(ips) > 0 {
				out := make([]string, len(ips))
				for i, ip := range ips {
					out[i] = ip.String()
				}
				return out, nil
			}
			last = errors.New("the guest agent reports no IPv4 address yet")
		case errors.Is(err, errAgentNotConfigured):
			return nil, err
		case pctx.Err() != nil && last != nil:
			// The deadline cut this poll short; the previous answer is the
			// real reason the wait failed.
		default:
			last = err
		}
		timer := time.NewTimer(p.poll)
		select {
		case <-pctx.Done():
			timer.Stop()
			if ctx.Err() != nil {
				return nil, ctx.Err()
			}
			return nil, fmt.Errorf("no IPv4 address from the guest agent within %s: %w", timeout, last)
		case <-timer.C:
		}
	}
}

// smbiosUUID returns the UUID from the config's smbios1 value. When there is
// none it generates one and also returns the smbios1 value to set, keeping
// any other smbios1 fields.
func smbiosUUID(cfg map[string]any) (uuid, newValue string, err error) {
	raw, _ := cfg["smbios1"].(string)
	var fields []string
	for _, f := range strings.Split(raw, ",") {
		if f = strings.TrimSpace(f); f == "" {
			continue
		}
		if v, ok := strings.CutPrefix(f, "uuid="); ok {
			if !uuidPattern.MatchString(v) {
				return "", "", fmt.Errorf("smbios1 uuid %q is not a UUID", v)
			}
			return strings.ToLower(v), "", nil
		}
		fields = append(fields, f)
	}
	uuid, err = newUUID()
	if err != nil {
		return "", "", err
	}
	return uuid, strings.Join(append(fields, "uuid="+uuid), ","), nil
}

func newUUID() (string, error) {
	var b [16]byte
	if _, err := rand.Read(b[:]); err != nil {
		return "", err
	}
	b[6] = b[6]&0x0f | 0x40 // version 4
	b[8] = b[8]&0x3f | 0x80 // RFC 4122 variant
	return fmt.Sprintf("%x-%x-%x-%x-%x", b[0:4], b[4:6], b[6:8], b[8:10], b[10:16]), nil
}

type metaData struct {
	InstanceID    string `json:"instance-id"`
	LocalHostname string `json:"local-hostname"`
}

// buildISO packs the NoCloud datasource. No network-config is written, so the
// image's default (DHCP on the first NIC) applies.
func buildISO(uuid, hostname string, userData []byte) ([]byte, error) {
	md, err := yaml.Marshal(metaData{InstanceID: uuid, LocalHostname: hostname})
	if err != nil {
		return nil, err
	}
	return cloudinit.ISO(&cloudinit.Bundle{UserData: userData, MetaData: md})
}

// vmConfig is the configuration applied to the clone before it starts.
func vmConfig(r Resolved, current map[string]any) map[string]string {
	tags := parseTags(str(current["tags"]))
	if !containsFold(tags, r.Tag) {
		tags = append(tags, r.Tag)
	}
	out := map[string]string{
		ISODevice:     r.ISOStorage + ":iso/" + r.ISOName + ",media=cdrom",
		"tags":        joinTags(tags),
		"description": "bootstrap node for " + r.Cluster + ", managed by klotho",
	}
	if !agentEnabled(str(current["agent"])) {
		out["agent"] = "1"
	}
	if r.Cores > 0 {
		out["cores"] = strconv.Itoa(r.Cores)
	}
	if r.Sockets > 0 {
		out["sockets"] = strconv.Itoa(r.Sockets)
	}
	if r.MemoryMiB > 0 {
		out["memory"] = strconv.Itoa(r.MemoryMiB)
	}
	if r.BridgeSet {
		out["net0"] = withBridge(str(current["net0"]), r.Bridge)
	}
	var del []string
	for k, v := range current {
		if k != ISODevice && cloudInitDrive.MatchString(k) && strings.Contains(str(v), "cloudinit") {
			del = append(del, k)
		}
	}
	if len(del) > 0 {
		sort.Strings(del)
		out["delete"] = strings.Join(del, ",")
	}
	return out
}

// agentEnabled reports whether a QEMU agent option string enables the agent:
// "1", "enabled=1", or either followed by further options.
func agentEnabled(v string) bool {
	first, _, _ := strings.Cut(v, ",")
	first = strings.TrimPrefix(strings.TrimSpace(first), "enabled=")
	return first == "1" || first == "true" || first == "on" || first == "yes"
}

// withBridge rewrites the bridge of a NIC option string, keeping the model
// and MAC address, or returns a fresh virtio NIC on bridge.
func withBridge(net0, bridge string) string {
	if strings.TrimSpace(net0) == "" {
		return "virtio,bridge=" + bridge
	}
	parts := strings.Split(net0, ",")
	replaced := false
	for i, p := range parts {
		if strings.HasPrefix(p, "bridge=") {
			parts[i] = "bridge=" + bridge
			replaced = true
		}
	}
	if !replaced {
		parts = append(parts, "bridge="+bridge)
	}
	return strings.Join(parts, ",")
}

func str(v any) string {
	switch t := v.(type) {
	case string:
		return t
	case nil:
		return ""
	case float64:
		return strconv.FormatFloat(t, 'f', -1, 64)
	default:
		return fmt.Sprint(t)
	}
}

func containsFold(list []string, s string) bool {
	for _, v := range list {
		if strings.EqualFold(v, s) {
			return true
		}
	}
	return false
}

// growBootDisk resizes the clone's boot disk to Resolved.BootSizeGiB when the
// template's disk is smaller. Shrinking is never attempted. cfg is the VM's
// configuration as read after cloning.
func (l *launch) growBootDisk(ctx context.Context, cfg map[string]any) error {
	t := l.t
	if t.BootSizeGiB <= 0 {
		return nil
	}
	entry := str(cfg[t.BootDevice])
	if entry == "" {
		return fmt.Errorf("growing boot disk: VM %d has no disk %s (set %s)", l.vmid, t.BootDevice, VarBootVolumeDevice)
	}
	want := int64(t.BootSizeGiB) << 30
	if cur, ok := diskSizeBytes(entry); ok && cur >= want {
		return nil
	}
	size := strconv.Itoa(t.BootSizeGiB) + "G"
	upid, err := l.p.resizeDisk(ctx, t.Node, l.vmid, t.BootDevice, size)
	if err != nil {
		return fmt.Errorf("growing boot disk %s to %s: %w", t.BootDevice, size, err)
	}
	if upid != "" {
		if err := l.p.wait(ctx, upid); err != nil {
			return fmt.Errorf("growing boot disk %s to %s: %w", t.BootDevice, size, err)
		}
	}
	return nil
}

// diskSizeBytes reads the size option of a Proxmox disk entry such as
// "local:9100/vm-9100-disk-0.qcow2,size=3584M".
func diskSizeBytes(entry string) (int64, bool) {
	for _, opt := range strings.Split(entry, ",") {
		v, ok := strings.CutPrefix(strings.TrimSpace(opt), "size=")
		if !ok {
			continue
		}
		mult := int64(1)
		switch {
		case strings.HasSuffix(v, "T"):
			mult, v = 1<<40, strings.TrimSuffix(v, "T")
		case strings.HasSuffix(v, "G"):
			mult, v = 1<<30, strings.TrimSuffix(v, "G")
		case strings.HasSuffix(v, "M"):
			mult, v = 1<<20, strings.TrimSuffix(v, "M")
		case strings.HasSuffix(v, "K"):
			mult, v = 1<<10, strings.TrimSuffix(v, "K")
		}
		n, err := strconv.ParseInt(v, 10, 64)
		if err != nil {
			return 0, false
		}
		return n * mult, true
	}
	return 0, false
}
