package libvirt

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
	// The values klotho's generator fills (CLUSTER_NAME, NAMESPACE, ...) and the
	// ones Prepare injects (LIBVIRT_*, CONTROL_PLANE_ENDPOINT_HOST).
	vars := map[string]string{
		"${CLUSTER_NAME}":                "lab1",
		"${NAMESPACE}":                   "default",
		"${KUBERNETES_VERSION}":          "v1.33.4",
		"${CONTROL_PLANE_MACHINE_COUNT}": "1",
		"${LIBVIRT_URI}":                 "qemu:///system",
		"${LIBVIRT_HOST}":                "localhost",
		"${CONTROL_PLANE_ENDPOINT_HOST}": "192.168.122.10",
		"${LIBVIRT_IMAGE}":               "debian-12.qcow2",
		"${LIBVIRT_POOL}":                "default",
		"${LIBVIRT_NETWORK}":             "default",
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

	// LibvirtCluster with connection URI and endpoint.
	lc, err := generate.FindOne(b, generate.InfrastructureGroup, "LibvirtCluster")
	if err != nil {
		t.Fatalf("LibvirtCluster: %v", err)
	}
	if uri, _, _ := unstructured.NestedString(lc.Object, "spec", "connectionURI"); uri != "qemu:///system" {
		t.Errorf("connectionURI = %q", uri)
	}
	if host, _, _ := unstructured.NestedString(lc.Object, "spec", "controlPlaneEndpoint", "host"); host != "192.168.122.10" {
		t.Errorf("endpoint host = %q", host)
	}

	// KubeadmControlPlane: single replica, provider-id kubelet arg.
	kcpObj, err := generate.FindOne(b, generate.ControlPlaneGroup, "KubeadmControlPlane")
	if err != nil {
		t.Fatalf("KubeadmControlPlane: %v", err)
	}
	if !strings.Contains(rendered, "libvirt://localhost/{{ ds.meta_data.instance_id }}") {
		t.Error("provider-id kubelet arg not wired to LIBVIRT_HOST + instance_id")
	}
	if repl, _, _ := unstructured.NestedInt64(kcpObj.Object, "spec", "replicas"); repl != 1 {
		t.Errorf("replicas = %d", repl)
	}

	// LibvirtMachineTemplate with image, pool, network.
	mt, err := generate.FindOne(b, generate.InfrastructureGroup, "LibvirtMachineTemplate")
	if err != nil {
		t.Fatalf("LibvirtMachineTemplate: %v", err)
	}
	if img, _, _ := unstructured.NestedString(mt.Object, "spec", "template", "spec", "image"); img != "debian-12.qcow2" {
		t.Errorf("image = %q", img)
	}
	if pool, _, _ := unstructured.NestedString(mt.Object, "spec", "template", "spec", "storagePool"); pool != "default" {
		t.Errorf("storagePool = %q", pool)
	}
	if net, _, _ := unstructured.NestedString(mt.Object, "spec", "template", "spec", "network", "name"); net != "default" {
		t.Errorf("network name = %q", net)
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
