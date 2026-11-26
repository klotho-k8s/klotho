package proxmox

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"time"

	gp "github.com/luthermonson/go-proxmox"

	"github.com/klotho-k8s/klotho/pkg/state"
)

// ProbeSizes are the comment sizes, in bytes, Probe tries.
var ProbeSizes = []int{1 << 10, 4 << 10, 8 << 10, 16 << 10, 32 << 10, 64 << 10, 128 << 10}

// cleanupTimeout bounds deleting the temporary pool, which runs even when the
// caller's context is already cancelled.
const cleanupTimeout = 30 * time.Second

// ProbeResult reports what a Proxmox server does with pool comments.
type ProbeResult struct {
	// MaxSize is the largest tried comment size, in bytes, that was stored
	// and read back byte-for-byte. Zero means none did.
	MaxSize int
	// NewlinesSurvive reports whether a comment containing "\n" read back
	// unchanged.
	NewlinesSurvive bool
	// Errors describes each size or check that failed.
	Errors []string
}

// Probe measures how the server stores pool comments: the largest of
// ProbeSizes that round-trips, and whether a newline survives. It creates the
// pool poolID for the duration of the probe and always deletes it afterwards.
// It refuses to touch a pool that already exists.
func Probe(ctx context.Context, client *gp.Client, poolID string) (*ProbeResult, error) {
	return probe(ctx, client, poolID, ProbeSizes, true)
}

// ProbeCommentLimit creates the temporary pool poolID, writes a comment of
// each size (a plain record marker followed by filler, no line breaks), reads
// it back, and returns the largest size that round-tripped byte-for-byte. A
// size that fails is simply not counted; err reports only failures to create
// or delete the pool. The pool is always deleted, and an existing pool is
// never touched.
func ProbeCommentLimit(ctx context.Context, client *gp.Client, poolID string, sizes []int) (maxOK int, err error) {
	res, err := probe(ctx, client, poolID, sizes, false)
	if res != nil {
		maxOK = res.MaxSize
	}
	return maxOK, err
}

func probe(ctx context.Context, client *gp.Client, poolID string, sizes []int, newlines bool) (res *ProbeResult, err error) {
	if err := validatePoolID(poolID); err != nil {
		return nil, err
	}
	if err := createPool(ctx, client, poolID, state.PlainMarker+"probe"); err != nil {
		if errors.Is(err, errPoolExists) {
			return nil, fmt.Errorf("proxmox backend: probe: pool %s already exists; refusing to use it", poolID)
		}
		return nil, fmt.Errorf("proxmox backend: probe: create pool %s: %w", poolID, err)
	}
	defer func() {
		dctx, cancel := context.WithTimeout(context.WithoutCancel(ctx), cleanupTimeout)
		defer cancel()
		if derr := deletePool(dctx, client, poolID); derr != nil {
			err = errors.Join(err, fmt.Errorf("proxmox backend: probe: delete temporary pool %s: %w", poolID, derr))
		}
	}()

	res = &ProbeResult{}
	for _, size := range sizes {
		if err := ctx.Err(); err != nil {
			return res, err
		}
		if size < len(state.PlainMarker) {
			res.Errors = append(res.Errors, fmt.Sprintf("size %d: smaller than the record marker", size))
			continue
		}
		comment := state.PlainMarker + filler(size-len(state.PlainMarker))
		if err := roundTrip(ctx, client, poolID, comment); err != nil {
			res.Errors = append(res.Errors, fmt.Sprintf("size %d: %v", size, err))
			continue
		}
		res.MaxSize = max(res.MaxSize, size)
	}
	if newlines {
		comment := state.PlainMarker + "first line\nsecond line"
		if err := roundTrip(ctx, client, poolID, comment); err != nil {
			res.Errors = append(res.Errors, fmt.Sprintf("newline: %v", err))
		} else {
			res.NewlinesSurvive = true
		}
	}
	return res, nil
}

// roundTrip writes comment to the pool and checks it reads back unchanged.
func roundTrip(ctx context.Context, client *gp.Client, poolID, comment string) error {
	if err := setComment(ctx, client, poolID, comment); err != nil {
		return fmt.Errorf("write: %w", err)
	}
	p, err := getPool(ctx, client, poolID)
	if err != nil {
		return fmt.Errorf("read: %w", err)
	}
	if p.Comment != comment {
		return fmt.Errorf("read back %d bytes, wrote %d; first difference at byte %d", len(p.Comment), len(comment), firstDiff(p.Comment, comment))
	}
	return nil
}

// filler returns n bytes of base64-alphabet text, like a sealed record body.
func filler(n int) string {
	const alphabet = "ABCDEFGHIJKLMNOPQRSTUVWXYZabcdefghijklmnopqrstuvwxyz0123456789+/"
	var b strings.Builder
	b.Grow(n)
	for i := range n {
		b.WriteByte(alphabet[i%len(alphabet)])
	}
	return b.String()
}

func firstDiff(a, b string) int {
	n := min(len(a), len(b))
	for i := range n {
		if a[i] != b[i] {
			return i
		}
	}
	return n
}
