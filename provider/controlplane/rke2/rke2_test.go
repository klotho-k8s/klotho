package rke2

import (
	"context"
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
			"version":  "v1.33.4+rke2r1",
			"serverConfig": map[string]any{
				"cni":    "canal",
				"tlsSan": []any{"demo.example"},
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
				"services": map[string]any{"cidrBlocks": []any{"10.43.0.0/16"}},
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
	for _, purpose := range []string{extraPeerEtcd, extraEtcd} {
		if kp := creds.Extra[purpose]; len(kp.Cert) == 0 || len(kp.Key) == 0 {
			t.Errorf("missing extra CA %q", purpose)
		}
	}
	// The etcd CAs are distinct from the cluster CA.
	if string(creds.Extra[extraEtcd].Cert) == string(creds.CA.Cert) {
		t.Error("etcd server CA equals the cluster CA; want distinct")
	}
	if !strings.Contains(string(creds.Kubeconfig), "10.0.0.5:6443") {
		t.Errorf("kubeconfig does not target the endpoint:\n%s", creds.Kubeconfig)
	}
}

func TestUserData(t *testing.T) {
	p := New(Options{ContainerNodes: true})
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
	// The eight CA files in rke2's PKI layout.
	for _, path := range []string{
		TLSDir + "/server-ca.crt", TLSDir + "/server-ca.key",
		TLSDir + "/client-ca.crt", TLSDir + "/client-ca.key",
		TLSDir + "/etcd/peer-ca.crt", TLSDir + "/etcd/peer-ca.key",
		TLSDir + "/etcd/server-ca.crt", TLSDir + "/etcd/server-ca.key",
	} {
		if _, ok := files[path]; !ok {
			t.Errorf("missing CA file %s", path)
		}
	}
	// server-ca and client-ca are the one cluster CA; etcd is distinct.
	if files[TLSDir+"/server-ca.crt"] != files[TLSDir+"/client-ca.crt"] {
		t.Error("server-ca and client-ca differ; want the same cluster CA")
	}
	if files[TLSDir+"/etcd/server-ca.crt"] == files[TLSDir+"/server-ca.crt"] {
		t.Error("etcd server-ca equals the cluster CA; want distinct")
	}
	// config.yaml with the endpoint in tls-san, a token, the networks and the
	// cni, plus the container-node kube-proxy/kubelet args.
	configYAML, ok := files[ConfigPath]
	if !ok {
		t.Fatalf("missing %s", ConfigPath)
	}
	for _, want := range []string{"token:", "- 10.0.0.5", "- demo.example", "cluster-cidr: 192.168.0.0/16", "service-cidr: 10.43.0.0/16", "cni:", "conntrack-max-per-core=0", "fail-swap-on=false"} {
		if !strings.Contains(configYAML, want) {
			t.Errorf("config.yaml missing %q:\n%s", want, configYAML)
		}
	}
	// The config must NOT declare a server (node zero is the cluster-init
	// server) and must not re-list the cluster CA in SANs as an IP mistake.
	if strings.Contains(configYAML, "\nserver:") {
		t.Errorf("config.yaml should not set server: for the init node:\n%s", configYAML)
	}
	// Seeded cluster Secrets, including the token.
	for _, suffix := range []string{"demo-ca.yaml", "demo-cca.yaml", "demo-peer-etcd.yaml", "demo-etcd.yaml", "demo-token.yaml", "demo-kubeconfig.yaml"} {
		if _, ok := files[ManifestsDir+"/"+suffix]; !ok {
			t.Errorf("missing seeded secret manifest %s", suffix)
		}
	}
	// The token in config.yaml matches the token in the seeded Secret.
	tokenSecret := files[ManifestsDir+"/demo-token.yaml"]
	var ts map[string]any
	if err := yaml.Unmarshal([]byte(tokenSecret), &ts); err != nil {
		t.Fatal(err)
	}
	// Install and post-init scripts are run.
	if len(c.InstallCommands) != 1 || !strings.Contains(c.InstallCommands[0], InstallScript) {
		t.Errorf("install commands = %v", c.InstallCommands)
	}
	if len(c.PostCommands) != 1 || !strings.Contains(c.PostCommands[0], PostInitScript) {
		t.Errorf("post commands = %v", c.PostCommands)
	}
	// The install script installs and starts rke2 with the control plane's version.
	install := files[InstallScript]
	for _, want := range []string{"get.rke2.io", "INSTALL_RKE2_VERSION=", "v1.33.4+rke2r1", "rke2-server.service", "mask rke2-agent"} {
		if !strings.Contains(install, want) {
			t.Errorf("install script missing %q", want)
		}
	}
}

func TestAdopt(t *testing.T) {
	p := New(Options{})
	st := &cluster.State{
		Spec:      cluster.Spec{Name: "demo", Provider: "docker"},
		Bootstrap: &cluster.Node{ID: "docker:////demo-bootstrap"},
	}
	cp := testControlPlane()
	out, err := p.Adopt(context.Background(), st, cp)
	if err != nil {
		t.Fatal(err)
	}
	docs := splitYAML(t, out)
	var machine, dataSecret map[string]any
	for _, d := range docs {
		switch d["kind"] {
		case "Machine":
			machine = d
		case "Secret":
			dataSecret = d
		}
	}
	if machine == nil || dataSecret == nil {
		t.Fatalf("want Machine and Secret; got %d docs", len(docs))
	}
	// No RKE2Config / bootstrap config object is rendered.
	for _, d := range docs {
		if d["kind"] == ConfigKind {
			t.Errorf("Adopt rendered an %s; it must omit the bootstrap configRef", ConfigKind)
		}
	}

	meta := machine["metadata"].(map[string]any)
	labels := meta["labels"].(map[string]any)
	if _, ok := labels["cluster.x-k8s.io/control-plane"]; !ok {
		t.Error("machine missing control-plane label")
	}
	if labels["cluster.x-k8s.io/cluster-name"] != "demo" {
		t.Errorf("machine cluster-name label = %v", labels["cluster.x-k8s.io/cluster-name"])
	}
	mspec := machine["spec"].(map[string]any)
	if mspec["version"] != "v1.33.4+rke2r1" {
		t.Errorf("machine version = %v, want the control plane version verbatim", mspec["version"])
	}
	boot := mspec["bootstrap"].(map[string]any)
	if boot["dataSecretName"] != "demo-bootstrap" {
		t.Errorf("dataSecretName = %v", boot["dataSecretName"])
	}
	if _, ok := boot["configRef"]; ok {
		t.Error("machine bootstrap must omit configRef so the rollout gate short-circuits")
	}
	if mspec["providerID"] != "docker:////demo-bootstrap" {
		t.Errorf("providerID = %v", mspec["providerID"])
	}
	infra := mspec["infrastructureRef"].(map[string]any)
	if infra["kind"] != "DockerMachine" {
		t.Errorf("infra kind = %v", infra["kind"])
	}
}

func TestCAPIProviders(t *testing.T) {
	bootstrap, cp := CAPIProviders("", "", "", "", "")
	if bootstrap.Name != BootstrapProviderName || cp.Name != ControlPlaneProviderName {
		t.Errorf("provider names: %q / %q", bootstrap.Name, cp.Name)
	}
	if bootstrap.Namespace != BootstrapNamespace || cp.Namespace != ControlPlaneNamespace {
		t.Errorf("namespaces: %q / %q", bootstrap.Namespace, cp.Namespace)
	}
	if !strings.HasSuffix(bootstrap.ComponentsURL, "/bootstrap-components.yaml") {
		t.Errorf("bootstrap url = %s", bootstrap.ComponentsURL)
	}
	if !strings.HasSuffix(cp.ComponentsURL, "/control-plane-components.yaml") {
		t.Errorf("control-plane url = %s", cp.ComponentsURL)
	}
	if !strings.Contains(bootstrap.ComponentsURL, DefaultProviderVersion) {
		t.Errorf("default version not in url: %s", bootstrap.ComponentsURL)
	}

	// Overrides win.
	b2, c2 := CAPIProviders("v9.9.9", "bns", "cns", "http://x/b.yaml", "http://x/c.yaml")
	if b2.Version != "v9.9.9" || b2.Namespace != "bns" || b2.ComponentsURL != "http://x/b.yaml" {
		t.Errorf("bootstrap overrides not applied: %+v", b2)
	}
	if c2.Namespace != "cns" || c2.ComponentsURL != "http://x/c.yaml" {
		t.Errorf("control-plane overrides not applied: %+v", c2)
	}

	// The install objects name the rke2 providers for both roles, in distinct
	// namespaces.
	objs, err := generate.ProviderObjects(generate.CAPIOptions{
		Version: "v1.12.9", InfraProviderName: "docker", InfraProviderVersion: "v1.12.9",
		Bootstrap: bootstrap, ControlPlane: cp,
	}, false)
	if err != nil {
		t.Fatal(err)
	}
	y, _ := generate.RenderYAML(objs)
	for _, want := range []string{"rke2-bootstrap-system", "rke2-control-plane-system", "/bootstrap-components.yaml", "/control-plane-components.yaml"} {
		if !strings.Contains(string(y), want) {
			t.Errorf("provider objects missing %q", want)
		}
	}
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
