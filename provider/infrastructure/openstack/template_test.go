package openstack

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
// objects the adopt/pivot model needs: no Octavia load balancer, the control
// plane endpoint as a floating IP, and the kubelet provider-id wired to the Nova
// server UUID.
func TestClusterTemplateGeneratesExpectedObjects(t *testing.T) {
	vars := map[string]string{
		"${CLUSTER_NAME}":                           "lab1",
		"${NAMESPACE}":                              "default",
		"${KUBERNETES_VERSION}":                     "v1.33.4",
		"${CONTROL_PLANE_MACHINE_COUNT}":            "1",
		"${WORKER_MACHINE_COUNT}":                   "0",
		"${OPENSTACK_CLOUD}":                        "mycloud",
		"${OPENSTACK_NETWORK_ID}":                   "net-abc",
		"${OPENSTACK_EXTERNAL_NETWORK_ID}":          "ext-abc",
		"${OPENSTACK_CONTROL_PLANE_FLOATING_IP}":    "203.0.113.20",
		"${OPENSTACK_IMAGE_NAME}":                   "ubuntu-22.04",
		"${OPENSTACK_CONTROL_PLANE_MACHINE_FLAVOR}": "m1.medium",
		"${OPENSTACK_NODE_MACHINE_FLAVOR}":          "m1.medium",
		"${OPENSTACK_SSH_KEY_NAME}":                 "mykey",
		"${OPENSTACK_CLOUDS_YAML_B64}":              "Y2xvdWRzOiB7fQo=",
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

	// OpenStackCluster: identityRef, BYO network, floating-IP endpoint, no LB.
	oc, err := generate.FindOne(b, generate.InfrastructureGroup, "OpenStackCluster")
	if err != nil {
		t.Fatalf("OpenStackCluster: %v", err)
	}
	if cn, _, _ := unstructured.NestedString(oc.Object, "spec", "identityRef", "cloudName"); cn != "mycloud" {
		t.Errorf("identityRef.cloudName = %q", cn)
	}
	if secret, _, _ := unstructured.NestedString(oc.Object, "spec", "identityRef", "name"); secret != "lab1-cloud-config" {
		t.Errorf("identityRef.name = %q, want lab1-cloud-config", secret)
	}
	if net, _, _ := unstructured.NestedString(oc.Object, "spec", "network", "id"); net != "net-abc" {
		t.Errorf("network.id = %q", net)
	}
	if fip, _, _ := unstructured.NestedString(oc.Object, "spec", "apiServer", "floatingIP"); fip != "203.0.113.20" {
		t.Errorf("apiServer.floatingIP = %q", fip)
	}
	if lb, _, _ := unstructured.NestedBool(oc.Object, "spec", "apiServer", "managedLoadBalancer", "enabled"); lb {
		t.Error("managedLoadBalancer.enabled must be false (no Octavia)")
	}

	// KubeadmControlPlane: single replica and the empty-region provider-id arg.
	kcpObj, err := generate.FindOne(b, generate.ControlPlaneGroup, "KubeadmControlPlane")
	if err != nil {
		t.Fatalf("KubeadmControlPlane: %v", err)
	}
	if repl, _, _ := unstructured.NestedInt64(kcpObj.Object, "spec", "replicas"); repl != 1 {
		t.Errorf("replicas = %d", repl)
	}
	if !strings.Contains(rendered, "openstack:///{{ ds.meta_data.uuid }}") {
		t.Error("provider-id kubelet arg not wired to the empty-region Nova UUID form")
	}

	// OpenStackMachineTemplate: structured flavor/image and the SSH key.
	mt, err := generate.FindOne(b, generate.InfrastructureGroup, "OpenStackMachineTemplate")
	if err != nil {
		// FindOne errors when more than one exists; fall back to Find.
		mts := generate.Find(b, generate.InfrastructureGroup, "OpenStackMachineTemplate")
		if len(mts) == 0 {
			t.Fatalf("OpenStackMachineTemplate: %v", err)
		}
		mt = mts[0]
	}
	if fl, _, _ := unstructured.NestedString(mt.Object, "spec", "template", "spec", "flavor", "filter", "name"); fl != "m1.medium" {
		t.Errorf("flavor.filter.name = %q", fl)
	}
	if img, _, _ := unstructured.NestedString(mt.Object, "spec", "template", "spec", "image", "filter", "name"); img != "ubuntu-22.04" {
		t.Errorf("image.filter.name = %q", img)
	}
	if key, _, _ := unstructured.NestedString(mt.Object, "spec", "template", "spec", "sshKeyName"); key != "mykey" {
		t.Errorf("sshKeyName = %q", key)
	}

	// Adopt must find the control plane's template in this bundle.
	if _, _, err := controlPlaneTemplate(b); err != nil {
		t.Fatalf("controlPlaneTemplate on generated bundle: %v", err)
	}
}
