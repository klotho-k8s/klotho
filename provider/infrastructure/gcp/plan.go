package gcp

import (
	"context"
	"strings"

	"github.com/klotho-k8s/klotho/pkg/cluster"
	"github.com/klotho-k8s/klotho/provider/infrastructure"
)

// ResourceKindInstance is the Plan kind for a Compute Engine instance. For an
// instance resource, ID is the instance name and Name is the short zone it
// lives in (Teardown deletes by project/zone/name).
const ResourceKindInstance = "instance"

// Plan lists the cluster's GCP resources by klotho's ownership label: every
// instance carrying it (including machines CAPG created from the tagged
// template, across all zones) and the reserved control plane address, plus
// anything recorded in state that is not found by label.
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

	insts, err := p.compute.ListInstancesByLabel(ctx, p.cfg.Project, ClusterLabelKey, Tag(name))
	if err != nil {
		return nil, errorf("listing instances: %w", err)
	}
	for _, in := range insts {
		add(cluster.Resource{Kind: ResourceKindInstance, ID: in.Name, Name: zoneShort(in.Zone)})
	}

	// The bootstrap instance, from state, in case a label search missed it.
	if state.Bootstrap != nil {
		if project, zone, inst, ok := parseProviderID(state.Bootstrap.ID); ok && project == p.cfg.Project {
			add(cluster.Resource{Kind: ResourceKindInstance, ID: inst, Name: zone})
		}
	}

	// Anything state recorded that the label search did not surface (the
	// reserved control plane address among them).
	for _, r := range state.Resources {
		add(r)
	}
	return plan, nil
}

// Teardown removes everything in the plan: every instance is deleted (and
// waited out), then the reserved global address is released.
func (p *Provider) Teardown(ctx context.Context, state *cluster.State, plan *infrastructure.Plan) error {
	if plan == nil {
		return nil
	}
	type inst struct{ zone, name string }
	var instances []inst
	var addresses []string
	for _, r := range plan.Resources {
		switch r.Kind {
		case ResourceKindInstance:
			zone := r.Name
			if zone == "" {
				zone = p.cfg.Zone
			}
			instances = append(instances, inst{zone: zone, name: r.ID})
		case ResourceKindAddress:
			addresses = append(addresses, r.ID)
		}
	}

	for _, in := range instances {
		op, err := p.compute.DeleteInstance(ctx, p.cfg.Project, in.zone, in.name)
		if err != nil {
			if notFound(err) {
				continue
			}
			return errorf("deleting instance %q: %w", in.name, err)
		}
		if _, err := p.compute.WaitZoneOperation(ctx, p.cfg.Project, in.zone, op.Name); err != nil && !notFound(err) {
			return errorf("waiting for instance %q to be deleted: %w", in.name, err)
		}
	}
	for _, a := range addresses {
		op, err := p.compute.DeleteGlobalAddress(ctx, p.cfg.Project, a)
		if err != nil {
			if notFound(err) {
				continue
			}
			return errorf("releasing address %q: %w", a, err)
		}
		if _, err := p.compute.WaitGlobalOperation(ctx, p.cfg.Project, op.Name); err != nil && !notFound(err) {
			return errorf("waiting for address %q to be released: %w", a, err)
		}
	}
	return nil
}

// zoneShort returns the short zone name from a zone URL
// (".../zones/us-central1-a" -> "us-central1-a"); a bare name is returned as is.
func zoneShort(zone string) string {
	if i := strings.LastIndex(zone, "/"); i >= 0 {
		return zone[i+1:]
	}
	return zone
}

// parseProviderID splits "gce://<project>/<zone>/<instance>".
func parseProviderID(id string) (project, zone, instance string, ok bool) {
	rest, found := strings.CutPrefix(id, ProviderIDPrefix)
	if !found {
		return "", "", "", false
	}
	parts := strings.Split(rest, "/")
	if len(parts) != 3 || parts[0] == "" || parts[1] == "" || parts[2] == "" {
		return "", "", "", false
	}
	return parts[0], parts[1], parts[2], true
}
