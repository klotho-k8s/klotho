package digitalocean

import (
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"

	"github.com/klotho-k8s/klotho/provider"
)

// Check reviews the generated DigitalOcean bundle for settings worth a decision
// before the cluster is created. It never edits the bundle; findings are advice.
func (p *Provider) Check(bundle []*unstructured.Unstructured) []provider.Finding {
	var doCluster *unstructured.Unstructured
	var machineTemplates []*unstructured.Unstructured
	for _, o := range bundle {
		if o == nil {
			continue
		}
		switch o.GetKind() {
		case "DOCluster":
			doCluster = o
		case "DOMachineTemplate":
			machineTemplates = append(machineTemplates, o)
		}
	}
	if doCluster == nil {
		return nil
	}

	var out []provider.Finding

	// An SSH key is the only way in if bootstrap fails; flag its absence. CAPDO
	// also requires DOMachine.spec.sshKeys, so an empty list means no diagnostic
	// access to any machine.
	hasKey := len(p.cfg.SSHKeys) > 0
	for _, t := range machineTemplates {
		if keys, found, _ := unstructured.NestedSlice(t.Object, "spec", "template", "spec", "sshKeys"); found && len(keys) > 0 {
			hasKey = true
		}
	}
	if !hasKey {
		out = append(out, provider.Finding{
			Object:  provider.ObjectRef(doCluster),
			Field:   "spec.template.spec.sshKeys",
			Message: "no SSH key is set on the machines. Without one there is no way to reach a node to diagnose a failed bootstrap. Set digitalocean.sshKeys (a fingerprint or ID already in your DigitalOcean account), or sshKeys in the template.",
		})
	}

	// The control plane load balancer is a standing hourly charge; make it a
	// conscious cost, not a surprise.
	out = append(out, provider.Finding{
		Object:  provider.ObjectRef(doCluster),
		Field:   "spec.network.apiServerLoadbalancers",
		Message: "CAPDO fronts the API server with a managed DigitalOcean load balancer (billed ~$12/mo even idle); it cannot be disabled upstream. klotho creates it up front so the endpoint is known before the certificates are minted, and CAPDO adopts it by UUID, but the load balancer is a standing cost for the life of the cluster. Delete the cluster when done.",
	})

	return out
}
