package azure

import (
	"context"
	"encoding/base64"
	"errors"
	"net/http"

	"github.com/Azure/azure-sdk-for-go/sdk/azcore"
	"github.com/Azure/azure-sdk-for-go/sdk/azcore/to"
	"github.com/Azure/azure-sdk-for-go/sdk/azidentity"
	"github.com/Azure/azure-sdk-for-go/sdk/resourcemanager/compute/armcompute/v6"
	"github.com/Azure/azure-sdk-for-go/sdk/resourcemanager/network/armnetwork/v6"

	"github.com/klotho-k8s/klotho/pkg/cluster"
	"github.com/klotho-k8s/klotho/provider/infrastructure"
)

// Client is the Azure-SDK-backed API. It talks to Azure Resource Manager
// with a service principal (client secret) credential.
type Client struct {
	vms     *armcompute.VirtualMachinesClient
	nics    *armnetwork.InterfacesClient
	pips    *armnetwork.PublicIPAddressesClient
	vnets   *armnetwork.VirtualNetworksClient
	subnets *armnetwork.SubnetsClient
}

var _ API = (*Client)(nil)

// NewClient builds an Azure client for the subscription in cfg, authenticated
// with the configured service principal. It makes no API calls.
func NewClient(cfg Config) (*Client, error) {
	if cfg.SubscriptionID == "" {
		return nil, errorf("subscriptionId is required (set azure.subscriptionId)")
	}
	if cfg.TenantID == "" || cfg.ClientID == "" || cfg.ClientSecret == "" {
		return nil, errorf("a service principal is required: set azure.tenantId, azure.clientId and AZURE_CLIENT_SECRET")
	}
	cred, err := azidentity.NewClientSecretCredential(cfg.TenantID, cfg.ClientID, cfg.ClientSecret, nil)
	if err != nil {
		return nil, errorf("building the Azure credential: %w", err)
	}
	vms, err := armcompute.NewVirtualMachinesClient(cfg.SubscriptionID, cred, nil)
	if err != nil {
		return nil, errorf("building the VM client: %w", err)
	}
	nics, err := armnetwork.NewInterfacesClient(cfg.SubscriptionID, cred, nil)
	if err != nil {
		return nil, errorf("building the NIC client: %w", err)
	}
	pips, err := armnetwork.NewPublicIPAddressesClient(cfg.SubscriptionID, cred, nil)
	if err != nil {
		return nil, errorf("building the public IP client: %w", err)
	}
	vnets, err := armnetwork.NewVirtualNetworksClient(cfg.SubscriptionID, cred, nil)
	if err != nil {
		return nil, errorf("building the virtual network client: %w", err)
	}
	subnets, err := armnetwork.NewSubnetsClient(cfg.SubscriptionID, cred, nil)
	if err != nil {
		return nil, errorf("building the subnet client: %w", err)
	}
	return &Client{vms: vms, nics: nics, pips: pips, vnets: vnets, subnets: subnets}, nil
}

// SubnetID confirms the resource group, virtual network and subnet exist and
// returns the subnet's ARM resource ID.
func (c *Client) SubnetID(ctx context.Context, resourceGroup, vnet, subnet string) (string, error) {
	resp, err := c.subnets.Get(ctx, resourceGroup, vnet, subnet, nil)
	if err != nil {
		return "", err
	}
	if resp.ID == nil {
		return "", errorf("subnet %s/%s/%s has no ID", resourceGroup, vnet, subnet)
	}
	return *resp.ID, nil
}

// EnsurePublicIP creates (or returns an existing) static Standard public IP.
func (c *Client) EnsurePublicIP(ctx context.Context, p PublicIPParams) (PublicIP, error) {
	if existing, err := c.pips.Get(ctx, p.ResourceGroup, p.Name, nil); err == nil {
		return publicIPView(existing.PublicIPAddress), nil
	} else if !isNotFound(err) {
		return PublicIP{}, err
	}
	poller, err := c.pips.BeginCreateOrUpdate(ctx, p.ResourceGroup, p.Name, armnetwork.PublicIPAddress{
		Location: to.Ptr(p.Location),
		SKU:      &armnetwork.PublicIPAddressSKU{Name: to.Ptr(armnetwork.PublicIPAddressSKUNameStandard)},
		Properties: &armnetwork.PublicIPAddressPropertiesFormat{
			PublicIPAllocationMethod: to.Ptr(armnetwork.IPAllocationMethodStatic),
		},
		Tags: toTags(p.Tags),
	}, nil)
	if err != nil {
		return PublicIP{}, err
	}
	resp, err := poller.PollUntilDone(ctx, nil)
	if err != nil {
		return PublicIP{}, err
	}
	return publicIPView(resp.PublicIPAddress), nil
}

// CreateNIC creates a NIC in the subnet, optionally associating a public IP.
func (c *Client) CreateNIC(ctx context.Context, p NICParams) (string, error) {
	ipConfig := &armnetwork.InterfaceIPConfiguration{
		Name: to.Ptr("ipconfig1"),
		Properties: &armnetwork.InterfaceIPConfigurationPropertiesFormat{
			Subnet:                    &armnetwork.Subnet{ID: to.Ptr(p.SubnetID)},
			PrivateIPAllocationMethod: to.Ptr(armnetwork.IPAllocationMethodDynamic),
		},
	}
	if p.PublicIPID != "" {
		ipConfig.Properties.PublicIPAddress = &armnetwork.PublicIPAddress{ID: to.Ptr(p.PublicIPID)}
	}
	poller, err := c.nics.BeginCreateOrUpdate(ctx, p.ResourceGroup, p.Name, armnetwork.Interface{
		Location: to.Ptr(p.Location),
		Properties: &armnetwork.InterfacePropertiesFormat{
			IPConfigurations: []*armnetwork.InterfaceIPConfiguration{ipConfig},
		},
		Tags: toTags(p.Tags),
	}, nil)
	if err != nil {
		return "", err
	}
	resp, err := poller.PollUntilDone(ctx, nil)
	if err != nil {
		return "", err
	}
	if resp.ID == nil {
		return "", errorf("created NIC %s has no ID", p.Name)
	}
	return *resp.ID, nil
}

// CreateVM creates the bootstrap VM.
func (c *Client) CreateVM(ctx context.Context, p VMParams) (VM, error) {
	osProfile := &armcompute.OSProfile{
		ComputerName:  to.Ptr(p.Name),
		AdminUsername: to.Ptr(p.AdminUsername),
		CustomData:    to.Ptr(base64.StdEncoding.EncodeToString(p.CustomData)),
	}
	if p.SSHPublicKey != "" {
		osProfile.LinuxConfiguration = &armcompute.LinuxConfiguration{
			DisablePasswordAuthentication: to.Ptr(true),
			SSH: &armcompute.SSHConfiguration{
				PublicKeys: []*armcompute.SSHPublicKey{{
					Path:    to.Ptr("/home/" + p.AdminUsername + "/.ssh/authorized_keys"),
					KeyData: to.Ptr(p.SSHPublicKey),
				}},
			},
		}
	} else {
		osProfile.AdminPassword = to.Ptr(p.AdminPassword)
	}

	vm := armcompute.VirtualMachine{
		Location: to.Ptr(p.Location),
		Properties: &armcompute.VirtualMachineProperties{
			HardwareProfile: &armcompute.HardwareProfile{
				VMSize: to.Ptr(armcompute.VirtualMachineSizeTypes(p.Size)),
			},
			StorageProfile: &armcompute.StorageProfile{
				ImageReference: &armcompute.ImageReference{
					Publisher: to.Ptr(p.Image.Publisher),
					Offer:     to.Ptr(p.Image.Offer),
					SKU:       to.Ptr(p.Image.SKU),
					Version:   to.Ptr(p.Image.Version),
				},
			},
			OSProfile: osProfile,
			NetworkProfile: &armcompute.NetworkProfile{
				NetworkInterfaces: []*armcompute.NetworkInterfaceReference{{
					ID: to.Ptr(p.NICID),
					Properties: &armcompute.NetworkInterfaceReferenceProperties{
						Primary: to.Ptr(true),
					},
				}},
			},
		},
		Tags: toTags(p.Tags),
	}
	if p.Spot {
		vm.Properties.Priority = to.Ptr(armcompute.VirtualMachinePriorityTypesSpot)
		vm.Properties.EvictionPolicy = to.Ptr(armcompute.VirtualMachineEvictionPolicyTypesDeallocate)
	}

	poller, err := c.vms.BeginCreateOrUpdate(ctx, p.ResourceGroup, p.Name, vm, nil)
	if err != nil {
		return VM{}, err
	}
	resp, err := poller.PollUntilDone(ctx, nil)
	if err != nil {
		return VM{}, err
	}
	return vmView(resp.VirtualMachine), nil
}

// GetVM returns the VM by name, or (nil, nil) when it does not exist.
func (c *Client) GetVM(ctx context.Context, resourceGroup, name string) (*VM, error) {
	resp, err := c.vms.Get(ctx, resourceGroup, name, nil)
	if err != nil {
		if isNotFound(err) {
			return nil, nil
		}
		return nil, err
	}
	v := vmView(resp.VirtualMachine)
	return &v, nil
}

// ListClusterResources lists the VMs, NICs and public IPs in the resource group
// carrying the given ownership tag value.
func (c *Client) ListClusterResources(ctx context.Context, resourceGroup, clusterTag string) ([]cluster.Resource, error) {
	var out []cluster.Resource

	vmPager := c.vms.NewListPager(resourceGroup, nil)
	for vmPager.More() {
		page, err := vmPager.NextPage(ctx)
		if err != nil {
			return nil, err
		}
		for _, v := range page.Value {
			if v != nil && hasTag(v.Tags, clusterTag) && v.Name != nil {
				out = append(out, cluster.Resource{Kind: ResourceKindVM, ID: *v.Name, Name: *v.Name})
			}
		}
	}

	nicPager := c.nics.NewListPager(resourceGroup, nil)
	for nicPager.More() {
		page, err := nicPager.NextPage(ctx)
		if err != nil {
			return nil, err
		}
		for _, n := range page.Value {
			if n != nil && hasTag(n.Tags, clusterTag) && n.Name != nil {
				out = append(out, cluster.Resource{Kind: ResourceKindNIC, ID: *n.Name, Name: *n.Name})
			}
		}
	}

	pipPager := c.pips.NewListPager(resourceGroup, nil)
	for pipPager.More() {
		page, err := pipPager.NextPage(ctx)
		if err != nil {
			return nil, err
		}
		for _, ip := range page.Value {
			if ip != nil && hasTag(ip.Tags, clusterTag) && ip.Name != nil {
				out = append(out, cluster.Resource{Kind: ResourceKindPublicIP, ID: *ip.Name, Name: *ip.Name})
			}
		}
	}
	return out, nil
}

// DeleteVM deletes a VM by name; a not-found VM is not an error.
func (c *Client) DeleteVM(ctx context.Context, resourceGroup, name string) error {
	poller, err := c.vms.BeginDelete(ctx, resourceGroup, name, nil)
	if err != nil {
		if isNotFound(err) {
			return nil
		}
		return err
	}
	_, err = poller.PollUntilDone(ctx, nil)
	return err
}

// DeleteNIC deletes a NIC by name; a not-found NIC is not an error.
func (c *Client) DeleteNIC(ctx context.Context, resourceGroup, name string) error {
	poller, err := c.nics.BeginDelete(ctx, resourceGroup, name, nil)
	if err != nil {
		if isNotFound(err) {
			return nil
		}
		return err
	}
	_, err = poller.PollUntilDone(ctx, nil)
	return err
}

// DeletePublicIP deletes a public IP by name; a not-found IP is not an error.
func (c *Client) DeletePublicIP(ctx context.Context, resourceGroup, name string) error {
	poller, err := c.pips.BeginDelete(ctx, resourceGroup, name, nil)
	if err != nil {
		if isNotFound(err) {
			return nil
		}
		return err
	}
	_, err = poller.PollUntilDone(ctx, nil)
	return err
}

// ListVirtualNetworks lists the virtual networks in a resource group.
func (c *Client) ListVirtualNetworks(ctx context.Context, resourceGroup string) ([]infrastructure.Item, error) {
	var out []infrastructure.Item
	pager := c.vnets.NewListPager(resourceGroup, nil)
	for pager.More() {
		page, err := pager.NextPage(ctx)
		if err != nil {
			return nil, err
		}
		for _, v := range page.Value {
			if v == nil || v.Name == nil {
				continue
			}
			item := infrastructure.Item{ID: deref(v.ID), Name: *v.Name}
			if v.Properties != nil && v.Properties.AddressSpace != nil && len(v.Properties.AddressSpace.AddressPrefixes) > 0 {
				item.Attrs = map[string]string{"cidr": deref(v.Properties.AddressSpace.AddressPrefixes[0])}
			}
			out = append(out, item)
		}
	}
	return out, nil
}

// ListSubnets lists the subnets of a virtual network.
func (c *Client) ListSubnets(ctx context.Context, resourceGroup, vnet string) ([]infrastructure.Item, error) {
	var out []infrastructure.Item
	pager := c.subnets.NewListPager(resourceGroup, vnet, nil)
	for pager.More() {
		page, err := pager.NextPage(ctx)
		if err != nil {
			return nil, err
		}
		for _, s := range page.Value {
			if s == nil || s.Name == nil {
				continue
			}
			item := infrastructure.Item{ID: deref(s.ID), Name: *s.Name}
			if s.Properties != nil && s.Properties.AddressPrefix != nil {
				item.Attrs = map[string]string{"cidr": *s.Properties.AddressPrefix}
			}
			out = append(out, item)
		}
	}
	return out, nil
}

// publicIPView maps an SDK public IP to the provider's view.
func publicIPView(ip armnetwork.PublicIPAddress) PublicIP {
	v := PublicIP{Name: deref(ip.Name), ID: deref(ip.ID)}
	if ip.Properties != nil {
		v.Address = deref(ip.Properties.IPAddress)
	}
	return v
}

// vmView maps an SDK VM to the provider's view.
func vmView(vm armcompute.VirtualMachine) VM {
	v := VM{Name: deref(vm.Name), ID: deref(vm.ID)}
	if vm.Properties != nil {
		v.ProvisioningState = deref(vm.Properties.ProvisioningState)
	}
	return v
}

// toTags converts a string map to the SDK's *string map.
func toTags(m map[string]string) map[string]*string {
	if len(m) == 0 {
		return nil
	}
	out := make(map[string]*string, len(m))
	for k, v := range m {
		out[k] = to.Ptr(v)
	}
	return out
}

// hasTag reports whether an SDK tag map carries klotho's ownership tag value.
func hasTag(tags map[string]*string, clusterTag string) bool {
	if v, ok := tags[ClusterTagKey]; ok && v != nil {
		return *v == clusterTag
	}
	return false
}

// isNotFound reports whether err is an Azure 404.
func isNotFound(err error) bool {
	var respErr *azcore.ResponseError
	if errors.As(err, &respErr) {
		return respErr.StatusCode == http.StatusNotFound
	}
	return false
}

func deref(s *string) string {
	if s == nil {
		return ""
	}
	return *s
}
