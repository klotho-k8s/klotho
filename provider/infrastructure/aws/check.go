package aws

import (
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"

	"github.com/klotho-k8s/klotho/provider"
)

// Check reviews the generated AWS bundle for settings worth a decision before
// the cluster is created. It never edits the bundle; findings are advice.
func (p *Provider) Check(bundle []*unstructured.Unstructured) []provider.Finding {
	var awsCluster *unstructured.Unstructured
	var machineTemplates []*unstructured.Unstructured
	for _, o := range bundle {
		if o == nil {
			continue
		}
		switch o.GetKind() {
		case "AWSCluster":
			awsCluster = o
		case "AWSMachineTemplate":
			machineTemplates = append(machineTemplates, o)
		}
	}
	if awsCluster == nil {
		return nil
	}

	var out []provider.Finding

	// An SSH key on the machines is the only way in if something goes wrong
	// during bootstrap; flag its absence.
	hasKey := false
	for _, t := range machineTemplates {
		if v, found, _ := unstructured.NestedString(t.Object, "spec", "template", "spec", "sshKeyName"); found && v != "" {
			hasKey = true
		}
	}
	if !hasKey && p.cfg.SSHKeyName == "" {
		out = append(out, provider.Finding{
			Object:  provider.ObjectRef(awsCluster),
			Field:   "spec.template.spec.sshKeyName",
			Message: "no SSH key is set on the control plane machines. Without one there is no way to reach a node to diagnose a failed bootstrap. Set aws.sshKeyName, or AWS_SSH_KEY_NAME in the template.",
		})
	}

	// A publicly reachable control plane endpoint on a cluster created from the
	// default flavor is worth a conscious choice.
	if host, found, _ := unstructured.NestedString(awsCluster.Object, "spec", "controlPlaneEndpoint", "host"); found && host != "" {
		_ = host // present; nothing to flag
	} else if p.cfg.ControlPlaneEndpoint == "" && !p.cfg.AllocateElasticIP {
		out = append(out, provider.Finding{
			Object:  provider.ObjectRef(awsCluster),
			Field:   "spec.controlPlaneEndpoint",
			Message: "no control plane endpoint is set and none will be allocated. klotho needs the endpoint before it mints certificates. Set aws.controlPlaneEndpoint or aws.allocateElasticIp.",
		})
	}

	return out
}
