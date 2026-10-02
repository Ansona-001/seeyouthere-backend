package content

import (
	"net"
	"net/url"
	"strings"
	"unicode/utf8"
)

// validateHTTPSURL enforces the shared https-only rule for links.url and
// location.map_url: scheme exactly "https", non-empty host, no userinfo, no
// IP-literal host, and port absent or 443. It returns the canonical string
// form (u.String()) to store.
func validateHTTPSURL(raw string, maxRunes int) (string, bool) {
	if raw == "" || utf8.RuneCountInString(raw) > maxRunes {
		return "", false
	}
	u, err := url.Parse(raw)
	if err != nil || u.Scheme != "https" || u.Host == "" || u.User != nil || u.Opaque != "" {
		return "", false
	}
	host := u.Hostname()
	if host == "" || net.ParseIP(host) != nil {
		return "", false
	}
	if port := u.Port(); port != "" && port != "443" {
		return "", false
	}
	return u.String(), true
}

// mapHosts lists the map providers accepted for location.map_url, with a
// predicate on the path where the host alone isn't specific enough.
var mapHosts = map[string]func(path string) bool{
	"maps.google.com":       func(string) bool { return true },
	"www.google.com":        func(p string) bool { return strings.HasPrefix(p, "/maps") },
	"maps.app.goo.gl":       func(string) bool { return true },
	"goo.gl":                func(p string) bool { return strings.HasPrefix(p, "/maps") },
	"maps.apple.com":        func(string) bool { return true },
	"www.openstreetmap.org": func(string) bool { return true },
}

// validateMapURL applies validateHTTPSURL and then restricts the host to a
// known map provider.
func validateMapURL(raw string, maxRunes int) (string, bool) {
	canon, ok := validateHTTPSURL(raw, maxRunes)
	if !ok {
		return "", false
	}
	u, err := url.Parse(canon)
	if err != nil {
		return "", false
	}
	check, known := mapHosts[u.Hostname()]
	if !known || !check(u.Path) {
		return "", false
	}
	return canon, true
}
