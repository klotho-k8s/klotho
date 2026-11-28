package generate

import (
	"bytes"
	"compress/gzip"
	"encoding/base64"
	"fmt"
	"slices"
	"sort"
	"strconv"
	"strings"

	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime/schema"

	"github.com/klotho-k8s/klotho/provider"
)

// Cluster API operator provider kinds, operator.cluster.x-k8s.io/v1alpha2.
const (
	OperatorGroup   = "operator.cluster.x-k8s.io"
	OperatorVersion = "v1alpha2"

	KindCoreProvider           = "CoreProvider"
	KindBootstrapProvider      = "BootstrapProvider"
	KindControlPlaneProvider   = "ControlPlaneProvider"
	KindInfrastructureProvider = "InfrastructureProvider"
)

// Namespaces and names of the providers klotho installs.
const (
	CoreNamespace         = "capi-system"
	BootstrapNamespace    = "capi-kubeadm-bootstrap-system"
	ControlPlaneNamespace = "capi-kubeadm-control-plane-system"
	CoreProviderName      = "cluster-api"
	KubeadmProviderName   = "kubeadm"
)

// Component file names in a Cluster API release, appended to
// CAPIOptions.FetchURL.
const (
	CoreComponents         = "core-components.yaml"
	BootstrapComponents    = "bootstrap-components.yaml"
	ControlPlaneComponents = "control-plane-components.yaml"
)

// Labels the Cluster API operator matches a components ConfigMap on. A provider
// whose fetchConfig uses a selector is served from a ConfigMap carrying these
// labels; the operator matches the selector and requires the version label to
// equal the provider's spec.version.
//
// Embedding components in a ConfigMap is how klotho serves a provider whose
// components file is reachable only over plain http (a local, unpublished
// build): the in-cluster operator refuses an http fetchConfig URL ("no provider
// implementation for \"http\" schema"), but reads components from a ConfigMap on
// any scheme. It is provider-agnostic - any of the core, bootstrap, control
// plane or infrastructure providers can be served this way.
const (
	ProviderNameLabel    = "provider.cluster.x-k8s.io/name"
	ProviderTypeLabel    = "provider.cluster.x-k8s.io/type"
	ProviderVersionLabel = "provider.cluster.x-k8s.io/version"
	// CompressedAnnotation marks a components ConfigMap whose components are
	// gzip-compressed in binaryData rather than plaintext in data. The Cluster
	// API operator reads binaryData["components"] and gunzips it when this is
	// "true" (its getComponentsData). Used for components that would exceed the
	// Kubernetes ConfigMap size limit uncompressed (the CAPI core components).
	CompressedAnnotation = "provider.cluster.x-k8s.io/compressed"
	compressedValue      = "true"

	// Operator provider-type label values, by provider role.
	CoreProviderType           = "core"
	BootstrapProviderType      = "bootstrap"
	ControlPlaneProviderType   = "control-plane"
	InfrastructureProviderType = "infrastructure"
)

// maxConfigMapBytes is the Kubernetes ConfigMap size limit (1 MiB). Components
// (plus metadata) over it are gzip-compressed into binaryData, mirroring the
// operator's own needToCompress threshold.
const maxConfigMapBytes = 1024 * 1024

// CAPIOptions configure the Cluster API providers the operator installs on
// the new cluster.
type CAPIOptions struct {
	// Version is the core, bootstrap and control plane provider version.
	Version string
	// FetchURL, when set, is the release directory the core, bootstrap and
	// control plane components are fetched from
	// (https://github.com/<owner>/cluster-api/releases/<version>/); the
	// component file name is appended. The in-cluster operator fetches it
	// directly, so it must be an https or oci URL; a plain-http URL is
	// rejected. To serve the CAPI components over http (a local mirror), set
	// Core/Bootstrap/ControlPlaneComponentsData instead and klotho embeds them
	// in ConfigMaps.
	FetchURL string
	// FeatureGates are set on the core provider's manager.
	FeatureGates map[string]bool

	// CoreComponentsData, BootstrapComponentsData and ControlPlaneComponentsData
	// are the rendered core-, bootstrap- and control-plane-components.yaml. When
	// set, klotho embeds each in a labeled ConfigMap and points the provider's
	// fetchConfig.selector at it rather than at FetchURL - the way a CAPI mirror
	// served over plain http (which the operator refuses to fetch) reaches the
	// operator. CAPIMetadata is the shared cluster-api metadata.yaml stored next
	// to each; empty synthesizes a minimal one from Version.
	CoreComponentsData         string
	BootstrapComponentsData    string
	ControlPlaneComponentsData string
	CAPIMetadata               string

	InfraProviderName    string
	InfraProviderVersion string
	// InfraFetchURL, when set, is the full URL of the infrastructure
	// provider's components file. The in-cluster operator fetches it directly,
	// so it must be an https or oci URL the operator accepts; a plain-http URL
	// is rejected ("no provider implementation for \"http\" schema"). To serve a
	// local, unpublished provider over http, set InfraComponents instead.
	InfraFetchURL string
	// InfraComponents, when set, is the infrastructure provider's rendered
	// components manifest (its infrastructure-components.yaml). klotho embeds it
	// in a labeled ConfigMap and points the provider's fetchConfig.selector at
	// that ConfigMap rather than at InfraFetchURL. When set, InfraFetchURL is
	// ignored for the infrastructure provider.
	InfraComponents string
	// InfraMetadata is the provider's metadata.yaml, stored next to the
	// components in the ConfigMap; the operator requires both keys. Empty
	// synthesizes a minimal Metadata from InfraProviderVersion.
	InfraMetadata string
	// InfraImage, when set, replaces the provider manager's container image.
	// The operator keeps the image repository it knows for a provider and
	// only follows the version tag, so components fetched from another
	// repository's release (a fork) need their image named here.
	InfraImage string
	// InfraNamespace is where the infrastructure provider runs. Empty means
	// DefaultInfraNamespace(InfraProviderName).
	InfraNamespace string
	// Bootstrap and ControlPlane select the bootstrap and control-plane
	// providers the operator installs. Zero fields default to the kubeadm
	// providers fetched from the core Cluster API release (so a cluster whose
	// control plane is kubeadm needs set neither). A control-plane provider
	// such as k0s sets them to the k0smotron providers, which carry their own
	// name, namespace, version and components URL.
	Bootstrap    ProviderInstall
	ControlPlane ProviderInstall
	// ProviderSecretData becomes the infrastructure provider's configSecret,
	// for example PROXMOX_URL, PROXMOX_TOKEN and PROXMOX_SECRET.
	ProviderSecretData map[string]string
	// Extra are further providers the infrastructure provider depends on
	// (see provider.Dependent); they are installed and waited for like the
	// four standard ones.
	Extra []provider.Dependency
}

// ProviderInstall identifies a bootstrap or control-plane provider for the
// Cluster API operator to install. Zero fields default to the kubeadm provider
// fetched from the core Cluster API release; a provider from another project
// (k0smotron) sets its own name, namespace, version and components URL.
type ProviderInstall struct {
	// Name is the operator provider name (for example "kubeadm" or
	// "k0sproject-k0smotron").
	Name string
	// Namespace is where the provider runs.
	Namespace string
	// Version is the provider version. Empty uses CAPIOptions.Version.
	Version string
	// ComponentsURL is the full URL of the components manifest. Empty uses the
	// core Cluster API release layout (CAPIOptions.FetchURL + the component
	// file name).
	ComponentsURL string
}

// resolved fills a bootstrap/control-plane provider's defaults: the kubeadm
// name and the given namespace, CAPIOptions.Version, and the CAPI-release
// component file under FetchURL when ComponentsURL is unset.
func (o CAPIOptions) resolved(p ProviderInstall, defaultNS, componentFile string) (name, ns, version string, fetch map[string]any) {
	name = p.Name
	if name == "" {
		name = KubeadmProviderName
	}
	ns = p.Namespace
	if ns == "" {
		ns = defaultNS
	}
	version = p.Version
	if version == "" {
		version = o.Version
	}
	if p.ComponentsURL != "" {
		return name, ns, version, map[string]any{"url": p.ComponentsURL}
	}
	return name, ns, version, o.fetch(componentFile)
}

func (o CAPIOptions) bootstrap() (name, ns, version string, fetch map[string]any) {
	return o.resolved(o.Bootstrap, BootstrapNamespace, BootstrapComponents)
}

func (o CAPIOptions) controlPlane() (name, ns, version string, fetch map[string]any) {
	return o.resolved(o.ControlPlane, ControlPlaneNamespace, ControlPlaneComponents)
}

// fetch is the fetchConfig for a core Cluster API release component file, or
// nil when no release directory (FetchURL) is set.
func (o CAPIOptions) fetch(file string) map[string]any {
	if o.FetchURL == "" {
		return nil
	}
	base := o.FetchURL
	if !strings.HasSuffix(base, "/") {
		base += "/"
	}
	return map[string]any{"url": base + file}
}

// DefaultInfraNamespace returns the namespace an infrastructure provider
// conventionally runs in: capmox-system for proxmox, cap<name>-system
// otherwise.
func DefaultInfraNamespace(provider string) string {
	switch provider {
	case "proxmox":
		return "capmox-system"
	case "libvirt":
		return "capi-libvirt-system"
	case "":
		return ""
	}
	return "cap" + strings.ToLower(provider) + "-system"
}

func (o CAPIOptions) infraNamespace() string {
	if o.InfraNamespace != "" {
		return o.InfraNamespace
	}
	return DefaultInfraNamespace(o.InfraProviderName)
}

func (o CAPIOptions) secretName() string { return o.InfraProviderName + "-variables" }

// OperatorProviderRef identifies one operator provider object.
type OperatorProviderRef struct {
	Kind, Namespace, Name string
}

// OperatorProviderRefs lists the operator provider objects klotho installs: the
// core, bootstrap, control-plane and infrastructure providers, plus any extras.
func (o CAPIOptions) OperatorProviderRefs() []OperatorProviderRef {
	bName, bNS, _, _ := o.bootstrap()
	cpName, cpNS, _, _ := o.controlPlane()
	refs := []OperatorProviderRef{
		{KindCoreProvider, CoreNamespace, CoreProviderName},
		{KindBootstrapProvider, bNS, bName},
		{KindControlPlaneProvider, cpNS, cpName},
		{KindInfrastructureProvider, o.infraNamespace(), o.InfraProviderName},
	}
	for _, d := range o.Extra {
		refs = append(refs, OperatorProviderRef{d.Kind, d.Namespace, d.Name})
	}
	return refs
}

// OperatorGVK returns the GroupVersionKind for a Cluster API operator object of
// the given kind.
func OperatorGVK(kind string) schema.GroupVersionKind {
	return schema.GroupVersionKind{Group: OperatorGroup, Version: OperatorVersion, Kind: kind}
}

// ProviderObjects renders the namespaces, the infrastructure provider's
// configuration Secret and the four operator provider objects. With redact,
// the Secret's values are replaced so the result can be written to disk.
func ProviderObjects(o CAPIOptions, redact bool) ([]*unstructured.Unstructured, error) {
	if o.Version == "" {
		return nil, fmt.Errorf("klotho: Cluster API version is required")
	}
	if o.InfraProviderName == "" || o.InfraProviderVersion == "" {
		return nil, fmt.Errorf("klotho: infrastructure provider name and version are required")
	}
	infraNS := o.infraNamespace()
	bName, bNS, bVersion, bFetch := o.bootstrap()
	cpName, cpNS, cpVersion, cpFetch := o.controlPlane()
	var objs []*unstructured.Unstructured
	var namespaces []string
	for _, ns := range []string{CoreNamespace, bNS, cpNS, infraNS} {
		if !slices.Contains(namespaces, ns) {
			namespaces = append(namespaces, ns)
		}
	}
	for _, d := range o.Extra {
		if d.Kind == "" || d.Name == "" || d.Namespace == "" || d.Version == "" {
			return nil, fmt.Errorf("klotho: provider dependency %+v needs kind, name, namespace and version", d)
		}
		if !slices.Contains(namespaces, d.Namespace) {
			namespaces = append(namespaces, d.Namespace)
		}
	}
	for _, ns := range namespaces {
		objs = append(objs, &unstructured.Unstructured{Object: map[string]any{
			"apiVersion": "v1", "kind": "Namespace",
			"metadata": map[string]any{"name": ns},
		}})
	}
	if len(o.ProviderSecretData) > 0 {
		data := map[string]any{}
		keys := make([]string, 0, len(o.ProviderSecretData))
		for k := range o.ProviderSecretData {
			keys = append(keys, k)
		}
		sort.Strings(keys)
		for _, k := range keys {
			v := o.ProviderSecretData[k]
			if redact {
				v = "REDACTED"
			}
			data[k] = v
		}
		objs = append(objs, &unstructured.Unstructured{Object: map[string]any{
			"apiVersion": "v1", "kind": "Secret",
			"metadata":   map[string]any{"name": o.secretName(), "namespace": infraNS},
			"type":       "Opaque",
			"stringData": data,
		}})
	}

	provider := func(kind, ns, name, version string, fetchConfig map[string]any) *unstructured.Unstructured {
		// The management stack runs on the bootstrap node, which kubeadm
		// taints as a control plane; a provider whose own manifest lacks
		// the toleration (CAPMOX does) would never schedule on a cluster
		// with no workers. The node's taints are left as the user's
		// configuration made them. The node selector keeps the providers
		// on control planes once workers exist: deleting a self-managed
		// cluster removes workers before control planes, so a provider on
		// a worker would have to be moved first, and an ordered deletion
		// that waits for the control planes while Cluster API waits for
		// the workers never finishes.
		spec := map[string]any{
			"version": version,
			"deployment": map[string]any{
				"tolerations":  controlPlaneTolerations(),
				"nodeSelector": map[string]any{"node-role.kubernetes.io/control-plane": ""},
			},
		}
		if fetchConfig != nil {
			spec["fetchConfig"] = fetchConfig
		}
		u := &unstructured.Unstructured{Object: map[string]any{"spec": spec}}
		u.SetGroupVersionKind(OperatorGVK(kind))
		u.SetNamespace(ns)
		u.SetName(name)
		return u
	}

	// embedFetch returns the fetchConfig for a provider: a selector pointing at
	// an embedded-components ConfigMap (appended to objs) when components are
	// supplied, else urlFetch for the operator to fetch directly. This is the
	// provider-agnostic path for a provider served only over plain http.
	//
	// TODO(dedup): the Hyper-V provider branch (feat/hyperv-provider) carries an
	// equivalent, infrastructure-only copy of this ConfigMap-embedding
	// mechanism (componentsConfigMap/syntheticMetadata here and
	// embedLocalComponents in pkg/cli/create.go). Once both the Hyper-V and
	// libvirt branches merge, de-duplicate to this single provider-agnostic
	// implementation (which also covers the core/bootstrap/control-plane
	// providers).
	embedFetch := func(name, ptype, version, ns, components, meta string, urlFetch map[string]any) map[string]any {
		if components == "" {
			return urlFetch
		}
		objs = append(objs, componentsConfigMap(name, ptype, version, ns, components, meta))
		return map[string]any{"selector": map[string]any{"matchLabels": map[string]any{
			ProviderNameLabel: name,
			ProviderTypeLabel: ptype,
		}}}
	}

	core := provider(KindCoreProvider, CoreNamespace, CoreProviderName, o.Version,
		embedFetch(CoreProviderName, CoreProviderType, o.Version, CoreNamespace, o.CoreComponentsData, o.CAPIMetadata, o.fetch(CoreComponents)))
	if len(o.FeatureGates) > 0 {
		gates := map[string]any{}
		for k, v := range o.FeatureGates {
			gates[k] = v
		}
		core.Object["spec"].(map[string]any)["manager"] = map[string]any{"featureGates": gates}
	}
	objs = append(objs,
		core,
		provider(KindBootstrapProvider, bNS, bName, bVersion,
			embedFetch(bName, BootstrapProviderType, bVersion, bNS, o.BootstrapComponentsData, o.CAPIMetadata, bFetch)),
		provider(KindControlPlaneProvider, cpNS, cpName, cpVersion,
			embedFetch(cpName, ControlPlaneProviderType, cpVersion, cpNS, o.ControlPlaneComponentsData, o.CAPIMetadata, cpFetch)),
	)
	var infraURLFetch map[string]any
	if o.InfraFetchURL != "" {
		infraURLFetch = map[string]any{"url": o.InfraFetchURL}
	}
	infraFetch := embedFetch(o.InfraProviderName, InfrastructureProviderType, o.InfraProviderVersion, infraNS, o.InfraComponents, o.InfraMetadata, infraURLFetch)
	infra := provider(KindInfrastructureProvider, infraNS, o.InfraProviderName, o.InfraProviderVersion, infraFetch)
	if o.InfraImage != "" {
		infra.Object["spec"].(map[string]any)["deployment"].(map[string]any)["containers"] = []any{
			map[string]any{"name": "manager", "imageUrl": o.InfraImage},
		}
	}
	if len(o.ProviderSecretData) > 0 {
		infra.Object["spec"].(map[string]any)["configSecret"] = map[string]any{
			"name": o.secretName(), "namespace": infraNS,
		}
	}
	objs = append(objs, infra)
	for _, d := range o.Extra {
		objs = append(objs, provider(d.Kind, d.Namespace, d.Name, d.Version, nil))
	}
	return objs, nil
}

// componentsConfigMap builds the ConfigMap the Cluster API operator reads a
// provider's components and metadata from when the provider is served through a
// fetchConfig.selector rather than a URL. Its labels carry the provider's name,
// type and version so the selector matches it and the operator's version check
// (the version label must equal spec.version) passes. The ConfigMap must live
// in the provider's namespace, where the operator looks for it.
func componentsConfigMap(name, ptype, version, ns, components, meta string) *unstructured.Unstructured {
	if meta == "" {
		meta = syntheticMetadata(version)
	}
	metadata := map[string]any{
		"name":      name + "-" + ptype + "-components-" + version,
		"namespace": ns,
		"labels": map[string]any{
			ProviderNameLabel:    name,
			ProviderTypeLabel:    ptype,
			ProviderVersionLabel: version,
		},
	}
	obj := map[string]any{
		"apiVersion": "v1", "kind": "ConfigMap",
		"metadata": metadata,
		// metadata.yaml is small and always stored plaintext, even when the
		// components are compressed (the operator never decompresses it).
		"data": map[string]any{"metadata": meta},
	}
	// Components over the ConfigMap size limit are gzip-compressed into
	// binaryData, with the compressed annotation the operator keys on; smaller
	// ones stay plaintext in data. The unstructured value is the base64 of the
	// gzip bytes, which is how a []byte binaryData value serializes to the API.
	if len(meta)+len(components) > maxConfigMapBytes {
		metadata["annotations"] = map[string]any{CompressedAnnotation: compressedValue}
		obj["binaryData"] = map[string]any{"components": base64.StdEncoding.EncodeToString(gzipBytes([]byte(components)))}
	} else {
		obj["data"].(map[string]any)["components"] = components
	}
	return &unstructured.Unstructured{Object: obj}
}

// gzipBytes returns b gzip-compressed.
func gzipBytes(b []byte) []byte {
	var buf bytes.Buffer
	w := gzip.NewWriter(&buf)
	_, _ = w.Write(b)
	_ = w.Close()
	return buf.Bytes()
}

// syntheticMetadata returns a minimal clusterctl Metadata for version, used when
// the provider's own metadata.yaml is not supplied. The release series is taken
// from the version's major and minor; the contract is the Cluster API contract
// klotho targets (v1beta2). Prefer supplying the real metadata.yaml: the
// operator rejects a provider whose declared contract does not match the core's.
func syntheticMetadata(version string) string {
	major, minor := 0, 0
	if parts := strings.SplitN(strings.TrimPrefix(version, "v"), ".", 3); len(parts) >= 2 {
		major, _ = strconv.Atoi(parts[0])
		minor, _ = strconv.Atoi(parts[1])
	}
	return fmt.Sprintf(`apiVersion: clusterctl.cluster.x-k8s.io/v1alpha3
kind: Metadata
releaseSeries:
- major: %d
  minor: %d
  contract: v1beta2
`, major, minor)
}

// RenderYAML renders objects as a multi-document manifest.
func RenderYAML(objs []*unstructured.Unstructured) ([]byte, error) {
	b := &Bundle{Objects: objs}
	return b.Render()
}

// ProviderReady reports whether an operator provider object has a Ready
// condition with status True, and otherwise a short reason.
func ProviderReady(u *unstructured.Unstructured) (bool, string) {
	return conditionTrue(u, "Ready")
}

func conditionTrue(u *unstructured.Unstructured, condType string) (bool, string) {
	conds, _, _ := unstructured.NestedSlice(u.Object, "status", "conditions")
	for _, c := range conds {
		m, ok := c.(map[string]any)
		if !ok || m["type"] != condType {
			continue
		}
		if m["status"] == "True" {
			return true, ""
		}
		msg, _ := m["message"].(string)
		reason, _ := m["reason"].(string)
		return false, strings.TrimSpace(reason + " " + msg)
	}
	return false, "no " + condType + " condition yet"
}

// controlPlaneTolerations lets a management component run on control plane
// nodes, as the Cluster API core components already do by default.
func controlPlaneTolerations() []any {
	return []any{
		map[string]any{"key": "node-role.kubernetes.io/control-plane", "operator": "Exists", "effect": "NoSchedule"},
		map[string]any{"key": "node-role.kubernetes.io/master", "operator": "Exists", "effect": "NoSchedule"},
	}
}
