package vz

import (
	"context"
	"errors"
	"fmt"
	"sort"
	"strings"

	"github.com/klotho-k8s/klotho/pkg/cluster"
	"github.com/klotho-k8s/klotho/provider/infrastructure"
)

// ResourceKindVM is the Plan/state resource kind for a vz VM.
const ResourceKindVM = "vm"

// namePrefix is the prefix every VM of the cluster shares: the bootstrap VM is
// "<cluster>-bootstrap" and the Cluster API vz provider names the rest after
// their Machine, "<cluster>-...".
func namePrefix(cluster string) string { return cluster + "-" }

// Plan lists every VM whose name begins with "<cluster>-" and carries the
// cluster tag in its notes (the bootstrap VM and the machines the provider
// created), plus any VM resources recorded in state. The per-VM working disk and
// seed are deleted with their VM by Teardown, so they are not listed separately.
// The base image is never touched.
func (p *Provider) Plan(ctx context.Context, state *cluster.State) (*infrastructure.Plan, error) {
	if state == nil || state.Spec.Name == "" {
		return nil, errorf("state has no cluster name")
	}
	name := state.Spec.Name
	tag := Tag(name)
	prefix := namePrefix(name)
	plan := &infrastructure.Plan{}
	seen := map[string]bool{}
	add := func(r cluster.Resource) {
		key := r.Kind + "|" + r.ID
		if !seen[key] {
			seen[key] = true
			plan.Resources = append(plan.Resources, r)
		}
	}

	vms, err := p.client.ListVMs(ctx)
	if err != nil {
		return nil, errorf("listing VMs of %q: %w", name, err)
	}
	sort.Slice(vms, func(i, j int) bool { return vms[i].Name < vms[j].Name })
	for _, vm := range vms {
		if strings.HasPrefix(vm.Name, prefix) && hasTag(vm.Notes, tag) {
			add(cluster.Resource{Kind: ResourceKindVM, ID: vm.Name, Name: vm.Name})
		}
	}
	for _, r := range state.Resources {
		if r.Kind == ResourceKindVM {
			add(r)
		}
	}
	return plan, nil
}

// Teardown removes the plan's VMs through the daemon: it force-stops each
// runner and forgets the VM, deleting its working disk and seed with it. Because
// the daemon runs on the host, not inside the cluster, this is the clean external
// finisher: klotho destroys node zero itself with no risk of the self-hosted
// deletion deadlock that an in-cluster agent would hit, so vz needs no deletion
// sidecar. A VM is removed only when its name still carries the cluster prefix,
// so an unrelated VM is left alone. Failures are collected and returned together.
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
		if r.Kind != ResourceKindVM {
			continue
		}
		if !strings.HasPrefix(r.ID, prefix) {
			errs = append(errs, errorf("refusing to remove VM %q: it does not start with %q", r.ID, prefix))
			continue
		}
		if err := p.client.StopVM(ctx, r.ID, true); err != nil {
			errs = append(errs, fmt.Errorf("stopping VM %s: %w", r.ID, err))
			continue
		}
		if err := p.client.RemoveVM(ctx, r.ID); err != nil {
			errs = append(errs, fmt.Errorf("removing VM %s: %w", r.ID, err))
		}
	}
	return errors.Join(errs...)
}

// hasTag reports whether a VM's notes carry tag. The notes may hold other text;
// the tag is matched as a whitespace- or comma-delimited token and, failing
// that, as a substring.
func hasTag(notes, tag string) bool {
	if tag == "" {
		return false
	}
	for _, field := range strings.FieldsFunc(notes, func(r rune) bool {
		return r == ',' || r == ' ' || r == '\t' || r == '\n' || r == '\r'
	}) {
		if field == tag {
			return true
		}
	}
	return strings.Contains(notes, tag)
}
