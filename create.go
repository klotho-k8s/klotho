package klotho

import (
	"archive/tar"
	"bytes"
	"compress/gzip"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"time"

	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"

	"github.com/klotho-k8s/klotho/pkg/autoscaler"
	"github.com/klotho-k8s/klotho/pkg/cloudinit"
	"github.com/klotho-k8s/klotho/pkg/cluster"
	"github.com/klotho-k8s/klotho/pkg/generate"
	"github.com/klotho-k8s/klotho/pkg/hooks"
	"github.com/klotho-k8s/klotho/pkg/kube"
	"github.com/klotho-k8s/klotho/pkg/state"
	"github.com/klotho-k8s/klotho/provider"
	"github.com/klotho-k8s/klotho/provider/controlplane"
	"github.com/klotho-k8s/klotho/provider/infrastructure"
	"github.com/klotho-k8s/klotho/provider/payload"
)

// ControlPlaneGroup is the API group of control plane objects.
const ControlPlaneGroup = generate.ControlPlaneGroup

// Default timeouts for the waits Create performs against the new cluster.
const (
	DefaultAPIServerTimeout = 15 * time.Minute
	DefaultProvidersTimeout = 10 * time.Minute
	DefaultApplyTimeout     = 10 * time.Minute
	DefaultNodeTimeout      = 10 * time.Minute
)

// Timeouts bound the waits Create performs once the bootstrap node runs.
// Zero fields use the defaults.
type Timeouts struct {
	// APIServer bounds the wait for /readyz through the endpoint.
	APIServer time.Duration
	// Providers bounds the wait for the operator to report the Cluster API
	// providers ready, and for the node to seed the cluster Secrets.
	Providers time.Duration
	// Apply bounds the retries of each apply step (CRDs and webhooks may
	// lag behind their deployments).
	Apply time.Duration
	// Node bounds the wait for the adopted Machine to get a nodeRef.
	Node time.Duration
}

func (t Timeouts) withDefaults() Timeouts {
	if t.APIServer <= 0 {
		t.APIServer = DefaultAPIServerTimeout
	}
	if t.Providers <= 0 {
		t.Providers = DefaultProvidersTimeout
	}
	if t.Apply <= 0 {
		t.Apply = DefaultApplyTimeout
	}
	if t.Node <= 0 {
		t.Node = DefaultNodeTimeout
	}
	return t
}

// CreateRequest describes a cluster to create.
type CreateRequest struct {
	// Spec names the cluster; Spec.Variables are the user's explicit
	// template variables and win over everything else. Provider and
	// ControlPlane are filled from Deps.
	Spec cluster.Spec
	// Provider pins the infrastructure provider release whose cluster
	// template is generated.
	Provider             generate.ProviderRef
	Kubernetes           string
	ControlPlaneReplicas int
	WorkerReplicas       int
	// Manifest, when set, is a complete cluster bundle used instead of a
	// generated one. It is validated and applied unmodified.
	Manifest []byte
	// Pool, when set, is put on every infrastructure MachineTemplate of a
	// generated bundle.
	Pool string
	// PodCIDRs and ServiceCIDRs, when set, replace the cluster network of a
	// generated bundle (the Cluster object's clusterNetwork). They are the
	// user's explicit choice, so they are applied, not merely checked.
	PodCIDRs     []string
	ServiceCIDRs []string
	// MachineTemplateTags, when set, make every infrastructure
	// MachineTemplate of a generated bundle select its VM template by these
	// tags (with MachineTemplateMatchPolicy: exact, uniqueSubset or
	// bestSubset; empty means exact) instead of a fixed template ID and
	// source node. The infrastructure provider's own bootstrap clone honours
	// the same tags.
	MachineTemplateTags        []string
	MachineTemplateMatchPolicy string
	// InstallKubernetes adds the install-at-boot script to the control plane
	// and every worker bootstrap template of a generated bundle, so plain
	// images get containerd and the bundle's Kubernetes version before
	// kubeadm runs. InstallOptions point it at local caches.
	InstallKubernetes bool
	InstallOptions    generate.InstallKubernetesOptions
	// Autoscaler installs the Cluster Autoscaler add-on (the clusterapi cloud
	// provider) into a generated bundle and stamps the node-group min/max
	// annotations on every worker MachineDeployment. AutoscalerOptions tune the
	// sizes, version and image; ClusterName, MachineNamespace and
	// KubernetesVersion are filled from the request.
	Autoscaler        bool
	AutoscalerOptions autoscaler.Options
	// DryRunDir, when set, renders everything into this directory and
	// returns without launching anything.
	DryRunDir string
	// Force creates the cluster even when a provider's Check reports
	// findings about the bundle. Findings are logged either way; the bundle
	// is never changed to resolve them. A dry run is never blocked by them.
	Force bool
	CAPI  generate.CAPIOptions
	// DefaultVariables are the lowest-precedence template variables (from
	// configuration). They are also visible to the infrastructure provider's
	// Prepare, below the user's.
	DefaultVariables map[string]string
	// SSHAuthorizedKeys are installed for the bootstrap node's default user.
	SSHAuthorizedKeys []string
	Timeouts          Timeouts
	// Hooks are the consumer's lifecycle hooks. Create fires the postPivot
	// hooks (in order) once the cluster is self-managed and healthy. Hooks for
	// other events are ignored here.
	Hooks []hooks.Hook
	// HookExec runs a single hook. Nil means the built-in engine
	// (hooks.NewEngine). It exists for tests and alternate runners.
	HookExec hooks.Executor
}

// Optional provider conventions, checked by type assertion so the library
// stays provider-agnostic.
type (
	// Tagger is implemented by infrastructure providers whose resources
	// carry a per-cluster tag. Without it the tag is "klotho-<name>".
	Tagger interface{ Tag(cluster string) string }
	// UntaggedMachineTemplate is implemented by an infrastructure provider
	// whose MachineTemplate CRD has no tag field, so klotho must not add
	// spec.template.spec.tags to the generated bundle: a strict CRD schema
	// (the Hyper-V provider's HyperVMachineTemplate) rejects the unknown field
	// on apply, and the adopted machine would carry it too. Such a provider
	// correlates its VMs for teardown by name instead of by template tag.
	UntaggedMachineTemplate interface{ UntaggedMachineTemplate() bool }
	// BootstrapNamer is implemented by control plane providers that name
	// the bootstrap machine. Without it the name is "<name>-bootstrap".
	BootstrapNamer interface {
		BootstrapMachineName(cluster string) string
	}
	// MachineTemplateTagger is implemented by infrastructure providers whose
	// MachineTemplate does not carry klotho's ownership tag in the default
	// spec.template.spec.tags string list (CAPA's AWSMachineTemplate uses a
	// map-valued additionalTags field instead, and its strict decoder rejects
	// an unknown "tags" field). klotho calls it to record the tag in the
	// provider's own schema in place of the default.
	MachineTemplateTagger interface {
		TagMachineTemplates(objs []*unstructured.Unstructured, tag string) error
	}
)

func tagFor(infra infrastructure.Provider, name string) string {
	if t, ok := infra.(Tagger); ok {
		return t.Tag(name)
	}
	return "klotho-" + name
}

func bootstrapName(cp controlplane.Provider, name string) string {
	if n, ok := cp.(BootstrapNamer); ok {
		return n.BootstrapMachineName(name)
	}
	return name + "-bootstrap"
}

// Dry-run output file names.
const (
	DryRunBundle       = "bundle.yaml"
	DryRunProviders    = "providers.yaml"
	DryRunState        = "state-plain.json"
	DryRunAdoptControl = "adopt-controlplane.yaml"
)

// Create provisions a cluster: it reserves the name, prepares the
// infrastructure, builds the cluster bundle and the bootstrap node's
// user-data, launches the node, persists state, and then installs Cluster
// API on the node and makes it adopt itself (see converge for the exact
// sequence against the new cluster).
//
// Failure semantics: before Prepare, or after a Prepare that created
// nothing, the reservation is released. After a Prepare that created
// resources, or after Launch, a state record is kept so "delete" can clean
// up. Once Launch succeeds the full state is persisted before any waiting,
// and later failures leave it in place.
func Create(ctx context.Context, deps Deps, req CreateRequest) (_ *cluster.State, err error) {
	if err := deps.check(true); err != nil {
		return nil, err
	}
	name := req.Spec.Name
	if err := ValidateName(name); err != nil {
		return nil, err
	}
	log := deps.log().With("cluster", name)

	spec := req.Spec
	spec.Provider = deps.Infra.Name()
	spec.ControlPlane = deps.ControlPlane.Name()
	userVars := copyMap(req.Spec.Variables)
	spec.Variables = mergeMaps(req.DefaultVariables, userVars)

	log.Info("reserving name", "backend", deps.Store.Name())
	ok, err := deps.Store.Reserve(ctx, name)
	if err != nil {
		return nil, fmt.Errorf("reserving %s: %w", name, err)
	}
	if !ok {
		return nil, fmt.Errorf("cluster %s already exists", name)
	}

	g := &guard{deps: deps, name: name, log: log, snapshot: &cluster.State{Spec: spec, CreatedAt: deps.now()}}
	defer g.finish(ctx, &err)

	log.Info("preparing infrastructure", "provider", spec.Provider)
	prepared, err := deps.Infra.Prepare(ctx, &spec)
	if err != nil {
		return nil, fmt.Errorf("preparing infrastructure: %w", err)
	}
	g.prepared(prepared)
	log.Info("endpoint", "host", prepared.Endpoint.Host, "port", prepared.Endpoint.Port)
	clientEP := prepared.Endpoint
	if prepared.ClientEndpoint.Host != "" {
		clientEP = prepared.ClientEndpoint
		log.Info("client endpoint (host-reachable)", "host", clientEP.Host, "port", clientEP.Port)
	}

	tag := tagFor(deps.Infra, name)
	bundle, err := buildBundle(ctx, deps, req, spec, userVars, prepared, tag)
	if err != nil {
		return nil, err
	}

	if err := checkBundle(deps, bundle, req.Force || req.DryRunDir != "", log); err != nil {
		return nil, err
	}

	kcp, err := generate.FindOne(bundle, ControlPlaneGroup, deps.ControlPlane.Kind())
	if err != nil {
		return nil, fmt.Errorf("bundle: %w", err)
	}
	if kcp.GetKind() != deps.ControlPlane.Kind() {
		return nil, fmt.Errorf("bundle: control plane object is %s, want %s", kcp.GetKind(), deps.ControlPlane.Kind())
	}

	log.Info("minting cluster certificates", "controlPlane", spec.ControlPlane)
	creds, err := deps.ControlPlane.Prepare(ctx, &spec, prepared.Endpoint)
	if err != nil {
		return nil, fmt.Errorf("preparing control plane: %w", err)
	}
	st := &cluster.State{
		Spec:           spec,
		Endpoint:       prepared.Endpoint,
		ClientEndpoint: prepared.ClientEndpoint,
		CA:             creds.CA,
		CreatedAt:      g.snapshot.CreatedAt,
		Resources:      append([]cluster.Resource(nil), prepared.Created...),
	}
	g.snapshot = st

	clusterObj, err := generate.FindOne(bundle, generate.ClusterGroup, "Cluster")
	if err != nil {
		return nil, fmt.Errorf("bundle: %w", err)
	}
	cpContrib, err := deps.ControlPlane.UserData(ctx, &spec, creds, kcp, clusterObj)
	if err != nil {
		return nil, fmt.Errorf("rendering control plane user-data: %w", err)
	}
	// The orchestrator contributes nothing to the node: the Cluster API
	// providers, the bundle and the adoption objects are applied from here
	// once the API server answers, so the list holds only non-empty
	// contributions.
	contribs := []controlplane.Contribution{*cpContrib}
	ciOpts := cloudinit.Options{
		Hostname:          bootstrapName(deps.ControlPlane, name),
		SSHAuthorizedKeys: req.SSHAuthorizedKeys,
		PackageUpdate:     false,
	}
	ci, err := cloudinit.Build(contribs, ciOpts)
	if err != nil {
		return nil, fmt.Errorf("building user-data: %w", err)
	}
	// Offload the bootstrap files to the payload host when the provider's
	// user-data channel is too small for them. Providers whose channel has no
	// practical cap (Proxmox's NoCloud ISO, for example) report no limit and
	// keep everything inline. A dry run inspects the full inlined user-data and
	// publishes nothing, so it is skipped there.
	if req.DryRunDir == "" {
		if ci, err = offloadUserData(ctx, deps, name, contribs, ci, ciOpts, log); err != nil {
			return nil, err
		}
	}

	if d, ok := deps.Infra.(provider.Dependent); ok {
		req.CAPI.Extra = append(req.CAPI.Extra, d.Dependencies()...)
	}
	providerObjs, err := generate.ProviderObjects(req.CAPI, false)
	if err != nil {
		return nil, err
	}

	if req.DryRunDir != "" {
		if err := writeDryRun(ctx, deps, req, st, ci, bundle, kcp); err != nil {
			return nil, err
		}
		g.releaseOnSuccess = true
		log.Info("dry run written", "dir", req.DryRunDir)
		return st, nil
	}

	log.Info("launching bootstrap node")
	node, err := deps.Infra.Launch(ctx, &spec, ci.UserData)
	if err != nil {
		return nil, fmt.Errorf("launching bootstrap node: %w", err)
	}
	st.Bootstrap = node
	g.launched = true

	// A provider with no load balancer (Hyper-V) cannot know the control plane
	// endpoint until the node boots and claims a DHCP address. Now that it has,
	// retarget the endpoint and the admin kubeconfig at the node so converge
	// reaches the API server kubeadm brings up; the node's own IP is in the
	// apiserver certificate SANs, so TLS to it verifies.
	if ea, ok := deps.Infra.(infrastructure.EndpointFromBootstrap); ok {
		if host := ea.EndpointFromBootstrap(node); host != "" && host != st.Endpoint.Host {
			log.Info("retargeting the control plane endpoint at the bootstrap node", "from", st.Endpoint.Host, "to", host)
			st.Endpoint.Host = host
			kc, err := kube.RetargetServer(creds.Kubeconfig, st.Endpoint.Host, st.Endpoint.Port)
			if err != nil {
				return nil, fmt.Errorf("retargeting admin kubeconfig at the bootstrap node: %w", err)
			}
			creds.Kubeconfig = kc
			// The bundle was authored with the placeholder endpoint. Converge
			// applies it to the self-hosted cluster, where the Cluster API
			// controllers reach the workload API through the kubeconfig the
			// control plane provider mints from the infrastructure cluster's
			// controlPlaneEndpoint. Leave it at the placeholder and that
			// kubeconfig points at an address the node never has, so the machine
			// controller can never set the bootstrap Machine's nodeRef and the
			// pivot stalls. Retarget the bundle's endpoint at the node too.
			if err := generate.SetControlPlaneEndpoint(bundle, st.Endpoint); err != nil {
				return nil, fmt.Errorf("retargeting bundle control plane endpoint at the bootstrap node: %w", err)
			}
		}
	}

	rec, err := deps.Codec.Seal(st)
	if err != nil {
		return nil, fmt.Errorf("sealing state: %w", err)
	}
	if err := deps.Store.Save(ctx, name, rec); err != nil {
		return nil, fmt.Errorf("saving state: %w", err)
	}
	g.committed = true
	log.Info("bootstrap node running; state saved", "node", node.Name, "id", node.ID, "addresses", node.Addresses)

	if err := converge(ctx, deps, req, st, creds, bundle, kcp, providerObjs); err != nil {
		return st, fmt.Errorf("%w (the state is saved and the node is running; \"klotho delete %s\" removes everything)", err, name)
	}
	log.Info("cluster ready", "endpoint", fmt.Sprintf("%s:%d", st.Endpoint.Host, st.Endpoint.Port))

	// The cluster is self-managed and healthy: fire the consumer's postPivot
	// hooks. The state is already saved and the cluster is up, so a failing
	// hook (policy fail) does not tear anything down; it is returned so the
	// caller sees the cluster came up but the hook did not succeed.
	if hooks.Any(req.Hooks, hooks.PostPivot) {
		target, err := hookTarget(st)
		if err != nil {
			return st, err
		}
		if err := hooks.Run(ctx, log, deps.hookExec(req.HookExec), hooks.PostPivot, target, req.Hooks); err != nil {
			return st, fmt.Errorf("%w (the cluster is up and self-managed; \"klotho delete %s\" removes it)", err, name)
		}
	}
	return st, nil
}

// PayloadArchivePath is where the bootstrap node downloads the offloaded files
// archive before extracting it.
const PayloadArchivePath = "/run/klotho/payload.tar.gz"

// offloadUserData shrinks the bootstrap node's user-data below the
// infrastructure provider's channel cap by moving the contributed files into a
// single gzip-compressed tar archive published to the payload host. The node
// downloads the archive through one presigned URL and extracts it, so the
// user-data carries one fetch command instead of every file, and no credential.
// It returns ci unchanged when the provider reports no cap or the user-data
// already fits. A capped provider over the limit with no payload host
// configured is an error naming what to set.
//
// One archive, not one fetch per file: a presigned URL is long (~2 KB), so a
// fetch per file would merely trade the file content for a comparable volume of
// URLs. Bundling them is what actually fits a many-file payload (the cluster's
// CAs, the admin kubeconfig and the install scripts) into AWS's 16 KB cap.
func offloadUserData(ctx context.Context, deps Deps, name string, contribs []controlplane.Contribution,
	ci *cloudinit.Bundle, ciOpts cloudinit.Options, log *slog.Logger) (*cloudinit.Bundle, error) {
	limiter, ok := deps.Infra.(infrastructure.UserDataLimiter)
	if !ok {
		return ci, nil
	}
	limit := limiter.UserDataLimit()
	if limit <= 0 || len(ci.UserData) <= limit {
		return ci, nil
	}
	if deps.Payload == nil {
		return nil, fmt.Errorf("the bootstrap user-data is %d bytes but provider %q caps it at %d; configure a payload host (set payload.kind: s3) so the large files are fetched at boot",
			len(ci.UserData), deps.Infra.Name(), limit)
	}

	// Appended files have no single authoritative content to archive; they
	// stay inline (they are small configuration fragments).
	var omit []string
	archive, count, err := archiveFiles(contribs, &omit)
	if err != nil {
		return nil, err
	}
	if count == 0 {
		return nil, fmt.Errorf("the bootstrap user-data is %d bytes, over provider %q's %d-byte limit, but has no files to offload",
			len(ci.UserData), deps.Infra.Name(), limit)
	}
	log.Info("user-data exceeds the provider's limit; offloading files to the payload host",
		"bytes", len(ci.UserData), "limit", limit, "host", deps.Payload.Name(), "files", count, "archiveBytes", len(archive))

	fetches, err := deps.Payload.Publish(ctx, name, []payload.File{{Path: PayloadArchivePath, Content: archive, Mode: "0600"}})
	if err != nil {
		return nil, fmt.Errorf("publishing the bootstrap payload archive: %w", err)
	}

	// Extract the archive to / (its entries are absolute paths with their
	// original modes) after it is fetched and before any command uses the
	// files, then remove it. A prepended contribution's PreCommands run after
	// the fetch and before every other contribution's commands.
	extract := controlplane.Contribution{PreCommands: []string{
		"tar -xpzf " + PayloadArchivePath + " -C / && rm -f " + PayloadArchivePath,
	}}

	ciOpts.Fetches = fetches
	ciOpts.Omit = omit
	ci, err = cloudinit.Build(append([]controlplane.Contribution{extract}, contribs...), ciOpts)
	if err != nil {
		return nil, fmt.Errorf("rebuilding user-data with the payload archive: %w", err)
	}
	if len(ci.UserData) > limit {
		return nil, fmt.Errorf("the bootstrap user-data is still %d bytes after offloading %d files, over the %d-byte limit of provider %q",
			len(ci.UserData), count, limit, deps.Infra.Name())
	}
	log.Info("user-data offloaded", "bytes", len(ci.UserData), "files", count)
	return ci, nil
}

// archiveFiles builds a gzip-compressed tar of every non-appended contributed
// file, preserving each file's absolute path and mode, and appends those paths
// to omit so they are dropped from the inline write_files. It returns the
// archive and the number of files in it.
func archiveFiles(contribs []controlplane.Contribution, omit *[]string) ([]byte, int, error) {
	var buf bytes.Buffer
	gz := gzip.NewWriter(&buf)
	tw := tar.NewWriter(gz)
	count := 0
	for _, c := range contribs {
		for _, f := range c.Files {
			if f.Append {
				continue
			}
			mode := parseFileMode(f.Mode)
			if err := tw.WriteHeader(&tar.Header{
				Name: strings.TrimPrefix(f.Path, "/"),
				Mode: int64(mode),
				Size: int64(len(f.Content)),
			}); err != nil {
				return nil, 0, fmt.Errorf("archiving %s: %w", f.Path, err)
			}
			if _, err := tw.Write(f.Content); err != nil {
				return nil, 0, fmt.Errorf("archiving %s: %w", f.Path, err)
			}
			*omit = append(*omit, f.Path)
			count++
		}
	}
	if err := tw.Close(); err != nil {
		return nil, 0, fmt.Errorf("closing payload archive: %w", err)
	}
	if err := gz.Close(); err != nil {
		return nil, 0, fmt.Errorf("compressing payload archive: %w", err)
	}
	return buf.Bytes(), count, nil
}

// parseFileMode reads an octal mode string ("0600") into a permission value,
// defaulting to 0644 when it is empty or unparseable.
func parseFileMode(s string) uint32 {
	if s == "" {
		return 0o644
	}
	m, err := strconv.ParseUint(s, 8, 32)
	if err != nil {
		return 0o644
	}
	return uint32(m)
}

// buildBundle returns the user's manifest, validated and untouched, or a
// generated bundle patched with the endpoint, tag and pool, and with the
// explicit choices in req (cluster network, template selector, install
// script).
func buildBundle(ctx context.Context, deps Deps, req CreateRequest, spec cluster.Spec, userVars map[string]string,
	prepared *infrastructure.Prepared, tag string) (*generate.Bundle, error) {
	log := deps.log().With("cluster", spec.Name)
	if len(req.Manifest) > 0 {
		b, err := generate.ParseBundle(req.Manifest)
		if err != nil {
			return nil, fmt.Errorf("manifest: %w", err)
		}
		if err := generate.Validate(b, spec.Name, tag); err != nil {
			return nil, fmt.Errorf("manifest: %w", err)
		}
		log.Info("using supplied manifest", "objects", len(b.Objects))
		return b, nil
	}
	gen := deps.Generate
	if gen == nil {
		gen = generate.Generate
	}
	gspec := spec
	gspec.Variables = nil
	log.Info("generating cluster bundle", "provider", req.Provider.Name, "version", req.Provider.Version, "flavor", spec.Flavor)
	b, err := gen(ctx, generate.Request{
		Spec:                 gspec,
		Provider:             req.Provider,
		Endpoint:             prepared.Endpoint,
		Kubernetes:           req.Kubernetes,
		ControlPlaneReplicas: req.ControlPlaneReplicas,
		WorkerReplicas:       req.WorkerReplicas,
		Variables: []generate.VariableSource{
			generate.MapSource(userVars),
			generate.MapSource(prepared.Variables),
			generate.MapSource(req.DefaultVariables),
		},
	})
	if err != nil {
		return nil, fmt.Errorf("generating bundle: %w", err)
	}
	if err := generate.SetControlPlaneEndpoint(b, prepared.Endpoint); err != nil {
		return nil, fmt.Errorf("bundle: %w", err)
	}
	if tagger, ok := deps.Infra.(MachineTemplateTagger); ok {
		if err := tagger.TagMachineTemplates(b.Objects, tag); err != nil {
			return nil, fmt.Errorf("bundle: %w", err)
		}
		if _, err := b.Render(); err != nil {
			return nil, fmt.Errorf("bundle: %w", err)
		}
	} else if u, ok := deps.Infra.(UntaggedMachineTemplate); !ok || !u.UntaggedMachineTemplate() {
		if err := generate.AddMachineTemplateTag(b, tag); err != nil {
			return nil, fmt.Errorf("bundle: %w", err)
		}
	}
	if req.Pool != "" {
		if err := generate.SetMachineTemplatePool(b, req.Pool); err != nil {
			return nil, fmt.Errorf("bundle: %w", err)
		}
	}
	if len(req.PodCIDRs) > 0 || len(req.ServiceCIDRs) > 0 {
		if err := generate.SetClusterNetwork(b, req.PodCIDRs, req.ServiceCIDRs); err != nil {
			return nil, fmt.Errorf("bundle: %w", err)
		}
	}
	if len(req.MachineTemplateTags) > 0 {
		log.Info("selecting machine templates by tag", "tags", req.MachineTemplateTags, "policy", req.MachineTemplateMatchPolicy)
		if err := generate.SetMachineTemplateSelector(b, req.MachineTemplateTags, req.MachineTemplateMatchPolicy); err != nil {
			return nil, fmt.Errorf("bundle: %w", err)
		}
	}
	if req.InstallKubernetes {
		if r, ok := deps.Infra.(provider.PackageRequirer); ok {
			req.InstallOptions.Packages = append(req.InstallOptions.Packages, r.RequiredPackages()...)
		}
		log.Info("adding the install-at-boot script", "aptProxy", req.InstallOptions.AptProxy, "registryMirrors", len(req.InstallOptions.RegistryMirrors))
		if err := generate.SetInstallKubernetes(b, req.InstallOptions); err != nil {
			return nil, fmt.Errorf("bundle: %w", err)
		}
	}
	if req.Autoscaler {
		if err := addAutoscaler(b, req, spec, log); err != nil {
			return nil, err
		}
	}
	return b, nil
}

// addAutoscaler renders the Cluster Autoscaler add-on and wires it into the
// bundle: it stamps the node-group min/max annotations on every worker
// MachineDeployment and appends the install objects. It reports advisory
// findings (no worker pool to scale, version skew) through the logger; like the
// rest of create it never edits a user's cluster YAML to resolve one, and the
// add-on is klotho's own objects.
func addAutoscaler(b *generate.Bundle, req CreateRequest, spec cluster.Spec, log *slog.Logger) error {
	opts := req.AutoscalerOptions
	opts.ClusterName = spec.Name
	if opts.KubernetesVersion == "" {
		opts.KubernetesVersion = req.Kubernetes
	}
	install, err := autoscaler.InstallObjects(opts)
	if err != nil {
		return fmt.Errorf("bundle: %w", err)
	}
	od := opts
	min, max := autoscalerSizes(od)
	stamped, err := generate.SetAutoscaler(b, install, autoscaler.MinMaxAnnotations(min, max))
	if err != nil {
		return fmt.Errorf("bundle: %w", err)
	}
	log.Info("adding the cluster-autoscaler add-on", "min", min, "max", max, "machineDeployments", stamped)
	if stamped == 0 {
		log.Warn("autoscaler finding: the bundle has no worker MachineDeployment to scale; the autoscaler installs but has nothing to autoscale until a worker pool exists (create with --worker-count N)",
			"object", "MachineDeployment", "cluster", spec.Name)
	}
	if skew := od.SkewFinding(); skew != "" {
		log.Warn("autoscaler finding: "+skew, "object", "Deployment/"+autoscaler.Name, "field", "spec.template.spec.containers[0].image")
	}
	if min == 0 {
		log.Warn("autoscaler finding: min size 0 enables scale-to-zero, but a pool only grows 0->1 when the autoscaler can read a would-be node's capacity; klotho grants it read access to the infrastructure MachineTemplate, which works only where the provider publishes template capacity - otherwise set the capacity.cluster-autoscaler.kubernetes.io/* annotations by hand (see docs/AUTOSCALER.md)",
			"object", "MachineDeployment", "field", "metadata.annotations")
	}
	return nil
}

// autoscalerSizes resolves the min/max a request's options describe, applying
// the same defaults the add-on module does.
func autoscalerSizes(o autoscaler.Options) (min, max int) {
	min, max = o.MinSize, o.MaxSize
	if max == 0 && min == 0 {
		return autoscaler.DefaultMinSize, autoscaler.DefaultMaxSize
	}
	if max == 0 {
		max = min
	}
	return min, max
}

func writeDryRun(ctx context.Context, deps Deps, req CreateRequest, st *cluster.State, ci *cloudinit.Bundle,
	bundle *generate.Bundle, kcp *unstructured.Unstructured) error {
	dir := req.DryRunDir
	if err := cloudinit.WriteDir(dir, ci); err != nil {
		return err
	}
	write := func(name string, data []byte) error {
		if err := os.WriteFile(filepath.Join(dir, name), data, 0o600); err != nil {
			return fmt.Errorf("dry run: %w", err)
		}
		return nil
	}
	if err := write(DryRunBundle, bundle.YAML); err != nil {
		return err
	}
	// The provider Secret carries a long-lived infrastructure credential;
	// the dry run shows its keys but not its values.
	objs, err := generate.ProviderObjects(req.CAPI, true)
	if err != nil {
		return err
	}
	y, err := generate.RenderYAML(objs)
	if err != nil {
		return err
	}
	if err := write(DryRunProviders, y); err != nil {
		return err
	}
	adopt, err := deps.ControlPlane.Adopt(ctx, st, kcp)
	if err != nil {
		return fmt.Errorf("dry run: rendering control plane adoption: %w", err)
	}
	if err := write(DryRunAdoptControl, adopt); err != nil {
		return err
	}
	rec, err := state.PlainCodec{}.Seal(st)
	if err != nil {
		return err
	}
	doc := bytes.TrimPrefix(rec, []byte(state.PlainMarker))
	var pretty bytes.Buffer
	if err := json.Indent(&pretty, doc, "", "  "); err != nil {
		return fmt.Errorf("dry run: %w", err)
	}
	pretty.WriteByte('\n')
	return write(DryRunState, pretty.Bytes())
}

// guard implements Create's failure semantics in one deferred call.
type guard struct {
	deps Deps
	name string
	log  *slog.Logger
	// snapshot is the state to persist when something was created.
	snapshot *cluster.State
	created  bool
	// launched is set once the bootstrap node exists.
	launched bool
	// committed is set once the full state was saved after Launch.
	committed bool
	// releaseOnSuccess makes a successful run (a dry run) clean up the
	// reservation like a failed one.
	releaseOnSuccess bool
}

func (g *guard) prepared(p *infrastructure.Prepared) {
	g.snapshot.Endpoint = p.Endpoint
	g.snapshot.ClientEndpoint = p.ClientEndpoint
	g.snapshot.Resources = append([]cluster.Resource(nil), p.Created...)
	g.created = len(p.Created) > 0
}

func (g *guard) finish(ctx context.Context, errp *error) {
	if *errp == nil && !g.releaseOnSuccess {
		return
	}
	if g.committed {
		return
	}
	// Clean up even when ctx was cancelled (an interrupt is the usual
	// reason to get here), but not forever.
	cctx, cancel := context.WithTimeout(context.WithoutCancel(ctx), 2*time.Minute)
	defer cancel()

	if g.created || g.launched {
		snap := *g.snapshot
		snap.Reserved = false
		rec, err := g.deps.Codec.Seal(&snap)
		if err == nil {
			err = g.deps.Store.Save(cctx, g.name, rec)
		}
		if err != nil {
			// Keep the reservation: it still blocks the name and tells the
			// user something is left behind.
			*errp = errors.Join(*errp, fmt.Errorf("saving a snapshot of %s for cleanup failed, the reservation is kept: %w", g.name, err))
			return
		}
		g.log.Warn("kept a state snapshot so \"klotho delete\" can remove what was created",
			"resources", len(snap.Resources), "launched", g.launched)
		return
	}
	if err := g.deps.Store.Delete(cctx, g.name); err != nil {
		*errp = errors.Join(*errp, fmt.Errorf("releasing reservation of %s: %w", g.name, err))
		return
	}
	g.log.Debug("released reservation")
}

func copyMap(m map[string]string) map[string]string {
	out := make(map[string]string, len(m))
	for k, v := range m {
		out[k] = v
	}
	return out
}

// mergeMaps returns lower overlaid by higher.
func mergeMaps(lower, higher map[string]string) map[string]string {
	out := copyMap(lower)
	for k, v := range higher {
		out[k] = v
	}
	if len(out) == 0 {
		return nil
	}
	return out
}

// FindingsError is returned by Create when a provider's Check reported
// findings about the bundle and the request did not set Force.
type FindingsError struct {
	Findings []provider.Finding
}

func (e *FindingsError) Error() string {
	var b bytes.Buffer
	fmt.Fprintf(&b, "%d finding(s) about the cluster bundle need a decision:\n", len(e.Findings))
	for _, f := range e.Findings {
		fmt.Fprintf(&b, "  - %s\n", f)
	}
	b.WriteString("edit the manifest, or re-run with --force to create the cluster as it is")
	return b.String()
}

// checkBundle runs every provider that implements provider.Checker over the
// bundle and logs each finding. Unless proceed is set, any finding stops the
// creation. The bundle is never modified.
func checkBundle(deps Deps, b *generate.Bundle, proceed bool, log *slog.Logger) error {
	var findings []provider.Finding
	for _, p := range []any{deps.Infra, deps.ControlPlane} {
		c, ok := p.(provider.Checker)
		if !ok {
			continue
		}
		findings = append(findings, c.Check(b.Objects)...)
	}
	if len(findings) == 0 {
		return nil
	}
	for _, f := range findings {
		log.Warn("bundle finding", "object", f.Object, "field", f.Field, "detail", f.Message)
	}
	if proceed {
		log.Warn("proceeding with the bundle as it is", "findings", len(findings))
		return nil
	}
	return &FindingsError{Findings: findings}
}
