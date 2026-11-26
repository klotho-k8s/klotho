package hyperv

import (
	"context"
	"errors"
)

// ErrVMNotFound is returned by GetVM when no VM with the requested name exists.
var ErrVMNotFound = errors.New("hyperv: virtual machine not found")

// Power states Hyper-V reports.
const (
	StateRunning = "Running"
	StateOff     = "Off"
)

// VM is the slice of a Hyper-V virtual machine this provider needs.
type VM struct {
	// Name is the VM name on the host (unique within a host).
	Name string
	// ID is the Hyper-V VM Id, a GUID, used to build the provider ID.
	ID string
	// State is the power state, for example "Running" or "Off".
	State string
	// Notes carries the klotho cluster tag.
	Notes string
}

// Running reports whether the VM is powered on.
func (v *VM) Running() bool { return v != nil && v.State == StateRunning }

// CreateInput describes the bootstrap VM to create. The provider copies Image
// to a per-VM disk, creates a Generation 2 VM from it on Switch, and writes
// Notes so Plan and Teardown can recognise it.
type CreateInput struct {
	Name       string
	Image      string // base VHDX to copy the VM's disk from
	Switch     string
	VMPath     string // directory for the VM's disk; empty means the host default
	MemoryMiB  int
	CPUs       int
	Notes      string
	SecureBoot bool
}

// Client is the subset of Hyper-V operations the provider needs, behind an
// interface so it is unit-tested against a fake without a Windows host.
type Client interface {
	// GetVM returns the VM with the given name, or ErrVMNotFound.
	GetVM(ctx context.Context, name string) (*VM, error)
	// CreateVM creates a Generation 2 VM from in (copy the base VHDX, New-VM,
	// set processor/memory, notes), attaches no ISO and does not start it.
	// It returns the created VM (with its GUID).
	CreateVM(ctx context.Context, in CreateInput) (*VM, error)
	// AttachISO attaches isoPath to the VM as a SCSI DVD drive.
	AttachISO(ctx context.Context, name, isoPath string) error
	// SetFirmware sets the Gen2 boot order (boot from the hard disk) and Secure
	// Boot. For an unsigned Linux image secureBoot must be false.
	SetFirmware(ctx context.Context, name string, secureBoot bool) error
	// StartVM powers the VM on.
	StartVM(ctx context.Context, name string) error
	// StopVM powers the VM off. When turnOff is true it is a hard power-off
	// (Stop-VM -TurnOff), otherwise a graceful shutdown.
	StopVM(ctx context.Context, name string, turnOff bool) error
	// RemoveVM deletes the VM and its disk; a missing VM is not an error.
	RemoveVM(ctx context.Context, name string) error
	// ListVMs returns every VM whose name begins with prefix.
	ListVMs(ctx context.Context, prefix string) ([]*VM, error)
	// GetIPAddresses returns the guest IPv4/IPv6 addresses reported by the
	// Hyper-V integration services.
	GetIPAddresses(ctx context.Context, name string) ([]string, error)
	// ListSwitches returns the names of the host's virtual switches.
	ListSwitches(ctx context.Context) ([]string, error)
}
