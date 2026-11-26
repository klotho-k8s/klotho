package kubeadm

import (
	"fmt"

	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"

	"github.com/klotho-k8s/klotho/provider"
)

// Rollout field paths of a KubeadmControlPlane, per API version.
const (
	certificatesExpiryFieldV1beta2 = "spec.rollout.before.certificatesExpiryDays"
	certificatesExpiryFieldV1beta1 = "spec.rolloutBefore.certificatesExpiryDays"
)

const certificatesExpiryMessage = "not set. kubeadm issues the control plane certificates " +
	"(API server, etcd, controller manager, scheduler, the admin kubeconfig) with a one-year " +
	"lifetime, and Cluster API never renews them in place. With this field set, " +
	"KubeadmControlPlane replaces each control plane machine that many days before its " +
	"certificates expire, so the cluster stays up. Without it nothing renews them, and the " +
	"control plane stops working one year after each machine was created unless you upgrade " +
	"or renew by hand. A common value is 30."

// Check implements provider.Checker. It reads every KubeadmControlPlane in the
// bundle and reports settings the cluster owner should decide on. It changes
// nothing.
func (p *Provider) Check(bundle []*unstructured.Unstructured) []provider.Finding {
	var out []provider.Finding
	for _, o := range bundle {
		if o == nil || o.GetKind() != p.Kind() {
			continue
		}
		if f, ok := checkCertificatesExpiry(o); ok {
			out = append(out, f)
		}
	}
	return out
}

// checkCertificatesExpiry reports a KubeadmControlPlane whose rollout-before
// window for certificate expiry is unset or zero.
func checkCertificatesExpiry(o *unstructured.Unstructured) (provider.Finding, bool) {
	field := certificatesExpiryFieldV1beta2
	path := []string{"spec", "rollout", "before", "certificatesExpiryDays"}
	if o.GetAPIVersion() == "controlplane.cluster.x-k8s.io/v1beta1" {
		field = certificatesExpiryFieldV1beta1
		path = []string{"spec", "rolloutBefore", "certificatesExpiryDays"}
	}
	days, found, err := unstructured.NestedInt64(o.Object, path...)
	if err != nil {
		return provider.Finding{
			Object:  provider.ObjectRef(o),
			Field:   field,
			Message: fmt.Sprintf("is not a number (%v); KubeadmControlPlane will reject it", err),
		}, true
	}
	if found && days > 0 {
		return provider.Finding{}, false
	}
	return provider.Finding{Object: provider.ObjectRef(o), Field: field, Message: certificatesExpiryMessage}, true
}
