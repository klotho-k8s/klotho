package oci

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
// objects the adopt/pivot model needs.
func TestClusterTemplateGeneratesExpectedObjects(t *testing.T) {
	vars := map[string]string{
		"${CLUSTER_NAME}":                   "lab1",
		"${NAMESPACE}":                      "default",
		"${KUBERNETES_VERSION}":             "v1.33.4",
		"${CONTROL_PLANE_MACHINE_COUNT}":    "1",
		"${WORKER_MACHINE_COUNT}":           "0",
		"${CONTROL_PLANE_ENDPOINT_IP}":      "203.0.113.10",
		"${CONTROL_PLANE_ENDPOINT_PORT}":    "6443",
		"${OCI_COMPARTMENT_ID}":             "ocid1.compartment.oc1..cmp",
		"${OCI_RESOURCE_IDENTIFIER}":        "lab1",
		"${OCI_VCN_ID}":                     "ocid1.vcn.oc1..vcn",
		"${OCI_SUBNET_ID}":                  "ocid1.subnet.oc1..sub",
		"${OCI_IMAGE_ID}":                   "ocid1.image.oc1..ubuntu",
		"${OCI_CONTROL_PLANE_MACHINE_TYPE}": "VM.Standard.E4.Flex",
		"${OCI_NODE_MACHINE_TYPE}":          "VM.Standard.E4.Flex",
		"${OCI_MACHINE_TYPE_OCPUS}":         "2",
		"${OCI_MACHINE_TYPE_MEMORY_GBS}":    "16",
		"${OCI_SSH_KEY}":                    "ssh-ed25519 AAAA",
	}
	rendered := substitute(clusterTemplate, vars)
	if i := strings.Index(rendered, "${"); i >= 0 {
		t.Fatalf("unsubstituted variable remains near: %s", rendered[i:min(i+40, len(rendered))])
	}

	b, err := generate.ParseBundle([]byte(rendered))
	if err != nil {
		t.Fatalf("ParseBundle: %v", err)
	}

	// Cluster carries the skip-apiserver-lb-management annotation.
	clusterObj, err := generate.FindOne(b, generate.ClusterGroup, "Cluster")
	if err != nil {
		t.Fatalf("Cluster: %v", err)
	}
	if clusterObj.GetName() != "lab1" {
		t.Errorf("cluster name = %q", clusterObj.GetName())
	}
	ann := clusterObj.GetAnnotations()
	if ann["cluster.x-k8s.io/skip-apiserver-lb-management"] != "true" {
		t.Errorf("missing skip-apiserver-lb-management annotation: %v", ann)
	}

	// OCICluster: pinned resource identifier, patched endpoint, existing-VCN skip.
	oc, err := generate.FindOne(b, generate.InfrastructureGroup, "OCICluster")
	if err != nil {
		t.Fatalf("OCICluster: %v", err)
	}
	if id, _, _ := unstructured.NestedString(oc.Object, "spec", "ociResourceIdentifier"); id != "lab1" {
		t.Errorf("ociResourceIdentifier = %q, want lab1", id)
	}
	if host, _, _ := unstructured.NestedString(oc.Object, "spec", "controlPlaneEndpoint", "host"); host != "203.0.113.10" {
		t.Errorf("endpoint host = %q", host)
	}
	if skip, _, _ := unstructured.NestedBool(oc.Object, "spec", "networkSpec", "vcn", "skip"); !skip {
		t.Error("networkSpec.vcn.skip is not true (klotho uses an existing VCN)")
	}
	if vcn, _, _ := unstructured.NestedString(oc.Object, "spec", "networkSpec", "vcn", "id"); vcn != "ocid1.vcn.oc1..vcn" {
		t.Errorf("vcn id = %q", vcn)
	}

	// KubeadmControlPlane: single replica, provider-id kubelet arg wired to the
	// OCI instance id.
	kcpObj, err := generate.FindOne(b, generate.ControlPlaneGroup, "KubeadmControlPlane")
	if err != nil {
		t.Fatalf("KubeadmControlPlane: %v", err)
	}
	if repl, _, _ := unstructured.NestedInt64(kcpObj.Object, "spec", "replicas"); repl != 1 {
		t.Errorf("replicas = %d", repl)
	}
	if !strings.Contains(rendered, `provider-id: oci://{{ ds["id"] }}`) {
		t.Error("provider-id kubelet arg not wired to the OCI instance id")
	}

	// OCIMachineTemplate for the control plane carries the image and shape.
	mt, err := controlPlaneTemplateInBundle(b)
	if err != nil {
		t.Fatalf("control plane OCIMachineTemplate: %v", err)
	}
	if img, _, _ := unstructured.NestedString(mt.Object, "spec", "template", "spec", "imageId"); img != "ocid1.image.oc1..ubuntu" {
		t.Errorf("imageId = %q", img)
	}
	if shape, _, _ := unstructured.NestedString(mt.Object, "spec", "template", "spec", "shape"); shape != "VM.Standard.E4.Flex" {
		t.Errorf("shape = %q", shape)
	}

	// Adopt must find the control plane's template in this bundle.
	if _, _, err := controlPlaneTemplate(b); err != nil {
		t.Fatalf("controlPlaneTemplate on generated bundle: %v", err)
	}
}

// controlPlaneTemplateInBundle returns the control-plane OCIMachineTemplate.
func controlPlaneTemplateInBundle(b *generate.Bundle) (*unstructured.Unstructured, error) {
	t, _, err := controlPlaneTemplate(b)
	return t, err
}

// substitute replaces ${VAR} and ${VAR:=default} / ${VAR=default} forms. Any
// known ${VAR} (optionally with a default suffix) is replaced by vars[VAR]; a
// default is used only when the value is empty.
func substitute(s string, vars map[string]string) string {
	for k, v := range vars {
		name := strings.TrimSuffix(strings.TrimPrefix(k, "${"), "}")
		// Replace the defaulted forms first, then the bare form.
		for _, form := range []string{
			"${" + name + ":=",
			"${" + name + "=",
		} {
			for {
				i := strings.Index(s, form)
				if i < 0 {
					break
				}
				end := strings.Index(s[i:], "}")
				if end < 0 {
					break
				}
				s = s[:i] + v + s[i+end+1:]
			}
		}
		s = strings.ReplaceAll(s, k, v)
	}
	return s
}
