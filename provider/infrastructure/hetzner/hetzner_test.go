package hetzner

import (
	"context"
	"strings"
	"testing"
	"time"

	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"

	"github.com/klotho-k8s/klotho/pkg/cluster"
	"github.com/klotho-k8s/klotho/provider/infrastructure"
)

func newTestProvider(f *fakeHetzner, cfg Config) *Provider {
	p := New(f, cfg)
	p.launchPoll = time.Millisecond
	p.launchWait = time.Second
	return p
}

func baseCfg() Config {
	return Config{Token: "tok", Location: "fsn1", SSHKeys: []string{"klotho"}, ControlPlaneEndpoint: "cp.example.com"}
}

func TestPrepareValidatesAndReturnsVariables(t *testing.T) {
	f := newFakeHetzner()
	p := newTestProvider(f, baseCfg())
	prepared, err := p.Prepare(context.Background(), &cluster.Spec{Name: "demo"})
	if err != nil {
		t.Fatalf("Prepare: %v", err)
	}
	if prepared.Endpoint.Host != "cp.example.com" || prepared.Endpoint.Port != DefaultPort {
		t.Fatalf("endpoint = %+v", prepared.Endpoint)
	}
	for _, k := range []string{VarLocation, VarControlPlaneType, VarNodeType, VarImage, VarSSHKey, VarControlPlaneCount} {
		if prepared.Variables[k] == "" {
			t.Errorf("variable %s not set: %+v", k, prepared.Variables)
		}
	}
	if prepared.Variables[VarControlPlaneType] != DefaultServerType {
		t.Errorf("control plane type = %q, want default %q", prepared.Variables[VarControlPlaneType], DefaultServerType)
	}
	if prepared.Variables[VarImage] != DefaultImage {
		t.Errorf("image = %q, want default %q", prepared.Variables[VarImage], DefaultImage)
	}
	if prepared.Variables[VarSSHKey] != "klotho" {
		t.Errorf("ssh key = %q, want klotho", prepared.Variables[VarSSHKey])
	}
	// The API token must never leak into the generated bundle's variables.
	for k, v := range prepared.Variables {
		if v == "tok" {
			t.Errorf("token leaked into template variable %s", k)
		}
	}
}

func TestPrepareRequiresLocation(t *testing.T) {
	cfg := baseCfg()
	cfg.Location = ""
	p := newTestProvider(newFakeHetzner(), cfg)
	if _, err := p.Prepare(context.Background(), &cluster.Spec{Name: "demo"}); err == nil {
		t.Fatal("expected an error when location is missing")
	}
}

func TestPrepareRequiresSSHKey(t *testing.T) {
	cfg := baseCfg()
	cfg.SSHKeys = nil
	p := newTestProvider(newFakeHetzner(), cfg)
	if _, err := p.Prepare(context.Background(), &cluster.Spec{Name: "demo"}); err == nil {
		t.Fatal("expected an error when no SSH key is configured")
	}
}

func TestPrepareRequiresEndpoint(t *testing.T) {
	cfg := baseCfg()
	cfg.ControlPlaneEndpoint = ""
	p := newTestProvider(newFakeHetzner(), cfg)
	if _, err := p.Prepare(context.Background(), &cluster.Spec{Name: "demo"}); err == nil {
		t.Fatal("expected an error when controlPlaneEndpoint is missing")
	}
}

func TestPrepareRejectsUnknownLocation(t *testing.T) {
	cfg := baseCfg()
	cfg.Location = "nowhere"
	p := newTestProvider(newFakeHetzner(), cfg)
	if _, err := p.Prepare(context.Background(), &cluster.Spec{Name: "demo"}); err == nil {
		t.Fatal("expected an error for an unknown location")
	}
}

func TestPrepareRejectsUnknownSSHKey(t *testing.T) {
	cfg := baseCfg()
	cfg.SSHKeys = []string{"missing"}
	p := newTestProvider(newFakeHetzner(), cfg)
	if _, err := p.Prepare(context.Background(), &cluster.Spec{Name: "demo"}); err == nil {
		t.Fatal("expected an error for an SSH key not present in the project")
	}
}

func TestLaunchLabelsNameAndUserData(t *testing.T) {
	f := newFakeHetzner()
	p := newTestProvider(f, baseCfg())
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
	if len(node.Addresses) == 0 {
		t.Errorf("addresses = %v", node.Addresses)
	}
	// The server name must equal the adopted HCloudMachine name, so CAPH's
	// name-based adoption takes it over rather than creating a duplicate.
	if f.createOpts.Name != MachineName("demo") {
		t.Errorf("server name = %q, want %q", f.createOpts.Name, MachineName("demo"))
	}
	// user-data is the cloud-init, delivered as the server's user_data verbatim.
	if f.createOpts.UserData != "#cloud-config\n" {
		t.Errorf("user-data = %q", f.createOpts.UserData)
	}
	// klotho's ownership label is present.
	if !hasClusterLabel(f.createOpts.Labels, "demo") {
		t.Errorf("server missing ownership label: %v", f.createOpts.Labels)
	}
	// The SSH key is attached.
	if len(f.createOpts.SSHKeys) != 1 || f.createOpts.SSHKeys[0].Name != "klotho" {
		t.Errorf("ssh keys = %v", f.createOpts.SSHKeys)
	}
}

func TestLaunchDefaultsTypeAndImage(t *testing.T) {
	f := newFakeHetzner()
	p := newTestProvider(f, baseCfg())
	if _, err := p.Launch(context.Background(), &cluster.Spec{Name: "demo"}, []byte("x")); err != nil {
		t.Fatalf("Launch: %v", err)
	}
	if f.createOpts.ServerType.Name != DefaultServerType {
		t.Errorf("server type = %q, want %q", f.createOpts.ServerType.Name, DefaultServerType)
	}
	if f.createOpts.Image.Name != DefaultImage {
		t.Errorf("image = %q, want %q", f.createOpts.Image.Name, DefaultImage)
	}
	if f.createOpts.Location.Name != "fsn1" {
		t.Errorf("location = %q, want fsn1", f.createOpts.Location.Name)
	}
}

func TestAdoptDoesNotSetProviderID(t *testing.T) {
	f := newFakeHetzner()
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
	if !strings.Contains(s, "kind: HCloudMachine") {
		t.Errorf("adopt output is not an HCloudMachine:\n%s", s)
	}
	// CAPH adopts by name; klotho must NOT pre-set providerID.
	if strings.Contains(s, "providerID") {
		t.Errorf("adopt output set providerID, but CAPH adopts by name:\n%s", s)
	}
	if !strings.Contains(s, "name: demo-bootstrap") {
		t.Errorf("adopt output has the wrong machine name (want demo-bootstrap):\n%s", s)
	}
	if !strings.Contains(s, "cloned-from-name: demo-control-plane") {
		t.Errorf("adopt output missing cloned-from annotation:\n%s", s)
	}
}

func TestMachineNameMatchesBootstrapConvention(t *testing.T) {
	if MachineName("demo") != "demo-bootstrap" {
		t.Fatalf("MachineName = %q, want demo-bootstrap", MachineName("demo"))
	}
}

func TestProviderID(t *testing.T) {
	if got := providerID(42); got != "hcloud://42" {
		t.Fatalf("providerID(42) = %q, want hcloud://42", got)
	}
}

func TestTagSanitizesLabelValue(t *testing.T) {
	// A value Hetzner would reject as a label is sanitised to a legal one.
	if got := Tag("Demo_Cluster!"); got != "Demo_Cluster" {
		t.Fatalf("Tag = %q, want Demo_Cluster", got)
	}
}

func TestPlanAndTeardown(t *testing.T) {
	f := newFakeHetzner()
	p := newTestProvider(f, baseCfg())
	f.seedServer(1001, MachineName("demo"), map[string]string{ClusterLabelKey: Tag("demo")})
	f.seedServer(1002, "demo-md-0-abc", map[string]string{ClusterLabelKey: Tag("demo")})
	f.seedServer(9999, "unrelated", map[string]string{ClusterLabelKey: Tag("other")})

	st := &cluster.State{Spec: cluster.Spec{Name: "demo"}}
	plan, err := p.Plan(context.Background(), st)
	if err != nil {
		t.Fatalf("Plan: %v", err)
	}
	if len(plan.Resources) != 2 {
		t.Fatalf("plan should list the two labelled servers, got %+v", plan.Resources)
	}
	for _, r := range plan.Resources {
		if r.Kind != ResourceKindServer {
			t.Errorf("unexpected resource kind %q", r.Kind)
		}
	}
	if err := p.Teardown(context.Background(), st, plan); err != nil {
		t.Fatalf("Teardown: %v", err)
	}
	if len(f.deleted) != 2 {
		t.Errorf("deleted = %v, want the two labelled servers", f.deleted)
	}
	// The unrelated server is untouched.
	if _, ok := f.servers[9999]; !ok {
		t.Error("Teardown deleted a server belonging to another cluster")
	}
}

func TestTeardownIgnoresAlreadyGone(t *testing.T) {
	f := newFakeHetzner()
	p := newTestProvider(f, baseCfg())
	plan := &infrastructure.Plan{Resources: []cluster.Resource{{Kind: ResourceKindServer, ID: "1234"}}}
	if err := p.Teardown(context.Background(), &cluster.State{Spec: cluster.Spec{Name: "demo"}}, plan); err != nil {
		t.Fatalf("Teardown of a missing server should not error: %v", err)
	}
}

func TestDiscover(t *testing.T) {
	f := newFakeHetzner()
	p := newTestProvider(f, baseCfg())
	inv, err := p.Discover(context.Background())
	if err != nil {
		t.Fatalf("Discover: %v", err)
	}
	if len(inv["location"]) != 1 || len(inv["type"]) != 1 || len(inv["image"]) != 1 {
		t.Errorf("inventory = %+v", inv)
	}
}

func TestCheckFlagsMissingSSHKeyAndLoadBalancer(t *testing.T) {
	p := newTestProvider(newFakeHetzner(), Config{Location: "fsn1"}) // no SSH keys
	bundle := []*unstructured.Unstructured{
		hetznerCluster("demo", true), // LB enabled
		hcloudMachineTemplate("demo-control-plane"),
	}
	findings := p.Check(bundle)
	var ssh, lb bool
	for _, f := range findings {
		if strings.Contains(f.Field, "sshKeys") {
			ssh = true
		}
		if strings.Contains(f.Field, "controlPlaneLoadBalancer") {
			lb = true
		}
	}
	if !ssh {
		t.Errorf("expected an sshKeys finding, got %+v", findings)
	}
	if !lb {
		t.Errorf("expected a controlPlaneLoadBalancer finding, got %+v", findings)
	}
}

func TestCheckNoFindingsForDisabledLBWithKey(t *testing.T) {
	p := newTestProvider(newFakeHetzner(), Config{Location: "fsn1", SSHKeys: []string{"klotho"}})
	bundle := []*unstructured.Unstructured{hetznerCluster("demo", false)}
	if findings := p.Check(bundle); len(findings) != 0 {
		t.Errorf("expected no findings for a disabled LB with a configured key, got %+v", findings)
	}
}

// --- helpers ---

func machineTemplate(name string) *unstructured.Unstructured {
	u := &unstructured.Unstructured{Object: map[string]any{
		"spec": map[string]any{"template": map[string]any{"spec": map[string]any{"type": "cpx21"}}},
	}}
	u.SetAPIVersion("infrastructure.cluster.x-k8s.io/v1beta1")
	u.SetKind("HCloudMachineTemplate")
	u.SetName(name)
	u.SetNamespace("default")
	return u
}

func hcloudMachineTemplate(name string) *unstructured.Unstructured { return machineTemplate(name) }

func kubeadmControlPlane(name, infraRef string) *unstructured.Unstructured {
	u := &unstructured.Unstructured{Object: map[string]any{
		"spec": map[string]any{"machineTemplate": map[string]any{"spec": map[string]any{
			"infrastructureRef": map[string]any{"name": infraRef, "kind": "HCloudMachineTemplate"},
		}}},
	}}
	u.SetAPIVersion("controlplane.cluster.x-k8s.io/v1beta2")
	u.SetKind("KubeadmControlPlane")
	u.SetName(name)
	u.SetNamespace("default")
	return u
}

func hetznerCluster(name string, lbEnabled bool) *unstructured.Unstructured {
	u := &unstructured.Unstructured{Object: map[string]any{
		"spec": map[string]any{"controlPlaneLoadBalancer": map[string]any{"enabled": lbEnabled}},
	}}
	u.SetAPIVersion("infrastructure.cluster.x-k8s.io/v1beta1")
	u.SetKind("HetznerCluster")
	u.SetName(name)
	u.SetNamespace("default")
	return u
}
