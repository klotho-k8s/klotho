package k8ssecret

import (
	"bytes"
	"context"
	"errors"
	"strings"
	"testing"

	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/client-go/kubernetes/fake"
	k8stesting "k8s.io/client-go/testing"

	"github.com/klotho-k8s/klotho/pkg/cluster"
	"github.com/klotho-k8s/klotho/provider/backend"
)

// fakeSealer seals a reservation to deterministic opaque bytes. The store
// treats the result as opaque, so this never needs real encryption.
type fakeSealer struct{}

func (fakeSealer) Seal(s *cluster.State) ([]byte, error) {
	return []byte("reservation:" + s.Spec.Name), nil
}

const ns = "klotho"

func newStore(t *testing.T) (*Store, *fake.Clientset) {
	t.Helper()
	client := fake.NewClientset()
	s, err := New(context.Background(), Config{Namespace: ns}, fakeSealer{}, WithClient(client))
	if err != nil {
		t.Fatal(err)
	}
	return s, client
}

func TestReserveIsAtomic(t *testing.T) {
	s, client := newStore(t)
	ctx := context.Background()

	ok, err := s.Reserve(ctx, "demo")
	if err != nil || !ok {
		t.Fatalf("first Reserve = %v, %v; want true, nil", ok, err)
	}
	sec, err := client.CoreV1().Secrets(ns).Get(ctx, "klotho-state-demo", metav1.GetOptions{})
	if err != nil {
		t.Fatalf("reservation Secret not written: %v", err)
	}
	if sec.Type != corev1.SecretTypeOpaque {
		t.Errorf("Secret type = %v, want Opaque", sec.Type)
	}
	if got := sec.Labels[ClusterLabel]; got != "demo" {
		t.Errorf("cluster label = %q, want demo", got)
	}
	if !bytes.Equal(sec.Data[DataKey], []byte("reservation:demo")) {
		t.Errorf("data = %q, want the sealed reservation", sec.Data[DataKey])
	}

	// A second Reserve of the same name loses: the Create is rejected.
	ok, err = s.Reserve(ctx, "demo")
	if err != nil {
		t.Fatalf("second Reserve error = %v; want nil", err)
	}
	if ok {
		t.Fatal("second Reserve = true; want false (name already taken)")
	}
}

func TestSaveCreatesThenRoundTrips(t *testing.T) {
	s, _ := newStore(t)
	ctx := context.Background()
	record := []byte("AGE-ENCRYPTED-OPAQUE-RECORD\x00\x01\x02")

	// Save with no prior reservation creates the Secret.
	if err := s.Save(ctx, "demo", record); err != nil {
		t.Fatalf("Save (create) = %v", err)
	}
	got, err := s.Load(ctx, "demo")
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(got, record) {
		t.Errorf("Load = %q, want %q", got, record)
	}

	// Save again replaces it.
	record2 := []byte("second")
	if err := s.Save(ctx, "demo", record2); err != nil {
		t.Fatalf("Save (update) = %v", err)
	}
	if got, _ := s.Load(ctx, "demo"); !bytes.Equal(got, record2) {
		t.Errorf("Load after update = %q, want %q", got, record2)
	}
}

func TestLoadMissingIsNotFound(t *testing.T) {
	s, _ := newStore(t)
	_, err := s.Load(context.Background(), "ghost")
	if !errors.Is(err, backend.ErrNotFound) {
		t.Fatalf("Load of missing = %v; want backend.ErrNotFound", err)
	}
}

func TestDelete(t *testing.T) {
	s, client := newStore(t)
	ctx := context.Background()
	if _, err := s.Reserve(ctx, "demo"); err != nil {
		t.Fatal(err)
	}
	if err := s.Delete(ctx, "demo"); err != nil {
		t.Fatalf("Delete = %v; want nil", err)
	}
	list, _ := client.CoreV1().Secrets(ns).List(ctx, metav1.ListOptions{})
	if len(list.Items) != 0 {
		t.Errorf("Secrets after Delete = %d, want 0", len(list.Items))
	}
	// Deleting again reports not found, matching the file and s3 backends.
	if err := s.Delete(ctx, "demo"); !errors.Is(err, backend.ErrNotFound) {
		t.Fatalf("second Delete = %v; want backend.ErrNotFound", err)
	}
}

func TestList(t *testing.T) {
	s, client := newStore(t)
	ctx := context.Background()
	for _, n := range []string{"beta", "alpha", "gamma"} {
		if _, err := s.Reserve(ctx, n); err != nil {
			t.Fatal(err)
		}
	}
	// Noise the store should ignore: an unlabelled Secret, and a Secret with a
	// different managed-by, both in the same namespace.
	mustCreate(t, client, &corev1.Secret{
		ObjectMeta: metav1.ObjectMeta{Name: "unrelated", Namespace: ns},
	})
	mustCreate(t, client, &corev1.Secret{
		ObjectMeta: metav1.ObjectMeta{
			Name:      "klotho-state-impostor",
			Namespace: ns,
			Labels:    map[string]string{ManagedByLabel: "someone-else"},
		},
	})

	names, err := s.List(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if want := "alpha,beta,gamma"; strings.Join(names, ",") != want {
		t.Errorf("List = %v, want %v", names, want)
	}
}

// TestSaveRetriesOnConflict drives the optimistic-concurrency path: the first
// Update is rejected with a conflict (as the API server does when the
// resourceVersion is stale), and Save must re-read and retry rather than lose
// the write.
func TestSaveRetriesOnConflict(t *testing.T) {
	s, client := newStore(t)
	ctx := context.Background()
	if _, err := s.Reserve(ctx, "demo"); err != nil {
		t.Fatal(err)
	}

	var updates int
	client.PrependReactor("update", "secrets", func(action k8stesting.Action) (bool, runtime.Object, error) {
		updates++
		if updates == 1 {
			name := action.(k8stesting.UpdateAction).GetObject().(*corev1.Secret).Name
			return true, nil, apierrors.NewConflict(
				schema.GroupResource{Resource: "secrets"}, name, errors.New("resourceVersion is stale"))
		}
		return false, nil, nil // let the tracker apply the retry
	})

	record := []byte("after-conflict")
	if err := s.Save(ctx, "demo", record); err != nil {
		t.Fatalf("Save = %v; want it to retry past the conflict", err)
	}
	if updates < 2 {
		t.Fatalf("Update attempts = %d; want the conflict to have forced a retry", updates)
	}
	if got, _ := s.Load(ctx, "demo"); !bytes.Equal(got, record) {
		t.Errorf("Load = %q, want %q", got, record)
	}
}

// TestSaveGivesUpAfterPersistentConflict confirms a conflict that never clears
// is surfaced rather than looped forever.
func TestSaveGivesUpAfterPersistentConflict(t *testing.T) {
	s, client := newStore(t)
	ctx := context.Background()
	if _, err := s.Reserve(ctx, "demo"); err != nil {
		t.Fatal(err)
	}
	client.PrependReactor("update", "secrets", func(action k8stesting.Action) (bool, runtime.Object, error) {
		name := action.(k8stesting.UpdateAction).GetObject().(*corev1.Secret).Name
		return true, nil, apierrors.NewConflict(
			schema.GroupResource{Resource: "secrets"}, name, errors.New("always stale"))
	})
	if err := s.Save(ctx, "demo", []byte("x")); !apierrors.IsConflict(err) {
		t.Fatalf("Save = %v; want a conflict error after exhausting retries", err)
	}
}

func TestInvalidNamesRejectedWithoutRequests(t *testing.T) {
	s, client := newStore(t)
	ctx := context.Background()
	var sawRequest bool
	client.PrependReactor("*", "*", func(k8stesting.Action) (bool, runtime.Object, error) {
		sawRequest = true
		return false, nil, nil
	})
	for _, name := range []string{"", ".", "..", "a/b", "-a", "a_b", "Demo", "a..b", strings.Repeat("a", 64)} {
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
	if sawRequest {
		t.Error("a request was sent for an invalid name")
	}
}

func TestNewValidation(t *testing.T) {
	ctx := context.Background()
	if _, err := New(ctx, Config{Namespace: ns}, nil, WithClient(fake.NewClientset())); err == nil {
		t.Error("nil sealer accepted")
	}
	if _, err := New(ctx, Config{Namespace: "Bad_NS"}, fakeSealer{}, WithClient(fake.NewClientset())); err == nil {
		t.Error("invalid namespace accepted")
	}
	// Empty namespace defaults to DefaultNamespace.
	s, err := New(ctx, Config{}, fakeSealer{}, WithClient(fake.NewClientset()))
	if err != nil {
		t.Fatal(err)
	}
	if s.Name() != "kubernetes" || s.Namespace() != DefaultNamespace {
		t.Errorf("defaults: name=%q namespace=%q", s.Name(), s.Namespace())
	}
}

func mustCreate(t *testing.T, client *fake.Clientset, sec *corev1.Secret) {
	t.Helper()
	if _, err := client.CoreV1().Secrets(sec.Namespace).Create(context.Background(), sec, metav1.CreateOptions{}); err != nil {
		t.Fatal(err)
	}
}
