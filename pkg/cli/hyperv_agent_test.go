package cli

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"testing"

	"github.com/klotho-k8s/klotho/pkg/config"
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
		"HYPERV_AGENT_CA":       "CA",
		"HYPERV_AGENT_TOKEN":    "T",
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
