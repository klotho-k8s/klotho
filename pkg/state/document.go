// Package state converts a cluster.State into the versioned, optionally
// encrypted record a backend.Store holds, and back again.
//
// A record is a single line of text. Encrypted records are
//
//	klotho/v1:<base64 of an ASCII-armored age ciphertext>
//
// and plain records, intended for local development only, are
//
//	klotho/v1-plain:<JSON document>
//
// Both wrap the same JSON Document envelope.
package state

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"

	"github.com/klotho-k8s/klotho/pkg/cluster"
)

const (
	// Version is the document version this package writes.
	Version = 1
	// Kind identifies a klotho state document.
	Kind = "klotho.state"
)

// ErrUnsupportedVersion is returned when a document was written by a version
// of klotho this build does not understand.
var ErrUnsupportedVersion = errors.New("unsupported state document version")

// Document is the envelope persisted for a cluster.
type Document struct {
	Version int            `json:"version"`
	Kind    string         `json:"kind"`
	State   *cluster.State `json:"state"`
}

// rawDocument defers decoding the payload until the version is known.
type rawDocument struct {
	Version int             `json:"version"`
	Kind    string          `json:"kind"`
	State   json.RawMessage `json:"state"`
}

// Marshal encodes a state as a current-version JSON document.
func Marshal(s *cluster.State) ([]byte, error) {
	if s == nil {
		return nil, errors.New("state: nil state")
	}
	b, err := json.Marshal(Document{Version: Version, Kind: Kind, State: s})
	if err != nil {
		return nil, fmt.Errorf("state: encode document: %w", err)
	}
	return b, nil
}

// Unmarshal decodes a JSON document, rejecting unknown kinds and versions.
func Unmarshal(b []byte) (*cluster.State, error) {
	var raw rawDocument
	if err := json.Unmarshal(b, &raw); err != nil {
		return nil, fmt.Errorf("state: decode document: %w", err)
	}
	if raw.Kind != Kind {
		return nil, fmt.Errorf("state: document kind is %q, want %q", raw.Kind, Kind)
	}
	if raw.Version != Version {
		return nil, fmt.Errorf("state: document version %d, this build reads version %d: %w",
			raw.Version, Version, ErrUnsupportedVersion)
	}
	payload := bytes.TrimSpace(raw.State)
	if len(payload) == 0 || bytes.Equal(payload, []byte("null")) {
		return nil, errors.New("state: document has no state payload")
	}
	var s cluster.State
	if err := json.Unmarshal(payload, &s); err != nil {
		return nil, fmt.Errorf("state: decode state payload: %w", err)
	}
	return &s, nil
}
