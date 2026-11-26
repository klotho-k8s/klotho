// Package linode is the infrastructure provider for clusters managed by the
// Cluster API Provider Linode (CAPL). It boots the bootstrap node as a single
// Linode instance with klotho's cloud-init delivered through the Linode
// Metadata service, resolves the control plane endpoint, renders the
// LinodeMachine that adopts the running instance, and tears everything down by
// tag.
//
// # Name, label and tag conventions
//
// The following conventions are the contract Launch, Adopt, Plan and Teardown
// rely on:
//
//   - every instance it creates carries klotho's ownership tag Tag(cluster)
//     (the cluster name, lower-cased) so Plan and Teardown can find it;
//   - the bootstrap instance's Linode label is MachineName(cluster)
//     ("<cluster>-bootstrap"), which is also the adopted LinodeMachine's name
//     and the kubeadm bootstrap Machine's infrastructureRef name;
//   - the instance ID is recorded in state as the bootstrap node's provider ID
//     ("linode://<id>"), which Adopt sets on the LinodeMachine.
//
// # CAPL adoption (why the provider ID above)
//
// CAPL adopts a pre-booted instance by a pre-set LinodeMachine.spec.providerID.
// Its LinodeMachine controller routes a machine with no Status.InstanceState to
// reconcileCreate, which gates every create step — firewall, VPC, metadata and
// the instance create itself — on "machineScope.LinodeMachine.Spec.ProviderID
// == nil". With the provider ID already set, all of that is skipped: CAPL parses
// the instance ID from the provider ID and only runs the configure, boot and
// ready preflights against that existing instance. The configure step is a
// no-op unless the machine template asks for extra disks (ours does not), and
// the boot step is idempotent (it tolerates an "already booted" instance), so a
// correctly-identified running instance is adopted, never recreated.
//
// As a second line of defence, even CAPL's create path will not duplicate: a
// Linode instance label is unique per account, so if CAPL ever did try to
// create an instance whose label collides with the bootstrap instance's it gets
// a 400 "Label must be unique", looks the instance up by label and adopts it.
// Launch therefore labels the instance MachineName(cluster) — the same name the
// LinodeMachine will carry. This is implemented from CAPL's
// internal/controller/linodemachine_controller.go and _helpers.go (api
// v1alpha2) and MUST be confirmed against the pinned CAPL version and a live
// run (see the package tests and TESTING.md).
package linode

import (
	"context"
	"fmt"
	"strings"
	"time"

	"github.com/linode/linodego/v2"

	"github.com/klotho-k8s/klotho/provider"
	"github.com/klotho-k8s/klotho/provider/infrastructure"
)

// ProviderName is the provider's identifier, as clusterctl knows CAPL and as
// --provider and state use it.
const ProviderName = "linode"

// ClusterTagKey is klotho's own ownership tag key; its value is the cluster
// name. Every instance klotho creates for a cluster carries it. Linode tags are
// flat strings, so the tag is stored as "klotho.io/cluster:<cluster>".
const ClusterTagKey = "klotho.io/cluster"

// ProviderIDPrefix is the scheme CAPL uses for a Linode instance's provider ID.
const ProviderIDPrefix = "linode://"

// Defaults applied by the provider.
const (
	DefaultPort = 6443
	// DefaultType is the smallest instance type that still clears kubeadm's
	// two-CPU control-plane preflight: g6-standard-2 (2 vCPU, 4 GB). The
	// cheaper g6-standard-1 (1 vCPU) and g6-nanode-1 fail that check.
	DefaultType = "g6-standard-2"
	// DefaultImage is a plain Ubuntu 22.04 cloud image. It carries no
	// Kubernetes, so a cluster booted from it needs klotho's install-at-boot
	// (nodes.installKubernetes / --install-kubernetes).
	DefaultImage      = "linode/ubuntu22.04"
	DefaultLaunchPoll = 5 * time.Second
	DefaultLaunchWait = 5 * time.Minute
)

// Template variables read into the generated bundle. They are the names the
// flat cluster-template.yaml uses; Prepare fills them from Config.
const (
	VarRegion            = "LINODE_REGION"
	VarControlPlaneType  = "LINODE_CONTROL_PLANE_MACHINE_TYPE"
	VarNodeType          = "LINODE_MACHINE_TYPE"
	VarImage             = "LINODE_OS"
	VarControlPlaneCount = "CONTROL_PLANE_MACHINE_COUNT"
)

var _ infrastructure.Provider = (*Provider)(nil)
var _ provider.Checker = (*Provider)(nil)

// API is the subset of the linodego client klotho uses, so it can be
// faked in tests. *linodego.Client satisfies it.
type API interface {
	CreateInstance(ctx context.Context, opts linodego.InstanceCreateOptions) (*linodego.Instance, error)
	GetInstance(ctx context.Context, id int) (*linodego.Instance, error)
	ListInstances(ctx context.Context, opts *linodego.ListOptions) ([]linodego.Instance, error)
	DeleteInstance(ctx context.Context, id int) error
	GetRegion(ctx context.Context, id string) (*linodego.Region, error)
	GetType(ctx context.Context, id string) (*linodego.LinodeType, error)
	GetImage(ctx context.Context, id string) (*linodego.Image, error)
	ListRegions(ctx context.Context, opts *linodego.ListOptions) ([]linodego.Region, error)
	ListTypes(ctx context.Context, opts *linodego.ListOptions) ([]linodego.LinodeType, error)
	ListImages(ctx context.Context, opts *linodego.ListOptions) ([]linodego.Image, error)
}

// Config configures the provider. Zero fields fall back to defaults; see the
// accessors and Resolve.
type Config struct {
	// Token is the Linode API token. It is also what the in-cluster CAPL
	// controller and the cloud controller manager need; it comes from the
	// environment (LINODE_TOKEN) and is never written back to disk.
	Token string
	// Region is the Linode region, e.g. "us-ord". Required.
	Region string
	// Type sizes the bootstrap instance (and the template's machines). Empty
	// means DefaultType.
	Type string
	// Image is the bootstrap image. Empty means DefaultImage (plain Ubuntu).
	Image string
	// AuthorizedKeys are SSH public keys installed for the instance's root
	// user, so a failed bootstrap can be diagnosed.
	AuthorizedKeys []string
	// RootPass is the instance's root password. Linode requires one when an
	// image is deployed; Launch generates a strong random one when this is
	// empty, since klotho reaches the node through cloud-init, not the password.
	RootPass string
	// Tags are extra Linode tags added to the bootstrap instance alongside
	// klotho's ownership tag.
	Tags []string
	// FirewallID attaches an existing Linode Cloud Firewall to the bootstrap
	// instance. Zero attaches none.
	FirewallID int
	// ControlPlaneEndpoint is the API server host. Empty means node zero's own
	// public IP is used (the cheapest endpoint: no NodeBalancer). A value here
	// is validated and used as-is (an existing NodeBalancer IP or a DNS name).
	ControlPlaneEndpoint string
	// Port is the API server port. Zero means DefaultPort.
	Port int
}

// Provider implements infrastructure.Provider for Linode via CAPL.
type Provider struct {
	client API
	cfg    Config

	launchPoll time.Duration
	launchWait time.Duration
}

// New returns a provider talking to Linode through client, normally built by
// NewClient.
func New(client API, cfg Config) *Provider {
	return &Provider{client: client, cfg: cfg, launchPoll: DefaultLaunchPoll, launchWait: DefaultLaunchWait}
}

// Name implements infrastructure.Provider.
func (p *Provider) Name() string { return ProviderName }

// Tag returns klotho's ownership tag value for cluster (the cluster name,
// lower-cased). The full Linode tag is clusterTag(cluster).
func Tag(cluster string) string { return strings.ToLower(cluster) }

// clusterTag is the flat Linode tag string carrying klotho's ownership tag for
// a cluster, "klotho.io/cluster:<cluster>".
func clusterTag(cluster string) string { return ClusterTagKey + ":" + Tag(cluster) }

// MachineName is the bootstrap machine's name, shared by the adopted
// LinodeMachine, the instance's Linode label, and the kubeadm control plane
// provider's BootstrapMachineName ("<cluster>-bootstrap"). Matching the label
// to the LinodeMachine name is what lets CAPL's label-uniqueness guard adopt
// the running instance rather than create a duplicate.
func MachineName(cluster string) string { return cluster + "-bootstrap" }

// Dependencies implements provider.Dependent: CAPL has no in-cluster dependency
// like the Proxmox provider's IPAM.
func (p *Provider) Dependencies() []provider.Dependency { return nil }

func (p *Provider) port() int {
	if p.cfg.Port > 0 {
		return p.cfg.Port
	}
	return DefaultPort
}

func (p *Provider) instanceType() string {
	if p.cfg.Type != "" {
		return p.cfg.Type
	}
	return DefaultType
}

func (p *Provider) image() string {
	if p.cfg.Image != "" {
		return p.cfg.Image
	}
	return DefaultImage
}

// providerID is the Kubernetes provider ID CAPL sets on a Linode instance.
func providerID(id int) string {
	return fmt.Sprintf("%s%d", ProviderIDPrefix, id)
}

// instanceTags are klotho's ownership tag plus any extra configured tags.
func (p *Provider) instanceTags(cluster string) []string {
	tags := []string{clusterTag(cluster)}
	tags = append(tags, p.cfg.Tags...)
	return tags
}

// hasTag reports whether tags contains the cluster's ownership tag.
func hasClusterTag(tags []string, cluster string) bool {
	want := clusterTag(cluster)
	for _, t := range tags {
		if t == want {
			return true
		}
	}
	return false
}

func errorf(format string, args ...any) error {
	return fmt.Errorf("linode provider: "+format, args...)
}
