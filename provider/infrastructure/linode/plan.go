package linode

import (
	"context"
	"strconv"
	"time"

	"github.com/linode/linodego/v2"

	"github.com/klotho-k8s/klotho/pkg/cluster"
	"github.com/klotho-k8s/klotho/provider/infrastructure"
)

// ResourceKindInstance is the Plan kind for a Linode instance.
const ResourceKindInstance = "instance"

// Plan lists the cluster's Linode instances by klotho's ownership tag: every
// instance carrying it (including machines CAPL created from the tagged
// template), plus anything recorded in state that is not found by tag.
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

	insts, err := p.taggedInstances(ctx, name)
	if err != nil {
		return nil, err
	}
	for i := range insts {
		add(cluster.Resource{Kind: ResourceKindInstance, ID: strconv.Itoa(insts[i].ID), Name: insts[i].Label})
	}

	// Anything state recorded that the tag search did not surface.
	for _, r := range state.Resources {
		add(r)
	}
	return plan, nil
}

// Teardown removes everything in the plan: every instance is deleted (and
// waited out so a dependent record is not left dangling).
func (p *Provider) Teardown(ctx context.Context, _ *cluster.State, plan *infrastructure.Plan) error {
	if plan == nil {
		return nil
	}
	var ids []int
	for _, r := range plan.Resources {
		if r.Kind != ResourceKindInstance {
			continue
		}
		id, err := strconv.Atoi(r.ID)
		if err != nil {
			return errorf("instance id %q is not an integer: %w", r.ID, err)
		}
		ids = append(ids, id)
	}
	for _, id := range ids {
		if err := p.client.DeleteInstance(ctx, id); err != nil {
			if isNotFound(err) {
				continue
			}
			return errorf("deleting instance %d: %w", id, err)
		}
	}
	p.waitDeleted(ctx, ids)
	return nil
}

// taggedInstances lists every instance carrying the cluster's ownership tag.
func (p *Provider) taggedInstances(ctx context.Context, cluster string) ([]linodego.Instance, error) {
	all, err := p.client.ListInstances(ctx, nil)
	if err != nil {
		return nil, errorf("listing instances: %w", err)
	}
	var out []linodego.Instance
	for i := range all {
		if hasClusterTag(all[i].Tags, cluster) {
			out = append(out, all[i])
		}
	}
	return out, nil
}

// findBootstrapInstance finds the single running bootstrap instance by its
// label and klotho's ownership tag.
func (p *Provider) findBootstrapInstance(ctx context.Context, clusterName string) (*linodego.Instance, error) {
	insts, err := p.taggedInstances(ctx, clusterName)
	if err != nil {
		return nil, err
	}
	label := MachineName(clusterName)
	var found []linodego.Instance
	for i := range insts {
		if insts[i].Label == label {
			found = append(found, insts[i])
		}
	}
	switch len(found) {
	case 1:
		return &found[0], nil
	case 0:
		return nil, errorf("no instance labelled %q carrying tag %q", label, clusterTag(clusterName))
	default:
		return nil, errorf("%d instances labelled %q carry tag %q", len(found), label, clusterTag(clusterName))
	}
}

// waitDeleted waits, best effort, for the instances to disappear.
func (p *Provider) waitDeleted(ctx context.Context, ids []int) {
	deadline := time.Now().Add(p.launchWait)
	for {
		remaining := false
		for _, id := range ids {
			if _, err := p.client.GetInstance(ctx, id); err == nil {
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

// isNotFound reports whether err is a Linode 404.
func isNotFound(err error) bool {
	return linodego.ErrHasStatus(err, 404)
}
