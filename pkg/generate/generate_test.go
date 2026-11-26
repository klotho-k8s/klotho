package generate

import (
	"context"
	"errors"
	"reflect"
	"testing"

	clusterctlv1 "sigs.k8s.io/cluster-api/cmd/clusterctl/api/v1alpha3"
	"sigs.k8s.io/cluster-api/cmd/clusterctl/client/config"
	"sigs.k8s.io/cluster-api/cmd/clusterctl/client/repository"

	"github.com/klotho-k8s/klotho/pkg/cluster"
)

const testVersion = "v0.9.2"

const testTemplate = `apiVersion: cluster.x-k8s.io/v1beta2
kind: Cluster
metadata:
  name: ${CLUSTER_NAME}
spec:
  infrastructureRef:
    apiGroup: infrastructure.cluster.x-k8s.io
    kind: ProxmoxCluster
    name: ${CLUSTER_NAME}
---
apiVersion: infrastructure.cluster.x-k8s.io/v1alpha1
kind: ProxmoxCluster
metadata:
  name: ${CLUSTER_NAME}
spec:
  controlPlaneEndpoint:
    host: ${CONTROL_PLANE_ENDPOINT_IP}
    port: 6443
  unknownField:
    keep: me
---
---
apiVersion: infrastructure.cluster.x-k8s.io/v1alpha1
kind: ProxmoxMachineTemplate
metadata:
  name: ${CLUSTER_NAME}-control-plane
spec:
  template:
    spec:
      templateID: ${TEMPLATE_VMID}
      description: "${FOO:=bar}"
---
apiVersion: infrastructure.cluster.x-k8s.io/v1alpha1
kind: ProxmoxMachineTemplate
metadata:
  name: ${CLUSTER_NAME}-worker
spec:
  template:
    spec:
      templateID: ${TEMPLATE_VMID}
      tags: [existing]
`

const devTemplate = `apiVersion: v1
kind: ConfigMap
metadata:
  name: ${CLUSTER_NAME}-dev
data:
  k8s: ${KUBERNETES_VERSION}
`

// memGenerator returns a generator whose repository client serves files from
// an in-memory repository instead of GitHub.
func memGenerator(t *testing.T) (generator, *config.Provider) {
	t.Helper()
	mem := repository.NewMemoryRepository().
		WithPaths("root", "infrastructure-components.yaml").
		WithDefaultVersion(testVersion).
		WithFile(testVersion, "cluster-template.yaml", []byte(testTemplate)).
		WithFile(testVersion, "cluster-template-dev.yaml", []byte(devTemplate)).
		WithMetadata(testVersion, &clusterctlv1.Metadata{
			ReleaseSeries: []clusterctlv1.ReleaseSeries{{Major: 0, Minor: 9, Contract: "v1beta1"}},
		})
	var seen config.Provider
	return generator{newRepo: func(ctx context.Context, p config.Provider, cfg config.Client) (repository.Client, error) {
		seen = p
		return repository.New(ctx, p, cfg, repository.InjectRepository(mem))
	}}, &seen
}

func baseRequest() Request {
	return Request{
		Spec:                 cluster.Spec{Name: "demo", Provider: "proxmox"},
		Provider:             ProviderRef{Name: "proxmox", Version: testVersion},
		Endpoint:             cluster.Endpoint{Host: "10.0.0.10", Port: 6443},
		Kubernetes:           "v1.33.4",
		ControlPlaneReplicas: 1,
		WorkerReplicas:       2,
	}
}

func TestResolvePrecedence(t *testing.T) {
	t.Setenv("KT_CLUSTER_NAME", "from-env")
	t.Setenv("KT_TEMPLATE_VMID", "200")
	t.Setenv("KT_KUBERNETES_VERSION", "v1.30.0")

	req := baseRequest()
	req.Variables = []VariableSource{
		MapSource(map[string]string{"TEMPLATE_VMID": "100"}),
		EnvSource("KT_"),
	}
	vars := []Variable{
		{Name: "CLUSTER_NAME", Required: true},
		{Name: "TEMPLATE_VMID", Required: true},
		{Name: "KUBERNETES_VERSION", Required: true},
		{Name: "WORKER_MACHINE_COUNT", Required: true},
	}
	r, err := resolve(vars, sources(req))
	if err != nil {
		t.Fatal(err)
	}
	want := map[string]string{
		"CLUSTER_NAME":         "from-env", // env beats request
		"TEMPLATE_VMID":        "100",      // map beats env
		"KUBERNETES_VERSION":   "v1.30.0",  // env beats request
		"WORKER_MACHINE_COUNT": "2",        // request default
	}
	if !reflect.DeepEqual(r.provided, want) {
		t.Fatalf("resolved %v, want %v", r.provided, want)
	}
}

func TestSpecVariablesBetweenSourcesAndDefaults(t *testing.T) {
	req := baseRequest()
	req.Spec.Variables = map[string]string{"CLUSTER_NAME": "spec", "TEMPLATE_VMID": "1"}
	req.Variables = []VariableSource{MapSource(map[string]string{"TEMPLATE_VMID": "2"})}
	r, err := resolve([]Variable{{Name: "CLUSTER_NAME"}, {Name: "TEMPLATE_VMID"}}, sources(req))
	if err != nil {
		t.Fatal(err)
	}
	if r.provided["CLUSTER_NAME"] != "spec" || r.provided["TEMPLATE_VMID"] != "2" {
		t.Fatalf("got %v", r.provided)
	}
}

func TestGenerateMissingVariables(t *testing.T) {
	g, _ := memGenerator(t)
	req := baseRequest()
	req.Endpoint = cluster.Endpoint{}
	_, err := g.generate(context.Background(), req)
	var mv *MissingVariablesError
	if !errors.As(err, &mv) {
		t.Fatalf("expected MissingVariablesError, got %v", err)
	}
	want := []string{"CONTROL_PLANE_ENDPOINT_IP", "TEMPLATE_VMID"}
	if !reflect.DeepEqual(mv.Names, want) {
		t.Fatalf("missing %v, want %v", mv.Names, want)
	}
}

func TestGenerate(t *testing.T) {
	g, seen := memGenerator(t)
	req := baseRequest()
	req.Spec.Namespace = "clusters"
	req.Variables = []VariableSource{MapSource(map[string]string{"TEMPLATE_VMID": "9000"})}
	b, err := g.generate(context.Background(), req)
	if err != nil {
		t.Fatal(err)
	}
	if got := (*seen).URL(); got != "https://github.com/ionos-cloud/cluster-api-provider-proxmox/releases/"+testVersion+"/infrastructure-components.yaml" {
		t.Fatalf("provider URL not pinned: %s", got)
	}
	wantKinds := []string{"Cluster", "ProxmoxCluster", "ProxmoxMachineTemplate", "ProxmoxMachineTemplate"}
	if len(b.Objects) != len(wantKinds) {
		t.Fatalf("got %d objects, want %d", len(b.Objects), len(wantKinds))
	}
	for i, k := range wantKinds {
		if b.Objects[i].GetKind() != k {
			t.Errorf("object %d kind %s, want %s", i, b.Objects[i].GetKind(), k)
		}
		if b.Objects[i].GetNamespace() != "clusters" {
			t.Errorf("object %d namespace %q", i, b.Objects[i].GetNamespace())
		}
	}
	if b.Objects[0].GetName() != "demo" || b.Objects[2].GetName() != "demo-control-plane" {
		t.Errorf("names not rendered: %s %s", b.Objects[0].GetName(), b.Objects[2].GetName())
	}
	host, _, _ := nestedString(b.Objects[1], "spec", "controlPlaneEndpoint", "host")
	if host != "10.0.0.10" {
		t.Errorf("endpoint host %q", host)
	}
	desc, _, _ := nestedString(b.Objects[2], "spec", "template", "spec", "description")
	if desc != "bar" {
		t.Errorf("default not applied: %q", desc)
	}
	if b.Variables["FOO"] != "bar" || b.Variables["TEMPLATE_VMID"] != "9000" || b.Variables["CLUSTER_NAME"] != "demo" {
		t.Errorf("variables %v", b.Variables)
	}
	again, err := ParseBundle(b.YAML)
	if err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(again.Objects, b.Objects) {
		t.Fatal("YAML and Objects diverge")
	}
}

func TestGenerateFlavorAndURLOverride(t *testing.T) {
	g, seen := memGenerator(t)
	req := baseRequest()
	req.Spec.Flavor = "dev"
	req.Provider.URL = "https://github.com/someone/fork/releases/v9.9.9/infrastructure-components.yaml"
	b, err := g.generate(context.Background(), req)
	if err != nil {
		t.Fatal(err)
	}
	if (*seen).URL() != req.Provider.URL {
		t.Fatalf("override not used: %s", (*seen).URL())
	}
	if len(b.Objects) != 1 || b.Objects[0].GetName() != "demo-dev" || b.Objects[0].GetNamespace() != "default" {
		t.Fatalf("unexpected objects %v", b.Objects)
	}
}

func TestGenerateRequiresPinnedVersion(t *testing.T) {
	g, _ := memGenerator(t)
	req := baseRequest()
	req.Provider.Version = ""
	if _, err := g.generate(context.Background(), req); err == nil {
		t.Fatal("expected error without version")
	}
}

func TestListVariables(t *testing.T) {
	g, _ := memGenerator(t)
	vars, err := g.listVariables(context.Background(), baseRequest())
	if err != nil {
		t.Fatal(err)
	}
	got := map[string]bool{}
	for _, v := range vars {
		got[v.Name] = v.Required
		if v.Name == "FOO" && (v.Default == nil || *v.Default != "bar") {
			t.Errorf("FOO default %v", v.Default)
		}
	}
	want := map[string]bool{
		"CLUSTER_NAME":              true,
		"CONTROL_PLANE_ENDPOINT_IP": true,
		"TEMPLATE_VMID":             true,
		"FOO":                       false,
	}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("got %v, want %v", got, want)
	}
	for i := 1; i < len(vars); i++ {
		if vars[i-1].Name > vars[i].Name {
			t.Fatal("variables not sorted")
		}
	}
}
