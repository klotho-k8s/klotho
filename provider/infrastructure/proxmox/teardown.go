package proxmox

import (
	"context"
	"errors"
	"fmt"
	"sort"
	"strconv"
	"strings"

	"github.com/klotho-k8s/klotho/pkg/cluster"
	"github.com/klotho-k8s/klotho/provider/infrastructure"
)

// Resource kinds in a Plan.
const (
	ResourceKindVM  = "vm"
	ResourceKindISO = "iso"
)

// Plan lists the cluster's VMs (every VM carrying Tag(cluster), including
// machines the Cluster API Proxmox provider created from the tagged
// templates) and the bootstrap ISO wherever it is found on an iso-capable
// storage of an online node.
//
// VM resources have ID "<node>/<vmid>" and the VM name as Name. ISO resources
// have ID "<node>/<volid>" (for example "pve1/local:iso/c1-bootstrap-cidata.iso")
// and the file name as Name. Resources of these kinds recorded in state are
// included too; Teardown still checks each VM's tag before touching it.
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

	vms, err := p.resources(ctx, resourceVM)
	if err != nil {
		return nil, err
	}
	sort.Slice(vms, func(i, j int) bool { return vms[i].VMID < vms[j].VMID })
	for _, v := range vms {
		if v.Type == "qemu" && v.Template == 0 && resHasTag(v, tag) {
			add(cluster.Resource{Kind: ResourceKindVM, ID: vmID(v.Node, int(v.VMID)), Name: v.Name})
		}
	}

	nodes, err := p.onlineNodes(ctx)
	if err != nil {
		return nil, err
	}
	file := ISOName(name)
	sharedSeen := map[string]bool{}
	for _, n := range nodes {
		storages, err := p.client.NodeRef(n.Node).Storages(ctx)
		if err != nil {
			return nil, err
		}
		for _, s := range storages {
			if !storageSupports(s, "iso") || s.Active == 0 || (p.cfg.ISOStorage != "" && s.Name != p.cfg.ISOStorage) {
				continue
			}
			if s.Shared != 0 {
				if sharedSeen[s.Name] {
					continue
				}
				sharedSeen[s.Name] = true
			}
			vols, err := p.listISOs(ctx, n.Node, s.Name)
			if err != nil {
				return nil, err
			}
			want := s.Name + ":iso/" + file
			for _, v := range vols {
				if v.Volid == want {
					add(cluster.Resource{Kind: ResourceKindISO, ID: n.Node + "/" + v.Volid, Name: file})
				}
			}
		}
	}

	for _, r := range state.Resources {
		if r.Kind == ResourceKindVM || r.Kind == ResourceKindISO {
			add(r)
		}
	}
	return plan, nil
}

// Teardown removes the plan's VMs, then its ISOs. A running VM is stopped
// (not shut down) first, and VMs are deleted with purge. A VM without the
// cluster's tag, or an ISO not named after the cluster, is refused. Failures
// do not stop the rest; they are returned together.
func (p *Provider) Teardown(ctx context.Context, state *cluster.State, plan *infrastructure.Plan) error {
	if state == nil || state.Spec.Name == "" {
		return errorf("state has no cluster name")
	}
	if plan == nil {
		return nil
	}
	tag := Tag(state.Spec.Name)
	file := ISOName(state.Spec.Name)
	var errs []error

	for _, r := range plan.Resources {
		if r.Kind != ResourceKindVM {
			continue
		}
		node, vmid, err := parseVMID(r.ID)
		if err != nil {
			errs = append(errs, err)
			continue
		}
		st, err := p.vmStatus(ctx, node, vmid)
		if errors.Is(err, errVMNotFound) {
			continue
		}
		if err != nil {
			errs = append(errs, fmt.Errorf("VM %d on %s: %w", vmid, node, err))
			continue
		}
		name := vmName(st)
		if !hasTag(vmTags(st), tag) {
			errs = append(errs, errorf("refusing to delete VM %d (%s) on %s: it does not carry tag %s", vmid, name, node, tag))
			continue
		}
		if err := p.destroyVM(ctx, node, vmid); err != nil {
			errs = append(errs, fmt.Errorf("VM %d (%s) on %s: %w", vmid, name, node, err))
		}
	}

	for _, r := range plan.Resources {
		if r.Kind != ResourceKindISO {
			continue
		}
		node, volid, ok := strings.Cut(r.ID, "/")
		storage, path, ok2 := strings.Cut(volid, ":")
		if !ok || !ok2 || node == "" || storage == "" {
			errs = append(errs, errorf("malformed ISO resource ID %q", r.ID))
			continue
		}
		if path != "iso/"+file {
			errs = append(errs, errorf("refusing to delete %s: not this cluster's bootstrap ISO", volid))
			continue
		}
		upid, err := p.deleteVolume(ctx, node, storage, volid)
		if err == nil {
			err = p.wait(ctx, upid)
		}
		if err != nil {
			errs = append(errs, fmt.Errorf("ISO %s on %s: %w", volid, node, err))
		}
	}
	return errors.Join(errs...)
}

func vmID(node string, vmid int) string { return node + "/" + strconv.Itoa(vmid) }

func parseVMID(id string) (string, int, error) {
	node, num, ok := strings.Cut(id, "/")
	vmid, err := strconv.Atoi(num)
	if !ok || node == "" || err != nil || vmid <= 0 {
		return "", 0, errorf("malformed VM resource ID %q, want <node>/<vmid>", id)
	}
	return node, vmid, nil
}
