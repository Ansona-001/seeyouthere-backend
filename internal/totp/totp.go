// Package totp implements TOTP (RFC 6238) two-factor codes for admin
// step-up auth, and seals/opens the per-user secret with AES-256-GCM so it
// is never stored in the clear. Everything here is stdlib: crypto/hmac,
// crypto/sha1 (mandated by the TOTP spec for compatibility with every
// authenticator app), crypto/aes, crypto/cipher.
package totp

import (
	"crypto/aes"
	"crypto/cipher"
	"crypto/hmac"
	"crypto/rand"
	"crypto/sha1"
	"crypto/subtle"
	"encoding/base32"
	"encoding/base64"
	"encoding/binary"
	"errors"
	"fmt"
	"net/url"
	"strings"
	"time"

	"github.com/google/uuid"
)

// secretLen is the recommended TOTP secret size: 160 bits, matching the
// SHA-1 block/output size used by the standard.
const secretLen = 20

// period is the standard 30-second TOTP step.
const period = 30

// NewSecret returns a fresh random TOTP secret.
func NewSecret() ([]byte, error) {
	secret := make([]byte, secretLen)
	if _, err := rand.Read(secret); err != nil {
		return nil, fmt.Errorf("totp: generate secret: %w", err)
	}
	return secret, nil
}

// hotp computes the 6-digit HOTP value (RFC 4226) for secret at counter.
func hotp(secret []byte, counter uint64) string {
	var buf [8]byte
	binary.BigEndian.PutUint64(buf[:], counter)
	mac := hmac.New(sha1.New, secret)
	mac.Write(buf[:])
	sum := mac.Sum(nil)

	offset := sum[len(sum)-1] & 0x0f
	code := (uint32(sum[offset]&0x7f) << 24) |
		(uint32(sum[offset+1]) << 16) |
		(uint32(sum[offset+2]) << 8) |
		uint32(sum[offset+3])
	return fmt.Sprintf("%06d", code%1_000_000)
}

// Verify checks code against secret within a +-1 step window of now (RFC
// 6238's recommended tolerance for clock skew), and enforces that the
// matched step is strictly after lastStep so a captured code can't be
// replayed. It returns the matched step so the caller can persist it as the
// new lastStep.
func Verify(secret []byte, code string, now time.Time, lastStep int64) (step int64, ok bool) {
	code = strings.TrimSpace(code)
	if len(code) != 6 {
		return 0, false
	}
	current := now.Unix() / period
	for _, s := range [3]int64{current - 1, current, current + 1} {
		if s <= lastStep {
			continue
		}
		want := hotp(secret, uint64(s))
		if subtle.ConstantTimeCompare([]byte(want), []byte(code)) == 1 {
			return s, true
		}
	}
	return 0, false
}

// URI builds the otpauth:// URI an authenticator app scans/imports to enrol
// account under issuer.
func URI(issuer, account string, secret []byte) string {
	v := url.Values{}
	v.Set("secret", base32.StdEncoding.WithPadding(base32.NoPadding).EncodeToString(secret))
	v.Set("issuer", issuer)
	v.Set("algorithm", "SHA1")
	v.Set("digits", "6")
	v.Set("period", fmt.Sprint(period))
	u := url.URL{
		Scheme:   "otpauth",
		Host:     "totp",
		Path:     "/" + issuer + ":" + account,
		RawQuery: v.Encode(),
	}
	return u.String()
}

// sealVersion is prefixed to every sealed secret so a future key-rotation
// or cipher change can coexist with old rows during a migration.
const sealVersion = "v1:"

// Sealer encrypts/decrypts TOTP secrets at rest with AES-256-GCM, binding
// each ciphertext to the owning user id as additional authenticated data so
// a sealed secret can't be copied onto a different user's row.
type Sealer struct {
	aead cipher.AEAD
}

// NewSealer builds a Sealer from a 32-byte key (TOTP_KEY).
func NewSealer(key []byte) (*Sealer, error) {
	if len(key) != 32 {
		return nil, errors.New("totp: sealing key must be 32 bytes")
	}
	block, err := aes.NewCipher(key)
	if err != nil {
		return nil, fmt.Errorf("totp: new cipher: %w", err)
	}
	aead, err := cipher.NewGCM(block)
	if err != nil {
		return nil, fmt.Errorf("totp: new gcm: %w", err)
	}
	return &Sealer{aead: aead}, nil
}

// Seal encrypts secret for userID.
func (s *Sealer) Seal(userID uuid.UUID, secret []byte) (string, error) {
	nonce := make([]byte, s.aead.NonceSize())
	if _, err := rand.Read(nonce); err != nil {
		return "", fmt.Errorf("totp: generate nonce: %w", err)
	}
	ct := s.aead.Seal(nonce, nonce, secret, userID[:])
	return sealVersion + base64.RawURLEncoding.EncodeToString(ct), nil
}

// Open decrypts a value produced by Seal for the same userID. It fails
// (rather than returning garbage) if the ciphertext was sealed for a
// different user or has been tampered with.
func (s *Sealer) Open(userID uuid.UUID, sealed string) ([]byte, error) {
	rest, ok := strings.CutPrefix(sealed, sealVersion)
	if !ok {
		return nil, errors.New("totp: unsupported seal version")
	}
	raw, err := base64.RawURLEncoding.DecodeString(rest)
	if err != nil {
		return nil, errors.New("totp: malformed sealed secret")
	}
	ns := s.aead.NonceSize()
	if len(raw) < ns {
		return nil, errors.New("totp: malformed sealed secret")
	}
	nonce, ct := raw[:ns], raw[ns:]
	secret, err := s.aead.Open(nil, nonce, ct, userID[:])
	if err != nil {
		return nil, errors.New("totp: cannot open sealed secret")
	}
	return secret, nil
}
