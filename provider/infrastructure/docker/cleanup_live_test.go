package docker

import (
	"context"
	"os"
	"os/exec"
	"strings"
	"testing"
	"time"

	"github.com/klotho-k8s/klotho/pkg/cluster"
)

// These tests drive the real provider against a live container runtime (podman
// by default). They are skipped unless KLOTHO_LIVE=1, so the normal unit suite
// needs no daemon. They reproduce the self-hosted deletion deadlock exactly:
// the "<cluster>-bootstrap" control-plane container runs with the runtime socket
// mounted (it stands in for the in-cluster CAPD controller that cannot remove
// the container it runs in), alongside the "<cluster>-lb" load balancer and a
// worker, on a dedicated network. The provider's cleanup container (default
// path) and in-process sweep (--watch path) must each tear all of it down to
// nothing.
//
//	KLOTHO_LIVE=1 KLOTHO_RUNTIME=podman KLOTHO_SOCKET=/run/user/1000/podman/podman.sock \
//	  go test ./provider/infrastructure/docker/ -run Live -v

func liveBin() string {
	if b := os.Getenv("KLOTHO_RUNTIME"); b != "" {
		return b
	}
	return "podman"
}

func liveSocket(t *testing.T) string {
	s := os.Getenv("KLOTHO_SOCKET")
	if s == "" {
		t.Fatal("KLOTHO_SOCKET must be set to the host runtime socket")
	}
	return s
}

func liveSkip(t *testing.T) {
	if os.Getenv("KLOTHO_LIVE") != "1" {
		t.Skip("set KLOTHO_LIVE=1 to run the live runtime test")
	}
}

func rt(t *testing.T, args ...string) string {
	t.Helper()
	out, err := exec.Command(liveBin(), args...).CombinedOutput()
	if err != nil {
		t.Fatalf("%s %s: %v\n%s", liveBin(), strings.Join(args, " "), err, out)
	}
	return strings.TrimSpace(string(out))
}

func rtTry(args ...string) { _ = exec.Command(liveBin(), args...).Run() }

// clusterContainers lists container names carrying the cluster label.
func clusterContainers(t *testing.T, name string) []string {
	t.Helper()
	out := rt(t, "ps", "--all", "--filter", "label="+ClusterLabelKey+"="+name, "--format", "{{.Names}}")
	if out == "" {
		return nil
	}
	return strings.Split(out, "\n")
}

func networkExists(name string) bool {
	return exec.Command(liveBin(), "network", "exists", name).Run() == nil
}

func containerExists(name string) bool {
	return exec.Command(liveBin(), "container", "exists", name).Run() == nil
}

// buildTopology creates the deadlock end-state: network, load balancer, one
// worker, and a control-plane container that holds the socket (the "controller").
func buildTopology(t *testing.T, name, network, socket string) {
	t.Helper()
	// A tiny image that stays up; alpine is already present on most hosts.
	image := "docker.io/library/alpine"
	rtTry("pull", "-q", image)
	rt(t, "network", "create", network)

	cp := MachineContainerName(name, BootstrapMachineName(name)) // <name>-bootstrap
	// The control-plane container mounts the socket: it is the container the
	// in-cluster controller runs in and cannot remove.
	rt(t, "run", "-d", "--name", cp, "--network", network,
		"--label", ClusterLabelKey+"="+name, "--label", RoleLabelKey+"="+RoleControlPlane,
		"-v", socket+":"+DockerSocketPath, image, "sleep", "3600")
	rt(t, "run", "-d", "--name", LBName(name), "--network", network,
		"--label", ClusterLabelKey+"="+name, "--label", RoleLabelKey+"="+RoleLoadBalancer,
		image, "sleep", "3600")
	rt(t, "run", "-d", "--name", name+"-md-0-worker", "--network", network,
		"--label", ClusterLabelKey+"="+name, "--label", RoleLabelKey+"="+RoleWorker,
		image, "sleep", "3600")
}

func teardownTopology(name, network string) {
	for _, c := range []string{
		CleanupContainerName(name),
		MachineContainerName(name, BootstrapMachineName(name)),
		LBName(name), name + "-md-0-worker",
	} {
		rtTry("rm", "-f", "-v", c)
	}
	rtTry("network", "rm", network)
}

func stateFor(name, network string) *cluster.State {
	return &cluster.State{
		Spec:      cluster.Spec{Name: name, Provider: ProviderName},
		Resources: []cluster.Resource{{Kind: ResourceKindNetwork, ID: network, Name: network}},
	}
}

// TestLiveCleanupContainerBreaksDeadlock drives the default fire-and-forget
// path: LaunchDeletionSidecar starts the cleanup container, which must remove
// the last control-plane container (the deadlock), the load balancer, the
// worker, the network, and finally itself (--rm).
func TestLiveCleanupContainerBreaksDeadlock(t *testing.T) {
	liveSkip(t)
	socket := liveSocket(t)
	name := "capdlive1"
	network := name + "-net"
	teardownTopology(name, network)
	t.Cleanup(func() { teardownTopology(name, network) })

	buildTopology(t, name, network, socket)
	cp := MachineContainerName(name, BootstrapMachineName(name))
	if !containerExists(cp) {
		t.Fatalf("control-plane container %s was not created", cp)
	}
	t.Logf("before: containers=%v network-exists=%v", clusterContainers(t, name), networkExists(network))

	p := New(nil, Config{Runtime: liveBin(), Socket: socket, Network: network, CleanupTimeoutSeconds: 30})
	if err := p.LaunchDeletionSidecar(context.Background(), stateFor(name, network)); err != nil {
		t.Fatalf("LaunchDeletionSidecar: %v", err)
	}

	// Wait for the cleanup container to finish and self-remove.
	deadline := time.Now().Add(90 * time.Second)
	for {
		left := clusterContainers(t, name)
		cleanupGone := !containerExists(CleanupContainerName(name))
		if len(left) == 0 && cleanupGone && !networkExists(network) {
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("not clean after 90s: cluster containers=%v cleanup-gone=%v network-exists=%v",
				left, cleanupGone, networkExists(network))
		}
		time.Sleep(3 * time.Second)
	}

	// The whole point: the last control-plane container was destroyed.
	if containerExists(cp) {
		t.Fatalf("the deadlocked control-plane container %s was left behind", cp)
	}
	if containerExists(LBName(name)) {
		t.Fatalf("load balancer %s left behind", LBName(name))
	}
	if networkExists(network) {
		t.Fatalf("network %s left behind", network)
	}
	if containerExists(CleanupContainerName(name)) {
		t.Fatal("cleanup container did not remove itself")
	}
	t.Logf("after: 100%% clean - last control-plane container, lb, worker, network and cleanup container all gone")
}

// TestLiveWatchSweep drives the in-process --watch path: Plan + Teardown must
// remove the same deadlocked topology directly over the local socket.
func TestLiveWatchSweep(t *testing.T) {
	liveSkip(t)
	socket := liveSocket(t)
	name := "capdlive2"
	network := name + "-net"
	teardownTopology(name, network)
	t.Cleanup(func() { teardownTopology(name, network) })

	buildTopology(t, name, network, socket)
	cp := MachineContainerName(name, BootstrapMachineName(name))
	t.Logf("before: containers=%v network-exists=%v", clusterContainers(t, name), networkExists(network))

	p := New(nil, Config{Runtime: liveBin(), Socket: socket, Network: network})
	st := stateFor(name, network)
	plan, err := p.Plan(context.Background(), st)
	if err != nil {
		t.Fatalf("Plan: %v", err)
	}
	if err := p.Teardown(context.Background(), st, plan); err != nil {
		t.Fatalf("Teardown: %v", err)
	}
	if left := clusterContainers(t, name); len(left) != 0 {
		t.Fatalf("containers left after in-process sweep: %v", left)
	}
	if containerExists(cp) {
		t.Fatalf("control-plane container %s left behind", cp)
	}
	if networkExists(network) {
		t.Fatalf("network %s left behind", network)
	}
	t.Logf("after: 100%% clean via in-process sweep")
}
