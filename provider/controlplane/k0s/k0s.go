// Package k0s is the control plane provider for k0s-based clusters managed by
// the k0smotron Cluster API control plane provider (K0sControlPlane, the
// "k0s on the machine" standalone mode, not the hosted control planes).
//
// It mints the cluster's certificate authorities and an admin kubeconfig
// before any node exists, turns the bootstrap node into a single-node k0s
// controller with those CAs pre-seeded, installs the Cluster API operator
// stack (k0smotron bootstrap and control plane providers included) on it,
// seeds the Secrets K0sControlPlane expects to find, and renders the Machine
// and K0sControllerConfig that let K0sControlPlane adopt the running node in
// place without a rollout.
//
// # Adoption model
//
// K0sControlPlane has no automatic adoption of an orphaned Machine the way
// KubeadmControlPlane does. Instead it selects control plane machines by label
// (cluster.x-k8s.io/control-plane + cluster name) and decides whether each is
// up to date by comparing the desired k0s config against the value recorded in
// the machine's k0s-config annotation. klotho exploits exactly that: Adopt
// renders a Machine labelled as a control plane member whose
// spec.bootstrap.dataSecretName is set (so no bootstrap data is generated or
// pushed to the already-running node) and whose k0s-config annotation matches
// the K0sControlPlane's k0sConfigSpec, so once klotho makes the K0sControlPlane
// the Machine's controller owner and unpauses the Cluster, the controller sees
// an existing, up-to-date member and leaves it alone. This is the automated
// equivalent of k0smotron's documented manual "adopt an existing cluster into
// CAPI" procedure (paused objects, dataSecretName set, PKI secrets imported).
//
// # Source of configuration
//
// The node is configured from the K0sControlPlane in the generated cluster
// bundle, not from settings of this package. Options only fill gaps the
// control plane object cannot express (CNI, cert-manager and operator URLs,
// the fallback Kubernetes version, extra SSH keys and post-init commands).
package k0s

import (
	"context"
	"errors"
	"fmt"
	"net"
	"strconv"
	"strings"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/client-go/tools/clientcmd"
	"sigs.k8s.io/yaml"

	clusterv1 "sigs.k8s.io/cluster-api/api/core/v1beta2"
	"sigs.k8s.io/cluster-api/util/certs"
	"sigs.k8s.io/cluster-api/util/kubeconfig"
	"sigs.k8s.io/cluster-api/util/secret"

	"github.com/klotho-k8s/klotho/pkg/cluster"
	"github.com/klotho-k8s/klotho/pkg/generate"
	"github.com/klotho-k8s/klotho/provider/controlplane"
)

// Name is the provider's identifier, as used by --control-plane and in state.
const Name = "k0s"

// Kind is the control plane object kind this provider handles.
const Kind = "K0sControlPlane"

// K0smotron Cluster API operator provider identity. k0smotron ships its
// bootstrap and control plane providers under one operator provider name and
// namespace.
const (
	// OperatorProviderName is the cluster-api-operator provider name for both
	// the k0smotron bootstrap and control plane providers.
	OperatorProviderName = "k0sproject-k0smotron"
	// OperatorNamespace is where the k0smotron providers run.
	OperatorNamespace = "k0smotron"
	// DefaultK0smotronVersion is the k0smotron release whose provider
	// components are installed when none is configured.
	DefaultK0smotronVersion = "v2.3.1"
)

// Cluster API group/version of the k0smotron CRDs.
const (
	ControlPlaneGroup   = "controlplane.cluster.x-k8s.io"
	ControlPlaneVersion = "v1beta1"
	BootstrapGroup      = "bootstrap.cluster.x-k8s.io"
	BootstrapVersion    = "v1beta1"

	// ControllerConfigKind is the bootstrap config kind K0sControlPlane
	// machines reference.
	ControllerConfigKind = "K0sControllerConfig"
)

// MachineK0sConfigAnnotation holds the JSON-serialized k0sConfigSpec a control
// plane machine was created with (cpv1beta2.MachineK0sConfigAnnotation in
// k0smotron). K0sControlPlane's isBootstrapConfigUpToDate unmarshals it,
// normalizes both it and the desired config through the v1beta2 defaults, and
// compares them; a match means no rollout, which is what adoption requires.
// With Kine storage no per-machine etcd member name is injected, so the value
// is simply the K0sControlPlane's k0sConfigSpec marshalled to JSON.
const MachineK0sConfigAnnotation = "k0s.controlplane.cluster.x-k8s.io/k0s-config"

// Pinned defaults, shared with the kubeadm provider's choices so a cluster can
// switch its control plane without changing the rest of the stack.
const (
	// DefaultCertManagerURL is the cert-manager release the Cluster API
	// operator's webhooks need.
	DefaultCertManagerURL = "https://github.com/cert-manager/cert-manager/releases/download/v1.21.3/cert-manager.yaml"
	// DefaultOperatorURL is the Cluster API operator release manifest.
	DefaultOperatorURL = "https://github.com/kubernetes-sigs/cluster-api-operator/releases/download/v0.30.1/operator-components.yaml"
	// DefaultCNIManifestURL is the Calico manifest install.
	DefaultCNIManifestURL = "https://raw.githubusercontent.com/projectcalico/calico/v3.33.1/manifests/calico.yaml"
	// DefaultPodCIDR is the pod network the cluster templates declare.
	DefaultPodCIDR = "192.168.0.0/16"
	// DefaultPort is the API server port when the endpoint does not set one.
	DefaultPort = 6443
)

// Options are this package's additions to what the K0sControlPlane specifies.
type Options struct {
	// KubernetesVersion is used only when the K0sControlPlane has no
	// spec.version. It may be a plain Kubernetes version (v1.33.13), which is
	// suffixed with the default k0s build, or a full k0s version
	// (v1.33.13+k0s.0).
	KubernetesVersion string
	// PodCIDR and ServiceCIDR stand in for the Cluster's clusterNetwork.
	PodCIDR, ServiceCIDR string
	CertManagerURL       string
	OperatorURL          string
	CNIManifestURL       string
	// SSHAuthorizedKeys are appended to root's authorized_keys.
	SSHAuthorizedKeys []string
	// ExtraPostCommands run at the end of the post-init script with KUBECONFIG
	// pointing at the admin kubeconfig.
	ExtraPostCommands []string
	// ContainerNodes tells the provider the control plane runs inside
	// containers (the Docker/CAPD provider), so it configures k0s for a
	// rootless, user-namespaced kindest/node container.
	ContainerNodes bool
}

// Provider is the k0s control plane provider.
type Provider struct {
	opts Options
}

var _ controlplane.Provider = (*Provider)(nil)

// New returns a provider with defaults filled in.
func New(opts Options) *Provider {
	if opts.CertManagerURL == "" {
		opts.CertManagerURL = DefaultCertManagerURL
	}
	if opts.OperatorURL == "" {
		opts.OperatorURL = DefaultOperatorURL
	}
	if opts.CNIManifestURL == "" {
		opts.CNIManifestURL = DefaultCNIManifestURL
	}
	if opts.PodCIDR == "" {
		opts.PodCIDR = DefaultPodCIDR
	}
	return &Provider{opts: opts}
}

// Name implements controlplane.Provider.
func (p *Provider) Name() string { return Name }

// Kind implements controlplane.Provider.
func (p *Provider) Kind() string { return Kind }

// InfraMachineKind maps an infrastructure provider name to the kind of its
// Cluster API infrastructure machine.
func InfraMachineKind(provider string) string {
	switch strings.ToLower(provider) {
	case "":
		return ""
	case "proxmox":
		return "ProxmoxMachine"
	case "docker":
		return "DockerMachine"
	case "vsphere":
		return "VSphereMachine"
	case "aws":
		return "AWSMachine"
	case "azure":
		return "AzureMachine"
	case "gcp":
		return "GCPMachine"
	}
	return strings.ToUpper(provider[:1]) + provider[1:] + "Machine"
}

// BootstrapMachineName is the name shared by the bootstrap node's Machine,
// K0sControllerConfig, bootstrap data Secret and infrastructure machine.
func BootstrapMachineName(clusterName string) string { return clusterName + "-bootstrap" }

// CAPIProviders returns the Cluster API operator bootstrap and control-plane
// provider installs for k0smotron, for CAPIOptions.Bootstrap and .ControlPlane.
// Each argument overrides the corresponding default when non-empty: the
// k0smotron release version, the provider namespace, and the two components
// manifest URLs (otherwise derived from the release assets for the version).
// The bootstrap and control-plane providers share the one k0smotron provider
// name and namespace; only the components file differs.
func CAPIProviders(version, namespace, bootstrapURL, controlPlaneURL string) (bootstrap, controlPlane generate.ProviderInstall) {
	if version == "" {
		version = DefaultK0smotronVersion
	}
	if namespace == "" {
		namespace = OperatorNamespace
	}
	base := "https://github.com/k0sproject/k0smotron/releases/download/" + version + "/"
	if bootstrapURL == "" {
		bootstrapURL = base + "bootstrap-components.yaml"
	}
	if controlPlaneURL == "" {
		controlPlaneURL = base + "control-plane-components.yaml"
	}
	bootstrap = generate.ProviderInstall{Name: OperatorProviderName, Namespace: namespace, Version: version, ComponentsURL: bootstrapURL}
	controlPlane = generate.ProviderInstall{Name: OperatorProviderName, Namespace: namespace, Version: version, ComponentsURL: controlPlaneURL}
	return bootstrap, controlPlane
}

// Prepare implements controlplane.Provider. It generates the cluster CA, etcd
// CA, front proxy CA and service account key pair, and an admin kubeconfig for
// the endpoint signed by the cluster CA. These are the standard Cluster API
// control-plane PKI secrets; k0smotron reads them and k0s is started with them
// pre-seeded, so the running cluster's CAs match what K0sControlPlane expects.
func (p *Provider) Prepare(_ context.Context, spec *cluster.Spec, ep cluster.Endpoint) (*controlplane.Credentials, error) {
	if spec == nil || spec.Name == "" {
		return nil, errors.New("k0s: cluster name is required")
	}
	if ep.Host == "" {
		return nil, errors.New("k0s: endpoint host is required")
	}
	certificates := secret.NewCertificatesForInitialControlPlane(nil)
	if err := certificates.Generate(); err != nil {
		return nil, fmt.Errorf("k0s: generating certificates: %w", err)
	}

	creds := &controlplane.Credentials{Extra: map[string]cluster.KeyPair{}}
	for _, c := range certificates {
		kp := cluster.KeyPair{Cert: c.KeyPair.Cert, Key: c.KeyPair.Key}
		if c.Purpose == secret.ClusterCA {
			creds.CA = kp
			continue
		}
		creds.Extra[string(c.Purpose)] = kp
	}

	caCert, err := certs.DecodeCertPEM(creds.CA.Cert)
	if err != nil {
		return nil, fmt.Errorf("k0s: decoding cluster CA: %w", err)
	}
	caKey, err := certs.DecodePrivateKeyPEM(creds.CA.Key)
	if err != nil {
		return nil, fmt.Errorf("k0s: decoding cluster CA key: %w", err)
	}
	cfg, err := kubeconfig.New(spec.Name, serverURL(ep), caCert, caKey)
	if err != nil {
		return nil, fmt.Errorf("k0s: building admin kubeconfig: %w", err)
	}
	creds.Kubeconfig, err = clientcmd.Write(*cfg)
	if err != nil {
		return nil, fmt.Errorf("k0s: serializing admin kubeconfig: %w", err)
	}
	return creds, nil
}

type manifest struct {
	name string
	body []byte
}

// secretManifests renders the namespace, the four certificate Secrets and the
// kubeconfig Secret that K0sControlPlane looks up for the cluster. They carry
// no owner references; the control plane provider adopts the unowned
// cluster.x-k8s.io/secret Secrets when it reconciles them.
func secretManifests(clusterName, ns string, certificates secret.Certificates, kubeconfigData []byte) ([]manifest, error) {
	nsObj := &corev1.Namespace{
		TypeMeta:   metav1.TypeMeta{APIVersion: "v1", Kind: "Namespace"},
		ObjectMeta: metav1.ObjectMeta{Name: ns},
	}
	body, err := marshalDocuments(nsObj)
	if err != nil {
		return nil, err
	}
	out := []manifest{{name: "00-namespace.yaml", body: body}}

	key := types.NamespacedName{Namespace: ns, Name: clusterName}
	for _, c := range certificates {
		s := c.AsSecret(key, metav1.OwnerReference{})
		s.OwnerReferences = nil
		s.TypeMeta = metav1.TypeMeta{APIVersion: "v1", Kind: "Secret"}
		body, err := marshalDocuments(s)
		if err != nil {
			return nil, err
		}
		out = append(out, manifest{name: s.Name + ".yaml", body: body})
	}

	kc := &corev1.Secret{
		TypeMeta: metav1.TypeMeta{APIVersion: "v1", Kind: "Secret"},
		ObjectMeta: metav1.ObjectMeta{
			Name:      secret.Name(clusterName, secret.Kubeconfig),
			Namespace: ns,
			Labels:    map[string]string{clusterv1.ClusterNameLabel: clusterName},
		},
		Type: clusterv1.ClusterSecretType,
		Data: map[string][]byte{secret.KubeconfigDataName: kubeconfigData},
	}
	body, err = marshalDocuments(kc)
	if err != nil {
		return nil, err
	}
	out = append(out, manifest{name: kc.Name + ".yaml", body: body})
	return out, nil
}

func marshalDocuments(objs ...any) ([]byte, error) {
	var b strings.Builder
	for _, o := range objs {
		y, err := yaml.Marshal(o)
		if err != nil {
			return nil, fmt.Errorf("k0s: marshalling manifest: %w", err)
		}
		b.WriteString("---\n")
		b.Write(y)
	}
	return []byte(b.String()), nil
}

// endpointFromKubeconfig recovers the endpoint Prepare baked into the admin
// kubeconfig, so UserData renders the same address.
func endpointFromKubeconfig(data []byte) (cluster.Endpoint, error) {
	cfg, err := clientcmd.Load(data)
	if err != nil {
		return cluster.Endpoint{}, fmt.Errorf("k0s: parsing admin kubeconfig: %w", err)
	}
	for _, c := range cfg.Clusters {
		rest, ok := strings.CutPrefix(c.Server, "https://")
		if !ok {
			continue
		}
		host, port, err := net.SplitHostPort(strings.TrimSuffix(rest, "/"))
		if err != nil {
			return cluster.Endpoint{}, fmt.Errorf("k0s: kubeconfig server %q: %w", c.Server, err)
		}
		n, err := strconv.Atoi(port)
		if err != nil {
			return cluster.Endpoint{}, fmt.Errorf("k0s: kubeconfig server %q: %w", c.Server, err)
		}
		return cluster.Endpoint{Host: host, Port: n}, nil
	}
	return cluster.Endpoint{}, errors.New("k0s: admin kubeconfig has no https server")
}

func serverURL(ep cluster.Endpoint) string {
	port := ep.Port
	if port == 0 {
		port = DefaultPort
	}
	return "https://" + net.JoinHostPort(ep.Host, strconv.Itoa(port))
}

func namespace(spec *cluster.Spec) string {
	if spec.Namespace == "" {
		return "default"
	}
	return spec.Namespace
}
