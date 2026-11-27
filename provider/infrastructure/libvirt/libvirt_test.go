package libvirt

import (
	"bytes"
	"context"
	"errors"
	"strings"
	"testing"

	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"sigs.k8s.io/yaml"

	"github.com/klotho-k8s/klotho/pkg/cluster"
)

func testProvider(cfg Config) (*Provider, *fakeClient) {
	f := newFakeClient()
	cfg.Image = "base.qcow2"
	cfg.EndpointHost = "192.168.122.10"
	if cfg.URI == "" && cfg.ControllerURI == "" {
		// A network controller URI (reachable from the in-cluster controller)
		// whose host is "localhost", so provider IDs stay libvirt://localhost/*.
		cfg.ControllerURI = "qemu+tcp://localhost/system"
	}
	return New(f, cfg), f
}

func TestNameAndHelpers(t *testing.T) {
	p, _ := testProvider(Config{})
	if p.Name() != "libvirt" {
		t.Fatalf("Name = %q", p.Name())
	}
	if got := Tag("Lab1"); got != "klotho-lab1" {
		t.Fatalf("Tag = %q", got)
	}
	if got := DomainName("lab1"); got != "lab1-bootstrap" {
		t.Fatalf("DomainName = %q", got)
	}
	if got := ProviderID("host1", "abc"); got != "libvirt://host1/abc" {
		t.Fatalf("ProviderID = %q", got)
	}
}

func TestHostFromURI(t *testing.T) {
	cases := map[string]string{
		"qemu:///system":             "localhost",
		"qemu+tcp://pve1/system":     "pve1",
		"qemu+tls://10.0.0.5/system": "10.0.0.5",
		"qemu+ssh://u@host2/system":  "host2",
	}
	for uri, want := range cases {
		if got := HostFromURI(uri); got != want {
			t.Errorf("HostFromURI(%q) = %q, want %q", uri, got, want)
		}
	}
}

func TestResolvePrecedence(t *testing.T) {
	p := New(newFakeClient(), Config{URI: "qemu+tcp://h/system", MemoryMiB: 8192})
	r := p.resolve(map[string]string{
		VarImage:        "fromvar.qcow2",
		VarPool:         "pool2",
		VarNetwork:      "net2",
		VarEndpointHost: "10.1.1.1",
	})
	if r.Host != "h" {
		t.Errorf("Host = %q", r.Host)
	}
	if r.Image != "fromvar.qcow2" || r.StoragePool != "pool2" || r.Network != "net2" {
		t.Errorf("vars not applied: %+v", r)
	}
	if r.MemoryMiB != 8192 {
		t.Errorf("MemoryMiB = %d (config should win)", r.MemoryMiB)
	}
	if r.CPUs != DefaultCPUs {
		t.Errorf("CPUs default = %d", r.CPUs)
	}
	if r.EndpointHost != "10.1.1.1" {
		t.Errorf("EndpointHost = %q", r.EndpointHost)
	}
	// Config wins over variable.
	p2 := New(newFakeClient(), Config{Image: "cfg.qcow2"})
	if got := p2.resolve(map[string]string{VarImage: "var.qcow2"}).Image; got != "cfg.qcow2" {
		t.Errorf("config should win: %q", got)
	}
}

func TestControllerURI(t *testing.T) {
	cases := []struct {
		name   string
		cfg    Config
		want   string
		idHost string
	}{
		{"explicit controllerURI overrides local uri",
			Config{URI: "qemu:///system", ControllerURI: "qemu+tcp://192.168.122.1/system"},
			"qemu+tcp://192.168.122.1/system", "192.168.122.1"},
		{"falls back to a network uri",
			Config{URI: "qemu+tcp://pve1/system"}, "qemu+tcp://pve1/system", "pve1"},
		{"falls back to local uri (then blocked by Prepare)",
			Config{URI: "qemu:///system"}, "qemu:///system", "localhost"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := ControllerURI(tc.cfg); got != tc.want {
				t.Errorf("ControllerURI = %q, want %q", got, tc.want)
			}
			p := New(newFakeClient(), tc.cfg)
			if got := p.idHost(); got != tc.idHost {
				t.Errorf("idHost = %q, want %q", got, tc.idHost)
			}
		})
	}
}

func TestPrepareControllerURI(t *testing.T) {
	// A local-socket controller URI is unreachable from a pod: Prepare blocks it.
	p, _ := testProvider(Config{URI: "qemu:///system"})
	if _, err := p.Prepare(context.Background(), &cluster.Spec{Name: "l"}); err == nil || !strings.Contains(err.Error(), "controller cannot reach") {
		t.Fatalf("want local-socket controller error, got %v", err)
	}
	// With a network controller URI, Prepare injects it as LIBVIRT_URI and the
	// controller host as LIBVIRT_HOST.
	p, _ = testProvider(Config{URI: "qemu:///system", ControllerURI: "qemu+tcp://192.168.122.1/system"})
	prep, err := p.Prepare(context.Background(), &cluster.Spec{Name: "lab1"})
	if err != nil {
		t.Fatal(err)
	}
	if prep.Variables[VarURI] != "qemu+tcp://192.168.122.1/system" {
		t.Errorf("LIBVIRT_URI = %q", prep.Variables[VarURI])
	}
	if prep.Variables[VarLibvirtHost] != "192.168.122.1" {
		t.Errorf("LIBVIRT_HOST = %q", prep.Variables[VarLibvirtHost])
	}
}

func TestPrepare(t *testing.T) {
	p, _ := testProvider(Config{})
	prep, err := p.Prepare(context.Background(), &cluster.Spec{Name: "lab1"})
	if err != nil {
		t.Fatal(err)
	}
	if prep.Endpoint.Host != "192.168.122.10" || prep.Endpoint.Port != DefaultPort {
		t.Fatalf("endpoint = %+v", prep.Endpoint)
	}
	if len(prep.Created) != 0 {
		t.Fatalf("Prepare allocated %d resources, want 0", len(prep.Created))
	}
	for _, k := range []string{VarURI, VarLibvirtHost, VarEndpointHost, VarImage, VarPool, VarNetwork} {
		if prep.Variables[k] == "" {
			t.Errorf("variable %s not injected", k)
		}
	}
	if prep.Variables[VarImage] != "base.qcow2" {
		t.Errorf("image var = %q", prep.Variables[VarImage])
	}
}

func TestPrepareErrors(t *testing.T) {
	t.Run("no image", func(t *testing.T) {
		p, _ := testProvider(Config{})
		p.cfg.Image = ""
		if _, err := p.Prepare(context.Background(), &cluster.Spec{Name: "l"}); err == nil || !strings.Contains(err.Error(), "base image") {
			t.Fatalf("err = %v", err)
		}
	})
	t.Run("no endpoint", func(t *testing.T) {
		p, _ := testProvider(Config{})
		p.cfg.EndpointHost = ""
		if _, err := p.Prepare(context.Background(), &cluster.Spec{Name: "l"}); err == nil || !strings.Contains(err.Error(), "endpoint") {
			t.Fatalf("err = %v", err)
		}
	})
	t.Run("missing pool", func(t *testing.T) {
		p, f := testProvider(Config{})
		f.pools = []string{"other"}
		if _, err := p.Prepare(context.Background(), &cluster.Spec{Name: "l"}); err == nil || !strings.Contains(err.Error(), "storage pool") {
			t.Fatalf("err = %v", err)
		}
	})
	t.Run("missing base image", func(t *testing.T) {
		p, f := testProvider(Config{})
		f.volumes = map[string]bool{}
		if _, err := p.Prepare(context.Background(), &cluster.Spec{Name: "l"}); err == nil || !strings.Contains(err.Error(), "base image volume") {
			t.Fatalf("err = %v", err)
		}
	})
	t.Run("missing network", func(t *testing.T) {
		p, f := testProvider(Config{})
		f.networks = []string{"other"}
		if _, err := p.Prepare(context.Background(), &cluster.Spec{Name: "l"}); err == nil || !strings.Contains(err.Error(), "network") {
			t.Fatalf("err = %v", err)
		}
	})
}

func TestLaunch(t *testing.T) {
	p, f := testProvider(Config{})
	f.addrs["lab1-bootstrap"] = []string{"192.168.122.10"}
	node, err := p.Launch(context.Background(), &cluster.Spec{Name: "lab1"}, []byte("#cloud-config\n"))
	if err != nil {
		t.Fatal(err)
	}
	if len(f.created) != 1 {
		t.Fatalf("created %d domains", len(f.created))
	}
	in := f.created[0]
	if in.Name != "lab1-bootstrap" || in.Description != "klotho-lab1" {
		t.Fatalf("create input = %+v", in)
	}
	if in.UUID == "" {
		t.Fatal("no UUID generated")
	}
	want := ProviderID("localhost", in.UUID)
	if node.ID != want {
		t.Fatalf("node.ID = %q, want %q", node.ID, want)
	}
	if node.Name != "lab1-bootstrap" {
		t.Fatalf("node.Name = %q", node.Name)
	}
	if len(node.Addresses) != 1 || node.Addresses[0] != "192.168.122.10" {
		t.Fatalf("addresses = %v", node.Addresses)
	}
	// The cidata seed carries the UUID as the meta-data instance-id.
	if !bytes.Contains(in.CIData, []byte(in.UUID)) {
		t.Fatal("cidata seed does not carry the domain UUID as instance-id")
	}
	if len(f.started) != 1 {
		t.Fatalf("started %d", len(f.started))
	}
}

func TestLaunchErrors(t *testing.T) {
	t.Run("empty user-data", func(t *testing.T) {
		p, _ := testProvider(Config{})
		if _, err := p.Launch(context.Background(), &cluster.Spec{Name: "l"}, nil); err == nil {
			t.Fatal("want error")
		}
	})
	t.Run("already exists", func(t *testing.T) {
		p, f := testProvider(Config{})
		f.domains["lab1-bootstrap"] = &Domain{Name: "lab1-bootstrap", UUID: "u", State: StateRunning}
		if _, err := p.Launch(context.Background(), &cluster.Spec{Name: "lab1"}, []byte("x")); err == nil || !strings.Contains(err.Error(), "already exists") {
			t.Fatalf("err = %v", err)
		}
	})
	t.Run("cleanup on start failure", func(t *testing.T) {
		p, f := testProvider(Config{})
		f.startErr = errors.New("boom")
		if _, err := p.Launch(context.Background(), &cluster.Spec{Name: "lab1"}, []byte("x")); err == nil {
			t.Fatal("want error")
		}
		if len(f.undefined) == 0 {
			t.Fatal("half-created domain was not cleaned up")
		}
		if _, ok := f.domains["lab1-bootstrap"]; ok {
			t.Fatal("domain still defined after cleanup")
		}
	})
}

func TestAdopt(t *testing.T) {
	p, f := testProvider(Config{})
	f.domains["lab1-bootstrap"] = &Domain{Name: "lab1-bootstrap", UUID: "dead-beef", State: StateRunning}
	bundle := controlPlaneBundle()
	state := &cluster.State{
		Spec:      cluster.Spec{Name: "lab1", Namespace: "default"},
		Bootstrap: &cluster.Node{ID: "libvirt://localhost/dead-beef"},
	}
	out, err := p.Adopt(context.Background(), state, bundle)
	if err != nil {
		t.Fatal(err)
	}
	var m map[string]any
	if err := yaml.Unmarshal(out, &m); err != nil {
		t.Fatal(err)
	}
	u := &unstructured.Unstructured{Object: m}
	if u.GetKind() != "LibvirtMachine" {
		t.Fatalf("kind = %q", u.GetKind())
	}
	if u.GetName() != "lab1-bootstrap" {
		t.Fatalf("name = %q", u.GetName())
	}
	spec, _, _ := unstructured.NestedMap(u.Object, "spec")
	if spec["domainName"] != "lab1-bootstrap" {
		t.Fatalf("domainName = %v", spec["domainName"])
	}
	if _, set := spec["providerID"]; set {
		t.Fatal("providerID should NOT be pre-set; the controller sets it")
	}
	if spec["image"] != "base.qcow2" {
		t.Fatalf("template spec not copied: %v", spec["image"])
	}
	if u.GetLabels()[LabelClusterName] != "lab1" {
		t.Fatalf("cluster label = %q", u.GetLabels()[LabelClusterName])
	}
	if u.GetAnnotations()[AnnotationClonedFromName] != "lab1-control-plane" {
		t.Fatalf("cloned-from = %q", u.GetAnnotations()[AnnotationClonedFromName])
	}
}

func TestAdoptProviderIDMismatch(t *testing.T) {
	p, f := testProvider(Config{})
	f.domains["lab1-bootstrap"] = &Domain{Name: "lab1-bootstrap", UUID: "aaaa", State: StateRunning}
	state := &cluster.State{
		Spec:      cluster.Spec{Name: "lab1"},
		Bootstrap: &cluster.Node{ID: "libvirt://localhost/bbbb"},
	}
	if _, err := p.Adopt(context.Background(), state, controlPlaneBundle()); err == nil || !strings.Contains(err.Error(), "provider ID") {
		t.Fatalf("err = %v", err)
	}
}

func TestAdoptMissingDomain(t *testing.T) {
	p, _ := testProvider(Config{})
	state := &cluster.State{Spec: cluster.Spec{Name: "lab1"}}
	if _, err := p.Adopt(context.Background(), state, controlPlaneBundle()); err == nil || !strings.Contains(err.Error(), "not found") {
		t.Fatalf("err = %v", err)
	}
}

func TestPlanAndTeardown(t *testing.T) {
	p, f := testProvider(Config{})
	f.domains["lab1-bootstrap"] = &Domain{Name: "lab1-bootstrap", UUID: "u1", State: StateRunning, Description: "klotho-lab1"}
	f.domains["lab1-md-0-xyz"] = &Domain{Name: "lab1-md-0-xyz", UUID: "u2", State: StateRunning, Description: "managed by klotho klotho-lab1"}
	f.domains["other-vm"] = &Domain{Name: "other-vm", UUID: "u3", State: StateRunning, Description: "not ours"}
	f.domains["lab1-stranger"] = &Domain{Name: "lab1-stranger", UUID: "u4", State: StateRunning, Description: "no tag here"}

	state := &cluster.State{Spec: cluster.Spec{Name: "lab1"}}
	plan, err := p.Plan(context.Background(), state)
	if err != nil {
		t.Fatal(err)
	}
	names := map[string]bool{}
	for _, r := range plan.Resources {
		if r.Kind != ResourceKindDomain {
			t.Fatalf("unexpected kind %q", r.Kind)
		}
		names[r.ID] = true
	}
	if !names["lab1-bootstrap"] || !names["lab1-md-0-xyz"] {
		t.Fatalf("plan missing tagged domains: %v", names)
	}
	if names["other-vm"] || names["lab1-stranger"] {
		t.Fatalf("plan includes untagged domains: %v", names)
	}

	if err := p.Teardown(context.Background(), state, plan); err != nil {
		t.Fatal(err)
	}
	if _, ok := f.domains["lab1-bootstrap"]; ok {
		t.Fatal("bootstrap domain not undefined")
	}
	if len(f.destroyed) != 2 || len(f.undefined) != 2 {
		t.Fatalf("destroyed=%v undefined=%v", f.destroyed, f.undefined)
	}
}

func TestTeardownRefusesForeign(t *testing.T) {
	p, f := testProvider(Config{})
	f.domains["elsewhere"] = &Domain{Name: "elsewhere", State: StateRunning}
	state := &cluster.State{Spec: cluster.Spec{Name: "lab1"}}
	bad := buildPlan(cluster.Resource{Kind: ResourceKindDomain, ID: "elsewhere", Name: "elsewhere"})
	err := p.Teardown(context.Background(), state, bad)
	if err == nil || !strings.Contains(err.Error(), "refusing") {
		t.Fatalf("err = %v", err)
	}
	if _, ok := f.domains["elsewhere"]; !ok {
		t.Fatal("foreign domain was removed")
	}
}

func TestDiscover(t *testing.T) {
	p, f := testProvider(Config{})
	f.networks = []string{"default", "isolated"}
	f.pools = []string{"default", "fast"}
	inv, err := p.Discover(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if len(inv[KindNetwork]) != 2 || len(inv[KindPool]) != 2 {
		t.Fatalf("inventory = %+v", inv)
	}
}

func TestCheck(t *testing.T) {
	p, _ := testProvider(Config{})
	// Full bundle: no findings.
	if f := p.Check(fullBundle()); len(f) != 0 {
		t.Fatalf("unexpected findings: %v", f)
	}
	// Missing LibvirtCluster.
	if f := p.Check(nil); len(f) != 1 {
		t.Fatalf("want 1 finding for missing cluster, got %v", f)
	}
}
