package libvirt

import (
	"bytes"
	"regexp"
	"testing"
)

func TestBuildCIData(t *testing.T) {
	iso, err := buildCIData("1111-2222", "lab1-bootstrap", []byte("#cloud-config\nusers: []\n"))
	if err != nil {
		t.Fatal(err)
	}
	if len(iso) == 0 {
		t.Fatal("empty iso")
	}
	// The CIDATA volume label and the user-data / meta-data contents are stored
	// in the iso9660 image in the clear.
	for _, want := range [][]byte{
		[]byte("instance-id: 1111-2222"),
		[]byte("local-hostname: lab1-bootstrap"),
		[]byte("#cloud-config"),
	} {
		if !bytes.Contains(iso, want) {
			t.Errorf("iso does not contain %q", want)
		}
	}
}

func TestBuildCIDataErrors(t *testing.T) {
	if _, err := buildCIData("uuid", "h", nil); err == nil {
		t.Error("want error for empty user-data")
	}
	if _, err := buildCIData("", "h", []byte("x")); err == nil {
		t.Error("want error for empty instance-id")
	}
}

var uuidRE = regexp.MustCompile(`^[0-9a-f]{8}-[0-9a-f]{4}-4[0-9a-f]{3}-[89ab][0-9a-f]{3}-[0-9a-f]{12}$`)

func TestNewUUID(t *testing.T) {
	seen := map[string]bool{}
	for i := 0; i < 100; i++ {
		u, err := newUUID()
		if err != nil {
			t.Fatal(err)
		}
		if !uuidRE.MatchString(u) {
			t.Fatalf("not a v4 UUID: %q", u)
		}
		if seen[u] {
			t.Fatalf("duplicate UUID %q", u)
		}
		seen[u] = true
	}
}
