package autoscaler

import (
	"strings"
	"testing"

	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
)

func TestMinMaxAnnotations(t *testing.T) {
	cases := []struct {
		min, max         int
		wantMin, wantMax string
	}{
		{1, 5, "1", "5"},
		{0, 3, "0", "3"},
		{2, 2, "2", "2"},
	}
	for _, tc := range cases {
		got := MinMaxAnnotations(tc.min, tc.max)
		if got[MinSizeAnnotation] != tc.wantMin || got[MaxSizeAnnotation] != tc.wantMax {
			t.Errorf("MinMaxAnnotations(%d,%d) = %v", tc.min, tc.max, got)
		}
	}
	// Exact annotation keys (regression guard against a typo).
	if MinSizeAnnotation != "cluster.x-k8s.io/cluster-api-autoscaler-node-group-min-size" {
		t.Errorf("min annotation key = %q", MinSizeAnnotation)
	}
	if MaxSizeAnnotation != "cluster.x-k8s.io/cluster-api-autoscaler-node-group-max-size" {
		t.Errorf("max annotation key = %q", MaxSizeAnnotation)
	}
}

func TestCapacityAnnotationKeys(t *testing.T) {
	// The exact scale-from-zero keys, including the single camelCase one.
	want := map[string]string{
		"cpu":            CapacityCPU,
		"memory":         CapacityMemory,
		"ephemeral-disk": CapacityEphemeralDisk,
		"maxPods":        CapacityMaxPods,
		"gpu-type":       CapacityGPUType,
		"gpu-count":      CapacityGPUCount,
		"labels":         CapacityLabels,
		"taints":         CapacityTaints,
	}
	for bare, full := range want {
		if full != "capacity.cluster-autoscaler.kubernetes.io/"+bare {
			t.Errorf("capacity key for %q = %q", bare, full)
		}
	}
	got := CapacityAnnotations(map[string]string{"cpu": "4", "memory": "8Gi", "gpu-count": ""})
	if got[CapacityCPU] != "4" || got[CapacityMemory] != "8Gi" {
		t.Errorf("CapacityAnnotations values = %v", got)
	}
	if _, ok := got[CapacityGPUCount]; ok {
		t.Error("empty capacity value should be skipped")
	}
}

func TestOptionsImage(t *testing.T) {
	cases := []struct {
		name string
		o    Options
		want string
		err  bool
	}{
		{"version wins", Options{Version: "v1.33.4", KubernetesVersion: "v1.33.9"}, "registry.k8s.io/autoscaling/cluster-autoscaler:v1.33.4", false},
		{"derive from k8s", Options{KubernetesVersion: "v1.31.2"}, "registry.k8s.io/autoscaling/cluster-autoscaler:v1.31.2", false},
		{"image overrides all", Options{Image: "ghcr.io/me/ca:dev", Version: "v1.33.4"}, "ghcr.io/me/ca:dev", false},
		{"no tag at all", Options{}, "", true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got, err := tc.o.image()
			if tc.err {
				if err == nil {
					t.Fatal("want error")
				}
				return
			}
			if err != nil {
				t.Fatal(err)
			}
			if got != tc.want {
				t.Errorf("image = %q, want %q", got, tc.want)
			}
		})
	}
}

func TestOptionsValidate(t *testing.T) {
	cases := []struct {
		name string
		o    Options
		ok   bool
	}{
		{"defaults ok", Options{ClusterName: "demo", KubernetesVersion: "v1.33.4"}, true},
		{"explicit ok", Options{ClusterName: "demo", MinSize: 2, MaxSize: 9, KubernetesVersion: "v1.33.4"}, true},
		{"scale to zero ok", Options{ClusterName: "demo", MinSize: 0, MaxSize: 3, KubernetesVersion: "v1.33.4"}, true},
		{"no cluster", Options{KubernetesVersion: "v1.33.4"}, false},
		{"min over max", Options{ClusterName: "demo", MinSize: 9, MaxSize: 2, KubernetesVersion: "v1.33.4"}, false},
		{"negative min", Options{ClusterName: "demo", MinSize: -1, MaxSize: 2, KubernetesVersion: "v1.33.4"}, false},
		{"no image tag", Options{ClusterName: "demo"}, false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			err := tc.o.Validate()
			if tc.ok && err != nil {
				t.Fatalf("want ok, got %v", err)
			}
			if !tc.ok && err == nil {
				t.Fatal("want error")
			}
		})
	}
}

func TestInstallObjects(t *testing.T) {
	objs, err := InstallObjects(Options{ClusterName: "demo", KubernetesVersion: "v1.33.4", MachineNamespace: "default"})
	if err != nil {
		t.Fatal(err)
	}
	byKind := map[string]*unstructured.Unstructured{}
	for _, o := range objs {
		byKind[o.GetKind()] = o
	}
	for _, kind := range []string{"ServiceAccount", "ClusterRole", "ClusterRoleBinding", "Deployment"} {
		if byKind[kind] == nil {
			t.Fatalf("missing %s; got kinds %v", kind, kinds(objs))
		}
	}
	// Two ClusterRoles and two bindings (management + workload).
	if n := countKind(objs, "ClusterRole"); n != 2 {
		t.Errorf("ClusterRoles = %d, want 2", n)
	}
	if n := countKind(objs, "ClusterRoleBinding"); n != 2 {
		t.Errorf("ClusterRoleBindings = %d, want 2", n)
	}

	// The Deployment runs the clusterapi provider, scoped to the cluster.
	deploy := byKind["Deployment"]
	containers, _, _ := unstructured.NestedSlice(deploy.Object, "spec", "template", "spec", "containers")
	if len(containers) != 1 {
		t.Fatalf("containers = %d", len(containers))
	}
	container := containers[0].(map[string]any)
	args := toStrings(container["command"])
	joined := strings.Join(args, " ")
	if !strings.Contains(joined, "--cloud-provider=clusterapi") {
		t.Errorf("command missing clusterapi cloud provider: %v", args)
	}
	if !strings.Contains(joined, "clusterapi:namespace=default,clusterName=demo") {
		t.Errorf("auto-discovery not scoped to the cluster: %v", args)
	}
	if got := container["image"]; got != "registry.k8s.io/autoscaling/cluster-autoscaler:v1.33.4" {
		t.Errorf("image = %v", got)
	}
	// Deployment uses the ServiceAccount and runs in kube-system.
	if sa, _, _ := unstructured.NestedString(deploy.Object, "spec", "template", "spec", "serviceAccountName"); sa != Name {
		t.Errorf("serviceAccountName = %q", sa)
	}
	if deploy.GetNamespace() != DefaultNamespace {
		t.Errorf("deployment namespace = %q", deploy.GetNamespace())
	}

	// The management ClusterRole grants the CAPI access, including the scale
	// subresource and the provider-agnostic infra-template read for
	// scale-from-zero.
	mgmt := findRole(objs, managementRole)
	if mgmt == nil {
		t.Fatalf("no %s ClusterRole", managementRole)
	}
	if !ruleGrants(mgmt, "cluster.x-k8s.io", "machinedeployments/scale", "update") {
		t.Error("management role missing machinedeployments/scale update")
	}
	if !ruleGrants(mgmt, "cluster.x-k8s.io", "machinedeployments", "patch") {
		t.Error("management role missing machinedeployments patch")
	}
	if !ruleGrants(mgmt, "infrastructure.cluster.x-k8s.io", "*", "get") {
		t.Error("management role missing infra template read (scale-from-zero enabler)")
	}

	// The workload ClusterRole grants the core scheduling inputs and eviction.
	work := findRole(objs, workloadRole)
	if work == nil {
		t.Fatalf("no %s ClusterRole", workloadRole)
	}
	if !ruleGrants(work, "", "nodes", "update") {
		t.Error("workload role missing nodes update")
	}
	if !ruleGrants(work, "", "pods/eviction", "create") {
		t.Error("workload role missing pods/eviction create")
	}
	if !ruleGrants(work, "coordination.k8s.io", "leases", "create") {
		t.Error("workload role missing leader-election leases")
	}
}

func TestSkewFinding(t *testing.T) {
	cases := []struct {
		name string
		o    Options
		want bool // want a finding
	}{
		{"derived tag never skews", Options{KubernetesVersion: "v1.33.4"}, false},
		{"same minor ok", Options{Version: "v1.33.1", KubernetesVersion: "v1.33.4"}, false},
		{"minor skew", Options{Version: "v1.31.0", KubernetesVersion: "v1.33.4"}, true},
		{"unparseable k8s", Options{Version: "v1.31.0", KubernetesVersion: "nope"}, false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := tc.o.SkewFinding()
			if (got != "") != tc.want {
				t.Errorf("SkewFinding() = %q, want finding=%v", got, tc.want)
			}
		})
	}
}

// --- helpers -----------------------------------------------------------------

func kinds(objs []*unstructured.Unstructured) []string {
	var out []string
	for _, o := range objs {
		out = append(out, o.GetKind())
	}
	return out
}

func countKind(objs []*unstructured.Unstructured, kind string) int {
	n := 0
	for _, o := range objs {
		if o.GetKind() == kind {
			n++
		}
	}
	return n
}

func findRole(objs []*unstructured.Unstructured, name string) *unstructured.Unstructured {
	for _, o := range objs {
		if o.GetKind() == "ClusterRole" && o.GetName() == name {
			return o
		}
	}
	return nil
}

// ruleGrants reports whether a ClusterRole has a rule covering the apiGroup,
// resource and verb.
func ruleGrants(role *unstructured.Unstructured, group, resource, verb string) bool {
	rules, _, _ := unstructured.NestedSlice(role.Object, "rules")
	for _, r := range rules {
		m, ok := r.(map[string]any)
		if !ok {
			continue
		}
		if contains(toStrings(m["apiGroups"]), group) &&
			contains(toStrings(m["resources"]), resource) &&
			contains(toStrings(m["verbs"]), verb) {
			return true
		}
	}
	return false
}

func toStrings(v any) []string {
	s, _ := v.([]any)
	out := make([]string, 0, len(s))
	for _, e := range s {
		if str, ok := e.(string); ok {
			out = append(out, str)
		}
	}
	return out
}

func contains(ss []string, want string) bool {
	for _, s := range ss {
		if s == want {
			return true
		}
	}
	return false
}
