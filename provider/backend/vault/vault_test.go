package vault

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"net/http"
	"sort"
	"strconv"
	"strings"
	"sync"
	"testing"

	vaultapi "github.com/hashicorp/vault/api"

	"github.com/klotho-k8s/klotho/pkg/cluster"
	"github.com/klotho-k8s/klotho/provider/backend"
)

// version is one stored revision of a secret, enough of KV v2 for the store.
type version struct {
	data    map[string]any
	deleted bool
}

// fakeVault is an in-memory logical client that mimics the KV v2 semantics the
// store depends on: versioning, the data/metadata path split, and check-and-set
// on write.
type fakeVault struct {
	mu      sync.Mutex
	secrets map[string][]version // keyed by the secret's logical name (mount+prefix+name)

	reads, writes, deletes, lists int

	// failWrite makes a write to this data path fail with a generic error.
	failWrite string
}

func newFakeVault() *fakeVault { return &fakeVault{secrets: map[string][]version{}} }

// split turns a KV v2 API path "<mount>/<segment>/<rest>" into its segment
// ("data" or "metadata") and the logical name beneath it.
func split(path string) (segment, name string) {
	parts := strings.SplitN(strings.Trim(path, "/"), "/", 3)
	if len(parts) < 2 {
		return "", ""
	}
	if len(parts) == 2 {
		return parts[1], "" // mount/segment, a list of the root
	}
	return parts[1], parts[2]
}

func (f *fakeVault) ReadWithContext(_ context.Context, path string) (*vaultapi.Secret, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.reads++
	segment, name := split(path)
	vs, ok := f.secrets[name]
	if !ok || len(vs) == 0 {
		return nil, nil
	}
	cur := vs[len(vs)-1]
	switch segment {
	case metadataSegment:
		return &vaultapi.Secret{Data: map[string]any{
			"current_version": json.Number(itoa(len(vs))),
		}}, nil
	case dataSegment:
		out := map[string]any{
			"metadata": map[string]any{"version": json.Number(itoa(len(vs)))},
		}
		if cur.deleted {
			out["data"] = nil
		} else {
			out["data"] = cur.data
		}
		return &vaultapi.Secret{Data: out}, nil
	}
	return nil, nil
}

func (f *fakeVault) WriteWithContext(_ context.Context, path string, in map[string]any) (*vaultapi.Secret, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.writes++
	if path == f.failWrite {
		return nil, errors.New("boom")
	}
	_, name := split(path)
	data, _ := in["data"].(map[string]any)
	cas, hasCAS := casValue(in)

	cur := len(f.secrets[name])
	if hasCAS && cas != cur {
		return nil, casError()
	}
	f.secrets[name] = append(f.secrets[name], version{data: data})
	return &vaultapi.Secret{Data: map[string]any{
		"version": json.Number(itoa(len(f.secrets[name]))),
	}}, nil
}

func (f *fakeVault) DeleteWithContext(_ context.Context, path string) (*vaultapi.Secret, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.deletes++
	_, name := split(path)
	delete(f.secrets, name)
	return nil, nil
}

func (f *fakeVault) ListWithContext(_ context.Context, path string) (*vaultapi.Secret, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.lists++
	// The list path is "<mount>/metadata/<prefix>"; names are stored as
	// "<prefix>/<name>" (or bare when no prefix). Derive the listing prefix.
	_, listPrefix := split(path)
	var keys []string
	for name, vs := range f.secrets {
		if len(vs) == 0 {
			continue
		}
		rest, ok := childUnder(name, listPrefix)
		if !ok {
			continue
		}
		keys = append(keys, rest)
	}
	if keys == nil {
		return nil, nil
	}
	sort.Strings(keys)
	anyKeys := make([]any, len(keys))
	for i, k := range keys {
		anyKeys[i] = k
	}
	return &vaultapi.Secret{Data: map[string]any{"keys": anyKeys}}, nil
}

// childUnder returns the immediate child segment of name beneath prefix. For a
// nested name it returns "<folder>/" to mirror KV v2's folder listing.
func childUnder(name, prefix string) (string, bool) {
	if prefix != "" {
		rest, ok := strings.CutPrefix(name, prefix+"/")
		if !ok {
			return "", false
		}
		name = rest
	}
	if i := strings.IndexByte(name, '/'); i >= 0 {
		return name[:i+1], true // a folder, with its trailing slash
	}
	return name, true
}

func casValue(in map[string]any) (int, bool) {
	opts, ok := in["options"].(map[string]any)
	if !ok {
		return 0, false
	}
	v, ok := opts["cas"]
	if !ok {
		return 0, false
	}
	return intFromJSON(v), true
}

// casError is the error Vault returns for a failed check-and-set: a 400 whose
// body carries the check-and-set message.
func casError() error {
	return &vaultapi.ResponseError{
		StatusCode: http.StatusBadRequest,
		Errors:     []string{"check-and-set parameter did not match the current version"},
	}
}

func itoa(i int) string { return strconv.Itoa(i) }

// fakeSealer seals a reservation to deterministic opaque bytes, including a null
// byte so the base64 transport is exercised on non-text data.
type fakeSealer struct{}

func (fakeSealer) Seal(s *cluster.State) ([]byte, error) {
	return []byte("reservation:" + s.Spec.Name + "\x00"), nil
}

func newStore(t *testing.T, cfg Config) (*Store, *fakeVault) {
	t.Helper()
	fake := newFakeVault()
	s, err := New(cfg, fakeSealer{}, withLogical(fake))
	if err != nil {
		t.Fatal(err)
	}
	return s, fake
}

func TestReserveIsAtomic(t *testing.T) {
	s, fake := newStore(t, Config{Mount: "secret", Prefix: "clusters"})
	ctx := context.Background()

	ok, err := s.Reserve(ctx, "demo")
	if err != nil || !ok {
		t.Fatalf("first Reserve = %v, %v; want true, nil", ok, err)
	}
	if _, exists := fake.secrets["clusters/demo"]; !exists {
		t.Fatalf("reservation not written; keys = %v", keysOf(fake))
	}

	// A second Reserve of the same name loses: the cas=0 write is rejected
	// because a version already exists.
	ok, err = s.Reserve(ctx, "demo")
	if err != nil {
		t.Fatalf("second Reserve error = %v; want nil", err)
	}
	if ok {
		t.Fatal("second Reserve = true; want false (name already taken)")
	}
}

func TestSaveLoadRoundTrip(t *testing.T) {
	s, _ := newStore(t, Config{Prefix: "clusters"})
	ctx := context.Background()
	record := []byte("AGE-ENCRYPTED-OPAQUE-RECORD\x00\x01\x02")

	if _, err := s.Reserve(ctx, "demo"); err != nil {
		t.Fatal(err)
	}
	if err := s.Save(ctx, "demo", record); err != nil {
		t.Fatal(err)
	}
	got, err := s.Load(ctx, "demo")
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(got, record) {
		t.Errorf("Load = %q, want %q", got, record)
	}
}

func TestSaveWithoutReserve(t *testing.T) {
	s, _ := newStore(t, Config{})
	ctx := context.Background()
	record := []byte("first-write")
	// A Save with no prior reservation starts at version 0 (cas=0) and succeeds.
	if err := s.Save(ctx, "solo", record); err != nil {
		t.Fatalf("Save of new key = %v; want nil", err)
	}
	got, err := s.Load(ctx, "solo")
	if err != nil || !bytes.Equal(got, record) {
		t.Fatalf("Load = %q, %v; want %q", got, err, record)
	}
}

func TestLoadMissingIsNotFound(t *testing.T) {
	s, _ := newStore(t, Config{})
	_, err := s.Load(context.Background(), "ghost")
	if !errors.Is(err, backend.ErrNotFound) {
		t.Fatalf("Load of missing = %v; want backend.ErrNotFound", err)
	}
}

func TestDelete(t *testing.T) {
	s, fake := newStore(t, Config{Prefix: "clusters"})
	ctx := context.Background()
	if _, err := s.Reserve(ctx, "demo"); err != nil {
		t.Fatal(err)
	}
	if err := s.Delete(ctx, "demo"); err != nil {
		t.Fatalf("Delete = %v; want nil", err)
	}
	if len(fake.secrets) != 0 {
		t.Errorf("secrets after Delete = %v", keysOf(fake))
	}
	// Deleting again reports not found, matching the other backends.
	if err := s.Delete(ctx, "demo"); !errors.Is(err, backend.ErrNotFound) {
		t.Fatalf("second Delete = %v; want backend.ErrNotFound", err)
	}
}

func TestList(t *testing.T) {
	s, fake := newStore(t, Config{Prefix: "clusters"})
	ctx := context.Background()
	for _, n := range []string{"beta", "alpha", "gamma"} {
		if _, err := s.Reserve(ctx, n); err != nil {
			t.Fatal(err)
		}
	}
	// Noise the store should ignore: a secret outside the prefix and a nested
	// folder under the prefix.
	fake.secrets["other/x"] = []version{{data: map[string]any{recordField: "x"}}}
	fake.secrets["clusters/sub/dir"] = []version{{data: map[string]any{recordField: "x"}}}

	names, err := s.List(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if want := []string{"alpha", "beta", "gamma"}; strings.Join(names, ",") != strings.Join(want, ",") {
		t.Errorf("List = %v, want %v", names, want)
	}
}

func TestListNoPrefix(t *testing.T) {
	s, _ := newStore(t, Config{})
	ctx := context.Background()
	if _, err := s.Reserve(ctx, "solo"); err != nil {
		t.Fatal(err)
	}
	names, err := s.List(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Join(names, ",") != "solo" {
		t.Errorf("List = %v, want [solo]", names)
	}
}

func TestListEmpty(t *testing.T) {
	s, _ := newStore(t, Config{Prefix: "clusters"})
	names, err := s.List(context.Background())
	if err != nil {
		t.Fatalf("List = %v", err)
	}
	if len(names) != 0 {
		t.Errorf("List of empty store = %v, want []", names)
	}
}

// TestSaveRetriesOnCASConflict drives a save against a key whose version jumps
// between the read and the write once, proving Save re-reads and wins on retry.
func TestSaveRetriesOnCASConflict(t *testing.T) {
	ctx := context.Background()
	fake := newFakeVault()
	racer := &racingVault{fakeVault: fake}
	s, err := New(Config{Prefix: "clusters"}, fakeSealer{}, withLogical(racer))
	if err != nil {
		t.Fatal(err)
	}
	if _, err := s.Reserve(ctx, "demo"); err != nil {
		t.Fatal(err)
	}
	// Arm one interfering write: the first Save read sees version 1, but before
	// its write a competitor bumps the key to version 2, so the cas=1 write is
	// rejected and Save must re-read (seeing version 2) and retry with cas=2.
	racer.armInterference("clusters/demo")

	if err := s.Save(ctx, "demo", []byte("winning-record")); err != nil {
		t.Fatalf("Save = %v; want nil after retry", err)
	}
	got, err := s.Load(ctx, "demo")
	if err != nil || !bytes.Equal(got, []byte("winning-record")) {
		t.Fatalf("Load = %q, %v; want winning-record", got, err)
	}
	if racer.conflicts != 1 {
		t.Errorf("CAS conflicts = %d; want exactly 1", racer.conflicts)
	}
}

// TestSaveGivesUpAfterRetries proves Save surfaces an error when every retry
// loses to a relentless competitor.
func TestSaveGivesUpAfterRetries(t *testing.T) {
	ctx := context.Background()
	fake := newFakeVault()
	racer := &racingVault{fakeVault: fake, relentless: true}
	s, err := New(Config{}, fakeSealer{}, withLogical(racer))
	if err != nil {
		t.Fatal(err)
	}
	if _, err := s.Reserve(ctx, "demo"); err != nil {
		t.Fatal(err)
	}
	racer.armInterference("demo")
	if err := s.Save(ctx, "demo", []byte("x")); err == nil {
		t.Fatal("Save = nil; want an error after exhausting retries")
	}
}

func TestSaveErrorSurfaces(t *testing.T) {
	s, fake := newStore(t, Config{Prefix: "p"})
	fake.failWrite = s.dataPath("demo")
	err := s.Save(context.Background(), "demo", []byte("x"))
	if err == nil || !strings.Contains(err.Error(), "boom") {
		t.Fatalf("Save error = %v; want it to carry the underlying failure", err)
	}
}

func TestInvalidNamesRejectedWithoutRequests(t *testing.T) {
	s, fake := newStore(t, Config{})
	ctx := context.Background()
	for _, name := range []string{"", ".", "..", "a/b", "-a", strings.Repeat("a", 64)} {
		if _, err := s.Reserve(ctx, name); err == nil {
			t.Errorf("Reserve(%q) accepted", name)
		}
		if _, err := s.Load(ctx, name); err == nil {
			t.Errorf("Load(%q) accepted", name)
		}
		if err := s.Delete(ctx, name); err == nil {
			t.Errorf("Delete(%q) accepted", name)
		}
	}
	if fake.reads+fake.writes+fake.deletes != 0 {
		t.Errorf("requests were sent for invalid names: %+v", fake)
	}
}

func TestPathLayout(t *testing.T) {
	for _, tc := range []struct {
		mount, prefix, name              string
		wantData, wantMeta, wantListPath string
	}{
		{"secret", "", "demo", "secret/data/demo", "secret/metadata/demo", "secret/metadata"},
		{"secret", "clusters", "demo", "secret/data/clusters/demo", "secret/metadata/clusters/demo", "secret/metadata/clusters"},
		{"kv", "/a/b/", "demo", "kv/data/a/b/demo", "kv/metadata/a/b/demo", "kv/metadata/a/b"},
	} {
		s := &Store{mount: strings.Trim(tc.mount, "/"), prefix: strings.Trim(tc.prefix, "/")}
		if got := s.dataPath(tc.name); got != tc.wantData {
			t.Errorf("dataPath(mount=%q,prefix=%q,%q) = %q, want %q", tc.mount, tc.prefix, tc.name, got, tc.wantData)
		}
		if got := s.metadataPath(tc.name); got != tc.wantMeta {
			t.Errorf("metadataPath = %q, want %q", got, tc.wantMeta)
		}
		if got := s.listPath(); got != tc.wantListPath {
			t.Errorf("listPath = %q, want %q", got, tc.wantListPath)
		}
	}
}

func TestNewValidation(t *testing.T) {
	if _, err := New(Config{}, nil, withLogical(newFakeVault())); err == nil {
		t.Error("nil sealer accepted")
	}
	s, err := New(Config{}, fakeSealer{}, withLogical(newFakeVault()))
	if err != nil {
		t.Fatal(err)
	}
	if s.Name() != "vault" || s.Mount() != DefaultMount || s.prefix != "" {
		t.Errorf("defaults: name=%q mount=%q prefix=%q", s.Name(), s.Mount(), s.prefix)
	}
}

func TestDecodeRecordErrors(t *testing.T) {
	// A soft-deleted secret (data == nil) is a load error, not a silent empty.
	if _, err := decodeRecord(&vaultapi.Secret{Data: map[string]any{"data": nil}}); err == nil {
		t.Error("nil data accepted")
	}
	// Bad base64 is surfaced.
	bad := &vaultapi.Secret{Data: map[string]any{"data": map[string]any{recordField: "!!notbase64!!"}}}
	if _, err := decodeRecord(bad); err == nil {
		t.Error("invalid base64 accepted")
	}
}

func keysOf(f *fakeVault) []string {
	f.mu.Lock()
	defer f.mu.Unlock()
	var keys []string
	for k := range f.secrets {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	return keys
}

// racingVault wraps fakeVault to inject a single (or relentless) check-and-set
// conflict, simulating a concurrent writer between the store's read and write.
type racingVault struct {
	*fakeVault
	mu         sync.Mutex
	armed      map[string]bool
	relentless bool
	conflicts  int
}

func (r *racingVault) armInterference(name string) {
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.armed == nil {
		r.armed = map[string]bool{}
	}
	r.armed[name] = true
}

func (r *racingVault) WriteWithContext(ctx context.Context, path string, in map[string]any) (*vaultapi.Secret, error) {
	r.mu.Lock()
	_, name := split(path)
	if r.armed[name] {
		if !r.relentless {
			r.armed[name] = false
		}
		r.conflicts++
		r.mu.Unlock()
		// Bump the real version so the store's cas value is now stale, then let
		// the store's own write fail its cas check on re-run.
		_, _ = r.fakeVault.WriteWithContext(ctx, path, map[string]any{
			"data": map[string]any{recordField: base64.StdEncoding.EncodeToString([]byte("competitor"))},
		})
		return nil, casError()
	}
	r.mu.Unlock()
	return r.fakeVault.WriteWithContext(ctx, path, in)
}
