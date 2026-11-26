package proxmox

import (
	"errors"
	"fmt"
	"net/http"
	"regexp"
	"strings"

	gp "github.com/luthermonson/go-proxmox"
)

// Sentinel errors for conditions the provider acts on. Helpers that recognise
// one return an error that matches it with errors.Is and still unwraps to the
// library's *gp.APIError with errors.As.
var (
	// errVMExists is returned by cloneVM when the target VMID is taken.
	errVMExists = errors.New("vm already exists")
	// errVMNotFound is returned by VM helpers when the VM does not exist on
	// the given node.
	errVMNotFound = errors.New("vm not found")
	// errAgentNotRunning is returned by guest agent calls when the VM or the
	// QEMU guest agent inside it is not running. It is usually transient
	// while a guest boots.
	errAgentNotRunning = errors.New("qemu guest agent is not running")
	// errAgentNotConfigured is returned by guest agent calls when the VM
	// config does not enable the guest agent.
	errAgentNotConfigured = errors.New("qemu guest agent is not configured")
	// errPoolNotFound is returned by pool lookups when the pool does not
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

func isAlreadyExists(e *gp.APIError) bool {
	return apiErrorContains(e, "already exists")
}

// isVMMissing matches only the guest's own config being absent, e.g.
// "Configuration file 'nodes/pve1/qemu-server/100.conf' does not exist", so a
// missing storage or pool named in the same request is not misreported.
func isVMMissing(e *gp.APIError) bool {
	return e.StatusCode == http.StatusNotFound ||
		(apiErrorContains(e, "configuration file") && apiErrorContains(e, "does not exist")) ||
		apiErrorContains(e, "unable to find configuration file")
}

func classifyVM(err error) error {
	return classify(err, errVMNotFound, isVMMissing)
}

// classifyAgent maps guest agent failures.
func classifyAgent(err error) error {
	apiErr, ok := gp.AsAPIError(err)
	if err == nil || !ok {
		return err
	}
	switch {
	case apiErrorContains(apiErr, "no qemu guest agent configured"), apiErrorContains(apiErr, "agent not configured"):
		return fmt.Errorf("%w: %w", errAgentNotConfigured, err)
	case apiErrorContains(apiErr, "is not running"):
		// Covers both "QEMU guest agent is not running" and
		// "VM 100 is not running".
		return fmt.Errorf("%w: %w", errAgentNotRunning, err)
	}
	return classifyVM(err)
}

// classifyPool maps a failed pool lookup. The library classifies Proxmox's
// many "does not exist" phrasings as gp.ErrNotFound (Proxmox rarely uses 404)
// and reports an empty result set with its own "pool not found" error.
func classifyPool(err error) error {
	if err == nil {
		return nil
	}
	if _, isAPI := gp.AsAPIError(err); !isAPI && strings.Contains(err.Error(), "pool not found") {
		return fmt.Errorf("%w: %w", errPoolNotFound, err)
	}
	return classify(err, errPoolNotFound, func(e *gp.APIError) bool {
		return e.StatusCode == http.StatusNotFound || errors.Is(e, gp.ErrNotFound)
	})
}
