package hooks

import (
	"context"
	"io"
	"log/slog"
	"os"
	"path/filepath"
	"testing"

	"helm.sh/helm/v3/pkg/action"
	"helm.sh/helm/v3/pkg/chartutil"
	kubefake "helm.sh/helm/v3/pkg/kube/fake"
	"helm.sh/helm/v3/pkg/storage"
	"helm.sh/helm/v3/pkg/storage/driver"
)

// memHelm is an in-memory Helm action configuration: a memory release store and
// a no-op kube client, so install/upgrade runs without a cluster.
func memHelm() *action.Configuration {
	return &action.Configuration{
		Releases:     storage.Init(driver.NewMemory()),
		KubeClient:   &kubefake.PrintingKubeClient{Out: io.Discard},
		Capabilities: chartutil.DefaultCapabilities,
		Log:          func(string, ...any) {},
	}
}

// writeChart writes a minimal, valid chart to a temp directory and returns its
// path.
func writeChart(t *testing.T) string {
	t.Helper()
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "Chart.yaml"),
		[]byte("apiVersion: v2\nname: demo\nversion: 0.1.0\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.Mkdir(filepath.Join(dir, "templates"), 0o750); err != nil {
		t.Fatal(err)
	}
	tmpl := "apiVersion: v1\nkind: ConfigMap\nmetadata:\n  name: {{ .Release.Name }}-cm\ndata:\n  replicas: \"{{ .Values.replicaCount | default 1 }}\"\n"
	if err := os.WriteFile(filepath.Join(dir, "templates", "cm.yaml"), []byte(tmpl), 0o600); err != nil {
		t.Fatal(err)
	}
	return dir
}

func TestHelmInstallThenUpgrade(t *testing.T) {
	chart := writeChart(t)
	cfg := memHelm() // one shared store across both runs
	e := NewEngine(slog.New(slog.DiscardHandler))
	e.helmActions = func(context.Context, *slog.Logger, string, string, string) (*action.Configuration, error) {
		return cfg, nil
	}

	h := Hook{
		Name:  "ingress",
		Event: PostPivot,
		Type:  Apply,
		Helm: &HelmSpec{
			Chart:     chart,
			Namespace: "apps",
			Values:    map[string]any{"replicaCount": 2},
		},
	}

	// First run installs: history is empty, so it is an install.
	if err := e.Run(t.Context(), h, Target{Cluster: "c"}); err != nil {
		t.Fatalf("install: %v", err)
	}
	hist, err := cfg.Releases.History("demo")
	if err != nil {
		t.Fatalf("history after install: %v", err)
	}
	if len(hist) != 1 || hist[0].Version != 1 {
		t.Fatalf("expected one revision after install, got %d", len(hist))
	}
	if hist[0].Namespace != "apps" {
		t.Fatalf("release namespace = %q", hist[0].Namespace)
	}

	// Second run upgrades: the release now exists.
	if err := e.Run(t.Context(), h, Target{Cluster: "c"}); err != nil {
		t.Fatalf("upgrade: %v", err)
	}
	hist, err = cfg.Releases.History("demo")
	if err != nil {
		t.Fatalf("history after upgrade: %v", err)
	}
	if len(hist) != 2 {
		t.Fatalf("expected two revisions after upgrade, got %d", len(hist))
	}
}

func TestHelmReleaseNameOverride(t *testing.T) {
	chart := writeChart(t)
	cfg := memHelm()
	e := NewEngine(nil)
	e.helmActions = func(context.Context, *slog.Logger, string, string, string) (*action.Configuration, error) {
		return cfg, nil
	}
	h := Hook{
		Name:  "h",
		Event: PostPivot,
		Type:  Apply,
		Helm:  &HelmSpec{Chart: chart, ReleaseName: "edge"},
	}
	if err := e.Run(t.Context(), h, Target{}); err != nil {
		t.Fatalf("install: %v", err)
	}
	if _, err := cfg.Releases.History("edge"); err != nil {
		t.Fatalf("release should be named edge: %v", err)
	}
}

func TestHelmValuesFileAndInlineMerge(t *testing.T) {
	dir := t.TempDir()
	valuesFile := filepath.Join(dir, "values.yaml")
	if err := os.WriteFile(valuesFile,
		[]byte("replicaCount: 1\nimage:\n  tag: stable\n  pullPolicy: IfNotPresent\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	spec := &HelmSpec{
		ValuesFile: valuesFile,
		Values: map[string]any{
			"replicaCount": 5,
			"image":        map[string]any{"tag": "canary"},
		},
	}
	vals, err := helmValues(spec)
	if err != nil {
		t.Fatal(err)
	}
	// inline overrides the file (deep-copied through JSON, so a number is float64).
	if got, _ := vals["replicaCount"].(float64); got != 5 {
		t.Fatalf("replicaCount = %v, want inline 5", vals["replicaCount"])
	}
	img, ok := vals["image"].(map[string]any)
	if !ok {
		t.Fatalf("image not a map: %T", vals["image"])
	}
	if img["tag"] != "canary" {
		t.Fatalf("image.tag = %v, want inline canary", img["tag"])
	}
	// file-only nested key survives the merge.
	if img["pullPolicy"] != "IfNotPresent" {
		t.Fatalf("image.pullPolicy = %v, want file value preserved", img["pullPolicy"])
	}
}

func TestHelmValuesFileMissing(t *testing.T) {
	_, err := helmValues(&HelmSpec{ValuesFile: filepath.Join(t.TempDir(), "nope.yaml")})
	if err == nil {
		t.Fatal("missing values file must error")
	}
}

func TestHelmBadChart(t *testing.T) {
	cfg := memHelm()
	e := NewEngine(nil)
	e.helmActions = func(context.Context, *slog.Logger, string, string, string) (*action.Configuration, error) {
		return cfg, nil
	}
	h := Hook{Name: "h", Event: PostPivot, Type: Apply, Helm: &HelmSpec{Chart: filepath.Join(t.TempDir(), "no-such-chart")}}
	if err := e.Run(t.Context(), h, Target{}); err == nil {
		t.Fatal("a missing chart must error")
	}
}
