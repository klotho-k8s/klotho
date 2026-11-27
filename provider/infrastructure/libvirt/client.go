package libvirt

import (
	"context"
	"errors"
	"sync"
)

// ErrDomainNotFound is returned by GetDomain when no domain with the requested
// name exists on the host.
var ErrDomainNotFound = errors.New("libvirt: domain not found")

// Normalized domain power states. They map libvirt's VIR_DOMAIN_* states to a
// small, stable set the provider reasons about.
const (
	StateRunning  = "Running"
	StateShutoff  = "Shutoff"
	StatePaused   = "Paused"
	StateNostate  = "Nostate"
	StateShutdown = "Shutdown"
	StateCrashed  = "Crashed"
)

// Domain is the provider's view of a libvirt domain.
type Domain struct {
	// Name is the domain name on the host (unique within a host).
	Name string
	// UUID is the domain UUID libvirt assigned, as the canonical 8-4-4-4-12
	// string. It is the stable identity the provider ID is built from.
	UUID string
	// State is the normalized power state, for example "Running" or "Shutoff".
	State string
	// Description is the domain's free-form <description>, which carries the
	// klotho cluster tag.
	Description string
}

// Running reports whether the domain is powered on.
func (d *Domain) Running() bool { return d != nil && d.State == StateRunning }

// CreateDomainInput is everything needed to define (but not start) the
// bootstrap domain: a qcow2 overlay backed by a base image, a NoCloud cidata
// seed built from CIData, and the domain XML tying them together on the chosen
// network.
type CreateDomainInput struct {
	// Name is the domain name to define.
	Name string
	// UUID is the domain UUID to assign. It is pre-generated so the NoCloud
	// meta-data instance-id matches the provider ID; it must be a canonical
	// UUID string.
	UUID string
	// Description is written into the domain's <description> (the cluster tag).
	Description string
	// MemoryBytes is the guest memory in bytes.
	MemoryBytes int64
	// VCPU is the number of virtual CPUs.
	VCPU int
	// BaseImage is the name of the base cloud image volume in StoragePool used
	// as the overlay's backing file.
	BaseImage string
	// OverlayName and CIDataName are the volume names created in StoragePool.
	OverlayName string
	CIDataName  string
	// StoragePool is the libvirt storage pool for the base image, the per-VM
	// overlay, and the cidata seed volume.
	StoragePool string
	// DiskSizeBytes optionally sets the overlay's virtual capacity. Zero
	// inherits the base image's virtual size.
	DiskSizeBytes int64
	// NetworkName is the libvirt network name or host bridge name for the NIC.
	NetworkName string
	// NetworkType is "network" (a libvirt virtual network) or "bridge".
	NetworkType string
	// CIData is the NoCloud cidata seed image (an iso9660 "CIDATA" filesystem);
	// the client uploads it into the cidata volume.
	CIData []byte
}

// Client is the subset of libvirt operations the provider needs, behind an
// interface so it is unit-tested against a fake without a running libvirtd.
type Client interface {
	// Host is the libvirt host used in provider IDs (parsed from the
	// connection URI; "localhost" for a local socket).
	Host() string
	// GetDomain returns the domain with the given name, or ErrDomainNotFound.
	GetDomain(ctx context.Context, name string) (*Domain, error)
	// CreateDomain creates the overlay disk and cidata seed volume and DEFINES
	// the domain (persistent, not started). StartDomain boots it.
	CreateDomain(ctx context.Context, in CreateDomainInput) (*Domain, error)
	// StartDomain boots a defined domain.
	StartDomain(ctx context.Context, name string) error
	// DestroyDomain force-stops a running domain (virsh destroy). A domain that
	// is not running, or does not exist, is not an error.
	DestroyDomain(ctx context.Context, name string) error
	// UndefineDomain removes the domain definition and deletes its per-domain
	// overlay and cidata volumes. The base image is never touched. A missing
	// domain is not an error.
	UndefineDomain(ctx context.Context, name string) error
	// GetDomainIPs returns the guest IP addresses libvirt knows about (from the
	// qemu-guest-agent, falling back to the network's DHCP leases).
	GetDomainIPs(ctx context.Context, name string) ([]string, error)
	// ListDomains returns every domain whose name begins with prefix.
	ListDomains(ctx context.Context, prefix string) ([]*Domain, error)
	// ListNetworks returns the names of the host's libvirt virtual networks.
	ListNetworks(ctx context.Context) ([]string, error)
	// ListStoragePools returns the names of the host's storage pools.
	ListStoragePools(ctx context.Context) ([]string, error)
	// VolumeExists reports whether a volume named vol exists in pool.
	VolumeExists(ctx context.Context, pool, vol string) (bool, error)
	// Close releases the connection to libvirtd.
	Close() error
}

// lazyClient defers opening the libvirtd connection until the first call, so
// building the provider (and read-only commands that never reach libvirt) does
// not require a reachable host. It is safe for concurrent use.
type lazyClient struct {
	uri  string
	mu   sync.Mutex
	real Client
	err  error
}

func newLazyClient(uri string) *lazyClient { return &lazyClient{uri: uri} }

func (c *lazyClient) get() (Client, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.real != nil || c.err != nil {
		return c.real, c.err
	}
	c.real, c.err = NewClient(c.uri)
	return c.real, c.err
}

func (c *lazyClient) Host() string { return HostFromURI(c.uri) }

func (c *lazyClient) GetDomain(ctx context.Context, name string) (*Domain, error) {
	r, err := c.get()
	if err != nil {
		return nil, err
	}
	return r.GetDomain(ctx, name)
}

func (c *lazyClient) CreateDomain(ctx context.Context, in CreateDomainInput) (*Domain, error) {
	r, err := c.get()
	if err != nil {
		return nil, err
	}
	return r.CreateDomain(ctx, in)
}

func (c *lazyClient) StartDomain(ctx context.Context, name string) error {
	r, err := c.get()
	if err != nil {
		return err
	}
	return r.StartDomain(ctx, name)
}

func (c *lazyClient) DestroyDomain(ctx context.Context, name string) error {
	r, err := c.get()
	if err != nil {
		return err
	}
	return r.DestroyDomain(ctx, name)
}

func (c *lazyClient) UndefineDomain(ctx context.Context, name string) error {
	r, err := c.get()
	if err != nil {
		return err
	}
	return r.UndefineDomain(ctx, name)
}

func (c *lazyClient) GetDomainIPs(ctx context.Context, name string) ([]string, error) {
	r, err := c.get()
	if err != nil {
		return nil, err
	}
	return r.GetDomainIPs(ctx, name)
}

func (c *lazyClient) ListDomains(ctx context.Context, prefix string) ([]*Domain, error) {
	r, err := c.get()
	if err != nil {
		return nil, err
	}
	return r.ListDomains(ctx, prefix)
}

func (c *lazyClient) ListNetworks(ctx context.Context) ([]string, error) {
	r, err := c.get()
	if err != nil {
		return nil, err
	}
	return r.ListNetworks(ctx)
}

func (c *lazyClient) ListStoragePools(ctx context.Context) ([]string, error) {
	r, err := c.get()
	if err != nil {
		return nil, err
	}
	return r.ListStoragePools(ctx)
}

func (c *lazyClient) VolumeExists(ctx context.Context, pool, vol string) (bool, error) {
	r, err := c.get()
	if err != nil {
		return false, err
	}
	return r.VolumeExists(ctx, pool, vol)
}

func (c *lazyClient) Close() error {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.real != nil {
		return c.real.Close()
	}
	return nil
}
