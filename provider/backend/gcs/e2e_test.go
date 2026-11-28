package gcs

import (
	"bytes"
	"context"
	"errors"
	"os"
	"testing"
	"time"

	"cloud.google.com/go/storage"

	"github.com/klotho-k8s/klotho/pkg/cluster"
	"github.com/klotho-k8s/klotho/provider/backend"
)

// The end-to-end test drives the real GCS client against a running
// fsouza/fake-gcs-server emulator. It runs only when KLOTHO_E2E_GCS=1, so a
// plain "go test ./..." needs no network and no emulator:
//
//	KLOTHO_E2E_GCS=1 go test ./provider/backend/gcs/ -run E2E -v
//
// Start the emulator yourself first (this test launches nothing and touches no
// container runtime), for example:
//
//	podman run --rm -p 4443:4443 fsouza/fake-gcs-server \
//	  -scheme http -backend memory -public-host localhost:4443
//
// KLOTHO_E2E_GCS_HOST overrides the emulator's host:port (default
// localhost:4443); it is exported as STORAGE_EMULATOR_HOST, which the storage
// SDK dials without credentials.
const (
	e2eGate        = "KLOTHO_E2E_GCS"
	e2eHostEnv     = "KLOTHO_E2E_GCS_HOST"
	e2eDefaultHost = "localhost:4443"
	e2eBucket      = "klotho-e2e"
	e2eProject     = "klotho-e2e"
)

// e2eSealer seals a reservation to deterministic opaque bytes.
type e2eSealer struct{}

func (e2eSealer) Seal(s *cluster.State) ([]byte, error) {
	return []byte("reservation:" + s.Spec.Name), nil
}

func TestE2E(t *testing.T) {
	if os.Getenv(e2eGate) != "1" {
		t.Skipf("set %s=1 to run the fake-gcs-server end-to-end test", e2eGate)
	}
	host := os.Getenv(e2eHostEnv)
	if host == "" {
		host = e2eDefaultHost
	}
	t.Setenv("STORAGE_EMULATOR_HOST", host)

	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	// Create the bucket the store expects; the emulator starts empty.
	client, err := storage.NewClient(ctx)
	if err != nil {
		t.Fatalf("emulator client: %v", err)
	}
	defer func() { _ = client.Close() }()
	if err := client.Bucket(e2eBucket).Create(ctx, e2eProject, nil); err != nil {
		// A rerun against a persistent backend may find it already there.
		t.Logf("create bucket (continuing): %v", err)
	}

	s, err := New(ctx, Config{Bucket: e2eBucket, Prefix: "clusters"}, e2eSealer{})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	defer func() { _ = s.Close() }()

	name := "e2e-demo"
	// Clean any leftover from a previous run so Reserve starts fresh.
	_ = s.Delete(ctx, name)

	ok, err := s.Reserve(ctx, name)
	if err != nil || !ok {
		t.Fatalf("Reserve = %v, %v; want true, nil", ok, err)
	}
	// A second Reserve loses on the DoesNotExist precondition.
	if ok, err := s.Reserve(ctx, name); err != nil || ok {
		t.Fatalf("second Reserve = %v, %v; want false, nil", ok, err)
	}

	record := []byte("AGE-ENCRYPTED-OPAQUE\x00\x01\x02")
	if err := s.Save(ctx, name, record); err != nil {
		t.Fatalf("Save: %v", err)
	}
	got, err := s.Load(ctx, name)
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if !bytes.Equal(got, record) {
		t.Fatalf("Load = %q, want %q", got, record)
	}

	names, err := s.List(ctx)
	if err != nil {
		t.Fatalf("List: %v", err)
	}
	found := false
	for _, n := range names {
		if n == name {
			found = true
		}
	}
	if !found {
		t.Fatalf("List = %v, want it to contain %q", names, name)
	}

	if err := s.Delete(ctx, name); err != nil {
		t.Fatalf("Delete: %v", err)
	}
	if err := s.Delete(ctx, name); !errors.Is(err, backend.ErrNotFound) {
		t.Fatalf("second Delete = %v; want ErrNotFound", err)
	}
	if _, err := s.Load(ctx, name); !errors.Is(err, backend.ErrNotFound) {
		t.Fatalf("Load after Delete = %v; want ErrNotFound", err)
	}
}
