// Package infrastructure defines what an infrastructure provider must do for a
// cluster to boot its first node and, later, to be torn down. The contract is
// phrased as outcomes, not resources: a provider that needs to allocate a load
// balancer and one that only validates a user-supplied address both satisfy it.
package infrastructure

import (
	"context"

	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"

	"github.com/klotho-k8s/klotho/pkg/cluster"
)

// Inventory is what Discover reports: the choices a template can be prefilled
// with, grouped by kind (for example "node", "template", "storage", "network").
type Inventory map[string][]Item

// Item is one selectable thing in an Inventory.
type Item struct {
	ID    string
	Name  string
	Attrs map[string]string
}

// Prepared is the result of Prepare.
type Prepared struct {
	Endpoint cluster.Endpoint
	// ClientEndpoint, when set, is a host-reachable address klotho dials for
	// its own API calls (converge and delete) instead of Endpoint. It exists
	// for environments where the cluster's controlPlaneEndpoint is not routable
	// from the host running klotho: the CAPD/Docker provider on rootless podman,
	// where the load balancer's container-network address is the cluster
	// endpoint but only its published host port (127.0.0.1:<port>) answers from
	// the host. Endpoint remains the cluster's controlPlaneEndpoint, baked into
	// the node configuration and the serving certificates; ClientEndpoint only
	// redirects klotho's own client. Zero means klotho dials Endpoint.
	ClientEndpoint cluster.Endpoint
	// Created lists resources Prepare allocated. Empty means Prepare only
	// validated, so a failed run has nothing to clean up yet.
	Created []cluster.Resource
	// Variables are template substitutions the provider wants injected into
	// generation because it created or chose the thing they name: a load
	// balancer ID, a security group, a reserved IP. They take precedence over
	// defaults but not over values the user set explicitly.
	Variables map[string]string
}

// Plan is what Teardown would remove, shown to the user before confirmation.
type Plan struct {
	Resources []cluster.Resource
}

// Provider provisions the environment a cluster runs in.
type Provider interface {
	// Name is the provider's identifier, as used by --provider and in state.
	Name() string
	// Discover reports the facts a cluster template can be prefilled with.
	Discover(ctx context.Context) (Inventory, error)
	// Prepare ensures everything the first node needs exists and returns the
	// control plane endpoint. It may allocate or only validate.
	Prepare(ctx context.Context, spec *cluster.Spec) (*Prepared, error)
	// Launch boots the bootstrap node with the given user-data.
	Launch(ctx context.Context, spec *cluster.Spec, userData []byte) (*cluster.Node, error)
	// Adopt renders the infrastructure Machine (and any infrastructure cluster
	// patch) that adopts the bootstrap node as a control plane member. bundle
	// is the generated cluster bundle's objects; the provider reads its machine
	// template from it so the adopted machine carries the same spec and the
	// cloned-from annotations the control plane controller checks, and no
	// rollout is triggered by adoption.
	Adopt(ctx context.Context, state *cluster.State, bundle []*unstructured.Unstructured) ([]byte, error)
	// Plan lists what Teardown would remove, by tag and from state.
	Plan(ctx context.Context, state *cluster.State) (*Plan, error)
	// Teardown removes everything in the plan in a safe order.
	Teardown(ctx context.Context, state *cluster.State, plan *Plan) error
}

// UserDataLimiter is implemented by an infrastructure provider whose user-data
// channel has a hard size cap. Create renders the bootstrap node's user-data,
// and when it exceeds the returned limit it offloads the contributed files to
// the configured payload host, inlining only a small fetch command per file, so
// the user-data fits. A provider whose channel has no practical cap does not
// implement this (or returns 0) and its user-data is always inlined, even when
// a payload host is configured.
type UserDataLimiter interface {
	// UserDataLimit is the maximum size, in bytes, of the raw (pre-base64)
	// user-data the provider's channel accepts. Zero means no limit.
	UserDataLimit() int
}

// DeletionSidecar is implemented by an infrastructure provider that, rather
// than requiring klotho to watch a cluster's deletion through, can launch a
// detached agent inside the infrastructure that finishes the teardown after
// klotho exits: destroying the last node the cluster cannot remove while it is
// still running on it, freeing the bootstrap ISO, and removing after itself.
//
// LaunchDeletionSidecar is called by the default fire-and-forget delete, after
// the Cluster object has been deleted, and returns once the agent is running.
// The agent carries a credential scoped to the task and self-destructs when it
// is done, so no further klotho involvement is needed. A provider whose own
// Teardown already finishes the last machine does not implement it; the
// fire-and-forget delete then simply returns.
type DeletionSidecar interface {
	LaunchDeletionSidecar(ctx context.Context, state *cluster.State) error
}
