package hyperv

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"

	"github.com/klotho-k8s/klotho/pkg/cluster"
	"github.com/klotho-k8s/klotho/provider/infrastructure"
)

// Resource kinds recorded in a Plan.
const (
	ResourceKindVM  = "vm"
	ResourceKindISO = "iso"
)

// namePrefix is the prefix every VM of the cluster shares: the bootstrap VM is
// "<cluster>-bootstrap" and the Cluster API Hyper-V provider names the rest
// after their Machine, "<cluster>-...".
func namePrefix(cluster string) string { return cluster + "-" }

// Plan lists every VM whose name begins with "<cluster>-" (the bootstrap VM and
// the machines the provider created) plus the cidata ISO, and any VM resources
// recorded in state.
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

	vms, err := p.client.ListVMs(ctx, namePrefix(name))
	if err != nil {
		return nil, errorf("listing VMs of %q: %w", name, err)
	}
	sort.Slice(vms, func(i, j int) bool { return vms[i].Name < vms[j].Name })
	for _, vm := range vms {
		add(cluster.Resource{Kind: ResourceKindVM, ID: vm.Name, Name: vm.Name})
	}
	for _, r := range state.Resources {
		if r.Kind == ResourceKindVM {
			add(r)
		}
	}

	if iso := p.isoPath(state); iso != "" {
		if _, err := os.Stat(iso); err == nil {
			add(cluster.Resource{Kind: ResourceKindISO, ID: iso, Name: filepath.Base(iso)})
		}
	}
	return plan, nil
}

// Teardown removes the plan's VMs and ISO. A VM is removed only when its name
// still carries the cluster prefix, so an unrelated VM is left alone. Failures
// are collected and returned together.
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
		switch r.Kind {
		case ResourceKindVM:
			if !strings.HasPrefix(r.ID, prefix) {
				errs = append(errs, errorf("refusing to remove VM %q: it does not start with %q", r.ID, prefix))
				continue
			}
			if err := p.client.RemoveVM(ctx, r.ID); err != nil {
				errs = append(errs, fmt.Errorf("removing VM %s: %w", r.ID, err))
			}
		case ResourceKindISO:
			if err := os.Remove(r.ID); err != nil && !os.IsNotExist(err) {
				errs = append(errs, fmt.Errorf("removing ISO %s: %w", r.ID, err))
			}
		}
	}
	return errors.Join(errs...)
}

// LaunchDeletionSidecar finishes a fire-and-forget delete for Hyper-V by
// removing node zero's VM, which Cluster API cannot. The in-cluster Cluster
// API Hyper-V controller runs on the bootstrap VM (node zero) and so cannot
// delete the VM it is running on; after the Cluster object is deleted that VM
// is left behind, orphaned. klotho runs on the host, so this removes node
// zero's VM and its cidata ISO directly over the host client - the same
// operation the --watch sweep performs through Teardown - leaving no orphan.
//
// It implements infrastructure.DeletionSidecar. "Sidecar" names the Proxmox
// provider's detached in-infra reaper; on Hyper-V klotho is already on the
// host, so the teardown runs in process rather than as a detached agent.
//
// It removes only the bootstrap VM, the last node Cluster API leaves behind.
// Hyper-V clusters are single-control-plane dev clusters; a multi-node cluster
// whose workers Cluster API is still deleting should be removed with "klotho
// delete --watch", which waits for Cluster API to drain them before sweeping.
func (p *Provider) LaunchDeletionSidecar(ctx context.Context, state *cluster.State) error {
	if state == nil || state.Spec.Name == "" {
		return errorf("state has no cluster name")
	}
	name := VMName(state.Spec.Name)
	if err := p.client.RemoveVM(ctx, name); err != nil {
		return errorf("removing bootstrap VM %q: %w", name, err)
	}
	if iso := p.isoPath(state); iso != "" {
		if err := os.Remove(iso); err != nil && !os.IsNotExist(err) {
			return errorf("removing cidata ISO %q: %w", iso, err)
		}
	}
	return nil
}

// isoPath is the deterministic cidata ISO path for the cluster, or "" when no
// ISO directory is configured (the ISO then went to a temp directory this
// provider cannot reconstruct).
func (p *Provider) isoPath(state *cluster.State) string {
	r := p.resolve(state.Spec.Variables)
	if r.ISODir == "" {
		return ""
	}
	return filepath.Join(r.ISODir, ISOName(state.Spec.Name))
}
