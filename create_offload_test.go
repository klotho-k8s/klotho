package klotho

import (
	"archive/tar"
	"bytes"
	"compress/gzip"
	"context"
	"io"
	"log/slog"
	"strings"
	"testing"

	"github.com/klotho-k8s/klotho/pkg/cloudinit"
	"github.com/klotho-k8s/klotho/provider/controlplane"
	"github.com/klotho-k8s/klotho/provider/payload"
)

// limiterInfra is a fakeInfra that also declares a user-data cap.
type limiterInfra struct {
	*fakeInfra
	limit int
}

func (l limiterInfra) UserDataLimit() int { return l.limit }

// capturePayload records the files published and returns one fetch per file.
type capturePayload struct {
	cluster string
	files   []payload.File
}

func (c *capturePayload) Name() string { return "capture" }

func (c *capturePayload) Publish(_ context.Context, cluster string, files []payload.File) ([]payload.Fetch, error) {
	c.cluster = cluster
	c.files = files
	fetches := make([]payload.Fetch, len(files))
	for i, f := range files {
		fetches[i] = payload.Fetch{Path: f.Path, Command: "curl -o " + f.Path + " https://host/" + cluster}
	}
	return fetches, nil
}

func (c *capturePayload) Remove(context.Context, string) error { return nil }

func offloadContribs() []controlplane.Contribution {
	return []controlplane.Contribution{{
		Files: []controlplane.File{
			{Path: "/etc/kubernetes/pki/ca.key", Content: bytes.Repeat([]byte("k"), 20000), Mode: "0600"},
			{Path: "/run/klotho/post-init.sh", Content: []byte("#!/bin/bash\necho hi\n"), Mode: "0700"},
			{Path: "/etc/motd.append", Content: []byte("more\n"), Append: true},
		},
		PostCommands: []string{"/run/klotho/post-init.sh"},
	}}
}

func buildCI(t *testing.T, contribs []controlplane.Contribution) (*cloudinit.Bundle, cloudinit.Options) {
	t.Helper()
	opts := cloudinit.Options{Hostname: "demo-bootstrap"}
	ci, err := cloudinit.Build(contribs, opts)
	if err != nil {
		t.Fatalf("Build: %v", err)
	}
	return ci, opts
}

func TestOffloadUserDataArchivesAndShrinks(t *testing.T) {
	contribs := offloadContribs()
	ci, opts := buildCI(t, contribs)
	if len(ci.UserData) <= 16384 {
		t.Fatalf("precondition: inline user-data is only %d bytes, expected over the limit", len(ci.UserData))
	}

	capt := &capturePayload{}
	deps := Deps{Infra: limiterInfra{fakeInfra: &fakeInfra{}, limit: 16384}, Payload: capt}
	out, err := offloadUserData(context.Background(), deps, "demo", contribs, ci, opts, slog.New(slog.DiscardHandler))
	if err != nil {
		t.Fatalf("offloadUserData: %v", err)
	}

	if len(out.UserData) > 16384 {
		t.Errorf("offloaded user-data is %d bytes, over the limit", len(out.UserData))
	}
	if capt.cluster != "demo" || len(capt.files) != 1 {
		t.Fatalf("published %d files for cluster %q, want 1 for demo", len(capt.files), capt.cluster)
	}
	if capt.files[0].Path != PayloadArchivePath {
		t.Errorf("archive path = %q, want %q", capt.files[0].Path, PayloadArchivePath)
	}

	ud := string(out.UserData)
	if !strings.Contains(ud, "tar -xpzf "+PayloadArchivePath) {
		t.Errorf("user-data missing the extract command:\n%s", ud)
	}
	if strings.Contains(ud, strings.Repeat("k", 100)) {
		t.Error("user-data still carries the offloaded file content")
	}
	// The appended file stays in write_files.
	if !strings.Contains(ud, "/etc/motd.append") {
		t.Error("appended file was wrongly dropped from write_files")
	}

	// The archive untars to the offloaded files with their modes preserved.
	got := untar(t, capt.files[0].Content)
	if string(got["etc/kubernetes/pki/ca.key"].data) != strings.Repeat("k", 20000) {
		t.Error("ca.key content not archived correctly")
	}
	if got["etc/kubernetes/pki/ca.key"].mode != 0o600 {
		t.Errorf("ca.key mode = %o, want 600", got["etc/kubernetes/pki/ca.key"].mode)
	}
	if got["run/klotho/post-init.sh"].mode != 0o700 {
		t.Errorf("post-init.sh mode = %o, want 700", got["run/klotho/post-init.sh"].mode)
	}
	if _, ok := got["etc/motd.append"]; ok {
		t.Error("appended file should not be in the archive")
	}
}

func TestOffloadUserDataNoLimiterPassthrough(t *testing.T) {
	contribs := offloadContribs()
	ci, opts := buildCI(t, contribs)
	deps := Deps{Infra: &fakeInfra{}} // fakeInfra does not implement UserDataLimiter
	out, err := offloadUserData(context.Background(), deps, "demo", contribs, ci, opts, slog.New(slog.DiscardHandler))
	if err != nil {
		t.Fatalf("offloadUserData: %v", err)
	}
	if !bytes.Equal(out.UserData, ci.UserData) {
		t.Error("user-data changed although the provider declares no limit")
	}
}

func TestOffloadUserDataNoPayloadHostErrors(t *testing.T) {
	contribs := offloadContribs()
	ci, opts := buildCI(t, contribs)
	deps := Deps{Infra: limiterInfra{fakeInfra: &fakeInfra{}, limit: 16384}} // no Payload
	if _, err := offloadUserData(context.Background(), deps, "demo", contribs, ci, opts, slog.New(slog.DiscardHandler)); err == nil {
		t.Fatal("expected an error when user-data is over the limit and no payload host is configured")
	}
}

type tarEntry struct {
	data []byte
	mode int64
}

func untar(t *testing.T, gzData []byte) map[string]tarEntry {
	t.Helper()
	gz, err := gzip.NewReader(bytes.NewReader(gzData))
	if err != nil {
		t.Fatalf("gzip: %v", err)
	}
	tr := tar.NewReader(gz)
	out := map[string]tarEntry{}
	for {
		h, err := tr.Next()
		if err == io.EOF {
			break
		}
		if err != nil {
			t.Fatalf("tar: %v", err)
		}
		data, err := io.ReadAll(tr)
		if err != nil {
			t.Fatalf("tar read: %v", err)
		}
		out[h.Name] = tarEntry{data: data, mode: h.Mode}
	}
	return out
}
