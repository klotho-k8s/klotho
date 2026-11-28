package generate

import (
	"bytes"
	"compress/gzip"
	"encoding/base64"
	"io"
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
		"affinity:",
		"operator: Exists",
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

func TestComponentsConfigMapCompression(t *testing.T) {
	// A components payload over the 1 MiB ConfigMap limit (as the real CAPI
	// core-components.yaml is) must be gzip-compressed into binaryData with the
	// compressed annotation, so it fits and the operator gunzips it.
	big := strings.Repeat("apiVersion: v1\nkind: ConfigMap\n---\n", 60000) // ~2 MiB
	if len(big) <= maxConfigMapBytes {
		t.Fatalf("test payload %d is not over the limit", len(big))
	}
	cm := componentsConfigMap("cluster-api", CoreProviderType, "v1.12.9", "capi-system", big, "capi: metadata")
	anns, _, _ := unstructured.NestedStringMap(cm.Object, "metadata", "annotations")
	if anns[CompressedAnnotation] != "true" {
		t.Fatalf("missing compressed annotation, got %v", anns)
	}
	if _, found, _ := unstructured.NestedString(cm.Object, "data", "components"); found {
		t.Error("compressed components must not be in data")
	}
	if v, _, _ := unstructured.NestedString(cm.Object, "data", "metadata"); v != "capi: metadata" {
		t.Errorf("metadata should stay plaintext, got %q", v)
	}
	b64, found, _ := unstructured.NestedString(cm.Object, "binaryData", "components")
	if !found {
		t.Fatal("compressed components must be in binaryData")
	}
	gz, err := base64.StdEncoding.DecodeString(b64)
	if err != nil {
		t.Fatalf("binaryData not base64: %v", err)
	}
	if len(gz) > maxConfigMapBytes {
		t.Errorf("compressed payload %d still over the limit", len(gz))
	}
	zr, err := gzip.NewReader(bytes.NewReader(gz))
	if err != nil {
		t.Fatal(err)
	}
	got, _ := io.ReadAll(zr)
	if string(got) != big {
		t.Error("gunzip did not round-trip the components")
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

// managerImage returns the imageUrl of a provider object's first deployment
// container, and whether a manager container override is present at all.
func managerImage(t *testing.T, u *unstructured.Unstructured) (string, bool) {
	t.Helper()
	containers, found, err := unstructured.NestedSlice(u.Object, "spec", "deployment", "containers")
	if err != nil {
		t.Fatalf("%s: containers: %v", u.GetKind(), err)
	}
	if !found || len(containers) == 0 {
		return "", false
	}
	c := containers[0].(map[string]any)
	if c["name"] != "manager" {
		t.Fatalf("%s: first container is %v, want name manager", u.GetKind(), c["name"])
	}
	img, _ := c["imageUrl"].(string)
	return img, true
}

// TestProviderObjectsImageLevers checks that a per-component image override
// lands as spec.deployment.containers[0].imageUrl on the matching provider CR,
// for the core, bootstrap, control plane and infrastructure providers.
func TestProviderObjectsImageLevers(t *testing.T) {
	const (
		coreImg  = "ghcr.io/klotho-k8s/cluster-api-controller:v1.12.9"
		bootImg  = "ghcr.io/klotho-k8s/cluster-api-kubeadm-bootstrap-controller:v1.12.9"
		cpImg    = "ghcr.io/klotho-k8s/cluster-api-kubeadm-control-plane-controller:v1.12.9"
		infraImg = "ghcr.io/me/cluster-api-provider-proxmox:v0.9.2-me.1"
	)
	objs, err := ProviderObjects(CAPIOptions{
		Version:              "v1.12.9",
		InfraProviderName:    "proxmox",
		InfraProviderVersion: "v0.9.2",
		CoreImage:            coreImg,
		BootstrapImage:       bootImg,
		ControlPlaneImage:    cpImg,
		InfraImage:           infraImg,
	}, false)
	if err != nil {
		t.Fatal(err)
	}
	want := map[string]string{
		KindCoreProvider:           coreImg,
		KindBootstrapProvider:      bootImg,
		KindControlPlaneProvider:   cpImg,
		KindInfrastructureProvider: infraImg,
	}
	seen := map[string]bool{}
	for _, o := range objs {
		exp, ok := want[o.GetKind()]
		if !ok {
			continue
		}
		seen[o.GetKind()] = true
		img, present := managerImage(t, o)
		if !present {
			t.Errorf("%s: no manager image override", o.GetKind())
			continue
		}
		if img != exp {
			t.Errorf("%s: imageUrl = %q, want %q", o.GetKind(), img, exp)
		}
	}
	for kind := range want {
		if !seen[kind] {
			t.Errorf("%s object not rendered", kind)
		}
	}
}

// TestProviderObjectsNoImageLevers checks that with no image levers set the
// rendered output carries no container image override on any provider - the
// unset path is byte-identical to the pre-feature default.
func TestProviderObjectsNoImageLevers(t *testing.T) {
	objs, err := ProviderObjects(CAPIOptions{
		Version: "v1.12.9", InfraProviderName: "proxmox", InfraProviderVersion: "v0.9.2",
	}, false)
	if err != nil {
		t.Fatal(err)
	}
	for _, o := range objs {
		if !strings.HasSuffix(o.GetKind(), "Provider") {
			continue
		}
		if _, present := managerImage(t, o); present {
			t.Errorf("%s: unexpected manager image override with no levers set", o.GetKind())
		}
	}
	y, _ := RenderYAML(objs)
	for _, forbidden := range []string{"imageUrl", "containers:"} {
		if strings.Contains(string(y), forbidden) {
			t.Errorf("rendered output contains %q with no image levers set:\n%s", forbidden, y)
		}
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
