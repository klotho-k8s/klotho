package libvirt

import (
	"encoding/xml"
	"strings"
	"testing"
)

func TestBuildDomainXML(t *testing.T) {
	out, err := buildDomainXML(domainXMLInput{
		Name:             "lab1-bootstrap",
		UUID:             "dead-beef",
		Description:      "klotho-lab1",
		MemoryBytes:      4096 << 20,
		VCPU:             2,
		OSDiskPath:       "/pool/lab1-bootstrap.qcow2",
		CloudInitISOPath: "/pool/lab1-bootstrap-cidata.iso",
		NetworkName:      "default",
		NetworkType:      "network",
	})
	if err != nil {
		t.Fatal(err)
	}
	// Must be well-formed XML.
	if err := xml.Unmarshal([]byte(out), new(struct{})); err != nil {
		t.Fatalf("invalid XML: %v", err)
	}
	for _, want := range []string{
		`type="kvm"`,
		"<name>lab1-bootstrap</name>",
		"<uuid>dead-beef</uuid>",
		"<description>klotho-lab1</description>",
		`name="org.qemu.guest_agent.0"`,
		`<source network="default">`,
		"/pool/lab1-bootstrap.qcow2",
		"/pool/lab1-bootstrap-cidata.iso",
		`device="cdrom"`,
	} {
		if !strings.Contains(out, want) {
			t.Errorf("domain XML missing %q", want)
		}
	}
}

func TestBuildDomainXMLBridge(t *testing.T) {
	out, err := buildDomainXML(domainXMLInput{
		Name:        "d",
		MemoryBytes: 1 << 30,
		VCPU:        1,
		OSDiskPath:  "/p/d.qcow2",
		NetworkName: "br0",
		NetworkType: "bridge",
	})
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(out, `<source bridge="br0">`) {
		t.Errorf("bridge source missing: %s", out)
	}
	// No cidata: no cdrom device.
	if strings.Contains(out, `device="cdrom"`) {
		t.Error("unexpected cdrom with no cidata path")
	}
}

func TestBuildDomainXMLValidation(t *testing.T) {
	cases := []domainXMLInput{
		{MemoryBytes: 1, VCPU: 1, OSDiskPath: "x"},                                // no name
		{Name: "d", VCPU: 1, OSDiskPath: "x"},                                     // no memory
		{Name: "d", MemoryBytes: 1, OSDiskPath: "x"},                              // no vcpu
		{Name: "d", MemoryBytes: 1, VCPU: 1},                                      // no disk
		{Name: "d", MemoryBytes: 1, VCPU: 1, OSDiskPath: "x", NetworkType: "wat"}, // bad net type
	}
	for i, in := range cases {
		if _, err := buildDomainXML(in); err == nil {
			t.Errorf("case %d: want error", i)
		}
	}
}

func TestOverlayVolumeXML(t *testing.T) {
	// Zero capacity: no <capacity> element (inherit backing size).
	out, err := overlayVolumeXML("o.qcow2", "/pool/base.qcow2", 0)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(out, "<capacity") {
		t.Errorf("unexpected capacity: %s", out)
	}
	if !strings.Contains(out, "<backingStore>") || !strings.Contains(out, "/pool/base.qcow2") {
		t.Errorf("backing store missing: %s", out)
	}
	// Explicit capacity.
	out2, _ := overlayVolumeXML("o.qcow2", "/p/base.qcow2", 20<<30)
	if !strings.Contains(out2, `unit="bytes"`) {
		t.Errorf("capacity missing: %s", out2)
	}
}

func TestRawVolumeXML(t *testing.T) {
	out, err := rawVolumeXML("seed.iso", 512)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(out, `type="raw"`) {
		t.Errorf("raw format missing: %s", out)
	}
}
