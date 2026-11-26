// Package controlplane defines what a control plane provider contributes to the
// bootstrap node: the cluster's certificates and kubeconfig, and the user-data
// fragments that install and configure the Kubernetes distribution.
//
// A provider never invents the node's configuration. It derives it from the
// control plane object in the generated cluster bundle (for kubeadm, the
// KubeadmControlPlane's kubeadmConfigSpec), so the bootstrap node and the
// config it is adopted with match what the control plane controller expects
// byte for byte, and no rollout is triggered by adoption.
package controlplane

import (
	"context"

	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"

	"github.com/klotho-k8s/klotho/pkg/cluster"
)

// Credentials are the secrets a control plane provider mints before the first
// node exists, so the endpoint is baked into them from the start.
type Credentials struct {
	// CA signs API server client certificates. It is persisted in state.
	CA cluster.KeyPair
	// Extra holds any further key pairs the distribution needs (etcd, service
	// account, front proxy), keyed by purpose. They ship to the node and into
	// the Cluster API secrets, but are not persisted.
	Extra map[string]cluster.KeyPair
	// Kubeconfig is an admin kubeconfig for the bootstrap flow itself. It is
	// never persisted; the shell command mints its own short-lived one.
	Kubeconfig []byte
}

// File is one file to write on the bootstrap node.
type File struct {
	Path    string
	Content []byte
	Mode    string
	Owner   string
	Append  bool
	Base64  bool
}

// Contribution is a provider's share of the bootstrap node's user-data.
type Contribution struct {
	Files []File
	// PreCommands run before the distribution is installed.
	PreCommands []string
	// InstallCommands install and start the distribution.
	InstallCommands []string
	// PostCommands run once the API server answers.
	PostCommands []string
}

// Provider installs a Kubernetes distribution on the bootstrap node and makes
// it adoptable by its Cluster API control plane provider.
//
// controlPlane is the control plane object from the generated bundle (for
// kubeadm, the KubeadmControlPlane). Providers read their configuration from
// it rather than from a template of their own.
type Provider interface {
	// Name is the provider's identifier, as used by --control-plane and in state.
	Name() string
	// Kind is the control plane object kind this provider handles, used to
	// locate it in a bundle (for example "KubeadmControlPlane").
	Kind() string
	// Prepare mints the credentials for a cluster whose endpoint is known.
	Prepare(ctx context.Context, spec *cluster.Spec, endpoint cluster.Endpoint) (*Credentials, error)
	// UserData renders this provider's share of the bootstrap node's cloud-init
	// from the control plane object's configuration. clusterObj is the
	// Cluster object from the bundle, for what lives there rather than on the
	// control plane object (the pod and service networks); it may be nil.
	UserData(ctx context.Context, spec *cluster.Spec, creds *Credentials, controlPlane, clusterObj *unstructured.Unstructured) (*Contribution, error)
	// Adopt renders the Cluster API objects that make the bootstrap node a
	// member of the control plane object, with a bootstrap config that matches
	// the control plane's own spec so adoption does not trigger a rollout.
	Adopt(ctx context.Context, state *cluster.State, controlPlane *unstructured.Unstructured) ([]byte, error)
}
