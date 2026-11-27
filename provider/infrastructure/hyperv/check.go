package hyperv

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

	hc, err := generate.FindOne(b, generate.InfrastructureGroup, "HyperVCluster")
	if err != nil {
		findings = append(findings, provider.Finding{
			Object:  "HyperVCluster",
			Message: "the bundle has no HyperVCluster; the Hyper-V provider expects a cluster template with HyperVCluster, HyperVMachineTemplate, KubeadmControlPlane and MachineDeployment.",
		})
		return findings
	}

	findings = append(findings, provider.Finding{
		Object:  provider.ObjectRef(hc),
		Message: "the Cluster API Hyper-V provider's in-cluster manager must reach the Windows host over the network (WinRM); klotho boots the first node from the host, but the cluster cannot self-manage further machines until the provider is configured for remote Hyper-V access.",
	})

	if tmpls := generate.Find(b, generate.InfrastructureGroup, "HyperVMachineTemplate"); len(tmpls) > 0 {
		for _, t := range tmpls {
			if v, ok, _ := unstructured.NestedString(t.Object, "spec", "template", "spec", "image"); !ok || v == "" {
				findings = append(findings, provider.Finding{
					Object:  provider.ObjectRef(t),
					Field:   "spec.template.spec.image",
					Message: "no VHDX image set; the Hyper-V provider cannot create a bootable VM without a base disk.",
				})
			}
		}
	}
	return findings
}
