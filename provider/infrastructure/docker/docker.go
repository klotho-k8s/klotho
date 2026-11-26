// Package docker is the infrastructure provider for clusters managed by the
// Cluster API Docker provider (CAPD). It boots the bootstrap node as a single
// kindest/node container, runs the bootstrap data by exec (containers do not
// boot cloud-init), renders the DockerMachine that adopts the running
// container, and tears everything down by label.
//
// # The self-managed, no-management-cluster model
//
// klotho never stands up a separate kind management cluster. Node zero is one
// running container that becomes the cluster: klotho creates it with CAPD's
// own naming and labels, kubeadm-inits it, installs the operator and CAPD on
// it, applies a self-describing bundle, and CAPD adopts the container instead
// of creating a new one. This mirrors how the Proxmox provider boots a VM and
// CAPMOX adopts it.
//
// # Name conventions (the contract Adopt, Plan and Teardown rely on)
//
// Nothing is recorded by container ID in state; these conventions are the
// contract, and they match CAPD's internal/docker package exactly so CAPD
// adopts what klotho creates:
//
//   - every container of the cluster carries the label
//     ClusterLabelKey ("io.x-k8s.kind.cluster") = <cluster> and a
//     RoleLabelKey ("io.x-k8s.kind.role") of RoleControlPlane, RoleWorker or
//     RoleLoadBalancer;
//   - the bootstrap container is named MachineContainerName(cluster,
//     BootstrapMachineName(cluster)), which is "<cluster>-bootstrap", and that
//     is also the adopted DockerMachine's name (CAPD matches an existing
//     container by name and the cluster label, and skips creation when it is
//     already running);
//   - the control plane endpoint is the load-balancer container
//     LBName(cluster), "<cluster>-lb", a haproxy container CAPD also adopts.
//
// # Template requirement (live-test finding)
//
// klotho's adopt/pivot model needs a FLAT cluster template: Cluster +
// DockerCluster + DockerMachineTemplate + KubeadmControlPlane +
// MachineDeployment, all present in the bundle (Adopt reads the infrastructure
// MachineTemplate from it; SetControlPlaneEndpoint patches the DockerCluster).
// CAPD's released "development" flavor is instead a managed-topology
// (ClusterClass "quick-start") template whose infrastructure objects do not
// appear in the bundle, so it cannot be used as is. A flat Docker template
// must be supplied (bundled by klotho or via --manifest) until klotho gains
// ClusterClass/topology support.
package docker

import (
	"fmt"
	"strings"

	"github.com/klotho-k8s/klotho/provider/infrastructure"
)

// ProviderName is the provider's identifier, as clusterctl knows it.
const ProviderName = "docker"

// Labels CAPD puts on every container it manages; klotho sets the same ones so
// CAPD adopts klotho's containers. See the fork's
// test/infrastructure/docker/internal/docker/util.go.
const (
	ClusterLabelKey = "io.x-k8s.kind.cluster"
	RoleLabelKey    = "io.x-k8s.kind.role"
)

// Container role label values (CAPD constants).
const (
	RoleControlPlane = "control-plane"
	RoleWorker       = "worker"
	RoleLoadBalancer = "external-load-balancer"
)

// Defaults.
const (
	// DefaultNetwork is the Docker network CAPD attaches nodes to. CAPD reads
	// DOCKER_NETWORK and defaults to "kind".
	DefaultNetwork = "kind"
	// DefaultPort is the control plane port behind the load balancer.
	DefaultPort = 6443
	// DefaultLBImage is the haproxy image CAPD uses for the load balancer.
	DefaultLBImage = "kindest/haproxy:v20230606-42a2262b"
	// DefaultCleanupImage is the image the deletion cleanup container runs. It
	// must carry a Docker-API-compatible CLI and a POSIX shell; the official
	// docker CLI image drives podman's socket as well as docker's.
	DefaultCleanupImage = "docker.io/library/docker:cli"
	// DefaultCleanupTimeoutSeconds bounds how long the cleanup container waits
	// for Cluster API to delete what it can before it force-removes the rest.
	DefaultCleanupTimeoutSeconds = 300
)

// VarKubernetesVersion is the template variable (and config fallback) that
// selects the kindest/node image tag for the bootstrap container.
const VarKubernetesVersion = "KUBERNETES_VERSION"

var _ infrastructure.Provider = (*Provider)(nil)

// Config configures the provider.
type Config struct {
	// Runtime is the container CLI, "docker" (default) or "podman".
	Runtime string
	// NodeImage overrides the kindest/node image for the bootstrap
	// container, for example "kindest/node:v1.33.0". Empty derives it from
	// the cluster's KUBERNETES_VERSION variable as "kindest/node:<version>".
	NodeImage string
	// Network is the Docker network nodes join. Empty means DefaultNetwork.
	Network string
	// LBImage overrides the load-balancer image. Empty means DefaultLBImage.
	LBImage string
	// Socket is the host container-runtime socket bind-mounted into every
	// node at /var/run/docker.sock, so the in-cluster CAPD manager can create
	// and adopt sibling containers. For podman it is typically
	// "/run/user/<uid>/podman/podman.sock"; for docker "/var/run/docker.sock".
	// Empty leaves nodes without host runtime access, so CAPD cannot
	// reconcile and the cluster never self-manages.
	Socket string
	// CleanupImage overrides the image the deletion cleanup container runs.
	// Empty means DefaultCleanupImage.
	CleanupImage string
	// CleanupTimeoutSeconds overrides how long the cleanup container waits for
	// Cluster API before force-removing the remainder. Zero means
	// DefaultCleanupTimeoutSeconds.
	CleanupTimeoutSeconds int
}

// Provider implements infrastructure.Provider for CAPD.
type Provider struct {
	rt  Runtime
	cfg Config
}

// New returns a provider that drives the container runtime rt. When rt is nil
// a CLI runtime is built from cfg.Runtime (NewCLIRuntime).
func New(rt Runtime, cfg Config) *Provider {
	if rt == nil {
		rt = NewCLIRuntime(cfg.Runtime)
	}
	return &Provider{rt: rt, cfg: cfg}
}

// Name implements infrastructure.Provider.
func (p *Provider) Name() string { return ProviderName }

// Tag returns the cluster label value every container of cluster carries. It
// is the cluster name; Plan and Teardown filter by ClusterLabelKey=Tag.
func Tag(cluster string) string { return cluster }

// clusterLabels returns the label selector for every container of cluster.
func clusterLabels(cluster string) map[string]string {
	return map[string]string{ClusterLabelKey: cluster}
}

// network returns the configured Docker network or the default.
func (p *Provider) network() string {
	if p.cfg.Network != "" {
		return p.cfg.Network
	}
	return DefaultNetwork
}

// lbImage returns the configured load-balancer image or the default.
func (p *Provider) lbImage() string {
	if p.cfg.LBImage != "" {
		return p.cfg.LBImage
	}
	return DefaultLBImage
}

// BootstrapMachineName is the adopted control plane machine's name, which is
// also its container name.
func BootstrapMachineName(cluster string) string { return cluster + "-bootstrap" }

// MachineContainerName computes the container name for a machine, exactly as
// CAPD's MachineContainerName does: the machine name when it already starts
// with the cluster name, otherwise "<cluster>-<machine>".
func MachineContainerName(cluster, machine string) string {
	if strings.HasPrefix(machine, cluster) {
		return machine
	}
	return cluster + "-" + machine
}

// LBName is the load-balancer container's name.
func LBName(cluster string) string { return cluster + "-lb" }

func errorf(format string, args ...any) error {
	return fmt.Errorf("docker provider: "+format, args...)
}
