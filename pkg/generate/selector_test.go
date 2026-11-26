package generate

import (
	"reflect"
	"strings"
	"testing"

	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
)

const selectorManifest = `apiVersion: infrastructure.cluster.x-k8s.io/v1alpha1
kind: ProxmoxCluster
metadata:
  name: demo
spec:
  allowedNodes: [pve1, pve2]
---
apiVersion: infrastructure.cluster.x-k8s.io/v1alpha1
kind: ProxmoxMachineTemplate
metadata:
  name: cp
spec:
  template:
    spec:
      sourceNode: pve1
      templateID: 9010
      tags: [klotho-demo]
      numCores: 2
---
apiVersion: infrastructure.cluster.x-k8s.io/v1alpha1
kind: ProxmoxMachineTemplate
metadata:
  name: workers
spec:
  template:
    spec:
      sourceNode: pve1
      templateID: 9010
`

func TestSetMachineTemplateSelector(t *testing.T) {
	b := mustParse(t, selectorManifest)
	for range 2 {
		if err := SetMachineTemplateSelector(b, []string{"k8s", "ubuntu-24.04"}, ""); err != nil {
			t.Fatal(err)
		}
	}
	assertInSync(t, b)
	for _, o := range Find(b, InfrastructureGroup, "MachineTemplate") {
		spec, _, _ := unstructured.NestedMap(o.Object, "spec", "template", "spec")
		for _, gone := range []string{"templateID", "sourceNode", "templateMatchPolicy"} {
			if _, ok := spec[gone]; ok {
				t.Errorf("%s still has %s", o.GetName(), gone)
			}
		}
		tags, _, _ := unstructured.NestedStringSlice(o.Object, "spec", "template", "spec", "templateSelector", "matchTags")
		if !reflect.DeepEqual(tags, []string{"k8s", "ubuntu-24.04"}) {
			t.Errorf("%s matchTags = %v", o.GetName(), tags)
		}
	}
	if cores, _, _ := unstructured.NestedInt64(b.Objects[1].Object, "spec", "template", "spec", "numCores"); cores != 2 {
		t.Error("sibling field dropped")
	}
	if tags, _, _ := unstructured.NestedStringSlice(b.Objects[1].Object, "spec", "template", "spec", "tags"); !reflect.DeepEqual(tags, []string{"klotho-demo"}) {
		t.Errorf("VM tags changed: %v", tags)
	}
	if _, ok, _ := unstructured.NestedFieldNoCopy(b.Objects[0].Object, "spec", "template"); ok {
		t.Error("infrastructure cluster was modified")
	}
	want := "      templateSelector:\n        matchTags:\n        - k8s\n        - ubuntu-24.04\n"
	if !strings.Contains(string(b.YAML), want) {
		t.Errorf("YAML lacks the selector:\n%s", b.YAML)
	}
	if strings.Contains(string(b.YAML), "templateID") || strings.Contains(string(b.YAML), "sourceNode") {
		t.Errorf("YAML still names the fixed template:\n%s", b.YAML)
	}
}

func TestSetMachineTemplateSelectorPolicy(t *testing.T) {
	b := mustParse(t, selectorManifest)
	if err := SetMachineTemplateSelector(b, []string{"k8s"}, TemplateMatchBestSubset); err != nil {
		t.Fatal(err)
	}
	for _, o := range Find(b, InfrastructureGroup, "MachineTemplate") {
		if p, _, _ := nestedString(o, "spec", "template", "spec", "templateMatchPolicy"); p != TemplateMatchBestSubset {
			t.Errorf("%s policy = %q", o.GetName(), p)
		}
	}
	// Going back to the default removes the field again.
	if err := SetMachineTemplateSelector(b, []string{"k8s"}, TemplateMatchExact); err != nil {
		t.Fatal(err)
	}
	for _, o := range Find(b, InfrastructureGroup, "MachineTemplate") {
		if _, ok, _ := nestedString(o, "spec", "template", "spec", "templateMatchPolicy"); ok {
			t.Errorf("%s keeps the policy for exact", o.GetName())
		}
	}
	assertInSync(t, b)
}

func TestSetMachineTemplateSelectorErrors(t *testing.T) {
	for name, tc := range map[string]struct {
		manifest string
		tags     []string
		policy   string
		want     string
	}{
		"no tags":       {selectorManifest, nil, "", "at least one"},
		"bad tag":       {selectorManifest, []string{"a b"}, "", `"a b" is not a valid tag`},
		"bad policy":    {selectorManifest, []string{"k8s"}, "fuzzy", `"fuzzy" is not one of exact, uniqueSubset, bestSubset`},
		"no templates":  {"apiVersion: v1\nkind: ConfigMap\nmetadata:\n  name: x\n", []string{"k8s"}, "", "no infrastructure.cluster.x-k8s.io MachineTemplate"},
		"bad spec type": {"apiVersion: infrastructure.cluster.x-k8s.io/v1alpha1\nkind: XMachineTemplate\nmetadata:\n  name: x\nspec:\n  template:\n    spec: 1\n", []string{"k8s"}, "", "reading spec"},
	} {
		t.Run(name, func(t *testing.T) {
			err := SetMachineTemplateSelector(mustParse(t, tc.manifest), tc.tags, tc.policy)
			if err == nil || !strings.Contains(err.Error(), tc.want) {
				t.Fatalf("err = %v, want containing %q", err, tc.want)
			}
		})
	}
}

func TestNormalizeTemplateMatchPolicy(t *testing.T) {
	for in, want := range map[string]string{"": TemplateMatchExact, "exact": "exact", "uniqueSubset": "uniqueSubset", "bestSubset": "bestSubset"} {
		if got, err := NormalizeTemplateMatchPolicy(in); err != nil || got != want {
			t.Errorf("%q: %q, %v", in, got, err)
		}
	}
	if _, err := NormalizeTemplateMatchPolicy("Exact"); err == nil {
		t.Error("case-insensitive match accepted")
	}
}
