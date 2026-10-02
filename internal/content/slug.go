package content

import (
	"regexp"
	"strings"
)

// slugRe matches 3-50 lower-case alphanumeric characters and hyphens,
// starting and ending with an alphanumeric character.
var slugRe = regexp.MustCompile(`^[a-z0-9](?:[a-z0-9-]{1,48}[a-z0-9])$`)

// NormalizeSlug trims and lower-cases s and reports whether the result is a
// structurally valid slug. Blocklist and uniqueness checks happen in SQL
// (SlugBlocked, the events_slug_key unique index).
func NormalizeSlug(s string) (string, bool) {
	s = strings.ToLower(strings.TrimSpace(s))
	if !slugRe.MatchString(s) || strings.Contains(s, "--") {
		return "", false
	}
	return s, true
}
