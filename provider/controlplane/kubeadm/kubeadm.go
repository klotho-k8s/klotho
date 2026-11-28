// Package kubeadm is the control plane provider for kubeadm-based clusters
// managed by the Cluster API KubeadmControlPlane provider.
//
// It mints the cluster's certificate authorities and an admin kubeconfig
// before any node exists, turns the bootstrap node into the first control
// plane member with kubeadm init, installs the Cluster API operator stack on
// it, seeds the Secrets KubeadmControlPlane expects to find, and renders the
// Machine and KubeadmConfig that let KubeadmControlPlane adopt the node.
//
// # Source of configuration
//
// The node is configured from the KubeadmControlPlane in the generated cluster
// bundle, not from settings of this package. The provider computes the
// KubeadmConfigSpec that KubeadmControlPlane itself would give the first
// control plane machine (spec.kubeadmConfigSpec with joinConfiguration
// cleared and the kubeadm feature gates KubeadmControlPlane defaults for the
// version), renders the bootstrap node's user-data from it the way the kubeadm
// bootstrap provider would, and records the same spec on the adopted
// KubeadmConfig so KubeadmControlPlane sees the machine as up to date.
//
// Precedence: anything the KubeadmControlPlane sets wins. Options only fill
// gaps the control plane object cannot express or leaves unset:
//   - KubernetesVersion is used only when spec.version is empty.
//   - PodCIDR and ServiceCIDR stand in for the Cluster's clusterNetwork, which
//     v1beta2 keeps on the Cluster object rather than in the
//     ClusterConfiguration.
//   - The cluster name comes from cluster.Spec.Name and the control plane
//     endpoint from the credentials, unless clusterConfiguration sets one.
//   - CertManagerURL, OperatorURL, CNIManifestURL, SSHAuthorizedKeys and
//     ExtraPostCommands are this package's own additions and have no
//     counterpart in the spec.
//
// # Jinja
//
// The kubeadm bootstrap provider marks its user-data "## template: jinja", so
// spec files, commands and kubeadm arguments may contain cloud-init Jinja
// expressions. This package's user-data is plain cloud-config, so it resolves
// them itself: the expressions {{ ds.meta_data.instance_id }},
// {{ ds.meta_data.hostname }} and {{ ds.meta_data.local_hostname }} are
// replaced with tokens at render time and filled in at boot from
// "cloud-init query" by a pre-command. Any other Jinja expression or
// statement makes UserData fail; a value missing at boot makes the
// pre-command fail, and kubeadm init then refuses to run. The adopted
// KubeadmConfig keeps the original, unresolved text.
//
// # Ordering
//
// cloudinit.Build runs every contribution's PreCommands, then every
// InstallCommands, then every PostCommands, each group in contribution order.
// This provider's PostCommands install the CNI, cert-manager and the Cluster
// API operator; an infrastructure provider's PostCommands that create
// operator provider objects or the cluster bundle depend on them, so the
// control plane contribution must come before the infrastructure
// contribution in the slice passed to cloudinit.Build.
package kubeadm

import (
	"context"
	"errors"
	"fmt"
	"net"
	"strconv"
	"strings"

	"github.com/blang/semver/v4"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/client-go/tools/clientcmd"
	"sigs.k8s.io/yaml"

	bootstrapv1 "sigs.k8s.io/cluster-api/api/bootstrap/kubeadm/v1beta2"
	controlplanev1 "sigs.k8s.io/cluster-api/api/controlplane/kubeadm/v1beta2"
	clusterv1 "sigs.k8s.io/cluster-api/api/core/v1beta2"
	kubeadmtypes "sigs.k8s.io/cluster-api/bootstrap/kubeadm/types"
	"sigs.k8s.io/cluster-api/bootstrap/kubeadm/types/upstream"
	"sigs.k8s.io/cluster-api/util/certs"
	"sigs.k8s.io/cluster-api/util/kubeconfig"
	"sigs.k8s.io/cluster-api/util/labels/format"
	"sigs.k8s.io/cluster-api/util/secret"

	"github.com/klotho-k8s/klotho/pkg/cluster"
	"github.com/klotho-k8s/klotho/pkg/generate"
	"github.com/klotho-k8s/klotho/provider/controlplane"
)

// Name is the provider's identifier.
const Name = "kubeadm"

// Kind is the control plane object kind this provider handles.
const Kind = "KubeadmControlPlane"

// Pinned defaults.
const (
	// DefaultCertManagerURL is the cert-manager release manifest. The Cluster
	// API operator's webhooks need cert-manager-issued serving certificates.
	DefaultCertManagerURL = "https://github.com/cert-manager/cert-manager/releases/download/v1.21.3/cert-manager.yaml"

	// DefaultOperatorURL is the Cluster API operator release manifest.
	DefaultOperatorURL = "https://github.com/kubernetes-sigs/cluster-api-operator/releases/download/v0.30.1/operator-components.yaml"

	// DefaultCNIManifestURL is the Calico manifest install (Kubernetes API
	// datastore) for v3.33.1. Calico is used because its plain manifest
	// needs no templating: with CALICO_IPV4POOL_CIDR left unset, calico-node
	// takes the pool from kubeadm's podSubnet, and its default pool
	// 192.168.0.0/16 matches the pod CIDR in the Proxmox cluster template.
	DefaultCNIManifestURL = "https://raw.githubusercontent.com/projectcalico/calico/v3.33.1/manifests/calico.yaml"

	// DefaultPodCIDR is the pod network the Proxmox cluster template's
	// Cluster object declares.
	DefaultPodCIDR = "192.168.0.0/16"

	// DefaultPort is the API server port when the endpoint does not set one.
	DefaultPort = 6443
)

// Paths on the bootstrap node.
const (
	KubeadmConfigPath = "/run/kubeadm/kubeadm.yaml"
	ManifestsDir      = "/run/klotho/manifests"
	CommandsDir       = "/run/klotho/commands"
	JinjaScript       = "/run/klotho/render-jinja.sh"
	JinjaDoneMarker   = "/run/klotho/jinja-rendered"
	UsersScript       = "/run/klotho/users.sh"
	PostInitScript    = "/run/klotho/post-init.sh"
	EndpointFixScript = "/run/kubeadm/endpoint-fix.sh"
	AdminKubeconfig   = "/etc/kubernetes/admin.conf"

	sentinelPlaceholder = "/run/cluster-api/placeholder"
	sentinelCommand     = "echo success > /run/cluster-api/bootstrap-success.complete"
)

// controlPlaneKubeletLocalMode is the kubeadm feature gate KubeadmControlPlane
// enables on every machine it creates for 1.31 <= version < 1.36.
const controlPlaneKubeletLocalMode = "ControlPlaneKubeletLocalMode"

var (
	kubeletLocalModeMin     = semver.MustParse("1.31.0")
	kubeletLocalModeDropped = semver.MustParse("1.36.0")
)

// Options are this package's additions to what the KubeadmControlPlane
// specifies. See the package documentation for precedence.
type Options struct {
	// KubernetesVersion is used only when the KubeadmControlPlane has no
	// spec.version.
	KubernetesVersion string
	// PodCIDR and ServiceCIDR stand in for the Cluster's clusterNetwork.
	// PodCIDR defaults to DefaultPodCIDR; an empty ServiceCIDR leaves
	// kubeadm's default.
	PodCIDR, ServiceCIDR string
	CertManagerURL       string
	OperatorURL          string
	CNIManifestURL       string
	// SSHAuthorizedKeys are appended to root's authorized_keys, in addition
	// to any users the KubeadmControlPlane defines.
	SSHAuthorizedKeys []string
	// ExtraPostCommands run at the end of the post-init script, with
	// KUBECONFIG pointing at the admin kubeconfig.
	ExtraPostCommands []string
	// ContainerNodes tells the provider the control plane runs inside
	// containers (the Docker/CAPD provider), where kube-proxy cannot write the
	// host's conntrack sysctls (/proc/sys/net/netfilter/nf_conntrack_*), which
	// are read-only in a rootless container and make kube-proxy crash-loop. It
	// makes kubeadm emit a KubeProxyConfiguration that disables those writes,
	// exactly as kind does for its nodes.
	ContainerNodes bool
}

// Provider is the kubeadm control plane provider.
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
	case "hyperv":
		// The default title-caser below would yield "HypervMachine", but the
		// Cluster API Hyper-V provider's CRD kind is "HyperVMachine" (capital
		// V); adoption references this kind by name, so it must match exactly.
		return "HyperVMachine"
	}
	return strings.ToUpper(provider[:1]) + provider[1:] + "Machine"
}

// BootstrapMachineName is the name shared by the bootstrap node's Machine,
// KubeadmConfig, bootstrap data Secret and infrastructure machine.
func BootstrapMachineName(clusterName string) string { return clusterName + "-bootstrap" }

// Prepare implements controlplane.Provider. It generates the cluster CA, etcd
// CA, front proxy CA and service account key pair, and an admin kubeconfig
// for the endpoint signed by the cluster CA.
func (p *Provider) Prepare(_ context.Context, spec *cluster.Spec, ep cluster.Endpoint) (*controlplane.Credentials, error) {
	if spec == nil || spec.Name == "" {
		return nil, errors.New("kubeadm: cluster name is required")
	}
	if ep.Host == "" {
		return nil, errors.New("kubeadm: endpoint host is required")
	}
	certificates := secret.NewCertificatesForInitialControlPlane(nil)
	if err := certificates.Generate(); err != nil {
		return nil, fmt.Errorf("kubeadm: generating certificates: %w", err)
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
		return nil, fmt.Errorf("kubeadm: decoding cluster CA: %w", err)
	}
	caKey, err := certs.DecodePrivateKeyPEM(creds.CA.Key)
	if err != nil {
		return nil, fmt.Errorf("kubeadm: decoding cluster CA key: %w", err)
	}
	cfg, err := kubeconfig.New(spec.Name, serverURL(ep), caCert, caKey)
	if err != nil {
		return nil, fmt.Errorf("kubeadm: building admin kubeconfig: %w", err)
	}
	creds.Kubeconfig, err = clientcmd.Write(*cfg)
	if err != nil {
		return nil, fmt.Errorf("kubeadm: serializing admin kubeconfig: %w", err)
	}
	return creds, nil
}

// UserData implements controlplane.Provider.
func (p *Provider) UserData(_ context.Context, spec *cluster.Spec, creds *controlplane.Credentials, cp, clusterObj *unstructured.Unstructured) (*controlplane.Contribution, error) {
	if spec == nil || spec.Name == "" {
		return nil, errors.New("kubeadm: cluster name is required")
	}
	if creds == nil {
		return nil, errors.New("kubeadm: credentials are required")
	}
	kcp, err := decodeControlPlane(cp)
	if err != nil {
		return nil, err
	}
	version, err := p.kubernetesVersion(kcp)
	if err != nil {
		return nil, err
	}
	cfg, err := FirstMachineConfigSpec(kcp, version)
	if err != nil {
		return nil, err
	}
	if err := checkSupported(cfg); err != nil {
		return nil, err
	}
	ep, err := endpointFromKubeconfig(creds.Kubeconfig)
	if err != nil {
		return nil, err
	}
	certificates, err := certificatesFromCredentials(creds, &cfg.ClusterConfiguration)
	if err != nil {
		return nil, err
	}
	nw, err := p.network(clusterObj)
	if err != nil {
		return nil, err
	}

	var (
		c        controlplane.Contribution
		jinja    = newJinjaRenderer()
		addFile  = func(f controlplane.File) { c.Files = append(c.Files, f) }
		rootKeys []string
	)

	for _, f := range certificates.AsFiles() {
		addFile(controlplane.File{Path: f.Path, Content: []byte(f.Content), Mode: f.Permissions, Owner: f.Owner})
	}

	// Spec files, decoded and with Jinja resolved as the kubeadm bootstrap
	// provider's user-data would have them.
	for _, f := range cfg.Files {
		content, plain, err := decodeFileContent(f)
		if err != nil {
			return nil, err
		}
		if plain {
			if content, err = jinja.prepare(f.Path, content); err != nil {
				return nil, err
			}
		}
		addFile(controlplane.File{
			Path:    f.Path,
			Content: content,
			Mode:    f.Permissions,
			Owner:   f.Owner,
			Append:  f.Append != nil && *f.Append,
		})
	}

	kubeadmYAML, err := p.kubeadmConfig(spec, cfg, ep, version, nw)
	if err != nil {
		return nil, err
	}
	if kubeadmYAML, err = jinja.prepare(KubeadmConfigPath, kubeadmYAML); err != nil {
		return nil, err
	}
	addFile(controlplane.File{Path: KubeadmConfigPath, Content: kubeadmYAML, Mode: "0640", Owner: "root:root"})
	addFile(controlplane.File{
		Path:    sentinelPlaceholder,
		Content: []byte("Creates the /run/cluster-api directory for the bootstrap sentinel file.\n"),
		Mode:    "0640", Owner: "root:root",
	})

	manifests, err := secretManifests(spec.Name, namespace(spec), certificates, creds.Kubeconfig)
	if err != nil {
		return nil, err
	}
	for _, m := range manifests {
		addFile(controlplane.File{Path: ManifestsDir + "/" + m.name, Content: m.body, Mode: "0600", Owner: "root:root"})
	}

	// Users: root's keys become an authorized_keys append; other users are
	// created by a pre-command script.
	var others []bootstrapv1.User
	for _, u := range cfg.Users {
		if u.Name == "root" {
			rootKeys = append(rootKeys, u.SSHAuthorizedKeys...)
			continue
		}
		others = append(others, u)
	}
	rootKeys = append(rootKeys, p.opts.SSHAuthorizedKeys...)
	if len(rootKeys) > 0 {
		addFile(controlplane.File{
			Path:    "/root/.ssh/authorized_keys",
			Content: []byte(strings.Join(rootKeys, "\n") + "\n"),
			Mode:    "0600", Owner: "root:root", Append: true,
		})
	}

	// Commands. Ones containing Jinja run from a script file so the boot-time
	// renderer can rewrite them.
	commands := func(group string, cmds []string) ([]string, error) {
		out := make([]string, 0, len(cmds))
		for i, cmd := range cmds {
			if !hasJinja(cmd) {
				out = append(out, cmd)
				continue
			}
			p := fmt.Sprintf("%s/%s-%02d.sh", CommandsDir, group, i)
			body, err := jinja.prepare(p, []byte(cmd))
			if err != nil {
				return nil, err
			}
			script := "#!/bin/sh\n" +
				"[ -f " + JinjaDoneMarker + " ] || { echo \"" + p + ": Jinja expressions were not rendered\" >&2; exit 1; }\n" +
				string(body) + "\n"
			addFile(controlplane.File{Path: p, Content: []byte(script), Mode: "0700", Owner: "root:root"})
			out = append(out, runSh(p))
		}
		return out, nil
	}
	pre, err := commands("pre", cfg.PreKubeadmCommands)
	if err != nil {
		return nil, err
	}
	post, err := commands("post", cfg.PostKubeadmCommands)
	if err != nil {
		return nil, err
	}

	// Before kubeadm init: if the control-plane endpoint is not an address this
	// node holds (a DHCP node whose endpoint could only be a placeholder before
	// boot), point it at this node's primary IPv4 so every kubeadm init phase
	// reaches the local API server and the cluster comes up complete.
	addFile(controlplane.File{Path: EndpointFixScript, Content: []byte(endpointFixScript), Mode: "0700", Owner: "root:root"})
	c.PreCommands = append(c.PreCommands, runBash(EndpointFixScript))

	if jinja.needed() {
		addFile(controlplane.File{Path: JinjaScript, Content: jinja.script(), Mode: "0700", Owner: "root:root"})
		c.PreCommands = append(c.PreCommands, runBash(JinjaScript))
	}
	if len(others) > 0 {
		addFile(controlplane.File{Path: UsersScript, Content: usersScript(others), Mode: "0700", Owner: "root:root"})
		c.PreCommands = append(c.PreCommands, runBash(UsersScript))
	}
	c.PreCommands = append(c.PreCommands, pre...)

	// --upload-certs is deliberately absent: it only stores the control plane
	// certificates in the kubeadm-certs Secret for "kubeadm join
	// --certificate-key". Machines joined by KubeadmControlPlane receive the
	// certificates from the <cluster>-ca/-etcd/-sa/-proxy Secrets instead.
	initCmd := "kubeadm init --config " + KubeadmConfigPath
	if cfg.Verbosity != nil {
		initCmd += " --v " + strconv.Itoa(int(*cfg.Verbosity))
	}
	initCmd += " && " + sentinelCommand
	if jinja.needed() {
		initCmd = "test -f " + JinjaDoneMarker + " && " + initCmd
	}
	c.InstallCommands = append([]string{initCmd}, post...)

	addFile(controlplane.File{Path: PostInitScript, Content: p.postInitScript(ep), Mode: "0700", Owner: "root:root"})
	c.PostCommands = []string{runBash(PostInitScript)}
	return &c, nil
}

// runBash and runSh return the runcmd entry that runs a script klotho wrote
// under /run. Ubuntu mounts /run noexec, so the file's mode does not make it
// runnable; handing it to the interpreter does.
func runBash(path string) string { return "/bin/bash " + path }
func runSh(path string) string   { return "/bin/sh " + path }

// Adopt implements controlplane.Provider. It renders a placeholder bootstrap
// data Secret, a KubeadmConfig and a Machine, all named "<cluster>-bootstrap",
// so that KubeadmControlPlane adopts the bootstrap node without a rollout.
func (p *Provider) Adopt(_ context.Context, state *cluster.State, cp *unstructured.Unstructured) ([]byte, error) {
	if state == nil || state.Spec.Name == "" {
		return nil, errors.New("kubeadm: cluster state with a name is required")
	}
	spec := &state.Spec
	kcp, err := decodeControlPlane(cp)
	if err != nil {
		return nil, err
	}
	version, err := p.kubernetesVersion(kcp)
	if err != nil {
		return nil, err
	}
	cfg, err := FirstMachineConfigSpec(kcp, version)
	if err != nil {
		return nil, err
	}
	kind := InfraMachineKind(spec.Provider)
	if kind == "" {
		return nil, errors.New("kubeadm: an infrastructure provider is required to adopt the bootstrap machine")
	}
	ns := namespace(spec)
	name := BootstrapMachineName(spec.Name)

	// The labels and annotations KubeadmControlPlane puts on the machines it
	// creates.
	labels := map[string]string{}
	for k, v := range kcp.Spec.MachineTemplate.ObjectMeta.Labels {
		labels[k] = v
	}
	labels[clusterv1.ClusterNameLabel] = spec.Name
	labels[clusterv1.MachineControlPlaneLabel] = ""
	if kcp.Name != "" {
		labels[clusterv1.MachineControlPlaneNameLabel] = format.MustFormatValue(kcp.Name)
	}
	var annotations map[string]string
	if len(kcp.Spec.MachineTemplate.ObjectMeta.Annotations) > 0 {
		annotations = map[string]string{}
		for k, v := range kcp.Spec.MachineTemplate.ObjectMeta.Annotations {
			annotations[k] = v
		}
	}

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

	config := &bootstrapv1.KubeadmConfig{
		TypeMeta:   metav1.TypeMeta{APIVersion: bootstrapv1.GroupVersion.String(), Kind: "KubeadmConfig"},
		ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: ns, Labels: labels, Annotations: annotations},
		Spec:       *cfg,
	}

	dataSecretName := name
	machine := &clusterv1.Machine{
		TypeMeta:   metav1.TypeMeta{APIVersion: clusterv1.GroupVersion.String(), Kind: "Machine"},
		ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: ns, Labels: labels, Annotations: annotations},
		Spec: clusterv1.MachineSpec{
			ClusterName: spec.Name,
			Version:     version,
			Bootstrap: clusterv1.Bootstrap{
				ConfigRef: clusterv1.ContractVersionedObjectReference{
					APIGroup: bootstrapv1.GroupVersion.Group,
					Kind:     "KubeadmConfig",
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

// FirstMachineConfigSpec returns the KubeadmConfigSpec KubeadmControlPlane
// would give the first control plane machine for version: the control
// plane's kubeadmConfigSpec with joinConfiguration cleared, and the kubeadm
// feature gates KubeadmControlPlane defaults for that version.
func FirstMachineConfigSpec(kcp *controlplanev1.KubeadmControlPlane, version string) (*bootstrapv1.KubeadmConfigSpec, error) {
	v, err := semver.ParseTolerant(version)
	if err != nil {
		return nil, fmt.Errorf("kubeadm: invalid Kubernetes version %q: %w", version, err)
	}
	spec := kcp.Spec.KubeadmConfigSpec.DeepCopy()
	spec.JoinConfiguration = bootstrapv1.JoinConfiguration{}

	core := semver.Version{Major: v.Major, Minor: v.Minor, Patch: v.Patch}
	if core.GTE(kubeletLocalModeMin) && core.LT(kubeletLocalModeDropped) {
		if spec.ClusterConfiguration.FeatureGates == nil {
			spec.ClusterConfiguration.FeatureGates = map[string]bool{}
		}
		if _, ok := spec.ClusterConfiguration.FeatureGates[controlPlaneKubeletLocalMode]; !ok {
			spec.ClusterConfiguration.FeatureGates[controlPlaneKubeletLocalMode] = true
		}
	}
	return spec, nil
}

func decodeControlPlane(u *unstructured.Unstructured) (*controlplanev1.KubeadmControlPlane, error) {
	if u == nil {
		return nil, errors.New("kubeadm: the KubeadmControlPlane object is required")
	}
	gvk := u.GroupVersionKind()
	if gvk.Kind != Kind || gvk.Group != controlplanev1.GroupVersion.Group {
		return nil, fmt.Errorf("kubeadm: control plane object is %s, want %s %s", gvk, controlplanev1.GroupVersion.Group, Kind)
	}
	if gvk.Version != controlplanev1.GroupVersion.Version {
		return nil, fmt.Errorf("kubeadm: KubeadmControlPlane %s is not supported, want %s", gvk.Version, controlplanev1.GroupVersion.Version)
	}
	kcp := &controlplanev1.KubeadmControlPlane{}
	if err := runtime.DefaultUnstructuredConverter.FromUnstructured(u.Object, kcp); err != nil {
		return nil, fmt.Errorf("kubeadm: decoding KubeadmControlPlane: %w", err)
	}
	return kcp, nil
}

// checkSupported rejects parts of a KubeadmConfigSpec the bootstrap node's
// user-data cannot reproduce, rather than silently diverging from the
// machines KubeadmControlPlane creates later.
func checkSupported(spec *bootstrapv1.KubeadmConfigSpec) error {
	var bad []string
	if spec.Format == bootstrapv1.Ignition {
		bad = append(bad, "format: ignition")
	}
	if spec.DiskSetup.IsDefined() {
		bad = append(bad, "diskSetup")
	}
	if len(spec.Mounts) > 0 {
		bad = append(bad, "mounts")
	}
	if len(spec.BootCommands) > 0 {
		bad = append(bad, "bootCommands")
	}
	if spec.NTP.IsDefined() {
		bad = append(bad, "ntp")
	}
	if spec.ClusterConfiguration.Etcd.External.IsDefined() {
		bad = append(bad, "clusterConfiguration.etcd.external")
	}
	for _, f := range spec.Files {
		if f.ContentFrom.IsDefined() {
			bad = append(bad, "files["+f.Path+"].contentFrom")
		}
	}
	for _, u := range spec.Users {
		if u.Passwd != "" || u.PasswdFrom.IsDefined() {
			bad = append(bad, "users["+u.Name+"] password")
		}
	}
	if len(bad) > 0 {
		return fmt.Errorf("kubeadm: kubeadmConfigSpec uses settings the bootstrap node cannot reproduce: %s", strings.Join(bad, ", "))
	}
	return nil
}

func (p *Provider) kubernetesVersion(kcp *controlplanev1.KubeadmControlPlane) (string, error) {
	v := kcp.Spec.Version
	if v == "" {
		v = p.opts.KubernetesVersion
	}
	if v == "" {
		return "", errors.New("kubeadm: a Kubernetes version is required (KubeadmControlPlane spec.version)")
	}
	if _, err := semver.ParseTolerant(v); err != nil {
		return "", fmt.Errorf("kubeadm: invalid Kubernetes version %q: %w", v, err)
	}
	return v, nil
}

// kubeadmConfig renders the ClusterConfiguration and InitConfiguration the
// way the kubeadm bootstrap provider does for the first control plane
// machine, in the kubeadm API version matching the Kubernetes version.
func (p *Provider) kubeadmConfig(spec *cluster.Spec, cfg *bootstrapv1.KubeadmConfigSpec, ep cluster.Endpoint, version string, nw network) ([]byte, error) {
	sv, err := semver.ParseTolerant(version)
	if err != nil {
		return nil, fmt.Errorf("kubeadm: invalid Kubernetes version %q: %w", version, err)
	}
	clusterCfg := cfg.ClusterConfiguration.DeepCopy()
	if clusterCfg.ControlPlaneEndpoint == "" {
		clusterCfg.ControlPlaneEndpoint = net.JoinHostPort(ep.Host, strconv.Itoa(ep.Port))
	}
	clusterName := spec.Name
	data := &upstream.AdditionalData{
		KubernetesVersion: &version,
		ClusterName:       &clusterName,
	}
	if nw.pods != "" {
		podCIDR := nw.pods
		data.PodSubnet = &podCIDR
	}
	if nw.services != "" {
		serviceCIDR := nw.services
		data.ServiceSubnet = &serviceCIDR
	}
	if s := cfg.InitConfiguration.Timeouts.ControlPlaneComponentHealthCheckSeconds; s != nil {
		data.ControlPlaneComponentHealthCheckSeconds = s
	}
	clusterYAML, err := kubeadmtypes.MarshalClusterConfigurationForVersion(clusterCfg, sv, data)
	if err != nil {
		return nil, fmt.Errorf("kubeadm: rendering ClusterConfiguration: %w", err)
	}
	initCfg := &cfg.InitConfiguration
	if p.opts.ContainerNodes {
		// The adopted bootstrap node is hand-booted and kubeadm-init'd by
		// klotho, so CAPD never runs the providerID patch it applies to the
		// nodes it creates itself. Without a providerID the adopted Machine
		// never gets a status.nodeRef and the control plane stays unavailable.
		// Give the kubelet the CAPD provider ID at init so node zero registers
		// with spec.providerID = docker:////<node> from the start; the node
		// name is the bootstrap container name, which is BootstrapMachineName.
		// This injects into the rendered bootstrap kubeadm.yaml only, not the
		// recorded KubeadmConfig, so KubeadmControlPlane sees no rollout and the
		// machines it creates later (whose providerID CAPD sets) are untouched.
		// Mirrors the rke2 kubelet-arg and k0s post-init patch on CAPD.
		initCfg = withBootstrapProviderID(initCfg, BootstrapMachineName(clusterName))
	}
	initYAML, err := kubeadmtypes.MarshalInitConfigurationForVersion(initCfg, sv)
	if err != nil {
		return nil, fmt.Errorf("kubeadm: rendering InitConfiguration: %w", err)
	}
	out := "---\n" + clusterYAML + "---\n" + initYAML
	if p.opts.ContainerNodes {
		out += "---\n" + kubeProxyNoConntrackConfig
	}
	return []byte(out), nil
}

// kubeProxyNoConntrackConfig disables every kube-proxy conntrack sysctl write.
// Inside a rootless container /proc/sys/net/netfilter/nf_conntrack_* is
// read-only, so a kube-proxy that tries to size the conntrack table crash-loops
// ("open /proc/sys/net/netfilter/nf_conntrack_max: permission denied"), which
// stops the CNI from reaching the API server and the node from going Ready.
// Zeroing maxPerCore, min and both timeouts makes kube-proxy skip the writes,
// the same configuration kind applies to its nodes.
const kubeProxyNoConntrackConfig = `apiVersion: kubeproxy.config.k8s.io/v1alpha1
kind: KubeProxyConfiguration
conntrack:
  maxPerCore: 0
  min: 0
  tcpEstablishedTimeout: 0s
  tcpCloseWaitTimeout: 0s
`

// dockerProviderIDPrefix is the DockerMachine (CAPD) provider ID scheme; the
// adopted bootstrap node's provider ID is this prefix plus its container name,
// which equals the bootstrap machine name. Kept as a literal here, as the rke2
// and k0s CAPD paths do, to avoid importing the docker infrastructure package.
const dockerProviderIDPrefix = "docker:////"

// bootstrapProviderIDArg is the kubelet flag that sets a node's provider ID.
const bootstrapProviderIDArg = "provider-id"

// withBootstrapProviderID returns a deep copy of init with a kubelet
// provider-id extra arg pinned to the CAPD provider ID for nodeName, so the
// adopted bootstrap node registers with spec.providerID already set. It is a
// no-op copy when the spec already sets provider-id (the KubeadmControlPlane
// wins). --provider-id is a valid kubelet flag for every supported version.
func withBootstrapProviderID(init *bootstrapv1.InitConfiguration, nodeName string) *bootstrapv1.InitConfiguration {
	out := init.DeepCopy()
	for _, a := range out.NodeRegistration.KubeletExtraArgs {
		if a.Name == bootstrapProviderIDArg {
			return out
		}
	}
	value := dockerProviderIDPrefix + nodeName
	out.NodeRegistration.KubeletExtraArgs = append(out.NodeRegistration.KubeletExtraArgs, bootstrapv1.Arg{
		Name:  bootstrapProviderIDArg,
		Value: &value,
	})
	return out
}

// providerID returns the provider ID for a bootstrap node ID: the ID itself
// when it already carries a scheme, the provider's scheme plus the ID when
// the provider has one, or "" so the Machine takes it from the
// infrastructure machine.
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

func certificatesFromCredentials(creds *controlplane.Credentials, cc *bootstrapv1.ClusterConfiguration) (secret.Certificates, error) {
	certificates := secret.NewCertificatesForInitialControlPlane(cc)
	for _, c := range certificates {
		var kp cluster.KeyPair
		if c.Purpose == secret.ClusterCA {
			kp = creds.CA
		} else {
			kp = creds.Extra[string(c.Purpose)]
		}
		if len(kp.Cert) == 0 || len(kp.Key) == 0 {
			return nil, fmt.Errorf("kubeadm: credentials are missing the %q key pair", c.Purpose)
		}
		c.KeyPair = &certs.KeyPair{Cert: kp.Cert, Key: kp.Key}
	}
	return certificates, nil
}

type manifest struct {
	name string
	body []byte
}

// secretManifests renders the namespace, the four certificate Secrets and the
// kubeconfig Secret that KubeadmControlPlane looks up for the cluster. They
// carry no owner references: KubeadmControlPlane makes itself the controller
// of unowned cluster.x-k8s.io/secret Secrets when it reconciles them.
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
			return nil, fmt.Errorf("kubeadm: marshalling manifest: %w", err)
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
		return cluster.Endpoint{}, fmt.Errorf("kubeadm: parsing admin kubeconfig: %w", err)
	}
	for _, c := range cfg.Clusters {
		rest, ok := strings.CutPrefix(c.Server, "https://")
		if !ok {
			continue
		}
		host, port, err := net.SplitHostPort(strings.TrimSuffix(rest, "/"))
		if err != nil {
			return cluster.Endpoint{}, fmt.Errorf("kubeadm: kubeconfig server %q: %w", c.Server, err)
		}
		n, err := strconv.Atoi(port)
		if err != nil {
			return cluster.Endpoint{}, fmt.Errorf("kubeadm: kubeconfig server %q: %w", c.Server, err)
		}
		return cluster.Endpoint{Host: host, Port: n}, nil
	}
	return cluster.Endpoint{}, errors.New("kubeadm: admin kubeconfig has no https server")
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

// network is what kubeadm's ClusterConfiguration.networking is filled from.
type network struct {
	pods, services string
}

// network takes the pod and service subnets from the Cluster object's
// clusterNetwork, where the v1beta2 API keeps them, falling back to the
// provider options when the object or a network is absent. Several blocks
// become kubeadm's comma-separated dual-stack form.
func (p *Provider) network(clusterObj *unstructured.Unstructured) (network, error) {
	nw := network{pods: p.opts.PodCIDR, services: p.opts.ServiceCIDR}
	pods, services, err := generate.ClusterNetwork(clusterObj)
	if err != nil {
		return network{}, fmt.Errorf("kubeadm: %w", err)
	}
	if len(pods) > 0 {
		nw.pods = strings.Join(pods, ",")
	}
	if len(services) > 0 {
		nw.services = strings.Join(services, ",")
	}
	return nw, nil
}
