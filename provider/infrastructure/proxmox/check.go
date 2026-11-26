package proxmox

import (
	"fmt"
	"net/netip"
	"strings"

	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"

	"github.com/klotho-k8s/klotho/pkg/generate"
	"github.com/klotho-k8s/klotho/provider"
)

// Check implements provider.Checker. It reports a cluster network (pod or
// service CIDR) that overlaps the network the nodes live on, as described by
// the ProxmoxCluster object: the gateway's subnet, the node IP ranges, the
// DNS servers and the control plane endpoint. Overlap means traffic for one
// of them is routed into the other; the usual symptom is pods that cannot
// reach the LAN's DNS or the API server. It changes nothing.
//
// It also reviews template selectors: when the machine templates select their
// VM template by tag, the tags must select exactly one template in the whole
// cluster, and when the ProxmoxCluster's allowedNodes include nodes other
// than that template's, its disks must be on shared storage, because the
// Cluster API Proxmox provider clones from the template's node to the node it
// schedules each machine to. That check uses the listing the last Prepare or
// Launch took and is skipped when there is none; Check never calls the API.
func (p *Provider) Check(bundle []*unstructured.Unstructured) []provider.Finding {
	var clusterObj, pxc *unstructured.Unstructured
	var machineTemplates []*unstructured.Unstructured
	for _, o := range bundle {
		if o == nil {
			continue
		}
		switch {
		case o.GetKind() == "Cluster" && strings.HasPrefix(o.GetAPIVersion(), generate.ClusterGroup+"/"):
			clusterObj = o
		case o.GetKind() == "ProxmoxCluster":
			pxc = o
		case o.GetKind() == "ProxmoxMachineTemplate":
			machineTemplates = append(machineTemplates, o)
		}
	}
	if pxc == nil {
		return nil
	}
	out := p.checkTemplateSelectors(pxc, machineTemplates)
	if clusterObj == nil {
		return out
	}
	pods, services, err := generate.ClusterNetwork(clusterObj)
	if err != nil {
		return append(out, provider.Finding{Object: provider.ObjectRef(clusterObj), Field: "spec.clusterNetwork", Message: err.Error()})
	}
	node := nodeNetworks(pxc)
	if len(node) == 0 {
		return out
	}

	for _, nw := range []struct {
		field  string
		blocks []string
	}{
		{"spec.clusterNetwork.pods.cidrBlocks", pods},
		{"spec.clusterNetwork.services.cidrBlocks", services},
	} {
		for _, block := range nw.blocks {
			prefix, err := netip.ParsePrefix(block)
			if err != nil {
				out = append(out, provider.Finding{
					Object: provider.ObjectRef(clusterObj), Field: nw.field,
					Message: fmt.Sprintf("%q is not a CIDR (%v)", block, err),
				})
				continue
			}
			for _, n := range node {
				if prefix.Overlaps(n.prefix) {
					out = append(out, provider.Finding{
						Object: provider.ObjectRef(clusterObj), Field: nw.field,
						Message: fmt.Sprintf("%s overlaps %s (%s from %s). Traffic for the one is routed into "+
							"the other, so pods cannot reach the LAN's DNS, gateway or API server, and the "+
							"cluster never comes up. The Proxmox cluster template fixes the pod network at "+
							"192.168.0.0/16, which collides with most home and office LANs; choose a free "+
							"range such as 10.244.0.0/16 with --pod-cidr (or --service-cidr), or set it in "+
							"the manifest.", block, n.prefix, n.what, provider.ObjectRef(pxc)),
					})
					break
				}
			}
		}
	}
	return out
}

// checkTemplateSelectors reviews each machine template that selects its VM
// template by tag the way the Cluster API Proxmox provider will: the selector
// is resolved once across the whole cluster and must match exactly one
// template, and every machine is cloned from that template to the node the
// scheduler picks among the ProxmoxCluster's allowedNodes. A clone to a node
// other than the template's own works only when the template's disks are on
// shared storage, so a template on local storage is reported for every other
// allowed node. Judged by the templates and storages remembered from the
// last Prepare or Launch; nothing remembered, nothing reported.
func (p *Provider) checkTemplateSelectors(pxc *unstructured.Unstructured, machineTemplates []*unstructured.Unstructured) []provider.Finding {
	allowed, _, _ := unstructured.NestedStringSlice(pxc.Object, "spec", "allowedNodes")
	templates, ok := p.templates()
	if !ok {
		return nil
	}
	var out []provider.Finding
	for _, mt := range machineTemplates {
		tags, found, err := unstructured.NestedStringSlice(mt.Object, "spec", "template", "spec", "templateSelector", "matchTags")
		if err != nil || !found || len(tags) == 0 {
			continue
		}
		policy, _, _ := unstructured.NestedString(mt.Object, "spec", "template", "spec", "templateMatchPolicy")
		tmpl, err := selectTemplate("", templates, tags, policy)
		if err != nil {
			out = append(out, provider.Finding{
				Object: provider.ObjectRef(mt), Field: "spec.template.spec.templateSelector",
				Message: fmt.Sprintf("%v. The Cluster API Proxmox provider resolves a template selector once across "+
					"the whole cluster and needs exactly one match, so no machine of this template can be cloned.",
					strings.TrimPrefix(err.Error(), "proxmox provider: ")),
			})
			continue
		}
		if drives := p.templateCloudInitDrives(int(tmpl.VMID)); len(drives) > 0 {
			out = append(out, provider.Finding{
				Object: provider.ObjectRef(mt), Field: "spec.template.spec.templateSelector",
				Message: fmt.Sprintf("tags [%s] select %s on node %q, which carries a Proxmox cloud-init drive (%s). "+
					"The Cluster API Proxmox provider gives each clone its own cloud-init ISO and leaves this drive "+
					"attached, so the guest sees two cloud-init datasources and may boot from the template's instead: "+
					"no network, no bootstrap, and a machine that never joins. Remove the drive from the template "+
					"(qm set %d --delete %s).",
					strings.Join(tags, " "), describeTemplate(tmpl), tmpl.Node, strings.Join(drives, ", "),
					tmpl.VMID, strings.Join(driveKeys(drives), " --delete ")),
			})
		}
		var elsewhere []string
		for _, node := range allowed {
			if node != tmpl.Node {
				elsewhere = append(elsewhere, node)
			}
		}
		if len(elsewhere) == 0 {
			continue
		}
		local := p.localDiskStorages(int(tmpl.VMID))
		if len(local) == 0 {
			// Shared storage, or not inspected: nothing to say.
			continue
		}
		out = append(out, provider.Finding{
			Object: provider.ObjectRef(mt), Field: "spec.template.spec.templateSelector",
			Message: fmt.Sprintf("tags [%s] select %s on node %q, whose disks are on %s, not shared storage. "+
				"The Cluster API Proxmox provider clones from the template's node to the node it schedules each "+
				"machine to, so a machine placed on %s cannot be cloned. Move the template to storage every "+
				"allowed node shares, or restrict spec.allowedNodes of %s to %q.",
				strings.Join(tags, " "), describeTemplate(tmpl), tmpl.Node, strings.Join(local, ", "),
				quoteList(elsewhere), provider.ObjectRef(pxc), tmpl.Node),
		})
	}
	return out
}

// driveKeys returns the "ide2" part of cloudInitDrives entries ("ide2=...").
func driveKeys(drives []string) []string {
	out := make([]string, len(drives))
	for i, d := range drives {
		out[i], _, _ = strings.Cut(d, "=")
	}
	return out
}

// quoteList renders names as "a", "b" or "c".
func quoteList(names []string) string {
	q := make([]string, len(names))
	for i, n := range names {
		q[i] = fmt.Sprintf("%q", n)
	}
	switch len(q) {
	case 0:
		return ""
	case 1:
		return q[0]
	default:
		return strings.Join(q[:len(q)-1], ", ") + " or " + q[len(q)-1]
	}
}

type nodeNetwork struct {
	prefix netip.Prefix
	what   string
}

// nodeNetworks lists the networks the ProxmoxCluster places nodes in or
// expects them to reach. Values it cannot parse are skipped: the provider's
// own validation reports them.
func nodeNetworks(pxc *unstructured.Unstructured) []nodeNetwork {
	var out []nodeNetwork
	add := func(p netip.Prefix, what string) { out = append(out, nodeNetwork{prefix: p.Masked(), what: what}) }

	prefixLen, _, _ := unstructured.NestedInt64(pxc.Object, "spec", "ipv4Config", "prefix")
	if gw, _, _ := unstructured.NestedString(pxc.Object, "spec", "ipv4Config", "gateway"); gw != "" {
		if addr, err := netip.ParseAddr(gw); err == nil {
			if prefixLen > 0 && prefixLen <= 32 {
				add(netip.PrefixFrom(addr, int(prefixLen)), "the node subnet")
			} else {
				add(netip.PrefixFrom(addr, 32), "the gateway")
			}
		}
	}
	addrs, _, _ := unstructured.NestedStringSlice(pxc.Object, "spec", "ipv4Config", "addresses")
	for _, a := range addrs {
		for _, p := range parseRange(a) {
			add(p, "the node IP range")
		}
	}
	dns, _, _ := unstructured.NestedStringSlice(pxc.Object, "spec", "dnsServers")
	for _, d := range dns {
		if addr, err := netip.ParseAddr(d); err == nil {
			add(netip.PrefixFrom(addr, addr.BitLen()), "a DNS server")
		}
	}
	if host, _, _ := unstructured.NestedString(pxc.Object, "spec", "controlPlaneEndpoint", "host"); host != "" {
		if addr, err := netip.ParseAddr(host); err == nil {
			add(netip.PrefixFrom(addr, addr.BitLen()), "the control plane endpoint")
		}
	}
	return out
}

// parseRange turns an IPAM address entry (a single address, a CIDR, or a
// "first-last" range) into prefixes covering it. A range becomes the smallest
// prefix containing both ends: a superset, which is close enough for an
// overlap warning the user can override.
func parseRange(s string) []netip.Prefix {
	s = strings.TrimSpace(s)
	if p, err := netip.ParsePrefix(s); err == nil {
		return []netip.Prefix{p}
	}
	if a, err := netip.ParseAddr(s); err == nil {
		return []netip.Prefix{netip.PrefixFrom(a, a.BitLen())}
	}
	first, last, ok := strings.Cut(s, "-")
	if !ok {
		return nil
	}
	a, err1 := netip.ParseAddr(strings.TrimSpace(first))
	b, err2 := netip.ParseAddr(strings.TrimSpace(last))
	if err1 != nil || err2 != nil || a.BitLen() != b.BitLen() {
		return nil
	}
	if b.Less(a) {
		a, b = b, a
	}
	bits := a.BitLen()
	for n := bits; n >= 0; n-- {
		p := netip.PrefixFrom(a, n).Masked()
		if p.Contains(b) {
			return []netip.Prefix{p}
		}
	}
	return nil
}
