// Package hyperv is the infrastructure provider for clusters managed by the
// Cluster API Hyper-V provider (luthermonson/cluster-api-provider-hyperv). It
// boots the bootstrap node as a single Generation 2 Hyper-V VM from a
// cloud-init-ready Linux VHDX with a NoCloud cidata ISO attached, validates
// (but never allocates) the control plane endpoint, renders the HyperVMachine
// that adopts the running VM, and tears everything down by name and Notes tag.
//
// # klotho runs on the host
//
// klotho the CLI runs on the Windows Hyper-V host, so this adapter shells to
// local powershell.exe to create and manage the bootstrap VM. That is enough
// to stand up node zero. A fully self-managing cluster additionally needs the
// in-cluster Cluster API Hyper-V manager to reach Hyper-V to create the rest
// of the machines; because that manager runs as a Linux pod inside the
// cluster, it must reach the host over the network (WinRM / PowerShell
// remoting), which is the CAPI provider repo's concern, not this adapter's.
//
// # Name conventions (the contract Adopt, Plan and Teardown rely on)
//
// Nothing is recorded by VM GUID in state as the lookup key; these conventions
// are the contract, and they match the Cluster API Hyper-V provider's
// vmservice so it adopts what klotho creates:
//
//   - the bootstrap VM is named VMName(cluster), "<cluster>-bootstrap", which
//     is also the adopted HyperVMachine's spec.vmName (the provider looks a VM
//     up by spec.vmName, then the owning Machine name, then the HyperVMachine
//     name, and adopts an existing one instead of creating it);
//   - every VM this provider creates carries its cluster tag, Tag(cluster)
//     ("klotho-<cluster>"), in the VM's Notes, so Plan and Teardown can tell
//     klotho's VMs from others on the same host;
//   - the bootstrap VM's NoCloud ISO is ISOName(cluster),
//     "<cluster>-bootstrap-cidata.iso", under the configured ISO directory.
package hyperv

import (
	"fmt"
	"strings"

	"github.com/klotho-k8s/klotho/provider/infrastructure"
)

// ProviderName is the provider's identifier, as used by --provider, in state,
// and as the key into config.providers.
const ProviderName = "hyperv"

// TagPrefix starts the Notes tag every VM of a klotho cluster carries.
const TagPrefix = "klotho-"

// Defaults applied when a Config field and the matching template variable are
// both unset.
const (
	// DefaultSwitch is the Hyper-V virtual switch the bootstrap VM joins. The
	// built-in "Default Switch" is an internal NAT switch that hands out
	// addresses and reaches the internet, which suits a dev cluster.
	DefaultSwitch = "Default Switch"
	// DefaultPort is the control plane port.
	DefaultPort = 6443
	// DefaultMemoryMiB and DefaultCPUs size the bootstrap VM when neither the
	// config nor the template set them.
	DefaultMemoryMiB = 4096
	DefaultCPUs      = 2
)

// ProviderIDPrefix is the Hyper-V provider-ID scheme; it matches the Cluster
// API Hyper-V provider's infrav1.ProviderIDPrefix so the provider IDs agree.
const ProviderIDPrefix = "hyperv://"

// Template variables, matching the names the Cluster API Hyper-V provider's
// cluster template uses.
const (
	VarEndpointHost = "CONTROL_PLANE_ENDPOINT_HOST"
	VarImagePath    = "HYPERV_IMAGE_PATH"
	VarSwitchName   = "HYPERV_SWITCH_NAME"
)

var (
	_ infrastructure.Provider        = (*Provider)(nil)
	_ infrastructure.DeletionSidecar = (*Provider)(nil)
)

// Config configures the provider. A zero field falls back to the cluster's
// template variables and then to a default.
type Config struct {
	// Switch is the virtual switch the bootstrap VM joins. Empty means the
	// HYPERV_SWITCH_NAME variable, then DefaultSwitch.
	Switch string
	// Image is the path to the cloud-init-ready base Linux VHDX the bootstrap
	// VM's disk is copied from. Empty means the HYPERV_IMAGE_PATH variable.
	// Required, since Hyper-V cannot create a bootable VM without a disk.
	Image string
	// VMPath is the directory VM disks and the cidata ISO are written under.
	// Empty means the host's default virtual-hard-disk path.
	VMPath string
	// ISODir is where the NoCloud cidata ISO is written. Empty means VMPath.
	ISODir string
	// MemoryMiB and CPUs size the bootstrap VM. Zero means DefaultMemoryMiB
	// and DefaultCPUs.
	MemoryMiB int
	CPUs      int
	// EndpointHost is the control plane endpoint address (a static VIP, since
	// Hyper-V has no load balancer). Empty means the CONTROL_PLANE_ENDPOINT_HOST
	// variable; Prepare fails if neither is set.
	EndpointHost string
	// SecureBoot enables UEFI Secure Boot. Default false, because the stock
	// Linux VHDX images are not signed for the Microsoft UEFI CA.
	SecureBoot bool
}

// Provider implements infrastructure.Provider for Hyper-V.
type Provider struct {
	client Client
	cfg    Config
}

// New returns a provider that drives Hyper-V through client. When client is
// nil a powershell-backed client is used (NewClient).
func New(client Client, cfg Config) *Provider {
	if client == nil {
		client = NewClient()
	}
	return &Provider{client: client, cfg: cfg}
}

// Name implements infrastructure.Provider.
func (p *Provider) Name() string { return ProviderName }

// UntaggedMachineTemplate reports that HyperVMachineTemplate has no tag field,
// so klotho must not add spec.template.spec.tags to the generated bundle: the
// provider's strict v1alpha1 CRD schema rejects the unknown field on apply (and
// the adopted HyperVMachine, built from the template's spec, would carry it
// too). Hyper-V VMs are correlated for teardown by name prefix and Notes tag.
func (p *Provider) UntaggedMachineTemplate() bool { return true }

// Tag returns the Notes tag every VM of cluster carries.
func Tag(cluster string) string { return TagPrefix + strings.ToLower(cluster) }

// VMName returns the bootstrap VM's name, which is also the adopted
// HyperVMachine's spec.vmName.
func VMName(cluster string) string { return cluster + "-bootstrap" }

// ISOName returns the file name of the bootstrap VM's NoCloud ISO.
func ISOName(cluster string) string { return cluster + "-bootstrap-cidata.iso" }

func errorf(format string, args ...any) error {
	return fmt.Errorf("hyperv provider: "+format, args...)
}

// resolved gathers the config-or-variable-or-default values a run needs.
type resolved struct {
	Switch       string
	Image        string
	VMPath       string
	ISODir       string
	MemoryMiB    int
	CPUs         int
	EndpointHost string
}

// resolve fills a resolved from the config, the spec's variables, and
// defaults, in that order of precedence.
func (p *Provider) resolve(vars map[string]string) resolved {
	get := func(cfg, varName string) string {
		if cfg != "" {
			return cfg
		}
		return vars[varName]
	}
	r := resolved{
		Switch:       get(p.cfg.Switch, VarSwitchName),
		Image:        get(p.cfg.Image, VarImagePath),
		VMPath:       p.cfg.VMPath,
		ISODir:       p.cfg.ISODir,
		MemoryMiB:    p.cfg.MemoryMiB,
		CPUs:         p.cfg.CPUs,
		EndpointHost: get(p.cfg.EndpointHost, VarEndpointHost),
	}
	if r.Switch == "" {
		r.Switch = DefaultSwitch
	}
	if r.MemoryMiB == 0 {
		r.MemoryMiB = DefaultMemoryMiB
	}
	if r.CPUs == 0 {
		r.CPUs = DefaultCPUs
	}
	if r.ISODir == "" {
		r.ISODir = r.VMPath
	}
	return r
}
