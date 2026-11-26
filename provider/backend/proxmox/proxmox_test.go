package proxmox

import (
	"bytes"
	"context"
	"errors"
	"reflect"
	"strings"
	"testing"

	"github.com/klotho-k8s/klotho/pkg/state"
	"github.com/klotho-k8s/klotho/provider/backend"
)

func TestReserveTwice(t *testing.T) {
	s, f := newStore(t)
	ctx := context.Background()
	ok, err := s.Reserve(ctx, "dev")
	if err != nil || !ok {
		t.Fatalf("first Reserve = %v, %v", ok, err)
	}
	ok, err = s.Reserve(ctx, "dev")
	if err != nil || ok {
		t.Fatalf("second Reserve = %v, %v; want false, nil", ok, err)
	}
	if !f.has("klotho-dev") {
		t.Fatal("pool klotho-dev not created")
	}
	rec, err := s.Load(ctx, "dev")
	if err != nil {
		t.Fatal(err)
	}
	st, err := state.PlainCodec{}.Open(rec)
	if err != nil {
		t.Fatal(err)
	}
	if !st.Reserved || st.Spec.Name != "dev" {
		t.Fatalf("reservation = %+v", st)
	}
}

func TestReserveOtherError(t *testing.T) {
	s, f := newStore(t)
	f.maxComment = 3
	if ok, err := s.Reserve(context.Background(), "dev"); err == nil || ok {
		t.Fatalf("Reserve = %v, %v; want error", ok, err)
	}
}

func TestRoundTrip(t *testing.T) {
	s, f := newStore(t)
	ctx := context.Background()
	if _, err := s.Reserve(ctx, "dev"); err != nil {
		t.Fatal(err)
	}
	rec := []byte(state.Marker + "YWdlLWVuY3J5cHRpb24ub3JnL3YxCi0+IFgyNTUxOSA/+=")
	if err := s.Save(ctx, "dev", rec); err != nil {
		t.Fatal(err)
	}
	got, err := s.Load(ctx, "dev")
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(got, rec) {
		t.Fatalf("Load = %q, want %q", got, rec)
	}
	if f.comment("klotho-dev") != string(rec) {
		t.Fatalf("stored comment = %q", f.comment("klotho-dev"))
	}
}

func TestLoadForeignPool(t *testing.T) {
	s, f := newStore(t)
	for _, comment := range []string{"", "team sandbox"} {
		f.put(pool{ID: "klotho-dev", Comment: comment})
		if _, err := s.Load(context.Background(), "dev"); !errors.Is(err, ErrNotAKlothoPool) {
			t.Errorf("comment %q: err = %v, want ErrNotAKlothoPool", comment, err)
		}
	}
}

func TestNotFound(t *testing.T) {
	s, _ := newStore(t)
	ctx := context.Background()
	if _, err := s.Load(ctx, "missing"); !errors.Is(err, backend.ErrNotFound) {
		t.Errorf("Load: err = %v", err)
	}
	if err := s.Save(ctx, "missing", []byte(state.PlainMarker+"{}")); !errors.Is(err, backend.ErrNotFound) {
		t.Errorf("Save: err = %v", err)
	}
	if err := s.Delete(ctx, "missing"); !errors.Is(err, backend.ErrNotFound) {
		t.Errorf("Delete: err = %v", err)
	}
}

func TestLoadTransportError(t *testing.T) {
	s, f := newStore(t)
	f.failGet = true
	_, err := s.Load(context.Background(), "dev")
	if err == nil || errors.Is(err, backend.ErrNotFound) || errors.Is(err, ErrNotAKlothoPool) {
		t.Fatalf("err = %v", err)
	}
}

func TestSaveRejects(t *testing.T) {
	s, f := newStore(t)
	ctx := context.Background()
	if _, err := s.Reserve(ctx, "dev"); err != nil {
		t.Fatal(err)
	}
	for _, rec := range []string{
		"",
		"plain text",
		state.PlainMarker + "{}\n",
		state.Marker + "abc\ndef",
		state.Marker + "abc\rdef",
	} {
		if err := s.Save(ctx, "dev", []byte(rec)); err == nil {
			t.Errorf("Save(%q) accepted", rec)
		}
	}
	for _, c := range f.callLog() {
		if strings.HasPrefix(c, "update") {
			t.Errorf("rejected record reached the server: %s", c)
		}
	}
}

func TestDeleteRemovesGuestMembers(t *testing.T) {
	s, f := newStore(t)
	f.put(pool{ID: "klotho-dev", Comment: state.PlainMarker + "{}", Members: []member{
		{ID: "qemu/120", Type: "qemu", VMID: 120},
		{ID: "lxc/130", Type: "lxc", VMID: 130},
	}})
	if err := s.Delete(context.Background(), "dev"); err != nil {
		t.Fatal(err)
	}
	if f.has("klotho-dev") {
		t.Fatal("pool still exists")
	}
	want := []string{"get klotho-dev", "remove klotho-dev [120 130]", "delete klotho-dev"}
	if got := f.callLog(); !reflect.DeepEqual(got, want) {
		t.Fatalf("calls = %q, want %q", got, want)
	}
}

func TestDeleteStorageMember(t *testing.T) {
	s, f := newStore(t)
	f.put(pool{ID: "klotho-dev", Comment: state.PlainMarker + "{}", Members: []member{
		{ID: "qemu/120", Type: "qemu", VMID: 120},
		{ID: "storage/pve1/local", Type: "storage", Storage: "local"},
	}})
	err := s.Delete(context.Background(), "dev")
	if err == nil || !strings.Contains(err.Error(), "storage/pve1/local") {
		t.Fatalf("err = %v, want one naming storage/pve1/local", err)
	}
	if !f.has("klotho-dev") {
		t.Fatal("pool deleted")
	}
	for _, c := range f.callLog() {
		if !strings.HasPrefix(c, "get") {
			t.Errorf("unexpected mutating call %s", c)
		}
	}
}

func TestDeleteForeignPool(t *testing.T) {
	s, f := newStore(t)
	f.put(pool{ID: "klotho-dev", Comment: "hand-made"})
	if err := s.Delete(context.Background(), "dev"); !errors.Is(err, ErrNotAKlothoPool) {
		t.Fatalf("err = %v, want ErrNotAKlothoPool", err)
	}
	if !f.has("klotho-dev") {
		t.Fatal("foreign pool deleted")
	}
}

func TestList(t *testing.T) {
	s, f := newStore(t)
	rec := state.PlainMarker + "{}"
	for _, p := range []pool{
		{ID: "klotho-zeta", Comment: rec},
		{ID: "klotho-alpha", Comment: state.Marker + "abc"},
		{ID: "klotho-mid", Comment: "  " + rec},
		{ID: "klotho-human", Comment: "my pool"},
		{ID: "klotho-empty"},
		{ID: "other", Comment: rec},
		{ID: "klotho-", Comment: rec},
	} {
		f.put(p)
	}
	got, err := s.List(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if want := []string{"alpha", "mid", "zeta"}; !reflect.DeepEqual(got, want) {
		t.Fatalf("List = %q, want %q", got, want)
	}
}

func TestListEmpty(t *testing.T) {
	s, _ := newStore(t)
	got, err := s.List(context.Background())
	if err != nil || got == nil || len(got) != 0 {
		t.Fatalf("List = %#v, %v", got, err)
	}
}

func TestInvalidNames(t *testing.T) {
	s, f := newStore(t)
	ctx := context.Background()
	for _, name := range []string{"", "-dev", ".dev", "a/b", "has space", "x?y", "ünï", strings.Repeat("a", 64)} {
		if _, err := s.Reserve(ctx, name); err == nil {
			t.Errorf("Reserve(%q) accepted", name)
		}
		if _, err := s.Load(ctx, name); err == nil {
			t.Errorf("Load(%q) accepted", name)
		}
		if err := s.Save(ctx, name, []byte(state.PlainMarker+"{}")); err == nil {
			t.Errorf("Save(%q) accepted", name)
		}
		if err := s.Delete(ctx, name); err == nil {
			t.Errorf("Delete(%q) accepted", name)
		}
	}
	if c := f.callLog(); len(c) != 0 {
		t.Fatalf("invalid names reached the server: %q", c)
	}
	// A prefix that is not valid in a pool ID makes every name invalid.
	bad := New(f.client(t), state.PlainCodec{}, WithPrefix("klotho/"))
	if _, err := bad.Reserve(ctx, "dev"); err == nil {
		t.Error("Reserve with invalid prefix accepted")
	}
}

func TestPrefixOption(t *testing.T) {
	s, f := newStore(t, WithPrefix("k8s_"))
	if s.Prefix() != "k8s_" || s.PoolID("dev") != "k8s_dev" {
		t.Fatalf("Prefix = %q, PoolID = %q", s.Prefix(), s.PoolID("dev"))
	}
	if s.Name() != "proxmox" {
		t.Fatalf("Name = %q", s.Name())
	}
	ctx := context.Background()
	if _, err := s.Reserve(ctx, "dev"); err != nil {
		t.Fatal(err)
	}
	if !f.has("k8s_dev") {
		t.Fatal("pool k8s_dev not created")
	}
	f.put(pool{ID: "klotho-other", Comment: state.PlainMarker + "{}"})
	got, err := s.List(ctx)
	if err != nil || !reflect.DeepEqual(got, []string{"dev"}) {
		t.Fatalf("List = %q, %v", got, err)
	}
	def, _ := newStore(t)
	if def.Prefix() != DefaultPrefix {
		t.Fatalf("default prefix = %q", def.Prefix())
	}
}

// TestStoreLifecycle walks a store through reserve, save, load, list and
// delete against the emulated server, with foreign pools present.
func TestStoreLifecycle(t *testing.T) {
	s, f := newStore(t)
	f.put(pool{ID: "klotho-human", Comment: "my pool"})
	f.put(pool{ID: "unrelated", Comment: state.PlainMarker + "{}"})
	ctx := context.Background()

	for _, name := range []string{"dev", "alpha"} {
		ok, err := s.Reserve(ctx, name)
		if err != nil || !ok {
			t.Fatalf("Reserve(%s) = %v, %v", name, ok, err)
		}
	}
	if ok, err := s.Reserve(ctx, "dev"); err != nil || ok {
		t.Fatalf("second Reserve = %v, %v; want false, nil", ok, err)
	}

	rec, err := s.Load(ctx, "dev")
	if err != nil {
		t.Fatal(err)
	}
	st, err := state.PlainCodec{}.Open(rec)
	if err != nil || !st.Reserved || st.Spec.Name != "dev" {
		t.Fatalf("Load = %+v, %v", st, err)
	}

	saved := []byte(state.Marker + "YWdlLWVuY3J5cHRpb24ub3JnL3YxCi0+IFgyNTUxOSA+/=")
	if err := s.Save(ctx, "dev", saved); err != nil {
		t.Fatal(err)
	}
	if got, err := s.Load(ctx, "dev"); err != nil || string(got) != string(saved) {
		t.Fatalf("Load after Save = %q, %v", got, err)
	}

	if _, err := s.Load(ctx, "missing"); !errors.Is(err, backend.ErrNotFound) {
		t.Fatalf("Load missing: err = %v", err)
	}
	if _, err := s.Load(ctx, "human"); !errors.Is(err, ErrNotAKlothoPool) {
		t.Fatalf("Load human: err = %v", err)
	}

	names, err := s.List(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if want := []string{"alpha", "dev"}; !reflect.DeepEqual(names, want) {
		t.Fatalf("List = %q, want %q", names, want)
	}

	if err := s.Delete(ctx, "alpha"); err != nil {
		t.Fatal(err)
	}
	if err := s.Delete(ctx, "alpha"); !errors.Is(err, backend.ErrNotFound) {
		t.Fatalf("second Delete: err = %v", err)
	}
	if names, err := s.List(ctx); err != nil || !reflect.DeepEqual(names, []string{"dev"}) {
		t.Fatalf("List after Delete = %q, %v", names, err)
	}
}
