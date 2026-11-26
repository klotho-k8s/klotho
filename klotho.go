// Package klotho is the orchestration library behind the command line: it
// creates a self-managed Cluster API cluster from nothing, deletes it again,
// and reads back what the state backend remembers. Everything it needs is
// injected through Deps, so a server can drive it the same way the CLI does.
package klotho

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"time"

	"k8s.io/apimachinery/pkg/util/validation"

	"github.com/klotho-k8s/klotho/pkg/cluster"
	"github.com/klotho-k8s/klotho/pkg/generate"
	"github.com/klotho-k8s/klotho/pkg/hooks"
	"github.com/klotho-k8s/klotho/pkg/kube"
	"github.com/klotho-k8s/klotho/pkg/state"
	"github.com/klotho-k8s/klotho/provider/backend"
	"github.com/klotho-k8s/klotho/provider/controlplane"
	"github.com/klotho-k8s/klotho/provider/infrastructure"
	"github.com/klotho-k8s/klotho/provider/payload"
)

// Deps are the collaborators an operation uses. Store and Codec are always
// required; Infra and ControlPlane are required by Create and Delete.
type Deps struct {
	Store        backend.Store
	Infra        infrastructure.Provider
	ControlPlane controlplane.Provider
	// Payload is nil when the infrastructure provider's user-data channel
	// needs no payload host.
	Payload payload.Host
	Codec   state.RecordCodec
	Logger  *slog.Logger

	// Connect opens the new cluster's API with the bootstrap admin
	// kubeconfig. Nil means kube.New (client-go).
	Connect func(ctx context.Context, kubeconfig []byte) (kube.Cluster, error)
	// ConnectAdmin opens an existing cluster's API for deletion, with an
	// admin credential minted from its state. Nil mints from state.CA, builds
	// a kubeconfig for state.Endpoint and dials it (returning a cause wrapping
	// errClusterUnreachable when the API cannot be reached).
	ConnectAdmin func(ctx context.Context, st *cluster.State) (kube.Cluster, error)
	// Generate renders the cluster template. Nil means generate.Generate.
	Generate func(ctx context.Context, req generate.Request) (*generate.Bundle, error)
	// PollInterval is how often waits poll. Zero means DefaultPollInterval.
	PollInterval time.Duration
	// Now is the clock. Nil means time.Now.
	Now func() time.Time
}

// DefaultPollInterval is how often Create polls the new cluster.
const DefaultPollInterval = 5 * time.Second

func (d *Deps) log() *slog.Logger {
	if d.Logger == nil {
		return slog.New(slog.DiscardHandler)
	}
	return d.Logger
}

func (d *Deps) now() time.Time {
	if d.Now == nil {
		return time.Now().UTC()
	}
	return d.Now().UTC()
}

func (d *Deps) poll() time.Duration {
	if d.PollInterval <= 0 {
		return DefaultPollInterval
	}
	return d.PollInterval
}

func (d *Deps) check(needProviders bool) error {
	var missing []string
	if d.Store == nil {
		missing = append(missing, "Store")
	}
	if d.Codec == nil {
		missing = append(missing, "Codec")
	}
	if needProviders && d.Infra == nil {
		missing = append(missing, "Infra")
	}
	if needProviders && d.ControlPlane == nil {
		missing = append(missing, "ControlPlane")
	}
	if len(missing) > 0 {
		return fmt.Errorf("klotho: missing dependencies: %v", missing)
	}
	return nil
}

// ValidateName checks a cluster name. It must be a DNS-1123 label (lowercase
// letters, digits and "-", at most 63 characters, starting and ending with
// an alphanumeric), because it becomes Kubernetes object names, label
// values, a Proxmox tag, and part of VM and pool names.
func ValidateName(name string) error {
	if name == "" {
		return errors.New("a cluster name is required")
	}
	if errs := validation.IsDNS1123Label(name); len(errs) > 0 {
		return fmt.Errorf("invalid cluster name %q: %s", name, errs[0])
	}
	return nil
}

// Load reads and decodes the state of a cluster. A missing cluster returns
// an error matching backend.ErrNotFound.
func Load(ctx context.Context, deps Deps, name string) (*cluster.State, error) {
	if err := deps.check(false); err != nil {
		return nil, err
	}
	rec, err := deps.Store.Load(ctx, name)
	if err != nil {
		return nil, err
	}
	st, err := deps.Codec.Open(rec)
	if err != nil {
		return nil, fmt.Errorf("cluster %s: %w", name, err)
	}
	if st.Spec.Name == "" {
		st.Spec.Name = name
	}
	if st.Spec.Name != name {
		return nil, fmt.Errorf("cluster %s: record names cluster %q", name, st.Spec.Name)
	}
	return st, nil
}

// Summary is one row of List.
type Summary struct {
	Name         string           `json:"name"`
	Provider     string           `json:"provider,omitempty"`
	ControlPlane string           `json:"controlPlane,omitempty"`
	Endpoint     cluster.Endpoint `json:"endpoint"`
	CreatedAt    time.Time        `json:"createdAt"`
	Reserved     bool             `json:"reserved,omitempty"`
	// Error is set when the record could not be read; the other fields
	// except Name are then empty.
	Error string `json:"error,omitempty"`
}

// List summarizes every cluster the store knows, from state alone. Records
// that cannot be opened are listed with Error set rather than failing the
// whole listing.
func List(ctx context.Context, deps Deps) ([]Summary, error) {
	if err := deps.check(false); err != nil {
		return nil, err
	}
	names, err := deps.Store.List(ctx)
	if err != nil {
		return nil, err
	}
	out := make([]Summary, 0, len(names))
	for _, n := range names {
		st, err := Load(ctx, deps, n)
		if err != nil {
			if errors.Is(err, backend.ErrNotFound) {
				continue // deleted while listing
			}
			out = append(out, Summary{Name: n, Error: err.Error()})
			continue
		}
		out = append(out, Summary{
			Name:         n,
			Provider:     st.Spec.Provider,
			ControlPlane: st.Spec.ControlPlane,
			Endpoint:     st.Endpoint,
			CreatedAt:    st.CreatedAt,
			Reserved:     st.Reserved,
		})
	}
	return out, nil
}

// DefaultWatchTimeout bounds a watched delete when DeleteOptions.WatchTimeout
// is zero.
const DefaultWatchTimeout = 20 * time.Minute

// DeleteOptions control Delete.
type DeleteOptions struct {
	// Confirm is shown the teardown plan and returns whether to proceed.
	// Nil proceeds without asking.
	Confirm func(*infrastructure.Plan) bool
	// Watch follows the Cluster API deletion to completion and then sweeps
	// any infrastructure the providers left, instead of returning as soon as
	// the deletion is under way.
	Watch bool
	// Graceful is kept for compatibility and implies Watch.
	Graceful bool
	// Force skips Cluster API entirely and removes the infrastructure
	// directly, by tag and from state. Use it when the cluster's API is
	// already gone; it is also the automatic fallback when the API cannot be
	// reached.
	Force bool
	// WatchTimeout bounds a watched delete. Zero means DefaultWatchTimeout.
	WatchTimeout time.Duration
	// Hooks are the consumer's lifecycle hooks. Delete fires the preDelete
	// hooks (in order) before any teardown, while the cluster's API is still
	// reachable. Hooks for other events are ignored here.
	Hooks []hooks.Hook
	// HookExec runs a single hook. Nil means the built-in engine
	// (hooks.NewEngine). It exists for tests and alternate runners.
	HookExec hooks.Executor
}

func (o DeleteOptions) watch() bool { return o.Watch || o.Graceful }

func (o DeleteOptions) watchTimeout() time.Duration {
	if o.WatchTimeout <= 0 {
		return DefaultWatchTimeout
	}
	return o.WatchTimeout
}

// ErrDeclined is returned by Delete when Confirm returns false.
var ErrDeclined = errors.New("deletion declined")

// Delete removes a cluster. A record that only reserves the name is removed
// without touching the infrastructure. Otherwise, unless Force is set, Delete
// drives the deletion through the cluster's own Cluster API: it arms any
// server-side cleanup the provider offers, deletes the Cluster object, and
// lets the controllers remove the machines in order. Without Watch it returns
// once the deletion is under way (the provider and host finish it); with Watch
// it follows the deletion to completion and sweeps anything left. When the
// cluster's API cannot be reached, and with Force, it removes the
// infrastructure directly by tag instead.
func Delete(ctx context.Context, deps Deps, name string, opts DeleteOptions) error {
	if err := deps.check(false); err != nil {
		return err
	}
	log := deps.log().With("cluster", name)
	st, err := Load(ctx, deps, name)
	if err != nil {
		return err
	}
	if st.Reserved {
		log.Info("removing reservation")
		return deps.Store.Delete(ctx, name)
	}
	if deps.Infra == nil {
		return errors.New("klotho: an infrastructure provider is required to delete a cluster")
	}
	if st.Spec.Provider != "" && st.Spec.Provider != deps.Infra.Name() {
		return fmt.Errorf("cluster %s was created with provider %q, not %q", name, st.Spec.Provider, deps.Infra.Name())
	}
	plan, err := deps.Infra.Plan(ctx, st)
	if err != nil {
		return fmt.Errorf("planning teardown of %s: %w", name, err)
	}
	if opts.Confirm != nil && !opts.Confirm(plan) {
		return ErrDeclined
	}
	// Fire the consumer's preDelete hooks before any teardown, while the
	// cluster's API is still reachable. A failing hook (policy fail) aborts
	// the delete: the cluster is left untouched so the consumer can retry.
	if hooks.Any(opts.Hooks, hooks.PreDelete) {
		target, err := hookTarget(st)
		if err != nil {
			return err
		}
		if err := hooks.Run(ctx, deps.log(), deps.hookExec(opts.HookExec), hooks.PreDelete, target, opts.Hooks); err != nil {
			return err
		}
	}
	if opts.Force {
		return deleteDirect(ctx, deps, name, st, plan, log)
	}
	cl, err := deps.connectAdmin(ctx, st)
	if err != nil {
		if errors.Is(err, errClusterUnreachable) {
			log.Warn("cluster API not reachable; removing the infrastructure directly", "err", err)
			return deleteDirect(ctx, deps, name, st, plan, log)
		}
		return err
	}
	return deleteViaClusterAPI(ctx, deps, name, st, cl, opts, log)
}
