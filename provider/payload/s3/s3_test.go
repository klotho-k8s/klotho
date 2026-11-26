package s3

import (
	"bytes"
	"context"
	"encoding/xml"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"sort"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/klotho-k8s/klotho/provider/payload"
)

// fakeStore is an httptest handler that speaks just enough path-style S3
// for the host: PutObject, DeleteObject, ListObjectsV2 and GetObject. It
// never checks signatures.
type fakeStore struct {
	mu      sync.Mutex
	bucket  string
	objects map[string]object
	calls   []call

	// failPut makes PutObject of this key fail with AccessDenied.
	failPut string
}

type object struct {
	body        []byte
	contentType string
}

type call struct {
	method, key string
}

func newFakeStore(bucket string) *fakeStore {
	return &fakeStore{bucket: bucket, objects: map[string]object{}}
}

func (f *fakeStore) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	f.mu.Lock()
	defer f.mu.Unlock()
	bucket, key, _ := strings.Cut(strings.TrimPrefix(r.URL.Path, "/"), "/")
	if bucket != f.bucket {
		s3Error(w, http.StatusNotFound, "NoSuchBucket")
		return
	}
	f.calls = append(f.calls, call{r.Method, key})
	switch {
	case r.Method == http.MethodPut:
		if key == f.failPut {
			s3Error(w, http.StatusForbidden, "AccessDenied")
			return
		}
		body, err := io.ReadAll(r.Body)
		if err != nil {
			s3Error(w, http.StatusBadRequest, "IncompleteBody")
			return
		}
		f.objects[key] = object{body: body, contentType: r.Header.Get("Content-Type")}
		w.Header().Set("ETag", `"fake"`)
		w.WriteHeader(http.StatusOK)
	case r.Method == http.MethodDelete:
		delete(f.objects, key)
		w.WriteHeader(http.StatusNoContent)
	case r.Method == http.MethodGet && key == "":
		prefix := r.URL.Query().Get("prefix")
		var keys []string
		for k := range f.objects {
			if strings.HasPrefix(k, prefix) {
				keys = append(keys, k)
			}
		}
		sort.Strings(keys)
		type contents struct {
			Key  string `xml:"Key"`
			Size int    `xml:"Size"`
		}
		res := struct {
			XMLName     xml.Name   `xml:"ListBucketResult"`
			Name        string     `xml:"Name"`
			Prefix      string     `xml:"Prefix"`
			KeyCount    int        `xml:"KeyCount"`
			IsTruncated bool       `xml:"IsTruncated"`
			Contents    []contents `xml:"Contents"`
		}{Name: f.bucket, Prefix: prefix, KeyCount: len(keys)}
		for _, k := range keys {
			res.Contents = append(res.Contents, contents{Key: k, Size: len(f.objects[k].body)})
		}
		w.Header().Set("Content-Type", "application/xml")
		_ = xml.NewEncoder(w).Encode(res)
	case r.Method == http.MethodGet:
		o, ok := f.objects[key]
		if !ok {
			s3Error(w, http.StatusNotFound, "NoSuchKey")
			return
		}
		w.Header().Set("Content-Type", o.contentType)
		_, _ = w.Write(o.body)
	default:
		s3Error(w, http.StatusMethodNotAllowed, "MethodNotAllowed")
	}
}

func s3Error(w http.ResponseWriter, status int, code string) {
	w.Header().Set("Content-Type", "application/xml")
	w.WriteHeader(status)
	_, _ = fmt.Fprintf(w, `<?xml version="1.0" encoding="UTF-8"?><Error><Code>%s</Code><Message>%s</Message></Error>`, code, code)
}

func (f *fakeStore) keys() []string {
	f.mu.Lock()
	defer f.mu.Unlock()
	var keys []string
	for k := range f.objects {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	return keys
}

func (f *fakeStore) callsOf(method string) []string {
	f.mu.Lock()
	defer f.mu.Unlock()
	var keys []string
	for _, c := range f.calls {
		if c.method == method {
			keys = append(keys, c.key)
		}
	}
	return keys
}

func (f *fakeStore) reset() {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.calls = nil
}

const (
	testBucket = "klotho-test"
	testKeyID  = "AKIATESTKEY"
	testSecret = "test-secret"
)

func newHost(t *testing.T, cfg Config) (*Host, *fakeStore, *httptest.Server) {
	t.Helper()
	store := newFakeStore(testBucket)
	srv := httptest.NewServer(store)
	t.Cleanup(srv.Close)
	cfg.Endpoint = srv.URL
	cfg.Bucket = testBucket
	cfg.UsePathStyle = true
	cfg.AccessKeyID = testKeyID
	cfg.SecretAccessKey = testSecret
	h, err := New(context.Background(), cfg)
	if err != nil {
		t.Fatal(err)
	}
	return h, store, srv
}

// urlFromCommand returns the last single-quoted word of a fetch command,
// which is where the URL goes.
func urlFromCommand(t *testing.T, cmd string) *url.URL {
	t.Helper()
	i := strings.LastIndex(strings.TrimSuffix(cmd, "'"), "'")
	if i < 0 {
		t.Fatalf("no quoted URL in %q", cmd)
	}
	u, err := url.Parse(strings.Trim(cmd[i:], "'"))
	if err != nil {
		t.Fatalf("parse URL from %q: %v", cmd, err)
	}
	return u
}

func TestPublishAndRemove(t *testing.T) {
	h, store, srv := newHost(t, Config{Prefix: "/pre/fix/", Region: "eu-west-1"})
	ctx := context.Background()
	files := []payload.File{
		{Path: "/opt/big.tar", Content: bytes.Repeat([]byte("x"), 70000)},
		{Path: "/etc/klotho/manifest.yaml", Content: []byte("kind: Cluster\n")},
	}

	fetches, err := h.Publish(ctx, "demo", files)
	if err != nil {
		t.Fatal(err)
	}
	if len(fetches) != 2 {
		t.Fatalf("fetches = %d", len(fetches))
	}
	wantKeys := []string{"pre/fix/demo/etc/klotho/manifest.yaml", "pre/fix/demo/opt/big.tar"}
	if got := store.keys(); strings.Join(got, ",") != strings.Join(wantKeys, ",") {
		t.Errorf("objects = %v, want %v", got, wantKeys)
	}
	if got := store.callsOf(http.MethodPut); len(got) != 2 {
		t.Errorf("PUT calls = %v", got)
	}
	for i, f := range files {
		store.mu.Lock()
		o := store.objects[wantKeys[1-i]]
		store.mu.Unlock()
		if !bytes.Equal(o.body, f.Content) {
			t.Errorf("%s: body differs", f.Path)
		}
		if o.contentType != ContentType {
			t.Errorf("%s: content type %q", f.Path, o.contentType)
		}
	}

	srvURL, _ := url.Parse(srv.URL)
	for i, f := range fetches {
		if f.Path != files[i].Path {
			t.Errorf("fetch %d path = %s", i, f.Path)
		}
		if !strings.HasPrefix(f.Command, "mkdir -p ") || !strings.Contains(f.Command, "curl --fail") {
			t.Errorf("fetch %d command = %s", i, f.Command)
		}
		if !strings.Contains(f.Command, "--output '"+files[i].Path+"'") {
			t.Errorf("fetch %d does not write %s: %s", i, files[i].Path, f.Command)
		}
		if strings.Contains(f.Command, testSecret) {
			t.Errorf("fetch %d leaks the secret", i)
		}
		u := urlFromCommand(t, f.Command)
		if u.Host != srvURL.Host || u.Scheme != "http" {
			t.Errorf("fetch %d URL host = %s://%s", i, u.Scheme, u.Host)
		}
		if want := "/" + testBucket + "/" + wantKeys[1-i]; u.Path != want {
			t.Errorf("fetch %d URL path = %s, want %s", i, u.Path, want)
		}
		q := u.Query()
		if q.Get("X-Amz-Algorithm") != "AWS4-HMAC-SHA256" {
			t.Errorf("fetch %d algorithm = %q", i, q.Get("X-Amz-Algorithm"))
		}
		if c := q.Get("X-Amz-Credential"); !strings.HasPrefix(c, testKeyID+"/") || !strings.Contains(c, "/eu-west-1/s3/aws4_request") {
			t.Errorf("fetch %d credential = %q", i, c)
		}
		if q.Get("X-Amz-Expires") != "3600" {
			t.Errorf("fetch %d expires = %q", i, q.Get("X-Amz-Expires"))
		}
		if q.Get("X-Amz-SignedHeaders") != "host" {
			t.Errorf("fetch %d signed headers = %q", i, q.Get("X-Amz-SignedHeaders"))
		}
		if len(q.Get("X-Amz-Signature")) != 64 {
			t.Errorf("fetch %d signature = %q", i, q.Get("X-Amz-Signature"))
		}
		if q.Get("X-Amz-Date") == "" {
			t.Errorf("fetch %d has no date", i)
		}

		// The URL alone, with no credentials, serves the bytes.
		res, err := http.Get(u.String())
		if err != nil {
			t.Fatal(err)
		}
		body, _ := io.ReadAll(res.Body)
		_ = res.Body.Close()
		if res.StatusCode != http.StatusOK || !bytes.Equal(body, files[i].Content) {
			t.Errorf("fetch %d GET: status %d, %d bytes", i, res.StatusCode, len(body))
		}
	}

	store.reset()
	if err := h.Remove(ctx, "demo"); err != nil {
		t.Fatal(err)
	}
	if got := store.keys(); len(got) != 0 {
		t.Errorf("objects after Remove = %v", got)
	}
	if got := store.callsOf(http.MethodDelete); strings.Join(got, ",") != strings.Join(wantKeys, ",") {
		t.Errorf("DELETE calls = %v, want %v", got, wantKeys)
	}

	// Removing again finds nothing and deletes nothing.
	store.reset()
	if err := h.Remove(ctx, "demo"); err != nil {
		t.Errorf("second Remove: %v", err)
	}
	if got := store.callsOf(http.MethodDelete); len(got) != 0 {
		t.Errorf("second Remove deleted %v", got)
	}
}

func TestRemoveOnlyTouchesTheCluster(t *testing.T) {
	h, store, _ := newHost(t, Config{})
	ctx := context.Background()
	for _, c := range []string{"a", "ab"} {
		if _, err := h.Publish(ctx, c, []payload.File{{Path: "/f", Content: []byte(c)}}); err != nil {
			t.Fatal(err)
		}
	}
	if err := h.Remove(ctx, "a"); err != nil {
		t.Fatal(err)
	}
	if got := store.keys(); strings.Join(got, ",") != "ab/f" {
		t.Errorf("objects = %v, want [ab/f]", got)
	}
}

func TestPresignTTL(t *testing.T) {
	h, _, _ := newHost(t, Config{PresignTTL: 15 * time.Minute})
	fetches, err := h.Publish(context.Background(), "demo", []payload.File{{Path: "/f", Content: []byte("x")}})
	if err != nil {
		t.Fatal(err)
	}
	if q := urlFromCommand(t, fetches[0].Command).Query(); q.Get("X-Amz-Expires") != "900" {
		t.Errorf("expires = %q", q.Get("X-Amz-Expires"))
	}
}

func TestPublishFailureLeavesNothing(t *testing.T) {
	h, store, _ := newHost(t, Config{Prefix: "p"})
	store.failPut = "p/demo/second"
	_, err := h.Publish(context.Background(), "demo", []payload.File{
		{Path: "/first", Content: []byte("1")},
		{Path: "/second", Content: []byte("2")},
	})
	if err == nil || !strings.Contains(err.Error(), "AccessDenied") || !strings.Contains(err.Error(), "p/demo/second") {
		t.Fatalf("err = %v", err)
	}
	if got := store.keys(); len(got) != 0 {
		t.Errorf("objects after failed Publish = %v", got)
	}
	if got := store.callsOf(http.MethodDelete); strings.Join(got, ",") != "p/demo/first" {
		t.Errorf("DELETE calls = %v", got)
	}
}

func TestPublishRejectsBadNamesWithoutRequests(t *testing.T) {
	h, store, _ := newHost(t, Config{})
	ctx := context.Background()
	one := []payload.File{{Path: "/f", Content: []byte("x")}}
	for _, cluster := range []string{"", ".", "..", "../x", "a/b", "-a", strings.Repeat("a", 64)} {
		if _, err := h.Publish(ctx, cluster, one); err == nil {
			t.Errorf("cluster %q accepted", cluster)
		}
		if err := h.Remove(ctx, cluster); err == nil {
			t.Errorf("Remove of cluster %q accepted", cluster)
		}
	}
	for _, p := range []string{"", "f", "./f", "/", "/a/../b", "/a/./b", "/a//b", "/a/", "/a/..", "../etc/passwd"} {
		if _, err := h.Publish(ctx, "demo", []payload.File{{Path: p, Content: []byte("x")}}); err == nil {
			t.Errorf("path %q accepted", p)
		}
	}
	if _, err := h.Publish(ctx, "demo", []payload.File{{Path: "/f"}, {Path: "/f"}}); err == nil {
		t.Error("duplicate path accepted")
	}
	if len(store.calls) != 0 {
		t.Errorf("requests were sent: %v", store.calls)
	}
}

func TestKeys(t *testing.T) {
	for _, tc := range []struct {
		prefix, cluster, path, want string
	}{
		{"", "demo", "/opt/x", "demo/opt/x"},
		{"a", "demo", "/x", "a/demo/x"},
		{"/a/b/", "demo", "/x/y", "a/b/demo/x/y"},
	} {
		h := &Host{prefix: strings.Trim(tc.prefix, "/")}
		got, err := h.key(tc.cluster, tc.path)
		if err != nil || got != tc.want {
			t.Errorf("key(%q, %q, %q) = %q, %v; want %q", tc.prefix, tc.cluster, tc.path, got, err, tc.want)
		}
	}
}

func TestNewValidation(t *testing.T) {
	ctx := context.Background()
	for name, cfg := range map[string]Config{
		"no bucket":          {AccessKeyID: "a", SecretAccessKey: "b"},
		"key without secret": {Bucket: "b", AccessKeyID: "a"},
		"secret without key": {Bucket: "b", SecretAccessKey: "s"},
		"bad endpoint":       {Bucket: "b", Endpoint: "minio:9000", AccessKeyID: "a", SecretAccessKey: "b"},
		"ftp endpoint":       {Bucket: "b", Endpoint: "ftp://minio", AccessKeyID: "a", SecretAccessKey: "b"},
		"negative ttl":       {Bucket: "b", PresignTTL: -time.Second, AccessKeyID: "a", SecretAccessKey: "b"},
	} {
		if _, err := New(ctx, cfg); err == nil {
			t.Errorf("%s: accepted", name)
		}
	}
	h, err := New(ctx, Config{Bucket: "b", AccessKeyID: "a", SecretAccessKey: "b"})
	if err != nil {
		t.Fatal(err)
	}
	if h.Name() != "s3" || h.ttl != DefaultPresignTTL || h.prefix != "" {
		t.Errorf("defaults: %+v", h)
	}
}

func TestFetchCommandQuoting(t *testing.T) {
	got := fetchCommand("http://h/b/k?X-Amz-Signature=abc&x=1", "/opt/it's here/file", "")
	want := `mkdir -p '/opt/it'\''s here' && curl --fail --silent --show-error --location --retry 5 --retry-delay 3 --output '/opt/it'\''s here/file' 'http://h/b/k?X-Amz-Signature=abc&x=1'`
	if got != want {
		t.Errorf("fetchCommand =\n%s\nwant\n%s", got, want)
	}
}

func TestFetchCommandMode(t *testing.T) {
	got := fetchCommand("http://h/k", "/etc/kubernetes/pki/ca.key", "0600")
	want := `mkdir -p '/etc/kubernetes/pki' && curl --fail --silent --show-error --location --retry 5 --retry-delay 3 --output '/etc/kubernetes/pki/ca.key' 'http://h/k' && chmod '0600' '/etc/kubernetes/pki/ca.key'`
	if got != want {
		t.Errorf("fetchCommand with mode =\n%s\nwant\n%s", got, want)
	}
}
