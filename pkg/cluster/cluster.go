// Package cluster holds the types shared by the command layer, the providers,
// and the state backend: what a cluster is asked to be, what was built for it,
// and what is remembered about it afterwards.
package cluster

import "time"

// Spec is the user's request for a cluster, resolved from flags, config, and
// provider discovery before any provider is called.
type Spec struct {
	Name         string `json:"name"`
	Namespace    string `json:"namespace,omitempty"`
	Provider     string `json:"provider"`
	ControlPlane string `json:"controlPlane"`
	Flavor       string `json:"flavor,omitempty"`
	// Variables are template substitutions, keyed by the template's own names.
	Variables map[string]string `json:"variables,omitempty"`
}

// Endpoint is where the API server will be reachable once the first node is up.
// Providers may allocate it or merely validate one the user supplied.
type Endpoint struct {
	Host string `json:"host"`
	Port int    `json:"port"`
}

// Node is the bootstrap machine a provider launched, as the provider identifies it.
type Node struct {
	ID        string   `json:"id"`
	Name      string   `json:"name"`
	Addresses []string `json:"addresses,omitempty"`
}

// Resource is one thing a provider created or found, recorded so delete can
// remove it even when it cannot be found by tag.
type Resource struct {
	Kind string `json:"kind"`
	ID   string `json:"id"`
	Name string `json:"name,omitempty"`
}

// State is everything persisted about a cluster. It must contain enough to
// delete the cluster and to mint credentials for it, and nothing that is
// itself a usable credential.
type State struct {
	Spec      Spec       `json:"spec"`
	Endpoint  Endpoint   `json:"endpoint"`
	Bootstrap *Node      `json:"bootstrap,omitempty"`
	Resources []Resource `json:"resources,omitempty"`
	// CA is the PEM-encoded cluster CA certificate and key used to mint
	// short-lived client certificates on demand.
	CA        KeyPair   `json:"ca"`
	CreatedAt time.Time `json:"createdAt"`
	// Reserved marks a record that only claims the name; provisioning never
	// completed, or had not started, when it was written.
	Reserved bool `json:"reserved,omitempty"`
}

// KeyPair is a PEM-encoded certificate and private key.
type KeyPair struct {
	Cert []byte `json:"cert,omitempty"`
	Key  []byte `json:"key,omitempty"`
}
