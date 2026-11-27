package klotho

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"reflect"
	"slices"
	"strings"
	"testing"
	"time"

	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/client-go/tools/clientcmd"

	"github.com/klotho-k8s/klotho/pkg/cluster"
	"github.com/klotho-k8s/klotho/pkg/generate"
	"github.com/klotho-k8s/klotho/pkg/kube"
	"github.com/klotho-k8s/klotho/pkg/state"
	"github.com/klotho-k8s/klotho/provider"
	"github.com/klotho-k8s/klotho/provider/backend"
	"github.com/klotho-k8s/klotho/provider/infrastructure"
)

type harness struct {
	rec   *recorder
	store *fakeStore
	infra *fakeInfra
	cp    *fakeCP
	gen   *fakeGenerator
	kube  *fakeCluster
	deps  Deps
}

func newHarness(t *testing.T) *harness {
	t.Helper()
	rec := &recorder{}
	h := &harness{
		rec:   rec,
		store: newFakeStore(rec),
		infra: &fakeInfra{rec: rec, fail: map[string]error{}},
		cp:    &fakeCP{rec: rec},
		gen:   &fakeGenerator{rec: rec},
		kube:  newFakeCluster(rec, "demo"),
	}
	h.deps = Deps{
		Store:        h.store,
		Infra:        h.infra,
		ControlPlane: h.cp,
		Codec:        state.PlainCodec{},
		Generate:     h.gen.generate,
		Connect: func(_ context.Context, kubeconfig []byte) (kube.Cluster, error) {
			rec.add("kube.Connect %s", kubeconfig)
			return h.kube, nil
		},
		ConnectAdmin: func(_ context.Context, st *cluster.State) (kube.Cluster, error) {
			rec.add("kube.ConnectAdmin %s", st.Spec.Name)
			return h.kube, nil
		},
		PollInterval: time.Millisecond,
	}
	return h
}

func request() CreateRequest {
	return CreateRequest{
		Spec: cluster.Spec{
			Name:      "demo",
			Variables: map[string]string{"CONTROL_PLANE_ENDPOINT_IP": "10.0.0.10", "SHARED": "user"},
		},
		Provider:   generate.ProviderRef{Name: "fake", Version: "v0.1.0"},
		Kubernetes: "v1.33.4",
		CAPI: generate.CAPIOptions{
			Version:              "v1.12.9",
			FeatureGates:         map[string]bool{"SelfHostedDeletion": false},
			InfraProviderName:    "fake",
			InfraProviderVersion: "v0.1.0",
			ProviderSecretData:   map[string]string{"FAKE_SECRET": "s3cret"},
		},
		DefaultVariables: map[string]string{"SHARED": "default", "ONLY_DEFAULT": "d"},
		Timeouts:         Timeouts{APIServer: time.Second, Providers: time.Second, Apply: time.Second, Node: time.Second},
	}
}

// assertOrder checks that each prefix occurs in calls, in this order.
func assertOrder(t *testing.T, calls []string, prefixes ...string) {
	t.Helper()
	i := 0
	for _, c := range calls {
		if i < len(prefixes) && strings.HasPrefix(c, prefixes[i]) {
			i++
		}
	}
	if i < len(prefixes) {
		t.Fatalf("call %q not found in order; calls:\n  %s", prefixes[i], strings.Join(calls, "\n  "))
	}
}

func TestCreateHappyPath(t *testing.T) {
	h := newHarness(t)
	h.kube.readyAfter = 3
	st, err := Create(t.Context(), h.deps, request())
	if err != nil {
		t.Fatal(err)
	}
	calls := h.rec.list()
	assertOrder(t, calls,
		"store.Reserve demo",
		"infra.Prepare",
		"generate",
		"cp.Prepare 10.0.0.10:6443",
		"cp.UserData demo-control-plane",
		"infra.Launch",
		"store.Save demo",
		"kube.Connect ",
		"kube.Ready",
		"kube.Apply klotho Namespace capi-system",
		"kube.Apply klotho Secret fake-variables",
		"kube.Apply klotho CoreProvider cluster-api",
		"kube.Apply klotho BootstrapProvider kubeadm",
		"kube.Apply klotho ControlPlaneProvider kubeadm",
		"kube.Apply klotho InfrastructureProvider fake",
		"kube.Apply klotho-pause Cluster demo paused=true",
		"kube.Apply klotho Cluster demo",
		"kube.Apply klotho KubeadmControlPlane demo-control-plane",
		"cp.Adopt demo-control-plane",
		"infra.Adopt bootstrap=true",
		"kube.Apply klotho Machine demo-bootstrap",
		"kube.Apply klotho FakeMachine demo-bootstrap",
		"kube.Patch Machine demo-bootstrap",
		"kube.Apply klotho-pause Cluster demo paused=false",
		"kube.Get Machine demo-bootstrap",
	)
	if h.rec.has("store.Delete") {
		t.Fatal("reservation released on success")
	}
	// Exactly one Save: the full state right after Launch.
	saves := 0
	for _, c := range calls {
		if strings.HasPrefix(c, "store.Save") {
			saves++
		}
	}
	if saves != 1 {
		t.Fatalf("%d saves", saves)
	}
	saved := h.store.state("demo")
	if saved.Reserved || saved.Bootstrap == nil || saved.Bootstrap.ID != "fake://1" || string(saved.CA.Key) != "CA-KEY" {
		t.Fatalf("saved state: %+v", saved)
	}
	if saved.Spec.Provider != "fake" || saved.Spec.ControlPlane != "kubeadm" {
		t.Fatalf("spec: %+v", saved.Spec)
	}
	if st.Endpoint != (cluster.Endpoint{Host: "10.0.0.10", Port: 6443}) {
		t.Fatalf("endpoint: %+v", st.Endpoint)
	}

	// The owner reference carries the live object's UID.
	if len(h.kube.patches) != 1 || !strings.Contains(h.kube.patches[0], `"uid":"uid-`) ||
		!strings.Contains(h.kube.patches[0], `"controller":true`) || !strings.Contains(h.kube.patches[0], `"kind":"KubeadmControlPlane"`) {
		t.Fatalf("patches: %v", h.kube.patches)
	}

	// Variable precedence: user > prepared > defaults, and Prepare sees
	// defaults below the user's values.
	req := h.gen.req
	if len(req.Variables) != 3 {
		t.Fatalf("variable sources: %d", len(req.Variables))
	}
	for name, want := range map[string]string{"SHARED": "user", "ONLY_DEFAULT": "d"} {
		var got string
		for _, s := range req.Variables {
			if v, ok := s.Get(name); ok {
				got = v
				break
			}
		}
		if got != want {
			t.Errorf("%s = %q, want %q", name, got, want)
		}
	}
	if h.infra.prepSpec.Variables["SHARED"] != "user" || h.infra.prepSpec.Variables["ONLY_DEFAULT"] != "d" {
		t.Errorf("prepare saw %v", h.infra.prepSpec.Variables)
	}
}

func TestCreatePreparedVariablesBeatDefaults(t *testing.T) {
	h := newHarness(t)
	h.infra.vars = map[string]string{"ONLY_DEFAULT": "provider", "SHARED": "provider"}
	if _, err := Create(t.Context(), h.deps, request()); err != nil {
		t.Fatal(err)
	}
	get := func(name string) string {
		for _, s := range h.gen.req.Variables {
			if v, ok := s.Get(name); ok {
				return v
			}
		}
		return ""
	}
	if get("ONLY_DEFAULT") != "provider" || get("SHARED") != "user" {
		t.Fatalf("ONLY_DEFAULT=%s SHARED=%s", get("ONLY_DEFAULT"), get("SHARED"))
	}
}

func TestCreateExistingName(t *testing.T) {
	h := newHarness(t)
	if _, err := h.store.Reserve(t.Context(), "demo"); err != nil {
		t.Fatal(err)
	}
	_, err := Create(t.Context(), h.deps, request())
	if err == nil || !strings.Contains(err.Error(), "already exists") {
		t.Fatalf("got %v", err)
	}
	if h.rec.has("store.Delete") || h.rec.has("infra.Prepare") {
		t.Fatal("an existing reservation must not be touched")
	}
}

func TestCreateInvalidName(t *testing.T) {
	h := newHarness(t)
	req := request()
	req.Spec.Name = "Bad_Name"
	if _, err := Create(t.Context(), h.deps, req); err == nil {
		t.Fatal("want error")
	}
	if h.rec.has("store.Reserve") {
		t.Fatal("reserved an invalid name")
	}
}

func TestCreatePrepareFailureReleases(t *testing.T) {
	h := newHarness(t)
	h.infra.fail["Prepare"] = errors.New("no template")
	_, err := Create(t.Context(), h.deps, request())
	if err == nil || !strings.Contains(err.Error(), "no template") {
		t.Fatalf("got %v", err)
	}
	if !h.rec.has("store.Delete demo") || h.store.state("demo") != nil {
		t.Fatal("reservation not released")
	}
}

func TestCreateFailureAfterPrepareNothingCreatedReleases(t *testing.T) {
	h := newHarness(t)
	h.gen.fail = errors.New("template fetch failed")
	if _, err := Create(t.Context(), h.deps, request()); err == nil {
		t.Fatal("want error")
	}
	if h.store.state("demo") != nil {
		t.Fatal("reservation not released")
	}
}

func TestCreateFailureAfterPrepareWithCreatedKeepsSnapshot(t *testing.T) {
	h := newHarness(t)
	h.infra.created = []cluster.Resource{{Kind: "lb", ID: "lb-1"}}
	h.gen.fail = errors.New("template fetch failed")
	if _, err := Create(t.Context(), h.deps, request()); err == nil {
		t.Fatal("want error")
	}
	if h.rec.has("store.Delete") {
		t.Fatal("reservation released although resources were created")
	}
	st := h.store.state("demo")
	if st == nil || st.Reserved || len(st.Resources) != 1 || st.Resources[0].ID != "lb-1" {
		t.Fatalf("snapshot: %+v", st)
	}
	if st.Endpoint.Host != "10.0.0.10" || st.Spec.Provider != "fake" {
		t.Fatalf("snapshot spec/endpoint: %+v", st)
	}
}

func TestCreateLaunchFailureReleases(t *testing.T) {
	h := newHarness(t)
	h.infra.fail["Launch"] = errors.New("clone failed")
	if _, err := Create(t.Context(), h.deps, request()); err == nil {
		t.Fatal("want error")
	}
	if h.store.state("demo") != nil {
		t.Fatal("reservation not released after a launch that cleaned up after itself")
	}
}

func TestCreateFailureAfterLaunchKeepsState(t *testing.T) {
	h := newHarness(t)
	h.kube.readyAfter = 1 << 30 // never ready
	req := request()
	req.Timeouts.APIServer = 20 * time.Millisecond
	st, err := Create(t.Context(), h.deps, req)
	if err == nil || !strings.Contains(err.Error(), "API server") || !strings.Contains(err.Error(), "klotho delete demo") {
		t.Fatalf("got %v", err)
	}
	if st == nil || st.Bootstrap == nil {
		t.Fatal("state not returned")
	}
	if h.rec.has("store.Delete") {
		t.Fatal("state removed after launch")
	}
	saved := h.store.state("demo")
	if saved == nil || saved.Reserved || saved.Bootstrap == nil {
		t.Fatalf("saved: %+v", saved)
	}
}

func TestCreateNodeTimeout(t *testing.T) {
	h := newHarness(t)
	h.kube.nodeRef = false
	req := request()
	req.Timeouts.Node = 20 * time.Millisecond
	_, err := Create(t.Context(), h.deps, req)
	if !errors.Is(err, ErrNodeTimeout) || !strings.Contains(err.Error(), "may still converge") {
		t.Fatalf("got %v", err)
	}
	if saved := h.store.state("demo"); saved == nil || saved.Reserved {
		t.Fatal("state must stay")
	}
}

func TestCreateCancelAfterReserveReleases(t *testing.T) {
	h := newHarness(t)
	ctx, cancel := context.WithCancel(t.Context())
	h.deps.Generate = func(context.Context, generate.Request) (*generate.Bundle, error) {
		cancel()
		return nil, context.Canceled
	}
	if _, err := Create(ctx, h.deps, request()); !errors.Is(err, context.Canceled) {
		t.Fatalf("got %v", err)
	}
	if h.store.state("demo") != nil {
		t.Fatal("reservation not released on a cancelled context")
	}
}

func TestCreateDryRun(t *testing.T) {
	h := newHarness(t)
	dir := filepath.Join(t.TempDir(), "out")
	req := request()
	req.DryRunDir = dir
	st, err := Create(t.Context(), h.deps, req)
	if err != nil {
		t.Fatal(err)
	}
	if st.Reserved || st.Bootstrap != nil {
		t.Fatalf("state: %+v", st)
	}
	if h.rec.has("infra.Launch") || h.rec.has("kube.") || h.rec.has("store.Save") {
		t.Fatalf("dry run did too much: %v", h.rec.list())
	}
	if h.store.state("demo") != nil {
		t.Fatal("dry run kept the reservation")
	}
	for _, f := range []string{"user-data", "meta-data", DryRunBundle, DryRunProviders, DryRunState, DryRunAdoptControl} {
		b, err := os.ReadFile(filepath.Join(dir, f))
		if err != nil || len(b) == 0 {
			t.Fatalf("%s: %v", f, err)
		}
	}
	providers, _ := os.ReadFile(filepath.Join(dir, DryRunProviders))
	if strings.Contains(string(providers), "s3cret") || !strings.Contains(string(providers), "FAKE_SECRET") {
		t.Fatalf("providers.yaml must name but redact the secret:\n%s", providers)
	}
	for _, want := range []string{"kind: CoreProvider", "SelfHostedDeletion: false", "configSecret:", "kind: InfrastructureProvider"} {
		if !strings.Contains(string(providers), want) {
			t.Errorf("providers.yaml lacks %q", want)
		}
	}
	plain, _ := os.ReadFile(filepath.Join(dir, DryRunState))
	if _, err := state.Unmarshal(plain); err != nil {
		t.Fatalf("state-plain.json: %v", err)
	}
	userData, _ := os.ReadFile(filepath.Join(dir, "user-data"))
	if !strings.Contains(string(userData), "hostname: demo-bootstrap") {
		t.Fatalf("user-data hostname:\n%s", userData)
	}
	bundle, _ := os.ReadFile(filepath.Join(dir, DryRunBundle))
	if !strings.Contains(string(bundle), "klotho-demo") || !strings.Contains(string(bundle), "10.0.0.10") {
		t.Fatalf("bundle not patched:\n%s", bundle)
	}
}

func TestCreateUserManifestUntouched(t *testing.T) {
	h := newHarness(t)
	req := request()
	req.Manifest = bundleYAML("demo", "klotho-demo", "mine")
	req.Pool = "ignored-for-manifests"
	req.InstallKubernetes = true
	req.MachineTemplateTags = []string{"ignored"}
	want, err := generate.ParseBundle(req.Manifest)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := Create(t.Context(), h.deps, req); err != nil {
		t.Fatal(err)
	}
	if h.rec.has("generate") {
		t.Fatal("generated although a manifest was given")
	}
	// Every applied bundle object is byte-identical to the input.
	for _, o := range want.Objects {
		got, err := h.kube.Get(t.Context(), o.GroupVersionKind(), o.GetNamespace(), o.GetName())
		if err != nil {
			t.Fatal(err)
		}
		g := got.DeepCopy()
		g.SetUID("")
		if o.GetKind() == "Cluster" {
			// spec.paused is owned by the pause field manager, not the bundle.
			delete(g.Object["spec"].(map[string]any), "paused")
		}
		gy, _ := generate.RenderYAML(toList(g))
		wy, _ := generate.RenderYAML(toList(o))
		if string(gy) != string(wy) {
			t.Fatalf("%s changed:\n%s\nvs\n%s", o.GetKind(), gy, wy)
		}
	}
}

func TestCreateUserManifestInvalid(t *testing.T) {
	h := newHarness(t)
	req := request()
	req.Manifest = bundleYAML("demo", "not-the-tag")
	_, err := Create(t.Context(), h.deps, req)
	var verr *generate.ValidationError
	if !errors.As(err, &verr) {
		t.Fatalf("got %v", err)
	}
	if h.store.state("demo") != nil || h.rec.has("cp.Prepare") {
		t.Fatal("invalid manifest went too far")
	}
}

func TestDeleteReservation(t *testing.T) {
	h := newHarness(t)
	if _, err := h.store.Reserve(t.Context(), "demo"); err != nil {
		t.Fatal(err)
	}
	if err := Delete(t.Context(), h.deps, "demo", DeleteOptions{Confirm: func(*infrastructure.Plan) bool {
		t.Fatal("no confirmation for a reservation")
		return false
	}}); err != nil {
		t.Fatal(err)
	}
	if h.rec.has("infra.") || h.store.state("demo") != nil {
		t.Fatalf("calls: %v", h.rec.list())
	}
}

func TestDeleteDeclined(t *testing.T) {
	h := newHarness(t)
	if _, err := Create(t.Context(), h.deps, request()); err != nil {
		t.Fatal(err)
	}
	var shown *infrastructure.Plan
	err := Delete(t.Context(), h.deps, "demo", DeleteOptions{Confirm: func(p *infrastructure.Plan) bool {
		shown = p
		return false
	}})
	if !errors.Is(err, ErrDeclined) {
		t.Fatalf("got %v", err)
	}
	if shown == nil || len(shown.Resources) != 1 {
		t.Fatalf("plan: %+v", shown)
	}
	if h.rec.has("infra.Teardown") || h.rec.has("store.Delete") || h.store.state("demo") == nil {
		t.Fatal("declined delete changed something")
	}
}

func TestDeleteWatch(t *testing.T) {
	h := newHarness(t)
	if _, err := Create(t.Context(), h.deps, request()); err != nil {
		t.Fatal(err)
	}
	// The confirm Plan is reported first, then the watch loop drains to empty.
	h.infra.planSeq = []int{1, 1, 0}
	if err := Delete(t.Context(), h.deps, "demo", DeleteOptions{Graceful: true}); err != nil {
		t.Fatal(err)
	}
	assertOrder(t, h.rec.list(),
		"kube.ConnectAdmin demo",
		"kube.Delete Cluster demo",
		"infra.Teardown",
		"store.Delete demo",
	)
	if h.rec.has("infra.LaunchDeletionSidecar") {
		t.Fatalf("watched delete launched a sidecar; calls: %v", h.rec.list())
	}
	if h.store.state("demo") != nil {
		t.Fatal("record not removed after watched delete")
	}
	if err := Delete(t.Context(), h.deps, "demo", DeleteOptions{}); !errors.Is(err, backend.ErrNotFound) {
		t.Fatalf("second delete: %v", err)
	}
}

func TestDeleteFireAndForget(t *testing.T) {
	h := newHarness(t)
	if _, err := Create(t.Context(), h.deps, request()); err != nil {
		t.Fatal(err)
	}
	if err := Delete(t.Context(), h.deps, "demo", DeleteOptions{}); err != nil {
		t.Fatal(err)
	}
	assertOrder(t, h.rec.list(),
		"kube.ConnectAdmin demo",
		"kube.Delete Cluster demo",
		"infra.LaunchDeletionSidecar demo",
		"store.Delete demo",
	)
	// Fire-and-forget hands the infrastructure teardown to the sidecar (no
	// in-process sweep), but clears the state record immediately so the cluster
	// does not linger in `klotho list` after it reports deleted.
	if h.rec.has("infra.Teardown") {
		t.Fatalf("fire-and-forget swept the infrastructure; calls: %v", h.rec.list())
	}
	if h.store.state("demo") != nil {
		t.Fatal("record not removed by a fire-and-forget delete")
	}
	if err := Delete(t.Context(), h.deps, "demo", DeleteOptions{}); !errors.Is(err, backend.ErrNotFound) {
		t.Fatalf("second delete: %v", err)
	}
}

func TestDeleteForce(t *testing.T) {
	h := newHarness(t)
	if _, err := Create(t.Context(), h.deps, request()); err != nil {
		t.Fatal(err)
	}
	if err := Delete(t.Context(), h.deps, "demo", DeleteOptions{Force: true}); err != nil {
		t.Fatal(err)
	}
	assertOrder(t, h.rec.list(), "infra.Plan", "infra.Teardown", "store.Delete demo")
	if h.rec.has("kube.ConnectAdmin") || h.rec.has("infra.LaunchDeletionSidecar") || h.rec.has("kube.Delete Cluster") {
		t.Fatalf("force went through Cluster API; calls: %v", h.rec.list())
	}
	if h.store.state("demo") != nil {
		t.Fatal("record not removed after force delete")
	}
}

func TestDeleteUnreachableFallsBack(t *testing.T) {
	h := newHarness(t)
	if _, err := Create(t.Context(), h.deps, request()); err != nil {
		t.Fatal(err)
	}
	h.deps.ConnectAdmin = func(context.Context, *cluster.State) (kube.Cluster, error) {
		return nil, errClusterUnreachable
	}
	if err := Delete(t.Context(), h.deps, "demo", DeleteOptions{Graceful: true}); err != nil {
		t.Fatal(err)
	}
	assertOrder(t, h.rec.list(), "infra.Teardown", "store.Delete demo")
	if h.rec.has("infra.LaunchDeletionSidecar") || h.rec.has("kube.Delete Cluster") {
		t.Fatalf("unreachable cluster still used Cluster API; calls: %v", h.rec.list())
	}
}

func TestConnectAdminBuildsKubeconfig(t *testing.T) {
	ca := testCA(t)
	var got []byte
	deps := Deps{
		Store: newFakeStore(&recorder{}),
		Codec: state.PlainCodec{},
		Connect: func(_ context.Context, kubeconfig []byte) (kube.Cluster, error) {
			got = kubeconfig
			return nil, errors.New("captured; not dialing")
		},
	}
	st := &cluster.State{
		Endpoint: cluster.Endpoint{Host: "10.0.0.10", Port: 6443},
		CA:       ca,
	}
	st.Spec.Name = "demo"
	if _, err := deps.connectAdmin(t.Context(), st); !errors.Is(err, errClusterUnreachable) {
		t.Fatalf("want errClusterUnreachable, got %v", err)
	}
	cfg, err := clientcmd.Load(got)
	if err != nil {
		t.Fatalf("connectAdmin wrote an unparseable kubeconfig: %v", err)
	}
	if cfg.CurrentContext != "demo" {
		t.Fatalf("current context %q", cfg.CurrentContext)
	}
	u := cfg.AuthInfos["klotho-admin@demo"]
	if u == nil || len(u.ClientCertificateData) == 0 || len(u.ClientKeyData) == 0 {
		t.Fatalf("admin auth info missing client cert/key: %+v", u)
	}
	if c := cfg.Clusters["demo"]; c == nil || c.Server != "https://10.0.0.10:6443" {
		t.Fatalf("cluster server %+v", c)
	}
}

func TestConnectAdminDialsClientEndpoint(t *testing.T) {
	ca := testCA(t)
	var got []byte
	deps := Deps{
		Store: newFakeStore(&recorder{}),
		Codec: state.PlainCodec{},
		Connect: func(_ context.Context, kubeconfig []byte) (kube.Cluster, error) {
			got = kubeconfig
			return nil, errors.New("captured; not dialing")
		},
	}
	st := &cluster.State{
		// The cluster endpoint is not host-routable; the client endpoint is.
		Endpoint:       cluster.Endpoint{Host: "10.89.1.15", Port: 6443},
		ClientEndpoint: cluster.Endpoint{Host: "127.0.0.1", Port: 35623},
		CA:             ca,
	}
	st.Spec.Name = "demo"
	if _, err := deps.connectAdmin(t.Context(), st); !errors.Is(err, errClusterUnreachable) {
		t.Fatalf("want errClusterUnreachable, got %v", err)
	}
	cfg, err := clientcmd.Load(got)
	if err != nil {
		t.Fatalf("unparseable kubeconfig: %v", err)
	}
	if c := cfg.Clusters["demo"]; c == nil || c.Server != "https://127.0.0.1:35623" {
		t.Fatalf("connectAdmin must dial the client endpoint, got %+v", c)
	}
}

func TestCreatePersistsClientEndpoint(t *testing.T) {
	h := newHarness(t)
	h.infra.clientEndpoint = cluster.Endpoint{Host: "127.0.0.1", Port: 35623}
	if _, err := Create(t.Context(), h.deps, request()); err != nil {
		t.Fatal(err)
	}
	st := h.store.state("demo")
	if st == nil {
		t.Fatal("no state persisted")
	}
	if st.Endpoint.Host != "10.0.0.10" {
		t.Fatalf("cluster endpoint = %+v, want the controlPlaneEndpoint", st.Endpoint)
	}
	if st.ClientEndpoint.Host != "127.0.0.1" || st.ClientEndpoint.Port != 35623 {
		t.Fatalf("client endpoint = %+v, want the host-reachable endpoint", st.ClientEndpoint)
	}
}

func TestListAndLoad(t *testing.T) {
	h := newHarness(t)
	if _, err := Create(t.Context(), h.deps, request()); err != nil {
		t.Fatal(err)
	}
	if _, err := h.store.Reserve(t.Context(), "other"); err != nil {
		t.Fatal(err)
	}
	h.store.records["broken"] = []byte("garbage")
	sums, err := List(t.Context(), h.deps)
	if err != nil {
		t.Fatal(err)
	}
	byName := map[string]Summary{}
	for _, s := range sums {
		byName[s.Name] = s
	}
	if s := byName["demo"]; s.Provider != "fake" || s.Endpoint.Host != "10.0.0.10" || s.Reserved {
		t.Errorf("demo: %+v", s)
	}
	if !byName["other"].Reserved {
		t.Errorf("other: %+v", byName["other"])
	}
	if byName["broken"].Error == "" {
		t.Errorf("broken: %+v", byName["broken"])
	}
	if _, err := Load(t.Context(), h.deps, "missing"); !errors.Is(err, backend.ErrNotFound) {
		t.Fatalf("missing: %v", err)
	}
}

func TestValidateName(t *testing.T) {
	for _, ok := range []string{"demo", "a", "lab-1"} {
		if err := ValidateName(ok); err != nil {
			t.Errorf("%s: %v", ok, err)
		}
	}
	for _, bad := range []string{"", "Demo", "a_b", "a.b", "-a", strings.Repeat("a", 64)} {
		if err := ValidateName(bad); err == nil {
			t.Errorf("%q accepted", bad)
		}
	}
}

func TestCreateFindingsBlockWithoutForce(t *testing.T) {
	h := newHarness(t)
	h.cp.findings = []provider.Finding{{Object: "KubeadmControlPlane/demo-control-plane", Field: "spec.rollout.before.certificatesExpiryDays", Message: "not set"}}
	_, err := Create(t.Context(), h.deps, request())
	var fe *FindingsError
	if !errors.As(err, &fe) || len(fe.Findings) != 1 {
		t.Fatalf("err = %v, want FindingsError with one finding", err)
	}
	if !strings.Contains(err.Error(), "--force") || !strings.Contains(err.Error(), "certificatesExpiryDays") {
		t.Fatalf("message: %v", err)
	}
	if h.rec.has("cp.Prepare") || h.rec.has("infra.Launch") {
		t.Fatalf("creation continued past the findings: %v", h.rec.list())
	}
	// Nothing was created, so the reservation is released.
	if !h.rec.has("store.Delete demo") {
		t.Fatalf("reservation kept: %v", h.rec.list())
	}
}

func TestCreateFindingsForce(t *testing.T) {
	h := newHarness(t)
	h.kube.readyAfter = 1
	h.cp.findings = []provider.Finding{{Object: "KubeadmControlPlane/demo-control-plane", Field: "f", Message: "m"}}
	req := request()
	req.Force = true
	if _, err := Create(t.Context(), h.deps, req); err != nil {
		t.Fatal(err)
	}
	assertOrder(t, h.rec.list(), "cp.Check objects=", "cp.Prepare 10.0.0.10:6443", "infra.Launch")
}

func TestCreateFindingsDryRunProceeds(t *testing.T) {
	h := newHarness(t)
	h.cp.findings = []provider.Finding{{Object: "o", Field: "f", Message: "m"}}
	req := request()
	req.DryRunDir = t.TempDir()
	if _, err := Create(t.Context(), h.deps, req); err != nil {
		t.Fatal(err)
	}
	if h.rec.has("infra.Launch") {
		t.Fatal("dry run launched")
	}
}

func TestCreateAppliesExplicitPatchesToGeneratedBundle(t *testing.T) {
	h := newHarness(t)
	req := request()
	req.MachineTemplateTags = []string{"k8s", "ubuntu"}
	req.MachineTemplateMatchPolicy = generate.TemplateMatchBestSubset
	req.InstallKubernetes = true
	req.InstallOptions = generate.InstallKubernetesOptions{AptProxy: "http://cache.lan:3142"}
	if _, err := Create(t.Context(), h.deps, req); err != nil {
		t.Fatal(err)
	}
	mt, err := h.kube.Get(t.Context(), schema.GroupVersionKind{Group: generate.InfrastructureGroup, Version: "v1alpha1", Kind: "FakeMachineTemplate"}, "default", "demo-control-plane")
	if err != nil {
		t.Fatal(err)
	}
	tags, _, _ := unstructured.NestedStringSlice(mt.Object, "spec", "template", "spec", "templateSelector", "matchTags")
	policy, _, _ := unstructured.NestedString(mt.Object, "spec", "template", "spec", "templateMatchPolicy")
	if !reflect.DeepEqual(tags, []string{"k8s", "ubuntu"}) || policy != generate.TemplateMatchBestSubset {
		t.Errorf("machine template selector: tags=%v policy=%q", tags, policy)
	}
	kcp, err := h.kube.Get(t.Context(), schema.GroupVersionKind{Group: ControlPlaneGroup, Version: "v1beta2", Kind: "KubeadmControlPlane"}, "default", "demo-control-plane")
	if err != nil {
		t.Fatal(err)
	}
	files, _, _ := unstructured.NestedSlice(kcp.Object, "spec", "kubeadmConfigSpec", "files")
	pre, _, _ := unstructured.NestedStringSlice(kcp.Object, "spec", "kubeadmConfigSpec", "preKubeadmCommands")
	if len(files) != 1 || files[0].(map[string]any)["path"] != generate.InstallScriptPath || !reflect.DeepEqual(pre, []string{generate.InstallScriptPath}) {
		t.Errorf("control plane install patch: files=%v pre=%v", files, pre)
	}
	if content, _ := files[0].(map[string]any)["content"].(string); !strings.Contains(content, "http://cache.lan:3142") {
		t.Error("install options not passed through")
	}
}

func TestCreateSkipsMachineTemplateTagWhenUntagged(t *testing.T) {
	mtGVK := schema.GroupVersionKind{Group: generate.InfrastructureGroup, Version: "v1alpha1", Kind: "FakeMachineTemplate"}
	clusterTag := func(t *testing.T, h *harness) []string {
		t.Helper()
		mt, err := h.kube.Get(t.Context(), mtGVK, "default", "demo-control-plane")
		if err != nil {
			t.Fatal(err)
		}
		tags, _, _ := unstructured.NestedStringSlice(mt.Object, "spec", "template", "spec", "tags")
		return tags
	}

	// By default klotho adds the per-cluster tag to the machine template.
	h := newHarness(t)
	if _, err := Create(t.Context(), h.deps, request()); err != nil {
		t.Fatal(err)
	}
	if tags := clusterTag(t, h); !slices.Contains(tags, "klotho-demo") {
		t.Errorf("default provider: expected klotho-demo in machine-template tags, got %v", tags)
	}

	// A provider that opts out (the strict Hyper-V CRD case) does not get the
	// per-cluster tag appended, so the apply is not rejected for an unknown field.
	h2 := newHarness(t)
	h2.infra.untagged = true
	if _, err := Create(t.Context(), h2.deps, request()); err != nil {
		t.Fatal(err)
	}
	if tags := clusterTag(t, h2); slices.Contains(tags, "klotho-demo") {
		t.Errorf("untagged provider: klotho-demo should not be appended, got %v", tags)
	}
}

func TestCreateExplicitPatchErrors(t *testing.T) {
	h := newHarness(t)
	req := request()
	req.InstallKubernetes = true
	req.InstallOptions = generate.InstallKubernetesOptions{AptProxy: "not a url"}
	if _, err := Create(t.Context(), h.deps, req); err == nil || !strings.Contains(err.Error(), "bundle: apt proxy") {
		t.Fatalf("got %v", err)
	}
	if h.store.state("demo") != nil {
		t.Fatal("reservation kept after a bundle error")
	}
	req = request()
	req.MachineTemplateTags = []string{"bad tag"}
	if _, err := Create(t.Context(), h.deps, req); err == nil || !strings.Contains(err.Error(), "bundle: template tag") {
		t.Fatalf("got %v", err)
	}
}
