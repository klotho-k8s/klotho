package vault

import (
	"encoding/json"
	"errors"
	"strings"

	vaultapi "github.com/hashicorp/vault/api"
)

// errCASExhausted is returned by Save when every check-and-set retry lost to a
// concurrent writer.
var errCASExhausted = errors.New("check-and-set conflict persisted after retries")

// isCASConflict reports whether err is a KV v2 check-and-set rejection: Vault
// answers a cas mismatch (including a cas=0 write to an existing key) with a
// 400 whose message mentions check-and-set.
func isCASConflict(err error) bool {
	if err == nil {
		return false
	}
	var re *vaultapi.ResponseError
	if errors.As(err, &re) {
		if re.StatusCode == 400 {
			for _, e := range re.Errors {
				if mentionsCAS(e) {
					return true
				}
			}
		}
	}
	return mentionsCAS(err.Error())
}

// mentionsCAS reports whether a Vault error message is a check-and-set failure.
func mentionsCAS(msg string) bool {
	msg = strings.ToLower(msg)
	return strings.Contains(msg, "check-and-set") ||
		strings.Contains(msg, "did not match the current version")
}

// intFromJSON coerces a value decoded from Vault JSON (typically json.Number)
// into an int, returning 0 for anything unexpected.
func intFromJSON(v any) int {
	switch n := v.(type) {
	case json.Number:
		i, err := n.Int64()
		if err != nil {
			return 0
		}
		return int(i)
	case float64:
		return int(n)
	case int:
		return n
	case int64:
		return int(n)
	default:
		return 0
	}
}
