package azureblob

import (
	"bytes"
	"context"
	"fmt"
	"net"
	"os"
	"os/exec"
	"strings"
	"testing"
	"time"

	"github.com/klotho-k8s/klotho/pkg/cluster"
	"github.com/klotho-k8s/klotho/provider/backend"

	"github.com/Azure/azure-sdk-for-go/sdk/storage/azblob/container"
)

// The end-to-end test boots the Azurite storage emulator in a podman container
// and drives the real Azure Blob client against it. It runs only when
// KLOTHO_E2E_AZURITE=1:
//
//	KLOTHO_E2E_AZURITE=1 go test ./provider/backend/azureblob/ -run E2E -v
//
// The container is removed again when the test ends, pass or fail. The image is
// left behind; remove it with "podman rmi" when it is no longer wanted.
//
// KLOTHO_E2E_AZURITE_IMAGE and KLOTHO_E2E_AZURITE_PORT override the image and
// the loopback blob port.
const (
	e2eGate          = "KLOTHO_E2E_AZURITE"
	e2ePortEnv       = "KLOTHO_E2E_AZURITE_PORT"
	e2eImageEnv      = "KLOTHO_E2E_AZURITE_IMAGE"
	e2eContainerName = "klotho-e2e-azurite"
	e2eImage         = "mcr.microsoft.com/azure-storage/azurite:latest"
	// Azurite's well-known development account. These are public emulator
	// credentials, not a secret: see the Azurite documentation.
	e2eAccount = "devstoreaccount1"
	e2eKey     = "Eby8vdM02xNOcqFlqUwJPLlmEtlCDXJ1OUzFT50uSRZ6IFsuFq2UVErCz4I6tq/K1SZFPTOtr/KBHBeksoGMGw=="
	e2eBlob    = "klotho-e2e"
)

// seal is a tiny Sealer for the e2e test; the store treats the bytes as opaque.
type sealer struct{}

func (sealer) Seal(s *cluster.State) ([]byte, error) {
	return []byte("reservation:" + s.Spec.Name), nil
}

func podman(t *testing.T, args ...string) (string, error) {
	t.Helper()
	cmd := exec.Command("podman", args...)
	out, err := cmd.CombinedOutput()
	return strings.TrimSpace(string(out)), err
}

// freePort returns the port named by KLOTHO_E2E_AZURITE_PORT, or a loopback
// port that is free right now.
func freePort(t *testing.T) string {
	t.Helper()
	if p := os.Getenv(e2ePortEnv); p != "" {
		return p
	}
	l, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = l.Close() }()
	_, port, _ := net.SplitHostPort(l.Addr().String())
	return port
}

// startAzurite runs Azurite on a loopback port and returns the blob endpoint.
func startAzurite(t *testing.T) string {
	t.Helper()
	if _, err := exec.LookPath("podman"); err != nil {
		t.Fatalf("%s is set but podman is not on PATH: %v", e2eGate, err)
	}
	if out, err := podman(t, "info", "--format", "{{.Host.Arch}}"); err != nil {
		t.Fatalf("%s is set but podman is not usable: %v\n%s", e2eGate, err, out)
	}
	_, _ = podman(t, "rm", "-f", "-v", e2eContainerName)
	t.Cleanup(func() {
		if out, err := podman(t, "rm", "-f", "-v", e2eContainerName); err != nil {
			t.Errorf("podman rm -f -v %s: %v\n%s", e2eContainerName, err, out)
		}
	})
	image := e2eImage
	if v := os.Getenv(e2eImageEnv); v != "" {
		image = v
	}
	port := freePort(t)
	// The official Azurite image already binds the blob service on 0.0.0.0.
	out, err := podman(t, "run", "-d", "--name", e2eContainerName,
		"-p", "127.0.0.1:"+port+":10000", image)
	if err != nil {
		t.Fatalf("podman run: %v\n%s", err, out)
	}
	return "http://127.0.0.1:" + port + "/" + e2eAccount
}

func TestE2EAzurite(t *testing.T) {
	if os.Getenv(e2eGate) != "1" {
		t.Skipf("set %s=1 to run the Azurite end-to-end test", e2eGate)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Minute)
	defer cancel()

	start := time.Now()
	endpoint := startAzurite(t)
	connStr := fmt.Sprintf(
		"DefaultEndpointsProtocol=http;AccountName=%s;AccountKey=%s;BlobEndpoint=%s;",
		e2eAccount, e2eKey, endpoint)

	// Create the container, retrying until Azurite answers.
	cc, err := container.NewClientFromConnectionString(connStr, e2eBlob, nil)
	if err != nil {
		t.Fatal(err)
	}
	deadline := time.Now().Add(90 * time.Second)
	for {
		if _, err = cc.Create(ctx, nil); err == nil {
			break
		}
		if time.Now().After(deadline) {
			logs, _ := podman(t, "logs", e2eContainerName)
			t.Fatalf("Azurite at %s did not accept a container create: %v\n%s", endpoint, err, logs)
		}
		time.Sleep(500 * time.Millisecond)
	}
	t.Logf("Azurite ready at %s after %s", endpoint, time.Since(start).Round(time.Millisecond))

	s, err := New(ctx, Config{
		Endpoint:         endpoint,
		AccountName:      e2eAccount,
		Container:        e2eBlob,
		Prefix:           "clusters",
		ConnectionString: connStr,
	}, sealer{})
	if err != nil {
		t.Fatal(err)
	}

	// Reserve is atomic: the first wins, a second loses without error.
	if ok, err := s.Reserve(ctx, "demo"); err != nil || !ok {
		t.Fatalf("first Reserve = %v, %v; want true, nil", ok, err)
	}
	if ok, err := s.Reserve(ctx, "demo"); err != nil || ok {
		t.Fatalf("second Reserve = %v, %v; want false, nil", ok, err)
	}

	record := []byte("AGE-ENCRYPTED-OPAQUE-RECORD\x00\x01\x02")
	if err := s.Save(ctx, "demo", record); err != nil {
		t.Fatalf("Save: %v", err)
	}
	got, err := s.Load(ctx, "demo")
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if !bytes.Equal(got, record) {
		t.Errorf("Load = %q, want %q", got, record)
	}

	if _, err := s.Reserve(ctx, "other"); err != nil {
		t.Fatal(err)
	}
	names, err := s.List(ctx)
	if err != nil {
		t.Fatalf("List: %v", err)
	}
	if strings.Join(names, ",") != "demo,other" {
		t.Errorf("List = %v, want [demo other]", names)
	}

	if err := s.Delete(ctx, "demo"); err != nil {
		t.Fatalf("Delete: %v", err)
	}
	if _, err := s.Load(ctx, "demo"); err == nil {
		t.Error("Load after Delete succeeded; want not found")
	}
	if err := s.Delete(ctx, "demo"); err == nil {
		t.Error("second Delete succeeded; want backend.ErrNotFound")
	} else if want := backend.ErrNotFound; !strings.Contains(err.Error(), want.Error()) {
		t.Errorf("second Delete = %v; want %v", err, want)
	}

	fmt.Fprintf(os.Stderr, "e2e: reserved, saved, loaded, listed and deleted in %s\n", time.Since(start).Round(time.Millisecond))
}
