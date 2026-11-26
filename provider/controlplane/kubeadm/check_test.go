package kubeadm

import (
	"strings"
	"testing"

	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
)

func kcpObject(apiVersion string, spec map[string]any) *unstructured.Unstructured {
	return &unstructured.Unstructured{Object: map[string]any{
		"apiVersion": apiVersion,
		"kind":       "KubeadmControlPlane",
		"metadata":   map[string]any{"name": "demo-control-plane", "namespace": "default"},
		"spec":       spec,
	}}
}

func TestCheckCertificatesExpiry(t *testing.T) {
	p := &Provider{}
	other := &unstructured.Unstructured{Object: map[string]any{
		"apiVersion": "cluster.x-k8s.io/v1beta2", "kind": "Cluster",
		"metadata": map[string]any{"name": "demo"},
	}}

	cases := []struct {
		name  string
		obj   *unstructured.Unstructured
		want  string // substring of the single finding, "" for none
		field string
	}{
		{
			name:  "v1beta2 unset",
			obj:   kcpObject("controlplane.cluster.x-k8s.io/v1beta2", map[string]any{"replicas": int64(1)}),
			want:  "one-year lifetime",
			field: "spec.rollout.before.certificatesExpiryDays",
		},
		{
			name: "v1beta2 zero",
			obj: kcpObject("controlplane.cluster.x-k8s.io/v1beta2", map[string]any{
				"rollout": map[string]any{"before": map[string]any{"certificatesExpiryDays": int64(0)}},
			}),
			want:  "one-year lifetime",
			field: "spec.rollout.before.certificatesExpiryDays",
		},
		{
			name: "v1beta2 set",
			obj: kcpObject("controlplane.cluster.x-k8s.io/v1beta2", map[string]any{
				"rollout": map[string]any{"before": map[string]any{"certificatesExpiryDays": int64(30)}},
			}),
		},
		{
			name:  "v1beta1 unset",
			obj:   kcpObject("controlplane.cluster.x-k8s.io/v1beta1", map[string]any{}),
			want:  "one-year lifetime",
			field: "spec.rolloutBefore.certificatesExpiryDays",
		},
		{
			name: "v1beta1 set",
			obj: kcpObject("controlplane.cluster.x-k8s.io/v1beta1", map[string]any{
				"rolloutBefore": map[string]any{"certificatesExpiryDays": int64(14)},
			}),
		},
		{
			name: "wrong type",
			obj: kcpObject("controlplane.cluster.x-k8s.io/v1beta2", map[string]any{
				"rollout": map[string]any{"before": map[string]any{"certificatesExpiryDays": "thirty"}},
			}),
			want:  "not a number",
			field: "spec.rollout.before.certificatesExpiryDays",
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := p.Check([]*unstructured.Unstructured{other, tc.obj, nil})
			if tc.want == "" {
				if len(got) != 0 {
					t.Fatalf("findings = %v, want none", got)
				}
				return
			}
			if len(got) != 1 {
				t.Fatalf("findings = %v, want one", got)
			}
			f := got[0]
			if f.Object != "KubeadmControlPlane/demo-control-plane" || f.Field != tc.field || !strings.Contains(f.Message, tc.want) {
				t.Fatalf("finding = %+v", f)
			}
			if s := f.String(); !strings.HasPrefix(s, f.Object+": "+f.Field+": ") {
				t.Fatalf("String() = %q", s)
			}
		})
	}

	t.Run("bundle untouched", func(t *testing.T) {
		obj := kcpObject("controlplane.cluster.x-k8s.io/v1beta2", map[string]any{"replicas": int64(1)})
		before := obj.DeepCopy()
		p.Check([]*unstructured.Unstructured{obj})
		if !equalUnstructured(before, obj) {
			t.Fatal("Check modified the object")
		}
	})
}

func equalUnstructured(a, b *unstructured.Unstructured) bool {
	aj, _ := a.MarshalJSON()
	bj, _ := b.MarshalJSON()
	return string(aj) == string(bj)
}
