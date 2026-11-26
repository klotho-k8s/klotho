package docker

import (
	"context"

	"github.com/klotho-k8s/klotho/pkg/cluster"
)

// ProviderIDPrefix is the DockerMachine provider ID scheme; the adopted
// machine's provider ID is ProviderIDPrefix + container name.
const ProviderIDPrefix = "docker:////"

// Launch creates the bootstrap node as a single control plane container with
// CAPD's naming and labels, runs the bootstrap user-data inside it by exec
// (containers do not boot cloud-init), points the load balancer at it, and
// returns the node. CAPD later adopts this container because its name and the
// cluster label match the DockerMachine Adopt renders, and it is running.
//
// NOTE: the bootstrap exec path (translating cloud-init write_files/runcmd to
// in-container commands) and the load-balancer backend configuration are wired
// but need live validation; see runBootstrap and configureLoadBalancer.
func (p *Provider) Launch(ctx context.Context, spec *cluster.Spec, userData []byte) (*cluster.Node, error) {
	if spec == nil || spec.Name == "" {
		return nil, errorf("spec has no cluster name")
	}
	name := spec.Name
	container := MachineContainerName(name, BootstrapMachineName(name))

	image, err := p.nodeImage(spec)
	if err != nil {
		return nil, err
	}

	if _, ok, err := p.rt.Inspect(ctx, container); err != nil {
		return nil, err
	} else if !ok {
		if _, err := p.rt.RunNode(ctx, RunOpts{
			Name:    container,
			Image:   image,
			Role:    RoleControlPlane,
			Network: p.network(),
			Labels: map[string]string{
				ClusterLabelKey: name,
				RoleLabelKey:    RoleControlPlane,
			},
			PublishAPIPort: true,
			SocketMount:    p.cfg.Socket,
		}); err != nil {
			return nil, errorf("creating bootstrap container %q: %v", container, err)
		}
	}

	// Point the load balancer at the bootstrap container before running the
	// bootstrap, not after: kubeadm init and the post-init script (CNI,
	// cert-manager, operator) reach the API server through the control plane
	// endpoint, which is the load balancer. With the backend set up front,
	// haproxy forwards as soon as the API server comes up mid-init; configuring
	// it afterwards would deadlock kubeadm's wait for a healthy API server.
	if err := p.configureLoadBalancer(ctx, name, container); err != nil {
		return nil, errorf("configuring load balancer: %v", err)
	}

	if err := p.runBootstrap(ctx, container, userData); err != nil {
		return nil, errorf("bootstrapping %q: %v", container, err)
	}

	ip, err := p.containerIP(ctx, container)
	addrs := []string(nil)
	if err == nil && ip != "" {
		addrs = []string{ip}
	}
	return &cluster.Node{
		ID:        ProviderIDPrefix + container,
		Name:      container,
		Addresses: addrs,
	}, nil
}

// nodeImage returns the kindest/node image for the bootstrap container: the
// configured override, else "kindest/node:<KUBERNETES_VERSION>".
func (p *Provider) nodeImage(spec *cluster.Spec) (string, error) {
	if p.cfg.NodeImage != "" {
		return p.cfg.NodeImage, nil
	}
	if v := spec.Variables[VarKubernetesVersion]; v != "" {
		return "kindest/node:" + v, nil
	}
	return "", errorf("no node image: set docker.nodeImage or the %s variable", VarKubernetesVersion)
}
