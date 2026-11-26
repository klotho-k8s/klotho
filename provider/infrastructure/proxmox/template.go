package proxmox

import (
	"fmt"
	"sort"
	"strconv"
	"strings"

	gp "github.com/luthermonson/go-proxmox"
	"github.com/klotho-k8s/klotho/pkg/generate"
)

// selectTemplate picks, among the QEMU templates of one node (or of the whole
// cluster when node is empty), the one that tags select under policy, with
// the semantics of the Cluster API Proxmox provider's templateSelector: exact
// wants a template whose tags equal tags; uniqueSubset wants the only template
// carrying every tag; bestSubset wants, of the templates carrying every tag,
// the one with the fewest extra tags. Tags compare case-insensitively, as
// Proxmox does. Nothing matching, or more than one template matching, is an
// error that names the candidates.
//
// The Proxmox provider itself always resolves cluster-wide (node empty) and
// clones from wherever the template lives to the node it scheduled, which
// works only when the template's disks are on shared storage. klotho resolves
// per node for its own bootstrap clone and falls back to the cluster-wide
// answer when that storage is shared; see Provider.findTemplateByTags.
func selectTemplate(node string, templates []*gp.ClusterResource, tags []string, policy string) (*gp.ClusterResource, error) {
	policy, err := generate.NormalizeTemplateMatchPolicy(policy)
	if err != nil {
		return nil, errorf("%v", err)
	}
	where := "in the cluster"
	if node != "" {
		where = fmt.Sprintf("on node %q", node)
	}
	want := tagSet(tags)
	type candidate struct {
		res    *gp.ClusterResource
		extras int
	}
	var cands []candidate
	for _, t := range templates {
		if !isQemuTemplate(t) || (node != "" && t.Node != node) {
			continue
		}
		have := tagSet(resTags(t))
		if !subset(want, have) {
			continue
		}
		extras := len(have) - len(want)
		if policy == generate.TemplateMatchExact && extras != 0 {
			continue
		}
		cands = append(cands, candidate{res: t, extras: extras})
	}
	if len(cands) == 0 {
		var there []string
		for _, t := range templates {
			if isQemuTemplate(t) && (node == "" || t.Node == node) {
				there = append(there, describeTemplate(t))
			}
		}
		msg := fmt.Sprintf("no template %s matches tags [%s] with policy %s", where, strings.Join(tags, " "), policy)
		if len(there) == 0 {
			if node == "" {
				return nil, errorf("%s: the cluster has no templates", msg)
			}
			return nil, errorf("%s: the node has no templates", msg)
		}
		return nil, errorf("%s; templates there: %s", msg, strings.Join(there, ", "))
	}
	if policy == generate.TemplateMatchBestSubset {
		sort.SliceStable(cands, func(i, j int) bool { return cands[i].extras < cands[j].extras })
		if len(cands) == 1 || cands[1].extras > cands[0].extras {
			return cands[0].res, nil
		}
		cands = func() []candidate {
			var tied []candidate
			for _, c := range cands {
				if c.extras == cands[0].extras {
					tied = append(tied, c)
				}
			}
			return tied
		}()
	}
	if len(cands) == 1 {
		return cands[0].res, nil
	}
	names := make([]string, len(cands))
	for i, c := range cands {
		names[i] = describeTemplate(c.res)
	}
	hint := "give one of them a distinguishing tag"
	if policy == generate.TemplateMatchExact {
		hint = "give one of them a distinguishing tag or use policy bestSubset"
	}
	return nil, errorf("%d templates %s match tags [%s] with policy %s: %s; %s",
		len(cands), where, strings.Join(tags, " "), policy, strings.Join(names, ", "), hint)
}

// diskStorages lists, sorted and without duplicates, the storages a VM
// config references through its disk and drive entries (scsiN, virtioN,
// sataN, ideN, efidisk0, tpmstate0): the storage is the part before the
// first colon of a "storage:volume,option=..." value. Drives without a
// volume ("none,media=cdrom") are skipped. These are the storages a full
// clone copies from and the clone's node must therefore be able to see.
func diskStorages(cfg map[string]any) []string {
	seen := map[string]bool{}
	var out []string
	for key, v := range cfg {
		if !isDriveKey(key) {
			continue
		}
		s, _ := v.(string)
		vol, _, _ := strings.Cut(s, ",")
		storage, _, ok := strings.Cut(vol, ":")
		if !ok || storage == "" || storage == "none" {
			continue
		}
		if !seen[storage] {
			seen[storage] = true
			out = append(out, storage)
		}
	}
	sort.Strings(out)
	return out
}

// cloudInitDrives lists, sorted, the drives of a VM config that hold a
// Proxmox cloud-init drive (a volume named "vm-<id>-cloudinit"), as
// "ide2=local-lvm:vm-9000-cloudinit". A template that carries one hands every
// clone a second cloud-init datasource next to the NoCloud ISO the Cluster
// API Proxmox provider attaches, and cloud-init may read the wrong one.
func cloudInitDrives(cfg map[string]any) []string {
	var out []string
	for key, v := range cfg {
		s, _ := v.(string)
		vol, _, _ := strings.Cut(s, ",")
		if isDriveKey(key) && strings.Contains(vol, "-cloudinit") {
			out = append(out, key+"="+vol)
		}
	}
	sort.Strings(out)
	return out
}

// isDriveKey reports whether a VM config key names a disk or drive.
func isDriveKey(key string) bool {
	for _, prefix := range []string{"scsi", "virtio", "sata", "ide", "efidisk", "tpmstate"} {
		if rest, ok := strings.CutPrefix(key, prefix); ok && rest != "" {
			if _, err := strconv.Atoi(rest); err == nil {
				return true
			}
		}
	}
	return false
}

func describeTemplate(t *gp.ClusterResource) string {
	return "VM " + strconv.Itoa(int(t.VMID)) + " (" + t.Name + ") tags [" + strings.Join(resTags(t), " ") + "]"
}

func tagSet(tags []string) map[string]struct{} {
	out := make(map[string]struct{}, len(tags))
	for _, t := range tags {
		out[strings.ToLower(strings.TrimSpace(t))] = struct{}{}
	}
	return out
}

// subset reports whether every key of want is in have.
func subset(want, have map[string]struct{}) bool {
	for k := range want {
		if _, ok := have[k]; !ok {
			return false
		}
	}
	return true
}

// templateNodes lists the nodes that hold a QEMU template, sorted.
func templateNodes(templates []*gp.ClusterResource) []string {
	seen := map[string]bool{}
	var out []string
	for _, t := range templates {
		if isQemuTemplate(t) && !seen[t.Node] {
			seen[t.Node] = true
			out = append(out, t.Node)
		}
	}
	sort.Strings(out)
	return out
}
