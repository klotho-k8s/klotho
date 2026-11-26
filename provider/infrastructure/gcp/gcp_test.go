package gcp

import (
	"context"
	"strings"
	"testing"
	"time"

	compute "google.golang.org/api/compute/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"

	"github.com/klotho-k8s/klotho/pkg/cluster"
	"github.com/klotho-k8s/klotho/provider/infrastructure"
)

func newTestProvider(f *fakeCompute, cfg Config) *Provider {
	p := New(f, cfg)
	p.launchPoll = time.Millisecond
	p.launchWait = time.Second
	return p
}

func baseCfg() Config {
	return Config{Project: "proj", Region: "us-central1", Zone: "us-central1-a", PublicIP: true}
}

func TestPrepareReservesAddressAndReturnsVariables(t *testing.T) {
	f := newFakeCompute()
	p := newTestProvider(f, baseCfg())
	prepared, err := p.Prepare(context.Background(), &cluster.Spec{Name: "demo"})
	if err != nil {
		t.Fatalf("Prepare: %v", err)
	}
	if prepared.Endpoint.Host == "" || prepared.Endpoint.Port != DefaultPort {
		t.Fatalf("endpoint = %+v", prepared.Endpoint)
	}
	// The reserved address is named the way CAPG's LB expects, and is recorded
	// so Teardown can release it.
	if _, ok := f.addresses[addressName("demo")]; !ok {
		t.Errorf("Prepare did not reserve %q: %+v", addressName("demo"), f.addresses)
	}
	if len(prepared.Created) != 1 || prepared.Created[0].Kind != ResourceKindAddress || prepared.Created[0].ID != addressName("demo") {
		t.Errorf("created = %+v", prepared.Created)
	}
	for _, k := range []string{VarProject, VarRegion, VarZone, VarNetwork, VarSubnet, VarControlPlaneType, VarNodeType, VarImage, VarControlPlaneCount} {
		if prepared.Variables[k] == "" {
			t.Errorf("variable %s not set: %+v", k, prepared.Variables)
		}
	}
	if prepared.Variables[VarControlPlaneType] != DefaultMachineType {
		t.Errorf("machine type = %q, want default %q", prepared.Variables[VarControlPlaneType], DefaultMachineType)
	}
}

func TestPrepareReusesExistingAddress(t *testing.T) {
	f := newFakeCompute()
	f.seedAddress(addressName("demo"), "34.8.8.8", clusterLabels("demo"))
	p := newTestProvider(f, baseCfg())
	prepared, err := p.Prepare(context.Background(), &cluster.Spec{Name: "demo"})
	if err != nil {
		t.Fatalf("Prepare: %v", err)
	}
	if prepared.Endpoint.Host != "34.8.8.8" {
		t.Errorf("endpoint = %q, want the pre-existing address", prepared.Endpoint.Host)
	}
	// Reusing an address is not a new creation to sweep.
	if len(prepared.Created) != 0 {
		t.Errorf("created = %+v, want none for a reused address", prepared.Created)
	}
}

func TestPrepareUsesSuppliedEndpoint(t *testing.T) {
	cfg := baseCfg()
	cfg.ControlPlaneEndpoint = "api.example.com"
	f := newFakeCompute()
	p := newTestProvider(f, cfg)
	prepared, err := p.Prepare(context.Background(), &cluster.Spec{Name: "demo"})
	if err != nil {
		t.Fatalf("Prepare: %v", err)
	}
	if prepared.Endpoint.Host != "api.example.com" {
		t.Errorf("endpoint = %q", prepared.Endpoint.Host)
	}
	if len(f.addresses) != 0 {
		t.Errorf("a supplied endpoint must reserve no address, got %+v", f.addresses)
	}
}

func TestPrepareRequiresProjectRegionZone(t *testing.T) {
	for _, mut := range []func(*Config){
		func(c *Config) { c.Project = "" },
		func(c *Config) { c.Region = "" },
		func(c *Config) { c.Zone = "" },
	} {
		cfg := baseCfg()
		mut(&cfg)
		p := newTestProvider(newFakeCompute(), cfg)
		if _, err := p.Prepare(context.Background(), &cluster.Spec{Name: "demo"}); err == nil {
			t.Errorf("expected an error for cfg %+v", cfg)
		}
	}
}

func TestPrepareRejectsMissingNetwork(t *testing.T) {
	f := newFakeCompute()
	f.noNetwork = true
	p := newTestProvider(f, baseCfg())
	if _, err := p.Prepare(context.Background(), &cluster.Spec{Name: "demo"}); err == nil {
		t.Fatal("expected an error when the network does not exist")
	}
}

func TestLaunchNamesLabelsAndUserData(t *testing.T) {
	f := newFakeCompute()
	cfg := baseCfg()
	cfg.AuthorizedKeys = []string{"ssh-ed25519 AAAA"}
	p := newTestProvider(f, cfg)
	spec := &cluster.Spec{Name: "demo"}

	node, err := p.Launch(context.Background(), spec, []byte("#cloud-config\n"))
	if err != nil {
		t.Fatalf("Launch: %v", err)
	}
	// The instance name must equal the adopted GCPMachine name, so CAPG's
	// name-keyed Get adopts rather than duplicates.
	if f.inserted.Name != MachineName("demo") {
		t.Errorf("instance name = %q, want %q", f.inserted.Name, MachineName("demo"))
	}
	if node.ID != providerID("proj", "us-central1-a", MachineName("demo")) {
		t.Errorf("node ID = %q", node.ID)
	}
	if node.Name != MachineName("demo") {
		t.Errorf("node Name = %q", node.Name)
	}
	if len(node.Addresses) != 2 {
		t.Errorf("addresses = %v", node.Addresses)
	}
	// klotho's ownership label is present.
	if !hasClusterLabel(f.inserted.Labels, "demo") {
		t.Errorf("instance missing ownership label: %v", f.inserted.Labels)
	}
	// cloud-init is delivered raw under the "user-data" metadata key CAPG reads.
	ud := metadataValue(f.inserted, metadataKeyUserData)
	if ud != "#cloud-config\n" {
		t.Errorf("user-data = %q", ud)
	}
	// SSH keys are installed for the login user via the "ssh-keys" metadata.
	ssh := metadataValue(f.inserted, metadataKeySSHKeys)
	if !strings.Contains(ssh, "klotho:ssh-ed25519 AAAA") {
		t.Errorf("ssh-keys metadata = %q", ssh)
	}
	// A public IP was requested.
	if len(f.inserted.NetworkInterfaces[0].AccessConfigs) == 0 {
		t.Error("no external IP requested despite PublicIP")
	}
}

func TestLaunchDefaultsAndPreemptible(t *testing.T) {
	f := newFakeCompute()
	cfg := baseCfg()
	cfg.Preemptible = true
	p := newTestProvider(f, cfg)
	if _, err := p.Launch(context.Background(), &cluster.Spec{Name: "demo"}, []byte("x")); err != nil {
		t.Fatalf("Launch: %v", err)
	}
	if !strings.Contains(f.inserted.MachineType, DefaultMachineType) {
		t.Errorf("machineType = %q, want default %q", f.inserted.MachineType, DefaultMachineType)
	}
	if f.inserted.Scheduling == nil || !f.inserted.Scheduling.Preemptible {
		t.Errorf("scheduling = %+v, want preemptible", f.inserted.Scheduling)
	}
	if !strings.Contains(f.inserted.Disks[0].InitializeParams.SourceImage, DefaultImageFamily) {
		t.Errorf("source image = %q, want the default family", f.inserted.Disks[0].InitializeParams.SourceImage)
	}
}

func TestLaunchNoPublicIP(t *testing.T) {
	f := newFakeCompute()
	cfg := baseCfg()
	cfg.PublicIP = false
	p := newTestProvider(f, cfg)
	if _, err := p.Launch(context.Background(), &cluster.Spec{Name: "demo"}, []byte("x")); err != nil {
		t.Fatalf("Launch: %v", err)
	}
	if len(f.inserted.NetworkInterfaces[0].AccessConfigs) != 0 {
		t.Errorf("an external IP was requested despite PublicIP=false")
	}
}

func TestAdoptRendersGCPMachineWithoutProviderID(t *testing.T) {
	f := newFakeCompute()
	p := newTestProvider(f, baseCfg())
	bundle := []*unstructured.Unstructured{
		gcpMachineTemplate("demo-control-plane"),
		kubeadmControlPlane("demo-control-plane", "demo-control-plane"),
	}
	st := &cluster.State{
		Spec:      cluster.Spec{Name: "demo", Namespace: "default"},
		Bootstrap: &cluster.Node{ID: providerID("proj", "us-central1-a", MachineName("demo")), Name: MachineName("demo")},
	}
	out, err := p.Adopt(context.Background(), st, bundle)
	if err != nil {
		t.Fatalf("Adopt: %v", err)
	}
	s := string(out)
	if !strings.Contains(s, "kind: GCPMachine") {
		t.Errorf("adopt output is not a GCPMachine:\n%s", s)
	}
	// CAPG adopts by name and sets providerID itself; klotho must NOT pre-set it.
	if strings.Contains(s, "providerID") {
		t.Errorf("adopt output must not set providerID (CAPG adopts by name):\n%s", s)
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

func TestAddressNameMatchesCAPG(t *testing.T) {
	if addressName("demo") != "demo-apiserver" {
		t.Fatalf("addressName = %q, want demo-apiserver", addressName("demo"))
	}
}

func TestPlanAndTeardown(t *testing.T) {
	f := newFakeCompute()
	p := newTestProvider(f, baseCfg())
	f.seedInstance(MachineName("demo"), "us-central1-a", clusterLabels("demo"))
	f.seedInstance("demo-md-0-abc", "us-central1-a", clusterLabels("demo"))
	f.seedInstance("unrelated", "us-central1-a", clusterLabels("other"))
	f.seedAddress(addressName("demo"), "34.1.2.3", clusterLabels("demo"))

	st := &cluster.State{
		Spec:      cluster.Spec{Name: "demo"},
		Resources: []cluster.Resource{{Kind: ResourceKindAddress, ID: addressName("demo"), Name: "34.1.2.3"}},
	}
	plan, err := p.Plan(context.Background(), st)
	if err != nil {
		t.Fatalf("Plan: %v", err)
	}
	var instCount, addrCount int
	for _, r := range plan.Resources {
		switch r.Kind {
		case ResourceKindInstance:
			instCount++
		case ResourceKindAddress:
			addrCount++
		}
	}
	if instCount != 2 {
		t.Errorf("plan should list the two tagged instances, got %+v", plan.Resources)
	}
	if addrCount != 1 {
		t.Errorf("plan should list the reserved address, got %+v", plan.Resources)
	}

	if err := p.Teardown(context.Background(), st, plan); err != nil {
		t.Fatalf("Teardown: %v", err)
	}
	if len(f.deleted) != 2 {
		t.Errorf("deleted instances = %v, want the two tagged ones", f.deleted)
	}
	if len(f.delAddrs) != 1 {
		t.Errorf("released addresses = %v, want the reserved one", f.delAddrs)
	}
	// The unrelated instance is untouched.
	if _, ok := f.instances["unrelated"]; !ok {
		t.Error("Teardown deleted an instance belonging to another cluster")
	}
}

func TestTeardownIgnoresAlreadyGone(t *testing.T) {
	f := newFakeCompute()
	p := newTestProvider(f, baseCfg())
	plan := &infrastructure.Plan{Resources: []cluster.Resource{
		{Kind: ResourceKindInstance, ID: "missing", Name: "us-central1-a"},
		{Kind: ResourceKindAddress, ID: "missing-apiserver"},
	}}
	if err := p.Teardown(context.Background(), &cluster.State{Spec: cluster.Spec{Name: "demo"}}, plan); err != nil {
		t.Fatalf("Teardown of missing resources should not error: %v", err)
	}
}

func TestDiscover(t *testing.T) {
	f := newFakeCompute()
	p := newTestProvider(f, baseCfg())
	inv, err := p.Discover(context.Background())
	if err != nil {
		t.Fatalf("Discover: %v", err)
	}
	for _, k := range []string{"project", "region", "zone", "machineType", "image"} {
		if len(inv[k]) == 0 {
			t.Errorf("inventory missing %q: %+v", k, inv)
		}
	}
}

func TestCheckFlagsLBAndMissingSSHKey(t *testing.T) {
	p := newTestProvider(newFakeCompute(), baseCfg())
	bundle := []*unstructured.Unstructured{
		gcpCluster("demo"),
		gcpMachineTemplatePreemptible("demo-control-plane"),
	}
	findings := p.Check(bundle)
	var lb, ssh, preempt bool
	for _, fnd := range findings {
		if strings.Contains(fnd.Field, "loadBalancer") {
			lb = true
		}
		if strings.Contains(fnd.Field, "authorizedKeys") {
			ssh = true
		}
		if strings.Contains(fnd.Field, "preemptible") {
			preempt = true
		}
	}
	if !lb {
		t.Errorf("expected a load-balancer cost finding, got %+v", findings)
	}
	if !ssh {
		t.Errorf("expected a missing-SSH-key finding, got %+v", findings)
	}
	if !preempt {
		t.Errorf("expected a preemptible finding, got %+v", findings)
	}
}

func TestCheckLBFindingAlwaysPresentWithKey(t *testing.T) {
	cfg := baseCfg()
	cfg.AuthorizedKeys = []string{"ssh-ed25519 AAAA"}
	p := newTestProvider(newFakeCompute(), cfg)
	bundle := []*unstructured.Unstructured{gcpCluster("demo")}
	findings := p.Check(bundle)
	if len(findings) != 1 || !strings.Contains(findings[0].Field, "loadBalancer") {
		t.Errorf("expected only the unavoidable load-balancer finding, got %+v", findings)
	}
}

func TestResolvers(t *testing.T) {
	p := newTestProvider(newFakeCompute(), Config{})
	if p.machineType() != DefaultMachineType {
		t.Errorf("machineType default = %q", p.machineType())
	}
	if p.network() != "default" || p.subnet() != "default" {
		t.Errorf("network/subnet defaults = %q/%q", p.network(), p.subnet())
	}
	if p.imageURL() != "projects/"+DefaultImageProject+"/global/images/family/"+DefaultImageFamily {
		t.Errorf("imageURL default = %q", p.imageURL())
	}
	p2 := newTestProvider(newFakeCompute(), Config{Image: "my-image", ImageProject: "my-proj"})
	if p2.imageURL() != "projects/my-proj/global/images/my-image" {
		t.Errorf("imageURL bare = %q", p2.imageURL())
	}
	p3 := newTestProvider(newFakeCompute(), Config{Image: "projects/x/global/images/y"})
	if p3.imageURL() != "projects/x/global/images/y" {
		t.Errorf("imageURL qualified = %q", p3.imageURL())
	}
}

func TestProviderID(t *testing.T) {
	if got := providerID("p", "z", "n"); got != "gce://p/z/n" {
		t.Fatalf("providerID = %q", got)
	}
	proj, zone, inst, ok := parseProviderID("gce://p/z/n")
	if !ok || proj != "p" || zone != "z" || inst != "n" {
		t.Fatalf("parseProviderID = %q %q %q %v", proj, zone, inst, ok)
	}
	if _, _, _, ok := parseProviderID("linode://1"); ok {
		t.Fatal("parseProviderID accepted a non-gce id")
	}
}

func TestLabelSanitize(t *testing.T) {
	if got := Tag("Demo.Cluster_1"); got != "demo-cluster_1" {
		t.Fatalf("Tag = %q", got)
	}
}

// --- helpers ---

func metadataValue(inst *compute.Instance, key string) string {
	if inst.Metadata == nil {
		return ""
	}
	for _, it := range inst.Metadata.Items {
		if it.Key == key && it.Value != nil {
			return *it.Value
		}
	}
	return ""
}

func gcpMachineTemplate(name string) *unstructured.Unstructured {
	u := &unstructured.Unstructured{Object: map[string]any{
		"spec": map[string]any{"template": map[string]any{"spec": map[string]any{"instanceType": "e2-medium"}}},
	}}
	u.SetAPIVersion("infrastructure.cluster.x-k8s.io/v1beta1")
	u.SetKind("GCPMachineTemplate")
	u.SetName(name)
	u.SetNamespace("default")
	return u
}

func gcpMachineTemplatePreemptible(name string) *unstructured.Unstructured {
	u := gcpMachineTemplate(name)
	_ = unstructured.SetNestedField(u.Object, true, "spec", "template", "spec", "preemptible")
	return u
}

func kubeadmControlPlane(name, infraRef string) *unstructured.Unstructured {
	u := &unstructured.Unstructured{Object: map[string]any{
		"spec": map[string]any{"machineTemplate": map[string]any{"spec": map[string]any{
			"infrastructureRef": map[string]any{"name": infraRef, "kind": "GCPMachineTemplate"},
		}}},
	}}
	u.SetAPIVersion("controlplane.cluster.x-k8s.io/v1beta2")
	u.SetKind("KubeadmControlPlane")
	u.SetName(name)
	u.SetNamespace("default")
	return u
}

func gcpCluster(name string) *unstructured.Unstructured {
	u := &unstructured.Unstructured{Object: map[string]any{
		"spec": map[string]any{"project": "proj", "region": "us-central1"},
	}}
	u.SetAPIVersion("infrastructure.cluster.x-k8s.io/v1beta1")
	u.SetKind("GCPCluster")
	u.SetName(name)
	u.SetNamespace("default")
	return u
}
