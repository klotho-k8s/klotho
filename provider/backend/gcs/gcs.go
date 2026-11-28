// Package gcs implements a backend.Store on Google Cloud Storage. Each
// cluster's state record is one object at <prefix>/<name>.record in a
// configured bucket.
//
// The store holds opaque, already-sealed bytes produced by the caller's state
// codec. It never sees plaintext state and does no encryption of its own.
//
// Reserve is atomic: it creates the object with a DoesNotExist generation
// precondition, so GCS itself rejects the write with 412 Precondition Failed
// when an object already exists at the key and exactly one of several
// concurrent callers wins. Save is last-writer-wins: the Store interface
// carries no generation to match, so there is no compare-and-swap at Save (see
// package README for the IfGenerationMatch analog).
package gcs

import (
	"context"
	"errors"
	"fmt"
	"io"
	"regexp"
	"sort"
	"strings"

	"cloud.google.com/go/storage"
	"github.com/googleapis/gax-go/v2/apierror"
	"google.golang.org/api/googleapi"
	"google.golang.org/api/iterator"
	"google.golang.org/api/option"

	"github.com/klotho-k8s/klotho/pkg/cluster"
	"github.com/klotho-k8s/klotho/pkg/state"
	"github.com/klotho-k8s/klotho/provider/backend"
)

// Constants shaping the object layout and the client.
const (
	// Suffix is the key extension of a cluster record. It matches the file and
	// s3 backends so a bucket and a directory of records read alike.
	Suffix = ".record"
	// ContentType is set on every record object. A record is opaque bytes,
	// never anything a client should sniff or render.
	ContentType = "application/octet-stream"
)

var _ backend.Store = (*Store)(nil)

var namePattern = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9._-]{0,62}$`)

// api is the slice of GCS the store uses. The real client (gcsClient) wraps
// *storage.Client; tests fake it so no test dials GCP.
type api interface {
	// create writes data at key only when no object exists there. It returns
	// false, without error, when the key is already taken (the DoesNotExist
	// precondition failed).
	create(ctx context.Context, key string, data []byte) (bool, error)
	// put writes data at key unconditionally (last-writer-wins).
	put(ctx context.Context, key string, data []byte) error
	// get returns the object at key, or storage.ErrObjectNotExist.
	get(ctx context.Context, key string) ([]byte, error)
	// remove deletes the object at key, or returns storage.ErrObjectNotExist
	// when it is absent.
	remove(ctx context.Context, key string) error
	// list returns every object key under prefix.
	list(ctx context.Context, prefix string) ([]string, error)
	// Close releases the underlying client.
	Close() error
}

// Sealer turns a state into an opaque record. The store uses it only to write
// reservation records, so it never handles keys itself.
type Sealer interface {
	Seal(*cluster.State) ([]byte, error)
}

// Config is the store's settings. Bucket is required; everything else has a
// default.
type Config struct {
	// Bucket must already exist; klotho does not create it.
	Bucket string
	// Prefix is a key prefix below which every record is stored. Leading and
	// trailing slashes are ignored.
	Prefix string
	// CredentialsFile is a path to a service-account key JSON. Empty uses
	// Application Default Credentials (GOOGLE_APPLICATION_CREDENTIALS, gcloud
	// login, or the host's attached service account). A path is not a
	// credential, so it may come from the configuration file; the key contents
	// never do.
	CredentialsFile string
}

// Option adjusts how New builds the store.
type Option func(*options)

type options struct {
	client api
}

// withAPI makes the store use client instead of a real GCS client. Tests use
// it; New still validates the configuration.
func withAPI(client api) Option {
	return func(o *options) { o.client = client }
}

// Store is a backend.Store on a GCS bucket.
type Store struct {
	client api
	sealer Sealer
	bucket string
	prefix string
}

// New builds a store for cfg, sealing reservation records with sealer. It
// creates the GCS client (resolving credentials lazily on first use) but does
// not touch the network: the bucket is not checked.
func New(ctx context.Context, cfg Config, sealer Sealer, opts ...Option) (*Store, error) {
	var o options
	for _, opt := range opts {
		opt(&o)
	}
	if cfg.Bucket == "" {
		return nil, errors.New("gcs backend: bucket is required")
	}
	if sealer == nil {
		return nil, errors.New("gcs backend: sealer is required")
	}

	client := o.client
	if client == nil {
		var err error
		if client, err = newClient(ctx, cfg); err != nil {
			return nil, err
		}
	}
	return &Store{
		client: client,
		sealer: sealer,
		bucket: cfg.Bucket,
		prefix: strings.Trim(cfg.Prefix, "/"),
	}, nil
}

// newClient builds a real GCS-backed api for cfg: a service-account key file
// when one is configured, otherwise Application Default Credentials. The
// STORAGE_EMULATOR_HOST environment variable is honoured by the SDK, so an
// emulator such as fake-gcs-server needs no endpoint field here.
func newClient(ctx context.Context, cfg Config) (api, error) {
	var clientOpts []option.ClientOption
	if cfg.CredentialsFile != "" {
		clientOpts = append(clientOpts, option.WithCredentialsFile(cfg.CredentialsFile))
	}
	c, err := storage.NewClient(ctx, clientOpts...)
	if err != nil {
		return nil, fmt.Errorf("gcs backend: build client: %w", err)
	}
	return &gcsClient{client: c, bucket: cfg.Bucket}, nil
}

// Name returns "gcs".
func (s *Store) Name() string { return "gcs" }

// Bucket returns the bucket holding the records.
func (s *Store) Bucket() string { return s.bucket }

// Reserve claims name by creating its object with a DoesNotExist generation
// precondition. GCS rejects the write when the object already exists, so
// Reserve returns false without error in that case.
func (s *Store) Reserve(ctx context.Context, name string) (bool, error) {
	key, err := s.key(name)
	if err != nil {
		return false, err
	}
	res := state.NewReservation()
	res.Spec.Name = name
	record, err := s.sealer.Seal(res)
	if err != nil {
		return false, fmt.Errorf("gcs backend: seal reservation for %s: %w", name, err)
	}
	created, err := s.client.create(ctx, key, record)
	if err != nil {
		return false, fmt.Errorf("gcs backend: reserve %s: %w", name, err)
	}
	return created, nil
}

// Load returns the record for name, or backend.ErrNotFound.
func (s *Store) Load(ctx context.Context, name string) ([]byte, error) {
	key, err := s.key(name)
	if err != nil {
		return nil, err
	}
	b, err := s.client.get(ctx, key)
	if err != nil {
		if errors.Is(err, storage.ErrObjectNotExist) {
			return nil, fmt.Errorf("gcs backend: %s: %w", name, backend.ErrNotFound)
		}
		return nil, fmt.Errorf("gcs backend: load %s: %w", name, err)
	}
	return b, nil
}

// Save writes the record for name, replacing any reservation or previous
// record. It is last-writer-wins.
func (s *Store) Save(ctx context.Context, name string, record []byte) error {
	key, err := s.key(name)
	if err != nil {
		return err
	}
	if err := s.client.put(ctx, key, record); err != nil {
		return fmt.Errorf("gcs backend: save %s: %w", name, err)
	}
	return nil
}

// Delete removes the record for name, or returns backend.ErrNotFound. GCS's
// Delete reports whether the object existed, so unlike s3 no prior Head is
// needed to keep the not-found contract of the other backends.
func (s *Store) Delete(ctx context.Context, name string) error {
	key, err := s.key(name)
	if err != nil {
		return err
	}
	if err := s.client.remove(ctx, key); err != nil {
		if errors.Is(err, storage.ErrObjectNotExist) {
			return fmt.Errorf("gcs backend: %s: %w", name, backend.ErrNotFound)
		}
		return fmt.Errorf("gcs backend: delete %s: %w", name, err)
	}
	return nil
}

// List returns the names of every record under the prefix, sorted. Keys that
// are not <prefix>/<valid-name>.record are ignored, so an object a person put
// in the bucket by hand is never mistaken for a cluster.
func (s *Store) List(ctx context.Context) ([]string, error) {
	listPrefix := s.prefix
	if listPrefix != "" {
		listPrefix += "/"
	}
	keys, err := s.client.list(ctx, listPrefix)
	if err != nil {
		return nil, fmt.Errorf("gcs backend: list %s: %w", listPrefix, err)
	}
	var names []string
	for _, key := range keys {
		name, ok := strings.CutPrefix(key, listPrefix)
		if !ok {
			continue
		}
		name, ok = strings.CutSuffix(name, Suffix)
		if !ok || strings.Contains(name, "/") || ValidateName(name) != nil {
			continue
		}
		names = append(names, name)
	}
	sort.Strings(names)
	return names, nil
}

// Close releases the underlying GCS client.
func (s *Store) Close() error { return s.client.Close() }

// ValidateName reports whether name is usable as a cluster name: it must match
// ^[A-Za-z0-9][A-Za-z0-9._-]{0,62}$, the same shape the file, proxmox and s3
// backends accept.
func ValidateName(name string) error {
	if !namePattern.MatchString(name) {
		return fmt.Errorf("gcs backend: invalid cluster name %q: must match %s", name, namePattern)
	}
	return nil
}

// key is the object key of the named cluster's record.
func (s *Store) key(name string) (string, error) {
	if err := ValidateName(name); err != nil {
		return "", err
	}
	if s.prefix == "" {
		return name + Suffix, nil
	}
	return s.prefix + "/" + name + Suffix, nil
}

// gcsClient is the real api, backed by a *storage.Client scoped to one bucket.
type gcsClient struct {
	client *storage.Client
	bucket string
}

func (g *gcsClient) object(key string) *storage.ObjectHandle {
	return g.client.Bucket(g.bucket).Object(key)
}

func (g *gcsClient) create(ctx context.Context, key string, data []byte) (bool, error) {
	// DoesNotExist is the generation precondition that makes the write succeed
	// only when no live object exists at the key: the GCS compare-and-swap.
	w := g.object(key).If(storage.Conditions{DoesNotExist: true}).NewWriter(ctx)
	w.ContentType = ContentType
	if _, err := w.Write(data); err != nil {
		_ = w.Close()
		if isPreconditionFailed(err) {
			return false, nil
		}
		return false, err
	}
	if err := w.Close(); err != nil {
		if isPreconditionFailed(err) {
			return false, nil
		}
		return false, err
	}
	return true, nil
}

func (g *gcsClient) put(ctx context.Context, key string, data []byte) error {
	w := g.object(key).NewWriter(ctx)
	w.ContentType = ContentType
	if _, err := w.Write(data); err != nil {
		_ = w.Close()
		return err
	}
	return w.Close()
}

func (g *gcsClient) get(ctx context.Context, key string) ([]byte, error) {
	r, err := g.object(key).NewReader(ctx)
	if err != nil {
		return nil, err
	}
	defer func() { _ = r.Close() }()
	return io.ReadAll(r)
}

func (g *gcsClient) remove(ctx context.Context, key string) error {
	return g.object(key).Delete(ctx)
}

func (g *gcsClient) list(ctx context.Context, prefix string) ([]string, error) {
	var keys []string
	it := g.client.Bucket(g.bucket).Objects(ctx, &storage.Query{Prefix: prefix})
	for {
		attrs, err := it.Next()
		if errors.Is(err, iterator.Done) {
			break
		}
		if err != nil {
			return nil, err
		}
		keys = append(keys, attrs.Name)
	}
	return keys, nil
}

func (g *gcsClient) Close() error { return g.client.Close() }

// isPreconditionFailed reports whether err is a conditional-write rejection: a
// 412 from the DoesNotExist precondition, which is how GCS says the object
// already exists.
func isPreconditionFailed(err error) bool {
	var ge *googleapi.Error
	if errors.As(err, &ge) && ge.Code == 412 {
		return true
	}
	var ae *apierror.APIError
	if errors.As(err, &ae) && ae.HTTPCode() == 412 {
		return true
	}
	return false
}
