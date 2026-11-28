package gcs

import (
	"bytes"
	"context"
	"errors"
	"sort"
	"strings"
	"sync"
	"testing"

	"cloud.google.com/go/storage"

	"github.com/klotho-k8s/klotho/pkg/cluster"
	"github.com/klotho-k8s/klotho/provider/backend"
)

// fakeGCS is an in-memory api: just enough of GCS for the store, including the
// DoesNotExist create semantics that Reserve depends on.
type fakeGCS struct {
	mu      sync.Mutex
	objects map[string][]byte

	creates, puts, gets, removes, lists int

	// failPut makes put of this key fail with a generic error, to test that a
	// write error is surfaced rather than swallowed.
	failPut string
}

func newFakeGCS() *fakeGCS { return &fakeGCS{objects: map[string][]byte{}} }

func (f *fakeGCS) create(_ context.Context, key string, data []byte) (bool, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.creates++
	if _, exists := f.objects[key]; exists {
		return false, nil
	}
	f.objects[key] = append([]byte(nil), data...)
	return true, nil
}

func (f *fakeGCS) put(_ context.Context, key string, data []byte) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.puts++
	if key == f.failPut {
		return errors.New("boom")
	}
	f.objects[key] = append([]byte(nil), data...)
	return nil
}

func (f *fakeGCS) get(_ context.Context, key string) ([]byte, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.gets++
	body, ok := f.objects[key]
	if !ok {
		return nil, storage.ErrObjectNotExist
	}
	return append([]byte(nil), body...), nil
}

func (f *fakeGCS) remove(_ context.Context, key string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.removes++
	if _, ok := f.objects[key]; !ok {
		return storage.ErrObjectNotExist
	}
	delete(f.objects, key)
	return nil
}

func (f *fakeGCS) list(_ context.Context, prefix string) ([]string, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.lists++
	var keys []string
	for k := range f.objects {
		if strings.HasPrefix(k, prefix) {
			keys = append(keys, k)
		}
	}
	sort.Strings(keys)
	return keys, nil
}

func (f *fakeGCS) Close() error { return nil }

// fakeSealer seals a reservation to deterministic opaque bytes. The store
// treats the result as opaque, so this never needs real encryption.
type fakeSealer struct{}

func (fakeSealer) Seal(s *cluster.State) ([]byte, error) {
	return []byte("reservation:" + s.Spec.Name), nil
}

func newStore(t *testing.T, cfg Config) (*Store, *fakeGCS) {
	t.Helper()
	fake := newFakeGCS()
	cfg.Bucket = "state"
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
	// The reservation is stored under the prefixed key with a body.
	if _, exists := fake.objects["clusters/demo.record"]; !exists {
		t.Fatalf("reservation object not written; keys = %v", keysOf(fake))
	}

	// A second Reserve of the same name loses: the conditional create is
	// rejected by the DoesNotExist precondition.
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
	if len(fake.objects) != 0 {
		t.Errorf("objects after Delete = %v", keysOf(fake))
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
	// Noise the store should ignore: a key outside the prefix, a key under the
	// prefix without the suffix, and a nested key.
	fake.objects["other/x.record"] = []byte("x")
	fake.objects["clusters/not-a-record.txt"] = []byte("x")
	fake.objects["clusters/sub/dir.record"] = []byte("x")

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
	if fake.creates+fake.puts+fake.gets+fake.removes != 0 {
		t.Errorf("requests were sent for invalid names: %+v", fake)
	}
}

func TestSaveErrorSurfaces(t *testing.T) {
	s, fake := newStore(t, Config{Prefix: "p"})
	fake.failPut = "p/demo.record"
	err := s.Save(context.Background(), "demo", []byte("x"))
	if err == nil || !strings.Contains(err.Error(), "boom") {
		t.Fatalf("Save error = %v; want it to carry the underlying failure", err)
	}
}

func TestNewValidation(t *testing.T) {
	ctx := context.Background()
	if _, err := New(ctx, Config{}, fakeSealer{}, withAPI(newFakeGCS())); err == nil {
		t.Error("no bucket accepted")
	}
	if _, err := New(ctx, Config{Bucket: "b"}, nil, withAPI(newFakeGCS())); err == nil {
		t.Error("nil sealer accepted")
	}
	s, err := New(ctx, Config{Bucket: "b"}, fakeSealer{}, withAPI(newFakeGCS()))
	if err != nil {
		t.Fatal(err)
	}
	if s.Name() != "gcs" || s.Bucket() != "b" || s.prefix != "" {
		t.Errorf("defaults: %+v", s)
	}
}

func keysOf(f *fakeGCS) []string {
	f.mu.Lock()
	defer f.mu.Unlock()
	var keys []string
	for k := range f.objects {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	return keys
}
