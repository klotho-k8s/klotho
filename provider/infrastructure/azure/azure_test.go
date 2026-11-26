package azure

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

func newTestProvider(f *fakeAzure, cfg Config) *Provider {
	p := New(f, cfg)
	p.launchPoll = time.Millisecond
	p.launchWait = time.Second
	return p
}

func baseCfg() Config {
	return Config{
		SubscriptionID: "sub-1",
		TenantID:       "tenant-1",
		ClientID:       "client-1",
		Location:       "eastus",
		ResourceGroup:  "rg-1",
		VNet:           "vnet-1",
		Subnet:         "subnet-1",
	}
}

func TestPrepareValidatesAndReturnsVariables(t *testing.T) {
	f := newFakeAzure()
	p := newTestProvider(f, baseCfg())
	prepared, err := p.Prepare(context.Background(), &cluster.Spec{Name: "demo"})
	if err != nil {
		t.Fatalf("Prepare: %v", err)
	}
	// Endpoint is the allocated public IP, port default.
	if prepared.Endpoint.Host != "20.0.0.5" || prepared.Endpoint.Port != DefaultPort {
		t.Fatalf("endpoint = %+v", prepared.Endpoint)
	}
	if len(prepared.Created) != 1 || prepared.Created[0].Kind != ResourceKindPublicIP {
		t.Fatalf("Created = %+v, want one public IP", prepared.Created)
	}
	for _, k := range []string{VarSubscriptionID, VarLocation, VarResourceGroup, VarVNet, VarSubnet, VarControlPlaneVMSize, VarControlPlaneCount, VarImagePublisher, VarIdentityName, VarClientSecretName} {
		if prepared.Variables[k] == "" {
			t.Errorf("variable %s not set: %+v", k, prepared.Variables)
		}
	}
	if prepared.Variables[VarImagePublisher] != DefaultImagePublisher {
		t.Errorf("image publisher = %q", prepared.Variables[VarImagePublisher])
	}
	if f.publicIPInput == nil || f.publicIPInput.Tags[ClusterTagKey] != "demo" {
		t.Errorf("public IP not tagged for the cluster: %+v", f.publicIPInput)
	}
}

func TestPrepareEncodesClientSecret(t *testing.T) {
	cfg := baseCfg()
	cfg.ClientSecret = "sp-password"
	p := newTestProvider(newFakeAzure(), cfg)
	prepared, err := p.Prepare(context.Background(), &cluster.Spec{Name: "demo"})
	if err != nil {
		t.Fatalf("Prepare: %v", err)
	}
	got := prepared.Variables[VarClientSecretB64]
	want := base64.StdEncoding.EncodeToString([]byte("sp-password"))
	if got != want {
		t.Errorf("%s = %q, want %q", VarClientSecretB64, got, want)
	}
	if prepared.Variables[VarClientID] != "client-1" || prepared.Variables[VarTenantID] != "tenant-1" {
		t.Errorf("identity variables not set: %+v", prepared.Variables)
	}
}

func TestPrepareRequiresNetwork(t *testing.T) {
	for _, field := range []string{"resourceGroup", "vnet", "subnet", "location", "subscription"} {
		cfg := baseCfg()
		switch field {
		case "resourceGroup":
			cfg.ResourceGroup = ""
		case "vnet":
			cfg.VNet = ""
		case "subnet":
			cfg.Subnet = ""
		case "location":
			cfg.Location = ""
		case "subscription":
			cfg.SubscriptionID = ""
		}
		p := newTestProvider(newFakeAzure(), cfg)
		if _, err := p.Prepare(context.Background(), &cluster.Spec{Name: "demo"}); err == nil {
			t.Errorf("expected an error when %s is missing", field)
		}
	}
}

func TestPrepareUsesConfiguredEndpoint(t *testing.T) {
	cfg := baseCfg()
	cfg.ControlPlaneEndpoint = "api.example.com"
	cfg.Port = 8443
	p := newTestProvider(newFakeAzure(), cfg)
	prepared, err := p.Prepare(context.Background(), &cluster.Spec{Name: "demo"})
	if err != nil {
		t.Fatalf("Prepare: %v", err)
	}
	if prepared.Endpoint.Host != "api.example.com" || prepared.Endpoint.Port != 8443 {
		t.Fatalf("endpoint = %+v, want the configured endpoint", prepared.Endpoint)
	}
	// A public IP is still allocated for node-zero reachability.
	if len(prepared.Created) != 1 {
		t.Errorf("Created = %+v, want the public IP still allocated", prepared.Created)
	}
}

func TestLaunchCreatesVMWithCustomDataAndTags(t *testing.T) {
	f := newFakeAzure()
	p := newTestProvider(f, baseCfg())
	spec := &cluster.Spec{Name: "demo"}
	if _, err := p.Prepare(context.Background(), spec); err != nil {
		t.Fatalf("Prepare: %v", err)
	}

	node, err := p.Launch(context.Background(), spec, []byte("#cloud-config\n"))
	if err != nil {
		t.Fatalf("Launch: %v", err)
	}
	wantID := providerID("/subscriptions/sub-1/resourceGroups/rg-1/providers/Microsoft.Compute/virtualMachines/demo-bootstrap")
	if node.ID != wantID {
		t.Errorf("node ID = %q, want %q", node.ID, wantID)
	}
	if node.Name != MachineName("demo") {
		t.Errorf("node Name = %q", node.Name)
	}
	if !contains(node.Addresses, "20.0.0.5") {
		t.Errorf("node addresses %v missing the public IP", node.Addresses)
	}
	// customData is the raw cloud-init (the client base64-encodes it, not the
	// provider).
	if customDataString(f.createVMInput) != "#cloud-config" {
		t.Errorf("customData = %q", customDataString(f.createVMInput))
	}
	// The VM carries klotho's ownership tag and is named for adoption.
	if f.createVMInput.Tags[ClusterTagKey] != "demo" {
		t.Errorf("VM missing ownership tag: %+v", f.createVMInput.Tags)
	}
	if f.createVMInput.Name != "demo-bootstrap" {
		t.Errorf("VM name = %q, want demo-bootstrap (adoption name)", f.createVMInput.Name)
	}
	if f.createVMInput.Size != DefaultVMSize {
		t.Errorf("VM size = %q, want %q", f.createVMInput.Size, DefaultVMSize)
	}
	// No SSH key configured, so a random admin password is set.
	if f.createVMInput.AdminPassword == "" || f.createVMInput.SSHPublicKey != "" {
		t.Errorf("expected a random admin password and no SSH key: pass=%q key=%q", f.createVMInput.AdminPassword, f.createVMInput.SSHPublicKey)
	}
	// The NIC associates the allocated public IP.
	if f.createNICInput == nil || f.createNICInput.PublicIPID == "" {
		t.Errorf("NIC did not associate the public IP: %+v", f.createNICInput)
	}
}

func TestLaunchWithSSHKeyAndSpot(t *testing.T) {
	f := newFakeAzure()
	cfg := baseCfg()
	cfg.SSHPublicKey = "ssh-ed25519 AAAA..."
	cfg.Spot = true
	p := newTestProvider(f, cfg)
	if _, err := p.Launch(context.Background(), &cluster.Spec{Name: "demo"}, []byte("x")); err != nil {
		t.Fatalf("Launch: %v", err)
	}
	if f.createVMInput.SSHPublicKey != "ssh-ed25519 AAAA..." {
		t.Errorf("SSH key not set: %q", f.createVMInput.SSHPublicKey)
	}
	if f.createVMInput.AdminPassword != "" {
		t.Errorf("admin password should be empty when an SSH key is set")
	}
	if !f.createVMInput.Spot {
		t.Errorf("Spot not requested")
	}
}

func TestAdoptSetsProviderID(t *testing.T) {
	f := newFakeAzure()
	p := newTestProvider(f, baseCfg())
	bundle := []*unstructured.Unstructured{
		machineTemplate("demo-control-plane"),
		kubeadmControlPlane("demo-control-plane", "demo-control-plane"),
	}
	st := &cluster.State{
		Spec:      cluster.Spec{Name: "demo", Namespace: "default"},
		Bootstrap: &cluster.Node{ID: providerID("/subscriptions/sub-1/resourceGroups/rg-1/providers/Microsoft.Compute/virtualMachines/demo-bootstrap"), Name: MachineName("demo")},
	}
	out, err := p.Adopt(context.Background(), st, bundle)
	if err != nil {
		t.Fatalf("Adopt: %v", err)
	}
	s := string(out)
	if !strings.Contains(s, "kind: AzureMachine") {
		t.Errorf("adopt output is not an AzureMachine:\n%s", s)
	}
	// CAPZ adopts by provider ID (the CAPL model), so it MUST be set.
	if !strings.Contains(s, "providerID: azure:///subscriptions/sub-1/") {
		t.Errorf("adopt output missing providerID:\n%s", s)
	}
	if !strings.Contains(s, "name: demo-bootstrap") {
		t.Errorf("adopt output has the wrong machine name (want demo-bootstrap):\n%s", s)
	}
	if !strings.Contains(s, "cloned-from-name: demo-control-plane") {
		t.Errorf("adopt output missing cloned-from annotation:\n%s", s)
	}
}

func TestAdoptRefindsProviderIDWhenStateMissing(t *testing.T) {
	f := newFakeAzure()
	f.vms["demo-bootstrap"] = &VM{
		Name:              "demo-bootstrap",
		ID:                "/subscriptions/sub-1/resourceGroups/rg-1/providers/Microsoft.Compute/virtualMachines/demo-bootstrap",
		ProvisioningState: "Succeeded",
	}
	p := newTestProvider(f, baseCfg())
	bundle := []*unstructured.Unstructured{machineTemplate("demo-control-plane")}
	st := &cluster.State{Spec: cluster.Spec{Name: "demo", Namespace: "default"}}
	out, err := p.Adopt(context.Background(), st, bundle)
	if err != nil {
		t.Fatalf("Adopt: %v", err)
	}
	if !strings.Contains(string(out), "providerID: azure:///subscriptions/sub-1/") {
		t.Errorf("adopt did not re-find the provider ID:\n%s", out)
	}
}

func TestMachineNameMatchesBootstrapConvention(t *testing.T) {
	if MachineName("demo") != "demo-bootstrap" {
		t.Fatalf("MachineName = %q, want demo-bootstrap (kubeadm BootstrapMachineName)", MachineName("demo"))
	}
}

func TestPlanAndTeardown(t *testing.T) {
	f := newFakeAzure()
	f.clusterResources = []cluster.Resource{
		{Kind: ResourceKindVM, ID: "demo-bootstrap", Name: "demo-bootstrap"},
		{Kind: ResourceKindNIC, ID: "demo-bootstrap-nic", Name: "demo-bootstrap-nic"},
		{Kind: ResourceKindPublicIP, ID: "demo-bootstrap-pip", Name: "demo-bootstrap-pip"},
	}
	p := newTestProvider(f, baseCfg())
	st := &cluster.State{Spec: cluster.Spec{Name: "demo"}}
	plan, err := p.Plan(context.Background(), st)
	if err != nil {
		t.Fatalf("Plan: %v", err)
	}
	if len(plan.Resources) != 3 {
		t.Fatalf("plan = %+v", plan.Resources)
	}
	if err := p.Teardown(context.Background(), st, plan); err != nil {
		t.Fatalf("Teardown: %v", err)
	}
	if len(f.deletedVMs) != 1 || f.deletedVMs[0] != "demo-bootstrap" {
		t.Errorf("deleted VMs = %v", f.deletedVMs)
	}
	if len(f.deletedNICs) != 1 || len(f.deletedPIPs) != 1 {
		t.Errorf("deleted NICs/PIPs = %v / %v", f.deletedNICs, f.deletedPIPs)
	}
}

func TestPlanIncludesStateResources(t *testing.T) {
	f := newFakeAzure()
	p := newTestProvider(f, baseCfg())
	st := &cluster.State{
		Spec:      cluster.Spec{Name: "demo"},
		Resources: []cluster.Resource{{Kind: ResourceKindPublicIP, ID: "demo-bootstrap-pip", Name: "20.0.0.5"}},
	}
	plan, err := p.Plan(context.Background(), st)
	if err != nil {
		t.Fatalf("Plan: %v", err)
	}
	if len(plan.Resources) != 1 || plan.Resources[0].ID != "demo-bootstrap-pip" {
		t.Fatalf("plan = %+v, want the state-recorded public IP", plan.Resources)
	}
}

func TestDiscover(t *testing.T) {
	f := newFakeAzure()
	f.vnets = []infrastructure.Item{{ID: "/vnet-1", Name: "vnet-1"}}
	f.subnets = []infrastructure.Item{{ID: "/subnet-1", Name: "subnet-1"}}
	p := newTestProvider(f, baseCfg())
	inv, err := p.Discover(context.Background())
	if err != nil {
		t.Fatalf("Discover: %v", err)
	}
	if len(inv["vnet"]) != 1 || len(inv["subnet"]) != 1 {
		t.Errorf("inventory = %+v", inv)
	}
	if inv["subnet"][0].Attrs["vnet"] != "vnet-1" {
		t.Errorf("subnet missing vnet attr: %+v", inv["subnet"][0])
	}
}

func TestCheckFindings(t *testing.T) {
	p := newTestProvider(newFakeAzure(), baseCfg())
	bundle := []*unstructured.Unstructured{
		azureCluster("demo", "", "Public"), // no endpoint, public LB
		machineTemplate("demo-control-plane"),
		// no AzureClusterIdentity
	}
	findings := p.Check(bundle)
	wantFields := map[string]bool{
		"spec.template.spec.sshPublicKey":   false,
		"spec.networkSpec.apiServerLB.type": false,
		"spec.identityRef":                  false,
	}
	for _, fnd := range findings {
		if _, ok := wantFields[fnd.Field]; ok {
			wantFields[fnd.Field] = true
		}
	}
	for field, found := range wantFields {
		if !found {
			t.Errorf("expected a finding on %s, got %+v", field, findings)
		}
	}
}

func TestConfigResolvers(t *testing.T) {
	p := New(newFakeAzure(), Config{})
	if p.vmSize() != DefaultVMSize {
		t.Errorf("vmSize default = %q", p.vmSize())
	}
	if p.adminUsername() != DefaultAdminUsername {
		t.Errorf("adminUsername default = %q", p.adminUsername())
	}
	if p.port() != DefaultPort {
		t.Errorf("port default = %d", p.port())
	}
	img := p.image()
	if img.Publisher != DefaultImagePublisher || img.Offer != DefaultImageOffer || img.SKU != DefaultImageSKU || img.Version != DefaultImageVersion {
		t.Errorf("image default = %+v", img)
	}

	custom := New(newFakeAzure(), Config{VMSize: "Standard_D2s_v5", Image: ImageRef{Publisher: "me"}})
	if custom.vmSize() != "Standard_D2s_v5" {
		t.Errorf("vmSize override = %q", custom.vmSize())
	}
	if custom.image().Publisher != "me" || custom.image().Offer != DefaultImageOffer {
		t.Errorf("image override = %+v", custom.image())
	}
}

// --- bundle object helpers ---

func machineTemplate(name string) *unstructured.Unstructured {
	u := &unstructured.Unstructured{Object: map[string]any{
		"spec": map[string]any{"template": map[string]any{"spec": map[string]any{"vmSize": "Standard_B2s"}}},
	}}
	u.SetAPIVersion("infrastructure.cluster.x-k8s.io/v1beta1")
	u.SetKind("AzureMachineTemplate")
	u.SetName(name)
	u.SetNamespace("default")
	return u
}

func kubeadmControlPlane(name, infraRef string) *unstructured.Unstructured {
	u := &unstructured.Unstructured{Object: map[string]any{
		"spec": map[string]any{"machineTemplate": map[string]any{"spec": map[string]any{
			"infrastructureRef": map[string]any{"name": infraRef, "kind": "AzureMachineTemplate"},
		}}},
	}}
	u.SetAPIVersion("controlplane.cluster.x-k8s.io/v1beta2")
	u.SetKind("KubeadmControlPlane")
	u.SetName(name)
	u.SetNamespace("default")
	return u
}

func azureCluster(name, endpoint, lbType string) *unstructured.Unstructured {
	spec := map[string]any{
		"networkSpec": map[string]any{"apiServerLB": map[string]any{"type": lbType}},
	}
	if endpoint != "" {
		spec["controlPlaneEndpoint"] = map[string]any{"host": endpoint, "port": int64(6443)}
	}
	u := &unstructured.Unstructured{Object: map[string]any{"spec": spec}}
	u.SetAPIVersion("infrastructure.cluster.x-k8s.io/v1beta1")
	u.SetKind("AzureCluster")
	u.SetName(name)
	u.SetNamespace("default")
	return u
}
