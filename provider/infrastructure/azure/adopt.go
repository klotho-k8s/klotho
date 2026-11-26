package azure

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

// Adopt renders the AzureMachine the control plane adopts for the bootstrap VM.
// It copies the AzureMachineTemplate the control plane references, names the
// AzureMachine MachineName(cluster) — the name the kubeadm provider gives the
// bootstrap Machine's infrastructureRef — and sets spec.providerID to the
// running VM ("azure://" + the ARM resource ID).
//
// Both the name and the provider ID make CAPZ adopt the VM klotho booted: CAPZ
// locates the VM by resource name, which it derives from the provider ID when
// set and otherwise from the AzureMachine name; since the VM already exists and
// is not Failed, CAPZ issues no CreateOrUpdate and leaves it untouched (see the
// package doc). No infrastructure cluster patch is rendered; klotho already set
// the generated AzureCluster's controlPlaneEndpoint. (This is the CAPL model,
// the opposite of the AWS provider where CAPA adopts by tag and must NOT see a
// pre-set provider ID.)
func (p *Provider) Adopt(_ context.Context, state *cluster.State, bundle []*unstructured.Unstructured) ([]byte, error) {
	if state == nil || state.Spec.Name == "" {
		return nil, errorf("state has no cluster name")
	}
	name := state.Spec.Name

	tmpl, kcp, err := controlPlaneTemplate(&generate.Bundle{Objects: bundle})
	if err != nil {
		return nil, err
	}

	pid, err := p.bootstrapProviderID(state)
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
	spec["providerID"] = pid

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

	out, err := yaml.Marshal(m.Object)
	if err != nil {
		return nil, errorf("rendering %s: %v", m.GetKind(), err)
	}
	return append([]byte("---\n"), out...), nil
}

// bootstrapProviderID returns the provider ID of the bootstrap VM: state's
// record from Launch when present, otherwise re-found by the VM's resource name
// so a lost status update does not strand the adoption.
func (p *Provider) bootstrapProviderID(state *cluster.State) (string, error) {
	if state.Bootstrap != nil && strings.HasPrefix(state.Bootstrap.ID, ProviderIDPrefix) {
		return state.Bootstrap.ID, nil
	}
	vm, err := p.api.GetVM(context.Background(), p.cfg.ResourceGroup, MachineName(state.Spec.Name))
	if err != nil {
		return "", errorf("finding the bootstrap VM %q: %w", MachineName(state.Spec.Name), err)
	}
	if vm == nil {
		return "", errorf("no bootstrap VM named %q in resource group %q", MachineName(state.Spec.Name), p.cfg.ResourceGroup)
	}
	return providerID(vm.ID), nil
}

// controlPlaneTemplate returns the infrastructure MachineTemplate the control
// plane object references, and the control plane object when there is one.
func controlPlaneTemplate(b *generate.Bundle) (*unstructured.Unstructured, *unstructured.Unstructured, error) {
	templates := generate.Find(b, generate.InfrastructureGroup, "MachineTemplate")
	if len(templates) == 0 {
		return nil, nil, errorf("bundle has no %s MachineTemplate", generate.InfrastructureGroup)
	}
	kcp, err := generate.FindOne(b, controlPlaneGroup, "ControlPlane")
	if err != nil {
		if len(templates) == 1 {
			return templates[0], nil, nil
		}
		return nil, nil, errorf("cannot tell which MachineTemplate the control plane uses: %v", err)
	}
	ref, err := infrastructureRefName(kcp)
	if err != nil {
		return nil, nil, err
	}
	for _, t := range templates {
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
