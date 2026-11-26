package proxmox

import (
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"maps"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	gp "github.com/luthermonson/go-proxmox"
)

const fakeUPID = "UPID:pve1:00000001:00000001:00000001:task:1:root@pam:"

// The fake's in-memory model. Each type mirrors what the emulated endpoint
// reports for the fields the provider reads.
type (
	vmRes struct {
		ID, Type                 string
		VMID                     int
		Name, Node, Status, Pool string
		Tags                     []string
		Template                 bool
	}
	nodeRes struct {
		Node, Status string
		MaxCPU       int
		MaxMem, Mem  uint64
	}
	storageRes struct {
		Storage, Type           string
		Content                 []string
		Active, Enabled, Shared bool
	}
	volRes struct {
		VolID string
		Size  uint64
	}
	bridgeRes struct {
		Iface             string
		Active, VLANAware bool
	}
	agentIface struct {
		Name        string
		IPAddresses []agentIP
	}
	agentIP struct {
		Address, Type string
		Prefix        int
	}
)

// fake emulates a Proxmox VE cluster behind the real API client: an httptest
// server answers the endpoints the provider uses from this in-memory model.
// VMs live in vms (with Status and Tags kept current) and their configs in
// configs; every mutating call is logged.
type fake struct {
	mu       sync.Mutex
	nodes    []nodeRes
	vms      []vmRes
	configs  map[int]map[string]any
	storages map[string][]storageRes
	volumes  map[string][]volRes // "<node>/<storage>"
	bridges  map[string][]bridgeRes
	pools    map[string]bool
	nextID   int
	uploads  map[string][]byte // volid
	sets     []map[string]string
	log      []string

	// agent answers the guest agent's network-get-interfaces; nil means the
	// agent is not running.
	agent func(vmid int) ([]agentIface, error)
	// fail makes the named operation ("delete-vm 120", "stop 120",
	// "delete-volume <volid>", "clone 120") answer with the error as a 500.
	fail map[string]error

	// lxcCreates records the body of each POST /nodes/{node}/lxc, and
	// tokenCreateErr, when set, makes token creation fail (to exercise the
	// sidecar's fallback to the cluster token).
	lxcCreates     []map[string]string
	tokenCreateErr error

	srv    *httptest.Server
	client *gp.Client
}

func newFake() *fake {
	return &fake{
		nodes: []nodeRes{
			{Node: "pve1", Status: "online", MaxCPU: 16, MaxMem: 64 << 30, Mem: 8 << 30},
			{Node: "pve2", Status: "online", MaxCPU: 8, MaxMem: 32 << 30},
			{Node: "pve3", Status: "offline"},
		},
		vms: []vmRes{
			{ID: "qemu/9000", Type: "qemu", VMID: 9000, Name: "ubuntu-2404", Node: "pve1", Status: "stopped", Template: true, Tags: []string{"k8s", "ubuntu"}},
			{ID: "qemu/100", Type: "qemu", VMID: 100, Name: "plain-vm", Node: "pve1", Status: "running"},
		},
		configs: map[int]map[string]any{
			9000: {"name": "ubuntu-2404", "template": float64(1), "tags": "k8s;ubuntu", "net0": "virtio=BC:24:11:00:00:01,bridge=vmbr0,firewall=1", "smbios1": "uuid=6c2c6f4e-1111-4222-8333-944455556666", "ide2": "local-lvm:vm-9000-cloudinit,media=cdrom"},
			100:  {"name": "plain-vm"},
		},
		storages: map[string][]storageRes{
			"pve1": {
				{Storage: "local", Type: "dir", Content: []string{"iso", "vztmpl", "backup"}, Active: true, Enabled: true},
				{Storage: "local-lvm", Type: "lvmthin", Content: []string{"images", "rootdir"}, Active: true, Enabled: true},
			},
			"pve2": {
				{Storage: "local", Type: "dir", Content: []string{"iso"}, Active: true, Enabled: true},
				{Storage: "nfs", Type: "nfs", Content: []string{"iso", "images"}, Active: true, Enabled: true, Shared: true},
			},
		},
		volumes: map[string][]volRes{},
		bridges: map[string][]bridgeRes{
			"pve1": {{Iface: "vmbr0", Active: true}, {Iface: "vmbr1", Active: true, VLANAware: true}},
			"pve2": {{Iface: "vmbr0", Active: true}},
		},
		pools:   map[string]bool{"lab": true},
		nextID:  120,
		uploads: map[string][]byte{},
		fail:    map[string]error{},
	}
}

// serve starts the emulation (once) and returns a client connected to it.
func (f *fake) serve(t *testing.T) *gp.Client {
	t.Helper()
	if f.client != nil {
		return f.client
	}
	f.srv = httptest.NewServer(f)
	t.Cleanup(f.srv.Close)
	client, err := NewClient(Config{URL: f.srv.URL, TokenID: testTokenID, Secret: testSecret})
	if err != nil {
		t.Fatal(err)
	}
	f.client = client
	return client
}

func newTestProvider(t *testing.T, f *fake, cfg Config) *Provider {
	t.Helper()
	p := New(f.serve(t), cfg)
	p.poll = time.Millisecond
	p.taskPoll = time.Millisecond
	p.cleanupTimeout = 5 * time.Second
	return p
}

func agentWithIP(ip string) func(int) ([]agentIface, error) {
	return func(int) ([]agentIface, error) {
		return []agentIface{{Name: "eth0", IPAddresses: []agentIP{
			{Address: "fe80::1", Type: "ipv6"}, {Address: ip, Type: "ipv4", Prefix: 24},
		}}}, nil
	}
}

// resource renders the VM the way GET /cluster/resources reports it.
func (v vmRes) resource() *gp.ClusterResource {
	r := &gp.ClusterResource{ID: v.ID, Type: v.Type, VMID: uint64(v.VMID), Name: v.Name, Node: v.Node, Status: v.Status, Pool: v.Pool, Tags: strings.Join(v.Tags, ";")}
	if v.Template {
		r.Template = 1
	}
	return r
}

func resourcesOf(vms []vmRes) []*gp.ClusterResource {
	out := make([]*gp.ClusterResource, len(vms))
	for i, v := range vms {
		out[i] = v.resource()
	}
	return out
}

func (f *fake) record(format string, args ...any) error {
	op := fmt.Sprintf(format, args...)
	f.log = append(f.log, op)
	return f.fail[op]
}

func (f *fake) find(vmid int) *vmRes {
	for i := range f.vms {
		if f.vms[i].VMID == vmid {
			return &f.vms[i]
		}
	}
	return nil
}

func (f *fake) ops() []string {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]string(nil), f.log...)
}

// request is one parsed API call. Its methods answer in Proxmox's shapes.
type request struct {
	f  *fake
	w  http.ResponseWriter
	r  *http.Request
	wr func(code int, reason, body string)
}

func (q *request) ok(v any) {
	q.w.Header().Set("Content-Type", "application/json;charset=UTF-8")
	_ = jsonEncode(q.w, map[string]any{"data": v})
}

// fail answers like Proxmox: the message in the status line's reason phrase.
func (q *request) fail(code int, reason string) {
	q.wr(code, reason, `{"data":null}`)
}

func (q *request) notFound(node string, vmid int) {
	q.fail(500, fmt.Sprintf("Configuration file 'nodes/%s/qemu-server/%d.conf' does not exist", node, vmid))
}

// vm looks a VM up on node, answering not-found when it is not there.
func (q *request) vm(node string, vmid int) *vmRes {
	v := q.f.find(vmid)
	if v == nil || v.Node != node {
		q.notFound(node, vmid)
		return nil
	}
	return v
}

func (f *fake) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	if got := r.Header.Get("Authorization"); got != testAuth {
		http.Error(w, "bad token", http.StatusUnauthorized)
		return
	}
	body, _ := io.ReadAll(r.Body)
	f.mu.Lock()
	defer f.mu.Unlock()
	q := &request{f: f, w: w, r: r, wr: func(code int, reason, body string) { writeRawConn(w, code, reason, body) }}
	path := strings.TrimPrefix(r.URL.Path, "/api2/json")
	parts := strings.Split(strings.Trim(path, "/"), "/")
	query := r.URL.Query()
	switch {
	case path == "/nodes" && r.Method == "GET":
		out := []map[string]any{}
		for _, n := range f.nodes {
			out = append(out, map[string]any{"node": n.Node, "status": n.Status, "maxcpu": n.MaxCPU, "maxmem": n.MaxMem, "mem": n.Mem})
		}
		q.ok(out)
	case path == "/cluster/nextid" && r.Method == "GET":
		for f.find(f.nextID) != nil {
			f.nextID++
		}
		q.ok(strconv.Itoa(f.nextID))
	case path == "/cluster/resources" && r.Method == "GET":
		q.clusterResources(query.Get("type"))
	case path == "/pools/" && r.Method == "GET":
		id := query.Get("poolid")
		if !f.pools[id] {
			q.fail(500, fmt.Sprintf("pool '%s' does not exist", id))
			return
		}
		q.ok([]map[string]any{{"poolid": id, "comment": ""}})
	case len(parts) == 5 && parts[0] == "access" && parts[1] == "users" && parts[3] == "token" && r.Method == "POST":
		q.createToken(parts[2], parts[4], body)
	case len(parts) >= 3 && parts[0] == "nodes":
		q.node(parts[1], parts[2:], body)
	default:
		http.Error(w, "no route: "+r.Method+" "+path, http.StatusNotImplemented)
	}
}

// createToken emulates POST /access/users/{userid}/token/{tokenid}. With
// tokenCreateErr set it fails, as a least-privilege token's attempt would.
func (q *request) createToken(userID, tokenID string, _ []byte) {
	f := q.f
	if err := f.record("create-token %s!%s", userID, tokenID); err != nil {
		q.fail(500, err.Error())
		return
	}
	if f.tokenCreateErr != nil {
		q.fail(403, f.tokenCreateErr.Error())
		return
	}
	q.ok(map[string]any{
		"full-tokenid": userID + "!" + tokenID,
		"value":        "minted-secret-0000",
		"info":         map[string]any{"privsep": "0"},
	})
}

func (q *request) clusterResources(kind string) {
	f := q.f
	switch kind {
	case resourceVM:
		out := []map[string]any{}
		for _, v := range f.vms {
			m := map[string]any{"id": v.ID, "type": v.Type, "vmid": v.VMID, "name": v.Name, "node": v.Node, "status": v.Status, "pool": v.Pool, "tags": strings.Join(v.Tags, ";")}
			if v.Template {
				m["template"] = 1
			}
			out = append(out, m)
		}
		q.ok(out)
	case resourceStorage:
		// Derived from the per-node storage listing: one resource per node
		// and storage, as Proxmox reports them.
		out := []map[string]any{}
		for node, list := range f.storages {
			for _, s := range list {
				m := map[string]any{"id": "storage/" + node + "/" + s.Storage, "type": "storage", "node": node, "storage": s.Storage}
				if s.Shared {
					m["shared"] = 1
				}
				out = append(out, m)
			}
		}
		q.ok(out)
	default:
		q.fail(400, fmt.Sprintf("Parameter verification failed. (type %q)", kind))
	}
}

func (q *request) node(node string, rest []string, body []byte) {
	f, r := q.f, q.r
	switch {
	case len(rest) == 1 && rest[0] == "storage" && r.Method == "GET":
		out := []map[string]any{}
		for _, s := range f.storages[node] {
			out = append(out, map[string]any{"storage": s.Storage, "type": s.Type, "content": strings.Join(s.Content, ","),
				"active": b2i(s.Active), "enabled": b2i(s.Enabled), "shared": b2i(s.Shared)})
		}
		q.ok(out)
	case len(rest) == 1 && rest[0] == "network" && r.Method == "GET":
		out := []map[string]any{}
		for _, b := range f.bridges[node] {
			out = append(out, map[string]any{"iface": b.Iface, "type": "bridge", "active": b2i(b.Active), "bridge_vlan_aware": b2i(b.VLANAware)})
		}
		q.ok(out)
	case len(rest) >= 3 && rest[0] == "storage":
		q.storage(node, rest[1], rest[2:], body)
	case len(rest) == 3 && rest[0] == "tasks" && rest[2] == "status" && r.Method == "GET":
		q.ok(map[string]any{"upid": rest[1], "node": nodeFromUPID(rest[1]), "status": "stopped", "exitstatus": "OK", "type": "task", "id": "1", "user": "root@pam"})
	case len(rest) == 1 && rest[0] == "lxc" && r.Method == "POST":
		args := map[string]string{}
		_ = jsonDecode(body, &args)
		if err := f.record("lxc-create %s/%s", node, args["vmid"]); err != nil {
			q.fail(500, err.Error())
			return
		}
		args["node"] = node
		f.lxcCreates = append(f.lxcCreates, args)
		q.ok(fakeUPID)
	case len(rest) >= 2 && rest[0] == "qemu":
		vmid, err := strconv.Atoi(rest[1])
		if err != nil {
			q.fail(400, "Parameter verification failed. (vmid)")
			return
		}
		q.qemu(node, vmid, rest[2:], body)
	default:
		http.Error(q.w, "no route: "+r.Method+" "+r.URL.Path, http.StatusNotImplemented)
	}
}

func (q *request) storage(node, storage string, rest []string, body []byte) {
	f, r := q.f, q.r
	key := node + "/" + storage
	switch {
	case len(rest) == 1 && rest[0] == "content" && r.Method == "GET":
		out := []map[string]any{}
		for _, v := range f.volumes[key] {
			if c := r.URL.Query().Get("content"); c != "" && !strings.HasPrefix(v.VolID, storage+":"+c+"/") {
				continue
			}
			out = append(out, map[string]any{"volid": v.VolID, "size": v.Size})
		}
		q.ok(out)
	case len(rest) >= 2 && rest[0] == "content" && r.Method == "DELETE":
		volid := strings.Join(rest[1:], "/")
		if err := f.record("delete-volume %s/%s", node, volid); err != nil {
			q.fail(500, err.Error())
			return
		}
		vols := f.volumes[key][:0]
		for _, v := range f.volumes[key] {
			if v.VolID != volid {
				vols = append(vols, v)
			}
		}
		f.volumes[key] = vols
		delete(f.uploads, volid)
		q.ok(fakeUPID)
	case len(rest) == 1 && rest[0] == "upload" && r.Method == "POST":
		filename, content, data, err := readUpload(r, body)
		if err != nil {
			q.fail(400, err.Error())
			return
		}
		if content == "" {
			content = "iso"
		}
		volid := storage + ":" + content + "/" + filename
		if err := f.record("upload %s/%s", node, volid); err != nil {
			q.fail(500, err.Error())
			return
		}
		f.uploads[volid] = data
		f.volumes[key] = append(f.volumes[key], volRes{VolID: volid, Size: uint64(len(data))})
		q.ok(fakeUPID)
	default:
		http.Error(q.w, "no route: "+r.Method+" "+r.URL.Path, http.StatusNotImplemented)
	}
}

func (q *request) qemu(node string, vmid int, rest []string, body []byte) {
	f, r := q.f, q.r
	action := strings.Join(rest, "/")
	switch {
	case action == "config" && r.Method == "GET":
		if q.vm(node, vmid) == nil {
			return
		}
		cfg := f.configs[vmid]
		if cfg == nil {
			cfg = map[string]any{}
		}
		q.ok(cfg)
	case action == "config" && r.Method == "PUT":
		v := q.vm(node, vmid)
		if v == nil {
			return
		}
		raw := map[string]any{}
		_ = jsonDecode(body, &raw)
		cfg := make(map[string]string, len(raw))
		for k, val := range raw {
			cfg[k] = fmt.Sprint(val)
		}
		f.sets = append(f.sets, cfg)
		f.log = append(f.log, fmt.Sprintf("config %d", vmid))
		if f.configs[vmid] == nil {
			f.configs[vmid] = map[string]any{}
		}
		for k, val := range cfg {
			if k == "delete" {
				for _, d := range strings.Split(val, ",") {
					delete(f.configs[vmid], d)
				}
				continue
			}
			f.configs[vmid][k] = typedOption(k, val)
			if k == "tags" {
				v.Tags = parseTags(val)
			}
		}
		q.ok(nil)
	case action == "status/current" && r.Method == "GET":
		v := q.vm(node, vmid)
		if v == nil {
			return
		}
		q.ok(map[string]any{"vmid": vmid, "name": v.Name, "status": v.Status, "tags": strings.Join(v.Tags, ";")})
	case (action == "status/start" || action == "status/stop") && r.Method == "POST":
		op, status := "start", "running"
		if action == "status/stop" {
			op, status = "stop", "stopped"
		}
		if err := f.record("%s %d", op, vmid); err != nil {
			q.fail(500, err.Error())
			return
		}
		v := q.vm(node, vmid)
		if v == nil {
			return
		}
		v.Status = status
		q.ok(fakeUPID)
	case action == "clone" && r.Method == "POST":
		q.clone(node, vmid, body)
	case action == "resize" && r.Method == "PUT":
		v := q.vm(node, vmid)
		if v == nil {
			return
		}
		args := map[string]string{}
		_ = jsonDecode(body, &args)
		if err := f.record("resize %d %s %s", vmid, args["disk"], args["size"]); err != nil {
			q.fail(500, err.Error())
			return
		}
		cur, _ := f.configs[vmid][args["disk"]].(string)
		if cur == "" {
			q.fail(500, fmt.Sprintf("proxmox: VM %d has no disk %s", vmid, args["disk"]))
			return
		}
		f.configs[vmid][args["disk"]] = strings.Split(cur, ",")[0] + ",size=" + args["size"]
		q.ok(fakeUPID)
	case action == "agent/network-get-interfaces" && r.Method == "GET":
		if f.agent == nil {
			q.fail(500, "QEMU guest agent is not running")
			return
		}
		ifaces, err := f.agent(vmid)
		switch {
		case errors.Is(err, errAgentNotConfigured):
			q.fail(500, "No QEMU guest agent configured")
			return
		case errors.Is(err, errAgentNotRunning):
			q.fail(500, "QEMU guest agent is not running")
			return
		case err != nil:
			q.fail(500, err.Error())
			return
		}
		out := []map[string]any{}
		for _, i := range ifaces {
			addrs := []map[string]any{}
			for _, a := range i.IPAddresses {
				addrs = append(addrs, map[string]any{"ip-address": a.Address, "ip-address-type": a.Type, "prefix": a.Prefix})
			}
			out = append(out, map[string]any{"name": i.Name, "ip-addresses": addrs})
		}
		q.ok(map[string]any{"result": out})
	case action == "" && r.Method == "DELETE":
		if err := f.record("delete-vm %d", vmid); err != nil {
			q.fail(500, err.Error())
			return
		}
		if r.URL.Query().Get("purge") != "1" {
			q.fail(500, "fake: delete without purge")
			return
		}
		v := q.vm(node, vmid)
		if v == nil {
			return
		}
		if v.Status == "running" {
			q.fail(500, fmt.Sprintf("fake: VM %d is running", vmid))
			return
		}
		out := f.vms[:0]
		for _, x := range f.vms {
			if x.VMID != vmid {
				out = append(out, x)
			}
		}
		f.vms = out
		delete(f.configs, vmid)
		q.ok(fakeUPID)
	default:
		http.Error(q.w, "no route: "+r.Method+" "+r.URL.Path, http.StatusNotImplemented)
	}
}

func (q *request) clone(node string, templateID int, body []byte) {
	f := q.f
	opts := struct {
		NewID  int     `json:"newid"`
		Name   string  `json:"name"`
		Full   float64 `json:"full"`
		Target string  `json:"target"`
		Pool   string  `json:"pool"`
	}{}
	_ = jsonDecode(body, &opts)
	if err := f.record("clone %d->%d full=%v target=%s pool=%s name=%s", templateID, opts.NewID, opts.Full == 1, opts.Target, opts.Pool, opts.Name); err != nil {
		q.fail(500, err.Error())
		return
	}
	if f.find(opts.NewID) != nil {
		q.fail(500, fmt.Sprintf("unable to create VM %d: config file already exists", opts.NewID))
		return
	}
	src := q.vm(node, templateID)
	if src == nil {
		return
	}
	target := node
	if opts.Target != "" {
		target = opts.Target
	}
	cfg := maps.Clone(f.configs[templateID])
	if cfg == nil {
		cfg = map[string]any{}
	}
	delete(cfg, "template")
	cfg["name"] = opts.Name
	f.configs[opts.NewID] = cfg
	f.vms = append(f.vms, vmRes{
		ID: fmt.Sprintf("qemu/%d", opts.NewID), Type: "qemu", VMID: opts.NewID, Name: opts.Name, Node: target,
		Status: "stopped", Pool: opts.Pool, Tags: append([]string(nil), src.Tags...),
	})
	q.ok(fakeUPID)
}

// typedOption stores a config value the way Proxmox reports it back: integer
// options come back as JSON numbers, everything else as strings.
func typedOption(key, val string) any {
	switch key {
	case "cores", "sockets", "memory", "balloon", "vcpus", "cpuunits":
		if n, err := strconv.Atoi(val); err == nil {
			return float64(n)
		}
	}
	return val
}

// readUpload parses the multipart upload body and returns the file name, the
// "content" form field (iso, vztmpl, ...), and the content of the file part.
func readUpload(r *http.Request, body []byte) (filename, content string, data []byte, err error) {
	ct := r.Header.Get("Content-Type")
	if !strings.HasPrefix(ct, "multipart/form-data") {
		return "", "", nil, fmt.Errorf("not multipart: %q", ct)
	}
	req, err := http.NewRequest(r.Method, r.URL.String(), strings.NewReader(string(body)))
	if err != nil {
		return "", "", nil, err
	}
	req.Header.Set("Content-Type", ct)
	mr, err := req.MultipartReader()
	if err != nil {
		return "", "", nil, err
	}
	for {
		part, perr := mr.NextPart()
		if perr == io.EOF {
			break
		}
		if perr != nil {
			return "", "", nil, perr
		}
		switch part.FormName() {
		case "content":
			b, _ := io.ReadAll(part)
			content = string(b)
		case "filename":
			data, err = io.ReadAll(part)
			filename = part.FileName()
		}
	}
	if filename == "" {
		return "", "", nil, errors.New("no file part")
	}
	return filename, content, data, err
}

func b2i(b bool) int {
	if b {
		return 1
	}
	return 0
}

func jsonEncode(w io.Writer, v any) error { return json.NewEncoder(w).Encode(v) }

func jsonDecode(b []byte, v any) error { return json.Unmarshal(b, v) }

// writeRawConn writes a response with a custom status-line reason phrase by
// hijacking the connection; see writeRaw.
func writeRawConn(w http.ResponseWriter, code int, reason, body string) {
	conn, bufrw, err := w.(http.Hijacker).Hijack()
	if err != nil {
		http.Error(w, reason, code)
		return
	}
	defer func() { _ = conn.Close() }()
	_, _ = fmt.Fprintf(bufrw, "HTTP/1.1 %d %s\r\nContent-Type: application/json;charset=UTF-8\r\nContent-Length: %d\r\nConnection: close\r\n\r\n%s",
		code, reason, len(body), body)
	_ = bufrw.Flush()
}
