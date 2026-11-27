package hooks

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/klotho-k8s/klotho/pkg/cluster"
)

// TestHelperProcess is re-executed as the hook command. It does nothing unless
// GO_WANT_HELPER_PROCESS is set, so it is inert during a normal test run.
func TestHelperProcess(_ *testing.T) {
	if os.Getenv("GO_WANT_HELPER_PROCESS") != "1" {
		return
	}
	switch os.Getenv("HELPER_MODE") {
	case "dump":
		kc, _ := os.ReadFile(os.Getenv(EnvKubeconfig))
		lines := []string{
			"KUBECONFIG_CONTENT=" + string(kc),
			EnvCluster + "=" + os.Getenv(EnvCluster),
			EnvEvent + "=" + os.Getenv(EnvEvent),
			EnvHookName + "=" + os.Getenv(EnvHookName),
			EnvEndpoint + "=" + os.Getenv(EnvEndpoint),
			EnvHost + "=" + os.Getenv(EnvHost),
			EnvPort + "=" + os.Getenv(EnvPort),
			EnvContext + "=" + os.Getenv(EnvContext),
		}
		_ = os.WriteFile(os.Getenv("HELPER_OUT"), []byte(strings.Join(lines, "\n")), 0o600)
		os.Exit(0)
	case "sleep":
		time.Sleep(30 * time.Second)
		os.Exit(0)
	case "fail":
		os.Exit(3)
	default:
		os.Exit(0)
	}
}

// helperHook runs the test binary back as the hook command, in HELPER_MODE.
func helperHook(name string) Hook {
	return Hook{
		Name:    name,
		Event:   PostPivot,
		Type:    Exec,
		Command: os.Args[0],
		Args:    []string{"-test.run=TestHelperProcess"},
	}
}

func TestExecEnvWiring(t *testing.T) {
	out := filepath.Join(t.TempDir(), "dump")
	t.Setenv("GO_WANT_HELPER_PROCESS", "1")
	t.Setenv("HELPER_MODE", "dump")
	t.Setenv("HELPER_OUT", out)

	target := Target{
		Cluster:     "demo",
		Endpoint:    cluster.Endpoint{Host: "10.0.0.10", Port: 6443},
		KubeContext: "demo",
		Kubeconfig:  []byte("THE-KUBECONFIG"),
	}
	// Run (not the engine directly) so the target's Event is set, as in use.
	if err := Run(t.Context(), nil, NewEngine(nil), PostPivot, target, []Hook{helperHook("dump")}); err != nil {
		t.Fatal(err)
	}
	b, err := os.ReadFile(out)
	if err != nil {
		t.Fatalf("helper wrote nothing: %v", err)
	}
	got := map[string]string{}
	for _, line := range strings.SplitN(string(b), "\n", 8) {
		k, v, _ := strings.Cut(line, "=")
		got[k] = v
	}
	if got["KUBECONFIG_CONTENT"] != "THE-KUBECONFIG" {
		t.Errorf("KUBECONFIG file content = %q", got["KUBECONFIG_CONTENT"])
	}
	for k, want := range map[string]string{
		EnvCluster:  "demo",
		EnvEvent:    string(PostPivot),
		EnvHookName: "dump",
		EnvEndpoint: "10.0.0.10:6443",
		EnvHost:     "10.0.0.10",
		EnvPort:     "6443",
		EnvContext:  "demo",
	} {
		if got[k] != want {
			t.Errorf("%s = %q, want %q", k, got[k], want)
		}
	}
}

func TestExecDefaultPort(t *testing.T) {
	out := filepath.Join(t.TempDir(), "dump")
	t.Setenv("GO_WANT_HELPER_PROCESS", "1")
	t.Setenv("HELPER_MODE", "dump")
	t.Setenv("HELPER_OUT", out)
	target := Target{Cluster: "c", Endpoint: cluster.Endpoint{Host: "h"}, KubeContext: "c"}
	if err := Run(t.Context(), nil, NewEngine(nil), PostPivot, target, []Hook{helperHook("dump")}); err != nil {
		t.Fatal(err)
	}
	b, _ := os.ReadFile(out)
	if !strings.Contains(string(b), EnvPort+"=6443") || !strings.Contains(string(b), EnvEndpoint+"=h:6443") {
		t.Fatalf("default port not applied:\n%s", b)
	}
}

func TestExecFailureExitCode(t *testing.T) {
	t.Setenv("GO_WANT_HELPER_PROCESS", "1")
	t.Setenv("HELPER_MODE", "fail")
	err := NewEngine(nil).Run(t.Context(), helperHook("fail"), Target{Cluster: "demo"})
	if err == nil {
		t.Fatal("a non-zero exit must be an error")
	}
}

func TestExecTimeout(t *testing.T) {
	t.Setenv("GO_WANT_HELPER_PROCESS", "1")
	t.Setenv("HELPER_MODE", "sleep")
	h := helperHook("slow")
	h.Timeout = 50 * time.Millisecond
	start := time.Now()
	err := Run(t.Context(), nil, NewEngine(nil), PostPivot, Target{Cluster: "demo"}, []Hook{h})
	if err == nil || !strings.Contains(err.Error(), "timed out") {
		t.Fatalf("got %v", err)
	}
	if elapsed := time.Since(start); elapsed > 10*time.Second {
		t.Fatalf("timeout did not kill the process: ran %s", elapsed)
	}
}

func TestExecNoCommand(t *testing.T) {
	err := NewEngine(nil).Run(t.Context(), Hook{Name: "x", Event: PostPivot, Type: Exec}, Target{})
	if err == nil || !strings.Contains(err.Error(), "no command") {
		t.Fatalf("got %v", err)
	}
}

func TestUnknownType(t *testing.T) {
	err := NewEngine(nil).Run(t.Context(), Hook{Name: "a", Event: PostPivot, Type: "bogus"}, Target{})
	if err == nil || !strings.Contains(err.Error(), "unknown hook type") {
		t.Fatalf("got %v", err)
	}
}
