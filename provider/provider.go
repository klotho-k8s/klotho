// Package provider holds what the infrastructure and control plane provider
// contracts share.
package provider

import (
	"fmt"
	"strings"

	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
)

// Finding is one thing a provider wants the user to know about the cluster
// bundle before it is created: a setting that is unset or unusual, what it
// does, and what happens without it. A finding is advice, not an error. klotho
// never edits the bundle to resolve one; the user decides, and may create the
// cluster as is with --force.
type Finding struct {
	// Object identifies what was checked, for example
	// "KubeadmControlPlane/lab1-control-plane".
	Object string
	// Field is the path the finding is about, for example
	// "spec.rollout.before.certificatesExpiryDays".
	Field string
	// Message explains what the field does and the consequence of leaving it
	// as it is. It is complete on its own, written for the person who owns
	// the cluster YAML.
	Message string
}

func (f Finding) String() string {
	var b strings.Builder
	if f.Object != "" {
		b.WriteString(f.Object)
		b.WriteString(": ")
	}
	if f.Field != "" {
		b.WriteString(f.Field)
		b.WriteString(": ")
	}
	b.WriteString(f.Message)
	return b.String()
}

// Checker is implemented by providers that review the cluster bundle before
// it is created. Check reads the bundle and never changes it.
type Checker interface {
	Check(bundle []*unstructured.Unstructured) []Finding
}

// ObjectRef names an object the way findings do.
func ObjectRef(o *unstructured.Unstructured) string {
	return fmt.Sprintf("%s/%s", o.GetKind(), o.GetName())
}

// Dependency is an extra Cluster API provider an infrastructure provider
// needs the operator to install alongside it, such as an IPAM provider.
type Dependency struct {
	// Kind is the operator object kind, for example "IPAMProvider".
	Kind string
	// Name is the provider's name in the operator's catalog, for example
	// "in-cluster".
	Name string
	// Namespace is where the provider runs.
	Namespace string
	// Version is the release to install.
	Version string
}

// Dependent is implemented by providers that need other Cluster API
// providers installed.
type Dependent interface {
	Dependencies() []Dependency
}

// PackageRequirer is implemented by infrastructure providers whose nodes need
// packages the base image may lack before anything else can proceed, for
// example an agent the provider reads node addresses from. The install-at-boot
// script installs and enables them first.
type PackageRequirer interface {
	RequiredPackages() []string
}
