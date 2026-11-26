package proxmox

import (
	"context"
	"errors"
	"net/http"
	"reflect"
	"testing"

	gp "github.com/luthermonson/go-proxmox"
)

// clusterResourcesVM mirrors GET /cluster/resources?type=vm.
const clusterResourcesVM = `{"data":[
	{"id":"qemu/9000","type":"qemu","vmid":9000,"name":"ubuntu-2404-tmpl","node":"pve1","status":"stopped","template":1,"tags":"klotho-template;ubuntu","maxmem":2147483648,"maxcpu":2,"maxdisk":3758096384,"uptime":0,"cpu":0,"mem":0,"disk":0,"netin":0,"netout":0,"diskread":0,"diskwrite":0},
	{"id":"qemu/101","type":"qemu","vmid":101,"name":"dev-cp-0","node":"pve2","status":"running","template":0,"pool":"klotho-dev","tags":"klotho;dev","maxmem":4294967296,"maxcpu":4,"cpu":0.0153,"mem":1073741824,"uptime":3600},
	{"id":"lxc/200","type":"lxc","vmid":200,"name":"ct-tmpl","node":"pve1","status":"stopped","template":1},
	{"id":"qemu/102","type":"qemu","vmid":102,"name":"plain","node":"pve1","status":"stopped"}
]}`

func TestResources(t *testing.T) {
	p := newRouteServer(t, routes{
		"GET /cluster/resources": func(w http.ResponseWriter, r *http.Request) {
			if got := r.URL.Query().Get("type"); got != "vm" {
				t.Errorf("type = %q, want vm", got)
			}
			rawJSON(clusterResourcesVM)(w, r)
		},
	})
	res, err := p.resources(context.Background(), resourceVM)
	if err != nil {
		t.Fatal(err)
	}
	if len(res) != 4 {
		t.Fatalf("got %d resources", len(res))
	}
	if r := res[0]; r.VMID != 9000 || !isQemuTemplate(r) || r.Name != "ubuntu-2404-tmpl" || !resHasTag(r, "ubuntu") || r.MaxMem != 2147483648 {
		t.Errorf("res[0] = %+v", r)
	}
	if r := res[1]; r.VMID != 101 || isQemuTemplate(r) || r.Pool != "klotho-dev" || r.Node != "pve2" || r.Status != "running" || r.Uptime != 3600 {
		t.Errorf("res[1] = %+v", r)
	}
	if got := resTags(res[1]); !reflect.DeepEqual(got, []string{"klotho", "dev"}) {
		t.Errorf("tags = %v", got)
	}
	if isQemuTemplate(res[2]) {
		t.Errorf("container template counted as a QEMU template: %+v", res[2])
	}
	if isQemuTemplate(res[3]) || len(resTags(res[3])) != 0 {
		t.Errorf("res[3] = %+v", res[3])
	}
}

func TestFindTemplates(t *testing.T) {
	p := newRouteServer(t, routes{"GET /cluster/resources": rawJSON(clusterResourcesVM)})
	tmpls, err := p.findTemplates(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if len(tmpls) != 1 || tmpls[0].VMID != 9000 {
		t.Fatalf("templates = %+v", tmpls)
	}
}

func TestNextID(t *testing.T) {
	// Proxmox returns the next ID as a JSON string.
	p := newRouteServer(t, routes{"GET /cluster/nextid": rawJSON(`{"data":"105"}`)})
	if id, err := p.nextID(context.Background()); err != nil || id != 105 {
		t.Fatalf("nextID = %d, %v", id, err)
	}
}

func TestBridgesDecode(t *testing.T) {
	p := newRouteServer(t, routes{
		"GET /nodes/pve1/network": func(w http.ResponseWriter, r *http.Request) {
			if got := r.URL.Query().Get("type"); got != "bridge" {
				t.Errorf("type = %q, want bridge", got)
			}
			rawJSON(`{"data":[
				{"iface":"vmbr0","type":"bridge","active":1,"autostart":1,"bridge_ports":"eno1","bridge_vlan_aware":1,"cidr":"192.0.2.10/24","gateway":"192.0.2.1","comments":"lan"},
				{"iface":"vmbr1","type":"bridge","bridge_ports":"none"}
			]}`)(w, r)
		},
	})
	got, err := p.client.NodeRef("pve1").Networks(context.Background(), "bridge")
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 2 {
		t.Fatalf("got %d bridges", len(got))
	}
	if b := got[0]; b.Iface != "vmbr0" || b.Active == 0 || b.Autostart == 0 || b.BridgeVLANAware == 0 || b.BridgePorts != "eno1" || b.CIDR != "192.0.2.10/24" || b.Gateway != "192.0.2.1" || b.Comments != "lan" {
		t.Errorf("got[0] = %+v", b)
	}
	if b := got[1]; b.Iface != "vmbr1" || b.Active != 0 || b.BridgeVLANAware != 0 || b.BridgePorts != "none" {
		t.Errorf("got[1] = %+v", b)
	}
	if !hasBridge(got, "vmbr1") || hasBridge(got, "vmbr9") {
		t.Error("hasBridge")
	}
}

func TestPoolLookupClassification(t *testing.T) {
	p := newRouteServer(t, routes{"GET /pools/": fail(t, 500, "pool 'missing' does not exist")})
	_, err := p.client.Pool(context.Background(), "missing")
	if err = classifyPool(err); !errors.Is(err, errPoolNotFound) {
		t.Fatalf("err = %v, want errPoolNotFound", err)
	}
	if _, ok := gp.AsAPIError(err); !ok {
		t.Fatalf("does not wrap APIError: %v", err)
	}

	p = newRouteServer(t, routes{"GET /pools/": data(t, []any{})})
	_, err = p.client.Pool(context.Background(), "missing")
	if err = classifyPool(err); !errors.Is(err, errPoolNotFound) {
		t.Fatalf("empty result: err = %v, want errPoolNotFound", err)
	}

	p = newRouteServer(t, routes{"GET /pools/": fail(t, 403, "Permission check failed (/pool/lab, Pool.Audit)")})
	_, err = p.client.Pool(context.Background(), "lab")
	if err = classifyPool(err); errors.Is(err, errPoolNotFound) {
		t.Fatalf("403 misclassified as errPoolNotFound: %v", err)
	}
	if classifyPool(nil) != nil {
		t.Error("classifyPool(nil) != nil")
	}
}
