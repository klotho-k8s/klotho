package cli

import (
	"context"
	"encoding/base64"
	"errors"
	"os"
	"path/filepath"
	"testing"

	"github.com/klotho-k8s/klotho"
	"github.com/klotho-k8s/klotho/pkg/cluster"
	"github.com/klotho-k8s/klotho/pkg/config"
	filebackend "github.com/klotho-k8s/klotho/provider/backend/file"
)

// fakeProbe builds an agentProbe with fixed answers.
func fakeProbe(installed, reachable bool) agentProbe {
	return agentProbe{
		serviceInstalled:  func(context.Context, string) (bool, error) { return installed, nil },
		endpointReachable: func(context.Context, string) bool { return reachable },
	}
}

func TestResolveHyperVAgentDefaults(t *testing.T) {
	// Nothing configured: endpoint defaults, CA read from the default file via
	// the injected reader, token taken from config, service seen as installed.
	h := config.Hyperv{AgentToken: "sekret"}
	readFile := func(path string) ([]byte, error) {
		if path == DefaultAgentCAFile {
			return []byte("CA-PEM"), nil
		}
		return nil, os.ErrNotExist
	}
	res, err := resolveHyperVAgent(context.Background(), h, fakeProbe(true, true), readFile)
	if err != nil {
		t.Fatal(err)
	}
	if res.Endpoint != DefaultAgentEndpoint {
		t.Errorf("endpoint = %q, want default", res.Endpoint)
	}
	if res.CA != "CA-PEM" {
		t.Errorf("ca = %q", res.CA)
	}
	if res.Token != "sekret" {
		t.Errorf("token = %q", res.Token)
	}
	if !res.Installed || !res.Reachable {
		t.Errorf("installed=%v reachable=%v", res.Installed, res.Reachable)
	}
}

func TestResolveHyperVAgentConfigWins(t *testing.T) {
	dir := t.TempDir()
	caFile := filepath.Join(dir, "ca.crt")
	tokFile := filepath.Join(dir, "tok")
	if err := os.WriteFile(caFile, []byte("FILE-CA"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(tokFile, []byte("  file-tok\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	h := config.Hyperv{
		AgentEndpoint:  "10.0.0.9:7443",
		AgentCAFile:    caFile,
		AgentTokenFile: tokFile,
	}
	res, err := resolveHyperVAgent(context.Background(), h, fakeProbe(false, false), os.ReadFile)
	if err != nil {
		t.Fatal(err)
	}
	if res.Endpoint != "10.0.0.9:7443" {
		t.Errorf("endpoint = %q", res.Endpoint)
	}
	if res.CA != "FILE-CA" {
		t.Errorf("ca = %q", res.CA)
	}
	if res.Token != "file-tok" { // trimmed
		t.Errorf("token = %q", res.Token)
	}
	if res.Installed || res.Reachable {
		t.Errorf("expected not installed/reachable")
	}
}

func TestProviderSecretDataHyperv(t *testing.T) {
	a := &app{cfg: &config.Config{Hyperv: config.Hyperv{
		AgentEndpoint: "172.30.32.1:7443",
		AgentCA:       "CA",
		AgentToken:    "T",
	}}}
	got := a.providerSecretData(providerHyperv)
	want := map[string]string{
		"HYPERV_AGENT_ENDPOINT": "172.30.32.1:7443",
		// base64-encoded so the operator's ${VAR} substitution carries the
		// multi-line PEM intact; the controller decodes it.
		"HYPERV_AGENT_CA":    base64.StdEncoding.EncodeToString([]byte("CA")),
		"HYPERV_AGENT_TOKEN": "T",
	}
	if len(got) != len(want) {
		t.Fatalf("got %v", got)
	}
	for k, v := range want {
		if got[k] != v {
			t.Errorf("%s = %q, want %q", k, got[k], v)
		}
	}
}

func TestProviderSecretDataHypervEmpty(t *testing.T) {
	a := &app{cfg: &config.Config{}}
	if got := a.providerSecretData(providerHyperv); got != nil {
		t.Errorf("want nil when nothing configured, got %v", got)
	}
}

func TestCreateAcceptsHyperv(t *testing.T) {
	// The guard must admit hyperv; it fails later (no agent / not Windows),
	// but never with ErrNotImplemented for the provider name.
	isolate(t)
	run(t, "", "init")
	r := run(t, "", "create", "demo", "--provider", "hyperv", "--no-install-agent")
	if errors.Is(r.runErr, ErrNotImplemented) {
		t.Fatalf("hyperv should be an accepted provider, got %v", r.runErr)
	}
}

// TestCountHyperVClusters checks the count that drives the delete-time note
// and the uninstall safety check: only non-reserved hyperv clusters count.
func TestCountHyperVClusters(t *testing.T) {
	isolate(t)
	if r := run(t, "", "init"); r.runErr != nil {
		t.Fatalf("init: %v\n%s", r.runErr, r.err)
	}
	cfg, err := config.Load("", "")
	if err != nil {
		t.Fatal(err)
	}
	codec, err := cfg.Codec()
	if err != nil {
		t.Fatal(err)
	}
	store, err := filebackend.New(cfg.RecordsDir(), codec)
	if err != nil {
		t.Fatal(err)
	}
	deps := klotho.Deps{Store: store, Codec: codec}

	save := func(name, provider string) {
		t.Helper()
		if ok, err := store.Reserve(t.Context(), name); err != nil || !ok {
			t.Fatalf("reserve %s: %v %v", name, ok, err)
		}
		if provider == "" { // leave as a bare reservation
			return
		}
		rec, err := codec.Seal(&cluster.State{Spec: cluster.Spec{Name: name, Provider: provider}})
		if err != nil {
			t.Fatal(err)
		}
		if err := store.Save(t.Context(), name, rec); err != nil {
			t.Fatal(err)
		}
	}
	save("hv1", providerHyperv)
	save("hv2", providerHyperv)
	save("px1", providerProxmox)
	save("pending", "") // reserved, no provider yet

	n, err := countHyperVClusters(t.Context(), deps)
	if err != nil {
		t.Fatal(err)
	}
	if n != 2 {
		t.Fatalf("countHyperVClusters = %d, want 2", n)
	}
}

func TestMintTokenUnique(t *testing.T) {
	a, err := mintToken()
	if err != nil {
		t.Fatal(err)
	}
	b, err := mintToken()
	if err != nil {
		t.Fatal(err)
	}
	if a == b || len(a) != 64 {
		t.Errorf("tokens not unique/sized: %q %q", a, b)
	}
}
