package cli

import (
	"strings"
	"testing"
)

func TestUpgradeNameRequired(t *testing.T) {
	isolate(t)
	r := run(t, "", "upgrade", "--kubernetes-version", "v1.33.0")
	if r.runErr == nil || !strings.Contains(r.runErr.Error(), "cluster name") {
		t.Fatalf("want a missing-name error, got %v", r.runErr)
	}
}

func TestUpgradeNothingToDo(t *testing.T) {
	isolate(t)
	// With neither a Kubernetes version nor any component flag, the command has
	// nothing to do and must say so before touching configuration or the cluster.
	r := run(t, "", "upgrade", "demo")
	if r.runErr == nil || !strings.Contains(r.runErr.Error(), "nothing to do") {
		t.Fatalf("want a nothing-to-do error, got %v", r.runErr)
	}
}

func TestUpgradeComponentsOnlyAccepted(t *testing.T) {
	isolate(t)
	// A components-only invocation (no --kubernetes-version) is accepted by the
	// flag/validation layer; it fails later only for want of configuration, not
	// for a missing version.
	r := run(t, "", "upgrade", "demo", "--capi-version", "v1.12.9")
	if r.runErr != nil && strings.Contains(r.runErr.Error(), "nothing to do") {
		t.Fatalf("components-only upgrade wrongly rejected as nothing-to-do: %v", r.runErr)
	}
}

func TestUpgradeNameTwice(t *testing.T) {
	isolate(t)
	r := run(t, "", "upgrade", "demo", "--cluster", "other", "--kubernetes-version", "v1.33.0")
	if r.runErr == nil || !strings.Contains(r.runErr.Error(), "named twice") {
		t.Fatalf("want a named-twice error, got %v", r.runErr)
	}
}

func TestUpgradeInHelp(t *testing.T) {
	r := run(t, "", "--help")
	if r.runErr != nil {
		t.Fatalf("help: %v", r.runErr)
	}
	if !strings.Contains(r.out, "upgrade") {
		t.Fatalf("help should list upgrade:\n%s", r.out)
	}
}
