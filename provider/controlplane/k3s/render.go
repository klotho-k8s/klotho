package k3s

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

// controlPlane is the part of a KThreesControlPlane klotho reads. In
// cluster-api-k3s the KThreesConfigSpec is nested under spec.kthreesConfigSpec,
// so the node/bootstrap fields (files, commands, serverConfig, agentConfig) sit
// under that key beside the control-plane-only fields (version) at spec level.
type controlPlane struct {
	name         string
	version      string         // spec.version, verbatim (e.g. v1.33.4+k3s1)
	serverConfig map[string]any // spec.kthreesConfigSpec.serverConfig
	agentConfig  map[string]any // spec.kthreesConfigSpec.agentConfig
	files        []k3sFile      // spec.kthreesConfigSpec.files
	preCommands  []string       // spec.kthreesConfigSpec.preK3sCommands
	postCommands []string       // spec.kthreesConfigSpec.postK3sCommands
}

// k3sFile is one kthreesConfigSpec.files entry: a file written to node zero, as
// the k3s bootstrap provider writes them on the machines it bootstraps.
type k3sFile struct {
	path, content, permissions, owner string
}

// decodeControlPlane reads the fields klotho needs from a KThreesControlPlane
// and checks its group/kind.
func (p *Provider) decodeControlPlane(u *unstructured.Unstructured) (*controlPlane, error) {
	if u == nil {
		return nil, errors.New("k3s: the KThreesControlPlane object is required")
	}
	gvk := u.GroupVersionKind()
	if gvk.Kind != Kind || gvk.Group != ControlPlaneGroup {
		return nil, fmt.Errorf("k3s: control plane object is %s, want %s %s", gvk, ControlPlaneGroup, Kind)
	}
	cp := &controlPlane{name: u.GetName()}

	version, _, _ := unstructured.NestedString(u.Object, "spec", "version")
	cp.version = p.k3sVersion(version)

	if sc, ok, _ := unstructured.NestedMap(u.Object, "spec", "kthreesConfigSpec", "serverConfig"); ok {
		cp.serverConfig = sc
	}
	if ac, ok, _ := unstructured.NestedMap(u.Object, "spec", "kthreesConfigSpec", "agentConfig"); ok {
		cp.agentConfig = ac
	}
	if pre, ok, _ := unstructured.NestedStringSlice(u.Object, "spec", "kthreesConfigSpec", "preK3sCommands"); ok {
		cp.preCommands = pre
	}
	if post, ok, _ := unstructured.NestedStringSlice(u.Object, "spec", "kthreesConfigSpec", "postK3sCommands"); ok {
		cp.postCommands = post
	}
	if files, ok, _ := unstructured.NestedSlice(u.Object, "spec", "kthreesConfigSpec", "files"); ok {
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
				cp.files = append(cp.files, k3sFile{path: path, content: content, permissions: perm, owner: owner})
			}
		}
	}
	return cp, nil
}

// k3sVersion normalizes a version to the k3s build form: a version that already
// carries a +k3sN suffix is used as is, otherwise +k3s1 is appended. An empty
// control plane version falls back to the configured KubernetesVersion. The
// cluster templates always carry a +k3sN suffix, so this only defends the
// Options fallback.
func (p *Provider) k3sVersion(v string) string {
	if v == "" {
		v = p.opts.KubernetesVersion
	}
	if v == "" {
		return ""
	}
	if strings.Contains(v, "+k3s") {
		return v
	}
	return v + "+k3s1"
}

// Adopt implements controlplane.Provider. It renders a placeholder bootstrap
// data Secret and a Machine, both named "<cluster>-bootstrap", with the
// machine's bootstrap.dataSecretName set and NO bootstrap configRef, so once
// klotho makes the KThreesControlPlane the Machine's controller owner (converge
// patches the owner reference with the live control plane UID) and unpauses the
// Cluster, the control plane sees an owned, up-to-date member and adopts it with
// no rollout.
//
// Why no KThreesConfig / no configRef: KThreesControlPlane decides a machine
// needs a rollout by comparing the machine's KThreesConfig against its own
// KThreesConfigSpec, but short-circuits to "matches" when the machine has no
// bootstrap configRef (the path meant for externally bootstrapped machines).
// Omitting configRef therefore reduces the rollout gate to a version-equality
// check, which the Machine satisfies by carrying the control plane's exact
// version. The k3s bootstrap controller is not involved for a machine that
// already has dataSecretName, so nothing is pushed to the running node.
func (p *Provider) Adopt(_ context.Context, state *cluster.State, cp *unstructured.Unstructured) ([]byte, error) {
	if state == nil || state.Spec.Name == "" {
		return nil, errors.New("k3s: cluster state with a name is required")
	}
	spec := &state.Spec
	kcp, err := p.decodeControlPlane(cp)
	if err != nil {
		return nil, err
	}
	if kcp.version == "" {
		return nil, errors.New("k3s: a version is required (KThreesControlPlane spec.version)")
	}
	kind := InfraMachineKind(spec.Provider)
	if kind == "" {
		return nil, errors.New("k3s: an infrastructure provider is required to adopt the bootstrap machine")
	}
	ns := namespace(spec)
	name := BootstrapMachineName(spec.Name)

	// The labels KThreesControlPlane selects control plane machines by
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
				// the k3s bootstrap controller generates nothing, and the
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

// network is what the k3s cluster networking is filled from.
type network struct {
	pods, services string
}

func (p *Provider) network(clusterObj *unstructured.Unstructured) (network, error) {
	nw := network{pods: p.opts.PodCIDR, services: p.opts.ServiceCIDR}
	pods, services, err := generate.ClusterNetwork(clusterObj)
	if err != nil {
		return network{}, fmt.Errorf("k3s: %w", err)
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
