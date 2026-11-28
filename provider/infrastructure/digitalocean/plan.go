package digitalocean

import (
	"context"
	"strconv"
	"time"

	"github.com/digitalocean/godo"

	"github.com/klotho-k8s/klotho/pkg/cluster"
	"github.com/klotho-k8s/klotho/provider/infrastructure"
)

// ResourceKindDroplet is the Plan kind for a DigitalOcean droplet.
const ResourceKindDroplet = "droplet"

// Plan lists the cluster's DigitalOcean resources: every droplet carrying
// klotho's ownership tag (including machines CAPDO created from the tagged
// templates) and the control plane load balancer recorded in state, plus
// anything else recorded in state that was not found by tag.
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

	droplets, err := p.taggedDroplets(ctx, name)
	if err != nil {
		return nil, err
	}
	for i := range droplets {
		add(cluster.Resource{Kind: ResourceKindDroplet, ID: strconv.Itoa(droplets[i].ID), Name: droplets[i].Name})
	}

	// The load balancer and anything else state recorded that the tag search did
	// not surface.
	for _, r := range state.Resources {
		add(r)
	}
	return plan, nil
}

// Teardown removes everything in the plan: every droplet is deleted (and waited
// out), then the control plane load balancer is released.
func (p *Provider) Teardown(ctx context.Context, _ *cluster.State, plan *infrastructure.Plan) error {
	if plan == nil {
		return nil
	}
	var dropletIDs []int
	var lbIDs []string
	for _, r := range plan.Resources {
		switch r.Kind {
		case ResourceKindDroplet:
			id, err := strconv.Atoi(r.ID)
			if err != nil {
				return errorf("droplet id %q is not an integer: %w", r.ID, err)
			}
			dropletIDs = append(dropletIDs, id)
		case ResourceKindLoadBalancer:
			lbIDs = append(lbIDs, r.ID)
		}
	}

	for _, id := range dropletIDs {
		if err := p.client.DeleteDroplet(ctx, id); err != nil {
			if isNotFound(err) {
				continue
			}
			return errorf("deleting droplet %d: %w", id, err)
		}
	}
	p.waitDeleted(ctx, dropletIDs)

	for _, id := range lbIDs {
		if err := p.client.DeleteLoadBalancer(ctx, id); err != nil {
			if isNotFound(err) {
				continue
			}
			return errorf("deleting load balancer %q: %w", id, err)
		}
	}
	return nil
}

// taggedDroplets lists every droplet carrying the cluster's ownership tag.
func (p *Provider) taggedDroplets(ctx context.Context, cluster string) ([]godo.Droplet, error) {
	droplets, err := p.client.ListDropletsByTag(ctx, clusterTag(cluster))
	if err != nil {
		return nil, errorf("listing droplets by tag: %w", err)
	}
	return droplets, nil
}

// findBootstrapDroplet finds the single bootstrap droplet by name and klotho's
// ownership tag.
func (p *Provider) findBootstrapDroplet(ctx context.Context, clusterName string) (*godo.Droplet, error) {
	droplets, err := p.taggedDroplets(ctx, clusterName)
	if err != nil {
		return nil, err
	}
	want := MachineName(clusterName)
	var found []godo.Droplet
	for i := range droplets {
		if droplets[i].Name == want {
			found = append(found, droplets[i])
		}
	}
	switch len(found) {
	case 1:
		return &found[0], nil
	case 0:
		return nil, errorf("no droplet named %q carrying tag %q", want, clusterTag(clusterName))
	default:
		return nil, errorf("%d droplets named %q carry tag %q", len(found), want, clusterTag(clusterName))
	}
}

// waitDeleted waits, best effort, for the droplets to disappear.
func (p *Provider) waitDeleted(ctx context.Context, ids []int) {
	deadline := time.Now().Add(p.launchWait)
	for {
		remaining := false
		for _, id := range ids {
			if _, err := p.client.GetDroplet(ctx, id); err == nil {
				remaining = true
			}
		}
		if !remaining || !time.Now().Before(deadline) {
			return
		}
		select {
		case <-ctx.Done():
			return
		case <-time.After(p.launchPoll):
		}
	}
}
