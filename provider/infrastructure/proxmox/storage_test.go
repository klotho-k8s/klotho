package proxmox

import (
	"context"
	"errors"
	"io"
	"net/http"
	"strings"
	"testing"
	"time"

	gp "github.com/luthermonson/go-proxmox"
)

func TestStoragesDecode(t *testing.T) {
	p := newRouteServer(t, routes{
		"GET /nodes/pve1/storage": rawJSON(`{"data":[
			{"storage":"local","type":"dir","content":"iso,vztmpl,backup","active":1,"enabled":1,"shared":0,"avail":50000000000,"total":100000000000,"used":50000000000,"used_fraction":0.5},
			{"storage":"local-lvm","type":"lvmthin","content":"rootdir,images","active":1,"enabled":1,"shared":0,"avail":200000000000,"total":400000000000,"used":0,"used_fraction":0}
		]}`),
	})
	st, err := p.client.NodeRef("pve1").Storages(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if len(st) != 2 {
		t.Fatalf("got %d", len(st))
	}
	if s := st[0]; s.Name != "local" || s.Type != "dir" || s.Active == 0 || s.Enabled == 0 || s.Shared != 0 || !storageSupports(s, "iso") || storageSupports(s, "images") || s.Avail != 50000000000 {
		t.Errorf("st[0] = %+v", s)
	}
	if s := st[1]; s.Avail != 200000000000 || !storageSupports(s, "images") {
		t.Errorf("st[1] = %+v", s)
	}
}

func TestListISOs(t *testing.T) {
	p := newRouteServer(t, routes{
		"GET /nodes/pve1/storage/local/content": func(w http.ResponseWriter, r *http.Request) {
			if got := r.URL.Query().Get("content"); got != "iso" {
				t.Errorf("content = %q", got)
			}
			rawJSON(`{"data":[{"volid":"local:iso/seed-120.iso","content":"iso","format":"iso","size":374784,"ctime":1700000000}]}`)(w, r)
		},
	})
	vols, err := p.listISOs(context.Background(), "pve1", "local")
	if err != nil {
		t.Fatal(err)
	}
	if len(vols) != 1 || vols[0].Volid != "local:iso/seed-120.iso" || vols[0].Size != 374784 || vols[0].Ctime != 1700000000 {
		t.Fatalf("vols = %+v", vols)
	}
	if _, err := p.listISOs(context.Background(), "pve1", "bad/storage"); err == nil {
		t.Error("invalid storage name accepted")
	}
}

func TestDeleteVolume(t *testing.T) {
	// The library places the volume ID in the path as-is.
	p := newRouteServer(t, routes{
		"DELETE /nodes/pve1/storage/local/content/local:iso/seed-120.iso": data(t, testUPID),
	})
	upid, err := p.deleteVolume(context.Background(), "pve1", "local", "local:iso/seed-120.iso")
	if err != nil || upid != testUPID {
		t.Fatalf("deleteVolume = %q, %v", upid, err)
	}
	if _, err := p.deleteVolume(context.Background(), "pve1", "local", "local:iso/x.iso?y"); err == nil {
		t.Error("volume ID with query characters accepted")
	}
}

func TestDeleteVolumeNullData(t *testing.T) {
	p := newRouteServer(t, routes{
		"DELETE /nodes/pve1/storage/local/content/local:iso/x.iso": data(t, nil),
	})
	upid, err := p.deleteVolume(context.Background(), "pve1", "local", "local:iso/x.iso")
	if err != nil || upid != "" {
		t.Fatalf("deleteVolume = %q, %v", upid, err)
	}
}

// patternReader yields size bytes of a deterministic pattern. After the first
// gateAfter bytes it blocks until release is closed, proving the client sends
// data before the reader is exhausted (i.e. it streams rather than buffers).
type patternReader struct {
	size, off, gateAfter int64
	release              <-chan struct{}
	waited               bool
}

func (p *patternReader) Read(b []byte) (int, error) {
	if p.off >= p.size {
		return 0, io.EOF
	}
	if p.off >= p.gateAfter && !p.waited {
		select {
		case <-p.release:
			p.waited = true
		case <-time.After(10 * time.Second):
			return 0, errors.New("upload was not streamed: server never saw the first bytes")
		}
	}
	limit := p.size - p.off
	if !p.waited && p.gateAfter-p.off < limit {
		limit = p.gateAfter - p.off
	}
	if int64(len(b)) > limit {
		b = b[:limit]
	}
	for i := range b {
		b[i] = byte((p.off + int64(i)) % 251)
	}
	p.off += int64(len(b))
	return len(b), nil
}

func TestUploadISOMultipartStreaming(t *testing.T) {
	const (
		size      = 4 << 20
		gateAfter = 256 << 10
		peek      = 16 << 10
	)
	release := make(chan struct{})
	var once bool
	unblock := func() {
		if !once {
			once = true
			close(release)
		}
	}
	p := newRouteServer(t, routes{
		"POST /nodes/pve1/storage/local/upload": func(w http.ResponseWriter, r *http.Request) {
			defer unblock()
			if !strings.HasPrefix(r.Header.Get("Content-Type"), "multipart/form-data; boundary=") {
				t.Errorf("Content-Type = %q", r.Header.Get("Content-Type"))
			}
			if len(r.TransferEncoding) != 0 {
				t.Errorf("TransferEncoding = %v, want identity with Content-Length", r.TransferEncoding)
			}
			if r.ContentLength <= size {
				t.Errorf("ContentLength = %d, want > %d", r.ContentLength, size)
			}
			mr, err := r.MultipartReader()
			if err != nil {
				t.Errorf("MultipartReader: %v", err)
				return
			}
			part, err := mr.NextPart()
			if err != nil || part.FormName() != "content" {
				t.Errorf("first part = %v, %v; want content field", part, err)
				return
			}
			if v, _ := io.ReadAll(part); string(v) != "iso" {
				t.Errorf("content = %q, want iso", v)
			}
			part, err = mr.NextPart()
			if err != nil || part.FormName() != "filename" || part.FileName() != "seed-120.iso" {
				t.Errorf("file part = %v, %v", part, err)
				return
			}
			// Read some file bytes before releasing the rest of the client's
			// reader; if the client buffered the whole body this would stall.
			head := make([]byte, peek)
			if _, err := io.ReadFull(part, head); err != nil {
				t.Errorf("reading head: %v", err)
			}
			unblock()
			rest, err := io.ReadAll(part)
			if err != nil {
				t.Errorf("reading rest: %v", err)
			}
			got := make([]byte, 0, len(head)+len(rest))
			got = append(got, head...)
			got = append(got, rest...)
			if len(got) != size {
				t.Errorf("received %d bytes, want %d", len(got), size)
			}
			for i, b := range got {
				if b != byte(i%251) {
					t.Errorf("byte %d = %d, want %d", i, b, byte(i%251))
					break
				}
			}
			if _, err := mr.NextPart(); err != io.EOF {
				t.Errorf("expected end of multipart, got %v", err)
			}
			writeData(t, w, "UPID:pve1:00001234:00ABCDEF:65A1B2C3:imgcopy::root@pam!klotho:")
		},
	})
	r := &patternReader{size: size, gateAfter: gateAfter, release: release}
	upid, err := p.uploadISO(context.Background(), "pve1", "local", "seed-120.iso", r, size)
	if err != nil {
		t.Fatal(err)
	}
	if nodeFromUPID(upid) != "pve1" {
		t.Fatalf("upid = %q", upid)
	}
}

func TestUploadISOShortReader(t *testing.T) {
	p := newRouteServer(t, routes{
		"POST /nodes/pve1/storage/local/upload": func(w http.ResponseWriter, r *http.Request) {
			_, _ = io.Copy(io.Discard, r.Body)
			writeData(t, w, testUPID)
		},
	})
	if _, err := p.uploadISO(context.Background(), "pve1", "local", "x.iso", strings.NewReader("short"), 100); err == nil {
		t.Fatal("expected error for reader shorter than declared size")
	}
}

func TestUploadISOCancelled(t *testing.T) {
	p := newRouteServer(t, routes{})
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	_, err := p.uploadISO(ctx, "pve1", "local", "x.iso", strings.NewReader("data"), 4)
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("err = %v, want context.Canceled", err)
	}
}

func TestUploadISORejectsBadInput(t *testing.T) {
	p := newRouteServer(t, routes{})
	ctx := context.Background()
	for _, name := range []string{"", "../x.iso", `a"b.iso`, "a\\b.iso"} {
		if _, err := p.uploadISO(ctx, "pve1", "local", name, strings.NewReader(""), 0); err == nil {
			t.Errorf("filename %q accepted", name)
		}
	}
	if _, err := p.uploadISO(ctx, "pve1", "local", "x.iso", strings.NewReader(""), -1); err == nil {
		t.Error("negative size accepted")
	}
	if _, err := p.uploadISO(ctx, "pve1", "local", "x.iso", nil, 0); err == nil {
		t.Error("nil reader accepted")
	}
}

func TestUploadISOServerError(t *testing.T) {
	p := newRouteServer(t, routes{
		"POST /nodes/pve1/storage/local/upload": fail(t, 500, "storage 'local' does not support content-type 'iso'"),
	})
	_, err := p.uploadISO(context.Background(), "pve1", "local", "x.iso", strings.NewReader(strings.Repeat("x", 1024)), 1024)
	apiErr, ok := gp.AsAPIError(err)
	if !ok || !strings.Contains(apiErr.Message, "content-type 'iso'") {
		t.Fatalf("err = %v", err)
	}
}
