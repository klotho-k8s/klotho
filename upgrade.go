package klotho

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"strings"
	"time"

	"github.com/blang/semver/v4"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/apimachinery/pkg/types"

	"github.com/klotho-k8s/klotho/pkg/generate"
	"github.com/klotho-k8s/klotho/pkg/kube"
	"github.com/klotho-k8s/klotho/provider"
)

// Cluster API labels klotho upgrade reads to tie control-plane objects,
// MachineDeployments and Machines to a cluster.
const (
	LabelClusterName    = "cluster.x-k8s.io/cluster-name"
	LabelControlPlane   = "cluster.x-k8s.io/control-plane"
	LabelDeploymentName = "cluster.x-k8s.io/deployment-name"
)

// controlPlaneContractVersions are the Cluster API control-plane API versions
// klotho tries, most-preferred first. CAPI serves older contract versions
// through conversion, so trying v1beta2 then v1beta1 finds the object whichever
// the cluster stores.
var controlPlaneContractVersions = []string{"v1beta2", "v1beta1"}

// DefaultUpgradeImageField is the field under an infrastructure
// MachineTemplate's spec.template.spec that carries the node image. It is
// CAPD's customImage; a provider whose image field is named differently
// (Proxmox's templateID, say) overrides it with UpgradeOptions.ImageField.
const DefaultUpgradeImageField = "customImage"

// controlPlaneKind is a Cluster API control-plane object klotho upgrade can
// drive. Every control-plane kind klotho knows carries spec.version and rolls
// its machines when that version changes, so one generic implementation serves
// them all. The interface is the seam where a kind that needs special handling
// (a version string its controller formats differently, a different machine
// template path) plugs in later without touching the orchestration.
type controlPlaneKind interface {
	// Kind is the object kind, for example "KubeadmControlPlane".
	Kind() string
	// SpecVersion reads spec.version from the control-plane object.
	SpecVersion(obj *unstructured.Unstructured) (string, error)
	// VersionPatch is a JSON merge patch that sets spec.version to version.
	VersionPatch(version string) ([]byte, error)
	// InfraRefPath is the path to the infrastructure MachineTemplate reference
	// inside the control-plane object, for the template-swap upgrade path.
	InfraRefPath() []string
}

// genericControlPlane drives any control-plane kind through the fields every
// Cluster API control plane shares. The known kinds differ only by name.
type genericControlPlane struct {
	kind     string
	infraRef []string
}

func (g genericControlPlane) Kind() string { return g.kind }

func (g genericControlPlane) SpecVersion(obj *unstructured.Unstructured) (string, error) {
	v, _, err := unstructured.NestedString(obj.Object, "spec", "version")
	return v, err
}

func (g genericControlPlane) VersionPatch(version string) ([]byte, error) {
	return json.Marshal(map[string]any{"spec": map[string]any{"version": version}})
}

func (g genericControlPlane) InfraRefPath() []string { return g.infraRef }

// knownControlPlaneKinds is the discovery order. KubeadmControlPlane is the one
// klotho ships; the k0smotron, RKE2 and k3s control planes are registered so
// discovery finds a cluster built with them, and they drive identically.
func knownControlPlaneKinds() []controlPlaneKind {
	mt := []string{"spec", "machineTemplate", "spec", "infrastructureRef"}
	return []controlPlaneKind{
		genericControlPlane{kind: "KubeadmControlPlane", infraRef: mt},
		genericControlPlane{kind: "K0sControlPlane", infraRef: mt},
		genericControlPlane{kind: "RKE2ControlPlane", infraRef: mt},
		genericControlPlane{kind: "KThreesControlPlane", infraRef: mt},
	}
}

// UpgradeOptions control Upgrade.
type UpgradeOptions struct {
	// KubernetesVersion is the target workload version, for example "v1.33.4".
	// Empty means no node-version upgrade; at least one of KubernetesVersion or
	// a Components field must be set.
	KubernetesVersion string
	// Components, when requested, upgrades the Cluster API management-plane
	// components (the controllers themselves) through the cluster's own operator
	// provider CRs. It is independent of KubernetesVersion; when both are set the
	// components upgrade runs first (newer controllers then manage the node
	// rollout). See ComponentUpgrade.
	Components ComponentUpgrade
	// Autoscaler, when requested, installs, retunes or removes the Cluster
	// Autoscaler add-on day-2. It is independent of the other fields and runs
	// last (after any node or component upgrade). See AutoscalerChange.
	Autoscaler AutoscalerChange
	// ControlPlaneOnly upgrades only the control plane. WorkersOnly upgrades
	// only the MachineDeployments. Both false upgrades the control plane first,
	// then the workers. Both true is rejected.
	ControlPlaneOnly bool
	WorkersOnly      bool
	// Watch follows each rollout to completion before moving on: the control
	// plane's rolling replace finishes (new machine joined, old removed, all at
	// the target version) before any worker is touched, honoring the Cluster
	// API one-minor skew. Without Watch, Upgrade patches the objects and
	// returns, leaving the controllers to roll at their own pace.
	Watch bool
	// Force applies the version change despite skew findings (a jump across
	// more than one minor, a downgrade, or workers ahead of the control plane).
	Force bool
	// WatchTimeout bounds each watched rollout. Zero means DefaultWatchTimeout.
	WatchTimeout time.Duration
	// ControlPlaneImage and WorkerImage point the control plane / workers at a
	// new node image for the target version (the template-swap path, for a
	// provider whose image pins the Kubernetes version rather than deriving it
	// from the Machine's spec.version). Empty leaves the machine template as is,
	// which is correct for CAPD, whose DockerMachine derives kindest/node:v<ver>
	// from the Machine version.
	ControlPlaneImage string
	WorkerImage       string
	// ImageField is the field under the infrastructure MachineTemplate's
	// spec.template.spec that the image is written to. Empty means
	// DefaultUpgradeImageField (customImage).
	ImageField string
}

func (o UpgradeOptions) watchTimeout() time.Duration {
	if o.WatchTimeout <= 0 {
		return DefaultWatchTimeout
	}
	return o.WatchTimeout
}

func (o UpgradeOptions) imageField() string {
	if o.ImageField != "" {
		return o.ImageField
	}
	return DefaultUpgradeImageField
}

// includeControlPlane and includeWorkers resolve the scope flags. Neither flag
// set means both; both set is rejected by Upgrade before this is read.
func (o UpgradeOptions) includeControlPlane() bool { return !o.WorkersOnly }
func (o UpgradeOptions) includeWorkers() bool      { return !o.ControlPlaneOnly }

// UpgradeFindingsError is returned by Upgrade when the version change violates
// the Cluster API version skew and the request did not set Force.
type UpgradeFindingsError struct {
	Findings []provider.Finding
}

func (e *UpgradeFindingsError) Error() string {
	var b strings.Builder
	fmt.Fprintf(&b, "%d finding(s) block the upgrade:\n", len(e.Findings))
	for _, f := range e.Findings {
		fmt.Fprintf(&b, "  - %s\n", f)
	}
	b.WriteString("fix the version, or re-run with --force to apply the change as it is")
	return b.String()
}

// discovered is what Upgrade finds in the cluster: the control-plane object and
// the MachineDeployments, with the GVKs they were read at.
type discovered struct {
	cp    controlPlaneKind
	cpObj *unstructured.Unstructured
	cpGVK schema.GroupVersionKind
	mds   []*unstructured.Unstructured
	mdGVK schema.GroupVersionKind
}

// Upgrade raises a cluster's Kubernetes version. It is pure Cluster API
// orchestration against the cluster's own API: it discovers the control-plane
// object and the MachineDeployments, checks the version skew, patches the
// control plane's spec.version (optionally waiting for the rolling replace),
// then patches each MachineDeployment's version (optionally waiting for the
// rollout). It holds no provider or workload opinion; the version bump is
// identical across infrastructure providers.
func Upgrade(ctx context.Context, deps Deps, name string, opts UpgradeOptions) error {
	if err := deps.check(false); err != nil {
		return err
	}
	if opts.ControlPlaneOnly && opts.WorkersOnly {
		return errors.New("klotho: --control-plane-only and --workers-only are mutually exclusive")
	}
	doKube := opts.KubernetesVersion != ""
	doComponents := opts.Components.Requested()
	doAutoscaler := opts.Autoscaler.Requested()
	if !doKube && !doComponents && !doAutoscaler {
		return errors.New("klotho: nothing to upgrade: set --kubernetes-version and/or a component flag (--capi-version, --provider-version, --operator-version, an image flag) and/or an autoscaler flag (--autoscaler, --autoscaler-min/-max, --autoscaler-version)")
	}

	// Parse the target version up front so a bad value fails before connecting.
	var target semver.Version
	if doKube {
		var err error
		target, err = parseUpgradeVersion(opts.KubernetesVersion)
		if err != nil {
			return err
		}
	}
	log := deps.log().With("cluster", name)

	st, err := Load(ctx, deps, name)
	if err != nil {
		return err
	}
	if st.Reserved {
		return fmt.Errorf("cluster %s is only a name reservation; nothing to upgrade", name)
	}
	cl, err := deps.connectAdmin(ctx, st)
	if err != nil {
		return err
	}

	// Components first: a management-plane upgrade rolls the controllers before
	// the node rollout, so the newer controllers manage it. When only components
	// are requested this is the whole upgrade.
	if doComponents {
		if err := upgradeComponents(ctx, deps, cl, opts.Components, opts.Force, log); err != nil {
			return err
		}
	}

	if doKube {
		if err := upgradeKubernetes(ctx, deps, cl, name, target, opts, log.With("version", "v"+target.String())); err != nil {
			return err
		}
	}

	// Autoscaler last: an install/retune/remove of the add-on is independent of
	// the version bumps and stamps the worker pools as they are after any
	// rollout.
	if doAutoscaler {
		if err := upgradeAutoscaler(ctx, cl, name, opts.Autoscaler, log); err != nil {
			return err
		}
	}

	log.Info("upgrade complete")
	return nil
}

// upgradeKubernetes is the node-version upgrade: pure, provider-agnostic Cluster
// API orchestration that discovers the control-plane object and the
// MachineDeployments, checks the version skew, and patches spec.version (control
// plane first, then workers), optionally waiting for each rollout.
func upgradeKubernetes(ctx context.Context, deps Deps, cl kube.Cluster, name string, target semver.Version, opts UpgradeOptions, log *slog.Logger) error {
	d, err := discover(ctx, cl, name)
	if err != nil {
		return err
	}
	cpVersion, err := d.cp.SpecVersion(d.cpObj)
	if err != nil {
		return fmt.Errorf("reading %s spec.version: %w", d.cp.Kind(), err)
	}
	log.Info("discovered cluster topology",
		"controlPlane", d.cp.Kind()+"/"+d.cpObj.GetName(),
		"controlPlaneVersion", cpVersion,
		"machineDeployments", len(d.mds))

	findings, err := checkSkew(skewInput{
		cpVersion:      cpVersion,
		mds:            machineDeploymentVersions(d.mds),
		target:         target,
		includeCP:      opts.includeControlPlane(),
		includeWorkers: opts.includeWorkers(),
	})
	if err != nil {
		return err
	}
	for _, f := range findings {
		log.Warn("skew finding", "object", f.Object, "field", f.Field, "detail", f.Message)
	}
	if len(findings) > 0 {
		if !opts.Force {
			return &UpgradeFindingsError{Findings: findings}
		}
		log.Warn("proceeding despite skew findings", "findings", len(findings), "force", true)
	}

	ns := d.cpObj.GetNamespace()
	if ns == "" {
		ns = "default"
	}

	if opts.includeControlPlane() {
		if err := upgradeControlPlane(ctx, deps, cl, d, ns, "v"+target.String(), target, opts, log); err != nil {
			return err
		}
	} else {
		log.Info("skipping the control plane (--workers-only)")
	}

	if opts.includeWorkers() {
		if err := upgradeWorkers(ctx, deps, cl, d, ns, "v"+target.String(), target, opts, log); err != nil {
			return err
		}
	} else {
		log.Info("skipping the workers (--control-plane-only)")
	}
	return nil
}

// upgradeControlPlane patches the control-plane object's version (and, when an
// image is given, repoints its machine template at a new one first), then waits
// for the rolling replace when Watch is set.
func upgradeControlPlane(ctx context.Context, deps Deps, cl kube.Cluster, d *discovered, ns, targetStr string, target semver.Version, opts UpgradeOptions, log *slog.Logger) error {
	patch := map[string]any{"spec": map[string]any{"version": targetStr}}
	if opts.ControlPlaneImage != "" {
		newRef, err := swapInfraTemplate(ctx, cl, d.cpObj, d.cp.InfraRefPath(), ns, targetStr, opts.imageField(), opts.ControlPlaneImage, log)
		if err != nil {
			return err
		}
		if err := setNestedPatch(patch, newRef, d.cp.InfraRefPath()...); err != nil {
			return err
		}
	}
	data, err := json.Marshal(patch)
	if err != nil {
		return err
	}
	log.Info("patching the control plane version", "kind", d.cp.Kind(), "name", d.cpObj.GetName())
	if err := cl.Patch(ctx, d.cpGVK, ns, d.cpObj.GetName(), types.MergePatchType, data); err != nil {
		return fmt.Errorf("patching %s %s: %w", d.cp.Kind(), d.cpObj.GetName(), err)
	}
	if !opts.Watch {
		log.Info("control plane version patched; the controller rolls the machines (not waiting, no --watch)")
		return nil
	}
	replicas := nestedInt(d.cpObj, 1, "spec", "replicas")
	clusterName := d.cpObj.GetLabels()[LabelClusterName]
	return waitFor(ctx, deps, opts.watchTimeout(), "the control plane rollout", func(ctx context.Context) (bool, string) {
		return machinesRolledOut(ctx, cl, ns, controlPlaneSelector(clusterName), replicas, target)
	})
}

// upgradeWorkers patches each MachineDeployment's version (and, when an image is
// given, repoints its machine template first), waiting for each rollout when
// Watch is set before moving to the next.
func upgradeWorkers(ctx context.Context, deps Deps, cl kube.Cluster, d *discovered, ns, targetStr string, target semver.Version, opts UpgradeOptions, log *slog.Logger) error {
	for _, md := range d.mds {
		patch := map[string]any{"spec": map[string]any{"template": map[string]any{"spec": map[string]any{"version": targetStr}}}}
		if opts.WorkerImage != "" {
			refPath := []string{"spec", "template", "spec", "infrastructureRef"}
			newRef, err := swapInfraTemplate(ctx, cl, md, refPath, ns, targetStr, opts.imageField(), opts.WorkerImage, log)
			if err != nil {
				return err
			}
			if err := setNestedPatch(patch, newRef, refPath...); err != nil {
				return err
			}
		}
		data, err := json.Marshal(patch)
		if err != nil {
			return err
		}
		log.Info("patching the MachineDeployment version", "name", md.GetName())
		if err := cl.Patch(ctx, d.mdGVK, ns, md.GetName(), types.MergePatchType, data); err != nil {
			return fmt.Errorf("patching MachineDeployment %s: %w", md.GetName(), err)
		}
		if !opts.Watch {
			continue
		}
		replicas := nestedInt(md, 1, "spec", "replicas")
		name := md.GetName()
		if err := waitFor(ctx, deps, opts.watchTimeout(), "the "+name+" rollout", func(ctx context.Context) (bool, string) {
			return machinesRolledOut(ctx, cl, ns, map[string]string{LabelDeploymentName: name}, replicas, target)
		}); err != nil {
			return err
		}
	}
	if !opts.Watch && len(d.mds) > 0 {
		log.Info("MachineDeployment versions patched; the controller rolls the workers (not waiting, no --watch)")
	}
	return nil
}

// discover finds the control-plane object and the MachineDeployments of a
// cluster by its Cluster API cluster-name label.
func discover(ctx context.Context, cl kube.Cluster, name string) (*discovered, error) {
	d := &discovered{}
	for _, k := range knownControlPlaneKinds() {
		for _, ver := range controlPlaneContractVersions {
			gvk := schema.GroupVersionKind{Group: generate.ControlPlaneGroup, Version: ver, Kind: k.Kind()}
			objs, err := cl.List(ctx, gvk, "")
			if err != nil {
				continue // kind not installed, or version not served
			}
			if obj := matchCluster(objs, name); obj != nil {
				d.cp, d.cpObj, d.cpGVK = k, obj, gvk
				break
			}
		}
		if d.cpObj != nil {
			break
		}
	}
	if d.cpObj == nil {
		return nil, fmt.Errorf("no control plane object found for cluster %q (looked for %s)", name, controlPlaneKindNames())
	}

	for _, ver := range controlPlaneContractVersions {
		gvk := schema.GroupVersionKind{Group: generate.ClusterGroup, Version: ver, Kind: "MachineDeployment"}
		objs, err := cl.List(ctx, gvk, "")
		if err != nil {
			continue
		}
		d.mdGVK = gvk
		for _, o := range objs {
			if belongsToCluster(o, name) {
				d.mds = append(d.mds, o)
			}
		}
		break
	}
	return d, nil
}

// matchCluster returns the first object labeled for cluster name.
func matchCluster(objs []*unstructured.Unstructured, name string) *unstructured.Unstructured {
	for _, o := range objs {
		if o.GetLabels()[LabelClusterName] == name {
			return o
		}
	}
	return nil
}

// belongsToCluster reports whether a MachineDeployment is part of cluster name,
// by its label or its spec.clusterName.
func belongsToCluster(o *unstructured.Unstructured, name string) bool {
	if o.GetLabels()[LabelClusterName] == name {
		return true
	}
	cn, _, _ := unstructured.NestedString(o.Object, "spec", "clusterName")
	return cn == name
}

func controlPlaneKindNames() string {
	var names []string
	for _, k := range knownControlPlaneKinds() {
		names = append(names, k.Kind())
	}
	return strings.Join(names, ", ")
}

// controlPlaneSelector is the label set that identifies a cluster's control
// plane Machines: the control-plane marker label (present with any value) and,
// when known, the cluster name. Every Cluster API control-plane provider sets
// both on the Machines it owns.
func controlPlaneSelector(clusterName string) map[string]string {
	sel := map[string]string{LabelControlPlane: ""}
	if clusterName != "" {
		sel[LabelClusterName] = clusterName
	}
	return sel
}

// machinesRolledOut reports whether every Machine matching selector is at the
// target version with a node, no Machine is being deleted, and the count has
// settled at replicas (the rolling replace has finished). The detail string is
// a progress line.
func machinesRolledOut(ctx context.Context, cl kube.Cluster, ns string, selector map[string]string, replicas int, target semver.Version) (bool, string) {
	all, err := cl.List(ctx, machineGVK, ns)
	if err != nil {
		return false, err.Error()
	}
	var total, atTarget, ready, deleting int
	for _, m := range all {
		if !hasLabels(m, selector) {
			continue
		}
		total++
		if m.GetDeletionTimestamp() != nil {
			deleting++
		}
		v, _, _ := unstructured.NestedString(m.Object, "spec", "version")
		mv, err := parseUpgradeVersion(v)
		if err == nil && mv.EQ(target) {
			atTarget++
		}
		if ref, ok, _ := unstructured.NestedMap(m.Object, "status", "nodeRef"); ok && len(ref) > 0 {
			ready++
		}
	}
	detail := fmt.Sprintf("%d/%d machines at v%s, %d ready, %d deleting", atTarget, replicas, target.String(), ready, deleting)
	done := total == replicas && atTarget == replicas && ready == replicas && deleting == 0
	return done, detail
}

func hasLabels(o *unstructured.Unstructured, want map[string]string) bool {
	got := o.GetLabels()
	for k, v := range want {
		cur, ok := got[k]
		if !ok {
			return false
		}
		if v != "" && cur != v {
			return false
		}
	}
	return true
}

// swapInfraTemplate creates a new infrastructure MachineTemplate that copies the
// one obj references at refPath, with one field under spec.template.spec set to
// image, applies it, and returns the reference map to point obj at. Cluster API
// MachineTemplates are immutable once referenced, so a version bump that must
// also change the node image creates a new template and repoints the ref.
func swapInfraTemplate(ctx context.Context, cl kube.Cluster, obj *unstructured.Unstructured, refPath []string, ns, versionStr, field, image string, log *slog.Logger) (map[string]any, error) {
	ref, _, err := unstructured.NestedMap(obj.Object, refPath...)
	if err != nil || ref == nil {
		return nil, fmt.Errorf("reading %s on %s %s: %w", strings.Join(refPath, "."), obj.GetKind(), obj.GetName(), err)
	}
	kind, _ := ref["kind"].(string)
	group, _ := ref["apiGroup"].(string)
	if group == "" {
		group = generate.InfrastructureGroup
	}
	curName, _ := ref["name"].(string)
	if kind == "" || curName == "" {
		return nil, fmt.Errorf("%s %s has no infrastructure template kind/name at %s", obj.GetKind(), obj.GetName(), strings.Join(refPath, "."))
	}

	var cur *unstructured.Unstructured
	for _, ver := range []string{"v1beta2", "v1beta1", "v1alpha1"} {
		gvk := schema.GroupVersionKind{Group: group, Version: ver, Kind: kind}
		u, gerr := cl.Get(ctx, gvk, ns, curName)
		if gerr == nil {
			cur = u
			break
		}
	}
	if cur == nil {
		return nil, fmt.Errorf("reading infrastructure template %s/%s", kind, curName)
	}

	newName := curName + "-" + strings.NewReplacer(".", "-", "+", "-").Replace(strings.TrimPrefix(versionStr, "v"))
	nt := cur.DeepCopy()
	nt.SetName(newName)
	nt.SetResourceVersion("")
	nt.SetUID("")
	unstructured.RemoveNestedField(nt.Object, "status")
	if err := unstructured.SetNestedField(nt.Object, image, "spec", "template", "spec", field); err != nil {
		return nil, fmt.Errorf("setting spec.template.spec.%s: %w", field, err)
	}
	log.Info("creating a new infrastructure template for the target image", "kind", kind, "name", newName, field, image)
	if err := cl.Apply(ctx, kube.FieldManager, nt); err != nil {
		return nil, fmt.Errorf("applying new infrastructure template %s/%s: %w", kind, newName, err)
	}
	newRef := map[string]any{"kind": kind, "name": newName}
	if ag, ok := ref["apiGroup"]; ok {
		newRef["apiGroup"] = ag
	}
	return newRef, nil
}

// setNestedPatch writes value at path inside a patch map, creating intermediate
// maps as needed.
func setNestedPatch(patch map[string]any, value any, path ...string) error {
	m := patch
	for _, p := range path[:len(path)-1] {
		next, ok := m[p].(map[string]any)
		if !ok {
			next = map[string]any{}
			m[p] = next
		}
		m = next
	}
	m[path[len(path)-1]] = value
	return nil
}

func nestedInt(o *unstructured.Unstructured, fallback int, path ...string) int {
	v, ok, err := unstructured.NestedInt64(o.Object, path...)
	if err != nil || !ok {
		return fallback
	}
	return int(v)
}

// mdVersion pairs a MachineDeployment name with its template version.
type mdVersion struct {
	name    string
	version string
}

func machineDeploymentVersions(mds []*unstructured.Unstructured) []mdVersion {
	out := make([]mdVersion, 0, len(mds))
	for _, md := range mds {
		v, _, _ := unstructured.NestedString(md.Object, "spec", "template", "spec", "version")
		out = append(out, mdVersion{name: md.GetName(), version: v})
	}
	return out
}

// skewInput is the data checkSkew reasons over.
type skewInput struct {
	cpVersion      string
	mds            []mdVersion
	target         semver.Version
	includeCP      bool
	includeWorkers bool
}

// checkSkew returns the findings that block an upgrade under the Cluster API
// version skew policy: the control plane may advance at most one minor at a
// time and never downgrade; workers may never run ahead of the control plane
// and may advance at most one minor at a time. A finding is advice the caller
// may override with Force; a malformed current version is an error.
func checkSkew(in skewInput) ([]provider.Finding, error) {
	cpv, err := parseUpgradeVersion(in.cpVersion)
	if err != nil {
		return nil, fmt.Errorf("control plane version %q: %w", in.cpVersion, err)
	}
	var findings []provider.Finding

	if in.includeCP {
		if in.target.LT(cpv) {
			findings = append(findings, provider.Finding{
				Object:  "KubeadmControlPlane",
				Field:   "spec.version",
				Message: fmt.Sprintf("target v%s is a downgrade from the control plane's v%s; Cluster API does not support downgrading the control plane", in.target, cpv),
			})
		} else if jump := minorJump(cpv, in.target); jump > 1 {
			findings = append(findings, provider.Finding{
				Object:  "KubeadmControlPlane",
				Field:   "spec.version",
				Message: fmt.Sprintf("target v%s is %d minor versions above the control plane's v%s; Cluster API allows at most one minor per upgrade", in.target, jump, cpv),
			})
		}
	}

	// The version the control plane will be at when the workers are patched.
	effectiveCP := cpv
	if in.includeCP {
		effectiveCP = in.target
	}
	if in.includeWorkers {
		for _, md := range in.mds {
			if in.target.GT(effectiveCP) {
				findings = append(findings, provider.Finding{
					Object:  "MachineDeployment/" + md.name,
					Field:   "spec.template.spec.version",
					Message: fmt.Sprintf("target v%s is ahead of the control plane's v%s; workers must not run a newer Kubernetes than the control plane", in.target, effectiveCP),
				})
			}
			mv, err := parseUpgradeVersion(md.version)
			if err != nil {
				// A MachineDeployment with no parseable version cannot be skew-checked;
				// leave it to the caller, do not block.
				continue
			}
			if in.target.LT(mv) {
				findings = append(findings, provider.Finding{
					Object:  "MachineDeployment/" + md.name,
					Field:   "spec.template.spec.version",
					Message: fmt.Sprintf("target v%s is a downgrade from this pool's v%s; Cluster API does not support downgrading workers", in.target, mv),
				})
			} else if jump := minorJump(mv, in.target); jump > 1 {
				findings = append(findings, provider.Finding{
					Object:  "MachineDeployment/" + md.name,
					Field:   "spec.template.spec.version",
					Message: fmt.Sprintf("target v%s is %d minor versions above this pool's v%s; Cluster API allows at most one minor per upgrade", in.target, jump, mv),
				})
			}
		}
	}
	return findings, nil
}

// minorJump returns how many minor versions target is above from, within the
// same major. A different (higher) major returns a large number so it reads as
// more than one minor; a lower-or-equal version returns 0.
func minorJump(from, target semver.Version) int {
	if target.Major != from.Major {
		if target.Major > from.Major {
			return int(target.Major-from.Major) * 1000
		}
		return 0
	}
	if target.Minor <= from.Minor {
		return 0
	}
	return int(target.Minor - from.Minor)
}

// parseUpgradeVersion parses a Kubernetes version, tolerating a leading v and
// build metadata such as the +k0s.0 a k0s version carries.
func parseUpgradeVersion(s string) (semver.Version, error) {
	s = strings.TrimSpace(s)
	if s == "" {
		return semver.Version{}, errors.New("is empty")
	}
	v, err := semver.ParseTolerant(s)
	if err != nil {
		return semver.Version{}, fmt.Errorf("%q is not a Kubernetes version: %w", s, err)
	}
	return v, nil
}
