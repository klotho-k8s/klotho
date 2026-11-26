package docker

import (
	"context"

	"github.com/klotho-k8s/klotho/provider/infrastructure"
)

// Discover reports the Docker network the provider would use. It ensures
// nothing; it only describes a choice a template can be prefilled with.
func (p *Provider) Discover(_ context.Context) (infrastructure.Inventory, error) {
	net := p.network()
	return infrastructure.Inventory{
		"network": {{
			ID:   net,
			Name: net,
			Attrs: map[string]string{
				"runtime": p.runtimeName(),
			},
		}},
	}, nil
}

func (p *Provider) runtimeName() string {
	if p.cfg.Runtime != "" {
		return p.cfg.Runtime
	}
	return "docker"
}
