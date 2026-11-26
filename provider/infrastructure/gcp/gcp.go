// Package gcp is the infrastructure provider for clusters managed by the
// Cluster API Provider GCP (CAPG). It boots the bootstrap node as a single
// Compute Engine instance with klotho's cloud-init delivered through the GCE
// metadata "user-data" key, resolves the control plane endpoint, renders the
// GCPMachine that adopts the running instance, and tears everything down by
// label.
//
// # Name and label conventions
//
// The following conventions are the contract Launch, Adopt, Plan and Teardown
// rely on:
//
//   - every instance it creates carries klotho's ownership label
//     ClusterLabelKey = Tag(cluster) (the cluster name, GCP-label-sanitised) so
//     Plan and Teardown can find it with an aggregated list filter;
//   - the bootstrap instance's GCE name is MachineName(cluster)
//     ("<cluster>-bootstrap"), which is also the adopted GCPMachine's name and
//     the kubeadm control plane provider's BootstrapMachineName. CAPG derives
//     the Compute Engine instance name from the GCPMachine's name, so matching
//     the two is what lets CAPG adopt the running instance instead of creating
//     a second one;
//   - the instance's providerID is recorded in state as
//     "gce://<project>/<zone>/<name>".
//
// # CAPG adoption (why the name above)
//
// CAPG adopts a pre-booted instance by NAME. Its GCPMachine reconciler's
// compute instance service looks the instance up with Instances.Get(project,
// zone, name) before creating one, and only creates when Get returns a 404.
// GCE instance names are unique per (project, zone), so an instance klotho
// already booted under the name CAPG derives from the GCPMachine is found and
// reused; CAPG then sets the Node's providerID from it and never issues a
// second Insert. This is the same name-based adoption CAPD and CAPMOX use, and
// klotho does NOT pre-set spec.providerID on the adopted GCPMachine (CAPG fills
// it itself from the found instance). This is implemented from CAPG's
// cloud/services/compute/instances source and MUST be confirmed against the
// pinned CAPG version and a live run (see the package tests and TESTING.md).
package gcp

import (
	"errors"
	"fmt"
	"regexp"
	"strings"
	"time"

	"google.golang.org/api/googleapi"

	"github.com/klotho-k8s/klotho/provider"
	"github.com/klotho-k8s/klotho/provider/infrastructure"
)

// ProviderName is the provider's identifier, as clusterctl knows CAPG and as
// --provider and state use it.
const ProviderName = "gcp"

// ClusterLabelKey is klotho's own ownership label key; its value is the cluster
// name (GCP-label-sanitised). Every instance klotho creates for a cluster
// carries it. GCP label keys cannot contain dots or slashes, so klotho's usual
// "klotho.io/cluster" is spelled "klotho-io-cluster" here.
const ClusterLabelKey = "klotho-io-cluster"

// ProviderIDPrefix is the scheme CAPG uses for a Compute Engine instance's
// provider ID: "gce://<project>/<zone>/<name>".
const ProviderIDPrefix = "gce://"

// Defaults applied by the provider.
const (
	DefaultPort = 6443
	// DefaultMachineType is the smallest sane type that still clears kubeadm's
	// two-CPU control-plane preflight: e2-medium (2 vCPU, 4 GB). e2-small (2 GB)
	// and e2-micro are cheaper but tight on memory for a control plane.
	DefaultMachineType = "e2-medium"
	// DefaultImageProject and DefaultImageFamily resolve a plain Ubuntu 22.04 LTS
	// cloud image. It carries no Kubernetes, so a cluster booted from it needs
	// klotho's install-at-boot (nodes.installKubernetes / --install-kubernetes).
	DefaultImageProject = "ubuntu-os-cloud"
	DefaultImageFamily  = "ubuntu-2204-lts"
	// DefaultDiskSizeGB sizes the bootstrap instance's boot disk.
	DefaultDiskSizeGB = 30
	// DefaultDiskType is a balanced persistent disk (cheaper than pd-ssd).
	DefaultDiskType   = "pd-balanced"
	DefaultLaunchPoll = 5 * time.Second
	DefaultLaunchWait = 5 * time.Minute
)

// Template variables read into the generated bundle. They are the names the
// flat cluster-template.yaml uses; Prepare fills them from Config.
const (
	VarProject           = "GCP_PROJECT"
	VarRegion            = "GCP_REGION"
	VarZone              = "GCP_ZONE"
	VarNetwork           = "GCP_NETWORK_NAME"
	VarSubnet            = "GCP_SUBNET_NAME"
	VarControlPlaneType  = "GCP_CONTROL_PLANE_MACHINE_TYPE"
	VarNodeType          = "GCP_NODE_MACHINE_TYPE"
	VarImage             = "GCP_IMAGE"
	VarControlPlaneCount = "CONTROL_PLANE_MACHINE_COUNT"
)

var _ infrastructure.Provider = (*Provider)(nil)
var _ provider.Checker = (*Provider)(nil)

// Config configures the provider. Zero fields fall back to defaults; see the
// accessors and Resolve.
type Config struct {
	// Project is the GCP project ID the cluster runs in. Required.
	Project string
	// Region is the GCP region, e.g. "us-central1". Required.
	Region string
	// Zone is the Compute Engine zone the bootstrap instance is placed in, e.g.
	// "us-central1-a". Required; it must be within Region.
	Zone string

	// MachineType sizes the bootstrap instance (and the template's machines).
	// Empty means DefaultMachineType.
	MachineType string
	// Image is a fully-qualified image ("projects/<p>/global/images/<name>" or a
	// bare image name in ImageProject). Empty resolves the newest image in
	// ImageFamily of ImageProject.
	Image string
	// ImageProject and ImageFamily resolve the bootstrap image when Image is
	// empty. Empty means DefaultImageProject / DefaultImageFamily (Ubuntu 22.04).
	ImageProject string
	ImageFamily  string
	// DiskSizeGB and DiskType size the bootstrap boot disk. Zero / empty mean
	// the defaults.
	DiskSizeGB int64
	DiskType   string

	// Network is the VPC network the instance joins. Empty means "default".
	Network string
	// Subnet is the subnetwork (in Region) the instance joins. Empty means the
	// network's region subnet named like Network (auto-mode networks have one
	// per region named "default").
	Subnet string
	// ServiceAccount is the instance service account email. Empty uses the
	// project's default compute service account.
	ServiceAccount string
	// NetworkTags are GCE network tags applied to the bootstrap instance (for
	// firewall targeting). Optional.
	NetworkTags []string
	// AuthorizedKeys are SSH public keys installed for the instance via the
	// "ssh-keys" metadata, so a failed bootstrap can be diagnosed. Each entry is
	// a bare public key; LoginUser names the user they are installed for.
	AuthorizedKeys []string
	// LoginUser is the OS user the authorized keys are installed for. Empty
	// means "klotho".
	LoginUser string

	// PublicIP gives the bootstrap instance an ephemeral external IP, so it can
	// reach the internet (to pull images) with no Cloud NAT and can serve as the
	// control plane endpoint. Default true; set false only with a Cloud NAT and
	// a supplied ControlPlaneEndpoint.
	PublicIP bool
	// Preemptible requests the bootstrap instance (and the template's machines)
	// as Spot/preemptible VMs: far cheaper, but reclaimable. Development and
	// test only.
	Preemptible bool

	// ControlPlaneEndpoint is the API server host. Empty means node zero's own
	// external IP is used (the cheapest endpoint: no managed load balancer). A
	// value here is validated and used as-is (an existing address or DNS name).
	ControlPlaneEndpoint string
	// Port is the API server port. Zero means DefaultPort.
	Port int

	// CredentialsJSON is a service-account key JSON. It authenticates klotho's
	// host-side Compute calls and, base64-encoded, is handed to the in-cluster
	// CAPG controller as GCP_B64ENCODED_CREDENTIALS. Empty means host calls use
	// Application Default Credentials; the in-cluster controller then has no
	// credential unless ADC resolves one the operator can read.
	CredentialsJSON []byte
}

// Provider implements infrastructure.Provider for GCP via CAPG.
type Provider struct {
	compute ComputeAPI
	cfg     Config

	launchPoll time.Duration
	launchWait time.Duration
}

// New returns a provider talking to Compute Engine through client, normally
// built by NewClient.
func New(client ComputeAPI, cfg Config) *Provider {
	return &Provider{compute: client, cfg: cfg, launchPoll: DefaultLaunchPoll, launchWait: DefaultLaunchWait}
}

// Name implements infrastructure.Provider.
func (p *Provider) Name() string { return ProviderName }

// labelSanitize makes s a valid GCP label value: lowercase, and anything
// outside [a-z0-9_-] replaced with "-", trimmed to 63 characters.
var labelInvalid = regexp.MustCompile(`[^a-z0-9_-]`)

func labelSanitize(s string) string {
	s = labelInvalid.ReplaceAllString(strings.ToLower(s), "-")
	if len(s) > 63 {
		s = s[:63]
	}
	return s
}

// Tag returns klotho's ownership label value for cluster (the cluster name,
// GCP-label-sanitised). The label key is ClusterLabelKey.
func Tag(cluster string) string { return labelSanitize(cluster) }

// MachineName is the bootstrap machine's name, shared by the adopted
// GCPMachine, the Compute Engine instance's name, and the kubeadm control plane
// provider's BootstrapMachineName ("<cluster>-bootstrap"). Matching the GCE
// instance name to the GCPMachine name is what lets CAPG's name-keyed
// Instances.Get adopt the running instance rather than create a duplicate.
func MachineName(cluster string) string { return cluster + "-bootstrap" }

// Dependencies implements provider.Dependent: CAPG has no in-cluster dependency
// like the Proxmox provider's IPAM.
func (p *Provider) Dependencies() []provider.Dependency { return nil }

func (p *Provider) port() int {
	if p.cfg.Port > 0 {
		return p.cfg.Port
	}
	return DefaultPort
}

func (p *Provider) machineType() string {
	if p.cfg.MachineType != "" {
		return p.cfg.MachineType
	}
	return DefaultMachineType
}

func (p *Provider) imageProject() string {
	if p.cfg.ImageProject != "" {
		return p.cfg.ImageProject
	}
	return DefaultImageProject
}

func (p *Provider) imageFamily() string {
	if p.cfg.ImageFamily != "" {
		return p.cfg.ImageFamily
	}
	return DefaultImageFamily
}

func (p *Provider) network() string {
	if p.cfg.Network != "" {
		return p.cfg.Network
	}
	return "default"
}

// subnet returns the configured subnetwork name, defaulting to the network name
// (auto-mode VPCs name their per-region subnet after the network).
func (p *Provider) subnet() string {
	if p.cfg.Subnet != "" {
		return p.cfg.Subnet
	}
	return p.network()
}

func (p *Provider) diskSizeGB() int64 {
	if p.cfg.DiskSizeGB > 0 {
		return p.cfg.DiskSizeGB
	}
	return DefaultDiskSizeGB
}

func (p *Provider) diskType() string {
	if p.cfg.DiskType != "" {
		return p.cfg.DiskType
	}
	return DefaultDiskType
}

func (p *Provider) loginUser() string {
	if p.cfg.LoginUser != "" {
		return p.cfg.LoginUser
	}
	return "klotho"
}

// clusterLabels are klotho's ownership label for a cluster.
func clusterLabels(cluster string) map[string]string {
	return map[string]string{ClusterLabelKey: Tag(cluster)}
}

// hasClusterLabel reports whether labels carry the cluster's ownership label.
func hasClusterLabel(labels map[string]string, cluster string) bool {
	return labels[ClusterLabelKey] == Tag(cluster)
}

// providerID is the Kubernetes provider ID CAPG sets on a Compute Engine
// instance: "gce://<project>/<zone>/<name>".
func providerID(project, zone, name string) string {
	return fmt.Sprintf("%s%s/%s/%s", ProviderIDPrefix, project, zone, name)
}

// apiServerRole is the suffix CAPG's ClusterScope.AddressSpec gives the control
// plane load balancer's resources; it matches CAPG's APIServerRoleTagValue.
const apiServerRole = "apiserver"

// addressName is the name of the global external IP CAPG's external load
// balancer uses for the API server: "<cluster>-apiserver" (CAPG's AddressSpec
// is `<ClusterScope.Name()>-<role>`). klotho reserves this address in Prepare
// so the endpoint is known before the certificates are minted; CAPG's
// createOrGetAddress is get-or-create by this name, so it adopts the reserved
// address instead of allocating its own.
func addressName(cluster string) string {
	return cluster + "-" + apiServerRole
}

// notFound reports whether err is a googleapi 404.
func notFound(err error) bool {
	var gerr *googleapi.Error
	return errors.As(err, &gerr) && gerr.Code == 404
}

// imageURL returns the full image reference used in the boot disk's
// sourceImage: the configured Image (qualified or bare), else the family
// selector "projects/<imageProject>/global/images/family/<family>".
func (p *Provider) imageURL() string {
	if p.cfg.Image != "" {
		if strings.Contains(p.cfg.Image, "/") {
			return p.cfg.Image
		}
		return fmt.Sprintf("projects/%s/global/images/%s", p.imageProject(), p.cfg.Image)
	}
	return fmt.Sprintf("projects/%s/global/images/family/%s", p.imageProject(), p.imageFamily())
}

func errorf(format string, args ...any) error {
	return fmt.Errorf("gcp provider: "+format, args...)
}

// ptr returns a pointer to v, for the SDK's pointer fields.
func ptr[T any](v T) *T { return &v }
