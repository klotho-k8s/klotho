package docker

import (
	"context"
	"errors"
	"fmt"
	"sort"

	"github.com/klotho-k8s/klotho/pkg/cluster"
	"github.com/klotho-k8s/klotho/provider/infrastructure"
)

// Plan lists every container carrying the cluster label
// (ClusterLabelKey=<cluster>): the control plane and worker nodes CAPD created
// and the load balancer, plus any container resources recorded in state. The
// shared Docker network is never torn down.
func (p *Provider) Plan(ctx context.Context, state *cluster.State) (*infrastructure.Plan, error) {
	if state == nil || state.Spec.Name == "" {
		return nil, errorf("state has no cluster name")
	}
	name := state.Spec.Name
	plan := &infrastructure.Plan{}
	seen := map[string]bool{}
	add := func(r cluster.Resource) {
		key := r.Kind + "|" + r.ID
		if !seen[key] {
			seen[key] = true
			plan.Resources = append(plan.Resources, r)
		}
	}

	containers, err := p.rt.List(ctx, clusterLabels(name))
	if err != nil {
		return nil, errorf("listing containers of %q: %v", name, err)
	}
	sort.Slice(containers, func(i, j int) bool { return containers[i].Name < containers[j].Name })
	for _, c := range containers {
		add(cluster.Resource{Kind: ResourceKindContainer, ID: c.Name, Name: c.Name})
	}
	for _, r := range state.Resources {
		if r.Kind == ResourceKindContainer {
			add(r)
		}
	}
	return plan, nil
}

// Teardown removes the plan's containers. A container is removed only when it
// still carries the cluster label, so a container recorded in state but since
// renamed or reused is left alone. Network resources are ignored: the Docker
// network is shared. Failures are collected and returned together.
func (p *Provider) Teardown(ctx context.Context, state *cluster.State, plan *infrastructure.Plan) error {
	if state == nil || state.Spec.Name == "" {
		return errorf("state has no cluster name")
	}
	if plan == nil {
		return nil
	}
	name := state.Spec.Name
	var errs []error
	for _, r := range plan.Resources {
		if r.Kind != ResourceKindContainer {
			continue
		}
		c, ok, err := p.rt.Inspect(ctx, r.ID)
		if err != nil {
			errs = append(errs, fmt.Errorf("inspecting %s: %w", r.ID, err))
			continue
		}
		if !ok {
			continue
		}
		if c.Labels[ClusterLabelKey] != name {
			errs = append(errs, errorf("refusing to remove container %q: it does not carry label %s=%s", r.ID, ClusterLabelKey, name))
			continue
		}
		if err := p.rt.Remove(ctx, r.ID); err != nil {
			errs = append(errs, fmt.Errorf("removing %s: %w", r.ID, err))
		}
	}
	return errors.Join(errs...)
}
