package config

import (
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
	"time"

	"filippo.io/age"

	"github.com/klotho-k8s/klotho/pkg/cluster"
	"github.com/klotho-k8s/klotho/pkg/state"
)

// isolate points every XDG directory and klotho variable at a temp dir.
func isolate(t *testing.T) string {
	t.Helper()
	dir := t.TempDir()
	t.Setenv("XDG_CONFIG_HOME", filepath.Join(dir, "config"))
	t.Setenv("XDG_STATE_HOME", filepath.Join(dir, "state"))
	for _, e := range []string{EnvBackend, EnvStateDir, EnvIdentityFile, EnvRecipients,
		EnvProxmoxURL, EnvProxmoxTokenID, EnvProxmoxSecret, EnvProxmoxInsecure,
		EnvS3AccessKeyID, EnvS3SecretKey, EnvLinodeToken} {
		t.Setenv(e, "")
	}
	return dir
}

func writeConfig(t *testing.T, body string) string {
	t.Helper()
	p := DefaultPath()
	if err := os.MkdirAll(filepath.Dir(p), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(p, []byte(body), 0o600); err != nil {
		t.Fatal(err)
	}
	return p
}

func TestLoadMissingFileGivesDefaults(t *testing.T) {
	dir := isolate(t)
	c, err := Load("", "")
	if err != nil {
		t.Fatal(err)
	}
	if c.StateDir != filepath.Join(dir, "state", "klotho") {
		t.Errorf("stateDir = %s", c.StateDir)
	}
	if c.Encryption.IdentityFile != filepath.Join(dir, "config", "klotho", IdentityFileName) {
		t.Errorf("identityFile = %s", c.Encryption.IdentityFile)
	}
	if c.CAPI.Version != DefaultCAPIVersion || c.Providers["proxmox"].Version != DefaultProxmoxVersion {
		t.Errorf("versions: %s %s", c.CAPI.Version, c.Providers["proxmox"].Version)
	}
	if c.Providers[ProviderLinode].Version != DefaultLinodeVersion {
		t.Errorf("linode default version = %q, want %q", c.Providers[ProviderLinode].Version, DefaultLinodeVersion)
	}
	if c.Providers[ProviderGCP].Version != DefaultGCPVersion {
		t.Errorf("gcp default version = %q, want %q", c.Providers[ProviderGCP].Version, DefaultGCPVersion)
	}
	if c.EffectiveBackend() != BackendFile {
		t.Errorf("backend = %s", c.EffectiveBackend())
	}
}

func TestLinodeTokenFromEnvOnly(t *testing.T) {
	isolate(t)
	// A token set in the file must be ignored (the field is json:"-"); only the
	// environment supplies it.
	writeConfig(t, "linode:\n  region: us-ord\n  controlPlaneEndpoint: cp.example.com\n")
	t.Setenv(EnvLinodeToken, "tok-from-env")
	c, err := Load("", "")
	if err != nil {
		t.Fatal(err)
	}
	if c.Linode.Region != "us-ord" || c.Linode.ControlPlaneEndpoint != "cp.example.com" {
		t.Errorf("linode config not loaded: %+v", c.Linode)
	}
	if c.Linode.Token != "tok-from-env" {
		t.Errorf("token = %q, want the environment value", c.Linode.Token)
	}
}

func TestLinodeTokenInFileRejected(t *testing.T) {
	isolate(t)
	// A "token" key in the linode block is an unknown field (json:"-"), so Load
	// rejects it rather than silently storing a credential in the file.
	writeConfig(t, "linode:\n  region: us-ord\n  token: should-not-be-here\n")
	if _, err := Load("", ""); err == nil {
		t.Fatal("expected Load to reject a token key in the config file")
	}
}

const sample = `
backend: file
stateDir: $XDG_STATE_HOME/klotho-test
proxmox:
  url: https://pve:8006
  tokenId: root@pam!klotho
  templateId: 9000
  vmidRange: [100, 199]
capi:
  featureGates:
    SelfHostedDeletion: false
variables:
  CONTROL_PLANE_ENDPOINT_IP: 10.0.0.10
  BRIDGE: vmbr0
profiles:
  lab:
    proxmox:
      url: https://lab:8006
      node: pve2
    variables:
      CONTROL_PLANE_ENDPOINT_IP: 10.1.0.10
`

func TestLoadProfileAndEnv(t *testing.T) {
	dir := isolate(t)
	writeConfig(t, sample)
	t.Setenv(EnvProxmoxSecret, "s3cret")
	t.Setenv(EnvProxmoxTokenID, "ops@pve!k")

	c, err := Load("", "lab")
	if err != nil {
		t.Fatal(err)
	}
	if c.Proxmox.URL != "https://lab:8006" || c.Proxmox.Node != "pve2" {
		t.Errorf("profile not merged: %+v", c.Proxmox)
	}
	if c.Proxmox.TemplateID != 9000 || len(c.Proxmox.VMIDRange) != 2 {
		t.Errorf("top level lost: %+v", c.Proxmox)
	}
	if c.Proxmox.TokenID != "ops@pve!k" || c.Proxmox.Secret != "s3cret" {
		t.Errorf("env not applied: %+v", c.Proxmox)
	}
	if c.Variables["CONTROL_PLANE_ENDPOINT_IP"] != "10.1.0.10" || c.Variables["BRIDGE"] != "vmbr0" {
		t.Errorf("variables: %v", c.Variables)
	}
	if c.StateDir != filepath.Join(dir, "state", "klotho-test") {
		t.Errorf("stateDir expansion: %s", c.StateDir)
	}
	if gate, ok := c.CAPI.FeatureGates["SelfHostedDeletion"]; !ok || gate {
		t.Errorf("feature gates: %v", c.CAPI.FeatureGates)
	}
	if err := c.CheckProxmox(); err != nil {
		t.Errorf("CheckProxmox: %v", err)
	}

	// Without the profile the top level stands.
	c, err = Load("", "")
	if err != nil {
		t.Fatal(err)
	}
	if c.Proxmox.URL != "https://pve:8006" || c.Variables["CONTROL_PLANE_ENDPOINT_IP"] != "10.0.0.10" {
		t.Errorf("top level: %+v %v", c.Proxmox, c.Variables)
	}
}

func TestLoadErrors(t *testing.T) {
	isolate(t)
	writeConfig(t, sample)
	if _, err := Load("", "nope"); err == nil || !strings.Contains(err.Error(), "lab") {
		t.Errorf("unknown profile: %v", err)
	}
	writeConfig(t, "bogus: 1\n")
	if _, err := Load("", ""); err == nil {
		t.Error("unknown field accepted")
	}
	writeConfig(t, "encryption:\n  recipients: [notakey]\n")
	if _, err := Load("", ""); err == nil {
		t.Error("bad recipient accepted")
	}
	writeConfig(t, "backend: s3\n")
	if _, err := Load("", ""); err == nil {
		t.Error("unknown backend accepted")
	}
}

func TestPayloadS3(t *testing.T) {
	isolate(t)

	// No payload section means no payload host.
	c, err := Load("", "")
	if err != nil {
		t.Fatal(err)
	}
	if c.PayloadConfigured() || c.Payload.Kind != "" {
		t.Errorf("payload configured by default: %+v", c.Payload)
	}

	writeConfig(t, `
payload:
  kind: s3
  s3:
    endpoint: http://minio:9000
    region: eu-west-1
    bucket: klotho
    prefix: payload
    usePathStyle: true
    presignTTL: 30m
`)
	t.Setenv(EnvS3AccessKeyID, "AKIA")
	t.Setenv(EnvS3SecretKey, "s3cret")
	c, err = Load("", "")
	if err != nil {
		t.Fatal(err)
	}
	s := c.Payload.S3
	if !c.PayloadConfigured() || c.Payload.Kind != PayloadS3 {
		t.Errorf("kind = %q", c.Payload.Kind)
	}
	if s.Endpoint != "http://minio:9000" || s.Region != "eu-west-1" || s.Bucket != "klotho" || s.Prefix != "payload" || !s.UsePathStyle {
		t.Errorf("s3 = %+v", s)
	}
	if s.AccessKeyID != "AKIA" || s.SecretAccessKey != "s3cret" {
		t.Errorf("keys not taken from the environment: %+v", s)
	}
	if s.TTL() != 30*time.Minute {
		t.Errorf("TTL = %s", s.TTL())
	}
	if (S3{}).TTL() != DefaultPresignTTL {
		t.Errorf("default TTL = %s", (S3{}).TTL())
	}

	// The keys never come from the file.
	writeConfig(t, "payload:\n  kind: s3\n  s3:\n    bucket: b\n    accessKeyId: AKIA\n")
	if _, err := Load("", ""); err == nil {
		t.Error("access key in the file accepted")
	}

	for name, body := range map[string]string{
		"unknown kind":   "payload:\n  kind: ftp\n",
		"missing bucket": "payload:\n  kind: s3\n",
		"bad endpoint":   "payload:\n  kind: s3\n  s3:\n    bucket: b\n    endpoint: minio:9000\n",
		"bad ttl":        "payload:\n  kind: s3\n  s3:\n    bucket: b\n    presignTTL: soon\n",
		"zero ttl":       "payload:\n  kind: s3\n  s3:\n    bucket: b\n    presignTTL: 0s\n",
	} {
		writeConfig(t, body)
		if _, err := Load("", ""); err == nil || !strings.Contains(err.Error(), "payload") {
			t.Errorf("%s: err = %v", name, err)
		}
	}

	// Half a key pair is an error that names the other variable.
	writeConfig(t, "payload:\n  kind: s3\n  s3:\n    bucket: b\n")
	t.Setenv(EnvS3SecretKey, "")
	if _, err := Load("", ""); err == nil || !strings.Contains(err.Error(), EnvS3SecretKey) {
		t.Errorf("half a key pair: %v", err)
	}
	// No keys at all is fine: the AWS credential chain takes over.
	t.Setenv(EnvS3AccessKeyID, "")
	if _, err := Load("", ""); err != nil {
		t.Errorf("no keys: %v", err)
	}
}

func TestCheckProxmoxNamesEnvVar(t *testing.T) {
	isolate(t)
	writeConfig(t, "proxmox:\n  url: https://pve:8006\n  tokenId: a@pam!b\n")
	c, err := Load("", "")
	if err != nil {
		t.Fatal(err)
	}
	err = c.CheckProxmox()
	if err == nil || !strings.Contains(err.Error(), EnvProxmoxSecret) {
		t.Fatalf("want error naming %s, got %v", EnvProxmoxSecret, err)
	}
	if c.EffectiveBackend() != BackendProxmox {
		t.Errorf("backend with proxmox url = %s", c.EffectiveBackend())
	}
}

func TestCodecPlainAndEncrypted(t *testing.T) {
	isolate(t)
	s := &cluster.State{Spec: cluster.Spec{Name: "demo"}, CA: cluster.KeyPair{Cert: []byte("c"), Key: []byte("k")}}

	writeConfig(t, "encryption:\n  plain: true\n")
	c, err := Load("", "")
	if err != nil {
		t.Fatal(err)
	}
	codec, err := c.Codec()
	if err != nil {
		t.Fatal(err)
	}
	rec, err := codec.Seal(s)
	if err != nil || !state.IsPlainRecord(rec) {
		t.Fatalf("plain codec: %v %q", err, rec)
	}

	writeConfig(t, "")
	c, err = Load("", "")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := c.Codec(); err == nil || !strings.Contains(err.Error(), "klotho init") {
		t.Fatalf("want a missing-identity error, got %v", err)
	}
	if _, err := Init(c.Encryption.IdentityFile); err != nil {
		t.Fatal(err)
	}
	codec, err = c.Codec()
	if err != nil {
		t.Fatal(err)
	}
	rec, err = codec.Seal(s)
	if err != nil || state.IsPlainRecord(rec) || !state.IsRecord(rec) {
		t.Fatalf("encrypted codec: %v %q", err, rec)
	}
	got, err := codec.Open(rec)
	if err != nil || got.Spec.Name != "demo" {
		t.Fatalf("open: %v %+v", err, got)
	}

	// An extra recipient: records still open with our identity.
	other, err := state.GenerateIdentity()
	if err != nil {
		t.Fatal(err)
	}
	mine, _ := Init(c.Encryption.IdentityFile)
	t.Setenv(EnvRecipients, mine+", "+other.Recipient().String())
	c, err = Load("", "")
	if err != nil {
		t.Fatal(err)
	}
	if len(c.Encryption.Recipients) != 2 {
		t.Fatalf("recipients: %v", c.Encryption.Recipients)
	}
	codec, err = c.Codec()
	if err != nil {
		t.Fatal(err)
	}
	rec, _ = codec.Seal(s)
	if _, err := codec.Open(rec); err != nil {
		t.Fatal(err)
	}
	otherCodec, err := state.NewCodec(nil, []age.Identity{other})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := otherCodec.Open(rec); err != nil {
		t.Fatalf("second recipient cannot open: %v", err)
	}
}

func TestInitIdempotent(t *testing.T) {
	isolate(t)
	p := filepath.Join(Dir(), IdentityFileName)
	k1, err := Init(p)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.HasPrefix(k1, "age1") {
		t.Fatalf("public key %q", k1)
	}
	b1, _ := os.ReadFile(p)
	k2, err := Init(p)
	if err != nil {
		t.Fatal(err)
	}
	b2, _ := os.ReadFile(p)
	if k1 != k2 || string(b1) != string(b2) {
		t.Fatal("second Init changed the identity")
	}
	if runtime.GOOS != "windows" {
		fi, _ := os.Stat(p)
		if fi.Mode().Perm() != 0o600 {
			t.Fatalf("mode %v", fi.Mode().Perm())
		}
	}
	wrote, err := WriteSkeleton("")
	if err != nil || !wrote {
		t.Fatalf("skeleton: %v %v", wrote, err)
	}
	wrote, err = WriteSkeleton("")
	if err != nil || wrote {
		t.Fatalf("second skeleton: %v %v", wrote, err)
	}
	if _, err := Load("", ""); err != nil {
		t.Fatalf("skeleton does not load: %v", err)
	}
}

func TestExpand(t *testing.T) {
	isolate(t)
	t.Setenv("KLOTHO_TEST_DIR", "/x")
	if got := Expand("$KLOTHO_TEST_DIR/y"); got != filepath.Clean("/x/y") {
		t.Errorf("env: %s", got)
	}
	h, _ := os.UserHomeDir()
	if got := Expand("~/a"); got != filepath.Join(h, "a") {
		t.Errorf("tilde: %s", got)
	}
	if got := Expand("$XDG_CONFIG_HOME/klotho"); got != Dir() {
		t.Errorf("xdg: %s vs %s", got, Dir())
	}
}

func TestTemplateAndNodes(t *testing.T) {
	isolate(t)
	writeConfig(t, `
proxmox:
  templateId: 9000
  template:
    tags: [k8s, ubuntu-24.04]
nodes:
  installKubernetes: true
  cache:
    aptProxy: http://cache.lan:3142
    registryMirrors:
      registry.k8s.io: [http://cache.lan:5000]
`)
	c, err := Load("", "")
	if err != nil {
		t.Fatal(err)
	}
	tpl := c.Proxmox.Template
	if !tpl.Selects() || len(tpl.Tags) != 2 || tpl.MatchPolicy != "exact" {
		t.Errorf("template: %+v", tpl)
	}
	if c.Proxmox.TemplateID != 9000 {
		t.Errorf("templateId alongside tags must load: %+v", c.Proxmox)
	}
	if !c.Nodes.InstallKubernetes {
		t.Error("installKubernetes not loaded")
	}
	opts := c.Nodes.InstallOptions()
	if opts.AptProxy != "http://cache.lan:3142" || opts.RegistryMirrors["registry.k8s.io"][0] != "http://cache.lan:5000" {
		t.Errorf("install options: %+v", opts)
	}

	// Without tags nothing is defaulted.
	writeConfig(t, "proxmox:\n  templateId: 9000\n")
	if c, err = Load("", ""); err != nil || c.Proxmox.Template.Selects() || c.Proxmox.Template.MatchPolicy != "" || c.Nodes.InstallKubernetes {
		t.Errorf("defaults: %+v %+v %v", c.Proxmox.Template, c.Nodes, err)
	}

	for name, tc := range map[string]struct{ body, want string }{
		"bad tag":             {"proxmox:\n  template:\n    tags: [\"a b\"]\n", `"a b" is not a Proxmox tag`},
		"bad policy":          {"proxmox:\n  template:\n    tags: [k8s]\n    matchPolicy: fuzzy\n", "matchPolicy"},
		"policy without tags": {"proxmox:\n  template:\n    matchPolicy: exact\n", "needs proxmox.template.tags"},
		"bad apt proxy":       {"nodes:\n  cache:\n    aptProxy: cache.lan:3142\n", "nodes.cache: apt proxy"},
		"bad mirror":          {"nodes:\n  cache:\n    registryMirrors:\n      docker.io: [cache.lan]\n", "nodes.cache: registry docker.io mirror"},
		"unknown node field":  {"nodes:\n  install: true\n", "unknown field"},
	} {
		t.Run(name, func(t *testing.T) {
			writeConfig(t, tc.body)
			if _, err := Load("", ""); err == nil || !strings.Contains(err.Error(), tc.want) {
				t.Fatalf("err = %v, want containing %q", err, tc.want)
			}
		})
	}
}
