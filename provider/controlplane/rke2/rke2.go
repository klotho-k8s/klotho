// Package rke2 is the control plane provider for RKE2-based clusters managed by
// the Cluster API RKE2 control plane provider (RKE2ControlPlane, from
// rancher/cluster-api-provider-rke2).
//
// It mints the cluster's certificate authorities and an admin kubeconfig
// before any node exists, turns the bootstrap node into a single-node rke2
// server with those CAs pre-seeded, installs the Cluster API operator stack
// (the RKE2 bootstrap and control plane providers included) on it, seeds the
// Secrets RKE2ControlPlane expects to find, and renders the Machine and
// RKE2Config that let RKE2ControlPlane adopt the running node in place without
// a rollout.
//
// # Adoption model
//
// RKE2ControlPlane is derived from KubeadmControlPlane and inherits its
// machine-adoption machinery: on reconcile it adopts pre-existing control
// plane Machines that match its selector but carry no controller owner
// reference, and it decides whether each machine needs a rollout by comparing
// the machine's RKE2Config against the desired RKE2ConfigSpec through a
// template hash recorded in an annotation. klotho exploits exactly that: Adopt
// renders a Machine labelled as a control plane member whose
// spec.bootstrap.dataSecretName is set (so the RKE2 bootstrap controller
// generates no bootstrap data and nothing is pushed to the already-running
// node) and an RKE2Config whose spec matches the RKE2ControlPlane's, so once
// the control plane adopts the Machine and the Cluster is unpaused it sees an
// existing, up-to-date member and leaves it alone.
//
// # Source of configuration
//
// The node is configured from the RKE2ControlPlane in the generated cluster
// bundle, not from settings of this package. Options only fill gaps the
// control plane object cannot express (cert-manager and operator URLs, the
// fallback Kubernetes version, extra SSH keys and post-init commands).
package rke2

import (
	"context"
	"errors"
	"fmt"
	"net"
	"strconv"
	"strings"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
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
const Name = "rke2"

// Kind is the control plane object kind this provider handles.
const Kind = "RKE2ControlPlane"

// RKE2 Cluster API operator provider identity. The bootstrap and control-plane
// providers of rancher/cluster-api-provider-rke2 are installed as two separate
// operator providers that share the components release.
const (
	// BootstrapProviderName is the cluster-api-operator provider name for the
	// RKE2 bootstrap provider.
	BootstrapProviderName = "rke2"
	// ControlPlaneProviderName is the cluster-api-operator provider name for
	// the RKE2 control plane provider.
	ControlPlaneProviderName = "rke2"
	// BootstrapNamespace is where the RKE2 bootstrap provider runs.
	BootstrapNamespace = "rke2-bootstrap-system"
	// ControlPlaneNamespace is where the RKE2 control plane provider runs.
	ControlPlaneNamespace = "rke2-control-plane-system"
	// DefaultProviderVersion is the cluster-api-provider-rke2 release whose
	// components are installed when none is configured.
	DefaultProviderVersion = "v0.27.3"
)

// Cluster API group/version of the RKE2 provider CRDs. v0.27.x serves both
// v1beta1 and v1beta2; v1beta2 is the storage version.
const (
	ControlPlaneGroup   = "controlplane.cluster.x-k8s.io"
	ControlPlaneVersion = "v1beta2"
	BootstrapGroup      = "bootstrap.cluster.x-k8s.io"
	BootstrapVersion    = "v1beta2"

	// ConfigKind is the bootstrap config kind RKE2ControlPlane machines
	// reference. klotho's adopted Machine omits the bootstrap configRef (it
	// sets dataSecretName directly), so no RKE2Config object is rendered; the
	// kind is kept for templates and callers.
	ConfigKind = "RKE2Config"
)

// Pinned defaults, shared with the kubeadm and k0s providers' choices so a
// cluster can switch its control plane without changing the rest of the stack.
const (
	// DefaultCertManagerURL is the cert-manager release the Cluster API
	// operator's webhooks (and the RKE2 provider's) need.
	DefaultCertManagerURL = "https://github.com/cert-manager/cert-manager/releases/download/v1.21.3/cert-manager.yaml"
	// DefaultOperatorURL is the Cluster API operator release manifest.
	DefaultOperatorURL = "https://github.com/kubernetes-sigs/cluster-api-operator/releases/download/v0.30.1/operator-components.yaml"
	// DefaultCNIManifestURL is the Calico manifest install. RKE2 ships its own
	// CNI by default (canal), so this is used only when the control plane
	// disables the embedded CNI.
	DefaultCNIManifestURL = "https://raw.githubusercontent.com/projectcalico/calico/v3.33.1/manifests/calico.yaml"
	// DefaultPodCIDR is the pod network the cluster templates declare.
	DefaultPodCIDR = "192.168.0.0/16"
	// DefaultServiceCIDR is the service network the cluster templates declare.
	DefaultServiceCIDR = "10.43.0.0/16"
	// DefaultPort is the API server port when the endpoint does not set one.
	DefaultPort = 6443
)

// Paths on the bootstrap node.
const (
	// TLSDir is rke2's server PKI directory. CA files pre-seeded here are used
	// instead of self-signed ones (RKE2's DefaultCertificatesDir).
	TLSDir = "/var/lib/rancher/rke2/server/tls"
	// ConfigPath is where rke2 reads its server configuration.
	ConfigPath = "/etc/rancher/rke2/config.yaml"
	// RKE2Kubeconfig is the admin kubeconfig rke2 writes once the server is up.
	RKE2Kubeconfig = "/etc/rancher/rke2/rke2.yaml"
	// KubectlBin is the kubectl rke2 installs.
	KubectlBin = "/var/lib/rancher/rke2/bin/kubectl"

	ManifestsDir   = "/run/klotho/manifests"
	InstallScript  = "/run/klotho/rke2-install.sh"
	PostInitScript = "/run/klotho/post-init.sh"
)

// The RKE2 control-plane CA set (rancher/cluster-api-provider-rke2 pkg/secret,
// NewCertificatesForInitialControlPlane): four self-signed CAs the control
// plane provider looks up by "<cluster><suffix>" and reuses when present. The
// server and client CAs are seeded as one cluster CA so klotho's own admin
// credential (minted from state.CA) is accepted by the API server and verifies
// its serving certificate; the etcd peer and server CAs are distinct so the
// control plane's etcd client matches node zero's etcd.
const (
	caSuffix       = "-ca"        // server-ca on the node
	ccaSuffix      = "-cca"       // client-ca on the node
	peerEtcdSuffix = "-peer-etcd" // etcd/peer-ca on the node
	etcdSuffix     = "-etcd"      // etcd/server-ca on the node
	tokenSuffix    = "-token"     // the rke2 cluster join token

	// Credential keys for the etcd CAs in Credentials.Extra. The server and
	// client CAs are Credentials.CA.
	extraPeerEtcd = "peer-etcd"
	extraEtcd     = "etcd"

	tlsCrtKey = "tls.crt"
	tlsKeyKey = "tls.key"
)

// Options are this package's additions to what the RKE2ControlPlane specifies.
type Options struct {
	// KubernetesVersion is used only when the RKE2ControlPlane has no
	// spec.version. It may be a plain Kubernetes version (v1.33.4), which is
	// suffixed with the default rke2 build, or a full rke2 version
	// (v1.33.4+rke2r1).
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
	// containers (the Docker/CAPD provider), so it configures rke2 for a
	// kindest/node container.
	ContainerNodes bool
}

// Provider is the rke2 control plane provider.
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
// RKE2Config, bootstrap data Secret and infrastructure machine.
func BootstrapMachineName(clusterName string) string { return clusterName + "-bootstrap" }

// CAPIProviders returns the Cluster API operator bootstrap and control-plane
// provider installs for the RKE2 provider, for CAPIOptions.Bootstrap and
// .ControlPlane. Each argument overrides the corresponding default when
// non-empty: the cluster-api-provider-rke2 release version, the provider
// namespaces, and the two components manifest URLs (otherwise derived from the
// release assets for the version). Unlike k0smotron, the RKE2 bootstrap and
// control-plane providers run in distinct namespaces.
func CAPIProviders(version, bootstrapNS, controlPlaneNS, bootstrapURL, controlPlaneURL string) (bootstrap, controlPlane generate.ProviderInstall) {
	if version == "" {
		version = DefaultProviderVersion
	}
	if bootstrapNS == "" {
		bootstrapNS = BootstrapNamespace
	}
	if controlPlaneNS == "" {
		controlPlaneNS = ControlPlaneNamespace
	}
	base := "https://github.com/rancher/cluster-api-provider-rke2/releases/download/" + version + "/"
	if bootstrapURL == "" {
		bootstrapURL = base + "bootstrap-components.yaml"
	}
	if controlPlaneURL == "" {
		controlPlaneURL = base + "control-plane-components.yaml"
	}
	bootstrap = generate.ProviderInstall{Name: BootstrapProviderName, Namespace: bootstrapNS, Version: version, ComponentsURL: bootstrapURL}
	controlPlane = generate.ProviderInstall{Name: ControlPlaneProviderName, Namespace: controlPlaneNS, Version: version, ComponentsURL: controlPlaneURL}
	return bootstrap, controlPlane
}

// Prepare implements controlplane.Provider. It generates the RKE2 control-plane
// CA set (one cluster CA used for both the server and client CAs, plus distinct
// etcd peer and etcd server CAs) and an admin kubeconfig for the endpoint signed
// by the cluster CA. These are pre-seeded into rke2's PKI on node zero and into
// the cluster.x-k8s.io/secret Secrets RKE2ControlPlane looks up, so the running
// cluster's CAs match what the control plane expects and no CA is regenerated.
func (p *Provider) Prepare(_ context.Context, spec *cluster.Spec, ep cluster.Endpoint) (*controlplane.Credentials, error) {
	if spec == nil || spec.Name == "" {
		return nil, errors.New("rke2: cluster name is required")
	}
	if ep.Host == "" {
		return nil, errors.New("rke2: endpoint host is required")
	}
	clusterCA, err := generateCA()
	if err != nil {
		return nil, fmt.Errorf("rke2: generating cluster CA: %w", err)
	}
	peerEtcdCA, err := generateCA()
	if err != nil {
		return nil, fmt.Errorf("rke2: generating etcd peer CA: %w", err)
	}
	etcdCA, err := generateCA()
	if err != nil {
		return nil, fmt.Errorf("rke2: generating etcd server CA: %w", err)
	}
	creds := &controlplane.Credentials{
		CA: clusterCA,
		Extra: map[string]cluster.KeyPair{
			extraPeerEtcd: peerEtcdCA,
			extraEtcd:     etcdCA,
		},
	}

	caCert, err := certs.DecodeCertPEM(creds.CA.Cert)
	if err != nil {
		return nil, fmt.Errorf("rke2: decoding cluster CA: %w", err)
	}
	caKey, err := certs.DecodePrivateKeyPEM(creds.CA.Key)
	if err != nil {
		return nil, fmt.Errorf("rke2: decoding cluster CA key: %w", err)
	}
	cfg, err := kubeconfig.New(spec.Name, serverURL(ep), caCert, caKey)
	if err != nil {
		return nil, fmt.Errorf("rke2: building admin kubeconfig: %w", err)
	}
	creds.Kubeconfig, err = clientcmd.Write(*cfg)
	if err != nil {
		return nil, fmt.Errorf("rke2: serializing admin kubeconfig: %w", err)
	}
	return creds, nil
}

// generateCA returns a fresh self-signed CA key pair (CN=kubernetes, RSA, ten
// year validity), the form rke2's own PKI uses.
func generateCA() (cluster.KeyPair, error) {
	c := &secret.Certificate{Purpose: secret.ClusterCA}
	if err := c.Generate(); err != nil {
		return cluster.KeyPair{}, err
	}
	return cluster.KeyPair{Cert: c.KeyPair.Cert, Key: c.KeyPair.Key}, nil
}

type manifest struct {
	name string
	body []byte
}

// caSecret builds one cluster.x-k8s.io/secret CA Secret with the tls.crt/tls.key
// keys RKE2ControlPlane reads. It carries no owner reference; the control plane
// provider's LookupOrGenerate reuses the unowned Secret when it reconciles.
func caSecret(clusterName, ns, name string, kp cluster.KeyPair) *corev1.Secret {
	return &corev1.Secret{
		TypeMeta: metav1.TypeMeta{APIVersion: "v1", Kind: "Secret"},
		ObjectMeta: metav1.ObjectMeta{
			Name: name, Namespace: ns,
			Labels: map[string]string{clusterv1.ClusterNameLabel: clusterName},
		},
		Type: clusterv1.ClusterSecretType,
		Data: map[string][]byte{tlsCrtKey: kp.Cert, tlsKeyKey: kp.Key},
	}
}

// seedManifests renders the namespace and the Secrets RKE2ControlPlane looks up
// for the cluster: the four CA Secrets (<name>-ca, -cca, -peer-etcd, -etcd), the
// cluster token (<name>-token, value matching node zero's config) and the admin
// kubeconfig (<name>-kubeconfig). The control plane provider never runs its own
// token generation for an adopted node, so klotho must seed the token here.
func seedManifests(clusterName, ns string, creds *controlplane.Credentials, token string, kubeconfigData []byte) ([]manifest, error) {
	nsObj := &corev1.Namespace{
		TypeMeta:   metav1.TypeMeta{APIVersion: "v1", Kind: "Namespace"},
		ObjectMeta: metav1.ObjectMeta{Name: ns},
	}
	body, err := marshalDocuments(nsObj)
	if err != nil {
		return nil, err
	}
	out := []manifest{{name: "00-namespace.yaml", body: body}}

	cas := []struct {
		name string
		kp   cluster.KeyPair
	}{
		{clusterName + caSuffix, creds.CA},
		{clusterName + ccaSuffix, creds.CA},
		{clusterName + peerEtcdSuffix, creds.Extra[extraPeerEtcd]},
		{clusterName + etcdSuffix, creds.Extra[extraEtcd]},
	}
	for _, c := range cas {
		if len(c.kp.Cert) == 0 || len(c.kp.Key) == 0 {
			return nil, fmt.Errorf("rke2: credentials are missing the CA for Secret %q", c.name)
		}
		body, err := marshalDocuments(caSecret(clusterName, ns, c.name, c.kp))
		if err != nil {
			return nil, err
		}
		out = append(out, manifest{name: c.name + ".yaml", body: body})
	}

	tokenSecret := &corev1.Secret{
		TypeMeta: metav1.TypeMeta{APIVersion: "v1", Kind: "Secret"},
		ObjectMeta: metav1.ObjectMeta{
			Name: clusterName + tokenSuffix, Namespace: ns,
			Labels: map[string]string{clusterv1.ClusterNameLabel: clusterName},
		},
		Type: clusterv1.ClusterSecretType,
		Data: map[string][]byte{"value": []byte(token)},
	}
	body, err = marshalDocuments(tokenSecret)
	if err != nil {
		return nil, err
	}
	out = append(out, manifest{name: clusterName + tokenSuffix + ".yaml", body: body})

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

// nodeCAFiles returns the eight CA files rke2 reads from its server tls
// directory, pre-seeded from the minted credentials so rke2 uses klotho's CAs
// instead of self-signing. The cluster CA serves as both the server and client
// CA; the etcd peer and server CAs are distinct.
func nodeCAFiles(creds *controlplane.Credentials) []controlplane.File {
	file := func(path string, content []byte, mode string) controlplane.File {
		return controlplane.File{Path: path, Content: content, Mode: mode, Owner: "root:root"}
	}
	pair := func(base string, kp cluster.KeyPair) []controlplane.File {
		return []controlplane.File{
			file(base+".crt", kp.Cert, "0640"),
			file(base+".key", kp.Key, "0600"),
		}
	}
	var out []controlplane.File
	out = append(out, pair(TLSDir+"/server-ca", creds.CA)...)
	out = append(out, pair(TLSDir+"/client-ca", creds.CA)...)
	out = append(out, pair(TLSDir+"/etcd/peer-ca", creds.Extra[extraPeerEtcd])...)
	out = append(out, pair(TLSDir+"/etcd/server-ca", creds.Extra[extraEtcd])...)
	return out
}

func marshalDocuments(objs ...any) ([]byte, error) {
	var b strings.Builder
	for _, o := range objs {
		y, err := yaml.Marshal(o)
		if err != nil {
			return nil, fmt.Errorf("rke2: marshalling manifest: %w", err)
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
		return cluster.Endpoint{}, fmt.Errorf("rke2: parsing admin kubeconfig: %w", err)
	}
	for _, c := range cfg.Clusters {
		rest, ok := strings.CutPrefix(c.Server, "https://")
		if !ok {
			continue
		}
		host, port, err := net.SplitHostPort(strings.TrimSuffix(rest, "/"))
		if err != nil {
			return cluster.Endpoint{}, fmt.Errorf("rke2: kubeconfig server %q: %w", c.Server, err)
		}
		n, err := strconv.Atoi(port)
		if err != nil {
			return cluster.Endpoint{}, fmt.Errorf("rke2: kubeconfig server %q: %w", c.Server, err)
		}
		return cluster.Endpoint{Host: host, Port: n}, nil
	}
	return cluster.Endpoint{}, errors.New("rke2: admin kubeconfig has no https server")
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
