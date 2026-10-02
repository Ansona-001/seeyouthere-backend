// Package auth handles email one-time login codes and session tokens.
package auth

import (
	"context"
	"crypto/hmac"
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"math/big"
	"time"

	"github.com/redis/go-redis/v9"
)

const (
	CodeTTL         = 10 * time.Minute
	MaxCodeAttempts = 5
)

var (
	ErrCodeInvalid     = errors.New("code is invalid or expired")
	ErrTooManyAttempts = errors.New("too many attempts")
)

// Codes stores one pending login code per email in Valkey, as an HMAC, never in plain text.
type Codes struct {
	rdb    *redis.Client
	secret []byte
}

func NewCodes(rdb *redis.Client, secret []byte) *Codes {
	return &Codes{rdb: rdb, secret: secret}
}

// Issue creates a fresh 6-digit code for email, replacing any earlier one.
func (c *Codes) Issue(ctx context.Context, email string) (string, error) {
	n, err := rand.Int(rand.Reader, big.NewInt(1_000_000))
	if err != nil {
		return "", err
	}
	code := fmt.Sprintf("%06d", n.Int64())

	key := codeKey(email)
	_, err = c.rdb.TxPipelined(ctx, func(p redis.Pipeliner) error {
		p.Del(ctx, key)
		p.HSet(ctx, key, "mac", c.mac(email, code), "attempts", 0)
		p.Expire(ctx, key, CodeTTL)
		return nil
	})
	if err != nil {
		return "", fmt.Errorf("store login code: %w", err)
	}
	return code, nil
}

// verifyScript checks a code atomically. Returns 1 on match, 0 on mismatch or missing,
// -1 when the attempt limit is exceeded. The code is deleted on success and on lockout.
var verifyScript = redis.NewScript(`
local mac = redis.call('HGET', KEYS[1], 'mac')
if not mac then return 0 end
local attempts = redis.call('HINCRBY', KEYS[1], 'attempts', 1)
if attempts > tonumber(ARGV[2]) then
  redis.call('DEL', KEYS[1])
  return -1
end
if mac == ARGV[1] then
  redis.call('DEL', KEYS[1])
  return 1
end
return 0
`)

// Verify consumes the code if it matches.
func (c *Codes) Verify(ctx context.Context, email, code string) error {
	res, err := verifyScript.Run(ctx, c.rdb, []string{codeKey(email)}, c.mac(email, code), MaxCodeAttempts).Int()
	if err != nil {
		return fmt.Errorf("verify login code: %w", err)
	}
	switch res {
	case 1:
		return nil
	case -1:
		return ErrTooManyAttempts
	default:
		return ErrCodeInvalid
	}
}

func (c *Codes) mac(email, code string) string {
	h := hmac.New(sha256.New, c.secret)
	h.Write([]byte("login-code\x00" + email + "\x00" + code))
	return hex.EncodeToString(h.Sum(nil))
}

func codeKey(email string) string { return "login:code:" + email }
