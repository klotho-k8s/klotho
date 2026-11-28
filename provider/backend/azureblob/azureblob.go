// Package azureblob implements a backend.Store on Azure Blob Storage. Each
// cluster's state record is one block blob at <prefix>/<name>.record in a
// configured container.
//
// The store holds opaque, already-sealed bytes produced by the caller's state
// codec. It never sees plaintext state and does no encryption of its own.
//
// Reserve is atomic: it uploads the blob with an If-None-Match: * access
// condition, so the service itself rejects the write when a blob already
// exists at the name and exactly one of several concurrent callers wins. Azure
// Blob honours this unconditionally (it is not an eventually-consistent
// opt-in like some S3-compatible stores), so Reserve needs no read-then-write
// and has no race. Save is last-writer-wins: there is no compare-and-swap. A
// per-record ETag If-Match would add one, but the backend.Store contract's
// Save takes no expected version, so none is attempted (see package README).
package azureblob

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net/url"
	"regexp"
	"sort"
	"strings"

	"github.com/Azure/azure-sdk-for-go/sdk/azcore"
	"github.com/Azure/azure-sdk-for-go/sdk/azcore/to"
	"github.com/Azure/azure-sdk-for-go/sdk/azidentity"
	"github.com/Azure/azure-sdk-for-go/sdk/storage/azblob/blob"
	"github.com/Azure/azure-sdk-for-go/sdk/storage/azblob/bloberror"
	"github.com/Azure/azure-sdk-for-go/sdk/storage/azblob/blockblob"
	"github.com/Azure/azure-sdk-for-go/sdk/storage/azblob/container"

	"github.com/klotho-k8s/klotho/pkg/cluster"
	"github.com/klotho-k8s/klotho/pkg/state"
	"github.com/klotho-k8s/klotho/provider/backend"
)

// Constants shaping the blob layout.
const (
	// Suffix is the blob name extension of a cluster record. It matches the
	// file and s3 backends so a container and a directory of records read
	// alike.
	Suffix = ".record"
	// ContentType is set on every record blob. A record is opaque bytes, never
	// anything a client should sniff or render.
	ContentType = "application/octet-stream"
	// blobEndpointSuffix is appended to an account name to form the public blob
	// service URL when no explicit endpoint is configured.
	blobEndpointSuffix = ".blob.core.windows.net"
)

var _ backend.Store = (*Store)(nil)

var namePattern = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9._-]{0,62}$`)

// api is the slice of the Azure Blob client the store uses. The real client
// (azContainer) satisfies it over a container.Client; tests fake it.
type api interface {
	// Upload writes data to the named blob. When ifNotExists is true it sets
	// If-None-Match: * and returns (false, nil) if a blob already exists,
	// leaving it untouched; otherwise it returns (true, nil) on a fresh write.
	Upload(ctx context.Context, name string, data []byte, ifNotExists bool) (bool, error)
	// Download returns the named blob's bytes, or found=false when it is
	// absent.
	Download(ctx context.Context, name string) (data []byte, found bool, err error)
	// Exists reports whether the named blob is present.
	Exists(ctx context.Context, name string) (bool, error)
	// Delete removes the named blob. Deleting an absent blob is not an error at
	// this level; the store checks existence first.
	Delete(ctx context.Context, name string) error
	// List returns the names of every blob under prefix.
	List(ctx context.Context, prefix string) ([]string, error)
}

// Sealer turns a state into an opaque record. The store uses it only to write
// reservation records, so it never handles keys itself.
type Sealer interface {
	Seal(*cluster.State) ([]byte, error)
}

// Config is the store's settings. Container is required; everything else has a
// default or comes from the environment.
type Config struct {
	// AccountName is the storage account, e.g. "klothostate". It forms the
	// service URL https://<account>.blob.core.windows.net when Endpoint is
	// empty, and is required for a shared-key credential. It may be empty only
	// when a connection string is used or Endpoint is set explicitly.
	AccountName string
	// Endpoint overrides the blob service URL, e.g.
	// http://127.0.0.1:10000/devstoreaccount1 for the Azurite emulator. Empty
	// means the public endpoint derived from AccountName.
	Endpoint string
	// Container must already exist; klotho does not create it.
	Container string
	// Prefix is a blob-name prefix below which every record is stored. Leading
	// and trailing slashes are ignored.
	Prefix string
	// ConnectionString authenticates with an account connection string. It
	// comes from the environment only and is never read from the config file.
	ConnectionString string
	// AccountKey authenticates with a shared key for AccountName. It comes from
	// the environment only and is never read from the config file. When neither
	// ConnectionString nor AccountKey is set, DefaultAzureCredential is used
	// (environment, workload identity, managed identity, Azure CLI, and so on).
	AccountKey string
}

// Option adjusts how New builds the store.
type Option func(*options)

type options struct {
	client api
}

// withAPI makes the store use client instead of a real Azure Blob client.
// Tests use it; New still validates the configuration.
func withAPI(client api) Option {
	return func(o *options) { o.client = client }
}

// Store is a backend.Store on an Azure Blob container.
type Store struct {
	client    api
	sealer    Sealer
	container string
	prefix    string
}

// New builds a store for cfg, sealing reservation records with sealer. It does
// not touch the network: the container is not checked and credentials are
// resolved on first use.
func New(ctx context.Context, cfg Config, sealer Sealer, opts ...Option) (*Store, error) {
	var o options
	for _, opt := range opts {
		opt(&o)
	}
	if cfg.Container == "" {
		return nil, errors.New("azureblob backend: container is required")
	}
	if sealer == nil {
		return nil, errors.New("azureblob backend: sealer is required")
	}
	if cfg.Endpoint != "" {
		u, err := url.Parse(cfg.Endpoint)
		if err != nil || u.Host == "" || (u.Scheme != "http" && u.Scheme != "https") {
			return nil, fmt.Errorf("azureblob backend: endpoint %q must be an http(s) URL", cfg.Endpoint)
		}
	}
	if cfg.ConnectionString == "" {
		if cfg.AccountName == "" && cfg.Endpoint == "" {
			return nil, errors.New("azureblob backend: account name is required unless a connection string or endpoint is given")
		}
		if cfg.AccountKey != "" && cfg.AccountName == "" {
			return nil, errors.New("azureblob backend: account key requires an account name")
		}
	}

	client := o.client
	if client == nil {
		var err error
		if client, err = newClient(cfg); err != nil {
			return nil, err
		}
	}
	return &Store{
		client:    client,
		sealer:    sealer,
		container: cfg.Container,
		prefix:    strings.Trim(cfg.Prefix, "/"),
	}, nil
}

// newClient builds a real container client for cfg. A connection string wins,
// then a shared account key, else DefaultAzureCredential. Nothing here dials
// Azure; credentials are exercised on the first blob operation.
func newClient(cfg Config) (api, error) {
	if cfg.ConnectionString != "" {
		c, err := container.NewClientFromConnectionString(cfg.ConnectionString, cfg.Container, nil)
		if err != nil {
			return nil, fmt.Errorf("azureblob backend: connection string: %w", err)
		}
		return &azContainer{c: c}, nil
	}

	serviceURL := cfg.Endpoint
	if serviceURL == "" {
		serviceURL = "https://" + cfg.AccountName + blobEndpointSuffix
	}
	containerURL := strings.TrimRight(serviceURL, "/") + "/" + cfg.Container

	if cfg.AccountKey != "" {
		cred, err := container.NewSharedKeyCredential(cfg.AccountName, cfg.AccountKey)
		if err != nil {
			return nil, fmt.Errorf("azureblob backend: shared key credential: %w", err)
		}
		c, err := container.NewClientWithSharedKeyCredential(containerURL, cred, nil)
		if err != nil {
			return nil, fmt.Errorf("azureblob backend: container client: %w", err)
		}
		return &azContainer{c: c}, nil
	}

	cred, err := azidentity.NewDefaultAzureCredential(nil)
	if err != nil {
		return nil, fmt.Errorf("azureblob backend: default credential: %w", err)
	}
	c, err := container.NewClient(containerURL, cred, nil)
	if err != nil {
		return nil, fmt.Errorf("azureblob backend: container client: %w", err)
	}
	return &azContainer{c: c}, nil
}

// Name returns "azureblob".
func (s *Store) Name() string { return "azureblob" }

// Container returns the container holding the records.
func (s *Store) Container() string { return s.container }

// Reserve claims name by uploading a sealed reservation record with
// If-None-Match: *. The service rejects the write when the blob already
// exists, so Reserve returns false without error in that case.
func (s *Store) Reserve(ctx context.Context, name string) (bool, error) {
	key, err := s.key(name)
	if err != nil {
		return false, err
	}
	res := state.NewReservation()
	res.Spec.Name = name
	record, err := s.sealer.Seal(res)
	if err != nil {
		return false, fmt.Errorf("azureblob backend: seal reservation for %s: %w", name, err)
	}
	created, err := s.client.Upload(ctx, key, record, true)
	if err != nil {
		return false, fmt.Errorf("azureblob backend: reserve %s: %w", name, err)
	}
	return created, nil
}

// Load returns the record for name, or backend.ErrNotFound.
func (s *Store) Load(ctx context.Context, name string) ([]byte, error) {
	key, err := s.key(name)
	if err != nil {
		return nil, err
	}
	data, found, err := s.client.Download(ctx, key)
	if err != nil {
		return nil, fmt.Errorf("azureblob backend: load %s: %w", name, err)
	}
	if !found {
		return nil, fmt.Errorf("azureblob backend: %s: %w", name, backend.ErrNotFound)
	}
	return data, nil
}

// Save writes the record for name, replacing any reservation or previous
// record. It is last-writer-wins.
func (s *Store) Save(ctx context.Context, name string, record []byte) error {
	key, err := s.key(name)
	if err != nil {
		return err
	}
	if _, err := s.client.Upload(ctx, key, record, false); err != nil {
		return fmt.Errorf("azureblob backend: save %s: %w", name, err)
	}
	return nil
}

// Delete removes the record for name, or returns backend.ErrNotFound. Azure's
// own delete fails on an absent blob, but to keep the same not-found contract
// as the other backends Delete first confirms the blob is present.
func (s *Store) Delete(ctx context.Context, name string) error {
	key, err := s.key(name)
	if err != nil {
		return err
	}
	found, err := s.client.Exists(ctx, key)
	if err != nil {
		return fmt.Errorf("azureblob backend: delete %s: %w", name, err)
	}
	if !found {
		return fmt.Errorf("azureblob backend: %s: %w", name, backend.ErrNotFound)
	}
	if err := s.client.Delete(ctx, key); err != nil {
		return fmt.Errorf("azureblob backend: delete %s: %w", name, err)
	}
	return nil
}

// List returns the names of every record under the prefix, sorted. Blobs that
// are not <prefix>/<valid-name>.record are ignored, so a blob a person put in
// the container by hand is never mistaken for a cluster.
func (s *Store) List(ctx context.Context) ([]string, error) {
	listPrefix := s.prefix
	if listPrefix != "" {
		listPrefix += "/"
	}
	blobs, err := s.client.List(ctx, listPrefix)
	if err != nil {
		return nil, fmt.Errorf("azureblob backend: list %s: %w", listPrefix, err)
	}
	var names []string
	for _, key := range blobs {
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

// ValidateName reports whether name is usable as a cluster name: it must match
// ^[A-Za-z0-9][A-Za-z0-9._-]{0,62}$, the same shape the file, proxmox and s3
// backends accept.
func ValidateName(name string) error {
	if !namePattern.MatchString(name) {
		return fmt.Errorf("azureblob backend: invalid cluster name %q: must match %s", name, namePattern)
	}
	return nil
}

// key is the blob name of the named cluster's record.
func (s *Store) key(name string) (string, error) {
	if err := ValidateName(name); err != nil {
		return "", err
	}
	if s.prefix == "" {
		return name + Suffix, nil
	}
	return s.prefix + "/" + name + Suffix, nil
}

// azContainer is the real api, over an Azure Blob container client.
type azContainer struct {
	c *container.Client
}

func (a *azContainer) Upload(ctx context.Context, name string, data []byte, ifNotExists bool) (bool, error) {
	bb := a.c.NewBlockBlobClient(name)
	opts := &blockblob.UploadBufferOptions{
		HTTPHeaders: &blob.HTTPHeaders{BlobContentType: to.Ptr(ContentType)},
	}
	if ifNotExists {
		opts.AccessConditions = &blob.AccessConditions{
			ModifiedAccessConditions: &blob.ModifiedAccessConditions{IfNoneMatch: to.Ptr(azcore.ETagAny)},
		}
	}
	if _, err := bb.UploadBuffer(ctx, data, opts); err != nil {
		if ifNotExists && isAlreadyExists(err) {
			return false, nil
		}
		return false, err
	}
	return true, nil
}

func (a *azContainer) Download(ctx context.Context, name string) ([]byte, bool, error) {
	resp, err := a.c.NewBlobClient(name).DownloadStream(ctx, nil)
	if err != nil {
		if bloberror.HasCode(err, bloberror.BlobNotFound) {
			return nil, false, nil
		}
		return nil, false, err
	}
	defer func() { _ = resp.Body.Close() }()
	b, err := io.ReadAll(resp.Body)
	if err != nil {
		return nil, false, err
	}
	return b, true, nil
}

func (a *azContainer) Exists(ctx context.Context, name string) (bool, error) {
	if _, err := a.c.NewBlobClient(name).GetProperties(ctx, nil); err != nil {
		if bloberror.HasCode(err, bloberror.BlobNotFound) {
			return false, nil
		}
		return false, err
	}
	return true, nil
}

func (a *azContainer) Delete(ctx context.Context, name string) error {
	_, err := a.c.NewBlobClient(name).Delete(ctx, nil)
	return err
}

func (a *azContainer) List(ctx context.Context, prefix string) ([]string, error) {
	var listOpts *container.ListBlobsFlatOptions
	if prefix != "" {
		listOpts = &container.ListBlobsFlatOptions{Prefix: to.Ptr(prefix)}
	}
	var names []string
	pager := a.c.NewListBlobsFlatPager(listOpts)
	for pager.More() {
		page, err := pager.NextPage(ctx)
		if err != nil {
			return nil, err
		}
		for _, item := range page.Segment.BlobItems {
			if item.Name != nil {
				names = append(names, *item.Name)
			}
		}
	}
	return names, nil
}

// isAlreadyExists reports whether err is the service rejecting a conditional
// If-None-Match: * upload because a blob is already there. Azure answers with
// either BlobAlreadyExists (409) or ConditionNotMet (412) depending on the
// path, so both count.
func isAlreadyExists(err error) bool {
	return bloberror.HasCode(err, bloberror.BlobAlreadyExists) ||
		bloberror.HasCode(err, bloberror.ConditionNotMet)
}
