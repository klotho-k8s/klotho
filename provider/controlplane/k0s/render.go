package k0s

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"sigs.k8s.io/yaml"

	bootstrapv1 "sigs.k8s.io/cluster-api/api/bootstrap/kubeadm/v1beta2"
	clusterv1 "sigs.k8s.io/cluster-api/api/core/v1beta2"
	"sigs.k8s.io/cluster-api/util/certs"
	"sigs.k8s.io/cluster-api/util/labels/format"
	"sigs.k8s.io/cluster-api/util/secret"

	"github.com/klotho-k8s/klotho/pkg/cluster"
	"github.com/klotho-k8s/klotho/pkg/generate"
	"github.com/klotho-k8s/klotho/provider/controlplane"
)

// controlPlane is the part of a K0sControlPlane klotho reads.
type controlPlane struct {
	name          string
	version       string
	k0sConfigSpec map[string]any // spec.k0sConfigSpec, verbatim
	args          []string       // spec.k0sConfigSpec.args
	k0sConfig     map[string]any // spec.k0sConfigSpec.k0s (the ClusterConfig)
	files         []k0sFile      // spec.k0sConfigSpec.files
	preStart      []string       // spec.k0sConfigSpec.preStartCommands
	postStart     []string       // spec.k0sConfigSpec.postStartCommands
}

// k0sFile is one spec.k0sConfigSpec.files entry: a file written to node zero
// before k0s starts, as k0smotron writes them on the machines it bootstraps.
type k0sFile struct {
	path, content, permissions string
}

// decodeControlPlane reads the fields klotho needs from a K0sControlPlane and
// checks its group/kind.
func (p *Provider) decodeControlPlane(u *unstructured.Unstructured) (*controlPlane, error) {
	if u == nil {
		return nil, errors.New("k0s: the K0sControlPlane object is required")
	}
	gvk := u.GroupVersionKind()
	if gvk.Kind != Kind || gvk.Group != ControlPlaneGroup {
		return nil, fmt.Errorf("k0s: control plane object is %s, want %s %s", gvk, ControlPlaneGroup, Kind)
	}
	cp := &controlPlane{name: u.GetName()}

	version, _, _ := unstructured.NestedString(u.Object, "spec", "version")
	cp.version = p.k0sVersion(version)

	spec, found, err := unstructured.NestedMap(u.Object, "spec", "k0sConfigSpec")
	if err != nil {
		return nil, fmt.Errorf("k0s: reading spec.k0sConfigSpec: %w", err)
	}
	if found {
		cp.k0sConfigSpec = spec
	} else {
		cp.k0sConfigSpec = map[string]any{}
	}
	if args, ok, _ := unstructured.NestedStringSlice(cp.k0sConfigSpec, "args"); ok {
		cp.args = args
	}
	if k0sCfg, ok, _ := unstructured.NestedMap(cp.k0sConfigSpec, "k0s"); ok {
		cp.k0sConfig = k0sCfg
	}
	if pre, ok, _ := unstructured.NestedStringSlice(cp.k0sConfigSpec, "preStartCommands"); ok {
		cp.preStart = pre
	}
	if post, ok, _ := unstructured.NestedStringSlice(cp.k0sConfigSpec, "postStartCommands"); ok {
		cp.postStart = post
	}
	if files, ok, _ := unstructured.NestedSlice(cp.k0sConfigSpec, "files"); ok {
		for _, f := range files {
			m, ok := f.(map[string]any)
			if !ok {
				continue
			}
			path, _ := m["path"].(string)
			content, _ := m["content"].(string)
			perm, _ := m["permissions"].(string)
			if path != "" {
				cp.files = append(cp.files, k0sFile{path: path, content: content, permissions: perm})
			}
		}
	}
	return cp, nil
}

// k0sVersion normalizes a version to the k0s build form: a version that already
// carries a +k0s.N suffix is used as is, otherwise +k0s.0 is appended. An empty
// control plane version falls back to the configured KubernetesVersion.
func (p *Provider) k0sVersion(v string) string {
	if v == "" {
		v = p.opts.KubernetesVersion
	}
	if v == "" {
		return ""
	}
	if strings.Contains(v, "+k0s.") {
		return v
	}
	return v + "+k0s.0"
}

// configAnnotation is the value K0sControlPlane records on the machines it
// creates and compares against to decide on a rollout: the k0sConfigSpec as
// JSON. Adopt sets the same value so the adopted machine is seen as up to date.
func (cp *controlPlane) configAnnotation() (string, error) {
	b, err := json.Marshal(cp.k0sConfigSpec)
	if err != nil {
		return "", fmt.Errorf("k0s: marshalling k0sConfigSpec: %w", err)
	}
	return string(b), nil
}

// UserData implements controlplane.Provider. It renders node zero's cloud-init:
// the cluster CAs in the k0s PKI layout, the k0s ClusterConfig with the control
// plane endpoint injected, the install-and-start script, the cluster Secret
// manifests K0sControlPlane looks up, and the post-init script.
func (p *Provider) UserData(_ context.Context, spec *cluster.Spec, creds *controlplane.Credentials, cp, clusterObj *unstructured.Unstructured) (*controlplane.Contribution, error) {
	if spec == nil || spec.Name == "" {
		return nil, errors.New("k0s: cluster name is required")
	}
	if creds == nil {
		return nil, errors.New("k0s: credentials are required")
	}
	kcp, err := p.decodeControlPlane(cp)
	if err != nil {
		return nil, err
	}
	if kcp.version == "" {
		return nil, errors.New("k0s: a version is required (K0sControlPlane spec.version)")
	}
	ep, err := endpointFromKubeconfig(creds.Kubeconfig)
	if err != nil {
		return nil, err
	}
	nw, err := p.network(clusterObj)
	if err != nil {
		return nil, err
	}

	var c controlplane.Contribution
	addFile := func(path string, content []byte, mode string) {
		c.Files = append(c.Files, controlplane.File{Path: path, Content: content, Mode: mode, Owner: "root:root"})
	}

	// The cluster CAs, written in the k0s PKI layout so k0s uses them instead
	// of self-signing. k0smotron writes the same files from the same CAPI
	// secrets when it bootstraps a control plane node.
	certificates, err := certificatesFromCredentials(creds, PKIDir)
	if err != nil {
		return nil, err
	}
	for _, f := range certificates.AsFiles() {
		addFile(f.Path, []byte(f.Content), f.Permissions)
	}

	// The k0s ClusterConfig, with the control plane endpoint and cluster
	// networking injected.
	k0sYAML, err := p.k0sConfigYAML(kcp, ep, nw)
	if err != nil {
		return nil, err
	}
	addFile(K0sConfigPath, k0sYAML, "0640")

	// spec.k0sConfigSpec.files, written as k0smotron writes them on the
	// machines it bootstraps (a control plane node's extra files, for example
	// the script that binds the control plane VIP).
	for _, f := range kcp.files {
		mode := f.permissions
		if mode == "" {
			mode = "0644"
		}
		addFile(f.path, []byte(f.content), mode)
	}

	// The cluster Secrets K0sControlPlane looks up (<name>-ca, -etcd, -proxy,
	// -sa, -kubeconfig), applied by the post-init script.
	manifests, err := secretManifests(spec.Name, namespace(spec), certificates, creds.Kubeconfig)
	if err != nil {
		return nil, err
	}
	for _, m := range manifests {
		addFile(ManifestsDir+"/"+m.name, m.body, "0600")
	}

	addFile(InstallScript, installScript(kcp.version, BootstrapMachineName(spec.Name), kcp.args, kcp.preStart, kcp.postStart), "0700")
	addFile(PostInitScript, p.postInitScript(ep), "0700")

	c.InstallCommands = []string{runBash(InstallScript)}
	c.PostCommands = []string{runBash(PostInitScript)}
	return &c, nil
}

// k0sConfigYAML renders node zero's /etc/k0s.yaml from the control plane's
// embedded k0s ClusterConfig (or a minimal one), injecting the API external
// address and SANs so the endpoint is reachable and covered by the serving
// certificate, Kine storage for a deterministic single-node control plane, and
// the pod and service networks. The injected endpoint is not part of the
// adoption annotation, which is the raw k0sConfigSpec.
func (p *Provider) k0sConfigYAML(cp *controlPlane, ep cluster.Endpoint, nw network) ([]byte, error) {
	cfg := map[string]any{}
	for k, v := range cp.k0sConfig {
		cfg[k] = v
	}
	if cfg["apiVersion"] == nil {
		cfg["apiVersion"] = "k0s.k0sproject.io/v1beta1"
	}
	if cfg["kind"] == nil {
		cfg["kind"] = "ClusterConfig"
	}
	if _, ok := cfg["metadata"]; !ok {
		cfg["metadata"] = map[string]any{"name": "k0s"}
	}
	s, _ := cfg["spec"].(map[string]any)
	if s == nil {
		s = map[string]any{}
	}

	// api.externalAddress + sans: the control plane endpoint.
	//
	// externalAddress is skipped for container nodes (CAPD): there the endpoint
	// is a haproxy load balancer that forwards only the API server port, but
	// k0s would also point the konnectivity agent at externalAddress:8132, which
	// the load balancer does not forward, so the apiserver's egress tunnel
	// (webhooks, kubectl logs/exec) never connects. Left unset, k0s uses node
	// zero's own address for konnectivity, which works on a single node. The
	// endpoint still goes in the SANs so the serving certificate covers how
	// klotho and clients reach the API through the load balancer.
	api, _ := s["api"].(map[string]any)
	if api == nil {
		api = map[string]any{}
	}
	if !p.opts.ContainerNodes {
		api["externalAddress"] = ep.Host
	}
	sans := toStringSlice(api["sans"])
	for _, v := range []string{ep.Host, "127.0.0.1", "0.0.0.0", "localhost"} {
		if !containsStr(sans, v) {
			sans = append(sans, v)
		}
	}
	api["sans"] = sans
	s["api"] = api

	// Kine storage: single-node, and no per-machine etcd member name is
	// injected into the adoption annotation.
	storage, _ := s["storage"].(map[string]any)
	if storage == nil {
		storage = map[string]any{"type": "kine"}
	}
	s["storage"] = storage

	// Networking from the Cluster object.
	netw, _ := s["network"].(map[string]any)
	if netw == nil {
		netw = map[string]any{}
	}
	if nw.pods != "" {
		netw["podCIDR"] = nw.pods
	}
	if nw.services != "" {
		netw["serviceCIDR"] = nw.services
	}
	if len(netw) > 0 {
		s["network"] = netw
	}

	cfg["spec"] = s
	out, err := yaml.Marshal(cfg)
	if err != nil {
		return nil, fmt.Errorf("k0s: rendering k0s.yaml: %w", err)
	}
	return out, nil
}

// Adopt implements controlplane.Provider. It renders a placeholder bootstrap
// data Secret, a K0sControllerConfig and a Machine, all named
// "<cluster>-bootstrap", with the machine's bootstrap.dataSecretName set (so no
// bootstrap data is pushed to the already-running node) and its k0s-config
// annotation matching the control plane's, so K0sControlPlane adopts node zero
// without a rollout.
func (p *Provider) Adopt(_ context.Context, state *cluster.State, cp *unstructured.Unstructured) ([]byte, error) {
	if state == nil || state.Spec.Name == "" {
		return nil, errors.New("k0s: cluster state with a name is required")
	}
	spec := &state.Spec
	kcp, err := p.decodeControlPlane(cp)
	if err != nil {
		return nil, err
	}
	if kcp.version == "" {
		return nil, errors.New("k0s: a version is required (K0sControlPlane spec.version)")
	}
	kind := InfraMachineKind(spec.Provider)
	if kind == "" {
		return nil, errors.New("k0s: an infrastructure provider is required to adopt the bootstrap machine")
	}
	ns := namespace(spec)
	name := BootstrapMachineName(spec.Name)

	annotationValue, err := kcp.configAnnotation()
	if err != nil {
		return nil, err
	}

	// The labels K0sControlPlane selects control plane machines by
	// (cluster.x-k8s.io/control-plane + cluster name), plus the control plane
	// name label Cluster API sets.
	labels := map[string]string{
		clusterv1.ClusterNameLabel:             spec.Name,
		clusterv1.MachineControlPlaneLabel:     "",
		clusterv1.MachineControlPlaneNameLabel: format.MustFormatValue(cp.GetName()),
	}
	annotations := map[string]string{MachineK0sConfigAnnotation: annotationValue}

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

	config := k0sControllerConfig(name, ns, labels, annotations, kcp)

	dataSecretName := name
	machine := &clusterv1.Machine{
		TypeMeta:   metav1.TypeMeta{APIVersion: clusterv1.GroupVersion.String(), Kind: "Machine"},
		ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: ns, Labels: labels, Annotations: annotations},
		Spec: clusterv1.MachineSpec{
			ClusterName: spec.Name,
			Version:     kcp.version,
			Bootstrap: clusterv1.Bootstrap{
				ConfigRef: clusterv1.ContractVersionedObjectReference{
					APIGroup: BootstrapGroup,
					Kind:     ControllerConfigKind,
					Name:     name,
				},
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

	return marshalDocuments(dataSecret, config, machine)
}

// k0sControllerConfig renders the bootstrap config the adopted Machine
// references. Its spec is the control plane's k0sConfigSpec with the version
// inlined (K0sControllerConfigSpec is Version plus an inlined K0sConfigSpec).
// With the Machine's dataSecretName already set the k0smotron bootstrap
// controller generates nothing from it; it exists so the reference resolves.
func k0sControllerConfig(name, ns string, labels, annotations map[string]string, cp *controlPlane) *unstructured.Unstructured {
	spec := map[string]any{"version": cp.version}
	for k, v := range cp.k0sConfigSpec {
		spec[k] = v
	}
	u := &unstructured.Unstructured{Object: map[string]any{
		"apiVersion": BootstrapGroup + "/" + BootstrapVersion,
		"kind":       ControllerConfigKind,
		"metadata": map[string]any{
			"name":        name,
			"namespace":   ns,
			"labels":      toStringMap(labels),
			"annotations": toStringMap(annotations),
		},
		"spec": spec,
	}}
	return u
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

// certificatesFromCredentials rebuilds the Cluster API certificate set from the
// minted credentials, rooted at certDir so AsFiles writes the k0s PKI layout.
func certificatesFromCredentials(creds *controlplane.Credentials, certDir string) (secret.Certificates, error) {
	certificates := secret.NewCertificatesForInitialControlPlane(&bootstrapv1.ClusterConfiguration{CertificatesDir: certDir})
	for _, c := range certificates {
		var kp cluster.KeyPair
		if c.Purpose == secret.ClusterCA {
			kp = creds.CA
		} else {
			kp = creds.Extra[string(c.Purpose)]
		}
		if len(kp.Cert) == 0 || len(kp.Key) == 0 {
			return nil, fmt.Errorf("k0s: credentials are missing the %q key pair", c.Purpose)
		}
		c.KeyPair = &certs.KeyPair{Cert: kp.Cert, Key: kp.Key}
	}
	return certificates, nil
}

// network is what the k0s ClusterConfig's spec.network is filled from.
type network struct {
	pods, services string
}

func (p *Provider) network(clusterObj *unstructured.Unstructured) (network, error) {
	nw := network{pods: p.opts.PodCIDR, services: p.opts.ServiceCIDR}
	pods, services, err := generate.ClusterNetwork(clusterObj)
	if err != nil {
		return network{}, fmt.Errorf("k0s: %w", err)
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

func toStringMap(m map[string]string) map[string]any {
	out := make(map[string]any, len(m))
	for k, v := range m {
		out[k] = v
	}
	return out
}
