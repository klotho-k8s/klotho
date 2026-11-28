package digitalocean

import (
	"context"
	"strconv"

	"github.com/klotho-k8s/klotho/provider/infrastructure"
)

// Discover reports the regions, sizes and images a cluster template can be
// prefilled with. It is read-only. Only available regions and sizes are listed.
func (p *Provider) Discover(ctx context.Context) (infrastructure.Inventory, error) {
	inv := infrastructure.Inventory{}

	regions, err := p.client.ListRegions(ctx)
	if err != nil {
		return nil, errorf("listing regions: %w", err)
	}
	for _, r := range regions {
		if !r.Available {
			continue
		}
		inv["region"] = append(inv["region"], infrastructure.Item{ID: r.Slug, Name: r.Name})
	}

	sizes, err := p.client.ListSizes(ctx)
	if err != nil {
		return nil, errorf("listing sizes: %w", err)
	}
	for _, s := range sizes {
		if !s.Available {
			continue
		}
		inv["size"] = append(inv["size"], infrastructure.Item{
			ID:   s.Slug,
			Name: s.Slug,
			Attrs: map[string]string{
				"vcpus":      strconv.Itoa(s.Vcpus),
				"memoryMB":   strconv.Itoa(s.Memory),
				"monthlyUSD": strconv.FormatFloat(s.PriceMonthly, 'f', 2, 64),
			},
		})
	}

	images, err := p.client.ListImages(ctx)
	if err != nil {
		return nil, errorf("listing images: %w", err)
	}
	for _, img := range images {
		if img.Slug == "" {
			continue
		}
		inv["image"] = append(inv["image"], infrastructure.Item{ID: img.Slug, Name: img.Name})
	}

	return inv, nil
}
