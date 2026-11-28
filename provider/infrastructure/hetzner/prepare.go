package hetzner

import (
	"context"

	"github.com/klotho-k8s/klotho/pkg/cluster"
	"github.com/klotho-k8s/klotho/provider/infrastructure"
)

// Prepare validates the location, server type, image and SSH keys, requires a
// control plane endpoint, and returns the endpoint and the template variables
// the generated bundle needs. It allocates nothing, so a failed run has nothing
// to sweep.
//
// The endpoint must be known here because klotho mints the cluster certificates
// and node zero's cloud-init (which carry it as the kubeadm controlPlaneEndpoint
// and a cert SAN) before Launch. The flat template disables CAPH's managed load
// balancer (controlPlaneLoadBalancer.enabled: false) to keep the cluster cheap,
// so there is no pre-assignable address and the endpoint is required: the
// cheapest choice is a DNS name you control, pointed at node zero after it boots
// (see cluster-template.yaml and TESTING.md).
func (p *Provider) Prepare(ctx context.Context, _ *cluster.Spec) (*infrastructure.Prepared, error) {
	if p.cfg.Location == "" {
		return nil, errorf("location is required (set hetzner.location, e.g. fsn1)")
	}
	if len(p.cfg.SSHKeys) == 0 {
		return nil, errorf("at least one SSH key name is required (set hetzner.sshKeys): CAPH's HetznerCluster requires an SSH key, and it is the only way into a node to diagnose a failed bootstrap. Create one in the Hetzner project and reference it by name")
	}
	if p.cfg.ControlPlaneEndpoint == "" {
		return nil, errorf("controlPlaneEndpoint is required: klotho mints the certificates and node zero's cloud-init before the node boots, and the flat template disables CAPH's load balancer. Set hetzner.controlPlaneEndpoint to a DNS name you control (cheapest), pointed at node zero after it boots, or an existing load balancer / floating IP address")
	}
	if err := p.validate(ctx); err != nil {
		return nil, err
	}

	prepared := &infrastructure.Prepared{
		Endpoint: cluster.Endpoint{Host: p.cfg.ControlPlaneEndpoint, Port: p.port()},
	}
	prepared.Variables = map[string]string{
		VarLocation:          p.cfg.Location,
		VarControlPlaneType:  p.serverType(),
		VarNodeType:          p.serverType(),
		VarImage:             p.image(),
		VarSSHKey:            p.firstSSHKey(),
		VarControlPlaneCount: "1",
	}
	return prepared, nil
}

// validate confirms the location, server type, image and SSH keys exist. hcloud
// GetByName returns (nil, nil) for a missing resource, so a nil result is a
// not-found.
func (p *Provider) validate(ctx context.Context) error {
	loc, err := p.client.GetLocation(ctx, p.cfg.Location)
	if err != nil {
		return errorf("looking up location %q: %w", p.cfg.Location, err)
	}
	if loc == nil {
		return errorf("location %q not found; choose one of fsn1, nbg1, hel1, ash, hil, sin", p.cfg.Location)
	}

	st, err := p.client.GetServerType(ctx, p.serverType())
	if err != nil {
		return errorf("looking up server type %q: %w", p.serverType(), err)
	}
	if st == nil {
		return errorf("server type %q not found", p.serverType())
	}

	img, err := p.client.GetImage(ctx, p.image())
	if err != nil {
		return errorf("looking up image %q: %w", p.image(), err)
	}
	if img == nil {
		return errorf("image %q not found", p.image())
	}

	for _, name := range p.cfg.SSHKeys {
		key, err := p.client.GetSSHKey(ctx, name)
		if err != nil {
			return errorf("looking up SSH key %q: %w", name, err)
		}
		if key == nil {
			return errorf("SSH key %q not found in the Hetzner project; create it first (names are case-sensitive)", name)
		}
	}
	return nil
}
