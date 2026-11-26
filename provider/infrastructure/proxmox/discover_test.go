package proxmox

import (
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"reflect"
	"strings"
	"testing"

	"github.com/klotho-k8s/klotho/provider/infrastructure"
)

// TestDiscoverOverHTTP runs Discover against an emulated Proxmox API
// through the real client.
func TestDiscoverOverHTTP(t *testing.T) {
	routes := map[string]string{
		"/nodes": `{"data":[
			{"node":"pve2","status":"online","cpu":0.25,"maxcpu":8,"mem":4294967296,"maxmem":34359738368,"uptime":100},
			{"node":"pve1","status":"online","cpu":0.5,"maxcpu":16,"mem":8589934592,"maxmem":68719476736,"uptime":200},
			{"node":"pve3","status":"offline"}
		]}`,
		"/cluster/resources?type=vm": `{"data":[
			{"id":"qemu/9001","type":"qemu","vmid":9001,"name":"debian-12","node":"pve2","template":1,"tags":"k8s"},
			{"id":"qemu/9000","type":"qemu","vmid":9000,"name":"ubuntu-2404","node":"pve1","template":1,"tags":"k8s;ubuntu","maxdisk":10737418240,"maxmem":2147483648},
			{"id":"qemu/100","type":"qemu","vmid":100,"name":"plain","node":"pve1","template":0}
		]}`,
		"/nodes/pve1/storage": `{"data":[
			{"storage":"local","type":"dir","content":"iso,vztmpl,backup","active":1,"enabled":1,"shared":0,"avail":50,"total":100},
			{"storage":"local-lvm","type":"lvmthin","content":"images,rootdir","active":1,"enabled":1,"shared":0,"avail":200,"total":400},
			{"storage":"pbs","type":"pbs","content":"backup","active":1,"enabled":1}
		]}`,
		"/nodes/pve2/storage": `{"data":[
			{"storage":"nfs","type":"nfs","content":"iso,images","active":1,"enabled":1,"shared":1,"avail":1,"total":2}
		]}`,
		"/nodes/pve1/network?type=bridge": `{"data":[
			{"iface":"vmbr0","type":"bridge","active":1,"bridge_ports":"eno1","cidr":"192.0.2.10/24","gateway":"192.0.2.1"},
			{"iface":"vmbr1","type":"bridge","active":1,"bridge_ports":"none","bridge_vlan_aware":1}
		]}`,
		"/nodes/pve2/network?type=bridge": `{"data":[{"iface":"vmbr0","type":"bridge","active":1}]}`,
	}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = io.Copy(io.Discard, r.Body)
		key := strings.TrimPrefix(r.URL.Path, "/api2/json")
		if r.URL.RawQuery != "" {
			key += "?" + r.URL.RawQuery
		}
		body, ok := routes[key]
		if r.Method != http.MethodGet || !ok {
			t.Errorf("unexpected request %s %s", r.Method, key)
			http.Error(w, "no route", http.StatusNotImplemented)
			return
		}
		if got := r.Header.Get("Authorization"); got != "PVEAPIToken=root@pam!klotho=secret" {
			t.Errorf("Authorization = %q", got)
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = io.WriteString(w, body)
	}))
	defer srv.Close()

	client, err := NewClient(Config{URL: srv.URL, TokenID: "root@pam!klotho", Secret: "secret"})
	if err != nil {
		t.Fatal(err)
	}
	inv, err := New(client, Config{}).Discover(context.Background())
	if err != nil {
		t.Fatal(err)
	}

	ids := func(kind string) []string {
		var out []string
		for _, it := range inv[kind] {
			out = append(out, it.ID)
		}
		return out
	}
	for kind, want := range map[string][]string{
		KindNode:     {"pve1", "pve2"},
		KindTemplate: {"9000", "9001"},
		KindStorage:  {"pve1/local", "pve1/local-lvm", "pve2/nfs"},
		KindBridge:   {"pve1/vmbr0", "pve1/vmbr1", "pve2/vmbr0"},
	} {
		if got := ids(kind); !reflect.DeepEqual(got, want) {
			t.Errorf("%s = %v, want %v", kind, got, want)
		}
	}

	check := func(it infrastructure.Item, want map[string]string) {
		t.Helper()
		for k, v := range want {
			if it.Attrs[k] != v {
				t.Errorf("%s: attr %s = %q, want %q", it.ID, k, it.Attrs[k], v)
			}
		}
	}
	check(inv[KindNode][0], map[string]string{"maxcpu": "16", "maxmem": "68719476736", "freemem": "60129542144", "status": "online"})
	if inv[KindTemplate][0].Name != "ubuntu-2404" {
		t.Errorf("template name = %q", inv[KindTemplate][0].Name)
	}
	check(inv[KindTemplate][0], map[string]string{"vmid": "9000", "node": "pve1", "tags": "k8s;ubuntu"})
	check(inv[KindStorage][0], map[string]string{"node": "pve1", "iso": "true", "images": "false", "type": "dir"})
	check(inv[KindStorage][2], map[string]string{"shared": "true", "iso": "true", "images": "true"})
	check(inv[KindBridge][1], map[string]string{"vlanAware": "true", "ports": "none"})
	check(inv[KindBridge][0], map[string]string{"cidr": "192.0.2.10/24", "gateway": "192.0.2.1"})
}
