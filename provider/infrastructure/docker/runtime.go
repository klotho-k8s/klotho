package docker

import "context"

// Runtime is the slice of a container engine (docker or podman) this provider
// needs. It is an interface so the provider is unit-tested against a fake
// without a running daemon.
type Runtime interface {
	// EnsureNetwork creates the named Docker network if it does not exist.
	EnsureNetwork(ctx context.Context, name string) error
	// RunNode creates and starts a container from opts and returns its ID.
	// For a control plane node it applies the kindest/node run recipe
	// (privileged, cgroupns, the kind tmpfs mounts, /sbin/init) and publishes
	// the API port; for a load balancer it runs the haproxy image.
	RunNode(ctx context.Context, opts RunOpts) (id string, err error)
	// Exec runs cmd in the named container. stdin, when non-nil, is piped in.
	Exec(ctx context.Context, name string, cmd []string, stdin []byte) (stdout string, err error)
	// List returns the containers matching every label in selector.
	List(ctx context.Context, selector map[string]string) ([]Container, error)
	// Inspect returns one container by name, or ok=false when absent.
	Inspect(ctx context.Context, name string) (c Container, ok bool, err error)
	// Remove force-removes the named container; a missing container is not an
	// error.
	Remove(ctx context.Context, name string) error
}

// RunOpts describes a container to create.
type RunOpts struct {
	Name    string
	Image   string
	Role    string // RoleControlPlane, RoleWorker or RoleLoadBalancer
	Network string
	Labels  map[string]string // the cluster/role labels are added if unset
	// PublishAPIPort publishes the container's DefaultPort to an ephemeral
	// host port (control plane nodes and the load balancer).
	PublishAPIPort bool
	// SocketMount, when set on a node, bind-mounts the host container-runtime
	// socket into the node at /var/run/docker.sock. The in-cluster CAPD
	// manager hostPath-mounts that path, so this is what lets the controller
	// running on the bootstrap node create and adopt sibling containers on
	// the host (CAPD talks a docker-compatible API; podman's socket serves
	// it). Empty leaves the node without host runtime access.
	SocketMount string
}

// DockerSocketPath is where CAPD's manager expects the runtime socket inside a
// node; SocketMount is bind-mounted here.
const DockerSocketPath = "/var/run/docker.sock"

// Container is what List and Inspect report.
type Container struct {
	ID      string
	Name    string
	Image   string
	Running bool
	Labels  map[string]string
	// IP is the container's address on its Docker network, empty when the
	// container is not running or has no address yet.
	IP string
}

// Role returns the container's CAPD role label, or "".
func (c Container) Role() string { return c.Labels[RoleLabelKey] }
