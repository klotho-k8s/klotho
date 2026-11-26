package generate

import (
	"strings"
	"testing"

	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"

	"github.com/klotho-k8s/klotho/provider"
)

func TestProviderObjectsFetchURL(t *testing.T) {
	objs, err := ProviderObjects(CAPIOptions{
		Version: "v1.12.9", FetchURL: "https://github.com/me/cluster-api/releases/v1.12.9",
		InfraProviderName: "proxmox", InfraProviderVersion: "v0.9.2",
		InfraFetchURL: "https://example/infrastructure-components.yaml",
		InfraImage:    "ghcr.io/me/cluster-api-provider-proxmox:v0.9.2-me.1",
	}, false)
	if err != nil {
		t.Fatal(err)
	}
	y, _ := RenderYAML(objs)
	for _, want := range []string{
		"url: https://github.com/me/cluster-api/releases/v1.12.9/core-components.yaml",
		"url: https://github.com/me/cluster-api/releases/v1.12.9/bootstrap-components.yaml",
		"url: https://github.com/me/cluster-api/releases/v1.12.9/control-plane-components.yaml",
		"url: https://example/infrastructure-components.yaml",
		"namespace: capmox-system",
		"imageUrl: ghcr.io/me/cluster-api-provider-proxmox:v0.9.2-me.1",
		"name: manager",
		"key: node-role.kubernetes.io/control-plane",
		"nodeSelector:",
	} {
		if !strings.Contains(string(y), want) {
			t.Errorf("missing %q in\n%s", want, y)
		}
	}
	if strings.Contains(string(y), "configSecret") || strings.Contains(string(y), "kind: Secret") {
		t.Error("no secret expected without data")
	}
}

func TestProviderObjectsTolerateControlPlane(t *testing.T) {
	objs, err := ProviderObjects(CAPIOptions{Version: "v1.12.9", InfraProviderName: "proxmox", InfraProviderVersion: "v0.9.2"}, false)
	if err != nil {
		t.Fatal(err)
	}
	n := 0
	for _, o := range objs {
		if !strings.HasSuffix(o.GetKind(), "Provider") {
			continue
		}
		n++
		tol, found, err := unstructured.NestedSlice(o.Object, "spec", "deployment", "tolerations")
		if err != nil || !found || len(tol) != 2 {
			t.Fatalf("%s: tolerations = %v (found %v, err %v)", o.GetKind(), tol, found, err)
		}
		first := tol[0].(map[string]any)
		if first["key"] != "node-role.kubernetes.io/control-plane" || first["operator"] != "Exists" || first["effect"] != "NoSchedule" {
			t.Fatalf("%s: toleration = %v", o.GetKind(), first)
		}
	}
	if n != 4 {
		t.Fatalf("%d provider objects", n)
	}
}

func TestProviderObjectsExtraDependencies(t *testing.T) {
	o := CAPIOptions{Version: "v1.12.9", InfraProviderName: "proxmox", InfraProviderVersion: "v0.9.2",
		Extra: []provider.Dependency{{Kind: "IPAMProvider", Name: "in-cluster", Namespace: "capi-ipam-in-cluster-system", Version: "v1.1.1"}}}
	objs, err := ProviderObjects(o, false)
	if err != nil {
		t.Fatal(err)
	}
	var ns, ipam bool
	for _, obj := range objs {
		switch {
		case obj.GetKind() == "Namespace" && obj.GetName() == "capi-ipam-in-cluster-system":
			ns = true
		case obj.GetKind() == "IPAMProvider":
			ipam = true
			v, _, _ := unstructured.NestedString(obj.Object, "spec", "version")
			if obj.GetName() != "in-cluster" || obj.GetNamespace() != "capi-ipam-in-cluster-system" || v != "v1.1.1" {
				t.Fatalf("ipam provider: %v", obj.Object)
			}
		}
	}
	if !ns || !ipam {
		t.Fatalf("namespace %v, provider %v", ns, ipam)
	}
	refs := o.OperatorProviderRefs()
	if last := refs[len(refs)-1]; last.Kind != "IPAMProvider" || last.Name != "in-cluster" {
		t.Fatalf("refs: %+v", refs)
	}
	o.Extra = append(o.Extra, provider.Dependency{Kind: "IPAMProvider"})
	if _, err := ProviderObjects(o, false); err == nil {
		t.Fatal("incomplete dependency accepted")
	}
}
