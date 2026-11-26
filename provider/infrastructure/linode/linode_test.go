package linode

import (
	"context"
	"encoding/base64"
	"strings"
	"testing"
	"time"

	"github.com/linode/linodego/v2"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"

	"github.com/klotho-k8s/klotho/pkg/cluster"
	"github.com/klotho-k8s/klotho/provider/infrastructure"
)

func newTestProvider(f *fakeLinode, cfg Config) *Provider {
	p := New(f, cfg)
	p.launchPoll = time.Millisecond
	p.launchWait = time.Second
	return p
}

func baseCfg() Config {
	return Config{Token: "tok", Region: "us-ord", ControlPlaneEndpoint: "cp.example.com"}
}

func TestPrepareValidatesAndReturnsVariables(t *testing.T) {
	f := newFakeLinode()
	p := newTestProvider(f, baseCfg())
	prepared, err := p.Prepare(context.Background(), &cluster.Spec{Name: "demo"})
	if err != nil {
		t.Fatalf("Prepare: %v", err)
	}
	if prepared.Endpoint.Host != "cp.example.com" || prepared.Endpoint.Port != DefaultPort {
		t.Fatalf("endpoint = %+v", prepared.Endpoint)
	}
	for _, k := range []string{VarRegion, VarControlPlaneType, VarNodeType, VarImage, VarControlPlaneCount} {
		if prepared.Variables[k] == "" {
			t.Errorf("variable %s not set: %+v", k, prepared.Variables)
		}
	}
	if prepared.Variables[VarControlPlaneType] != DefaultType {
		t.Errorf("control plane type = %q, want default %q", prepared.Variables[VarControlPlaneType], DefaultType)
	}
	if prepared.Variables[VarImage] != DefaultImage {
		t.Errorf("image = %q, want default %q", prepared.Variables[VarImage], DefaultImage)
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
	p := newTestProvider(newFakeLinode(), cfg)
	if _, err := p.Prepare(context.Background(), &cluster.Spec{Name: "demo"}); err == nil {
		t.Fatal("expected an error when region is missing")
	}
}

func TestPrepareRequiresEndpoint(t *testing.T) {
	cfg := baseCfg()
	cfg.ControlPlaneEndpoint = ""
	p := newTestProvider(newFakeLinode(), cfg)
	if _, err := p.Prepare(context.Background(), &cluster.Spec{Name: "demo"}); err == nil {
		t.Fatal("expected an error when controlPlaneEndpoint is missing")
	}
}

func TestPrepareRejectsRegionWithoutMetadata(t *testing.T) {
	f := newFakeLinode()
	f.regions[0].Capabilities = []string{"Linodes"} // no Metadata
	p := newTestProvider(f, baseCfg())
	if _, err := p.Prepare(context.Background(), &cluster.Spec{Name: "demo"}); err == nil {
		t.Fatal("expected an error when the region lacks the Metadata capability")
	}
}

func TestPrepareRejectsImageWithoutCloudInit(t *testing.T) {
	f := newFakeLinode()
	f.images[0].Capabilities = nil // no cloud-init
	p := newTestProvider(f, baseCfg())
	if _, err := p.Prepare(context.Background(), &cluster.Spec{Name: "demo"}); err == nil {
		t.Fatal("expected an error when the image lacks cloud-init")
	}
}

func TestLaunchTagsLabelAndUserData(t *testing.T) {
	f := newFakeLinode()
	cfg := baseCfg()
	cfg.AuthorizedKeys = []string{"ssh-ed25519 AAAA"}
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
		t.Errorf("addresses = %v", node.Addresses)
	}
	// The instance label must equal the adopted LinodeMachine name, so CAPL's
	// label-uniqueness guard adopts rather than duplicates.
	if f.createOpts.Label != MachineName("demo") {
		t.Errorf("label = %q, want %q", f.createOpts.Label, MachineName("demo"))
	}
	// user-data is base64 of the cloud-init, delivered via Metadata.
	if f.createOpts.Metadata == nil {
		t.Fatal("no Metadata set on the create options")
	}
	dec, _ := base64.StdEncoding.DecodeString(f.createOpts.Metadata.UserData)
	if string(dec) != "#cloud-config\n" {
		t.Errorf("user-data = %q", dec)
	}
	// klotho's ownership tag is present.
	if !hasClusterTag(f.createOpts.Tags, "demo") {
		t.Errorf("instance missing ownership tag: %v", f.createOpts.Tags)
	}
	if len(f.createOpts.AuthorizedKeys) != 1 {
		t.Errorf("authorized keys = %v", f.createOpts.AuthorizedKeys)
	}
	// A root password is always set (Linode requires one with an image).
	if f.createOpts.RootPass == "" {
		t.Error("no root password set")
	}
}

func TestLaunchDefaultsTypeAndImage(t *testing.T) {
	f := newFakeLinode()
	p := newTestProvider(f, baseCfg())
	if _, err := p.Launch(context.Background(), &cluster.Spec{Name: "demo"}, []byte("x")); err != nil {
		t.Fatalf("Launch: %v", err)
	}
	if f.createOpts.Type != DefaultType {
		t.Errorf("type = %q, want %q", f.createOpts.Type, DefaultType)
	}
	if f.createOpts.Image != DefaultImage {
		t.Errorf("image = %q, want %q", f.createOpts.Image, DefaultImage)
	}
}

func TestAdoptSetsProviderID(t *testing.T) {
	f := newFakeLinode()
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
	if !strings.Contains(s, "kind: LinodeMachine") {
		t.Errorf("adopt output is not a LinodeMachine:\n%s", s)
	}
	// CAPL adopts by a pre-set providerID; it MUST be present.
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

func TestAdoptRefindsProviderIDByLabel(t *testing.T) {
	f := newFakeLinode()
	// state has no recorded bootstrap node; the instance is found by label+tag.
	f.seedInstance(2002, MachineName("demo"), []string{clusterTag("demo")})
	p := newTestProvider(f, baseCfg())
	bundle := []*unstructured.Unstructured{machineTemplate("demo-control-plane")}
	st := &cluster.State{Spec: cluster.Spec{Name: "demo", Namespace: "default"}}
	out, err := p.Adopt(context.Background(), st, bundle)
	if err != nil {
		t.Fatalf("Adopt: %v", err)
	}
	if !strings.Contains(string(out), "providerID: "+providerID(2002)) {
		t.Errorf("adopt output did not re-find the provider ID by label:\n%s", out)
	}
}

func TestMachineNameMatchesBootstrapConvention(t *testing.T) {
	if MachineName("demo") != "demo-bootstrap" {
		t.Fatalf("MachineName = %q, want demo-bootstrap", MachineName("demo"))
	}
}

func TestPlanAndTeardown(t *testing.T) {
	f := newFakeLinode()
	p := newTestProvider(f, baseCfg())
	f.seedInstance(1001, MachineName("demo"), []string{clusterTag("demo")})
	f.seedInstance(1002, "demo-md-0-abc", []string{clusterTag("demo")})
	f.seedInstance(9999, "unrelated", []string{clusterTag("other")})

	st := &cluster.State{Spec: cluster.Spec{Name: "demo"}}
	plan, err := p.Plan(context.Background(), st)
	if err != nil {
		t.Fatalf("Plan: %v", err)
	}
	if len(plan.Resources) != 2 {
		t.Fatalf("plan should list the two tagged instances, got %+v", plan.Resources)
	}
	for _, r := range plan.Resources {
		if r.Kind != ResourceKindInstance {
			t.Errorf("unexpected resource kind %q", r.Kind)
		}
	}
	if err := p.Teardown(context.Background(), st, plan); err != nil {
		t.Fatalf("Teardown: %v", err)
	}
	if len(f.deleted) != 2 {
		t.Errorf("deleted = %v, want the two tagged instances", f.deleted)
	}
	// The unrelated instance is untouched.
	if _, ok := f.instances[9999]; !ok {
		t.Error("Teardown deleted an instance belonging to another cluster")
	}
}

func TestTeardownIgnoresAlreadyGone(t *testing.T) {
	f := newFakeLinode()
	p := newTestProvider(f, baseCfg())
	plan := &infrastructure.Plan{Resources: []cluster.Resource{{Kind: ResourceKindInstance, ID: "1234"}}}
	if err := p.Teardown(context.Background(), &cluster.State{Spec: cluster.Spec{Name: "demo"}}, plan); err != nil {
		t.Fatalf("Teardown of a missing instance should not error: %v", err)
	}
}

func TestDiscover(t *testing.T) {
	f := newFakeLinode()
	p := newTestProvider(f, baseCfg())
	inv, err := p.Discover(context.Background())
	if err != nil {
		t.Fatalf("Discover: %v", err)
	}
	if len(inv["region"]) != 1 || len(inv["type"]) != 1 || len(inv["image"]) != 1 {
		t.Errorf("inventory = %+v", inv)
	}
}

func TestDiscoverFiltersNonMetadataRegionsAndNonCloudInitImages(t *testing.T) {
	f := newFakeLinode()
	f.regions = append(f.regions, linodego.Region{ID: "xx-old", Capabilities: []string{"Linodes"}})
	f.images = append(f.images, linodego.Image{ID: "linode/noci", Capabilities: nil})
	p := newTestProvider(f, baseCfg())
	inv, err := p.Discover(context.Background())
	if err != nil {
		t.Fatalf("Discover: %v", err)
	}
	if len(inv["region"]) != 1 {
		t.Errorf("non-Metadata region was not filtered out: %+v", inv["region"])
	}
	if len(inv["image"]) != 1 {
		t.Errorf("non-cloud-init image was not filtered out: %+v", inv["image"])
	}
}

func TestCheckFlagsMissingSSHKeyAndNodeBalancer(t *testing.T) {
	p := newTestProvider(newFakeLinode(), Config{Region: "us-ord"})
	bundle := []*unstructured.Unstructured{
		linodeCluster("demo", "NodeBalancer"),
		linodeMachineTemplate("demo-control-plane"), // no authorizedKeys
	}
	findings := p.Check(bundle)
	var ssh, nb bool
	for _, f := range findings {
		if strings.Contains(f.Field, "authorizedKeys") {
			ssh = true
		}
		if strings.Contains(f.Field, "loadBalancerType") {
			nb = true
		}
	}
	if !ssh {
		t.Errorf("expected an authorizedKeys finding, got %+v", findings)
	}
	if !nb {
		t.Errorf("expected a loadBalancerType=NodeBalancer finding, got %+v", findings)
	}
}

func TestCheckNoFindingsForExternalLBWithKey(t *testing.T) {
	p := newTestProvider(newFakeLinode(), Config{Region: "us-ord", AuthorizedKeys: []string{"ssh-ed25519 AAAA"}})
	bundle := []*unstructured.Unstructured{linodeCluster("demo", "external")}
	if findings := p.Check(bundle); len(findings) != 0 {
		t.Errorf("expected no findings for external LB with a configured key, got %+v", findings)
	}
}

// --- helpers ---

func machineTemplate(name string) *unstructured.Unstructured {
	u := &unstructured.Unstructured{Object: map[string]any{
		"spec": map[string]any{"template": map[string]any{"spec": map[string]any{"type": "g6-standard-2"}}},
	}}
	u.SetAPIVersion("infrastructure.cluster.x-k8s.io/v1alpha2")
	u.SetKind("LinodeMachineTemplate")
	u.SetName(name)
	u.SetNamespace("default")
	return u
}

func linodeMachineTemplate(name string) *unstructured.Unstructured { return machineTemplate(name) }

func kubeadmControlPlane(name, infraRef string) *unstructured.Unstructured {
	u := &unstructured.Unstructured{Object: map[string]any{
		"spec": map[string]any{"machineTemplate": map[string]any{"spec": map[string]any{
			"infrastructureRef": map[string]any{"name": infraRef, "kind": "LinodeMachineTemplate"},
		}}},
	}}
	u.SetAPIVersion("controlplane.cluster.x-k8s.io/v1beta2")
	u.SetKind("KubeadmControlPlane")
	u.SetName(name)
	u.SetNamespace("default")
	return u
}

func linodeCluster(name, lbType string) *unstructured.Unstructured {
	u := &unstructured.Unstructured{Object: map[string]any{
		"spec": map[string]any{"network": map[string]any{"loadBalancerType": lbType}},
	}}
	u.SetAPIVersion("infrastructure.cluster.x-k8s.io/v1alpha2")
	u.SetKind("LinodeCluster")
	u.SetName(name)
	u.SetNamespace("default")
	return u
}
