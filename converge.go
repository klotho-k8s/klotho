package klotho

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"time"

	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/apimachinery/pkg/types"

	"github.com/klotho-k8s/klotho/pkg/cluster"
	"github.com/klotho-k8s/klotho/pkg/generate"
	"github.com/klotho-k8s/klotho/pkg/kube"
	"github.com/klotho-k8s/klotho/provider/controlplane"
)

// PauseFieldManager owns spec.paused on the Cluster while the bootstrap node
// is adopted. It is separate from FieldManager so applying the bundle, which
// does not mention spec.paused, does not unpause the Cluster.
const PauseFieldManager = "klotho-pause"

// ErrNodeTimeout is wrapped by Create's error when everything was applied
// but the adopted Machine did not report a node in time.
var ErrNodeTimeout = errors.New("timed out waiting for the bootstrap Machine to report its node")

// converge installs Cluster API on the running bootstrap node and makes the
// node adopt itself. Each step is retried until its timeout:
//
//  1. wait for /readyz through the endpoint;
//  2. apply the provider namespaces, the provider Secret and the operator
//     provider objects;
//  3. wait for every operator provider object to report Ready;
//  4. wait for the cluster Secrets (<name>-ca, <name>-kubeconfig) the node
//     seeds, so no controller generates its own CA;
//  5. create the Cluster paused (spec.paused owned by PauseFieldManager), so
//     no controller acts before the bootstrap Machine is owned;
//  6. apply the bundle;
//  7. read the live control plane object for its UID;
//  8. render and apply the adoption objects (control plane provider's, then
//     the infrastructure provider's);
//  9. make the control plane object the controller owner of the Machine;
//  10. unpause the Cluster;
//  11. wait for the Machine's status.nodeRef.
func converge(ctx context.Context, deps Deps, req CreateRequest, st *cluster.State, creds *controlplane.Credentials,
	bundle *generate.Bundle, kcp *unstructured.Unstructured, providerObjs []*unstructured.Unstructured) error {
	t := req.Timeouts.withDefaults()
	log := deps.log().With("cluster", st.Spec.Name)

	connect := deps.Connect
	if connect == nil {
		connect = kube.New
	}
	cl, err := connect(ctx, creds.Kubeconfig)
	if err != nil {
		return err
	}

	clusterObj, err := clusterObject(bundle)
	if err != nil {
		return err
	}
	ns := clusterObj.GetNamespace()
	if ns == "" {
		ns = "default"
	}

	log.Info("waiting for the API server", "endpoint", fmt.Sprintf("%s:%d", st.Endpoint.Host, st.Endpoint.Port), "timeout", t.APIServer)
	if err := waitFor(ctx, deps, t.APIServer, "the API server", func(ctx context.Context) (bool, string) {
		if err := cl.Ready(ctx); err != nil {
			return false, err.Error()
		}
		return true, ""
	}); err != nil {
		return err
	}

	log.Info("installing Cluster API providers", "version", req.CAPI.Version, "infrastructure", req.CAPI.InfraProviderName+"@"+req.CAPI.InfraProviderVersion)
	if err := applyAll(ctx, deps, cl, kube.FieldManager, providerObjs, t.Apply, "the Cluster API providers"); err != nil {
		return err
	}
	for _, ref := range req.CAPI.OperatorProviderRefs() {
		ref := ref
		what := fmt.Sprintf("%s %s/%s to be ready", ref.Kind, ref.Namespace, ref.Name)
		if err := waitFor(ctx, deps, t.Providers, what, func(ctx context.Context) (bool, string) {
			u, err := cl.Get(ctx, generate.OperatorGVK(ref.Kind), ref.Namespace, ref.Name)
			if err != nil {
				return false, err.Error()
			}
			return generate.ProviderReady(u)
		}); err != nil {
			return err
		}
		log.Info("provider ready", "kind", ref.Kind, "name", ref.Name)
	}

	secretGVK := schema.GroupVersionKind{Version: "v1", Kind: "Secret"}
	for _, suffix := range []string{"-ca", "-kubeconfig"} {
		secret := st.Spec.Name + suffix
		if err := waitFor(ctx, deps, t.Providers, "Secret "+ns+"/"+secret+" from the bootstrap node", func(ctx context.Context) (bool, string) {
			if _, err := cl.Get(ctx, secretGVK, ns, secret); err != nil {
				return false, err.Error()
			}
			return true, ""
		}); err != nil {
			return err
		}
	}

	if err := setPaused(ctx, deps, cl, clusterObj, true, t.Apply); err != nil {
		return err
	}
	log.Info("applying the cluster bundle", "objects", len(bundle.Objects))
	if err := applyAll(ctx, deps, cl, kube.FieldManager, bundle.Objects, t.Apply, "the cluster bundle"); err != nil {
		return err
	}

	var live *unstructured.Unstructured
	if err := waitFor(ctx, deps, t.Apply, fmt.Sprintf("%s %s", kcp.GetKind(), kcp.GetName()), func(ctx context.Context) (bool, string) {
		u, err := cl.Get(ctx, kcp.GroupVersionKind(), objNamespace(kcp, ns), kcp.GetName())
		if err != nil {
			return false, err.Error()
		}
		if u.GetUID() == "" {
			return false, "no UID yet"
		}
		live = u
		return true, ""
	}); err != nil {
		return err
	}

	log.Info("adopting the bootstrap node")
	cpAdopt, err := deps.ControlPlane.Adopt(ctx, st, kcp)
	if err != nil {
		return fmt.Errorf("rendering control plane adoption: %w", err)
	}
	infraAdopt, err := deps.Infra.Adopt(ctx, st, bundle.Objects)
	if err != nil {
		return fmt.Errorf("rendering infrastructure adoption: %w", err)
	}
	adopt, err := generate.ParseBundle(joinYAML(cpAdopt, infraAdopt))
	if err != nil {
		return fmt.Errorf("parsing adoption objects: %w", err)
	}
	machine, err := machineObject(adopt)
	if err != nil {
		return err
	}
	if err := applyAll(ctx, deps, cl, kube.FieldManager, adopt.Objects, t.Apply, "the adoption objects"); err != nil {
		return err
	}

	patch, err := ownerPatch(live)
	if err != nil {
		return err
	}
	machineNS := objNamespace(machine, ns)
	if err := waitFor(ctx, deps, t.Apply, "the Machine owner reference", func(ctx context.Context) (bool, string) {
		if err := cl.Patch(ctx, machine.GroupVersionKind(), machineNS, machine.GetName(), types.MergePatchType, patch); err != nil {
			return false, err.Error()
		}
		return true, ""
	}); err != nil {
		return err
	}
	log.Info("bootstrap Machine owned by the control plane", "machine", machine.GetName(), "owner", live.GetKind()+"/"+live.GetName())

	if err := setPaused(ctx, deps, cl, clusterObj, false, t.Apply); err != nil {
		return err
	}

	log.Info("waiting for the bootstrap Machine's node", "machine", machine.GetName(), "timeout", t.Node)
	if err := waitFor(ctx, deps, t.Node, "Machine "+machine.GetName()+" status.nodeRef", func(ctx context.Context) (bool, string) {
		u, err := cl.Get(ctx, machine.GroupVersionKind(), machineNS, machine.GetName())
		if err != nil {
			return false, err.Error()
		}
		if ref, ok, _ := unstructured.NestedMap(u.Object, "status", "nodeRef"); ok && len(ref) > 0 {
			return true, ""
		}
		return false, "no nodeRef yet"
	}); err != nil {
		if ctx.Err() != nil {
			return err
		}
		return fmt.Errorf("%w: %v; the cluster may still converge, check with \"klotho shell %s -- kubectl get machines -A\"",
			ErrNodeTimeout, err, st.Spec.Name)
	}
	return nil
}

func clusterObject(b *generate.Bundle) (*unstructured.Unstructured, error) {
	for _, o := range b.Objects {
		gvk := o.GroupVersionKind()
		if gvk.Group == generate.ClusterGroup && gvk.Kind == "Cluster" {
			return o, nil
		}
	}
	return nil, fmt.Errorf("bundle has no %s Cluster", generate.ClusterGroup)
}

func machineObject(b *generate.Bundle) (*unstructured.Unstructured, error) {
	var found []*unstructured.Unstructured
	for _, o := range b.Objects {
		gvk := o.GroupVersionKind()
		if gvk.Group == generate.ClusterGroup && gvk.Kind == "Machine" {
			found = append(found, o)
		}
	}
	if len(found) != 1 {
		return nil, fmt.Errorf("adoption objects hold %d %s Machines, want 1", len(found), generate.ClusterGroup)
	}
	return found[0], nil
}

func objNamespace(o *unstructured.Unstructured, fallback string) string {
	if ns := o.GetNamespace(); ns != "" {
		return ns
	}
	return fallback
}

// setPaused applies spec.paused on the Cluster as PauseFieldManager.
//
// Pausing creates the Cluster, so it applies the whole object from the
// bundle plus spec.paused: the admission webhook rejects a Cluster without
// its references. The later bundle apply takes every other field over as
// its own manager, leaving PauseFieldManager owning spec.paused alone, which
// is all unpausing then applies.
func setPaused(ctx context.Context, deps Deps, cl kube.Cluster, clusterObj *unstructured.Unstructured, paused bool, timeout time.Duration) error {
	var u *unstructured.Unstructured
	if paused {
		u = clusterObj.DeepCopy()
		unstructured.RemoveNestedField(u.Object, "status")
	} else {
		u = &unstructured.Unstructured{Object: map[string]any{"spec": map[string]any{}}}
		u.SetGroupVersionKind(clusterObj.GroupVersionKind())
		u.SetName(clusterObj.GetName())
		if ns := clusterObj.GetNamespace(); ns != "" {
			u.SetNamespace(ns)
		}
	}
	if err := unstructured.SetNestedField(u.Object, paused, "spec", "paused"); err != nil {
		return fmt.Errorf("setting spec.paused: %w", err)
	}
	what := "pausing the Cluster"
	if !paused {
		what = "unpausing the Cluster"
	}
	deps.log().Info(what, "cluster", clusterObj.GetName())
	return applyAll(ctx, deps, cl, PauseFieldManager, []*unstructured.Unstructured{u}, timeout, what)
}

// ownerPatch is a JSON merge patch making owner the Machine's controller.
// The adopted Machine is new and has no other owner references.
func ownerPatch(owner *unstructured.Unstructured) ([]byte, error) {
	patch := map[string]any{
		"metadata": map[string]any{
			"ownerReferences": []any{map[string]any{
				"apiVersion":         owner.GetAPIVersion(),
				"kind":               owner.GetKind(),
				"name":               owner.GetName(),
				"uid":                string(owner.GetUID()),
				"controller":         true,
				"blockOwnerDeletion": true,
			}},
		},
	}
	return json.Marshal(patch)
}

// applyAll applies objs in order, retrying from the first failure until
// timeout.
func applyAll(ctx context.Context, deps Deps, cl kube.Cluster, manager string, objs []*unstructured.Unstructured, timeout time.Duration, what string) error {
	next := 0
	return waitFor(ctx, deps, timeout, "applying "+what, func(ctx context.Context) (bool, string) {
		for next < len(objs) {
			if err := cl.Apply(ctx, manager, objs[next]); err != nil {
				return false, err.Error()
			}
			next++
		}
		return true, ""
	})
}

// waitFor calls check until it reports done, timeout passes, or ctx ends.
// check's detail is logged when it changes and quoted on timeout.
func waitFor(ctx context.Context, deps Deps, timeout time.Duration, what string, check func(context.Context) (bool, string)) error {
	wctx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()
	log := deps.log()
	last := ""
	for {
		done, detail := check(wctx)
		if done {
			return nil
		}
		if detail != last {
			log.Debug("waiting", "for", what, "detail", detail)
			last = detail
		}
		select {
		case <-wctx.Done():
			if err := ctx.Err(); err != nil {
				return fmt.Errorf("%s: %w", what, err)
			}
			return fmt.Errorf("timed out after %s waiting for %s: %s", timeout, what, last)
		case <-time.After(deps.poll()):
		}
	}
}

func joinYAML(docs ...[]byte) []byte {
	var b bytes.Buffer
	for _, d := range docs {
		if len(bytes.TrimSpace(d)) == 0 {
			continue
		}
		if b.Len() > 0 {
			b.WriteString("\n---\n")
		}
		b.Write(d)
	}
	return b.Bytes()
}
