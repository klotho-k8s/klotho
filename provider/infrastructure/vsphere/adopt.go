package vsphere

import (
	"context"
	"strings"

	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"sigs.k8s.io/yaml"

	"github.com/klotho-k8s/klotho/pkg/cluster"
	"github.com/klotho-k8s/klotho/pkg/generate"
)

// Label and annotation keys the adopted machine carries. The cloned-from pair
// is what the KubeadmControlPlane controller compares with its
// spec.machineTemplate.spec.infrastructureRef to decide whether a machine is out
// of date; matching values mean adoption triggers no rollout.
const (
	labelClusterName          = "cluster.x-k8s.io/cluster-name"
	labelControlPlane         = "cluster.x-k8s.io/control-plane"
	labelControlPlaneName     = "cluster.x-k8s.io/control-plane-name"
	annotationClonedFromName  = "cluster.x-k8s.io/cloned-from-name"
	annotationClonedFromGroup = "cluster.x-k8s.io/cloned-from-groupkind"
	controlPlaneGroup         = "controlplane.cluster.x-k8s.io"
)

// Adopt renders the VSphereMachine the control plane adopts for the bootstrap
// VM. It copies the VSphereMachineTemplate the control plane references, names
// the VSphereMachine VMName(cluster) — the name the kubeadm provider gives the
// bootstrap Machine's infrastructureRef — and sets spec.providerID to the
// running VM's "vsphere://<bios-uuid>".
//
// Pre-setting the provider ID is what makes CAPV adopt rather than clone a
// duplicate: CAPV has no name-based lookup for an existing VM (its native create
// path is always clone-from-template), and it identifies its VM by the BIOS UUID
// behind the provider ID. This is the Linode (CAPL) model, the opposite of the
// GCP/Docker name-based adoption. It is NOT an officially documented "adopt
// existing VM" feature, so it MUST be confirmed against a live vCenter (see
// TESTING.md). No infrastructure cluster patch is rendered; the generated
// VSphereCluster already carries the endpoint.
func (p *Provider) Adopt(ctx context.Context, state *cluster.State, bundle []*unstructured.Unstructured) ([]byte, error) {
	if state == nil || state.Spec.Name == "" {
		return nil, errorf("state has no cluster name")
	}
	name := state.Spec.Name

	tmpl, kcp, err := controlPlaneTemplate(&generate.Bundle{Objects: bundle})
	if err != nil {
		return nil, err
	}

	pid, err := p.bootstrapProviderID(ctx, state)
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
	m.SetName(VMName(name))
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
// record from Launch when present, otherwise re-found by name so a lost status
// update does not strand the adoption.
func (p *Provider) bootstrapProviderID(ctx context.Context, state *cluster.State) (string, error) {
	if state.Bootstrap != nil && strings.HasPrefix(state.Bootstrap.ID, ProviderIDPrefix) {
		return state.Bootstrap.ID, nil
	}
	vm, err := p.client.FindVM(ctx, VMName(state.Spec.Name))
	if err != nil {
		return "", err
	}
	if vm.UUID == "" {
		return "", errorf("bootstrap VM %q has no BIOS UUID", vm.Name)
	}
	return ProviderID(vm.UUID), nil
}

// TagMachineTemplates is a no-op for vSphere, implementing klotho's optional
// MachineTemplateTagger so the orchestrator does not fall back to the default
// spec.template.spec.tags string list. The VSphereMachineTemplate CRD has no
// such field (its ownership field is tagIDs, a list of vSphere tag managed-object
// IDs), and its strict schema rejects an unknown "tags" field. vSphere records
// klotho's cluster tag in each VM's annotation when the VM is created (the
// bootstrap VM klotho launches carries Tag(cluster)), and Teardown finds
// klotho's VMs by that annotation and the name prefix — so nothing needs to be,
// or can be, written into the MachineTemplate.
func (p *Provider) TagMachineTemplates(_ []*unstructured.Unstructured, _ string) error {
	return nil
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
	return nil, nil, errorf("%s %q references MachineTemplate %q, which is not in the bundle", kcp.GetKind(), kcp.GetName(), ref)
}

// infrastructureRefName reads the machine template reference of a control plane
// object: spec.machineTemplate.spec.infrastructureRef (v1beta2) or
// spec.machineTemplate.infrastructureRef (v1beta1).
func infrastructureRefName(kcp *unstructured.Unstructured) (string, error) {
	for _, path := range [][]string{
		{"spec", "machineTemplate", "spec", "infrastructureRef", "name"},
		{"spec", "machineTemplate", "infrastructureRef", "name"},
	} {
		if v, ok, _ := unstructured.NestedString(kcp.Object, path...); ok && v != "" {
			return v, nil
		}
	}
	return "", errorf("%s %q has no machine template infrastructureRef", kcp.GetKind(), kcp.GetName())
}
