package proxmox

import (
	"net/netip"
	"strings"
	"testing"

	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
)

func checkBundle(pods, services []string, pxcSpec map[string]any) []*unstructured.Unstructured {
	nets := map[string]any{}
	if pods != nil {
		nets["pods"] = map[string]any{"cidrBlocks": toAny(pods)}
	}
	if services != nil {
		nets["services"] = map[string]any{"cidrBlocks": toAny(services)}
	}
	return []*unstructured.Unstructured{
		nil,
		{Object: map[string]any{
			"apiVersion": "cluster.x-k8s.io/v1beta2", "kind": "Cluster",
			"metadata": map[string]any{"name": "demo"},
			"spec":     map[string]any{"clusterNetwork": nets},
		}},
		{Object: map[string]any{
			"apiVersion": "infrastructure.cluster.x-k8s.io/v1alpha1", "kind": "ProxmoxCluster",
			"metadata": map[string]any{"name": "demo"},
			"spec":     pxcSpec,
		}},
	}
}

func toAny(s []string) []any {
	out := make([]any, len(s))
	for i, v := range s {
		out[i] = v
	}
	return out
}

func labSpec() map[string]any {
	return map[string]any{
		"controlPlaneEndpoint": map[string]any{"host": "192.168.1.250", "port": int64(6443)},
		"ipv4Config": map[string]any{
			"addresses": []any{"192.168.1.200-192.168.1.220"},
			"prefix":    int64(20),
			"gateway":   "192.168.1.1",
		},
		"dnsServers": []any{"192.168.1.1"},
	}
}

func TestCheckClusterNetworkOverlap(t *testing.T) {
	p := &Provider{}

	t.Run("template default overlaps a 192.168 LAN", func(t *testing.T) {
		got := p.Check(checkBundle([]string{"192.168.0.0/16"}, []string{"10.96.0.0/12"}, labSpec()))
		if len(got) != 1 {
			t.Fatalf("findings = %v", got)
		}
		f := got[0]
		if f.Object != "Cluster/demo" || f.Field != "spec.clusterNetwork.pods.cidrBlocks" ||
			!strings.Contains(f.Message, "192.168.0.0/16 overlaps 192.168.0.0/20 (the node subnet from ProxmoxCluster/demo)") ||
			!strings.Contains(f.Message, "--pod-cidr") {
			t.Fatalf("finding = %+v", f)
		}
	})

	t.Run("disjoint networks pass", func(t *testing.T) {
		if got := p.Check(checkBundle([]string{"10.244.0.0/16"}, []string{"10.96.0.0/12"}, labSpec())); len(got) != 0 {
			t.Fatalf("findings = %v", got)
		}
	})

	t.Run("service network overlapping the endpoint", func(t *testing.T) {
		got := p.Check(checkBundle([]string{"10.244.0.0/16"}, []string{"192.168.1.0/24"}, labSpec()))
		if len(got) != 1 || got[0].Field != "spec.clusterNetwork.services.cidrBlocks" {
			t.Fatalf("findings = %v", got)
		}
	})

	t.Run("no prefix: only the gateway address counts", func(t *testing.T) {
		spec := labSpec()
		delete(spec["ipv4Config"].(map[string]any), "prefix")
		// 192.168.8.0/22 misses the gateway, the DNS server, the endpoint and the node range.
		if got := p.Check(checkBundle([]string{"192.168.8.0/22"}, nil, spec)); len(got) != 0 {
			t.Fatalf("findings = %v", got)
		}
		// 192.168.0.0/22 covers the node range and the endpoint.
		if got := p.Check(checkBundle([]string{"192.168.0.0/22"}, nil, spec)); len(got) != 1 {
			t.Fatalf("findings = %v", got)
		}
	})

	t.Run("bad CIDR is reported", func(t *testing.T) {
		got := p.Check(checkBundle([]string{"nope"}, nil, labSpec()))
		if len(got) != 1 || !strings.Contains(got[0].Message, "not a CIDR") {
			t.Fatalf("findings = %v", got)
		}
	})

	t.Run("missing objects: nothing to say", func(t *testing.T) {
		b := checkBundle([]string{"192.168.0.0/16"}, nil, labSpec())
		if got := p.Check(b[:2]); len(got) != 0 {
			t.Fatalf("findings = %v", got)
		}
		if got := p.Check(nil); len(got) != 0 {
			t.Fatalf("findings = %v", got)
		}
	})
}

func TestParseRange(t *testing.T) {
	cases := map[string]string{
		"192.168.1.200-192.168.1.220": "192.168.1.192/27",
		"10.0.0.0/8":                  "10.0.0.0/8",
		"10.1.2.3":                    "10.1.2.3/32",
		"10.0.0.9 - 10.0.0.2":         "10.0.0.0/28",
		"garbage":                     "",
		"10.0.0.1-fe80::1":            "",
	}
	for in, want := range cases {
		got := parseRange(in)
		if want == "" {
			if len(got) != 0 {
				t.Errorf("%q: got %v, want none", in, got)
			}
			continue
		}
		if len(got) != 1 || got[0] != netip.MustParsePrefix(want) {
			t.Errorf("%q: got %v, want %s", in, got, want)
		}
	}
}

func TestDependencies(t *testing.T) {
	d := (&Provider{}).Dependencies()
	if len(d) != 1 || d[0].Kind != "IPAMProvider" || d[0].Name != "in-cluster" || d[0].Version != DefaultIPAMVersion || d[0].Namespace == "" {
		t.Fatalf("dependencies = %+v", d)
	}
}

func TestRequiredPackages(t *testing.T) {
	if got := (&Provider{}).RequiredPackages(); len(got) != 1 || got[0] != "qemu-guest-agent" {
		t.Fatalf("RequiredPackages = %v", got)
	}
}
