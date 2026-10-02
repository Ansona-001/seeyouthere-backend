package totp

import (
	"net/url"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"
)

// rfc6238Secret is the ASCII test secret from RFC 6238 Appendix B.
var rfc6238Secret = []byte("12345678901234567890")

func TestHOTP_RFC6238Vectors(t *testing.T) {
	// Expected values are the last 6 digits of RFC 6238's 8-digit test
	// vectors (mod 10^6 of the same dynamic-truncation value).
	cases := []struct {
		unixTime int64
		want     string
	}{
		{59, "287082"},
		{1111111109, "081804"},
		{1111111111, "050471"},
		{1234567890, "005924"},
		{2000000000, "279037"},
	}
	for _, c := range cases {
		counter := uint64(c.unixTime / period)
		if got := hotp(rfc6238Secret, counter); got != c.want {
			t.Errorf("hotp(t=%d) = %s, want %s", c.unixTime, got, c.want)
		}
	}
}

func TestVerify_RFC6238Vectors(t *testing.T) {
	for _, c := range []struct {
		unixTime int64
		want     string
	}{
		{59, "287082"},
		{1111111111, "050471"},
	} {
		now := time.Unix(c.unixTime, 0).UTC()
		step, ok := Verify(rfc6238Secret, c.want, now, 0)
		if !ok {
			t.Errorf("Verify(t=%d) failed, want success", c.unixTime)
		}
		if want := c.unixTime / period; step != want {
			t.Errorf("Verify(t=%d) step = %d, want %d", c.unixTime, step, want)
		}
	}
}

func TestVerify_WindowAndReplay(t *testing.T) {
	secret := []byte("a-test-secret-value!")
	now := time.Unix(1_700_000_000, 0).UTC()
	currentStep := now.Unix() / period
	code := hotp(secret, uint64(currentStep))

	step, ok := Verify(secret, code, now, 0)
	if !ok || step != currentStep {
		t.Fatalf("Verify(current) = %d, %v; want %d, true", step, ok, currentStep)
	}

	// The same code must not verify twice (replay): lastStep is now current.
	if _, ok := Verify(secret, code, now, step); ok {
		t.Error("replaying the same code should be rejected")
	}

	// +-1 step window: a code from one period earlier or later still
	// verifies against the same "now", tolerating clock skew.
	prevCode := hotp(secret, uint64(currentStep-1))
	if _, ok := Verify(secret, prevCode, now, 0); !ok {
		t.Error("a code from the previous step should verify within the window")
	}
	nextCode := hotp(secret, uint64(currentStep+1))
	if _, ok := Verify(secret, nextCode, now, 0); !ok {
		t.Error("a code from the next step should verify within the window")
	}

	// Outside the +-1 window it must fail.
	farCode := hotp(secret, uint64(currentStep-5))
	if _, ok := Verify(secret, farCode, now, 0); ok {
		t.Error("a code 5 steps away should be outside the +-1 window")
	}
}

func TestVerify_WrongLength(t *testing.T) {
	if _, ok := Verify(rfc6238Secret, "12345", time.Now(), 0); ok {
		t.Error("a 5-digit code should never verify")
	}
	if _, ok := Verify(rfc6238Secret, "1234567", time.Now(), 0); ok {
		t.Error("a 7-digit code should never verify")
	}
}

func TestURI(t *testing.T) {
	secret := []byte("12345678901234567890")
	raw := URI("See You There", "user@example.com", secret)
	u, err := url.Parse(raw)
	if err != nil {
		t.Fatalf("URI produced an unparseable value: %v", err)
	}
	if u.Scheme != "otpauth" || u.Host != "totp" {
		t.Errorf("scheme/host = %s://%s", u.Scheme, u.Host)
	}
	if !strings.Contains(u.Path, "See You There") || !strings.Contains(u.Path, "user@example.com") {
		t.Errorf("path missing label: %s", u.Path)
	}
	q := u.Query()
	if q.Get("issuer") != "See You There" || q.Get("algorithm") != "SHA1" || q.Get("digits") != "6" || q.Get("period") != "30" {
		t.Errorf("unexpected query: %v", q)
	}
	if q.Get("secret") == "" {
		t.Error("expected a non-empty base32 secret")
	}
}

func TestSealer_RoundTrip(t *testing.T) {
	key := make([]byte, 32)
	for i := range key {
		key[i] = byte(i)
	}
	sealer, err := NewSealer(key)
	if err != nil {
		t.Fatalf("NewSealer: %v", err)
	}
	userID := uuid.Must(uuid.NewV7())
	secret := []byte("plaintext-totp-secret")

	sealed, err := sealer.Seal(userID, secret)
	if err != nil {
		t.Fatalf("Seal: %v", err)
	}
	opened, err := sealer.Open(userID, sealed)
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	if string(opened) != string(secret) {
		t.Errorf("Open = %q, want %q", opened, secret)
	}
}

func TestSealer_WrongUserFails(t *testing.T) {
	key := make([]byte, 32)
	sealer, err := NewSealer(key)
	if err != nil {
		t.Fatalf("NewSealer: %v", err)
	}
	userA, userB := uuid.Must(uuid.NewV7()), uuid.Must(uuid.NewV7())
	sealed, err := sealer.Seal(userA, []byte("secret"))
	if err != nil {
		t.Fatalf("Seal: %v", err)
	}
	if _, err := sealer.Open(userB, sealed); err == nil {
		t.Error("Open with the wrong user id should fail")
	}
}

func TestSealer_TamperedCiphertextFails(t *testing.T) {
	key := make([]byte, 32)
	sealer, err := NewSealer(key)
	if err != nil {
		t.Fatalf("NewSealer: %v", err)
	}
	userID := uuid.Must(uuid.NewV7())
	sealed, err := sealer.Seal(userID, []byte("secret"))
	if err != nil {
		t.Fatalf("Seal: %v", err)
	}
	tampered := sealed[:len(sealed)-2] + "AA"
	if _, err := sealer.Open(userID, tampered); err == nil {
		t.Error("Open with a tampered ciphertext should fail")
	}
}

func TestNewSealer_RequiresKeyLength(t *testing.T) {
	if _, err := NewSealer(make([]byte, 16)); err == nil {
		t.Error("expected an error for a 16-byte key")
	}
}

func TestNewSecret_Length(t *testing.T) {
	secret, err := NewSecret()
	if err != nil {
		t.Fatalf("NewSecret: %v", err)
	}
	if len(secret) != secretLen {
		t.Errorf("len(secret) = %d, want %d", len(secret), secretLen)
	}
}
