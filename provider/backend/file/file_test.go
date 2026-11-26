package file

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"reflect"
	"sync"
	"testing"

	"github.com/klotho-k8s/klotho/pkg/cluster"
	"github.com/klotho-k8s/klotho/pkg/state"
	"github.com/klotho-k8s/klotho/provider/backend"
)

func newStore(t *testing.T) *Store {
	t.Helper()
	s, err := New(filepath.Join(t.TempDir(), "state"), state.PlainCodec{})
	if err != nil {
		t.Fatal(err)
	}
	return s
}

func TestNew(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "a", "b")
	s, err := New(dir, state.PlainCodec{})
	if err != nil {
		t.Fatal(err)
	}
	if s.Name() != "file" {
		t.Fatalf("Name = %q", s.Name())
	}
	if fi, err := os.Stat(dir); err != nil || !fi.IsDir() {
		t.Fatalf("directory not created: %v", err)
	}
	if _, err := New("", state.PlainCodec{}); err == nil {
		t.Fatal("empty dir accepted")
	}
	if _, err := New(dir, nil); err == nil {
		t.Fatal("nil sealer accepted")
	}
}

func TestReserveTwice(t *testing.T) {
	ctx := context.Background()
	s := newStore(t)
	ok, err := s.Reserve(ctx, "dev")
	if err != nil || !ok {
		t.Fatalf("first reserve: %v, %v", ok, err)
	}
	ok, err = s.Reserve(ctx, "dev")
	if err != nil || ok {
		t.Fatalf("second reserve: %v, %v", ok, err)
	}

	rec, err := s.Load(ctx, "dev")
	if err != nil {
		t.Fatal(err)
	}
	got, err := state.PlainCodec{}.Open(rec)
	if err != nil {
		t.Fatal(err)
	}
	if !got.Reserved || got.Spec.Name != "dev" || got.CreatedAt.IsZero() {
		t.Fatalf("bad reservation: %+v", got)
	}
}

func TestReserveConcurrent(t *testing.T) {
	ctx := context.Background()
	s := newStore(t)
	const n = 16
	var wg sync.WaitGroup
	results := make([]bool, n)
	errs := make([]error, n)
	for i := range n {
		wg.Add(1)
		go func() {
			defer wg.Done()
			results[i], errs[i] = s.Reserve(ctx, "race")
		}()
	}
	wg.Wait()
	wins := 0
	for i := range n {
		if errs[i] != nil {
			t.Fatal(errs[i])
		}
		if results[i] {
			wins++
		}
	}
	if wins != 1 {
		t.Fatalf("%d reservations succeeded, want 1", wins)
	}
}

type failingSealer struct{}

func (failingSealer) Seal(*cluster.State) ([]byte, error) { return nil, errors.New("boom") }

func TestReserveSealFailureLeavesNothing(t *testing.T) {
	ctx := context.Background()
	s, err := New(t.TempDir(), failingSealer{})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := s.Reserve(ctx, "dev"); err == nil {
		t.Fatal("reserve succeeded with failing sealer")
	}
	if _, err := s.Load(ctx, "dev"); !errors.Is(err, backend.ErrNotFound) {
		t.Fatalf("want ErrNotFound, got %v", err)
	}
}

func TestReserveSaveLoad(t *testing.T) {
	ctx := context.Background()
	s := newStore(t)
	if ok, err := s.Reserve(ctx, "dev"); err != nil || !ok {
		t.Fatalf("reserve: %v, %v", ok, err)
	}
	want := []byte("klotho/v1:first")
	if err := s.Save(ctx, "dev", want); err != nil {
		t.Fatal(err)
	}
	got, err := s.Load(ctx, "dev")
	if err != nil {
		t.Fatal(err)
	}
	if string(got) != string(want) {
		t.Fatalf("Load = %q, want %q", got, want)
	}

	// Saving again replaces the existing file.
	want = []byte("klotho/v1:second")
	if err := s.Save(ctx, "dev", want); err != nil {
		t.Fatal(err)
	}
	if got, err = s.Load(ctx, "dev"); err != nil || string(got) != string(want) {
		t.Fatalf("Load after replace = %q, %v", got, err)
	}

	// Save does not need a prior reservation.
	if err := s.Save(ctx, "other", []byte("x")); err != nil {
		t.Fatal(err)
	}

	// No temporary files are left behind.
	entries, err := os.ReadDir(s.Dir())
	if err != nil {
		t.Fatal(err)
	}
	if len(entries) != 2 {
		var names []string
		for _, e := range entries {
			names = append(names, e.Name())
		}
		t.Fatalf("unexpected directory contents: %v", names)
	}
}

func TestDeleteThenLoad(t *testing.T) {
	ctx := context.Background()
	s := newStore(t)
	if err := s.Save(ctx, "dev", []byte("x")); err != nil {
		t.Fatal(err)
	}
	if err := s.Delete(ctx, "dev"); err != nil {
		t.Fatal(err)
	}
	if _, err := s.Load(ctx, "dev"); !errors.Is(err, backend.ErrNotFound) {
		t.Fatalf("Load after delete: want ErrNotFound, got %v", err)
	}
	if err := s.Delete(ctx, "dev"); !errors.Is(err, backend.ErrNotFound) {
		t.Fatalf("second delete: want ErrNotFound, got %v", err)
	}
	if ok, err := s.Reserve(ctx, "dev"); err != nil || !ok {
		t.Fatalf("reserve after delete: %v, %v", ok, err)
	}
}

func TestListSorted(t *testing.T) {
	ctx := context.Background()
	s := newStore(t)
	if names, err := s.List(ctx); err != nil || len(names) != 0 {
		t.Fatalf("empty list: %v, %v", names, err)
	}
	for _, n := range []string{"zeta", "alpha", "mid.1", "Beta"} {
		if err := s.Save(ctx, n, []byte("x")); err != nil {
			t.Fatal(err)
		}
	}
	// Noise that must be ignored.
	for _, n := range []string{"notes.txt", "alpha.record.bak", ".hidden.tmp", "..record", "bad name.record"} {
		if err := os.WriteFile(filepath.Join(s.Dir(), n), []byte("x"), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	if err := os.Mkdir(filepath.Join(s.Dir(), "dir.record"), 0o700); err != nil {
		t.Fatal(err)
	}
	names, err := s.List(ctx)
	if err != nil {
		t.Fatal(err)
	}
	want := []string{"Beta", "alpha", "mid.1", "zeta"}
	if !reflect.DeepEqual(names, want) {
		t.Fatalf("List = %v, want %v", names, want)
	}
}

func TestInvalidNameRejected(t *testing.T) {
	ctx := context.Background()
	s := newStore(t)
	bad := []string{
		"", ".", "..", "../x", `..\x`, "a/b", `a\b`, "/abs", `C:\x`, "C:x",
		".hidden", "-dash", "_under", "has space", "a\x00b", "é",
		"CON", "nul", "com1", "LPT9.x",
		"a234567890123456789012345678901234567890123456789012345678901234", // 64 chars
	}
	for _, n := range bad {
		if _, err := s.Reserve(ctx, n); err == nil {
			t.Errorf("Reserve(%q) accepted", n)
		}
		if _, err := s.Load(ctx, n); err == nil || errors.Is(err, backend.ErrNotFound) {
			t.Errorf("Load(%q): want validation error, got %v", n, err)
		}
		if err := s.Save(ctx, n, []byte("x")); err == nil {
			t.Errorf("Save(%q) accepted", n)
		}
		if err := s.Delete(ctx, n); err == nil || errors.Is(err, backend.ErrNotFound) {
			t.Errorf("Delete(%q): want validation error, got %v", n, err)
		}
	}
	good := []string{"a", "A1", "0x", "dev.cluster-1_b",
		"a23456789012345678901234567890123456789012345678901234567890123"} // 63 chars
	for _, n := range good {
		if err := ValidateName(n); err != nil {
			t.Errorf("ValidateName(%q): %v", n, err)
		}
	}
}

func TestCanceledContext(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	s := newStore(t)
	if _, err := s.Reserve(ctx, "dev"); !errors.Is(err, context.Canceled) {
		t.Fatalf("Reserve: %v", err)
	}
	if err := s.Save(ctx, "dev", nil); !errors.Is(err, context.Canceled) {
		t.Fatalf("Save: %v", err)
	}
	if _, err := s.List(ctx); !errors.Is(err, context.Canceled) {
		t.Fatalf("List: %v", err)
	}
}
