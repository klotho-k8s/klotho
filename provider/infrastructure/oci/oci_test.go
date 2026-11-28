package oci

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

func newTestProvider(f *fakeOCI, cfg Config) *Provider {
	p := New(f, cfg)
	p.launchPoll = time.Millisecond
	p.launchWait = time.Second
	return p
}

func baseCfg() Config {
	return Config{
		CompartmentID:        "ocid1.compartment.oc1..cmp",
		Region:               "us-ashburn-1",
		AvailabilityDomain:   "Uocm:US-ASHBURN-AD-1",
		VCNID:                "ocid1.vcn.oc1..vcn",
		SubnetID:             "ocid1.subnet.oc1..sub",
		ImageID:              "ocid1.image.oc1..ubuntu",
		ControlPlaneEndpoint: "203.0.113.10",
		AssignPublicIP:       true,
	}
}

func TestPrepareValidatesAndReturnsVariables(t *testing.T) {
	f := newFakeOCI()
	p := newTestProvider(f, baseCfg())
	prepared, err := p.Prepare(context.Background(), &cluster.Spec{Name: "demo"})
	if err != nil {
		t.Fatalf("Prepare: %v", err)
	}
	if prepared.Endpoint.Host != "203.0.113.10" || prepared.Endpoint.Port != DefaultPort {
		t.Fatalf("endpoint = %+v", prepared.Endpoint)
	}
	for _, k := range []string{VarCompartmentID, VarResourceID, VarVCNID, VarSubnetID, VarImageID, VarControlPlaneShape, VarNodeShape, VarOCPUs, VarMemoryGBs, VarControlPlaneCount} {
		if prepared.Variables[k] == "" {
			t.Errorf("variable %s not set: %+v", k, prepared.Variables)
		}
	}
	// ResourceIdentifier defaults to the cluster name (so the template's
	// ${CLUSTER_NAME} pin and the stamped tag agree).
	if prepared.Variables[VarResourceID] != "demo" {
		t.Errorf("resource identifier = %q, want demo", prepared.Variables[VarResourceID])
	}
	if prepared.Variables[VarControlPlaneShape] != DefaultShape {
		t.Errorf("shape = %q, want default %q", prepared.Variables[VarControlPlaneShape], DefaultShape)
	}
}

func TestPrepareRequiresFields(t *testing.T) {
	for _, tc := range []struct {
		name   string
		mutate func(*Config)
	}{
		{"compartment", func(c *Config) { c.CompartmentID = "" }},
		{"region", func(c *Config) { c.Region = "" }},
		{"availabilityDomain", func(c *Config) { c.AvailabilityDomain = "" }},
		{"vcn", func(c *Config) { c.VCNID = "" }},
		{"subnet", func(c *Config) { c.SubnetID = "" }},
		{"image", func(c *Config) { c.ImageID = "" }},
		{"endpoint", func(c *Config) { c.ControlPlaneEndpoint = "" }},
	} {
		t.Run(tc.name, func(t *testing.T) {
			cfg := baseCfg()
			tc.mutate(&cfg)
			p := newTestProvider(newFakeOCI(), cfg)
			if _, err := p.Prepare(context.Background(), &cluster.Spec{Name: "demo"}); err == nil {
				t.Fatalf("expected an error when %s is missing", tc.name)
			}
		})
	}
}

func TestPrepareRejectsUnknownAvailabilityDomain(t *testing.T) {
	cfg := baseCfg()
	cfg.AvailabilityDomain = "Uocm:US-ASHBURN-AD-9"
	p := newTestProvider(newFakeOCI(), cfg)
	if _, err := p.Prepare(context.Background(), &cluster.Spec{Name: "demo"}); err == nil {
		t.Fatal("expected an error for an availability domain not in the compartment")
	}
}

func TestLaunchTagsDisplayNameAndUserData(t *testing.T) {
	f := newFakeOCI()
	cfg := baseCfg()
	cfg.SSHAuthorizedKeys = []string{"ssh-ed25519 AAAA"}
	p := newTestProvider(f, cfg)
	spec := &cluster.Spec{Name: "demo"}

	node, err := p.Launch(context.Background(), spec, []byte("#cloud-config\n"))
	if err != nil {
		t.Fatalf("Launch: %v", err)
	}
	if !strings.HasPrefix(node.ID, ProviderIDPrefix) {
		t.Errorf("node ID = %q, want oci:// prefix", node.ID)
	}
	if node.Name != MachineName("demo") {
		t.Errorf("node Name = %q", node.Name)
	}
	if len(node.Addresses) != 2 {
		t.Errorf("addresses = %v", node.Addresses)
	}
	d := f.launchDetails
	if d == nil {
		t.Fatal("no launch details recorded")
	}
	// The display name must equal the adopted OCIMachine name, so CAPOCI's
	// display-name lookup adopts rather than duplicates.
	if strVal(d.DisplayName) != MachineName("demo") {
		t.Errorf("display name = %q, want %q", strVal(d.DisplayName), MachineName("demo"))
	}
	// user_data is base64 of the cloud-init, in the instance metadata.
	dec, _ := base64.StdEncoding.DecodeString(d.Metadata[metadataKeyUserData])
	if string(dec) != "#cloud-config\n" {
		t.Errorf("user_data = %q", dec)
	}
	if d.Metadata[metadataKeySSHKeys] != "ssh-ed25519 AAAA" {
		t.Errorf("ssh key metadata = %q", d.Metadata[metadataKeySSHKeys])
	}
	// CAPOCI's ownership tags are present with the pinned resource identifier,
	// and klotho's ownership tag is present.
	if d.FreeformTags[CreatedByTagKey] != CreatedByTagValue {
		t.Errorf("missing CreatedBy tag: %v", d.FreeformTags)
	}
	if d.FreeformTags[ClusterResourceIdentifierTagKey] != "demo" {
		t.Errorf("ClusterResourceIdentifier = %q, want demo", d.FreeformTags[ClusterResourceIdentifierTagKey])
	}
	if !hasClusterTag(d.FreeformTags, "demo") {
		t.Errorf("missing klotho ownership tag: %v", d.FreeformTags)
	}
	// A flexible shape carries an OCPU/memory configuration.
	if d.ShapeConfig == nil || d.ShapeConfig.Ocpus == nil || *d.ShapeConfig.Ocpus != DefaultOCPUs {
		t.Errorf("shape config = %+v, want %v OCPUs", d.ShapeConfig, DefaultOCPUs)
	}
}

func TestLaunchFixedShapeHasNoShapeConfig(t *testing.T) {
	f := newFakeOCI()
	cfg := baseCfg()
	cfg.Shape = "VM.Standard2.1" // a fixed (non-Flex) shape
	p := newTestProvider(f, cfg)
	if _, err := p.Launch(context.Background(), &cluster.Spec{Name: "demo"}, []byte("x")); err != nil {
		t.Fatalf("Launch: %v", err)
	}
	if f.launchDetails.ShapeConfig != nil {
		t.Errorf("fixed shape must not carry a shape config: %+v", f.launchDetails.ShapeConfig)
	}
}

func TestAdoptDoesNotSetProviderID(t *testing.T) {
	f := newFakeOCI()
	p := newTestProvider(f, baseCfg())
	bundle := []*unstructured.Unstructured{
		machineTemplate("demo-control-plane"),
		kubeadmControlPlane("demo-control-plane", "demo-control-plane"),
	}
	st := &cluster.State{
		Spec:      cluster.Spec{Name: "demo", Namespace: "default"},
		Bootstrap: &cluster.Node{ID: providerID("ocid1.instance.oc1..0001"), Name: MachineName("demo")},
	}
	out, err := p.Adopt(context.Background(), st, bundle)
	if err != nil {
		t.Fatalf("Adopt: %v", err)
	}
	s := string(out)
	if !strings.Contains(s, "kind: OCIMachine") {
		t.Errorf("adopt output is not an OCIMachine:\n%s", s)
	}
	// CAPOCI adopts by display name + tags, not a pre-set providerID; it MUST
	// NOT be present (the GCP/CAPD model).
	if strings.Contains(s, "providerID") {
		t.Errorf("adopt output must not set providerID (CAPOCI sets it):\n%s", s)
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

func TestProviderIDRoundTrip(t *testing.T) {
	id := providerID("ocid1.instance.oc1..abc")
	if id != "oci://ocid1.instance.oc1..abc" {
		t.Fatalf("providerID = %q", id)
	}
	ocid, ok := ocidFromProviderID(id)
	if !ok || ocid != "ocid1.instance.oc1..abc" {
		t.Fatalf("ocidFromProviderID(%q) = %q, %v", id, ocid, ok)
	}
	if _, ok := ocidFromProviderID("gce://x"); ok {
		t.Error("ocidFromProviderID accepted a non-oci provider ID")
	}
}

func TestPlanAndTeardown(t *testing.T) {
	f := newFakeOCI()
	p := newTestProvider(f, baseCfg())
	tags := p.clusterFreeformTags("demo")
	f.seedInstance("ocid1.instance.oc1..cp", MachineName("demo"), tags)
	f.seedInstance("ocid1.instance.oc1..md", "demo-md-0-abc", tags)
	f.seedInstance("ocid1.instance.oc1..other", "unrelated", map[string]string{ClusterTagKey: "other"})

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
	if len(f.terminated) != 2 {
		t.Errorf("terminated = %v, want the two tagged instances", f.terminated)
	}
	if _, ok := f.instances["ocid1.instance.oc1..other"]; !ok {
		t.Error("Teardown terminated an instance belonging to another cluster")
	}
}

func TestTeardownIgnoresAlreadyGone(t *testing.T) {
	f := newFakeOCI()
	p := newTestProvider(f, baseCfg())
	plan := &infrastructure.Plan{Resources: []cluster.Resource{{Kind: ResourceKindInstance, ID: "ocid1.instance.oc1..gone"}}}
	if err := p.Teardown(context.Background(), &cluster.State{Spec: cluster.Spec{Name: "demo"}}, plan); err != nil {
		t.Fatalf("Teardown of a missing instance should not error: %v", err)
	}
}

func TestFindBootstrapInstance(t *testing.T) {
	f := newFakeOCI()
	p := newTestProvider(f, baseCfg())
	f.seedInstance("ocid1.instance.oc1..cp", MachineName("demo"), p.clusterFreeformTags("demo"))
	inst, err := p.findBootstrapInstance(context.Background(), "demo")
	if err != nil {
		t.Fatalf("findBootstrapInstance: %v", err)
	}
	if strVal(inst.Id) != "ocid1.instance.oc1..cp" {
		t.Errorf("found instance = %q", strVal(inst.Id))
	}
}

func TestDiscover(t *testing.T) {
	f := newFakeOCI()
	p := newTestProvider(f, baseCfg())
	inv, err := p.Discover(context.Background())
	if err != nil {
		t.Fatalf("Discover: %v", err)
	}
	if len(inv["availabilityDomain"]) != 1 || len(inv["shape"]) != 1 || len(inv["image"]) != 1 {
		t.Errorf("inventory = %+v", inv)
	}
}

func TestCheckFlagsUnpinnedIdentifierAndMissingKey(t *testing.T) {
	p := newTestProvider(newFakeOCI(), Config{})
	bundle := []*unstructured.Unstructured{
		ociCluster("demo", ""), // no ociResourceIdentifier
		ociMachineTemplate("demo-control-plane"),
	}
	findings := p.Check(bundle)
	var id, ssh bool
	for _, f := range findings {
		if strings.Contains(f.Field, "ociResourceIdentifier") {
			id = true
		}
		if strings.Contains(f.Field, "ssh_authorized_keys") {
			ssh = true
		}
	}
	if !id {
		t.Errorf("expected an ociResourceIdentifier finding, got %+v", findings)
	}
	if !ssh {
		t.Errorf("expected an ssh key finding, got %+v", findings)
	}
}

func TestCheckNoFindingsWhenPinnedWithKey(t *testing.T) {
	p := newTestProvider(newFakeOCI(), Config{SSHAuthorizedKeys: []string{"ssh-ed25519 AAAA"}})
	bundle := []*unstructured.Unstructured{ociCluster("demo", "demo")}
	if findings := p.Check(bundle); len(findings) != 0 {
		t.Errorf("expected no findings for a pinned identifier with a configured key, got %+v", findings)
	}
}

// --- helpers ---

func machineTemplate(name string) *unstructured.Unstructured {
	u := &unstructured.Unstructured{Object: map[string]any{
		"spec": map[string]any{"template": map[string]any{"spec": map[string]any{"shape": "VM.Standard.E4.Flex"}}},
	}}
	u.SetAPIVersion("infrastructure.cluster.x-k8s.io/v1beta2")
	u.SetKind("OCIMachineTemplate")
	u.SetName(name)
	u.SetNamespace("default")
	return u
}

func ociMachineTemplate(name string) *unstructured.Unstructured { return machineTemplate(name) }

func kubeadmControlPlane(name, infraRef string) *unstructured.Unstructured {
	u := &unstructured.Unstructured{Object: map[string]any{
		"spec": map[string]any{"machineTemplate": map[string]any{"spec": map[string]any{
			"infrastructureRef": map[string]any{"name": infraRef, "kind": "OCIMachineTemplate"},
		}}},
	}}
	u.SetAPIVersion("controlplane.cluster.x-k8s.io/v1beta2")
	u.SetKind("KubeadmControlPlane")
	u.SetName(name)
	u.SetNamespace("default")
	return u
}

func ociCluster(name, resourceID string) *unstructured.Unstructured {
	spec := map[string]any{"compartmentId": "ocid1.compartment.oc1..cmp"}
	if resourceID != "" {
		spec["ociResourceIdentifier"] = resourceID
	}
	u := &unstructured.Unstructured{Object: map[string]any{"spec": spec}}
	u.SetAPIVersion("infrastructure.cluster.x-k8s.io/v1beta2")
	u.SetKind("OCICluster")
	u.SetName(name)
	u.SetNamespace("default")
	return u
}
