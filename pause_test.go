package klotho

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime/schema"

	"github.com/klotho-k8s/klotho/pkg/cluster"
	"github.com/klotho-k8s/klotho/pkg/kube"
	"github.com/klotho-k8s/klotho/pkg/state"
	"github.com/klotho-k8s/klotho/provider"
)

// pauseHarness wires a fake store (with a non-reserved record) and a fake
// cluster the pause commands drive through ConnectAdmin.
type pauseHarness struct {
	rec   *recorder
	store *fakeStore
	kube  *fakeCluster
	deps  Deps
}

func newPauseHarness(t *testing.T) *pauseHarness {
	t.Helper()
	rec := &recorder{}
	store := newFakeStore(rec)
	st := &cluster.State{
		Spec:     cluster.Spec{Name: "demo", Provider: "fake", ControlPlane: "kubeadm"},
		Endpoint: cluster.Endpoint{Host: "10.0.0.10", Port: 6443},
		CA:       cluster.KeyPair{Cert: []byte("CA-CERT"), Key: []byte("CA-KEY")},
	}
	sealed, err := state.PlainCodec{}.Seal(st)
	if err != nil {
		t.Fatal(err)
	}
	store.records["demo"] = sealed
	fc := newFakeCluster(rec, "demo")
	h := &pauseHarness{rec: rec, store: store, kube: fc}
	h.deps = Deps{
		Store: store,
		Codec: state.PlainCodec{},
		ConnectAdmin: func(_ context.Context, s *cluster.State) (kube.Cluster, error) {
			rec.add("kube.ConnectAdmin %s", s.Spec.Name)
			return fc, nil
		},
		PollInterval: time.Millisecond,
	}
	return h
}

// seedCluster adds the demo Cluster object to the fake, paused or not.
func (h *pauseHarness) seedCluster(paused bool) *unstructured.Unstructured {
	u := &unstructured.Unstructured{Object: map[string]any{}}
	u.SetGroupVersionKind(clusterGVK)
	u.SetName("demo")
	u.SetNamespace("default")
	_ = unstructured.SetNestedMap(u.Object, map[string]any{
		"apiGroup": ControlPlaneGroup,
		"kind":     "KubeadmControlPlane",
		"name":     "demo-control-plane",
	}, "spec", "controlPlaneRef")
	if paused {
		_ = unstructured.SetNestedField(u.Object, true, "spec", "paused")
	}
	h.put(u)
	return u
}

func (h *pauseHarness) put(u *unstructured.Unstructured) {
	h.kube.objs[keyOf(u)] = u
}

// machine builds a Cluster API Machine for the demo cluster.
func machine(name string, opts ...func(*unstructured.Unstructured)) *unstructured.Unstructured {
	u := &unstructured.Unstructured{Object: map[string]any{}}
	u.SetGroupVersionKind(machineGVK)
	u.SetName(name)
	u.SetNamespace("default")
	u.SetLabels(map[string]string{clusterNameLabel: "demo"})
	for _, o := range opts {
		o(u)
	}
	return u
}

func controlPlaneMachine(u *unstructured.Unstructured) {
	l := u.GetLabels()
	l[controlPlaneLabel] = ""
	u.SetLabels(l)
}

func deleting(u *unstructured.Unstructured) {
	now := metav1.NewTime(time.Now())
	u.SetDeletionTimestamp(&now)
}

func ready(u *unstructured.Unstructured) {
	_ = unstructured.SetNestedSlice(u.Object, []any{map[string]any{"type": "Ready", "status": "True"}}, "status", "conditions")
}

func notReady(u *unstructured.Unstructured) {
	_ = unstructured.SetNestedSlice(u.Object, []any{map[string]any{"type": "Ready", "status": "False"}}, "status", "conditions")
}

func TestPauseSetsPaused(t *testing.T) {
	h := newPauseHarness(t)
	h.seedCluster(false)
	res, err := Pause(t.Context(), h.deps, "demo", PauseOptions{})
	if err != nil {
		t.Fatal(err)
	}
	if !res.Changed || !res.Paused {
		t.Fatalf("result: %+v", res)
	}
	if !h.rec.has("kube.Apply klotho-pause Cluster demo paused=true") {
		t.Fatalf("did not apply spec.paused=true under the pause manager; calls:\n  %s", strings.Join(h.rec.list(), "\n  "))
	}
	// The apply must own only spec.paused: no controlPlaneRef re-sent.
	for _, c := range h.rec.list() {
		if strings.HasPrefix(c, "kube.Apply klotho ") {
			t.Fatalf("pause re-applied the whole object under the klotho manager: %q", c)
		}
	}
	if !clusterPaused(h.kube.objs[clusterKey()]) {
		t.Fatal("spec.paused not true on the stored Cluster")
	}
}

func TestUnpauseClearsPaused(t *testing.T) {
	h := newPauseHarness(t)
	h.seedCluster(true)
	res, err := Unpause(t.Context(), h.deps, "demo", PauseOptions{})
	if err != nil {
		t.Fatal(err)
	}
	if !res.Changed || res.Paused {
		t.Fatalf("result: %+v", res)
	}
	if !h.rec.has("kube.Apply klotho-pause Cluster demo paused=false") {
		t.Fatalf("did not clear spec.paused under the pause manager; calls:\n  %s", strings.Join(h.rec.list(), "\n  "))
	}
	if clusterPaused(h.kube.objs[clusterKey()]) {
		t.Fatal("spec.paused still true after unpause")
	}
}

func TestPauseAlreadyPaused(t *testing.T) {
	h := newPauseHarness(t)
	u := h.seedCluster(true)
	// Model a clusterctl move pause: spec.paused owned by another manager.
	_ = unstructured.SetNestedSlice(u.Object, []any{map[string]any{
		"manager":   "clusterctl",
		"operation": "Update",
		"fieldsV1":  map[string]any{"f:spec": map[string]any{"f:paused": map[string]any{}}},
	}}, "metadata", "managedFields")
	h.put(u)
	res, err := Pause(t.Context(), h.deps, "demo", PauseOptions{})
	if err != nil {
		t.Fatal(err)
	}
	if res.Changed {
		t.Fatal("re-paused an already-paused cluster")
	}
	if res.SetBy != "clusterctl" {
		t.Fatalf("SetBy = %q, want clusterctl", res.SetBy)
	}
	if h.rec.has("kube.Apply klotho-pause") {
		t.Fatalf("clobbered an existing pause; calls:\n  %s", strings.Join(h.rec.list(), "\n  "))
	}
	if len(res.Findings) != 1 || !strings.Contains(res.Findings[0].Message, "clusterctl") {
		t.Fatalf("findings: %+v", res.Findings)
	}
}

func TestUnpauseNotPaused(t *testing.T) {
	h := newPauseHarness(t)
	h.seedCluster(false)
	res, err := Unpause(t.Context(), h.deps, "demo", PauseOptions{})
	if err != nil {
		t.Fatal(err)
	}
	if res.Changed || res.Paused {
		t.Fatalf("result: %+v", res)
	}
	if h.rec.has("kube.Apply klotho-pause") {
		t.Fatal("unpause applied to a cluster that was not paused")
	}
}

func TestUnpauseReportsLingeringAnnotation(t *testing.T) {
	h := newPauseHarness(t)
	h.seedCluster(true)
	m := machine("demo-cp-0", controlPlaneMachine, ready)
	m.SetAnnotations(map[string]string{pausedAnnotation: "true"})
	h.put(m)
	res, err := Unpause(t.Context(), h.deps, "demo", PauseOptions{})
	if err != nil {
		t.Fatal(err)
	}
	if !res.Changed {
		t.Fatal("unpause did not clear spec.paused")
	}
	var found bool
	for _, f := range res.Findings {
		if strings.Contains(f.Field, pausedAnnotation) && strings.Contains(f.Object, "demo-cp-0") {
			found = true
		}
	}
	if !found {
		t.Fatalf("lingering annotation not reported; findings: %+v", res.Findings)
	}
}

func TestPauseGuards(t *testing.T) {
	cpDeleting := func(h *pauseHarness) { h.put(machine("demo-cp-0", controlPlaneMachine, deleting, ready)) }
	clusterDeleting := func(h *pauseHarness) {
		u := h.seedCluster(false)
		deleting(u)
		h.put(u)
	}
	midRollout := func(h *pauseHarness) {
		cp := &unstructured.Unstructured{Object: map[string]any{}}
		cp.SetGroupVersionKind(controlPlaneGVK())
		cp.SetName("demo-control-plane")
		cp.SetNamespace("default")
		_ = unstructured.SetNestedField(cp.Object, "v1.34.0", "spec", "version")
		_ = unstructured.SetNestedField(cp.Object, "v1.33.4", "status", "version")
		h.put(cp)
	}
	unhealthy := func(h *pauseHarness) { h.put(machine("demo-cp-0", controlPlaneMachine, notReady)) }

	tests := []struct {
		name       string
		seed       func(*pauseHarness)
		force      bool
		wantBlock  bool
		wantPaused bool // whether spec.paused ended up applied
		wantField  string
	}{
		{name: "healthy", seed: func(h *pauseHarness) { h.put(machine("demo-cp-0", controlPlaneMachine, ready)) }, wantPaused: true},
		{name: "cluster deleting blocks", seed: clusterDeleting, wantBlock: true, wantField: "metadata.deletionTimestamp"},
		{name: "cluster deleting with force", seed: clusterDeleting, force: true, wantPaused: true, wantField: "metadata.deletionTimestamp"},
		{name: "machine deleting blocks", seed: cpDeleting, wantBlock: true, wantField: "metadata.deletionTimestamp"},
		{name: "machine deleting with force", seed: cpDeleting, force: true, wantPaused: true, wantField: "metadata.deletionTimestamp"},
		{name: "mid rollout warns", seed: midRollout, wantPaused: true, wantField: "spec.version"},
		{name: "unhealthy warns", seed: unhealthy, wantPaused: true, wantField: "status.conditions[Ready]"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			h := newPauseHarness(t)
			// clusterDeleting seeds its own cluster; others need the default.
			if _, ok := h.kube.objs[clusterKey()]; !ok {
				h.seedCluster(false)
			}
			tt.seed(h)
			res, err := Pause(t.Context(), h.deps, "demo", PauseOptions{Force: tt.force})
			if tt.wantBlock {
				var blocked *PauseBlockedError
				if !errors.As(err, &blocked) {
					t.Fatalf("want PauseBlockedError, got %v", err)
				}
				if h.rec.has("kube.Apply klotho-pause") {
					t.Fatal("blocked pause still applied spec.paused")
				}
				if !findingHasField(blocked.Findings, tt.wantField) {
					t.Fatalf("blocking finding %q not present: %+v", tt.wantField, blocked.Findings)
				}
				return
			}
			if err != nil {
				t.Fatal(err)
			}
			applied := h.rec.has("kube.Apply klotho-pause Cluster demo paused=true")
			if applied != tt.wantPaused {
				t.Fatalf("applied=%v want %v; calls:\n  %s", applied, tt.wantPaused, strings.Join(h.rec.list(), "\n  "))
			}
			if tt.wantField != "" && !findingHasField(res.Findings, tt.wantField) {
				t.Fatalf("finding %q not present: %+v", tt.wantField, res.Findings)
			}
		})
	}
}

func clusterKey() objKey {
	u := &unstructured.Unstructured{Object: map[string]any{}}
	u.SetGroupVersionKind(clusterGVK)
	u.SetName("demo")
	u.SetNamespace("default")
	return keyOf(u)
}

func controlPlaneGVK() schema.GroupVersionKind {
	return schema.GroupVersionKind{Group: ControlPlaneGroup, Version: clusterContractVersion, Kind: "KubeadmControlPlane"}
}

func findingHasField(findings []provider.Finding, field string) bool {
	for _, f := range findings {
		if f.Field == field {
			return true
		}
	}
	return false
}
