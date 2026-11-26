package proxmox

import (
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

const (
	testTokenID = "root@pam!klotho"
	testSecret  = "5f3c9a2e-1b7d-4c8e-9f60-2a4b6c8d0e1f"
	testAuth    = "PVEAPIToken=" + testTokenID + "=" + testSecret
	testUPID    = "UPID:pve1:000ABCDE:0123ABCD:65A1B2C3:qmclone:9000:root@pam!klotho:"
)

// routes maps "METHOD /path" (path relative to /api2/json, as escaped on the
// wire, without the query string) to a handler emulating that endpoint.
type routes map[string]http.HandlerFunc

// newRouteServer starts an httptest server that emulates the Proxmox API
// under /api2/json from fixed routes and returns a provider whose client is
// pointed at it. Requests that match no route fail the test.
func newRouteServer(t *testing.T, rs routes) *Provider {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if got := r.Header.Get("Authorization"); got != testAuth {
			t.Errorf("%s %s: Authorization = %q", r.Method, r.URL, got)
		}
		key := r.Method + " " + strings.TrimPrefix(r.URL.EscapedPath(), "/api2/json")
		h, ok := rs[key]
		if !ok {
			t.Errorf("unexpected request %s (%s)", key, r.URL.RawQuery)
			http.Error(w, "no route", http.StatusNotImplemented)
			return
		}
		h(w, r)
	}))
	t.Cleanup(srv.Close)
	client, err := NewClient(Config{URL: srv.URL + "/api2/json", TokenID: testTokenID, Secret: testSecret})
	if err != nil {
		t.Fatalf("NewClient: %v", err)
	}
	p := New(client, Config{})
	p.taskPoll = 5 * time.Millisecond
	return p
}

// data returns a handler that writes v inside the {"data": ...} envelope.
func data(t *testing.T, v any) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		_, _ = io.Copy(io.Discard, r.Body)
		writeData(t, w, v)
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

// fail returns a handler that answers like Proxmox does for a failed call:
// the message in the status line's reason phrase and a null data body.
func fail(t *testing.T, code int, reason string) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		_, _ = io.Copy(io.Discard, r.Body)
		writeRaw(t, w, code, reason, `{"data":null}`)
	}
}

func writeData(t *testing.T, w http.ResponseWriter, v any) {
	t.Helper()
	w.Header().Set("Content-Type", "application/json;charset=UTF-8")
	if err := json.NewEncoder(w).Encode(map[string]any{"data": v}); err != nil {
		t.Errorf("encoding response: %v", err)
	}
}

// writeRaw writes a response with a custom status-line reason phrase, which
// net/http's server cannot do directly. Proxmox puts its error messages in the
// reason phrase (e.g. "500 VM 100 already exists"), so the emulation hijacks
// the connection to reproduce that. Callers must consume the request body
// first.
func writeRaw(t *testing.T, w http.ResponseWriter, code int, reason, body string) {
	t.Helper()
	if _, ok := w.(http.Hijacker); !ok {
		t.Errorf("response writer cannot hijack")
		return
	}
	writeRawConn(w, code, reason, body)
}

// readBody decodes a JSON request body, failing the test if it is not JSON.
func readBody(t *testing.T, r *http.Request) map[string]any {
	t.Helper()
	if ct := r.Header.Get("Content-Type"); ct != "application/json" {
		t.Errorf("%s %s: Content-Type = %q, want application/json", r.Method, r.URL.Path, ct)
	}
	m := map[string]any{}
	if err := json.NewDecoder(r.Body).Decode(&m); err != nil {
		t.Errorf("%s %s: decoding body: %v", r.Method, r.URL.Path, err)
	}
	return m
}

// wantFields checks that body holds exactly the expected fields. JSON numbers
// decode as float64, so expectations use the same representation.
func wantFields(t *testing.T, body, want map[string]any) {
	t.Helper()
	for k, v := range want {
		if got, ok := body[k]; !ok || got != v {
			t.Errorf("field %s = %#v (present %v), want %#v", k, got, ok, v)
		}
	}
	for k := range body {
		if _, ok := want[k]; !ok {
			t.Errorf("unexpected field %s = %#v", k, body[k])
		}
	}
}
