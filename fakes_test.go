package klotho

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"sync"

	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/apimachinery/pkg/types"

	"github.com/klotho-k8s/klotho/pkg/cluster"
	"github.com/klotho-k8s/klotho/pkg/generate"
	"github.com/klotho-k8s/klotho/pkg/state"
	"github.com/klotho-k8s/klotho/provider"
	"github.com/klotho-k8s/klotho/provider/backend"
	"github.com/klotho-k8s/klotho/provider/controlplane"
	"github.com/klotho-k8s/klotho/provider/infrastructure"
)

// recorder is the shared call log every fake appends to.
type recorder struct {
	mu    sync.Mutex
	calls []string
}

func (r *recorder) add(format string, args ...any) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.calls = append(r.calls, fmt.Sprintf(format, args...))
}

func (r *recorder) list() []string {
	r.mu.Lock()
	defer r.mu.Unlock()
	return append([]string(nil), r.calls...)
}

func (r *recorder) has(prefix string) bool {
	for _, c := range r.list() {
		if strings.HasPrefix(c, prefix) {
			return true
		}
	}
	return false
}

// --- store

type fakeStore struct {
	rec     *recorder
	records map[string][]byte
}

func newFakeStore(rec *recorder) *fakeStore {
	return &fakeStore{rec: rec, records: map[string][]byte{}}
}

func (s *fakeStore) Name() string { return "fake" }

func (s *fakeStore) Reserve(_ context.Context, name string) (bool, error) {
	s.rec.add("store.Reserve %s", name)
	if _, ok := s.records[name]; ok {
		return false, nil
	}
	r := state.NewReservation()
	r.Spec.Name = name
	b, err := state.PlainCodec{}.Seal(r)
	if err != nil {
		return false, err
	}
	s.records[name] = b
	return true, nil
}

func (s *fakeStore) Load(_ context.Context, name string) ([]byte, error) {
	b, ok := s.records[name]
	if !ok {
		return nil, backend.ErrNotFound
	}
	return b, nil
}

func (s *fakeStore) Save(_ context.Context, name string, record []byte) error {
	s.rec.add("store.Save %s", name)
	s.records[name] = record
	return nil
}

func (s *fakeStore) Delete(_ context.Context, name string) error {
	s.rec.add("store.Delete %s", name)
	if _, ok := s.records[name]; !ok {
		return backend.ErrNotFound
	}
	delete(s.records, name)
	return nil
}

func (s *fakeStore) List(context.Context) ([]string, error) {
	var out []string
	for n := range s.records {
		out = append(out, n)
	}
	return out, nil
}

func (s *fakeStore) state(name string) *cluster.State {
	b, ok := s.records[name]
	if !ok {
		return nil
	}
	st, err := state.PlainCodec{}.Open(b)
	if err != nil {
		panic(err)
	}
	return st
}

// --- infrastructure

type fakeInfra struct {
	rec            *recorder
	created        []cluster.Resource
	vars           map[string]string
	fail           map[string]error
	prepSpec       *cluster.Spec
	clientEndpoint cluster.Endpoint
	// planSeq, when non-nil, is the number of resources successive Plan calls
	// report; after it is exhausted Plan reports none. Nil keeps the default
	// of one resource, so a watched delete can drain to empty.
	planSeq []int
	planIdx int
}

func (f *fakeInfra) Name() string                                               { return "fake" }
func (f *fakeInfra) Tag(name string) string                                     { return "klotho-" + name }
func (f *fakeInfra) err(op string) error                                        { return f.fail[op] }
func (f *fakeInfra) Discover(context.Context) (infrastructure.Inventory, error) { return nil, nil }

func (f *fakeInfra) Prepare(_ context.Context, spec *cluster.Spec) (*infrastructure.Prepared, error) {
	f.rec.add("infra.Prepare")
	s := *spec
	f.prepSpec = &s
	if err := f.err("Prepare"); err != nil {
		return nil, err
	}
	return &infrastructure.Prepared{
		Endpoint:       cluster.Endpoint{Host: "10.0.0.10", Port: 6443},
		ClientEndpoint: f.clientEndpoint,
		Created:        f.created,
		Variables:      f.vars,
	}, nil
}

func (f *fakeInfra) Launch(_ context.Context, _ *cluster.Spec, userData []byte) (*cluster.Node, error) {
	f.rec.add("infra.Launch")
	if err := f.err("Launch"); err != nil {
		return nil, err
	}
	if !strings.HasPrefix(string(userData), "#cloud-config") {
		return nil, errors.New("user-data is not cloud-config")
	}
	return &cluster.Node{ID: "fake://1", Name: "demo-bootstrap", Addresses: []string{"10.0.0.20"}}, nil
}

func (f *fakeInfra) Adopt(_ context.Context, st *cluster.State, bundle []*unstructured.Unstructured) ([]byte, error) {
	f.rec.add("infra.Adopt bootstrap=%v objects=%d", st.Bootstrap != nil, len(bundle))
	if st.Bootstrap == nil {
		return nil, errors.New("no bootstrap node")
	}
	return []byte("apiVersion: infrastructure.cluster.x-k8s.io/v1alpha1\nkind: FakeMachine\nmetadata:\n  name: demo-bootstrap\n  namespace: default\nspec:\n  providerID: fake://1\n"), nil
}

func (f *fakeInfra) Plan(_ context.Context, _ *cluster.State) (*infrastructure.Plan, error) {
	f.rec.add("infra.Plan")
	n := 1
	if f.planSeq != nil {
		if f.planIdx < len(f.planSeq) {
			n = f.planSeq[f.planIdx]
		} else {
			n = 0
		}
		f.planIdx++
	}
	res := make([]cluster.Resource, n)
	for i := range res {
		res[i] = cluster.Resource{Kind: "vm", ID: fmt.Sprintf("%d", i+1)}
	}
	return &infrastructure.Plan{Resources: res}, nil
}

func (f *fakeInfra) Teardown(context.Context, *cluster.State, *infrastructure.Plan) error {
	f.rec.add("infra.Teardown")
	return nil
}

// LaunchDeletionSidecar makes fakeInfra an infrastructure.DeletionSidecar.
func (f *fakeInfra) LaunchDeletionSidecar(_ context.Context, state *cluster.State) error {
	f.rec.add("infra.LaunchDeletionSidecar %s", state.Spec.Name)
	return f.err("LaunchDeletionSidecar")
}

// --- control plane

type fakeCP struct {
	rec      *recorder
	findings []provider.Finding
	// ca, when set, is the cluster CA Prepare returns instead of the default
	// placeholder, so a test needing a mintable CA (hook kubeconfigs) can
	// inject a real one.
	ca *cluster.KeyPair
}

// Check implements provider.Checker with canned findings.
func (f *fakeCP) Check(bundle []*unstructured.Unstructured) []provider.Finding {
	f.rec.add("cp.Check objects=%d", len(bundle))
	return f.findings
}

func (f *fakeCP) Name() string { return "kubeadm" }
func (f *fakeCP) Kind() string { return "KubeadmControlPlane" }

// fakeKubeconfig is a minimal but valid kubeconfig, so credential.WithServer
// can parse and rewrite it when a ClientEndpoint is set.
const fakeKubeconfig = `apiVersion: v1
kind: Config
clusters:
- name: demo
  cluster:
    server: https://10.0.0.10:6443
contexts:
- name: demo
  context:
    cluster: demo
current-context: demo
`

func (f *fakeCP) Prepare(_ context.Context, _ *cluster.Spec, ep cluster.Endpoint) (*controlplane.Credentials, error) {
	f.rec.add("cp.Prepare %s:%d", ep.Host, ep.Port)
	ca := cluster.KeyPair{Cert: []byte("CA-CERT"), Key: []byte("CA-KEY")}
	if f.ca != nil {
		ca = *f.ca
	}
	return &controlplane.Credentials{
		CA:         ca,
		Kubeconfig: []byte(fakeKubeconfig),
	}, nil
}

func (f *fakeCP) UserData(_ context.Context, _ *cluster.Spec, _ *controlplane.Credentials, cp, _ *unstructured.Unstructured) (*controlplane.Contribution, error) {
	f.rec.add("cp.UserData %s", cp.GetName())
	return &controlplane.Contribution{
		Files:           []controlplane.File{{Path: "/run/fake/config", Content: []byte("x")}},
		InstallCommands: []string{"echo install"},
	}, nil
}

func (f *fakeCP) Adopt(_ context.Context, st *cluster.State, cp *unstructured.Unstructured) ([]byte, error) {
	f.rec.add("cp.Adopt %s", cp.GetName())
	return []byte("apiVersion: cluster.x-k8s.io/v1beta2\nkind: Machine\nmetadata:\n  name: " + st.Spec.Name + "-bootstrap\n  namespace: default\nspec:\n  clusterName: " + st.Spec.Name + "\n"), nil
}

// --- cluster bundle

const bundleTemplate = `apiVersion: cluster.x-k8s.io/v1beta2
kind: Cluster
metadata:
  name: %[1]s
  namespace: default
spec:
  controlPlaneRef:
    apiGroup: controlplane.cluster.x-k8s.io
    kind: KubeadmControlPlane
    name: %[1]s-control-plane
---
apiVersion: infrastructure.cluster.x-k8s.io/v1alpha1
kind: FakeCluster
metadata:
  name: %[1]s
  namespace: default
spec: {}
---
apiVersion: controlplane.cluster.x-k8s.io/v1beta2
kind: KubeadmControlPlane
metadata:
  name: %[1]s-control-plane
  namespace: default
spec:
  replicas: 1
  version: v1.33.4
---
apiVersion: infrastructure.cluster.x-k8s.io/v1alpha1
kind: FakeMachineTemplate
metadata:
  name: %[1]s-control-plane
  namespace: default
spec:
  template:
    spec:
      tags: [%[2]s]
`

func bundleYAML(name string, tags ...string) []byte {
	return []byte(fmt.Sprintf(bundleTemplate, name, strings.Join(tags, ", ")))
}

type fakeGenerator struct {
	rec  *recorder
	req  *generate.Request
	fail error
}

func (g *fakeGenerator) generate(_ context.Context, req generate.Request) (*generate.Bundle, error) {
	g.rec.add("generate")
	g.req = &req
	if g.fail != nil {
		return nil, g.fail
	}
	return generate.ParseBundle(bundleYAML(req.Spec.Name))
}

// --- Kubernetes API

type objKey struct {
	gvk       schema.GroupVersionKind
	namespace string
	name      string
}

type fakeCluster struct {
	rec        *recorder
	mu         sync.Mutex
	objs       map[objKey]*unstructured.Unstructured
	readyAfter int // Ready fails this many times first
	readyCalls int
	nodeRef    bool // the Machine reports a node once owned and unpaused
	patches    []string
	applied    []string
}

func newFakeCluster(rec *recorder, clusterName string) *fakeCluster {
	f := &fakeCluster{rec: rec, objs: map[objKey]*unstructured.Unstructured{}, nodeRef: true}
	for _, s := range []string{clusterName + "-ca", clusterName + "-kubeconfig"} {
		u := &unstructured.Unstructured{Object: map[string]any{}}
		u.SetAPIVersion("v1")
		u.SetKind("Secret")
		u.SetNamespace("default")
		u.SetName(s)
		f.objs[keyOf(u)] = u
	}
	return f
}

func keyOf(u *unstructured.Unstructured) objKey {
	ns := u.GetNamespace()
	if ns == "" && u.GetKind() != "Namespace" {
		ns = "default"
	}
	return objKey{u.GroupVersionKind(), ns, u.GetName()}
}

func (f *fakeCluster) Ready(context.Context) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.readyCalls++
	if f.readyCalls <= f.readyAfter {
		return errors.New("connection refused")
	}
	f.rec.add("kube.Ready")
	return nil
}

func (f *fakeCluster) Apply(_ context.Context, manager string, obj *unstructured.Unstructured) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	o := obj.DeepCopy()
	desc := fmt.Sprintf("kube.Apply %s %s %s", manager, o.GetKind(), o.GetName())
	if manager == PauseFieldManager {
		p, _, _ := unstructured.NestedBool(o.Object, "spec", "paused")
		desc += fmt.Sprintf(" paused=%v", p)
	}
	f.rec.add("%s", desc)
	f.applied = append(f.applied, o.GetKind()+"/"+o.GetName())
	k := keyOf(o)
	if old, ok := f.objs[k]; ok {
		// Keep the UID and merge spec.paused the way field ownership would.
		o.SetUID(old.GetUID())
		if p, ok, _ := unstructured.NestedBool(old.Object, "spec", "paused"); ok && manager != PauseFieldManager {
			_ = unstructured.SetNestedField(o.Object, p, "spec", "paused")
		}
		if manager == PauseFieldManager {
			p, _, _ := unstructured.NestedBool(o.Object, "spec", "paused")
			merged := old.DeepCopy()
			_ = unstructured.SetNestedField(merged.Object, p, "spec", "paused")
			o = merged
		}
	} else {
		o.SetUID(types.UID(fmt.Sprintf("uid-%d", len(f.objs)+1)))
	}
	if o.GroupVersionKind().Group == generate.OperatorGroup {
		_ = unstructured.SetNestedSlice(o.Object, []any{map[string]any{"type": "Ready", "status": "True"}}, "status", "conditions")
	}
	f.objs[k] = o
	return nil
}

func (f *fakeCluster) Get(_ context.Context, gvk schema.GroupVersionKind, namespace, name string) (*unstructured.Unstructured, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if namespace == "" {
		namespace = "default"
	}
	if gvk.Kind == "Namespace" {
		namespace = ""
	}
	o, ok := f.objs[objKey{gvk, namespace, name}]
	if !ok {
		return nil, fmt.Errorf("%s %s/%s not found", gvk.Kind, namespace, name)
	}
	o = o.DeepCopy()
	if gvk.Kind == "Machine" {
		f.rec.add("kube.Get Machine %s", name)
		if f.nodeRef && len(o.GetOwnerReferences()) > 0 && !f.clusterPaused() {
			_ = unstructured.SetNestedMap(o.Object, map[string]any{"name": "node-1"}, "status", "nodeRef")
		}
	}
	return o, nil
}

func (f *fakeCluster) clusterPaused() bool {
	for k, o := range f.objs {
		if k.gvk.Kind == "Cluster" {
			p, _, _ := unstructured.NestedBool(o.Object, "spec", "paused")
			return p
		}
	}
	return false
}

func (f *fakeCluster) Patch(_ context.Context, gvk schema.GroupVersionKind, namespace, name string, pt types.PatchType, data []byte) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.rec.add("kube.Patch %s %s", gvk.Kind, name)
	f.patches = append(f.patches, string(data))
	if pt != types.MergePatchType {
		return fmt.Errorf("unexpected patch type %s", pt)
	}
	o, ok := f.objs[objKey{gvk, namespace, name}]
	if !ok {
		return fmt.Errorf("%s %s not found", gvk.Kind, name)
	}
	var p map[string]any
	if err := json.Unmarshal(data, &p); err != nil {
		return err
	}
	mergePatch(o.Object, p)
	return nil
}

func (f *fakeCluster) Delete(_ context.Context, gvk schema.GroupVersionKind, namespace, name string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	if namespace == "" && gvk.Kind != "Namespace" {
		namespace = "default"
	}
	f.rec.add("kube.Delete %s %s", gvk.Kind, name)
	delete(f.objs, objKey{gvk, namespace, name})
	return nil
}

func (f *fakeCluster) List(_ context.Context, gvk schema.GroupVersionKind, namespace string) ([]*unstructured.Unstructured, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	var out []*unstructured.Unstructured
	for k, o := range f.objs {
		if k.gvk == gvk && (namespace == "" || k.namespace == namespace) {
			out = append(out, o.DeepCopy())
		}
	}
	return out, nil
}

func mergePatch(dst, patch map[string]any) {
	for k, v := range patch {
		if pm, ok := v.(map[string]any); ok {
			dm, ok := dst[k].(map[string]any)
			if !ok {
				dm = map[string]any{}
				dst[k] = dm
			}
			mergePatch(dm, pm)
			continue
		}
		if v == nil {
			delete(dst, k)
			continue
		}
		dst[k] = v
	}
}

func toList(objs ...*unstructured.Unstructured) []*unstructured.Unstructured { return objs }
