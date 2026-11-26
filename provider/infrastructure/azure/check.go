package azure

import (
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"

	"github.com/klotho-k8s/klotho/provider"
)

// Check reviews the generated Azure bundle for settings worth a decision before
// the cluster is created. It never edits the bundle; findings are advice.
func (p *Provider) Check(bundle []*unstructured.Unstructured) []provider.Finding {
	var azureCluster *unstructured.Unstructured
	var machineTemplates []*unstructured.Unstructured
	var identity *unstructured.Unstructured
	for _, o := range bundle {
		if o == nil {
			continue
		}
		switch o.GetKind() {
		case "AzureCluster":
			azureCluster = o
		case "AzureMachineTemplate":
			machineTemplates = append(machineTemplates, o)
		case "AzureClusterIdentity":
			identity = o
		}
	}
	if azureCluster == nil {
		return nil
	}

	var out []provider.Finding

	// An SSH key on the machines is the only way in if something goes wrong
	// during bootstrap; flag its absence.
	hasKey := p.cfg.SSHPublicKey != ""
	for _, t := range machineTemplates {
		if keys, found, _ := unstructured.NestedSlice(t.Object, "spec", "template", "spec", "sshPublicKey"); found && len(keys) > 0 {
			hasKey = true
		}
		if v, found, _ := unstructured.NestedString(t.Object, "spec", "template", "spec", "sshPublicKey"); found && v != "" {
			hasKey = true
		}
	}
	if !hasKey {
		out = append(out, provider.Finding{
			Object:  provider.ObjectRef(azureCluster),
			Field:   "spec.template.spec.sshPublicKey",
			Message: "no SSH key is set on the control plane machines. Without one there is no way to reach a node to diagnose a failed bootstrap. Set azure.sshPublicKey, or sshPublicKey in the template.",
		})
	}

	// CAPZ cannot run without an API server load balancer; an Internal one uses
	// a private frontend and no extra public IP. A Public one allocates a public
	// IP and bills more. Flag a Public API server LB so it is a conscious choice.
	if lbType, found, _ := unstructured.NestedString(azureCluster.Object, "spec", "networkSpec", "apiServerLB", "type"); found {
		if lbType == "Public" {
			out = append(out, provider.Finding{
				Object:  provider.ObjectRef(azureCluster),
				Field:   "spec.networkSpec.apiServerLB.type",
				Message: "apiServerLB.type is Public, which allocates a public Standard load balancer and public IP (billed hourly). klotho's cheap flat template uses \"Internal\" and serves the API on node zero's own public IP; CAPZ cannot drop the load balancer entirely. Use Public only if you want a managed public API endpoint.",
			})
		}
	}

	// The control plane endpoint must be known before the certificates are
	// minted; flag its absence so it is set deliberately.
	if host, found, _ := unstructured.NestedString(azureCluster.Object, "spec", "controlPlaneEndpoint", "host"); !found || host == "" {
		if p.cfg.ControlPlaneEndpoint == "" {
			out = append(out, provider.Finding{
				Object:  provider.ObjectRef(azureCluster),
				Field:   "spec.controlPlaneEndpoint",
				Message: "no control plane endpoint is set. klotho needs the endpoint before it mints certificates; it allocates a static public IP for node zero and uses that. Set azure.controlPlaneEndpoint to override.",
			})
		}
	}

	// CAPZ needs an AzureClusterIdentity to authenticate; flag its absence.
	if identity == nil {
		out = append(out, provider.Finding{
			Object:  provider.ObjectRef(azureCluster),
			Field:   "spec.identityRef",
			Message: "no AzureClusterIdentity is in the bundle. CAPZ authenticates to Azure through an AzureClusterIdentity referenced by spec.identityRef; without it the cluster cannot reconcile. Set azure.tenantId/clientId/clientSecret so klotho renders one.",
		})
	}

	return out
}
