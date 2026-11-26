package docker

import (
	"bytes"
	"compress/gzip"
	"context"
	"encoding/base64"
	"strings"
	"testing"

	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"

	"github.com/klotho-k8s/klotho/pkg/cluster"
	"github.com/klotho-k8s/klotho/provider/infrastructure"
)

func newProvider(rt Runtime) *Provider {
	return &Provider{rt: rt, cfg: Config{NodeImage: "kindest/node:v1.33.0"}}
}

func TestNaming(t *testing.T) {
	if got := MachineContainerName("c1", "c1-bootstrap"); got != "c1-bootstrap" {
		t.Fatalf("container name = %q, want c1-bootstrap", got)
	}
	if got := MachineContainerName("c1", "worker"); got != "c1-worker" {
		t.Fatalf("container name = %q, want c1-worker", got)
	}
	if got := LBName("c1"); got != "c1-lb" {
		t.Fatalf("lb name = %q, want c1-lb", got)
	}
	if got := Tag("c1"); got != "c1" {
		t.Fatalf("tag = %q, want c1", got)
	}
}

func TestPrepareCreatesNetworkAndLB(t *testing.T) {
	rt := newFakeRuntime()
	p := newProvider(rt)
	prep, err := p.Prepare(context.Background(), &cluster.Spec{Name: "c1"})
	if err != nil {
		t.Fatal(err)
	}
	if !rt.networks[DefaultNetwork] {
		t.Fatalf("network %s not created", DefaultNetwork)
	}
	if _, ok := rt.containers["c1-lb"]; !ok {
		t.Fatal("load balancer container not created")
	}
	if prep.Endpoint.Host == "" || prep.Endpoint.Port != DefaultPort {
		t.Fatalf("endpoint = %+v, want LB IP and port %d", prep.Endpoint, DefaultPort)
	}
	// Created records the network and the LB container.
	var net, lb bool
	for _, r := range prep.Created {
		if r.Kind == ResourceKindNetwork {
			net = true
		}
		if r.Kind == ResourceKindContainer && r.ID == "c1-lb" {
			lb = true
		}
	}
	if !net || !lb {
		t.Fatalf("Created missing network/lb: %+v", prep.Created)
	}
}

func TestLaunchCreatesNodeZeroAndBootstraps(t *testing.T) {
	rt := newFakeRuntime()
	p := newProvider(rt)
	if _, err := p.Prepare(context.Background(), &cluster.Spec{Name: "c1"}); err != nil {
		t.Fatal(err)
	}
	userData := []byte("#cloud-config\n" +
		"write_files:\n" +
		"- path: /run/kubeadm/kubeadm.yaml\n" +
		"  content: hello\n" +
		"  permissions: \"0640\"\n" +
		"runcmd:\n" +
		"- kubeadm init --config /run/kubeadm/kubeadm.yaml\n")
	node, err := p.Launch(context.Background(), &cluster.Spec{Name: "c1"}, userData)
	if err != nil {
		t.Fatal(err)
	}
	if node.Name != "c1-bootstrap" {
		t.Fatalf("node name = %q, want c1-bootstrap", node.Name)
	}
	if node.ID != ProviderIDPrefix+"c1-bootstrap" {
		t.Fatalf("node ID = %q", node.ID)
	}
	c, ok := rt.containers["c1-bootstrap"]
	if !ok {
		t.Fatal("bootstrap container not created")
	}
	if c.Labels[ClusterLabelKey] != "c1" || c.Labels[RoleLabelKey] != RoleControlPlane {
		t.Fatalf("bootstrap labels = %v", c.Labels)
	}
	// The bootstrap ran: mkdir + cp + chmod for the file, then the runcmd,
	// into the bootstrap container; plus LB reconfigure execs into c1-lb.
	if rt.execCount("c1-bootstrap") < 4 {
		t.Fatalf("bootstrap exec count = %d, want >= 4", rt.execCount("c1-bootstrap"))
	}
	if rt.execCount("c1-lb") < 1 {
		t.Fatal("load balancer was not reconfigured")
	}
}

func TestLaunchNoNodeImage(t *testing.T) {
	p := &Provider{rt: newFakeRuntime(), cfg: Config{}}
	_, err := p.Launch(context.Background(), &cluster.Spec{Name: "c1"}, []byte("#cloud-config\n"))
	if err == nil || !strings.Contains(err.Error(), "node image") {
		t.Fatalf("want node image error, got %v", err)
	}
}

func TestAdoptRendersDockerMachine(t *testing.T) {
	rt := newFakeRuntime()
	p := newProvider(rt)
	// node zero must exist and be running.
	rt.containers["c1-bootstrap"] = &Container{Name: "c1-bootstrap", Running: true, Labels: map[string]string{ClusterLabelKey: "c1"}, IP: "172.18.0.3"}

	tmpl := &unstructured.Unstructured{Object: map[string]any{
		"apiVersion": "infrastructure.cluster.x-k8s.io/v1beta1",
		"kind":       "DockerMachineTemplate",
		"metadata":   map[string]any{"name": "c1-control-plane", "namespace": "default"},
		"spec":       map[string]any{"template": map[string]any{"spec": map[string]any{"customImage": "kindest/node:v1.33.0"}}},
	}}
	kcp := &unstructured.Unstructured{Object: map[string]any{
		"apiVersion": "controlplane.cluster.x-k8s.io/v1beta1",
		"kind":       "KubeadmControlPlane",
		"metadata":   map[string]any{"name": "c1-control-plane", "namespace": "default"},
		"spec": map[string]any{"machineTemplate": map[string]any{
			"infrastructureRef": map[string]any{"name": "c1-control-plane"},
		}},
	}}
	state := &cluster.State{Spec: cluster.Spec{Name: "c1", Namespace: "default"}}
	out, err := p.Adopt(context.Background(), state, []*unstructured.Unstructured{tmpl, kcp})
	if err != nil {
		t.Fatal(err)
	}
	s := string(out)
	for _, want := range []string{"kind: DockerMachine\n", "name: c1-bootstrap", ProviderIDPrefix + "c1-bootstrap", "kindest/node:v1.33.0"} {
		if !strings.Contains(s, want) {
			t.Fatalf("adopt output missing %q:\n%s", want, s)
		}
	}
}

func TestPlanAndTeardown(t *testing.T) {
	rt := newFakeRuntime()
	p := newProvider(rt)
	// three containers of c1, one of another cluster.
	for _, n := range []string{"c1-bootstrap", "c1-lb", "c1-md-0-xyz"} {
		rt.containers[n] = &Container{Name: n, Running: true, Labels: map[string]string{ClusterLabelKey: "c1"}}
	}
	rt.containers["c2-bootstrap"] = &Container{Name: "c2-bootstrap", Running: true, Labels: map[string]string{ClusterLabelKey: "c2"}}

	state := &cluster.State{Spec: cluster.Spec{Name: "c1"}}
	plan, err := p.Plan(context.Background(), state)
	if err != nil {
		t.Fatal(err)
	}
	if len(plan.Resources) != 3 {
		t.Fatalf("plan has %d resources, want 3: %+v", len(plan.Resources), plan.Resources)
	}
	if err := p.Teardown(context.Background(), state, plan); err != nil {
		t.Fatal(err)
	}
	for _, n := range []string{"c1-bootstrap", "c1-lb", "c1-md-0-xyz"} {
		if _, ok := rt.containers[n]; ok {
			t.Fatalf("container %s not removed", n)
		}
	}
	if _, ok := rt.containers["c2-bootstrap"]; !ok {
		t.Fatal("c2's container must not be removed")
	}
}

func TestTeardownRefusesUnlabeled(t *testing.T) {
	rt := newFakeRuntime()
	p := newProvider(rt)
	rt.containers["c1-bootstrap"] = &Container{Name: "c1-bootstrap", Running: true, Labels: map[string]string{}}
	state := &cluster.State{Spec: cluster.Spec{Name: "c1"}}
	plan := &infrastructure.Plan{Resources: []cluster.Resource{{Kind: ResourceKindContainer, ID: "c1-bootstrap", Name: "c1-bootstrap"}}}
	err := p.Teardown(context.Background(), state, plan)
	if err == nil || !strings.Contains(err.Error(), "refusing to remove") {
		t.Fatalf("want refusal, got %v", err)
	}
	if _, ok := rt.containers["c1-bootstrap"]; !ok {
		t.Fatal("unlabeled container must be left in place")
	}
}

func TestDecodeContent(t *testing.T) {
	if b, err := decodeContent("hi", ""); err != nil || string(b) != "hi" {
		t.Fatalf("plain: %q %v", b, err)
	}
	if b, err := decodeContent(base64.StdEncoding.EncodeToString([]byte("hi")), "base64"); err != nil || string(b) != "hi" {
		t.Fatalf("base64: %q %v", b, err)
	}
	var buf bytes.Buffer
	zw := gzip.NewWriter(&buf)
	_, _ = zw.Write([]byte("hi"))
	_ = zw.Close()
	enc := base64.StdEncoding.EncodeToString(buf.Bytes())
	if b, err := decodeContent(enc, "gzip+base64"); err != nil || string(b) != "hi" {
		t.Fatalf("gzip+base64: %q %v", b, err)
	}
}
