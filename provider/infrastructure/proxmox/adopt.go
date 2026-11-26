package proxmox

import (
	"context"
	"strings"

	gp "github.com/luthermonson/go-proxmox"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"sigs.k8s.io/yaml"

	"github.com/klotho-k8s/klotho/pkg/cluster"
	"github.com/klotho-k8s/klotho/pkg/generate"
)

// Label and annotation keys the adopted machine carries. The cloned-from
// pair is what the KubeadmControlPlane controller compares with its
// spec.machineTemplate.spec.infrastructureRef to decide whether a machine is
// out of date; matching values mean adoption triggers no rollout.
const (
	LabelClusterName          = "cluster.x-k8s.io/cluster-name"
	LabelControlPlane         = "cluster.x-k8s.io/control-plane"
	LabelControlPlaneName     = "cluster.x-k8s.io/control-plane-name"
	AnnotationClonedFromName  = "cluster.x-k8s.io/cloned-from-name"
	AnnotationClonedFromGroup = "cluster.x-k8s.io/cloned-from-groupkind"
)

const controlPlaneGroup = "controlplane.cluster.x-k8s.io"

// Adopt renders the ProxmoxMachine that adopts the bootstrap VM. It finds the
// infrastructure template the control plane references in bundle (or the
// only MachineTemplate when there is no control plane object), copies its
// spec.template, and adds virtualMachineID, providerID and sourceNode for the
// running VM. The Cluster API Proxmox provider then finds the VM by
// virtualMachineID on sourceNode, checks its name equals the machine's, and
// because it is already running skips configuration, bootstrap data and
// power-on instead of cloning a new one.
//
// The VMID is looked up by name and tag. When state records the bootstrap
// node, its provider ID must match the VM's SMBIOS UUID. No infrastructure
// cluster patch is rendered: the generated ProxmoxCluster already carries the
// endpoint.
func (p *Provider) Adopt(ctx context.Context, state *cluster.State, bundle []*unstructured.Unstructured) ([]byte, error) {
	if state == nil || state.Spec.Name == "" {
		return nil, errorf("state has no cluster name")
	}
	name := state.Spec.Name
	tmpl, kcp, err := controlPlaneTemplate(&generate.Bundle{Objects: bundle})
	if err != nil {
		return nil, err
	}

	vm, err := p.findBootstrapVM(ctx, name)
	if err != nil {
		return nil, err
	}
	cfg, err := p.readVMConfig(ctx, vm.Node, int(vm.VMID))
	if err != nil {
		return nil, err
	}
	uuid, missing, err := smbiosUUID(cfg)
	if err != nil {
		return nil, err
	}
	if missing != "" {
		return nil, errorf("VM %d has no SMBIOS UUID", vm.VMID)
	}
	providerID := ProviderIDPrefix + uuid
	if state.Bootstrap != nil && state.Bootstrap.ID != "" && !strings.EqualFold(state.Bootstrap.ID, providerID) {
		return nil, errorf("VM %d (%s) has provider ID %s, but state records %s", vm.VMID, vm.Name, providerID, state.Bootstrap.ID)
	}

	spec, found, err := unstructured.NestedMap(tmpl.Object, "spec", "template", "spec")
	if err != nil {
		return nil, errorf("reading spec.template.spec of %s %q: %v", tmpl.GetKind(), tmpl.GetName(), err)
	}
	if !found {
		spec = map[string]any{}
	}
	spec["virtualMachineID"] = int64(vm.VMID)
	spec["providerID"] = providerID
	spec["sourceNode"] = vm.Node
	// A template selected by tags must become a concrete template on the
	// adopted machine: the Proxmox provider finds an existing VM through
	// sourceNode, and its API rejects sourceNode next to a selector.
	if _, selected := spec["templateSelector"]; selected {
		id, err := p.templateIDFor(ctx, vm.Node)
		if err != nil {
			return nil, err
		}
		delete(spec, "templateSelector")
		delete(spec, "templateMatchPolicy")
		spec["templateID"] = int64(id)
	}

	labels, _, _ := unstructured.NestedStringMap(tmpl.Object, "spec", "template", "metadata", "labels")
	annotations, _, _ := unstructured.NestedStringMap(tmpl.Object, "spec", "template", "metadata", "annotations")
	if labels == nil {
		labels = map[string]string{}
	}
	if annotations == nil {
		annotations = map[string]string{}
	}
	labels[LabelClusterName] = name
	labels[LabelControlPlane] = ""
	if kcp != nil {
		labels[LabelControlPlaneName] = kcp.GetName()
	}
	gvk := tmpl.GroupVersionKind()
	annotations[AnnotationClonedFromName] = tmpl.GetName()
	annotations[AnnotationClonedFromGroup] = gvk.Kind + "." + gvk.Group

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

// controlPlaneTemplate returns the infrastructure MachineTemplate the control
// plane object references, and the control plane object when there is one.
func controlPlaneTemplate(b *generate.Bundle) (*unstructured.Unstructured, *unstructured.Unstructured, error) {
	templates := generate.Find(b, generate.InfrastructureGroup, "MachineTemplate")
	if len(templates) == 0 {
		return nil, nil, errorf("bundle has no %s MachineTemplate", generate.InfrastructureGroup)
	}
	kcp, err := generate.FindOne(b, controlPlaneGroup, "ControlPlane")
	if err != nil {
		// Without a control plane object the template is only unambiguous
		// when there is exactly one.
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

// infrastructureRefName reads the machine template reference of a control
// plane object: spec.machineTemplate.spec.infrastructureRef (v1beta2) or
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

// findBootstrapVM finds the bootstrap VM by name and tag.
func (p *Provider) findBootstrapVM(ctx context.Context, clusterName string) (*gp.ClusterResource, error) {
	vms, err := p.resources(ctx, resourceVM)
	if err != nil {
		return nil, err
	}
	name, tag := VMName(clusterName), Tag(clusterName)
	var found []*gp.ClusterResource
	for _, v := range vms {
		if v.Type == "qemu" && v.Template == 0 && v.Name == name && resHasTag(v, tag) {
			found = append(found, v)
		}
	}
	switch len(found) {
	case 1:
		return found[0], nil
	case 0:
		return nil, errorf("no VM named %s with tag %s", name, tag)
	default:
		return nil, errorf("%d VMs named %s carry tag %s", len(found), name, tag)
	}
}

// templateIDFor resolves the template the provider's tags select, as Launch
// did when it cloned the bootstrap VM: a template on node first, else the one
// the tags select cluster-wide (the bootstrap was then a cross-node clone
// from shared storage).
func (p *Provider) templateIDFor(ctx context.Context, node string) (int, error) {
	if len(p.cfg.TemplateTags) == 0 {
		if p.cfg.TemplateID > 0 {
			return p.cfg.TemplateID, nil
		}
		return 0, errorf("adopt: machine template selects by tags but the provider has no template tags configured")
	}
	vms, err := p.findTemplates(ctx)
	if err != nil {
		return 0, errorf("adopt: listing templates: %v", err)
	}
	t, err := selectTemplate(node, vms, p.cfg.TemplateTags, p.cfg.TemplateMatchPolicy)
	if err != nil {
		var cwErr error
		if t, cwErr = selectTemplate("", vms, p.cfg.TemplateTags, p.cfg.TemplateMatchPolicy); cwErr != nil {
			return 0, err
		}
	}
	return int(t.VMID), nil
}
