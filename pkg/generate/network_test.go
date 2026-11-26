package generate

import (
	"context"
	"os"
	"testing"
	"time"

	"github.com/klotho-k8s/klotho/pkg/cluster"
)

// TestGenerateProxmoxNetwork renders the real proxmox template. It downloads
// from GitHub and runs only when KLOTHO_NETWORK_TESTS=1.
func TestGenerateProxmoxNetwork(t *testing.T) {
	if os.Getenv("KLOTHO_NETWORK_TESTS") != "1" {
		t.Skip("set KLOTHO_NETWORK_TESTS=1 to run tests that use the network")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
	defer cancel()

	req := Request{
		Spec:                 cluster.Spec{Name: "nettest", Provider: "proxmox"},
		Provider:             ProviderRef{Name: "proxmox", Version: "v0.9.2"},
		Endpoint:             cluster.Endpoint{Host: "192.0.2.10", Port: 6443},
		Kubernetes:           "v1.33.4",
		ControlPlaneReplicas: 1,
		WorkerReplicas:       1,
		Variables: []VariableSource{MapSource(map[string]string{
			"PROXMOX_SOURCENODE": "pve",
			"TEMPLATE_VMID":      "9000",
			"BOOT_VOLUME_DEVICE": "scsi0",
			"BRIDGE":             "vmbr0",
			"DNS_SERVERS":        "[192.0.2.1]",
			"GATEWAY":            "192.0.2.1",
			"IP_PREFIX":          "24",
			"NODE_IP_RANGES":     "[192.0.2.20-192.0.2.30]",
			"VM_SSH_KEYS":        "ssh-ed25519 AAAA test",
		})},
	}

	vars, err := ListVariables(ctx, req)
	if err != nil {
		t.Fatal(err)
	}
	t.Logf("template variables: %d", len(vars))

	b, err := Generate(ctx, req)
	if err != nil {
		t.Fatal(err)
	}
	pc, err := FindOne(b, InfrastructureGroup, "Cluster")
	if err != nil {
		t.Fatal(err)
	}
	if pc.GetKind() != "ProxmoxCluster" || pc.GetName() != "nettest" {
		t.Fatalf("got %s %q", pc.GetKind(), pc.GetName())
	}
	if err := SetControlPlaneEndpoint(b, req.Endpoint); err != nil {
		t.Fatal(err)
	}
	if err := AddMachineTemplateTag(b, "klotho"); err != nil {
		t.Fatal(err)
	}
	if err := Validate(b, "nettest", "klotho"); err != nil {
		t.Fatal(err)
	}
	t.Logf("rendered %d objects, %d bytes", len(b.Objects), len(b.YAML))
}
