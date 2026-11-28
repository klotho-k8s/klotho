package openstack

import (
	"context"
	"strings"

	"github.com/klotho-k8s/klotho/pkg/cluster"
	"github.com/klotho-k8s/klotho/provider/infrastructure"
)

// Plan lists the cluster's OpenStack resources by klotho's ownership tag: every
// Nova server carrying it (including machines CAPO created from the tagged
// template), the bootstrap server from state in case the tag search missed it,
// and anything recorded in state that the tag search did not surface (the
// allocated control plane floating IP among them).
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

	servers, err := p.client.ListServersByTag(ctx, clusterTag(name))
	if err != nil {
		return nil, errorf("listing servers: %w", err)
	}
	for i := range servers {
		add(cluster.Resource{Kind: ResourceKindServer, ID: servers[i].ID, Name: servers[i].Name})
	}

	// The bootstrap server, from state, in case the tag search missed it.
	if state.Bootstrap != nil {
		if id, ok := parseProviderID(state.Bootstrap.ID); ok {
			add(cluster.Resource{Kind: ResourceKindServer, ID: id, Name: state.Bootstrap.Name})
		}
	}

	// Anything state recorded that the tag search did not surface (the allocated
	// floating IP among them).
	for _, r := range state.Resources {
		add(r)
	}
	return plan, nil
}

// Teardown removes everything in the plan: every server is deleted, then any
// allocated floating IP is released.
//
// Like the other cloud providers, this reaches zero but is not a hands-off
// finisher: CAPO's controller runs on the last control-plane server it must
// delete, so the final server cannot destroy itself from inside. The external
// deletion finisher (an out-of-cluster agent that removes the last server after
// klotho exits) is the outstanding work — see docs/PROVIDERS.md and TESTING.md.
func (p *Provider) Teardown(ctx context.Context, _ *cluster.State, plan *infrastructure.Plan) error {
	if plan == nil {
		return nil
	}
	var serverIDs, fipIDs []string
	for _, r := range plan.Resources {
		switch r.Kind {
		case ResourceKindServer:
			serverIDs = append(serverIDs, r.ID)
		case ResourceKindFloatingIP:
			fipIDs = append(fipIDs, r.ID)
		}
	}

	for _, id := range serverIDs {
		if err := p.client.DeleteServer(ctx, id); err != nil {
			if notFound(err) {
				continue
			}
			return errorf("deleting server %q: %w", id, err)
		}
	}
	for _, id := range fipIDs {
		if err := p.client.DeleteFloatingIP(ctx, id); err != nil {
			if notFound(err) {
				continue
			}
			return errorf("releasing floating IP %q: %w", id, err)
		}
	}
	return nil
}

// parseProviderID extracts the server UUID from "openstack://<region>/<uuid>"
// ("openstack:///<uuid>" with an empty region).
func parseProviderID(id string) (string, bool) {
	rest, found := strings.CutPrefix(id, ProviderIDScheme)
	if !found {
		return "", false
	}
	// rest is "<region>/<uuid>"; the UUID is the last path element.
	i := strings.LastIndex(rest, "/")
	if i < 0 || i+1 >= len(rest) {
		return "", false
	}
	return rest[i+1:], true
}
