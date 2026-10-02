package httpapi

import (
	"net/http"
	"net/http/httptest"
	"net/netip"
	"testing"

	"github.com/ansonarose/seeyouthere-backend/internal/config"
)

func TestNormalizeEmail(t *testing.T) {
	cases := []struct {
		in   string
		want string
		ok   bool
	}{
		{"Priya@Example.com", "priya@example.com", true},
		{"  priya@example.com ", "priya@example.com", true},
		{"priya@localhost", "", false},
		{"Priya <priya@example.com>", "", false},
		{"not-an-email", "", false},
		{"", "", false},
	}
	for _, c := range cases {
		got, ok := normalizeEmail(c.in)
		if got != c.want || ok != c.ok {
			t.Errorf("normalizeEmail(%q) = %q, %v; want %q, %v", c.in, got, ok, c.want, c.ok)
		}
	}
}

func TestValidCode(t *testing.T) {
	for code, want := range map[string]bool{"123456": true, "000000": true, "12345": false, "12345a": false, "1234567": false} {
		if got := validCode(code); got != want {
			t.Errorf("validCode(%q) = %v, want %v", code, got, want)
		}
	}
}

func TestClientIP(t *testing.T) {
	cases := []struct {
		name       string
		trustProxy bool
		xff        string
		want       string
	}{
		{"direct ignores header", false, "1.2.3.4", "10.0.0.1"},
		{"proxy uses rightmost entry", true, "6.6.6.6, 1.2.3.4", "1.2.3.4"},
		{"proxy with bad header falls back", true, "garbage", "10.0.0.1"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			s := &Server{cfg: config.Config{TrustProxy: c.trustProxy}}
			var got netip.Addr
			h := s.clientIP(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				got = clientIPFrom(r.Context())
			}))
			req := httptest.NewRequest(http.MethodGet, "/", nil)
			req.RemoteAddr = "10.0.0.1:5555"
			req.Header.Set("X-Forwarded-For", c.xff)
			h.ServeHTTP(httptest.NewRecorder(), req)
			if got.String() != c.want {
				t.Errorf("got %s, want %s", got, c.want)
			}
		})
	}
}

func TestIPRateKey(t *testing.T) {
	cases := []struct {
		name string
		in   string
		want string
	}{
		{"ipv4 unchanged", "203.0.113.42", "203.0.113.42"},
		{"ipv4-in-ipv6 unchanged", "::ffff:203.0.113.42", "203.0.113.42"},
		{"ipv6 collapses to /64", "2001:db8:1234:5678:aaaa:bbbb:cccc:dddd", "2001:db8:1234:5678::"},
		{"ipv6 same /64 collapses identically", "2001:db8:1234:5678::1", "2001:db8:1234:5678::"},
		{"invalid addr", "", "invalid IP"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			var addr netip.Addr
			if c.in != "" {
				var err error
				addr, err = netip.ParseAddr(c.in)
				if err != nil {
					t.Fatalf("ParseAddr(%q): %v", c.in, err)
				}
			}
			if got := ipRateKey(addr); got != c.want {
				t.Errorf("ipRateKey(%q) = %q, want %q", c.in, got, c.want)
			}
		})
	}

	// Two addresses in the same /64 but different host bits must collapse
	// to the same rate-limit key.
	a := ipRateKey(netip.MustParseAddr("2001:db8::1"))
	b := ipRateKey(netip.MustParseAddr("2001:db8::ffff:ffff:ffff:ffff"))
	if a != b {
		t.Errorf("expected same /64 to collapse to one key, got %q and %q", a, b)
	}
}

func TestCORSAndCSRF(t *testing.T) {
	s := &Server{cfg: config.Config{AppOrigins: []string{"http://localhost:3100"}}}
	ok := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { w.WriteHeader(http.StatusOK) })
	h := s.cors(s.csrf(ok))

	t.Run("preflight from app origin", func(t *testing.T) {
		req := httptest.NewRequest(http.MethodOptions, "/v1/auth/code", nil)
		req.Header.Set("Origin", "http://localhost:3100")
		req.Header.Set("Access-Control-Request-Method", "POST")
		rec := httptest.NewRecorder()
		h.ServeHTTP(rec, req)
		if rec.Code != http.StatusNoContent || rec.Header().Get("Access-Control-Allow-Credentials") != "true" {
			t.Fatalf("status %d, headers %v", rec.Code, rec.Header())
		}
	})

	t.Run("post from app origin allowed", func(t *testing.T) {
		req := httptest.NewRequest(http.MethodPost, "http://localhost:8080/v1/auth/code", nil)
		req.Header.Set("Origin", "http://localhost:3100")
		req.Header.Set("Sec-Fetch-Site", "same-site")
		rec := httptest.NewRecorder()
		h.ServeHTTP(rec, req)
		if rec.Code != http.StatusOK {
			t.Fatalf("status %d", rec.Code)
		}
	})

	t.Run("post from other site blocked", func(t *testing.T) {
		req := httptest.NewRequest(http.MethodPost, "http://localhost:8080/v1/auth/logout", nil)
		req.Header.Set("Origin", "https://evil.example")
		req.Header.Set("Sec-Fetch-Site", "cross-site")
		rec := httptest.NewRecorder()
		h.ServeHTTP(rec, req)
		if rec.Code != http.StatusForbidden {
			t.Fatalf("status %d", rec.Code)
		}
		if rec.Header().Get("Access-Control-Allow-Origin") != "" {
			t.Fatal("unexpected CORS header for untrusted origin")
		}
	})

	t.Run("server-side call without origin allowed", func(t *testing.T) {
		req := httptest.NewRequest(http.MethodPost, "http://localhost:8080/v1/auth/logout", nil)
		rec := httptest.NewRecorder()
		h.ServeHTTP(rec, req)
		if rec.Code != http.StatusOK {
			t.Fatalf("status %d", rec.Code)
		}
	})
}
