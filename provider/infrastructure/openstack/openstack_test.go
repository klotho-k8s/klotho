package openstack

import (
	"context"
	"encoding/base64"
	"strings"
	"testing"
	"time"

	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"

	"github.com/klotho-k8s/klotho/pkg/cluster"
	"github.com/klotho-k8s/klotho/provider/infrastructure"
)

func newTestProvider(f *fakeClient, cfg Config) *Provider {
	p := New(f, cfg)
	p.launchPoll = time.Millisecond
	p.launchWait = time.Second
	return p
}

func baseCfg() Config {
	return Config{
		Cloud:           "mycloud",
		Network:         "k8s-net",
		ExternalNetwork: "public",
		Flavor:          "m1.medium",
		Image:           "ubuntu-22.04",
		SSHKeyName:      "mykey",
	}
}

func TestPrepareAllocatesFloatingIPAndReturnsVariables(t *testing.T) {
	f := newFakeClient()
	cfg := baseCfg()
	cfg.CloudsYAMLData = []byte("clouds:\n  mycloud: {}\n")
	p := newTestProvider(f, cfg)

	prepared, err := p.Prepare(context.Background(), &cluster.Spec{Name: "demo"})
	if err != nil {
		t.Fatalf("Prepare: %v", err)
	}
	if prepared.Endpoint.Host != f.fipAddr || prepared.Endpoint.Port != DefaultPort {
		t.Fatalf("endpoint = %+v, want the allocated floating IP on the default port", prepared.Endpoint)
	}
	if len(prepared.Created) != 1 || prepared.Created[0].Kind != ResourceKindFloatingIP {
		t.Fatalf("created = %+v, want one floating IP", prepared.Created)
	}
	if p.fipID == "" {
		t.Error("Prepare did not record the allocated floating IP ID for Launch to associate")
	}
	for _, k := range []string{VarCloud, VarNetworkID, VarExternalNetworkID, VarControlPlaneFloatingIP, VarImageName, VarControlPlaneFlavor, VarNodeFlavor, VarControlPlaneCount} {
		if prepared.Variables[k] == "" {
			t.Errorf("variable %s not set: %+v", k, prepared.Variables)
		}
	}
	if prepared.Variables[VarControlPlaneFloatingIP] != f.fipAddr {
		t.Errorf("%s = %q, want the floating IP address", VarControlPlaneFloatingIP, prepared.Variables[VarControlPlaneFloatingIP])
	}
	wantB64 := base64.StdEncoding.EncodeToString(cfg.CloudsYAMLData)
	if prepared.Variables[VarCloudsYAMLB64] != wantB64 {
		t.Errorf("%s = %q, want base64 clouds.yaml", VarCloudsYAMLB64, prepared.Variables[VarCloudsYAMLB64])
	}
}

func TestPrepareUsesSuppliedEndpoint(t *testing.T) {
	f := newFakeClient()
	cfg := baseCfg()
	cfg.ControlPlaneEndpoint = "api.example.com"
	p := newTestProvider(f, cfg)

	prepared, err := p.Prepare(context.Background(), &cluster.Spec{Name: "demo"})
	if err != nil {
		t.Fatalf("Prepare: %v", err)
	}
	if prepared.Endpoint.Host != "api.example.com" {
		t.Errorf("endpoint = %q", prepared.Endpoint.Host)
	}
	if len(prepared.Created) != 0 || len(f.allocated) != 0 {
		t.Errorf("a supplied endpoint must allocate no floating IP, created=%+v allocated=%+v", prepared.Created, f.allocated)
	}
	if p.fipID != "" {
		t.Error("a supplied endpoint must leave fipID empty so Launch associates nothing")
	}
}

func TestPrepareValidation(t *testing.T) {
	tests := []struct {
		name string
		mut  func(*Config, *fakeClient)
	}{
		{"no cloud", func(c *Config, _ *fakeClient) { c.Cloud = "" }},
		{"no network", func(c *Config, _ *fakeClient) { c.Network = "" }},
		{"no flavor", func(c *Config, _ *fakeClient) { c.Flavor = "" }},
		{"no image", func(c *Config, _ *fakeClient) { c.Image = "" }},
		{"no external network", func(c *Config, _ *fakeClient) { c.ExternalNetwork = "" }},
		{"missing network", func(_ *Config, f *fakeClient) { f.noNetwork = true }},
		{"missing flavor", func(_ *Config, f *fakeClient) { f.missingFlavor = true }},
		{"missing image", func(_ *Config, f *fakeClient) { f.missingImage = true }},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			cfg := baseCfg()
			f := newFakeClient()
			tt.mut(&cfg, f)
			p := newTestProvider(f, cfg)
			if _, err := p.Prepare(context.Background(), &cluster.Spec{Name: "demo"}); err == nil {
				t.Errorf("expected an error for %s", tt.name)
			}
		})
	}
}

func TestLaunchNamesTagsAndAssociatesFloatingIP(t *testing.T) {
	f := newFakeClient()
	cfg := baseCfg()
	p := newTestProvider(f, cfg)
	spec := &cluster.Spec{Name: "demo", Variables: map[string]string{
		VarNetworkID:          "net-k8s-net",
		VarControlPlaneFlavor: "m1.medium",
		VarImageName:          "ubuntu-22.04",
	}}
	// Simulate Prepare having allocated a floating IP.
	f.seedFIP("fip-1", "203.0.113.20")
	p.fipID = "fip-1"

	node, err := p.Launch(context.Background(), spec, []byte("#cloud-config\n"))
	if err != nil {
		t.Fatalf("Launch: %v", err)
	}
	// The server name must equal the adopted OpenStackMachine name, so CAPO's
	// name-keyed lookup adopts rather than duplicates.
	if f.created.Name != MachineName("demo") {
		t.Errorf("server name = %q, want %q", f.created.Name, MachineName("demo"))
	}
	if !hasClusterTag(f.created.Tags, "demo") {
		t.Errorf("server missing ownership tag: %v", f.created.Tags)
	}
	if string(f.created.UserData) != "#cloud-config\n" {
		t.Errorf("user-data = %q", string(f.created.UserData))
	}
	if f.created.SSHKeyName != "mykey" {
		t.Errorf("sshKeyName = %q", f.created.SSHKeyName)
	}
	// Empty-region provider ID, matching the template's kubelet provider-id.
	if !strings.HasPrefix(node.ID, "openstack:///") {
		t.Errorf("node ID = %q, want empty-region openstack:/// form", node.ID)
	}
	if node.Name != MachineName("demo") {
		t.Errorf("node Name = %q", node.Name)
	}
	// The allocated floating IP was associated with node zero.
	if f.associations["fip-1"] != f.created.Name && len(f.associations) == 0 {
		t.Errorf("floating IP was not associated with the server: %+v", f.associations)
	}
	if len(f.associations) != 1 {
		t.Errorf("expected exactly one floating IP association, got %+v", f.associations)
	}
}

func TestLaunchNoFloatingIPWhenEndpointSupplied(t *testing.T) {
	f := newFakeClient()
	cfg := baseCfg()
	cfg.ControlPlaneEndpoint = "10.1.2.3"
	p := newTestProvider(f, cfg)
	spec := &cluster.Spec{Name: "demo", Variables: map[string]string{VarNetworkID: "net-k8s-net"}}

	node, err := p.Launch(context.Background(), spec, []byte("x"))
	if err != nil {
		t.Fatalf("Launch: %v", err)
	}
	if len(f.associations) != 0 {
		t.Errorf("no floating IP should be associated for a supplied endpoint, got %+v", f.associations)
	}
	var found bool
	for _, a := range node.Addresses {
		if a == "10.1.2.3" {
			found = true
		}
	}
	if !found {
		t.Errorf("supplied endpoint not recorded in node addresses: %v", node.Addresses)
	}
}

func TestAdoptRendersOpenStackMachineWithoutProviderID(t *testing.T) {
	f := newFakeClient()
	p := newTestProvider(f, baseCfg())
	bundle := []*unstructured.Unstructured{
		osMachineTemplate("demo-control-plane"),
		kubeadmControlPlane("demo-control-plane", "demo-control-plane"),
	}
	st := &cluster.State{
		Spec:      cluster.Spec{Name: "demo", Namespace: "default"},
		Bootstrap: &cluster.Node{ID: providerID("", "uuid-1"), Name: MachineName("demo")},
	}
	out, err := p.Adopt(context.Background(), st, bundle)
	if err != nil {
		t.Fatalf("Adopt: %v", err)
	}
	s := string(out)
	if !strings.Contains(s, "kind: OpenStackMachine") {
		t.Errorf("adopt output is not an OpenStackMachine:\n%s", s)
	}
	// CAPO adopts by name and sets providerID itself; klotho must NOT pre-set it.
	if strings.Contains(s, "providerID") {
		t.Errorf("adopt output must not set providerID (CAPO adopts by name):\n%s", s)
	}
	if !strings.Contains(s, "name: demo-bootstrap") {
		t.Errorf("adopt output has the wrong machine name (want demo-bootstrap):\n%s", s)
	}
	if !strings.Contains(s, "cloned-from-name: demo-control-plane") {
		t.Errorf("adopt output missing cloned-from annotation:\n%s", s)
	}
}

func TestMachineNameAndTag(t *testing.T) {
	if MachineName("demo") != "demo-bootstrap" {
		t.Fatalf("MachineName = %q, want demo-bootstrap", MachineName("demo"))
	}
	if Tag("Demo.Cluster_1") != "demo.cluster_1" {
		t.Fatalf("Tag = %q", Tag("Demo.Cluster_1"))
	}
	if clusterTag("demo") != "klotho-io-cluster=demo" {
		t.Fatalf("clusterTag = %q", clusterTag("demo"))
	}
	if CloudConfigSecretName("demo") != "demo-cloud-config" {
		t.Fatalf("CloudConfigSecretName = %q", CloudConfigSecretName("demo"))
	}
}

func TestProviderID(t *testing.T) {
	if got := providerID("", "uuid-1"); got != "openstack:///uuid-1" {
		t.Fatalf("providerID empty region = %q", got)
	}
	if got := providerID("RegionOne", "uuid-1"); got != "openstack://RegionOne/uuid-1" {
		t.Fatalf("providerID with region = %q", got)
	}
	uuid, ok := parseProviderID("openstack:///uuid-1")
	if !ok || uuid != "uuid-1" {
		t.Fatalf("parseProviderID empty region = %q %v", uuid, ok)
	}
	uuid, ok = parseProviderID("openstack://RegionOne/uuid-2")
	if !ok || uuid != "uuid-2" {
		t.Fatalf("parseProviderID with region = %q %v", uuid, ok)
	}
	if _, ok := parseProviderID("gce://p/z/n"); ok {
		t.Fatal("parseProviderID accepted a non-openstack id")
	}
}

func TestPlanAndTeardown(t *testing.T) {
	f := newFakeClient()
	p := newTestProvider(f, baseCfg())
	f.seedServer("srv-1", MachineName("demo"), []string{clusterTag("demo")})
	f.seedServer("srv-2", "demo-md-0-abc", []string{clusterTag("demo")})
	f.seedServer("srv-x", "unrelated", []string{clusterTag("other")})
	f.seedFIP("fip-1", "203.0.113.20")

	st := &cluster.State{
		Spec:      cluster.Spec{Name: "demo"},
		Resources: []cluster.Resource{{Kind: ResourceKindFloatingIP, ID: "fip-1", Name: "203.0.113.20"}},
	}
	plan, err := p.Plan(context.Background(), st)
	if err != nil {
		t.Fatalf("Plan: %v", err)
	}
	var servers, fips int
	for _, r := range plan.Resources {
		switch r.Kind {
		case ResourceKindServer:
			servers++
		case ResourceKindFloatingIP:
			fips++
		}
	}
	if servers != 2 {
		t.Errorf("plan should list the two tagged servers, got %+v", plan.Resources)
	}
	if fips != 1 {
		t.Errorf("plan should list the allocated floating IP, got %+v", plan.Resources)
	}

	if err := p.Teardown(context.Background(), st, plan); err != nil {
		t.Fatalf("Teardown: %v", err)
	}
	if len(f.deleted) != 2 {
		t.Errorf("deleted servers = %v, want the two tagged ones", f.deleted)
	}
	if len(f.delFIPs) != 1 {
		t.Errorf("released floating IPs = %v, want the allocated one", f.delFIPs)
	}
	if _, ok := f.servers["srv-x"]; !ok {
		t.Error("Teardown deleted a server belonging to another cluster")
	}
}

func TestTeardownIgnoresAlreadyGone(t *testing.T) {
	f := newFakeClient()
	p := newTestProvider(f, baseCfg())
	plan := &infrastructure.Plan{Resources: []cluster.Resource{
		{Kind: ResourceKindServer, ID: "missing"},
		{Kind: ResourceKindFloatingIP, ID: "missing-fip"},
	}}
	if err := p.Teardown(context.Background(), &cluster.State{Spec: cluster.Spec{Name: "demo"}}, plan); err != nil {
		t.Fatalf("Teardown of missing resources should not error: %v", err)
	}
}

func TestCheckFlagsEndpointAndMissingKey(t *testing.T) {
	cfg := baseCfg()
	cfg.SSHKeyName = ""
	p := newTestProvider(newFakeClient(), cfg)
	bundle := []*unstructured.Unstructured{osCluster("demo")}
	findings := p.Check(bundle)
	var key, endpoint bool
	for _, fnd := range findings {
		if strings.Contains(fnd.Field, "sshKeyName") {
			key = true
		}
		if strings.Contains(fnd.Field, "floatingIP") {
			endpoint = true
		}
	}
	if !key {
		t.Errorf("expected a missing-keypair finding, got %+v", findings)
	}
	if !endpoint {
		t.Errorf("expected a floating-IP endpoint finding, got %+v", findings)
	}
}

func TestCheckEndpointFindingAbsentWhenSupplied(t *testing.T) {
	cfg := baseCfg()
	cfg.ControlPlaneEndpoint = "api.example.com"
	p := newTestProvider(newFakeClient(), cfg)
	findings := p.Check([]*unstructured.Unstructured{osCluster("demo")})
	for _, fnd := range findings {
		if strings.Contains(fnd.Field, "floatingIP") {
			t.Errorf("no floating-IP finding expected with a supplied endpoint, got %+v", findings)
		}
	}
}

func TestDiscover(t *testing.T) {
	p := newTestProvider(newFakeClient(), baseCfg())
	inv, err := p.Discover(context.Background())
	if err != nil {
		t.Fatalf("Discover: %v", err)
	}
	for _, k := range []string{"cloud", "network", "externalNetwork", "flavor", "image"} {
		if len(inv[k]) == 0 {
			t.Errorf("inventory missing %q: %+v", k, inv)
		}
	}
}

// --- helpers ---

func osMachineTemplate(name string) *unstructured.Unstructured {
	u := &unstructured.Unstructured{Object: map[string]any{
		"spec": map[string]any{"template": map[string]any{"spec": map[string]any{
			"flavor": map[string]any{"filter": map[string]any{"name": "m1.medium"}},
		}}},
	}}
	u.SetAPIVersion("infrastructure.cluster.x-k8s.io/v1beta2")
	u.SetKind("OpenStackMachineTemplate")
	u.SetName(name)
	u.SetNamespace("default")
	return u
}

func kubeadmControlPlane(name, infraRef string) *unstructured.Unstructured {
	u := &unstructured.Unstructured{Object: map[string]any{
		"spec": map[string]any{"machineTemplate": map[string]any{"spec": map[string]any{
			"infrastructureRef": map[string]any{"name": infraRef, "kind": "OpenStackMachineTemplate"},
		}}},
	}}
	u.SetAPIVersion("controlplane.cluster.x-k8s.io/v1beta2")
	u.SetKind("KubeadmControlPlane")
	u.SetName(name)
	u.SetNamespace("default")
	return u
}

func osCluster(name string) *unstructured.Unstructured {
	u := &unstructured.Unstructured{Object: map[string]any{
		"spec": map[string]any{"identityRef": map[string]any{"cloudName": "mycloud"}},
	}}
	u.SetAPIVersion("infrastructure.cluster.x-k8s.io/v1beta2")
	u.SetKind("OpenStackCluster")
	u.SetName(name)
	u.SetNamespace("default")
	return u
}
