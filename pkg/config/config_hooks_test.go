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
		{"apply without manifest", "hooks:\n  - name: h\n    event: postPivot\n    type: apply\n", "needs a manifest or helm"},
		{"apply with command", "hooks:\n  - name: h\n    event: postPivot\n    type: apply\n    manifest: m.yaml\n    command: x\n", "may not set command"},
		{"apply both manifest and helm", "hooks:\n  - name: h\n    event: postPivot\n    type: apply\n    manifest: m.yaml\n    helm:\n      chart: stable/nginx\n", "exactly one of manifest or helm"},
		{"apply helm without chart", "hooks:\n  - name: h\n    event: postPivot\n    type: apply\n    helm:\n      version: 1.2.3\n", "helm needs a chart"},
		{"exec with helm", "hooks:\n  - name: h\n    event: postPivot\n    command: x\n    helm:\n      chart: stable/nginx\n", "may not set helm"},
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

func TestLoadApplyManifestHook(t *testing.T) {
	isolate(t)
	writeConfig(t, `
hooks:
  - name: cni
    event: postPivot
    type: apply
    manifest: ./manifests/
    timeout: 5m
    onFailure: warn
`)
	c, err := Load("", "")
	if err != nil {
		t.Fatal(err)
	}
	specs, err := c.HookSpecs()
	if err != nil {
		t.Fatal(err)
	}
	s := specs[0]
	if s.Type != hooks.Apply || s.Manifest != "./manifests/" || s.Helm != nil {
		t.Fatalf("apply manifest hook: %+v", s)
	}
	if s.Timeout != 5*time.Minute || s.OnFailure != hooks.Warn {
		t.Fatalf("apply manifest details: %+v", s)
	}
}

func TestLoadApplyHelmHookValuesFile(t *testing.T) {
	isolate(t)
	writeConfig(t, `
hooks:
  - name: ingress
    event: postPivot
    type: apply
    helm:
      chart: oci://ghcr.io/nginx/charts/nginx-ingress
      version: 2.3.0
      namespace: ingress
      releaseName: edge
      values: ./values.yaml
`)
	c, err := Load("", "")
	if err != nil {
		t.Fatal(err)
	}
	specs, err := c.HookSpecs()
	if err != nil {
		t.Fatal(err)
	}
	h := specs[0].Helm
	if h == nil {
		t.Fatal("helm spec is nil")
	}
	if h.Chart != "oci://ghcr.io/nginx/charts/nginx-ingress" || h.Version != "2.3.0" ||
		h.Namespace != "ingress" || h.ReleaseName != "edge" {
		t.Fatalf("helm spec: %+v", h)
	}
	if h.ValuesFile != "./values.yaml" || h.Values != nil {
		t.Fatalf("values should be a file path: %+v", h)
	}
}

func TestLoadApplyHelmHookInlineValues(t *testing.T) {
	isolate(t)
	writeConfig(t, `
hooks:
  - name: ingress
    event: postPivot
    type: apply
    helm:
      chart: stable/nginx
      values:
        replicaCount: 3
        service:
          type: LoadBalancer
`)
	c, err := Load("", "")
	if err != nil {
		t.Fatal(err)
	}
	specs, err := c.HookSpecs()
	if err != nil {
		t.Fatal(err)
	}
	h := specs[0].Helm
	if h == nil || h.ValuesFile != "" {
		t.Fatalf("inline values should not set a file: %+v", h)
	}
	if got, _ := h.Values["replicaCount"].(float64); got != 3 {
		t.Fatalf("inline replicaCount: %+v", h.Values)
	}
	svc, ok := h.Values["service"].(map[string]any)
	if !ok || svc["type"] != "LoadBalancer" {
		t.Fatalf("nested inline values: %+v", h.Values)
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
