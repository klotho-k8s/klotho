package hyperv

import (
	"context"
	"reflect"
	"strings"
	"testing"
)

func TestNormalizeMAC(t *testing.T) {
	for in, want := range map[string]string{
		"00155D0A1B2C":      "00155D0A1B2C",
		"00-15-5d-0a-1b-2c": "00155D0A1B2C",
		"00:15:5d:0a:1b:2c": "00155D0A1B2C",
		"00.15.5d.0a.1b.2c": "00155D0A1B2C",
		"00 15 5d 0a 1b 2c": "00155D0A1B2C",
		"":                  "",
	} {
		if got := normalizeMAC(in); got != want {
			t.Errorf("normalizeMAC(%q) = %q, want %q", in, got, want)
		}
	}
}

func TestSwitchInterfaceAlias(t *testing.T) {
	if got := switchInterfaceAlias("Default Switch"); got != "vEthernet (Default Switch)" {
		t.Errorf("switchInterfaceAlias = %q", got)
	}
}

func TestFallbackScriptsExitZero(t *testing.T) {
	for name, s := range map[string]string{
		"adapterInfo": adapterInfoScript("lab-bootstrap"),
		"neighbor":    neighborScript("vEthernet (Default Switch)"),
	} {
		if !strings.HasSuffix(strings.TrimSpace(s), "exit 0") {
			t.Errorf("%s script must end with 'exit 0': %s", name, s)
		}
	}
	if !strings.Contains(adapterInfoScript("x"), "Get-VMNetworkAdapter") {
		t.Error("adapterInfoScript must read Get-VMNetworkAdapter")
	}
	if !strings.Contains(neighborScript("if"), "Get-NetNeighbor") {
		t.Error("neighborScript must read Get-NetNeighbor")
	}
}

func TestParseAdapters(t *testing.T) {
	// single object
	one, err := parseAdapters(`{"MacAddress":"00155D0A1B2C","SwitchName":"Default Switch"}`)
	if err != nil || len(one) != 1 || one[0].MacAddress != "00155D0A1B2C" || one[0].SwitchName != "Default Switch" {
		t.Fatalf("single: %v %+v", err, one)
	}
	// array
	many, err := parseAdapters(`[{"MacAddress":"AA","SwitchName":"s1"},{"MacAddress":"BB","SwitchName":"s2"}]`)
	if err != nil || len(many) != 2 {
		t.Fatalf("array: %v %+v", err, many)
	}
	// empty / null
	for _, in := range []string{"", "  ", "null"} {
		if got, err := parseAdapters(in); err != nil || got != nil {
			t.Errorf("parseAdapters(%q) = %v, %v", in, got, err)
		}
	}
}

func TestParseNeighbors(t *testing.T) {
	one, err := parseNeighbors(`{"IPAddress":"172.30.46.222","LinkLayerAddress":"00-15-5D-0A-1B-2C","State":"Reachable"}`)
	if err != nil || len(one) != 1 || one[0].IPAddress != "172.30.46.222" {
		t.Fatalf("single: %v %+v", err, one)
	}
	many, err := parseNeighbors(`[{"IPAddress":"1.1.1.1","LinkLayerAddress":"AA","State":5},{"IPAddress":"2.2.2.2","LinkLayerAddress":"BB","State":2}]`)
	if err != nil || len(many) != 2 {
		t.Fatalf("array: %v %+v", err, many)
	}
}

func TestNeighborState(t *testing.T) {
	for in, want := range map[string]string{
		`"Reachable"`: "Reachable",
		`5`:           "Reachable",
		`2`:           "Stale",
		`"Stale"`:     "Stale",
		`0`:           "Unreachable",
		`6`:           "Permanent",
	} {
		if got := neighborState([]byte(in)); got != want {
			t.Errorf("neighborState(%s) = %q, want %q", in, got, want)
		}
	}
}

func TestPickNeighborIPv4(t *testing.T) {
	macs := map[string]bool{"00155D0A1B2C": true}
	entries := []neighbor{
		{IPAddress: "fe80::1", LinkLayerAddress: "00-15-5D-0A-1B-2C", State: []byte(`"Reachable"`)}, // v6 dropped
		{IPAddress: "172.30.46.50", LinkLayerAddress: "00-15-5D-0A-1B-2C", State: []byte(`"Stale"`)},
		{IPAddress: "172.30.46.222", LinkLayerAddress: "00-15-5D-0A-1B-2C", State: []byte(`"Reachable"`)},
		{IPAddress: "172.30.46.99", LinkLayerAddress: "00-15-5D-FF-FF-FF", State: []byte(`"Reachable"`)},  // other MAC
		{IPAddress: "172.30.46.7", LinkLayerAddress: "00-15-5D-0A-1B-2C", State: []byte(`"Unreachable"`)}, // dropped
		{IPAddress: "172.30.46.222", LinkLayerAddress: "00-15-5D-0A-1B-2C", State: []byte(`"Stale"`)},     // dup of reachable
	}
	got := pickNeighborIPv4(entries, macs)
	want := []string{"172.30.46.222", "172.30.46.50"}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("pickNeighborIPv4 = %v, want %v (Reachable before Stale, v6/other-MAC/Unreachable dropped, deduped)", got, want)
	}
}

// dispatchRunner routes a script to a canned response by a substring match, so
// the multi-step client methods can be driven from a table.
type dispatchRunner struct {
	responses map[string]string // substring -> stdout
}

func (d dispatchRunner) run(_ context.Context, script string) (string, error) {
	for sub, out := range d.responses {
		if strings.Contains(script, sub) {
			return out, nil
		}
	}
	return "", nil
}

func TestGetIPAddressesKVPWins(t *testing.T) {
	c := &psClient{r: dispatchRunner{responses: map[string]string{
		"IPAddresses": "fe80::1\n192.168.1.50\n",
		// if the fallback were (wrongly) consulted it would add this:
		"Get-NetNeighbor": `{"IPAddress":"10.0.0.9","LinkLayerAddress":"AA","State":"Reachable"}`,
	}}}
	got, err := c.GetIPAddresses(context.Background(), "lab-bootstrap")
	if err != nil {
		t.Fatal(err)
	}
	if v4 := ipv4(got); len(v4) != 1 || v4[0] != "192.168.1.50" {
		t.Errorf("KVP IPv4 should win without fallback: %v", got)
	}
}

func TestGetIPAddressesFallback(t *testing.T) {
	c := &psClient{r: dispatchRunner{responses: map[string]string{
		"IPAddresses":           "", // KVP empty (no hyperv-daemons in guest)
		"MacAddress,SwitchName": `{"MacAddress":"00155D0A1B2C","SwitchName":"Default Switch"}`,
		"Get-NetNeighbor":       `[{"IPAddress":"172.30.46.222","LinkLayerAddress":"00-15-5D-0A-1B-2C","State":"Reachable"}]`,
	}}}
	got, err := c.GetIPAddresses(context.Background(), "lab-bootstrap")
	if err != nil {
		t.Fatal(err)
	}
	if v4 := ipv4(got); len(v4) != 1 || v4[0] != "172.30.46.222" {
		t.Errorf("fallback should resolve the neighbor-table IPv4: %v", got)
	}
}

// Note: the adapterInfoScript substring "Get-VMNetworkAdapter" also appears in
// ipAddressesScript, but the dispatch map is checked in GetIPAddresses order
// (KVP first, which matches "IPAddresses"), so routing stays correct.
