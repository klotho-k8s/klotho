package libvirt

import (
	"context"
	"errors"
	"fmt"
	"sort"
	"strings"

	"github.com/klotho-k8s/klotho/pkg/cluster"
	"github.com/klotho-k8s/klotho/provider/infrastructure"
)

// ResourceKindDomain is the Plan/state resource kind for a libvirt domain.
const ResourceKindDomain = "domain"

// namePrefix is the prefix every domain of the cluster shares: the bootstrap
// domain is "<cluster>-bootstrap" and the Cluster API libvirt provider names
// the rest after their Machine, "<cluster>-...".
func namePrefix(cluster string) string { return cluster + "-" }

// Plan lists every domain whose name begins with "<cluster>-" and carries the
// cluster tag in its description (the bootstrap domain and the machines the
// provider created), plus any domain resources recorded in state. The overlay
// and cidata volumes are deleted with their domain by Teardown, so they are not
// listed separately. The base image and the storage pool are never touched.
func (p *Provider) Plan(ctx context.Context, state *cluster.State) (*infrastructure.Plan, error) {
	if state == nil || state.Spec.Name == "" {
		return nil, errorf("state has no cluster name")
	}
	name := state.Spec.Name
	tag := Tag(name)
	plan := &infrastructure.Plan{}
	seen := map[string]bool{}
	add := func(r cluster.Resource) {
		key := r.Kind + "|" + r.ID
		if !seen[key] {
			seen[key] = true
			plan.Resources = append(plan.Resources, r)
		}
	}

	doms, err := p.client.ListDomains(ctx, namePrefix(name))
	if err != nil {
		return nil, errorf("listing domains of %q: %w", name, err)
	}
	sort.Slice(doms, func(i, j int) bool { return doms[i].Name < doms[j].Name })
	for _, d := range doms {
		if hasTag(d.Description, tag) {
			add(cluster.Resource{Kind: ResourceKindDomain, ID: d.Name, Name: d.Name})
		}
	}
	for _, r := range state.Resources {
		if r.Kind == ResourceKindDomain {
			add(r)
		}
	}
	return plan, nil
}

// Teardown removes the plan's domains: it destroys (force-stops) and undefines
// each, deleting its overlay and cidata volumes with it. A domain is removed
// only when its name still carries the cluster prefix, so an unrelated domain is
// left alone. Failures are collected and returned together.
func (p *Provider) Teardown(ctx context.Context, state *cluster.State, plan *infrastructure.Plan) error {
	if state == nil || state.Spec.Name == "" {
		return errorf("state has no cluster name")
	}
	if plan == nil {
		return nil
	}
	prefix := namePrefix(state.Spec.Name)
	var errs []error
	for _, r := range plan.Resources {
		if r.Kind != ResourceKindDomain {
			continue
		}
		if !strings.HasPrefix(r.ID, prefix) {
			errs = append(errs, errorf("refusing to remove domain %q: it does not start with %q", r.ID, prefix))
			continue
		}
		if err := p.client.DestroyDomain(ctx, r.ID); err != nil {
			errs = append(errs, fmt.Errorf("destroying domain %s: %w", r.ID, err))
			continue
		}
		if err := p.client.UndefineDomain(ctx, r.ID); err != nil {
			errs = append(errs, fmt.Errorf("undefining domain %s: %w", r.ID, err))
		}
	}
	return errors.Join(errs...)
}

// hasTag reports whether a domain description carries tag. The description may
// hold other text; the tag is matched as a whitespace- or comma-delimited token
// and, failing that, as a substring.
func hasTag(description, tag string) bool {
	if tag == "" {
		return false
	}
	for _, field := range strings.FieldsFunc(description, func(r rune) bool {
		return r == ',' || r == ' ' || r == '\t' || r == '\n' || r == '\r'
	}) {
		if field == tag {
			return true
		}
	}
	return strings.Contains(description, tag)
}
