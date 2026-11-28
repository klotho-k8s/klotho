package klotho

import (
	"context"
	"errors"
	"testing"
	"time"

	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"

	"github.com/klotho-k8s/klotho/pkg/cluster"
	"github.com/klotho-k8s/klotho/pkg/kube"
	"github.com/klotho-k8s/klotho/pkg/state"
)

// --- object builders ---------------------------------------------------------

func kcpObject(name, version string, replicas int64) *unstructured.Unstructured {
	u := &unstructured.Unstructured{Object: map[string]any{}}
	u.SetAPIVersion("controlplane.cluster.x-k8s.io/v1beta2")
	u.SetKind("KubeadmControlPlane")
	u.SetNamespace("default")
	u.SetName(name)
	u.SetLabels(map[string]string{LabelClusterName: "demo"})
	_ = unstructured.SetNestedField(u.Object, version, "spec", "version")
	_ = unstructured.SetNestedField(u.Object, replicas, "spec", "replicas")
	_ = unstructured.SetNestedMap(u.Object, map[string]any{
		"apiGroup": "infrastructure.cluster.x-k8s.io",
		"kind":     "DockerMachineTemplate",
		"name":     name,
	}, "spec", "machineTemplate", "spec", "infrastructureRef")
	return u
}

func mdObject(name, version string, replicas int64) *unstructured.Unstructured {
	u := &unstructured.Unstructured{Object: map[string]any{}}
	u.SetAPIVersion("cluster.x-k8s.io/v1beta2")
	u.SetKind("MachineDeployment")
	u.SetNamespace("default")
	u.SetName(name)
	u.SetLabels(map[string]string{LabelClusterName: "demo"})
	_ = unstructured.SetNestedField(u.Object, "demo", "spec", "clusterName")
	_ = unstructured.SetNestedField(u.Object, replicas, "spec", "replicas")
	_ = unstructured.SetNestedField(u.Object, version, "spec", "template", "spec", "version")
	_ = unstructured.SetNestedMap(u.Object, map[string]any{
		"apiGroup": "infrastructure.cluster.x-k8s.io",
		"kind":     "DockerMachineTemplate",
		"name":     name,
	}, "spec", "template", "spec", "infrastructureRef")
	return u
}

func machineObj(name, version string, labels map[string]string, nodeRef bool) *unstructured.Unstructured {
	u := &unstructured.Unstructured{Object: map[string]any{}}
	u.SetAPIVersion("cluster.x-k8s.io/v1beta1")
	u.SetKind("Machine")
	u.SetNamespace("default")
	u.SetName(name)
	u.SetLabels(labels)
	_ = unstructured.SetNestedField(u.Object, version, "spec", "version")
	if nodeRef {
		_ = unstructured.SetNestedMap(u.Object, map[string]any{"name": name + "-node"}, "status", "nodeRef")
	}
	return u
}

func seed(f *fakeCluster, objs ...*unstructured.Unstructured) {
	for _, o := range objs {
		f.objs[keyOf(o)] = o
	}
}

// upgradeHarness builds Deps wired to a fake cluster and a non-reserved state
// record, plus the fake cluster so tests can seed and inspect objects.
func upgradeHarness(t *testing.T) (Deps, *fakeCluster) {
	t.Helper()
	rec := &recorder{}
	store := newFakeStore(rec)
	st := &cluster.State{Spec: cluster.Spec{Name: "demo", Provider: "docker"}, CreatedAt: time.Now().UTC()}
	sealed, err := state.PlainCodec{}.Seal(st)
	if err != nil {
		t.Fatal(err)
	}
	if err := store.Save(context.Background(), "demo", sealed); err != nil {
		t.Fatal(err)
	}
	f := newFakeCluster(rec, "demo")
	deps := Deps{
		Store: store,
		Codec: state.PlainCodec{},
		ConnectAdmin: func(context.Context, *cluster.State) (kube.Cluster, error) {
			return f, nil
		},
		PollInterval: time.Millisecond,
	}
	return deps, f
}

// --- skew guard --------------------------------------------------------------

func TestCheckSkew(t *testing.T) {
	cases := []struct {
		name           string
		cp             string
		mds            []mdVersion
		target         string
		includeCP      bool
		includeWorkers bool
		wantFindings   int
	}{
		{name: "patch bump is fine", cp: "v1.33.0", target: "v1.33.4", includeCP: true, includeWorkers: true},
		{name: "one minor is fine", cp: "v1.32.5", target: "v1.33.0", includeCP: true, includeWorkers: true},
		{name: "two minors blocked", cp: "v1.31.0", target: "v1.33.0", includeCP: true, wantFindings: 1},
		{name: "downgrade blocked", cp: "v1.33.0", target: "v1.32.0", includeCP: true, wantFindings: 1},
		{name: "major jump blocked", cp: "v1.33.0", target: "v2.0.0", includeCP: true, wantFindings: 1},
		{
			name: "workers ahead of control plane blocked",
			cp:   "v1.32.0", target: "v1.33.0",
			mds:            []mdVersion{{name: "md-0", version: "v1.32.0"}},
			includeCP:      false, // workers-only: control plane stays at v1.32.0
			includeWorkers: true,
			wantFindings:   1,
		},
		{
			name: "workers two minors blocked",
			cp:   "v1.33.0", target: "v1.33.0",
			mds:            []mdVersion{{name: "md-0", version: "v1.31.0"}},
			includeCP:      false,
			includeWorkers: true,
			wantFindings:   1,
		},
		{
			name: "both at once, one minor, fine",
			cp:   "v1.32.0", target: "v1.33.0",
			mds:            []mdVersion{{name: "md-0", version: "v1.32.0"}, {name: "md-1", version: "v1.32.3"}},
			includeCP:      true,
			includeWorkers: true,
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			target, err := parseUpgradeVersion(tc.target)
			if err != nil {
				t.Fatal(err)
			}
			got, err := checkSkew(skewInput{
				cpVersion:      tc.cp,
				mds:            tc.mds,
				target:         target,
				includeCP:      tc.includeCP,
				includeWorkers: tc.includeWorkers,
			})
			if err != nil {
				t.Fatal(err)
			}
			if len(got) != tc.wantFindings {
				t.Fatalf("got %d findings, want %d: %v", len(got), tc.wantFindings, got)
			}
		})
	}
}

func TestParseUpgradeVersion(t *testing.T) {
	for _, s := range []string{"v1.33.4", "1.33.4", "v1.33.1+k0s.0"} {
		if _, err := parseUpgradeVersion(s); err != nil {
			t.Errorf("parseUpgradeVersion(%q): %v", s, err)
		}
	}
	if _, err := parseUpgradeVersion("nope"); err == nil {
		t.Error("parseUpgradeVersion(nope): want error")
	}
}

func TestMinorJump(t *testing.T) {
	j := func(a, b string) int {
		x, _ := parseUpgradeVersion(a)
		y, _ := parseUpgradeVersion(b)
		return minorJump(x, y)
	}
	if got := j("v1.32.0", "v1.33.0"); got != 1 {
		t.Errorf("one minor: got %d", got)
	}
	if got := j("v1.31.0", "v1.33.0"); got != 2 {
		t.Errorf("two minors: got %d", got)
	}
	if got := j("v1.33.0", "v1.33.9"); got != 0 {
		t.Errorf("same minor: got %d", got)
	}
	if got := j("v1.33.0", "v2.0.0"); got <= 1 {
		t.Errorf("major jump should read as >1 minor: got %d", got)
	}
}

// --- discovery ---------------------------------------------------------------

func TestDiscover(t *testing.T) {
	rec := &recorder{}
	f := newFakeCluster(rec, "demo")
	seed(f,
		kcpObject("demo-control-plane", "v1.32.0", 1),
		mdObject("demo-md-0", "v1.32.0", 2),
		mdObject("demo-md-1", "v1.32.0", 1),
		// a MachineDeployment of another cluster must be ignored
		func() *unstructured.Unstructured {
			o := mdObject("other-md-0", "v1.30.0", 1)
			o.SetName("other-md-0")
			o.SetLabels(map[string]string{LabelClusterName: "other"})
			_ = unstructured.SetNestedField(o.Object, "other", "spec", "clusterName")
			return o
		}(),
	)
	d, err := discover(context.Background(), f, "demo")
	if err != nil {
		t.Fatal(err)
	}
	if d.cp.Kind() != "KubeadmControlPlane" || d.cpObj.GetName() != "demo-control-plane" {
		t.Fatalf("control plane: got %s/%s", d.cp.Kind(), d.cpObj.GetName())
	}
	if len(d.mds) != 2 {
		t.Fatalf("got %d MachineDeployments, want 2 (other cluster excluded): %v", len(d.mds), mdNames(d.mds))
	}
}

func TestDiscoverNoControlPlane(t *testing.T) {
	rec := &recorder{}
	f := newFakeCluster(rec, "demo")
	if _, err := discover(context.Background(), f, "demo"); err == nil {
		t.Fatal("want error when no control plane object exists")
	}
}

func mdNames(mds []*unstructured.Unstructured) []string {
	var out []string
	for _, m := range mds {
		out = append(out, m.GetName())
	}
	return out
}

// --- orchestration -----------------------------------------------------------

func TestUpgradePatchesNoWatch(t *testing.T) {
	deps, f := upgradeHarness(t)
	seed(f,
		kcpObject("demo-control-plane", "v1.32.0", 1),
		mdObject("demo-md-0", "v1.32.0", 2),
	)
	err := Upgrade(context.Background(), deps, "demo", UpgradeOptions{KubernetesVersion: "v1.33.0"})
	if err != nil {
		t.Fatal(err)
	}
	// The control plane and the pool each took a version patch.
	if !f.rec.has("kube.Patch KubeadmControlPlane demo-control-plane") {
		t.Errorf("control plane not patched; calls: %v", f.rec.list())
	}
	if !f.rec.has("kube.Patch MachineDeployment demo-md-0") {
		t.Errorf("MachineDeployment not patched; calls: %v", f.rec.list())
	}
	// The stored objects now carry the target version.
	kcp := f.objs[keyOf(kcpObject("demo-control-plane", "", 1))]
	if v, _, _ := unstructured.NestedString(kcp.Object, "spec", "version"); v != "v1.33.0" {
		t.Errorf("control plane spec.version = %q, want v1.33.0", v)
	}
	md := f.objs[keyOf(mdObject("demo-md-0", "", 2))]
	if v, _, _ := unstructured.NestedString(md.Object, "spec", "template", "spec", "version"); v != "v1.33.0" {
		t.Errorf("MachineDeployment version = %q, want v1.33.0", v)
	}
}

func TestUpgradeControlPlaneOnly(t *testing.T) {
	deps, f := upgradeHarness(t)
	seed(f,
		kcpObject("demo-control-plane", "v1.32.0", 1),
		mdObject("demo-md-0", "v1.32.0", 2),
	)
	err := Upgrade(context.Background(), deps, "demo", UpgradeOptions{KubernetesVersion: "v1.33.0", ControlPlaneOnly: true})
	if err != nil {
		t.Fatal(err)
	}
	if f.rec.has("kube.Patch MachineDeployment") {
		t.Errorf("--control-plane-only must not patch workers; calls: %v", f.rec.list())
	}
	if !f.rec.has("kube.Patch KubeadmControlPlane demo-control-plane") {
		t.Error("control plane not patched")
	}
}

func TestUpgradeWatchCompletes(t *testing.T) {
	deps, f := upgradeHarness(t)
	seed(f,
		kcpObject("demo-control-plane", "v1.32.0", 1),
		// a single control plane Machine already at the target with a node:
		// the rollout reads as complete at once.
		machineObj("demo-control-plane-abc", "v1.33.0",
			map[string]string{LabelClusterName: "demo", LabelControlPlane: ""}, true),
	)
	err := Upgrade(context.Background(), deps, "demo", UpgradeOptions{
		KubernetesVersion: "v1.33.0",
		ControlPlaneOnly:  true,
		Watch:             true,
		WatchTimeout:      2 * time.Second,
	})
	if err != nil {
		t.Fatalf("watch upgrade: %v", err)
	}
}

func TestUpgradeBlockedBySkew(t *testing.T) {
	deps, f := upgradeHarness(t)
	seed(f, kcpObject("demo-control-plane", "v1.31.0", 1))
	err := Upgrade(context.Background(), deps, "demo", UpgradeOptions{KubernetesVersion: "v1.33.0", ControlPlaneOnly: true})
	var fe *UpgradeFindingsError
	if !errors.As(err, &fe) {
		t.Fatalf("want *UpgradeFindingsError, got %v", err)
	}
	if f.rec.has("kube.Patch KubeadmControlPlane") {
		t.Error("must not patch when blocked by findings")
	}
}

func TestUpgradeForceOverridesSkew(t *testing.T) {
	deps, f := upgradeHarness(t)
	seed(f, kcpObject("demo-control-plane", "v1.31.0", 1))
	err := Upgrade(context.Background(), deps, "demo", UpgradeOptions{KubernetesVersion: "v1.33.0", ControlPlaneOnly: true, Force: true})
	if err != nil {
		t.Fatalf("force upgrade: %v", err)
	}
	if !f.rec.has("kube.Patch KubeadmControlPlane demo-control-plane") {
		t.Error("force should patch despite findings")
	}
}

func TestUpgradeMutuallyExclusiveFlags(t *testing.T) {
	deps, _ := upgradeHarness(t)
	err := Upgrade(context.Background(), deps, "demo", UpgradeOptions{
		KubernetesVersion: "v1.33.0",
		ControlPlaneOnly:  true,
		WorkersOnly:       true,
	})
	if err == nil {
		t.Fatal("want error for --control-plane-only with --workers-only")
	}
}

func TestUpgradeImageSwap(t *testing.T) {
	deps, f := upgradeHarness(t)
	seed(f,
		kcpObject("demo-control-plane", "v1.32.0", 1),
		// the current infra template the control plane references
		func() *unstructured.Unstructured {
			u := &unstructured.Unstructured{Object: map[string]any{}}
			u.SetAPIVersion("infrastructure.cluster.x-k8s.io/v1beta2")
			u.SetKind("DockerMachineTemplate")
			u.SetNamespace("default")
			u.SetName("demo-control-plane")
			_ = unstructured.SetNestedMap(u.Object, map[string]any{}, "spec", "template", "spec")
			return u
		}(),
	)
	err := Upgrade(context.Background(), deps, "demo", UpgradeOptions{
		KubernetesVersion: "v1.33.0",
		ControlPlaneOnly:  true,
		ControlPlaneImage: "kindest/node:v1.33.0",
	})
	if err != nil {
		t.Fatal(err)
	}
	// A new template named for the version was applied with the image set.
	nt, ok := f.objs[keyOf(func() *unstructured.Unstructured {
		u := &unstructured.Unstructured{Object: map[string]any{}}
		u.SetAPIVersion("infrastructure.cluster.x-k8s.io/v1beta2")
		u.SetKind("DockerMachineTemplate")
		u.SetNamespace("default")
		u.SetName("demo-control-plane-1-33-0")
		return u
	}())]
	if !ok {
		t.Fatalf("new infra template not created; objects: %v", objKinds(f))
	}
	if img, _, _ := unstructured.NestedString(nt.Object, "spec", "template", "spec", "customImage"); img != "kindest/node:v1.33.0" {
		t.Errorf("new template customImage = %q", img)
	}
}

func objKinds(f *fakeCluster) []string {
	var out []string
	for k := range f.objs {
		out = append(out, k.gvk.Kind+"/"+k.name)
	}
	return out
}
