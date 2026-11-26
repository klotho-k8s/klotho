package linode

import (
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"

	"github.com/klotho-k8s/klotho/provider"
)

// Check reviews the generated Linode bundle for settings worth a decision
// before the cluster is created. It never edits the bundle; findings are advice.
func (p *Provider) Check(bundle []*unstructured.Unstructured) []provider.Finding {
	var linodeCluster *unstructured.Unstructured
	var machineTemplates []*unstructured.Unstructured
	for _, o := range bundle {
		if o == nil {
			continue
		}
		switch o.GetKind() {
		case "LinodeCluster":
			linodeCluster = o
		case "LinodeMachineTemplate":
			machineTemplates = append(machineTemplates, o)
		}
	}
	if linodeCluster == nil {
		return nil
	}

	var out []provider.Finding

	// An SSH key on the machines is the only way in if something goes wrong
	// during bootstrap; flag its absence.
	hasKey := len(p.cfg.AuthorizedKeys) > 0
	for _, t := range machineTemplates {
		if keys, found, _ := unstructured.NestedSlice(t.Object, "spec", "template", "spec", "authorizedKeys"); found && len(keys) > 0 {
			hasKey = true
		}
	}
	if !hasKey {
		out = append(out, provider.Finding{
			Object:  provider.ObjectRef(linodeCluster),
			Field:   "spec.template.spec.authorizedKeys",
			Message: "no SSH key is set on the control plane machines. Without one there is no way to reach a node to diagnose a failed bootstrap. Set linode.authorizedKeys, or authorizedKeys in the template.",
		})
	}

	// A NodeBalancer is billed hourly and is not klotho's cheap default; flag
	// it so leaving loadBalancerType at its NodeBalancer default is a conscious
	// choice rather than an accident.
	if lbType, found, _ := unstructured.NestedString(linodeCluster.Object, "spec", "network", "loadBalancerType"); found {
		if lbType == "NodeBalancer" {
			out = append(out, provider.Finding{
				Object:  provider.ObjectRef(linodeCluster),
				Field:   "spec.network.loadBalancerType",
				Message: "loadBalancerType is NodeBalancer, which provisions a NodeBalancer billed hourly (~$10/mo). klotho's cheap flat template uses \"external\" with a control plane endpoint you supply. Leave it as NodeBalancer only if you want a managed load balancer.",
			})
		}
	}

	return out
}
