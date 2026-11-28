package azureblob

import (
	"bytes"
	"context"
	"errors"
	"sort"
	"strings"
	"sync"
	"testing"

	"github.com/klotho-k8s/klotho/pkg/cluster"
	"github.com/klotho-k8s/klotho/provider/backend"
)

// fakeBlob is an in-memory api: just enough of Azure Blob for the store,
// including the If-None-Match: * semantics that Reserve depends on.
type fakeBlob struct {
	mu    sync.Mutex
	blobs map[string][]byte

	uploads, downloads, exists, deletes, lists int

	// failUpload makes Upload of this name fail with a generic error, to test
	// that a write error is surfaced rather than swallowed.
	failUpload string
}

func newFakeBlob() *fakeBlob { return &fakeBlob{blobs: map[string][]byte{}} }

func (f *fakeBlob) Upload(_ context.Context, name string, data []byte, ifNotExists bool) (bool, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.uploads++
	if name == f.failUpload {
		return false, errors.New("boom")
	}
	if ifNotExists {
		if _, ok := f.blobs[name]; ok {
			return false, nil
		}
	}
	// Copy so callers cannot mutate the stored record.
	stored := make([]byte, len(data))
	copy(stored, data)
	f.blobs[name] = stored
	return true, nil
}

func (f *fakeBlob) Download(_ context.Context, name string) ([]byte, bool, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.downloads++
	b, ok := f.blobs[name]
	if !ok {
		return nil, false, nil
	}
	return b, true, nil
}

func (f *fakeBlob) Exists(_ context.Context, name string) (bool, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.exists++
	_, ok := f.blobs[name]
	return ok, nil
}

func (f *fakeBlob) Delete(_ context.Context, name string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.deletes++
	delete(f.blobs, name)
	return nil
}

func (f *fakeBlob) List(_ context.Context, prefix string) ([]string, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.lists++
	var names []string
	for k := range f.blobs {
		if strings.HasPrefix(k, prefix) {
			names = append(names, k)
		}
	}
	sort.Strings(names)
	return names, nil
}

// fakeSealer seals a reservation to deterministic opaque bytes. The store
// treats the result as opaque, so this never needs real encryption.
type fakeSealer struct{}

func (fakeSealer) Seal(s *cluster.State) ([]byte, error) {
	return []byte("reservation:" + s.Spec.Name), nil
}

func newStore(t *testing.T, cfg Config) (*Store, *fakeBlob) {
	t.Helper()
	fake := newFakeBlob()
	cfg.Container = "state"
	if cfg.AccountName == "" {
		cfg.AccountName = "acct"
	}
	s, err := New(context.Background(), cfg, fakeSealer{}, withAPI(fake))
	if err != nil {
		t.Fatal(err)
	}
	return s, fake
}

func TestReserveIsAtomic(t *testing.T) {
	s, fake := newStore(t, Config{Prefix: "clusters"})
	ctx := context.Background()

	ok, err := s.Reserve(ctx, "demo")
	if err != nil || !ok {
		t.Fatalf("first Reserve = %v, %v; want true, nil", ok, err)
	}
	// The reservation is stored under the prefixed name with a body.
	if _, exists := fake.blobs["clusters/demo.record"]; !exists {
		t.Fatalf("reservation blob not written; names = %v", namesOf(fake))
	}

	// A second Reserve of the same name loses: the conditional upload is
	// rejected.
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
	if len(fake.blobs) != 0 {
		t.Errorf("blobs after Delete = %v", namesOf(fake))
	}
	// Deleting again reports not found, matching the file, proxmox and s3
	// backends.
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
	// Noise the store should ignore: a name outside the prefix, a name under
	// the prefix without the suffix, and a nested name.
	fake.blobs["other/x.record"] = []byte("x")
	fake.blobs["clusters/not-a-record.txt"] = []byte("x")
	fake.blobs["clusters/sub/dir.record"] = []byte("x")

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

func TestKeyLayout(t *testing.T) {
	for _, tc := range []struct {
		prefix, name, want string
	}{
		{"", "demo", "demo.record"},
		{"clusters", "demo", "clusters/demo.record"},
		{"/a/b/", "demo", "a/b/demo.record"},
	} {
		s := &Store{prefix: strings.Trim(tc.prefix, "/")}
		got, err := s.key(tc.name)
		if err != nil || got != tc.want {
			t.Errorf("key(prefix=%q, %q) = %q, %v; want %q", tc.prefix, tc.name, got, err, tc.want)
		}
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
	if fake.uploads+fake.downloads+fake.exists+fake.deletes != 0 {
		t.Errorf("requests were sent for invalid names: %+v", fake)
	}
}

func TestSaveErrorSurfaces(t *testing.T) {
	s, fake := newStore(t, Config{Prefix: "p"})
	fake.failUpload = "p/demo.record"
	err := s.Save(context.Background(), "demo", []byte("x"))
	if err == nil || !strings.Contains(err.Error(), "boom") {
		t.Fatalf("Save error = %v; want it to carry the underlying failure", err)
	}
}

func TestNewValidation(t *testing.T) {
	ctx := context.Background()
	for name, cfg := range map[string]Config{
		"no container":          {AccountName: "acct"},
		"bad endpoint":          {Container: "c", AccountName: "acct", Endpoint: "blob:10000"},
		"ftp endpoint":          {Container: "c", AccountName: "acct", Endpoint: "ftp://blob"},
		"no account no connstr": {Container: "c"},
		"key without account":   {Container: "c", Endpoint: "http://127.0.0.1:10000/dev", AccountKey: "k"},
	} {
		if _, err := New(ctx, cfg, fakeSealer{}, withAPI(newFakeBlob())); err == nil {
			t.Errorf("%s: accepted", name)
		}
	}
	if _, err := New(ctx, Config{Container: "c", AccountName: "acct"}, nil, withAPI(newFakeBlob())); err == nil {
		t.Error("nil sealer accepted")
	}
	s, err := New(ctx, Config{Container: "c", AccountName: "acct"}, fakeSealer{}, withAPI(newFakeBlob()))
	if err != nil {
		t.Fatal(err)
	}
	if s.Name() != "azureblob" || s.Container() != "c" || s.prefix != "" {
		t.Errorf("defaults: %+v", s)
	}
}

func namesOf(f *fakeBlob) []string {
	f.mu.Lock()
	defer f.mu.Unlock()
	var names []string
	for k := range f.blobs {
		names = append(names, k)
	}
	sort.Strings(names)
	return names
}
