package proxmox

import (
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"sync"
	"testing"

	gp "github.com/luthermonson/go-proxmox"

	"github.com/klotho-k8s/klotho/pkg/state"
	"github.com/klotho-k8s/klotho/provider/infrastructure/proxmox"
)

const (
	testTokenID = "root@pam!klotho"
	testSecret  = "5f3c9a2e-1b7d-4c8e-9f60-2a4b6c8d0e1f"
	testAuth    = "PVEAPIToken=" + testTokenID + "=" + testSecret
)

// pool and member are the fake server's model of a resource pool.
type pool struct {
	ID      string
	Comment string
	Members []member
}

type member struct {
	ID, Type string
	VMID     int
	Storage  string
}

// fakeServer emulates the Proxmox pool endpoints under /api2/json with an
// in-memory pool table, logging every call.
type fakeServer struct {
	mu    sync.Mutex
	pools map[string]*pool
	order []string
	calls []string

	// maxComment, when positive, makes writes of longer comments fail the
	// way Proxmox rejects an over-long parameter.
	maxComment int
	// failGet makes reading a pool fail with a transport error.
	failGet bool
	// stripNewlines stores comments without line breaks, as a server that
	// sanitises comments would.
	stripNewlines bool

	srv *httptest.Server
	c   *gp.Client
}

func newFake() *fakeServer { return &fakeServer{pools: map[string]*pool{}} }

// client starts the server (once) and returns a client connected to it.
func (f *fakeServer) client(t *testing.T) *gp.Client {
	t.Helper()
	if f.c != nil {
		return f.c
	}
	f.srv = httptest.NewServer(f)
	t.Cleanup(f.srv.Close)
	c, err := proxmox.NewClient(proxmox.Config{URL: f.srv.URL, TokenID: testTokenID, Secret: testSecret})
	if err != nil {
		t.Fatal(err)
	}
	f.c = c
	return c
}

func newStore(t *testing.T, opts ...Option) (*Store, *fakeServer) {
	t.Helper()
	f := newFake()
	return New(f.client(t), state.PlainCodec{}, opts...), f
}

func (f *fakeServer) record(format string, args ...any) {
	f.calls = append(f.calls, fmt.Sprintf(format, args...))
}

func (f *fakeServer) put(p pool) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if _, ok := f.pools[p.ID]; !ok {
		f.order = append(f.order, p.ID)
	}
	f.pools[p.ID] = &p
}

func (f *fakeServer) has(id string) bool {
	f.mu.Lock()
	defer f.mu.Unlock()
	_, ok := f.pools[id]
	return ok
}

func (f *fakeServer) comment(id string) string {
	f.mu.Lock()
	defer f.mu.Unlock()
	if p, ok := f.pools[id]; ok {
		return p.Comment
	}
	return ""
}

func (f *fakeServer) callLog() []string {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]string(nil), f.calls...)
}

func (f *fakeServer) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	if got := r.Header.Get("Authorization"); got != testAuth {
		http.Error(w, "bad token", http.StatusUnauthorized)
		return
	}
	body, _ := io.ReadAll(r.Body)
	key := r.Method + " " + strings.TrimPrefix(r.URL.EscapedPath(), "/api2/json")
	f.mu.Lock()
	defer f.mu.Unlock()
	switch key {
	case "POST /pools":
		b := decode(body)
		id, _ := b["poolid"].(string)
		comment, _ := b["comment"].(string)
		f.record("create %s", id)
		if _, ok := f.pools[id]; ok {
			writeRawConn(w, 500, fmt.Sprintf("create pool failed: pool '%s' already exists", id), `{"data":null}`)
			return
		}
		if f.tooLong(w, comment) {
			return
		}
		f.pools[id] = &pool{ID: id, Comment: comment}
		f.order = append(f.order, id)
		writeJSON(w, nil)
	case "GET /pools/":
		id := r.URL.Query().Get("poolid")
		f.record("get %s", id)
		if f.failGet {
			// Drop the connection without answering.
			if conn, _, err := w.(http.Hijacker).Hijack(); err == nil {
				_ = conn.Close()
			}
			return
		}
		p, ok := f.pools[id]
		if !ok {
			writeRawConn(w, 500, fmt.Sprintf("pool '%s' does not exist", id), `{"data":null}`)
			return
		}
		writeJSON(w, []any{p.json()})
	case "GET /pools":
		f.record("list")
		if r.URL.RawQuery != "" {
			http.Error(w, "unexpected query "+r.URL.RawQuery, http.StatusBadRequest)
			return
		}
		out := []any{}
		for _, id := range f.order {
			if p, ok := f.pools[id]; ok {
				out = append(out, map[string]any{"poolid": p.ID, "comment": p.Comment})
			}
		}
		writeJSON(w, out)
	case "PUT /pools":
		b := decode(body)
		id, _ := b["poolid"].(string)
		p, ok := f.pools[id]
		if !ok {
			writeRawConn(w, 500, fmt.Sprintf("pool '%s' does not exist", id), `{"data":null}`)
			return
		}
		if c, ok := b["comment"].(string); ok {
			f.record("update %s", id)
			if f.tooLong(w, c) {
				return
			}
			if f.stripNewlines {
				c = strings.ReplaceAll(c, "\n", "")
			}
			p.Comment = c
		}
		if vms, ok := b["vms"].(string); ok {
			var ids []int
			for _, s := range strings.Split(vms, ",") {
				n, _ := strconv.Atoi(s)
				ids = append(ids, n)
			}
			if del, _ := b["delete"].(float64); del == 1 {
				f.record("remove %s %v", id, ids)
				keep := p.Members[:0]
				for _, m := range p.Members {
					if (m.Type == "qemu" || m.Type == "lxc") && containsInt(ids, m.VMID) {
						continue
					}
					keep = append(keep, m)
				}
				p.Members = keep
			} else {
				f.record("add %s %v", id, ids)
			}
		}
		writeJSON(w, nil)
	case "DELETE /pools":
		id := r.URL.Query().Get("poolid")
		f.record("delete %s", id)
		p, ok := f.pools[id]
		if !ok {
			writeRawConn(w, 500, fmt.Sprintf("pool '%s' does not exist", id), `{"data":null}`)
			return
		}
		if len(p.Members) > 0 {
			writeRawConn(w, 500, fmt.Sprintf("delete pool failed: pool '%s' is not empty", id), `{"data":null}`)
			return
		}
		delete(f.pools, id)
		writeJSON(w, nil)
	default:
		http.Error(w, "no route: "+key, http.StatusNotImplemented)
	}
}

// tooLong answers a parameter verification failure for an over-long comment
// and reports whether it did.
func (f *fakeServer) tooLong(w http.ResponseWriter, comment string) bool {
	if f.maxComment <= 0 || len(comment) <= f.maxComment {
		return false
	}
	writeRawConn(w, 400, "Parameter verification failed.",
		fmt.Sprintf(`{"data":null,"errors":{"comment":"value may only be %d characters long"}}`, f.maxComment))
	return true
}

func (p *pool) json() map[string]any {
	out := map[string]any{"poolid": p.ID, "comment": p.Comment}
	if len(p.Members) > 0 {
		var members []map[string]any
		for _, m := range p.Members {
			e := map[string]any{"id": m.ID, "type": m.Type}
			if m.VMID != 0 {
				e["vmid"] = m.VMID
			}
			if m.Storage != "" {
				e["storage"] = m.Storage
			}
			members = append(members, e)
		}
		out["members"] = members
	}
	return out
}

func containsInt(list []int, n int) bool {
	for _, v := range list {
		if v == n {
			return true
		}
	}
	return false
}

func decode(body []byte) map[string]any {
	m := map[string]any{}
	_ = json.Unmarshal(body, &m)
	return m
}

func writeJSON(w http.ResponseWriter, v any) {
	w.Header().Set("Content-Type", "application/json;charset=UTF-8")
	_ = json.NewEncoder(w).Encode(map[string]any{"data": v})
}

// writeRawConn writes a response with a custom status-line reason phrase,
// where Proxmox puts its error messages, by hijacking the connection.
func writeRawConn(w http.ResponseWriter, code int, reason, body string) {
	conn, bufrw, err := w.(http.Hijacker).Hijack()
	if err != nil {
		http.Error(w, reason, code)
		return
	}
	defer func() { _ = conn.Close() }()
	_, _ = fmt.Fprintf(bufrw, "HTTP/1.1 %d %s\r\nContent-Type: application/json;charset=UTF-8\r\nContent-Length: %d\r\nConnection: close\r\n\r\n%s",
		code, reason, len(body), body)
	_ = bufrw.Flush()
}

// routes maps "METHOD /path" (relative to /api2/json, without the query) to
// a handler, for tests that need a specific server answer.
type routes map[string]http.HandlerFunc

func newRouteClient(t *testing.T, rs routes) *gp.Client {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		key := r.Method + " " + strings.TrimPrefix(r.URL.EscapedPath(), "/api2/json")
		h, ok := rs[key]
		if !ok {
			_, _ = io.Copy(io.Discard, r.Body)
			t.Errorf("unexpected request %s (%s)", key, r.URL.RawQuery)
			http.Error(w, "no route", http.StatusNotImplemented)
			return
		}
		h(w, r)
	}))
	t.Cleanup(srv.Close)
	c, err := proxmox.NewClient(proxmox.Config{URL: srv.URL, TokenID: testTokenID, Secret: testSecret})
	if err != nil {
		t.Fatal(err)
	}
	return c
}

// fail returns a handler that answers like Proxmox does for a failed call.
func fail(code int, reason string) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		_, _ = io.Copy(io.Discard, r.Body)
		writeRawConn(w, code, reason, `{"data":null}`)
	}
}

// rawJSON returns a handler that writes body verbatim.
func rawJSON(body string) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		_, _ = io.Copy(io.Discard, r.Body)
		w.Header().Set("Content-Type", "application/json;charset=UTF-8")
		_, _ = io.WriteString(w, body)
	}
}
