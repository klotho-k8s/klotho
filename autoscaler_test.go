package klotho

import (
	"context"
	"log/slog"
	"strconv"
	"strings"
	"testing"

	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"

	"github.com/klotho-k8s/klotho/pkg/autoscaler"
	"github.com/klotho-k8s/klotho/pkg/cluster"
	"github.com/klotho-k8s/klotho/pkg/generate"
)

const (
	minAnn = autoscaler.MinSizeAnnotation
	maxAnn = autoscaler.MaxSizeAnnotation
)

// --- day-2 install -----------------------------------------------------------

func TestUpgradeAutoscalerInstalls(t *testing.T) {
	deps, f := upgradeHarness(t)
	seed(f,
		kcpObject("demo-control-plane", "v1.33.4", 1),
		mdObject("demo-md-0", "v1.33.4", 0),
	)
	err := Upgrade(context.Background(), deps, "demo", UpgradeOptions{
		Autoscaler: AutoscalerChange{Set: true, Enabled: true, Min: 2, MinSet: true, Max: 7, MaxSet: true},
	})
	if err != nil {
		t.Fatal(err)
	}
	// The core add-on objects were applied.
	for _, want := range []string{
		"kube.Apply klotho ServiceAccount cluster-autoscaler",
		"kube.Apply klotho ClusterRole cluster-autoscaler-management",
		"kube.Apply klotho ClusterRole cluster-autoscaler-workload",
		"kube.Apply klotho Deployment cluster-autoscaler",
	} {
		if !f.rec.has(want) {
			t.Errorf("missing apply %q; calls:\n  %s", want, strings.Join(f.rec.list(), "\n  "))
		}
	}
	// The worker pool was stamped with the requested bounds.
	md := f.objs[keyOf(mdObject("demo-md-0", "", 0))]
	if got := md.GetAnnotations()[minAnn]; got != "2" {
		t.Errorf("min annotation = %q, want 2", got)
	}
	if got := md.GetAnnotations()[maxAnn]; got != "7" {
		t.Errorf("max annotation = %q, want 7", got)
	}
	// The autoscaler image tracks the control plane's Kubernetes version.
	deploy := f.objs[keyOf(deployKey())]
	containers, _, _ := unstructured.NestedSlice(deploy.Object, "spec", "template", "spec", "containers")
	if got := containers[0].(map[string]any)["image"]; got != "registry.k8s.io/autoscaling/cluster-autoscaler:v1.33.4" {
		t.Errorf("autoscaler image = %v", got)
	}
}

func TestUpgradeAutoscalerRetuneKeepsUnsetBound(t *testing.T) {
	deps, f := upgradeHarness(t)
	md := mdObject("demo-md-0", "v1.33.4", 3)
	md.SetAnnotations(map[string]string{minAnn: "1", maxAnn: "9"})
	seed(f, kcpObject("demo-control-plane", "v1.33.4", 1), md)

	// Only --autoscaler-max is set: min stays at the existing 1.
	err := Upgrade(context.Background(), deps, "demo", UpgradeOptions{
		Autoscaler: AutoscalerChange{Max: 4, MaxSet: true},
	})
	if err != nil {
		t.Fatal(err)
	}
	got := f.objs[keyOf(mdObject("demo-md-0", "", 0))].GetAnnotations()
	if got[minAnn] != "1" {
		t.Errorf("min changed to %q, want kept at 1", got[minAnn])
	}
	if got[maxAnn] != "4" {
		t.Errorf("max = %q, want 4", got[maxAnn])
	}
}

func TestUpgradeAutoscalerRemoves(t *testing.T) {
	deps, f := upgradeHarness(t)
	md := mdObject("demo-md-0", "v1.33.4", 2)
	md.SetAnnotations(map[string]string{minAnn: "1", maxAnn: "5"})
	seed(f, kcpObject("demo-control-plane", "v1.33.4", 1), md)

	err := Upgrade(context.Background(), deps, "demo", UpgradeOptions{
		Autoscaler: AutoscalerChange{Set: true, Enabled: false},
	})
	if err != nil {
		t.Fatal(err)
	}
	// The add-on objects were deleted.
	for _, want := range []string{
		"kube.Delete ServiceAccount cluster-autoscaler",
		"kube.Delete Deployment cluster-autoscaler",
		"kube.Delete ClusterRole cluster-autoscaler-management",
	} {
		if !f.rec.has(want) {
			t.Errorf("missing delete %q; calls:\n  %s", want, strings.Join(f.rec.list(), "\n  "))
		}
	}
	// The bounds were stripped from the pool.
	got := f.objs[keyOf(mdObject("demo-md-0", "", 0))].GetAnnotations()
	if _, ok := got[minAnn]; ok {
		t.Errorf("min annotation not cleared: %v", got)
	}
	if _, ok := got[maxAnn]; ok {
		t.Errorf("max annotation not cleared: %v", got)
	}
}

func TestUpgradeAutoscalerNoWorkersStillInstalls(t *testing.T) {
	deps, f := upgradeHarness(t)
	seed(f, kcpObject("demo-control-plane", "v1.33.4", 1))
	err := Upgrade(context.Background(), deps, "demo", UpgradeOptions{
		Autoscaler: AutoscalerChange{Set: true, Enabled: true},
	})
	if err != nil {
		t.Fatal(err)
	}
	if !f.rec.has("kube.Apply klotho Deployment cluster-autoscaler") {
		t.Error("autoscaler not installed when there are no workers")
	}
}

func TestUpgradeAutoscalerImageBump(t *testing.T) {
	deps, f := upgradeHarness(t)
	seed(f, kcpObject("demo-control-plane", "v1.33.4", 1), mdObject("demo-md-0", "v1.33.4", 1))
	err := Upgrade(context.Background(), deps, "demo", UpgradeOptions{
		Autoscaler: AutoscalerChange{Version: "v1.33.9"},
	})
	if err != nil {
		t.Fatal(err)
	}
	containers, _, _ := unstructured.NestedSlice(f.objs[keyOf(deployKey())].Object, "spec", "template", "spec", "containers")
	if got := containers[0].(map[string]any)["image"]; got != "registry.k8s.io/autoscaling/cluster-autoscaler:v1.33.9" {
		t.Errorf("image = %v, want the bumped tag", got)
	}
}

// --- create wiring -----------------------------------------------------------

func TestCreateWithAutoscaler(t *testing.T) {
	b, log := bundleWithWorkers(t)
	req := CreateRequest{
		Kubernetes:        "v1.33.4",
		Autoscaler:        true,
		AutoscalerOptions: autoscaler.Options{MinSize: 1, MaxSize: 4},
	}
	spec := clusterSpec("demo")
	if err := addAutoscaler(b, req, spec, log); err != nil {
		t.Fatal(err)
	}
	// Worker pool stamped.
	md := findBundleMD(t, b, "demo-md-0")
	if md.GetAnnotations()[minAnn] != "1" || md.GetAnnotations()[maxAnn] != "4" {
		t.Errorf("worker annotations = %v", md.GetAnnotations())
	}
	// Add-on objects present in the bundle.
	if !bundleHas(b, "Deployment", "cluster-autoscaler") || !bundleHas(b, "ServiceAccount", "cluster-autoscaler") {
		t.Error("autoscaler install objects not added to the bundle")
	}
}

func TestCreateAutoscalerDefaults(t *testing.T) {
	b, log := bundleWithWorkers(t)
	req := CreateRequest{Kubernetes: "v1.33.4", Autoscaler: true}
	if err := addAutoscaler(b, req, clusterSpec("demo"), log); err != nil {
		t.Fatal(err)
	}
	md := findBundleMD(t, b, "demo-md-0")
	if md.GetAnnotations()[minAnn] != strconv.Itoa(autoscaler.DefaultMinSize) ||
		md.GetAnnotations()[maxAnn] != strconv.Itoa(autoscaler.DefaultMaxSize) {
		t.Errorf("default bounds = %v, want %d/%d", md.GetAnnotations(), autoscaler.DefaultMinSize, autoscaler.DefaultMaxSize)
	}
}

// --- helpers -----------------------------------------------------------------

// deployKey is the object-store key of the autoscaler Deployment.
func deployKey() *unstructured.Unstructured {
	u := &unstructured.Unstructured{Object: map[string]any{}}
	u.SetAPIVersion("apps/v1")
	u.SetKind("Deployment")
	u.SetNamespace(autoscaler.DefaultNamespace)
	u.SetName(autoscaler.Name)
	return u
}

func clusterSpec(name string) cluster.Spec { return cluster.Spec{Name: name} }

// bundleWithWorkers returns a minimal generated bundle holding a Cluster and one
// worker MachineDeployment, plus a discard logger.
func bundleWithWorkers(t *testing.T) (*generate.Bundle, *slog.Logger) {
	t.Helper()
	b, err := generate.ParseBundle([]byte(`apiVersion: cluster.x-k8s.io/v1beta2
kind: Cluster
metadata:
  name: demo
  namespace: default
---
apiVersion: cluster.x-k8s.io/v1beta2
kind: MachineDeployment
metadata:
  name: demo-md-0
  namespace: default
spec:
  replicas: 2
  template:
    spec:
      version: v1.33.4
`))
	if err != nil {
		t.Fatal(err)
	}
	return b, slog.New(slog.DiscardHandler)
}

func findBundleMD(t *testing.T, b *generate.Bundle, name string) *unstructured.Unstructured {
	t.Helper()
	for _, md := range generate.Find(b, generate.ClusterGroup, "MachineDeployment") {
		if md.GetName() == name {
			return md
		}
	}
	t.Fatalf("MachineDeployment %q not in bundle", name)
	return nil
}

func bundleHas(b *generate.Bundle, kind, name string) bool {
	for _, o := range b.Objects {
		if o.GetKind() == kind && o.GetName() == name {
			return true
		}
	}
	return false
}
