// Package backend defines where cluster state lives. A backend is chosen
// explicitly with --backend, or defaults to the infrastructure provider's
// native store when it offers one; it is never inferred from a manifest.
//
// A store holds opaque, already-encrypted bytes. It never sees plaintext
// state and never holds a usable credential.
package backend

import (
	"context"
	"errors"
)

// ErrNotFound is returned by Load and Delete when no record exists for the name.
var ErrNotFound = errors.New("cluster not found")

// Store persists cluster state records by cluster name.
type Store interface {
	// Name is the backend's identifier, as used by --backend.
	Name() string
	// Reserve claims a cluster name by writing an empty reservation record.
	// It returns false, without error, if the name is already taken.
	// Implementations document how atomic the claim is.
	Reserve(ctx context.Context, name string) (bool, error)
	// Load returns the record for a cluster, or ErrNotFound.
	Load(ctx context.Context, name string) ([]byte, error)
	// Save writes a record, replacing any reservation or previous record.
	Save(ctx context.Context, name string, record []byte) error
	// Delete removes a cluster's record, or returns ErrNotFound.
	Delete(ctx context.Context, name string) error
	// List returns the names of all clusters the backend knows.
	List(ctx context.Context) ([]string, error)
}
