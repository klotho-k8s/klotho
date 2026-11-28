package digitalocean

import (
	"context"
	"time"

	"github.com/digitalocean/godo"

	"github.com/klotho-k8s/klotho/pkg/cluster"
	"github.com/klotho-k8s/klotho/provider/infrastructure"
)

// ResourceKindLoadBalancer is the Plan/Created kind for a DigitalOcean load
// balancer.
const ResourceKindLoadBalancer = "loadbalancer"

// Prepare validates the region, size and image, and resolves the control plane
// endpoint. Stock CAPDO always fronts the API server with a managed
// DigitalOcean load balancer and overwrites spec.controlPlaneEndpoint with the
// load balancer's IP, so — unless the user supplies an endpoint or an existing
// load balancer — klotho creates the load balancer here, before the
// certificates and node zero's cloud-init are minted, and injects its UUID so
// CAPDO's DOCluster adopts it (spec.network.apiServerLoadbalancers.resourceId)
// rather than provisioning a second. The load balancer targets the apiserver
// role tag, which Launch puts on node zero.
//
// It returns the endpoint, anything it created (so a failed run can be swept),
// and the template variables the generated bundle needs.
func (p *Provider) Prepare(ctx context.Context, spec *cluster.Spec) (*infrastructure.Prepared, error) {
	if p.cfg.Region == "" {
		return nil, errorf("region is required (set digitalocean.region)")
	}
	if err := p.validate(ctx); err != nil {
		return nil, err
	}

	endpoint, created, lbID, err := p.resolveEndpoint(ctx, spec.Name)
	if err != nil {
		return nil, err
	}

	prepared := &infrastructure.Prepared{
		Endpoint: endpoint,
		Created:  created,
		Variables: map[string]string{
			VarRegion:            p.cfg.Region,
			VarControlPlaneSize:  p.size(),
			VarNodeSize:          p.size(),
			VarImage:             p.image(),
			VarLoadBalancerID:    lbID,
			VarControlPlaneCount: "1",
		},
	}
	return prepared, nil
}

// validate confirms the region, size and image exist.
func (p *Provider) validate(ctx context.Context) error {
	regions, err := p.client.ListRegions(ctx)
	if err != nil {
		return errorf("listing regions: %w", err)
	}
	if !slugIn(regions, p.cfg.Region) {
		return errorf("region %q not found (or not available on this account)", p.cfg.Region)
	}

	sizes, err := p.client.ListSizes(ctx)
	if err != nil {
		return errorf("listing sizes: %w", err)
	}
	if !sizeIn(sizes, p.size()) {
		return errorf("size %q not found", p.size())
	}

	// An image given as a numeric ID is trusted (no per-ID lookup); a slug is
	// confirmed to exist.
	if _, err := parseInt(p.image()); err != nil {
		if _, err := p.client.GetImageBySlug(ctx, p.image()); err != nil {
			return errorf("image %q: %w", p.image(), err)
		}
	}
	return nil
}

// resolveEndpoint returns the control plane endpoint, anything created, and the
// load balancer UUID to inject. A configured endpoint is used as-is; an existing
// load balancer is looked up for its IP; otherwise a load balancer is created.
func (p *Provider) resolveEndpoint(ctx context.Context, clusterName string) (cluster.Endpoint, []cluster.Resource, string, error) {
	if p.cfg.ControlPlaneEndpoint != "" {
		return cluster.Endpoint{Host: p.cfg.ControlPlaneEndpoint, Port: p.port()}, nil, p.cfg.LoadBalancerID, nil
	}
	if p.cfg.LoadBalancerID != "" {
		lb, err := p.client.GetLoadBalancer(ctx, p.cfg.LoadBalancerID)
		if err != nil {
			return cluster.Endpoint{}, nil, "", errorf("looking up load balancer %q: %w", p.cfg.LoadBalancerID, err)
		}
		if lb.IP == "" {
			return cluster.Endpoint{}, nil, "", errorf("load balancer %q has no IP yet; retry once it is active", p.cfg.LoadBalancerID)
		}
		return cluster.Endpoint{Host: lb.IP, Port: p.port()}, nil, lb.ID, nil
	}

	lb, err := p.createLoadBalancer(ctx, clusterName)
	if err != nil {
		return cluster.Endpoint{}, nil, "", err
	}
	created := []cluster.Resource{{Kind: ResourceKindLoadBalancer, ID: lb.ID, Name: lb.Name}}
	return cluster.Endpoint{Host: lb.IP, Port: p.port()}, created, lb.ID, nil
}

// createLoadBalancer provisions the control plane load balancer and waits for
// its IP. It forwards TCP p.port() to the droplets carrying the apiserver tag,
// which Launch puts on node zero and CAPDO puts on the machines it creates.
func (p *Provider) createLoadBalancer(ctx context.Context, clusterName string) (*godo.LoadBalancer, error) {
	req := &godo.LoadBalancerRequest{
		Name:      loadBalancerName(clusterName),
		Algorithm: "round_robin",
		Region:    p.cfg.Region,
		ForwardingRules: []godo.ForwardingRule{{
			EntryProtocol:  "tcp",
			EntryPort:      p.port(),
			TargetProtocol: "tcp",
			TargetPort:     p.port(),
		}},
		HealthCheck: &godo.HealthCheck{
			Protocol:               "tcp",
			Port:                   p.port(),
			CheckIntervalSeconds:   10,
			ResponseTimeoutSeconds: 5,
			UnhealthyThreshold:     3,
			HealthyThreshold:       5,
		},
		Tag:     apiServerTag(clusterName),
		Tags:    []string{clusterTag(clusterName)},
		VPCUUID: p.cfg.VPCUUID,
	}
	lb, err := p.client.CreateLoadBalancer(ctx, req)
	if err != nil {
		return nil, errorf("creating the control plane load balancer: %w", err)
	}
	if lb.IP != "" {
		return lb, nil
	}
	return p.waitLoadBalancerIP(ctx, lb.ID)
}

// waitLoadBalancerIP polls until the load balancer has been assigned an IP.
func (p *Provider) waitLoadBalancerIP(ctx context.Context, id string) (*godo.LoadBalancer, error) {
	deadline := time.Now().Add(p.launchWait)
	for {
		lb, err := p.client.GetLoadBalancer(ctx, id)
		if err != nil {
			return nil, errorf("reading load balancer %q: %w", id, err)
		}
		if lb.IP != "" {
			return lb, nil
		}
		if !time.Now().Before(deadline) {
			return nil, errorf("timed out waiting for load balancer %q to be assigned an IP", id)
		}
		select {
		case <-ctx.Done():
			return nil, ctx.Err()
		case <-time.After(p.launchPoll):
		}
	}
}

func slugIn(regions []godo.Region, slug string) bool {
	for i := range regions {
		if regions[i].Slug == slug {
			return true
		}
	}
	return false
}

func sizeIn(sizes []godo.Size, slug string) bool {
	for i := range sizes {
		if sizes[i].Slug == slug {
			return true
		}
	}
	return false
}
