// Package digitalocean is the infrastructure provider for clusters managed by
// the Cluster API Provider DigitalOcean (CAPDO). It boots the bootstrap node as
// a single DigitalOcean Droplet with klotho's cloud-init delivered through the
// Droplet user-data field, resolves the control plane endpoint, renders the
// DOMachine that adopts the running droplet, and tears everything down by tag.
//
// # Name and tag conventions
//
// The following conventions are the contract Launch, Adopt, Plan and Teardown
// rely on:
//
//   - every droplet it creates carries klotho's ownership tag Tag(cluster)
//     ("klotho:cluster:<cluster>") so Plan and Teardown can find it, and the
//     generator copies the same tag into every DOMachineTemplate's
//     spec.template.spec.additionalTags (see TagMachineTemplates) so droplets
//     CAPDO creates from the templates carry it too;
//   - the bootstrap droplet is also tagged with CAPDO's own cluster and
//     apiserver role tags ("sigs-k8s-io:capdo:<cluster>" and
//     "sigs-k8s-io:capdo:<cluster>:apiserver") so the control plane load
//     balancer, whose target is that apiserver tag, forwards to node zero;
//   - the bootstrap droplet's name is MachineName(cluster)
//     ("<cluster>-bootstrap"), which is also the adopted DOMachine's name and
//     the kubeadm bootstrap Machine's infrastructureRef name;
//   - the droplet ID is recorded in state as the bootstrap node's provider ID
//     ("digitalocean://<id>"), which Adopt sets on the DOMachine.
//
// # CAPDO adoption (why the provider ID above)
//
// CAPDO adopts a pre-booted droplet by a pre-set DOMachine.spec.providerID. Its
// DOMachine controller resolves the droplet only through GetInstanceID(), which
// parses the droplet ID out of spec.providerID; with no providerID set
// GetInstanceID returns "" and GetDroplet("") returns nil, so the controller
// unconditionally calls CreateDroplet — there is NO lookup by name or tag on the
// create path. Pre-setting spec.providerID to "digitalocean://<id>" is therefore
// what makes CAPDO parse that droplet ID, GET the existing droplet and adopt it
// instead of creating a duplicate. This is the CAPL (Linode) adoption model, the
// opposite of CAPA/CAPG which adopt by name/tag and must NOT see a pre-set
// provider ID. This is implemented from CAPDO's
// internal/controller/domachine_controller.go, cloud/scope/machine.go and
// cloud/services/computes/droplets.go, and MUST be confirmed against the pinned
// CAPDO version and a live run (see the package tests and TESTING.md).
package digitalocean

import (
	"context"
	"fmt"
	"strings"
	"time"

	"github.com/digitalocean/godo"

	"github.com/klotho-k8s/klotho/provider"
	"github.com/klotho-k8s/klotho/provider/infrastructure"
)

// ProviderName is the provider's identifier, as clusterctl knows CAPDO and as
// --provider and state use it.
const ProviderName = "digitalocean"

// ClusterTagKey is the prefix of klotho's own ownership tag; the full tag is
// clusterTag(cluster) = "klotho:cluster:<cluster>". DigitalOcean tag names may
// contain only letters, numbers, colons, dashes and underscores (no dots or
// slashes), so klotho's usual "klotho.io/cluster" cannot be used here.
const ClusterTagKey = "klotho:cluster"

// capdoTagPrefix is the prefix CAPDO itself tags droplets with
// ("sigs-k8s-io:capdo"). Matches CAPDO's NameDigitalOceanProviderPrefix.
const capdoTagPrefix = "sigs-k8s-io:capdo"

// apiServerRole is the CAPDO role tag value for a control plane droplet; the
// control plane load balancer forwards to droplets carrying the apiserver tag.
const apiServerRole = "apiserver"

// ProviderIDPrefix is the scheme CAPDO uses for a Droplet's provider ID.
const ProviderIDPrefix = "digitalocean://"

// Defaults applied by the provider.
const (
	DefaultPort = 6443
	// DefaultSize is the smallest Droplet size that still clears kubeadm's
	// two-CPU control-plane preflight: s-2vcpu-4gb (2 vCPU, 4 GB). The cheaper
	// s-1vcpu-* sizes fail that check.
	DefaultSize = "s-2vcpu-4gb"
	// DefaultImage is a plain Ubuntu 22.04 image slug. It carries no Kubernetes,
	// so a cluster booted from it needs klotho's install-at-boot
	// (nodes.installKubernetes / --install-kubernetes).
	DefaultImage      = "ubuntu-22-04-x64"
	DefaultLaunchPoll = 5 * time.Second
	DefaultLaunchWait = 5 * time.Minute
)

// Template variables read into the generated bundle. They are the names the
// flat cluster-template.yaml uses; Prepare fills them from Config.
const (
	VarRegion            = "DO_REGION"
	VarControlPlaneSize  = "DO_CONTROL_PLANE_MACHINE_TYPE"
	VarNodeSize          = "DO_NODE_MACHINE_TYPE"
	VarImage             = "DO_IMAGE"
	VarLoadBalancerID    = "DO_LOADBALANCER_ID"
	VarControlPlaneCount = "CONTROL_PLANE_MACHINE_COUNT"
)

var _ infrastructure.Provider = (*Provider)(nil)
var _ provider.Checker = (*Provider)(nil)

// API is the subset of the godo client klotho uses, so it can be faked in
// tests. The client returned by NewClient satisfies it.
type API interface {
	CreateDroplet(ctx context.Context, req *godo.DropletCreateRequest) (*godo.Droplet, error)
	GetDroplet(ctx context.Context, id int) (*godo.Droplet, error)
	DeleteDroplet(ctx context.Context, id int) error
	ListDropletsByTag(ctx context.Context, tag string) ([]godo.Droplet, error)

	CreateLoadBalancer(ctx context.Context, req *godo.LoadBalancerRequest) (*godo.LoadBalancer, error)
	GetLoadBalancer(ctx context.Context, id string) (*godo.LoadBalancer, error)
	DeleteLoadBalancer(ctx context.Context, id string) error

	ListRegions(ctx context.Context) ([]godo.Region, error)
	ListSizes(ctx context.Context) ([]godo.Size, error)
	ListImages(ctx context.Context) ([]godo.Image, error)
	GetImageBySlug(ctx context.Context, slug string) (*godo.Image, error)
}

// Config configures the provider. Zero fields fall back to defaults; see the
// accessors and the Prepare validation.
type Config struct {
	// Token is the DigitalOcean API token. It is also what the in-cluster CAPDO
	// controller needs; it comes from the environment (DIGITALOCEAN_TOKEN) and is
	// never written back to disk.
	Token string
	// Region is the DigitalOcean region slug, e.g. "nyc3". Required.
	Region string
	// Size sizes the bootstrap droplet (and the template's machines). Empty means
	// DefaultSize.
	Size string
	// Image is the bootstrap image slug or numeric ID (as a string). Empty means
	// DefaultImage (plain Ubuntu).
	Image string
	// SSHKeys are DigitalOcean SSH key fingerprints or numeric IDs installed on
	// the bootstrap droplet, so a failed bootstrap can be diagnosed. The keys
	// must already exist in the DigitalOcean account.
	SSHKeys []string
	// Tags are extra DigitalOcean tags added to the bootstrap droplet alongside
	// klotho's ownership tag.
	Tags []string
	// VPCUUID places the bootstrap droplet (and CAPDO's machines) in an existing
	// VPC. Empty uses the region's default VPC.
	VPCUUID string
	// LoadBalancerID reuses an existing DigitalOcean load balancer as the control
	// plane endpoint instead of creating one. Its UUID is injected so CAPDO's
	// DOCluster adopts it (spec.network.apiServerLoadbalancers.resourceId) rather
	// than provisioning a second. Empty means klotho creates one in Prepare.
	LoadBalancerID string
	// ControlPlaneEndpoint is the API server host. Empty means klotho creates (or
	// reuses, see LoadBalancerID) a DigitalOcean load balancer and uses its IP. A
	// value here is used as-is (a DNS name you point at the load balancer); set
	// LoadBalancerID alongside it so CAPDO adopts the same load balancer.
	ControlPlaneEndpoint string
	// Port is the API server port. Zero means DefaultPort.
	Port int
}

// Provider implements infrastructure.Provider for DigitalOcean via CAPDO.
type Provider struct {
	client API
	cfg    Config

	launchPoll time.Duration
	launchWait time.Duration
}

// New returns a provider talking to DigitalOcean through client, normally built
// by NewClient.
func New(client API, cfg Config) *Provider {
	return &Provider{client: client, cfg: cfg, launchPoll: DefaultLaunchPoll, launchWait: DefaultLaunchWait}
}

// Name implements infrastructure.Provider.
func (p *Provider) Name() string { return ProviderName }

// Tag returns klotho's ownership tag for cluster ("klotho:cluster:<cluster>").
// Every droplet klotho creates for a cluster carries it, and the generator
// copies it into every DOMachineTemplate's additionalTags, so Plan and Teardown
// find every droplet by it. The cluster name is DigitalOcean-tag-sanitised.
func Tag(cluster string) string { return clusterTag(cluster) }

// safeName makes s a valid DigitalOcean tag/name component: anything outside
// [A-Za-z0-9:_-] replaced with "-" (dots and slashes are the common offenders).
func safeName(s string) string {
	var b strings.Builder
	for _, r := range s {
		switch {
		case r >= 'a' && r <= 'z', r >= 'A' && r <= 'Z', r >= '0' && r <= '9', r == '-', r == '_':
			b.WriteRune(r)
		default:
			b.WriteByte('-')
		}
	}
	return b.String()
}

// clusterTag is klotho's ownership tag for a cluster.
func clusterTag(cluster string) string { return ClusterTagKey + ":" + safeName(cluster) }

// capdoClusterTag is the cluster tag CAPDO itself applies to its droplets.
func capdoClusterTag(cluster string) string { return capdoTagPrefix + ":" + safeName(cluster) }

// apiServerTag is the control plane role tag the control plane load balancer
// targets. CAPDO applies it to the droplets it creates; klotho applies it to
// node zero so the load balancer forwards to it.
func apiServerTag(cluster string) string {
	return capdoTagPrefix + ":" + safeName(cluster) + ":" + apiServerRole
}

// MachineName is the bootstrap machine's name, shared by the adopted DOMachine,
// the bootstrap droplet's name, and the control plane provider's bootstrap
// Machine name ("<cluster>-bootstrap").
func MachineName(cluster string) string { return cluster + "-bootstrap" }

// loadBalancerName is the name of the control plane load balancer klotho
// creates (and CAPDO adopts by UUID): "<cluster>-apiserver".
func loadBalancerName(cluster string) string { return safeName(cluster) + "-" + apiServerRole }

// Dependencies implements provider.Dependent: CAPDO has no in-cluster dependency
// like the Proxmox provider's IPAM.
func (p *Provider) Dependencies() []provider.Dependency { return nil }

func (p *Provider) port() int {
	if p.cfg.Port > 0 {
		return p.cfg.Port
	}
	return DefaultPort
}

func (p *Provider) size() string {
	if p.cfg.Size != "" {
		return p.cfg.Size
	}
	return DefaultSize
}

func (p *Provider) image() string {
	if p.cfg.Image != "" {
		return p.cfg.Image
	}
	return DefaultImage
}

// providerID is the Kubernetes provider ID CAPDO sets on a Droplet.
func providerID(id int) string {
	return fmt.Sprintf("%s%d", ProviderIDPrefix, id)
}

// bootstrapTags are klotho's ownership tag, CAPDO's cluster and apiserver role
// tags, and any extra configured tags. The CAPDO tags make the control plane
// load balancer forward to node zero and let CAPDO's own teardown-by-tag find
// it.
func (p *Provider) bootstrapTags(cluster string) []string {
	tags := []string{clusterTag(cluster), capdoClusterTag(cluster), apiServerTag(cluster)}
	tags = append(tags, p.cfg.Tags...)
	return tags
}

// hasClusterTag reports whether tags contains the cluster's ownership tag.
func hasClusterTag(tags []string, cluster string) bool {
	want := clusterTag(cluster)
	for _, t := range tags {
		if t == want {
			return true
		}
	}
	return false
}

// sshKeys turns the configured fingerprints/IDs into godo SSH key references. A
// numeric entry is treated as a key ID, anything else as a fingerprint.
func (p *Provider) sshKeys() []godo.DropletCreateSSHKey {
	var out []godo.DropletCreateSSHKey
	for _, k := range p.cfg.SSHKeys {
		if id, err := parseInt(k); err == nil {
			out = append(out, godo.DropletCreateSSHKey{ID: id})
			continue
		}
		out = append(out, godo.DropletCreateSSHKey{Fingerprint: k})
	}
	return out
}

// dropletImage turns the configured image (slug or numeric ID) into a godo
// create image.
func (p *Provider) dropletImage() godo.DropletCreateImage {
	if id, err := parseInt(p.image()); err == nil {
		return godo.DropletCreateImage{ID: id}
	}
	return godo.DropletCreateImage{Slug: p.image()}
}

func parseInt(s string) (int, error) {
	var n int
	_, err := fmt.Sscanf(s, "%d", &n)
	if err != nil {
		return 0, err
	}
	// Sscanf with %d tolerates a trailing suffix; reject it by re-formatting.
	if fmt.Sprintf("%d", n) != s {
		return 0, fmt.Errorf("%q is not an integer", s)
	}
	return n, nil
}

func errorf(format string, args ...any) error {
	return fmt.Errorf("digitalocean provider: "+format, args...)
}
