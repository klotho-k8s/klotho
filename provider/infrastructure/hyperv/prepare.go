package hyperv

import (
	"context"
	"os"
	"slices"

	"github.com/klotho-k8s/klotho/pkg/cluster"
	"github.com/klotho-k8s/klotho/provider/infrastructure"
)

// Prepare validates that the switch and base image exist and resolves the
// control plane endpoint. Hyper-V has no load balancer, so the endpoint must be
// a static address the user supplies (hyperv.endpointHost or the
// CONTROL_PLANE_ENDPOINT_HOST variable); Prepare only validates it, it never
// allocates. It injects the switch, image and endpoint as template variables so
// a generated bundle resolves to the same values this provider boots with.
func (p *Provider) Prepare(ctx context.Context, spec *cluster.Spec) (*infrastructure.Prepared, error) {
	if spec == nil || spec.Name == "" {
		return nil, errorf("spec has no cluster name")
	}
	r := p.resolve(spec.Variables)
	if r.Image == "" {
		return nil, errorf("no base image: set hyperv.image or the %s variable", VarImagePath)
	}
	if _, err := os.Stat(r.Image); err != nil {
		return nil, errorf("base image %q is not readable on this host: %v", r.Image, err)
	}
	if r.EndpointHost == "" {
		return nil, errorf("no control plane endpoint: Hyper-V has no load balancer, so set hyperv.endpointHost or the %s variable to a static address", VarEndpointHost)
	}

	switches, err := p.client.ListSwitches(ctx)
	if err != nil {
		return nil, errorf("listing virtual switches: %w", err)
	}
	if !slices.Contains(switches, r.Switch) {
		return nil, errorf("virtual switch %q does not exist on this host (have: %v)", r.Switch, switches)
	}

	return &infrastructure.Prepared{
		Endpoint: cluster.Endpoint{Host: r.EndpointHost, Port: DefaultPort},
		Variables: map[string]string{
			VarEndpointHost: r.EndpointHost,
			VarSwitchName:   r.Switch,
			VarImagePath:    r.Image,
		},
	}, nil
}
