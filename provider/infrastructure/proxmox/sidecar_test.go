package proxmox

import (
	"archive/tar"
	"bytes"
	"compress/gzip"
	"context"
	"errors"
	"io"
	"strings"
	"testing"
	"time"

	"github.com/klotho-k8s/klotho/pkg/cluster"
)

var errPermissionDenied = errors.New("permission check failed (/access/users, User.Modify)")

// makeRootfs builds a tiny gzip-compressed tar standing in for the Alpine
// minirootfs: just an /etc/inittab and a marker file.
func makeRootfs(t *testing.T, inittab string) []byte {
	t.Helper()
	var buf bytes.Buffer
	gzw := gzip.NewWriter(&buf)
	tw := tar.NewWriter(gzw)
	write := func(name string, mode int64, data string) {
		if err := tw.WriteHeader(&tar.Header{Name: name, Typeflag: tar.TypeReg, Mode: mode, Size: int64(len(data))}); err != nil {
			t.Fatal(err)
		}
		if _, err := tw.Write([]byte(data)); err != nil {
			t.Fatal(err)
		}
	}
	if inittab != "" {
		write("etc/inittab", 0o644, inittab)
	}
	write("etc/alpine-release", 0o644, "3.21.3\n")
	if err := tw.Close(); err != nil {
		t.Fatal(err)
	}
	if err := gzw.Close(); err != nil {
		t.Fatal(err)
	}
	return buf.Bytes()
}

// untar reads a gzip-compressed tar into a name->content map (regular files).
func untar(t *testing.T, data []byte) map[string]string {
	t.Helper()
	gzr, err := gzip.NewReader(bytes.NewReader(data))
	if err != nil {
		t.Fatalf("gunzip: %v", err)
	}
	out := map[string]string{}
	tr := tar.NewReader(gzr)
	for {
		hdr, err := tr.Next()
		if err == io.EOF {
			break
		}
		if err != nil {
			t.Fatalf("untar: %v", err)
		}
		if hdr.Typeflag == tar.TypeReg {
			b, _ := io.ReadAll(tr)
			out[strings.TrimPrefix(hdr.Name, "./")] = string(b)
		}
	}
	return out
}

func TestInjectSweepModifiesInittab(t *testing.T) {
	base := makeRootfs(t, "::sysinit:/sbin/openrc sysinit\n::ctrlaltdel:/sbin/reboot\n")
	out, err := injectSweep(bytes.NewReader(base), []byte("#!/bin/sh\necho sweep\n"), []byte("KLOTHO_PVE_URL='https://pve'\n"), []byte("CURLBIN"), []byte("JQBIN"))
	if err != nil {
		t.Fatal(err)
	}
	files := untar(t, out)

	if got := files[sweepScriptPath]; !strings.Contains(got, "echo sweep") {
		t.Fatalf("sweep script not baked in: %q", got)
	}
	if files[sweepScriptPath] == "" {
		t.Fatal("sweep script missing")
	}
	if files["usr/local/bin/curl"] != "CURLBIN" || files["usr/local/bin/jq"] != "JQBIN" {
		t.Fatalf("curl/jq not baked in: curl=%q jq=%q", files["usr/local/bin/curl"], files["usr/local/bin/jq"])
	}
	if got := files[sweepEnvPath]; !strings.Contains(got, "KLOTHO_PVE_URL") {
		t.Fatalf("env file not baked in: %q", got)
	}
	inittab := files["etc/inittab"]
	if !strings.Contains(inittab, "::sysinit:/sbin/openrc sysinit") {
		t.Fatalf("original inittab lines lost: %q", inittab)
	}
	if !strings.Contains(inittab, "::once:/"+sweepScriptPath) {
		t.Fatalf("run-once init line not added: %q", inittab)
	}
}

func TestInjectSweepNoDuplicatePaths(t *testing.T) {
	// A base whose directory entries carry a trailing slash, as a real Alpine
	// minirootfs does ("usr/local/"), must not collide with the baked dirs
	// ("usr/local"): the output has to be free of duplicate paths or a strict
	// unpacker (containers/storage) rejects the template.
	var buf bytes.Buffer
	gzw := gzip.NewWriter(&buf)
	tw := tar.NewWriter(gzw)
	for _, d := range []string{"usr/", "usr/local/", "usr/local/bin/"} {
		if err := tw.WriteHeader(&tar.Header{Name: d, Typeflag: tar.TypeDir, Mode: 0o755}); err != nil {
			t.Fatal(err)
		}
	}
	if err := tw.WriteHeader(&tar.Header{Name: "etc/inittab", Typeflag: tar.TypeReg, Mode: 0o644, Size: 1}); err != nil {
		t.Fatal(err)
	}
	_, _ = tw.Write([]byte("\n"))
	_ = tw.Close()
	_ = gzw.Close()

	out, err := injectSweep(bytes.NewReader(buf.Bytes()), []byte("#!/bin/sh\n"), []byte("X='1'\n"), []byte("c"), []byte("j"))
	if err != nil {
		t.Fatal(err)
	}
	gzr, _ := gzip.NewReader(bytes.NewReader(out))
	seen := map[string]bool{}
	tr := tar.NewReader(gzr)
	for {
		hdr, err := tr.Next()
		if err == io.EOF {
			break
		}
		if err != nil {
			t.Fatal(err)
		}
		name := strings.TrimSuffix(hdr.Name, "/")
		if seen[name] {
			t.Fatalf("duplicate path in template: %q", name)
		}
		seen[name] = true
	}
	for _, want := range []string{"usr/local/bin/curl", "usr/local/bin/jq", sweepScriptPath} {
		if !seen[want] {
			t.Errorf("missing %q", want)
		}
	}
}

func TestInjectSweepSynthesizesInittab(t *testing.T) {
	base := makeRootfs(t, "") // no inittab in the base
	out, err := injectSweep(bytes.NewReader(base), []byte("#!/bin/sh\n"), []byte("X='1'\n"), []byte("c"), []byte("j"))
	if err != nil {
		t.Fatal(err)
	}
	inittab := untar(t, out)["etc/inittab"]
	if !strings.Contains(inittab, "::once:/"+sweepScriptPath) {
		t.Fatalf("synthesized inittab missing run-once line: %q", inittab)
	}
	if !strings.Contains(inittab, "sysinit") {
		t.Fatalf("synthesized inittab missing sysinit: %q", inittab)
	}
}

func TestRenderEnvFile(t *testing.T) {
	got := string(renderEnvFile(map[string]string{
		"B": "two",
		"A": "has'quote",
	}))
	want := "A='has'\\''quote'\nB='two'\n"
	if got != want {
		t.Fatalf("renderEnvFile = %q, want %q", got, want)
	}
}

func TestSanitizeName(t *testing.T) {
	cases := map[string]string{
		"demo":        "demo",
		"My_Cluster1": "my-cluster1",
		"a.b.c":       "a-b-c",
		"--x--":       "x",
		"!!!":         "cluster",
	}
	for in, want := range cases {
		if got := sanitizeName(in); got != want {
			t.Errorf("sanitizeName(%q) = %q, want %q", in, got, want)
		}
	}
}

// sidecarProvider wires a provider to the fake with a stubbed rootfs source and
// the connection config the sidecar needs.
func sidecarProvider(t *testing.T, f *fake) *Provider {
	t.Helper()
	client := f.serve(t)
	p := New(client, Config{URL: f.srv.URL, TokenID: testTokenID, Secret: testSecret})
	p.poll = time.Millisecond
	p.taskPoll = time.Millisecond
	rootfs := makeRootfs(t, "::sysinit:/sbin/openrc sysinit\n")
	p.rootfsSource = func(context.Context) (io.ReadCloser, error) {
		return io.NopCloser(bytes.NewReader(rootfs)), nil
	}
	p.toolSource = func(_ context.Context, url, _ string) ([]byte, error) {
		if strings.Contains(url, "jq") {
			return []byte("JQBIN"), nil
		}
		return []byte("CURLBIN"), nil
	}
	return p
}

func sidecarState() *cluster.State {
	return &cluster.State{Spec: cluster.Spec{Name: "demo", Provider: ProviderName}}
}

func TestMintSidecarToken(t *testing.T) {
	f := newFake()
	p := sidecarProvider(t, f)

	id, secret, minted := p.mintSidecarToken(t.Context(), "demo")
	if !minted {
		t.Fatal("expected a minted token")
	}
	if !strings.HasPrefix(id, "root@pam!klotho-del-demo-") {
		t.Fatalf("minted token id = %q", id)
	}
	if secret != "minted-secret-0000" {
		t.Fatalf("minted secret = %q", secret)
	}
}

func TestMintSidecarTokenFallsBack(t *testing.T) {
	f := newFake()
	f.tokenCreateErr = errPermissionDenied
	p := sidecarProvider(t, f)

	id, secret, minted := p.mintSidecarToken(t.Context(), "demo")
	if minted {
		t.Fatal("expected fallback, not a minted token")
	}
	if id != testTokenID || secret != testSecret {
		t.Fatalf("fallback token = %q/%q, want the cluster token", id, secret)
	}
}

func TestLaunchDeletionSidecar(t *testing.T) {
	f := newFake()
	p := sidecarProvider(t, f)

	if err := p.LaunchDeletionSidecar(t.Context(), sidecarState()); err != nil {
		t.Fatal(err)
	}

	if len(f.lxcCreates) != 1 {
		t.Fatalf("expected one container create, got %d", len(f.lxcCreates))
	}
	c := f.lxcCreates[0]
	if c["start"] != "1" {
		t.Errorf("container not started: start=%q", c["start"])
	}
	if c["unprivileged"] != "1" {
		t.Errorf("container not unprivileged: %q", c["unprivileged"])
	}
	if !strings.HasPrefix(c["ostemplate"], "local:vztmpl/klotho-sweep-demo-") {
		t.Errorf("unexpected ostemplate %q", c["ostemplate"])
	}
	if !strings.Contains(c["tags"], Tag("demo")) || !strings.Contains(c["tags"], SidecarTag) {
		t.Errorf("tags missing cluster/sidecar tag: %q", c["tags"])
	}
	if !strings.Contains(c["net0"], "ip=dhcp") {
		t.Errorf("net0 not dhcp: %q", c["net0"])
	}
	if !strings.HasPrefix(c["rootfs"], "local-lvm:") {
		t.Errorf("rootfs not on the rootdir storage: %q", c["rootfs"])
	}

	// The template was uploaded as a vztmpl and carries the baked sweep, env
	// file, and the minted token.
	var tmpl []byte
	for volid, data := range f.uploads {
		if strings.HasPrefix(volid, "local:vztmpl/klotho-sweep-demo-") {
			tmpl = data
		}
	}
	if tmpl == nil {
		t.Fatal("sidecar template not uploaded as a vztmpl")
	}
	files := untar(t, tmpl)
	if !strings.Contains(files[sweepScriptPath], "klotho-sweep") {
		t.Errorf("uploaded template missing the sweep script")
	}
	env := files[sweepEnvPath]
	if !strings.Contains(env, envToken+"='root@pam!klotho-del-demo-") {
		t.Errorf("env did not carry the minted token: %q", env)
	}
	if !strings.Contains(env, envSelfNode+"='pve1'") {
		t.Errorf("env did not carry the self node: %q", env)
	}
	if !strings.Contains(env, envTemplateVolid+"='pve1|local:vztmpl/klotho-sweep-demo-") {
		t.Errorf("env did not carry its own template volid for cleanup: %q", env)
	}
}

func TestLaunchDeletionSidecarBakesClusterTokenOnFallback(t *testing.T) {
	f := newFake()
	f.tokenCreateErr = errPermissionDenied
	p := sidecarProvider(t, f)

	if err := p.LaunchDeletionSidecar(t.Context(), sidecarState()); err != nil {
		t.Fatal(err)
	}
	var tmpl []byte
	for volid, data := range f.uploads {
		if strings.HasPrefix(volid, "local:vztmpl/") {
			tmpl = data
		}
	}
	if tmpl == nil {
		t.Fatal("template not uploaded")
	}
	env := untar(t, tmpl)[sweepEnvPath]
	if !strings.Contains(env, envToken+"='"+testTokenID+"'") {
		t.Errorf("fallback did not bake the cluster token: %q", env)
	}
}

func TestDeriveSidecarIP(t *testing.T) {
	cases := []struct {
		name, ranges string
		prefix       int
		endpoint, gw string
		want         string
	}{
		{"above pool top", "[192.168.1.200-192.168.1.220]", 20, "192.168.1.250", "192.168.1.1", "192.168.1.221"},
		{"skips endpoint", "[192.168.1.200-192.168.1.220]", 24, "192.168.1.221", "192.168.1.1", "192.168.1.222"},
		{"single address", "[10.0.0.5]", 24, "", "", "10.0.0.6"},
		{"bad prefix", "[192.168.1.200-192.168.1.220]", 0, "", "", ""},
		{"bad ranges", "[]", 24, "", "", ""},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			if got := deriveSidecarIP(c.ranges, c.prefix, c.endpoint, c.gw); got != c.want {
				t.Fatalf("deriveSidecarIP(%q,%d) = %q, want %q", c.ranges, c.prefix, got, c.want)
			}
		})
	}
}

func TestSidecarNetwork(t *testing.T) {
	p := New(nil, Config{})
	staticVars := map[string]string{
		VarGateway: "192.168.1.1", VarIPPrefix: "20",
		VarDNSServers: "[192.168.1.1]", VarNodeIPRanges: "[192.168.1.200-192.168.1.220]",
		VarEndpointIP: "192.168.1.250",
	}
	// Derived from the cluster's network config.
	n := p.sidecarNetwork(&cluster.State{Spec: cluster.Spec{Variables: staticVars}})
	if !n.static || n.ip != "192.168.1.221" || n.prefix != 20 || n.gateway != "192.168.1.1" || n.dns != "192.168.1.1" {
		t.Fatalf("derived net = %+v", n)
	}
	if got := n.net0("vmbr0"); got != "name=eth0,bridge=vmbr0,ip=192.168.1.221/20,gw=192.168.1.1" {
		t.Fatalf("net0 = %q", got)
	}
	// Explicit sidecar.ip wins, with a bare address taking the prefix from vars.
	p2 := New(nil, Config{Sidecar: SidecarConfig{IP: "192.168.1.9"}})
	n2 := p2.sidecarNetwork(&cluster.State{Spec: cluster.Spec{Variables: staticVars}})
	if !n2.static || n2.ip != "192.168.1.9" || n2.prefix != 20 {
		t.Fatalf("explicit net = %+v", n2)
	}
	// No network config: DHCP fallback.
	n3 := p.sidecarNetwork(&cluster.State{Spec: cluster.Spec{Name: "x"}})
	if n3.static {
		t.Fatalf("expected DHCP fallback, got %+v", n3)
	}
	if got := n3.net0("vmbr0"); got != "name=eth0,bridge=vmbr0,ip=dhcp" {
		t.Fatalf("dhcp net0 = %q", got)
	}
}

func TestLaunchDeletionSidecarStaticIP(t *testing.T) {
	f := newFake()
	p := sidecarProvider(t, f)
	st := sidecarState()
	st.Spec.Variables = map[string]string{
		VarGateway: "192.168.1.1", VarIPPrefix: "20",
		VarDNSServers: "[192.168.1.1]", VarNodeIPRanges: "[192.168.1.200-192.168.1.220]",
	}
	if err := p.LaunchDeletionSidecar(t.Context(), st); err != nil {
		t.Fatal(err)
	}
	c := f.lxcCreates[0]
	if c["net0"] != "name=eth0,bridge=vmbr0,ip=192.168.1.221/20,gw=192.168.1.1" {
		t.Errorf("net0 not static: %q", c["net0"])
	}
	if c["nameserver"] != "192.168.1.1" {
		t.Errorf("nameserver not set: %q", c["nameserver"])
	}
	var tmpl []byte
	for volid, data := range f.uploads {
		if strings.HasPrefix(volid, "local:vztmpl/klotho-sweep-demo-") {
			tmpl = data
		}
	}
	env := untar(t, tmpl)[sweepEnvPath]
	for _, want := range []string{envNetIP + "='192.168.1.221'", envNetPrefix + "='20'", envNetGW + "='192.168.1.1'", envNetDNS + "='192.168.1.1'"} {
		if !strings.Contains(env, want) {
			t.Errorf("env missing %q: %q", want, env)
		}
	}
}

func TestLaunchDeletionSidecarNeedsConnection(t *testing.T) {
	f := newFake()
	client := f.serve(t)
	p := New(client, Config{}) // no URL/token
	if err := p.LaunchDeletionSidecar(t.Context(), sidecarState()); err == nil {
		t.Fatal("expected an error without connection config")
	}
}
