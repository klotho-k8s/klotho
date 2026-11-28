package vz

import (
	"context"

	"github.com/klotho-k8s/klotho/pkg/cluster"
	"github.com/klotho-k8s/klotho/provider/infrastructure"
)

// Prepare validates the configuration and resolves the control plane endpoint.
// vz has no load balancer, so the endpoint must be a static address the user
// supplies (vz.endpointHost or the CONTROL_PLANE_ENDPOINT_HOST variable): the
// address node zero will have on the NAT network. Prepare only validates it, it
// never allocates, so Created is always empty. It injects the host, endpoint and
// image as template variables so a generated bundle resolves to the same values
// this provider boots with.
//
// Prepare does not require the daemon to be reachable: the single-host dev layout
// has no host objects to enumerate, and reachability is proven when Launch
// actually boots node zero. This keeps `klotho create` validation fast and
// offline-friendly, matching the libvirt and Hyper-V adapters.
func (p *Provider) Prepare(_ context.Context, spec *cluster.Spec) (*infrastructure.Prepared, error) {
	if spec == nil || spec.Name == "" {
		return nil, errorf("spec has no cluster name")
	}
	r := p.resolve(spec.Variables)
	if r.Image == "" {
		return nil, errorf("no base image: set vz.image or the %s variable to the path, on the Mac, of a raw arm64 cloud image", VarImagePath)
	}
	if r.EndpointHost == "" {
		return nil, errorf("no control plane endpoint: vz has no load balancer, so set vz.endpointHost or the %s variable to the static address node zero will have", VarEndpointHost)
	}
	if p.cfg.Endpoint == "" {
		return nil, errorf("no daemon endpoint: set vz.endpoint to the capvz-daemon's gRPC address (host:port) so klotho can boot node zero")
	}

	return &infrastructure.Prepared{
		Endpoint: cluster.Endpoint{Host: r.EndpointHost, Port: DefaultPort},
		Variables: map[string]string{
			VarHost:         r.Host,
			VarImagePath:    r.Image,
			VarEndpointHost: r.EndpointHost,
		},
	}, nil
}
