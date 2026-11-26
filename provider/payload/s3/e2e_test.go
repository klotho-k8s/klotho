package s3

import (
	"bytes"
	"context"
	"crypto/rand"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"os"
	"os/exec"
	"strings"
	"testing"
	"time"

	"github.com/aws/aws-sdk-go-v2/aws"
	awss3 "github.com/aws/aws-sdk-go-v2/service/s3"
	"github.com/aws/aws-sdk-go-v2/service/s3/types"

	"github.com/klotho-k8s/klotho/provider/payload"
)

// The end-to-end test boots MinIO in a podman container and drives the host
// against it. It runs only when KLOTHO_E2E_S3=1:
//
//	KLOTHO_E2E_S3=1 go test ./payload/s3/ -run E2E -v
//
// The container is removed again when the test ends, pass or fail. The
// image is left behind; remove it with "podman rmi" when it is no longer
// wanted.
//
// KLOTHO_E2E_S3_IMAGE, KLOTHO_E2E_S3_ARGS (space separated, appended to the
// container command) and KLOTHO_E2E_S3_PORT override the image, its
// arguments and the loopback port, e.g. an official MinIO build needs
// KLOTHO_E2E_S3_ARGS="server /data". The image must honour
// MINIO_ROOT_USER and MINIO_ROOT_PASSWORD and serve S3 on port 9000.
const (
	e2eGate      = "KLOTHO_E2E_S3"
	e2ePortEnv   = "KLOTHO_E2E_S3_PORT"
	e2eImageEnv  = "KLOTHO_E2E_S3_IMAGE"
	e2eArgsEnv   = "KLOTHO_E2E_S3_ARGS"
	e2eContainer = "klotho-e2e-minio"
	// e2eImage is a pinned MinIO server build. MinIO's own registries no
	// longer serve public images, so this packaging of it is used; its
	// entrypoint starts the server, so no command is passed.
	e2eImage    = "docker.io/bitnamilegacy/minio:2025.7.23-debian-12-r5"
	e2eUser     = "klotho-e2e"
	e2ePassword = "klotho-e2e-secret"
	e2eBucket   = "klotho-e2e"
)

func podman(t *testing.T, args ...string) (string, error) {
	t.Helper()
	cmd := exec.Command("podman", args...)
	out, err := cmd.CombinedOutput()
	return strings.TrimSpace(string(out)), err
}

// freePort returns the port named by KLOTHO_E2E_S3_PORT, or a loopback port
// that is free right now. podman cannot pick one itself.
func freePort(t *testing.T) string {
	t.Helper()
	if p := os.Getenv(e2ePortEnv); p != "" {
		return p
	}
	l, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer l.Close()
	_, port, _ := net.SplitHostPort(l.Addr().String())
	return port
}

// startMinIO runs MinIO on a loopback port and returns its endpoint.
func startMinIO(t *testing.T) string {
	t.Helper()
	if _, err := exec.LookPath("podman"); err != nil {
		t.Fatalf("%s is set but podman is not on PATH: %v", e2eGate, err)
	}
	if out, err := podman(t, "info", "--format", "{{.Host.Arch}}"); err != nil {
		t.Fatalf("%s is set but podman is not usable: %v\n%s", e2eGate, err, out)
	}
	// A leftover from an interrupted run would block the name. -v also
	// drops the anonymous volumes the image declares, so nothing outlives
	// the container.
	_, _ = podman(t, "rm", "-f", "-v", e2eContainer)
	t.Cleanup(func() {
		if out, err := podman(t, "rm", "-f", "-v", e2eContainer); err != nil {
			t.Errorf("podman rm -f -v %s: %v\n%s", e2eContainer, err, out)
		}
	})
	image := e2eImage
	if v := os.Getenv(e2eImageEnv); v != "" {
		image = v
	}
	port := freePort(t)
	args := []string{"run", "-d", "--name", e2eContainer,
		"-p", "127.0.0.1:" + port + ":9000",
		"-e", "MINIO_ROOT_USER=" + e2eUser,
		"-e", "MINIO_ROOT_PASSWORD=" + e2ePassword,
		image}
	args = append(args, strings.Fields(os.Getenv(e2eArgsEnv))...)
	out, err := podman(t, args...)
	if err != nil {
		t.Fatalf("podman run: %v\n%s", err, out)
	}
	endpoint := "http://127.0.0.1:" + port

	deadline := time.Now().Add(90 * time.Second)
	for {
		res, err := http.Get(endpoint + "/minio/health/live")
		if err == nil {
			res.Body.Close()
			if res.StatusCode == http.StatusOK {
				return endpoint
			}
		}
		if time.Now().After(deadline) {
			logs, _ := podman(t, "logs", e2eContainer)
			t.Fatalf("MinIO at %s did not become healthy: %v\n%s", endpoint, err, logs)
		}
		time.Sleep(500 * time.Millisecond)
	}
}

func TestE2EMinIO(t *testing.T) {
	if os.Getenv(e2eGate) != "1" {
		t.Skipf("set %s=1 to run the MinIO end-to-end test", e2eGate)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Minute)
	defer cancel()

	start := time.Now()
	endpoint := startMinIO(t)
	t.Logf("MinIO ready at %s after %s", endpoint, time.Since(start).Round(time.Millisecond))

	h, err := New(ctx, Config{
		Endpoint:        endpoint,
		Bucket:          e2eBucket,
		Prefix:          "klotho/payload",
		UsePathStyle:    true,
		PresignTTL:      5 * time.Minute,
		AccessKeyID:     e2eUser,
		SecretAccessKey: e2ePassword,
	})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := h.client.CreateBucket(ctx, &awss3.CreateBucketInput{Bucket: aws.String(e2eBucket)}); err != nil {
		t.Fatalf("create bucket: %v", err)
	}

	big := make([]byte, 3<<20)
	if _, err := rand.Read(big); err != nil {
		t.Fatal(err)
	}
	files := []payload.File{
		{Path: "/opt/klotho/images.tar", Content: big},
		{Path: "/etc/klotho/cluster.yaml", Content: []byte("kind: Cluster\n")},
	}
	fetches, err := h.Publish(ctx, "e2e", files)
	if err != nil {
		t.Fatalf("Publish: %v", err)
	}
	if len(fetches) != len(files) {
		t.Fatalf("fetches = %d", len(fetches))
	}
	keys := []string{"klotho/payload/e2e/opt/klotho/images.tar", "klotho/payload/e2e/etc/klotho/cluster.yaml"}

	for i, f := range fetches {
		// The presigned URL alone, from a client with no credentials, must
		// serve exactly the published bytes.
		u := urlFromCommand(t, f.Command)
		if !strings.HasPrefix(u.String(), endpoint+"/"+e2eBucket+"/"+keys[i]+"?") {
			t.Errorf("fetch %d URL = %s", i, u)
		}
		res, err := http.Get(u.String())
		if err != nil {
			t.Fatalf("GET %s: %v", u, err)
		}
		body, err := io.ReadAll(res.Body)
		res.Body.Close()
		if err != nil || res.StatusCode != http.StatusOK {
			t.Fatalf("GET %s: status %d, %v", u, res.StatusCode, err)
		}
		if !bytes.Equal(body, files[i].Content) {
			t.Errorf("fetch %d: got %d bytes, want %d; content differs", i, len(body), len(files[i].Content))
		}
		// Tampering with the signature must be refused.
		bad := strings.Replace(u.String(), "X-Amz-Signature=", "X-Amz-Signature=0", 1)
		if res, err := http.Get(bad); err != nil {
			t.Fatal(err)
		} else {
			res.Body.Close()
			if res.StatusCode == http.StatusOK {
				t.Errorf("fetch %d: tampered signature was accepted", i)
			}
		}

		head, err := h.client.HeadObject(ctx, &awss3.HeadObjectInput{Bucket: aws.String(e2eBucket), Key: aws.String(keys[i])})
		if err != nil {
			t.Fatalf("HeadObject %s: %v", keys[i], err)
		}
		if aws.ToInt64(head.ContentLength) != int64(len(files[i].Content)) || aws.ToString(head.ContentType) != ContentType {
			t.Errorf("HeadObject %s: length %d, type %s", keys[i], aws.ToInt64(head.ContentLength), aws.ToString(head.ContentType))
		}
	}

	if err := h.Remove(ctx, "e2e"); err != nil {
		t.Fatalf("Remove: %v", err)
	}
	for _, k := range keys {
		_, err := h.client.HeadObject(ctx, &awss3.HeadObjectInput{Bucket: aws.String(e2eBucket), Key: aws.String(k)})
		var nf *types.NotFound
		if !errors.As(err, &nf) {
			t.Errorf("HeadObject %s after Remove: %v, want NotFound", k, err)
		}
	}
	if err := h.Remove(ctx, "e2e"); err != nil {
		t.Errorf("second Remove: %v", err)
	}
	fmt.Fprintf(os.Stderr, "e2e: published, fetched, verified and removed %d objects in %s\n", len(files), time.Since(start).Round(time.Millisecond))
}
