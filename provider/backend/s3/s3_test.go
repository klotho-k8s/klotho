package s3

import (
	"bytes"
	"context"
	"errors"
	"io"
	"net/http"
	"sort"
	"strings"
	"sync"
	"testing"

	"github.com/aws/aws-sdk-go-v2/aws"
	awss3 "github.com/aws/aws-sdk-go-v2/service/s3"
	s3types "github.com/aws/aws-sdk-go-v2/service/s3/types"
	smithy "github.com/aws/smithy-go"
	smithyhttp "github.com/aws/smithy-go/transport/http"

	"github.com/klotho-k8s/klotho/pkg/cluster"
	"github.com/klotho-k8s/klotho/provider/backend"
)

// fakeS3 is an in-memory api: just enough of S3 for the store, including the
// conditional-write semantics of If-None-Match that Reserve depends on.
type fakeS3 struct {
	mu      sync.Mutex
	objects map[string][]byte

	puts, gets, heads, deletes, lists int

	// failPut makes PutObject of this key fail with a generic error, to test
	// that a write error is surfaced rather than swallowed.
	failPut string
}

func newFakeS3() *fakeS3 { return &fakeS3{objects: map[string][]byte{}} }

func (f *fakeS3) PutObject(_ context.Context, in *awss3.PutObjectInput, _ ...func(*awss3.Options)) (*awss3.PutObjectOutput, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.puts++
	key := aws.ToString(in.Key)
	if key == f.failPut {
		return nil, errors.New("boom")
	}
	if in.IfNoneMatch != nil && *in.IfNoneMatch == anyETag {
		if _, exists := f.objects[key]; exists {
			return nil, preconditionFailed()
		}
	}
	body, err := io.ReadAll(in.Body)
	if err != nil {
		return nil, err
	}
	f.objects[key] = body
	return &awss3.PutObjectOutput{}, nil
}

func (f *fakeS3) GetObject(_ context.Context, in *awss3.GetObjectInput, _ ...func(*awss3.Options)) (*awss3.GetObjectOutput, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.gets++
	body, ok := f.objects[aws.ToString(in.Key)]
	if !ok {
		return nil, &s3types.NoSuchKey{}
	}
	return &awss3.GetObjectOutput{Body: io.NopCloser(bytes.NewReader(body))}, nil
}

func (f *fakeS3) HeadObject(_ context.Context, in *awss3.HeadObjectInput, _ ...func(*awss3.Options)) (*awss3.HeadObjectOutput, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.heads++
	if _, ok := f.objects[aws.ToString(in.Key)]; !ok {
		return nil, &s3types.NotFound{}
	}
	return &awss3.HeadObjectOutput{}, nil
}

func (f *fakeS3) DeleteObject(_ context.Context, in *awss3.DeleteObjectInput, _ ...func(*awss3.Options)) (*awss3.DeleteObjectOutput, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.deletes++
	delete(f.objects, aws.ToString(in.Key))
	return &awss3.DeleteObjectOutput{}, nil
}

func (f *fakeS3) ListObjectsV2(_ context.Context, in *awss3.ListObjectsV2Input, _ ...func(*awss3.Options)) (*awss3.ListObjectsV2Output, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.lists++
	prefix := aws.ToString(in.Prefix)
	var keys []string
	for k := range f.objects {
		if strings.HasPrefix(k, prefix) {
			keys = append(keys, k)
		}
	}
	sort.Strings(keys)
	out := &awss3.ListObjectsV2Output{}
	for _, k := range keys {
		out.Contents = append(out.Contents, s3types.Object{Key: aws.String(k)})
	}
	return out, nil
}

// preconditionFailed is the error AWS S3 returns for a failed If-None-Match: a
// 412 response wrapping a PreconditionFailed API error.
func preconditionFailed() error {
	return &smithyhttp.ResponseError{
		Response: &smithyhttp.Response{Response: &http.Response{StatusCode: http.StatusPreconditionFailed}},
		Err:      &smithy.GenericAPIError{Code: "PreconditionFailed", Message: "At least one of the pre-conditions you specified did not hold"},
	}
}

// fakeSealer seals a reservation to deterministic opaque bytes. The store
// treats the result as opaque, so this never needs real encryption.
type fakeSealer struct{}

func (fakeSealer) Seal(s *cluster.State) ([]byte, error) {
	return []byte("reservation:" + s.Spec.Name), nil
}

func newStore(t *testing.T, cfg Config) (*Store, *fakeS3) {
	t.Helper()
	fake := newFakeS3()
	cfg.Bucket = "state"
	s, err := New(context.Background(), cfg, fakeSealer{}, withAPI(fake))
	if err != nil {
		t.Fatal(err)
	}
	return s, fake
}

func TestReserveIsAtomic(t *testing.T) {
	s, fake := newStore(t, Config{Prefix: "clusters"})
	ctx := context.Background()

	ok, err := s.Reserve(ctx, "demo")
	if err != nil || !ok {
		t.Fatalf("first Reserve = %v, %v; want true, nil", ok, err)
	}
	// The reservation is stored under the prefixed key with a body.
	if _, exists := fake.objects["clusters/demo.record"]; !exists {
		t.Fatalf("reservation object not written; keys = %v", keysOf(fake))
	}

	// A second Reserve of the same name loses: the conditional PUT is rejected.
	ok, err = s.Reserve(ctx, "demo")
	if err != nil {
		t.Fatalf("second Reserve error = %v; want nil", err)
	}
	if ok {
		t.Fatal("second Reserve = true; want false (name already taken)")
	}
}

func TestSaveLoadRoundTrip(t *testing.T) {
	s, _ := newStore(t, Config{Prefix: "clusters"})
	ctx := context.Background()
	record := []byte("AGE-ENCRYPTED-OPAQUE-RECORD\x00\x01\x02")

	if _, err := s.Reserve(ctx, "demo"); err != nil {
		t.Fatal(err)
	}
	if err := s.Save(ctx, "demo", record); err != nil {
		t.Fatal(err)
	}
	got, err := s.Load(ctx, "demo")
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(got, record) {
		t.Errorf("Load = %q, want %q", got, record)
	}
}

func TestLoadMissingIsNotFound(t *testing.T) {
	s, _ := newStore(t, Config{})
	_, err := s.Load(context.Background(), "ghost")
	if !errors.Is(err, backend.ErrNotFound) {
		t.Fatalf("Load of missing = %v; want backend.ErrNotFound", err)
	}
}

func TestDelete(t *testing.T) {
	s, fake := newStore(t, Config{Prefix: "clusters"})
	ctx := context.Background()
	if _, err := s.Reserve(ctx, "demo"); err != nil {
		t.Fatal(err)
	}
	if err := s.Delete(ctx, "demo"); err != nil {
		t.Fatalf("Delete = %v; want nil", err)
	}
	if len(fake.objects) != 0 {
		t.Errorf("objects after Delete = %v", keysOf(fake))
	}
	// Deleting again reports not found, matching the file and proxmox backends.
	if err := s.Delete(ctx, "demo"); !errors.Is(err, backend.ErrNotFound) {
		t.Fatalf("second Delete = %v; want backend.ErrNotFound", err)
	}
}

func TestList(t *testing.T) {
	s, fake := newStore(t, Config{Prefix: "clusters"})
	ctx := context.Background()
	for _, n := range []string{"beta", "alpha", "gamma"} {
		if _, err := s.Reserve(ctx, n); err != nil {
			t.Fatal(err)
		}
	}
	// Noise the store should ignore: a key outside the prefix, a key under the
	// prefix without the suffix, and a nested key.
	fake.objects["other/x.record"] = []byte("x")
	fake.objects["clusters/not-a-record.txt"] = []byte("x")
	fake.objects["clusters/sub/dir.record"] = []byte("x")

	names, err := s.List(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if want := []string{"alpha", "beta", "gamma"}; strings.Join(names, ",") != strings.Join(want, ",") {
		t.Errorf("List = %v, want %v", names, want)
	}
}

func TestListNoPrefix(t *testing.T) {
	s, _ := newStore(t, Config{})
	ctx := context.Background()
	if _, err := s.Reserve(ctx, "solo"); err != nil {
		t.Fatal(err)
	}
	names, err := s.List(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Join(names, ",") != "solo" {
		t.Errorf("List = %v, want [solo]", names)
	}
}

func TestKeyLayout(t *testing.T) {
	for _, tc := range []struct {
		prefix, name, want string
	}{
		{"", "demo", "demo.record"},
		{"clusters", "demo", "clusters/demo.record"},
		{"/a/b/", "demo", "a/b/demo.record"},
	} {
		s := &Store{prefix: strings.Trim(tc.prefix, "/")}
		got, err := s.key(tc.name)
		if err != nil || got != tc.want {
			t.Errorf("key(prefix=%q, %q) = %q, %v; want %q", tc.prefix, tc.name, got, err, tc.want)
		}
	}
}

func TestInvalidNamesRejectedWithoutRequests(t *testing.T) {
	s, fake := newStore(t, Config{})
	ctx := context.Background()
	for _, name := range []string{"", ".", "..", "a/b", "-a", strings.Repeat("a", 64)} {
		if _, err := s.Reserve(ctx, name); err == nil {
			t.Errorf("Reserve(%q) accepted", name)
		}
		if _, err := s.Load(ctx, name); err == nil {
			t.Errorf("Load(%q) accepted", name)
		}
		if err := s.Delete(ctx, name); err == nil {
			t.Errorf("Delete(%q) accepted", name)
		}
	}
	if fake.puts+fake.gets+fake.heads+fake.deletes != 0 {
		t.Errorf("requests were sent for invalid names: %+v", fake)
	}
}

func TestSaveErrorSurfaces(t *testing.T) {
	s, fake := newStore(t, Config{Prefix: "p"})
	fake.failPut = "p/demo.record"
	err := s.Save(context.Background(), "demo", []byte("x"))
	if err == nil || !strings.Contains(err.Error(), "boom") {
		t.Fatalf("Save error = %v; want it to carry the underlying failure", err)
	}
}

func TestNewValidation(t *testing.T) {
	ctx := context.Background()
	for name, cfg := range map[string]Config{
		"no bucket":          {AccessKeyID: "a", SecretAccessKey: "b"},
		"key without secret": {Bucket: "b", AccessKeyID: "a"},
		"secret without key": {Bucket: "b", SecretAccessKey: "s"},
		"bad endpoint":       {Bucket: "b", Endpoint: "minio:9000"},
		"ftp endpoint":       {Bucket: "b", Endpoint: "ftp://minio"},
	} {
		if _, err := New(ctx, cfg, fakeSealer{}, withAPI(newFakeS3())); err == nil {
			t.Errorf("%s: accepted", name)
		}
	}
	if _, err := New(ctx, Config{Bucket: "b"}, nil, withAPI(newFakeS3())); err == nil {
		t.Error("nil sealer accepted")
	}
	s, err := New(ctx, Config{Bucket: "b"}, fakeSealer{}, withAPI(newFakeS3()))
	if err != nil {
		t.Fatal(err)
	}
	if s.Name() != "s3" || s.Bucket() != "b" || s.prefix != "" {
		t.Errorf("defaults: %+v", s)
	}
}

func keysOf(f *fakeS3) []string {
	f.mu.Lock()
	defer f.mu.Unlock()
	var keys []string
	for k := range f.objects {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	return keys
}
