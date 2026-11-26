package proxmox

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"net/url"
	"regexp"
	"strings"

	gp "github.com/luthermonson/go-proxmox"
)

// Sentinel errors for the pool conditions the store acts on. The helpers
// below return an error that matches one of these with errors.Is and still
// unwraps to the library's *gp.APIError with errors.As.
var (
	// errPoolExists is returned by createPool when the pool already exists.
	errPoolExists = errors.New("pool already exists")
	// errPoolNotFound is returned by pool helpers when the pool does not
	// exist.
	errPoolNotFound = errors.New("pool not found")
)

// poolIDPattern matches the pve-poolid format: letters, digits, '.', '-' and
// '_'. Nested pool IDs (containing '/') are deliberately not accepted.
var poolIDPattern = regexp.MustCompile(`^[A-Za-z0-9._-]+$`)

// validatePoolID reports whether id is a valid Proxmox pool ID.
func validatePoolID(id string) error {
	if !poolIDPattern.MatchString(id) {
		return fmt.Errorf("proxmox: invalid pool ID %q: only letters, digits, '.', '-' and '_' are allowed", id)
	}
	return nil
}

// apiErrorContains reports whether sub occurs, case-insensitively, in the
// status line, the message, or the per-parameter errors of an API error.
func apiErrorContains(e *gp.APIError, sub string) bool {
	sub = strings.ToLower(sub)
	if strings.Contains(strings.ToLower(e.Status), sub) || strings.Contains(strings.ToLower(e.Message), sub) {
		return true
	}
	for _, v := range e.Errors {
		if strings.Contains(strings.ToLower(v), sub) {
			return true
		}
	}
	return false
}

// classify wraps err with kind when it is an API error for which match
// returns true. Errors that did not come from the API (transport failures,
// context cancellation, decoding errors) are returned unchanged.
func classify(err error, kind error, match func(*gp.APIError) bool) error {
	if err == nil {
		return nil
	}
	apiErr, ok := gp.AsAPIError(err)
	if !ok || !match(apiErr) {
		return err
	}
	return fmt.Errorf("%w: %w", kind, err)
}

// isNotFound defers to the library's not-found classification, which knows
// Proxmox's many "does not exist" phrasings (Proxmox rarely uses 404).
func isNotFound(e *gp.APIError) bool {
	return e.StatusCode == http.StatusNotFound || errors.Is(e, gp.ErrNotFound)
}

func classifyNotFound(err error) error {
	if err == nil {
		return nil
	}
	if _, isAPI := gp.AsAPIError(err); !isAPI && strings.Contains(err.Error(), "pool not found") {
		// The library's own error for an empty result set.
		return fmt.Errorf("%w: %w", errPoolNotFound, err)
	}
	return classify(err, errPoolNotFound, isNotFound)
}

// createPool creates a pool with comment. It returns an error matching
// errPoolExists if a pool with that ID already exists.
func createPool(ctx context.Context, c *gp.Client, id, comment string) error {
	if err := validatePoolID(id); err != nil {
		return err
	}
	return classify(c.NewPool(ctx, id, comment), errPoolExists, func(e *gp.APIError) bool {
		return apiErrorContains(e, "already exists")
	})
}

// getPool fetches a pool with its members, which is also the handle every
// mutating pool call needs because the library only builds handles from a
// read. It returns an error matching errPoolNotFound if the pool does not
// exist.
func getPool(ctx context.Context, c *gp.Client, id string) (*gp.Pool, error) {
	if err := validatePoolID(id); err != nil {
		return nil, err
	}
	p, err := c.Pool(ctx, id)
	if err != nil {
		return nil, classifyNotFound(err)
	}
	return p, nil
}

// setComment replaces a pool's comment. It returns an error matching
// errPoolNotFound if the pool is gone.
func setComment(ctx context.Context, c *gp.Client, id, comment string) error {
	p, err := getPool(ctx, c, id)
	if err != nil {
		return err
	}
	return classifyNotFound(p.Update(ctx, &gp.PoolUpdateOption{Comment: comment}))
}

// deletePool deletes a pool by ID without reading it first, so a pool can
// still be removed when reads fail. Proxmox only deletes empty pools. The
// request is the one the library's Pool.Delete sends.
func deletePool(ctx context.Context, c *gp.Client, id string) error {
	if err := validatePoolID(id); err != nil {
		return err
	}
	u := url.URL{Path: "/pools", RawQuery: url.Values{"poolid": {id}}.Encode()}
	return classifyNotFound(c.Delete(ctx, u.String(), nil))
}
