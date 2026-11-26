package proxmox

import (
	"context"
	"sort"
	"strconv"
	"strings"

	gp "github.com/luthermonson/go-proxmox"

	"github.com/klotho-k8s/klotho/provider/infrastructure"
)

// Inventory kinds reported by Discover.
const (
	KindNode     = "node"
	KindTemplate = "template"
	KindStorage  = "storage"
	KindBridge   = "bridge"
)

// Discover reports online nodes, VM templates, and per online node the
// storages that allow iso or images content and the Linux bridges. Node items
// are keyed by node name, templates by VMID, and storages and bridges by
// "<node>/<name>".
func (p *Provider) Discover(ctx context.Context) (infrastructure.Inventory, error) {
	nodes, err := p.onlineNodes(ctx)
	if err != nil {
		return nil, err
	}
	inv := infrastructure.Inventory{
		KindNode: {}, KindTemplate: {}, KindStorage: {}, KindBridge: {},
	}
	for _, n := range nodes {
		inv[KindNode] = append(inv[KindNode], infrastructure.Item{
			ID: n.Node, Name: n.Node,
			Attrs: map[string]string{
				"status":  n.Status,
				"maxcpu":  strconv.Itoa(n.MaxCPU),
				"cpu":     strconv.FormatFloat(n.CPU, 'f', 3, 64),
				"maxmem":  strconv.FormatUint(n.MaxMem, 10),
				"mem":     strconv.FormatUint(n.Mem, 10),
				"freemem": strconv.FormatUint(sub(n.MaxMem, n.Mem), 10),
			},
		})
	}

	templates, err := p.findTemplates(ctx)
	if err != nil {
		return nil, err
	}
	sort.Slice(templates, func(i, j int) bool { return templates[i].VMID < templates[j].VMID })
	for _, t := range templates {
		inv[KindTemplate] = append(inv[KindTemplate], infrastructure.Item{
			ID: strconv.FormatUint(t.VMID, 10), Name: t.Name,
			Attrs: map[string]string{
				"vmid":    strconv.FormatUint(t.VMID, 10),
				"node":    t.Node,
				"tags":    joinTags(resTags(t)),
				"maxdisk": strconv.FormatUint(t.MaxDisk, 10),
				"maxmem":  strconv.FormatUint(t.MaxMem, 10),
			},
		})
	}

	for _, n := range nodes {
		storages, err := p.client.NodeRef(n.Node).Storages(ctx)
		if err != nil {
			return nil, err
		}
		for _, s := range storages {
			if !storageSupports(s, "iso") && !storageSupports(s, "images") {
				continue
			}
			inv[KindStorage] = append(inv[KindStorage], infrastructure.Item{
				ID: n.Node + "/" + s.Name, Name: s.Name,
				Attrs: map[string]string{
					"node":    n.Node,
					"type":    s.Type,
					"content": strings.Join(storageContent(s), ","),
					"iso":     strconv.FormatBool(storageSupports(s, "iso")),
					"images":  strconv.FormatBool(storageSupports(s, "images")),
					"shared":  strconv.FormatBool(s.Shared != 0),
					"active":  strconv.FormatBool(s.Active != 0),
					"avail":   strconv.FormatUint(s.Avail, 10),
					"total":   strconv.FormatUint(s.Total, 10),
				},
			})
		}
		bridges, err := p.client.NodeRef(n.Node).Networks(ctx, "bridge")
		if err != nil {
			return nil, err
		}
		for _, b := range bridges {
			if b == nil {
				continue
			}
			inv[KindBridge] = append(inv[KindBridge], infrastructure.Item{
				ID: n.Node + "/" + b.Iface, Name: b.Iface,
				Attrs: map[string]string{
					"node":      n.Node,
					"active":    strconv.FormatBool(b.Active != 0),
					"vlanAware": strconv.FormatBool(b.BridgeVLANAware != 0),
					"ports":     b.BridgePorts,
					"cidr":      b.CIDR,
					"gateway":   b.Gateway,
					"comments":  b.Comments,
				},
			})
		}
	}
	return inv, nil
}

// onlineNodes lists the online nodes sorted by name.
func (p *Provider) onlineNodes(ctx context.Context) ([]*gp.NodeStatus, error) {
	all, err := p.client.Nodes(ctx)
	if err != nil {
		return nil, err
	}
	var out []*gp.NodeStatus
	for _, n := range all {
		if n != nil && n.Status == "online" {
			out = append(out, n)
		}
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Node < out[j].Node })
	return out, nil
}

func sub(a, b uint64) uint64 {
	if b > a {
		return 0
	}
	return a - b
}
