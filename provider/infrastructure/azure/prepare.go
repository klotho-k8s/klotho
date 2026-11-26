package azure

import (
	"context"
	"encoding/base64"

	"github.com/klotho-k8s/klotho/pkg/cluster"
	"github.com/klotho-k8s/klotho/provider/infrastructure"
)

// Prepare validates the subscription, resource group, virtual network and
// subnet, allocates a static public IP for node zero when no control plane
// endpoint is configured, and returns the endpoint together with the template
// variables the generated bundle needs.
//
// The endpoint must be known here because klotho mints the cluster
// certificates and node zero's cloud-init (which carry it as the kubeadm
// controlPlaneEndpoint and a cert SAN) before Launch. Azure has no free,
// pre-assignable floating IP, so klotho allocates a static Standard public IP
// now and associates it with node zero's NIC in Launch; that address is the
// endpoint. A configured ControlPlaneEndpoint (an existing LB IP or a DNS name)
// is used as-is instead, and the public IP still gives node zero reachability
// and outbound.
func (p *Provider) Prepare(ctx context.Context, spec *cluster.Spec) (*infrastructure.Prepared, error) {
	if p.cfg.SubscriptionID == "" {
		return nil, errorf("subscriptionId is required (set azure.subscriptionId)")
	}
	if p.cfg.Location == "" {
		return nil, errorf("location is required (set azure.location)")
	}
	if p.cfg.ResourceGroup == "" {
		return nil, errorf("resourceGroup is required (set azure.resourceGroup); klotho does not create resource groups")
	}
	if p.cfg.VNet == "" || p.cfg.Subnet == "" {
		return nil, errorf("vnet and subnet are required (set azure.vnet and azure.subnet); klotho does not create a network")
	}
	if _, err := p.api.SubnetID(ctx, p.cfg.ResourceGroup, p.cfg.VNet, p.cfg.Subnet); err != nil {
		return nil, errorf("validating network %s/%s/%s: %w", p.cfg.ResourceGroup, p.cfg.VNet, p.cfg.Subnet, err)
	}

	prepared := &infrastructure.Prepared{}

	endpoint, created, err := p.resolveEndpoint(ctx, spec.Name)
	if err != nil {
		return nil, err
	}
	prepared.Endpoint = endpoint
	prepared.Created = created

	img := p.image()
	prepared.Variables = map[string]string{
		VarSubscriptionID:     p.cfg.SubscriptionID,
		VarLocation:           p.cfg.Location,
		VarResourceGroup:      p.cfg.ResourceGroup,
		VarVNet:               p.cfg.VNet,
		VarSubnet:             p.cfg.Subnet,
		VarControlPlaneVMSize: p.vmSize(),
		VarNodeVMSize:         p.vmSize(),
		VarImagePublisher:     img.Publisher,
		VarImageOffer:         img.Offer,
		VarImageSKU:           img.SKU,
		VarImageVersion:       img.Version,
		VarControlPlaneCount:  "1",
		VarIdentityName:       IdentityName(spec.Name),
		VarClientSecretName:   ClientSecretName(spec.Name),
	}
	if p.cfg.ClientID != "" {
		prepared.Variables[VarClientID] = p.cfg.ClientID
	}
	if p.cfg.TenantID != "" {
		prepared.Variables[VarTenantID] = p.cfg.TenantID
	}
	if p.cfg.ClientSecret != "" {
		// The AzureClusterIdentity's Secret carries the base64-encoded client
		// secret. This is a generation-time variable only; create.go does not
		// persist prepared.Variables into the cluster's state record.
		prepared.Variables[VarClientSecretB64] = base64.StdEncoding.EncodeToString([]byte(p.cfg.ClientSecret))
	}
	return prepared, nil
}

// IdentityName is the AzureClusterIdentity object's name for a cluster.
func IdentityName(cluster string) string { return cluster + "-identity" }

// ClientSecretName is the name of the Secret holding the service principal's
// client secret for a cluster.
func ClientSecretName(cluster string) string { return cluster + "-azure-client-secret" }

// resolveEndpoint returns the control plane endpoint. A configured endpoint is
// validated only; otherwise a static public IP is allocated for node zero and
// recorded as created.
func (p *Provider) resolveEndpoint(ctx context.Context, clusterName string) (cluster.Endpoint, []cluster.Resource, error) {
	pip, err := p.api.EnsurePublicIP(ctx, PublicIPParams{
		ResourceGroup: p.cfg.ResourceGroup,
		Location:      p.cfg.Location,
		Name:          publicIPName(clusterName),
		Tags:          tags(clusterName),
	})
	if err != nil {
		return cluster.Endpoint{}, nil, errorf("allocating a public IP: %w", err)
	}
	created := []cluster.Resource{{Kind: ResourceKindPublicIP, ID: pip.Name, Name: pip.Address}}

	if p.cfg.ControlPlaneEndpoint != "" {
		return cluster.Endpoint{Host: p.cfg.ControlPlaneEndpoint, Port: p.port()}, created, nil
	}
	if pip.Address == "" {
		return cluster.Endpoint{}, created, errorf("allocated public IP %q has no address yet; set azure.controlPlaneEndpoint", pip.Name)
	}
	return cluster.Endpoint{Host: pip.Address, Port: p.port()}, created, nil
}
