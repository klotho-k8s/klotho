package proxmox

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"fmt"
	"io"
	"net/http"
	"net/netip"
	"sort"
	"strconv"
	"strings"
	"time"

	gp "github.com/luthermonson/go-proxmox"

	"github.com/klotho-k8s/klotho/pkg/cluster"
	"github.com/klotho-k8s/klotho/provider/infrastructure"
)

// Deletion-sidecar defaults. The sidecar is a short-lived, unprivileged Alpine
// LXC klotho builds and starts so a fire-and-forget delete can finish a
// cluster's teardown after klotho exits; see LaunchDeletionSidecar.
const (
	// DefaultAlpineRootfsURL is the Alpine minirootfs klotho fetches, bakes the
	// sweep into, and uploads as a one-shot container template. It is an
	// official Alpine release tarball served over HTTPS.
	DefaultAlpineRootfsURL = "https://dl-cdn.alpinelinux.org/alpine/v3.21/releases/x86_64/alpine-minirootfs-3.21.3-x86_64.tar.gz"
	// DefaultAlpineRootfsSHA256 is empty: the default path trusts HTTPS to the
	// official mirror and does not verify a pinned digest, so a mirror bump
	// does not wedge delete. Pin SidecarConfig.AlpineRootfsSHA256 (with a
	// matching URL) to require verification.
	DefaultAlpineRootfsSHA256 = ""
	// DefaultSidecarCurlURL and DefaultSidecarJqURL are the statically linked
	// curl and jq binaries klotho bakes into the sidecar's rootfs, so the sweep
	// runs over the Proxmox API with no package install and no internet, only
	// LAN reachability. Both are immutable GitHub release assets, so the
	// digests below are pinned: baking an executable into the sweep warrants
	// verifying it, and an immutable asset cannot wedge delete by changing.
	DefaultSidecarCurlURL    = "https://github.com/moparisthebest/static-curl/releases/download/v8.7.1/curl-amd64"
	DefaultSidecarCurlSHA256 = "af5959046be89599c42085d64bfea87a9ecb5021452ba8eeb8ce04f1e0b9e6be"
	DefaultSidecarJqURL      = "https://github.com/jqlang/jq/releases/download/jq-1.7.1/jq-linux-amd64"
	DefaultSidecarJqSHA256   = "5942c9b0934e510ee61eb3e30273f1b3fe2590df93933a93d7c58b81d19c8ff5"
	// DefaultSidecarTokenTTL bounds the scoped API token klotho mints for the
	// sidecar. The token self-expires, capping the credential's lifetime even
	// if the sidecar is interrupted before it removes itself.
	DefaultSidecarTokenTTL = 30 * time.Minute
	// DefaultSidecarMemoryMiB sizes the sidecar container.
	DefaultSidecarMemoryMiB = 128
	// DefaultSidecarRootfsGiB sizes the sidecar's throwaway rootfs.
	DefaultSidecarRootfsGiB = 1

	// SidecarTag marks the sidecar container so it is recognisable and never
	// mistaken for a cluster node (it is not a <klotho-cluster> VM).
	SidecarTag = "klotho-sidecar"

	// sweepScriptPath is where the baked sweep lands in the container, and
	// sweepEnvPath is the file it sources its parameters and credential from.
	sweepScriptPath = "usr/local/bin/klotho-sweep.sh"
	sweepEnvPath    = "etc/klotho/sweep.env"
)

var _ infrastructure.DeletionSidecar = (*Provider)(nil)

// newAPIToken is the POST /access/users/{userid}/token/{tokenid} response.
type newAPIToken struct {
	FullTokenID string `json:"full-tokenid"`
	Value       string `json:"value"`
}

// LaunchDeletionSidecar builds a throwaway Alpine LXC that finishes a cluster's
// teardown and then removes itself, so klotho can return as soon as the Cluster
// object is deleted. It is the default delete's counterpart to the in-process
// watch+sweep that --watch runs. Implements infrastructure.DeletionSidecar.
//
// The sidecar runs the sweep baked into its own init, over the Proxmox API,
// with a credential scoped to the task when klotho can mint one and the
// cluster's own token otherwise. The sidecar: waits for Cluster API to finish
// deleting what it can, destroys the control plane VM the cluster could not
// remove while running on it, frees the bootstrap ISO, deletes its own one-shot
// template, and finally destroys its own container.
func (p *Provider) LaunchDeletionSidecar(ctx context.Context, state *cluster.State) error {
	if state == nil || state.Spec.Name == "" {
		return errorf("state has no cluster name")
	}
	if p.cfg.URL == "" || p.cfg.TokenID == "" || p.cfg.Secret == "" {
		return errorf("deletion sidecar needs the Proxmox URL and token; configure proxmox.url/tokenId/secret or use `klotho delete --watch`")
	}
	name := state.Spec.Name

	node, err := p.sidecarNode(ctx)
	if err != nil {
		return err
	}
	tmplStorage, err := p.sidecarStorage(ctx, node, "vztmpl", p.cfg.Sidecar.TemplateStorage)
	if err != nil {
		return fmt.Errorf("choosing a template storage for the deletion sidecar: %w", err)
	}
	rootfsStorage := firstNonEmpty(p.cfg.Sidecar.RootfsStorage, p.cfg.Sidecar.TemplateStorage)
	rootfsStorage, err = p.sidecarStorage(ctx, node, "rootdir", rootfsStorage)
	if err != nil {
		return fmt.Errorf("choosing a rootfs storage for the deletion sidecar: %w", err)
	}

	isoVolids, err := p.sidecarISOs(ctx, state)
	if err != nil {
		return err
	}

	vmid, err := p.nextID(ctx)
	if err != nil {
		return fmt.Errorf("allocating a VMID for the deletion sidecar: %w", err)
	}

	// minted is best effort; the sidecar runs with the cluster token when a
	// scoped one cannot be created. Either way it carries a working credential.
	tokenID, secret, _ := p.mintSidecarToken(ctx, name)

	// Address the sidecar from the cluster's own network config: this
	// environment has no DHCP, so the baked sweep brings eth0 up with this
	// static address and reaches the Proxmox API over the LAN.
	net := p.sidecarNetwork(state)

	env := p.sweepEnv(sweepParams{
		clusterName:   name,
		node:          node,
		selfVMID:      vmid,
		tokenID:       tokenID,
		secret:        secret,
		isoVolids:     isoVolids,
		rootfsStorage: rootfsStorage,
		net:           net,
	})

	// The sidecar deletes its own template as its last cleanup, so its volid
	// must be baked into the env before the template is built. The filename is
	// deterministic, so the volid is known in advance.
	templateFile := fmt.Sprintf("klotho-sweep-%s-%d.tar.gz", sanitizeName(name), vmid)
	tmplVolid := tmplStorage + ":vztmpl/" + templateFile
	env[envTemplateVolid] = node + "|" + tmplVolid
	env[envTemplateStorage] = tmplStorage

	tmpl, err := p.buildSidecarTemplate(ctx, env)
	if err != nil {
		return fmt.Errorf("building the deletion sidecar template: %w", err)
	}
	uploaded, err := p.uploadVZTmpl(ctx, node, tmplStorage, templateFile, tmpl)
	if err != nil {
		return fmt.Errorf("uploading the deletion sidecar template: %w", err)
	}
	if uploaded != tmplVolid {
		// Should not happen; the volid is derived from the same inputs.
		_, _ = p.deleteVolume(ctx, node, tmplStorage, uploaded)
		return errorf("sidecar template volid mismatch: built for %q, uploaded %q", tmplVolid, uploaded)
	}

	if err := p.createSidecarContainer(ctx, sidecarSpec{
		node:          node,
		vmid:          vmid,
		clusterName:   name,
		ostemplate:    tmplVolid,
		rootfsStorage: rootfsStorage,
		bridge:        p.sidecarBridge(),
		net:           net,
		env:           env,
	}); err != nil {
		// Best effort: do not leave the one-shot template behind if the
		// container never came up to delete it.
		_, _ = p.deleteVolume(ctx, node, tmplStorage, tmplVolid)
		return fmt.Errorf("creating the deletion sidecar container: %w", err)
	}
	return nil
}

// sidecarNode picks the online node the sidecar runs on: the configured Node
// when it is online, otherwise the first online node.
func (p *Provider) sidecarNode(ctx context.Context) (string, error) {
	nodes, err := p.onlineNodes(ctx)
	if err != nil {
		return "", err
	}
	if len(nodes) == 0 {
		return "", errorf("no online node to run the deletion sidecar on")
	}
	if p.cfg.Node != "" {
		for _, n := range nodes {
			if n.Node == p.cfg.Node {
				return p.cfg.Node, nil
			}
		}
	}
	return nodes[0].Node, nil
}

// sidecarStorage returns override when it is an active storage on node that
// allows content, or the first such storage otherwise.
func (p *Provider) sidecarStorage(ctx context.Context, node, content, override string) (string, error) {
	storages, err := p.client.NodeRef(node).Storages(ctx)
	if err != nil {
		return "", err
	}
	var candidates []string
	for _, s := range storages {
		if s.Active == 0 || !storageSupports(s, content) {
			continue
		}
		if override != "" && s.Name == override {
			return override, nil
		}
		candidates = append(candidates, s.Name)
	}
	if override != "" {
		return "", errorf("storage %q on node %s is not active or does not allow %s content", override, node, content)
	}
	if len(candidates) == 0 {
		return "", errorf("no active storage on node %s allows %s content", node, content)
	}
	sort.Strings(candidates)
	return candidates[0], nil
}

// sidecarBridge is the bridge the sidecar's interface attaches to.
func (p *Provider) sidecarBridge() string {
	return firstNonEmpty(p.cfg.Sidecar.Bridge, p.cfg.Bridge, DefaultBridge)
}

// sidecarNet is the sidecar container's addressing: a static address derived
// from the cluster's network variables or configured explicitly, or DHCP when
// neither is available.
type sidecarNet struct {
	static  bool
	ip      string // address without prefix, e.g. 192.168.1.221
	prefix  int
	gateway string
	dns     string
}

// net0 renders the LXC net0 value for this addressing on bridge.
func (n sidecarNet) net0(bridge string) string {
	if !n.static {
		return fmt.Sprintf("name=eth0,bridge=%s,ip=dhcp", bridge)
	}
	s := fmt.Sprintf("name=eth0,bridge=%s,ip=%s/%d", bridge, n.ip, n.prefix)
	if n.gateway != "" {
		s += ",gw=" + n.gateway
	}
	return s
}

// sidecarNetwork determines how the sidecar addresses itself. SidecarConfig.IP
// wins; otherwise a free address is derived from the cluster's node range. When
// neither yields an address it returns DHCP addressing (static=false), the
// legacy behaviour, so environments that do run DHCP keep working.
func (p *Provider) sidecarNetwork(state *cluster.State) sidecarNet {
	s := p.cfg.Sidecar
	vars := map[string]string{}
	if state != nil {
		vars = state.Spec.Variables
	}
	gateway := firstNonEmpty(s.Gateway, vars[VarGateway])
	dns := firstNonEmpty(s.DNS, firstListItem(vars[VarDNSServers]))
	prefix := atoiOr(vars[VarIPPrefix], 0)

	// Explicit sidecar.ip wins; it may be "ip/prefix" or a bare address (prefix
	// then taken from IP_PREFIX).
	if s.IP != "" {
		ip, p2 := splitCIDR(s.IP)
		if p2 > 0 {
			prefix = p2
		}
		if ip != "" && prefix > 0 {
			return sidecarNet{static: true, ip: ip, prefix: prefix, gateway: gateway, dns: dns}
		}
	}

	// Derive a free address: the one just above the IPAM pool's top, which the
	// pool never hands to a node, kept inside the subnet and off the gateway
	// and control-plane endpoint.
	if ip := deriveSidecarIP(vars[VarNodeIPRanges], prefix, vars[VarEndpointIP], gateway); ip != "" && prefix > 0 {
		return sidecarNet{static: true, ip: ip, prefix: prefix, gateway: gateway, dns: dns}
	}

	return sidecarNet{static: false}
}

// firstListItem returns the first element of a bracketed, comma-separated list
// variable such as DNS_SERVERS ("[192.168.1.1,192.168.1.2]" -> "192.168.1.1").
func firstListItem(v string) string {
	v = strings.TrimSpace(v)
	v = strings.TrimPrefix(v, "[")
	v = strings.TrimSuffix(v, "]")
	if i := strings.IndexByte(v, ','); i >= 0 {
		v = v[:i]
	}
	return strings.TrimSpace(v)
}

// splitCIDR splits "ip/prefix" into its address and prefix; a bare address
// returns prefix 0.
func splitCIDR(v string) (string, int) {
	v = strings.TrimSpace(v)
	if ip, pfx, ok := strings.Cut(v, "/"); ok {
		return strings.TrimSpace(ip), atoiOr(pfx, 0)
	}
	return v, 0
}

func atoiOr(v string, def int) int {
	n, err := strconv.Atoi(strings.TrimSpace(v))
	if err != nil {
		return def
	}
	return n
}

// deriveSidecarIP returns a free IPv4 address for the sidecar derived from the
// cluster's NODE_IP_RANGES: the address just above the first range's top (and,
// failing that, just below its bottom), kept within the /prefix subnet and off
// the network, broadcast, gateway and control-plane endpoint addresses. An
// address outside the pool cannot collide with a live cluster node, which is
// addressed from inside it.
func deriveSidecarIP(nodeRanges string, prefix int, endpointIP, gateway string) string {
	if prefix <= 0 || prefix > 32 {
		return ""
	}
	start, end := firstRange(nodeRanges)
	avoid := map[string]bool{}
	for _, a := range []string{endpointIP, gateway} {
		if a != "" {
			avoid[a] = true
		}
	}
	pick := func(seed string, next func(netip.Addr) netip.Addr) string {
		addr, err := netip.ParseAddr(seed)
		if err != nil || !addr.Is4() {
			return ""
		}
		pfx, err := addr.Prefix(prefix)
		if err != nil {
			return ""
		}
		network := pfx.Masked().Addr()
		broadcast := lastAddr(pfx)
		for cand, i := next(addr), 0; cand.IsValid() && i < 16; cand, i = next(cand), i+1 {
			if !pfx.Contains(cand) || cand == network || cand == broadcast || avoid[cand.String()] {
				continue
			}
			return cand.String()
		}
		return ""
	}
	if ip := pick(end, netip.Addr.Next); ip != "" {
		return ip
	}
	return pick(start, netip.Addr.Prev)
}

// firstRange parses the first entry of a NODE_IP_RANGES value
// ("[192.168.1.200-192.168.1.220]") into its start and end addresses. A single
// address returns it as both ends.
func firstRange(v string) (start, end string) {
	v = firstListItem(v)
	if lo, hi, ok := strings.Cut(v, "-"); ok {
		return strings.TrimSpace(lo), strings.TrimSpace(hi)
	}
	return v, v
}

// lastAddr returns the broadcast address of an IPv4 prefix.
func lastAddr(p netip.Prefix) netip.Addr {
	a := p.Masked().Addr().As4()
	v := uint32(a[0])<<24 | uint32(a[1])<<16 | uint32(a[2])<<8 | uint32(a[3])
	if host := 32 - p.Bits(); host > 0 {
		v |= (uint32(1) << host) - 1
	}
	return netip.AddrFrom4([4]byte{byte(v >> 24), byte(v >> 16), byte(v >> 8), byte(v)})
}

// sidecarISOs returns the bootstrap ISO volumes to free, as "<node>|<volid>"
// entries, from the cluster's teardown plan.
func (p *Provider) sidecarISOs(ctx context.Context, state *cluster.State) ([]string, error) {
	plan, err := p.Plan(ctx, state)
	if err != nil {
		return nil, fmt.Errorf("planning the deletion sidecar's ISO cleanup: %w", err)
	}
	var out []string
	for _, r := range plan.Resources {
		if r.Kind != ResourceKindISO {
			continue
		}
		node, volid, ok := strings.Cut(r.ID, "/")
		if !ok || node == "" || volid == "" {
			continue
		}
		out = append(out, node+"|"+volid)
	}
	return out, nil
}

// mintSidecarToken tries to mint a non-privsep, short-expiry API token for the
// cluster's own user so the sidecar carries a credential scoped to the task. It
// is best effort: a least-privilege token usually cannot create tokens, so on
// any failure it falls back to the cluster token and reports minted=false.
func (p *Provider) mintSidecarToken(ctx context.Context, clusterName string) (tokenID, secret string, minted bool) {
	userID, _, ok := strings.Cut(p.cfg.TokenID, "!")
	if !ok || userID == "" {
		return p.cfg.TokenID, p.cfg.Secret, false
	}
	suffix, err := shortRand()
	if err != nil {
		return p.cfg.TokenID, p.cfg.Secret, false
	}
	tokenName := "klotho-del-" + sanitizeName(clusterName) + "-" + suffix
	ttl := p.cfg.Sidecar.TokenTTL
	if ttl <= 0 {
		ttl = DefaultSidecarTokenTTL
	}
	body := map[string]string{
		"privsep": "0",
		"expire":  strconv.FormatInt(time.Now().Add(ttl).Unix(), 10),
		"comment": "klotho deletion sidecar for " + clusterName,
	}
	var nt newAPIToken
	path := fmt.Sprintf("/access/users/%s/token/%s", userID, tokenName)
	if err := p.client.Post(ctx, path, body, &nt); err != nil || nt.FullTokenID == "" || nt.Value == "" {
		return p.cfg.TokenID, p.cfg.Secret, false
	}
	return nt.FullTokenID, nt.Value, true
}

// uploadVZTmpl uploads data as a vztmpl (container template) to storage on
// node and returns its volume ID.
func (p *Provider) uploadVZTmpl(ctx context.Context, node, storage, filename string, data []byte) (string, error) {
	if _, err := p.storageRef(node, storage); err != nil {
		return "", err
	}
	if filename == "" || strings.ContainsAny(filename, "/\\\r\n\"") {
		return "", fmt.Errorf("proxmox: invalid template filename %q", filename)
	}
	var upid gp.UPID
	err := p.client.UploadReader(fmt.Sprintf("/nodes/%s/storage/%s/upload", node, storage),
		map[string]string{"content": "vztmpl"}, filename, &ctxReader{ctx: ctx, r: strings.NewReader(string(data))}, int64(len(data)), &upid)
	if err != nil {
		if ctxErr := ctx.Err(); ctxErr != nil {
			return "", fmt.Errorf("proxmox: template upload aborted: %w", ctxErr)
		}
		return "", err
	}
	if s := string(upid); s != "" {
		if err := p.wait(ctx, s); err != nil {
			return "", err
		}
	}
	return storage + ":vztmpl/" + filename, nil
}

// sidecarSpec is the input to createSidecarContainer.
type sidecarSpec struct {
	node          string
	vmid          int
	clusterName   string
	ostemplate    string
	rootfsStorage string
	bridge        string
	net           sidecarNet
	env           map[string]string
}

// createSidecarContainer creates and starts the unprivileged Alpine LXC. The
// container boots straight into the baked sweep (see buildSidecarTemplate), so
// start=1 is all that is needed to set it running.
func (p *Provider) createSidecarContainer(ctx context.Context, s sidecarSpec) error {
	mem := p.cfg.Sidecar.MemoryMiB
	if mem <= 0 {
		mem = DefaultSidecarMemoryMiB
	}
	body := map[string]string{
		"vmid":         strconv.Itoa(s.vmid),
		"ostemplate":   s.ostemplate,
		"hostname":     fmt.Sprintf("klotho-del-%s", sanitizeName(s.clusterName)),
		"cores":        "1",
		"memory":       strconv.Itoa(mem),
		"swap":         "0",
		"rootfs":       fmt.Sprintf("%s:%d", s.rootfsStorage, DefaultSidecarRootfsGiB),
		"net0":         s.net.net0(s.bridge),
		"unprivileged": "1",
		"onboot":       "0",
		"start":        "1",
		"tags":         strings.Join([]string{Tag(s.clusterName), SidecarTag}, ";"),
		"description":  fmt.Sprintf("klotho deletion sidecar for cluster %s; destroys itself when done", s.clusterName),
	}
	// Set the container's resolver too, so name resolution works even though the
	// sweep reaches the API by IP; the baked init writes /etc/resolv.conf as a
	// backstop for custom rootfs images Proxmox does not configure.
	if s.net.static && s.net.dns != "" {
		body["nameserver"] = s.net.dns
	}
	var upid gp.UPID
	if err := p.client.Post(ctx, fmt.Sprintf("/nodes/%s/lxc", s.node), body, &upid); err != nil {
		return err
	}
	if sv := string(upid); sv != "" {
		return p.wait(ctx, sv)
	}
	return nil
}

// shortRand returns 4 random hex bytes for disambiguating generated names.
func shortRand() (string, error) {
	var b [4]byte
	if _, err := rand.Read(b[:]); err != nil {
		return "", err
	}
	return hex.EncodeToString(b[:]), nil
}

// sanitizeName reduces a cluster name to the characters Proxmox accepts in a
// token ID and hostname: letters, digits and hyphens, lower-cased.
func sanitizeName(name string) string {
	var b strings.Builder
	for _, r := range strings.ToLower(name) {
		switch {
		case r >= 'a' && r <= 'z', r >= '0' && r <= '9':
			b.WriteRune(r)
		default:
			b.WriteByte('-')
		}
	}
	out := strings.Trim(b.String(), "-")
	if out == "" {
		return "cluster"
	}
	return out
}

func firstNonEmpty(vals ...string) string {
	for _, v := range vals {
		if v != "" {
			return v
		}
	}
	return ""
}

// httpClient is the HTTP client used to fetch the Alpine rootfs; a field so a
// test can stub it. nil means http.DefaultClient.
var sidecarHTTPGet = func(ctx context.Context, url string) (io.ReadCloser, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return nil, err
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return nil, err
	}
	if resp.StatusCode != http.StatusOK {
		_ = resp.Body.Close()
		return nil, fmt.Errorf("fetching %s: unexpected status %s", url, resp.Status)
	}
	return resp.Body, nil
}
