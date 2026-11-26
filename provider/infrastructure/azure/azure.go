// Package azure is the infrastructure provider for clusters managed by the
// Cluster API Provider Azure (CAPZ). It boots the bootstrap node as a single
// Azure virtual machine with klotho's cloud-init delivered as the VM's
// customData, resolves the control plane endpoint, renders the AzureMachine
// that adopts the running VM, and tears everything down by tag.
//
// # Name and tag conventions
//
// klotho creates the bootstrap node's Azure resources itself (so the endpoint
// is known before the certificates are minted) and CAPZ adopts them. The
// following conventions are the contract Launch, Adopt, Plan and Teardown rely
// on:
//
//   - every resource it creates carries the tag ClusterTagKey / <cluster>
//     (klotho's own ownership tag, "klotho.io/cluster"), so Plan and Teardown
//     find them;
//   - the bootstrap VM's Azure resource name is MachineName(cluster)
//     ("<cluster>-bootstrap"), which is also the adopted AzureMachine's name and
//     the kubeadm bootstrap Machine's infrastructureRef name;
//   - the VM lives in the cluster's resource group (Config.ResourceGroup),
//     which is CAPZ's node resource group for an AzureCluster;
//   - the VM's Azure resource ID is recorded in state as the bootstrap node's
//     provider ID ("azure://" + the ARM resource ID), which Adopt sets on the
//     AzureMachine.
//
// # CAPZ adoption (why the name and provider ID above)
//
// CAPZ adopts a pre-booted VM, it does not duplicate it. Its AzureMachine
// controller reconciles a VM through virtualmachines.Service.Reconcile, which
// Gets the VM by (node resource group, resource name) before any create. The
// resource name is MachineScope.Name(): the VM name parsed from
// AzureMachine.spec.providerID when that is set, otherwise the AzureMachine's
// own name. When the Get finds a VM that is not in a Failed state, the VM
// spec's Parameters(existing) returns (nil, nil) — Azure's "no change" — so
// CAPZ issues no CreateOrUpdate and leaves the running VM untouched; it only
// reads the real VM, sets the provider ID from the actual resource ID, and
// records the addresses and VM state. So a VM that klotho booted, named
// MachineName(cluster) in the cluster resource group, is adopted.
//
// Setting the provider ID does NOT break adoption (this is the CAPL model, the
// opposite of CAPA): CAPZ uses it to locate the VM by name, and in fact refuses
// to recreate a VM whose provider ID is set but whose VM is missing ("do not
// recreate it as Machines are immutable"). Adopt therefore both names the
// AzureMachine MachineName(cluster) and sets spec.providerID to the running
// VM's resource ID, which is belt-and-braces: either path adopts the same VM.
//
// This is implemented from CAPZ's azure/scope/machine.go (Name/GetVMID,
// SetProviderID), azure/services/virtualmachines/virtualmachines.go (Reconcile)
// and spec.go (Parameters), and MUST be confirmed against the pinned CAPZ
// version and a live run (see the package tests and TESTING.md).
//
// # The API server load balancer (an Azure cost CAPZ cannot drop)
//
// Unlike CAPA (loadBalancerType: disabled) and CAPL (external, no
// NodeBalancer), CAPZ's AzureCluster reconciler ALWAYS runs the load balancer
// service and builds an API server load balancer from
// spec.networkSpec.apiServerLB; there is no "disabled". klotho does not depend
// on it for routing (node zero serves the API on its own address, which klotho
// sets as spec.controlPlaneEndpoint — CAPZ only fills that field when it is
// empty), so the flat template sets the API server LB to type Internal, which
// uses a private frontend IP and allocates no extra public IP. The internal LB
// resource itself is the one Azure cost this provider cannot avoid. See
// cluster-template.yaml and TESTING.md.
package azure

import (
	"context"
	"fmt"
	"strings"
	"time"

	"github.com/klotho-k8s/klotho/pkg/cluster"
	"github.com/klotho-k8s/klotho/provider"
	"github.com/klotho-k8s/klotho/provider/infrastructure"
)

// ProviderName is the provider's identifier, as clusterctl knows CAPZ and as
// --provider and state use it.
const ProviderName = "azure"

// ClusterTagKey is klotho's own ownership tag key; its value is the cluster
// name. Every Azure resource klotho creates for a cluster carries it.
const ClusterTagKey = "klotho.io/cluster"

// ProviderIDPrefix is the scheme CAPZ uses for an Azure VM's provider ID. The
// full provider ID is this prefix followed by the VM's ARM resource ID (which
// itself begins with "/subscriptions/..."), giving the familiar triple slash
// "azure:///subscriptions/.../virtualMachines/<name>".
const ProviderIDPrefix = "azure://"

// Defaults applied by the provider.
const (
	DefaultPort = 6443
	// DefaultVMSize is the smallest burstable size that still clears kubeadm's
	// two-CPU control-plane preflight: Standard_B2s (2 vCPU, 4 GB). The cheaper
	// B1s/B1ms (1 vCPU) fail that check.
	DefaultVMSize = "Standard_B2s"
	// DefaultAdminUsername is the Linux admin user Azure creates on the VM.
	// klotho reaches the node through cloud-init, not this account.
	DefaultAdminUsername = "klotho"
	DefaultLaunchPoll    = 5 * time.Second
	DefaultLaunchWait    = 5 * time.Minute
)

// Default bootstrap image: Canonical's Ubuntu 22.04 LTS (gen2) marketplace
// image. It carries no Kubernetes, so a cluster booted from it needs klotho's
// install-at-boot (nodes.installKubernetes / --install-kubernetes).
const (
	DefaultImagePublisher = "Canonical"
	DefaultImageOffer     = "0001-com-ubuntu-server-jammy"
	DefaultImageSKU       = "22_04-lts-gen2"
	DefaultImageVersion   = "latest"
)

// Template variables read into the generated bundle. They are the names the
// flat cluster-template.yaml uses; Prepare fills them from Config.
const (
	VarSubscriptionID     = "AZURE_SUBSCRIPTION_ID"
	VarLocation           = "AZURE_LOCATION"
	VarResourceGroup      = "AZURE_RESOURCE_GROUP"
	VarVNet               = "AZURE_VNET_NAME"
	VarSubnet             = "AZURE_SUBNET_NAME"
	VarControlPlaneVMSize = "AZURE_CONTROL_PLANE_MACHINE_TYPE"
	VarNodeVMSize         = "AZURE_NODE_MACHINE_TYPE"
	VarImagePublisher     = "AZURE_IMAGE_PUBLISHER"
	VarImageOffer         = "AZURE_IMAGE_OFFER"
	VarImageSKU           = "AZURE_IMAGE_SKU"
	VarImageVersion       = "AZURE_IMAGE_VERSION"
	VarControlPlaneCount  = "CONTROL_PLANE_MACHINE_COUNT"
	VarIdentityName       = "AZURE_CLUSTER_IDENTITY_NAME"
	VarClientID           = "AZURE_CLIENT_ID"
	VarTenantID           = "AZURE_TENANT_ID"
	VarClientSecretName   = "AZURE_CLIENT_SECRET_NAME"
	// VarClientSecretB64 is the base64-encoded service principal password the
	// bundle's Secret (referenced by the AzureClusterIdentity) carries. It is
	// used only at generation time, not persisted in klotho's state record.
	VarClientSecretB64 = "AZURE_CLIENT_SECRET_B64"
)

var _ infrastructure.Provider = (*Provider)(nil)
var _ provider.Checker = (*Provider)(nil)

// Config configures the provider. Zero fields fall back to defaults; see the
// accessors and Prepare.
type Config struct {
	// SubscriptionID is the Azure subscription the cluster lives in. Required.
	SubscriptionID string
	// TenantID, ClientID and ClientSecret are the service principal klotho
	// authenticates with and hands to the in-cluster CAPZ controller (through
	// the AzureClusterIdentity and its secret). TenantID/ClientID are required;
	// ClientSecret comes from the environment and is never written to disk.
	TenantID     string
	ClientID     string
	ClientSecret string

	// Location is the Azure region, e.g. "eastus". Required.
	Location string
	// ResourceGroup is an existing resource group the bootstrap VM is created
	// in; it is also CAPZ's node resource group for the cluster. Required (klotho
	// does not create resource groups, so nothing it does not own can leak).
	ResourceGroup string
	// VNet and Subnet are an existing virtual network and subnet in ResourceGroup
	// the bootstrap VM's NIC attaches to. Both required (klotho does not create a
	// network).
	VNet   string
	Subnet string

	// VMSize sizes the bootstrap VM (and the template's machines). Empty means
	// DefaultVMSize.
	VMSize string
	// Image overrides the bootstrap marketplace image. Empty means the default
	// Ubuntu 22.04 LTS gen2.
	Image ImageRef
	// AdminUsername is the Linux admin user Azure creates. Empty means
	// DefaultAdminUsername.
	AdminUsername string
	// SSHPublicKey is an OpenSSH public key installed for the admin user, so a
	// failed bootstrap can be diagnosed. When empty, Launch sets a strong random
	// admin password instead (Azure requires one credential or the other; klotho
	// reaches the node through cloud-init regardless).
	SSHPublicKey string
	// Spot requests the bootstrap VM (and, in the template, the machines) as Spot
	// instances. Development and test only: a reclaimed Spot VM is deallocated.
	Spot bool

	// ControlPlaneEndpoint is the API server host. Empty means node zero's own
	// public IP (allocated in Prepare) is used, which is the cheapest endpoint:
	// no public load balancer. A value here (an existing LB IP or a DNS name you
	// control) is validated and used as-is.
	ControlPlaneEndpoint string
	// Port is the API server port. Zero means DefaultPort.
	Port int
}

// ImageRef is a marketplace image reference.
type ImageRef struct {
	Publisher string
	Offer     string
	SKU       string
	Version   string
}

// Provider implements infrastructure.Provider for Azure via CAPZ.
type Provider struct {
	api API
	cfg Config

	launchPoll time.Duration
	launchWait time.Duration
}

// New returns a provider talking to Azure through api, normally built by
// NewClient.
func New(api API, cfg Config) *Provider {
	return &Provider{api: api, cfg: cfg, launchPoll: DefaultLaunchPoll, launchWait: DefaultLaunchWait}
}

// Name implements infrastructure.Provider.
func (p *Provider) Name() string { return ProviderName }

// Tag returns klotho's ownership tag value for cluster (the cluster name,
// lower-cased). The key is ClusterTagKey.
func Tag(cluster string) string { return strings.ToLower(cluster) }

// MachineName is the bootstrap machine's name, shared by the bootstrap VM's
// Azure resource name, the adopted AzureMachine, and the kubeadm control plane
// provider's BootstrapMachineName ("<cluster>-bootstrap"). Naming the VM the
// same as the AzureMachine is what lets CAPZ's get-by-name adopt the running VM
// instead of creating a duplicate.
func MachineName(cluster string) string { return cluster + "-bootstrap" }

// nicName is the bootstrap VM's NIC resource name.
func nicName(cluster string) string { return MachineName(cluster) + "-nic" }

// publicIPName is the bootstrap VM's public IP resource name.
func publicIPName(cluster string) string { return MachineName(cluster) + "-pip" }

// Dependencies implements provider.Dependent: CAPZ has no in-cluster dependency
// like the Proxmox provider's IPAM.
func (p *Provider) Dependencies() []provider.Dependency { return nil }

func (p *Provider) port() int {
	if p.cfg.Port > 0 {
		return p.cfg.Port
	}
	return DefaultPort
}

func (p *Provider) vmSize() string {
	if p.cfg.VMSize != "" {
		return p.cfg.VMSize
	}
	return DefaultVMSize
}

func (p *Provider) adminUsername() string {
	if p.cfg.AdminUsername != "" {
		return p.cfg.AdminUsername
	}
	return DefaultAdminUsername
}

func (p *Provider) image() ImageRef {
	img := p.cfg.Image
	if img.Publisher == "" {
		img.Publisher = DefaultImagePublisher
	}
	if img.Offer == "" {
		img.Offer = DefaultImageOffer
	}
	if img.SKU == "" {
		img.SKU = DefaultImageSKU
	}
	if img.Version == "" {
		img.Version = DefaultImageVersion
	}
	return img
}

// tags returns klotho's ownership tag for a cluster, as an Azure tag map.
func tags(cluster string) map[string]string {
	return map[string]string{ClusterTagKey: Tag(cluster)}
}

// providerID is the Kubernetes provider ID CAPZ uses for a VM: the scheme
// prefix followed by the VM's ARM resource ID.
func providerID(vmResourceID string) string {
	return ProviderIDPrefix + vmResourceID
}

func errorf(format string, args ...any) error {
	return fmt.Errorf("azure provider: "+format, args...)
}

// VM is a minimal view of an Azure virtual machine, returned by the client so
// tests need not depend on the Azure SDK types.
type VM struct {
	// Name is the VM's Azure resource name.
	Name string
	// ID is the VM's full ARM resource ID ("/subscriptions/.../virtualMachines/<name>").
	ID string
	// PrivateIPs and PublicIPs are the addresses discovered on the VM's NICs.
	PrivateIPs []string
	PublicIPs  []string
	// ProvisioningState is the Azure provisioning state, e.g. "Succeeded".
	ProvisioningState string
	// PowerState is the Azure power state when known, e.g. "running".
	PowerState string
}

// PublicIP is a minimal view of an Azure public IP address.
type PublicIP struct {
	Name    string
	ID      string
	Address string
}

// PublicIPParams are the inputs to EnsurePublicIP.
type PublicIPParams struct {
	ResourceGroup string
	Location      string
	Name          string
	Tags          map[string]string
}

// NICParams are the inputs to CreateNIC.
type NICParams struct {
	ResourceGroup string
	Location      string
	Name          string
	SubnetID      string
	// PublicIPID, when set, associates that public IP with the NIC's IP config.
	PublicIPID string
	Tags       map[string]string
}

// VMParams are the inputs to CreateVM.
type VMParams struct {
	ResourceGroup string
	Location      string
	Name          string
	Size          string
	AdminUsername string
	// SSHPublicKey, when set, installs key auth for the admin user; otherwise
	// AdminPassword is set.
	SSHPublicKey  string
	AdminPassword string
	Image         ImageRef
	NICID         string
	// CustomData is the raw cloud-init; the client base64-encodes it.
	CustomData []byte
	Spot       bool
	Tags       map[string]string
}

// API is the subset of Azure klotho uses, behind a small interface so it
// can be faked in tests. *Client (client.go) satisfies it with the Azure SDK.
type API interface {
	// SubnetID confirms the resource group, virtual network and subnet exist and
	// returns the subnet's ARM resource ID.
	SubnetID(ctx context.Context, resourceGroup, vnet, subnet string) (string, error)
	// EnsurePublicIP creates (or returns an existing) static Standard public IP
	// and returns its address and resource ID.
	EnsurePublicIP(ctx context.Context, p PublicIPParams) (PublicIP, error)
	// CreateNIC creates a NIC in the subnet, optionally associating a public IP,
	// and returns its ARM resource ID.
	CreateNIC(ctx context.Context, p NICParams) (string, error)
	// CreateVM creates the bootstrap VM and returns its resource view.
	CreateVM(ctx context.Context, p VMParams) (VM, error)
	// GetVM returns the VM by name, or (nil, nil) when it does not exist.
	GetVM(ctx context.Context, resourceGroup, name string) (*VM, error)
	// ListClusterResources lists the VMs, NICs and public IPs in the resource
	// group carrying the given ownership tag value, as teardown resources.
	ListClusterResources(ctx context.Context, resourceGroup, clusterTag string) ([]cluster.Resource, error)
	// DeleteVM, DeleteNIC and DeletePublicIP remove a resource by name; a
	// not-found resource is not an error.
	DeleteVM(ctx context.Context, resourceGroup, name string) error
	DeleteNIC(ctx context.Context, resourceGroup, name string) error
	DeletePublicIP(ctx context.Context, resourceGroup, name string) error
	// ListSubnets lists the subnets of a virtual network, for Discover.
	ListSubnets(ctx context.Context, resourceGroup, vnet string) ([]infrastructure.Item, error)
	// ListVirtualNetworks lists the virtual networks in a resource group, for
	// Discover.
	ListVirtualNetworks(ctx context.Context, resourceGroup string) ([]infrastructure.Item, error)
}
