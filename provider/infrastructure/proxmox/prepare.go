package proxmox

import (
	"context"
	"errors"
	"net/netip"
	"strconv"
	"strings"

	gp "github.com/luthermonson/go-proxmox"

	"github.com/klotho-k8s/klotho/pkg/cluster"
	"github.com/klotho-k8s/klotho/provider/infrastructure"
)

// target is a Resolved configuration completed against the live cluster.
type target struct {
	Resolved
	// Template is the template to clone; its Node is where the clone runs.
	Template *gp.ClusterResource
	// VMs is the cluster's VM listing taken while locating, reused to pick a
	// VMID and to look for leftovers.
	VMs []*gp.ClusterResource
}

// locate resolves spec and checks it against the cluster: the template exists
// and is a template, the node exists and is online, and the ISO storage
// exists on the node, is active, and allows iso content. It fills the node
// and the ISO storage when Resolve left them to be chosen: the node becomes
// the template's node when that is online, else the first online node by
// name; the storage becomes the node's first active iso-capable storage.
func (p *Provider) locate(ctx context.Context, spec *cluster.Spec) (*target, error) {
	r, err := p.Resolve(spec)
	if err != nil {
		return nil, err
	}
	t := &target{Resolved: r}

	if t.VMs, err = p.resources(ctx, resourceVM); err != nil {
		return nil, err
	}
	p.remember(t.VMs)
	p.rememberStorages(ctx)

	nodes, err := p.client.Nodes(ctx)
	if err != nil {
		return nil, err
	}
	status := map[string]string{}
	var online []string
	for _, n := range nodes {
		if n == nil {
			continue
		}
		status[n.Node] = n.Status
		if n.Status == "online" {
			online = append(online, n.Node)
		}
	}
	if len(r.TemplateTags) > 0 {
		t.Template, err = p.findTemplateByTags(ctx, t.VMs, r, status)
	} else {
		t.Template, err = findTemplate(t.VMs, r)
	}
	if err != nil {
		return nil, err
	}
	if t.Node == "" {
		switch {
		case status[t.Template.Node] == "online":
			t.Node = t.Template.Node
		case len(online) > 0:
			t.Node = minString(online)
		default:
			return nil, errorf("no online node")
		}
	}
	switch st, ok := status[t.Node]; {
	case !ok:
		return nil, errorf("node %q does not exist", t.Node)
	case st != "online":
		return nil, errorf("node %q is %s, not online", t.Node, st)
	}

	storages, err := p.client.NodeRef(t.Node).Storages(ctx)
	if err != nil {
		return nil, err
	}
	if t.ISOStorage == "" {
		for _, s := range storages {
			if s.Active != 0 && storageSupports(s, "iso") {
				t.ISOStorage = s.Name
				break
			}
		}
		if t.ISOStorage == "" {
			return nil, errorf("node %q has no active storage that allows iso content", t.Node)
		}
		return t, nil
	}
	for _, s := range storages {
		if s.Name != t.ISOStorage {
			continue
		}
		if !storageSupports(s, "iso") {
			return nil, errorf("storage %q on node %q does not allow iso content (content: %s)",
				s.Name, t.Node, strings.Join(storageContent(s), ","))
		}
		if s.Active == 0 {
			return nil, errorf("storage %q on node %q is not active", s.Name, t.Node)
		}
		return t, nil
	}
	return nil, errorf("storage %q does not exist on node %q", t.ISOStorage, t.Node)
}

func findTemplate(vms []*gp.ClusterResource, r Resolved) (*gp.ClusterResource, error) {
	if r.TemplateID > 0 {
		for _, v := range vms {
			if int(v.VMID) != r.TemplateID {
				continue
			}
			if !isQemuTemplate(v) {
				return nil, errorf("VM %d (%s) is not a QEMU template", v.VMID, v.Name)
			}
			return v, nil
		}
		return nil, errorf("template %d does not exist", r.TemplateID)
	}
	var found []*gp.ClusterResource
	for _, v := range vms {
		if isQemuTemplate(v) && resHasTag(v, r.TemplateTag) {
			found = append(found, v)
		}
	}
	switch len(found) {
	case 1:
		return found[0], nil
	case 0:
		return nil, errorf("no template carries tag %q", r.TemplateTag)
	default:
		ids := make([]string, len(found))
		for i, f := range found {
			ids[i] = strconv.Itoa(int(f.VMID))
		}
		return nil, errorf("%d templates carry tag %q (%s); set TemplateID",
			len(found), r.TemplateTag, strings.Join(ids, ", "))
	}
}

// findTemplateByTags selects the bootstrap template by r.TemplateTags on
// r.Node, or, when no node is configured, on the first online node (by name)
// that has a matching template. When r.Node has no match but the tags select
// exactly one template cluster-wide whose disks are all on shared storage,
// that template is used and the clone is sent to r.Node, which is the same
// cross-node clone the Cluster API Proxmox provider performs. A configured
// TemplateID must be the template the tags select.
//
// The template the tags select cluster-wide is inspected along the way (its
// disk storages are recorded) so that Check can judge the bundle's selectors
// without another round trip.
func (p *Provider) findTemplateByTags(ctx context.Context, vms []*gp.ClusterResource, r Resolved, status map[string]string) (*gp.ClusterResource, error) {
	var (
		tmpl *gp.ClusterResource
		err  error
	)
	// The provider's own answer, remembered for Check whether or not the
	// bootstrap uses it.
	clusterWide, cwErr := selectTemplate("", vms, r.TemplateTags, r.TemplateMatchPolicy)
	if cwErr == nil {
		p.inspectTemplate(ctx, clusterWide)
	}
	if r.Node != "" {
		if tmpl, err = selectTemplate(r.Node, vms, r.TemplateTags, r.TemplateMatchPolicy); err != nil {
			if cwErr != nil {
				return nil, err
			}
			local := p.localDiskStorages(int(clusterWide.VMID))
			if local == nil {
				return nil, errorf("%v; %s on node %q matches, but its disks could not be inspected, so it cannot be cloned to %q",
					err, describeTemplate(clusterWide), clusterWide.Node, r.Node)
			}
			if len(local) > 0 {
				return nil, errorf("%v; %s on node %q matches, but its disks are on %s, which is not shared storage, so it cannot be cloned to %q",
					err, describeTemplate(clusterWide), clusterWide.Node, strings.Join(local, ", "), r.Node)
			}
			tmpl = clusterWide
		}
	} else {
		var problems []string
		found := false
		for _, node := range templateNodes(vms) {
			if status[node] != "online" {
				continue
			}
			if tmpl, err = selectTemplate(node, vms, r.TemplateTags, r.TemplateMatchPolicy); err == nil {
				found = true
				break
			}
			problems = append(problems, err.Error())
		}
		if !found {
			if len(problems) == 0 {
				return nil, errorf("no online node has a template matching tags [%s]", strings.Join(r.TemplateTags, " "))
			}
			return nil, errorf("no online node has a template matching tags [%s]: %s",
				strings.Join(r.TemplateTags, " "), strings.Join(problems, "; "))
		}
	}
	if r.TemplateID > 0 && int(tmpl.VMID) != r.TemplateID {
		return nil, errorf("templateId %d is set but tags [%s] select %s on node %q; drop the templateId or change the tags",
			r.TemplateID, strings.Join(r.TemplateTags, " "), describeTemplate(tmpl), tmpl.Node)
	}
	return tmpl, nil
}

// remember keeps the QEMU templates of a cluster listing for Check.
func (p *Provider) remember(vms []*gp.ClusterResource) {
	var templates []*gp.ClusterResource
	for _, v := range vms {
		if isQemuTemplate(v) {
			templates = append(templates, v)
		}
	}
	p.mu.Lock()
	p.seen = templates
	p.mu.Unlock()
}

// templates returns the templates remembered by the last Prepare or Launch,
// and whether there was one.
func (p *Provider) templates() ([]*gp.ClusterResource, bool) {
	p.mu.Lock()
	defer p.mu.Unlock()
	return p.seen, p.seen != nil
}

// rememberStorages records which storages are shared across nodes, from the
// cluster's storage resources. A listing failure leaves the record empty,
// which Check and the bootstrap fallback treat as "unknown", never as
// "shared".
func (p *Provider) rememberStorages(ctx context.Context) {
	res, err := p.resources(ctx, resourceStorage)
	if err != nil {
		return
	}
	shared := map[string]bool{}
	for _, r := range res {
		if r.Storage != "" {
			shared[r.Storage] = shared[r.Storage] || r.Shared != 0
		}
	}
	p.mu.Lock()
	p.shared = shared
	p.mu.Unlock()
}

// inspectTemplate records the storages a template's disks live on. A config
// read failure records nothing, which later reads as "unknown".
func (p *Provider) inspectTemplate(ctx context.Context, t *gp.ClusterResource) {
	cfg, err := p.readVMConfig(ctx, t.Node, int(t.VMID))
	if err != nil {
		return
	}
	p.mu.Lock()
	if p.disks == nil {
		p.disks = map[int][]string{}
		p.cloudInit = map[int][]string{}
	}
	p.disks[int(t.VMID)] = diskStorages(cfg)
	p.cloudInit[int(t.VMID)] = cloudInitDrives(cfg)
	p.mu.Unlock()
}

// templateCloudInitDrives returns the Proxmox cloud-init drives an inspected
// template carries; nil when it was not inspected or carries none.
func (p *Provider) templateCloudInitDrives(vmid int) []string {
	p.mu.Lock()
	defer p.mu.Unlock()
	return p.cloudInit[vmid]
}

// localDiskStorages returns the storages of template vmid's disks that are
// not shared across nodes: empty when every disk is on shared storage, so the
// template can be cloned to any node; nil when the template was not
// inspected or the storage listing is missing, so nothing can be said.
func (p *Provider) localDiskStorages(vmid int) []string {
	p.mu.Lock()
	defer p.mu.Unlock()
	disks, ok := p.disks[vmid]
	if !ok || p.shared == nil {
		return nil
	}
	local := []string{}
	for _, s := range disks {
		if !p.shared[s] {
			local = append(local, s)
		}
	}
	return local
}

// Endpoint reads the control plane endpoint from spec: the address from
// CONTROL_PLANE_ENDPOINT_IP, which must be a unicast IP address, and the port
// from CONTROL_PLANE_ENDPOINT_PORT or DefaultPort.
func Endpoint(spec *cluster.Spec) (cluster.Endpoint, error) {
	raw := strings.TrimSpace(spec.Variables[VarEndpointIP])
	if raw == "" {
		return cluster.Endpoint{}, errorf("%s is required: this provider does not allocate the control plane address", VarEndpointIP)
	}
	ip, err := netip.ParseAddr(raw)
	if err != nil {
		return cluster.Endpoint{}, errorf("%s=%q is not an IP address", VarEndpointIP, raw)
	}
	if ip.IsUnspecified() || ip.IsLoopback() || ip.IsMulticast() {
		return cluster.Endpoint{}, errorf("%s=%q is not a usable unicast address", VarEndpointIP, raw)
	}
	port := DefaultPort
	if s := strings.TrimSpace(spec.Variables[VarEndpointPort]); s != "" {
		port, err = strconv.Atoi(s)
		if err != nil || port < 1 || port > 65535 {
			return cluster.Endpoint{}, errorf("%s=%q is not a port", VarEndpointPort, s)
		}
	}
	return cluster.Endpoint{Host: ip.String(), Port: port}, nil
}

// Prepare validates the configuration against the cluster and returns the
// user-supplied endpoint. It allocates nothing, so Created is always empty.
//
// VIP conflict detection is best-effort: the VIP is rejected when a VM
// carries the Cluster API Proxmox provider's "ip_net0_<address>" tag for it,
// or when the guest agent of a running VM tagged by any klotho cluster
// reports it (kube-vip binds the VIP on its current leader, so another
// cluster's VIP shows up there). Other guests, and guests whose agent does
// not answer, are not checked.
func (p *Provider) Prepare(ctx context.Context, spec *cluster.Spec) (*infrastructure.Prepared, error) {
	if spec == nil {
		return nil, errorf("nil spec")
	}
	ep, err := Endpoint(spec)
	if err != nil {
		return nil, err
	}
	t, err := p.locate(ctx, spec)
	if err != nil {
		return nil, err
	}
	if t.BridgeSet {
		bridges, err := p.client.NodeRef(t.Node).Networks(ctx, "bridge")
		if err != nil {
			return nil, err
		}
		if !hasBridge(bridges, t.Bridge) {
			return nil, errorf("bridge %q does not exist on node %q", t.Bridge, t.Node)
		}
	}
	if t.Pool != "" {
		if _, err := p.client.Pool(ctx, t.Pool); err != nil {
			if err = classifyPool(err); errors.Is(err, errPoolNotFound) {
				return nil, errorf("pool %q does not exist", t.Pool)
			}
			return nil, err
		}
	}
	if owner := p.vipOwner(ctx, t.VMs, ep.Host); owner != "" {
		return nil, errorf("control plane address %s is already in use by %s", ep.Host, owner)
	}
	prepared := &infrastructure.Prepared{Endpoint: ep}
	if len(t.TemplateTags) > 0 {
		// The cluster template still wants a fixed template; the selected
		// one stands in until the bundle's machine templates get their
		// selector. User variables still win over these.
		prepared.Variables = map[string]string{
			VarTemplateID: strconv.Itoa(int(t.Template.VMID)),
			VarSourceNode: t.Template.Node,
		}
	}
	return prepared, nil
}

func hasBridge(bridges gp.NodeNetworks, name string) bool {
	for _, b := range bridges {
		if b != nil && b.Iface == name {
			return true
		}
	}
	return false
}

// vipOwner returns a description of a VM that already holds addr, or "".
func (p *Provider) vipOwner(ctx context.Context, vms []*gp.ClusterResource, addr string) string {
	ipTag := "ip_net0_" + addr
	for _, v := range vms {
		if resHasTag(v, ipTag) {
			return describeVM(v)
		}
	}
	for _, v := range vms {
		if v.Template != 0 || v.Type != "qemu" || v.Status != "running" || !hasKlothoTag(resTags(v)) {
			continue
		}
		ifaces, err := p.agentInterfaces(ctx, v.Node, int(v.VMID))
		if err != nil {
			continue
		}
		for _, ip := range guestIPv4Addresses(ifaces) {
			if ip.String() == addr {
				return describeVM(v)
			}
		}
	}
	return ""
}

func describeVM(v *gp.ClusterResource) string {
	return "VM " + strconv.Itoa(int(v.VMID)) + " (" + v.Name + ") on node " + v.Node
}

func minString(s []string) string {
	m := s[0]
	for _, v := range s[1:] {
		if v < m {
			m = v
		}
	}
	return m
}
