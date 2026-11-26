package docker

import (
	"context"

	"github.com/klotho-k8s/klotho/pkg/cluster"
	"github.com/klotho-k8s/klotho/provider/infrastructure"
)

// Resource kinds recorded in a Plan / state.
const (
	ResourceKindContainer = "container"
	ResourceKindNetwork   = "network"
)

// Prepare ensures the Docker network exists and creates the cluster's load
// balancer container so a stable control plane endpoint is known before the
// bootstrap node boots. CAPD adopts this same "<cluster>-lb" haproxy container
// when it later reconciles the DockerCluster.
//
// The endpoint is the load balancer's address on the Docker network, port
// DefaultPort. Launch points the load balancer at the bootstrap node once it
// is up.
func (p *Provider) Prepare(ctx context.Context, spec *cluster.Spec) (*infrastructure.Prepared, error) {
	if spec == nil || spec.Name == "" {
		return nil, errorf("spec has no cluster name")
	}
	name := spec.Name
	net := p.network()
	if err := p.rt.EnsureNetwork(ctx, net); err != nil {
		return nil, errorf("ensuring network %q: %v", net, err)
	}

	created := []cluster.Resource{{Kind: ResourceKindNetwork, ID: net, Name: net}}

	lb := LBName(name)
	if _, ok, err := p.rt.Inspect(ctx, lb); err != nil {
		return nil, err
	} else if !ok {
		if _, err := p.rt.RunNode(ctx, RunOpts{
			Name:           lb,
			Image:          p.lbImage(),
			Role:           RoleLoadBalancer,
			Network:        net,
			Labels:         map[string]string{ClusterLabelKey: name, RoleLabelKey: RoleLoadBalancer},
			PublishAPIPort: true,
		}); err != nil {
			return nil, errorf("creating load balancer %q: %v", lb, err)
		}
	}
	created = append(created, cluster.Resource{Kind: ResourceKindContainer, ID: lb, Name: lb})

	host, err := p.containerIP(ctx, lb)
	if err != nil {
		return nil, err
	}

	prepared := &infrastructure.Prepared{
		Endpoint: cluster.Endpoint{Host: host, Port: DefaultPort},
		Created:  created,
	}

	// The load balancer's container-network address is the cluster's
	// controlPlaneEndpoint, but on a rootless runtime the host cannot route to
	// the container network. The load balancer also publishes its port to an
	// ephemeral host port, which the host can reach; report it as the
	// host-reachable client endpoint so klotho dials it for its own API calls.
	clientHost, clientPort, err := p.rt.PublishedPort(ctx, lb, DefaultPort)
	if err != nil {
		return nil, errorf("reading the load balancer's published host port: %v", err)
	}
	prepared.ClientEndpoint = cluster.Endpoint{Host: clientHost, Port: clientPort}

	return prepared, nil
}

// containerIP returns a running container's address on its Docker network.
func (p *Provider) containerIP(ctx context.Context, name string) (string, error) {
	c, ok, err := p.rt.Inspect(ctx, name)
	if err != nil {
		return "", err
	}
	if !ok {
		return "", errorf("container %q not found", name)
	}
	if c.IP == "" {
		return "", errorf("container %q has no network address yet", name)
	}
	return c.IP, nil
}
