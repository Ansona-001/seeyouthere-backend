package httpapi

import (
	"encoding/hex"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/ansonarose/seeyouthere-backend/internal/config"
)

func TestSetEventCookie(t *testing.T) {
	s := &Server{cfg: config.Config{CookieDomain: ".seeuthere.at", CookieSecure: true}}
	eventID := uuid.Must(uuid.NewV7())
	rec := httptest.NewRecorder()

	s.setEventCookie(rec, "syt_pw_", eventID, "the-value", 30*24*time.Hour)

	cookies := rec.Result().Cookies()
	if len(cookies) != 1 {
		t.Fatalf("expected 1 cookie, got %d", len(cookies))
	}
	c := cookies[0]
	wantName := "syt_pw_" + hex.EncodeToString(eventID[:])
	if c.Name != wantName {
		t.Errorf("cookie name = %q, want %q", c.Name, wantName)
	}
	// Go's http.Cookie strips a leading "." from Domain when serialising
	// the header (RFC 6265bis treats it as equivalent); re-parsing the
	// Set-Cookie header back therefore yields the dot-less form.
	if c.Value != "the-value" || !c.HttpOnly || !c.Secure || c.Domain != "seeuthere.at" {
		t.Errorf("unexpected cookie: %+v", c)
	}
}

// validToken1/validToken2 are the exact shape randomToken produces: 32
// random bytes, base64url-encoded without padding, 43 characters.
const (
	validToken1 = "AAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAA"
	validToken2 = "BBBBBBBBBBBBBBBBBBBBBBBBBBBBBBBBBBBBBBBBBBB"
)

func TestDraftCookieHash(t *testing.T) {
	req := httptest.NewRequest("GET", "/", nil)
	if _, ok := draftCookieHash(req); ok {
		t.Error("expected ok=false with no cookie set")
	}

	req.AddCookie(&http.Cookie{Name: "syt_draft", Value: validToken1})
	hash, ok := draftCookieHash(req)
	if !ok || len(hash) != 32 {
		t.Fatalf("hash = %x, ok = %v", hash, ok)
	}

	// Same value must hash the same; a different value must hash differently.
	req2 := httptest.NewRequest("GET", "/", nil)
	req2.AddCookie(&http.Cookie{Name: "syt_draft", Value: validToken1})
	hash2, _ := draftCookieHash(req2)
	if string(hash) != string(hash2) {
		t.Error("same cookie value should hash the same")
	}

	req3 := httptest.NewRequest("GET", "/", nil)
	req3.AddCookie(&http.Cookie{Name: "syt_draft", Value: validToken2})
	hash3, _ := draftCookieHash(req3)
	if string(hash) == string(hash3) {
		t.Error("different cookie values should hash differently")
	}
}

func TestIsValidDraftToken(t *testing.T) {
	cases := []struct {
		name string
		in   string
		want bool
	}{
		{"valid token", validToken1, true},
		{"empty", "", false},
		{"too short", "abc123", false},
		{"too long", validToken1 + "A", false},
		{"padding char", validToken1[:42] + "=", false},
		{"standard base64 plus", validToken1[:42] + "+", false},
		{"standard base64 slash", validToken1[:42] + "/", false},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			if got := isValidDraftToken(c.in); got != c.want {
				t.Errorf("isValidDraftToken(%q) = %v, want %v", c.in, got, c.want)
			}
		})
	}
}

func TestDraftCookieHash_RejectsMalformedValues(t *testing.T) {
	// A client-supplied syt_draft that isn't exactly 43 chars of strict
	// base64url is rejected outright, so an attacker can't fix a victim's
	// cookie to a short, guessable, or otherwise malformed value.
	for _, v := range []string{"", "abc123", validToken1 + "A"} {
		req := httptest.NewRequest("GET", "/", nil)
		req.AddCookie(&http.Cookie{Name: "syt_draft", Value: v})
		if _, ok := draftCookieHash(req); ok {
			t.Errorf("draftCookieHash accepted malformed value %q", v)
		}
	}
}
