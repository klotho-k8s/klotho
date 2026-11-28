package openstack

import (
	"context"

	"github.com/klotho-k8s/klotho/provider/infrastructure"
)

// Discover reports what a cluster template can be prefilled with. OpenStack's
// catalog (flavors, images, networks) is large and project-scoped, and klotho
// already knows the cloud, network, flavor and image from configuration, so
// Discover surfaces the configured choices rather than enumerating the catalog.
// It is read-only and makes no API calls.
func (p *Provider) Discover(_ context.Context) (infrastructure.Inventory, error) {
	inv := infrastructure.Inventory{}
	if p.cfg.Cloud != "" {
		inv["cloud"] = []infrastructure.Item{{ID: p.cfg.Cloud, Name: p.cfg.Cloud}}
	}
	if p.cfg.Network != "" {
		inv["network"] = []infrastructure.Item{{ID: p.cfg.Network, Name: p.cfg.Network}}
	}
	if p.cfg.ExternalNetwork != "" {
		inv["externalNetwork"] = []infrastructure.Item{{ID: p.cfg.ExternalNetwork, Name: p.cfg.ExternalNetwork}}
	}
	if p.cfg.Flavor != "" {
		inv["flavor"] = []infrastructure.Item{{ID: p.cfg.Flavor, Name: p.cfg.Flavor}}
	}
	if p.cfg.Image != "" {
		inv["image"] = []infrastructure.Item{{ID: p.cfg.Image, Name: p.cfg.Image}}
	}
	return inv, nil
}
