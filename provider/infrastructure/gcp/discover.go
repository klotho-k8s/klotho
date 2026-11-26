package gcp

import (
	"context"

	"github.com/klotho-k8s/klotho/provider/infrastructure"
)

// Discover reports what a cluster template can be prefilled with. GCP's API is
// project-scoped and klotho already knows the project, region and zone from
// configuration, so Discover surfaces the configured placement and the default
// image rather than enumerating every machine type and image in the catalog
// (which is large and needs no credentials to reason about). It is read-only
// and makes no API calls.
func (p *Provider) Discover(_ context.Context) (infrastructure.Inventory, error) {
	inv := infrastructure.Inventory{}
	if p.cfg.Project != "" {
		inv["project"] = []infrastructure.Item{{ID: p.cfg.Project, Name: p.cfg.Project}}
	}
	if p.cfg.Region != "" {
		inv["region"] = []infrastructure.Item{{ID: p.cfg.Region, Name: p.cfg.Region}}
	}
	if p.cfg.Zone != "" {
		inv["zone"] = []infrastructure.Item{{ID: p.cfg.Zone, Name: p.cfg.Zone}}
	}
	inv["machineType"] = []infrastructure.Item{{ID: p.machineType(), Name: p.machineType()}}
	inv["image"] = []infrastructure.Item{{
		ID:   p.imageURL(),
		Name: p.imageURL(),
		Attrs: map[string]string{
			"project": p.imageProject(),
			"family":  p.imageFamily(),
		},
	}}
	return inv, nil
}
