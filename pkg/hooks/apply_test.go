package hooks

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"

	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime/schema"
	k8stypes "k8s.io/apimachinery/pkg/types"

	"github.com/klotho-k8s/klotho/pkg/kube"
)

// fakeKube is a kube.Cluster that records applies and answers Get from a set of
// known objects, so apply tests can assert order and namespace creation without
// a real cluster.
type fakeKube struct {
	applied  []string
	exists   map[string]bool
	applyErr error
}

func newFakeKube() *fakeKube { return &fakeKube{exists: map[string]bool{}} }

func objKeyStr(gvk schema.GroupVersionKind, ns, name string) string {
	return gvk.Kind + "/" + ns + "/" + name
}

func (f *fakeKube) Ready(context.Context) error { return nil }

func (f *fakeKube) Apply(_ context.Context, _ string, obj *unstructured.Unstructured) error {
	if f.applyErr != nil {
		return f.applyErr
	}
	f.applied = append(f.applied, describeObject(obj))
	f.exists[objKeyStr(obj.GroupVersionKind(), obj.GetNamespace(), obj.GetName())] = true
	return nil
}

func (f *fakeKube) Get(_ context.Context, gvk schema.GroupVersionKind, ns, name string) (*unstructured.Unstructured, error) {
	if f.exists[objKeyStr(gvk, ns, name)] {
		return &unstructured.Unstructured{}, nil
	}
	return nil, apierrors.NewNotFound(schema.GroupResource{Resource: strings.ToLower(gvk.Kind)}, name)
}

func (f *fakeKube) Patch(context.Context, schema.GroupVersionKind, string, string, k8stypes.PatchType, []byte) error {
	return nil
}
func (f *fakeKube) Delete(context.Context, schema.GroupVersionKind, string, string) error { return nil }
func (f *fakeKube) List(context.Context, schema.GroupVersionKind, string) ([]*unstructured.Unstructured, error) {
	return nil, nil
}

// engineWith returns an Engine wired to fake as its cluster client.
func engineWith(fake kube.Cluster) Engine {
	e := NewEngine(nil)
	e.connect = func(context.Context, []byte) (kube.Cluster, error) { return fake, nil }
	return e
}

const multiDocManifest = `apiVersion: v1
kind: Namespace
metadata:
  name: demo
---
apiVersion: v1
kind: ConfigMap
metadata:
  name: cm1
  namespace: demo
data:
  a: b
---
apiVersion: v1
kind: ConfigMap
metadata:
  name: cm2
  namespace: other
data:
  c: d
`

func applyHook(manifest string) Hook {
	return Hook{Name: "apply", Event: PostPivot, Type: Apply, Manifest: manifest}
}

func TestApplyManifestFile(t *testing.T) {
	path := filepath.Join(t.TempDir(), "m.yaml")
	if err := os.WriteFile(path, []byte(multiDocManifest), 0o600); err != nil {
		t.Fatal(err)
	}
	fake := newFakeKube()
	if err := engineWith(fake).Run(t.Context(), applyHook(path), Target{Cluster: "c"}); err != nil {
		t.Fatal(err)
	}
	want := []string{
		"Namespace demo",      // namespace defined in the manifest, applied first
		"ConfigMap demo/cm1",  // its namespace already created, no placeholder
		"Namespace other",     // created for cm2's namespace
		"ConfigMap other/cm2", //
	}
	if strings.Join(fake.applied, ",") != strings.Join(want, ",") {
		t.Fatalf("applied:\n got %v\nwant %v", fake.applied, want)
	}
}

func TestApplyManifestDir(t *testing.T) {
	dir := t.TempDir()
	write := func(name, body string) {
		if err := os.WriteFile(filepath.Join(dir, name), []byte(body), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	// Sorted filename order drives apply order; a non-YAML file is ignored.
	write("02-cm.yaml", "apiVersion: v1\nkind: ConfigMap\nmetadata:\n  name: second\n")
	write("01-cm.yml", "apiVersion: v1\nkind: ConfigMap\nmetadata:\n  name: first\n")
	write("notes.txt", "ignore me")
	fake := newFakeKube()
	if err := engineWith(fake).Run(t.Context(), applyHook(dir), Target{Cluster: "c"}); err != nil {
		t.Fatal(err)
	}
	want := "ConfigMap first,ConfigMap second"
	if strings.Join(fake.applied, ",") != want {
		t.Fatalf("applied %v, want %v", fake.applied, want)
	}
}

func TestApplyManifestURL(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write([]byte(multiDocManifest))
	}))
	defer srv.Close()
	fake := newFakeKube()
	if err := engineWith(fake).Run(t.Context(), applyHook(srv.URL), Target{Cluster: "c"}); err != nil {
		t.Fatal(err)
	}
	if len(fake.applied) != 4 {
		t.Fatalf("applied %v", fake.applied)
	}
}

func TestApplyManifestURLStatusError(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		http.Error(w, "nope", http.StatusNotFound)
	}))
	defer srv.Close()
	err := engineWith(newFakeKube()).Run(t.Context(), applyHook(srv.URL), Target{Cluster: "c"})
	if err == nil || !strings.Contains(err.Error(), "status") {
		t.Fatalf("got %v", err)
	}
}

func TestApplyManifestExistingNamespaceNotRecreated(t *testing.T) {
	path := filepath.Join(t.TempDir(), "m.yaml")
	body := "apiVersion: v1\nkind: ConfigMap\nmetadata:\n  name: cm\n  namespace: prod\n"
	if err := os.WriteFile(path, []byte(body), 0o600); err != nil {
		t.Fatal(err)
	}
	fake := newFakeKube()
	fake.exists[objKeyStr(namespaceGVK, "", "prod")] = true // prod already exists
	if err := engineWith(fake).Run(t.Context(), applyHook(path), Target{Cluster: "c"}); err != nil {
		t.Fatal(err)
	}
	if strings.Join(fake.applied, ",") != "ConfigMap prod/cm" {
		t.Fatalf("existing namespace must not be reapplied: %v", fake.applied)
	}
}

func TestApplyManifestMissingSource(t *testing.T) {
	err := engineWith(newFakeKube()).Run(t.Context(), applyHook(filepath.Join(t.TempDir(), "nope.yaml")), Target{})
	if err == nil {
		t.Fatal("missing manifest must error")
	}
}

func TestApplyFailureRoutedThroughPolicy(t *testing.T) {
	path := filepath.Join(t.TempDir(), "m.yaml")
	if err := os.WriteFile(path, []byte("apiVersion: v1\nkind: ConfigMap\nmetadata:\n  name: cm\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	boom := errors.New("apply boom")

	// fail policy aborts the run.
	fake := newFakeKube()
	fake.applyErr = boom
	h := applyHook(path)
	h.OnFailure = Fail
	if err := Run(t.Context(), nil, engineWith(fake), PostPivot, Target{}, []Hook{h}); err == nil ||
		!strings.Contains(err.Error(), "apply boom") {
		t.Fatalf("fail policy: got %v", err)
	}

	// warn policy continues.
	fake2 := newFakeKube()
	fake2.applyErr = boom
	h2 := applyHook(path)
	h2.OnFailure = Warn
	if err := Run(t.Context(), nil, engineWith(fake2), PostPivot, Target{}, []Hook{h2}); err != nil {
		t.Fatalf("warn policy must not fail the run: %v", err)
	}
}

func TestRunApplyRejectsBothAndNeither(t *testing.T) {
	e := NewEngine(nil)
	both := Hook{Name: "x", Event: PostPivot, Type: Apply, Manifest: "m", Helm: &HelmSpec{Chart: "c"}}
	if err := e.runApply(t.Context(), both, Target{}); err == nil || !strings.Contains(err.Error(), "both") {
		t.Fatalf("both: got %v", err)
	}
	neither := Hook{Name: "x", Event: PostPivot, Type: Apply}
	if err := e.runApply(t.Context(), neither, Target{}); err == nil || !strings.Contains(err.Error(), "neither") {
		t.Fatalf("neither: got %v", err)
	}
}
