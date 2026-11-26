package aws

import (
	"context"
	"encoding/base64"
	"strings"
	"testing"
	"time"

	ec2types "github.com/aws/aws-sdk-go-v2/service/ec2/types"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"

	"github.com/klotho-k8s/klotho/pkg/cluster"
)

func newTestProvider(f *fakeEC2, cfg Config) *Provider {
	p := New(f, cfg)
	p.launchPoll = time.Millisecond
	p.launchWait = time.Second
	return p
}

func baseCfg() Config {
	return Config{Region: "us-east-1", VPCID: "vpc-1", SubnetID: "subnet-1", AMIID: "ami-123", ControlPlaneEndpoint: "cp.example.com"}
}

func fakeWithNetwork() *fakeEC2 {
	f := newFakeEC2()
	f.vpcs = []ec2types.Vpc{{VpcId: ptr("vpc-1"), CidrBlock: ptr("10.0.0.0/16")}}
	f.subnets = []ec2types.Subnet{{SubnetId: ptr("subnet-1"), VpcId: ptr("vpc-1"), AvailabilityZone: ptr("us-east-1a"), CidrBlock: ptr("10.0.0.0/24")}}
	return f
}

func TestPrepareValidatesAndReturnsVariables(t *testing.T) {
	f := fakeWithNetwork()
	p := newTestProvider(f, baseCfg())
	prepared, err := p.Prepare(context.Background(), &cluster.Spec{Name: "demo"})
	if err != nil {
		t.Fatalf("Prepare: %v", err)
	}
	if prepared.Endpoint.Host != "cp.example.com" || prepared.Endpoint.Port != DefaultPort {
		t.Fatalf("endpoint = %+v", prepared.Endpoint)
	}
	for _, k := range []string{VarRegion, VarAMIID, VarVPCID, VarSubnetID, VarControlPlaneType, VarControlPlaneCount} {
		if prepared.Variables[k] == "" {
			t.Errorf("variable %s not set: %+v", k, prepared.Variables)
		}
	}
	if prepared.Variables[VarAMIID] != "ami-123" {
		t.Errorf("AMI = %q", prepared.Variables[VarAMIID])
	}
}

func TestPrepareRequiresNetwork(t *testing.T) {
	f := newFakeEC2()
	cfg := baseCfg()
	cfg.VPCID = ""
	p := newTestProvider(f, cfg)
	if _, err := p.Prepare(context.Background(), &cluster.Spec{Name: "demo"}); err == nil {
		t.Fatal("expected an error when vpcId is missing")
	}
}

func TestPrepareAllocatesElasticIP(t *testing.T) {
	f := fakeWithNetwork()
	cfg := baseCfg()
	cfg.ControlPlaneEndpoint = ""
	cfg.AllocateElasticIP = true
	p := newTestProvider(f, cfg)
	prepared, err := p.Prepare(context.Background(), &cluster.Spec{Name: "demo"})
	if err != nil {
		t.Fatalf("Prepare: %v", err)
	}
	if prepared.Endpoint.Host != "198.51.100.5" {
		t.Fatalf("endpoint host = %q, want the allocated EIP", prepared.Endpoint.Host)
	}
	if len(prepared.Created) != 1 || prepared.Created[0].Kind != "eip" {
		t.Fatalf("Created = %+v, want one eip", prepared.Created)
	}
	if !hasTag(f.addresses[0].Tags, ClusterTagKey, "demo") {
		t.Errorf("allocated EIP not tagged for the cluster: %s", tagKeys(f.addresses[0].Tags))
	}
}

func TestResolveAMINewest(t *testing.T) {
	f := fakeWithNetwork()
	f.images = []ec2types.Image{
		{ImageId: ptr("ami-old"), CreationDate: ptr("2023-01-01T00:00:00Z")},
		{ImageId: ptr("ami-new"), CreationDate: ptr("2024-06-01T00:00:00Z")},
	}
	cfg := baseCfg()
	cfg.AMIID = ""
	p := newTestProvider(f, cfg)
	ami, err := p.resolveAMI(context.Background())
	if err != nil {
		t.Fatalf("resolveAMI: %v", err)
	}
	if ami != "ami-new" {
		t.Errorf("resolveAMI = %q, want the newest", ami)
	}
}

func TestLaunchTagsAndUserData(t *testing.T) {
	f := fakeWithNetwork()
	cfg := baseCfg()
	cfg.SSHKeyName = "mykey"
	p := newTestProvider(f, cfg)
	spec := &cluster.Spec{Name: "demo", Variables: map[string]string{VarAMIID: "ami-123", VarSubnetID: "subnet-1"}}

	node, err := p.Launch(context.Background(), spec, []byte("#cloud-config\n"))
	if err != nil {
		t.Fatalf("Launch: %v", err)
	}
	if node.ID != providerID("us-east-1a", "i-0bootstrap") {
		t.Errorf("node ID = %q", node.ID)
	}
	if node.Name != MachineName("demo") {
		t.Errorf("node Name = %q", node.Name)
	}
	if len(node.Addresses) != 2 {
		t.Errorf("addresses = %v", node.Addresses)
	}
	// user-data is base64 of the cloud-init.
	dec, _ := base64.StdEncoding.DecodeString(awsString(f.runInput.UserData))
	if string(dec) != "#cloud-config\n" {
		t.Errorf("user-data = %q", dec)
	}
	// the instance carries the four adoption tags.
	var tags []ec2types.Tag
	for _, ts := range f.runInput.TagSpecifications {
		if ts.ResourceType == ec2types.ResourceTypeInstance {
			tags = ts.Tags
		}
	}
	if !hasTag(tags, ClusterTagKey, "demo") ||
		!hasTag(tags, capaClusterTagKey("demo"), capaOwnedValue) ||
		!hasTag(tags, capaRoleTagKey, controlPlaneRole) ||
		!hasTag(tags, nameTagKey, MachineName("demo")) {
		t.Errorf("instance missing adoption tags: %s", tagKeys(tags))
	}
	if awsString(f.runInput.KeyName) != "mykey" {
		t.Errorf("KeyName = %q", awsString(f.runInput.KeyName))
	}
}

func TestLaunchAssociatesElasticIP(t *testing.T) {
	f := fakeWithNetwork()
	cfg := baseCfg()
	cfg.ControlPlaneEndpoint = ""
	cfg.AllocateElasticIP = true
	p := newTestProvider(f, cfg)
	spec := &cluster.Spec{Name: "demo"}
	if _, err := p.Prepare(context.Background(), spec); err != nil {
		t.Fatalf("Prepare: %v", err)
	}
	spec.Variables = map[string]string{VarAMIID: "ami-123", VarSubnetID: "subnet-1"}
	if _, err := p.Launch(context.Background(), spec, []byte("x")); err != nil {
		t.Fatalf("Launch: %v", err)
	}
	if got := f.associated["eipalloc-001"]; got != "i-0bootstrap" {
		t.Errorf("EIP not associated with the instance: %v", f.associated)
	}
}

func TestPlanAndTeardown(t *testing.T) {
	f := fakeWithNetwork()
	p := newTestProvider(f, baseCfg())
	// one running tagged instance and one tagged EIP.
	f.instances["i-0bootstrap"] = &ec2types.Instance{
		InstanceId: ptr("i-0bootstrap"),
		State:      &ec2types.InstanceState{Name: ec2types.InstanceStateNameRunning},
		Tags:       []ec2types.Tag{{Key: ptr(ClusterTagKey), Value: ptr("demo")}, {Key: ptr(nameTagKey), Value: ptr(MachineName("demo"))}},
	}
	f.addresses = []ec2types.Address{{AllocationId: ptr("eipalloc-001"), PublicIp: ptr("198.51.100.5"), Tags: []ec2types.Tag{{Key: ptr(ClusterTagKey), Value: ptr("demo")}}}}

	st := &cluster.State{Spec: cluster.Spec{Name: "demo"}}
	plan, err := p.Plan(context.Background(), st)
	if err != nil {
		t.Fatalf("Plan: %v", err)
	}
	var inst, eip int
	for _, r := range plan.Resources {
		switch r.Kind {
		case ResourceKindInstance:
			inst++
		case ResourceKindEIP:
			eip++
		}
	}
	if inst != 1 || eip != 1 {
		t.Fatalf("plan = %+v", plan.Resources)
	}
	if err := p.Teardown(context.Background(), st, plan); err != nil {
		t.Fatalf("Teardown: %v", err)
	}
	if len(f.terminated) != 1 || f.terminated[0] != "i-0bootstrap" {
		t.Errorf("terminated = %v", f.terminated)
	}
	if len(f.released) != 1 || f.released[0] != "eipalloc-001" {
		t.Errorf("released = %v", f.released)
	}
}

func TestAdoptByTagNotProviderID(t *testing.T) {
	p := newTestProvider(newFakeEC2(), baseCfg())
	bundle := []*unstructured.Unstructured{
		machineTemplate("demo-control-plane"),
		kubeadmControlPlane("demo-control-plane", "demo-control-plane"),
	}
	st := &cluster.State{
		Spec:      cluster.Spec{Name: "demo", Namespace: "default"},
		Bootstrap: &cluster.Node{ID: "aws:///us-west-2d/i-0bootstrap", Name: MachineName("demo")},
	}
	out, err := p.Adopt(context.Background(), st, bundle)
	if err != nil {
		t.Fatalf("Adopt: %v", err)
	}
	s := string(out)
	if !strings.Contains(s, "kind: AWSMachine") {
		t.Errorf("adopt output is not an AWSMachine:\n%s", s)
	}
	// CAPA adopts by tag, so the AWSMachine must NOT carry a providerID.
	if strings.Contains(s, "providerID") {
		t.Errorf("adopt output should not set providerID (CAPA adopts by tag):\n%s", s)
	}
	// The AWSMachine is named <cluster>-bootstrap, matching the kubeadm
	// bootstrap Machine's infrastructureRef and the instance Name tag.
	if !strings.Contains(s, "name: demo-bootstrap") {
		t.Errorf("adopt output has the wrong machine name (want demo-bootstrap):\n%s", s)
	}
	if !strings.Contains(s, "cloned-from-name: demo-control-plane") {
		t.Errorf("adopt output missing cloned-from annotation:\n%s", s)
	}
}

func TestMachineNameMatchesBootstrapConvention(t *testing.T) {
	if MachineName("demo") != "demo-bootstrap" {
		t.Fatalf("MachineName = %q, want demo-bootstrap (kubeadm BootstrapMachineName)", MachineName("demo"))
	}
}

func TestLaunchRequestsSpotAndPublicIP(t *testing.T) {
	f := fakeWithNetwork()
	cfg := baseCfg()
	cfg.Spot = true
	cfg.PublicIP = true
	cfg.SecurityGroupIDs = []string{"sg-1"}
	p := newTestProvider(f, cfg)
	if _, err := p.Launch(context.Background(), &cluster.Spec{Name: "demo"}, []byte("x")); err != nil {
		t.Fatalf("Launch: %v", err)
	}
	if f.runInput.InstanceMarketOptions == nil || f.runInput.InstanceMarketOptions.MarketType != ec2types.MarketTypeSpot {
		t.Errorf("RunInstances did not request a spot instance: %+v", f.runInput.InstanceMarketOptions)
	}
	if len(f.runInput.NetworkInterfaces) != 1 || f.runInput.NetworkInterfaces[0].AssociatePublicIpAddress == nil || !*f.runInput.NetworkInterfaces[0].AssociatePublicIpAddress {
		t.Errorf("RunInstances did not request a public IP: %+v", f.runInput.NetworkInterfaces)
	}
	if f.runInput.SubnetId != nil {
		t.Errorf("SubnetId must be nil when using a network interface")
	}
}

func TestCheckFlagsMissingSSHKey(t *testing.T) {
	p := newTestProvider(newFakeEC2(), Config{Region: "us-east-1"})
	bundle := []*unstructured.Unstructured{
		awsCluster("demo", "cp.example.com"),
		machineTemplate("demo-control-plane"), // no sshKeyName
	}
	findings := p.Check(bundle)
	found := false
	for _, f := range findings {
		if strings.Contains(f.Field, "sshKeyName") {
			found = true
		}
	}
	if !found {
		t.Errorf("expected an sshKeyName finding, got %+v", findings)
	}
}

func TestDiscover(t *testing.T) {
	f := fakeWithNetwork()
	f.keyPairs = []ec2types.KeyPairInfo{{KeyPairId: ptr("key-1"), KeyName: ptr("mykey")}}
	p := newTestProvider(f, baseCfg())
	inv, err := p.Discover(context.Background())
	if err != nil {
		t.Fatalf("Discover: %v", err)
	}
	if len(inv["vpc"]) != 1 || len(inv["subnet"]) != 1 || len(inv["keypair"]) != 1 {
		t.Errorf("inventory = %+v", inv)
	}
}

// --- bundle object helpers ---

func machineTemplate(name string) *unstructured.Unstructured {
	u := &unstructured.Unstructured{Object: map[string]any{
		"spec": map[string]any{"template": map[string]any{"spec": map[string]any{"instanceType": "t3.large"}}},
	}}
	u.SetAPIVersion("infrastructure.cluster.x-k8s.io/v1beta2")
	u.SetKind("AWSMachineTemplate")
	u.SetName(name)
	u.SetNamespace("default")
	return u
}

func TestTagMachineTemplatesUsesAdditionalTags(t *testing.T) {
	p := newTestProvider(&fakeEC2{}, Config{Region: "us-west-2"})
	objs := []*unstructured.Unstructured{
		machineTemplate("awse2e-control-plane"),
		machineTemplate("awse2e-md-0"),
		awsCluster("awse2e", "1.2.3.4"),
	}
	if err := p.TagMachineTemplates(objs, "awse2e"); err != nil {
		t.Fatalf("TagMachineTemplates: %v", err)
	}
	for _, o := range objs[:2] {
		tags, found, err := unstructured.NestedStringMap(o.Object, "spec", "template", "spec", "additionalTags")
		if err != nil || !found {
			t.Fatalf("%s: additionalTags not set (found=%v err=%v)", o.GetName(), found, err)
		}
		if tags[ClusterTagKey] != "awse2e" {
			t.Errorf("%s: additionalTags[%s] = %q, want awse2e", o.GetName(), ClusterTagKey, tags[ClusterTagKey])
		}
		// The Proxmox-style string list must not be present; CAPA rejects it.
		if _, found, _ := unstructured.NestedStringSlice(o.Object, "spec", "template", "spec", "tags"); found {
			t.Errorf("%s: spec.template.spec.tags should not be set for AWS", o.GetName())
		}
	}
	// Non-MachineTemplate objects are untouched.
	if _, found, _ := unstructured.NestedStringMap(objs[2].Object, "spec", "template", "spec", "additionalTags"); found {
		t.Error("AWSCluster should not get additionalTags")
	}
}

func kubeadmControlPlane(name, infraRef string) *unstructured.Unstructured {
	u := &unstructured.Unstructured{Object: map[string]any{
		"spec": map[string]any{"machineTemplate": map[string]any{"spec": map[string]any{
			"infrastructureRef": map[string]any{"name": infraRef, "kind": "AWSMachineTemplate"},
		}}},
	}}
	u.SetAPIVersion("controlplane.cluster.x-k8s.io/v1beta2")
	u.SetKind("KubeadmControlPlane")
	u.SetName(name)
	u.SetNamespace("default")
	return u
}

func awsCluster(name, endpoint string) *unstructured.Unstructured {
	u := &unstructured.Unstructured{Object: map[string]any{
		"spec": map[string]any{"controlPlaneEndpoint": map[string]any{"host": endpoint, "port": int64(6443)}},
	}}
	u.SetAPIVersion("infrastructure.cluster.x-k8s.io/v1beta2")
	u.SetKind("AWSCluster")
	u.SetName(name)
	u.SetNamespace("default")
	return u
}
