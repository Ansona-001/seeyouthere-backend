// Package token derives guest invite tokens, RSVP-edit tokens and signed
// access cookies from AUTH_SECRET using HKDF-SHA256 subkeys. Nothing here
// is stored in the database: a token is the id it names plus a truncated
// HMAC over that id (and, where noted, a version or expiry), so a database
// leak reveals no usable secret and revocation is just bumping a version
// column server-side (see decision 1 in the build-out plan).
package token

import (
	"crypto/hkdf"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/base64"
	"encoding/binary"
	"errors"
	"fmt"
	"net/netip"
	"time"

	"github.com/google/uuid"
)

// macLen is the truncated HMAC length embedded in guest/RSVP tokens and the
// access cookie: 16 bytes (128 bits) is ample for an unguessable, non-stored MAC.
const macLen = 16

// tokenEncodedLen and accessCookieEncodedLen are the exact base64
// (RawURLEncoding, unpadded) lengths of a guest/RSVP token (16-byte id +
// macLen MAC) and an access cookie (8-byte expiry + macLen MAC). Split and
// CheckAccessCookie both require an exact length match before decoding, so
// a non-canonical encoding (padded, alternate alphabet, trailing junk) that
// would decode to the right byte count is rejected outright rather than
// silently accepted as equivalent to the canonical token.
var (
	tokenEncodedLen        = base64.RawURLEncoding.EncodedLen(16 + macLen)
	accessCookieEncodedLen = base64.RawURLEncoding.EncodedLen(8 + macLen)
)

// ErrMalformed is returned by Split for a token that isn't the right shape.
// It deliberately carries no detail: every caller maps it to the same
// generic 404, so malformed and merely-wrong tokens look identical.
var ErrMalformed = errors.New("token: malformed")

// Keys holds the subkeys derived from AUTH_SECRET. It is safe for concurrent use.
type Keys struct {
	guest, rsvp, access, ip []byte
}

// NewKeys derives the four subkeys used across the package. authSecret must
// be at least 32 bytes (config.Load already enforces this; the check here
// guards direct callers such as tests).
func NewKeys(authSecret []byte) (*Keys, error) {
	if len(authSecret) < 32 {
		return nil, errors.New("token: auth secret must be at least 32 bytes")
	}
	labels := map[string]*[]byte{}
	k := &Keys{}
	labels["syt guest v1"] = &k.guest
	labels["syt rsvp v1"] = &k.rsvp
	labels["syt access v1"] = &k.access
	labels["syt ip v1"] = &k.ip
	for label, dst := range labels {
		key, err := hkdf.Key(sha256.New, authSecret, nil, label, 32)
		if err != nil {
			return nil, fmt.Errorf("derive %q: %w", label, err)
		}
		*dst = key
	}
	return k, nil
}

// GuestToken derives the invite/RSVP token for a guest. Rotating
// guests.token_version invalidates every token issued for a previous version.
func (k *Keys) GuestToken(guestID uuid.UUID, version int32) string {
	return encode(guestID, sign(k.guest, versionedMessage(guestID, version)))
}

// CheckGuest reports whether mac is the correct, current-version MAC for guestID.
func (k *Keys) CheckGuest(id uuid.UUID, version int32, mac []byte) bool {
	return hmac.Equal(sign(k.guest, versionedMessage(id, version)), mac)
}

// RSVPToken derives the edit-link token for an open (non-guest) RSVP. It
// carries rsvps.edit_token_version the same way a guest token carries
// guests.token_version, so RotateRSVPEditToken can revoke a leaked or
// no-longer-wanted edit link without touching the RSVP row's identity.
func (k *Keys) RSVPToken(rsvpID uuid.UUID, version int32) string {
	return encode(rsvpID, sign(k.rsvp, versionedMessage(rsvpID, version)))
}

// CheckRSVP reports whether mac is the correct, current-version MAC for rsvpID.
func (k *Keys) CheckRSVP(id uuid.UUID, version int32, mac []byte) bool {
	return hmac.Equal(sign(k.rsvp, versionedMessage(id, version)), mac)
}

// Split decodes a token into the id it names and its MAC. It does not
// verify the MAC: callers pass the id and mac to CheckGuest/CheckRSVP,
// which know which subkey applies.
func Split(tok string) (id uuid.UUID, mac []byte, err error) {
	// Strict + an exact expected-length check rejects non-canonical
	// encodings (extra padding, alternate alphabets a non-Go client could
	// smuggle through) that would otherwise decode to the same bytes as a
	// legitimate token but let two different-looking strings both pass.
	if len(tok) != tokenEncodedLen {
		return uuid.Nil, nil, ErrMalformed
	}
	raw, err := base64.RawURLEncoding.Strict().DecodeString(tok)
	if err != nil || len(raw) != 16+macLen {
		return uuid.Nil, nil, ErrMalformed
	}
	id, err = uuid.FromBytes(raw[:16])
	if err != nil {
		return uuid.Nil, nil, ErrMalformed
	}
	return id, raw[16:], nil
}

// versionedMessage is the signed message for both guest and RSVP tokens:
// the id plus a 4-byte big-endian version, so bumping the version in the
// database invalidates every token signed under the old one.
func versionedMessage(id uuid.UUID, version int32) []byte {
	msg := make([]byte, 16+4)
	copy(msg, id[:])
	binary.BigEndian.PutUint32(msg[16:], uint32(version))
	return msg
}

func sign(key, msg []byte) []byte {
	mac := hmac.New(sha256.New, key)
	mac.Write(msg)
	return mac.Sum(nil)[:macLen]
}

func encode(id uuid.UUID, mac []byte) string {
	buf := make([]byte, 16+macLen)
	copy(buf, id[:])
	copy(buf[16:], mac)
	return base64.RawURLEncoding.EncodeToString(buf)
}

// AccessCookie derives the value of a syt_pw_<event> password-gate cookie.
// Binding SHA-256(passwordHash) means every issued cookie stops working the
// moment the event's password changes, without tracking cookies anywhere.
func (k *Keys) AccessCookie(eventID uuid.UUID, passwordHash string, exp time.Time) string {
	expSec := exp.Unix()
	mac := sign(k.access, accessMessage(eventID, expSec, passwordHash))
	buf := make([]byte, 8+macLen)
	binary.BigEndian.PutUint64(buf, uint64(expSec))
	copy(buf[8:], mac)
	return base64.RawURLEncoding.EncodeToString(buf)
}

// CheckAccessCookie reports whether v is a current, unexpired access cookie
// for eventID under passwordHash.
func (k *Keys) CheckAccessCookie(v string, eventID uuid.UUID, passwordHash string, now time.Time) bool {
	if len(v) != accessCookieEncodedLen {
		return false
	}
	raw, err := base64.RawURLEncoding.Strict().DecodeString(v)
	if err != nil || len(raw) != 8+macLen {
		return false
	}
	expSec := int64(binary.BigEndian.Uint64(raw[:8]))
	if now.Unix() > expSec {
		return false
	}
	return hmac.Equal(sign(k.access, accessMessage(eventID, expSec, passwordHash)), raw[8:])
}

func accessMessage(eventID uuid.UUID, expSec int64, passwordHash string) []byte {
	fp := sha256.Sum256([]byte(passwordHash))
	msg := make([]byte, 0, 16+8+len(fp))
	msg = append(msg, eventID[:]...)
	expBytes := make([]byte, 8)
	binary.BigEndian.PutUint64(expBytes, uint64(expSec))
	msg = append(msg, expBytes...)
	msg = append(msg, fp[:]...)
	return msg
}

// HashIP returns a keyed HMAC-SHA256 of ip, used for reports.reporter_ip_hash.
// A keyed hash (rather than plain SHA-256) can't be reversed by brute-forcing
// the IPv4 address space.
func (k *Keys) HashIP(ip netip.Addr) []byte {
	ip = ip.Unmap()
	mac := hmac.New(sha256.New, k.ip)
	mac.Write(ip.AsSlice())
	return mac.Sum(nil)
}
