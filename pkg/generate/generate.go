// Package generate fetches an infrastructure provider's cluster template for a
// pinned provider version and flavor, resolves its variables from layered
// sources, renders it, and returns the result as Kubernetes objects plus the
// rendered YAML. No management cluster is ever contacted.
package generate

import (
	"context"
	"fmt"
	"os"
	"sort"
	"strconv"
	"strings"

	"github.com/pkg/errors"
	clusterctlv1 "sigs.k8s.io/cluster-api/cmd/clusterctl/api/v1alpha3"
	"sigs.k8s.io/cluster-api/cmd/clusterctl/client/config"
	"sigs.k8s.io/cluster-api/cmd/clusterctl/client/repository"

	"github.com/klotho-k8s/klotho/pkg/cluster"
)

// DefaultNamespace is used when Spec.Namespace is empty.
const DefaultNamespace = "default"

// Standard template variables populated from Request fields.
const (
	VarClusterName              = "CLUSTER_NAME"
	VarNamespace                = "NAMESPACE"
	VarKubernetesVersion        = "KUBERNETES_VERSION"
	VarControlPlaneMachineCount = "CONTROL_PLANE_MACHINE_COUNT"
	VarWorkerMachineCount       = "WORKER_MACHINE_COUNT"
	VarControlPlaneEndpointIP   = "CONTROL_PLANE_ENDPOINT_IP"
	VarControlPlaneEndpointPort = "CONTROL_PLANE_ENDPOINT_PORT"
)

// Request describes the template to fetch and how to fill it in.
type Request struct {
	// Spec supplies Name, Namespace (default "default") and Flavor (empty
	// means the provider's default template). Spec.Variables, when set, is
	// consulted after Variables and before the values derived from Request.
	Spec cluster.Spec
	// Provider pins the infrastructure provider and its version.
	Provider ProviderRef
	// Endpoint is the control plane endpoint, already known.
	Endpoint cluster.Endpoint
	// Kubernetes is the Kubernetes version, e.g. "v1.33.4".
	Kubernetes string
	// ControlPlaneReplicas is used when greater than zero.
	ControlPlaneReplicas int
	// WorkerReplicas is used when zero or greater.
	WorkerReplicas int
	// Variables are consulted in order; the first source that has a value wins.
	Variables []VariableSource
}

// ProviderRef identifies an infrastructure provider release.
type ProviderRef struct {
	// Name is the provider name as clusterctl knows it, e.g. "proxmox".
	Name string
	// Version is the release tag, e.g. "v0.9.2". Required.
	Version string
	// URL optionally overrides the provider's components URL, in the form
	// https://github.com/<owner>/<repo>/releases/<version>/infrastructure-components.yaml.
	// Templates are fetched from the same release directory.
	URL string
}

// VariableSource supplies template variable values.
type VariableSource interface {
	Get(name string) (string, bool)
}

type mapSource map[string]string

func (m mapSource) Get(name string) (string, bool) {
	v, ok := m[name]
	return v, ok
}

// MapSource returns a VariableSource backed by m.
func MapSource(m map[string]string) VariableSource {
	return mapSource(m)
}

type envSource string

func (p envSource) Get(name string) (string, bool) {
	return os.LookupEnv(string(p) + name)
}

// EnvSource returns a VariableSource that reads the environment variable
// prefix+NAME. A variable set to the empty string counts as set.
func EnvSource(prefix string) VariableSource {
	return envSource(prefix)
}

// Variable describes one template variable.
type Variable struct {
	Name string
	// Default is the template's own default (${NAME:=default}), or nil.
	Default *string
	// Required is true when the template provides no default.
	Required bool
}

// MissingVariablesError lists every required template variable that no
// source resolved.
type MissingVariablesError struct {
	Names []string
}

func (e *MissingVariablesError) Error() string {
	return fmt.Sprintf("missing required template variables: %s", strings.Join(e.Names, ", "))
}

// repoFactory creates the clusterctl repository client for a provider. Tests
// replace it to avoid the network.
type repoFactory func(ctx context.Context, provider config.Provider, cfg config.Client) (repository.Client, error)

func defaultRepoFactory(ctx context.Context, provider config.Provider, cfg config.Client) (repository.Client, error) {
	return repository.New(ctx, provider, cfg)
}

// generator holds the injectable dependencies of Generate and ListVariables.
type generator struct {
	newRepo repoFactory
}

var defaultGenerator = generator{newRepo: defaultRepoFactory}

// Generate fetches, resolves and renders the cluster template described by req.
func Generate(ctx context.Context, req Request) (*Bundle, error) {
	return defaultGenerator.generate(ctx, req)
}

// ListVariables returns the variables of the template described by req,
// sorted by name. Variable values in req are not consulted.
func ListVariables(ctx context.Context, req Request) ([]Variable, error) {
	return defaultGenerator.listVariables(ctx, req)
}

func (g generator) listVariables(ctx context.Context, req Request) ([]Variable, error) {
	reader, err := newReader(ctx, req)
	if err != nil {
		return nil, err
	}
	tc, err := g.templates(ctx, req, reader)
	if err != nil {
		return nil, err
	}
	tmpl, err := tc.Get(ctx, req.Spec.Flavor, namespace(req), true)
	if err != nil {
		return nil, errors.Wrap(err, "reading template variables")
	}
	return toVariables(tmpl.VariableMap()), nil
}

func (g generator) generate(ctx context.Context, req Request) (*Bundle, error) {
	reader, err := newReader(ctx, req)
	if err != nil {
		return nil, err
	}
	tc, err := g.templates(ctx, req, reader)
	if err != nil {
		return nil, err
	}
	ns := namespace(req)

	vars, err := tc.Get(ctx, req.Spec.Flavor, ns, true)
	if err != nil {
		return nil, errors.Wrap(err, "reading template variables")
	}
	resolved, err := resolve(toVariables(vars.VariableMap()), sources(req))
	if err != nil {
		return nil, err
	}
	for name, value := range resolved.provided {
		reader.Set(name, value)
	}

	tmpl, err := tc.Get(ctx, req.Spec.Flavor, ns, false)
	if err != nil {
		return nil, errors.Wrap(err, "rendering template")
	}
	raw, err := tmpl.Yaml()
	if err != nil {
		return nil, errors.Wrap(err, "serializing rendered template")
	}
	b, err := ParseBundle(raw)
	if err != nil {
		return nil, errors.Wrap(err, "parsing rendered template")
	}
	b.Variables = resolved.used
	return b, nil
}

// templates builds an in-memory clusterctl configuration and returns the
// template client for the pinned provider version.
func (g generator) templates(ctx context.Context, req Request, reader *config.MemoryReader) (repository.TemplateClient, error) {
	if req.Provider.Name == "" {
		return nil, errors.New("provider name is required")
	}
	if req.Provider.Version == "" {
		return nil, errors.New("provider version is required")
	}
	if req.Spec.Name == "" {
		return nil, errors.New("cluster name is required")
	}
	cfg, err := config.New(ctx, "", config.InjectReader(reader))
	if err != nil {
		return nil, errors.Wrap(err, "creating clusterctl configuration")
	}
	p, err := cfg.Providers().Get(req.Provider.Name, clusterctlv1.InfrastructureProviderType)
	if err != nil {
		return nil, errors.Wrapf(err, "unknown infrastructure provider %q", req.Provider.Name)
	}
	// Pin the release in the URL so the repository never resolves "latest"
	// through the GitHub API.
	p = config.NewProvider(p.Name(), pinURL(p.URL(), req.Provider.Version), p.Type())
	repo, err := g.newRepo(ctx, p, cfg)
	if err != nil {
		return nil, errors.Wrapf(err, "creating repository client for provider %q", req.Provider.Name)
	}
	return repo.Templates(req.Provider.Version), nil
}

// newReader returns an in-memory configuration reader carrying the provider
// URL override, when one is set.
func newReader(ctx context.Context, req Request) (*config.MemoryReader, error) {
	reader := config.NewMemoryReader()
	if req.Provider.URL != "" {
		if _, err := reader.AddProvider(req.Provider.Name, clusterctlv1.InfrastructureProviderType, req.Provider.URL); err != nil {
			return nil, errors.Wrap(err, "adding provider URL override")
		}
	}
	if err := reader.Init(ctx, ""); err != nil {
		return nil, errors.Wrap(err, "initializing in-memory configuration")
	}
	return reader, nil
}

// pinURL replaces a "/releases/latest/" path segment with the pinned version.
func pinURL(u, version string) string {
	return strings.Replace(u, "/releases/latest/", "/releases/"+version+"/", 1)
}

func namespace(req Request) string {
	if req.Spec.Namespace == "" {
		return DefaultNamespace
	}
	return req.Spec.Namespace
}

// sources returns the variable sources for req, highest precedence first:
// req.Variables, then req.Spec.Variables, then values derived from req.
func sources(req Request) []VariableSource {
	out := make([]VariableSource, 0, len(req.Variables)+2)
	for _, s := range req.Variables {
		if s != nil {
			out = append(out, s)
		}
	}
	if len(req.Spec.Variables) > 0 {
		out = append(out, MapSource(req.Spec.Variables))
	}
	return append(out, MapSource(requestDefaults(req)))
}

// requestDefaults derives the standard variables from the Request fields.
func requestDefaults(req Request) map[string]string {
	m := map[string]string{
		VarNamespace: namespace(req),
	}
	if req.Spec.Name != "" {
		m[VarClusterName] = req.Spec.Name
	}
	if req.Kubernetes != "" {
		m[VarKubernetesVersion] = req.Kubernetes
	}
	if req.ControlPlaneReplicas > 0 {
		m[VarControlPlaneMachineCount] = strconv.Itoa(req.ControlPlaneReplicas)
	}
	if req.WorkerReplicas >= 0 {
		m[VarWorkerMachineCount] = strconv.Itoa(req.WorkerReplicas)
	}
	if req.Endpoint.Host != "" {
		m[VarControlPlaneEndpointIP] = req.Endpoint.Host
	}
	if req.Endpoint.Port > 0 {
		m[VarControlPlaneEndpointPort] = strconv.Itoa(req.Endpoint.Port)
	}
	return m
}

type resolution struct {
	// provided holds values found in a source; they are injected into the
	// clusterctl configuration.
	provided map[string]string
	// used is provided plus the template defaults that applied.
	used map[string]string
}

// resolve looks up every template variable in the sources, in order, and
// fails with MissingVariablesError when any required one is not found.
func resolve(vars []Variable, srcs []VariableSource) (resolution, error) {
	r := resolution{provided: map[string]string{}, used: map[string]string{}}
	var missing []string
	for _, v := range vars {
		found := false
		for _, s := range srcs {
			if val, ok := s.Get(v.Name); ok {
				r.provided[v.Name] = val
				r.used[v.Name] = val
				found = true
				break
			}
		}
		switch {
		case found:
		case v.Default != nil:
			r.used[v.Name] = *v.Default
		default:
			missing = append(missing, v.Name)
		}
	}
	if len(missing) > 0 {
		sort.Strings(missing)
		return r, &MissingVariablesError{Names: missing}
	}
	return r, nil
}

func toVariables(m map[string]*string) []Variable {
	out := make([]Variable, 0, len(m))
	for name, def := range m {
		out = append(out, Variable{Name: name, Default: def, Required: def == nil})
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Name < out[j].Name })
	return out
}
