package klotho

import (
	"context"
	"errors"
	"strings"
	"sync"
	"testing"

	"github.com/klotho-k8s/klotho/pkg/hooks"
	"github.com/klotho-k8s/klotho/provider/infrastructure"
)

// capturingExec records every hook it is asked to run and the target it saw,
// and returns a canned error per hook name.
type capturingExec struct {
	mu      sync.Mutex
	rec     *recorder // optional: records "hook.Run <name>" for ordering checks
	names   []string
	events  []hooks.Event
	targets []hooks.Target
	errs    map[string]error
}

func (c *capturingExec) Run(_ context.Context, h hooks.Hook, t hooks.Target) error {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.names = append(c.names, h.Name)
	c.events = append(c.events, t.Event)
	c.targets = append(c.targets, t)
	if c.rec != nil {
		c.rec.add("hook.Run %s", h.Name)
	}
	return c.errs[h.Name]
}

func postPivotHook(name string, policy hooks.Policy) hooks.Hook {
	return hooks.Hook{Name: name, Event: hooks.PostPivot, Type: hooks.Exec, Command: "x", OnFailure: policy}
}

func preDeleteHook(name string, policy hooks.Policy) hooks.Hook {
	return hooks.Hook{Name: name, Event: hooks.PreDelete, Type: hooks.Exec, Command: "x", OnFailure: policy}
}

func TestCreateFiresPostPivotHooks(t *testing.T) {
	h := newHarness(t)
	ca := testCA(t)
	h.cp.ca = &ca
	h.kube.readyAfter = 3
	ex := &capturingExec{}
	req := request()
	req.HookExec = ex
	req.Hooks = []hooks.Hook{
		preDeleteHook("ignored", hooks.Fail), // wrong event: must not fire now
		postPivotHook("one", hooks.Fail),
		postPivotHook("two", hooks.Fail),
	}
	if _, err := Create(t.Context(), h.deps, req); err != nil {
		t.Fatal(err)
	}
	if strings.Join(ex.names, ",") != "one,two" {
		t.Fatalf("fired %v, want one,two", ex.names)
	}
	tg := ex.targets[0]
	if tg.Event != hooks.PostPivot || tg.Cluster != "demo" || tg.KubeContext != "demo" {
		t.Fatalf("target: %+v", tg)
	}
	if len(tg.Kubeconfig) == 0 || !strings.Contains(string(tg.Kubeconfig), "klotho-admin@demo") {
		t.Fatalf("target has no usable kubeconfig: %s", tg.Kubeconfig)
	}
	if tg.Endpoint.Host != "10.0.0.10" || tg.Endpoint.Port != 6443 {
		t.Fatalf("endpoint: %+v", tg.Endpoint)
	}
}

func TestCreatePostPivotHookFailSurfacesButKeepsCluster(t *testing.T) {
	h := newHarness(t)
	ca := testCA(t)
	h.cp.ca = &ca
	h.kube.readyAfter = 1
	ex := &capturingExec{errs: map[string]error{"boom": errors.New("flux exploded")}}
	req := request()
	req.HookExec = ex
	req.Hooks = []hooks.Hook{postPivotHook("boom", hooks.Fail)}
	st, err := Create(t.Context(), h.deps, req)
	if err == nil || !strings.Contains(err.Error(), "flux exploded") ||
		!strings.Contains(err.Error(), "self-managed") || !strings.Contains(err.Error(), "klotho delete demo") {
		t.Fatalf("got %v", err)
	}
	// The cluster is up: state is kept, nothing torn down.
	if st == nil || h.rec.has("store.Delete") {
		t.Fatal("a failed postPivot hook must not tear the cluster down")
	}
	if saved := h.store.state("demo"); saved == nil || saved.Reserved {
		t.Fatalf("state not kept: %+v", saved)
	}
}

func TestCreatePostPivotHookWarnContinues(t *testing.T) {
	h := newHarness(t)
	ca := testCA(t)
	h.cp.ca = &ca
	h.kube.readyAfter = 1
	ex := &capturingExec{errs: map[string]error{"soft": errors.New("nope")}}
	req := request()
	req.HookExec = ex
	req.Hooks = []hooks.Hook{postPivotHook("soft", hooks.Warn)}
	if _, err := Create(t.Context(), h.deps, req); err != nil {
		t.Fatalf("warn hook must not fail create: %v", err)
	}
}

func TestCreateNoHooksSkipsKubeconfig(t *testing.T) {
	// Default harness CA is not mintable; with no postPivot hooks Create must
	// not build a hook kubeconfig, so it still succeeds.
	h := newHarness(t)
	h.kube.readyAfter = 1
	ex := &capturingExec{}
	req := request()
	req.HookExec = ex
	if _, err := Create(t.Context(), h.deps, req); err != nil {
		t.Fatal(err)
	}
	if len(ex.names) != 0 {
		t.Fatalf("ran hooks with none configured: %v", ex.names)
	}
}

func TestDeleteFiresPreDeleteBeforeTeardown(t *testing.T) {
	h := newHarness(t)
	ca := testCA(t)
	h.cp.ca = &ca
	if _, err := Create(t.Context(), h.deps, request()); err != nil {
		t.Fatal(err)
	}
	ex := &capturingExec{rec: h.rec}
	err := Delete(t.Context(), h.deps, "demo", DeleteOptions{
		Force:    true,
		HookExec: ex,
		Hooks:    []hooks.Hook{preDeleteHook("drain", hooks.Fail)},
	})
	if err != nil {
		t.Fatal(err)
	}
	if strings.Join(ex.names, ",") != "drain" {
		t.Fatalf("fired %v", ex.names)
	}
	if ex.targets[0].Event != hooks.PreDelete || len(ex.targets[0].Kubeconfig) == 0 {
		t.Fatalf("target: %+v", ex.targets[0])
	}
	// The hook ran before any teardown.
	assertOrder(t, h.rec.list(), "hook.Run drain", "infra.Teardown", "store.Delete demo")
}

func TestDeletePreDeleteHookFailAborts(t *testing.T) {
	h := newHarness(t)
	ca := testCA(t)
	h.cp.ca = &ca
	if _, err := Create(t.Context(), h.deps, request()); err != nil {
		t.Fatal(err)
	}
	ex := &capturingExec{errs: map[string]error{"drain": errors.New("still draining")}}
	err := Delete(t.Context(), h.deps, "demo", DeleteOptions{
		Force:    true,
		HookExec: ex,
		Hooks:    []hooks.Hook{preDeleteHook("drain", hooks.Fail)},
	})
	if err == nil || !strings.Contains(err.Error(), "still draining") || !strings.Contains(err.Error(), "preDelete") {
		t.Fatalf("got %v", err)
	}
	// Teardown never began; the cluster is left intact for a retry.
	if h.rec.has("infra.Teardown") || h.rec.has("store.Delete") {
		t.Fatalf("teardown ran despite a failed preDelete hook: %v", h.rec.list())
	}
	if h.store.state("demo") == nil {
		t.Fatal("state removed after an aborted delete")
	}
}

func TestDeleteReservationSkipsHooks(t *testing.T) {
	h := newHarness(t)
	if _, err := h.store.Reserve(t.Context(), "demo"); err != nil {
		t.Fatal(err)
	}
	ex := &capturingExec{}
	err := Delete(t.Context(), h.deps, "demo", DeleteOptions{
		HookExec: ex,
		Hooks:    []hooks.Hook{preDeleteHook("drain", hooks.Fail)},
		Confirm:  func(*infrastructure.Plan) bool { return true },
	})
	if err != nil {
		t.Fatal(err)
	}
	if len(ex.names) != 0 {
		t.Fatalf("fired hooks for a reservation: %v", ex.names)
	}
}
