package docker

import (
	_ "embed"

	"context"

	"github.com/klotho-k8s/klotho/pkg/cluster"
	"github.com/klotho-k8s/klotho/provider/infrastructure"
)

// cleanupScript is the sweep the deletion cleanup container runs. It is baked
// into the binary so klotho carries no external file.
//
//go:embed cleanup_script.sh
var cleanupScript string

var _ infrastructure.DeletionSidecar = (*Provider)(nil)

// CleanupContainerName is the deletion cleanup container's name.
func CleanupContainerName(clusterName string) string { return "klotho-cleanup-" + clusterName }

// LaunchDeletionSidecar launches the detached cleanup container that finishes a
// CAPD cluster's teardown after klotho exits, and returns once it is running.
// Implements infrastructure.DeletionSidecar.
//
// CAPD runs its controller inside the cluster, so deleting the Cluster object
// removes the machines in order but deadlocks on the last control-plane
// container: the controller cannot remove the container it is running in. The
// cleanup container, launched with the host runtime socket mounted, waits for
// Cluster API to do what it can and then force-removes the last control-plane
// container, the load balancer and any leftover workers (all carry the cluster
// label), and the cluster network. It runs with --rm, so the runtime removes it
// when its sweep exits -- it never removes itself over the socket, which a
// rootless runtime cannot complete for a running container.
//
// It is the default fire-and-forget delete's counterpart to the in-process
// watch+sweep that `klotho delete --watch` runs; the watch path needs no
// cleanup container because klotho stays attached and finishes the teardown
// itself over the local socket.
func (p *Provider) LaunchDeletionSidecar(ctx context.Context, state *cluster.State) error {
	if state == nil || state.Spec.Name == "" {
		return errorf("state has no cluster name")
	}
	if p.cfg.Socket == "" {
		return errorf("deletion cleanup needs the host runtime socket; set docker.socket or use `klotho delete --watch`")
	}
	name := state.Spec.Name

	_, err := p.rt.RunCleanup(ctx, CleanupOpts{
		Name:           CleanupContainerName(name),
		Image:          p.cleanupImage(),
		Cluster:        name,
		Network:        p.clusterNetwork(state),
		Socket:         p.cfg.Socket,
		Script:         cleanupScript,
		TimeoutSeconds: p.cleanupTimeoutSeconds(),
	})
	return err
}

// cleanupImage returns the configured cleanup image or the default.
func (p *Provider) cleanupImage() string {
	if p.cfg.CleanupImage != "" {
		return p.cfg.CleanupImage
	}
	return DefaultCleanupImage
}

// cleanupTimeoutSeconds returns the configured cleanup timeout or the default.
func (p *Provider) cleanupTimeoutSeconds() int {
	if p.cfg.CleanupTimeoutSeconds > 0 {
		return p.cfg.CleanupTimeoutSeconds
	}
	return DefaultCleanupTimeoutSeconds
}

// clusterNetwork returns the network the cleanup should remove: the one klotho
// recorded in state (what Prepare ensured), falling back to the configured
// network. The cleanup removes it only once nothing is attached, so a shared
// network another cluster still uses is left in place.
func (p *Provider) clusterNetwork(state *cluster.State) string {
	if state != nil {
		for _, r := range state.Resources {
			if r.Kind == ResourceKindNetwork {
				return r.ID
			}
		}
	}
	return p.network()
}
