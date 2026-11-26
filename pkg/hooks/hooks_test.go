package hooks

import (
	"context"
	"errors"
	"strings"
	"testing"
)

// fakeExec records the hooks it is asked to run, in order, and returns a canned
// error per hook name. It also captures the target each hook saw.
type fakeExec struct {
	calls  []string
	events []Event
	errs   map[string]error
}

func (f *fakeExec) Run(_ context.Context, h Hook, t Target) error {
	f.calls = append(f.calls, h.Name)
	f.events = append(f.events, t.Event)
	return f.errs[h.Name]
}

func hook(name string, event Event, policy Policy) Hook {
	return Hook{Name: name, Event: event, Type: Exec, Command: "true", OnFailure: policy}
}

func TestRunFiresMatchingEventInOrder(t *testing.T) {
	hooks := []Hook{
		hook("a", PostPivot, Fail),
		hook("skip-me", PreDelete, Fail),
		hook("b", PostPivot, Fail),
		hook("c", PostPivot, Fail),
	}
	ex := &fakeExec{}
	if err := Run(t.Context(), nil, ex, PostPivot, Target{Cluster: "demo"}, hooks); err != nil {
		t.Fatal(err)
	}
	if strings.Join(ex.calls, ",") != "a,b,c" {
		t.Fatalf("order: %v", ex.calls)
	}
	for _, e := range ex.events {
		if e != PostPivot {
			t.Fatalf("target event = %q, want postPivot", e)
		}
	}
}

func TestRunNoHooksForEvent(t *testing.T) {
	ex := &fakeExec{}
	if err := Run(t.Context(), nil, ex, PostDelete, Target{}, []Hook{hook("a", PostPivot, Fail)}); err != nil {
		t.Fatal(err)
	}
	if len(ex.calls) != 0 {
		t.Fatalf("ran %v for an event with no hooks", ex.calls)
	}
}

func TestRunFailPolicyStops(t *testing.T) {
	hooks := []Hook{
		hook("a", PostPivot, Fail),
		hook("b", PostPivot, Fail),
		hook("c", PostPivot, Fail),
	}
	ex := &fakeExec{errs: map[string]error{"b": errors.New("boom")}}
	err := Run(t.Context(), nil, ex, PostPivot, Target{}, hooks)
	if err == nil || !strings.Contains(err.Error(), "boom") ||
		!strings.Contains(err.Error(), "postPivot") || !strings.Contains(err.Error(), `"b"`) {
		t.Fatalf("got %v", err)
	}
	// a ran, b failed and stopped the dispatch, c never ran.
	if strings.Join(ex.calls, ",") != "a,b" {
		t.Fatalf("calls: %v", ex.calls)
	}
}

func TestRunWarnPolicyContinues(t *testing.T) {
	hooks := []Hook{
		hook("a", PostPivot, Warn),
		hook("b", PostPivot, Warn),
		hook("c", PostPivot, Fail),
	}
	ex := &fakeExec{errs: map[string]error{"a": errors.New("boom")}}
	if err := Run(t.Context(), nil, ex, PostPivot, Target{}, hooks); err != nil {
		t.Fatalf("warn policy must not fail the run: %v", err)
	}
	if strings.Join(ex.calls, ",") != "a,b,c" {
		t.Fatalf("calls: %v", ex.calls)
	}
}

func TestRunEmptyPolicyIsFail(t *testing.T) {
	ex := &fakeExec{errs: map[string]error{"a": errors.New("boom")}}
	err := Run(t.Context(), nil, ex, PostPivot, Target{}, []Hook{hook("a", PostPivot, "")})
	if err == nil {
		t.Fatal("empty policy must behave as fail")
	}
}

func TestAny(t *testing.T) {
	hooks := []Hook{hook("a", PostPivot, Fail), hook("b", PreDelete, Fail)}
	if !Any(hooks, PostPivot) || !Any(hooks, PreDelete) {
		t.Fatal("Any missed a present event")
	}
	if Any(hooks, PostDelete) || Any(nil, PostPivot) {
		t.Fatal("Any reported an absent event")
	}
}

func TestEventTypePolicyValidity(t *testing.T) {
	for _, e := range Events() {
		if !e.Valid() {
			t.Errorf("registered event %q is invalid", e)
		}
	}
	if Event("bogus").Valid() {
		t.Error("unknown event accepted")
	}
	if !Exec.Valid() || !Apply.Valid() || Type("bogus").Valid() {
		t.Error("type validity wrong")
	}
	if !Policy("").Valid() || !Fail.Valid() || !Warn.Valid() || Policy("bogus").Valid() {
		t.Error("policy validity wrong")
	}
	// The registry carries the deferred events so configuration validates now.
	if !PostBootstrap.Valid() || !PostDelete.Valid() {
		t.Error("deferred events must be registered")
	}
}
