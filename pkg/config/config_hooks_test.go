package config

import (
	"strings"
	"testing"
	"time"

	"github.com/klotho-k8s/klotho/pkg/hooks"
)

func TestLoadHooks(t *testing.T) {
	isolate(t)
	writeConfig(t, `
hooks:
  - name: flux-bootstrap
    event: postPivot
    command: flux
    args: [bootstrap, github, --owner=me]
    timeout: 10m
    onFailure: fail
  - name: remove-apps
    event: preDelete
    type: exec
    command: ./teardown.sh
    onFailure: warn
`)
	c, err := Load("", "")
	if err != nil {
		t.Fatal(err)
	}
	if len(c.Hooks) != 2 {
		t.Fatalf("hooks: %+v", c.Hooks)
	}

	specs, err := c.HookSpecs()
	if err != nil {
		t.Fatal(err)
	}
	if len(specs) != 2 {
		t.Fatalf("specs: %+v", specs)
	}
	first := specs[0]
	if first.Name != "flux-bootstrap" || first.Event != hooks.PostPivot || first.Type != hooks.Exec {
		t.Errorf("first hook: %+v", first)
	}
	if first.Command != "flux" || len(first.Args) != 3 || first.Timeout != 10*time.Minute || first.OnFailure != hooks.Fail {
		t.Errorf("first hook details: %+v", first)
	}
	second := specs[1]
	if second.Event != hooks.PreDelete || second.OnFailure != hooks.Warn || second.Type != hooks.Exec {
		t.Errorf("second hook: %+v", second)
	}
}

func TestHookSpecsDefaults(t *testing.T) {
	isolate(t)
	// No type and no onFailure: type defaults to exec, policy to fail.
	writeConfig(t, `
hooks:
  - name: h
    event: postPivot
    command: run
`)
	c, err := Load("", "")
	if err != nil {
		t.Fatal(err)
	}
	specs, err := c.HookSpecs()
	if err != nil {
		t.Fatal(err)
	}
	if specs[0].Type != hooks.Exec || specs[0].OnFailure != hooks.Fail || specs[0].Timeout != 0 {
		t.Fatalf("defaults wrong: %+v", specs[0])
	}
}

func TestHookValidation(t *testing.T) {
	isolate(t)
	cases := []struct {
		name string
		body string
		want string
	}{
		{"unknown event", "hooks:\n  - name: h\n    event: whenever\n    command: x\n", "event"},
		{"missing event", "hooks:\n  - name: h\n    command: x\n", "event is required"},
		{"missing name", "hooks:\n  - event: postPivot\n    command: x\n", "needs a name"},
		{"exec without command", "hooks:\n  - name: h\n    event: postPivot\n", "needs a command"},
		{"exec with manifest", "hooks:\n  - name: h\n    event: postPivot\n    command: x\n    manifest: m.yaml\n", "may not set manifest"},
		{"apply without manifest", "hooks:\n  - name: h\n    event: postPivot\n    type: apply\n", "needs a manifest"},
		{"apply with command", "hooks:\n  - name: h\n    event: postPivot\n    type: apply\n    manifest: m.yaml\n    command: x\n", "may not set command"},
		{"unknown type", "hooks:\n  - name: h\n    event: postPivot\n    type: magic\n    command: x\n", "type"},
		{"bad onFailure", "hooks:\n  - name: h\n    event: postPivot\n    command: x\n    onFailure: explode\n", "onFailure"},
		{"bad timeout", "hooks:\n  - name: h\n    event: postPivot\n    command: x\n    timeout: soon\n", "timeout"},
		{"negative timeout", "hooks:\n  - name: h\n    event: postPivot\n    command: x\n    timeout: -5m\n", "timeout"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			writeConfig(t, tc.body)
			_, err := Load("", "")
			if err == nil {
				t.Fatalf("%s accepted", tc.name)
			}
			if !strings.Contains(err.Error(), tc.want) {
				t.Fatalf("error %q lacks %q", err, tc.want)
			}
		})
	}
}

func TestHookUnknownFieldRejected(t *testing.T) {
	isolate(t)
	writeConfig(t, "hooks:\n  - name: h\n    event: postPivot\n    command: x\n    bogus: 1\n")
	if _, err := Load("", ""); err == nil {
		t.Fatal("unknown hook field accepted")
	}
}

func TestNoHooksIsNil(t *testing.T) {
	isolate(t)
	c, err := Load("", "")
	if err != nil {
		t.Fatal(err)
	}
	specs, err := c.HookSpecs()
	if err != nil || specs != nil {
		t.Fatalf("specs = %v, err = %v", specs, err)
	}
}
