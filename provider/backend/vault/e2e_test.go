package vault

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"net"
	"os"
	"os/exec"
	"testing"
	"time"

	"github.com/klotho-k8s/klotho/provider/backend"
)

// The end-to-end test boots a dev-mode Vault and drives the real store against
// its KV v2 engine. It runs only when KLOTHO_E2E_VAULT=1 and a "vault" binary is
// on PATH (no container, so podman is never touched):
//
//	KLOTHO_E2E_VAULT=1 go test ./provider/backend/vault/ -run E2E -v
//
// A dev-mode Vault keeps everything in memory and is thrown away when the
// process ends, which the test does on cleanup. KLOTHO_E2E_VAULT_BIN overrides
// the binary and KLOTHO_E2E_VAULT_PORT the loopback port.
const (
	e2eGate    = "KLOTHO_E2E_VAULT"
	e2eBinEnv  = "KLOTHO_E2E_VAULT_BIN"
	e2ePortEnv = "KLOTHO_E2E_VAULT_PORT"
	e2eToken   = "klotho-e2e-root"
	e2eMount   = "secret" // the KV v2 engine a dev-mode Vault enables by default
)

// freePort returns the port named by KLOTHO_E2E_VAULT_PORT, or a loopback port
// that is free right now.
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

// startDevVault launches "vault server -dev" and waits for it to answer,
// returning its address. The server is killed on cleanup.
func startDevVault(t *testing.T) string {
	t.Helper()
	bin := os.Getenv(e2eBinEnv)
	if bin == "" {
		bin = "vault"
	}
	if _, err := exec.LookPath(bin); err != nil {
		t.Skipf("%s=1 but %q is not on PATH: %v", e2eGate, bin, err)
	}
	port := freePort(t)
	addr := "http://127.0.0.1:" + port

	ctx, cancel := context.WithCancel(context.Background())
	cmd := exec.CommandContext(ctx, bin, "server", "-dev",
		"-dev-root-token-id="+e2eToken,
		"-dev-listen-address=127.0.0.1:"+port,
	)
	cmd.Stdout, cmd.Stderr = os.Stderr, os.Stderr
	if err := cmd.Start(); err != nil {
		cancel()
		t.Fatalf("start dev vault: %v", err)
	}
	t.Cleanup(func() {
		cancel()
		_ = cmd.Wait()
	})

	// Wait for the listener to accept connections.
	deadline := time.Now().Add(20 * time.Second)
	for time.Now().Before(deadline) {
		c, err := net.DialTimeout("tcp", "127.0.0.1:"+port, 200*time.Millisecond)
		if err == nil {
			_ = c.Close()
			return addr
		}
		time.Sleep(100 * time.Millisecond)
	}
	t.Fatal("dev vault did not come up in time")
	return ""
}

func TestE2E(t *testing.T) {
	if os.Getenv(e2eGate) != "1" {
		t.Skipf("set %s=1 to run the dev-mode Vault end-to-end test", e2eGate)
	}
	addr := startDevVault(t)
	ctx := context.Background()

	s, err := New(Config{
		Address: addr,
		Token:   e2eToken,
		Mount:   e2eMount,
		Prefix:  "klotho-e2e",
	}, fakeSealer{})
	if err != nil {
		t.Fatalf("New: %v", err)
	}

	name := fmt.Sprintf("e2e-%d", time.Now().UnixNano())
	t.Cleanup(func() { _ = s.Delete(context.Background(), name) })

	// Reserve is atomic: the first wins, the second loses without error.
	ok, err := s.Reserve(ctx, name)
	if err != nil || !ok {
		t.Fatalf("first Reserve = %v, %v; want true, nil", ok, err)
	}
	ok, err = s.Reserve(ctx, name)
	if err != nil || ok {
		t.Fatalf("second Reserve = %v, %v; want false, nil", ok, err)
	}

	// Save then Load round-trips opaque bytes, including a null byte.
	record := []byte("AGE-ENCRYPTED\x00\x01\x02opaque")
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

	// A second Save (new version via cas) round-trips too.
	record2 := []byte("second-version\x00")
	if err := s.Save(ctx, name, record2); err != nil {
		t.Fatalf("second Save: %v", err)
	}
	if got, _ := s.Load(ctx, name); !bytes.Equal(got, record2) {
		t.Fatalf("Load after second Save = %q, want %q", got, record2)
	}

	// List finds it.
	names, err := s.List(ctx)
	if err != nil {
		t.Fatalf("List: %v", err)
	}
	if !contains(names, name) {
		t.Fatalf("List = %v; want it to contain %q", names, name)
	}

	// Delete removes it; a second Delete reports not found.
	if err := s.Delete(ctx, name); err != nil {
		t.Fatalf("Delete: %v", err)
	}
	if err := s.Delete(ctx, name); !errors.Is(err, backend.ErrNotFound) {
		t.Fatalf("second Delete = %v; want backend.ErrNotFound", err)
	}
	if _, err := s.Load(ctx, name); !errors.Is(err, backend.ErrNotFound) {
		t.Fatalf("Load after Delete = %v; want backend.ErrNotFound", err)
	}
}

func contains(s []string, v string) bool {
	for _, x := range s {
		if x == v {
			return true
		}
	}
	return false
}
