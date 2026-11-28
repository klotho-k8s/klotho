// Package vsphere is the infrastructure provider for clusters managed by the
// upstream Cluster API provider for vSphere (CAPV), the govmomi-based variant
// (kubernetes-sigs/cluster-api-provider-vsphere), not the supervisor /
// vm-operator one. It boots the bootstrap node by cloning a VM template through
// the vCenter API with govmomi, delivers klotho's bootstrap user-data through
// the VMware guestinfo datasource, validates (but never allocates) the control
// plane VIP, renders the VSphereMachine that adopts the running VM, and tears
// everything down from the host through the same vCenter API.
//
// # klotho reaches vCenter directly
//
// vCenter exposes a remote SOAP/REST API, so klotho the CLI drives the
// bootstrap VM by connecting straight to it with the official Go SDK
// github.com/vmware/govmomi. There is no custom host agent and, because vCenter
// is reachable from outside the cluster (like Proxmox's API, and unlike a cloud
// where the control plane endpoint is a managed load balancer), no deletion
// sidecar is needed: klotho's own Teardown powers off and deletes the VMs from
// the host, so the fire-and-forget delete simply returns. The in-cluster CAPV
// controller reaches the same vCenter (its per-cluster identity Secret) to
// reconcile any further machines.
//
// # Node-zero adoption (by providerID, CAPL-style — VERIFY live)
//
// CAPV has no field to point a VSphereMachine/VSphereVM at a pre-existing VM by
// name: its only native create path is clone-from-template, and it identifies
// the VM it created by the BIOS UUID vCenter assigns (VSphereVM.status.biosUUID
// -> VSphereMachine.spec.providerID "vsphere://<bios-uuid>"). klotho therefore
// adopts node zero the way the Linode provider does, by pre-setting the
// VSphereMachine's spec.providerID to the running VM's "vsphere://<uuid>" so the
// controller reconciles the existing VM rather than cloning a duplicate. This is
// the CAPL model, the opposite of CAPG/CAPD name-based adoption; because it is
// not an officially documented "adopt existing VM" path, it MUST be confirmed
// against a live vCenter (see TESTING.md) — the provider lands build+unit green
// and help-wanted for a live run.
//
// # Name conventions (the contract Adopt, Plan and Teardown rely on)
//
// Nothing this provider creates is recorded by managed-object reference in state
// as the lookup key; these conventions are the contract:
//
//   - the bootstrap VM is named VMName(cluster), "<cluster>-bootstrap", which is
//     also the adopted VSphereMachine's name;
//   - every VM this provider creates carries the cluster tag Tag(cluster),
//     "klotho-<cluster>", in its vSphere annotation (notes), so Plan and
//     Teardown can tell klotho's VMs from others in the same folder;
//   - the BIOS UUID is not stored as the lookup key: Adopt and Plan look the VM
//     up again by name.
//
// # Provider ID and the endpoint
//
// A machine's provider ID is "vsphere://<bios-uuid>", the VM's SMBIOS/BIOS UUID
// (govmomi config.uuid), matching CAPV's providerID contract. klotho seeds the
// guestinfo metadata instance-id with that UUID, so the kubelet --provider-id
// "vsphere://{{ ds.meta_data.instance_id }}" resolves to the same ID the adopted
// VSphereMachine carries and the Machine<->Node nodeRef completes without a
// cloud controller manager (the flat single-node template self-registers the
// provider ID, so it needs neither the vSphere CPI nor CSI).
//
// vSphere has no cloud load balancer, so the control plane endpoint is a static
// VIP the user supplies (vsphere.endpointIP or the CONTROL_PLANE_ENDPOINT_IP
// variable), realized by a kube-vip static pod in the control plane's kubeadm
// configuration. klotho does not allocate it; Prepare only validates it.
package vsphere

import (
	"fmt"
	"strings"

	"github.com/klotho-k8s/klotho/provider"
	"github.com/klotho-k8s/klotho/provider/infrastructure"
)

// ProviderName is the provider's identifier, as clusterctl knows CAPV and as
// --provider, state and the key into config.providers use it.
const ProviderName = "vsphere"

// TagPrefix starts the annotation tag every VM of a klotho cluster carries.
const TagPrefix = "klotho-"

// ProviderIDPrefix is the CAPV provider-ID scheme; a full provider ID is
// "vsphere://<bios-uuid>".
const ProviderIDPrefix = "vsphere://"

// Clone modes, matching CAPV's VirtualMachineCloneSpec.cloneMode values.
const (
	CloneModeFull   = "fullClone"
	CloneModeLinked = "linkedClone"
)

// Defaults applied when a Config field and the matching template variable are
// both unset.
const (
	// DefaultPort is the control plane port.
	DefaultPort = 6443
	// DefaultCloneMode is a full clone: it needs no template snapshot and works
	// on any datastore, which suits a one-off bootstrap VM.
	DefaultCloneMode = CloneModeFull
	// DefaultNumCPUs and DefaultMemoryMiB size the bootstrap VM when neither the
	// config nor the template set them. Two vCPUs clears kubeadm's control-plane
	// preflight.
	DefaultNumCPUs   = 2
	DefaultMemoryMiB = 8192
	DefaultDiskGiB   = 0 // 0 inherits the template's disk size
)

// Template variables, matching the names the upstream CAPV cluster template
// uses. Prepare fills them from Config so a generated bundle resolves to the
// same values this provider boots with.
const (
	VarServer       = "VSPHERE_SERVER"
	VarThumbprint   = "VSPHERE_TLS_THUMBPRINT"
	VarDatacenter   = "VSPHERE_DATACENTER"
	VarDatastore    = "VSPHERE_DATASTORE"
	VarFolder       = "VSPHERE_FOLDER"
	VarResourcePool = "VSPHERE_RESOURCE_POOL"
	VarNetwork      = "VSPHERE_NETWORK"
	VarTemplate     = "VSPHERE_TEMPLATE"
	VarUsername     = "VSPHERE_USERNAME"
	VarPassword     = "VSPHERE_PASSWORD"
	VarSSHKey       = "VSPHERE_SSH_AUTHORIZED_KEY"
	VarEndpointIP   = "CONTROL_PLANE_ENDPOINT_IP"
	VarEndpointPort = "CONTROL_PLANE_ENDPOINT_PORT"
	VarVIPInterface = "VIP_NETWORK_INTERFACE"
)

var _ infrastructure.Provider = (*Provider)(nil)

// Config configures the provider. A zero field falls back to the cluster's
// template variables and then to a default; see resolve.
type Config struct {
	// Server is the vCenter host or URL klotho dials, for example
	// "vcenter.example.com" or "https://vcenter.example.com/sdk". Required.
	Server string
	// Username and Password authenticate the host-side govmomi session. They are
	// also handed to the generated bundle (VSPHERE_USERNAME/VSPHERE_PASSWORD) for
	// the per-cluster identity Secret the in-cluster CAPV controller uses.
	Username string
	Password string
	// Thumbprint is the colon-separated SHA-1 of the vCenter host certificate
	// (CAPV's VSPHERE_TLS_THUMBPRINT). Empty with Insecure true skips
	// verification; empty without Insecure verifies against the system roots.
	Thumbprint string
	// Insecure skips vCenter TLS verification for klotho's own session. The
	// in-cluster controller still uses Thumbprint.
	Insecure bool

	// Datacenter is the vSphere datacenter the VM is placed in. Empty means the
	// VSPHERE_DATACENTER variable; required when the vCenter has more than one.
	Datacenter string
	// Datastore receives the clone. Empty means the VSPHERE_DATASTORE variable.
	Datastore string
	// Folder is the VM folder the clone is created in. Empty means the
	// VSPHERE_FOLDER variable, then the datacenter's default vm folder.
	Folder string
	// ResourcePool is the resource pool the clone runs in. Empty means the
	// VSPHERE_RESOURCE_POOL variable, then the datacenter's default.
	ResourcePool string
	// Network is the vSphere network the VM's NIC attaches to. Empty means the
	// VSPHERE_NETWORK variable.
	Network string
	// Template is the name (or inventory path) of the VM template to clone. Empty
	// means the VSPHERE_TEMPLATE variable. Required; the provider never builds it.
	Template string
	// CloneMode is "fullClone" (the default) or "linkedClone". A linked clone
	// needs the template to carry a snapshot.
	CloneMode string

	// NumCPUs and MemoryMiB size the bootstrap VM. Zero means DefaultNumCPUs and
	// DefaultMemoryMiB.
	NumCPUs   int
	MemoryMiB int
	// DiskGiB optionally grows the clone's primary disk. Zero inherits the
	// template's disk size.
	DiskGiB int

	// EndpointIP is the static control plane VIP (vSphere has no load balancer).
	// Empty means the CONTROL_PLANE_ENDPOINT_IP variable; Prepare fails if
	// neither is set.
	EndpointIP string
	// Port is the API server port. Zero means DefaultPort.
	Port int
	// VIPInterface names the NIC kube-vip binds the VIP on. Empty lets kube-vip
	// auto-detect (VIP_NETWORK_INTERFACE="").
	VIPInterface string
	// SSHAuthorizedKey is installed for the template's default user so a failed
	// bootstrap can be diagnosed (VSPHERE_SSH_AUTHORIZED_KEY). Optional.
	SSHAuthorizedKey string
}

// Provider implements infrastructure.Provider for vSphere via CAPV.
type Provider struct {
	client Client
	cfg    Config
}

// New returns a provider that drives vCenter through client. When client is nil
// a govmomi-backed client is built from cfg (newLazyClient); its session is
// opened lazily on the first call, so building the provider and read-only
// commands that never reach vCenter do not require a reachable host.
func New(client Client, cfg Config) *Provider {
	if client == nil {
		client = newLazyClient(cfg)
	}
	return &Provider{client: client, cfg: cfg}
}

// Name implements infrastructure.Provider.
func (p *Provider) Name() string { return ProviderName }

// Dependencies implements provider.Dependent: the flat single-node template
// self-registers each node's provider ID, so CAPV needs no in-cluster
// dependency (no vSphere CPI/CSI, unlike the upstream multi-node template).
func (p *Provider) Dependencies() []provider.Dependency { return nil }

// Tag returns the annotation tag every VM of cluster carries.
func Tag(cluster string) string { return TagPrefix + strings.ToLower(cluster) }

// VMName returns the bootstrap VM's name, which is also the adopted
// VSphereMachine's name.
func VMName(cluster string) string { return cluster + "-bootstrap" }

// ProviderID returns the full provider ID for a BIOS UUID.
func ProviderID(uuid string) string { return ProviderIDPrefix + strings.ToLower(uuid) }

func errorf(format string, args ...any) error {
	return fmt.Errorf("vsphere provider: "+format, args...)
}

// resolved gathers the config-or-variable-or-default values a run needs. It
// makes no API calls.
type resolved struct {
	Server       string
	Username     string
	Password     string
	Thumbprint   string
	Insecure     bool
	Datacenter   string
	Datastore    string
	Folder       string
	ResourcePool string
	Network      string
	Template     string
	CloneMode    string
	NumCPUs      int
	MemoryMiB    int
	DiskGiB      int
	EndpointIP   string
	Port         int
	VIPInterface string
	SSHKey       string
}

// resolve fills a resolved from the config, the spec's variables, and defaults,
// in that order of precedence.
func (p *Provider) resolve(vars map[string]string) resolved {
	get := func(cfg, varName string) string {
		if cfg != "" {
			return cfg
		}
		return strings.TrimSpace(vars[varName])
	}
	r := resolved{
		Server:       get(p.cfg.Server, VarServer),
		Username:     get(p.cfg.Username, VarUsername),
		Password:     get(p.cfg.Password, VarPassword),
		Thumbprint:   get(p.cfg.Thumbprint, VarThumbprint),
		Insecure:     p.cfg.Insecure,
		Datacenter:   get(p.cfg.Datacenter, VarDatacenter),
		Datastore:    get(p.cfg.Datastore, VarDatastore),
		Folder:       get(p.cfg.Folder, VarFolder),
		ResourcePool: get(p.cfg.ResourcePool, VarResourcePool),
		Network:      get(p.cfg.Network, VarNetwork),
		Template:     get(p.cfg.Template, VarTemplate),
		CloneMode:    p.cfg.CloneMode,
		NumCPUs:      p.cfg.NumCPUs,
		MemoryMiB:    p.cfg.MemoryMiB,
		DiskGiB:      p.cfg.DiskGiB,
		EndpointIP:   get(p.cfg.EndpointIP, VarEndpointIP),
		Port:         p.cfg.Port,
		VIPInterface: get(p.cfg.VIPInterface, VarVIPInterface),
		SSHKey:       get(p.cfg.SSHAuthorizedKey, VarSSHKey),
	}
	if r.CloneMode == "" {
		r.CloneMode = DefaultCloneMode
	}
	if r.NumCPUs == 0 {
		r.NumCPUs = DefaultNumCPUs
	}
	if r.MemoryMiB == 0 {
		r.MemoryMiB = DefaultMemoryMiB
	}
	if r.Port == 0 {
		r.Port = DefaultPort
	}
	return r
}
