package klotho

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"os"
	"path/filepath"
	"time"

	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"

	"github.com/klotho-k8s/klotho/pkg/cloudinit"
	"github.com/klotho-k8s/klotho/pkg/cluster"
	"github.com/klotho-k8s/klotho/pkg/generate"
	"github.com/klotho-k8s/klotho/pkg/state"
	"github.com/klotho-k8s/klotho/provider"
	"github.com/klotho-k8s/klotho/provider/controlplane"
	"github.com/klotho-k8s/klotho/provider/infrastructure"
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
}

// Optional provider conventions, checked by type assertion so the library
// stays provider-agnostic.
type (
	// Tagger is implemented by infrastructure providers whose resources
	// carry a per-cluster tag. Without it the tag is "klotho-<name>".
	Tagger interface{ Tag(cluster string) string }
	// BootstrapNamer is implemented by control plane providers that name
	// the bootstrap machine. Without it the name is "<name>-bootstrap".
	BootstrapNamer interface {
		BootstrapMachineName(cluster string) string
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
		Spec:      spec,
		Endpoint:  prepared.Endpoint,
		CA:        creds.CA,
		CreatedAt: g.snapshot.CreatedAt,
		Resources: append([]cluster.Resource(nil), prepared.Created...),
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
	ci, err := cloudinit.Build(contribs, cloudinit.Options{
		Hostname:          bootstrapName(deps.ControlPlane, name),
		SSHAuthorizedKeys: req.SSHAuthorizedKeys,
		PackageUpdate:     false,
	})
	if err != nil {
		return nil, fmt.Errorf("building user-data: %w", err)
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
	return st, nil
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
	if err := generate.AddMachineTemplateTag(b, tag); err != nil {
		return nil, fmt.Errorf("bundle: %w", err)
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
	return b, nil
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
