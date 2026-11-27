package vz

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

// testProvider returns a provider wired to a fresh fake, with the config fields a
// run needs so each test can override just what it exercises.
func testProvider(cfg Config) (*Provider, *fakeClient) {
	f := newFakeClient()
	if cfg.Endpoint == "" {
		cfg.Endpoint = "mac.local:7443"
	}
	if cfg.Image == "" {
		cfg.Image = "ubuntu-arm64.raw"
	}
	if cfg.EndpointHost == "" {
		cfg.EndpointHost = "192.168.64.10"
	}
	return New(f, cfg), f
}

func TestNameAndHelpers(t *testing.T) {
	p, _ := testProvider(Config{})
	if p.Name() != "vz" {
		t.Fatalf("Name = %q", p.Name())
	}
	if got := Tag("Lab1"); got != "klotho-lab1" {
		t.Fatalf("Tag = %q", got)
	}
	if got := VMName("lab1"); got != "lab1-bootstrap" {
		t.Fatalf("VMName = %q", got)
	}
	if got := ProviderID("host1", "abc"); got != "vz://host1/abc" {
		t.Fatalf("ProviderID = %q", got)
	}
}

func TestParseProviderID(t *testing.T) {
	host, vmid, ok := ParseProviderID("vz://localhost/dead-beef")
	if !ok || host != "localhost" || vmid != "dead-beef" {
		t.Fatalf("ParseProviderID = %q/%q ok=%v", host, vmid, ok)
	}
	// Round-trips with ProviderID.
	if h, v, ok := ParseProviderID(ProviderID("mac", "uuid-9")); !ok || h != "mac" || v != "uuid-9" {
		t.Fatalf("round-trip = %q/%q ok=%v", h, v, ok)
	}
	for _, bad := range []string{"", "vz://", "vz://host", "vz://host/", "vz:///vmid", "libvirt://h/v", "host/vmid"} {
		if _, _, ok := ParseProviderID(bad); ok {
			t.Errorf("ParseProviderID(%q) unexpectedly ok", bad)
		}
	}
}

func TestResolvePrecedence(t *testing.T) {
	p := New(newFakeClient(), Config{Host: "mac1", MemoryMiB: 8192})
	r := p.resolve(map[string]string{
		VarImagePath:    "fromvar.raw",
		VarHost:         "ignored-host",
		VarEndpointHost: "10.1.1.1",
	})
	if r.Host != "mac1" {
		t.Errorf("Host = %q (config should win)", r.Host)
	}
	if r.Image != "fromvar.raw" {
		t.Errorf("Image = %q (var should apply)", r.Image)
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
	// Host defaults to localhost when neither config nor var set it.
	if got := p2Resolve(t).Host; got != DefaultHost {
		t.Errorf("default Host = %q", got)
	}
}

func p2Resolve(t *testing.T) resolved {
	t.Helper()
	return New(newFakeClient(), Config{}).resolve(nil)
}

func TestPrepare(t *testing.T) {
	p, _ := testProvider(Config{})
	prep, err := p.Prepare(context.Background(), &cluster.Spec{Name: "lab1"})
	if err != nil {
		t.Fatal(err)
	}
	if prep.Endpoint.Host != "192.168.64.10" || prep.Endpoint.Port != DefaultPort {
		t.Fatalf("endpoint = %+v", prep.Endpoint)
	}
	if len(prep.Created) != 0 {
		t.Fatalf("Prepare allocated %d resources, want 0", len(prep.Created))
	}
	for _, k := range []string{VarHost, VarImagePath, VarEndpointHost} {
		if prep.Variables[k] == "" {
			t.Errorf("variable %s not injected", k)
		}
	}
	if prep.Variables[VarImagePath] != "ubuntu-arm64.raw" {
		t.Errorf("image var = %q", prep.Variables[VarImagePath])
	}
	if prep.Variables[VarHost] != DefaultHost {
		t.Errorf("host var = %q", prep.Variables[VarHost])
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
	t.Run("no endpoint host", func(t *testing.T) {
		p, _ := testProvider(Config{})
		p.cfg.EndpointHost = ""
		if _, err := p.Prepare(context.Background(), &cluster.Spec{Name: "l"}); err == nil || !strings.Contains(err.Error(), "control plane endpoint") {
			t.Fatalf("err = %v", err)
		}
	})
	t.Run("no daemon endpoint", func(t *testing.T) {
		p, _ := testProvider(Config{})
		p.cfg.Endpoint = ""
		if _, err := p.Prepare(context.Background(), &cluster.Spec{Name: "l"}); err == nil || !strings.Contains(err.Error(), "daemon endpoint") {
			t.Fatalf("err = %v", err)
		}
	})
}

func TestLaunch(t *testing.T) {
	p, f := testProvider(Config{})
	userData := []byte("#cloud-config\nhostname: lab1\n")
	f.addrs["lab1-bootstrap"] = []string{"192.168.64.10"}
	node, err := p.Launch(context.Background(), &cluster.Spec{Name: "lab1"}, userData)
	if err != nil {
		t.Fatal(err)
	}
	if len(f.created) != 1 {
		t.Fatalf("created %d VMs", len(f.created))
	}
	in := f.created[0]
	if in.Name != "lab1-bootstrap" || in.Notes != "klotho-lab1" {
		t.Fatalf("create input = %+v", in)
	}
	if in.DiskImage != "ubuntu-arm64.raw" {
		t.Fatalf("disk image = %q", in.DiskImage)
	}
	if in.MemoryBytes != int64(DefaultMemoryMiB)<<20 || in.CPUCount != DefaultCPUs {
		t.Fatalf("size = %d bytes / %d cpu", in.MemoryBytes, in.CPUCount)
	}
	// providerID uses the vmid the daemon assigned.
	vm := f.vms["lab1-bootstrap"]
	want := ProviderID("localhost", vm.ID)
	if node.ID != want {
		t.Fatalf("node.ID = %q, want %q", node.ID, want)
	}
	if node.Name != "lab1-bootstrap" {
		t.Fatalf("node.Name = %q", node.Name)
	}
	if len(node.Addresses) != 1 || node.Addresses[0] != "192.168.64.10" {
		t.Fatalf("addresses = %v", node.Addresses)
	}
	// The cidata seed is delivered to the daemon as the exact bootstrap
	// user-data (the daemon builds the NoCloud seed from it).
	if got := f.attached["lab1-bootstrap"]; !bytes.Equal(got, userData) {
		t.Fatalf("AttachCloudInit user-data = %q, want %q", got, userData)
	}
	if len(f.started) != 1 {
		t.Fatalf("started %d", len(f.started))
	}
}

func TestLaunchHostFromConfig(t *testing.T) {
	p, f := testProvider(Config{Host: "studio.local"})
	f.addrs["lab1-bootstrap"] = []string{"10.0.0.5"}
	node, err := p.Launch(context.Background(), &cluster.Spec{Name: "lab1"}, []byte("x"))
	if err != nil {
		t.Fatal(err)
	}
	if !strings.HasPrefix(node.ID, "vz://studio.local/") {
		t.Fatalf("node.ID host not from config: %q", node.ID)
	}
}

func TestLaunchErrors(t *testing.T) {
	t.Run("empty user-data", func(t *testing.T) {
		p, _ := testProvider(Config{})
		if _, err := p.Launch(context.Background(), &cluster.Spec{Name: "l"}, nil); err == nil {
			t.Fatal("want error")
		}
	})
	t.Run("no image", func(t *testing.T) {
		p, _ := testProvider(Config{})
		p.cfg.Image = ""
		if _, err := p.Launch(context.Background(), &cluster.Spec{Name: "l"}, []byte("x")); err == nil || !strings.Contains(err.Error(), "base image") {
			t.Fatalf("err = %v", err)
		}
	})
	t.Run("already exists", func(t *testing.T) {
		p, f := testProvider(Config{})
		f.seed("lab1-bootstrap", "u", "klotho-lab1")
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
		if len(f.removed) == 0 {
			t.Fatal("half-created VM was not cleaned up")
		}
		if _, ok := f.vms["lab1-bootstrap"]; ok {
			t.Fatal("VM still present after cleanup")
		}
	})
}

func TestAdopt(t *testing.T) {
	p, f := testProvider(Config{})
	f.seed("lab1-bootstrap", "dead-beef", "klotho-lab1")
	bundle := controlPlaneBundle()
	state := &cluster.State{
		Spec:      cluster.Spec{Name: "lab1", Namespace: "default"},
		Bootstrap: &cluster.Node{ID: "vz://localhost/dead-beef"},
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
	if u.GetKind() != "VZMachine" {
		t.Fatalf("kind = %q", u.GetKind())
	}
	if u.GetName() != "lab1-bootstrap" {
		t.Fatalf("name = %q", u.GetName())
	}
	spec, _, _ := unstructured.NestedMap(u.Object, "spec")
	if spec["vmName"] != "lab1-bootstrap" {
		t.Fatalf("vmName = %v", spec["vmName"])
	}
	if _, set := spec["providerID"]; set {
		t.Fatal("providerID should NOT be pre-set; the controller sets it")
	}
	if spec["image"] != "ubuntu-arm64.raw" {
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
	f.seed("lab1-bootstrap", "aaaa", "klotho-lab1")
	state := &cluster.State{
		Spec:      cluster.Spec{Name: "lab1"},
		Bootstrap: &cluster.Node{ID: "vz://localhost/bbbb"},
	}
	if _, err := p.Adopt(context.Background(), state, controlPlaneBundle()); err == nil || !strings.Contains(err.Error(), "provider ID") {
		t.Fatalf("err = %v", err)
	}
}

func TestAdoptMissingVM(t *testing.T) {
	p, _ := testProvider(Config{})
	state := &cluster.State{Spec: cluster.Spec{Name: "lab1"}}
	if _, err := p.Adopt(context.Background(), state, controlPlaneBundle()); err == nil || !strings.Contains(err.Error(), "not found") {
		t.Fatalf("err = %v", err)
	}
}

func TestPlanAndTeardown(t *testing.T) {
	p, f := testProvider(Config{})
	f.seed("lab1-bootstrap", "u1", "klotho-lab1")
	f.seed("lab1-md-0-xyz", "u2", "managed by klotho klotho-lab1")
	f.seed("other-vm", "u3", "not ours")
	f.seed("lab1-stranger", "u4", "no tag here")

	state := &cluster.State{Spec: cluster.Spec{Name: "lab1"}}
	plan, err := p.Plan(context.Background(), state)
	if err != nil {
		t.Fatal(err)
	}
	names := map[string]bool{}
	for _, r := range plan.Resources {
		if r.Kind != ResourceKindVM {
			t.Fatalf("unexpected kind %q", r.Kind)
		}
		names[r.ID] = true
	}
	if !names["lab1-bootstrap"] || !names["lab1-md-0-xyz"] {
		t.Fatalf("plan missing tagged VMs: %v", names)
	}
	if names["other-vm"] || names["lab1-stranger"] {
		t.Fatalf("plan includes untagged/foreign VMs: %v", names)
	}

	if err := p.Teardown(context.Background(), state, plan); err != nil {
		t.Fatal(err)
	}
	if _, ok := f.vms["lab1-bootstrap"]; ok {
		t.Fatal("bootstrap VM not removed")
	}
	if len(f.stopped) != 2 || len(f.removed) != 2 {
		t.Fatalf("stopped=%v removed=%v", f.stopped, f.removed)
	}
}

func TestTeardownRefusesForeign(t *testing.T) {
	p, f := testProvider(Config{})
	f.seed("elsewhere", "x", "")
	state := &cluster.State{Spec: cluster.Spec{Name: "lab1"}}
	bad := buildPlan(cluster.Resource{Kind: ResourceKindVM, ID: "elsewhere", Name: "elsewhere"})
	err := p.Teardown(context.Background(), state, bad)
	if err == nil || !strings.Contains(err.Error(), "refusing") {
		t.Fatalf("err = %v", err)
	}
	if _, ok := f.vms["elsewhere"]; !ok {
		t.Fatal("foreign VM was removed")
	}
}

func TestDiscover(t *testing.T) {
	p, f := testProvider(Config{Host: "mac1"})
	f.seed("lab1-bootstrap", "u1", "klotho-lab1", "10.0.0.9")
	f.seed("other", "u2", "x")
	inv, err := p.Discover(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if len(inv[KindHost]) != 1 || inv[KindHost][0].ID != "mac1" {
		t.Fatalf("host inventory = %+v", inv[KindHost])
	}
	if len(inv[KindVM]) != 2 {
		t.Fatalf("vm inventory = %+v", inv[KindVM])
	}
}

func TestDiscoverDaemonError(t *testing.T) {
	p, f := testProvider(Config{})
	f.listErr = errors.New("unreachable")
	if _, err := p.Discover(context.Background()); err == nil {
		t.Fatal("want error when the daemon is unreachable")
	}
}

func TestCheck(t *testing.T) {
	p, _ := testProvider(Config{})
	// Full bundle: no findings.
	if f := p.Check(fullBundle()); len(f) != 0 {
		t.Fatalf("unexpected findings: %v", f)
	}
	// Missing VZCluster.
	if f := p.Check(nil); len(f) != 1 {
		t.Fatalf("want 1 finding for missing cluster, got %v", f)
	}
	// Missing image on the machine template.
	bundle := []*unstructured.Unstructured{vzCluster("lab1"), machineTemplate("lab1-control-plane", ""), kcp("lab1")}
	found := false
	for _, fnd := range p.Check(bundle) {
		if strings.Contains(fnd.Field, "image") {
			found = true
		}
	}
	if !found {
		t.Fatal("expected a finding about the missing image")
	}
}
