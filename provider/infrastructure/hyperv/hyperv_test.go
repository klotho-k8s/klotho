package hyperv

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"sigs.k8s.io/yaml"

	"github.com/klotho-k8s/klotho/pkg/cluster"
	"github.com/klotho-k8s/klotho/provider/infrastructure"
)

func TestPSQuote(t *testing.T) {
	cases := map[string]string{
		"plain":      "'plain'",
		"a b":        "'a b'",
		"it's":       "'it''s'",
		"x'; rm -rf": "'x''; rm -rf'",
		`$(whoami)`:  `'$(whoami)'`,
		"back`tick":  "'back`tick'",
	}
	for in, want := range cases {
		if got := psQuote(in); got != want {
			t.Errorf("psQuote(%q) = %q, want %q", in, got, want)
		}
	}
}

func TestCreateVMScript(t *testing.T) {
	s := createVMScript(CreateInput{
		Name: "lab-bootstrap", Image: `C:\img\base.vhdx`, Switch: "Default Switch",
		VMPath: `C:\vms`, MemoryMiB: 4096, CPUs: 2, Notes: "klotho-lab",
	})
	for _, want := range []string{
		"New-VM -Name 'lab-bootstrap' -Generation 2 -MemoryStartupBytes 4096MB",
		"-SwitchName 'Default Switch'",
		"Set-VMProcessor -VMName 'lab-bootstrap' -Count 2",
		"Set-VM -Name 'lab-bootstrap' -Notes 'klotho-lab'",
		"ConvertTo-Json",
	} {
		if !strings.Contains(s, want) {
			t.Errorf("createVMScript missing %q in:\n%s", want, s)
		}
	}
}

func TestQueryScriptsExitZero(t *testing.T) {
	// Get-VM/Get-VM list leave powershell.exe a non-zero exit code for a
	// missing name even under SilentlyContinue, which the runner would treat
	// as a hard error; the query scripts must end with "exit 0" and guard so a
	// missing VM is reported as empty output, not a failure.
	for name, s := range map[string]string{
		"getVM":  getVMScript("lab-bootstrap"),
		"listVM": listVMsScript("lab-"),
	} {
		if !strings.HasSuffix(strings.TrimSpace(s), "exit 0") {
			t.Errorf("%s script must end with 'exit 0': %s", name, s)
		}
	}
	if !strings.Contains(getVMScript("x"), "if ($vm)") {
		t.Errorf("getVMScript must guard on a found VM: %s", getVMScript("x"))
	}
}

func TestSetFirmwareScript(t *testing.T) {
	if s := setFirmwareScript("vm", false); !strings.Contains(s, "-EnableSecureBoot Off") {
		t.Errorf("secureBoot=false should disable Secure Boot: %s", s)
	}
	if s := setFirmwareScript("vm", true); !strings.Contains(s, "-EnableSecureBoot On") {
		t.Errorf("secureBoot=true should enable Secure Boot: %s", s)
	}
}

func TestNormalizeState(t *testing.T) {
	for in, want := range map[string]string{`"Running"`: StateRunning, `2`: StateRunning, `3`: StateOff, `"Off"`: StateOff} {
		if got := normalizeState([]byte(in)); got != want {
			t.Errorf("normalizeState(%s) = %q, want %q", in, got, want)
		}
	}
}

func TestLaunch(t *testing.T) {
	f := newFakeClient()
	f.addrs[VMName("lab")] = []string{"fe80::1", "192.168.1.50"}
	dir := t.TempDir()
	p := New(f, Config{Image: filepath.Join(dir, "base.vhdx"), ISODir: dir, Switch: DefaultSwitch})

	node, err := p.Launch(context.Background(), &cluster.Spec{Name: "lab"}, []byte("#cloud-config\n"))
	if err != nil {
		t.Fatalf("Launch: %v", err)
	}
	if node.ID != ProviderIDPrefix+"guid-1" {
		t.Errorf("node ID = %q, want %q", node.ID, ProviderIDPrefix+"guid-1")
	}
	if len(node.Addresses) != 1 || node.Addresses[0] != "192.168.1.50" {
		t.Errorf("addresses = %v, want [192.168.1.50]", node.Addresses)
	}
	if len(f.created) != 1 || f.created[0].Notes != Tag("lab") {
		t.Errorf("VM not created with the cluster tag: %+v", f.created)
	}
	iso := f.attached[VMName("lab")]
	if iso == "" {
		t.Error("no ISO attached")
	}
	if _, err := os.Stat(iso); err != nil {
		t.Errorf("ISO not written: %v", err)
	}
	if len(f.started) != 1 {
		t.Errorf("VM not started: %v", f.started)
	}
}

func TestLaunchRefusesExistingVM(t *testing.T) {
	f := newFakeClient()
	f.vms[VMName("lab")] = &VM{Name: VMName("lab"), ID: "x", State: StateRunning}
	p := New(f, Config{Image: "img.vhdx", ISODir: t.TempDir()})
	if _, err := p.Launch(context.Background(), &cluster.Spec{Name: "lab"}, []byte("x")); err == nil {
		t.Fatal("expected error for pre-existing bootstrap VM")
	}
}

func TestLaunchCleansUpOnFailure(t *testing.T) {
	f := newFakeClient()
	// no address ever -> waitForAddress times out; but that is slow. Instead
	// force CreateVM to appear then fail firmware by removing the switch is not
	// it; simplest: make GetIPAddresses return nothing and shorten by using a
	// create error path.
	f.createErr = context.Canceled
	p := New(f, Config{Image: "img.vhdx", ISODir: t.TempDir()})
	if _, err := p.Launch(context.Background(), &cluster.Spec{Name: "lab"}, []byte("x")); err == nil {
		t.Fatal("expected launch failure")
	}
	if len(f.removed) == 0 {
		t.Error("expected cleanup to remove the VM after failure")
	}
}

func TestPrepare(t *testing.T) {
	dir := t.TempDir()
	img := filepath.Join(dir, "base.vhdx")
	if err := os.WriteFile(img, []byte("vhdx"), 0o600); err != nil {
		t.Fatal(err)
	}
	f := newFakeClient()
	p := New(f, Config{Image: img, EndpointHost: "192.168.1.10", Switch: DefaultSwitch})
	prep, err := p.Prepare(context.Background(), &cluster.Spec{Name: "lab"})
	if err != nil {
		t.Fatalf("Prepare: %v", err)
	}
	if prep.Endpoint.Host != "192.168.1.10" || prep.Endpoint.Port != DefaultPort {
		t.Errorf("endpoint = %+v", prep.Endpoint)
	}
	if prep.Variables[VarImagePath] != img || prep.Variables[VarSwitchName] != DefaultSwitch || prep.Variables[VarEndpointHost] != "192.168.1.10" {
		t.Errorf("variables not injected: %v", prep.Variables)
	}
	if len(prep.Created) != 0 {
		t.Errorf("Prepare should allocate nothing, created %v", prep.Created)
	}
}

func TestPrepareErrors(t *testing.T) {
	dir := t.TempDir()
	img := filepath.Join(dir, "base.vhdx")
	_ = os.WriteFile(img, []byte("x"), 0o600)

	// missing image
	p := New(newFakeClient(), Config{EndpointHost: "1.2.3.4"})
	if _, err := p.Prepare(context.Background(), &cluster.Spec{Name: "lab"}); err == nil {
		t.Error("expected error for missing image")
	}
	// missing endpoint
	p = New(newFakeClient(), Config{Image: img})
	if _, err := p.Prepare(context.Background(), &cluster.Spec{Name: "lab"}); err == nil {
		t.Error("expected error for missing endpoint")
	}
	// switch not present
	f := newFakeClient()
	f.switches = []string{"OtherSwitch"}
	p = New(f, Config{Image: img, EndpointHost: "1.2.3.4", Switch: "Default Switch"})
	if _, err := p.Prepare(context.Background(), &cluster.Spec{Name: "lab"}); err == nil {
		t.Error("expected error for missing switch")
	}
}

func TestAdopt(t *testing.T) {
	f := newFakeClient()
	f.vms[VMName("lab")] = &VM{Name: VMName("lab"), ID: "guid-7", State: StateRunning}
	p := New(f, Config{})

	tmpl := &unstructured.Unstructured{Object: map[string]any{
		"spec": map[string]any{"template": map[string]any{"spec": map[string]any{
			"image":      `C:\img\base.vhdx`,
			"switchName": "Default Switch",
		}}},
	}}
	tmpl.SetAPIVersion("infrastructure.cluster.x-k8s.io/v1alpha1")
	tmpl.SetKind("HyperVMachineTemplate")
	tmpl.SetName("lab-control-plane")

	kcp := &unstructured.Unstructured{Object: map[string]any{
		"spec": map[string]any{"machineTemplate": map[string]any{"spec": map[string]any{
			"infrastructureRef": map[string]any{"name": "lab-control-plane"},
		}}},
	}}
	kcp.SetAPIVersion("controlplane.cluster.x-k8s.io/v1beta2")
	kcp.SetKind("KubeadmControlPlane")
	kcp.SetName("lab-control-plane")

	state := &cluster.State{Spec: cluster.Spec{Name: "lab"}, Bootstrap: &cluster.Node{ID: ProviderIDPrefix + "guid-7"}}
	out, err := p.Adopt(context.Background(), state, []*unstructured.Unstructured{tmpl, kcp})
	if err != nil {
		t.Fatalf("Adopt: %v", err)
	}
	var m map[string]any
	if err := yaml.Unmarshal(out, &m); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	u := &unstructured.Unstructured{Object: m}
	if u.GetKind() != "HyperVMachine" {
		t.Errorf("kind = %q, want HyperVMachine", u.GetKind())
	}
	if u.GetName() != VMName("lab") {
		t.Errorf("name = %q, want %q", u.GetName(), VMName("lab"))
	}
	if vmName, _, _ := unstructured.NestedString(u.Object, "spec", "vmName"); vmName != VMName("lab") {
		t.Errorf("spec.vmName = %q, want %q", vmName, VMName("lab"))
	}
	if pid, _, _ := unstructured.NestedString(u.Object, "spec", "providerID"); pid != ProviderIDPrefix+"guid-7" {
		t.Errorf("spec.providerID = %q", pid)
	}
	if img, _, _ := unstructured.NestedString(u.Object, "spec", "image"); img != `C:\img\base.vhdx` {
		t.Errorf("spec.image not carried from template: %q", img)
	}
	ann := u.GetAnnotations()
	if ann[AnnotationClonedFromName] != "lab-control-plane" {
		t.Errorf("cloned-from-name = %q", ann[AnnotationClonedFromName])
	}
}

func TestPlanAndTeardown(t *testing.T) {
	f := newFakeClient()
	f.vms["lab-bootstrap"] = &VM{Name: "lab-bootstrap", ID: "g1", State: StateRunning, Notes: Tag("lab")}
	f.vms["lab-control-plane-abc"] = &VM{Name: "lab-control-plane-abc", ID: "g2", State: StateRunning}
	f.vms["other-vm"] = &VM{Name: "other-vm", ID: "g3", State: StateRunning}
	p := New(f, Config{})
	state := &cluster.State{Spec: cluster.Spec{Name: "lab"}}

	plan, err := p.Plan(context.Background(), state)
	if err != nil {
		t.Fatalf("Plan: %v", err)
	}
	names := map[string]bool{}
	for _, r := range plan.Resources {
		if r.Kind == ResourceKindVM {
			names[r.ID] = true
		}
	}
	if !names["lab-bootstrap"] || !names["lab-control-plane-abc"] {
		t.Errorf("plan missing cluster VMs: %v", names)
	}
	if names["other-vm"] {
		t.Error("plan should not include unrelated VM")
	}

	if err := p.Teardown(context.Background(), state, plan); err != nil {
		t.Fatalf("Teardown: %v", err)
	}
	if _, ok := f.vms["lab-bootstrap"]; ok {
		t.Error("bootstrap VM not removed")
	}
	if _, ok := f.vms["lab-control-plane-abc"]; ok {
		t.Error("control plane VM not removed")
	}
	if _, ok := f.vms["other-vm"]; !ok {
		t.Error("unrelated VM should not be removed")
	}
}

func TestLaunchDeletionSidecar(t *testing.T) {
	f := newFakeClient()
	dir := t.TempDir()
	f.vms["lab-bootstrap"] = &VM{Name: "lab-bootstrap", ID: "g1", State: StateRunning, Notes: Tag("lab")}
	iso := filepath.Join(dir, ISOName("lab"))
	if err := os.WriteFile(iso, []byte("iso"), 0o600); err != nil {
		t.Fatal(err)
	}
	p := New(f, Config{ISODir: dir})
	state := &cluster.State{Spec: cluster.Spec{Name: "lab"}}

	if err := p.LaunchDeletionSidecar(context.Background(), state); err != nil {
		t.Fatalf("LaunchDeletionSidecar: %v", err)
	}
	if _, ok := f.vms["lab-bootstrap"]; ok {
		t.Error("bootstrap VM (node zero) not removed")
	}
	if len(f.removed) != 1 || f.removed[0] != "lab-bootstrap" {
		t.Errorf("removed = %v, want [lab-bootstrap]", f.removed)
	}
	if _, err := os.Stat(iso); !os.IsNotExist(err) {
		t.Errorf("cidata ISO not removed: %v", err)
	}
	// Idempotent: a second call with the VM and ISO already gone is not an error.
	if err := p.LaunchDeletionSidecar(context.Background(), state); err != nil {
		t.Fatalf("second LaunchDeletionSidecar: %v", err)
	}
}

func TestTeardownRefusesForeignName(t *testing.T) {
	f := newFakeClient()
	f.vms["notlab-vm"] = &VM{Name: "notlab-vm", ID: "g9", State: StateRunning}
	p := New(f, Config{})
	state := &cluster.State{Spec: cluster.Spec{Name: "lab"}}
	plan := &infrastructure.Plan{Resources: []cluster.Resource{{Kind: ResourceKindVM, ID: "notlab-vm"}}}
	if err := p.Teardown(context.Background(), state, plan); err == nil {
		t.Error("expected refusal to remove a VM without the cluster prefix")
	}
	if _, ok := f.vms["notlab-vm"]; !ok {
		t.Error("foreign VM must not be removed")
	}
}
