package cloudinit

import (
	"bytes"
	"encoding/base64"
	"io"
	"os"
	"path/filepath"
	"reflect"
	"runtime"
	"strings"
	"testing"

	"github.com/kdomanski/iso9660"
	"sigs.k8s.io/yaml"

	"github.com/klotho-k8s/klotho/provider/controlplane"
	"github.com/klotho-k8s/klotho/provider/payload"
)

func twoContribs() []controlplane.Contribution {
	return []controlplane.Contribution{
		{
			Files: []controlplane.File{
				{Path: "/etc/a.conf", Content: []byte("alpha\nbeta\n")},
				{Path: "/etc/secret.key", Content: []byte("key"), Mode: "0600", Owner: "app:app"},
			},
			PreCommands:     []string{"pre0"},
			InstallCommands: []string{"install0"},
			PostCommands:    []string{"post0"},
		},
		{
			Files: []controlplane.File{
				{Path: "/etc/b.conf", Content: []byte("gamma")},
				{Path: "/etc/a.conf", Content: []byte("more\n"), Append: true},
			},
			PreCommands:     []string{"pre1"},
			InstallCommands: []string{"install1"},
			PostCommands:    []string{"post1"},
		},
	}
}

func writeFiles(t *testing.T, doc map[string]any) []map[string]any {
	t.Helper()
	raw, _ := doc["write_files"].([]any)
	out := make([]map[string]any, 0, len(raw))
	for _, r := range raw {
		out = append(out, r.(map[string]any))
	}
	return out
}

func strs(v any) []string {
	raw, _ := v.([]any)
	out := make([]string, 0, len(raw))
	for _, r := range raw {
		out = append(out, r.(string))
	}
	return out
}

func TestBuildMerges(t *testing.T) {
	b, err := Build(twoContribs(), Options{
		Hostname:          "node1.example.com",
		SSHAuthorizedKeys: []string{"ssh-ed25519 AAAA test@host"},
		PackageUpdate:     true,
		Timezone:          "UTC",
	})
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.HasPrefix(b.UserData, []byte("#cloud-config\n")) {
		t.Fatalf("user-data header: %q", strings.SplitN(string(b.UserData), "\n", 2)[0])
	}
	doc, err := Parse(b.UserData)
	if err != nil {
		t.Fatal(err)
	}

	wantCmd := []string{"pre0", "pre1", "install0", "install1", "post0", "post1"}
	if got := strs(doc["runcmd"]); !reflect.DeepEqual(got, wantCmd) {
		t.Errorf("runcmd = %v, want %v", got, wantCmd)
	}
	if got := strs(doc["ssh_authorized_keys"]); len(got) != 1 || got[0] != "ssh-ed25519 AAAA test@host" {
		t.Errorf("ssh_authorized_keys = %v", got)
	}
	if doc["hostname"] != "node1" || doc["fqdn"] != "node1.example.com" || doc["preserve_hostname"] != false {
		t.Errorf("hostname fields: %v %v %v", doc["hostname"], doc["fqdn"], doc["preserve_hostname"])
	}
	if doc["package_update"] != true || doc["timezone"] != "UTC" {
		t.Errorf("package_update/timezone: %v %v", doc["package_update"], doc["timezone"])
	}

	files := writeFiles(t, doc)
	if len(files) != 4 {
		t.Fatalf("write_files has %d entries, want 4", len(files))
	}
	first := files[0]
	if first["path"] != "/etc/a.conf" || first["content"] != "alpha\nbeta\n" ||
		first["permissions"] != "0644" || first["owner"] != "root:root" {
		t.Errorf("defaults not applied: %v", first)
	}
	if _, ok := first["encoding"]; ok {
		t.Errorf("text file should not be encoded: %v", first)
	}
	if files[1]["permissions"] != "0600" || files[1]["owner"] != "app:app" {
		t.Errorf("explicit mode/owner lost: %v", files[1])
	}
	if files[3]["path"] != "/etc/a.conf" || files[3]["append"] != true {
		t.Errorf("append entry: %v", files[3])
	}
}

func TestBuildDuplicatePath(t *testing.T) {
	contribs := []controlplane.Contribution{
		{Files: []controlplane.File{{Path: "/etc/x", Content: []byte("a")}}},
		{},
		{Files: []controlplane.File{{Path: "/etc/x", Content: []byte("b")}}},
	}
	_, err := Build(contribs, Options{Hostname: "n"})
	if err == nil {
		t.Fatal("expected duplicate path error")
	}
	msg := err.Error()
	if !strings.Contains(msg, `"/etc/x"`) || !strings.Contains(msg, "0") || !strings.Contains(msg, "2") {
		t.Errorf("error does not name path and indexes: %v", err)
	}
	if !strings.Contains(msg, "contributions 0 and 2") {
		t.Errorf("unexpected wording: %v", err)
	}
}

func TestBuildFetchReplacesFile(t *testing.T) {
	contribs := twoContribs()
	contribs[0].Files = append(contribs[0].Files, controlplane.File{Path: "/opt/big.tar", Content: []byte("huge")})
	b, err := Build(contribs, Options{
		Hostname: "n",
		Fetches:  []payload.Fetch{{Path: "/opt/big.tar", Command: "curl -fsSL https://x/big -o /opt/big.tar"}},
	})
	if err != nil {
		t.Fatal(err)
	}
	doc, err := Parse(b.UserData)
	if err != nil {
		t.Fatal(err)
	}
	for _, f := range writeFiles(t, doc) {
		if f["path"] == "/opt/big.tar" {
			t.Fatal("fetched file still in write_files")
		}
	}
	cmds := strs(doc["runcmd"])
	if len(cmds) == 0 || cmds[0] != "curl -fsSL https://x/big -o /opt/big.tar" {
		t.Errorf("fetch command not first: %v", cmds)
	}
}

func TestBuildBase64(t *testing.T) {
	binary := []byte{0x00, 0xff, 0xfe, 0x01, '\r', '\n', 0x7f, 0x80}
	contribs := []controlplane.Contribution{{Files: []controlplane.File{
		{Path: "/bin/blob", Content: binary},
		{Path: "/etc/forced", Content: []byte("plain text"), Base64: true},
		{Path: "/etc/crlf", Content: []byte("a\r\nb\r\n")},
	}}}
	b, err := Build(contribs, Options{Hostname: "n"})
	if err != nil {
		t.Fatal(err)
	}
	doc, err := Parse(b.UserData)
	if err != nil {
		t.Fatal(err)
	}
	want := map[string][]byte{"/bin/blob": binary, "/etc/forced": []byte("plain text"), "/etc/crlf": []byte("a\r\nb\r\n")}
	for _, f := range writeFiles(t, doc) {
		p := f["path"].(string)
		if f["encoding"] != "b64" {
			t.Errorf("%s: encoding = %v, want b64", p, f["encoding"])
			continue
		}
		got, err := base64.StdEncoding.DecodeString(f["content"].(string))
		if err != nil {
			t.Fatalf("%s: %v", p, err)
		}
		if !bytes.Equal(got, want[p]) {
			t.Errorf("%s: round trip got %v want %v", p, got, want[p])
		}
	}
}

func TestMetaData(t *testing.T) {
	b1, err := Build(nil, Options{Hostname: "node1"})
	if err != nil {
		t.Fatal(err)
	}
	b2, err := Build(nil, Options{Hostname: "node1"})
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(b1.MetaData, b2.MetaData) {
		t.Errorf("meta-data not stable:\n%s\n%s", b1.MetaData, b2.MetaData)
	}
	var md map[string]string
	if err := yaml.Unmarshal(b1.MetaData, &md); err != nil {
		t.Fatal(err)
	}
	if md["instance-id"] != DeriveInstanceID("node1") || md["local-hostname"] != "node1" {
		t.Errorf("meta-data = %v", md)
	}
	if DeriveInstanceID("node1") == DeriveInstanceID("node2") {
		t.Error("derived instance IDs collide")
	}

	b3, err := Build(nil, Options{Hostname: "node1", InstanceID: "explicit"})
	if err != nil {
		t.Fatal(err)
	}
	if err := yaml.Unmarshal(b3.MetaData, &md); err != nil {
		t.Fatal(err)
	}
	if md["instance-id"] != "explicit" {
		t.Errorf("explicit instance-id ignored: %v", md)
	}
	if b1.NetworkConfig != nil {
		t.Error("network-config present without Options.Network")
	}
	if _, err := Build(nil, Options{}); err == nil {
		t.Error("expected error with neither hostname nor instance id")
	}
}

func testNetwork() *NetworkConfig {
	return &NetworkConfig{Ethernets: []Ethernet{
		{
			ID:          "primary",
			Match:       Match{MACAddress: "52:54:00:12:34:56"},
			SetName:     "eth0",
			Addresses:   []string{"192.0.2.10/24"},
			Gateway4:    "192.0.2.1",
			Nameservers: Nameservers{Addresses: []string{"192.0.2.53"}, Search: []string{"example.com"}},
		},
		{Match: Match{Name: "ens*"}, DHCP4: true},
	}}
}

func TestNetworkConfig(t *testing.T) {
	b, err := Build(nil, Options{Hostname: "n", Network: testNetwork()})
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Contains(b.NetworkConfig, []byte("version: 2\n")) {
		t.Errorf("missing version: 2:\n%s", b.NetworkConfig)
	}
	var doc map[string]any
	if err := yaml.Unmarshal(b.NetworkConfig, &doc); err != nil {
		t.Fatal(err)
	}
	want := map[string]any{
		"version": float64(2),
		"ethernets": map[string]any{
			"primary": map[string]any{
				"match":     map[string]any{"macaddress": "52:54:00:12:34:56"},
				"set-name":  "eth0",
				"dhcp4":     false,
				"addresses": []any{"192.0.2.10/24"},
				"gateway4":  "192.0.2.1",
				"nameservers": map[string]any{
					"addresses": []any{"192.0.2.53"},
					"search":    []any{"example.com"},
				},
			},
			"eth1": map[string]any{
				"match": map[string]any{"name": "ens*"},
				"dhcp4": true,
			},
		},
	}
	if !reflect.DeepEqual(doc, want) {
		t.Errorf("network-config:\n%s\ngot  %#v\nwant %#v", b.NetworkConfig, doc, want)
	}

	bad := &NetworkConfig{Ethernets: []Ethernet{{Addresses: []string{"192.0.2.10"}}}}
	if _, err := bad.Marshal(); err == nil {
		t.Error("expected error for non-CIDR address")
	}
}

func TestISO(t *testing.T) {
	b, err := Build(twoContribs(), Options{Hostname: "node1", Network: testNetwork()})
	if err != nil {
		t.Fatal(err)
	}
	img, err := ISO(b)
	if err != nil {
		t.Fatal(err)
	}
	if len(img)%2048 != 0 {
		t.Errorf("image size %d is not a multiple of 2048", len(img))
	}

	r, err := iso9660.OpenImage(bytes.NewReader(img))
	if err != nil {
		t.Fatal(err)
	}
	label, err := r.Label()
	if err != nil {
		t.Fatal(err)
	}
	if label != "cidata" {
		t.Errorf("volume label = %q, want cidata", label)
	}
	root, err := r.RootDir()
	if err != nil {
		t.Fatal(err)
	}
	children, err := root.GetChildren()
	if err != nil {
		t.Fatal(err)
	}
	got := map[string][]byte{}
	for _, c := range children {
		data, err := io.ReadAll(c.Reader())
		if err != nil {
			t.Fatal(err)
		}
		got[c.Name()] = data
	}
	want := map[string][]byte{
		"user-data":      b.UserData,
		"meta-data":      b.MetaData,
		"network-config": b.NetworkConfig,
	}
	if len(got) != len(want) {
		names := make([]string, 0, len(got))
		for n := range got {
			names = append(names, n)
		}
		t.Fatalf("iso root has %v, want user-data, meta-data, network-config", names)
	}
	for name, data := range want {
		if !bytes.Equal(got[name], data) {
			t.Errorf("%s differs:\ngot  %q\nwant %q", name, got[name], data)
		}
	}

	// Without network-config only two files are written.
	b.NetworkConfig = nil
	var buf bytes.Buffer
	if err := ISOTo(&buf, b); err != nil {
		t.Fatal(err)
	}
	r, err = iso9660.OpenImage(bytes.NewReader(buf.Bytes()))
	if err != nil {
		t.Fatal(err)
	}
	root, err = r.RootDir()
	if err != nil {
		t.Fatal(err)
	}
	if children, err = root.GetChildren(); err != nil || len(children) != 2 {
		t.Errorf("expected 2 files without network-config, got %d (%v)", len(children), err)
	}
}

func TestWriteDir(t *testing.T) {
	b, err := Build(twoContribs(), Options{Hostname: "node1", Network: testNetwork()})
	if err != nil {
		t.Fatal(err)
	}
	dir := filepath.Join(t.TempDir(), "seed")
	if err := WriteDir(dir, b); err != nil {
		t.Fatal(err)
	}
	for name, want := range map[string][]byte{
		"user-data":      b.UserData,
		"meta-data":      b.MetaData,
		"network-config": b.NetworkConfig,
	} {
		p := filepath.Join(dir, name)
		got, err := os.ReadFile(p)
		if err != nil {
			t.Fatal(err)
		}
		if !bytes.Equal(got, want) {
			t.Errorf("%s content differs", name)
		}
		if runtime.GOOS != "windows" {
			fi, err := os.Stat(p)
			if err != nil {
				t.Fatal(err)
			}
			if fi.Mode().Perm() != 0o600 {
				t.Errorf("%s mode = %v, want 0600", name, fi.Mode().Perm())
			}
		}
	}

	b.NetworkConfig = nil
	if err := WriteDir(dir, b); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(filepath.Join(dir, "network-config")); !os.IsNotExist(err) {
		t.Errorf("stale network-config not removed: %v", err)
	}
}

func TestParseRequiresHeader(t *testing.T) {
	if _, err := Parse([]byte("runcmd: []\n")); err == nil {
		t.Error("expected error without header")
	}
	doc, err := Parse([]byte("#cloud-config\r\nruncmd: [a]\n"))
	if err != nil {
		t.Fatal(err)
	}
	if got := strs(doc["runcmd"]); len(got) != 1 || got[0] != "a" {
		t.Errorf("runcmd = %v", got)
	}
}
