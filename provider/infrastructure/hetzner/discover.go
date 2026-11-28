package hetzner

import (
	"context"
	"strconv"

	"github.com/klotho-k8s/klotho/provider/infrastructure"
)

// Discover reports the locations, server types and images a cluster template can
// be prefilled with. It is read-only.
func (p *Provider) Discover(ctx context.Context) (infrastructure.Inventory, error) {
	inv := infrastructure.Inventory{}

	locations, err := p.client.ListLocations(ctx)
	if err != nil {
		return nil, errorf("listing locations: %w", err)
	}
	for _, l := range locations {
		inv["location"] = append(inv["location"], infrastructure.Item{
			ID:   l.Name,
			Name: l.Description,
			Attrs: map[string]string{
				"city":    l.City,
				"country": l.Country,
			},
		})
	}

	types, err := p.client.ListServerTypes(ctx)
	if err != nil {
		return nil, errorf("listing server types: %w", err)
	}
	for _, t := range types {
		inv["type"] = append(inv["type"], infrastructure.Item{
			ID:   t.Name,
			Name: t.Description,
			Attrs: map[string]string{
				"cores":    strconv.Itoa(t.Cores),
				"memoryGB": strconv.FormatFloat(float64(t.Memory), 'f', 0, 64),
				"arch":     string(t.Architecture),
			},
		})
	}

	images, err := p.client.ListImages(ctx)
	if err != nil {
		return nil, errorf("listing images: %w", err)
	}
	for _, img := range images {
		inv["image"] = append(inv["image"], infrastructure.Item{
			ID:   img.Name,
			Name: img.Description,
			Attrs: map[string]string{
				"osFlavor": img.OSFlavor,
				"arch":     string(img.Architecture),
			},
		})
	}

	return inv, nil
}
