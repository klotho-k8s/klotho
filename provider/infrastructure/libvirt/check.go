package libvirt

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

	lc, err := generate.FindOne(b, generate.InfrastructureGroup, "LibvirtCluster")
	if err != nil {
		findings = append(findings, provider.Finding{
			Object:  "LibvirtCluster",
			Message: "the bundle has no LibvirtCluster; the libvirt provider expects a flat template with LibvirtCluster, LibvirtMachineTemplate and KubeadmControlPlane.",
		})
		return findings
	}

	if uri, ok, _ := unstructured.NestedString(lc.Object, "spec", "connectionURI"); !ok || uri == "" {
		findings = append(findings, provider.Finding{
			Object:  provider.ObjectRef(lc),
			Field:   "spec.connectionURI",
			Message: "no connection URI set; the in-cluster Cluster API libvirt controller cannot reach libvirtd to reconcile further machines (set libvirt.uri or the LIBVIRT_URI variable).",
		})
	}
	if host, ok, _ := unstructured.NestedString(lc.Object, "spec", "controlPlaneEndpoint", "host"); !ok || host == "" {
		findings = append(findings, provider.Finding{
			Object:  provider.ObjectRef(lc),
			Field:   "spec.controlPlaneEndpoint.host",
			Message: "no control plane endpoint set; libvirt has no load balancer, so set it to the static address node zero will have (CONTROL_PLANE_ENDPOINT_HOST).",
		})
	}

	for _, t := range generate.Find(b, generate.InfrastructureGroup, "LibvirtMachineTemplate") {
		if v, ok, _ := unstructured.NestedString(t.Object, "spec", "template", "spec", "image"); !ok || v == "" {
			findings = append(findings, provider.Finding{
				Object:  provider.ObjectRef(t),
				Field:   "spec.template.spec.image",
				Message: "no base image set; the libvirt provider cannot create a bootable domain without a qcow2 base image volume in the storage pool.",
			})
		}
	}
	return findings
}
