package azure

import (
	"context"

	"github.com/klotho-k8s/klotho/pkg/cluster"
	"github.com/klotho-k8s/klotho/provider/infrastructure"
)

// Resource kinds in a Plan. They double as the names Teardown deletes by, so
// the ID of each planned resource is its Azure resource name.
const (
	ResourceKindVM       = "vm"
	ResourceKindNIC      = "nic"
	ResourceKindPublicIP = "publicip"
)

// Plan lists the cluster's Azure resources by klotho's ownership tag: every VM,
// NIC and public IP in the cluster resource group carrying it (including
// machines CAPZ created from the tagged template), plus anything recorded in
// state that is not found by tag.
func (p *Provider) Plan(ctx context.Context, state *cluster.State) (*infrastructure.Plan, error) {
	if state == nil || state.Spec.Name == "" {
		return nil, errorf("state has no cluster name")
	}
	name := state.Spec.Name
	plan := &infrastructure.Plan{}
	seen := map[string]bool{}
	add := func(r cluster.Resource) {
		key := r.Kind + "|" + r.ID
		if r.ID != "" && !seen[key] {
			seen[key] = true
			plan.Resources = append(plan.Resources, r)
		}
	}

	if p.cfg.ResourceGroup != "" {
		resources, err := p.api.ListClusterResources(ctx, p.cfg.ResourceGroup, Tag(name))
		if err != nil {
			return nil, errorf("listing cluster resources: %w", err)
		}
		for _, r := range resources {
			add(r)
		}
	}

	// Anything state recorded that the tag search did not surface.
	for _, r := range state.Resources {
		add(r)
	}
	return plan, nil
}

// Teardown removes everything in the plan in dependency order: VMs first, then
// the NICs that were attached to them, then the public IPs. A resource already
// gone is not an error.
func (p *Provider) Teardown(ctx context.Context, _ *cluster.State, plan *infrastructure.Plan) error {
	if plan == nil {
		return nil
	}
	var vms, nics, pips []string
	for _, r := range plan.Resources {
		switch r.Kind {
		case ResourceKindVM:
			vms = append(vms, r.ID)
		case ResourceKindNIC:
			nics = append(nics, r.ID)
		case ResourceKindPublicIP:
			pips = append(pips, r.ID)
		}
	}
	for _, name := range vms {
		if err := p.api.DeleteVM(ctx, p.cfg.ResourceGroup, name); err != nil {
			return errorf("deleting VM %s: %w", name, err)
		}
	}
	for _, name := range nics {
		if err := p.api.DeleteNIC(ctx, p.cfg.ResourceGroup, name); err != nil {
			return errorf("deleting NIC %s: %w", name, err)
		}
	}
	for _, name := range pips {
		if err := p.api.DeletePublicIP(ctx, p.cfg.ResourceGroup, name); err != nil {
			return errorf("deleting public IP %s: %w", name, err)
		}
	}
	return nil
}
