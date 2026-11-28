package vz

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
// and the provider's Prepare fill, then verifies the flat single-node bundle
// parses into exactly the objects the adopt/pivot model needs.
func TestClusterTemplateGeneratesExpectedObjects(t *testing.T) {
	vars := map[string]string{
		"${CLUSTER_NAME}":                "lab1",
		"${NAMESPACE}":                   "default",
		"${KUBERNETES_VERSION}":          "v1.33.4",
		"${CONTROL_PLANE_MACHINE_COUNT}": "1",
		"${VZ_HOST}":                     "localhost",
		"${CONTROL_PLANE_ENDPOINT_HOST}": "192.168.64.10",
		"${VZ_IMAGE_PATH}":               "/opt/images/ubuntu-arm64.raw",
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

	// VZCluster with host and endpoint.
	vc, err := generate.FindOne(b, generate.InfrastructureGroup, "VZCluster")
	if err != nil {
		t.Fatalf("VZCluster: %v", err)
	}
	if host, _, _ := unstructured.NestedString(vc.Object, "spec", "host"); host != "localhost" {
		t.Errorf("host = %q", host)
	}
	if host, _, _ := unstructured.NestedString(vc.Object, "spec", "controlPlaneEndpoint", "host"); host != "192.168.64.10" {
		t.Errorf("endpoint host = %q", host)
	}

	// KubeadmControlPlane: single replica, provider-id kubelet arg wired to the host.
	kcpObj, err := generate.FindOne(b, generate.ControlPlaneGroup, "KubeadmControlPlane")
	if err != nil {
		t.Fatalf("KubeadmControlPlane: %v", err)
	}
	if !strings.Contains(rendered, "vz://localhost/{{ ds.meta_data.instance_id }}") {
		t.Error("provider-id kubelet arg not wired to VZ_HOST + instance_id")
	}
	if repl, _, _ := unstructured.NestedInt64(kcpObj.Object, "spec", "replicas"); repl != 1 {
		t.Errorf("replicas = %d", repl)
	}

	// VZMachineTemplate with the image path.
	mt, err := generate.FindOne(b, generate.InfrastructureGroup, "VZMachineTemplate")
	if err != nil {
		t.Fatalf("VZMachineTemplate: %v", err)
	}
	if img, _, _ := unstructured.NestedString(mt.Object, "spec", "template", "spec", "image"); img != "/opt/images/ubuntu-arm64.raw" {
		t.Errorf("image = %q", img)
	}

	// Single-node: no MachineDeployment.
	if md := generate.Find(b, generate.ClusterGroup, "MachineDeployment"); len(md) != 0 {
		t.Errorf("expected no MachineDeployment in a single-node template, got %d", len(md))
	}

	// Adopt must find the control plane's template in this bundle.
	if _, _, err := controlPlaneTemplate(b); err != nil {
		t.Fatalf("controlPlaneTemplate on generated bundle: %v", err)
	}
}
