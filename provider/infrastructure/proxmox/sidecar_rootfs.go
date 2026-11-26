package proxmox

import (
	"archive/tar"
	"bytes"
	"compress/gzip"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"io"
	"sort"
	"strconv"
	"strings"
)

// Environment variables the baked sweep reads from sweepEnvPath. They carry
// everything the sidecar needs to finish a cluster's teardown and remove
// itself, including the API credential.
const (
	envURL             = "KLOTHO_PVE_URL"
	envToken           = "KLOTHO_PVE_TOKEN"
	envSecret          = "KLOTHO_PVE_SECRET"
	envInsecure        = "KLOTHO_PVE_INSECURE"
	envClusterTag      = "KLOTHO_CLUSTER_TAG"
	envSelfNode        = "KLOTHO_SELF_NODE"
	envSelfVMID        = "KLOTHO_SELF_VMID"
	envISOVolids       = "KLOTHO_ISO_VOLIDS"
	envTemplateVolid   = "KLOTHO_TEMPLATE_VOLID"
	envTemplateStorage = "KLOTHO_TEMPLATE_STORAGE"
	envPollSeconds     = "KLOTHO_POLL_SECONDS"
	envMaxWaitSeconds  = "KLOTHO_MAX_WAIT_SECONDS"
	// Static network the baked init brings eth0 up with. Unset means the sweep
	// falls back to requesting a DHCP lease.
	envNetIP     = "KLOTHO_NET_IP"
	envNetPrefix = "KLOTHO_NET_PREFIX"
	envNetGW     = "KLOTHO_NET_GW"
	envNetDNS    = "KLOTHO_NET_DNS"
)

// Sweep pacing baked into the sidecar: how often it re-checks the cluster's
// VMs while waiting for Cluster API, and the absolute cap after which it sweeps
// whatever remains regardless.
const (
	sweepPollSeconds    = 15
	sweepMaxWaitSeconds = 30 * 60
)

// sweepParams is what sweepEnv turns into the baked environment.
type sweepParams struct {
	clusterName   string
	node          string
	selfVMID      int
	tokenID       string
	secret        string
	isoVolids     []string
	rootfsStorage string
	net           sidecarNet
}

// sweepEnv renders the environment the sidecar sources. The template volid is
// filled in by the caller after upload (envTemplateVolid), so it is left unset
// here.
func (p *Provider) sweepEnv(s sweepParams) map[string]string {
	insecure := "0"
	if p.cfg.Insecure {
		insecure = "1"
	}
	env := map[string]string{
		envURL:            p.cfg.URL,
		envToken:          s.tokenID,
		envSecret:         s.secret,
		envInsecure:       insecure,
		envClusterTag:     Tag(s.clusterName),
		envSelfNode:       s.node,
		envSelfVMID:       strconv.Itoa(s.selfVMID),
		envISOVolids:      strings.Join(s.isoVolids, " "),
		envPollSeconds:    strconv.Itoa(sweepPollSeconds),
		envMaxWaitSeconds: strconv.Itoa(sweepMaxWaitSeconds),
	}
	// Static addressing the baked init applies to eth0; omitted for DHCP.
	if s.net.static {
		env[envNetIP] = s.net.ip
		env[envNetPrefix] = strconv.Itoa(s.net.prefix)
		if s.net.gateway != "" {
			env[envNetGW] = s.net.gateway
		}
		if s.net.dns != "" {
			env[envNetDNS] = s.net.dns
		}
	}
	return env
}

// renderEnvFile renders env as shell-sourceable KEY='value' lines in a stable
// order. Single quotes in values are escaped so the file is safe to source.
func renderEnvFile(env map[string]string) []byte {
	keys := make([]string, 0, len(env))
	for k := range env {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	var b strings.Builder
	for _, k := range keys {
		b.WriteString(k)
		b.WriteString("='")
		b.WriteString(strings.ReplaceAll(env[k], "'", `'\''`))
		b.WriteString("'\n")
	}
	return []byte(b.String())
}

// buildSidecarTemplate builds the one-shot Alpine LXC template: it takes the
// base minirootfs, bakes in the sweep script and the env file, and wires the
// container's init to run the sweep once at boot. The result is a
// gzip-compressed tar ready to upload as a vztmpl.
func (p *Provider) buildSidecarTemplate(ctx context.Context, env map[string]string) ([]byte, error) {
	base, err := p.alpineRootfs(ctx)
	if err != nil {
		return nil, err
	}
	defer func() { _ = base.Close() }()
	curlBin, err := p.sidecarTool(ctx,
		firstNonEmpty(p.cfg.Sidecar.CurlURL, DefaultSidecarCurlURL),
		firstNonEmpty(p.cfg.Sidecar.CurlSHA256, DefaultSidecarCurlSHA256))
	if err != nil {
		return nil, fmt.Errorf("fetching the sidecar's curl: %w", err)
	}
	jqBin, err := p.sidecarTool(ctx,
		firstNonEmpty(p.cfg.Sidecar.JqURL, DefaultSidecarJqURL),
		firstNonEmpty(p.cfg.Sidecar.JqSHA256, DefaultSidecarJqSHA256))
	if err != nil {
		return nil, fmt.Errorf("fetching the sidecar's jq: %w", err)
	}
	// Normalize line endings: the script is embedded from the working tree and
	// runs under BusyBox sh in the container, which rejects CRLF.
	script := bytes.ReplaceAll(sweepScript, []byte("\r\n"), []byte("\n"))
	return injectSweep(base, script, renderEnvFile(env), curlBin, jqBin)
}

// sidecarTool fetches a baked sidecar binary (curl or jq) and verifies its
// digest when one is set. The toolSource hook lets tests supply stand-ins
// without reaching the network.
func (p *Provider) sidecarTool(ctx context.Context, url, sum string) ([]byte, error) {
	if p.toolSource != nil {
		return p.toolSource(ctx, url, sum)
	}
	body, err := sidecarHTTPGet(ctx, url)
	if err != nil {
		return nil, err
	}
	defer func() { _ = body.Close() }()
	data, err := io.ReadAll(body)
	if err != nil {
		return nil, err
	}
	if sum != "" {
		got := sha256.Sum256(data)
		if hex.EncodeToString(got[:]) != strings.ToLower(sum) {
			return nil, fmt.Errorf("proxmox: sidecar tool %s failed digest check", url)
		}
	}
	return data, nil
}

// alpineRootfs returns the base minirootfs reader, from the injected source in
// tests or by fetching (and optionally verifying) the configured tarball.
func (p *Provider) alpineRootfs(ctx context.Context) (io.ReadCloser, error) {
	if p.rootfsSource != nil {
		return p.rootfsSource(ctx)
	}
	url := firstNonEmpty(p.cfg.Sidecar.AlpineRootfsURL, DefaultAlpineRootfsURL)
	sum := firstNonEmpty(p.cfg.Sidecar.AlpineRootfsSHA256, DefaultAlpineRootfsSHA256)
	body, err := sidecarHTTPGet(ctx, url)
	if err != nil {
		return nil, err
	}
	if sum == "" {
		return body, nil
	}
	// Verify: the digest matters, so the whole tarball is buffered and checked
	// before it is used.
	defer func() { _ = body.Close() }()
	data, err := io.ReadAll(body)
	if err != nil {
		return nil, err
	}
	got := sha256.Sum256(data)
	if hex.EncodeToString(got[:]) != strings.ToLower(sum) {
		return nil, fmt.Errorf("proxmox: Alpine rootfs %s failed digest check", url)
	}
	return io.NopCloser(bytes.NewReader(data)), nil
}

// inittabRunOnce is appended to the container's /etc/inittab so BusyBox init
// runs the sweep exactly once at boot, logging to a file in the container.
const inittabRunOnce = "::once:/" + sweepScriptPath + " > /var/log/klotho-sweep.log 2>&1\n"

// injectSweep reads a gzip-compressed tar (the base rootfs) and returns a new
// gzip-compressed tar with the sweep script and env file added and the init
// wired to run the sweep once at boot. The whole archive is held in memory;
// the Alpine minirootfs is a few megabytes.
func injectSweep(base io.Reader, script, envFile, curlBin, jqBin []byte) ([]byte, error) {
	gzr, err := gzip.NewReader(base)
	if err != nil {
		return nil, fmt.Errorf("reading rootfs gzip: %w", err)
	}
	defer func() { _ = gzr.Close() }()

	type entry struct {
		hdr  *tar.Header
		data []byte
	}
	var entries []entry
	inittabIdx := -1
	tr := tar.NewReader(gzr)
	for {
		hdr, err := tr.Next()
		if err == io.EOF {
			break
		}
		if err != nil {
			return nil, fmt.Errorf("reading rootfs tar: %w", err)
		}
		name := strings.TrimPrefix(hdr.Name, "./")
		var data []byte
		if hdr.Typeflag == tar.TypeReg {
			if data, err = io.ReadAll(tr); err != nil {
				return nil, fmt.Errorf("reading %s from rootfs: %w", hdr.Name, err)
			}
		}
		hdr.Name = name
		entries = append(entries, entry{hdr: hdr, data: data})
		if name == "etc/inittab" {
			inittabIdx = len(entries) - 1
		}
	}

	// Wire the container's init to run the sweep once at boot. Done by index,
	// not a retained pointer, so later appends that reallocate entries do not
	// leave us editing a stale backing array.
	if inittabIdx >= 0 {
		data := append(bytes.TrimRight(entries[inittabIdx].data, "\n"), append([]byte("\n"), inittabRunOnce...)...)
		entries[inittabIdx].data = data
		entries[inittabIdx].hdr.Size = int64(len(data))
	} else {
		// No inittab in the base: supply a minimal BusyBox-init one that runs
		// the sweep and keeps the container alive long enough to finish.
		data := []byte("::sysinit:/sbin/openrc sysinit\n" + inittabRunOnce)
		entries = append(entries, entry{hdr: &tar.Header{Name: "etc/inittab", Typeflag: tar.TypeReg, Mode: 0o644, Size: int64(len(data))}, data: data})
	}

	// Our baked files. Written last so they override anything in the base.
	// curl and jq are baked in statically linked, so the sweep needs no package
	// install and no internet, only LAN reachability to the Proxmox API.
	entries = append(entries,
		entry{hdr: &tar.Header{Name: "etc/klotho", Typeflag: tar.TypeDir, Mode: 0o755}},
		entry{hdr: &tar.Header{Name: sweepEnvPath, Typeflag: tar.TypeReg, Mode: 0o600, Size: int64(len(envFile))}, data: envFile},
		entry{hdr: &tar.Header{Name: sweepScriptPath, Typeflag: tar.TypeReg, Mode: 0o755, Size: int64(len(script))}, data: script},
		entry{hdr: &tar.Header{Name: "usr/local", Typeflag: tar.TypeDir, Mode: 0o755}},
		entry{hdr: &tar.Header{Name: "usr/local/bin", Typeflag: tar.TypeDir, Mode: 0o755}},
		entry{hdr: &tar.Header{Name: "usr/local/bin/curl", Typeflag: tar.TypeReg, Mode: 0o755, Size: int64(len(curlBin))}, data: curlBin},
		entry{hdr: &tar.Header{Name: "usr/local/bin/jq", Typeflag: tar.TypeReg, Mode: 0o755, Size: int64(len(jqBin))}, data: jqBin},
	)

	// Collapse duplicate paths, last writer winning: our baked dirs (etc/klotho,
	// usr/local, usr/local/bin) may already exist in the base, which names them
	// with a trailing slash ("usr/local/"). GNU tar tolerates duplicates, but a
	// strict unpacker (containers/storage) treats "usr/local/" and "usr/local"
	// as the same path and rejects the archive, so emit each path once (keyed
	// without the trailing slash), keeping first-seen order.
	idx := make(map[string]int, len(entries))
	deduped := entries[:0]
	for _, e := range entries {
		key := strings.TrimSuffix(e.hdr.Name, "/")
		if i, ok := idx[key]; ok {
			deduped[i] = e
			continue
		}
		idx[key] = len(deduped)
		deduped = append(deduped, e)
	}
	entries = deduped

	var out bytes.Buffer
	gzw := gzip.NewWriter(&out)
	tw := tar.NewWriter(gzw)
	for _, e := range entries {
		if e.hdr.Typeflag == tar.TypeReg {
			e.hdr.Size = int64(len(e.data))
		}
		if err := tw.WriteHeader(e.hdr); err != nil {
			return nil, fmt.Errorf("writing %s header: %w", e.hdr.Name, err)
		}
		if len(e.data) > 0 {
			if _, err := tw.Write(e.data); err != nil {
				return nil, fmt.Errorf("writing %s: %w", e.hdr.Name, err)
			}
		}
	}
	if err := tw.Close(); err != nil {
		return nil, err
	}
	if err := gzw.Close(); err != nil {
		return nil, err
	}
	return out.Bytes(), nil
}
