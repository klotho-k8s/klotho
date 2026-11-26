package linode

import (
	"context"
	"strconv"

	"github.com/klotho-k8s/klotho/provider/infrastructure"
)

// Discover reports the regions, instance types and images a cluster template
// can be prefilled with. It is read-only. Regions are limited to those that
// support the Metadata service klotho delivers cloud-init through.
func (p *Provider) Discover(ctx context.Context) (infrastructure.Inventory, error) {
	inv := infrastructure.Inventory{}

	regions, err := p.client.ListRegions(ctx, nil)
	if err != nil {
		return nil, errorf("listing regions: %w", err)
	}
	for _, r := range regions {
		if !hasCapability(r.Capabilities, metadataCapability) {
			continue
		}
		inv["region"] = append(inv["region"], infrastructure.Item{
			ID:    r.ID,
			Name:  r.Label,
			Attrs: map[string]string{"country": r.Country},
		})
	}

	types, err := p.client.ListTypes(ctx, nil)
	if err != nil {
		return nil, errorf("listing instance types: %w", err)
	}
	for _, t := range types {
		attrs := map[string]string{
			"vcpus":    strconv.Itoa(t.VCPUs),
			"memoryMB": strconv.Itoa(t.Memory),
		}
		if t.Price != nil {
			attrs["monthlyUSD"] = strconv.FormatFloat(float64(t.Price.Monthly), 'f', 2, 64)
		}
		inv["type"] = append(inv["type"], infrastructure.Item{ID: t.ID, Name: t.Label, Attrs: attrs})
	}

	images, err := p.client.ListImages(ctx, nil)
	if err != nil {
		return nil, errorf("listing images: %w", err)
	}
	for _, img := range images {
		if !hasCapability(img.Capabilities, cloudInitCapability) {
			continue
		}
		inv["image"] = append(inv["image"], infrastructure.Item{ID: img.ID, Name: img.Label})
	}

	return inv, nil
}
