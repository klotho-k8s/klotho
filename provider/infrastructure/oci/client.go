package oci

import (
	"context"

	"github.com/oracle/oci-go-sdk/v65/common"
	"github.com/oracle/oci-go-sdk/v65/common/auth"
	"github.com/oracle/oci-go-sdk/v65/core"
	"github.com/oracle/oci-go-sdk/v65/identity"
)

// ociClient wraps the OCI SDK compute, virtual-network and identity clients to
// satisfy API.
type ociClient struct {
	compute  core.ComputeClient
	vnet     core.VirtualNetworkClient
	identity identity.IdentityClient
}

// NewClient builds an OCI client from cfg's authentication settings. It
// authenticates with the instance principal when cfg.UseInstancePrincipal is
// set, otherwise with the SDK config file (cfg.ConfigFile / cfg.Profile, or the
// SDK default ~/.oci/config DEFAULT profile). It makes no API calls.
func NewClient(cfg Config) (API, error) {
	provider, err := configProvider(cfg)
	if err != nil {
		return nil, err
	}

	compute, err := core.NewComputeClientWithConfigurationProvider(provider)
	if err != nil {
		return nil, errorf("building the compute client: %w", err)
	}
	vnet, err := core.NewVirtualNetworkClientWithConfigurationProvider(provider)
	if err != nil {
		return nil, errorf("building the network client: %w", err)
	}
	id, err := identity.NewIdentityClientWithConfigurationProvider(provider)
	if err != nil {
		return nil, errorf("building the identity client: %w", err)
	}
	if cfg.Region != "" {
		compute.SetRegion(cfg.Region)
		vnet.SetRegion(cfg.Region)
		id.SetRegion(cfg.Region)
	}
	return &ociClient{compute: compute, vnet: vnet, identity: id}, nil
}

// configProvider resolves the OCI SDK ConfigurationProvider from cfg.
func configProvider(cfg Config) (common.ConfigurationProvider, error) {
	if cfg.UseInstancePrincipal {
		provider, err := auth.InstancePrincipalConfigurationProvider()
		if err != nil {
			return nil, errorf("instance principal authentication: %w", err)
		}
		return provider, nil
	}
	profile := cfg.Profile
	if cfg.ConfigFile != "" || profile != "" {
		// An empty private-key password is correct for an unencrypted key.
		provider, err := common.ConfigurationProviderFromFileWithProfile(cfg.ConfigFile, profile, "")
		if err != nil {
			return nil, errorf("reading OCI config file: %w", err)
		}
		return provider, nil
	}
	return common.DefaultConfigProvider(), nil
}

func (c *ociClient) LaunchInstance(ctx context.Context, details core.LaunchInstanceDetails) (core.Instance, error) {
	resp, err := c.compute.LaunchInstance(ctx, core.LaunchInstanceRequest{LaunchInstanceDetails: details})
	if err != nil {
		return core.Instance{}, err
	}
	return resp.Instance, nil
}

func (c *ociClient) GetInstance(ctx context.Context, instanceID string) (core.Instance, error) {
	resp, err := c.compute.GetInstance(ctx, core.GetInstanceRequest{InstanceId: common.String(instanceID)})
	if err != nil {
		return core.Instance{}, err
	}
	return resp.Instance, nil
}

func (c *ociClient) ListInstances(ctx context.Context, compartmentID string) ([]core.Instance, error) {
	var out []core.Instance
	var page *string
	for {
		resp, err := c.compute.ListInstances(ctx, core.ListInstancesRequest{
			CompartmentId: common.String(compartmentID),
			Page:          page,
		})
		if err != nil {
			return nil, err
		}
		out = append(out, resp.Items...)
		if resp.OpcNextPage == nil {
			return out, nil
		}
		page = resp.OpcNextPage
	}
}

func (c *ociClient) TerminateInstance(ctx context.Context, instanceID string) error {
	_, err := c.compute.TerminateInstance(ctx, core.TerminateInstanceRequest{
		InstanceId: common.String(instanceID),
	})
	return err
}

// PrimaryVnicAddresses resolves the instance's primary VNIC and returns its
// public then private IPv4 addresses. It is best-effort: an error resolving the
// VNIC is returned to the caller, which treats missing addresses as non-fatal.
func (c *ociClient) PrimaryVnicAddresses(ctx context.Context, compartmentID, instanceID string) ([]string, error) {
	att, err := c.compute.ListVnicAttachments(ctx, core.ListVnicAttachmentsRequest{
		CompartmentId: common.String(compartmentID),
		InstanceId:    common.String(instanceID),
	})
	if err != nil {
		return nil, err
	}
	var out []string
	for _, a := range att.Items {
		if a.VnicId == nil {
			continue
		}
		vnic, err := c.vnet.GetVnic(ctx, core.GetVnicRequest{VnicId: a.VnicId})
		if err != nil {
			return nil, err
		}
		if ip := strVal(vnic.PublicIp); ip != "" {
			out = append(out, ip)
		}
		if ip := strVal(vnic.PrivateIp); ip != "" {
			out = append(out, ip)
		}
	}
	return out, nil
}

func (c *ociClient) ListImages(ctx context.Context, compartmentID string) ([]core.Image, error) {
	var out []core.Image
	var page *string
	for {
		resp, err := c.compute.ListImages(ctx, core.ListImagesRequest{
			CompartmentId: common.String(compartmentID),
			Page:          page,
		})
		if err != nil {
			return nil, err
		}
		out = append(out, resp.Items...)
		if resp.OpcNextPage == nil {
			return out, nil
		}
		page = resp.OpcNextPage
	}
}

func (c *ociClient) ListShapes(ctx context.Context, compartmentID string) ([]core.Shape, error) {
	var out []core.Shape
	var page *string
	for {
		resp, err := c.compute.ListShapes(ctx, core.ListShapesRequest{
			CompartmentId: common.String(compartmentID),
			Page:          page,
		})
		if err != nil {
			return nil, err
		}
		out = append(out, resp.Items...)
		if resp.OpcNextPage == nil {
			return out, nil
		}
		page = resp.OpcNextPage
	}
}

func (c *ociClient) ListAvailabilityDomains(ctx context.Context, compartmentID string) ([]AvailabilityDomain, error) {
	resp, err := c.identity.ListAvailabilityDomains(ctx, identity.ListAvailabilityDomainsRequest{
		CompartmentId: common.String(compartmentID),
	})
	if err != nil {
		return nil, err
	}
	out := make([]AvailabilityDomain, 0, len(resp.Items))
	for _, ad := range resp.Items {
		out = append(out, AvailabilityDomain{Name: strVal(ad.Name), ID: strVal(ad.Id)})
	}
	return out, nil
}
