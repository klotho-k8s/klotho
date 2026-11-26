package proxmox

import (
	"context"
	"reflect"
	"strconv"
	"strings"
	"testing"

	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"

	"github.com/klotho-k8s/klotho/pkg/generate"
)

func tmpl(vmid int, node string, tags ...string) vmRes {
	return vmRes{ID: "qemu/" + strconv.Itoa(vmid), Type: "qemu", VMID: vmid, Name: "t" + strconv.Itoa(vmid), Node: node, Status: "stopped", Template: true, Tags: tags}
}

func TestSelectTemplate(t *testing.T) {
	templates := resourcesOf([]vmRes{
		tmpl(9000, "pve1", "k8s", "ubuntu"),
		tmpl(9001, "pve1", "k8s", "ubuntu", "nightly"),
		tmpl(9002, "pve1", "k8s", "ubuntu", "nightly", "gpu"),
		tmpl(9003, "pve1", "K8S", "Ubuntu"), // same tags as 9000, differently cased
		tmpl(9100, "pve2", "k8s", "ubuntu"),
		{ID: "qemu/100", Type: "qemu", VMID: 100, Name: "vm", Node: "pve1", Status: "running", Tags: []string{"k8s", "ubuntu"}},
		{ID: "lxc/200", Type: "lxc", VMID: 200, Name: "ct", Node: "pve1", Template: true, Tags: []string{"k8s", "ubuntu"}},
	})
	for name, tc := range map[string]struct {
		node   string
		tags   []string
		policy string
		want   int
		err    string
	}{
		"exact on pve2":                {"pve2", []string{"k8s", "ubuntu"}, "", 9100, ""},
		"exact ignores order and case": {"pve2", []string{"UBUNTU", "k8s"}, "exact", 9100, ""},
		"exact ambiguous":              {"pve1", []string{"k8s", "ubuntu"}, "", 0, "2 templates on node \"pve1\" match tags [k8s ubuntu] with policy exact: VM 9000 (t9000) tags [k8s ubuntu], VM 9003 (t9003) tags [K8S Ubuntu]; give one"},
		"exact none":                   {"pve1", []string{"k8s", "ubuntu", "gpu"}, "", 0, "no template on node \"pve1\" matches tags [k8s ubuntu gpu] with policy exact; templates there: VM 9000"},
		"exact wrong node":             {"pve3", []string{"k8s", "ubuntu"}, "", 0, "the node has no templates"},
		"uniqueSubset":                 {"pve1", []string{"gpu"}, "uniqueSubset", 9002, ""},
		"uniqueSubset ambiguous":       {"pve1", []string{"nightly"}, "uniqueSubset", 0, "2 templates on node \"pve1\" match tags [nightly] with policy uniqueSubset"},
		"bestSubset":                   {"pve1", []string{"nightly"}, "bestSubset", 9001, ""},
		"bestSubset exact wins":        {"pve1", []string{"k8s", "ubuntu", "nightly"}, "bestSubset", 9001, ""},
		"bestSubset tie":               {"pve1", []string{"k8s"}, "bestSubset", 0, "2 templates on node \"pve1\" match tags [k8s] with policy bestSubset: VM 9000 (t9000) tags [k8s ubuntu], VM 9003"},
		"bad policy":                   {"pve1", []string{"k8s"}, "fuzzy", 0, "not one of"},
	} {
		t.Run(name, func(t *testing.T) {
			got, err := selectTemplate(tc.node, templates, tc.tags, tc.policy)
			if tc.err != "" {
				if err == nil || !strings.Contains(err.Error(), tc.err) {
					t.Fatalf("err = %v, want containing %q", err, tc.err)
				}
				return
			}
			if err != nil {
				t.Fatal(err)
			}
			if int(got.VMID) != tc.want {
				t.Fatalf("got VM %d; want %d", got.VMID, tc.want)
			}
		})
	}
}

func TestResolveTemplateTags(t *testing.T) {
	r, err := newTestProvider(t, newFake(), Config{TemplateTags: []string{"k8s", "ubuntu"}, TemplateTag: "ignored"}).Resolve(testSpec(map[string]string{VarTemplateID: ""}))
	if err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(r.TemplateTags, []string{"k8s", "ubuntu"}) || r.TemplateMatchPolicy != generate.TemplateMatchExact || r.TemplateID != 0 || r.TemplateTag != "" {
		t.Errorf("tags: %+v", r)
	}
	// TEMPLATE_VMID is ignored with tags; a configured TemplateID is kept.
	r, err = newTestProvider(t, newFake(), Config{TemplateTags: []string{"k8s"}, TemplateMatchPolicy: "bestSubset", TemplateID: 9000}).Resolve(testSpec(map[string]string{VarTemplateID: "9999"}))
	if err != nil || r.TemplateID != 9000 || r.TemplateMatchPolicy != "bestSubset" {
		t.Errorf("id with tags: %+v, %v", r, err)
	}
	for name, cfg := range map[string]Config{
		"bad tag":    {TemplateTags: []string{"a b"}},
		"bad policy": {TemplateTags: []string{"k8s"}, TemplateMatchPolicy: "fuzzy"},
	} {
		if _, err := newTestProvider(t, newFake(), cfg).Resolve(testSpec(nil)); err == nil {
			t.Errorf("%s accepted", name)
		}
	}
}

func TestPrepareByTags(t *testing.T) {
	ctx := context.Background()
	newFakeWithTemplates := func() *fake {
		f := newFake()
		f.vms = append(f.vms,
			tmpl(9001, "pve1", "k8s", "ubuntu", "nightly"),
			tmpl(9100, "pve2", "k8s", "ubuntu"),
			tmpl(9300, "pve3", "k8s", "ubuntu", "gpu"), // pve3 is offline
		)
		return f
	}

	t.Run("configured node", func(t *testing.T) {
		f := newFakeWithTemplates()
		p := newTestProvider(t, f, Config{Node: "pve2", TemplateTags: []string{"k8s", "ubuntu"}})
		got, err := p.Prepare(ctx, testSpec(map[string]string{VarTemplateID: ""}))
		if err != nil {
			t.Fatal(err)
		}
		want := map[string]string{VarTemplateID: "9100", VarSourceNode: "pve2"}
		if !reflect.DeepEqual(got.Variables, want) {
			t.Errorf("variables = %v, want %v", got.Variables, want)
		}
		if ops := f.ops(); len(ops) != 0 {
			t.Errorf("Prepare changed something: %v", ops)
		}
		if seen, ok := p.templates(); !ok || len(seen) != 4 {
			t.Errorf("remembered templates: %v %v", seen, ok)
		}
	})

	t.Run("first online node by name", func(t *testing.T) {
		f := newFakeWithTemplates()
		p := newTestProvider(t, f, Config{TemplateTags: []string{"k8s", "ubuntu"}})
		got, err := p.Prepare(ctx, testSpec(map[string]string{VarTemplateID: ""}))
		if err != nil {
			t.Fatal(err)
		}
		if got.Variables[VarTemplateID] != "9000" || got.Variables[VarSourceNode] != "pve1" {
			t.Errorf("variables = %v", got.Variables)
		}
	})

	t.Run("ALLOWED_NODES picks the node", func(t *testing.T) {
		f := newFakeWithTemplates()
		p := newTestProvider(t, f, Config{TemplateTags: []string{"k8s", "ubuntu"}})
		got, err := p.Prepare(ctx, testSpec(map[string]string{VarTemplateID: "", VarAllowedNodes: "[pve2]"}))
		if err != nil || got.Variables[VarTemplateID] != "9100" {
			t.Errorf("variables = %v, %v", got.Variables, err)
		}
	})

	t.Run("skips nodes where the policy fails", func(t *testing.T) {
		f := newFakeWithTemplates()
		f.vms = append(f.vms, tmpl(9003, "pve1", "k8s", "ubuntu")) // pve1 now ambiguous
		p := newTestProvider(t, f, Config{TemplateTags: []string{"k8s", "ubuntu"}})
		got, err := p.Prepare(ctx, testSpec(map[string]string{VarTemplateID: ""}))
		if err != nil || got.Variables[VarTemplateID] != "9100" || got.Variables[VarSourceNode] != "pve2" {
			t.Errorf("variables = %v, %v", got.Variables, err)
		}
	})

	t.Run("templateId agreeing with the tags", func(t *testing.T) {
		f := newFakeWithTemplates()
		p := newTestProvider(t, f, Config{Node: "pve1", TemplateID: 9001, TemplateTags: []string{"nightly"}, TemplateMatchPolicy: "uniqueSubset"})
		if _, err := p.Prepare(ctx, testSpec(nil)); err != nil {
			t.Fatal(err)
		}
	})

	t.Run("launch clones the selected template", func(t *testing.T) {
		f := newFakeWithTemplates()
		f.agent = agentWithIP("192.0.2.60")
		f.configs[9100] = map[string]any{"name": "t9100", "template": float64(1), "smbios1": "uuid=" + templateUUID, "net0": "virtio=BC:24:11:00:00:02,bridge=vmbr0"}
		p := newTestProvider(t, f, Config{Node: "pve2", TemplateTags: []string{"k8s", "ubuntu"}})
		if _, err := p.Launch(ctx, testSpec(map[string]string{VarTemplateID: ""}), []byte("#cloud-config\n")); err != nil {
			t.Fatal(err)
		}
		var cloned bool
		for _, op := range f.ops() {
			if strings.HasPrefix(op, "clone 9100->") {
				cloned = true
			}
		}
		if !cloned {
			t.Errorf("template 9100 was not cloned: %v", f.ops())
		}
	})

	for name, tc := range map[string]struct {
		cfg   Config
		setup func(*fake)
		want  string
	}{
		"none on configured node": {
			cfg:  Config{Node: "pve2", TemplateTags: []string{"gpu"}},
			want: `no template on node "pve2" matches tags [gpu] with policy exact; templates there: VM 9100 (t9100) tags [k8s ubuntu]`,
		},
		"ambiguous on configured node": {
			cfg:  Config{Node: "pve1", TemplateTags: []string{"k8s"}, TemplateMatchPolicy: "uniqueSubset"},
			want: `2 templates on node "pve1" match tags [k8s] with policy uniqueSubset: VM 9000 (ubuntu-2404) tags [k8s ubuntu], VM 9001 (t9001) tags [k8s ubuntu nightly]`,
		},
		"none on any online node": {
			cfg:  Config{TemplateTags: []string{"gpu"}},
			want: `no online node has a template matching tags [gpu]: proxmox provider: no template on node "pve1"`,
		},
		"templateId disagreeing with the tags": {
			cfg:  Config{Node: "pve1", TemplateID: 9000, TemplateTags: []string{"nightly"}, TemplateMatchPolicy: "uniqueSubset"},
			want: `templateId 9000 is set but tags [nightly] select VM 9001 (t9001) tags [k8s ubuntu nightly] on node "pve1"`,
		},
	} {
		t.Run(name, func(t *testing.T) {
			f := newFakeWithTemplates()
			if tc.setup != nil {
				tc.setup(f)
			}
			_, err := newTestProvider(t, f, tc.cfg).Prepare(ctx, testSpec(map[string]string{VarTemplateID: ""}))
			if err == nil || !strings.Contains(err.Error(), tc.want) {
				t.Fatalf("err = %v, want containing %q", err, tc.want)
			}
		})
	}
}

func TestCheckTemplateSelectors(t *testing.T) {
	bundle := func(allowed []string, policy string) []*unstructured.Unstructured {
		spec := map[string]any{
			"templateSelector": map[string]any{"matchTags": []any{"k8s", "ubuntu"}},
			"tags":             []any{"klotho-demo"},
		}
		if policy != "" {
			spec["templateMatchPolicy"] = policy
		}
		pxcSpec := map[string]any{}
		if allowed != nil {
			pxcSpec["allowedNodes"] = toAny(allowed)
		}
		return []*unstructured.Unstructured{
			{Object: map[string]any{
				"apiVersion": "infrastructure.cluster.x-k8s.io/v1alpha1", "kind": "ProxmoxCluster",
				"metadata": map[string]any{"name": "demo"}, "spec": pxcSpec,
			}},
			{Object: map[string]any{
				"apiVersion": "infrastructure.cluster.x-k8s.io/v1alpha1", "kind": "ProxmoxMachineTemplate",
				"metadata": map[string]any{"name": "demo-control-plane"},
				"spec":     map[string]any{"template": map[string]any{"spec": spec}},
			}},
			{Object: map[string]any{
				"apiVersion": "infrastructure.cluster.x-k8s.io/v1alpha1", "kind": "ProxmoxMachineTemplate",
				"metadata": map[string]any{"name": "demo-worker"},
				"spec":     map[string]any{"template": map[string]any{"spec": map[string]any{"sourceNode": "pve1", "templateID": int64(9000)}}},
			}},
		}
	}
	// The cluster has one template matching [k8s ubuntu] exactly, 9100 on
	// pve2, and a nightly one on pve1 that only subset policies see. storage
	// is where 9100's disks live.
	prepared := func(t *testing.T, storage string) *Provider {
		t.Helper()
		f := newFake()
		f.vms = []vmRes{tmpl(9001, "pve1", "k8s", "ubuntu", "nightly"), tmpl(9100, "pve2", "k8s", "ubuntu")}
		f.configs[9100] = map[string]any{"scsi0": storage + ":base-9100-disk-0,size=4G", "efidisk0": storage + ":base-9100-disk-1,efitype=4m", "ide0": "none,media=cdrom"}
		for _, node := range []string{"pve1", "pve2"} {
			f.storages[node] = append(f.storages[node], storageRes{Storage: "nfs-shared", Type: "nfs", Content: []string{"images"}, Active: true, Enabled: true, Shared: true})
		}
		p := newTestProvider(t, f, Config{Node: "pve2", TemplateTags: []string{"k8s", "ubuntu"}})
		if _, err := p.Prepare(context.Background(), testSpec(map[string]string{VarTemplateID: ""})); err != nil {
			t.Fatal(err)
		}
		return p
	}

	t.Run("only the template's node is allowed", func(t *testing.T) {
		if got := prepared(t, "local-lvm").Check(bundle([]string{"pve2"}, "")); len(got) != 0 {
			t.Fatalf("findings = %v", got)
		}
	})
	t.Run("other nodes allowed, template on local storage", func(t *testing.T) {
		got := prepared(t, "local-lvm").Check(bundle([]string{"pve1", "pve2", "pve3"}, ""))
		if len(got) != 1 {
			t.Fatalf("findings = %v", got)
		}
		f := got[0]
		if f.Object != "ProxmoxMachineTemplate/demo-control-plane" || f.Field != "spec.template.spec.templateSelector" ||
			!strings.Contains(f.Message, `select VM 9100 (t9100) tags [k8s ubuntu] on node "pve2", whose disks are on local-lvm, not shared storage`) ||
			!strings.Contains(f.Message, `placed on "pve1" or "pve3" cannot be cloned`) ||
			!strings.Contains(f.Message, `restrict spec.allowedNodes of ProxmoxCluster/demo to "pve2"`) {
			t.Fatalf("finding = %+v", f)
		}
	})
	t.Run("other nodes allowed, template on shared storage", func(t *testing.T) {
		if got := prepared(t, "nfs-shared").Check(bundle([]string{"pve1", "pve2", "pve3"}, "")); len(got) != 0 {
			t.Fatalf("findings = %v", got)
		}
	})
	t.Run("template carries a Proxmox cloud-init drive", func(t *testing.T) {
		f := newFake()
		f.vms = []vmRes{tmpl(9100, "pve2", "k8s", "ubuntu")}
		f.configs[9100] = map[string]any{"scsi0": "local-lvm:base-9100-disk-0,size=4G", "ide2": "local-lvm:vm-9100-cloudinit,media=cdrom"}
		p := newTestProvider(t, f, Config{Node: "pve2", TemplateTags: []string{"k8s", "ubuntu"}})
		if _, err := p.Prepare(context.Background(), testSpec(map[string]string{VarTemplateID: ""})); err != nil {
			t.Fatal(err)
		}
		got := p.Check(bundle([]string{"pve2"}, ""))
		if len(got) != 1 || !strings.Contains(got[0].Message, "carries a Proxmox cloud-init drive (ide2=local-lvm:vm-9100-cloudinit)") ||
			!strings.Contains(got[0].Message, "qm set 9100 --delete ide2") {
			t.Fatalf("findings = %v", got)
		}
	})
	t.Run("ambiguity under the bundle's policy", func(t *testing.T) {
		got := prepared(t, "local-lvm").Check(bundle([]string{"pve2"}, "uniqueSubset"))
		if len(got) != 1 || !strings.Contains(got[0].Message, "2 templates in the cluster match tags [k8s ubuntu] with policy uniqueSubset") ||
			!strings.Contains(got[0].Message, "exactly one match") {
			t.Fatalf("findings = %v", got)
		}
	})
	t.Run("no allowed nodes", func(t *testing.T) {
		if got := prepared(t, "local-lvm").Check(bundle(nil, "")); len(got) != 0 {
			t.Fatalf("findings = %v", got)
		}
	})
	t.Run("nothing remembered", func(t *testing.T) {
		if got := newTestProvider(t, newFake(), Config{}).Check(bundle([]string{"pve9"}, "")); len(got) != 0 {
			t.Fatalf("findings = %v", got)
		}
	})
}

func TestPrepareByTagsSharedStorageFallback(t *testing.T) {
	// The only matching template is on pve1; the bootstrap node is pve2.
	newFakeShared := func(storage string) *fake {
		f := newFake()
		f.configs[9000] = map[string]any{"scsi0": storage + ":base-9000-disk-0,size=4G", "ide2": storage + ":vm-9000-cloudinit,media=cdrom"}
		for _, node := range []string{"pve1", "pve2"} {
			f.storages[node] = append(f.storages[node], storageRes{Storage: "nfs-shared", Type: "nfs", Content: []string{"images"}, Active: true, Enabled: true, Shared: true})
		}
		return f
	}
	t.Run("shared storage: cloned across nodes", func(t *testing.T) {
		p := newTestProvider(t, newFakeShared("nfs-shared"), Config{Node: "pve2", TemplateTags: []string{"k8s", "ubuntu"}})
		got, err := p.Prepare(context.Background(), testSpec(map[string]string{VarTemplateID: ""}))
		if err != nil {
			t.Fatal(err)
		}
		if got.Variables[VarTemplateID] != "9000" {
			t.Errorf("variables = %v", got.Variables)
		}
	})
	t.Run("local storage: refused with the reason", func(t *testing.T) {
		p := newTestProvider(t, newFakeShared("local-lvm"), Config{Node: "pve2", TemplateTags: []string{"k8s", "ubuntu"}})
		_, err := p.Prepare(context.Background(), testSpec(map[string]string{VarTemplateID: ""}))
		if err == nil || !strings.Contains(err.Error(), `no template on node "pve2" matches`) ||
			!strings.Contains(err.Error(), `VM 9000 (ubuntu-2404) tags [k8s ubuntu] on node "pve1" matches, but its disks are on local-lvm, which is not shared storage, so it cannot be cloned to "pve2"`) {
			t.Fatalf("err = %v", err)
		}
	})
}

func TestDiskStorages(t *testing.T) {
	cfg := map[string]any{
		"scsi0":    "nfs-klotho:9020/base-9020-disk-0.qcow2,discard=on,size=3584M,ssd=1",
		"ide2":     "nfs-klotho:9020/vm-9020-cloudinit.qcow2,media=cdrom,size=4M",
		"ide0":     "local:iso/seed.iso,media=cdrom",
		"ide1":     "none,media=cdrom",
		"efidisk0": "local-lvm:vm-9020-disk-1,efitype=4m",
		"scsihw":   "virtio-scsi-single",
		"net0":     "virtio=BC:24:11:00:00:01,bridge=vmbr0",
		"name":     "scsi0 is not a drive here",
	}
	if got := diskStorages(cfg); !reflect.DeepEqual(got, []string{"local", "local-lvm", "nfs-klotho"}) {
		t.Fatalf("diskStorages = %v", got)
	}
	if diskStorages(map[string]any{}) != nil {
		t.Fatal("empty config should give nil")
	}
	if got := cloudInitDrives(cfg); !reflect.DeepEqual(got, []string{"ide2=nfs-klotho:9020/vm-9020-cloudinit.qcow2"}) {
		t.Fatalf("cloudInitDrives = %v", got)
	}
	delete(cfg, "ide2")
	if got := cloudInitDrives(cfg); got != nil {
		t.Fatalf("cloudInitDrives = %v, want none", got)
	}
	cfg["ide2"] = "nfs-klotho:9020/vm-9020-cloudinit.qcow2,media=cdrom,size=4M"
	cfg["sata1"] = "local-lvm:vm-9020-cloudinit,media=cdrom"
	if got := cloudInitDrives(cfg); !reflect.DeepEqual(got, []string{"ide2=nfs-klotho:9020/vm-9020-cloudinit.qcow2", "sata1=local-lvm:vm-9020-cloudinit"}) {
		t.Fatalf("cloudInitDrives = %v", got)
	}
}
