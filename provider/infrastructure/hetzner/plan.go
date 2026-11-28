package hetzner

import (
	"context"
	"strconv"
	"time"

	"github.com/hetznercloud/hcloud-go/v2/hcloud"

	"github.com/klotho-k8s/klotho/pkg/cluster"
	"github.com/klotho-k8s/klotho/provider/infrastructure"
)

// ResourceKindServer is the Plan kind for a Hetzner Cloud server.
const ResourceKindServer = "server"

// Plan lists the cluster's Hetzner servers by klotho's ownership label: every
// server carrying it (including machines CAPH created from the labelled
// template), plus anything recorded in state that is not found by label.
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

	servers, err := p.labelledServers(ctx, name)
	if err != nil {
		return nil, err
	}
	for _, s := range servers {
		add(cluster.Resource{Kind: ResourceKindServer, ID: strconv.FormatInt(s.ID, 10), Name: s.Name})
	}

	// Anything state recorded that the label search did not surface.
	for _, r := range state.Resources {
		add(r)
	}
	return plan, nil
}

// Teardown removes everything in the plan: every server is deleted (and waited
// out so a dependent record is not left dangling).
func (p *Provider) Teardown(ctx context.Context, _ *cluster.State, plan *infrastructure.Plan) error {
	if plan == nil {
		return nil
	}
	var ids []int64
	for _, r := range plan.Resources {
		if r.Kind != ResourceKindServer {
			continue
		}
		id, err := strconv.ParseInt(r.ID, 10, 64)
		if err != nil {
			return errorf("server id %q is not an integer: %w", r.ID, err)
		}
		ids = append(ids, id)
	}
	for _, id := range ids {
		if err := p.client.DeleteServer(ctx, id); err != nil {
			if notFound(err) {
				continue
			}
			return errorf("deleting server %d: %w", id, err)
		}
	}
	p.waitDeleted(ctx, ids)
	return nil
}

// labelledServers lists every server carrying the cluster's ownership label.
func (p *Provider) labelledServers(ctx context.Context, cluster string) ([]*hcloud.Server, error) {
	servers, err := p.client.ListServers(ctx, clusterSelector(cluster))
	if err != nil {
		return nil, errorf("listing servers: %w", err)
	}
	// Defence in depth: AllWithOpts already filters by the selector, but confirm
	// the ownership label so a stray match is never deleted.
	var out []*hcloud.Server
	for _, s := range servers {
		if hasClusterLabel(s.Labels, cluster) {
			out = append(out, s)
		}
	}
	return out, nil
}

// waitDeleted waits, best effort, for the servers to disappear.
func (p *Provider) waitDeleted(ctx context.Context, ids []int64) {
	deadline := time.Now().Add(p.launchWait)
	for {
		remaining := false
		for _, id := range ids {
			if s, err := p.client.GetServer(ctx, id); err == nil && s != nil {
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
