package hetzner

import (
	_ "embed"
	"strings"
	"testing"

	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"

	"github.com/klotho-k8s/klotho/pkg/generate"
)

//go:embed cluster-template.yaml
var clusterTemplate string

// TestClusterTemplateGeneratesExpectedObjects substitutes the variables klotho
// and the provider's Prepare fill, then verifies the flat bundle parses into the
// objects the adopt/pivot model needs: a Cluster, a HetznerCluster with the
// load balancer disabled and the endpoint wired, a single-replica
// KubeadmControlPlane, the control-plane HCloudMachineTemplate, and a worker
// MachineDeployment pair.
func TestClusterTemplateGeneratesExpectedObjects(t *testing.T) {
	vars := map[string]string{
		"${CLUSTER_NAME}":                      "lab1",
		"${NAMESPACE}":                         "default",
		"${KUBERNETES_VERSION}":                "v1.33.4",
		"${CONTROL_PLANE_MACHINE_COUNT}":       "1",
		"${WORKER_MACHINE_COUNT}":              "0",
		"${CONTROL_PLANE_ENDPOINT_IP}":         "203.0.113.10",
		"${CONTROL_PLANE_ENDPOINT_PORT:=6443}": "6443",
		"${HCLOUD_LOCATION}":                   "fsn1",
		"${HCLOUD_CONTROL_PLANE_MACHINE_TYPE}": "cpx21",
		"${HCLOUD_WORKER_MACHINE_TYPE}":        "cpx21",
		"${HCLOUD_IMAGE_NAME:=ubuntu-22.04}":   "ubuntu-22.04",
		"${HCLOUD_SSH_KEY}":                    "klotho",
	}
	rendered := clusterTemplate
	for k, v := range vars {
		rendered = strings.ReplaceAll(rendered, k, v)
	}
	if strings.Contains(rendered, "${") {
		t.Fatalf("unsubstituted variable remains:\n%s", rendered)
	}

	b, err := generate.ParseBundle([]byte(rendered))
	if err != nil {
		t.Fatalf("ParseBundle: %v", err)
	}

	// Cluster.
	clusterObj, err := generate.FindOne(b, generate.ClusterGroup, "Cluster")
	if err != nil {
		t.Fatalf("Cluster: %v", err)
	}
	if clusterObj.GetName() != "lab1" {
		t.Errorf("cluster name = %q", clusterObj.GetName())
	}

	// HetznerCluster: endpoint wired, load balancer disabled, region set.
	hc, err := generate.FindOne(b, generate.InfrastructureGroup, "Cluster")
	if err != nil {
		t.Fatalf("HetznerCluster: %v", err)
	}
	if hc.GetKind() != "HetznerCluster" {
		t.Errorf("infra cluster kind = %q, want HetznerCluster", hc.GetKind())
	}
	if host, _, _ := unstructured.NestedString(hc.Object, "spec", "controlPlaneEndpoint", "host"); host != "203.0.113.10" {
		t.Errorf("endpoint host = %q", host)
	}
	if enabled, _, _ := unstructured.NestedBool(hc.Object, "spec", "controlPlaneLoadBalancer", "enabled"); enabled {
		t.Error("controlPlaneLoadBalancer should be disabled in the cheap flat template")
	}
	regions, _, _ := unstructured.NestedStringSlice(hc.Object, "spec", "controlPlaneRegions")
	if len(regions) != 1 || regions[0] != "fsn1" {
		t.Errorf("controlPlaneRegions = %v, want [fsn1]", regions)
	}

	// KubeadmControlPlane: single replica.
	kcpObj, err := generate.FindOne(b, generate.ControlPlaneGroup, "KubeadmControlPlane")
	if err != nil {
		t.Fatalf("KubeadmControlPlane: %v", err)
	}
	if repl, _, _ := unstructured.NestedInt64(kcpObj.Object, "spec", "replicas"); repl != 1 {
		t.Errorf("replicas = %d", repl)
	}
	// provider-id is injected from Hetzner's instance metadata (no CCM).
	if !strings.Contains(rendered, "provider-id=hcloud://") || !strings.Contains(rendered, "metadata/instance-id") {
		t.Error("kubelet provider-id not wired to the Hetzner instance-id metadata")
	}

	// HCloudMachineTemplate(s): control plane + worker, carrying the server type.
	mts := generate.Find(b, generate.InfrastructureGroup, "MachineTemplate")
	if len(mts) != 2 {
		t.Fatalf("expected two HCloudMachineTemplates (control plane + worker), got %d", len(mts))
	}
	for _, mt := range mts {
		if typ, _, _ := unstructured.NestedString(mt.Object, "spec", "template", "spec", "type"); typ != "cpx21" {
			t.Errorf("%s type = %q, want cpx21", mt.GetName(), typ)
		}
	}

	// Worker MachineDeployment present.
	if md := generate.Find(b, generate.ClusterGroup, "MachineDeployment"); len(md) != 1 {
		t.Errorf("expected one MachineDeployment, got %d", len(md))
	}

	// Adopt must find the control plane's template in this bundle.
	if _, _, err := controlPlaneTemplate(b); err != nil {
		t.Fatalf("controlPlaneTemplate on generated bundle: %v", err)
	}
}
