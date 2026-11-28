package openstack

import (
	"context"
	"encoding/base64"

	"github.com/klotho-k8s/klotho/pkg/cluster"
	"github.com/klotho-k8s/klotho/provider/infrastructure"
)

// Prepare validates the cloud inputs, resolves the node network and external
// network to IDs, confirms the flavor and image exist, and determines the
// control plane endpoint.
//
// The endpoint must be known here because klotho mints the cluster certificates
// and node zero's cloud-init (which carry it as the kubeadm controlPlaneEndpoint
// and a certificate SAN) before Launch. When no ControlPlaneEndpoint is
// configured, Prepare allocates a Neutron floating IP from the external network
// and returns its address; Launch then associates that floating IP with node
// zero once it is up, and CAPO keeps it (the template's
// apiServer.floatingIP) after adoption — the cheapest endpoint, with no Octavia
// load balancer. A configured ControlPlaneEndpoint (an existing floating IP you
// have pointed at node zero, an Octavia VIP, or a DNS name) is used as-is and no
// floating IP is allocated.
//
// It returns the endpoint, anything it allocated (so a failed run can be swept),
// and the template variables the generated bundle needs.
func (p *Provider) Prepare(ctx context.Context, _ *cluster.Spec) (*infrastructure.Prepared, error) {
	if p.cfg.Cloud == "" {
		return nil, errorf("cloud is required (set openstack.cloud, the clouds.yaml entry)")
	}
	if p.cfg.Network == "" {
		return nil, errorf("network is required (set openstack.network, the Neutron network node zero joins)")
	}
	if p.cfg.Flavor == "" {
		return nil, errorf("flavor is required (set openstack.flavor)")
	}
	if p.cfg.Image == "" {
		return nil, errorf("image is required (set openstack.image)")
	}
	if p.cfg.ExternalNetwork == "" {
		return nil, errorf("externalNetwork is required (set openstack.externalNetwork, the public network the control plane floating IP comes from)")
	}

	networkID, err := p.client.NetworkIDByName(ctx, p.cfg.Network)
	if err != nil {
		return nil, errorf("resolving network %q: %w", p.cfg.Network, err)
	}
	externalNetworkID, err := p.client.NetworkIDByName(ctx, p.cfg.ExternalNetwork)
	if err != nil {
		return nil, errorf("resolving external network %q: %w", p.cfg.ExternalNetwork, err)
	}
	if _, err := p.client.FlavorIDByName(ctx, p.cfg.Flavor); err != nil {
		return nil, errorf("resolving flavor %q: %w", p.cfg.Flavor, err)
	}
	if _, err := p.client.ImageIDByName(ctx, p.cfg.Image); err != nil {
		return nil, errorf("resolving image %q: %w", p.cfg.Image, err)
	}

	prepared := &infrastructure.Prepared{}
	endpointHost, fip, err := p.resolveEndpoint(ctx, externalNetworkID)
	if err != nil {
		return nil, err
	}
	prepared.Endpoint = cluster.Endpoint{Host: endpointHost, Port: p.port()}
	if fip != nil {
		p.fipID = fip.ID
		prepared.Created = []cluster.Resource{{Kind: ResourceKindFloatingIP, ID: fip.ID, Name: fip.Address}}
	}

	prepared.Variables = map[string]string{
		VarCloud:                  p.cfg.Cloud,
		VarNetworkID:              networkID,
		VarExternalNetworkID:      externalNetworkID,
		VarControlPlaneFloatingIP: endpointHost,
		VarImageName:              p.cfg.Image,
		VarControlPlaneFlavor:     p.cfg.Flavor,
		VarNodeFlavor:             p.cfg.Flavor,
		VarSSHKeyName:             p.cfg.SSHKeyName,
		VarControlPlaneCount:      "1",
	}
	if len(p.cfg.CloudsYAMLData) > 0 {
		prepared.Variables[VarCloudsYAMLB64] = base64.StdEncoding.EncodeToString(p.cfg.CloudsYAMLData)
	}
	return prepared, nil
}

// resolveEndpoint returns the control plane endpoint host and, when it allocated
// one, the floating IP. A configured ControlPlaneEndpoint is used as-is (nothing
// allocated); otherwise a floating IP is allocated from the external network.
func (p *Provider) resolveEndpoint(ctx context.Context, externalNetworkID string) (string, *FloatingIP, error) {
	if p.cfg.ControlPlaneEndpoint != "" {
		return p.cfg.ControlPlaneEndpoint, nil, nil
	}
	fip, err := p.client.AllocateFloatingIP(ctx, externalNetworkID, "klotho control plane endpoint")
	if err != nil {
		return "", nil, errorf("allocating a control plane floating IP: %w", err)
	}
	if fip.Address == "" {
		return "", nil, errorf("allocated floating IP %s has no address", fip.ID)
	}
	return fip.Address, fip, nil
}
