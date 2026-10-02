package content

import "testing"

func TestValidateHTTPSURL(t *testing.T) {
	cases := []struct {
		name string
		in   string
		ok   bool
	}{
		{"https ok", "https://example.com/path", true},
		{"http rejected", "http://example.com", false},
		{"javascript scheme rejected", "javascript:alert(1)", false},
		{"data scheme rejected", "data:text/html,<script>", false},
		{"userinfo rejected", "https://user:pass@example.com", false},
		{"ipv4 host rejected", "https://1.2.3.4/", false},
		{"ipv6 host rejected", "https://[::1]/", false},
		{"default port ok", "https://example.com", true},
		{"port 443 ok", "https://example.com:443", true},
		{"port 8443 rejected", "https://example.com:8443", false},
		{"empty rejected", "", false},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			_, ok := validateHTTPSURL(c.in, 2048)
			if ok != c.ok {
				t.Errorf("validateHTTPSURL(%q) ok = %v, want %v", c.in, ok, c.ok)
			}
		})
	}
}

func TestValidateHTTPSURL_MaxLength(t *testing.T) {
	long := "https://example.com/" + repeat("a", 2048)
	if _, ok := validateHTTPSURL(long, 2048); ok {
		t.Error("URL over the max length should be rejected")
	}
}

func TestValidateMapURL(t *testing.T) {
	cases := []struct {
		name string
		in   string
		ok   bool
	}{
		{"maps.google.com", "https://maps.google.com/?q=x", true},
		{"maps.app.goo.gl", "https://maps.app.goo.gl/abc", true},
		{"goo.gl with /maps path", "https://goo.gl/maps/abc", true},
		{"goo.gl without /maps path", "https://goo.gl/abc", false},
		{"maps.apple.com", "https://maps.apple.com/?q=x", true},
		{"unrelated host", "https://evil.example/maps", false},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			_, ok := validateMapURL(c.in, 2048)
			if ok != c.ok {
				t.Errorf("validateMapURL(%q) ok = %v, want %v", c.in, ok, c.ok)
			}
		})
	}
}
