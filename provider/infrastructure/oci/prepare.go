package oci

import (
	"context"
	"strconv"

	"github.com/klotho-k8s/klotho/pkg/cluster"
	"github.com/klotho-k8s/klotho/provider/infrastructure"
)

// Prepare validates the required OCI placement settings and the control plane
// endpoint, and returns the endpoint and the template variables the generated
// bundle needs. It allocates nothing, so a failed run has nothing to sweep.
//
// The endpoint must be known here because klotho mints the cluster certificates
// and node zero's cloud-init (which carry it as the kubeadm controlPlaneEndpoint
// and a cert SAN) before Launch. Stock CAPOCI would otherwise only learn the
// endpoint after provisioning its control-plane load balancer, so the endpoint
// is required: supply an address the control-plane LB answers on, or node zero's
// own reserved public IP for a single-node setup (see cluster-template.yaml and
// TESTING.md).
func (p *Provider) Prepare(ctx context.Context, spec *cluster.Spec) (*infrastructure.Prepared, error) {
	if p.cfg.CompartmentID == "" {
		return nil, errorf("compartmentId is required (set oci.compartmentId)")
	}
	if p.cfg.Region == "" {
		return nil, errorf("region is required (set oci.region)")
	}
	if p.cfg.AvailabilityDomain == "" {
		return nil, errorf("availabilityDomain is required (set oci.availabilityDomain)")
	}
	if p.cfg.VCNID == "" {
		return nil, errorf("vcnId is required: klotho boots node zero into an existing VCN and the template points CAPOCI at the same one so it adopts in place (set oci.vcnId)")
	}
	if p.cfg.SubnetID == "" {
		return nil, errorf("subnetId is required (set oci.subnetId)")
	}
	if p.cfg.ImageID == "" {
		return nil, errorf("imageId is required: OCI images are region/tenancy-specific OCIDs, so there is no default. Set oci.imageId to a plain Ubuntu image OCID (install-at-boot adds Kubernetes)")
	}
	if p.cfg.ControlPlaneEndpoint == "" {
		return nil, errorf("controlPlaneEndpoint is required: klotho mints the certificates and node zero's cloud-init before the node boots, and stock CAPOCI learns its endpoint only after provisioning a load balancer. Set oci.controlPlaneEndpoint to an address the control-plane load balancer answers on, or node zero's own reserved public IP")
	}
	if err := p.validateAvailabilityDomain(ctx); err != nil {
		return nil, err
	}

	prepared := &infrastructure.Prepared{
		Endpoint: cluster.Endpoint{Host: p.cfg.ControlPlaneEndpoint, Port: p.port()},
	}
	prepared.Variables = map[string]string{
		VarCompartmentID:     p.cfg.CompartmentID,
		VarResourceID:        p.resourceIdentifier(spec.Name),
		VarVCNID:             p.cfg.VCNID,
		VarSubnetID:          p.cfg.SubnetID,
		VarImageID:           p.cfg.ImageID,
		VarControlPlaneShape: p.shape(),
		VarNodeShape:         p.shape(),
		VarOCPUs:             strconv.FormatFloat(float64(p.ocpus()), 'f', -1, 32),
		VarMemoryGBs:         strconv.FormatFloat(float64(p.memoryGBs()), 'f', -1, 32),
		VarControlPlaneCount: "1",
	}
	return prepared, nil
}

// validateAvailabilityDomain confirms the configured availability domain exists
// in the compartment. The check is advisory: if the identity API cannot be
// reached it is skipped (a wrong AD then fails at Launch with a clear error).
func (p *Provider) validateAvailabilityDomain(ctx context.Context) error {
	ads, err := p.client.ListAvailabilityDomains(ctx, p.cfg.CompartmentID)
	if err != nil {
		// Advisory only: do not block Prepare on a read failure.
		return nil
	}
	for _, ad := range ads {
		if ad.Name == p.cfg.AvailabilityDomain {
			return nil
		}
	}
	names := make([]string, 0, len(ads))
	for _, ad := range ads {
		names = append(names, ad.Name)
	}
	return errorf("availabilityDomain %q not found in the compartment; available: %v", p.cfg.AvailabilityDomain, names)
}
