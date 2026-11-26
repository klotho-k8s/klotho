package generate

import (
	"errors"
	"reflect"
	"strings"
	"testing"

	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"

	"github.com/klotho-k8s/klotho/pkg/cluster"
)

const manifest = `apiVersion: cluster.x-k8s.io/v1beta2
kind: Cluster
metadata:
  name: demo
  namespace: default
---
apiVersion: infrastructure.cluster.x-k8s.io/v1alpha1
kind: ProxmoxCluster
metadata:
  name: demo
spec:
  controlPlaneEndpoint:
    host: 1.2.3.4
    port: 6443
  extra:
    nested: [1, 2]
---
apiVersion: infrastructure.cluster.x-k8s.io/v1alpha1
kind: ProxmoxMachineTemplate
metadata:
  name: cp
spec:
  template:
    spec:
      sourceNode: pve
---
apiVersion: infrastructure.cluster.x-k8s.io/v1alpha1
kind: ProxmoxMachineTemplate
metadata:
  name: workers
spec:
  template:
    spec:
      tags: [a]
`

func nestedString(o *unstructured.Unstructured, path ...string) (string, bool, error) {
	return unstructured.NestedString(o.Object, path...)
}

func mustParse(t *testing.T, s string) *Bundle {
	t.Helper()
	b, err := ParseBundle([]byte(s))
	if err != nil {
		t.Fatal(err)
	}
	return b
}

func assertInSync(t *testing.T, b *Bundle) {
	t.Helper()
	again, err := ParseBundle(b.YAML)
	if err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(again.Objects, b.Objects) {
		t.Fatal("YAML does not match Objects")
	}
}

func TestParseBundleSkipsEmptyDocuments(t *testing.T) {
	b := mustParse(t, "---\n\n---\n# comment only\n---\n"+manifest+"---\n")
	if len(b.Objects) != 4 {
		t.Fatalf("got %d objects", len(b.Objects))
	}
	if b.Objects[3].GetName() != "workers" {
		t.Fatal("order not kept")
	}
}

func TestParseBundleRejectsKindless(t *testing.T) {
	if _, err := ParseBundle([]byte("foo: bar\n")); err == nil {
		t.Fatal("expected error")
	}
}

func TestRenderRoundTrip(t *testing.T) {
	b := mustParse(t, manifest)
	out, err := b.Render()
	if err != nil {
		t.Fatal(err)
	}
	if strings.Count(string(out), "\n---\n") != 3 {
		t.Fatalf("unexpected separators:\n%s", out)
	}
	assertInSync(t, b)
}

func TestSetControlPlaneEndpoint(t *testing.T) {
	b := mustParse(t, manifest)
	if err := SetControlPlaneEndpoint(b, cluster.Endpoint{Host: "10.1.1.1", Port: 8443}); err != nil {
		t.Fatal(err)
	}
	obj := b.Objects[1]
	host, _, _ := unstructured.NestedString(obj.Object, "spec", "controlPlaneEndpoint", "host")
	port, _, _ := unstructured.NestedInt64(obj.Object, "spec", "controlPlaneEndpoint", "port")
	if host != "10.1.1.1" || port != 8443 {
		t.Fatalf("got %s:%d", host, port)
	}
	if _, ok, _ := unstructured.NestedSlice(obj.Object, "spec", "extra", "nested"); !ok {
		t.Fatal("unknown field dropped")
	}
	if !strings.Contains(string(b.YAML), "10.1.1.1") {
		t.Fatal("YAML not re-rendered")
	}
	assertInSync(t, b)
}

func TestSetControlPlaneEndpointErrors(t *testing.T) {
	b := mustParse(t, manifest)
	b.Objects = append(b.Objects[:1], b.Objects[2:]...)
	if err := SetControlPlaneEndpoint(b, cluster.Endpoint{Host: "h", Port: 1}); err == nil {
		t.Fatal("expected error with no infrastructure cluster")
	}
	b = mustParse(t, manifest)
	b.Objects = append(b.Objects, b.Objects[1].DeepCopy())
	if err := SetControlPlaneEndpoint(b, cluster.Endpoint{Host: "h", Port: 1}); err == nil {
		t.Fatal("expected error with two infrastructure clusters")
	}
}

func TestAddMachineTemplateTag(t *testing.T) {
	b := mustParse(t, manifest)
	for range 2 {
		if err := AddMachineTemplateTag(b, "klotho"); err != nil {
			t.Fatal(err)
		}
	}
	cp, _, _ := unstructured.NestedStringSlice(b.Objects[2].Object, "spec", "template", "spec", "tags")
	w, _, _ := unstructured.NestedStringSlice(b.Objects[3].Object, "spec", "template", "spec", "tags")
	if !reflect.DeepEqual(cp, []string{"klotho"}) || !reflect.DeepEqual(w, []string{"a", "klotho"}) {
		t.Fatalf("tags cp=%v workers=%v", cp, w)
	}
	if src, _, _ := nestedString(b.Objects[2], "spec", "template", "spec", "sourceNode"); src != "pve" {
		t.Fatal("sibling field dropped")
	}
	if _, ok, _ := unstructured.NestedFieldNoCopy(b.Objects[1].Object, "spec", "template"); ok {
		t.Fatal("infrastructure cluster was modified")
	}
	assertInSync(t, b)
}

func TestSetMachineTemplatePool(t *testing.T) {
	b := mustParse(t, manifest)
	if err := SetMachineTemplatePool(b, "k8s"); err != nil {
		t.Fatal(err)
	}
	for _, o := range Find(b, InfrastructureGroup, "MachineTemplate") {
		if p, _, _ := nestedString(o, "spec", "template", "spec", "pool"); p != "k8s" {
			t.Fatalf("%s pool %q", o.GetName(), p)
		}
	}
	assertInSync(t, b)
}

func TestFind(t *testing.T) {
	b := mustParse(t, manifest)
	if n := len(Find(b, InfrastructureGroup, "MachineTemplate")); n != 2 {
		t.Fatalf("found %d templates", n)
	}
	if _, err := FindOne(b, InfrastructureGroup, "MachineTemplate"); err == nil {
		t.Fatal("expected error for two matches")
	}
	c, err := FindOne(b, ClusterGroup, "Cluster")
	if err != nil || c.GetKind() != "Cluster" {
		t.Fatalf("FindOne Cluster: %v", err)
	}
}

func TestValidate(t *testing.T) {
	b := mustParse(t, manifest)
	if err := AddMachineTemplateTag(b, "klotho"); err != nil {
		t.Fatal(err)
	}
	if err := Validate(b, "demo", "klotho"); err != nil {
		t.Fatalf("valid manifest rejected: %v", err)
	}
}

func TestValidateReportsAllProblems(t *testing.T) {
	b := mustParse(t, manifest)
	// Drop the infrastructure cluster; leave the cluster misnamed and the
	// templates untagged.
	b.Objects = append(b.Objects[:1], b.Objects[2:]...)
	err := Validate(b, "other", "klotho")
	var ve *ValidationError
	if !errors.As(err, &ve) {
		t.Fatalf("expected ValidationError, got %v", err)
	}
	if len(ve.Problems) != 4 {
		t.Fatalf("got %d problems: %v", len(ve.Problems), err)
	}
	for _, s := range []string{`expected "other"`, "*Cluster", `"cp" lacks tag`, `"workers" lacks tag`} {
		if !strings.Contains(err.Error(), s) {
			t.Errorf("error lacks %q: %v", s, err)
		}
	}
}

func TestValidateStructure(t *testing.T) {
	b := mustParse(t, "apiVersion: v1\nkind: ConfigMap\nmetadata:\n  name: x\n")
	err := Validate(b, "demo", "klotho")
	var ve *ValidationError
	if !errors.As(err, &ve) || len(ve.Problems) != 3 {
		t.Fatalf("expected 3 problems, got %v", err)
	}
}

func TestSetClusterNetwork(t *testing.T) {
	b := mustParse(t, `apiVersion: cluster.x-k8s.io/v1beta2
kind: Cluster
metadata:
  name: demo
spec:
  clusterNetwork:
    pods:
      cidrBlocks: ["192.168.0.0/16"]
---
apiVersion: infrastructure.cluster.x-k8s.io/v1alpha1
kind: ProxmoxCluster
metadata:
  name: demo
`)
	if err := SetClusterNetwork(b, []string{"10.244.0.0/16"}, nil); err != nil {
		t.Fatal(err)
	}
	assertInSync(t, b)
	obj, err := FindOne(b, ClusterGroup, "Cluster")
	if err != nil {
		t.Fatal(err)
	}
	pods, services, err := ClusterNetwork(obj)
	if err != nil || len(pods) != 1 || pods[0] != "10.244.0.0/16" || services != nil {
		t.Fatalf("pods=%v services=%v err=%v", pods, services, err)
	}
	if err := SetClusterNetwork(b, nil, []string{"10.96.0.0/12", "fd00::/108"}); err != nil {
		t.Fatal(err)
	}
	pods, services, err = ClusterNetwork(obj)
	if err != nil || pods[0] != "10.244.0.0/16" || len(services) != 2 {
		t.Fatalf("pods=%v services=%v err=%v", pods, services, err)
	}
	if !strings.Contains(string(b.YAML), "fd00::/108") {
		t.Fatalf("YAML not re-rendered:\n%s", b.YAML)
	}
	if p, s, err := ClusterNetwork(nil); p != nil || s != nil || err != nil {
		t.Fatalf("nil object: %v %v %v", p, s, err)
	}
}
