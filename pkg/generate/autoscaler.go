package generate

import (
	"github.com/pkg/errors"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
)

// SetAutoscaler wires the cluster-autoscaler add-on into a generated bundle: it
// stamps annotations (the node-group min/max, and any scale-from-zero capacity
// keys the caller supplies) onto the top-level metadata.annotations of every
// worker MachineDeployment, appends the install objects, and re-renders b.YAML.
// It returns the number of MachineDeployments it stamped, so the caller can warn
// when a cluster has no worker pool to autoscale. The install objects are the
// caller's (rendered by pkg/autoscaler), so generate stays decoupled from the
// add-on's shape. Applying it twice leaves the bundle as after the first time.
func SetAutoscaler(b *Bundle, install []*unstructured.Unstructured, annotations map[string]string) (int, error) {
	mds := Find(b, ClusterGroup, "MachineDeployment")
	for _, md := range mds {
		anns := md.GetAnnotations()
		if anns == nil {
			anns = map[string]string{}
		}
		for k, v := range annotations {
			anns[k] = v
		}
		md.SetAnnotations(anns)
	}
	// Append the add-on objects that are not already present (idempotent on a
	// second call, so a re-render does not duplicate them).
	for _, obj := range install {
		if containsObject(b.Objects, obj) {
			continue
		}
		b.Objects = append(b.Objects, obj)
	}
	if _, err := b.Render(); err != nil {
		return 0, errors.Wrap(err, "rendering bundle with the autoscaler add-on")
	}
	return len(mds), nil
}

// containsObject reports whether objs already holds an object with the same
// GVK, namespace and name as want.
func containsObject(objs []*unstructured.Unstructured, want *unstructured.Unstructured) bool {
	for _, o := range objs {
		if o.GroupVersionKind() == want.GroupVersionKind() &&
			o.GetNamespace() == want.GetNamespace() && o.GetName() == want.GetName() {
			return true
		}
	}
	return false
}
