package kube

import (
	"context"
	"fmt"
	"sync"
	"time"

	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/client-go/discovery"
	"k8s.io/client-go/discovery/cached/memory"
	"k8s.io/client-go/dynamic"
	"k8s.io/client-go/restmapper"
	"k8s.io/client-go/tools/clientcmd"
)

// FieldManager is the server-side apply field manager klotho uses.
const FieldManager = "klotho"

// Cluster is the slice of a Kubernetes API Create drives. Every method is a
// single attempt; Create does the waiting and retrying, so a fake needs no
// clock.
type Cluster interface {
	// Ready returns nil when the API server's /readyz answers ok.
	Ready(ctx context.Context) error
	// Apply server-side applies obj with the given field manager, forcing
	// conflicts. Namespaced objects without a namespace go to "default".
	Apply(ctx context.Context, manager string, obj *unstructured.Unstructured) error
	// Get reads one object.
	Get(ctx context.Context, gvk schema.GroupVersionKind, namespace, name string) (*unstructured.Unstructured, error)
	// Patch patches one object with field manager FieldManager.
	Patch(ctx context.Context, gvk schema.GroupVersionKind, namespace, name string, pt types.PatchType, data []byte) error
	// Delete removes one object. A missing object is not an error.
	Delete(ctx context.Context, gvk schema.GroupVersionKind, namespace, name string) error
	// List returns every object of a kind in a namespace. An empty namespace
	// lists across all namespaces for a namespaced kind.
	List(ctx context.Context, gvk schema.GroupVersionKind, namespace string) ([]*unstructured.Unstructured, error)
}

// NewCluster returns a client-go Cluster for kubeconfig. Each request is
// bounded by 30 seconds. Discovery is cached and refreshed when a kind is
// not found, since Create installs CRDs while it runs.
func New(_ context.Context, kubeconfig []byte) (Cluster, error) {
	cfg, err := clientcmd.RESTConfigFromKubeConfig(kubeconfig)
	if err != nil {
		return nil, fmt.Errorf("klotho: admin kubeconfig: %w", err)
	}
	cfg.Timeout = 30 * time.Second
	cfg.UserAgent = "klotho"
	dc, err := discovery.NewDiscoveryClientForConfig(cfg)
	if err != nil {
		return nil, fmt.Errorf("klotho: discovery client: %w", err)
	}
	dyn, err := dynamic.NewForConfig(cfg)
	if err != nil {
		return nil, fmt.Errorf("klotho: dynamic client: %w", err)
	}
	cached := memory.NewMemCacheClient(dc)
	return &kubeCluster{
		disc:   dc,
		dyn:    dyn,
		mapper: restmapper.NewDeferredDiscoveryRESTMapper(cached),
	}, nil
}

type kubeCluster struct {
	disc   *discovery.DiscoveryClient
	dyn    dynamic.Interface
	mu     sync.Mutex
	mapper *restmapper.DeferredDiscoveryRESTMapper
}

func (k *kubeCluster) Ready(ctx context.Context) error {
	body, err := k.disc.RESTClient().Get().AbsPath("/readyz").DoRaw(ctx)
	if err != nil {
		return fmt.Errorf("readyz: %w", err)
	}
	if string(body) != "ok" {
		return fmt.Errorf("readyz: %q", body)
	}
	return nil
}

func (k *kubeCluster) mapping(gvk schema.GroupVersionKind) (*meta.RESTMapping, error) {
	k.mu.Lock()
	defer k.mu.Unlock()
	m, err := k.mapper.RESTMapping(gvk.GroupKind(), gvk.Version)
	if meta.IsNoMatchError(err) {
		k.mapper.Reset()
		m, err = k.mapper.RESTMapping(gvk.GroupKind(), gvk.Version)
	}
	return m, err
}

func (k *kubeCluster) resource(gvk schema.GroupVersionKind, namespace string) (dynamic.ResourceInterface, error) {
	m, err := k.mapping(gvk)
	if err != nil {
		return nil, err
	}
	if m.Scope.Name() == meta.RESTScopeNameNamespace {
		if namespace == "" {
			namespace = "default"
		}
		return k.dyn.Resource(m.Resource).Namespace(namespace), nil
	}
	return k.dyn.Resource(m.Resource), nil
}

// listResource returns the interface List queries: for a namespaced kind an
// empty namespace lists across all namespaces instead of defaulting to
// "default".
func (k *kubeCluster) listResource(gvk schema.GroupVersionKind, namespace string) (dynamic.ResourceInterface, error) {
	m, err := k.mapping(gvk)
	if err != nil {
		return nil, err
	}
	if m.Scope.Name() == meta.RESTScopeNameNamespace && namespace != "" {
		return k.dyn.Resource(m.Resource).Namespace(namespace), nil
	}
	return k.dyn.Resource(m.Resource), nil
}

func (k *kubeCluster) Apply(ctx context.Context, manager string, obj *unstructured.Unstructured) error {
	ri, err := k.resource(obj.GroupVersionKind(), obj.GetNamespace())
	if err != nil {
		return fmt.Errorf("%s: %w", describeObj(obj), err)
	}
	o := obj.DeepCopy()
	o.SetManagedFields(nil)
	o.SetResourceVersion("")
	o.SetUID("")
	if _, err := ri.Apply(ctx, o.GetName(), o, metav1.ApplyOptions{FieldManager: manager, Force: true}); err != nil {
		return fmt.Errorf("applying %s: %w", describeObj(obj), err)
	}
	return nil
}

func (k *kubeCluster) Get(ctx context.Context, gvk schema.GroupVersionKind, namespace, name string) (*unstructured.Unstructured, error) {
	ri, err := k.resource(gvk, namespace)
	if err != nil {
		return nil, err
	}
	return ri.Get(ctx, name, metav1.GetOptions{})
}

func (k *kubeCluster) Patch(ctx context.Context, gvk schema.GroupVersionKind, namespace, name string, pt types.PatchType, data []byte) error {
	ri, err := k.resource(gvk, namespace)
	if err != nil {
		return err
	}
	_, err = ri.Patch(ctx, name, pt, data, metav1.PatchOptions{FieldManager: FieldManager})
	return err
}

func (k *kubeCluster) Delete(ctx context.Context, gvk schema.GroupVersionKind, namespace, name string) error {
	ri, err := k.resource(gvk, namespace)
	if err != nil {
		return err
	}
	if err := ri.Delete(ctx, name, metav1.DeleteOptions{}); err != nil && !apierrors.IsNotFound(err) {
		return err
	}
	return nil
}

func (k *kubeCluster) List(ctx context.Context, gvk schema.GroupVersionKind, namespace string) ([]*unstructured.Unstructured, error) {
	ri, err := k.listResource(gvk, namespace)
	if err != nil {
		return nil, err
	}
	l, err := ri.List(ctx, metav1.ListOptions{})
	if err != nil {
		return nil, err
	}
	out := make([]*unstructured.Unstructured, len(l.Items))
	for i := range l.Items {
		out[i] = &l.Items[i]
	}
	return out, nil
}

func describeObj(o *unstructured.Unstructured) string {
	s := o.GetKind() + " "
	if ns := o.GetNamespace(); ns != "" {
		s += ns + "/"
	}
	return s + o.GetName()
}
