package cli

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/urfave/cli/v3"
	"k8s.io/client-go/tools/clientcmd"

	"github.com/klotho-k8s/klotho/pkg/cluster"
	"github.com/klotho-k8s/klotho/pkg/config"
	"github.com/klotho-k8s/klotho/pkg/credential"
	filebackend "github.com/klotho-k8s/klotho/provider/backend/file"
	"github.com/klotho-k8s/klotho/provider/controlplane/kubeadm"
)

// isolate points configuration and state at a temp dir.
func isolate(t *testing.T) string {
	t.Helper()
	dir := t.TempDir()
	t.Setenv("XDG_CONFIG_HOME", filepath.Join(dir, "config"))
	t.Setenv("XDG_STATE_HOME", filepath.Join(dir, "state"))
	for _, e := range []string{"KLOTHO_BACKEND", "KLOTHO_CONFIG", "KLOTHO_PROFILE",
		config.EnvStateDir, config.EnvIdentityFile, config.EnvRecipients,
		config.EnvProxmoxURL, config.EnvProxmoxTokenID, config.EnvProxmoxSecret, config.EnvProxmoxInsecure,
		config.EnvLinodeToken} {
		t.Setenv(e, "")
	}
	return dir
}

type result struct {
	out, err string
	exit     int
	runErr   error
}

func run(t *testing.T, input string, args ...string) result {
	t.Helper()
	var out, errOut bytes.Buffer
	root := New(BuildInfo{Version: "test"})
	root.Writer = &out
	root.ErrWriter = &errOut
	root.Reader = strings.NewReader(input)
	r := result{}
	root.ExitErrHandler = func(_ context.Context, _ *cli.Command, err error) {
		var ec cli.ExitCoder
		if errors.As(err, &ec) {
			r.exit = ec.ExitCode()
		}
	}
	r.runErr = root.Run(t.Context(), append([]string{"klotho"}, args...))
	r.out, r.err = out.String(), errOut.String()
	return r
}

// fixture runs "klotho init" and stores a provisioned cluster named name
// (with a real CA from the kubeadm provider) and a reservation named
// "pending" in the file backend, through the library packages.
func fixture(t *testing.T, name string) *cluster.State {
	t.Helper()
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
	spec := cluster.Spec{Name: name, Provider: "proxmox", ControlPlane: "kubeadm"}
	ep := cluster.Endpoint{Host: "127.0.0.1", Port: 1}
	creds, err := kubeadm.New(kubeadm.Options{}).Prepare(t.Context(), &spec, ep)
	if err != nil {
		t.Fatal(err)
	}
	st := &cluster.State{
		Spec: spec, Endpoint: ep, CA: creds.CA,
		CreatedAt: time.Now().Add(-2 * time.Hour).UTC(),
		Bootstrap: &cluster.Node{ID: "proxmox://x", Name: name + "-bootstrap"},
	}
	for _, n := range []string{name, "pending"} {
		if ok, err := store.Reserve(t.Context(), n); err != nil || !ok {
			t.Fatalf("reserve %s: %v %v", n, ok, err)
		}
	}
	rec, err := codec.Seal(st)
	if err != nil {
		t.Fatal(err)
	}
	if err := store.Save(t.Context(), name, rec); err != nil {
		t.Fatal(err)
	}
	return st
}

func TestVersionCommand(t *testing.T) {
	r := run(t, "", "version")
	if r.runErr != nil {
		t.Fatal(r.runErr)
	}
	info := BuildInfo{Version: "v1.2.3", Commit: "abc1234", Date: "2020-01-01"}
	var out bytes.Buffer
	root := New(info)
	root.Writer = &out
	if err := root.Run(t.Context(), []string{"klotho", "version"}); err != nil {
		t.Fatalf("run version: %v", err)
	}
	want := "klotho v1.2.3 (abc1234, 2020-01-01)"
	if !strings.Contains(out.String(), want) {
		t.Fatalf("output %q does not contain %q", out.String(), want)
	}
}

func TestClusterNameIsRequired(t *testing.T) {
	isolate(t)
	for _, name := range []string{"create", "delete", "shell", "state", "credential"} {
		t.Run(name, func(t *testing.T) {
			args := []string{name}
			if name == "create" {
				args = append(args, "--provider", "proxmox")
			}
			r := run(t, "", args...)
			if r.runErr == nil || !strings.Contains(r.runErr.Error(), "cluster name") {
				t.Fatalf("want a missing-name error, got %v", r.runErr)
			}
		})
	}
}

func TestInvalidClusterName(t *testing.T) {
	isolate(t)
	r := run(t, "", "state", "Not_Valid")
	if r.runErr == nil || !strings.Contains(r.runErr.Error(), "invalid cluster name") {
		t.Fatalf("got %v", r.runErr)
	}
}

func TestCredentialIsHidden(t *testing.T) {
	r := run(t, "", "--help")
	if r.runErr != nil {
		t.Fatalf("help: %v", r.runErr)
	}
	if strings.Contains(r.out, "credential") {
		t.Fatalf("credential should be hidden from help:\n%s", r.out)
	}
	for _, visible := range []string{"init", "create", "delete", "list", "shell", "state", "version", "--config", "--profile"} {
		if !strings.Contains(r.out, visible) {
			t.Fatalf("help should list %q:\n%s", visible, r.out)
		}
	}
}

func TestInit(t *testing.T) {
	dir := isolate(t)
	r := run(t, "", "init")
	if r.runErr != nil {
		t.Fatal(r.runErr)
	}
	cfgPath := filepath.Join(dir, "config", "klotho", "config.yaml")
	idPath := filepath.Join(dir, "config", "klotho", "identity.txt")
	for _, p := range []string{cfgPath, idPath} {
		if _, err := os.Stat(p); err != nil {
			t.Fatalf("%s: %v", p, err)
		}
	}
	if !strings.Contains(r.out, "public key: age1") || !strings.Contains(r.out, cfgPath) || !strings.Contains(r.out, "backend:    file") {
		t.Fatalf("output:\n%s", r.out)
	}
	again := run(t, "", "init")
	if again.runErr != nil {
		t.Fatal(again.runErr)
	}
	key := func(s string) string { _, k, _ := strings.Cut(s, "public key: "); return k }
	if key(r.out) != key(again.out) || !strings.Contains(again.out, "(existing)") {
		t.Fatalf("init is not idempotent:\n%s\n%s", r.out, again.out)
	}
}

func TestListWithoutInit(t *testing.T) {
	isolate(t)
	r := run(t, "", "list")
	if r.runErr == nil || !strings.Contains(r.runErr.Error(), "klotho init") {
		t.Fatalf("want a hint to run init, got %v", r.runErr)
	}
}

func TestList(t *testing.T) {
	isolate(t)
	fixture(t, "demo")
	r := run(t, "", "list")
	if r.runErr != nil {
		t.Fatal(r.runErr)
	}
	lines := strings.Split(strings.TrimSpace(r.out), "\n")
	if len(lines) != 3 || !strings.HasPrefix(lines[0], "NAME") {
		t.Fatalf("table:\n%s", r.out)
	}
	if f := strings.Fields(lines[1]); f[0] != "demo" || f[1] != "proxmox" || f[3] != "127.0.0.1:1" || f[4] != "2h" || f[5] != "false" {
		t.Fatalf("demo row: %v", f)
	}
	if f := strings.Fields(lines[2]); f[0] != "pending" || f[len(f)-1] != "true" {
		t.Fatalf("pending row: %v", f)
	}

	r = run(t, "", "list", "-o", "json", "--nodes")
	if r.runErr != nil {
		t.Fatal(r.runErr)
	}
	var rows []map[string]any
	if err := json.Unmarshal([]byte(r.out), &rows); err != nil {
		t.Fatalf("%v\n%s", err, r.out)
	}
	if len(rows) != 2 || rows[0]["name"] != "demo" {
		t.Fatalf("rows: %v", rows)
	}
	nodes, ok := rows[0]["nodes"].(map[string]any)
	if !ok || nodes["error"] == "" {
		t.Fatalf("demo is unreachable, want a node error: %v", rows[0])
	}
	if _, ok := rows[1]["nodes"]; ok {
		t.Fatalf("a reservation is not probed: %v", rows[1])
	}
}

func TestStateRedacts(t *testing.T) {
	isolate(t)
	fixture(t, "demo")
	r := run(t, "", "state", "demo")
	if r.runErr != nil {
		t.Fatal(r.runErr)
	}
	if !strings.Contains(r.out, "key: "+redacted) || strings.Contains(r.out, "PRIVATE KEY") {
		t.Fatalf("CA key not redacted:\n%s", r.out)
	}
	if !strings.Contains(r.out, "BEGIN CERTIFICATE") || !strings.Contains(r.out, "name: demo") {
		t.Fatalf("state output:\n%s", r.out)
	}
	r = run(t, "", "state", "demo", "-o", "json", "--show-secrets")
	if r.runErr != nil {
		t.Fatal(r.runErr)
	}
	var m map[string]any
	if err := json.Unmarshal([]byte(r.out), &m); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(m["ca"].(map[string]any)["key"].(string), "PRIVATE KEY") {
		t.Fatalf("--show-secrets did not show the key")
	}
}

func TestCredential(t *testing.T) {
	isolate(t)
	st := fixture(t, "demo")
	cfgPath := config.DefaultPath()
	// Flags after the name, as the shell kubeconfig passes them.
	r := run(t, "", "credential", "demo", "--config", cfgPath)
	if r.runErr != nil {
		t.Fatalf("%v\n%s", r.runErr, r.err)
	}
	var ec map[string]any
	if err := json.Unmarshal([]byte(r.out), &ec); err != nil {
		t.Fatalf("not JSON: %v\n%s", err, r.out)
	}
	if ec["apiVersion"] != credential.ExecAPIVersion || ec["kind"] != "ExecCredential" {
		t.Fatalf("header: %v", ec)
	}
	cert, _, expiry, err := credential.ParseExecCredential([]byte(r.out))
	if err != nil {
		t.Fatal(err)
	}
	if err := credential.Verify(cert, st.CA.Cert); err != nil {
		t.Fatalf("cert does not verify: %v", err)
	}
	if d := time.Until(expiry); d < 55*time.Minute || d > 61*time.Minute {
		t.Fatalf("expiry in %s", d)
	}
	cfg, _ := config.Load("", "")
	if _, err := os.Stat(filepath.Join(cfg.CredentialsDir(), "demo.json")); err != nil {
		t.Fatalf("not cached: %v", err)
	}
	again := run(t, "", "credential", "demo")
	if again.out != r.out {
		t.Fatal("cached credential not reused")
	}

	if r := run(t, "", "credential", "pending"); r.runErr == nil || !strings.Contains(r.runErr.Error(), "reserved") {
		t.Fatalf("reservation: %v", r.runErr)
	}
	if r := run(t, "", "credential", "missing"); r.runErr == nil {
		t.Fatal("missing cluster gave a credential")
	}
}

func TestCredentialCacheRejectsOtherCA(t *testing.T) {
	isolate(t)
	st := fixture(t, "demo")
	dir := t.TempDir()
	first, err := execCredential(dir, st, time.Now())
	if err != nil {
		t.Fatal(err)
	}
	// Same name, new CA (the cluster was recreated): the cache must not be
	// reused.
	creds, err := kubeadm.New(kubeadm.Options{}).Prepare(t.Context(), &st.Spec, st.Endpoint)
	if err != nil {
		t.Fatal(err)
	}
	st2 := *st
	st2.CA = creds.CA
	second, err := execCredential(dir, &st2, time.Now())
	if err != nil {
		t.Fatal(err)
	}
	if bytes.Equal(first, second) {
		t.Fatal("credential for a different CA was reused")
	}
	// Close to expiry: re-mint.
	third, err := execCredential(dir, &st2, time.Now().Add(58*time.Minute))
	if err != nil {
		t.Fatal(err)
	}
	if bytes.Equal(second, third) {
		t.Fatal("near-expiry credential was reused")
	}
}

func TestDeleteReservationWithoutProvider(t *testing.T) {
	isolate(t)
	fixture(t, "demo")
	r := run(t, "", "delete", "pending", "--yes")
	if r.runErr != nil {
		t.Fatal(r.runErr)
	}
	if r := run(t, "", "state", "pending"); r.runErr == nil {
		t.Fatal("reservation still there")
	}
	// A provisioned cluster needs the provider, which is not configured.
	r = run(t, "", "delete", "demo", "--yes")
	if r.runErr == nil || !strings.Contains(r.runErr.Error(), config.EnvProxmoxURL) {
		t.Fatalf("got %v", r.runErr)
	}
}

func TestCreateRejectsUnknownProviders(t *testing.T) {
	isolate(t)
	r := run(t, "", "create", "demo", "--provider", "openstack")
	if !errors.Is(r.runErr, ErrNotImplemented) {
		t.Fatalf("provider: %v", r.runErr)
	}
	r = run(t, "", "create", "demo", "--provider", "proxmox", "--control-plane", "k3s")
	if !errors.Is(r.runErr, ErrNotImplemented) {
		t.Fatalf("control plane: %v", r.runErr)
	}
	r = run(t, "", "create", "demo", "--provider", "proxmox", "--var", "novalue")
	if r.runErr == nil || !strings.Contains(r.runErr.Error(), "KEY=VALUE") {
		t.Fatalf("var: %v", r.runErr)
	}
}

func TestCreateAdmitsLinodeButNeedsToken(t *testing.T) {
	isolate(t)
	run(t, "", "init")
	// linode is a known provider (it passes the create guard), but with no
	// LINODE_TOKEN the provider cannot be built, so the error is about the
	// token, not ErrNotImplemented.
	r := run(t, "", "create", "demo", "--provider", "linode")
	if errors.Is(r.runErr, ErrNotImplemented) {
		t.Fatalf("linode should be an admitted provider, got %v", r.runErr)
	}
	if r.runErr == nil || !strings.Contains(r.runErr.Error(), "LINODE_TOKEN") {
		t.Fatalf("want a missing-token error, got %v", r.runErr)
	}
}

func TestCreateAdmitsGCPButNeedsProject(t *testing.T) {
	isolate(t)
	run(t, "", "init")
	// gcp is a known provider (it passes the create guard), but with no
	// gcp.project configured the provider cannot be built, so the error is
	// about the project, not ErrNotImplemented.
	r := run(t, "", "create", "demo", "--provider", "gcp")
	if errors.Is(r.runErr, ErrNotImplemented) {
		t.Fatalf("gcp should be an admitted provider, got %v", r.runErr)
	}
	if r.runErr == nil || !strings.Contains(r.runErr.Error(), "project") {
		t.Fatalf("want a missing-project error, got %v", r.runErr)
	}
}

func TestProviderSecretDataGCP(t *testing.T) {
	dir := t.TempDir()
	keyFile := filepath.Join(dir, "sa.json")
	if err := os.WriteFile(keyFile, []byte(`{"type":"service_account"}`), 0o600); err != nil {
		t.Fatal(err)
	}
	a := &app{cfg: &config.Config{GCP: config.GCP{CredentialsFile: keyFile}}}
	data := a.providerSecretData(providerGCP)
	want := base64.StdEncoding.EncodeToString([]byte(`{"type":"service_account"}`))
	if data["GCP_B64ENCODED_CREDENTIALS"] != want {
		t.Fatalf("providerSecretData(gcp) = %v, want GCP_B64ENCODED_CREDENTIALS=%s", data, want)
	}
	// With no credentials file, nothing is emitted.
	a2 := &app{cfg: &config.Config{}}
	if d := a2.providerSecretData(providerGCP); d != nil {
		t.Fatalf("providerSecretData(gcp) with no creds = %v, want nil", d)
	}
}

func TestProviderSecretDataLinode(t *testing.T) {
	a := &app{cfg: &config.Config{Linode: config.Linode{Token: "tok-xyz"}}}
	data := a.providerSecretData(providerLinode)
	if data["LINODE_TOKEN"] != "tok-xyz" {
		t.Fatalf("providerSecretData(linode) = %v, want LINODE_TOKEN=tok-xyz", data)
	}
	// With no token, nothing is emitted.
	a2 := &app{cfg: &config.Config{}}
	if d := a2.providerSecretData(providerLinode); d != nil {
		t.Fatalf("providerSecretData(linode) with no token = %v, want nil", d)
	}
}

func TestProviderSecretDataAzure(t *testing.T) {
	a := &app{cfg: &config.Config{Azure: config.Azure{
		SubscriptionID: "sub-1", TenantID: "tenant-1", ClientID: "client-1", ClientSecret: "shh",
	}}}
	data := a.providerSecretData(providerAzure)
	for k, want := range map[string]string{
		"AZURE_SUBSCRIPTION_ID": "sub-1",
		"AZURE_TENANT_ID":       "tenant-1",
		"AZURE_CLIENT_ID":       "client-1",
		"AZURE_CLIENT_SECRET":   "shh",
	} {
		if data[k] != want {
			t.Fatalf("providerSecretData(azure)[%s] = %q, want %q", k, data[k], want)
		}
	}
	// With nothing configured, nothing is emitted.
	a2 := &app{cfg: &config.Config{}}
	if d := a2.providerSecretData(providerAzure); d != nil {
		t.Fatalf("providerSecretData(azure) with no config = %v, want nil", d)
	}
}

func TestCreateAdmitsAzureButNeedsCredentials(t *testing.T) {
	isolate(t)
	run(t, "", "init")
	// azure is a known provider (it passes the create guard), but with no
	// service principal the provider cannot be built, so the error is about the
	// credentials, not ErrNotImplemented.
	r := run(t, "", "create", "demo", "--provider", "azure")
	if errors.Is(r.runErr, ErrNotImplemented) {
		t.Fatalf("azure should be an admitted provider, got %v", r.runErr)
	}
	if r.runErr == nil || !strings.Contains(r.runErr.Error(), "subscriptionId") {
		t.Fatalf("want a missing-credentials error, got %v", r.runErr)
	}
}

func TestCreateNeedsProxmoxSecret(t *testing.T) {
	isolate(t)
	run(t, "", "init")
	t.Setenv(config.EnvProxmoxURL, "https://pve:8006")
	t.Setenv(config.EnvProxmoxTokenID, "root@pam!k")
	r := run(t, "", "create", "demo", "--provider", "proxmox")
	if r.runErr == nil || !strings.Contains(r.runErr.Error(), config.EnvProxmoxSecret) {
		t.Fatalf("got %v", r.runErr)
	}
}

func TestShellPrint(t *testing.T) {
	isolate(t)
	fixture(t, "demo")
	r := run(t, "", "--profile", "", "shell", "demo", "--print")
	if r.runErr != nil {
		t.Fatal(r.runErr)
	}
	path, ok := strings.CutPrefix(strings.TrimSpace(r.out), "export KUBECONFIG=")
	if !ok {
		t.Fatalf("output: %q", r.out)
	}
	path = strings.Trim(path, "'")
	kc, err := clientcmd.LoadFromFile(path)
	if err != nil {
		t.Fatal(err)
	}
	ai := kc.AuthInfos[kc.Contexts[kc.CurrentContext].AuthInfo]
	if ai.Exec == nil || ai.Exec.Args[0] != "credential" || ai.Exec.Args[1] != "demo" {
		t.Fatalf("exec: %+v", ai.Exec)
	}
}

// TestHelperProcess is the command the shell tests run.
func TestHelperProcess(_ *testing.T) {
	if os.Getenv("KLOTHO_TEST_HELPER") != "1" {
		return
	}
	kc := os.Getenv("KUBECONFIG")
	if _, err := os.Stat(kc); err != nil || os.Getenv(EnvCluster) != "demo" {
		fmt.Println("bad environment")
		os.Exit(2)
	}
	fmt.Println("kubeconfig=" + kc)
	if os.Getenv("KLOTHO_TEST_EXIT") != "" {
		os.Exit(7)
	}
	os.Exit(0)
}

func TestShellRunsCommand(t *testing.T) {
	isolate(t)
	fixture(t, "demo")
	t.Setenv("KLOTHO_TEST_HELPER", "1")
	exe, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	r := run(t, "", "shell", "demo", "--", exe, "-test.run=^TestHelperProcess$")
	if r.runErr != nil {
		t.Fatalf("%v\n%s\n%s", r.runErr, r.out, r.err)
	}
	_, kc, ok := strings.Cut(r.out, "kubeconfig=")
	if !ok {
		t.Fatalf("output: %s", r.out)
	}
	kc = strings.TrimSpace(strings.Split(kc, "\n")[0])
	if _, err := os.Stat(kc); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("temporary kubeconfig %s not removed: %v", kc, err)
	}

	t.Setenv("KLOTHO_TEST_EXIT", "1")
	r = run(t, "", "shell", "demo", "--", exe, "-test.run=^TestHelperProcess$")
	if r.exit != 7 {
		t.Fatalf("exit code %d, err %v", r.exit, r.runErr)
	}
}

func TestInstallKubernetesFlagWinsOverConfig(t *testing.T) {
	for name, tc := range map[string]struct {
		args []string
		cfg  bool
		want bool
	}{
		"off by default":          {nil, false, false},
		"config turns it on":      {nil, true, true},
		"flag turns it on":        {[]string{"--install-kubernetes"}, false, true},
		"flag=false beats config": {[]string{"--install-kubernetes=false"}, true, false},
	} {
		t.Run(name, func(t *testing.T) {
			var got bool
			cmd := &cli.Command{
				Flags: []cli.Flag{&cli.BoolFlag{Name: "install-kubernetes"}},
				Action: func(_ context.Context, c *cli.Command) error {
					got = installKubernetes(c, &config.Config{Nodes: config.Nodes{InstallKubernetes: tc.cfg}})
					return nil
				},
			}
			if err := cmd.Run(t.Context(), append([]string{"create"}, tc.args...)); err != nil {
				t.Fatal(err)
			}
			if got != tc.want {
				t.Fatalf("got %v, want %v", got, tc.want)
			}
		})
	}
}

func TestCreateHelpListsInstallKubernetes(t *testing.T) {
	r := run(t, "", "create", "--help")
	if r.runErr != nil {
		t.Fatal(r.runErr)
	}
	if !strings.Contains(r.out, "--install-kubernetes") || !strings.Contains(r.out, "nodes.installKubernetes") {
		t.Fatalf("help:\n%s", r.out)
	}
}
