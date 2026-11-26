package hyperv

import (
	"context"

	"github.com/klotho-k8s/klotho/provider/infrastructure"
)

// Discover reports the host's virtual switches, the choice a template's
// switchName can be prefilled with. It ensures nothing.
func (p *Provider) Discover(ctx context.Context) (infrastructure.Inventory, error) {
	switches, err := p.client.ListSwitches(ctx)
	if err != nil {
		return nil, errorf("listing virtual switches: %w", err)
	}
	inv := infrastructure.Inventory{}
	for _, s := range switches {
		inv["switch"] = append(inv["switch"], infrastructure.Item{ID: s, Name: s})
	}
	return inv, nil
}
