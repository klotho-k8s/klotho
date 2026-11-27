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

func TestProviderObjectsComponentsConfigMap(t *testing.T) {
	objs, err := ProviderObjects(CAPIOptions{
		Version:              "v1.12.9",
		InfraProviderName:    "hyperv",
		InfraProviderVersion: "v0.1.0",
		// A local provider served over http: the operator would reject the URL,
		// so klotho hands the components in directly to be embedded.
		InfraFetchURL:   "http://172.30.32.1:8080/infrastructure-components.yaml",
		InfraComponents: "apiVersion: v1\nkind: Namespace\nmetadata:\n  name: caphv-system\n",
	}, false)
	if err != nil {
		t.Fatal(err)
	}

	var cm, infra *unstructured.Unstructured
	for _, o := range objs {
		switch o.GetKind() {
		case "ConfigMap":
			cm = o
		case KindInfrastructureProvider:
			infra = o
		}
	}
	if cm == nil {
		t.Fatal("no components ConfigMap emitted")
	}
	if infra == nil {
		t.Fatal("no InfrastructureProvider emitted")
	}

	// The provider must select the ConfigMap, not carry a URL the operator rejects.
	if _, found, _ := unstructured.NestedMap(infra.Object, "spec", "fetchConfig", "url"); found {
		t.Error("infra provider still has a fetchConfig.url")
	}
	sel, found, err := unstructured.NestedStringMap(infra.Object, "spec", "fetchConfig", "selector", "matchLabels")
	if err != nil || !found {
		t.Fatalf("no fetchConfig.selector.matchLabels: found=%v err=%v", found, err)
	}
	if sel[ProviderNameLabel] != "hyperv" || sel[ProviderTypeLabel] != InfrastructureProviderType {
		t.Errorf("selector matchLabels = %v", sel)
	}

	// The ConfigMap must carry the matching labels (including the version the
	// operator checks) and both data keys.
	labels := cm.GetLabels()
	if labels[ProviderNameLabel] != "hyperv" || labels[ProviderTypeLabel] != InfrastructureProviderType || labels[ProviderVersionLabel] != "v0.1.0" {
		t.Errorf("ConfigMap labels = %v", labels)
	}
	if cm.GetNamespace() != DefaultInfraNamespace("hyperv") {
		t.Errorf("ConfigMap namespace = %q", cm.GetNamespace())
	}
	data, _, _ := unstructured.NestedStringMap(cm.Object, "data")
	if !strings.Contains(data["components"], "caphv-system") {
		t.Errorf("ConfigMap data.components = %q", data["components"])
	}
	if !strings.Contains(data["metadata"], "releaseSeries") || !strings.Contains(data["metadata"], "contract: v1beta2") {
		t.Errorf("synthesized ConfigMap data.metadata = %q", data["metadata"])
	}
}

func TestProviderObjectsEmbeddedComponents(t *testing.T) {
	// An unpublished infra provider AND a local CAPI mirror both served over
	// http: each provider is served from a labeled ConfigMap via
	// fetchConfig.selector, not a URL the in-cluster operator would reject.
	objs, err := ProviderObjects(CAPIOptions{
		Version:                    "v1.12.9",
		CoreComponentsData:         "core: yaml",
		BootstrapComponentsData:    "bootstrap: yaml",
		ControlPlaneComponentsData: "controlplane: yaml",
		CAPIMetadata:               "capi: metadata",
		InfraProviderName:          "libvirt",
		InfraProviderVersion:       "v0.1.0",
		InfraComponents:            "infra: yaml",
		InfraMetadata:              "infra: metadata",
		InfraImage:                 "ghcr.io/klotho-k8s/cluster-api-provider-libvirt:sha-39126d4",
	}, false)
	if err != nil {
		t.Fatal(err)
	}
	y, _ := RenderYAML(objs)
	for _, want := range []string{
		// Every provider is served via a selector, never a URL.
		"selector:",
		"provider.cluster.x-k8s.io/name: cluster-api",
		"provider.cluster.x-k8s.io/name: kubeadm",
		"provider.cluster.x-k8s.io/name: libvirt",
		"provider.cluster.x-k8s.io/type: core",
		"provider.cluster.x-k8s.io/type: infrastructure",
		"provider.cluster.x-k8s.io/version: v1.12.9",
		"provider.cluster.x-k8s.io/version: v0.1.0",
		// The components land in ConfigMaps in each provider's namespace.
		"kind: ConfigMap",
		"name: cluster-api-core-components-v1.12.9",
		"name: libvirt-infrastructure-components-v0.1.0",
		"components: 'infra: yaml'",
		"metadata: 'capi: metadata'",
		// The manager image override still flows through.
		"imageUrl: ghcr.io/klotho-k8s/cluster-api-provider-libvirt:sha-39126d4",
	} {
		if !strings.Contains(string(y), want) {
			t.Errorf("missing %q in\n%s", want, y)
		}
	}
	// Nothing is fetched by URL when every provider is embedded.
	if strings.Contains(string(y), "url: http") {
		t.Errorf("no url fetch expected when components are embedded:\n%s", y)
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
