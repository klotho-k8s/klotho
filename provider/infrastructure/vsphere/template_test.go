package vsphere

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
		"${VSPHERE_SERVER}":              "vcenter.example.com",
		"${VSPHERE_TLS_THUMBPRINT}":      "AA:BB:CC:DD",
		"${CONTROL_PLANE_ENDPOINT_IP}":   "192.168.1.200",
		"${VIP_NETWORK_INTERFACE}":       "",
		"${VSPHERE_TEMPLATE}":            "ubuntu-2404-template",
		"${VSPHERE_DATACENTER}":          "dc1",
		"${VSPHERE_DATASTORE}":           "datastore1",
		"${VSPHERE_FOLDER}":              "klotho",
		"${VSPHERE_RESOURCE_POOL}":       "*/Resources",
		"${VSPHERE_NETWORK}":             "VM Network",
		"${VSPHERE_USERNAME}":            "klotho@vsphere.local",
		"${VSPHERE_PASSWORD}":            "secret",
		"${VSPHERE_SSH_AUTHORIZED_KEY}":  "ssh-ed25519 AAAA",
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

	// VSphereCluster with server and endpoint.
	vc, err := generate.FindOne(b, generate.InfrastructureGroup, "VSphereCluster")
	if err != nil {
		t.Fatalf("VSphereCluster: %v", err)
	}
	if server, _, _ := unstructured.NestedString(vc.Object, "spec", "server"); server != "vcenter.example.com" {
		t.Errorf("server = %q", server)
	}
	if host, _, _ := unstructured.NestedString(vc.Object, "spec", "controlPlaneEndpoint", "host"); host != "192.168.1.200" {
		t.Errorf("endpoint host = %q", host)
	}
	if idName, _, _ := unstructured.NestedString(vc.Object, "spec", "identityRef", "name"); idName != "lab1" {
		t.Errorf("identityRef name = %q", idName)
	}

	// KubeadmControlPlane: single replica, provider-id kubelet arg, kube-vip.
	kcpObj, err := generate.FindOne(b, generate.ControlPlaneGroup, "KubeadmControlPlane")
	if err != nil {
		t.Fatalf("KubeadmControlPlane: %v", err)
	}
	if repl, _, _ := unstructured.NestedInt64(kcpObj.Object, "spec", "replicas"); repl != 1 {
		t.Errorf("replicas = %d", repl)
	}
	if !strings.Contains(rendered, "vsphere://{{ ds.meta_data.instance_id }}") {
		t.Error("provider-id kubelet arg not wired to instance_id")
	}
	if !strings.Contains(rendered, "/etc/kubernetes/manifests/kube-vip.yaml") {
		t.Error("kube-vip static pod file not present")
	}

	// VSphereMachineTemplate with source template, datacenter, network.
	mt, err := generate.FindOne(b, generate.InfrastructureGroup, "VSphereMachineTemplate")
	if err != nil {
		t.Fatalf("VSphereMachineTemplate: %v", err)
	}
	if tmpl, _, _ := unstructured.NestedString(mt.Object, "spec", "template", "spec", "template"); tmpl != "ubuntu-2404-template" {
		t.Errorf("template = %q", tmpl)
	}
	if dc, _, _ := unstructured.NestedString(mt.Object, "spec", "template", "spec", "datacenter"); dc != "dc1" {
		t.Errorf("datacenter = %q", dc)
	}
	devices, _, _ := unstructured.NestedSlice(mt.Object, "spec", "template", "spec", "network", "devices")
	if len(devices) != 1 {
		t.Fatalf("expected one network device, got %d", len(devices))
	}
	dev := devices[0].(map[string]any)
	if dev["networkName"] != "VM Network" {
		t.Errorf("networkName = %v", dev["networkName"])
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
