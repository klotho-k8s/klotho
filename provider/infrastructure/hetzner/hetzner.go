// Package hetzner is the infrastructure provider for clusters managed by the
// Cluster API Provider Hetzner (CAPH), hcloud (Hetzner Cloud) variant. It boots
// the bootstrap node as a single Hetzner Cloud server with klotho's cloud-init
// delivered through the server's user_data, resolves the control plane endpoint,
// renders the HCloudMachine that adopts the running server, and tears everything
// down by label.
//
// # Name and label conventions
//
// The following conventions are the contract Launch, Adopt, Plan and Teardown
// rely on:
//
//   - every server it creates carries klotho's ownership label
//     ClusterLabelKey = Tag(cluster) (the cluster name, Hetzner-label-sanitised)
//     so Plan and Teardown can find it with a label selector;
//   - the bootstrap server's name is MachineName(cluster) ("<cluster>-bootstrap"),
//     which is also the adopted HCloudMachine's name and the kubeadm control
//     plane provider's BootstrapMachineName. CAPH sets the server name it creates
//     to the HCloudMachine's name, so matching the two is what lets CAPH adopt the
//     running server instead of creating a second one;
//   - the server's providerID is recorded in state as "hcloud://<id>".
//
// # CAPH adoption (why the name above)
//
// CAPH adopts a pre-booted server by NAME. Its HCloudMachine reconciler sets the
// create request's server name to the HCloudMachine's own name
// (pkg/scope/machine.go: ServerCreateOpts.Name = scope.Name(), and Name() returns
// HCloudMachine.Name). A Hetzner server name is unique per project, so when CAPH
// reconciles a machine whose name matches a server klotho already booted, the
// create returns a uniqueness_error and CAPH's server service resolves it by
// looking the server up by name and adopting it ("server already exists after a
// uniqueness error, adopting it instead of failing"), then sets the providerID
// itself from the found server. So klotho does NOT pre-set spec.providerID on the
// adopted HCloudMachine; it relies on the name match. (This is the GCP/CAPD model,
// the opposite of CAPL.) This is implemented from CAPH's hcloudmachine controller
// and pkg/services/hcloud/server and MUST be confirmed against the pinned CAPH
// version and a live run (see the package tests and TESTING.md).
package hetzner

import (
	"fmt"
	"regexp"
	"time"

	"github.com/hetznercloud/hcloud-go/v2/hcloud"

	"github.com/klotho-k8s/klotho/provider"
	"github.com/klotho-k8s/klotho/provider/infrastructure"
)

// ProviderName is the provider's identifier, as clusterctl knows CAPH and as
// --provider and state use it.
const ProviderName = "hetzner"

// ClusterLabelKey is klotho's own ownership label key; its value is the cluster
// name (Hetzner-label-sanitised). Every server klotho creates for a cluster
// carries it. Hetzner labels follow the Kubernetes label syntax, so the usual
// prefixed key "klotho.io/cluster" is valid as-is.
const ClusterLabelKey = "klotho.io/cluster"

// ProviderIDPrefix is the scheme CAPH uses for a Hetzner Cloud server's provider
// ID: "hcloud://<id>" (pkg/scope/machine.go SetProviderID).
const ProviderIDPrefix = "hcloud://"

// Defaults applied by the provider.
const (
	DefaultPort = 6443
	// DefaultServerType is the smallest type that still clears kubeadm's two-CPU
	// control-plane preflight and is available in every Hetzner location: cpx21
	// (3 shared AMD vCPU, 4 GB). The Intel cx-line equivalents are EU-only; cax11
	// (Arm) works too but the default image below is amd64.
	DefaultServerType = "cpx21"
	// DefaultImage is a plain Ubuntu 22.04 cloud image. It carries no Kubernetes,
	// so a cluster booted from it needs klotho's install-at-boot
	// (nodes.installKubernetes / --install-kubernetes).
	DefaultImage      = "ubuntu-22.04"
	DefaultLaunchPoll = 5 * time.Second
	DefaultLaunchWait = 5 * time.Minute
)

// Template variables read into the generated bundle. They are the names the flat
// cluster-template.yaml uses; Prepare fills them from Config.
const (
	VarLocation          = "HCLOUD_LOCATION"
	VarControlPlaneType  = "HCLOUD_CONTROL_PLANE_MACHINE_TYPE"
	VarNodeType          = "HCLOUD_WORKER_MACHINE_TYPE"
	VarImage             = "HCLOUD_IMAGE_NAME"
	VarSSHKey            = "HCLOUD_SSH_KEY"
	VarControlPlaneCount = "CONTROL_PLANE_MACHINE_COUNT"
)

var _ infrastructure.Provider = (*Provider)(nil)
var _ provider.Checker = (*Provider)(nil)

// Config configures the provider. Zero fields fall back to defaults; see the
// accessors and Resolve.
type Config struct {
	// Token is the Hetzner Cloud API token. It is also what the in-cluster CAPH
	// controller and cloud controller manager need; it comes from the environment
	// (HCLOUD_TOKEN) and is never written back to disk.
	Token string
	// Location is the Hetzner location the bootstrap server is placed in and the
	// cluster's control-plane region, e.g. "fsn1", "nbg1", "hel1", "ash", "hil",
	// "sin". Required.
	Location string
	// ServerType sizes the bootstrap server (and the template's machines). Empty
	// means DefaultServerType.
	ServerType string
	// Image is the bootstrap image name (or label). Empty means DefaultImage
	// (plain Ubuntu).
	Image string
	// SSHKeys are the names of SSH keys that already exist in the Hetzner project.
	// At least one is required: CAPH's HetznerCluster spec requires an SSH key, and
	// it is the only way into a node to diagnose a failed bootstrap. klotho
	// installs all of them on the bootstrap server and uses the first for the
	// generated template.
	SSHKeys []string
	// Labels are extra Hetzner labels added to the bootstrap server alongside
	// klotho's ownership label.
	Labels map[string]string
	// ControlPlaneEndpoint is the API server host. Required: klotho mints the
	// certificates and node zero's cloud-init before the node boots, and the flat
	// template disables CAPH's managed load balancer, so there is no
	// pre-assignable address otherwise. The cheapest choice is a DNS name you
	// control, pointed at node zero after it boots; an existing Hetzner load
	// balancer or floating IP address works too.
	ControlPlaneEndpoint string
	// Port is the API server port. Zero means DefaultPort.
	Port int
}

// Provider implements infrastructure.Provider for Hetzner Cloud via CAPH.
type Provider struct {
	client API
	cfg    Config

	launchPoll time.Duration
	launchWait time.Duration
}

// New returns a provider talking to Hetzner Cloud through client, normally built
// by NewClient.
func New(client API, cfg Config) *Provider {
	return &Provider{client: client, cfg: cfg, launchPoll: DefaultLaunchPoll, launchWait: DefaultLaunchWait}
}

// Name implements infrastructure.Provider.
func (p *Provider) Name() string { return ProviderName }

// labelSanitize makes s a valid Hetzner label value: anything outside
// [a-zA-Z0-9_.-] replaced with "-", trimmed to 63 characters and to a valid
// leading/trailing character.
var labelInvalid = regexp.MustCompile(`[^a-zA-Z0-9_.-]`)
var labelEdge = regexp.MustCompile(`^[^a-zA-Z0-9]+|[^a-zA-Z0-9]+$`)

func labelSanitize(s string) string {
	s = labelInvalid.ReplaceAllString(s, "-")
	if len(s) > 63 {
		s = s[:63]
	}
	s = labelEdge.ReplaceAllString(s, "")
	return s
}

// Tag returns klotho's ownership label value for cluster (the cluster name,
// Hetzner-label-sanitised). The label key is ClusterLabelKey.
func Tag(cluster string) string { return labelSanitize(cluster) }

// MachineName is the bootstrap machine's name, shared by the adopted
// HCloudMachine, the Hetzner server's name, and the kubeadm control plane
// provider's BootstrapMachineName ("<cluster>-bootstrap"). Matching the server
// name to the HCloudMachine name is what lets CAPH adopt the running server
// rather than create a duplicate.
func MachineName(cluster string) string { return cluster + "-bootstrap" }

// Dependencies implements provider.Dependent: CAPH has no in-cluster dependency
// like the Proxmox provider's IPAM.
func (p *Provider) Dependencies() []provider.Dependency { return nil }

func (p *Provider) port() int {
	if p.cfg.Port > 0 {
		return p.cfg.Port
	}
	return DefaultPort
}

func (p *Provider) serverType() string {
	if p.cfg.ServerType != "" {
		return p.cfg.ServerType
	}
	return DefaultServerType
}

func (p *Provider) image() string {
	if p.cfg.Image != "" {
		return p.cfg.Image
	}
	return DefaultImage
}

// providerID is the Kubernetes provider ID CAPH sets on a Hetzner server:
// "hcloud://<id>".
func providerID(id int64) string {
	return fmt.Sprintf("%s%d", ProviderIDPrefix, id)
}

// serverLabels are klotho's ownership label for a cluster plus any extra
// configured labels.
func (p *Provider) serverLabels(cluster string) map[string]string {
	labels := map[string]string{ClusterLabelKey: Tag(cluster)}
	for k, v := range p.cfg.Labels {
		labels[k] = v
	}
	return labels
}

// clusterSelector is the label selector that matches every server carrying the
// cluster's ownership label.
func clusterSelector(cluster string) string {
	return ClusterLabelKey + "=" + Tag(cluster)
}

// hasClusterLabel reports whether labels carry the cluster's ownership label.
func hasClusterLabel(labels map[string]string, cluster string) bool {
	return labels[ClusterLabelKey] == Tag(cluster)
}

// notFound reports whether err is a Hetzner "not_found" API error.
func notFound(err error) bool {
	return hcloud.IsError(err, hcloud.ErrorCodeNotFound)
}

func errorf(format string, args ...any) error {
	return fmt.Errorf("hetzner provider: "+format, args...)
}

// firstSSHKey returns the first configured SSH key name, or "".
func (p *Provider) firstSSHKey() string {
	if len(p.cfg.SSHKeys) > 0 {
		return p.cfg.SSHKeys[0]
	}
	return ""
}
