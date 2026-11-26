package proxmox

import (
	"context"
	"crypto/x509"
	"errors"
	"fmt"
	"io"
	"net/netip"
	"net/url"
	"strings"

	gp "github.com/luthermonson/go-proxmox"
)

// UserAgent is the User-Agent header NewClient sends.
const UserAgent = "klotho-proxmox"

const apiRoot = "/api2/json"

// NewClient connects to the Proxmox VE API described by cfg's connection
// settings (URL, TokenID, Secret, Insecure and CACert).
//
// URL may be the server root ("https://pve.example:8006") or the API root
// ("https://pve.example:8006/api2/json"); a bare root gets /api2/json
// appended. TokenID is the full token identifier "USER@REALM!TOKENID" and
// Secret its UUID secret. Proxmox nodes ship with self-signed certificates:
// CACert trusts the PEM-encoded cluster CA (/etc/pve/pve-root-ca.pem) and
// Insecure disables verification altogether. Further go-proxmox options are
// applied after these.
func NewClient(cfg Config, opts ...gp.Option) (*gp.Client, error) {
	base, err := normalizeBaseURL(cfg.URL)
	if err != nil {
		return nil, err
	}
	if !strings.Contains(cfg.TokenID, "@") || !strings.Contains(cfg.TokenID, "!") {
		return nil, fmt.Errorf("proxmox: token ID %q must have the form USER@REALM!TOKENID", cfg.TokenID)
	}
	if cfg.Secret == "" {
		return nil, errors.New("proxmox: empty token secret")
	}
	gopts := []gp.Option{
		gp.WithAPIToken(cfg.TokenID, cfg.Secret),
		gp.WithUserAgent(UserAgent),
		// Callers log returned errors themselves.
		gp.WithLogger(discardLogger{}),
	}
	if cfg.Insecure {
		gopts = append(gopts, gp.WithInsecureSkipVerify())
	}
	if len(cfg.CACert) > 0 {
		pool := x509.NewCertPool()
		if !pool.AppendCertsFromPEM(cfg.CACert) {
			return nil, errors.New("proxmox: no certificates found in CA PEM")
		}
		gopts = append(gopts, gp.WithRootCAs(pool))
	}
	return gp.NewClient(base, append(gopts, opts...)...), nil
}

func normalizeBaseURL(raw string) (string, error) {
	u, err := url.Parse(strings.TrimSpace(raw))
	if err != nil {
		return "", fmt.Errorf("proxmox: invalid base URL: %w", err)
	}
	if u.Scheme != "https" && u.Scheme != "http" {
		return "", fmt.Errorf("proxmox: base URL %q must use http or https", raw)
	}
	if u.Host == "" {
		return "", fmt.Errorf("proxmox: base URL %q has no host", raw)
	}
	u.RawQuery, u.Fragment, u.RawPath = "", "", ""
	u.Path = strings.TrimRight(u.Path, "/")
	if u.Path == "" {
		u.Path = apiRoot
	}
	return u.String(), nil
}

// discardLogger satisfies go-proxmox's logger interface and drops everything.
type discardLogger struct{}

func (discardLogger) Debugf(string, ...interface{}) {}
func (discardLogger) Errorf(string, ...interface{}) {}
func (discardLogger) Infof(string, ...interface{})  {}
func (discardLogger) Warnf(string, ...interface{})  {}

// checkName rejects identifiers (node and storage names) that would alter
// the request path, since go-proxmox interpolates them unescaped.
func checkName(kind, name string) error {
	if name == "" {
		return fmt.Errorf("proxmox: %s is required", kind)
	}
	if strings.ContainsAny(name, "/?#% \t\r\n") {
		return fmt.Errorf("proxmox: invalid %s %q", kind, name)
	}
	return nil
}

func checkVM(node string, vmid int) error {
	if err := checkName("node", node); err != nil {
		return err
	}
	if vmid <= 0 {
		return fmt.Errorf("proxmox: invalid VMID %d", vmid)
	}
	return nil
}

// Resource kinds accepted by resources.
const (
	resourceVM      = "vm"
	resourceStorage = "storage"
)

// cluster returns a request-free cluster handle; the library's
// Client.Cluster accessor first reads /cluster/status, which nothing here
// needs.
func (p *Provider) cluster() *gp.Cluster {
	return (&gp.Cluster{}).New(p.client)
}

// resources lists cluster-wide resources of kind (resourceVM or
// resourceStorage). Listing VMs this way is the cheapest way to find guests
// by tag across all nodes.
func (p *Provider) resources(ctx context.Context, kind string) ([]*gp.ClusterResource, error) {
	res, err := p.cluster().Resources(ctx, kind)
	if err != nil {
		return nil, err
	}
	out := make([]*gp.ClusterResource, 0, len(res))
	for _, r := range res {
		if r != nil {
			out = append(out, r)
		}
	}
	return out, nil
}

// findTemplates returns every QEMU VM template in the cluster.
func (p *Provider) findTemplates(ctx context.Context) ([]*gp.ClusterResource, error) {
	res, err := p.resources(ctx, resourceVM)
	if err != nil {
		return nil, err
	}
	var out []*gp.ClusterResource
	for _, r := range res {
		if isQemuTemplate(r) {
			out = append(out, r)
		}
	}
	return out, nil
}

// nextID returns the next free VMID in the cluster. The ID is only free at
// the time of the call; a concurrent create can still take it.
func (p *Provider) nextID(ctx context.Context) (int, error) {
	return p.cluster().NextID(ctx)
}

func isQemuTemplate(r *gp.ClusterResource) bool {
	return r.Template != 0 && r.Type == "qemu"
}

// resTags returns a cluster resource's tags.
func resTags(r *gp.ClusterResource) []string { return parseTags(r.Tags) }

// resHasTag reports whether a cluster resource carries tag. Proxmox compares
// tags case-insensitively, and so does this.
func resHasTag(r *gp.ClusterResource, tag string) bool { return hasTag(resTags(r), tag) }

// storageContent lists the content types a storage allows.
func storageContent(s *gp.Storage) []string { return splitCSV(s.Content) }

// storageSupports reports whether the storage allows content of the given
// type ("iso", "images", ...).
func storageSupports(s *gp.Storage, content string) bool {
	for _, c := range storageContent(s) {
		if c == content {
			return true
		}
	}
	return false
}

func splitCSV(s string) []string {
	var out []string
	for _, p := range strings.Split(s, ",") {
		if p = strings.TrimSpace(p); p != "" {
			out = append(out, p)
		}
	}
	return out
}

// vmRef returns a request-free handle to a QEMU VM.
func (p *Provider) vmRef(node string, vmid int) (*gp.VirtualMachine, error) {
	if err := checkVM(node, vmid); err != nil {
		return nil, err
	}
	return p.client.NodeRef(node).VirtualMachineRef(vmid), nil
}

// upidOf extracts the UPID from a library task, which is nil when the server
// returned no UPID.
func upidOf(t *gp.Task) string {
	if t == nil {
		return ""
	}
	return string(t.UPID)
}

// cloneVM clones the VM or template templateID on node into newID and
// returns the clone task's UPID. It returns an error matching errVMExists if
// newID is already taken. A false opts.Full is omitted from the request,
// which makes Proxmox create a linked clone of a template.
func (p *Provider) cloneVM(ctx context.Context, node string, templateID, newID int, opts *gp.VirtualMachineCloneOptions) (string, error) {
	vm, err := p.vmRef(node, templateID)
	if err != nil {
		return "", err
	}
	// A zero newid would make the library pick one itself, which the caller
	// could then not learn from the UPID alone.
	if newID <= 0 {
		return "", fmt.Errorf("proxmox: invalid new VMID %d", newID)
	}
	if opts == nil {
		opts = &gp.VirtualMachineCloneOptions{}
	}
	if opts.Pool != "" {
		if err := validatePoolID(opts.Pool); err != nil {
			return "", err
		}
	}
	opts.NewID = newID
	_, task, err := vm.Clone(ctx, opts)
	if err != nil {
		if wrapped := classify(err, errVMExists, isAlreadyExists); errors.Is(wrapped, errVMExists) {
			return "", wrapped
		}
		return "", classifyVM(err)
	}
	return upidOf(task), nil
}

// setVMConfig updates VM configuration keys synchronously (PUT .../config).
// To remove keys, pass them comma-separated under "delete".
func (p *Provider) setVMConfig(ctx context.Context, node string, vmid int, cfg map[string]string) error {
	vm, err := p.vmRef(node, vmid)
	if err != nil {
		return err
	}
	if len(cfg) == 0 {
		return nil
	}
	opts := make([]gp.VirtualMachineOption, 0, len(cfg))
	for k, v := range cfg {
		opts = append(opts, gp.VirtualMachineOption{Name: k, Value: v})
	}
	return classifyVM(vm.ConfigSync(ctx, opts...))
}

// readVMConfig returns the VM's current configuration as raw key/value
// pairs. Values keep the JSON types Proxmox sends (strings and numbers). The
// library only decodes into its fixed config struct, which drops keys it
// does not model, so the raw map is read with its generic GET.
func (p *Provider) readVMConfig(ctx context.Context, node string, vmid int) (map[string]any, error) {
	if err := checkVM(node, vmid); err != nil {
		return nil, err
	}
	cfg := map[string]any{}
	if err := p.client.Get(ctx, fmt.Sprintf("/nodes/%s/qemu/%d/config", node, vmid), &cfg); err != nil {
		return nil, classifyVM(err)
	}
	return cfg, nil
}

// vmStatus returns the VM with its runtime status and config loaded. It
// returns an error matching errVMNotFound when the VM does not exist on node.
func (p *Provider) vmStatus(ctx context.Context, node string, vmid int) (*gp.VirtualMachine, error) {
	vm, err := p.vmRef(node, vmid)
	if err != nil {
		return nil, err
	}
	if err := vm.Ping(ctx); err != nil {
		return nil, classifyVM(err)
	}
	return vm, nil
}

// vmTags returns a VM's tags from its status, or from its config where the
// status endpoint omits them.
func vmTags(vm *gp.VirtualMachine) []string {
	tags := vm.Tags
	if tags == "" && vm.VirtualMachineConfig != nil {
		tags = vm.VirtualMachineConfig.Tags
	}
	return parseTags(tags)
}

func vmName(vm *gp.VirtualMachine) string {
	if vm.Name == "" && vm.VirtualMachineConfig != nil {
		return vm.VirtualMachineConfig.Name
	}
	return vm.Name
}

func (p *Provider) vmTask(node string, vmid int, act func(*gp.VirtualMachine) (*gp.Task, error)) (string, error) {
	vm, err := p.vmRef(node, vmid)
	if err != nil {
		return "", err
	}
	task, err := act(vm)
	if err != nil {
		return "", classifyVM(err)
	}
	return upidOf(task), nil
}

// startVM starts the VM and returns the task UPID.
func (p *Provider) startVM(ctx context.Context, node string, vmid int) (string, error) {
	return p.vmTask(node, vmid, func(vm *gp.VirtualMachine) (*gp.Task, error) { return vm.Start(ctx) })
}

// stopVM stops the VM immediately (like pulling the power plug) and returns
// the task UPID.
func (p *Provider) stopVM(ctx context.Context, node string, vmid int) (string, error) {
	return p.vmTask(node, vmid, func(vm *gp.VirtualMachine) (*gp.Task, error) { return vm.Stop(ctx) })
}

// deleteVM destroys the VM and its owned volumes and returns the task UPID.
// With purge, the VMID is also removed from backup, replication and HA
// configuration, and disks that match the VMID but are not referenced in the
// config are destroyed too. The VM must be stopped.
func (p *Provider) deleteVM(ctx context.Context, node string, vmid int, purge bool) (string, error) {
	return p.vmTask(node, vmid, func(vm *gp.VirtualMachine) (*gp.Task, error) {
		return vm.Delete(ctx, &gp.VirtualMachineDeleteOptions{
			Purge:                    gp.IntOrBool(purge),
			DestroyUnreferencedDisks: gp.IntOrBool(purge),
		})
	})
}

// resizeDisk grows the disk at device (for example "scsi0") to size (a
// Proxmox size such as "20G") and returns the resize task's UPID, which may
// be empty when Proxmox completed the change synchronously.
func (p *Provider) resizeDisk(ctx context.Context, node string, vmid int, device, size string) (string, error) {
	return p.vmTask(node, vmid, func(vm *gp.VirtualMachine) (*gp.Task, error) { return vm.ResizeDisk(ctx, device, size) })
}

// agentInterfaces asks the guest agent for the VM's network interfaces
// (without the loopback). It returns an error matching errAgentNotRunning
// while the VM or its agent is not running, and errAgentNotConfigured when
// the agent is not enabled in the VM config.
func (p *Provider) agentInterfaces(ctx context.Context, node string, vmid int) ([]*gp.AgentNetworkIface, error) {
	vm, err := p.vmRef(node, vmid)
	if err != nil {
		return nil, err
	}
	ifaces, err := vm.AgentGetNetworkIFaces(ctx)
	if err != nil {
		return nil, classifyAgent(err)
	}
	return ifaces, nil
}

// guestIPv4Addresses returns the usable IPv4 addresses in ifaces, skipping
// loopback, link-local and unspecified addresses, in interface order.
func guestIPv4Addresses(ifaces []*gp.AgentNetworkIface) []netip.Addr {
	var out []netip.Addr
	for _, iface := range ifaces {
		if iface == nil {
			continue
		}
		for _, a := range iface.IPAddresses {
			if a == nil {
				continue
			}
			ip, err := netip.ParseAddr(a.IPAddress)
			if err != nil || !ip.Is4() || ip.IsLoopback() || ip.IsLinkLocalUnicast() || ip.IsUnspecified() {
				continue
			}
			out = append(out, ip)
		}
	}
	return out
}

func (p *Provider) storageRef(node, storage string) (*gp.Storage, error) {
	if err := checkName("node", node); err != nil {
		return nil, err
	}
	if err := checkName("storage", storage); err != nil {
		return nil, err
	}
	return p.client.NodeRef(node).StorageRef(storage), nil
}

// listISOs lists the iso content of storage on node. The library's
// GetContent has no content-type filter, and its result type does not carry
// the content type to filter on afterwards, so the filtered listing uses the
// generic GET.
func (p *Provider) listISOs(ctx context.Context, node, storage string) ([]*gp.StorageContent, error) {
	if _, err := p.storageRef(node, storage); err != nil {
		return nil, err
	}
	var items []*gp.StorageContent
	err := p.client.GetWithParams(ctx, fmt.Sprintf("/nodes/%s/storage/%s/content", node, storage),
		map[string]string{"content": "iso"}, &items)
	if err != nil {
		return nil, err
	}
	out := items[:0]
	for _, it := range items {
		if it != nil {
			out = append(out, it)
		}
	}
	return out, nil
}

// deleteVolume deletes a volume and returns the task UPID. volid is the full
// volume ID ("local:iso/seed.iso"). The UPID is empty if the server completed
// the deletion without a task.
func (p *Provider) deleteVolume(ctx context.Context, node, storage, volid string) (string, error) {
	st, err := p.storageRef(node, storage)
	if err != nil {
		return "", err
	}
	if volid == "" || strings.ContainsAny(volid, "?#") {
		return "", fmt.Errorf("proxmox: invalid volume ID %q", volid)
	}
	task, err := st.DeleteContent(ctx, volid)
	if err != nil {
		return "", err
	}
	return upidOf(task), nil
}

// uploadISO uploads an ISO image to storage on node and returns the UPID of
// the server-side import task; wait for it before attaching the ISO. The body
// is streamed from r without being buffered, and size must be the exact
// number of bytes r yields because it determines the Content-Length.
//
// Storage.Upload in the library only accepts a file path and takes no
// context, so the reader-based client call is used directly with the same
// path and fields. Cancelling ctx aborts the transfer of the body; it cannot
// interrupt the wait for the server's response once the body has been sent.
func (p *Provider) uploadISO(ctx context.Context, node, storage, filename string, r io.Reader, size int64) (string, error) {
	if _, err := p.storageRef(node, storage); err != nil {
		return "", err
	}
	if filename == "" || strings.ContainsAny(filename, "/\\\r\n\"") {
		return "", fmt.Errorf("proxmox: invalid upload filename %q", filename)
	}
	if r == nil {
		return "", errors.New("proxmox: nil upload reader")
	}
	if size < 0 {
		return "", fmt.Errorf("proxmox: upload size must be known, got %d", size)
	}
	if err := ctx.Err(); err != nil {
		return "", err
	}
	var upid gp.UPID
	err := p.client.UploadReader(fmt.Sprintf("/nodes/%s/storage/%s/upload", node, storage),
		map[string]string{"content": "iso"}, filename, &ctxReader{ctx: ctx, r: r}, size, &upid)
	if err != nil {
		if ctxErr := ctx.Err(); ctxErr != nil {
			return "", fmt.Errorf("proxmox: upload aborted: %w", ctxErr)
		}
		return "", err
	}
	return string(upid), nil
}

// ctxReader fails reads once ctx is done. The library's upload call takes no
// context, so this is what lets cancellation abort an in-flight upload body.
type ctxReader struct {
	ctx context.Context
	r   io.Reader
}

func (c *ctxReader) Read(p []byte) (int, error) {
	if err := c.ctx.Err(); err != nil {
		return 0, err
	}
	return c.r.Read(p)
}
