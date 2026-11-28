package klotho

import (
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"strconv"

	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/apimachinery/pkg/types"

	"github.com/klotho-k8s/klotho/pkg/autoscaler"
	"github.com/klotho-k8s/klotho/pkg/kube"
)

// AutoscalerChange is the day-2 cluster-autoscaler request carried in
// UpgradeOptions. It installs or enables the add-on on an existing cluster,
// retunes the node-group min/max (patching the MachineDeployment annotations),
// bumps the autoscaler image, or removes it. The add-on is klotho's own objects
// and the min/max annotations are klotho's own stamp, so changing them does not
// edit a user's cluster YAML.
type AutoscalerChange struct {
	// Set reports whether --autoscaler was given, and Enabled its value.
	// --autoscaler=false removes the add-on; --autoscaler (or =true) installs or
	// updates it.
	Set     bool
	Enabled bool
	// Min and Max retune the node-group bounds; MinSet / MaxSet report whether
	// each was given, so an unset bound keeps a pool's existing annotation.
	Min    int
	MinSet bool
	Max    int
	MaxSet bool
	// Version is the autoscaler image tag; Image fully overrides the image.
	// Either one bumps the running Deployment.
	Version string
	Image   string
}

// Requested reports whether any autoscaler change was asked for.
func (c AutoscalerChange) Requested() bool {
	return c.Set || c.MinSet || c.MaxSet || c.Version != "" || c.Image != ""
}

// removing reports whether the request disables the add-on.
func (c AutoscalerChange) removing() bool { return c.Set && !c.Enabled }

// upgradeAutoscaler performs the day-2 autoscaler change against a live cluster:
// it discovers the control plane and worker MachineDeployments, then either
// removes the add-on (deletes the install objects and strips the min/max
// annotations) or installs/updates it (server-side applies the install objects
// at the target image and patches each worker pool's min/max annotations). It
// reports advisory findings (no worker pool, version skew, scale-from-zero
// caveat) through the logger; none blocks.
func upgradeAutoscaler(ctx context.Context, cl kube.Cluster, name string, change AutoscalerChange, log *slog.Logger) error {
	d, err := discover(ctx, cl, name)
	if err != nil {
		return err
	}
	cpVersion, err := d.cp.SpecVersion(d.cpObj)
	if err != nil {
		return fmt.Errorf("reading %s spec.version: %w", d.cp.Kind(), err)
	}
	ns := d.cpObj.GetNamespace()
	if ns == "" {
		ns = "default"
	}

	if change.removing() {
		return removeAutoscaler(ctx, cl, name, cpVersion, d, log)
	}

	opts := autoscaler.Options{
		ClusterName:       name,
		MachineNamespace:  ns,
		Version:           change.Version,
		Image:             change.Image,
		KubernetesVersion: cpVersion,
	}
	install, err := autoscaler.InstallObjects(opts)
	if err != nil {
		return err
	}
	log.Info("installing/updating the cluster-autoscaler add-on", "objects", len(install), "machineDeployments", len(d.mds))
	for _, obj := range install {
		if err := cl.Apply(ctx, kube.FieldManager, obj); err != nil {
			return fmt.Errorf("applying autoscaler %s %s: %w", obj.GetKind(), obj.GetName(), err)
		}
	}

	for _, md := range d.mds {
		min, max := resolveSizes(md, change)
		if err := patchMDAnnotations(ctx, cl, d.mdGVK, ns, md.GetName(), autoscaler.MinMaxAnnotations(min, max)); err != nil {
			return err
		}
		log.Info("stamped autoscaler bounds", "machineDeployment", md.GetName(), "min", min, "max", max)
		if min == 0 {
			log.Warn("autoscaler finding: min 0 enables scale-to-zero, but a pool only grows 0->1 when the autoscaler can read a would-be node's capacity; klotho grants read of the infrastructure MachineTemplate, which works only where the provider publishes template capacity - otherwise set the capacity.cluster-autoscaler.kubernetes.io/* annotations by hand (see docs/AUTOSCALER.md)",
				"machineDeployment", md.GetName())
		}
	}
	if len(d.mds) == 0 {
		log.Warn("autoscaler finding: the cluster has no worker MachineDeployment to scale; the autoscaler installs but has nothing to autoscale until a worker pool exists",
			"cluster", name)
	}
	if skew := opts.SkewFinding(); skew != "" {
		log.Warn("autoscaler finding: " + skew)
	}
	return nil
}

// removeAutoscaler deletes the add-on's objects and strips the min/max
// annotations klotho stamped on the worker MachineDeployments.
func removeAutoscaler(ctx context.Context, cl kube.Cluster, name, cpVersion string, d *discovered, log *slog.Logger) error {
	// Render the object set (sizes are irrelevant to the object list) to know
	// what to delete.
	install, err := autoscaler.InstallObjects(autoscaler.Options{ClusterName: name, KubernetesVersion: cpVersion})
	if err != nil {
		return err
	}
	ns := d.cpObj.GetNamespace()
	if ns == "" {
		ns = "default"
	}
	log.Info("removing the cluster-autoscaler add-on", "objects", len(install))
	for _, obj := range install {
		if err := cl.Delete(ctx, obj.GroupVersionKind(), obj.GetNamespace(), obj.GetName()); err != nil {
			return fmt.Errorf("deleting autoscaler %s %s: %w", obj.GetKind(), obj.GetName(), err)
		}
	}
	// Strip klotho's node-group annotations (null deletes them in a merge patch).
	for _, md := range d.mds {
		clear := map[string]string{autoscaler.MinSizeAnnotation: "", autoscaler.MaxSizeAnnotation: ""}
		if err := patchMDAnnotationsNull(ctx, cl, d.mdGVK, ns, md.GetName(), clear); err != nil {
			return err
		}
		log.Info("cleared autoscaler bounds", "machineDeployment", md.GetName())
	}
	return nil
}

// resolveSizes computes a MachineDeployment's min/max for a change: an explicit
// --autoscaler-min / -max wins, else the pool's existing annotation, else the
// module default.
func resolveSizes(md *unstructured.Unstructured, change AutoscalerChange) (min, max int) {
	min = annotationInt(md, autoscaler.MinSizeAnnotation, autoscaler.DefaultMinSize)
	max = annotationInt(md, autoscaler.MaxSizeAnnotation, autoscaler.DefaultMaxSize)
	if change.MinSet {
		min = change.Min
	}
	if change.MaxSet {
		max = change.Max
	}
	if max < 1 {
		max = 1
	}
	if min > max {
		min = max
	}
	if min < 0 {
		min = 0
	}
	return min, max
}

// annotationInt reads an integer annotation, falling back to def when absent or
// unparseable.
func annotationInt(o *unstructured.Unstructured, key string, def int) int {
	v, ok := o.GetAnnotations()[key]
	if !ok {
		return def
	}
	n, err := strconv.Atoi(v)
	if err != nil {
		return def
	}
	return n
}

// patchMDAnnotations merge-patches metadata.annotations onto a MachineDeployment.
func patchMDAnnotations(ctx context.Context, cl kube.Cluster, gvk schema.GroupVersionKind, ns, name string, anns map[string]string) error {
	m := make(map[string]any, len(anns))
	for k, v := range anns {
		m[k] = v
	}
	data, err := json.Marshal(map[string]any{"metadata": map[string]any{"annotations": m}})
	if err != nil {
		return err
	}
	if err := cl.Patch(ctx, gvk, ns, name, types.MergePatchType, data); err != nil {
		return fmt.Errorf("patching MachineDeployment %s annotations: %w", name, err)
	}
	return nil
}

// patchMDAnnotationsNull merge-patches the given annotation keys to null, which
// removes them.
func patchMDAnnotationsNull(ctx context.Context, cl kube.Cluster, gvk schema.GroupVersionKind, ns, name string, keys map[string]string) error {
	m := make(map[string]any, len(keys))
	for k := range keys {
		m[k] = nil
	}
	data, err := json.Marshal(map[string]any{"metadata": map[string]any{"annotations": m}})
	if err != nil {
		return err
	}
	if err := cl.Patch(ctx, gvk, ns, name, types.MergePatchType, data); err != nil {
		return fmt.Errorf("clearing MachineDeployment %s annotations: %w", name, err)
	}
	return nil
}
