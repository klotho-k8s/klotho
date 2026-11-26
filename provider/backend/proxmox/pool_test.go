package proxmox

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"testing"

	gp "github.com/luthermonson/go-proxmox"
)

// These tests pin the wire-level behaviour the store relies on: how Proxmox's
// error phrasings map to the pool sentinels and what each helper sends.

func TestCreatePoolBody(t *testing.T) {
	c := newRouteClient(t, routes{
		"POST /pools": func(w http.ResponseWriter, r *http.Request) {
			body := map[string]any{}
			if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
				t.Errorf("decoding body: %v", err)
			}
			if body["poolid"] != "klotho-dev" || body["comment"] != `{"v":1}` || len(body) != 2 {
				t.Errorf("body = %v", body)
			}
			writeJSON(w, nil)
		},
	})
	if err := createPool(context.Background(), c, "klotho-dev", `{"v":1}`); err != nil {
		t.Fatal(err)
	}
}

func TestCreatePoolExists(t *testing.T) {
	c := newRouteClient(t, routes{
		"POST /pools": fail(500, "create pool failed: pool 'klotho-dev' already exists"),
	})
	err := createPool(context.Background(), c, "klotho-dev", "")
	if !errors.Is(err, errPoolExists) {
		t.Fatalf("err = %v, want errPoolExists", err)
	}
	if apiErr, ok := gp.AsAPIError(err); !ok || apiErr.StatusCode != 500 {
		t.Fatalf("errPoolExists does not wrap the APIError: %v", err)
	}
}

func TestCreatePoolOtherErrorNotClassified(t *testing.T) {
	c := newRouteClient(t, routes{
		"POST /pools": fail(403, "Permission check failed (/pool/klotho-dev, Pool.Allocate)"),
	})
	err := createPool(context.Background(), c, "klotho-dev", "")
	if errors.Is(err, errPoolExists) {
		t.Fatalf("403 misclassified as errPoolExists: %v", err)
	}
	if apiErr, ok := gp.AsAPIError(err); !ok || apiErr.StatusCode != 403 {
		t.Fatalf("err = %v", err)
	}
}

func TestPoolIDValidation(t *testing.T) {
	c := newRouteClient(t, routes{})
	ctx := context.Background()
	for _, id := range []string{"", "a/b", "has space", "x?y", "ünï"} {
		if err := createPool(ctx, c, id, ""); err == nil {
			t.Errorf("createPool(%q) accepted invalid ID", id)
		}
		if _, err := getPool(ctx, c, id); err == nil {
			t.Errorf("getPool(%q) accepted invalid ID", id)
		}
		if err := deletePool(ctx, c, id); err == nil {
			t.Errorf("deletePool(%q) accepted invalid ID", id)
		}
	}
	for _, id := range []string{"klotho", "k8s.dev-01_a", "A1"} {
		if err := validatePoolID(id); err != nil {
			t.Errorf("validatePoolID(%q) = %v", id, err)
		}
	}
}

func TestGetPoolNotFound(t *testing.T) {
	c := newRouteClient(t, routes{"GET /pools/": fail(500, "pool 'missing' does not exist")})
	_, err := getPool(context.Background(), c, "missing")
	if !errors.Is(err, errPoolNotFound) {
		t.Fatalf("err = %v, want errPoolNotFound", err)
	}
	if _, ok := gp.AsAPIError(err); !ok {
		t.Fatalf("does not wrap APIError: %v", err)
	}
}

func TestGetPoolEmptyResult(t *testing.T) {
	c := newRouteClient(t, routes{"GET /pools/": rawJSON(`{"data":[]}`)})
	if _, err := getPool(context.Background(), c, "missing"); !errors.Is(err, errPoolNotFound) {
		t.Fatalf("err = %v, want errPoolNotFound", err)
	}
}

func TestGetPoolMembers(t *testing.T) {
	c := newRouteClient(t, routes{
		"GET /pools/": func(w http.ResponseWriter, r *http.Request) {
			if got := r.URL.Query().Get("poolid"); got != "klotho-dev" {
				t.Errorf("poolid query = %q", got)
			}
			rawJSON(`{"data":[{"poolid":"klotho-dev","comment":"{\"cluster\":\"dev\"}","members":[
				{"id":"qemu/120","node":"pve1","type":"qemu","vmid":120,"name":"dev-cp-0","status":"running","template":0,"tags":"klotho;dev","maxmem":4294967296,"uptime":300},
				{"id":"qemu/121","node":"pve2","type":"qemu","vmid":121,"name":"dev-worker-0","status":"stopped"},
				{"id":"storage/pve1/local","node":"pve1","type":"storage","storage":"local","status":"available","maxdisk":100000000000}
			]}]}`)(w, r)
		},
	})
	p, err := getPool(context.Background(), c, "klotho-dev")
	if err != nil {
		t.Fatal(err)
	}
	if p.PoolID != "klotho-dev" || p.Comment != `{"cluster":"dev"}` {
		t.Errorf("pool = %+v", p)
	}
	if len(p.Members) != 3 {
		t.Fatalf("members = %d", len(p.Members))
	}
	m := p.Members[0]
	if m.Type != "qemu" || m.VMID != 120 || m.Node != "pve1" || m.Name != "dev-cp-0" || m.Status != "running" || m.Template != 0 {
		t.Errorf("member 0 = %+v", m)
	}
	if p.Members[1].VMID != 121 || p.Members[1].Node != "pve2" {
		t.Errorf("member 1 = %+v", p.Members[1])
	}
	if s := p.Members[2]; s.Type != "storage" || s.Storage != "local" || s.VMID != 0 {
		t.Errorf("member 2 = %+v", s)
	}
}

func TestSetCommentBody(t *testing.T) {
	c := newRouteClient(t, routes{
		"GET /pools/": rawJSON(`{"data":[{"poolid":"klotho-dev","comment":"old"}]}`),
		"PUT /pools": func(w http.ResponseWriter, r *http.Request) {
			body := map[string]any{}
			if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
				t.Errorf("decoding body: %v", err)
			}
			if body["poolid"] != "klotho-dev" || body["comment"] != `{"v":2}` || len(body) != 2 {
				t.Errorf("body = %v", body)
			}
			writeJSON(w, nil)
		},
	})
	if err := setComment(context.Background(), c, "klotho-dev", `{"v":2}`); err != nil {
		t.Fatal(err)
	}
}

func TestSetCommentNotFound(t *testing.T) {
	c := newRouteClient(t, routes{"GET /pools/": fail(500, "pool 'gone' does not exist")})
	if err := setComment(context.Background(), c, "gone", "x"); !errors.Is(err, errPoolNotFound) {
		t.Fatalf("err = %v, want errPoolNotFound", err)
	}
}

func TestDeletePoolQuery(t *testing.T) {
	c := newRouteClient(t, routes{
		"DELETE /pools": func(w http.ResponseWriter, r *http.Request) {
			if got := r.URL.Query().Get("poolid"); got != "klotho-dev" {
				t.Errorf("poolid = %q", got)
			}
			writeJSON(w, nil)
		},
	})
	if err := deletePool(context.Background(), c, "klotho-dev"); err != nil {
		t.Fatal(err)
	}
}

func TestDeletePoolNotEmpty(t *testing.T) {
	c := newRouteClient(t, routes{
		"DELETE /pools": fail(500, "delete pool failed: pool 'klotho-dev' is not empty"),
	})
	err := deletePool(context.Background(), c, "klotho-dev")
	if _, ok := gp.AsAPIError(err); !ok || errors.Is(err, errPoolNotFound) {
		t.Fatalf("err = %v", err)
	}
}

func TestDeletePoolNotFound(t *testing.T) {
	c := newRouteClient(t, routes{"DELETE /pools": fail(500, "pool 'gone' does not exist")})
	if err := deletePool(context.Background(), c, "gone"); !errors.Is(err, errPoolNotFound) {
		t.Fatalf("err = %v, want errPoolNotFound", err)
	}
}
