// Package openstack is the infrastructure provider for clusters managed by the
// Cluster API Provider OpenStack (CAPO). It boots the bootstrap node as a
// single Nova server with klotho's cloud-init delivered through the server's
// user-data, resolves the control plane endpoint (a Neutron floating IP it
// allocates, or a user-supplied address), renders the OpenStackMachine that
// adopts the running server, and tears everything down by Nova server tag.
//
// # Name and tag conventions
//
// The following conventions are the contract Launch, Adopt, Plan and Teardown
// rely on:
//
//   - every server it creates carries klotho's ownership tag clusterTag(cluster)
//     ("klotho-io-cluster=<cluster>") so Plan and Teardown can find it with a
//     Nova tag filter;
//   - the bootstrap server's Nova name is MachineName(cluster)
//     ("<cluster>-bootstrap"), which is also the adopted OpenStackMachine's name
//     and the kubeadm control plane provider's BootstrapMachineName. CAPO
//     derives the Nova server name from the OpenStackMachine's name, so matching
//     the two is what lets CAPO adopt the running server instead of creating a
//     second one;
//   - the server's providerID is recorded in state as "openstack://<region>/<id>"
//     ("openstack:///<id>" when no region is configured, the common case).
//
// # CAPO adoption (why the name above)
//
// CAPO adopts a pre-booted server by NAME. When its OpenStackServer reconciler
// has no recorded instance ID, getOrCreateServer calls
// GetInstanceStatusByName(server, server.Name), which lists Nova servers with an
// anchored-exact name filter ("^<name>$"): exactly one match is reused (adopted)
// and its UUID becomes the machine's providerID; zero matches creates a new
// server; more than one is an error ("DuplicateServerNames"). The Nova server
// name equals the OpenStackServer object name, which equals the OpenStackMachine
// object name — so a server klotho already booted under the name CAPO derives
// from the OpenStackMachine is found and reused, and CAPO never issues a second
// create.
//
// klotho therefore does NOT pre-set spec.providerID on the adopted
// OpenStackMachine: CAPO fills it itself from the found server, and
// spec.providerID is output-only (CAPO overwrites it unconditionally once the
// server is ACTIVE). This is the same name-based adoption CAPG and CAPD use, the
// opposite of CAPL. It is implemented from CAPO's openstackserver_controller.go
// (getOrCreateServer) and pkg/cloud/services/compute/instance.go
// (GetInstanceStatusByName) at v0.15.2 and MUST be confirmed against the pinned
// CAPO version and a live run (see the package tests and TESTING.md).
package openstack

import (
	"fmt"
	"regexp"
	"strings"
	"time"

	"github.com/klotho-k8s/klotho/provider"
	"github.com/klotho-k8s/klotho/provider/infrastructure"
)

// ProviderName is the provider's identifier, as clusterctl knows CAPO and as
// --provider and state use it.
const ProviderName = "openstack"

// ClusterTagKey is klotho's own ownership tag key; the full Nova tag is
// clusterTag(cluster) = "<ClusterTagKey>=<cluster>". Nova server tags cannot
// contain a comma and are kept to plain characters, so klotho's usual
// "klotho.io/cluster" is spelled "klotho-io-cluster" here.
const ClusterTagKey = "klotho-io-cluster"

// ProviderIDScheme is the scheme CAPO uses for a Nova server's provider ID:
// "openstack://<region>/<uuid>" ("openstack:///<uuid>" with an empty region).
const ProviderIDScheme = "openstack://"

// Defaults applied by the provider.
const (
	DefaultPort = 6443
	// DefaultLaunchPoll and DefaultLaunchWait bound the wait for a server to
	// become ACTIVE.
	DefaultLaunchPoll = 5 * time.Second
	DefaultLaunchWait = 5 * time.Minute
)

// Template variables read into the generated bundle. They are the names the flat
// cluster-template.yaml uses; Prepare fills them from Config and from what it
// resolved or allocated.
const (
	VarCloud                  = "OPENSTACK_CLOUD"
	VarNetworkID              = "OPENSTACK_NETWORK_ID"
	VarExternalNetworkID      = "OPENSTACK_EXTERNAL_NETWORK_ID"
	VarControlPlaneFloatingIP = "OPENSTACK_CONTROL_PLANE_FLOATING_IP"
	VarImageName              = "OPENSTACK_IMAGE_NAME"
	VarControlPlaneFlavor     = "OPENSTACK_CONTROL_PLANE_MACHINE_FLAVOR"
	VarNodeFlavor             = "OPENSTACK_NODE_MACHINE_FLAVOR"
	VarSSHKeyName             = "OPENSTACK_SSH_KEY_NAME"
	VarControlPlaneCount      = "CONTROL_PLANE_MACHINE_COUNT"
	// VarCloudsYAMLB64 is the base64-encoded clouds.yaml the in-cluster CAPO
	// controller authenticates with, embedded in the per-cluster credential
	// Secret the template renders.
	VarCloudsYAMLB64 = "OPENSTACK_CLOUDS_YAML_B64"
)

// CloudConfigSecretName is the name of the Secret the template renders holding
// the clouds.yaml the in-cluster CAPO controller reads (the OpenStackCluster's
// identityRef.name).
func CloudConfigSecretName(cluster string) string { return cluster + "-cloud-config" }

var _ infrastructure.Provider = (*Provider)(nil)
var _ provider.Checker = (*Provider)(nil)

// Config configures the provider. Zero fields fall back to defaults; see the
// accessors and Prepare.
type Config struct {
	// Cloud is the entry in clouds.yaml to authenticate with (the cloudName the
	// in-cluster CAPO controller's identityRef also selects). Required.
	Cloud string
	// CloudsYAML is an optional path to a clouds.yaml. Empty uses the standard
	// OpenStack locations (OS_CLIENT_CONFIG_FILE, ./clouds.yaml,
	// ~/.config/openstack, /etc/openstack).
	CloudsYAML string
	// CloudsYAMLData is the raw clouds.yaml content handed, base64-encoded, to the
	// in-cluster CAPO controller as its per-cluster credential Secret. The CLI
	// reads it from CloudsYAML (or the standard locations); it is never written
	// back to disk by klotho.
	CloudsYAMLData []byte
	// Region overrides the clouds.yaml region when selecting the Nova/Neutron/
	// Glance endpoints. It is NOT propagated into the providerID: klotho keeps the
	// empty-region form ("openstack:///<uuid>") so node zero's kubelet provider-id
	// matches the value CAPO sets when identityRef.region is unset.
	Region string

	// Network is the name of the existing Neutron network node zero (and every
	// adopted/created machine) attaches to. Required: klotho boots node zero on
	// it and the cluster template references the same network by ID so CAPO's
	// machines land beside it.
	Network string
	// Subnet is the name of the subnet within Network; empty lets CAPO pick the
	// network's subnet.
	Subnet string
	// ExternalNetwork is the name of the external (public) network a floating IP
	// is allocated from when ControlPlaneEndpoint is empty. Required in that
	// case.
	ExternalNetwork string

	// Flavor is the Nova flavor name for the bootstrap server and the template's
	// machines. Required (OpenStack has no universal default flavor).
	Flavor string
	// Image is the Glance image name the bootstrap server boots from. Required; a
	// plain cloud image carries no Kubernetes, so a cluster booted from it needs
	// klotho's install-at-boot (nodes.installKubernetes / --install-kubernetes).
	Image string
	// SSHKeyName is an existing Nova keypair installed on the bootstrap server, so
	// a failed bootstrap can be diagnosed. Optional but strongly recommended.
	SSHKeyName string
	// SecurityGroups are existing security group names applied to the bootstrap
	// server. Optional.
	SecurityGroups []string
	// AvailabilityZone places the bootstrap server. Optional.
	AvailabilityZone string

	// ControlPlaneEndpoint is the API server host. Empty means klotho allocates a
	// Neutron floating IP from ExternalNetwork and associates it with node zero
	// (the cheapest endpoint: no Octavia load balancer). A value here is used
	// as-is (an existing floating IP, an Octavia VIP, or a DNS name you control)
	// and no floating IP is allocated.
	ControlPlaneEndpoint string
	// Port is the API server port. Zero means DefaultPort.
	Port int
}

// Provider implements infrastructure.Provider for OpenStack via CAPO.
type Provider struct {
	client ComputeAPI
	cfg    Config

	// fipID is the floating IP Prepare allocated for the control plane endpoint,
	// so Launch can associate it with node zero once the server is up. Empty when
	// the endpoint was user-supplied (klotho allocates and associates nothing).
	fipID string

	launchPoll time.Duration
	launchWait time.Duration
}

// ResourceKindServer and ResourceKindFloatingIP are the Plan/Created kinds for
// a Nova server and a Neutron floating IP.
const (
	ResourceKindServer     = "server"
	ResourceKindFloatingIP = "floatingip"
)

// New returns a provider talking to OpenStack through client, normally built by
// NewClient.
func New(client ComputeAPI, cfg Config) *Provider {
	return &Provider{client: client, cfg: cfg, launchPoll: DefaultLaunchPoll, launchWait: DefaultLaunchWait}
}

// Name implements infrastructure.Provider.
func (p *Provider) Name() string { return ProviderName }

// tagInvalid matches characters not kept in a Nova tag value.
var tagInvalid = regexp.MustCompile(`[^a-z0-9_.-]`)

// Tag returns klotho's ownership tag value for cluster (the cluster name,
// lower-cased and sanitised). The full Nova tag is clusterTag(cluster).
func Tag(cluster string) string {
	s := tagInvalid.ReplaceAllString(strings.ToLower(cluster), "-")
	if len(s) > 60 {
		s = s[:60]
	}
	return s
}

// clusterTag is the Nova server tag carrying klotho's ownership tag for a
// cluster, "klotho-io-cluster=<cluster>".
func clusterTag(cluster string) string { return ClusterTagKey + "=" + Tag(cluster) }

// hasClusterTag reports whether tags carry the cluster's ownership tag.
func hasClusterTag(tags []string, cluster string) bool {
	want := clusterTag(cluster)
	for _, t := range tags {
		if t == want {
			return true
		}
	}
	return false
}

// MachineName is the bootstrap machine's name, shared by the adopted
// OpenStackMachine, the Nova server's name, and the kubeadm control plane
// provider's BootstrapMachineName ("<cluster>-bootstrap"). Matching the Nova
// server name to the OpenStackMachine name is what lets CAPO's name-keyed
// GetInstanceStatusByName adopt the running server rather than create a
// duplicate.
func MachineName(cluster string) string { return cluster + "-bootstrap" }

// Dependencies implements provider.Dependent: CAPO has no in-cluster dependency
// like the Proxmox provider's IPAM.
func (p *Provider) Dependencies() []provider.Dependency { return nil }

func (p *Provider) port() int {
	if p.cfg.Port > 0 {
		return p.cfg.Port
	}
	return DefaultPort
}

// serverTags are klotho's ownership tag for a cluster.
func (p *Provider) serverTags(cluster string) []string {
	return []string{clusterTag(cluster)}
}

// providerID is the Kubernetes provider ID CAPO sets on a Nova server:
// "openstack://<region>/<uuid>" ("openstack:///<uuid>" with an empty region).
func providerID(region, uuid string) string {
	return fmt.Sprintf("%s%s/%s", ProviderIDScheme, region, uuid)
}

func errorf(format string, args ...any) error {
	return fmt.Errorf("openstack provider: "+format, args...)
}
