package aws

import (
	"context"
	"fmt"
	"strings"

	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"sigs.k8s.io/yaml"

	"github.com/klotho-k8s/klotho/pkg/cluster"
	"github.com/klotho-k8s/klotho/pkg/generate"
)

// Label and annotation keys the adopted machine carries. The cloned-from pair
// is what the KubeadmControlPlane controller compares with its
// spec.machineTemplate.spec.infrastructureRef to decide whether a machine is
// out of date; matching values mean adoption triggers no rollout.
const (
	labelClusterName          = "cluster.x-k8s.io/cluster-name"
	labelControlPlane         = "cluster.x-k8s.io/control-plane"
	labelControlPlaneName     = "cluster.x-k8s.io/control-plane-name"
	annotationClonedFromName  = "cluster.x-k8s.io/cloned-from-name"
	annotationClonedFromGroup = "cluster.x-k8s.io/cloned-from-groupkind"
	controlPlaneGroup         = "controlplane.cluster.x-k8s.io"
)

// Adopt renders the AWSMachine the control plane adopts for the bootstrap
// instance. It copies the AWSMachineTemplate the control plane references and
// names the AWSMachine MachineName(cluster), the same name the kubeadm
// provider gives the bootstrap Machine's infrastructureRef.
//
// It does NOT set spec.providerID. CAPA's AWSMachine controller, finding no
// providerID, calls GetRunningInstanceByTags, which filters on the Name tag
// (equal to this AWSMachine's name) and the cluster-owned tag. Launch tagged
// the running instance with exactly those, so CAPA adopts it and sets
// providerID itself, instead of launching a new instance. Pre-setting
// providerID is unreliable (CAPA treats an externally set providerID as a
// not-yet-found instance and can create a duplicate), so we rely on the tag
// match, which is CAPA's documented adoption path.
func (p *Provider) Adopt(_ context.Context, state *cluster.State, bundle []*unstructured.Unstructured) ([]byte, error) {
	if state == nil || state.Spec.Name == "" {
		return nil, errorf("state has no cluster name")
	}
	name := state.Spec.Name

	tmpl, kcp, err := controlPlaneTemplate(&generate.Bundle{Objects: bundle})
	if err != nil {
		return nil, err
	}

	spec, found, err := unstructured.NestedMap(tmpl.Object, "spec", "template", "spec")
	if err != nil {
		return nil, errorf("reading spec.template.spec of %s %q: %v", tmpl.GetKind(), tmpl.GetName(), err)
	}
	if !found {
		spec = map[string]any{}
	}

	labels, _, _ := unstructured.NestedStringMap(tmpl.Object, "spec", "template", "metadata", "labels")
	annotations, _, _ := unstructured.NestedStringMap(tmpl.Object, "spec", "template", "metadata", "annotations")
	if labels == nil {
		labels = map[string]string{}
	}
	if annotations == nil {
		annotations = map[string]string{}
	}
	labels[labelClusterName] = name
	labels[labelControlPlane] = ""
	if kcp != nil {
		labels[labelControlPlaneName] = kcp.GetName()
	}
	gvk := tmpl.GroupVersionKind()
	annotations[annotationClonedFromName] = tmpl.GetName()
	annotations[annotationClonedFromGroup] = gvk.Kind + "." + gvk.Group

	namespace := tmpl.GetNamespace()
	if namespace == "" {
		namespace = state.Spec.Namespace
	}
	m := &unstructured.Unstructured{Object: map[string]any{"spec": spec}}
	m.SetAPIVersion(tmpl.GetAPIVersion())
	m.SetKind(strings.TrimSuffix(gvk.Kind, "Template"))
	m.SetName(MachineName(name))
	if namespace != "" {
		m.SetNamespace(namespace)
	}
	m.SetLabels(labels)
	m.SetAnnotations(annotations)

	return yaml.Marshal(m.Object)
}

// controlPlaneTemplate returns the infrastructure MachineTemplate the control
// plane references, and the control plane object, from the bundle.
func controlPlaneTemplate(b *generate.Bundle) (*unstructured.Unstructured, *unstructured.Unstructured, error) {
	kcp, err := generate.FindOne(b, controlPlaneGroup, "ControlPlane")
	if err != nil {
		return nil, nil, err
	}
	ref, err := infrastructureRefName(kcp)
	if err != nil {
		return nil, nil, err
	}
	for _, t := range generate.Find(b, generate.InfrastructureGroup, "MachineTemplate") {
		if t.GetName() == ref {
			return t, kcp, nil
		}
	}
	return nil, nil, fmt.Errorf("bundle has no %s MachineTemplate named %q", generate.InfrastructureGroup, ref)
}

// infrastructureRefName reads the machine template reference of a control plane
// object: spec.machineTemplate.spec.infrastructureRef (v1beta2) or
// spec.machineTemplate.infrastructureRef (v1beta1).
func infrastructureRefName(kcp *unstructured.Unstructured) (string, error) {
	for _, path := range [][]string{
		{"spec", "machineTemplate", "spec", "infrastructureRef", "name"},
		{"spec", "machineTemplate", "infrastructureRef", "name"},
	} {
		if v, found, _ := unstructured.NestedString(kcp.Object, path...); found && v != "" {
			return v, nil
		}
	}
	return "", fmt.Errorf("control plane %q has no machineTemplate infrastructureRef name", kcp.GetName())
}
