package hetzner

import (
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"

	"github.com/klotho-k8s/klotho/provider"
)

// Check reviews the generated Hetzner bundle for settings worth a decision
// before the cluster is created. It never edits the bundle; findings are advice.
func (p *Provider) Check(bundle []*unstructured.Unstructured) []provider.Finding {
	var hetznerCluster *unstructured.Unstructured
	var machineTemplates []*unstructured.Unstructured
	for _, o := range bundle {
		if o == nil {
			continue
		}
		switch o.GetKind() {
		case "HetznerCluster":
			hetznerCluster = o
		case "HCloudMachineTemplate":
			machineTemplates = append(machineTemplates, o)
		}
	}
	if hetznerCluster == nil {
		return nil
	}

	var out []provider.Finding

	// An SSH key on the machines is the only way in if something goes wrong during
	// bootstrap; flag its absence. CAPH's HetznerCluster requires sshKeys.hcloud,
	// so this normally fires only on a hand-edited template.
	hasKey := len(p.cfg.SSHKeys) > 0
	if keys, found, _ := unstructured.NestedSlice(hetznerCluster.Object, "spec", "sshKeys", "hcloud"); found && len(keys) > 0 {
		hasKey = true
	}
	for _, t := range machineTemplates {
		if keys, found, _ := unstructured.NestedSlice(t.Object, "spec", "template", "spec", "sshKeys"); found && len(keys) > 0 {
			hasKey = true
		}
	}
	if !hasKey {
		out = append(out, provider.Finding{
			Object:  provider.ObjectRef(hetznerCluster),
			Field:   "spec.sshKeys.hcloud",
			Message: "no SSH key is set on the cluster. Without one there is no way to reach a node to diagnose a failed bootstrap (and CAPH requires one). Set hetzner.sshKeys, or spec.sshKeys.hcloud in the template.",
		})
	}

	// CAPH's managed control-plane load balancer is billed hourly; klotho's cheap
	// flat template disables it and uses a supplied endpoint. Flag leaving it on so
	// it is a conscious choice rather than an accident.
	if enabled, found, _ := unstructured.NestedBool(hetznerCluster.Object, "spec", "controlPlaneLoadBalancer", "enabled"); found && enabled {
		out = append(out, provider.Finding{
			Object:  provider.ObjectRef(hetznerCluster),
			Field:   "spec.controlPlaneLoadBalancer.enabled",
			Message: "controlPlaneLoadBalancer.enabled is true, which provisions a Hetzner Cloud load balancer billed hourly (~€5+/mo). klotho's cheap flat template sets it false and uses the control plane endpoint you supply (hetzner.controlPlaneEndpoint). Leave it enabled only if you want a managed load balancer.",
		})
	}

	return out
}
