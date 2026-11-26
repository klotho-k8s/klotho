package azure

import (
	"context"

	"github.com/klotho-k8s/klotho/provider/infrastructure"
)

// Discover reports the virtual networks and subnets in the configured resource
// group that a cluster template can be prefilled with. It is read-only. It
// needs a resource group to scope the lookups; with none configured it reports
// nothing rather than scanning the whole subscription.
func (p *Provider) Discover(ctx context.Context) (infrastructure.Inventory, error) {
	inv := infrastructure.Inventory{}
	if p.cfg.ResourceGroup == "" {
		return inv, nil
	}

	vnets, err := p.api.ListVirtualNetworks(ctx, p.cfg.ResourceGroup)
	if err != nil {
		return nil, errorf("listing virtual networks in %s: %w", p.cfg.ResourceGroup, err)
	}
	for _, v := range vnets {
		inv["vnet"] = append(inv["vnet"], v)
		subnets, err := p.api.ListSubnets(ctx, p.cfg.ResourceGroup, v.Name)
		if err != nil {
			return nil, errorf("listing subnets of %s: %w", v.Name, err)
		}
		for _, s := range subnets {
			if s.Attrs == nil {
				s.Attrs = map[string]string{}
			}
			s.Attrs["vnet"] = v.Name
			inv["subnet"] = append(inv["subnet"], s)
		}
	}
	return inv, nil
}
