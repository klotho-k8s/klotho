package openstack

import (
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"

	"github.com/klotho-k8s/klotho/provider"
)

// Check reviews the generated OpenStack bundle for settings worth a decision
// before the cluster is created. It never edits the bundle; findings are advice.
func (p *Provider) Check(bundle []*unstructured.Unstructured) []provider.Finding {
	var osCluster *unstructured.Unstructured
	for _, o := range bundle {
		if o == nil {
			continue
		}
		if o.GetKind() == "OpenStackCluster" {
			osCluster = o
		}
	}
	if osCluster == nil {
		return nil
	}

	var out []provider.Finding

	// A keypair is the only way in if bootstrap fails; flag its absence.
	if p.cfg.SSHKeyName == "" {
		out = append(out, provider.Finding{
			Object:  provider.ObjectRef(osCluster),
			Field:   "openstack.sshKeyName",
			Message: "no Nova keypair is set for the bootstrap node. Without one there is no way to reach it to diagnose a failed bootstrap. Set openstack.sshKeyName to an existing keypair.",
		})
	}

	// The endpoint is node zero's own floating IP (no Octavia LB). Make it a
	// conscious choice: if node zero is lost before pivot, the endpoint is lost.
	if p.cfg.ControlPlaneEndpoint == "" {
		out = append(out, provider.Finding{
			Object:  provider.ObjectRef(osCluster),
			Field:   "spec.apiServer.floatingIP",
			Message: "the control plane endpoint is a Neutron floating IP klotho allocated and associated with node zero; no Octavia load balancer is provisioned (the cheapest option). The floating IP is stable across the node's life, but a single-control-plane cluster has no API-server redundancy. For HA, front the control plane with your own Octavia VIP and set openstack.controlPlaneEndpoint to it.",
		})
	}

	return out
}
