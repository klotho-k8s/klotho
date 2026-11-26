package gcp

import (
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"

	"github.com/klotho-k8s/klotho/provider"
)

// Check reviews the generated GCP bundle for settings worth a decision before
// the cluster is created. It never edits the bundle; findings are advice.
func (p *Provider) Check(bundle []*unstructured.Unstructured) []provider.Finding {
	var gcpCluster *unstructured.Unstructured
	var machineTemplates []*unstructured.Unstructured
	for _, o := range bundle {
		if o == nil {
			continue
		}
		switch o.GetKind() {
		case "GCPCluster":
			gcpCluster = o
		case "GCPMachineTemplate":
			machineTemplates = append(machineTemplates, o)
		}
	}
	if gcpCluster == nil {
		return nil
	}

	var out []provider.Finding

	// An SSH key is the only way in if bootstrap fails; flag its absence.
	if len(p.cfg.AuthorizedKeys) == 0 {
		out = append(out, provider.Finding{
			Object:  provider.ObjectRef(gcpCluster),
			Field:   "gcp.authorizedKeys",
			Message: "no SSH key is set for the bootstrap node. Without one there is no way to reach it to diagnose a failed bootstrap. Set gcp.authorizedKeys (installed via the instance \"ssh-keys\" metadata).",
		})
	}

	// The managed control plane load balancer is unavoidable in stock CAPG and
	// is billed hourly; make it a conscious cost, not a surprise.
	out = append(out, provider.Finding{
		Object:  provider.ObjectRef(gcpCluster),
		Field:   "spec.loadBalancer",
		Message: "CAPG always provisions a managed external load balancer for the API server (a global forwarding rule, backend service and static IP), billed ~$20-30/mo even idle; it cannot be disabled upstream. klotho reserves the load balancer's IP up front so the endpoint is stable, but the load balancer itself is a standing cost for the life of the cluster. Delete the cluster when done.",
	})

	// Spot/preemptible control plane nodes are reclaimable; note when they are
	// used so it is a deliberate choice for a throwaway cluster.
	preempt := p.cfg.Preemptible
	for _, t := range machineTemplates {
		if v, found, _ := unstructured.NestedBool(t.Object, "spec", "template", "spec", "preemptible"); found && v {
			preempt = true
		}
		if v, found, _ := unstructured.NestedString(t.Object, "spec", "template", "spec", "provisioningModel"); found && v == "SPOT" {
			preempt = true
		}
	}
	if preempt {
		out = append(out, provider.Finding{
			Object:  provider.ObjectRef(gcpCluster),
			Field:   "spec.template.spec.preemptible",
			Message: "the machines are Spot/preemptible: cheap, but GCP can reclaim them at any time and a reclaimed node is terminated, not restarted. Fine for a throwaway test cluster; do not use for anything you want to keep.",
		})
	}

	return out
}
