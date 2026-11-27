package libvirt

import (
	"context"

	"github.com/klotho-k8s/klotho/provider/infrastructure"
)

// Inventory kinds reported by Discover.
const (
	KindNetwork = "network"
	KindPool    = "pool"
)

// Discover reports the host's libvirt virtual networks and storage pools, the
// choices a template's network and storagePool can be prefilled with. It
// ensures nothing.
func (p *Provider) Discover(ctx context.Context) (infrastructure.Inventory, error) {
	nets, err := p.client.ListNetworks(ctx)
	if err != nil {
		return nil, errorf("listing networks: %w", err)
	}
	pools, err := p.client.ListStoragePools(ctx)
	if err != nil {
		return nil, errorf("listing storage pools: %w", err)
	}
	inv := infrastructure.Inventory{KindNetwork: {}, KindPool: {}}
	for _, n := range nets {
		inv[KindNetwork] = append(inv[KindNetwork], infrastructure.Item{ID: n, Name: n})
	}
	for _, pl := range pools {
		inv[KindPool] = append(inv[KindPool], infrastructure.Item{ID: pl, Name: pl})
	}
	return inv, nil
}
