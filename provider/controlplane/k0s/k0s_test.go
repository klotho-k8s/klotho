package k0s

import (
	"context"
	"encoding/json"
	"strings"
	"testing"

	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"sigs.k8s.io/yaml"

	"github.com/klotho-k8s/klotho/pkg/cluster"
	"github.com/klotho-k8s/klotho/pkg/generate"
)

func testControlPlane() *unstructured.Unstructured {
	return &unstructured.Unstructured{Object: map[string]any{
		"apiVersion": ControlPlaneGroup + "/" + ControlPlaneVersion,
		"kind":       Kind,
		"metadata":   map[string]any{"name": "demo-control-plane", "namespace": "default"},
		"spec": map[string]any{
			"replicas": int64(1),
			"version":  "v1.33.13+k0s.0",
			"k0sConfigSpec": map[string]any{
				"args": []any{"--enable-worker", "--no-taints"},
				"k0s": map[string]any{
					"apiVersion": "k0s.k0sproject.io/v1beta1",
					"kind":       "ClusterConfig",
					"metadata":   map[string]any{"name": "k0s"},
					"spec": map[string]any{
						"storage": map[string]any{"type": "kine"},
					},
				},
			},
		},
	}}
}

func testCluster() *unstructured.Unstructured {
	return &unstructured.Unstructured{Object: map[string]any{
		"apiVersion": "cluster.x-k8s.io/v1beta2",
		"kind":       "Cluster",
		"metadata":   map[string]any{"name": "demo", "namespace": "default"},
		"spec": map[string]any{
			"clusterNetwork": map[string]any{
				"pods":     map[string]any{"cidrBlocks": []any{"192.168.0.0/16"}},
				"services": map[string]any{"cidrBlocks": []any{"10.128.0.0/12"}},
			},
		},
	}}
}

func TestPrepare(t *testing.T) {
	p := New(Options{})
	spec := &cluster.Spec{Name: "demo"}
	creds, err := p.Prepare(context.Background(), spec, cluster.Endpoint{Host: "10.0.0.5", Port: 6443})
	if err != nil {
		t.Fatal(err)
	}
	if len(creds.CA.Cert) == 0 || len(creds.CA.Key) == 0 {
		t.Fatal("no cluster CA")
	}
	for _, purpose := range []string{"etcd", "proxy", "sa"} {
		if kp := creds.Extra[purpose]; len(kp.Cert) == 0 && len(kp.Key) == 0 {
			t.Errorf("missing extra key pair %q (have %v)", purpose, keysOf(creds.Extra))
		}
	}
	if !strings.Contains(string(creds.Kubeconfig), "10.0.0.5:6443") {
		t.Errorf("kubeconfig does not target the endpoint:\n%s", creds.Kubeconfig)
	}
}

func TestUserData(t *testing.T) {
	p := New(Options{})
	spec := &cluster.Spec{Name: "demo"}
	creds, err := p.Prepare(context.Background(), spec, cluster.Endpoint{Host: "10.0.0.5", Port: 6443})
	if err != nil {
		t.Fatal(err)
	}
	c, err := p.UserData(context.Background(), spec, creds, testControlPlane(), testCluster())
	if err != nil {
		t.Fatal(err)
	}

	files := map[string]string{}
	for _, f := range c.Files {
		files[f.Path] = string(f.Content)
	}
	// CAs in the k0s PKI layout.
	for _, path := range []string{
		PKIDir + "/ca.crt", PKIDir + "/ca.key",
		PKIDir + "/etcd/ca.crt", PKIDir + "/sa.pub", PKIDir + "/front-proxy-ca.crt",
	} {
		if _, ok := files[path]; !ok {
			t.Errorf("missing PKI file %s", path)
		}
	}
	// k0s.yaml with the endpoint, Kine storage and the pod/service networks.
	k0sYAML, ok := files[K0sConfigPath]
	if !ok {
		t.Fatalf("missing %s", K0sConfigPath)
	}
	for _, want := range []string{"externalAddress: 10.0.0.5", "type: kine", "podCIDR: 192.168.0.0/16", "serviceCIDR: 10.128.0.0/12"} {
		if !strings.Contains(k0sYAML, want) {
			t.Errorf("k0s.yaml missing %q:\n%s", want, k0sYAML)
		}
	}
	// Seeded cluster Secrets.
	for _, suffix := range []string{"demo-ca.yaml", "demo-etcd.yaml", "demo-proxy.yaml", "demo-sa.yaml", "demo-kubeconfig.yaml"} {
		if _, ok := files[ManifestsDir+"/"+suffix]; !ok {
			t.Errorf("missing seeded secret manifest %s", suffix)
		}
	}
	// Install and post-init scripts are run.
	if len(c.InstallCommands) != 1 || !strings.Contains(c.InstallCommands[0], InstallScript) {
		t.Errorf("install commands = %v", c.InstallCommands)
	}
	if len(c.PostCommands) != 1 || !strings.Contains(c.PostCommands[0], PostInitScript) {
		t.Errorf("post commands = %v", c.PostCommands)
	}
	// The install script installs the k0s controller with the control plane's args.
	install := files[InstallScript]
	for _, want := range []string{"install controller", "--enable-worker", "--no-taints", "v1.33.13+k0s.0", "AUTOPILOT_HOSTNAME='demo-bootstrap'"} {
		if !strings.Contains(install, want) {
			t.Errorf("install script missing %q", want)
		}
	}
}

func TestAdopt(t *testing.T) {
	p := New(Options{})
	st := &cluster.State{
		Spec:      cluster.Spec{Name: "demo", Provider: "docker"},
		Bootstrap: &cluster.Node{ID: "docker:////demo-worker-demo-control-plane"},
	}
	cp := testControlPlane()
	out, err := p.Adopt(context.Background(), st, cp)
	if err != nil {
		t.Fatal(err)
	}
	docs := splitYAML(t, out)
	var machine, config, dataSecret map[string]any
	for _, d := range docs {
		switch d["kind"] {
		case "Machine":
			machine = d
		case ControllerConfigKind:
			config = d
		case "Secret":
			dataSecret = d
		}
	}
	if machine == nil || config == nil || dataSecret == nil {
		t.Fatalf("want Machine, %s and Secret; got %d docs", ControllerConfigKind, len(docs))
	}

	// The Machine carries the control-plane labels, the k0s-config annotation,
	// dataSecretName (already bootstrapped), the config ref and the infra ref.
	meta := machine["metadata"].(map[string]any)
	labels := meta["labels"].(map[string]any)
	if _, ok := labels["cluster.x-k8s.io/control-plane"]; !ok {
		t.Error("machine missing control-plane label")
	}
	if labels["cluster.x-k8s.io/cluster-name"] != "demo" {
		t.Errorf("machine cluster-name label = %v", labels["cluster.x-k8s.io/cluster-name"])
	}
	anns := meta["annotations"].(map[string]any)
	got, _ := anns[MachineK0sConfigAnnotation].(string)
	if got == "" {
		t.Fatal("machine missing k0s-config annotation")
	}
	// The annotation is the control plane's k0sConfigSpec as JSON.
	wantSpec, _, _ := unstructured.NestedMap(cp.Object, "spec", "k0sConfigSpec")
	wantJSON, _ := json.Marshal(wantSpec)
	if got != string(wantJSON) {
		t.Errorf("annotation = %s\nwant %s", got, wantJSON)
	}
	mspec := machine["spec"].(map[string]any)
	boot := mspec["bootstrap"].(map[string]any)
	if boot["dataSecretName"] != "demo-bootstrap" {
		t.Errorf("dataSecretName = %v", boot["dataSecretName"])
	}
	cref := boot["configRef"].(map[string]any)
	if cref["kind"] != ControllerConfigKind || cref["name"] != "demo-bootstrap" {
		t.Errorf("configRef = %v", cref)
	}
	if mspec["providerID"] != "docker:////demo-worker-demo-control-plane" {
		t.Errorf("providerID = %v", mspec["providerID"])
	}
	infra := mspec["infrastructureRef"].(map[string]any)
	if infra["kind"] != "DockerMachine" {
		t.Errorf("infra kind = %v", infra["kind"])
	}

	// The K0sControllerConfig carries the version and the k0sConfigSpec.
	cspec := config["spec"].(map[string]any)
	if cspec["version"] != "v1.33.13+k0s.0" {
		t.Errorf("controller config version = %v", cspec["version"])
	}
	if _, ok := cspec["k0s"]; !ok {
		t.Error("controller config missing k0s config")
	}
}

func TestCAPIProviders(t *testing.T) {
	bootstrap, cp := CAPIProviders("", "", "", "")
	if bootstrap.Name != OperatorProviderName || cp.Name != OperatorProviderName {
		t.Errorf("provider names: %q / %q", bootstrap.Name, cp.Name)
	}
	if bootstrap.Namespace != OperatorNamespace || cp.Namespace != OperatorNamespace {
		t.Errorf("namespaces: %q / %q", bootstrap.Namespace, cp.Namespace)
	}
	if !strings.HasSuffix(bootstrap.ComponentsURL, "/bootstrap-components.yaml") {
		t.Errorf("bootstrap url = %s", bootstrap.ComponentsURL)
	}
	if !strings.HasSuffix(cp.ComponentsURL, "/control-plane-components.yaml") {
		t.Errorf("control-plane url = %s", cp.ComponentsURL)
	}
	if !strings.Contains(bootstrap.ComponentsURL, DefaultK0smotronVersion) {
		t.Errorf("default version not in url: %s", bootstrap.ComponentsURL)
	}

	// Overrides win.
	b2, _ := CAPIProviders("v9.9.9", "ns", "http://x/b.yaml", "http://x/c.yaml")
	if b2.Version != "v9.9.9" || b2.Namespace != "ns" || b2.ComponentsURL != "http://x/b.yaml" {
		t.Errorf("overrides not applied: %+v", b2)
	}

	// The install objects name the k0smotron providers for both roles.
	objs, err := generate.ProviderObjects(generate.CAPIOptions{
		Version: "v1.12.9", InfraProviderName: "docker", InfraProviderVersion: "v1.12.9",
		Bootstrap: bootstrap, ControlPlane: cp,
	}, false)
	if err != nil {
		t.Fatal(err)
	}
	y, _ := generate.RenderYAML(objs)
	for _, want := range []string{"name: k0sproject-k0smotron", "namespace: k0smotron", "/bootstrap-components.yaml", "/control-plane-components.yaml"} {
		if !strings.Contains(string(y), want) {
			t.Errorf("provider objects missing %q", want)
		}
	}
}

func keysOf(m map[string]cluster.KeyPair) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	return out
}

func splitYAML(t *testing.T, data []byte) []map[string]any {
	t.Helper()
	var out []map[string]any
	for _, doc := range strings.Split(string(data), "\n---\n") {
		doc = strings.TrimSpace(doc)
		if doc == "" {
			continue
		}
		m := map[string]any{}
		if err := yaml.Unmarshal([]byte(doc), &m); err != nil {
			t.Fatalf("unmarshalling doc: %v\n%s", err, doc)
		}
		if len(m) > 0 {
			out = append(out, m)
		}
	}
	return out
}
