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

func TestUpgradeVersionRequired(t *testing.T) {
	isolate(t)
	r := run(t, "", "upgrade", "demo")
	if r.runErr == nil || !strings.Contains(r.runErr.Error(), "kubernetes-version") {
		t.Fatalf("want a required-flag error for --kubernetes-version, got %v", r.runErr)
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
