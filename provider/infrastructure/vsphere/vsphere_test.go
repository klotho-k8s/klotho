package vsphere

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"strings"
	"testing"

	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"sigs.k8s.io/yaml"

	"github.com/klotho-k8s/klotho/pkg/cluster"
)

func testProvider(f *fakeClient) *Provider {
	return New(f, Config{
		Server:     "vcenter.example.com",
		Username:   "klotho@vsphere.local",
		Password:   "secret",
		Datacenter: "dc1",
		Datastore:  "datastore1",
		Network:    "VM Network",
		Template:   "ubuntu-2404-template",
		EndpointIP: "192.168.1.200",
	})
}

func TestProviderID(t *testing.T) {
	cases := []struct {
		uuid string
		want string
	}{
		{"423F1A2B-0000-4000-8000-000000000001", "vsphere://423f1a2b-0000-4000-8000-000000000001"},
		{"abcdef00-1111-2222-3333-444455556666", "vsphere://abcdef00-1111-2222-3333-444455556666"},
	}
	for _, c := range cases {
		if got := ProviderID(c.uuid); got != c.want {
			t.Errorf("ProviderID(%q) = %q, want %q", c.uuid, got, c.want)
		}
	}
}

func TestTagAndVMName(t *testing.T) {
	if got := Tag("Lab1"); got != "klotho-lab1" {
		t.Errorf("Tag = %q, want klotho-lab1", got)
	}
	if got := VMName("lab1"); got != "lab1-bootstrap" {
		t.Errorf("VMName = %q, want lab1-bootstrap", got)
	}
}

func TestEndpoint(t *testing.T) {
	cases := []struct {
		name    string
		ip      string
		port    int
		wantErr bool
		want    cluster.Endpoint
	}{
		{name: "default port", ip: "10.0.0.5", want: cluster.Endpoint{Host: "10.0.0.5", Port: 6443}},
		{name: "explicit port", ip: "10.0.0.5", port: 7443, want: cluster.Endpoint{Host: "10.0.0.5", Port: 7443}},
		{name: "empty", ip: "", wantErr: true},
		{name: "not an ip", ip: "not-an-ip", wantErr: true},
		{name: "loopback", ip: "127.0.0.1", wantErr: true},
		{name: "unspecified", ip: "0.0.0.0", wantErr: true},
		{name: "multicast", ip: "224.0.0.1", wantErr: true},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			p := New(newFakeClient(), Config{EndpointIP: c.ip, Port: c.port})
			r := p.resolve(nil)
			ep, err := r.Endpoint()
			if c.wantErr {
				if err == nil {
					t.Fatalf("expected error, got endpoint %+v", ep)
				}
				return
			}
			if err != nil {
				t.Fatalf("unexpected error: %v", err)
			}
			if ep != c.want {
				t.Errorf("endpoint = %+v, want %+v", ep, c.want)
			}
		})
	}
}

func TestResolvePrecedence(t *testing.T) {
	// Config wins over variables; variables fill what config leaves empty;
	// defaults fill the rest.
	p := New(newFakeClient(), Config{
		Template:  "cfg-template",
		CloneMode: "",
		NumCPUs:   0,
	})
	vars := map[string]string{
		VarTemplate:   "var-template",
		VarDatacenter: "var-dc",
		VarEndpointIP: "10.1.2.3",
	}
	r := p.resolve(vars)
	if r.Template != "cfg-template" {
		t.Errorf("template = %q, want cfg-template (config wins)", r.Template)
	}
	if r.Datacenter != "var-dc" {
		t.Errorf("datacenter = %q, want var-dc (from variable)", r.Datacenter)
	}
	if r.EndpointIP != "10.1.2.3" {
		t.Errorf("endpointIP = %q, want 10.1.2.3 (from variable)", r.EndpointIP)
	}
	if r.CloneMode != DefaultCloneMode {
		t.Errorf("cloneMode = %q, want default %q", r.CloneMode, DefaultCloneMode)
	}
	if r.NumCPUs != DefaultNumCPUs || r.MemoryMiB != DefaultMemoryMiB || r.Port != DefaultPort {
		t.Errorf("defaults not applied: cpus=%d mem=%d port=%d", r.NumCPUs, r.MemoryMiB, r.Port)
	}
}

func TestBuildGuestinfo(t *testing.T) {
	uuid := "423f1a2b-0000-4000-8000-000000000001"
	gi, err := buildGuestinfo(uuid, "lab1-bootstrap", []byte("#cloud-config\n"))
	if err != nil {
		t.Fatalf("buildGuestinfo: %v", err)
	}
	if gi[GuestinfoUserDataEncoding] != "base64" || gi[GuestinfoMetadataEncoding] != "base64" {
		t.Errorf("encoding keys not set: %v", gi)
	}
	ud, err := base64.StdEncoding.DecodeString(gi[GuestinfoUserData])
	if err != nil || string(ud) != "#cloud-config\n" {
		t.Errorf("userdata round-trip failed: %q err=%v", ud, err)
	}
	mdRaw, err := base64.StdEncoding.DecodeString(gi[GuestinfoMetadata])
	if err != nil {
		t.Fatalf("metadata decode: %v", err)
	}
	var md metaData
	if err := json.Unmarshal(mdRaw, &md); err != nil {
		t.Fatalf("metadata json: %v", err)
	}
	if md.InstanceID != uuid {
		t.Errorf("instance-id = %q, want %q", md.InstanceID, uuid)
	}
	if md.LocalHostname != "lab1-bootstrap" {
		t.Errorf("local-hostname = %q, want lab1-bootstrap", md.LocalHostname)
	}
	if _, err := buildGuestinfo("", "h", []byte("x")); err == nil {
		t.Error("expected error for empty uuid")
	}
	if _, err := buildGuestinfo("u", "h", nil); err == nil {
		t.Error("expected error for empty user-data")
	}
}

func TestLaunch(t *testing.T) {
	f := newFakeClient()
	p := testProvider(f)
	spec := &cluster.Spec{Name: "lab1"}
	node, err := p.Launch(context.Background(), spec, []byte("#cloud-config\n"))
	if err != nil {
		t.Fatalf("Launch: %v", err)
	}
	if node.Name != "lab1-bootstrap" {
		t.Errorf("node name = %q, want lab1-bootstrap", node.Name)
	}
	if !strings.HasPrefix(node.ID, ProviderIDPrefix) {
		t.Errorf("node ID = %q, want vsphere:// prefix", node.ID)
	}
	if len(f.cloned) != 1 {
		t.Fatalf("expected 1 clone, got %d", len(f.cloned))
	}
	in := f.cloned[0]
	if in.Name != "lab1-bootstrap" || in.Template != "ubuntu-2404-template" || in.Annotation != "klotho-lab1" {
		t.Errorf("clone input wrong: %+v", in)
	}
	// guestinfo set, VM powered on, address returned.
	gi := f.guestinfo["lab1-bootstrap"]
	if gi[GuestinfoUserDataEncoding] != "base64" {
		t.Errorf("guestinfo not set: %v", gi)
	}
	if len(f.poweredOn) != 1 || f.poweredOn[0] != "lab1-bootstrap" {
		t.Errorf("VM not powered on: %v", f.poweredOn)
	}
	if len(node.Addresses) == 0 {
		t.Error("node has no addresses")
	}
	// provider ID matches the clone's UUID.
	if node.ID != ProviderID(f.vms["lab1-bootstrap"].UUID) {
		t.Errorf("node ID %q does not match VM UUID", node.ID)
	}
}

func TestLaunchErrors(t *testing.T) {
	t.Run("already exists", func(t *testing.T) {
		f := newFakeClient()
		f.vms["lab1-bootstrap"] = &VM{Name: "lab1-bootstrap", UUID: "u"}
		p := testProvider(f)
		if _, err := p.Launch(context.Background(), &cluster.Spec{Name: "lab1"}, []byte("x")); err == nil {
			t.Error("expected error when VM already exists")
		}
	})
	t.Run("no template", func(t *testing.T) {
		f := newFakeClient()
		p := New(f, Config{Server: "s", EndpointIP: "10.0.0.1"})
		if _, err := p.Launch(context.Background(), &cluster.Spec{Name: "lab1"}, []byte("x")); err == nil {
			t.Error("expected error when template unset")
		}
	})
	t.Run("ip wait fails cleans up", func(t *testing.T) {
		f := newFakeClient()
		f.ipErr = errors.New("timeout")
		p := testProvider(f)
		if _, err := p.Launch(context.Background(), &cluster.Spec{Name: "lab1"}, []byte("x")); err == nil {
			t.Error("expected error when IP wait fails")
		}
		if len(f.destroyed) != 1 {
			t.Errorf("expected cleanup destroy, got %v", f.destroyed)
		}
	})
	t.Run("empty user-data", func(t *testing.T) {
		p := testProvider(newFakeClient())
		if _, err := p.Launch(context.Background(), &cluster.Spec{Name: "lab1"}, nil); err == nil {
			t.Error("expected error for empty user-data")
		}
	})
}

func TestPrepare(t *testing.T) {
	f := newFakeClient()
	p := testProvider(f)
	prep, err := p.Prepare(context.Background(), &cluster.Spec{Name: "lab1"})
	if err != nil {
		t.Fatalf("Prepare: %v", err)
	}
	if prep.Endpoint.Host != "192.168.1.200" || prep.Endpoint.Port != 6443 {
		t.Errorf("endpoint = %+v", prep.Endpoint)
	}
	if len(prep.Created) != 0 {
		t.Errorf("Prepare allocated resources: %v", prep.Created)
	}
	if prep.Variables[VarServer] != "vcenter.example.com" || prep.Variables[VarTemplate] != "ubuntu-2404-template" {
		t.Errorf("variables not injected: %v", prep.Variables)
	}
	if prep.Variables[VarDatacenter] != "dc1" {
		t.Errorf("datacenter variable = %q", prep.Variables[VarDatacenter])
	}
}

func TestPrepareErrors(t *testing.T) {
	t.Run("missing template", func(t *testing.T) {
		f := newFakeClient()
		f.templates = map[string]bool{} // template not present
		p := testProvider(f)
		if _, err := p.Prepare(context.Background(), &cluster.Spec{Name: "lab1"}); err == nil {
			t.Error("expected error when template does not exist")
		}
	})
	t.Run("no endpoint", func(t *testing.T) {
		f := newFakeClient()
		p := New(f, Config{Server: "s", Template: "ubuntu-2404-template"})
		if _, err := p.Prepare(context.Background(), &cluster.Spec{Name: "lab1"}); err == nil {
			t.Error("expected error when endpoint unset")
		}
	})
}

func TestAdopt(t *testing.T) {
	f := newFakeClient()
	f.vms["lab1-bootstrap"] = &VM{Name: "lab1-bootstrap", UUID: "423f1a2b-0000-4000-8000-000000000001"}
	p := testProvider(f)
	state := &cluster.State{
		Spec:      cluster.Spec{Name: "lab1", Namespace: "default"},
		Bootstrap: &cluster.Node{ID: "vsphere://423f1a2b-0000-4000-8000-000000000001", Name: "lab1-bootstrap"},
	}
	out, err := p.Adopt(context.Background(), state, controlPlaneBundle())
	if err != nil {
		t.Fatalf("Adopt: %v", err)
	}
	var m map[string]any
	if err := yaml.Unmarshal(out, &m); err != nil {
		t.Fatalf("unmarshal adopt output: %v", err)
	}
	if m["kind"] != "VSphereMachine" {
		t.Errorf("kind = %v, want VSphereMachine", m["kind"])
	}
	meta := m["metadata"].(map[string]any)
	if meta["name"] != "lab1-bootstrap" {
		t.Errorf("machine name = %v, want lab1-bootstrap", meta["name"])
	}
	spec := m["spec"].(map[string]any)
	if spec["providerID"] != "vsphere://423f1a2b-0000-4000-8000-000000000001" {
		t.Errorf("providerID = %v (should be pre-set, CAPL-style)", spec["providerID"])
	}
	labels := meta["labels"].(map[string]any)
	if labels[labelClusterName] != "lab1" {
		t.Errorf("cluster-name label = %v", labels[labelClusterName])
	}
	annos := meta["annotations"].(map[string]any)
	if annos[annotationClonedFromName] != "lab1-control-plane" {
		t.Errorf("cloned-from-name = %v", annos[annotationClonedFromName])
	}
}

func TestAdoptRefindsWhenStateMissing(t *testing.T) {
	f := newFakeClient()
	f.vms["lab1-bootstrap"] = &VM{Name: "lab1-bootstrap", UUID: "423f1a2b-0000-4000-8000-000000000009"}
	p := testProvider(f)
	// No Bootstrap record in state: Adopt re-finds the VM by name.
	state := &cluster.State{Spec: cluster.Spec{Name: "lab1", Namespace: "default"}}
	out, err := p.Adopt(context.Background(), state, controlPlaneBundle())
	if err != nil {
		t.Fatalf("Adopt: %v", err)
	}
	var m map[string]any
	_ = yaml.Unmarshal(out, &m)
	spec := m["spec"].(map[string]any)
	if spec["providerID"] != "vsphere://423f1a2b-0000-4000-8000-000000000009" {
		t.Errorf("providerID = %v, want re-found UUID", spec["providerID"])
	}
}

func TestPlanAndTeardown(t *testing.T) {
	f := newFakeClient()
	f.vms["lab1-bootstrap"] = &VM{Name: "lab1-bootstrap", UUID: "u1", Annotation: "klotho-lab1"}
	f.vms["lab1-cp-abc"] = &VM{Name: "lab1-cp-abc", UUID: "u2", Annotation: "klotho-lab1"}
	f.vms["other-vm"] = &VM{Name: "other-vm", UUID: "u3", Annotation: "someone-else"}
	f.vms["lab1-untagged"] = &VM{Name: "lab1-untagged", UUID: "u4", Annotation: ""}
	p := testProvider(f)
	state := &cluster.State{Spec: cluster.Spec{Name: "lab1"}}

	plan, err := p.Plan(context.Background(), state)
	if err != nil {
		t.Fatalf("Plan: %v", err)
	}
	got := map[string]bool{}
	for _, r := range plan.Resources {
		got[r.ID] = true
	}
	if !got["lab1-bootstrap"] || !got["lab1-cp-abc"] {
		t.Errorf("plan missing tagged VMs: %v", plan.Resources)
	}
	if got["other-vm"] || got["lab1-untagged"] {
		t.Errorf("plan included untagged/foreign VMs: %v", plan.Resources)
	}

	if err := p.Teardown(context.Background(), state, plan); err != nil {
		t.Fatalf("Teardown: %v", err)
	}
	for _, d := range []string{"lab1-bootstrap", "lab1-cp-abc"} {
		if _, exists := f.vms[d]; exists {
			t.Errorf("%s not destroyed", d)
		}
	}
	if _, exists := f.vms["other-vm"]; !exists {
		t.Error("foreign VM was destroyed")
	}
}

func TestTeardownRefusesForeignPrefix(t *testing.T) {
	f := newFakeClient()
	p := testProvider(f)
	state := &cluster.State{Spec: cluster.Spec{Name: "lab1"}}
	plan := buildPlan(cluster.Resource{Kind: ResourceKindVM, ID: "other-vm", Name: "other-vm"})
	if err := p.Teardown(context.Background(), state, plan); err == nil {
		t.Error("expected refusal to remove a VM outside the cluster prefix")
	}
	if len(f.destroyed) != 0 {
		t.Errorf("destroyed a foreign VM: %v", f.destroyed)
	}
}

func TestDiscover(t *testing.T) {
	f := newFakeClient()
	p := testProvider(f)
	inv, err := p.Discover(context.Background())
	if err != nil {
		t.Fatalf("Discover: %v", err)
	}
	if len(inv[KindNetwork]) != 1 || inv[KindNetwork][0].Name != "VM Network" {
		t.Errorf("networks = %v", inv[KindNetwork])
	}
	if len(inv[KindDatastore]) != 1 {
		t.Errorf("datastores = %v", inv[KindDatastore])
	}
	if len(inv[KindTemplate]) != 1 {
		t.Errorf("templates = %v", inv[KindTemplate])
	}
}

func TestCheck(t *testing.T) {
	p := testProvider(newFakeClient())
	if fs := p.Check(fullBundle()); len(fs) != 0 {
		t.Errorf("expected no findings on a complete bundle, got %v", fs)
	}
}

func TestCheckNoVSphereCluster(t *testing.T) {
	p := testProvider(newFakeClient())
	fs := p.Check([]*unstructured.Unstructured{
		machineTemplate("lab1-control-plane", "ubuntu-2404-template"),
		kcp("lab1"),
	})
	if len(fs) != 1 || fs[0].Object != "VSphereCluster" {
		t.Errorf("expected a single missing-VSphereCluster finding, got %v", fs)
	}
}

func TestCheckMissingPieces(t *testing.T) {
	p := testProvider(newFakeClient())
	// A VSphereCluster missing server and endpoint, and a machine template
	// missing its source template, should each produce a finding.
	vc := vsphereCluster("lab1")
	unstructured.RemoveNestedField(vc.Object, "spec", "server")
	unstructured.RemoveNestedField(vc.Object, "spec", "controlPlaneEndpoint")
	mt := machineTemplate("lab1-control-plane", "")
	fs := p.Check([]*unstructured.Unstructured{vc, mt, kcp("lab1")})
	fields := map[string]bool{}
	for _, f := range fs {
		fields[f.Field] = true
	}
	if !fields["spec.server"] || !fields["spec.controlPlaneEndpoint.host"] || !fields["spec.template.spec.template"] {
		t.Errorf("missing expected findings, got %v", fs)
	}
}

func TestDependenciesNil(t *testing.T) {
	p := testProvider(newFakeClient())
	if deps := p.Dependencies(); deps != nil {
		t.Errorf("expected no dependencies, got %v", deps)
	}
}

func TestTagMachineTemplatesNoop(t *testing.T) {
	p := testProvider(newFakeClient())
	objs := controlPlaneBundle()
	if err := p.TagMachineTemplates(objs, "klotho-lab1"); err != nil {
		t.Fatalf("TagMachineTemplates: %v", err)
	}
	// It must not add a spec.template.spec.tags field (the CRD rejects it).
	for _, o := range objs {
		if o.GetKind() != "VSphereMachineTemplate" {
			continue
		}
		if _, found, _ := unstructured.NestedSlice(o.Object, "spec", "template", "spec", "tags"); found {
			t.Error("tags field should not be set on VSphereMachineTemplate")
		}
	}
}
