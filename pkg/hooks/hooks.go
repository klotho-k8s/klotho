// Package hooks is klotho's lifecycle extension point. klotho boots a
// self-managed Cluster API cluster and steps back; hooks are the seam that
// lets a consumer run their own actions against the booted cluster at
// well-defined moments - install GitOps, set up recovery, deploy apps -
// without klotho bundling, installing, or having an opinion about any of it.
//
// klotho ships zero default hooks and installs nothing. A hook is entirely the
// consumer's: an exec hook runs a command with KUBECONFIG and cluster metadata
// in its environment, so the consumer can run flux, helm, kubectl or their own
// CLI against the cluster klotho just handed them.
//
// Events name the moments hooks fire at (see Event). Run dispatches the hooks
// for one event, in configuration order, honoring each hook's timeout and
// failure policy.
package hooks

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"time"

	"github.com/klotho-k8s/klotho/pkg/cluster"
)

// Event is a lifecycle moment a hook can fire at. The set is a closed registry
// (see events) so a configuration naming an unknown event is rejected, and so
// new moments can be added here without touching the dispatch.
type Event string

const (
	// PostBootstrap fires once node zero is a working single-node cluster,
	// before Cluster API has adopted it. Reserved for a later cut; wired into
	// the registry so it validates, but klotho does not fire it yet.
	PostBootstrap Event = "postBootstrap"
	// PostPivot fires after the cluster is self-managed and healthy: the
	// cluster is alive and the consumer's to use. This is the primary event.
	PostPivot Event = "postPivot"
	// PreDelete fires before teardown begins, while the cluster's API is still
	// reachable, so a consumer can remove their own resources first.
	PreDelete Event = "preDelete"
	// PostDelete fires after teardown completes. Reserved for a later cut;
	// wired into the registry so it validates, but klotho does not fire it yet.
	PostDelete Event = "postDelete"
)

// events is the closed set of known events. A later cut fires PostBootstrap
// and PostDelete; they are registered now so configuration referencing them
// validates and no refactor is needed to start firing them.
var events = map[Event]bool{
	PostBootstrap: true,
	PostPivot:     true,
	PreDelete:     true,
	PostDelete:    true,
}

// Valid reports whether e is a known event.
func (e Event) Valid() bool { return events[e] }

// Events returns the known events, for help text and error messages.
func Events() []Event {
	return []Event{PostBootstrap, PostPivot, PreDelete, PostDelete}
}

// Type is a hook's kind: how klotho runs it.
type Type string

const (
	// Exec runs a command or script with KUBECONFIG and cluster metadata in
	// its environment. This is the generic plumbing: it lets a consumer do
	// anything against the cluster.
	Exec Type = "exec"
	// Apply applies a manifest file, directory or URL to the booted cluster
	// via klotho's own client. Reserved: the type validates, but the engine
	// does not run it yet (use an exec hook that runs kubectl apply).
	Apply Type = "apply"
)

// types is the closed set of known hook types.
var types = map[Type]bool{Exec: true, Apply: true}

// Valid reports whether t is a known hook type.
func (t Type) Valid() bool { return types[t] }

// Types returns the known hook types, for help text and error messages.
func Types() []Type { return []Type{Exec, Apply} }

// Policy is what klotho does when a hook fails.
type Policy string

const (
	// Fail aborts the operation with the hook's error. It is the default.
	Fail Policy = "fail"
	// Warn logs the failure and continues.
	Warn Policy = "warn"
)

// Valid reports whether p is a known failure policy (the empty policy is valid
// and means Fail).
func (p Policy) Valid() bool { return p == "" || p == Fail || p == Warn }

// Hook is one resolved lifecycle hook, ready to run. The CLI builds these from
// the hooks section of the configuration (see pkg/config).
type Hook struct {
	// Name identifies the hook in logs and errors.
	Name string
	// Event is when the hook fires.
	Event Event
	// Type is how it runs (Exec or Apply).
	Type Type
	// Command and Args are the exec command line.
	Command string
	Args    []string
	// Manifest is the apply source (file, directory or URL).
	Manifest string
	// Timeout bounds the hook. Zero means no klotho-imposed bound (the parent
	// context still applies).
	Timeout time.Duration
	// OnFailure is the failure policy. The empty policy means Fail.
	OnFailure Policy
}

// Target describes the booted cluster a hook runs against. klotho fills it in
// and hands it to the executor, which exposes it to the hook (KUBECONFIG and
// KLOTHO_* environment variables for an exec hook).
type Target struct {
	// Event is the event being fired.
	Event Event
	// Cluster is the cluster's name.
	Cluster string
	// Endpoint is the API server endpoint.
	Endpoint cluster.Endpoint
	// KubeContext is the context name in Kubeconfig.
	KubeContext string
	// Kubeconfig is an admin kubeconfig for the cluster. The executor writes it
	// to a private temporary file and points KUBECONFIG at it.
	Kubeconfig []byte
}

// Executor runs a single hook against a target. Run owns ordering, timeout and
// failure policy; an Executor just runs one hook under the context it is given.
// Engine is the default implementation.
type Executor interface {
	Run(ctx context.Context, h Hook, t Target) error
}

// Any reports whether any hook in hooks fires at event. Callers use it to skip
// building a kubeconfig when there is nothing to run.
func Any(hooks []Hook, event Event) bool {
	for _, h := range hooks {
		if h.Event == event {
			return true
		}
	}
	return false
}

// Run fires every hook that matches event, in slice order, through ex. Each
// hook is bounded by its own timeout. On failure, a hook whose policy is Warn
// is logged and skipped; otherwise Run stops and returns the error. The target
// has its Event set for each hook.
func Run(ctx context.Context, log *slog.Logger, ex Executor, event Event, target Target, hooks []Hook) error {
	if log == nil {
		log = slog.New(slog.DiscardHandler)
	}
	target.Event = event
	for _, h := range hooks {
		if h.Event != event {
			continue
		}
		log.Info("running hook", "name", h.Name, "event", event, "type", h.Type)

		hctx := ctx
		var cancel context.CancelFunc
		if h.Timeout > 0 {
			hctx, cancel = context.WithTimeout(ctx, h.Timeout)
		}
		err := ex.Run(hctx, h, target)
		timedOut := h.Timeout > 0 && errors.Is(hctx.Err(), context.DeadlineExceeded) && ctx.Err() == nil
		if cancel != nil {
			cancel()
		}
		if err == nil {
			log.Info("hook completed", "name", h.Name, "event", event)
			continue
		}
		if timedOut {
			err = fmt.Errorf("timed out after %s: %w", h.Timeout, err)
		}
		if h.OnFailure == Warn {
			log.Warn("hook failed; continuing", "name", h.Name, "event", event, "err", err)
			continue
		}
		return fmt.Errorf("%s hook %q failed: %w", event, h.Name, err)
	}
	return nil
}
