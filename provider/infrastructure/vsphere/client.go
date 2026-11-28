package vsphere

import (
	"context"
	"errors"
	"sync"
)

// ErrVMNotFound is returned by FindVM when no VM with the requested name exists.
var ErrVMNotFound = errors.New("vsphere: virtual machine not found")

// VM is the provider's view of a vSphere virtual machine.
type VM struct {
	// Name is the VM's name in its folder.
	Name string
	// UUID is the VM's BIOS/SMBIOS UUID (govmomi config.uuid), the stable
	// identity the provider ID "vsphere://<uuid>" is built from.
	UUID string
	// Annotation is the VM's free-form notes, which carry the klotho cluster tag.
	Annotation string
	// PoweredOn reports whether the VM is running.
	PoweredOn bool
	// Addresses are the guest IPv4 addresses vCenter knows about (from VMware
	// Tools), empty until the guest reports them.
	Addresses []string
}

// CloneInput is everything needed to clone the template into the bootstrap VM.
// The VM is created powered off so guestinfo can be set before boot.
type CloneInput struct {
	// Name is the VM name to create.
	Name string
	// Template is the source template name or inventory path.
	Template string
	// Datacenter, Datastore, Folder, ResourcePool and Network place the clone;
	// empty fields fall back to the datacenter defaults (see the client).
	Datacenter   string
	Datastore    string
	Folder       string
	ResourcePool string
	Network      string
	// CloneMode is "fullClone" or "linkedClone".
	CloneMode string
	// NumCPUs, MemoryMiB and DiskGiB size the clone. DiskGiB zero inherits the
	// template's disk size.
	NumCPUs   int32
	MemoryMiB int64
	DiskGiB   int32
	// Annotation is written into the VM's notes (the cluster tag).
	Annotation string
}

// Client is the subset of vCenter operations the provider needs, behind an
// interface so it is unit-tested against a fake without a reachable vCenter.
type Client interface {
	// FindVM returns the VM with the given name, or ErrVMNotFound.
	FindVM(ctx context.Context, name string) (*VM, error)
	// CloneVM clones the template into a powered-off VM and returns it with its
	// BIOS UUID assigned.
	CloneVM(ctx context.Context, in CloneInput) (*VM, error)
	// SetGuestinfo writes the given guestinfo.* ExtraConfig keys on a VM.
	SetGuestinfo(ctx context.Context, name string, data map[string]string) error
	// PowerOn powers a VM on. A VM already on is not an error.
	PowerOn(ctx context.Context, name string) error
	// WaitForIP waits for the guest to report at least one IPv4 address and
	// returns the addresses.
	WaitForIP(ctx context.Context, name string) ([]string, error)
	// DestroyVM powers off (if needed) and deletes a VM. A missing VM is not an
	// error.
	DestroyVM(ctx context.Context, name string) error
	// ListVMs returns every VM whose name begins with prefix.
	ListVMs(ctx context.Context, prefix string) ([]*VM, error)
	// TemplateExists reports whether a VM template named (or at the inventory
	// path) name exists and is marked as a template.
	TemplateExists(ctx context.Context, name string) (bool, error)
	// ListNetworks returns the datacenter's network names.
	ListNetworks(ctx context.Context) ([]string, error)
	// ListDatastores returns the datacenter's datastore names.
	ListDatastores(ctx context.Context) ([]string, error)
	// ListTemplates returns the names of VMs marked as templates.
	ListTemplates(ctx context.Context) ([]string, error)
	// Close releases the vCenter session.
	Close() error
}

// lazyClient defers opening the vCenter session until the first call, so
// building the provider (and read-only commands that never reach vCenter) does
// not require a reachable host. It is safe for concurrent use.
type lazyClient struct {
	cfg  Config
	mu   sync.Mutex
	real Client
	err  error
}

func newLazyClient(cfg Config) *lazyClient { return &lazyClient{cfg: cfg} }

func (c *lazyClient) get() (Client, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.real != nil || c.err != nil {
		return c.real, c.err
	}
	c.real, c.err = NewClient(c.cfg)
	return c.real, c.err
}

func (c *lazyClient) FindVM(ctx context.Context, name string) (*VM, error) {
	r, err := c.get()
	if err != nil {
		return nil, err
	}
	return r.FindVM(ctx, name)
}

func (c *lazyClient) CloneVM(ctx context.Context, in CloneInput) (*VM, error) {
	r, err := c.get()
	if err != nil {
		return nil, err
	}
	return r.CloneVM(ctx, in)
}

func (c *lazyClient) SetGuestinfo(ctx context.Context, name string, data map[string]string) error {
	r, err := c.get()
	if err != nil {
		return err
	}
	return r.SetGuestinfo(ctx, name, data)
}

func (c *lazyClient) PowerOn(ctx context.Context, name string) error {
	r, err := c.get()
	if err != nil {
		return err
	}
	return r.PowerOn(ctx, name)
}

func (c *lazyClient) WaitForIP(ctx context.Context, name string) ([]string, error) {
	r, err := c.get()
	if err != nil {
		return nil, err
	}
	return r.WaitForIP(ctx, name)
}

func (c *lazyClient) DestroyVM(ctx context.Context, name string) error {
	r, err := c.get()
	if err != nil {
		return err
	}
	return r.DestroyVM(ctx, name)
}

func (c *lazyClient) ListVMs(ctx context.Context, prefix string) ([]*VM, error) {
	r, err := c.get()
	if err != nil {
		return nil, err
	}
	return r.ListVMs(ctx, prefix)
}

func (c *lazyClient) TemplateExists(ctx context.Context, name string) (bool, error) {
	r, err := c.get()
	if err != nil {
		return false, err
	}
	return r.TemplateExists(ctx, name)
}

func (c *lazyClient) ListNetworks(ctx context.Context) ([]string, error) {
	r, err := c.get()
	if err != nil {
		return nil, err
	}
	return r.ListNetworks(ctx)
}

func (c *lazyClient) ListDatastores(ctx context.Context) ([]string, error) {
	r, err := c.get()
	if err != nil {
		return nil, err
	}
	return r.ListDatastores(ctx)
}

func (c *lazyClient) ListTemplates(ctx context.Context) ([]string, error) {
	r, err := c.get()
	if err != nil {
		return nil, err
	}
	return r.ListTemplates(ctx)
}

func (c *lazyClient) Close() error {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.real != nil {
		return c.real.Close()
	}
	return nil
}
