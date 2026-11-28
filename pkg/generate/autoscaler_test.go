package generate

import (
	"testing"

	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
)

const autoscalerManifest = `apiVersion: cluster.x-k8s.io/v1beta2
kind: Cluster
metadata:
  name: demo
---
apiVersion: cluster.x-k8s.io/v1beta2
kind: MachineDeployment
metadata:
  name: demo-workers
spec:
  replicas: 0
  template:
    spec:
      version: v1.33.4
---
apiVersion: cluster.x-k8s.io/v1beta2
kind: MachineDeployment
metadata:
  name: demo-workers-b
  annotations:
    keep.me/here: "1"
spec:
  template:
    spec:
      version: v1.33.4
`

func installObj(kind, name, namespace string) *unstructured.Unstructured {
	u := &unstructured.Unstructured{Object: map[string]any{}}
	u.SetAPIVersion("v1")
	u.SetKind(kind)
	u.SetName(name)
	if namespace != "" {
		u.SetNamespace(namespace)
	}
	return u
}

func TestSetAutoscalerStampsAndAppends(t *testing.T) {
	b := mustParse(t, autoscalerManifest)
	install := []*unstructured.Unstructured{
		installObj("ServiceAccount", "cluster-autoscaler", "kube-system"),
		installObj("Deployment", "cluster-autoscaler", "kube-system"),
	}
	anns := map[string]string{
		"cluster.x-k8s.io/cluster-api-autoscaler-node-group-min-size": "1",
		"cluster.x-k8s.io/cluster-api-autoscaler-node-group-max-size": "5",
	}

	count, err := SetAutoscaler(b, install, anns)
	if err != nil {
		t.Fatal(err)
	}
	if count != 2 {
		t.Fatalf("stamped %d MachineDeployments, want 2", count)
	}
	for _, name := range []string{"demo-workers", "demo-workers-b"} {
		md := findMD(t, b, name)
		got := md.GetAnnotations()
		if got["cluster.x-k8s.io/cluster-api-autoscaler-node-group-min-size"] != "1" ||
			got["cluster.x-k8s.io/cluster-api-autoscaler-node-group-max-size"] != "5" {
			t.Errorf("%s annotations = %v", name, got)
		}
	}
	// Existing annotations are preserved.
	if findMD(t, b, "demo-workers-b").GetAnnotations()["keep.me/here"] != "1" {
		t.Error("existing annotation dropped")
	}
	// The install objects were appended.
	if !hasObject(b, "ServiceAccount", "cluster-autoscaler") || !hasObject(b, "Deployment", "cluster-autoscaler") {
		t.Fatalf("install objects not appended: %d objects", len(b.Objects))
	}
}

func TestSetAutoscalerIdempotent(t *testing.T) {
	b := mustParse(t, autoscalerManifest)
	install := []*unstructured.Unstructured{installObj("ServiceAccount", "cluster-autoscaler", "kube-system")}
	anns := map[string]string{"cluster.x-k8s.io/cluster-api-autoscaler-node-group-min-size": "2"}
	if _, err := SetAutoscaler(b, install, anns); err != nil {
		t.Fatal(err)
	}
	first := len(b.Objects)
	if _, err := SetAutoscaler(b, install, anns); err != nil {
		t.Fatal(err)
	}
	if len(b.Objects) != first {
		t.Fatalf("second call changed object count from %d to %d (not idempotent)", first, len(b.Objects))
	}
}

func TestSetAutoscalerNoWorkers(t *testing.T) {
	b := mustParse(t, `apiVersion: cluster.x-k8s.io/v1beta2
kind: Cluster
metadata:
  name: demo
`)
	count, err := SetAutoscaler(b, []*unstructured.Unstructured{installObj("ServiceAccount", "cluster-autoscaler", "kube-system")}, nil)
	if err != nil {
		t.Fatal(err)
	}
	if count != 0 {
		t.Fatalf("want 0 stamped MachineDeployments, got %d", count)
	}
	if !hasObject(b, "ServiceAccount", "cluster-autoscaler") {
		t.Error("install object still appended when there are no workers")
	}
}

func findMD(t *testing.T, b *Bundle, name string) *unstructured.Unstructured {
	t.Helper()
	for _, md := range Find(b, ClusterGroup, "MachineDeployment") {
		if md.GetName() == name {
			return md
		}
	}
	t.Fatalf("MachineDeployment %q not found", name)
	return nil
}

func hasObject(b *Bundle, kind, name string) bool {
	for _, o := range b.Objects {
		if o.GetKind() == kind && o.GetName() == name {
			return true
		}
	}
	return false
}
