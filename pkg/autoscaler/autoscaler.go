// Package autoscaler renders the Kubernetes Cluster Autoscaler add-on for a
// self-managed Cluster API cluster and computes the MachineDeployment
// annotations that drive it. It is the one module both klotho create (inject
// into the generated bundle) and klotho upgrade (apply day-2) use, so the two
// paths install byte-identical objects.
//
// The autoscaler runs the native clusterapi cloud provider
// (--cloud-provider=clusterapi). It is provider-agnostic: it operates on
// MachineDeployments through the Cluster API contract, so it works on every
// infrastructure provider with no per-provider code. It scales the node
// COUNT of a pool between a min and a max; it never changes a node's SIZE.
//
// Self-managed single cluster: management and workload are the same cluster,
// so the autoscaler runs in-cluster and resolves both the Cluster API objects
// and the Nodes through its own ServiceAccount. Per the upstream README, with
// neither --kubeconfig nor --cloud-config set it uses the in-cluster config
// for the workload and falls back to that same config for the management side,
// so the only cloud flag it needs is --cloud-provider=clusterapi.
package autoscaler

import (
	"fmt"
	"strconv"

	"github.com/blang/semver/v4"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
)

// Node-group discovery annotations the autoscaler reads on a MachineDeployment
// (or MachineSet / MachinePool). They go on the object's top-level
// metadata.annotations and bound how far the pool is scaled.
const (
	MinSizeAnnotation = "cluster.x-k8s.io/cluster-api-autoscaler-node-group-min-size"
	MaxSizeAnnotation = "cluster.x-k8s.io/cluster-api-autoscaler-node-group-max-size"
)

// Scale-from-zero capacity annotations. When a pool is at 0 replicas the
// autoscaler has no sample Node to read a would-be node's resources from, so it
// reads them from these annotations on the MachineDeployment (top-level
// metadata.annotations). Without them a pool can grow 1->N but not 0->1. The
// values are provider- and flavor-specific (they describe the VM a template
// clones), so klotho does not guess them; it grants the autoscaler read access
// to the infrastructure MachineTemplate instead, which lets the clusterapi
// provider infer the same capacity where the provider publishes it. These keys
// are exported so a caller that does know the shape can set them, and for the
// tests.
const (
	CapacityPrefix        = "capacity.cluster-autoscaler.kubernetes.io/"
	CapacityCPU           = CapacityPrefix + "cpu"
	CapacityMemory        = CapacityPrefix + "memory"
	CapacityEphemeralDisk = CapacityPrefix + "ephemeral-disk"
	CapacityMaxPods       = CapacityPrefix + "maxPods" // the only camelCase key
	CapacityGPUType       = CapacityPrefix + "gpu-type"
	CapacityGPUCount      = CapacityPrefix + "gpu-count"
	CapacityLabels        = CapacityPrefix + "labels"
	CapacityTaints        = CapacityPrefix + "taints"
)

// Install constants.
const (
	// Name is the shared name of the ServiceAccount, Deployment and the base of
	// the ClusterRole / binding names.
	Name = "cluster-autoscaler"
	// DefaultNamespace is where the autoscaler Deployment, ServiceAccount and
	// leader-election objects live.
	DefaultNamespace = "kube-system"
	// DefaultMachineNamespace is where klotho places the Cluster API objects,
	// so the default namespace the autoscaler discovers MachineDeployments in.
	DefaultMachineNamespace = "default"
	// ImageRepository is the upstream image. The tag tracks the cluster's
	// Kubernetes minor version.
	ImageRepository = "registry.k8s.io/autoscaling/cluster-autoscaler"
	// DefaultMinSize and DefaultMaxSize bound a pool when the user gives none.
	DefaultMinSize = 1
	DefaultMaxSize = 5

	appLabel = "app"
)

// managementRole is the name of the ClusterRole granting access to the Cluster
// API objects; workloadRole grants access to the target cluster's core API.
const (
	managementRole = Name + "-management"
	workloadRole   = Name + "-workload"
)

// Options configure the add-on.
type Options struct {
	// ClusterName scopes node-group auto-discovery to this cluster's
	// MachineDeployments. Required.
	ClusterName string
	// Namespace is where the autoscaler Deployment and RBAC are installed.
	// Empty means DefaultNamespace.
	Namespace string
	// MachineNamespace is where the MachineDeployments live. Empty means
	// DefaultMachineNamespace.
	MachineNamespace string
	// MinSize and MaxSize bound every worker pool. Zero MaxSize means
	// DefaultMaxSize; a negative MinSize is rejected by Validate.
	MinSize int
	MaxSize int
	// Version is the autoscaler image tag, for example "v1.33.4". Empty derives
	// it from KubernetesVersion.
	Version string
	// Image fully overrides the container image ("repo:tag"), for a mirror or a
	// fork. Empty means ImageRepository + ":" + the resolved tag.
	Image string
	// KubernetesVersion is the cluster's Kubernetes version, used to derive the
	// default image tag (minor-matched) and to check version skew.
	KubernetesVersion string
}

// withDefaults fills the zero-value fields.
func (o Options) withDefaults() Options {
	if o.Namespace == "" {
		o.Namespace = DefaultNamespace
	}
	if o.MachineNamespace == "" {
		o.MachineNamespace = DefaultMachineNamespace
	}
	if o.MaxSize == 0 && o.MinSize == 0 {
		o.MinSize = DefaultMinSize
		o.MaxSize = DefaultMaxSize
	} else if o.MaxSize == 0 {
		o.MaxSize = o.MinSize
	}
	return o
}

// Validate checks the sizes and that an image can be resolved.
func (o Options) Validate() error {
	od := o.withDefaults()
	if od.ClusterName == "" {
		return fmt.Errorf("autoscaler: a cluster name is required")
	}
	if od.MinSize < 0 {
		return fmt.Errorf("autoscaler: min size %d is negative", od.MinSize)
	}
	if od.MaxSize < 1 {
		return fmt.Errorf("autoscaler: max size %d must be at least 1", od.MaxSize)
	}
	if od.MinSize > od.MaxSize {
		return fmt.Errorf("autoscaler: min size %d is greater than max size %d", od.MinSize, od.MaxSize)
	}
	if _, err := o.image(); err != nil {
		return err
	}
	return nil
}

// image resolves the container image. Image wins; otherwise the repository with
// the resolved tag.
func (o Options) image() (string, error) {
	if o.Image != "" {
		return o.Image, nil
	}
	tag := o.Version
	if tag == "" {
		tag = o.KubernetesVersion
	}
	if tag == "" {
		return "", fmt.Errorf("autoscaler: no image tag: set --autoscaler-version or the cluster Kubernetes version")
	}
	return ImageRepository + ":" + tag, nil
}

// MinMaxAnnotations returns the two node-group discovery annotations for a
// min/max. It is the single source of the annotation values both paths stamp.
func MinMaxAnnotations(min, max int) map[string]string {
	return map[string]string{
		MinSizeAnnotation: strconv.Itoa(min),
		MaxSizeAnnotation: strconv.Itoa(max),
	}
}

// CapacityAnnotations returns scale-from-zero capacity annotations for a set of
// resource values, keyed by the bare resource name (for example "cpu",
// "memory", "ephemeral-disk", "maxPods", "gpu-type", "gpu-count"). An empty
// value is skipped. klotho does not call this with derived numbers (they are
// provider-specific); it exists so a caller that knows the node shape, and the
// tests, can build the keys correctly.
func CapacityAnnotations(values map[string]string) map[string]string {
	out := map[string]string{}
	for k, v := range values {
		if v == "" {
			continue
		}
		out[CapacityPrefix+k] = v
	}
	return out
}

// InstallObjects renders the add-on: a ServiceAccount, the management and
// workload ClusterRoles and their bindings, and the Deployment running
// --cloud-provider=clusterapi scoped to the cluster's MachineDeployments. The
// objects are provider-agnostic. Order is apply-safe (RBAC before the
// Deployment that uses it).
func InstallObjects(o Options) ([]*unstructured.Unstructured, error) {
	if err := o.Validate(); err != nil {
		return nil, err
	}
	od := o.withDefaults()
	image, err := od.image()
	if err != nil {
		return nil, err
	}
	labels := map[string]any{appLabel: Name}

	sa := object("v1", "ServiceAccount", Name, od.Namespace)
	setLabels(sa, labels)

	mgmt := clusterRole(managementRole, labels, managementRules())
	work := clusterRole(workloadRole, labels, workloadRules())
	mgmtBinding := clusterRoleBinding(managementRole, managementRole, Name, od.Namespace, labels)
	workBinding := clusterRoleBinding(workloadRole, workloadRole, Name, od.Namespace, labels)

	deploy := deployment(od, image, labels)

	return []*unstructured.Unstructured{sa, mgmt, work, mgmtBinding, workBinding, deploy}, nil
}

// managementRules grant the clusterapi provider the Cluster API access it needs:
// read/update the MachineDeployments and their scale subresource, and read the
// MachineSets/Machines/MachinePools. The last rule is klotho's provider-agnostic
// scale-from-zero enabler: read any infrastructure MachineTemplate so the
// provider can infer a would-be node's capacity when a pool is at 0 and carries
// no capacity annotations.
func managementRules() []any {
	return []any{
		rule([]string{"cluster.x-k8s.io"},
			[]string{"machinedeployments", "machinedeployments/scale", "machines", "machinesets", "machinepools"},
			[]string{"get", "list", "update", "watch", "patch"}),
		rule([]string{"infrastructure.cluster.x-k8s.io"},
			[]string{"*"},
			[]string{"get", "list", "watch"}),
	}
}

// workloadRules grant the autoscaler the core-API access it needs to see pods,
// nodes and the scheduling inputs, evict pods, and run leader election. For a
// self-managed single cluster the workload cluster is this cluster, so these
// bind to the same ServiceAccount.
func workloadRules() []any {
	return []any{
		rule([]string{""},
			[]string{"namespaces", "persistentvolumeclaims", "persistentvolumes", "pods", "replicationcontrollers", "services"},
			[]string{"get", "list", "watch"}),
		rule([]string{""}, []string{"nodes"}, []string{"get", "list", "update", "watch"}),
		rule([]string{""}, []string{"pods/eviction"}, []string{"create"}),
		rule([]string{""}, []string{"events"}, []string{"create", "patch"}),
		rule([]string{""}, []string{"configmaps"}, []string{"create", "delete", "get", "update"}),
		rule([]string{"coordination.k8s.io"}, []string{"leases"}, []string{"create", "get", "update"}),
		rule([]string{"policy"}, []string{"poddisruptionbudgets"}, []string{"list", "watch"}),
		rule([]string{"apps"}, []string{"daemonsets", "replicasets", "statefulsets"}, []string{"list", "watch"}),
		rule([]string{"batch"}, []string{"jobs"}, []string{"list", "watch"}),
		rule([]string{"storage.k8s.io"},
			[]string{"csinodes", "storageclasses", "csidrivers", "csistoragecapacities", "volumeattachments"},
			[]string{"get", "list", "watch"}),
		rule([]string{"resource.k8s.io"},
			[]string{"resourceslices", "deviceclasses", "resourceclaims"},
			[]string{"watch", "list", "get"}),
	}
}

// deployment renders the autoscaler Deployment. It runs on a control plane
// (tolerating the control-plane taint) because a self-managed cluster may have
// no worker nodes, and the management stack lives on the control plane anyway.
func deployment(od Options, image string, labels map[string]any) *unstructured.Unstructured {
	args := []any{
		"/cluster-autoscaler",
		"--cloud-provider=clusterapi",
		fmt.Sprintf("--node-group-auto-discovery=clusterapi:namespace=%s,clusterName=%s", od.MachineNamespace, od.ClusterName),
		"--logtostderr=true",
		"--stderrthreshold=info",
		"--v=4",
	}
	container := map[string]any{
		"name":    Name,
		"image":   image,
		"command": args,
		"resources": map[string]any{
			"requests": map[string]any{"cpu": "100m", "memory": "300Mi"},
		},
	}
	podSpec := map[string]any{
		"serviceAccountName": Name,
		"containers":         []any{container},
		"nodeSelector":       map[string]any{"kubernetes.io/os": "linux"},
		"tolerations": []any{
			map[string]any{"key": "node-role.kubernetes.io/control-plane", "effect": "NoSchedule"},
			map[string]any{"key": "node-role.kubernetes.io/master", "effect": "NoSchedule"},
		},
	}
	u := object("apps/v1", "Deployment", Name, od.Namespace)
	setLabels(u, labels)
	_ = unstructured.SetNestedField(u.Object, int64(1), "spec", "replicas")
	_ = unstructured.SetNestedMap(u.Object, map[string]any{appLabel: Name}, "spec", "selector", "matchLabels")
	_ = unstructured.SetNestedMap(u.Object, map[string]any{appLabel: Name}, "spec", "template", "metadata", "labels")
	_ = unstructured.SetNestedMap(u.Object, podSpec, "spec", "template", "spec")
	return u
}

// --- object helpers ----------------------------------------------------------

func object(apiVersion, kind, name, namespace string) *unstructured.Unstructured {
	u := &unstructured.Unstructured{Object: map[string]any{}}
	u.SetAPIVersion(apiVersion)
	u.SetKind(kind)
	u.SetName(name)
	if namespace != "" {
		u.SetNamespace(namespace)
	}
	return u
}

func setLabels(u *unstructured.Unstructured, labels map[string]any) {
	m := make(map[string]string, len(labels))
	for k, v := range labels {
		m[k] = fmt.Sprint(v)
	}
	u.SetLabels(m)
}

func rule(apiGroups, resources, verbs []string) any {
	return map[string]any{
		"apiGroups": toAny(apiGroups),
		"resources": toAny(resources),
		"verbs":     toAny(verbs),
	}
}

func clusterRole(name string, labels map[string]any, rules []any) *unstructured.Unstructured {
	u := object("rbac.authorization.k8s.io/v1", "ClusterRole", name, "")
	setLabels(u, labels)
	_ = unstructured.SetNestedSlice(u.Object, rules, "rules")
	return u
}

func clusterRoleBinding(name, roleName, saName, saNamespace string, labels map[string]any) *unstructured.Unstructured {
	u := object("rbac.authorization.k8s.io/v1", "ClusterRoleBinding", name, "")
	setLabels(u, labels)
	_ = unstructured.SetNestedMap(u.Object, map[string]any{
		"apiGroup": "rbac.authorization.k8s.io",
		"kind":     "ClusterRole",
		"name":     roleName,
	}, "roleRef")
	_ = unstructured.SetNestedSlice(u.Object, []any{map[string]any{
		"kind":      "ServiceAccount",
		"name":      saName,
		"namespace": saNamespace,
	}}, "subjects")
	return u
}

func toAny(ss []string) []any {
	out := make([]any, len(ss))
	for i, s := range ss {
		out[i] = s
	}
	return out
}

// SkewFinding returns a human-readable reason when the autoscaler version's
// minor does not match the cluster's Kubernetes minor, or "" when they match or
// cannot be parsed. The autoscaler is released per Kubernetes minor and should
// track the control plane's minor.
func (o Options) SkewFinding() string {
	image := o.Version
	if image == "" {
		return "" // tag derived from the Kubernetes version, so minors match by construction
	}
	av, err := semver.ParseTolerant(image)
	if err != nil {
		return ""
	}
	kv, err := semver.ParseTolerant(o.KubernetesVersion)
	if err != nil {
		return ""
	}
	if av.Major == kv.Major && av.Minor == kv.Minor {
		return ""
	}
	return fmt.Sprintf("autoscaler %s is a different minor from the cluster's Kubernetes v%d.%d; the cluster-autoscaler is released per Kubernetes minor and should match the control plane's minor",
		image, kv.Major, kv.Minor)
}
