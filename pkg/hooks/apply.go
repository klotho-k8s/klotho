package hooks

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"

	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime/schema"

	"github.com/klotho-k8s/klotho/pkg/generate"
	"github.com/klotho-k8s/klotho/pkg/kube"
)

// namespaceGVK is the core Namespace kind klotho creates to back a manifest's
// objects.
var namespaceGVK = schema.GroupVersionKind{Group: "", Version: "v1", Kind: "Namespace"}

// runApply dispatches an apply hook to its manifest or Helm path. Validation in
// pkg/config guarantees exactly one is set; the engine re-checks so a
// hand-built Hook fails cleanly rather than silently doing nothing.
func (e Engine) runApply(ctx context.Context, h Hook, t Target) error {
	switch {
	case h.Manifest != "" && h.Helm != nil:
		return errors.New("apply hook sets both manifest and helm")
	case h.Manifest != "":
		return e.applyManifest(ctx, h, t)
	case h.Helm != nil:
		return e.applyHelm(ctx, h, t)
	default:
		return errors.New("apply hook has neither manifest nor helm")
	}
}

// applyManifest loads the hook's manifest (a file, a directory of YAML, or an
// http(s) URL), splits it into objects, and server-side applies each to the
// booted cluster through klotho's own client. Namespaces an object needs are
// created first. Progress streams into the logger as each object is applied.
func (e Engine) applyManifest(ctx context.Context, h Hook, t Target) error {
	data, err := loadManifest(ctx, h.Manifest)
	if err != nil {
		return err
	}
	bundle, err := generate.ParseBundle(data)
	if err != nil {
		return fmt.Errorf("manifest %s: %w", h.Manifest, err)
	}
	if len(bundle.Objects) == 0 {
		return fmt.Errorf("manifest %s: no objects", h.Manifest)
	}

	connect := e.connect
	if connect == nil {
		connect = kube.New
	}
	cl, err := connect(ctx, t.Kubeconfig)
	if err != nil {
		return fmt.Errorf("manifest %s: %w", h.Manifest, err)
	}

	// Apply Namespace objects the manifest itself defines first, so their
	// labels and annotations win over any klotho-created placeholder.
	created := map[string]bool{}
	for _, obj := range bundle.Objects {
		if isNamespace(obj) {
			if err := e.applyObject(ctx, cl, h.Name, obj); err != nil {
				return err
			}
			created[obj.GetName()] = true
		}
	}

	for _, obj := range bundle.Objects {
		if isNamespace(obj) {
			continue
		}
		if ns := obj.GetNamespace(); ns != "" && !created[ns] {
			if err := ensureNamespace(ctx, cl, ns); err != nil {
				return fmt.Errorf("creating namespace %q: %w", ns, err)
			}
			created[ns] = true
			e.log.Info("hook ensured namespace", "name", h.Name, "namespace", ns)
		}
		if err := e.applyObject(ctx, cl, h.Name, obj); err != nil {
			return err
		}
	}
	return nil
}

// applyObject server-side applies one object and logs it.
func (e Engine) applyObject(ctx context.Context, cl kube.Cluster, hookName string, obj *unstructured.Unstructured) error {
	if err := cl.Apply(ctx, kube.FieldManager, obj); err != nil {
		return err
	}
	e.log.Info("hook applied", "name", hookName, "object", describeObject(obj))
	return nil
}

// ensureNamespace creates ns when it does not already exist. An existing
// namespace is left untouched so klotho does not stamp its metadata.
func ensureNamespace(ctx context.Context, cl kube.Cluster, ns string) error {
	if _, err := cl.Get(ctx, namespaceGVK, "", ns); err == nil {
		return nil
	} else if !apierrors.IsNotFound(err) {
		return err
	}
	obj := &unstructured.Unstructured{Object: map[string]any{
		"apiVersion": "v1",
		"kind":       "Namespace",
		"metadata":   map[string]any{"name": ns},
	}}
	return cl.Apply(ctx, kube.FieldManager, obj)
}

func isNamespace(obj *unstructured.Unstructured) bool {
	return obj.GroupVersionKind() == namespaceGVK
}

func describeObject(o *unstructured.Unstructured) string {
	s := o.GetKind()
	if ns := o.GetNamespace(); ns != "" {
		s += " " + ns + "/" + o.GetName()
	} else {
		s += " " + o.GetName()
	}
	return s
}

// loadManifest reads a manifest source: an http(s) URL, a directory of YAML
// files, or a single file. A directory's *.yaml and *.yml files are read in
// sorted order and joined into one multi-document stream.
func loadManifest(ctx context.Context, source string) ([]byte, error) {
	if isURL(source) {
		return fetchURL(ctx, source)
	}
	info, err := os.Stat(source)
	if err != nil {
		return nil, fmt.Errorf("manifest %s: %w", source, err)
	}
	if info.IsDir() {
		return readManifestDir(source)
	}
	data, err := os.ReadFile(source)
	if err != nil {
		return nil, fmt.Errorf("manifest %s: %w", source, err)
	}
	return data, nil
}

func isURL(s string) bool {
	return strings.HasPrefix(s, "http://") || strings.HasPrefix(s, "https://")
}

// readManifestDir joins every YAML file directly under dir into a single
// multi-document stream, in sorted filename order.
func readManifestDir(dir string) ([]byte, error) {
	entries, err := os.ReadDir(dir)
	if err != nil {
		return nil, fmt.Errorf("manifest %s: %w", dir, err)
	}
	var names []string
	for _, e := range entries {
		if e.IsDir() {
			continue
		}
		switch strings.ToLower(filepath.Ext(e.Name())) {
		case ".yaml", ".yml":
			names = append(names, e.Name())
		}
	}
	if len(names) == 0 {
		return nil, fmt.Errorf("manifest %s: no YAML files in directory", dir)
	}
	sort.Strings(names)
	var buf strings.Builder
	for i, name := range names {
		b, err := os.ReadFile(filepath.Join(dir, name))
		if err != nil {
			return nil, fmt.Errorf("manifest %s: %w", filepath.Join(dir, name), err)
		}
		if i > 0 {
			buf.WriteString("\n---\n")
		}
		buf.Write(b)
	}
	return []byte(buf.String()), nil
}

// fetchURL downloads a manifest over http(s), bounded by ctx.
func fetchURL(ctx context.Context, url string) ([]byte, error) {
	reqCtx := ctx
	if _, ok := ctx.Deadline(); !ok {
		var cancel context.CancelFunc
		reqCtx, cancel = context.WithTimeout(ctx, 60*time.Second)
		defer cancel()
	}
	req, err := http.NewRequestWithContext(reqCtx, http.MethodGet, url, nil)
	if err != nil {
		return nil, fmt.Errorf("manifest %s: %w", url, err)
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return nil, fmt.Errorf("manifest %s: %w", url, err)
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("manifest %s: status %s", url, resp.Status)
	}
	body, err := io.ReadAll(resp.Body)
	if err != nil {
		return nil, fmt.Errorf("manifest %s: %w", url, err)
	}
	return body, nil
}
