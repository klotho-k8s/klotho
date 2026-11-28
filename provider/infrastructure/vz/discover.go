package vz

import (
	"context"

	"github.com/klotho-k8s/klotho/provider/infrastructure"
)

// Inventory kinds reported by Discover.
const (
	// KindHost is the single vz host (the Mac running capvz-daemon).
	KindHost = "host"
	// KindVM is a VM the daemon already knows about.
	KindVM = "vm"
)

// Discover reports the facts a cluster template can be prefilled with. The
// single-host dev layout has no networks or storage pools to enumerate, so
// Discover reports the configured vz host (the value baked into provider IDs) and
// the VMs the daemon currently knows about. Reaching the daemon also proves it is
// up and the token is accepted before a create is attempted. It ensures nothing.
func (p *Provider) Discover(ctx context.Context) (infrastructure.Inventory, error) {
	r := p.resolve(nil)
	inv := infrastructure.Inventory{
		KindHost: {{ID: r.Host, Name: r.Host}},
		KindVM:   {},
	}
	vms, err := p.client.ListVMs(ctx)
	if err != nil {
		return nil, errorf("listing VMs on the daemon: %w", err)
	}
	for _, vm := range vms {
		inv[KindVM] = append(inv[KindVM], infrastructure.Item{
			ID:   vm.ID,
			Name: vm.Name,
			Attrs: map[string]string{
				"state": vm.State,
				"notes": vm.Notes,
			},
		})
	}
	return inv, nil
}
