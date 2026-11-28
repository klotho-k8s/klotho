package vz

import (
	"context"
	"errors"
	"sync"
)

// ErrVMNotFound is returned by GetVM when no VM with the requested name exists
// on the host. The gRPC client maps the daemon's codes.NotFound back to it so
// callers that use errors.Is keep working across the transport.
var ErrVMNotFound = errors.New("vz: virtual machine not found")

// Normalized VM power states the daemon reports, a small stable set the provider
// reasons about.
const (
	StateRunning  = "Running"
	StateStopped  = "Stopped"
	StateStarting = "Starting"
	StateStopping = "Stopping"
	StateError    = "Error"
)

// VM is the provider's view of a vz virtual machine.
type VM struct {
	// Name is the VM name on the host (unique within a host).
	Name string
	// ID is the vmid, a UUID the daemon assigns. It is the stable identity the
	// provider ID is built from ("vz://<host>/<vmid>") and the cloud-init
	// instance-id the daemon seeds.
	ID string
	// State is the normalized power state, for example "Running" or "Stopped".
	State string
	// CPUCount is the number of virtual CPUs.
	CPUCount int
	// MemoryBytes is the configured memory, in bytes.
	MemoryBytes int64
	// DiskImage is the path, on the host, to the VM's boot disk.
	DiskImage string
	// Notes carries the klotho cluster tag so Plan and Teardown can recognise
	// VMs this provider created.
	Notes string
}

// Running reports whether the VM is powered on.
func (v *VM) Running() bool { return v != nil && v.State == StateRunning }

// CreateVMInput describes the bootstrap VM to register with the daemon. The
// daemon copies DiskImage to a per-VM boot disk and records Notes; the VM is
// created in the Stopped state and does not boot until StartVM.
type CreateVMInput struct {
	Name        string
	MemoryBytes int64
	CPUCount    int
	DiskImage   string // base cloud image path on the host; the daemon copies it per VM
	Notes       string
}

// Client is the subset of capvz-daemon operations the provider needs, behind an
// interface so it is unit-tested against a fake without a Mac or a running
// daemon. In production it is a gRPC client to the capvz-daemon.
type Client interface {
	// Exists reports whether a VM with the given name exists on the host.
	Exists(ctx context.Context, name string) (bool, error)
	// GetVM returns the VM with the given name, or ErrVMNotFound.
	GetVM(ctx context.Context, name string) (*VM, error)
	// CreateVM registers a new VM (in the Stopped state) with the daemon; it does
	// not boot a runner yet (StartVM does). It returns the created VM with its
	// assigned vmid.
	CreateVM(ctx context.Context, in CreateVMInput) (*VM, error)
	// StartVM boots the VM by bootstrapping its launchd-managed runner.
	StartVM(ctx context.Context, name string) error
	// StopVM stops the VM. When force is true the runner is killed instead of
	// asked to shut the guest down gracefully. A VM that is not running, or does
	// not exist, is not an error.
	StopVM(ctx context.Context, name string, force bool) error
	// RemoveVM stops the runner and forgets the VM, deleting its per-VM working
	// disk and seed image. The base image is never touched. A missing VM is not
	// an error.
	RemoveVM(ctx context.Context, name string) error
	// AttachCloudInit hands the daemon the raw cloud-init user-data for a VM; the
	// daemon builds the NoCloud cidata seed on the host from it and records it for
	// the runner to attach at boot. The meta-data (instance-id = the vmid,
	// local-hostname = name) is derived by the daemon. No seed path crosses the
	// host boundary.
	AttachCloudInit(ctx context.Context, name string, userData []byte) error
	// GetIPAddresses returns the guest IP addresses the runner discovered (from
	// the host's DHCP leases, matched by the VM's MAC).
	GetIPAddresses(ctx context.Context, name string) ([]string, error)
	// ListVMs returns every VM the daemon knows about.
	ListVMs(ctx context.Context) ([]*VM, error)
	// Close releases the connection to the daemon.
	Close() error
}

// lazyClient defers building the gRPC client (reading the CA bundle, setting up
// TLS and token credentials, dialing) until the first call, so building the
// provider and read-only commands that never reach the daemon do not require a
// reachable Mac or a valid CA file. It is safe for concurrent use.
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

func (c *lazyClient) Exists(ctx context.Context, name string) (bool, error) {
	r, err := c.get()
	if err != nil {
		return false, err
	}
	return r.Exists(ctx, name)
}

func (c *lazyClient) GetVM(ctx context.Context, name string) (*VM, error) {
	r, err := c.get()
	if err != nil {
		return nil, err
	}
	return r.GetVM(ctx, name)
}

func (c *lazyClient) CreateVM(ctx context.Context, in CreateVMInput) (*VM, error) {
	r, err := c.get()
	if err != nil {
		return nil, err
	}
	return r.CreateVM(ctx, in)
}

func (c *lazyClient) StartVM(ctx context.Context, name string) error {
	r, err := c.get()
	if err != nil {
		return err
	}
	return r.StartVM(ctx, name)
}

func (c *lazyClient) StopVM(ctx context.Context, name string, force bool) error {
	r, err := c.get()
	if err != nil {
		return err
	}
	return r.StopVM(ctx, name, force)
}

func (c *lazyClient) RemoveVM(ctx context.Context, name string) error {
	r, err := c.get()
	if err != nil {
		return err
	}
	return r.RemoveVM(ctx, name)
}

func (c *lazyClient) AttachCloudInit(ctx context.Context, name string, userData []byte) error {
	r, err := c.get()
	if err != nil {
		return err
	}
	return r.AttachCloudInit(ctx, name, userData)
}

func (c *lazyClient) GetIPAddresses(ctx context.Context, name string) ([]string, error) {
	r, err := c.get()
	if err != nil {
		return nil, err
	}
	return r.GetIPAddresses(ctx, name)
}

func (c *lazyClient) ListVMs(ctx context.Context) ([]*VM, error) {
	r, err := c.get()
	if err != nil {
		return nil, err
	}
	return r.ListVMs(ctx)
}

func (c *lazyClient) Close() error {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.real != nil {
		return c.real.Close()
	}
	return nil
}
