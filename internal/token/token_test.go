package token

import (
	"net/netip"
	"testing"
	"time"

	"github.com/google/uuid"
)

func testKeys(t *testing.T) *Keys {
	t.Helper()
	secret := make([]byte, 32)
	for i := range secret {
		secret[i] = byte(i + 1)
	}
	k, err := NewKeys(secret)
	if err != nil {
		t.Fatalf("NewKeys: %v", err)
	}
	return k
}

func TestNewKeys_RequiresLength(t *testing.T) {
	if _, err := NewKeys(make([]byte, 31)); err == nil {
		t.Fatal("expected error for a 31-byte secret")
	}
	if _, err := NewKeys(make([]byte, 32)); err != nil {
		t.Fatalf("32-byte secret should be accepted: %v", err)
	}
}

func TestGuestToken_RoundTrip(t *testing.T) {
	k := testKeys(t)
	id := uuid.Must(uuid.NewV7())
	tok := k.GuestToken(id, 1)

	gotID, mac, err := Split(tok)
	if err != nil {
		t.Fatalf("Split: %v", err)
	}
	if gotID != id {
		t.Errorf("Split id = %v, want %v", gotID, id)
	}
	if !k.CheckGuest(gotID, 1, mac) {
		t.Error("CheckGuest should accept its own token")
	}
}

func TestGuestToken_TamperEachByte(t *testing.T) {
	k := testKeys(t)
	id := uuid.Must(uuid.NewV7())
	tok := k.GuestToken(id, 1)
	_, mac, err := Split(tok)
	if err != nil {
		t.Fatalf("Split: %v", err)
	}
	for i := range mac {
		tampered := append([]byte{}, mac...)
		tampered[i] ^= 0xFF
		if k.CheckGuest(id, 1, tampered) {
			t.Errorf("tampering byte %d of the MAC should be rejected", i)
		}
	}
}

func TestGuestToken_WrongVersionRejected(t *testing.T) {
	k := testKeys(t)
	id := uuid.Must(uuid.NewV7())
	_, mac, _ := Split(k.GuestToken(id, 1))
	if k.CheckGuest(id, 2, mac) {
		t.Error("a token for version 1 should not validate against version 2")
	}
}

func TestGuestToken_WrongIDRejected(t *testing.T) {
	k := testKeys(t)
	id := uuid.Must(uuid.NewV7())
	other := uuid.Must(uuid.NewV7())
	_, mac, _ := Split(k.GuestToken(id, 1))
	if k.CheckGuest(other, 1, mac) {
		t.Error("a token for one guest should not validate for another")
	}
}

func TestRSVPToken_RoundTrip(t *testing.T) {
	k := testKeys(t)
	id := uuid.Must(uuid.NewV7())
	tok := k.RSVPToken(id, 1)
	gotID, mac, err := Split(tok)
	if err != nil {
		t.Fatalf("Split: %v", err)
	}
	if !k.CheckRSVP(gotID, 1, mac) {
		t.Error("CheckRSVP should accept its own token")
	}
}

func TestRSVPToken_WrongVersionRejected(t *testing.T) {
	k := testKeys(t)
	id := uuid.Must(uuid.NewV7())
	_, mac, _ := Split(k.RSVPToken(id, 1))
	if k.CheckRSVP(id, 2, mac) {
		t.Error("a token for version 1 should not validate against version 2 (RotateRSVPEditToken)")
	}
}

func TestCrossPurposeTokenRejected(t *testing.T) {
	k := testKeys(t)
	id := uuid.Must(uuid.NewV7())
	guestTok := k.GuestToken(id, 1)
	gotID, mac, err := Split(guestTok)
	if err != nil {
		t.Fatalf("Split: %v", err)
	}
	if k.CheckRSVP(gotID, 1, mac) {
		t.Error("a guest token should not validate as an RSVP token")
	}
}

func TestSplit_Malformed(t *testing.T) {
	cases := []string{"", "not-base64url!!!", "AAAA", string(make([]byte, 100))}
	for _, c := range cases {
		if _, _, err := Split(c); err != ErrMalformed {
			t.Errorf("Split(%q) err = %v, want ErrMalformed", c, err)
		}
	}
}

// TestSplit_NonCanonicalBase64Rejected covers the fix requiring
// base64.RawURLEncoding.Strict(): a token whose last symbol's would-be-zero
// padding bits are nonzero decodes to the same 32 bytes under lenient
// decoding, so two differently-spelled strings would otherwise both be
// accepted as the same token.
func TestSplit_NonCanonicalBase64Rejected(t *testing.T) {
	k := testKeys(t)
	id := uuid.Must(uuid.NewV7())
	tok := k.GuestToken(id, 1)

	b := []byte(tok)
	last := b[len(b)-1]
	alt := byte('B') // base64url value 1: binary 000001, nonzero low 2 bits
	if last == alt {
		alt = 'C' // value 2: binary 000010, also nonzero low 2 bits
	}
	b[len(b)-1] = alt
	tampered := string(b)

	if tampered == tok {
		t.Fatal("test setup: tampered token equals original")
	}
	if _, _, err := Split(tampered); err != ErrMalformed {
		t.Errorf("Split(non-canonical) err = %v, want ErrMalformed", err)
	}
}

// TestSplit_WrongLengthRejected covers the exact encoded-length check added
// alongside Strict decoding.
func TestSplit_WrongLengthRejected(t *testing.T) {
	k := testKeys(t)
	tok := k.GuestToken(uuid.Must(uuid.NewV7()), 1)
	cases := []string{tok[:len(tok)-1], tok + "A", tok[:len(tok)-1] + "AA"}
	for _, c := range cases {
		if _, _, err := Split(c); err != ErrMalformed {
			t.Errorf("Split(%q) err = %v, want ErrMalformed", c, err)
		}
	}
}

func TestAccessCookie_RoundTrip(t *testing.T) {
	k := testKeys(t)
	eventID := uuid.Must(uuid.NewV7())
	hash := "$argon2id$v=19$m=19456,t=2,p=1$salt$hash"
	exp := time.Now().Add(30 * 24 * time.Hour)
	cookie := k.AccessCookie(eventID, hash, exp)

	if !k.CheckAccessCookie(cookie, eventID, hash, time.Now()) {
		t.Error("a freshly issued cookie should validate")
	}
}

func TestAccessCookie_Expired(t *testing.T) {
	k := testKeys(t)
	eventID := uuid.Must(uuid.NewV7())
	hash := "hash"
	exp := time.Now().Add(-time.Second)
	cookie := k.AccessCookie(eventID, hash, exp)
	if k.CheckAccessCookie(cookie, eventID, hash, time.Now()) {
		t.Error("an expired cookie should not validate")
	}
}

func TestAccessCookie_PasswordFingerprintChange(t *testing.T) {
	k := testKeys(t)
	eventID := uuid.Must(uuid.NewV7())
	exp := time.Now().Add(time.Hour)
	cookie := k.AccessCookie(eventID, "old-hash", exp)
	if k.CheckAccessCookie(cookie, eventID, "new-hash", time.Now()) {
		t.Error("changing the password hash should invalidate existing cookies")
	}
}

func TestAccessCookie_WrongEventRejected(t *testing.T) {
	k := testKeys(t)
	hash := "hash"
	exp := time.Now().Add(time.Hour)
	cookie := k.AccessCookie(uuid.Must(uuid.NewV7()), hash, exp)
	if k.CheckAccessCookie(cookie, uuid.Must(uuid.NewV7()), hash, time.Now()) {
		t.Error("a cookie for one event should not validate for another")
	}
}

func TestHashIP_DeterministicAndKeyed(t *testing.T) {
	k1 := testKeys(t)
	secret2 := make([]byte, 32)
	for i := range secret2 {
		secret2[i] = byte(255 - i)
	}
	k2, err := NewKeys(secret2)
	if err != nil {
		t.Fatalf("NewKeys: %v", err)
	}

	ip := netip.MustParseAddr("203.0.113.42")
	h1a := k1.HashIP(ip)
	h1b := k1.HashIP(ip)
	if string(h1a) != string(h1b) {
		t.Error("HashIP should be deterministic for the same key and address")
	}
	h2 := k2.HashIP(ip)
	if string(h1a) == string(h2) {
		t.Error("HashIP should differ across keys (it isn't plain unkeyed SHA-256)")
	}

	mapped := netip.MustParseAddr("::ffff:203.0.113.42")
	if string(k1.HashIP(mapped)) != string(h1a) {
		t.Error("an IPv4-mapped IPv6 address should hash the same as its IPv4 form")
	}
}
