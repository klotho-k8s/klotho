// Package s3 implements a backend.Store on an S3-compatible object store:
// AWS S3, MinIO, Ceph RGW, Cloudflare R2 and the like. Each cluster's state
// record is one object at <prefix>/<name>.record.
//
// The store holds opaque, already-sealed bytes produced by the caller's state
// codec. It never sees plaintext state and does no encryption of its own.
//
// Reserve is atomic on stores that honour conditional writes: it PUTs the
// object with If-None-Match: *, so the store itself rejects the write when the
// key already exists and exactly one of several concurrent callers wins. AWS
// S3 and recent MinIO, Ceph RGW and R2 support this; a store that silently
// ignores the condition cannot make Reserve atomic (see package README), so
// New refuses the dangerous mistake of pointing at such a store only at the
// level of documentation, not code. Save is last-writer-wins: there is no
// compare-and-swap.
package s3

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"net/url"
	"regexp"
	"sort"
	"strings"

	"github.com/aws/aws-sdk-go-v2/aws"
	awsconfig "github.com/aws/aws-sdk-go-v2/config"
	"github.com/aws/aws-sdk-go-v2/credentials"
	awss3 "github.com/aws/aws-sdk-go-v2/service/s3"
	s3types "github.com/aws/aws-sdk-go-v2/service/s3/types"
	smithyhttp "github.com/aws/smithy-go/transport/http"

	"github.com/klotho-k8s/klotho/pkg/cluster"
	"github.com/klotho-k8s/klotho/pkg/state"
	"github.com/klotho-k8s/klotho/provider/backend"
)

// Constants shaping the object layout and the client.
const (
	// Suffix is the key extension of a cluster record. It matches the file
	// backend so a bucket and a directory of records read alike.
	Suffix = ".record"
	// DefaultRegion is used when no region is configured. Most S3-compatible
	// stores ignore it; AWS needs the bucket's real region.
	DefaultRegion = "us-east-1"
	// ContentType is set on every record object. A record is opaque bytes,
	// never anything a client should sniff or render.
	ContentType = "application/octet-stream"
	// anyETag is the If-None-Match value that makes a PUT succeed only when no
	// object exists at the key.
	anyETag = "*"
)

var _ backend.Store = (*Store)(nil)

var namePattern = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9._-]{0,62}$`)

// api is the slice of the S3 client the store uses. *awss3.Client satisfies
// it; tests fake it.
type api interface {
	PutObject(context.Context, *awss3.PutObjectInput, ...func(*awss3.Options)) (*awss3.PutObjectOutput, error)
	GetObject(context.Context, *awss3.GetObjectInput, ...func(*awss3.Options)) (*awss3.GetObjectOutput, error)
	HeadObject(context.Context, *awss3.HeadObjectInput, ...func(*awss3.Options)) (*awss3.HeadObjectOutput, error)
	DeleteObject(context.Context, *awss3.DeleteObjectInput, ...func(*awss3.Options)) (*awss3.DeleteObjectOutput, error)
	ListObjectsV2(context.Context, *awss3.ListObjectsV2Input, ...func(*awss3.Options)) (*awss3.ListObjectsV2Output, error)
}

// Sealer turns a state into an opaque record. The store uses it only to write
// reservation records, so it never handles keys itself.
type Sealer interface {
	Seal(*cluster.State) ([]byte, error)
}

// Config is the store's settings. Bucket is required; everything else has a
// default.
type Config struct {
	// Endpoint is the store's URL, e.g. http://minio:9000 or
	// https://<account>.r2.cloudflarestorage.com. Empty means AWS S3.
	Endpoint string
	// Region is the signing region; see DefaultRegion.
	Region string
	// Bucket must already exist.
	Bucket string
	// Prefix is a key prefix below which every record is stored. Leading and
	// trailing slashes are ignored.
	Prefix string
	// UsePathStyle addresses the bucket in the path (host/bucket/key) rather
	// than the host (bucket.host/key). MinIO needs it; AWS does not.
	UsePathStyle bool
	// AccessKeyID and SecretAccessKey are static credentials. Both or neither
	// must be set; neither selects the default AWS credential chain
	// (environment, shared config, instance role).
	AccessKeyID     string
	SecretAccessKey string
}

// Option adjusts how New builds the store.
type Option func(*options)

type options struct {
	client api
}

// withAPI makes the store use client instead of a real S3 client. Tests use
// it; New still validates the configuration.
func withAPI(client api) Option {
	return func(o *options) { o.client = client }
}

// Store is a backend.Store on an S3 bucket.
type Store struct {
	client api
	sealer Sealer
	bucket string
	prefix string
}

// New builds a store for cfg, sealing reservation records with sealer. It
// does not touch the network: the bucket is not checked and credentials are
// resolved on first use.
func New(ctx context.Context, cfg Config, sealer Sealer, opts ...Option) (*Store, error) {
	var o options
	for _, opt := range opts {
		opt(&o)
	}
	if cfg.Bucket == "" {
		return nil, errors.New("s3 backend: bucket is required")
	}
	if sealer == nil {
		return nil, errors.New("s3 backend: sealer is required")
	}
	if (cfg.AccessKeyID == "") != (cfg.SecretAccessKey == "") {
		return nil, errors.New("s3 backend: access key ID and secret access key must be set together")
	}
	if cfg.Endpoint != "" {
		u, err := url.Parse(cfg.Endpoint)
		if err != nil || u.Host == "" || (u.Scheme != "http" && u.Scheme != "https") {
			return nil, fmt.Errorf("s3 backend: endpoint %q must be an http(s) URL", cfg.Endpoint)
		}
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

// newClient builds a real S3 client for cfg, reusing the payload host's
// conventions: static credentials when a key pair is given, otherwise the
// default AWS credential chain; checksums only when an operation requires
// them, so stores that lag AWS on checksum support keep working.
func newClient(ctx context.Context, cfg Config) (api, error) {
	region := cfg.Region
	if region == "" {
		region = DefaultRegion
	}
	var awsCfg aws.Config
	if cfg.AccessKeyID != "" {
		awsCfg = aws.Config{
			Region:      region,
			Credentials: credentials.NewStaticCredentialsProvider(cfg.AccessKeyID, cfg.SecretAccessKey, ""),
		}
	} else {
		var err error
		awsCfg, err = awsconfig.LoadDefaultConfig(ctx, awsconfig.WithRegion(region))
		if err != nil {
			return nil, fmt.Errorf("s3 backend: load AWS configuration: %w", err)
		}
	}
	return awss3.NewFromConfig(awsCfg, func(so *awss3.Options) {
		if cfg.Endpoint != "" {
			so.BaseEndpoint = aws.String(cfg.Endpoint)
		}
		so.UsePathStyle = cfg.UsePathStyle
		so.RequestChecksumCalculation = aws.RequestChecksumCalculationWhenRequired
		so.ResponseChecksumValidation = aws.ResponseChecksumValidationWhenRequired
		so.DisableLogOutputChecksumValidationSkipped = true
	}), nil
}

// Name returns "s3".
func (s *Store) Name() string { return "s3" }

// Bucket returns the bucket holding the records.
func (s *Store) Bucket() string { return s.bucket }

// Reserve claims name with a conditional PutObject (If-None-Match: *) of a
// sealed reservation record. The store rejects the write when the key already
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
		return false, fmt.Errorf("s3 backend: seal reservation for %s: %w", name, err)
	}
	_, err = s.client.PutObject(ctx, &awss3.PutObjectInput{
		Bucket:        aws.String(s.bucket),
		Key:           aws.String(key),
		Body:          bytes.NewReader(record),
		ContentLength: aws.Int64(int64(len(record))),
		ContentType:   aws.String(ContentType),
		IfNoneMatch:   aws.String(anyETag),
	})
	if err != nil {
		if isPreconditionFailed(err) {
			return false, nil
		}
		return false, fmt.Errorf("s3 backend: reserve %s: %w", name, err)
	}
	return true, nil
}

// Load returns the record for name, or backend.ErrNotFound.
func (s *Store) Load(ctx context.Context, name string) ([]byte, error) {
	key, err := s.key(name)
	if err != nil {
		return nil, err
	}
	out, err := s.client.GetObject(ctx, &awss3.GetObjectInput{
		Bucket: aws.String(s.bucket),
		Key:    aws.String(key),
	})
	if err != nil {
		if isNotFound(err) {
			return nil, fmt.Errorf("s3 backend: %s: %w", name, backend.ErrNotFound)
		}
		return nil, fmt.Errorf("s3 backend: load %s: %w", name, err)
	}
	defer out.Body.Close()
	b, err := io.ReadAll(out.Body)
	if err != nil {
		return nil, fmt.Errorf("s3 backend: load %s: %w", name, err)
	}
	return b, nil
}

// Save writes the record for name, replacing any reservation or previous
// record.
func (s *Store) Save(ctx context.Context, name string, record []byte) error {
	key, err := s.key(name)
	if err != nil {
		return err
	}
	if _, err := s.client.PutObject(ctx, &awss3.PutObjectInput{
		Bucket:        aws.String(s.bucket),
		Key:           aws.String(key),
		Body:          bytes.NewReader(record),
		ContentLength: aws.Int64(int64(len(record))),
		ContentType:   aws.String(ContentType),
	}); err != nil {
		return fmt.Errorf("s3 backend: save %s: %w", name, err)
	}
	return nil
}

// Delete removes the record for name, or returns backend.ErrNotFound. S3's
// DeleteObject is itself idempotent and reports nothing about whether the key
// existed, so Delete first confirms the object is there with HeadObject to
// keep the same not-found contract as the other backends.
func (s *Store) Delete(ctx context.Context, name string) error {
	key, err := s.key(name)
	if err != nil {
		return err
	}
	if _, err := s.client.HeadObject(ctx, &awss3.HeadObjectInput{
		Bucket: aws.String(s.bucket),
		Key:    aws.String(key),
	}); err != nil {
		if isNotFound(err) {
			return fmt.Errorf("s3 backend: %s: %w", name, backend.ErrNotFound)
		}
		return fmt.Errorf("s3 backend: delete %s: %w", name, err)
	}
	if _, err := s.client.DeleteObject(ctx, &awss3.DeleteObjectInput{
		Bucket: aws.String(s.bucket),
		Key:    aws.String(key),
	}); err != nil {
		return fmt.Errorf("s3 backend: delete %s: %w", name, err)
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
	var names []string
	pages := awss3.NewListObjectsV2Paginator(s.client, &awss3.ListObjectsV2Input{
		Bucket: aws.String(s.bucket),
		Prefix: aws.String(listPrefix),
	})
	for pages.HasMorePages() {
		page, err := pages.NextPage(ctx)
		if err != nil {
			return nil, fmt.Errorf("s3 backend: list %s: %w", listPrefix, err)
		}
		for _, obj := range page.Contents {
			if obj.Key == nil {
				continue
			}
			name, ok := strings.CutPrefix(*obj.Key, listPrefix)
			if !ok {
				continue
			}
			name, ok = strings.CutSuffix(name, Suffix)
			if !ok || strings.Contains(name, "/") || ValidateName(name) != nil {
				continue
			}
			names = append(names, name)
		}
	}
	sort.Strings(names)
	return names, nil
}

// ValidateName reports whether name is usable as a cluster name: it must match
// ^[A-Za-z0-9][A-Za-z0-9._-]{0,62}$, the same shape the file and proxmox
// backends accept.
func ValidateName(name string) error {
	if !namePattern.MatchString(name) {
		return fmt.Errorf("s3 backend: invalid cluster name %q: must match %s", name, namePattern)
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

// isPreconditionFailed reports whether err is a conditional-write rejection: a
// 412 from If-None-Match, which is how S3 says the key already exists.
func isPreconditionFailed(err error) bool {
	return httpStatus(err) == 412
}

// isNotFound reports whether err is an S3 "no such key" or "not found", from
// either a typed error or a 404 status.
func isNotFound(err error) bool {
	var nsk *s3types.NoSuchKey
	var nf *s3types.NotFound
	if errors.As(err, &nsk) || errors.As(err, &nf) {
		return true
	}
	return httpStatus(err) == 404
}

// httpStatus returns the HTTP status code of err's S3 response, or 0.
func httpStatus(err error) int {
	var re *smithyhttp.ResponseError
	if errors.As(err, &re) && re.Response != nil {
		return re.Response.StatusCode
	}
	return 0
}
