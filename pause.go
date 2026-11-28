package klotho

import (
	"context"
	"fmt"
	"strings"

	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime/schema"

	"github.com/klotho-k8s/klotho/pkg/generate"
	"github.com/klotho-k8s/klotho/pkg/kube"
	"github.com/klotho-k8s/klotho/provider"
)

// Cluster API labels and the per-object pause annotation klotho's guards read.
const (
	clusterNameLabel  = "cluster.x-k8s.io/cluster-name"
	controlPlaneLabel = "cluster.x-k8s.io/control-plane"
	// pausedAnnotation is the surgical, single-object pause the per-object
	// contract honors. klotho's pause never sets it (it sets spec.paused on the
	// Cluster), but unpause reports any left on child objects because clearing
	// spec.paused does not remove them.
	pausedAnnotation = "cluster.x-k8s.io/paused"
)

var (
	machineDeploymentGVK = schema.GroupVersionKind{Group: generate.ClusterGroup, Version: clusterContractVersion, Kind: "MachineDeployment"}
	machineSetGVK        = schema.GroupVersionKind{Group: generate.ClusterGroup, Version: clusterContractVersion, Kind: "MachineSet"}
)

// PauseOptions control Pause and Unpause.
type PauseOptions struct {
	// Force proceeds even when a blocking guard fires (a cluster or machine
	// being deleted). The guard is still reported; nothing about the cluster is
	// changed to resolve it.
	Force bool
}

// PauseResult reports what Pause or Unpause observed and did. Findings are
// advisory guard output (mid-rollout, unhealthy, already-paused state, lingering
// annotations); they are also logged through deps' logger as they are found.
type PauseResult struct {
	// Cluster is the cluster name.
	Cluster string
	// Paused is the Cluster's spec.paused after the call.
	Paused bool
	// Changed is whether this call changed spec.paused.
	Changed bool
	// SetBy is the field manager that owns spec.paused, when the cluster was
	// already in the requested state. It is empty when klotho could not tell.
	SetBy string
	// Findings are the advisory guard findings, in the order they were found.
	Findings []provider.Finding
}

// PauseBlockedError is returned by Pause when a blocking guard fired (the
// Cluster or a Machine is being deleted) and PauseOptions.Force was not set.
// Nothing was changed.
type PauseBlockedError struct {
	Findings []provider.Finding
}

func (e *PauseBlockedError) Error() string {
	var b strings.Builder
	fmt.Fprintf(&b, "%d finding(s) block pausing the cluster:\n", len(e.Findings))
	for _, f := range e.Findings {
		fmt.Fprintf(&b, "  - %s\n", f)
	}
	b.WriteString("pausing stalls an in-flight deletion; unpause and let it finish, or re-run with --force to pause anyway")
	return b.String()
}

// Pause sets spec.paused on the cluster's Cluster object, which stops every
// Cluster API controller that owns the cluster from reconciling it. It changes
// nothing else: the API server, etcd, kubelets and workloads keep running, so
// pausing frees no resources and lets no failed machine self-heal. It also
// stalls an in-flight deletion, so a Cluster or Machine being deleted blocks the
// pause unless opts.Force is set.
//
// The field is set with a server-side apply of only spec.paused under the
// dedicated PauseFieldManager, so klotho owns that one field and never clobbers
// the rest of the Cluster (nor a pause a clusterctl move set under its own
// manager). A cluster already paused is reported, with the field manager that
// set it, and left untouched.
func Pause(ctx context.Context, deps Deps, name string, opts PauseOptions) (*PauseResult, error) {
	cl, live, err := connectAndLoadCluster(ctx, deps, name)
	if err != nil {
		return nil, err
	}
	log := deps.log().With("cluster", name)
	res := &PauseResult{Cluster: name}

	// Already paused: report who owns the field and do nothing, so a clusterctl
	// move pause is never clobbered.
	if clusterPaused(live) {
		res.Paused = true
		res.SetBy = pausedFieldManager(live)
		f := provider.Finding{
			Object:  provider.ObjectRef(live),
			Field:   "spec.paused",
			Message: pausedByMessage(res.SetBy),
		}
		res.Findings = append(res.Findings, f)
		log.Info("cluster already paused", "setBy", res.SetBy)
		return res, nil
	}

	blocking, advisory := pauseGuards(ctx, deps, cl, live, name)
	res.Findings = append(res.Findings, advisory...)
	res.Findings = append(res.Findings, blocking...)
	for _, f := range advisory {
		log.Warn("pause finding", "object", f.Object, "field", f.Field, "detail", f.Message)
	}
	for _, f := range blocking {
		log.Warn("pause blocked", "object", f.Object, "field", f.Field, "detail", f.Message)
	}
	if len(blocking) > 0 && !opts.Force {
		return nil, &PauseBlockedError{Findings: blocking}
	}
	if len(blocking) > 0 {
		log.Warn("pausing despite blocking findings", "findings", len(blocking))
	}

	if err := setClusterPaused(ctx, deps, cl, live, true); err != nil {
		return nil, err
	}
	log.Info("paused")
	res.Paused = true
	res.Changed = true
	return res, nil
}

// Unpause clears spec.paused on the cluster's Cluster object, so the Cluster API
// controllers resume reconciling it. A cluster that is not paused is reported
// and left untouched. Any lingering per-object cluster.x-k8s.io/paused
// annotations on child objects are reported, because clearing spec.paused does
// not remove them.
func Unpause(ctx context.Context, deps Deps, name string, _ PauseOptions) (*PauseResult, error) {
	cl, live, err := connectAndLoadCluster(ctx, deps, name)
	if err != nil {
		return nil, err
	}
	log := deps.log().With("cluster", name)
	res := &PauseResult{Cluster: name}

	// Lingering per-object annotations survive unpausing spec.paused; report
	// them either way so they do not silently keep an object paused.
	annotations := lingeringPauseAnnotations(ctx, cl, live, name)
	res.Findings = append(res.Findings, annotations...)
	for _, f := range annotations {
		log.Warn("pause finding", "object", f.Object, "field", f.Field, "detail", f.Message)
	}

	if !clusterPaused(live) {
		res.SetBy = pausedFieldManager(live)
		res.Findings = append(res.Findings, provider.Finding{
			Object:  provider.ObjectRef(live),
			Field:   "spec.paused",
			Message: "the cluster is not paused; nothing to do",
		})
		log.Info("cluster not paused")
		return res, nil
	}

	if err := setClusterPaused(ctx, deps, cl, live, false); err != nil {
		return nil, err
	}
	log.Info("unpaused")
	res.Paused = false
	res.Changed = true
	return res, nil
}

// connectAndLoadCluster loads a cluster's state, connects to its API with an
// admin credential minted from that state, and reads the live Cluster object.
func connectAndLoadCluster(ctx context.Context, deps Deps, name string) (kube.Cluster, *unstructured.Unstructured, error) {
	if err := deps.check(false); err != nil {
		return nil, nil, err
	}
	st, err := Load(ctx, deps, name)
	if err != nil {
		return nil, nil, err
	}
	if st.Reserved {
		return nil, nil, fmt.Errorf("cluster %s is only reserved; there is nothing to pause", name)
	}
	cl, err := deps.connectAdmin(ctx, st)
	if err != nil {
		return nil, nil, err
	}
	live, err := cl.Get(ctx, clusterGVK, "default", name)
	if err != nil {
		return nil, nil, fmt.Errorf("reading Cluster %s: %w", name, err)
	}
	return cl, live, nil
}

// setClusterPaused server-side applies only spec.paused on the live Cluster
// under PauseFieldManager. It re-sends neither the Cluster's references nor any
// other field, so it never takes ownership of anything but spec.paused — the
// day-2 equivalent of clusterctl's own setClusterPause patch.
func setClusterPaused(ctx context.Context, _ Deps, cl kube.Cluster, live *unstructured.Unstructured, paused bool) error {
	u := &unstructured.Unstructured{Object: map[string]any{}}
	u.SetGroupVersionKind(live.GroupVersionKind())
	u.SetName(live.GetName())
	if ns := live.GetNamespace(); ns != "" {
		u.SetNamespace(ns)
	}
	if err := unstructured.SetNestedField(u.Object, paused, "spec", "paused"); err != nil {
		return fmt.Errorf("setting spec.paused: %w", err)
	}
	if err := cl.Apply(ctx, PauseFieldManager, u); err != nil {
		return fmt.Errorf("applying spec.paused=%v to Cluster %s: %w", paused, live.GetName(), err)
	}
	return nil
}

// pauseGuards runs the pause guards against the live cluster and returns the
// blocking findings (a deletion in flight) and the advisory ones (mid-rollout,
// unhealthy control plane). It never changes anything.
func pauseGuards(ctx context.Context, _ Deps, cl kube.Cluster, live *unstructured.Unstructured, name string) (blocking, advisory []provider.Finding) {
	machines := clusterMachines(ctx, cl, name)

	// Guard 1 (block): a deletion in flight. Pausing stalls it, because the
	// paused check precedes reconcileDelete.
	if live.GetDeletionTimestamp() != nil {
		blocking = append(blocking, provider.Finding{
			Object:  provider.ObjectRef(live),
			Field:   "metadata.deletionTimestamp",
			Message: "the Cluster is being deleted; pausing stalls the deletion until the cluster is unpaused",
		})
	}
	for _, m := range machines {
		if m.GetDeletionTimestamp() != nil {
			blocking = append(blocking, provider.Finding{
				Object:  provider.ObjectRef(m),
				Field:   "metadata.deletionTimestamp",
				Message: "this Machine is being deleted; pausing stalls the deletion until the cluster is unpaused",
			})
		}
	}

	// Guard 2 (warn): mid-rollout. A control plane or pool rollout freezes
	// half-done while paused.
	advisory = append(advisory, rolloutFindings(ctx, cl, live)...)

	// Guard 3 (warn): an unhealthy control plane. Pausing removes the only
	// thing that could remediate it.
	for _, m := range machines {
		if !isControlPlaneMachine(m) {
			continue
		}
		if !machineReady(m) {
			advisory = append(advisory, provider.Finding{
				Object:  provider.ObjectRef(m),
				Field:   "status.conditions[Ready]",
				Message: "this control plane Machine is not Ready; pausing stops the controllers that could remediate it",
			})
		}
	}
	return blocking, advisory
}

// rolloutFindings reports a control plane or pool rollout that is in progress,
// best effort: an object it cannot read is simply not reported.
func rolloutFindings(ctx context.Context, cl kube.Cluster, live *unstructured.Unstructured) []provider.Finding {
	var out []provider.Finding
	if cp := controlPlaneObject(ctx, cl, live); cp != nil {
		specVer, _, _ := unstructured.NestedString(cp.Object, "spec", "version")
		statusVer, _, _ := unstructured.NestedString(cp.Object, "status", "version")
		if specVer != "" && statusVer != "" && specVer != statusVer {
			out = append(out, provider.Finding{
				Object:  provider.ObjectRef(cp),
				Field:   "spec.version",
				Message: fmt.Sprintf("a control plane rollout is in progress (spec.version %s, status.version %s); pausing freezes it half-done", specVer, statusVer),
			})
		} else if replicas, updated, ok := replicaProgress(cp); ok && updated < replicas {
			out = append(out, provider.Finding{
				Object:  provider.ObjectRef(cp),
				Field:   "status.updatedReplicas",
				Message: fmt.Sprintf("a control plane rollout is in progress (%d of %d replicas updated); pausing freezes it half-done", updated, replicas),
			})
		}
	}
	for _, md := range list(ctx, cl, machineDeploymentGVK) {
		if replicas, updated, ok := replicaProgress(md); ok && updated < replicas {
			out = append(out, provider.Finding{
				Object:  provider.ObjectRef(md),
				Field:   "status.updatedReplicas",
				Message: fmt.Sprintf("a pool rollout is in progress (%d of %d replicas updated); pausing freezes it half-done", updated, replicas),
			})
		}
	}
	return out
}

// lingeringPauseAnnotations reports every child object still carrying the
// per-object cluster.x-k8s.io/paused annotation, which unpausing spec.paused
// does not remove.
func lingeringPauseAnnotations(ctx context.Context, cl kube.Cluster, live *unstructured.Unstructured, name string) []provider.Finding {
	var out []provider.Finding
	seen := map[string]bool{}
	report := func(o *unstructured.Unstructured) {
		if o == nil {
			return
		}
		if _, ok := o.GetAnnotations()[pausedAnnotation]; !ok {
			return
		}
		ref := provider.ObjectRef(o)
		if seen[ref] {
			return
		}
		seen[ref] = true
		out = append(out, provider.Finding{
			Object:  ref,
			Field:   "metadata.annotations[" + pausedAnnotation + "]",
			Message: "this object still carries the per-object pause annotation; unpausing the Cluster does not clear it, so the object stays paused until the annotation is removed",
		})
	}
	report(controlPlaneObject(ctx, cl, live))
	for _, m := range clusterMachines(ctx, cl, name) {
		report(m)
	}
	for _, md := range list(ctx, cl, machineDeploymentGVK) {
		report(md)
	}
	for _, ms := range list(ctx, cl, machineSetGVK) {
		report(ms)
	}
	return out
}

// clusterMachines lists the Machines belonging to the named cluster, best
// effort. A Machine with no cluster-name label is included (a self-hosted
// management cluster holds only its own objects).
func clusterMachines(ctx context.Context, cl kube.Cluster, name string) []*unstructured.Unstructured {
	var out []*unstructured.Unstructured
	for _, m := range list(ctx, cl, machineGVK) {
		if owner, ok := m.GetLabels()[clusterNameLabel]; ok && owner != name {
			continue
		}
		out = append(out, m)
	}
	return out
}

// controlPlaneObject reads the Cluster's spec.controlPlaneRef and returns the
// live control plane object, or nil when it has none or cannot be read.
func controlPlaneObject(ctx context.Context, cl kube.Cluster, live *unstructured.Unstructured) *unstructured.Unstructured {
	ref, ok, _ := unstructured.NestedMap(live.Object, "spec", "controlPlaneRef")
	if !ok || len(ref) == 0 {
		return nil
	}
	kind, _ := ref["kind"].(string)
	cpName, _ := ref["name"].(string)
	if kind == "" || cpName == "" {
		return nil
	}
	group := ControlPlaneGroup
	if ag, _ := ref["apiGroup"].(string); ag != "" {
		group = ag
	} else if av, _ := ref["apiVersion"].(string); av != "" {
		if i := strings.IndexByte(av, '/'); i >= 0 {
			group = av[:i]
		}
	}
	ns := live.GetNamespace()
	if ns == "" {
		ns = "default"
	}
	gvk := schema.GroupVersionKind{Group: group, Version: clusterContractVersion, Kind: kind}
	cp, err := cl.Get(ctx, gvk, ns, cpName)
	if err != nil {
		return nil
	}
	return cp
}

// list reads every object of a kind across all namespaces, best effort.
func list(ctx context.Context, cl kube.Cluster, gvk schema.GroupVersionKind) []*unstructured.Unstructured {
	objs, err := cl.List(ctx, gvk, "")
	if err != nil {
		return nil
	}
	return objs
}

// clusterPaused reports whether spec.paused is true on the object.
func clusterPaused(u *unstructured.Unstructured) bool {
	p, _, _ := unstructured.NestedBool(u.Object, "spec", "paused")
	return p
}

// pausedFieldManager returns the field manager that owns spec.paused, from the
// object's managedFields, or "" when none can be found.
func pausedFieldManager(u *unstructured.Unstructured) string {
	entries, ok, _ := unstructured.NestedSlice(u.Object, "metadata", "managedFields")
	if !ok {
		return ""
	}
	for _, e := range entries {
		entry, ok := e.(map[string]any)
		if !ok {
			continue
		}
		fields, ok := entry["fieldsV1"].(map[string]any)
		if !ok {
			continue
		}
		spec, ok := fields["f:spec"].(map[string]any)
		if !ok {
			continue
		}
		if _, ok := spec["f:paused"]; ok {
			if m, ok := entry["manager"].(string); ok {
				return m
			}
		}
	}
	return ""
}

// pausedByMessage describes an already-paused cluster and who set it.
func pausedByMessage(setBy string) string {
	switch {
	case setBy == PauseFieldManager:
		return "the cluster is already paused by klotho; nothing to do"
	case setBy != "":
		return fmt.Sprintf("the cluster is already paused (spec.paused set by field manager %q); leaving it as it is so klotho does not clobber it", setBy)
	default:
		return "the cluster is already paused; leaving it as it is"
	}
}

// isControlPlaneMachine reports whether a Machine carries the control-plane
// label.
func isControlPlaneMachine(m *unstructured.Unstructured) bool {
	_, ok := m.GetLabels()[controlPlaneLabel]
	return ok
}

// machineReady reports whether a Machine's Ready condition is True.
func machineReady(m *unstructured.Unstructured) bool {
	conds, ok, _ := unstructured.NestedSlice(m.Object, "status", "conditions")
	if !ok {
		return false
	}
	for _, c := range conds {
		cond, ok := c.(map[string]any)
		if !ok {
			continue
		}
		if t, _ := cond["type"].(string); t == "Ready" {
			s, _ := cond["status"].(string)
			return s == "True"
		}
	}
	return false
}

// replicaProgress reads an object's status.replicas and status.updatedReplicas.
func replicaProgress(o *unstructured.Unstructured) (replicas, updated int64, ok bool) {
	replicas, rok, _ := unstructured.NestedInt64(o.Object, "status", "replicas")
	updated, uok, _ := unstructured.NestedInt64(o.Object, "status", "updatedReplicas")
	return replicas, updated, rok && uok
}
