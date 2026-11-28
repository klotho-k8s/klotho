// Package vault implements a backend.Store on a HashiCorp Vault KV v2 secrets
// engine. Each cluster's sealed state record is one secret at
// <mount>/<prefix>/<name>, stored as a single base64 field so a record of
// arbitrary bytes survives KV v2's JSON transport untouched.
//
// The store holds opaque, already-sealed bytes produced by the caller's state
// codec. It never sees plaintext state and does no encryption of its own; what
// Vault adds on top is IAM-scoped access, an audit trail and rotation of the
// path's own credentials.
//
// Reserve and Save use KV v2 check-and-set (cas) for atomicity. Reserve writes
// with cas=0, which Vault accepts only when no version of the secret exists, so
// exactly one of several concurrent callers wins a name and the rest get false
// without error. Save reads the current version and writes with cas set to it,
// retrying on a mismatch, so a concurrent writer never silently loses an
// update. See the package README.
package vault

import (
	"context"
	"encoding/base64"
	"errors"
	"fmt"
	"regexp"
	"sort"
	"strings"

	vaultapi "github.com/hashicorp/vault/api"

	"github.com/klotho-k8s/klotho/pkg/cluster"
	"github.com/klotho-k8s/klotho/pkg/state"
	"github.com/klotho-k8s/klotho/provider/backend"
)

// Constants shaping the secret layout and the client.
const (
	// DefaultMount is the KV v2 mount used when none is configured. "secret" is
	// the mount a dev-mode Vault enables by default.
	DefaultMount = "secret"
	// recordField is the key, inside a secret's data, that holds the record. The
	// value is the standard base64 encoding of the opaque record bytes.
	recordField = "record"
	// dataSegment and metadataSegment are the KV v2 API path segments. KV v2
	// splits read/write (data) from list/delete-all-versions (metadata).
	dataSegment     = "data"
	metadataSegment = "metadata"
	// saveRetries bounds how many times Save re-reads and retries after a
	// check-and-set conflict before giving up.
	saveRetries = 5
)

var _ backend.Store = (*Store)(nil)

var namePattern = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9._-]{0,62}$`)

// logical is the slice of Vault's logical client the store uses.
// *vaultapi.Logical satisfies it; tests fake it.
type logical interface {
	ReadWithContext(ctx context.Context, path string) (*vaultapi.Secret, error)
	WriteWithContext(ctx context.Context, path string, data map[string]any) (*vaultapi.Secret, error)
	DeleteWithContext(ctx context.Context, path string) (*vaultapi.Secret, error)
	ListWithContext(ctx context.Context, path string) (*vaultapi.Secret, error)
}

// Sealer turns a state into an opaque record. The store uses it only to write
// reservation records, so it never handles keys itself.
type Sealer interface {
	Seal(*cluster.State) ([]byte, error)
}

// Config is the store's settings. Address and token are taken from the
// environment (VAULT_ADDR, VAULT_TOKEN) when left empty, never from the
// configuration file.
type Config struct {
	// Address is the Vault server URL, e.g. https://vault.example:8200. Empty
	// means VAULT_ADDR.
	Address string
	// Token is the Vault token. Empty means VAULT_TOKEN. It is never read from
	// or written to the configuration file.
	Token string
	// Mount is the path the KV v2 engine is mounted at; empty means
	// DefaultMount. Leading and trailing slashes are ignored.
	Mount string
	// Prefix is a path prefix below the mount beneath which every record is
	// stored. Leading and trailing slashes are ignored.
	Prefix string
	// Namespace is the Vault Enterprise / HCP namespace, if any.
	Namespace string
	// Insecure skips TLS verification when dialing Vault. Development only.
	Insecure bool
}

// Option adjusts how New builds the store.
type Option func(*options)

type options struct {
	client logical
}

// withLogical makes the store use client instead of a real Vault logical
// client. Tests use it; New still validates the configuration.
func withLogical(client logical) Option {
	return func(o *options) { o.client = client }
}

// Store is a backend.Store on a Vault KV v2 engine.
type Store struct {
	client logical
	sealer Sealer
	mount  string
	prefix string
}

// New builds a store for cfg, sealing reservation records with sealer. It does
// not touch the network: the token is not checked and the connection is not
// opened until first use.
func New(cfg Config, sealer Sealer, opts ...Option) (*Store, error) {
	var o options
	for _, opt := range opts {
		opt(&o)
	}
	if sealer == nil {
		return nil, errors.New("vault backend: sealer is required")
	}
	mount := strings.Trim(cfg.Mount, "/")
	if mount == "" {
		mount = DefaultMount
	}

	client := o.client
	if client == nil {
		var err error
		if client, err = newLogical(cfg); err != nil {
			return nil, err
		}
	}
	return &Store{
		client: client,
		sealer: sealer,
		mount:  mount,
		prefix: strings.Trim(cfg.Prefix, "/"),
	}, nil
}

// newLogical builds a real Vault logical client for cfg. The address and token
// default to the standard VAULT_ADDR / VAULT_TOKEN environment variables; the
// client is structured so another auth method (AppRole, Kubernetes) can set the
// token before the store is used.
func newLogical(cfg Config) (logical, error) {
	apiCfg := vaultapi.DefaultConfig()
	if apiCfg.Error != nil {
		return nil, fmt.Errorf("vault backend: load configuration: %w", apiCfg.Error)
	}
	if cfg.Address != "" {
		apiCfg.Address = cfg.Address
	}
	if cfg.Insecure {
		if err := apiCfg.ConfigureTLS(&vaultapi.TLSConfig{Insecure: true}); err != nil {
			return nil, fmt.Errorf("vault backend: configure TLS: %w", err)
		}
	}
	client, err := vaultapi.NewClient(apiCfg)
	if err != nil {
		return nil, fmt.Errorf("vault backend: new client: %w", err)
	}
	// NewClient already reads VAULT_TOKEN (and VAULT_NAMESPACE) from the
	// environment; an explicit value overrides it.
	if cfg.Token != "" {
		client.SetToken(cfg.Token)
	}
	if cfg.Namespace != "" {
		client.SetNamespace(cfg.Namespace)
	}
	return client.Logical(), nil
}

// Name returns "vault".
func (s *Store) Name() string { return "vault" }

// Mount returns the KV v2 mount holding the records.
func (s *Store) Mount() string { return s.mount }

// Reserve claims name by writing a sealed reservation record with cas=0, which
// Vault accepts only when no version of the secret exists yet. A concurrent
// loser gets a check-and-set rejection, so Reserve returns false without error
// in that case.
func (s *Store) Reserve(ctx context.Context, name string) (bool, error) {
	if err := ValidateName(name); err != nil {
		return false, err
	}
	res := state.NewReservation()
	res.Spec.Name = name
	record, err := s.sealer.Seal(res)
	if err != nil {
		return false, fmt.Errorf("vault backend: seal reservation for %s: %w", name, err)
	}
	if err := s.writeCAS(ctx, name, record, 0); err != nil {
		if isCASConflict(err) {
			return false, nil
		}
		return false, fmt.Errorf("vault backend: reserve %s: %w", name, err)
	}
	return true, nil
}

// Load returns the record for name, or backend.ErrNotFound.
func (s *Store) Load(ctx context.Context, name string) ([]byte, error) {
	if err := ValidateName(name); err != nil {
		return nil, err
	}
	secret, err := s.client.ReadWithContext(ctx, s.dataPath(name))
	if err != nil {
		return nil, fmt.Errorf("vault backend: load %s: %w", name, err)
	}
	if secret == nil || secret.Data == nil {
		return nil, fmt.Errorf("vault backend: %s: %w", name, backend.ErrNotFound)
	}
	record, err := decodeRecord(secret)
	if err != nil {
		return nil, fmt.Errorf("vault backend: load %s: %w", name, err)
	}
	return record, nil
}

// Save writes the record for name, replacing any reservation or previous
// record. It reads the current version and writes with cas set to it, retrying
// on a check-and-set conflict so a concurrent writer never silently loses an
// update.
func (s *Store) Save(ctx context.Context, name string, record []byte) error {
	if err := ValidateName(name); err != nil {
		return err
	}
	for attempt := 0; attempt < saveRetries; attempt++ {
		version, err := s.currentVersion(ctx, name)
		if err != nil {
			return fmt.Errorf("vault backend: save %s: %w", name, err)
		}
		if err := s.writeCAS(ctx, name, record, version); err != nil {
			if isCASConflict(err) {
				continue // another writer moved the version; re-read and retry.
			}
			return fmt.Errorf("vault backend: save %s: %w", name, err)
		}
		return nil
	}
	return fmt.Errorf("vault backend: save %s: %w", name, errCASExhausted)
}

// Delete removes every version of name's secret and its metadata, or returns
// backend.ErrNotFound. A DELETE of the metadata path is idempotent and reports
// nothing about prior existence, so Delete first confirms the metadata exists
// to keep the same not-found contract as the other backends.
func (s *Store) Delete(ctx context.Context, name string) error {
	if err := ValidateName(name); err != nil {
		return err
	}
	meta, err := s.client.ReadWithContext(ctx, s.metadataPath(name))
	if err != nil {
		return fmt.Errorf("vault backend: delete %s: %w", name, err)
	}
	if meta == nil || meta.Data == nil {
		return fmt.Errorf("vault backend: %s: %w", name, backend.ErrNotFound)
	}
	if _, err := s.client.DeleteWithContext(ctx, s.metadataPath(name)); err != nil {
		return fmt.Errorf("vault backend: delete %s: %w", name, err)
	}
	return nil
}

// List returns the names of every record under the prefix, sorted. KV v2's
// metadata LIST returns the immediate child keys; a nested folder (a key ending
// in "/") or a key that is not a valid cluster name is ignored, so a secret a
// person wrote by hand is never mistaken for a cluster.
func (s *Store) List(ctx context.Context) ([]string, error) {
	secret, err := s.client.ListWithContext(ctx, s.listPath())
	if err != nil {
		return nil, fmt.Errorf("vault backend: list: %w", err)
	}
	if secret == nil || secret.Data == nil {
		return []string{}, nil
	}
	raw, ok := secret.Data["keys"].([]any)
	if !ok {
		return []string{}, nil
	}
	names := []string{}
	for _, k := range raw {
		key, ok := k.(string)
		if !ok {
			continue
		}
		if strings.HasSuffix(key, "/") || ValidateName(key) != nil {
			continue
		}
		names = append(names, key)
	}
	sort.Strings(names)
	return names, nil
}

// ValidateName reports whether name is usable as a cluster name: it must match
// ^[A-Za-z0-9][A-Za-z0-9._-]{0,62}$, the same shape the file, s3 and proxmox
// backends accept.
func ValidateName(name string) error {
	if !namePattern.MatchString(name) {
		return fmt.Errorf("vault backend: invalid cluster name %q: must match %s", name, namePattern)
	}
	return nil
}

// writeCAS writes record for name as a base64 field with the given cas value.
// cas=0 succeeds only when no version exists; cas=n succeeds only when the
// current version is n.
func (s *Store) writeCAS(ctx context.Context, name string, record []byte, cas int) error {
	payload := map[string]any{
		"data": map[string]any{
			recordField: base64.StdEncoding.EncodeToString(record),
		},
		"options": map[string]any{
			"cas": cas,
		},
	}
	_, err := s.client.WriteWithContext(ctx, s.dataPath(name), payload)
	return err
}

// currentVersion returns the current version number of name's secret, or 0 when
// no secret exists (so a first Save writes with cas=0).
func (s *Store) currentVersion(ctx context.Context, name string) (int, error) {
	secret, err := s.client.ReadWithContext(ctx, s.dataPath(name))
	if err != nil {
		return 0, err
	}
	if secret == nil || secret.Data == nil {
		return 0, nil
	}
	meta, ok := secret.Data["metadata"].(map[string]any)
	if !ok {
		return 0, nil
	}
	return intFromJSON(meta["version"]), nil
}

// dataPath is the KV v2 data path of the named cluster's secret, used for read
// and write.
func (s *Store) dataPath(name string) string { return s.join(dataSegment, name) }

// metadataPath is the KV v2 metadata path of the named cluster's secret, used
// for delete-all-versions.
func (s *Store) metadataPath(name string) string { return s.join(metadataSegment, name) }

// listPath is the KV v2 metadata path the LIST is issued against.
func (s *Store) listPath() string {
	if s.prefix == "" {
		return s.mount + "/" + metadataSegment
	}
	return s.mount + "/" + metadataSegment + "/" + s.prefix
}

// join builds "<mount>/<segment>/<prefix>/<name>" with the prefix omitted when
// empty.
func (s *Store) join(segment, name string) string {
	if s.prefix == "" {
		return s.mount + "/" + segment + "/" + name
	}
	return s.mount + "/" + segment + "/" + s.prefix + "/" + name
}

// decodeRecord extracts and base64-decodes the record field of a KV v2 read.
func decodeRecord(secret *vaultapi.Secret) ([]byte, error) {
	data, ok := secret.Data["data"].(map[string]any)
	if !ok || data == nil {
		return nil, errors.New("secret has no data (soft-deleted or malformed)")
	}
	encoded, ok := data[recordField].(string)
	if !ok {
		return nil, fmt.Errorf("secret is missing the %q field", recordField)
	}
	record, err := base64.StdEncoding.DecodeString(encoded)
	if err != nil {
		return nil, fmt.Errorf("decode %q field: %w", recordField, err)
	}
	return record, nil
}
