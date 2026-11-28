package vsphere

import (
	"context"

	"github.com/klotho-k8s/klotho/provider/infrastructure"
)

// Inventory kinds reported by Discover.
const (
	KindNetwork   = "network"
	KindDatastore = "datastore"
	KindTemplate  = "template"
)

// Discover reports the datacenter's networks, datastores and VM templates, the
// choices a template's network, datastore and template can be prefilled with.
// It ensures nothing.
func (p *Provider) Discover(ctx context.Context) (infrastructure.Inventory, error) {
	nets, err := p.client.ListNetworks(ctx)
	if err != nil {
		return nil, errorf("listing networks: %w", err)
	}
	stores, err := p.client.ListDatastores(ctx)
	if err != nil {
		return nil, errorf("listing datastores: %w", err)
	}
	tmpls, err := p.client.ListTemplates(ctx)
	if err != nil {
		return nil, errorf("listing templates: %w", err)
	}
	inv := infrastructure.Inventory{KindNetwork: {}, KindDatastore: {}, KindTemplate: {}}
	for _, n := range nets {
		inv[KindNetwork] = append(inv[KindNetwork], infrastructure.Item{ID: n, Name: n})
	}
	for _, d := range stores {
		inv[KindDatastore] = append(inv[KindDatastore], infrastructure.Item{ID: d, Name: d})
	}
	for _, t := range tmpls {
		inv[KindTemplate] = append(inv[KindTemplate], infrastructure.Item{ID: t, Name: t})
	}
	return inv, nil
}
