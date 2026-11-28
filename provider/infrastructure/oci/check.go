package oci

import (
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"

	"github.com/klotho-k8s/klotho/provider"
)

// Check reviews the generated OCI bundle for settings worth a decision before
// the cluster is created. It never edits the bundle; findings are advice.
func (p *Provider) Check(bundle []*unstructured.Unstructured) []provider.Finding {
	var ociCluster *unstructured.Unstructured
	var machineTemplates []*unstructured.Unstructured
	for _, o := range bundle {
		if o == nil {
			continue
		}
		switch o.GetKind() {
		case "OCICluster":
			ociCluster = o
		case "OCIMachineTemplate":
			machineTemplates = append(machineTemplates, o)
		}
	}
	if ociCluster == nil {
		return nil
	}

	var out []provider.Finding

	// ociResourceIdentifier must be pinned for klotho's adoption to be
	// deterministic: it is the value of the ClusterResourceIdentifier freeform
	// tag Launch stamps, and CAPOCI matches the hand-booted instance against it.
	// If it is unset, CAPOCI's webhook assigns a random UUID at apply time, which
	// klotho cannot have stamped — so the instance would not be adopted.
	if id, found, _ := unstructured.NestedString(ociCluster.Object, "spec", "ociResourceIdentifier"); !found || id == "" {
		out = append(out, provider.Finding{
			Object:  provider.ObjectRef(ociCluster),
			Field:   "spec.ociResourceIdentifier",
			Message: "ociResourceIdentifier is not pinned. CAPOCI adopts the hand-booted node by display name cross-checked against its ClusterResourceIdentifier freeform tag; klotho stamps that tag before boot from this field. If it is left empty CAPOCI assigns a random UUID at apply time and will not recognise node zero, launching a duplicate. klotho's flat template pins it to the cluster name.",
		})
	}

	// An SSH key on the machines is the only way in if something goes wrong
	// during bootstrap; flag its absence.
	hasKey := len(p.cfg.SSHAuthorizedKeys) > 0
	for _, t := range machineTemplates {
		if md, found, _ := unstructured.NestedStringMap(t.Object, "spec", "template", "spec", "metadata"); found {
			if md["ssh_authorized_keys"] != "" {
				hasKey = true
			}
		}
	}
	if !hasKey {
		out = append(out, provider.Finding{
			Object:  provider.ObjectRef(ociCluster),
			Field:   "spec.template.spec.metadata.ssh_authorized_keys",
			Message: "no SSH key is set on the control plane machines. Without one there is no way to reach a node to diagnose a failed bootstrap. Set oci.sshAuthorizedKeys, or metadata.ssh_authorized_keys in the template.",
		})
	}

	return out
}
