package proxmox

import (
	"bytes"
	"context"
	"errors"
	"io"
	"reflect"
	"regexp"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/kdomanski/iso9660"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"sigs.k8s.io/yaml"

	"github.com/klotho-k8s/klotho/pkg/cluster"
	"github.com/klotho-k8s/klotho/pkg/generate"
	"github.com/klotho-k8s/klotho/provider/infrastructure"
)

const templateUUID = "6c2c6f4e-1111-4222-8333-944455556666"

func testSpec(vars map[string]string) *cluster.Spec {
	v := map[string]string{
		VarEndpointIP: "192.0.2.50",
		VarTemplateID: "9000",
	}
	for k, val := range vars {
		if val == "" {
			delete(v, k)
		} else {
			v[k] = val
		}
	}
	return &cluster.Spec{Name: "c1", Namespace: "default", Provider: ProviderName, Variables: v}
}

func TestNameAndConventions(t *testing.T) {
	p := newTestProvider(t, newFake(), Config{})
	if p.Name() != "proxmox" {
		t.Errorf("Name = %q", p.Name())
	}
	if Tag("Dev") != "klotho-dev" || VMName("dev") != "dev-bootstrap" || ISOName("dev") != "dev-bootstrap-cidata.iso" {
		t.Errorf("conventions: %s %s %s", Tag("Dev"), VMName("dev"), ISOName("dev"))
	}
}

func TestResolvePrecedence(t *testing.T) {
	no := false
	vars := map[string]string{
		VarAllowedNodes: "[pve2, pve1]",
		VarSourceNode:   "pve1",
		VarTemplateID:   "9000",
		VarBridge:       "vmbr1",
		VarNumCores:     "4",
		VarNumSockets:   "2",
		VarMemoryMiB:    "8192",
	}

	// Spec variables over defaults.
	r, err := newTestProvider(t, newFake(), Config{}).Resolve(testSpec(vars))
	if err != nil {
		t.Fatal(err)
	}
	want := Resolved{
		Cluster: "c1", Node: "pve2", TemplateID: 9000, Bridge: "vmbr1", BridgeSet: true,
		Cores: 4, Sockets: 2, MemoryMiB: 8192, FullClone: true, AgentTimeout: DefaultAgentTimeout,
		Tag: "klotho-c1", VMName: "c1-bootstrap", ISOName: "c1-bootstrap-cidata.iso",
	}
	if !reflect.DeepEqual(r, want) {
		t.Errorf("vars:\n got %+v\nwant %+v", r, want)
	}

	// Config fields over spec variables.
	r, err = newTestProvider(t, newFake(), Config{
		Node: "pve9", TemplateID: 9100, Bridge: "vmbr7", Cores: 8, Sockets: 1, MemoryMiB: 4096,
		FullClone: &no, Pool: "lab", ISOStorage: "nfs", AgentTimeout: time.Minute, VMIDRange: [2]int{500, 599},
	}).Resolve(testSpec(vars))
	if err != nil {
		t.Fatal(err)
	}
	want = Resolved{
		Cluster: "c1", Node: "pve9", TemplateID: 9100, Bridge: "vmbr7", BridgeSet: true,
		Cores: 8, Sockets: 1, MemoryMiB: 4096, FullClone: false, Pool: "lab", ISOStorage: "nfs",
		AgentTimeout: time.Minute, VMIDRange: [2]int{500, 599},
		Tag: "klotho-c1", VMName: "c1-bootstrap", ISOName: "c1-bootstrap-cidata.iso",
	}
	if !reflect.DeepEqual(r, want) {
		t.Errorf("config:\n got %+v\nwant %+v", r, want)
	}

	// Defaults: no node (chosen live), default bridge not applied, template sizing kept.
	r, err = newTestProvider(t, newFake(), Config{}).Resolve(testSpec(nil))
	if err != nil {
		t.Fatal(err)
	}
	if r.Node != "" || r.Bridge != DefaultBridge || r.BridgeSet || r.Cores != 0 || r.MemoryMiB != 0 || !r.FullClone {
		t.Errorf("defaults: %+v", r)
	}

	// PROXMOX_SOURCENODE when ALLOWED_NODES is empty; plain comma lists work.
	r, _ = newTestProvider(t, newFake(), Config{}).Resolve(testSpec(map[string]string{VarSourceNode: "pve1", VarAllowedNodes: "[]"}))
	if r.Node != "pve1" {
		t.Errorf("source node: %q", r.Node)
	}
	r, _ = newTestProvider(t, newFake(), Config{}).Resolve(testSpec(map[string]string{VarAllowedNodes: "pve3,pve1"}))
	if r.Node != "pve3" {
		t.Errorf("comma list: %q", r.Node)
	}

	// Template tag beats TEMPLATE_VMID; a template ID in Config beats both.
	r, _ = newTestProvider(t, newFake(), Config{TemplateTag: "k8s"}).Resolve(testSpec(nil))
	if r.TemplateID != 0 || r.TemplateTag != "k8s" {
		t.Errorf("tag: %+v", r)
	}
	r, _ = newTestProvider(t, newFake(), Config{TemplateTag: "k8s", TemplateID: 7}).Resolve(testSpec(nil))
	if r.TemplateID != 7 || r.TemplateTag != "" {
		t.Errorf("id over tag: %+v", r)
	}

	for name, tc := range map[string]struct {
		cfg  Config
		vars map[string]string
		want string
	}{
		"no template":   {vars: map[string]string{VarTemplateID: ""}, want: "no template"},
		"bad int":       {vars: map[string]string{VarNumCores: "four"}, want: "NUM_CORES"},
		"bad range":     {cfg: Config{VMIDRange: [2]int{200, 100}}, want: "VMID range"},
		"bad pool":      {cfg: Config{Pool: "a/b"}, want: "pool"},
		"bad allowlist": {vars: map[string]string{VarAllowedNodes: "[pve1"}, want: VarAllowedNodes},
	} {
		if _, err := newTestProvider(t, newFake(), tc.cfg).Resolve(testSpec(tc.vars)); err == nil || !strings.Contains(err.Error(), tc.want) {
			t.Errorf("%s: err = %v, want containing %q", name, err, tc.want)
		}
	}
}

func TestPrepare(t *testing.T) {
	f := newFake()
	p := newTestProvider(t, f, Config{Pool: "lab"})
	got, err := p.Prepare(context.Background(), testSpec(map[string]string{VarEndpointPort: "8443", VarBridge: "vmbr1"}))
	if err != nil {
		t.Fatal(err)
	}
	if got.Endpoint != (cluster.Endpoint{Host: "192.0.2.50", Port: 8443}) || len(got.Created) != 0 {
		t.Errorf("Prepare = %+v", got)
	}
	got, err = p.Prepare(context.Background(), testSpec(nil))
	if err != nil || got.Endpoint.Port != 6443 {
		t.Fatalf("default port: %+v, %v", got, err)
	}
	if ops := f.ops(); len(ops) != 0 {
		t.Errorf("Prepare changed something: %v", ops)
	}
}

func TestPrepareErrors(t *testing.T) {
	klothoVM := vmRes{ID: "qemu/300", Type: "qemu", VMID: 300, Name: "c2-cp-x", Node: "pve2", Status: "running", Tags: []string{"klotho-c2"}}
	ipTagged := vmRes{ID: "qemu/301", Type: "qemu", VMID: 301, Name: "other", Node: "pve2", Status: "stopped", Tags: []string{"ip_net0_192.0.2.50"}}
	for name, tc := range map[string]struct {
		cfg   Config
		vars  map[string]string
		setup func(*fake)
		want  string
	}{
		"missing VIP":        {vars: map[string]string{VarEndpointIP: ""}, want: "CONTROL_PLANE_ENDPOINT_IP is required"},
		"bad VIP":            {vars: map[string]string{VarEndpointIP: "vip.example"}, want: "not an IP address"},
		"loopback VIP":       {vars: map[string]string{VarEndpointIP: "127.0.0.1"}, want: "not a usable"},
		"bad port":           {vars: map[string]string{VarEndpointPort: "70000"}, want: "not a port"},
		"not a template":     {vars: map[string]string{VarTemplateID: "100"}, want: "not a QEMU template"},
		"missing template":   {vars: map[string]string{VarTemplateID: "9999"}, want: "template 9999 does not exist"},
		"no tagged template": {cfg: Config{TemplateTag: "nope"}, want: `no template carries tag "nope"`},
		"storage without iso": {
			cfg: Config{ISOStorage: "local-lvm"}, want: `storage "local-lvm" on node "pve1" does not allow iso content`,
		},
		"missing storage": {cfg: Config{ISOStorage: "ceph"}, want: `storage "ceph" does not exist on node "pve1"`},
		"inactive storage": {
			cfg:   Config{ISOStorage: "local"},
			setup: func(f *fake) { f.storages["pve1"][0].Active = false },
			want:  "not active",
		},
		"no iso storage": {
			setup: func(f *fake) { f.storages["pve1"] = f.storages["pve1"][1:] },
			want:  "no active storage that allows iso",
		},
		"offline node":   {cfg: Config{Node: "pve3"}, want: `node "pve3" is offline`},
		"unknown node":   {cfg: Config{Node: "pve9"}, want: `node "pve9" does not exist`},
		"missing bridge": {vars: map[string]string{VarBridge: "vmbr9"}, want: `bridge "vmbr9" does not exist`},
		"missing pool":   {cfg: Config{Pool: "nope"}, want: `pool "nope" does not exist`},
		"VIP tagged": {
			setup: func(f *fake) { f.vms = append(f.vms, ipTagged) },
			want:  "already in use by VM 301",
		},
		"VIP on klotho guest": {
			setup: func(f *fake) {
				f.vms = append(f.vms, klothoVM)
				f.agent = func(vmid int) ([]agentIface, error) {
					if vmid != 300 {
						t.Errorf("agent queried for VM %d", vmid)
					}
					return agentWithIP("192.0.2.50")(vmid)
				}
			},
			want: "already in use by VM 300 (c2-cp-x)",
		},
	} {
		t.Run(name, func(t *testing.T) {
			f := newFake()
			if tc.setup != nil {
				tc.setup(f)
			}
			_, err := newTestProvider(t, f, tc.cfg).Prepare(context.Background(), testSpec(tc.vars))
			if err == nil || !strings.Contains(err.Error(), tc.want) {
				t.Fatalf("err = %v, want containing %q", err, tc.want)
			}
		})
	}
}

func TestPrepareVIPCheckIgnoresAgentErrors(t *testing.T) {
	f := newFake()
	f.vms = append(f.vms, vmRes{Type: "qemu", VMID: 300, Name: "c2-cp", Node: "pve2", Status: "running", Tags: []string{"klotho-c2"}})
	f.agent = func(int) ([]agentIface, error) { return nil, errors.New("timeout") }
	if _, err := newTestProvider(t, f, Config{}).Prepare(context.Background(), testSpec(nil)); err != nil {
		t.Fatal(err)
	}
}

func readISO(t *testing.T, img []byte) map[string]string {
	t.Helper()
	r, err := iso9660.OpenImage(bytes.NewReader(img))
	if err != nil {
		t.Fatal(err)
	}
	if label, _ := r.Label(); label != "cidata" {
		t.Errorf("label = %q", label)
	}
	root, err := r.RootDir()
	if err != nil {
		t.Fatal(err)
	}
	children, err := root.GetChildren()
	if err != nil {
		t.Fatal(err)
	}
	out := map[string]string{}
	for _, c := range children {
		data, err := io.ReadAll(c.Reader())
		if err != nil {
			t.Fatal(err)
		}
		out[c.Name()] = string(data)
	}
	return out
}

const userData = "#cloud-config\nruncmd:\n- echo hi\n"

func TestLaunch(t *testing.T) {
	f := newFake()
	f.agent = agentWithIP("192.0.2.31")
	p := newTestProvider(t, f, Config{Cores: 4, MemoryMiB: 8192})
	node, err := p.Launch(context.Background(), testSpec(map[string]string{VarBridge: "vmbr1"}), []byte(userData))
	if err != nil {
		t.Fatal(err)
	}
	want := &cluster.Node{ID: "proxmox://" + templateUUID, Name: "c1-bootstrap", Addresses: []string{"192.0.2.31"}}
	if !reflect.DeepEqual(node, want) {
		t.Errorf("node = %+v, want %+v", node, want)
	}

	wantOps := []string{
		"clone 9000->120 full=true target= pool= name=c1-bootstrap",
		"upload pve1/local:iso/c1-bootstrap-cidata.iso",
		"config 120",
		"start 120",
	}
	if ops := f.ops(); !reflect.DeepEqual(ops, wantOps) {
		t.Errorf("ops:\n got %q\nwant %q", ops, wantOps)
	}

	if len(f.sets) != 1 {
		t.Fatalf("config sets = %v", f.sets)
	}
	wantCfg := map[string]string{
		"ide0":        "local:iso/c1-bootstrap-cidata.iso,media=cdrom",
		"tags":        "k8s;ubuntu;klotho-c1",
		"agent":       "1",
		"cores":       "4",
		"memory":      "8192",
		"net0":        "virtio=BC:24:11:00:00:01,bridge=vmbr1,firewall=1",
		"description": "bootstrap node for c1, managed by klotho",
		"delete":      "ide2",
	}
	if !reflect.DeepEqual(f.sets[0], wantCfg) {
		t.Errorf("config:\n got %v\nwant %v", f.sets[0], wantCfg)
	}

	files := readISO(t, f.uploads["local:iso/c1-bootstrap-cidata.iso"])
	if files["user-data"] != userData {
		t.Errorf("user-data = %q", files["user-data"])
	}
	var md map[string]string
	if err := yaml.Unmarshal([]byte(files["meta-data"]), &md); err != nil {
		t.Fatal(err)
	}
	if md["instance-id"] != templateUUID || md["local-hostname"] != "c1-bootstrap" {
		t.Errorf("meta-data = %v", md)
	}
	if _, ok := files["network-config"]; ok {
		t.Error("unexpected network-config")
	}
}

func TestLaunchKeepsNet0WithoutBridgeAndCrossNodeClone(t *testing.T) {
	f := newFake()
	f.agent = agentWithIP("192.0.2.32")
	f.configs[9000]["agent"] = "enabled=1,fstrim_cloned_disks=1"
	p := newTestProvider(t, f, Config{Node: "pve2", Pool: "lab"})
	if _, err := p.Launch(context.Background(), testSpec(nil), []byte(userData)); err != nil {
		t.Fatal(err)
	}
	if ops := f.ops(); ops[0] != "clone 9000->120 full=true target=pve2 pool=lab name=c1-bootstrap" || ops[1] != "upload pve2/local:iso/c1-bootstrap-cidata.iso" {
		t.Errorf("ops = %q", ops)
	}
	cfg := f.sets[0]
	for _, k := range []string{"net0", "agent", "cores", "memory"} {
		if v, ok := cfg[k]; ok {
			t.Errorf("unexpected %s=%q", k, v)
		}
	}
}

func TestLaunchSetsMissingUUID(t *testing.T) {
	f := newFake()
	f.agent = agentWithIP("192.0.2.33")
	f.configs[9000]["smbios1"] = "manufacturer=QUVNRQ==,base64=1"
	node, err := newTestProvider(t, f, Config{}).Launch(context.Background(), testSpec(nil), []byte(userData))
	if err != nil {
		t.Fatal(err)
	}
	m := regexp.MustCompile(`^proxmox://([0-9a-f-]{36})$`).FindStringSubmatch(node.ID)
	if m == nil {
		t.Fatalf("ID = %q", node.ID)
	}
	if got, want := f.sets[0]["smbios1"], "manufacturer=QUVNRQ==,base64=1,uuid="+m[1]; got != want {
		t.Errorf("smbios1 = %q, want %q", got, want)
	}
	if len(node.ID) != 46 {
		t.Errorf("provider ID %q must be 46 characters", node.ID)
	}
	var md map[string]string
	_ = yaml.Unmarshal([]byte(readISO(t, f.uploads["local:iso/c1-bootstrap-cidata.iso"])["meta-data"]), &md)
	if md["instance-id"] != m[1] {
		t.Errorf("instance-id %q, want %q", md["instance-id"], m[1])
	}
}

func TestLaunchCleanupOnAgentTimeout(t *testing.T) {
	f := newFake()
	p := newTestProvider(t, f, Config{AgentTimeout: 100 * time.Millisecond})
	_, err := p.Launch(context.Background(), testSpec(nil), []byte(userData))
	if err == nil || !strings.Contains(err.Error(), "no IPv4 address from the guest agent") || !errors.Is(err, errAgentNotRunning) {
		t.Fatalf("err = %v", err)
	}
	if f.find(120) != nil {
		t.Error("VM 120 was not removed")
	}
	if len(f.uploads) != 0 || len(f.volumes["pve1/local"]) != 0 {
		t.Errorf("ISO was not removed: %v", f.volumes)
	}
	wantTail := []string{"stop 120", "delete-vm 120", "delete-volume pve1/local:iso/c1-bootstrap-cidata.iso"}
	ops := f.ops()
	if !reflect.DeepEqual(ops[len(ops)-3:], wantTail) {
		t.Errorf("ops = %q", ops)
	}
}

func TestLaunchCleanupFailureIsReported(t *testing.T) {
	f := newFake()
	f.fail["delete-vm 120"] = errors.New("locked")
	_, err := newTestProvider(t, f, Config{AgentTimeout: 50 * time.Millisecond}).Launch(context.Background(), testSpec(nil), []byte(userData))
	if err == nil || !strings.Contains(err.Error(), "no IPv4 address") || !strings.Contains(err.Error(), "cleanup: VM 120") {
		t.Fatalf("err = %v", err)
	}
}

func TestLaunchAgentNotConfiguredFailsFast(t *testing.T) {
	f := newFake()
	f.agent = func(int) ([]agentIface, error) { return nil, errAgentNotConfigured }
	_, err := newTestProvider(t, f, Config{AgentTimeout: time.Hour}).Launch(context.Background(), testSpec(nil), []byte(userData))
	if !errors.Is(err, errAgentNotConfigured) {
		t.Fatalf("err = %v", err)
	}
	if f.find(120) != nil {
		t.Error("VM 120 was not removed")
	}
}

func TestLaunchRefusesExistingBootstrapVM(t *testing.T) {
	f := newFake()
	f.vms = append(f.vms, vmRes{Type: "qemu", VMID: 150, Name: "c1-bootstrap", Node: "pve1", Tags: []string{"klotho-c1"}})
	_, err := newTestProvider(t, f, Config{}).Launch(context.Background(), testSpec(nil), []byte(userData))
	if err == nil || !strings.Contains(err.Error(), "already exists as VM 150") {
		t.Fatalf("err = %v", err)
	}
	if len(f.ops()) != 0 {
		t.Errorf("ops = %v", f.ops())
	}
}

func TestLaunchVMIDRange(t *testing.T) {
	f := newFake()
	f.agent = agentWithIP("192.0.2.34")
	f.vms = append(f.vms, vmRes{Type: "qemu", VMID: 500, Name: "x", Node: "pve1"})
	p := newTestProvider(t, f, Config{VMIDRange: [2]int{500, 502}})
	if _, err := p.Launch(context.Background(), testSpec(nil), []byte(userData)); err != nil {
		t.Fatal(err)
	}
	if v := f.find(501); v == nil || v.Name != "c1-bootstrap" {
		t.Errorf("VM 501 = %+v", v)
	}
}

func TestLaunchReplacesStaleISO(t *testing.T) {
	f := newFake()
	f.agent = agentWithIP("192.0.2.35")
	f.volumes["pve1/local"] = []volRes{{VolID: "local:iso/c1-bootstrap-cidata.iso"}}
	if _, err := newTestProvider(t, f, Config{}).Launch(context.Background(), testSpec(nil), []byte(userData)); err != nil {
		t.Fatal(err)
	}
	if ops := f.ops(); ops[1] != "delete-volume pve1/local:iso/c1-bootstrap-cidata.iso" || ops[2] != "upload pve1/local:iso/c1-bootstrap-cidata.iso" {
		t.Errorf("ops = %q", ops)
	}
}

const bundleYAML = `apiVersion: cluster.x-k8s.io/v1beta2
kind: Cluster
metadata:
  name: c1
  namespace: default
spec:
  infrastructureRef:
    apiGroup: infrastructure.cluster.x-k8s.io
    kind: ProxmoxCluster
    name: c1
---
apiVersion: infrastructure.cluster.x-k8s.io/v1alpha2
kind: ProxmoxCluster
metadata:
  name: c1
  namespace: default
spec:
  controlPlaneEndpoint:
    host: 192.0.2.50
    port: 6443
---
apiVersion: controlplane.cluster.x-k8s.io/v1beta2
kind: KubeadmControlPlane
metadata:
  name: c1-control-plane
  namespace: default
spec:
  machineTemplate:
    spec:
      infrastructureRef:
        apiGroup: infrastructure.cluster.x-k8s.io
        kind: ProxmoxMachineTemplate
        name: c1-control-plane
---
apiVersion: infrastructure.cluster.x-k8s.io/v1alpha2
kind: ProxmoxMachineTemplate
metadata:
  name: c1-worker
  namespace: default
spec:
  template:
    spec:
      sourceNode: pve1
      templateID: 9000
      numCores: 2
---
apiVersion: infrastructure.cluster.x-k8s.io/v1alpha2
kind: ProxmoxMachineTemplate
metadata:
  name: c1-control-plane
  namespace: default
spec:
  template:
    metadata:
      labels:
        team: infra
    spec:
      sourceNode: pve1
      templateID: 9000
      format: qcow2
      full: true
      numSockets: 2
      numCores: 4
      memoryMiB: 16384
      disks:
        bootVolume:
          disk: scsi0
          sizeGb: 100
      network:
        networkDevices:
        - bridge: vmbr0
          model: virtio
          name: net0
      tags:
      - klotho-c1
`

func parseBundle(t *testing.T, s string) []*unstructured.Unstructured {
	t.Helper()
	b, err := generate.ParseBundle([]byte(s))
	if err != nil {
		t.Fatal(err)
	}
	return b.Objects
}

func adoptFake() *fake {
	f := newFake()
	f.vms = append(f.vms, vmRes{ID: "qemu/120", Type: "qemu", VMID: 120, Name: "c1-bootstrap", Node: "pve2", Status: "running", Tags: []string{"k8s", "klotho-c1"}})
	f.configs[120] = map[string]any{"name": "c1-bootstrap", "smbios1": "uuid=" + templateUUID}
	return f
}

func TestAdopt(t *testing.T) {
	f := adoptFake()
	state := &cluster.State{Spec: *testSpec(nil), Bootstrap: &cluster.Node{ID: "proxmox://" + templateUUID, Name: "c1-bootstrap"}}
	out, err := newTestProvider(t, f, Config{}).Adopt(context.Background(), state, parseBundle(t, bundleYAML))
	if err != nil {
		t.Fatal(err)
	}
	objs := parseBundle(t, string(out))
	if len(objs) != 1 {
		t.Fatalf("got %d objects:\n%s", len(objs), out)
	}
	m := objs[0]
	if m.GetAPIVersion() != "infrastructure.cluster.x-k8s.io/v1alpha2" || m.GetKind() != "ProxmoxMachine" ||
		m.GetName() != "c1-bootstrap" || m.GetNamespace() != "default" {
		t.Errorf("object = %s %s %s/%s", m.GetAPIVersion(), m.GetKind(), m.GetNamespace(), m.GetName())
	}
	wantLabels := map[string]string{
		"cluster.x-k8s.io/cluster-name":       "c1",
		"cluster.x-k8s.io/control-plane":      "",
		"cluster.x-k8s.io/control-plane-name": "c1-control-plane",
		"team":                                "infra",
	}
	if !reflect.DeepEqual(m.GetLabels(), wantLabels) {
		t.Errorf("labels = %v", m.GetLabels())
	}
	wantAnn := map[string]string{
		"cluster.x-k8s.io/cloned-from-name":      "c1-control-plane",
		"cluster.x-k8s.io/cloned-from-groupkind": "ProxmoxMachineTemplate.infrastructure.cluster.x-k8s.io",
	}
	if !reflect.DeepEqual(m.GetAnnotations(), wantAnn) {
		t.Errorf("annotations = %v", m.GetAnnotations())
	}

	spec, _, _ := unstructured.NestedMap(m.Object, "spec")
	want := map[string]any{
		"virtualMachineID": int64(120),
		"providerID":       "proxmox://" + templateUUID,
		"sourceNode":       "pve2",
		"templateID":       int64(9000),
		"format":           "qcow2",
		"full":             true,
		"numSockets":       int64(2),
		"numCores":         int64(4),
		"memoryMiB":        int64(16384),
		"disks":            map[string]any{"bootVolume": map[string]any{"disk": "scsi0", "sizeGb": int64(100)}},
		"network": map[string]any{"networkDevices": []any{
			map[string]any{"bridge": "vmbr0", "model": "virtio", "name": "net0"},
		}},
		"tags": []any{"klotho-c1"},
	}
	if !reflect.DeepEqual(spec, want) {
		t.Errorf("spec:\n got %v\nwant %v", spec, want)
	}
	if len(f.ops()) != 0 {
		t.Errorf("Adopt changed something: %v", f.ops())
	}
}

func TestAdoptV1beta1AndSingleTemplate(t *testing.T) {
	v1beta1 := strings.Replace(bundleYAML, `  machineTemplate:
    spec:
      infrastructureRef:
        apiGroup: infrastructure.cluster.x-k8s.io
        kind: ProxmoxMachineTemplate
        name: c1-control-plane`, `  machineTemplate:
    infrastructureRef:
      apiVersion: infrastructure.cluster.x-k8s.io/v1alpha2
      kind: ProxmoxMachineTemplate
      name: c1-control-plane`, 1)
	state := &cluster.State{Spec: *testSpec(nil)}
	out, err := newTestProvider(t, adoptFake(), Config{}).Adopt(context.Background(), state, parseBundle(t, v1beta1))
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(out), "cloned-from-name: c1-control-plane") {
		t.Errorf("v1beta1:\n%s", out)
	}

	// No control plane object and a single template: that template is used.
	var single []*unstructured.Unstructured
	for _, o := range parseBundle(t, bundleYAML) {
		if o.GetKind() != "KubeadmControlPlane" && o.GetName() != "c1-worker" {
			single = append(single, o)
		}
	}
	out, err = newTestProvider(t, adoptFake(), Config{}).Adopt(context.Background(), state, single)
	if err != nil || !strings.Contains(string(out), "cloned-from-name: c1-control-plane") {
		t.Errorf("single: %v\n%s", err, out)
	}
}

func TestAdoptErrors(t *testing.T) {
	ctx := context.Background()
	bundle := parseBundle(t, bundleYAML)

	state := &cluster.State{Spec: *testSpec(nil), Bootstrap: &cluster.Node{ID: "proxmox://00000000-0000-4000-8000-000000000000"}}
	if _, err := newTestProvider(t, adoptFake(), Config{}).Adopt(ctx, state, bundle); err == nil || !strings.Contains(err.Error(), "state records") {
		t.Errorf("uuid mismatch: %v", err)
	}

	state = &cluster.State{Spec: *testSpec(nil)}
	if _, err := newTestProvider(t, newFake(), Config{}).Adopt(ctx, state, bundle); err == nil || !strings.Contains(err.Error(), "no VM named c1-bootstrap") {
		t.Errorf("no VM: %v", err)
	}

	var noKCP []*unstructured.Unstructured
	for _, o := range bundle {
		if o.GetKind() != "KubeadmControlPlane" {
			noKCP = append(noKCP, o)
		}
	}
	if _, err := newTestProvider(t, adoptFake(), Config{}).Adopt(ctx, state, noKCP); err == nil || !strings.Contains(err.Error(), "cannot tell which MachineTemplate") {
		t.Errorf("ambiguous: %v", err)
	}
}

func planFake() *fake {
	f := newFake()
	f.vms = append(f.vms,
		vmRes{Type: "qemu", VMID: 121, Name: "c1-control-plane-abcde", Node: "pve2", Status: "running", Tags: []string{"klotho-c1"}},
		vmRes{Type: "qemu", VMID: 120, Name: "c1-bootstrap", Node: "pve1", Status: "running", Tags: []string{"KLOTHO-C1", "k8s"}},
		vmRes{Type: "qemu", VMID: 130, Name: "c2-bootstrap", Node: "pve1", Status: "running", Tags: []string{"klotho-c2"}},
		vmRes{Type: "qemu", VMID: 9100, Name: "tmpl", Node: "pve1", Template: true, Tags: []string{"klotho-c1"}},
	)
	f.volumes["pve1/local"] = []volRes{{VolID: "local:iso/c1-bootstrap-cidata.iso"}, {VolID: "local:iso/c2-bootstrap-cidata.iso"}}
	f.volumes["pve2/nfs"] = []volRes{{VolID: "nfs:iso/c1-bootstrap-cidata.iso"}}
	return f
}

func TestPlan(t *testing.T) {
	f := planFake()
	state := &cluster.State{Spec: *testSpec(nil), Resources: []cluster.Resource{
		{Kind: "vm", ID: "pve1/120", Name: "c1-bootstrap"},
		{Kind: "pool", ID: "klotho-c1"},
	}}
	plan, err := newTestProvider(t, f, Config{}).Plan(context.Background(), state)
	if err != nil {
		t.Fatal(err)
	}
	want := []cluster.Resource{
		{Kind: "vm", ID: "pve1/120", Name: "c1-bootstrap"},
		{Kind: "vm", ID: "pve2/121", Name: "c1-control-plane-abcde"},
		{Kind: "iso", ID: "pve1/local:iso/c1-bootstrap-cidata.iso", Name: "c1-bootstrap-cidata.iso"},
		{Kind: "iso", ID: "pve2/nfs:iso/c1-bootstrap-cidata.iso", Name: "c1-bootstrap-cidata.iso"},
	}
	if !reflect.DeepEqual(plan.Resources, want) {
		t.Errorf("plan:\n got %+v\nwant %+v", plan.Resources, want)
	}
	if len(f.ops()) != 0 {
		t.Errorf("Plan changed something: %v", f.ops())
	}
}

func TestTeardown(t *testing.T) {
	f := planFake()
	f.vms = append(f.vms, vmRes{Type: "qemu", VMID: 140, Name: "stopped", Node: "pve1", Status: "stopped", Tags: []string{"klotho-c1"}})
	f.fail["delete-vm 121"] = errors.New("vm is locked")
	plan := &infrastructure.Plan{Resources: []cluster.Resource{
		{Kind: "iso", ID: "pve1/local:iso/c1-bootstrap-cidata.iso"},
		{Kind: "vm", ID: "pve1/120"},
		{Kind: "vm", ID: "pve2/121"},
		{Kind: "vm", ID: "pve1/130"},                                // another cluster's: refused
		{Kind: "vm", ID: "pve1/140"},                                // stopped: no stop call
		{Kind: "vm", ID: "pve1/999"},                                // already gone
		{Kind: "iso", ID: "pve1/local:iso/c2-bootstrap-cidata.iso"}, // refused
		{Kind: "iso", ID: "pve2/nfs:iso/c1-bootstrap-cidata.iso"},
		{Kind: "pool", ID: "klotho-c1"}, // not this provider's
	}}
	err := newTestProvider(t, f, Config{}).Teardown(context.Background(), &cluster.State{Spec: *testSpec(nil)}, plan)
	if err == nil {
		t.Fatal("expected an aggregate error")
	}
	for _, want := range []string{"VM 121 (c1-control-plane-abcde) on pve2", "vm is locked", "refusing to delete VM 130", "refusing to delete local:iso/c2-bootstrap-cidata.iso"} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("error %q lacks %q", err, want)
		}
	}
	wantOps := []string{
		"stop 120", "delete-vm 120",
		"stop 121", "delete-vm 121",
		"delete-vm 140",
		"delete-volume pve1/local:iso/c1-bootstrap-cidata.iso",
		"delete-volume pve2/nfs:iso/c1-bootstrap-cidata.iso",
	}
	if ops := f.ops(); !reflect.DeepEqual(ops, wantOps) {
		t.Errorf("ops:\n got %q\nwant %q", ops, wantOps)
	}
	if f.find(130) == nil {
		t.Error("another cluster's VM was deleted")
	}
	if !slices.ContainsFunc(f.volumes["pve1/local"], func(v volRes) bool { return v.VolID == "local:iso/c2-bootstrap-cidata.iso" }) {
		t.Error("another cluster's ISO was deleted")
	}
}

func TestTeardownAfterPlanRemovesEverything(t *testing.T) {
	f := planFake()
	p := newTestProvider(t, f, Config{})
	state := &cluster.State{Spec: *testSpec(nil)}
	plan, err := p.Plan(context.Background(), state)
	if err != nil {
		t.Fatal(err)
	}
	if err := p.Teardown(context.Background(), state, plan); err != nil {
		t.Fatal(err)
	}
	again, err := p.Plan(context.Background(), state)
	if err != nil || len(again.Resources) != 0 {
		t.Fatalf("after teardown: %+v, %v", again, err)
	}
}

func TestLaunchGrowsBootDisk(t *testing.T) {
	f := newFake()
	f.agent = agentWithIP("192.0.2.31")
	f.configs[9000]["scsi0"] = "local:9000/base-9000-disk-0.qcow2,size=3584M"
	p := newTestProvider(t, f, Config{})
	if _, err := p.Launch(context.Background(), testSpec(map[string]string{VarBootVolumeSize: "20"}), []byte(userData)); err != nil {
		t.Fatal(err)
	}
	ops := f.ops()
	want := []string{"config 120", "resize 120 scsi0 20G", "start 120"}
	if len(ops) < 4 || !reflect.DeepEqual(ops[len(ops)-3:], want) {
		t.Fatalf("ops = %q, want tail %q", ops, want)
	}
	if got := f.configs[120]["scsi0"]; got != "local:9000/base-9000-disk-0.qcow2,size=20G" {
		t.Fatalf("scsi0 after resize = %v", got)
	}

	// Already large enough: no resize. Explicit device honoured.
	f = newFake()
	f.agent = agentWithIP("192.0.2.31")
	f.configs[9000]["virtio0"] = "local:9000/base-9000-disk-0.qcow2,size=40G"
	p = newTestProvider(t, f, Config{})
	if _, err := p.Launch(context.Background(), testSpec(map[string]string{VarBootVolumeSize: "20", VarBootVolumeDevice: "virtio0"}), []byte(userData)); err != nil {
		t.Fatal(err)
	}
	for _, op := range f.ops() {
		if strings.HasPrefix(op, "resize") {
			t.Fatalf("unexpected %q", op)
		}
	}

	// Missing device is an error that names the variable.
	f = newFake()
	f.agent = agentWithIP("192.0.2.31")
	p = newTestProvider(t, f, Config{})
	if _, err := p.Launch(context.Background(), testSpec(map[string]string{VarBootVolumeSize: "20", VarBootVolumeDevice: "sata3"}), []byte(userData)); err == nil || !strings.Contains(err.Error(), VarBootVolumeDevice) {
		t.Fatalf("err = %v", err)
	}
}

func TestDiskSizeBytes(t *testing.T) {
	cases := map[string]int64{
		"local:9000/x.qcow2,size=3584M": 3584 << 20,
		"local:9000/x.qcow2,size=20G":   20 << 30,
		"size=1T,discard=on":            1 << 40,
		"local:9000/x.qcow2":            -1,
		"size=abc":                      -1,
	}
	for in, want := range cases {
		got, ok := diskSizeBytes(in)
		if want < 0 {
			if ok {
				t.Errorf("%q: parsed %d, want failure", in, got)
			}
			continue
		}
		if !ok || got != want {
			t.Errorf("%q: got %d ok=%v, want %d", in, got, ok, want)
		}
	}
}

func TestAdoptResolvesTemplateSelector(t *testing.T) {
	f := adoptFake()
	// The bootstrap VM lives on pve2 in this fake, so the tags must resolve there.
	f.vms = append(f.vms, vmRes{ID: "qemu/9002", Type: "qemu", VMID: 9002, Name: "ubuntu-2404", Node: "pve2", Status: "stopped", Template: true, Tags: []string{"k8s", "ubuntu"}})
	selectorBundle := strings.ReplaceAll(bundleYAML,
		"      sourceNode: pve1\n      templateID: 9000\n",
		"      templateSelector:\n        matchTags: [k8s, ubuntu]\n")
	if !strings.Contains(selectorBundle, "templateSelector") {
		t.Fatal("test bundle did not get a selector")
	}
	state := &cluster.State{Spec: *testSpec(nil), Bootstrap: &cluster.Node{ID: "proxmox://" + templateUUID, Name: "c1-bootstrap"}}
	out, err := newTestProvider(t, f, Config{TemplateTags: []string{"k8s", "ubuntu"}}).Adopt(context.Background(), state, parseBundle(t, selectorBundle))
	if err != nil {
		t.Fatal(err)
	}
	m := parseBundle(t, string(out))[0]
	id, _, _ := unstructured.NestedInt64(m.Object, "spec", "templateID")
	node, _, _ := unstructured.NestedString(m.Object, "spec", "sourceNode")
	_, hasSel, _ := unstructured.NestedMap(m.Object, "spec", "templateSelector")
	if id != 9002 || node != "pve2" || hasSel {
		t.Fatalf("spec = %v", m.Object["spec"])
	}

	// Without tags configured and no fixed template the selector cannot be resolved.
	if _, err := newTestProvider(t, f, Config{}).Adopt(context.Background(), state, parseBundle(t, selectorBundle)); err == nil || !strings.Contains(err.Error(), "template tags") {
		t.Fatalf("err = %v", err)
	}
}
