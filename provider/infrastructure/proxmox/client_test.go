package proxmox

import (
	"context"
	"encoding/pem"
	"errors"
	"io"
	"log"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	gp "github.com/luthermonson/go-proxmox"
)

func TestNewClientValidation(t *testing.T) {
	cases := []struct {
		name, url, token, secret string
	}{
		{"bad scheme", "ftp://pve:8006", testTokenID, testSecret},
		{"no host", "https:///api2/json", testTokenID, testSecret},
		{"token without realm", "https://pve:8006", "root!klotho", testSecret},
		{"token without id", "https://pve:8006", "root@pam", testSecret},
		{"empty secret", "https://pve:8006", testTokenID, ""},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if _, err := NewClient(Config{URL: tc.url, TokenID: tc.token, Secret: tc.secret}); err == nil {
				t.Fatal("expected error")
			}
		})
	}
	if _, err := NewClient(Config{URL: "https://pve:8006", TokenID: testTokenID, Secret: testSecret, CACert: []byte("not pem")}); err == nil {
		t.Fatal("expected error for invalid CA PEM")
	}
}

func TestBaseURLNormalization(t *testing.T) {
	for in, want := range map[string]string{
		"https://pve:8006":                 "https://pve:8006/api2/json",
		"https://pve:8006/":                "https://pve:8006/api2/json",
		"https://pve:8006/api2/json/":      "https://pve:8006/api2/json",
		"https://pve:8006/api2/json":       "https://pve:8006/api2/json",
		"https://pve:8006/proxy/api2/json": "https://pve:8006/proxy/api2/json",
	} {
		got, err := normalizeBaseURL(in)
		if err != nil || got != want {
			t.Errorf("normalizeBaseURL(%q) = %q, %v; want %q", in, got, err, want)
		}
	}
}

func TestAuthAndHeaders(t *testing.T) {
	p := newRouteServer(t, routes{
		"GET /cluster/nextid": func(w http.ResponseWriter, r *http.Request) {
			// newRouteServer already checks Authorization on every request.
			if got := r.Header.Get("User-Agent"); got != UserAgent {
				t.Errorf("User-Agent = %q", got)
			}
			// Proxmox returns the next ID as a JSON string.
			writeData(t, w, "101")
		},
	})
	if id, err := p.nextID(context.Background()); err != nil || id != 101 {
		t.Fatalf("nextID = %d, %v", id, err)
	}
}

func TestInsecureTLS(t *testing.T) {
	srv := httptest.NewUnstartedServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		writeData(t, w, []map[string]any{{"node": "pve1", "status": "online"}})
	}))
	// The strict client below fails the handshake on purpose; keep the
	// server from logging it.
	srv.Config.ErrorLog = log.New(io.Discard, "", 0)
	srv.StartTLS()
	defer srv.Close()
	ctx := context.Background()
	conn := Config{URL: srv.URL, TokenID: testTokenID, Secret: testSecret}

	strict, err := NewClient(conn)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := strict.Nodes(ctx); err == nil {
		t.Fatal("expected certificate verification failure without Insecure")
	}

	insecure := conn
	insecure.Insecure = true
	c, err := NewClient(insecure)
	if err != nil {
		t.Fatal(err)
	}
	if nodes, err := c.Nodes(ctx); err != nil || len(nodes) != 1 {
		t.Fatalf("insecure Nodes = %v, %v", nodes, err)
	}

	pinned := conn
	pinned.CACert = pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: srv.Certificate().Raw})
	c, err = NewClient(pinned)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := c.Nodes(ctx); err != nil {
		t.Fatalf("CA-pinned Nodes: %v", err)
	}

	// TLS options compose with a caller-supplied client.
	c, err = NewClient(insecure, gp.WithHTTPClient(&http.Client{}))
	if err != nil {
		t.Fatal(err)
	}
	if _, err := c.Nodes(ctx); err != nil {
		t.Fatalf("custom client + insecure Nodes: %v", err)
	}
}

func TestContextCancellation(t *testing.T) {
	p := newRouteServer(t, routes{"GET /nodes": data(t, []any{})})
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if _, err := p.client.Nodes(ctx); !errors.Is(err, context.Canceled) {
		t.Fatalf("err = %v, want context.Canceled", err)
	}
}

func TestAPIErrorStatusAndReason(t *testing.T) {
	p := newRouteServer(t, routes{
		"POST /nodes/pve1/qemu/777/status/start": fail(t, 500, "VM 777 already running"),
	})
	_, err := p.startVM(context.Background(), "pve1", 777)
	apiErr, ok := gp.AsAPIError(err)
	if !ok {
		t.Fatalf("err = %v, want *gp.APIError", err)
	}
	if apiErr.StatusCode != 500 || apiErr.Status != "500 VM 777 already running" || apiErr.Message != "VM 777 already running" {
		t.Errorf("status/message = %d %q %q", apiErr.StatusCode, apiErr.Status, apiErr.Message)
	}
	if apiErr.Method != http.MethodPost || apiErr.Path != "/api2/json/nodes/pve1/qemu/777/status/start" {
		t.Errorf("Method/Path = %s %s", apiErr.Method, apiErr.Path)
	}
	if !strings.Contains(err.Error(), "500 VM 777 already running") {
		t.Errorf("Error() = %q", err.Error())
	}
	if errors.Is(err, errVMNotFound) {
		t.Errorf("misclassified as not found")
	}
}

func TestAPIErrorParameterErrors(t *testing.T) {
	p := newRouteServer(t, routes{
		"POST /nodes/pve1/qemu/9000/clone": func(w http.ResponseWriter, r *http.Request) {
			readBody(t, r)
			writeRaw(t, w, 400, "Parameter verification failed.",
				`{"errors":{"name":"invalid format - value does not look like a valid DNS name\n"},"data":null}`)
		},
	})
	_, err := p.cloneVM(context.Background(), "pve1", 9000, 120, &gp.VirtualMachineCloneOptions{Name: "bad_name"})
	apiErr, ok := gp.AsAPIError(err)
	if !ok {
		t.Fatalf("err = %v, want *gp.APIError", err)
	}
	if apiErr.StatusCode != 400 || apiErr.Status != "400 Parameter verification failed." {
		t.Errorf("status = %d %q", apiErr.StatusCode, apiErr.Status)
	}
	if !strings.Contains(apiErr.Errors["name"], "valid DNS name") {
		t.Errorf("Errors = %v", apiErr.Errors)
	}
	if errors.Is(err, errVMNotFound) || errors.Is(err, errVMExists) {
		t.Errorf("parameter error misclassified: %v", err)
	}
}

func TestAPIErrorUnauthorized(t *testing.T) {
	p := newRouteServer(t, routes{"GET /nodes": fail(t, 401, "invalid token value!")})
	_, err := p.client.Nodes(context.Background())
	apiErr, ok := gp.AsAPIError(err)
	if !ok || apiErr.StatusCode != 401 || apiErr.Message != "invalid token value!" {
		t.Fatalf("err = %#v", err)
	}
}

func TestNodesDecode(t *testing.T) {
	p := newRouteServer(t, routes{
		"GET /nodes": rawJSON(`{"data":[
			{"node":"pve1","status":"online","cpu":0.0421,"maxcpu":16,"mem":8589934592,"maxmem":68719476736,"disk":10737418240,"maxdisk":107374182400,"uptime":86400,"level":"","id":"node/pve1","type":"node","ssl_fingerprint":"AA:BB"},
			{"node":"pve2","status":"offline","id":"node/pve2","type":"node"}
		]}`),
	})
	nodes, err := p.onlineNodes(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if len(nodes) != 1 {
		t.Fatalf("got %d online nodes", len(nodes))
	}
	n := nodes[0]
	if n.Node != "pve1" || n.Status != "online" || n.MaxCPU != 16 || n.MaxMem != 68719476736 || n.Disk != 10737418240 || n.Uptime != 86400 || n.CPU != 0.0421 {
		t.Errorf("node decoded wrong: %+v", n)
	}
}
