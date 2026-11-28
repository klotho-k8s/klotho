package vsphere

import (
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"

	"github.com/klotho-k8s/klotho/pkg/generate"
	"github.com/klotho-k8s/klotho/provider"
)

var _ provider.Checker = (*Provider)(nil)

// Check reviews the generated bundle. It implements provider.Checker. The
// findings are advisory: klotho never edits the bundle to resolve one.
func (p *Provider) Check(bundle []*unstructured.Unstructured) []provider.Finding {
	b := &generate.Bundle{Objects: bundle}
	var findings []provider.Finding

	vc, err := generate.FindOne(b, generate.InfrastructureGroup, "VSphereCluster")
	if err != nil {
		findings = append(findings, provider.Finding{
			Object:  "VSphereCluster",
			Message: "the bundle has no VSphereCluster; the vSphere provider expects a flat template with VSphereCluster, VSphereMachineTemplate and KubeadmControlPlane.",
		})
		return findings
	}

	if server, ok, _ := unstructured.NestedString(vc.Object, "spec", "server"); !ok || server == "" {
		findings = append(findings, provider.Finding{
			Object:  provider.ObjectRef(vc),
			Field:   "spec.server",
			Message: "no vCenter server set; the in-cluster CAPV controller cannot reach vCenter to reconcile further machines (set vsphere.server or the VSPHERE_SERVER variable).",
		})
	}
	if host, ok, _ := unstructured.NestedString(vc.Object, "spec", "controlPlaneEndpoint", "host"); !ok || host == "" {
		findings = append(findings, provider.Finding{
			Object:  provider.ObjectRef(vc),
			Field:   "spec.controlPlaneEndpoint.host",
			Message: "no control plane endpoint set; vSphere has no load balancer, so set it to the static VIP kube-vip will hold (CONTROL_PLANE_ENDPOINT_IP).",
		})
	}

	for _, t := range generate.Find(b, generate.InfrastructureGroup, "VSphereMachineTemplate") {
		if v, ok, _ := unstructured.NestedString(t.Object, "spec", "template", "spec", "template"); !ok || v == "" {
			findings = append(findings, provider.Finding{
				Object:  provider.ObjectRef(t),
				Field:   "spec.template.spec.template",
				Message: "no source template set; the vSphere provider cannot clone a bootable VM without a template (VSPHERE_TEMPLATE).",
			})
		}
	}
	return findings
}
