// Package s3 implements a payload.Host on an S3-compatible object store:
// AWS S3, MinIO, Ceph RGW, Cloudflare R2 and the like.
//
// Publish writes each file to <prefix>/<cluster><path> and hands back a
// presigned GET URL with a bounded lifetime, so the user-data the node boots
// with never carries a credential. Remove deletes everything under
// <prefix>/<cluster>/ and succeeds when nothing is there.
//
// Credentials are static when an access key pair is given, otherwise the
// standard AWS credential chain (environment, shared config, instance role)
// applies. Request and response checksums are only computed when the
// operation requires them, so stores that lag behind AWS on checksum support
// keep working.
package s3

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"net/http"
	"net/url"
	"path"
	"regexp"
	"strings"
	"time"

	"github.com/aws/aws-sdk-go-v2/aws"
	awsconfig "github.com/aws/aws-sdk-go-v2/config"
	"github.com/aws/aws-sdk-go-v2/credentials"
	awss3 "github.com/aws/aws-sdk-go-v2/service/s3"

	"github.com/klotho-k8s/klotho/provider/payload"
)

// Defaults.
const (
	// DefaultPresignTTL is how long a fetch URL stays valid when the
	// configuration does not say.
	DefaultPresignTTL = time.Hour
	// DefaultRegion is used when no region is configured. Most
	// S3-compatible stores ignore it; AWS needs the bucket's real region.
	DefaultRegion = "us-east-1"
	// ContentType is set on every published object. Nothing a bootstrap
	// node fetches needs to be sniffed or rendered.
	ContentType = "application/octet-stream"
)

var _ payload.Host = (*Host)(nil)

var clusterPattern = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9._-]{0,62}$`)

// Config is the host's settings. Bucket is required; everything else has a
// default.
type Config struct {
	// Endpoint is the store's URL, e.g. http://minio:9000 or
	// https://<account>.r2.cloudflarestorage.com. Empty means AWS S3.
	Endpoint string
	// Region is the signing region; see DefaultRegion.
	Region string
	// Bucket must already exist.
	Bucket string
	// Prefix is a key prefix below which everything is published. Leading
	// and trailing slashes are ignored.
	Prefix string
	// UsePathStyle addresses the bucket in the path (host/bucket/key)
	// rather than the host (bucket.host/key). MinIO needs it; AWS does not.
	UsePathStyle bool
	// PresignTTL bounds how long a fetch URL is valid; see
	// DefaultPresignTTL.
	PresignTTL time.Duration
	// AccessKeyID and SecretAccessKey are static credentials. Both or
	// neither must be set; neither selects the AWS credential chain.
	AccessKeyID     string
	SecretAccessKey string
}

// Option adjusts how New builds the host.
type Option func(*options)

type options struct {
	httpClient *http.Client
}

// WithHTTPClient makes the host send requests through c.
func WithHTTPClient(c *http.Client) Option {
	return func(o *options) { o.httpClient = c }
}

// Host is a payload.Host on an S3 bucket.
type Host struct {
	client  *awss3.Client
	presign *awss3.PresignClient
	bucket  string
	prefix  string
	ttl     time.Duration
}

// New builds a host for cfg. It does not touch the network: the bucket is
// not checked and credentials are resolved on first use.
func New(ctx context.Context, cfg Config, opts ...Option) (*Host, error) {
	var o options
	for _, opt := range opts {
		opt(&o)
	}
	if cfg.Bucket == "" {
		return nil, errors.New("s3 payload: bucket is required")
	}
	if (cfg.AccessKeyID == "") != (cfg.SecretAccessKey == "") {
		return nil, errors.New("s3 payload: access key ID and secret access key must be set together")
	}
	if cfg.PresignTTL < 0 {
		return nil, fmt.Errorf("s3 payload: presign TTL %s is negative", cfg.PresignTTL)
	}
	if cfg.Endpoint != "" {
		u, err := url.Parse(cfg.Endpoint)
		if err != nil || u.Host == "" || (u.Scheme != "http" && u.Scheme != "https") {
			return nil, fmt.Errorf("s3 payload: endpoint %q must be an http(s) URL", cfg.Endpoint)
		}
	}
	region := cfg.Region
	if region == "" {
		region = DefaultRegion
	}
	ttl := cfg.PresignTTL
	if ttl == 0 {
		ttl = DefaultPresignTTL
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
			return nil, fmt.Errorf("s3 payload: load AWS configuration: %w", err)
		}
	}
	if o.httpClient != nil {
		awsCfg.HTTPClient = o.httpClient
	}

	client := awss3.NewFromConfig(awsCfg, func(so *awss3.Options) {
		if cfg.Endpoint != "" {
			so.BaseEndpoint = aws.String(cfg.Endpoint)
		}
		so.UsePathStyle = cfg.UsePathStyle
		so.RequestChecksumCalculation = aws.RequestChecksumCalculationWhenRequired
		so.ResponseChecksumValidation = aws.ResponseChecksumValidationWhenRequired
		so.DisableLogOutputChecksumValidationSkipped = true
	})
	return &Host{
		client:  client,
		presign: awss3.NewPresignClient(client),
		bucket:  cfg.Bucket,
		prefix:  strings.Trim(cfg.Prefix, "/"),
		ttl:     ttl,
	}, nil
}

// Name returns "s3".
func (h *Host) Name() string { return "s3" }

// Publish writes every file under the cluster's prefix and returns a fetch
// per file whose command downloads it through a presigned URL. If any write
// fails, whatever was written for the cluster is removed again and the
// error is returned, so a failed Publish leaves nothing behind.
func (h *Host) Publish(ctx context.Context, cluster string, files []payload.File) ([]payload.Fetch, error) {
	if err := validateCluster(cluster); err != nil {
		return nil, err
	}
	keys := make([]string, len(files))
	seen := make(map[string]bool, len(files))
	for i, f := range files {
		k, err := h.key(cluster, f.Path)
		if err != nil {
			return nil, err
		}
		if seen[k] {
			return nil, fmt.Errorf("s3 payload: %s is listed twice", f.Path)
		}
		seen[k] = true
		keys[i] = k
	}

	fetches := make([]payload.Fetch, 0, len(files))
	for i, f := range files {
		if _, err := h.client.PutObject(ctx, &awss3.PutObjectInput{
			Bucket:        aws.String(h.bucket),
			Key:           aws.String(keys[i]),
			Body:          bytes.NewReader(f.Content),
			ContentLength: aws.Int64(int64(len(f.Content))),
			ContentType:   aws.String(ContentType),
		}); err != nil {
			h.cleanup(ctx, cluster)
			return nil, fmt.Errorf("s3 payload: put %s: %w", keys[i], err)
		}
		req, err := h.presign.PresignGetObject(ctx, &awss3.GetObjectInput{
			Bucket: aws.String(h.bucket),
			Key:    aws.String(keys[i]),
		}, awss3.WithPresignExpires(h.ttl))
		if err != nil {
			h.cleanup(ctx, cluster)
			return nil, fmt.Errorf("s3 payload: presign %s: %w", keys[i], err)
		}
		fetches = append(fetches, payload.Fetch{
			Path:    f.Path,
			Command: fetchCommand(req.URL, f.Path),
		})
	}
	return fetches, nil
}

// Remove deletes every object under the cluster's prefix. Nothing to delete
// is not an error.
func (h *Host) Remove(ctx context.Context, cluster string) error {
	if err := validateCluster(cluster); err != nil {
		return err
	}
	keys, err := h.list(ctx, cluster)
	if err != nil {
		return err
	}
	for _, k := range keys {
		if _, err := h.client.DeleteObject(ctx, &awss3.DeleteObjectInput{
			Bucket: aws.String(h.bucket),
			Key:    aws.String(k),
		}); err != nil {
			return fmt.Errorf("s3 payload: delete %s: %w", k, err)
		}
	}
	return nil
}

// cleanup is Remove with a fresh context and no error, for undoing a
// partial Publish whose own context may already be done.
func (h *Host) cleanup(ctx context.Context, cluster string) {
	if ctx.Err() != nil {
		var cancel context.CancelFunc
		ctx, cancel = context.WithTimeout(context.WithoutCancel(ctx), 30*time.Second)
		defer cancel()
	}
	_ = h.Remove(ctx, cluster)
}

// list returns every key under the cluster's prefix.
func (h *Host) list(ctx context.Context, cluster string) ([]string, error) {
	prefix := h.clusterPrefix(cluster)
	var keys []string
	pages := awss3.NewListObjectsV2Paginator(h.client, &awss3.ListObjectsV2Input{
		Bucket: aws.String(h.bucket),
		Prefix: aws.String(prefix),
	})
	for pages.HasMorePages() {
		page, err := pages.NextPage(ctx)
		if err != nil {
			return nil, fmt.Errorf("s3 payload: list %s: %w", prefix, err)
		}
		for _, obj := range page.Contents {
			if obj.Key != nil {
				keys = append(keys, *obj.Key)
			}
		}
	}
	return keys, nil
}

// clusterPrefix is the key prefix every object of the cluster lives under,
// with a trailing slash so a cluster named "a" never matches "ab".
func (h *Host) clusterPrefix(cluster string) string {
	if h.prefix == "" {
		return cluster + "/"
	}
	return h.prefix + "/" + cluster + "/"
}

// key maps a node path to an object key below the cluster's prefix. The
// path must be absolute and already clean, so a key can never escape the
// prefix.
func (h *Host) key(cluster, p string) (string, error) {
	if !path.IsAbs(p) || path.Clean(p) != p || p == "/" {
		return "", fmt.Errorf("s3 payload: file path %q must be an absolute, clean path", p)
	}
	return h.clusterPrefix(cluster) + strings.TrimPrefix(p, "/"), nil
}

func validateCluster(name string) error {
	if !clusterPattern.MatchString(name) {
		return fmt.Errorf("s3 payload: invalid cluster name %q: must match %s", name, clusterPattern)
	}
	return nil
}

// fetchCommand is what cloud-init runs to download one file. It works with
// nothing but curl and a POSIX shell; retries cover a store that is briefly
// unreachable while the node's network settles.
func fetchCommand(url, dest string) string {
	return fmt.Sprintf("mkdir -p %s && curl --fail --silent --show-error --location --retry 5 --retry-delay 3 --output %s %s",
		shellQuote(path.Dir(dest)), shellQuote(dest), shellQuote(url))
}

// shellQuote single-quotes s for a POSIX shell.
func shellQuote(s string) string {
	return "'" + strings.ReplaceAll(s, "'", `'\''`) + "'"
}
