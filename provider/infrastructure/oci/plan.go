package oci

import (
	"context"
	"time"

	"github.com/oracle/oci-go-sdk/v65/core"

	"github.com/klotho-k8s/klotho/pkg/cluster"
	"github.com/klotho-k8s/klotho/provider/infrastructure"
)

// ResourceKindInstance is the Plan kind for an OCI compute instance. ID is the
// instance OCID; Name is its display name.
const ResourceKindInstance = "instance"

// Plan lists the cluster's OCI instances by klotho's ownership freeform tag:
// every instance in the compartment carrying it (including machines CAPOCI
// launched from the tagged template), plus the bootstrap instance from state and
// anything else state recorded that the tag search did not surface.
//
// The compute API cannot filter by freeform tag server-side, so this lists the
// compartment's instances and filters on the tag in-process — the same approach
// CAPOCI itself uses for ownership checks.
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
		add(cluster.Resource{Kind: ResourceKindInstance, ID: strVal(insts[i].Id), Name: strVal(insts[i].DisplayName)})
	}

	// The bootstrap instance, from state, in case a tag search missed it.
	if state.Bootstrap != nil {
		if ocid, ok := ocidFromProviderID(state.Bootstrap.ID); ok && ocid != "" {
			add(cluster.Resource{Kind: ResourceKindInstance, ID: ocid, Name: state.Bootstrap.Name})
		}
	}

	// Anything state recorded that the tag search did not surface.
	for _, r := range state.Resources {
		add(r)
	}
	return plan, nil
}

// Teardown removes everything in the plan: every instance is terminated (and
// waited out so a dependent record is not left dangling).
//
// NOTE: like the other cloud providers (GCP, Linode), this reaches zero but is
// not yet hands-off. The in-cluster CAPOCI controller runs on the last
// control-plane node, so it cannot issue the final terminate from inside; the
// external-API deletion finisher (a DeletionSidecar) is TODO. A live `klotho
// delete` needs `--force` plus a check that the control-plane load balancer, any
// CAPOCI-created VCN/subnets, and the dynamic-group policy are gone. See
// docs/PROVIDERS.md and TESTING.md.
func (p *Provider) Teardown(ctx context.Context, _ *cluster.State, plan *infrastructure.Plan) error {
	if plan == nil {
		return nil
	}
	var ids []string
	for _, r := range plan.Resources {
		if r.Kind != ResourceKindInstance {
			continue
		}
		ids = append(ids, r.ID)
	}
	for _, id := range ids {
		if err := p.client.TerminateInstance(ctx, id); err != nil {
			if isNotFound(err) {
				continue
			}
			return errorf("terminating instance %q: %w", id, err)
		}
	}
	p.waitTerminated(ctx, ids)
	return nil
}

// taggedInstances lists every non-terminated instance in the compartment
// carrying the cluster's klotho ownership freeform tag.
func (p *Provider) taggedInstances(ctx context.Context, clusterName string) ([]core.Instance, error) {
	all, err := p.client.ListInstances(ctx, p.cfg.CompartmentID)
	if err != nil {
		return nil, errorf("listing instances: %w", err)
	}
	var out []core.Instance
	for i := range all {
		if all[i].LifecycleState == core.InstanceLifecycleStateTerminated {
			continue
		}
		if hasClusterTag(all[i].FreeformTags, clusterName) {
			out = append(out, all[i])
		}
	}
	return out, nil
}

// findBootstrapInstance finds the single running bootstrap instance by its
// display name and klotho's ownership freeform tag.
func (p *Provider) findBootstrapInstance(ctx context.Context, clusterName string) (*core.Instance, error) {
	insts, err := p.taggedInstances(ctx, clusterName)
	if err != nil {
		return nil, err
	}
	display := MachineName(clusterName)
	var found []core.Instance
	for i := range insts {
		if strVal(insts[i].DisplayName) == display {
			found = append(found, insts[i])
		}
	}
	switch len(found) {
	case 1:
		return &found[0], nil
	case 0:
		return nil, errorf("no instance named %q carrying tag %s=%s", display, ClusterTagKey, Tag(clusterName))
	default:
		return nil, errorf("%d instances named %q carry tag %s=%s", len(found), display, ClusterTagKey, Tag(clusterName))
	}
}

// waitTerminated waits, best effort, for the instances to reach a terminated
// state.
func (p *Provider) waitTerminated(ctx context.Context, ids []string) {
	deadline := time.Now().Add(p.launchWait)
	for {
		remaining := false
		for _, id := range ids {
			inst, err := p.client.GetInstance(ctx, id)
			if err != nil {
				continue
			}
			if inst.LifecycleState != core.InstanceLifecycleStateTerminated {
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
