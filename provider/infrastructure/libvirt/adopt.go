package libvirt

import (
	"context"
	"errors"
	"strings"

	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"sigs.k8s.io/yaml"

	"github.com/klotho-k8s/klotho/pkg/cluster"
	"github.com/klotho-k8s/klotho/pkg/generate"
)

// Label and annotation keys the adopted machine carries. The cloned-from pair
// is what the KubeadmControlPlane controller compares with its machine
// template's infrastructureRef to decide whether a machine is up to date;
// matching values mean adoption triggers no rollout.
const (
	LabelClusterName          = "cluster.x-k8s.io/cluster-name"
	LabelControlPlane         = "cluster.x-k8s.io/control-plane"
	LabelControlPlaneName     = "cluster.x-k8s.io/control-plane-name"
	AnnotationClonedFromName  = "cluster.x-k8s.io/cloned-from-name"
	AnnotationClonedFromGroup = "cluster.x-k8s.io/cloned-from-groupkind"
)

const controlPlaneGroup = "controlplane.cluster.x-k8s.io"

// Adopt renders the LibvirtMachine that adopts the bootstrap domain. It copies
// the control plane's infrastructure MachineTemplate spec and sets
// spec.domainName to the domain Launch created, so the Cluster API libvirt
// provider looks that existing domain up by name and adopts it instead of
// defining a new one. It does NOT pre-set spec.providerID: the provider sets it
// itself once the domain exists ("libvirt://<host>/<uuid>"), and klotho seeds
// the NoCloud meta-data instance-id with the same UUID so the kubelet
// provider-id agrees. The machine is named DomainName(cluster).
func (p *Provider) Adopt(ctx context.Context, state *cluster.State, bundle []*unstructured.Unstructured) ([]byte, error) {
	if state == nil || state.Spec.Name == "" {
		return nil, errorf("state has no cluster name")
	}
	name := state.Spec.Name
	domName := DomainName(name)

	tmpl, kcp, err := controlPlaneTemplate(&generate.Bundle{Objects: bundle})
	if err != nil {
		return nil, err
	}

	// The domain must still exist so the provider adopts rather than creates it.
	dom, err := p.client.GetDomain(ctx, domName)
	if err != nil {
		if errors.Is(err, ErrDomainNotFound) {
			return nil, errorf("bootstrap domain %q not found for adoption", domName)
		}
		return nil, err
	}
	providerID := ProviderID(p.idHost(), dom.UUID)
	if state.Bootstrap != nil && state.Bootstrap.ID != "" && !strings.EqualFold(state.Bootstrap.ID, providerID) {
		return nil, errorf("bootstrap domain %q has provider ID %s, but state records %s", domName, providerID, state.Bootstrap.ID)
	}

	spec, found, err := unstructured.NestedMap(tmpl.Object, "spec", "template", "spec")
	if err != nil {
		return nil, errorf("reading spec.template.spec of %s %q: %v", tmpl.GetKind(), tmpl.GetName(), err)
	}
	if !found {
		spec = map[string]any{}
	}
	spec["domainName"] = domName

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
	m.SetName(domName)
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
// plane references, and the control plane object when there is one.
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
// object, supporting the v1beta2 and v1beta1 paths.
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
