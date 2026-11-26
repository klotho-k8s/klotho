package linode

import (
	"context"

	"github.com/klotho-k8s/klotho/pkg/cluster"
	"github.com/klotho-k8s/klotho/provider/infrastructure"
)

// metadataCapability is the Region capability that indicates the Linode
// Metadata service (how klotho's cloud-init reaches node zero) is available.
const metadataCapability = "Metadata"

// cloudInitCapability is the Image capability that indicates the image runs
// cloud-init, so the Metadata user-data is processed at boot.
const cloudInitCapability = "cloud-init"

// Prepare validates the region, instance type and image, confirms the region
// and image support the Metadata/cloud-init path klotho boots node zero
// through, and returns the control plane endpoint and the template variables
// the generated bundle needs. It allocates nothing, so a failed run has nothing
// to sweep.
//
// The endpoint must be known here because klotho mints the cluster
// certificates and node zero's cloud-init (which carry it as the kubeadm
// controlPlaneEndpoint and a cert SAN) before Launch. Linode has no free,
// pre-assignable floating IP, so the endpoint is required: the cheapest choice
// is a DNS name the user controls, pointed at node zero after it boots, with
// the template's loadBalancerType left "external" so CAPL provisions no
// NodeBalancer (see cluster-template.yaml and TESTING.md).
func (p *Provider) Prepare(ctx context.Context, spec *cluster.Spec) (*infrastructure.Prepared, error) {
	if p.cfg.Region == "" {
		return nil, errorf("region is required (set linode.region)")
	}
	if p.cfg.ControlPlaneEndpoint == "" {
		return nil, errorf("controlPlaneEndpoint is required: klotho mints the certificates and node zero's cloud-init before the node boots, and Linode has no free pre-assignable IP. Set linode.controlPlaneEndpoint to a DNS name you control (cheapest, no NodeBalancer) or an existing load balancer address")
	}
	if err := p.validate(ctx); err != nil {
		return nil, err
	}

	prepared := &infrastructure.Prepared{
		Endpoint: cluster.Endpoint{Host: p.cfg.ControlPlaneEndpoint, Port: p.port()},
	}
	prepared.Variables = map[string]string{
		VarRegion:            p.cfg.Region,
		VarControlPlaneType:  p.instanceType(),
		VarNodeType:          p.instanceType(),
		VarImage:             p.image(),
		VarControlPlaneCount: "1",
	}
	return prepared, nil
}

// validate confirms the region, type and image exist and that the Metadata/
// cloud-init delivery path is available. The capability checks are advisory:
// if the API cannot be reached for them the resource-existence checks have
// already failed, so a capability that cannot be read is treated as present.
func (p *Provider) validate(ctx context.Context) error {
	region, err := p.client.GetRegion(ctx, p.cfg.Region)
	if err != nil {
		return errorf("region %q: %w", p.cfg.Region, err)
	}
	if !hasCapability(region.Capabilities, metadataCapability) {
		return errorf("region %q does not support the Metadata service, which klotho uses to deliver cloud-init; choose a region that lists the %q capability", p.cfg.Region, metadataCapability)
	}

	if _, err := p.client.GetType(ctx, p.instanceType()); err != nil {
		return errorf("instance type %q: %w", p.instanceType(), err)
	}

	img, err := p.client.GetImage(ctx, p.image())
	if err != nil {
		return errorf("image %q: %w", p.image(), err)
	}
	if !hasCapability(img.Capabilities, cloudInitCapability) {
		return errorf("image %q does not support cloud-init, so the Metadata user-data would be ignored; choose a cloud-init image", p.image())
	}
	return nil
}

func hasCapability(caps []string, want string) bool {
	for _, c := range caps {
		if c == want {
			return true
		}
	}
	return false
}
