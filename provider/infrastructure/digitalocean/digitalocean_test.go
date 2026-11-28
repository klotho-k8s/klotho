package digitalocean

import (
	"context"
	"strings"
	"testing"
	"time"

	"github.com/digitalocean/godo"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"

	"github.com/klotho-k8s/klotho/pkg/cluster"
	"github.com/klotho-k8s/klotho/provider/infrastructure"
)

func newTestProvider(f *fakeDO, cfg Config) *Provider {
	p := New(f, cfg)
	p.launchPoll = time.Millisecond
	p.launchWait = time.Second
	return p
}

func baseCfg() Config {
	return Config{Token: "tok", Region: "nyc3"}
}

func TestPrepareValidatesCreatesLBAndReturnsVariables(t *testing.T) {
	f := newFakeDO()
	p := newTestProvider(f, baseCfg())
	prepared, err := p.Prepare(context.Background(), &cluster.Spec{Name: "demo"})
	if err != nil {
		t.Fatalf("Prepare: %v", err)
	}
	// Endpoint is the created load balancer's IP.
	if prepared.Endpoint.Host != "198.51.100.5" || prepared.Endpoint.Port != DefaultPort {
		t.Fatalf("endpoint = %+v", prepared.Endpoint)
	}
	// The load balancer is recorded as created, so a failed run can sweep it.
	if len(prepared.Created) != 1 || prepared.Created[0].Kind != ResourceKindLoadBalancer {
		t.Fatalf("created = %+v", prepared.Created)
	}
	for _, k := range []string{VarRegion, VarControlPlaneSize, VarNodeSize, VarImage, VarLoadBalancerID, VarControlPlaneCount} {
		if prepared.Variables[k] == "" {
			t.Errorf("variable %s not set: %+v", k, prepared.Variables)
		}
	}
	if prepared.Variables[VarControlPlaneSize] != DefaultSize {
		t.Errorf("size = %q, want default %q", prepared.Variables[VarControlPlaneSize], DefaultSize)
	}
	if prepared.Variables[VarImage] != DefaultImage {
		t.Errorf("image = %q, want default %q", prepared.Variables[VarImage], DefaultImage)
	}
	// The load balancer UUID is injected so CAPDO adopts it.
	if prepared.Variables[VarLoadBalancerID] != prepared.Created[0].ID {
		t.Errorf("DO_LOADBALANCER_ID = %q, want %q", prepared.Variables[VarLoadBalancerID], prepared.Created[0].ID)
	}
	// The created load balancer targets the apiserver tag.
	if f.createdLB == nil || f.createdLB.Tag != apiServerTag("demo") {
		t.Errorf("load balancer target tag = %q, want %q", f.createdLB.Tag, apiServerTag("demo"))
	}
	// The API token must never leak into the generated bundle's variables.
	for k, v := range prepared.Variables {
		if v == "tok" {
			t.Errorf("token leaked into template variable %s", k)
		}
	}
}

func TestPrepareRequiresRegion(t *testing.T) {
	cfg := baseCfg()
	cfg.Region = ""
	p := newTestProvider(newFakeDO(), cfg)
	if _, err := p.Prepare(context.Background(), &cluster.Spec{Name: "demo"}); err == nil {
		t.Fatal("expected an error when region is missing")
	}
}

func TestPrepareRejectsUnknownRegion(t *testing.T) {
	cfg := baseCfg()
	cfg.Region = "xx9"
	p := newTestProvider(newFakeDO(), cfg)
	if _, err := p.Prepare(context.Background(), &cluster.Spec{Name: "demo"}); err == nil {
		t.Fatal("expected an error for an unknown region")
	}
}

func TestPrepareRejectsUnknownImage(t *testing.T) {
	cfg := baseCfg()
	cfg.Image = "no-such-image"
	p := newTestProvider(newFakeDO(), cfg)
	if _, err := p.Prepare(context.Background(), &cluster.Spec{Name: "demo"}); err == nil {
		t.Fatal("expected an error for an unknown image slug")
	}
}

func TestPrepareReusesConfiguredLoadBalancer(t *testing.T) {
	f := newFakeDO()
	f.seedLB("lb-existing", "203.0.113.200")
	cfg := baseCfg()
	cfg.LoadBalancerID = "lb-existing"
	p := newTestProvider(f, cfg)
	prepared, err := p.Prepare(context.Background(), &cluster.Spec{Name: "demo"})
	if err != nil {
		t.Fatalf("Prepare: %v", err)
	}
	if prepared.Endpoint.Host != "203.0.113.200" {
		t.Errorf("endpoint = %+v, want the existing LB IP", prepared.Endpoint)
	}
	if len(prepared.Created) != 0 {
		t.Errorf("reusing a load balancer should create nothing, got %+v", prepared.Created)
	}
	if f.createdLB != nil {
		t.Error("a load balancer was created despite LoadBalancerID being set")
	}
	if prepared.Variables[VarLoadBalancerID] != "lb-existing" {
		t.Errorf("DO_LOADBALANCER_ID = %q, want lb-existing", prepared.Variables[VarLoadBalancerID])
	}
}

func TestPrepareUsesSuppliedEndpoint(t *testing.T) {
	f := newFakeDO()
	cfg := baseCfg()
	cfg.ControlPlaneEndpoint = "cp.example.com"
	p := newTestProvider(f, cfg)
	prepared, err := p.Prepare(context.Background(), &cluster.Spec{Name: "demo"})
	if err != nil {
		t.Fatalf("Prepare: %v", err)
	}
	if prepared.Endpoint.Host != "cp.example.com" {
		t.Errorf("endpoint = %+v, want the supplied host", prepared.Endpoint)
	}
	if f.createdLB != nil {
		t.Error("a load balancer was created despite a supplied endpoint")
	}
}

func TestLaunchTagsUserDataAndProviderID(t *testing.T) {
	f := newFakeDO()
	cfg := baseCfg()
	cfg.SSHKeys = []string{"aa:bb:cc", "12345"}
	p := newTestProvider(f, cfg)
	spec := &cluster.Spec{Name: "demo"}

	node, err := p.Launch(context.Background(), spec, []byte("#cloud-config\n"))
	if err != nil {
		t.Fatalf("Launch: %v", err)
	}
	if node.ID != providerID(1001) {
		t.Errorf("node ID = %q, want %q", node.ID, providerID(1001))
	}
	if node.Name != MachineName("demo") {
		t.Errorf("node Name = %q", node.Name)
	}
	if len(node.Addresses) != 2 {
		t.Errorf("addresses = %v, want public+private", node.Addresses)
	}
	if f.createReq.Name != MachineName("demo") {
		t.Errorf("droplet name = %q, want %q", f.createReq.Name, MachineName("demo"))
	}
	// user-data is the raw cloud-init.
	if f.createReq.UserData != "#cloud-config\n" {
		t.Errorf("user-data = %q", f.createReq.UserData)
	}
	// klotho's ownership tag and CAPDO's apiserver tag are present.
	if !hasClusterTag(f.createReq.Tags, "demo") {
		t.Errorf("droplet missing ownership tag: %v", f.createReq.Tags)
	}
	if !containsTag(f.createReq.Tags, apiServerTag("demo")) {
		t.Errorf("droplet missing apiserver tag (LB target): %v", f.createReq.Tags)
	}
	// SSH keys: a numeric entry is an ID, the rest fingerprints.
	if len(f.createReq.SSHKeys) != 2 {
		t.Fatalf("ssh keys = %+v", f.createReq.SSHKeys)
	}
	if f.createReq.SSHKeys[0].Fingerprint != "aa:bb:cc" || f.createReq.SSHKeys[1].ID != 12345 {
		t.Errorf("ssh keys not parsed by kind: %+v", f.createReq.SSHKeys)
	}
}

func TestLaunchDefaultsSizeAndImage(t *testing.T) {
	f := newFakeDO()
	p := newTestProvider(f, baseCfg())
	if _, err := p.Launch(context.Background(), &cluster.Spec{Name: "demo"}, []byte("x")); err != nil {
		t.Fatalf("Launch: %v", err)
	}
	if f.createReq.Size != DefaultSize {
		t.Errorf("size = %q, want %q", f.createReq.Size, DefaultSize)
	}
	if f.createReq.Image.Slug != DefaultImage {
		t.Errorf("image slug = %q, want %q", f.createReq.Image.Slug, DefaultImage)
	}
}

func TestAdoptSetsProviderID(t *testing.T) {
	f := newFakeDO()
	p := newTestProvider(f, baseCfg())
	bundle := []*unstructured.Unstructured{
		machineTemplate("demo-control-plane"),
		kubeadmControlPlane("demo-control-plane", "demo-control-plane"),
	}
	st := &cluster.State{
		Spec:      cluster.Spec{Name: "demo", Namespace: "default"},
		Bootstrap: &cluster.Node{ID: providerID(1001), Name: MachineName("demo")},
	}
	out, err := p.Adopt(context.Background(), st, bundle)
	if err != nil {
		t.Fatalf("Adopt: %v", err)
	}
	s := string(out)
	if !strings.Contains(s, "kind: DOMachine") {
		t.Errorf("adopt output is not a DOMachine:\n%s", s)
	}
	// CAPDO adopts by a pre-set providerID; it MUST be present.
	if !strings.Contains(s, "providerID: "+providerID(1001)) {
		t.Errorf("adopt output missing providerID %q:\n%s", providerID(1001), s)
	}
	if !strings.Contains(s, "name: demo-bootstrap") {
		t.Errorf("adopt output has the wrong machine name (want demo-bootstrap):\n%s", s)
	}
	if !strings.Contains(s, "cloned-from-name: demo-control-plane") {
		t.Errorf("adopt output missing cloned-from annotation:\n%s", s)
	}
}

func TestAdoptRefindsProviderIDByTag(t *testing.T) {
	f := newFakeDO()
	// state has no recorded bootstrap node; the droplet is found by name+tag.
	f.seedDroplet(2002, MachineName("demo"), []string{clusterTag("demo")})
	p := newTestProvider(f, baseCfg())
	bundle := []*unstructured.Unstructured{machineTemplate("demo-control-plane")}
	st := &cluster.State{Spec: cluster.Spec{Name: "demo", Namespace: "default"}}
	out, err := p.Adopt(context.Background(), st, bundle)
	if err != nil {
		t.Fatalf("Adopt: %v", err)
	}
	if !strings.Contains(string(out), "providerID: "+providerID(2002)) {
		t.Errorf("adopt output did not re-find the provider ID by tag:\n%s", out)
	}
}

func TestTagMachineTemplatesUsesAdditionalTags(t *testing.T) {
	f := newFakeDO()
	p := newTestProvider(f, baseCfg())
	objs := []*unstructured.Unstructured{machineTemplate("demo-control-plane")}
	if err := p.TagMachineTemplates(objs, Tag("demo")); err != nil {
		t.Fatalf("TagMachineTemplates: %v", err)
	}
	tags, found, err := unstructured.NestedStringSlice(objs[0].Object, "spec", "template", "spec", "additionalTags")
	if err != nil || !found {
		t.Fatalf("additionalTags not set: found=%v err=%v", found, err)
	}
	if !containsTag(tags, Tag("demo")) {
		t.Errorf("additionalTags = %v, want it to contain %q", tags, Tag("demo"))
	}
	// It must not use the Proxmox-schema spec.template.spec.tags string list.
	if _, found, _ := unstructured.NestedStringSlice(objs[0].Object, "spec", "template", "spec", "tags"); found {
		t.Error("TagMachineTemplates wrote spec.template.spec.tags, which CAPDO rejects")
	}
}

func TestMachineNameMatchesBootstrapConvention(t *testing.T) {
	if MachineName("demo") != "demo-bootstrap" {
		t.Fatalf("MachineName = %q, want demo-bootstrap", MachineName("demo"))
	}
}

func TestProviderIDFormat(t *testing.T) {
	if got := providerID(42); got != "digitalocean://42" {
		t.Fatalf("providerID = %q, want digitalocean://42", got)
	}
}

func TestTagIsDigitalOceanSafe(t *testing.T) {
	// Dots and slashes in a cluster name must not leak into a DO tag.
	got := Tag("my.cluster/one")
	if strings.ContainsAny(got, "./") {
		t.Errorf("Tag(%q) = %q, contains a character DigitalOcean tags reject", "my.cluster/one", got)
	}
}

func TestPlanAndTeardown(t *testing.T) {
	f := newFakeDO()
	p := newTestProvider(f, baseCfg())
	f.seedDroplet(1001, MachineName("demo"), []string{clusterTag("demo")})
	f.seedDroplet(1002, "demo-md-0-abc", []string{clusterTag("demo")})
	f.seedDroplet(9999, "unrelated", []string{clusterTag("other")})
	f.seedLB("lb-demo", "198.51.100.5")

	st := &cluster.State{
		Spec:      cluster.Spec{Name: "demo"},
		Resources: []cluster.Resource{{Kind: ResourceKindLoadBalancer, ID: "lb-demo", Name: "demo-apiserver"}},
	}
	plan, err := p.Plan(context.Background(), st)
	if err != nil {
		t.Fatalf("Plan: %v", err)
	}
	// Two tagged droplets plus the load balancer from state.
	var droplets, lbs int
	for _, r := range plan.Resources {
		switch r.Kind {
		case ResourceKindDroplet:
			droplets++
		case ResourceKindLoadBalancer:
			lbs++
		}
	}
	if droplets != 2 || lbs != 1 {
		t.Fatalf("plan = %+v, want 2 droplets + 1 load balancer", plan.Resources)
	}
	if err := p.Teardown(context.Background(), st, plan); err != nil {
		t.Fatalf("Teardown: %v", err)
	}
	if len(f.deleted) != 2 {
		t.Errorf("deleted droplets = %v, want the two tagged ones", f.deleted)
	}
	if len(f.deletedLB) != 1 {
		t.Errorf("deleted load balancers = %v, want 1", f.deletedLB)
	}
	// The unrelated droplet is untouched.
	if _, ok := f.droplets[9999]; !ok {
		t.Error("Teardown deleted a droplet belonging to another cluster")
	}
}

func TestTeardownIgnoresAlreadyGone(t *testing.T) {
	f := newFakeDO()
	p := newTestProvider(f, baseCfg())
	plan := &infrastructure.Plan{Resources: []cluster.Resource{
		{Kind: ResourceKindDroplet, ID: "1234"},
		{Kind: ResourceKindLoadBalancer, ID: "lb-gone"},
	}}
	if err := p.Teardown(context.Background(), &cluster.State{Spec: cluster.Spec{Name: "demo"}}, plan); err != nil {
		t.Fatalf("Teardown of missing resources should not error: %v", err)
	}
}

func TestDiscover(t *testing.T) {
	f := newFakeDO()
	p := newTestProvider(f, baseCfg())
	inv, err := p.Discover(context.Background())
	if err != nil {
		t.Fatalf("Discover: %v", err)
	}
	if len(inv["region"]) != 1 || len(inv["size"]) != 1 || len(inv["image"]) != 1 {
		t.Errorf("inventory = %+v", inv)
	}
}

func TestDiscoverFiltersUnavailable(t *testing.T) {
	f := newFakeDO()
	f.regions = append(f.regions, godo.Region{Slug: "old", Name: "Old", Available: false})
	f.sizes = append(f.sizes, godo.Size{Slug: "retired", Available: false})
	p := newTestProvider(f, baseCfg())
	inv, err := p.Discover(context.Background())
	if err != nil {
		t.Fatalf("Discover: %v", err)
	}
	if len(inv["region"]) != 1 {
		t.Errorf("unavailable region not filtered: %+v", inv["region"])
	}
	if len(inv["size"]) != 1 {
		t.Errorf("unavailable size not filtered: %+v", inv["size"])
	}
}

func TestCheckFlagsMissingSSHKeyAndLoadBalancer(t *testing.T) {
	p := newTestProvider(newFakeDO(), Config{Region: "nyc3"})
	bundle := []*unstructured.Unstructured{
		doCluster("demo"),
		doMachineTemplate("demo-control-plane"), // no sshKeys
	}
	findings := p.Check(bundle)
	var ssh, lb bool
	for _, f := range findings {
		if strings.Contains(f.Field, "sshKeys") {
			ssh = true
		}
		if strings.Contains(f.Field, "apiServerLoadbalancers") {
			lb = true
		}
	}
	if !ssh {
		t.Errorf("expected an sshKeys finding, got %+v", findings)
	}
	if !lb {
		t.Errorf("expected a load balancer cost finding, got %+v", findings)
	}
}

func TestCheckNoSSHFindingWithConfiguredKey(t *testing.T) {
	p := newTestProvider(newFakeDO(), Config{Region: "nyc3", SSHKeys: []string{"aa:bb"}})
	bundle := []*unstructured.Unstructured{doCluster("demo")}
	for _, f := range p.Check(bundle) {
		if strings.Contains(f.Field, "sshKeys") {
			t.Errorf("unexpected sshKeys finding when a key is configured: %+v", f)
		}
	}
}

// --- helpers ---

func containsTag(tags []string, want string) bool {
	for _, t := range tags {
		if t == want {
			return true
		}
	}
	return false
}

func machineTemplate(name string) *unstructured.Unstructured {
	u := &unstructured.Unstructured{Object: map[string]any{
		"spec": map[string]any{"template": map[string]any{"spec": map[string]any{"size": "s-2vcpu-4gb"}}},
	}}
	u.SetAPIVersion("infrastructure.cluster.x-k8s.io/v1beta1")
	u.SetKind("DOMachineTemplate")
	u.SetName(name)
	u.SetNamespace("default")
	return u
}

func doMachineTemplate(name string) *unstructured.Unstructured { return machineTemplate(name) }

func kubeadmControlPlane(name, infraRef string) *unstructured.Unstructured {
	u := &unstructured.Unstructured{Object: map[string]any{
		"spec": map[string]any{"machineTemplate": map[string]any{"spec": map[string]any{
			"infrastructureRef": map[string]any{"name": infraRef, "kind": "DOMachineTemplate"},
		}}},
	}}
	u.SetAPIVersion("controlplane.cluster.x-k8s.io/v1beta2")
	u.SetKind("KubeadmControlPlane")
	u.SetName(name)
	u.SetNamespace("default")
	return u
}

func doCluster(name string) *unstructured.Unstructured {
	u := &unstructured.Unstructured{Object: map[string]any{
		"spec": map[string]any{"region": "nyc3"},
	}}
	u.SetAPIVersion("infrastructure.cluster.x-k8s.io/v1beta1")
	u.SetKind("DOCluster")
	u.SetName(name)
	u.SetNamespace("default")
	return u
}
