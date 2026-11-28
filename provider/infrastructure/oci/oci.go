// Package oci is the infrastructure provider for clusters managed by the
// Cluster API Provider for Oracle Cloud Infrastructure (CAPOCI, upstream
// oracle/cluster-api-provider-oci). It boots the bootstrap node as a single
// OCI compute instance with klotho's cloud-init delivered through the instance
// metadata "user_data" key, resolves the control plane endpoint, renders the
// OCIMachine that adopts the running instance, and tears everything down by
// klotho's ownership freeform tag.
//
// # Name and tag conventions
//
// The following conventions are the contract Launch, Adopt, Plan and Teardown
// rely on:
//
//   - the bootstrap instance's OCI display name is MachineName(cluster)
//     ("<cluster>-bootstrap"), which is also the adopted OCIMachine's name and
//     the kubeadm control plane provider's BootstrapMachineName. CAPOCI resolves
//     a machine's instance by listing compartment instances filtered to that
//     display name, so matching the two is what lets CAPOCI adopt the running
//     instance instead of launching a second one;
//   - every instance it creates carries the two freeform tags CAPOCI uses to
//     recognise its own resources — CreatedBy=OCIClusterAPIProvider and
//     ClusterResourceIdentifier=ResourceIdentifier(cluster) — plus klotho's own
//     ownership tag ClusterTagKey=Tag(cluster), which Plan and Teardown find it
//     by;
//   - the instance's providerID is "oci://<instance-ocid>", which CAPOCI sets
//     itself from the adopted instance.
//
// # CAPOCI adoption (why the display name and freeform tags above)
//
// CAPOCI adopts a pre-booted instance by DISPLAY NAME, cross-checked by its own
// ownership freeform tags. Its MachineScope.GetMachine falls back to
// GetMachineByDisplayName, which calls core.ListInstances(compartmentId,
// displayName=OCIMachine.Name) and, for each returned instance, accepts it only
// when IsResourceCreatedByClusterAPI(instance.FreeformTags) is true — that is,
// when the instance carries CreatedBy=OCIClusterAPIProvider and
// ClusterResourceIdentifier equal to OCICluster.spec.ociResourceIdentifier. The
// OCIMachine's spec.providerID is an OUTPUT (CAPOCI sets it from the found
// instance's OCID); klotho does NOT pre-set it. This is the GCP/CAPD model, not
// the Linode/CAPL pre-set-providerID model.
//
// Two things make the adoption deterministic before boot:
//
//   - the flat cluster-template.yaml pins OCICluster.spec.ociResourceIdentifier
//     to "${CLUSTER_NAME}" (the field is preserve-if-set: CAPOCI's defaulting
//     webhook fills it with a UUID only when empty), so klotho knows the
//     ClusterResourceIdentifier tag value before the node boots;
//   - Launch stamps that tag, the CreatedBy tag and the display name on the
//     instance, so CAPOCI's display-name lookup finds and adopts it.
//
// This is implemented from CAPOCI's cloud/scope/machine.go and
// cloud/ociutil/ociutil.go (api v1beta2) and MUST be confirmed against the
// pinned CAPOCI version and a live run (see the package tests and TESTING.md).
//
// # Endpoint / load balancer
//
// Stock CAPOCI always fronts the API server with an OCI load balancer (a Network
// Load Balancer by default) and populates OCICluster.spec.controlPlaneEndpoint
// from it. klotho mints the cluster certificates and node zero's cloud-init
// (which carry the endpoint as the kubeadm controlPlaneEndpoint and a cert SAN)
// before Launch, so the endpoint must be known up front. The provider therefore
// requires a user-supplied ControlPlaneEndpoint (an address the control-plane
// LB will answer on, or node zero's own reserved public IP for a single-node
// setup) and uses it as-is; it allocates nothing. See prepare.go and TESTING.md.
package oci

import (
	"context"
	"fmt"
	"strings"
	"time"

	"github.com/oracle/oci-go-sdk/v65/core"

	"github.com/klotho-k8s/klotho/provider"
	"github.com/klotho-k8s/klotho/provider/infrastructure"
)

// ProviderName is the provider's identifier, as clusterctl knows CAPOCI and as
// --provider and state use it.
const ProviderName = "oci"

// ClusterTagKey is klotho's own ownership freeform-tag key; its value is the
// cluster name (lower-cased). Every instance klotho creates for a cluster
// carries it so Plan and Teardown can find it.
const ClusterTagKey = "klotho.io/cluster"

// CAPOCI's own ownership freeform tags, from cloud/ociutil/ociutil.go. An
// instance must carry both for CAPOCI's IsResourceCreatedByClusterAPI to accept
// it as its own and adopt it by display name instead of launching a duplicate.
const (
	CreatedByTagKey                 = "CreatedBy"
	CreatedByTagValue               = "OCIClusterAPIProvider"
	ClusterResourceIdentifierTagKey = "ClusterResourceIdentifier"
)

// ProviderIDPrefix is the scheme CAPOCI uses for an OCI instance's provider ID:
// "oci://<instance-ocid>".
const ProviderIDPrefix = "oci://"

// Defaults applied by the provider.
const (
	DefaultPort = 6443
	// DefaultShape is a flexible AMD shape; klotho sizes it to clear kubeadm's
	// two-CPU control-plane preflight via DefaultOCPUs. VM.Standard.E4.Flex is
	// broadly available; it is also eligible as a paid flex shape. For the OCI
	// Always Free tier use VM.Standard.A1.Flex (Ampere/arm64) instead and an
	// arm64 image (set oci.shape and oci.imageId).
	DefaultShape = "VM.Standard.E4.Flex"
	// DefaultOCPUs and DefaultMemoryGBs size a flex shape. Two OCPUs clear
	// kubeadm's two-CPU control-plane preflight.
	DefaultOCPUs     float32 = 2
	DefaultMemoryGBs float32 = 16
	// DefaultBootVolumeGBs is the boot volume size; OCI's minimum is 50 GB.
	DefaultBootVolumeGBs int64 = 50
	DefaultLaunchPoll          = 5 * time.Second
	DefaultLaunchWait          = 5 * time.Minute
)

// Template variables read into the generated bundle. They are the names the
// flat cluster-template.yaml uses; Prepare fills them from Config.
const (
	VarCompartmentID     = "OCI_COMPARTMENT_ID"
	VarResourceID        = "OCI_RESOURCE_IDENTIFIER"
	VarVCNID             = "OCI_VCN_ID"
	VarSubnetID          = "OCI_SUBNET_ID"
	VarImageID           = "OCI_IMAGE_ID"
	VarControlPlaneShape = "OCI_CONTROL_PLANE_MACHINE_TYPE"
	VarNodeShape         = "OCI_NODE_MACHINE_TYPE"
	VarOCPUs             = "OCI_MACHINE_TYPE_OCPUS"
	VarMemoryGBs         = "OCI_MACHINE_TYPE_MEMORY_GBS"
	VarControlPlaneCount = "CONTROL_PLANE_MACHINE_COUNT"
)

var _ infrastructure.Provider = (*Provider)(nil)
var _ provider.Checker = (*Provider)(nil)

// API is the subset of the OCI Go SDK (github.com/oracle/oci-go-sdk/v65) klotho
// uses, reduced to plain request/response methods so it can be faked in tests
// (the SDK's clients use request/response wrapper structs that do not fake
// cleanly). client, built by NewClient, wraps the compute, network and identity
// clients to satisfy it.
type API interface {
	// LaunchInstance creates a compute instance and returns it.
	LaunchInstance(ctx context.Context, details core.LaunchInstanceDetails) (core.Instance, error)
	// GetInstance returns one instance by its OCID.
	GetInstance(ctx context.Context, instanceID string) (core.Instance, error)
	// ListInstances returns every instance in a compartment (the caller filters
	// by freeform tag; the compute API cannot filter by tag server-side).
	ListInstances(ctx context.Context, compartmentID string) ([]core.Instance, error)
	// TerminateInstance deletes one instance by its OCID (boot volume included).
	TerminateInstance(ctx context.Context, instanceID string) error
	// PrimaryVnicAddresses returns the instance's primary VNIC public and
	// private IPv4 addresses (public first), best-effort for diagnostics.
	PrimaryVnicAddresses(ctx context.Context, compartmentID, instanceID string) ([]string, error)
	// ListImages returns the images available in a compartment.
	ListImages(ctx context.Context, compartmentID string) ([]core.Image, error)
	// ListShapes returns the compute shapes available in a compartment.
	ListShapes(ctx context.Context, compartmentID string) ([]core.Shape, error)
	// ListAvailabilityDomains returns the tenancy's availability domains.
	ListAvailabilityDomains(ctx context.Context, compartmentID string) ([]AvailabilityDomain, error)
}

// AvailabilityDomain is the subset of an OCI availability domain klotho uses,
// decoupled from the identity SDK type so the API interface stays in one
// package.
type AvailabilityDomain struct {
	Name string
	ID   string
}

// Config configures the provider. Zero fields fall back to defaults; see the
// accessors.
type Config struct {
	// CompartmentID is the OCID of the compartment the cluster runs in.
	// Required.
	CompartmentID string
	// Region is the OCI region identifier, e.g. "us-ashburn-1". Required.
	Region string
	// AvailabilityDomain is the AD the bootstrap instance is placed in, e.g.
	// "Uocm:US-ASHBURN-AD-1". Required.
	AvailabilityDomain string
	// VCNID is the OCID of the existing VCN the cluster's network lives in.
	// Required: klotho boots node zero into an existing subnet, and the flat
	// template points CAPOCI at the same VCN/subnet (networkSpec with
	// skipNetworkManagement) so it adopts node zero in place instead of creating
	// a new VCN that would strand it.
	VCNID string
	// SubnetID is the OCID of the subnet the bootstrap instance's primary VNIC
	// joins. Required. A public subnet (with AssignPublicIP) is the simplest
	// path for a node that must reach the internet and serve as the endpoint.
	SubnetID string
	// NSGIDs are OCIDs of network security groups attached to the primary VNIC.
	// Optional.
	NSGIDs []string

	// ImageID is the OCID of the boot image. Required: OCI images are
	// region/tenancy-specific OCIDs, so there is no sane built-in default. Use a
	// plain Ubuntu image; it carries no Kubernetes, so a cluster booted from it
	// needs klotho's install-at-boot (nodes.installKubernetes).
	ImageID string
	// Shape sizes the bootstrap instance (and the template's machines). Empty
	// means DefaultShape.
	Shape string
	// OCPUs and MemoryGBs size a flexible shape. Zero means DefaultOCPUs /
	// DefaultMemoryGBs. They are ignored for fixed (non-Flex) shapes.
	OCPUs     float32
	MemoryGBs float32
	// BootVolumeGBs sizes the boot volume. Zero means DefaultBootVolumeGBs
	// (OCI's 50 GB minimum).
	BootVolumeGBs int64

	// AssignPublicIP gives the bootstrap instance's primary VNIC a public IP, so
	// it can reach the internet (to pull images) and serve as the control plane
	// endpoint. Default true; set false only with a NAT gateway and a supplied
	// ControlPlaneEndpoint reachable another way.
	AssignPublicIP bool

	// SSHAuthorizedKeys are public keys installed via the instance metadata
	// "ssh_authorized_keys" key (and the template's), so a failed bootstrap can
	// be diagnosed.
	SSHAuthorizedKeys []string

	// FreeformTags are extra OCI freeform tags added to the bootstrap instance
	// alongside klotho's and CAPOCI's ownership tags.
	FreeformTags map[string]string

	// ResourceIdentifier is the value pinned into
	// OCICluster.spec.ociResourceIdentifier and the instance's
	// ClusterResourceIdentifier freeform tag. Empty means the cluster name, which
	// the flat template also pins ("${CLUSTER_NAME}").
	ResourceIdentifier string

	// ControlPlaneEndpoint is the API server host. Required: klotho mints the
	// certificates and node zero's cloud-init before the node boots, and stock
	// CAPOCI would otherwise only learn the endpoint after provisioning its load
	// balancer. Supply an address the control-plane LB answers on, or node zero's
	// own reserved public IP for a single-node setup.
	ControlPlaneEndpoint string
	// Port is the API server port. Zero means DefaultPort.
	Port int

	// --- authentication (resolved by NewClient, never placed in the bundle) ---

	// ConfigFile is the path to an OCI SDK config file. Empty means the SDK
	// default (~/.oci/config).
	ConfigFile string
	// Profile is the config-file profile to use. Empty means "DEFAULT".
	Profile string
	// UseInstancePrincipal authenticates with the instance principal of the host
	// klotho runs on (an OCI compute instance in a dynamic group) instead of a
	// config file. Mutually exclusive with ConfigFile; takes precedence.
	UseInstancePrincipal bool
}

// Provider implements infrastructure.Provider for OCI via CAPOCI.
type Provider struct {
	client API
	cfg    Config

	launchPoll time.Duration
	launchWait time.Duration
}

// New returns a provider talking to OCI through client, normally built by
// NewClient.
func New(client API, cfg Config) *Provider {
	return &Provider{client: client, cfg: cfg, launchPoll: DefaultLaunchPoll, launchWait: DefaultLaunchWait}
}

// Name implements infrastructure.Provider.
func (p *Provider) Name() string { return ProviderName }

// Tag returns klotho's ownership freeform-tag value for cluster (the cluster
// name, lower-cased). The tag key is ClusterTagKey.
func Tag(cluster string) string { return strings.ToLower(cluster) }

// MachineName is the bootstrap machine's name, shared by the adopted OCIMachine,
// the compute instance's display name, and the kubeadm control plane provider's
// BootstrapMachineName ("<cluster>-bootstrap"). Matching the display name to the
// OCIMachine name is what lets CAPOCI's display-name lookup adopt the running
// instance rather than launch a duplicate.
func MachineName(cluster string) string { return cluster + "-bootstrap" }

// Dependencies implements provider.Dependent: CAPOCI has no in-cluster
// dependency like the Proxmox provider's IPAM.
func (p *Provider) Dependencies() []provider.Dependency { return nil }

func (p *Provider) port() int {
	if p.cfg.Port > 0 {
		return p.cfg.Port
	}
	return DefaultPort
}

func (p *Provider) shape() string {
	if p.cfg.Shape != "" {
		return p.cfg.Shape
	}
	return DefaultShape
}

func (p *Provider) ocpus() float32 {
	if p.cfg.OCPUs > 0 {
		return p.cfg.OCPUs
	}
	return DefaultOCPUs
}

func (p *Provider) memoryGBs() float32 {
	if p.cfg.MemoryGBs > 0 {
		return p.cfg.MemoryGBs
	}
	return DefaultMemoryGBs
}

func (p *Provider) bootVolumeGBs() int64 {
	if p.cfg.BootVolumeGBs > 0 {
		return p.cfg.BootVolumeGBs
	}
	return DefaultBootVolumeGBs
}

// resourceIdentifier is the value pinned into the OCICluster and the instance's
// ClusterResourceIdentifier tag. It defaults to the cluster name, which the flat
// template also pins.
func (p *Provider) resourceIdentifier(cluster string) string {
	if p.cfg.ResourceIdentifier != "" {
		return p.cfg.ResourceIdentifier
	}
	return cluster
}

// providerID is the Kubernetes provider ID CAPOCI sets on an OCI instance:
// "oci://<instance-ocid>".
func providerID(ocid string) string { return ProviderIDPrefix + ocid }

// ocidFromProviderID returns the instance OCID carried in an "oci://<ocid>"
// provider ID, and whether it had the expected prefix.
func ocidFromProviderID(id string) (string, bool) {
	return strings.CutPrefix(id, ProviderIDPrefix)
}

// clusterFreeformTags are the freeform tags every bootstrap instance carries:
// CAPOCI's two ownership tags (so it adopts the instance by display name) plus
// klotho's ownership tag (so Plan/Teardown find it), plus any extra configured
// tags.
func (p *Provider) clusterFreeformTags(cluster string) map[string]string {
	tags := map[string]string{
		CreatedByTagKey:                 CreatedByTagValue,
		ClusterResourceIdentifierTagKey: p.resourceIdentifier(cluster),
		ClusterTagKey:                   Tag(cluster),
	}
	for k, v := range p.cfg.FreeformTags {
		tags[k] = v
	}
	return tags
}

// hasClusterTag reports whether tags carry the cluster's klotho ownership tag.
func hasClusterTag(tags map[string]string, cluster string) bool {
	return tags[ClusterTagKey] == Tag(cluster)
}

func errorf(format string, args ...any) error {
	return fmt.Errorf("oci provider: "+format, args...)
}

// strVal returns the string a *string points at, or "" when it is nil.
func strVal(s *string) string {
	if s == nil {
		return ""
	}
	return *s
}
