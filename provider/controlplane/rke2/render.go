package rke2

import (
	"context"
	"errors"
	"fmt"
	"strings"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"

	bootstrapv1 "sigs.k8s.io/cluster-api/api/bootstrap/kubeadm/v1beta2"
	clusterv1 "sigs.k8s.io/cluster-api/api/core/v1beta2"
	"sigs.k8s.io/cluster-api/util/labels/format"

	"github.com/klotho-k8s/klotho/pkg/cluster"
	"github.com/klotho-k8s/klotho/pkg/generate"
)

// controlPlane is the part of an RKE2ControlPlane klotho reads. In v1beta2 the
// RKE2ConfigSpec is embedded inline at the top of spec, so the node/bootstrap
// fields (files, commands, agentConfig) sit beside the control-plane-only
// fields (version, serverConfig).
type controlPlane struct {
	name         string
	version      string         // spec.version, verbatim (e.g. v1.33.4+rke2r1)
	serverConfig map[string]any // spec.serverConfig
	agentConfig  map[string]any // spec.agentConfig
	files        []rke2File     // spec.files
	preCommands  []string       // spec.preRKE2Commands
	postCommands []string       // spec.postRKE2Commands
}

// rke2File is one spec.files entry: a file written to node zero, as the RKE2
// bootstrap provider writes them on the machines it bootstraps.
type rke2File struct {
	path, content, permissions, owner string
}

// decodeControlPlane reads the fields klotho needs from an RKE2ControlPlane and
// checks its group/kind.
func (p *Provider) decodeControlPlane(u *unstructured.Unstructured) (*controlPlane, error) {
	if u == nil {
		return nil, errors.New("rke2: the RKE2ControlPlane object is required")
	}
	gvk := u.GroupVersionKind()
	if gvk.Kind != Kind || gvk.Group != ControlPlaneGroup {
		return nil, fmt.Errorf("rke2: control plane object is %s, want %s %s", gvk, ControlPlaneGroup, Kind)
	}
	cp := &controlPlane{name: u.GetName()}

	version, _, _ := unstructured.NestedString(u.Object, "spec", "version")
	cp.version = p.rke2Version(version)

	if sc, ok, _ := unstructured.NestedMap(u.Object, "spec", "serverConfig"); ok {
		cp.serverConfig = sc
	}
	if ac, ok, _ := unstructured.NestedMap(u.Object, "spec", "agentConfig"); ok {
		cp.agentConfig = ac
	}
	if pre, ok, _ := unstructured.NestedStringSlice(u.Object, "spec", "preRKE2Commands"); ok {
		cp.preCommands = pre
	}
	if post, ok, _ := unstructured.NestedStringSlice(u.Object, "spec", "postRKE2Commands"); ok {
		cp.postCommands = post
	}
	if files, ok, _ := unstructured.NestedSlice(u.Object, "spec", "files"); ok {
		for _, f := range files {
			m, ok := f.(map[string]any)
			if !ok {
				continue
			}
			path, _ := m["path"].(string)
			content, _ := m["content"].(string)
			perm, _ := m["permissions"].(string)
			owner, _ := m["owner"].(string)
			if path != "" {
				cp.files = append(cp.files, rke2File{path: path, content: content, permissions: perm, owner: owner})
			}
		}
	}
	return cp, nil
}

// rke2Version normalizes a version to the rke2 build form: a version that
// already carries a +rke2rN suffix is used as is, otherwise +rke2r1 is
// appended. An empty control plane version falls back to the configured
// KubernetesVersion. The RKE2ControlPlane CRD requires the +rke2rN suffix, so
// the templates always carry one and this only defends the Options fallback.
func (p *Provider) rke2Version(v string) string {
	if v == "" {
		v = p.opts.KubernetesVersion
	}
	if v == "" {
		return ""
	}
	if strings.Contains(v, "+rke2r") {
		return v
	}
	return v + "+rke2r1"
}

// Adopt implements controlplane.Provider. It renders a placeholder bootstrap
// data Secret and a Machine, both named "<cluster>-bootstrap", with the
// machine's bootstrap.dataSecretName set and NO bootstrap configRef, so once
// klotho makes the RKE2ControlPlane the Machine's controller owner (converge
// patches the owner reference with the live control plane UID) and unpauses the
// Cluster, the control plane sees an owned, up-to-date member and adopts it
// with no rollout.
//
// Why no RKE2Config / no configRef: RKE2ControlPlane decides a machine needs a
// rollout by reflect.DeepEqual of the machine's RKE2Config against its own
// RKE2ConfigSpec, but short-circuits to "matches" when the machine has no
// bootstrap configRef (the path meant for externally bootstrapped machines).
// Omitting configRef therefore reduces the rollout gate to a version-equality
// check, which the Machine satisfies by carrying the control plane's exact
// version. The RKE2 bootstrap controller is not involved for a machine that
// already has dataSecretName, so nothing is pushed to the running node.
func (p *Provider) Adopt(_ context.Context, state *cluster.State, cp *unstructured.Unstructured) ([]byte, error) {
	if state == nil || state.Spec.Name == "" {
		return nil, errors.New("rke2: cluster state with a name is required")
	}
	spec := &state.Spec
	kcp, err := p.decodeControlPlane(cp)
	if err != nil {
		return nil, err
	}
	if kcp.version == "" {
		return nil, errors.New("rke2: a version is required (RKE2ControlPlane spec.version)")
	}
	kind := InfraMachineKind(spec.Provider)
	if kind == "" {
		return nil, errors.New("rke2: an infrastructure provider is required to adopt the bootstrap machine")
	}
	ns := namespace(spec)
	name := BootstrapMachineName(spec.Name)

	// The labels RKE2ControlPlane selects control plane machines by
	// (cluster.x-k8s.io/control-plane + cluster name), plus the control plane
	// name label Cluster API sets.
	labels := map[string]string{
		clusterv1.ClusterNameLabel:             spec.Name,
		clusterv1.MachineControlPlaneLabel:     "",
		clusterv1.MachineControlPlaneNameLabel: format.MustFormatValue(cp.GetName()),
	}

	dataSecretName := name
	dataSecret := &corev1.Secret{
		TypeMeta: metav1.TypeMeta{APIVersion: "v1", Kind: "Secret"},
		ObjectMeta: metav1.ObjectMeta{
			Name: name, Namespace: ns,
			Labels: map[string]string{clusterv1.ClusterNameLabel: spec.Name},
		},
		Type: clusterv1.ClusterSecretType,
		Data: map[string][]byte{
			"value":  []byte("#cloud-config\n"),
			"format": []byte(bootstrapv1.CloudConfig),
		},
	}

	machine := &clusterv1.Machine{
		TypeMeta:   metav1.TypeMeta{APIVersion: clusterv1.GroupVersion.String(), Kind: "Machine"},
		ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: ns, Labels: labels},
		Spec: clusterv1.MachineSpec{
			ClusterName: spec.Name,
			Version:     kcp.version,
			Bootstrap: clusterv1.Bootstrap{
				// No ConfigRef: the Machine carries a populated data secret, so
				// the RKE2 bootstrap controller generates nothing, and the
				// rollout gate short-circuits to a version-only comparison.
				DataSecretName: &dataSecretName,
			},
			InfrastructureRef: clusterv1.ContractVersionedObjectReference{
				APIGroup: "infrastructure.cluster.x-k8s.io",
				Kind:     kind,
				Name:     name,
			},
		},
	}
	if state.Bootstrap != nil {
		machine.Spec.ProviderID = providerID(spec.Provider, state.Bootstrap.ID)
	}

	return marshalDocuments(dataSecret, machine)
}

// providerID returns the provider ID for a bootstrap node ID: the ID itself
// when it already carries a scheme, the provider's scheme plus the ID for
// proxmox, or "" so the Machine takes it from the infrastructure machine.
func providerID(provider, id string) string {
	if id == "" {
		return ""
	}
	if strings.Contains(id, "://") {
		return id
	}
	if strings.EqualFold(provider, "proxmox") {
		return "proxmox://" + strings.ToLower(id)
	}
	return ""
}

// network is what the rke2 cluster networking is filled from.
type network struct {
	pods, services string
}

func (p *Provider) network(clusterObj *unstructured.Unstructured) (network, error) {
	nw := network{pods: p.opts.PodCIDR, services: p.opts.ServiceCIDR}
	pods, services, err := generate.ClusterNetwork(clusterObj)
	if err != nil {
		return network{}, fmt.Errorf("rke2: %w", err)
	}
	if len(pods) > 0 {
		nw.pods = strings.Join(pods, ",")
	}
	if len(services) > 0 {
		nw.services = strings.Join(services, ",")
	}
	return nw, nil
}

func toStringSlice(v any) []string {
	switch s := v.(type) {
	case []string:
		return append([]string(nil), s...)
	case []any:
		out := make([]string, 0, len(s))
		for _, e := range s {
			if str, ok := e.(string); ok {
				out = append(out, str)
			}
		}
		return out
	}
	return nil
}

func containsStr(s []string, v string) bool {
	for _, e := range s {
		if e == v {
			return true
		}
	}
	return false
}
