package proxmox

import (
	"context"
	"errors"
	"io"
	"net/http"
	"testing"

	gp "github.com/luthermonson/go-proxmox"
)

const vmConfig120 = `{"data":{"name":"dev-cp-0","cores":4,"memory":"4096","tags":"klotho;dev","net0":"virtio=BC:24:11:00:00:01,bridge=vmbr0","agent":"1","digest":"0123abcd"}}`

func TestCloneVM(t *testing.T) {
	p := newRouteServer(t, routes{
		"POST /nodes/pve1/qemu/9000/clone": func(w http.ResponseWriter, r *http.Request) {
			wantFields(t, readBody(t, r), map[string]any{
				"newid":       float64(120),
				"name":        "dev-cp-0",
				"full":        float64(1),
				"storage":     "local-lvm",
				"pool":        "klotho-dev",
				"description": "managed by klotho",
			})
			writeData(t, w, testUPID)
		},
	})
	upid, err := p.cloneVM(context.Background(), "pve1", 9000, 120, &gp.VirtualMachineCloneOptions{
		Name: "dev-cp-0", Full: true, Storage: "local-lvm", Pool: "klotho-dev", Description: "managed by klotho",
	})
	if err != nil {
		t.Fatal(err)
	}
	if upid != testUPID {
		t.Fatalf("upid = %q", upid)
	}
}

func TestCloneVMLinkedOmitsFull(t *testing.T) {
	p := newRouteServer(t, routes{
		"POST /nodes/pve1/qemu/9000/clone": func(w http.ResponseWriter, r *http.Request) {
			wantFields(t, readBody(t, r), map[string]any{"newid": float64(121)})
			writeData(t, w, testUPID)
		},
	})
	if _, err := p.cloneVM(context.Background(), "pve1", 9000, 121, nil); err != nil {
		t.Fatal(err)
	}
}

func TestCloneVMExists(t *testing.T) {
	p := newRouteServer(t, routes{
		"POST /nodes/pve1/qemu/9000/clone": fail(t, 500, "unable to create VM 120: config file already exists"),
	})
	_, err := p.cloneVM(context.Background(), "pve1", 9000, 120, nil)
	if !errors.Is(err, errVMExists) {
		t.Fatalf("err = %v, want errVMExists", err)
	}
	if _, ok := gp.AsAPIError(err); !ok {
		t.Fatalf("does not wrap APIError: %v", err)
	}
}

func TestCloneVMSourceMissing(t *testing.T) {
	p := newRouteServer(t, routes{
		"POST /nodes/pve1/qemu/9999/clone": fail(t, 500, "Configuration file 'nodes/pve1/qemu-server/9999.conf' does not exist"),
	})
	if _, err := p.cloneVM(context.Background(), "pve1", 9999, 120, nil); !errors.Is(err, errVMNotFound) {
		t.Fatalf("err = %v, want errVMNotFound", err)
	}
}

func TestStorageMissingIsNotVMNotFound(t *testing.T) {
	p := newRouteServer(t, routes{
		"POST /nodes/pve1/qemu/9000/clone": fail(t, 500, "storage 'nope' does not exist"),
	})
	_, err := p.cloneVM(context.Background(), "pve1", 9000, 120, &gp.VirtualMachineCloneOptions{Storage: "nope", Full: true})
	if errors.Is(err, errVMNotFound) {
		t.Fatalf("missing storage misclassified as errVMNotFound: %v", err)
	}
	// The library's own classification still sees a missing resource.
	if !errors.Is(err, gp.ErrNotFound) {
		t.Fatalf("err = %v, want gp.ErrNotFound", err)
	}
}

func TestVMNotFound(t *testing.T) {
	p := newRouteServer(t, routes{
		"GET /nodes/pve1/qemu/404/status/current": fail(t, 500, "Configuration file 'nodes/pve1/qemu-server/404.conf' does not exist"),
	})
	if _, err := p.vmStatus(context.Background(), "pve1", 404); !errors.Is(err, errVMNotFound) {
		t.Fatalf("err = %v, want errVMNotFound", err)
	}
}

func TestReadVMConfig(t *testing.T) {
	p := newRouteServer(t, routes{"GET /nodes/pve1/qemu/120/config": rawJSON(vmConfig120)})
	cfg, err := p.readVMConfig(context.Background(), "pve1", 120)
	if err != nil {
		t.Fatal(err)
	}
	if cfg["name"] != "dev-cp-0" || cfg["cores"] != float64(4) || cfg["memory"] != "4096" || cfg["net0"] == nil {
		t.Fatalf("cfg = %v", cfg)
	}
}

func TestSetVMConfigBody(t *testing.T) {
	p := newRouteServer(t, routes{
		"PUT /nodes/pve1/qemu/105/config": func(w http.ResponseWriter, r *http.Request) {
			if r.URL.RawQuery != "" {
				t.Errorf("PUT carried a query string: %q", r.URL.RawQuery)
			}
			wantFields(t, readBody(t, r), map[string]any{
				"tags":      "klotho;cluster-a;control-plane",
				"ide2":      "local:iso/seed-105.iso,media=cdrom",
				"cores":     "4",
				"ipconfig0": "ip=dhcp",
				"agent":     "enabled=1",
			})
			writeData(t, w, nil)
		},
	})
	err := p.setVMConfig(context.Background(), "pve1", 105, map[string]string{
		"tags":      joinTags([]string{"klotho", "cluster-a", "control-plane"}),
		"ide2":      "local:iso/seed-105.iso,media=cdrom",
		"cores":     "4",
		"ipconfig0": "ip=dhcp",
		"agent":     "enabled=1",
	})
	if err != nil {
		t.Fatal(err)
	}
	// Nothing to set makes no request.
	if err := p.setVMConfig(context.Background(), "pve1", 106, nil); err != nil {
		t.Fatal(err)
	}
}

func TestVMStatus(t *testing.T) {
	p := newRouteServer(t, routes{
		"GET /nodes/pve1/qemu/120/status/current": rawJSON(`{"data":{"vmid":120,"name":"dev-cp-0","status":"running","qmpstatus":"running","agent":1,"cpus":4,"cpu":0.02,"mem":1073741824,"maxmem":4294967296,"maxdisk":34359738368,"uptime":42,"pid":12345,"ha":{"managed":0}}}`),
		"GET /nodes/pve1/qemu/120/config":         rawJSON(vmConfig120),
	})
	st, err := p.vmStatus(context.Background(), "pve1", 120)
	if err != nil {
		t.Fatal(err)
	}
	if st.Status != "running" || vmName(st) != "dev-cp-0" || !st.Agent || st.VMID != 120 || st.CPUs != 4 || st.PID != 12345 || st.Uptime != 42 {
		t.Fatalf("status = %+v", st)
	}
	// Tags are absent from status/current here and come from the config.
	tags := vmTags(st)
	if !hasTag(tags, "klotho") || !hasTag(tags, "DEV") {
		t.Errorf("tags = %v", tags)
	}
}

func TestVMPowerActions(t *testing.T) {
	noBody := func(w http.ResponseWriter, r *http.Request) {
		if b, _ := io.ReadAll(r.Body); len(b) != 0 {
			t.Errorf("%s: unexpected body %q", r.URL.Path, b)
		}
		writeData(t, w, testUPID)
	}
	p := newRouteServer(t, routes{
		"POST /nodes/pve1/qemu/120/status/start": noBody,
		"POST /nodes/pve1/qemu/120/status/stop":  noBody,
	})
	ctx := context.Background()
	for name, fn := range map[string]func() (string, error){
		"start": func() (string, error) { return p.startVM(ctx, "pve1", 120) },
		"stop":  func() (string, error) { return p.stopVM(ctx, "pve1", 120) },
	} {
		if upid, err := fn(); err != nil || upid != testUPID {
			t.Errorf("%s = %q, %v", name, upid, err)
		}
	}
}

func TestDeleteVM(t *testing.T) {
	p := newRouteServer(t, routes{
		// The library reads the config first to look for its own cloud-init
		// tag before destroying the VM.
		"GET /nodes/pve1/qemu/120/config": rawJSON(vmConfig120),
		"DELETE /nodes/pve1/qemu/120": func(w http.ResponseWriter, r *http.Request) {
			q := r.URL.Query()
			if q.Get("purge") != "1" || q.Get("destroy-unreferenced-disks") != "1" {
				t.Errorf("query = %v", q)
			}
			writeData(t, w, testUPID)
		},
	})
	if upid, err := p.deleteVM(context.Background(), "pve1", 120, true); err != nil || upid != testUPID {
		t.Fatalf("deleteVM = %q, %v", upid, err)
	}
}

func TestDeleteVMWithoutPurge(t *testing.T) {
	p := newRouteServer(t, routes{
		"GET /nodes/pve1/qemu/120/config": rawJSON(vmConfig120),
		"DELETE /nodes/pve1/qemu/120": func(w http.ResponseWriter, r *http.Request) {
			if r.URL.Query().Has("purge") || r.URL.Query().Has("destroy-unreferenced-disks") {
				t.Errorf("query = %q", r.URL.RawQuery)
			}
			writeData(t, w, testUPID)
		},
	})
	if _, err := p.deleteVM(context.Background(), "pve1", 120, false); err != nil {
		t.Fatal(err)
	}
}

func TestResizeDisk(t *testing.T) {
	p := newRouteServer(t, routes{
		"PUT /nodes/pve1/qemu/120/resize": func(w http.ResponseWriter, r *http.Request) {
			wantFields(t, readBody(t, r), map[string]any{"disk": "scsi0", "size": "20G"})
			writeData(t, w, testUPID)
		},
	})
	if upid, err := p.resizeDisk(context.Background(), "pve1", 120, "scsi0", "20G"); err != nil || upid != testUPID {
		t.Fatalf("resizeDisk = %q, %v", upid, err)
	}
}

func TestAgentInterfaces(t *testing.T) {
	p := newRouteServer(t, routes{
		"GET /nodes/pve1/qemu/120/agent/network-get-interfaces": rawJSON(`{"data":{"result":[
			{"name":"lo","hardware-address":"00:00:00:00:00:00","ip-addresses":[{"ip-address":"127.0.0.1","ip-address-type":"ipv4","prefix":8},{"ip-address":"::1","ip-address-type":"ipv6","prefix":128}],"statistics":{"rx-bytes":100}},
			{"name":"eth0","hardware-address":"bc:24:11:00:00:01","ip-addresses":[{"ip-address":"192.168.1.50","ip-address-type":"ipv4","prefix":24},{"ip-address":"fe80::be24:11ff:fe00:1","ip-address-type":"ipv6","prefix":64}]},
			{"name":"cni0","hardware-address":"aa:bb:cc:dd:ee:ff","ip-addresses":[{"ip-address":"169.254.1.1","ip-address-type":"ipv4","prefix":16}]}
		]}}`),
	})
	ifaces, err := p.agentInterfaces(context.Background(), "pve1", 120)
	if err != nil {
		t.Fatal(err)
	}
	if len(ifaces) != 2 || ifaces[0].Name != "eth0" || ifaces[0].HardwareAddress != "bc:24:11:00:00:01" {
		t.Fatalf("ifaces = %+v (loopback should be dropped)", ifaces)
	}
	if a := ifaces[0].IPAddresses[0]; a.IPAddress != "192.168.1.50" || a.IPAddressType != "ipv4" || a.Prefix != 24 {
		t.Errorf("addr = %+v", a)
	}
	ips := guestIPv4Addresses(ifaces)
	if len(ips) != 1 || ips[0].String() != "192.168.1.50" {
		t.Errorf("guestIPv4Addresses = %v", ips)
	}
}

func TestAgentErrors(t *testing.T) {
	for reason, want := range map[string]error{
		"QEMU guest agent is not running":                                     errAgentNotRunning,
		"VM 120 is not running":                                               errAgentNotRunning,
		"No QEMU guest agent configured":                                      errAgentNotConfigured,
		"Configuration file 'nodes/pve1/qemu-server/120.conf' does not exist": errVMNotFound,
	} {
		p := newRouteServer(t, routes{
			"GET /nodes/pve1/qemu/120/agent/network-get-interfaces": fail(t, 500, reason),
		})
		_, err := p.agentInterfaces(context.Background(), "pve1", 120)
		if !errors.Is(err, want) {
			t.Errorf("%q: err = %v, want %v", reason, err, want)
		}
		if apiErr, ok := gp.AsAPIError(err); !ok || apiErr.StatusCode != 500 {
			t.Errorf("%q: does not wrap APIError: %v", reason, err)
		}
	}
}

func TestVMArgumentValidation(t *testing.T) {
	p := newRouteServer(t, routes{})
	ctx := context.Background()
	if _, err := p.vmStatus(ctx, "", 100); err == nil {
		t.Error("empty node accepted")
	}
	if _, err := p.vmStatus(ctx, "pve1/../x", 100); err == nil {
		t.Error("node with path separator accepted")
	}
	if _, err := p.startVM(ctx, "pve1", 0); err == nil {
		t.Error("zero vmid accepted")
	}
	if _, err := p.cloneVM(ctx, "pve1", 9000, 0, nil); err == nil {
		t.Error("zero newid accepted")
	}
	if _, err := p.cloneVM(ctx, "pve1", 9000, 120, &gp.VirtualMachineCloneOptions{Pool: "bad/pool"}); err == nil {
		t.Error("invalid pool accepted")
	}
	if _, err := p.readVMConfig(ctx, "pve1", -1); err == nil {
		t.Error("negative vmid accepted")
	}
}
