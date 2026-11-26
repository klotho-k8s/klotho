package docker

import (
	"context"
	"strings"
	"testing"

	"github.com/klotho-k8s/klotho/pkg/cluster"
	"github.com/klotho-k8s/klotho/provider/infrastructure"
)

func TestPrepareReportsHostReachableClientEndpoint(t *testing.T) {
	rt := newFakeRuntime()
	p := newProvider(rt)
	prep, err := p.Prepare(context.Background(), &cluster.Spec{Name: "c1"})
	if err != nil {
		t.Fatal(err)
	}
	// The cluster endpoint is the load balancer's container-network address.
	if !strings.HasPrefix(prep.Endpoint.Host, "172.18.0.") || prep.Endpoint.Port != DefaultPort {
		t.Fatalf("cluster endpoint = %+v, want container IP and port %d", prep.Endpoint, DefaultPort)
	}
	// The client endpoint is the load balancer's published host port.
	if prep.ClientEndpoint.Host != "127.0.0.1" || prep.ClientEndpoint.Port == 0 {
		t.Fatalf("client endpoint = %+v, want 127.0.0.1 and a published port", prep.ClientEndpoint)
	}
	if prep.ClientEndpoint.Host == prep.Endpoint.Host {
		t.Fatal("client endpoint must differ from the cluster endpoint")
	}
}

func TestLaunchDeletionSidecarLaunchesCleanupContainer(t *testing.T) {
	rt := newFakeRuntime()
	p := &Provider{rt: rt, cfg: Config{NodeImage: "kindest/node:v1.33.0", Socket: "/run/user/1000/podman/podman.sock"}}

	st := &cluster.State{Spec: cluster.Spec{Name: "c1"}, Resources: []cluster.Resource{
		{Kind: ResourceKindNetwork, ID: "kind"},
	}}
	if err := p.LaunchDeletionSidecar(context.Background(), st); err != nil {
		t.Fatal(err)
	}
	if rt.cleanup == nil {
		t.Fatal("no cleanup container launched")
	}
	c := rt.cleanup
	if c.Cluster != "c1" {
		t.Fatalf("cleanup cluster = %q, want c1", c.Cluster)
	}
	if c.Name != CleanupContainerName("c1") {
		t.Fatalf("cleanup name = %q", c.Name)
	}
	if c.Network != "kind" {
		t.Fatalf("cleanup network = %q, want kind (from state)", c.Network)
	}
	if c.Socket == "" {
		t.Fatal("cleanup must mount the host socket")
	}
	if c.Image != DefaultCleanupImage {
		t.Fatalf("cleanup image = %q, want default", c.Image)
	}
	// The sweep filters by the cluster label; the cleanup container carries the
	// cleanup label instead, so it never matches its own sweep.
	cont := rt.containers[CleanupContainerName("c1")]
	if cont == nil || cont.Labels[ClusterLabelKey] == "c1" {
		t.Fatalf("cleanup container must not carry the cluster label: %+v", cont)
	}
	if !strings.Contains(c.Script, "io.x-k8s.kind.cluster") {
		t.Fatal("sweep script must filter by the cluster label")
	}
}

func TestLaunchDeletionSidecarNeedsSocket(t *testing.T) {
	rt := newFakeRuntime()
	p := &Provider{rt: rt, cfg: Config{NodeImage: "kindest/node:v1.33.0"}} // no Socket
	err := p.LaunchDeletionSidecar(context.Background(), &cluster.State{Spec: cluster.Spec{Name: "c1"}})
	if err == nil || !strings.Contains(err.Error(), "socket") {
		t.Fatalf("want a socket error, got %v", err)
	}
	if rt.cleanup != nil {
		t.Fatal("no cleanup container should be launched without a socket")
	}
}

func TestProviderImplementsDeletionSidecar(t *testing.T) {
	var _ infrastructure.DeletionSidecar = newProvider(newFakeRuntime())
}

func TestTeardownRemovesClusterNetwork(t *testing.T) {
	rt := newFakeRuntime()
	rt.networks["kind"] = true
	p := newProvider(rt)
	rt.containers["c1-lb"] = &Container{Name: "c1-lb", Running: true, Labels: map[string]string{ClusterLabelKey: "c1"}}

	st := &cluster.State{Spec: cluster.Spec{Name: "c1"}, Resources: []cluster.Resource{
		{Kind: ResourceKindNetwork, ID: "kind"},
	}}
	plan, err := p.Plan(context.Background(), st)
	if err != nil {
		t.Fatal(err)
	}
	var hasNet bool
	for _, r := range plan.Resources {
		if r.Kind == ResourceKindNetwork && r.ID == "kind" {
			hasNet = true
		}
	}
	if !hasNet {
		t.Fatalf("plan missing the network resource: %+v", plan.Resources)
	}
	if err := p.Teardown(context.Background(), st, plan); err != nil {
		t.Fatal(err)
	}
	if rt.networks["kind"] {
		t.Fatal("cluster network not removed")
	}
	if _, ok := rt.containers["c1-lb"]; ok {
		t.Fatal("lb container not removed")
	}
}

func TestTeardownLeavesBuiltinNetwork(t *testing.T) {
	rt := newFakeRuntime()
	rt.networks["podman"] = true
	p := newProvider(rt)
	st := &cluster.State{Spec: cluster.Spec{Name: "c1"}, Resources: []cluster.Resource{
		{Kind: ResourceKindNetwork, ID: "podman"},
	}}
	plan, err := p.Plan(context.Background(), st)
	if err != nil {
		t.Fatal(err)
	}
	if err := p.Teardown(context.Background(), st, plan); err != nil {
		t.Fatal(err)
	}
	if !rt.networks["podman"] {
		t.Fatal("built-in network must never be removed")
	}
}
