package vz

import (
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"

	"github.com/klotho-k8s/klotho/pkg/generate"
	"github.com/klotho-k8s/klotho/provider"
)

// Check reviews the generated bundle. It implements provider.Checker. The
// findings are advisory: klotho never edits the bundle to resolve one.
func (p *Provider) Check(bundle []*unstructured.Unstructured) []provider.Finding {
	b := &generate.Bundle{Objects: bundle}
	var findings []provider.Finding

	vc, err := generate.FindOne(b, generate.InfrastructureGroup, "VZCluster")
	if err != nil {
		findings = append(findings, provider.Finding{
			Object:  "VZCluster",
			Message: "the bundle has no VZCluster; the vz provider expects a flat template with VZCluster, VZMachineTemplate and KubeadmControlPlane.",
		})
		return findings
	}

	if host, ok, _ := unstructured.NestedString(vc.Object, "spec", "controlPlaneEndpoint", "host"); !ok || host == "" {
		findings = append(findings, provider.Finding{
			Object:  provider.ObjectRef(vc),
			Field:   "spec.controlPlaneEndpoint.host",
			Message: "no control plane endpoint set; vz has no load balancer, so set it to the static address node zero will have (CONTROL_PLANE_ENDPOINT_HOST).",
		})
	}
	if host, ok, _ := unstructured.NestedString(vc.Object, "spec", "host"); !ok || host == "" {
		findings = append(findings, provider.Finding{
			Object:  provider.ObjectRef(vc),
			Field:   "spec.host",
			Message: "no vz host set; it is recorded in the machine providerID (vz://<host>/<vmid>) and must match the kubelet --provider-id host, or the Machine<->Node nodeRef will not complete (VZ_HOST, default localhost).",
		})
	}

	for _, t := range generate.Find(b, generate.InfrastructureGroup, "VZMachineTemplate") {
		if v, ok, _ := unstructured.NestedString(t.Object, "spec", "template", "spec", "image"); !ok || v == "" {
			findings = append(findings, provider.Finding{
				Object:  provider.ObjectRef(t),
				Field:   "spec.template.spec.image",
				Message: "no base image set; the vz provider cannot boot a VM without a raw arm64 cloud image path on the Mac host (VZ_IMAGE_PATH).",
			})
		}
	}
	return findings
}
