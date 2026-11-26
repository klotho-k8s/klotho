package proxmox

import "strings"

// parseTags splits a Proxmox tag string into individual tags. Proxmox stores
// tags separated by ';' but also accepts ',' and whitespace on input, so all
// three are treated as separators. Empty entries and case-insensitive
// duplicates are dropped and the original order is preserved.
func parseTags(s string) []string {
	fields := strings.FieldsFunc(s, func(r rune) bool {
		return r == ';' || r == ',' || r == ' ' || r == '\t' || r == '\n' || r == '\r'
	})
	if len(fields) == 0 {
		return nil
	}
	out := make([]string, 0, len(fields))
	seen := make(map[string]struct{}, len(fields))
	for _, f := range fields {
		key := strings.ToLower(f)
		if _, dup := seen[key]; dup {
			continue
		}
		seen[key] = struct{}{}
		out = append(out, f)
	}
	return out
}

// joinTags renders tags as the ';'-separated string Proxmox uses for the
// "tags" config key. Surrounding whitespace is trimmed and empty entries are
// dropped.
func joinTags(tags []string) string {
	out := make([]string, 0, len(tags))
	for _, t := range tags {
		if t = strings.TrimSpace(t); t != "" {
			out = append(out, t)
		}
	}
	return strings.Join(out, ";")
}

// hasTag reports whether tags contains tag, compared case-insensitively as
// Proxmox does.
func hasTag(tags []string, tag string) bool {
	for _, t := range tags {
		if strings.EqualFold(t, tag) {
			return true
		}
	}
	return false
}
