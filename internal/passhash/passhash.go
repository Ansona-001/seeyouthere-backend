// Package passhash hashes and verifies event passwords with Argon2id
// (OWASP-recommended parameters), storing/parsing the standard PHC string
// format. Every call runs behind a caller-sized semaphore: Argon2id is
// deliberately memory- and CPU-hard, so on a 1 vCPU host unbounded
// concurrent hashing is itself a denial-of-service vector.
package passhash

import (
	"context"
	"crypto/rand"
	"crypto/subtle"
	"encoding/base64"
	"errors"
	"fmt"
	"io"
	"strings"

	"golang.org/x/crypto/argon2"
)

// OWASP-recommended Argon2id parameters for the "less memory available"
// profile: m=19 MiB, t=2, p=1. 16-byte salt, 32-byte key.
const (
	saltLen      = 16
	keyLen       = 32
	argonTime    = 2
	argonMemory  = 19 * 1024 // KiB
	argonThreads = 1
)

// ErrMalformedHash is returned by Verify when encoded isn't a hash this
// package produced (wrong algorithm, unparseable parameters/salt/hash).
var ErrMalformedHash = errors.New("passhash: malformed encoded hash")

// Hasher bounds concurrent Argon2id computations to maxConcurrent.
type Hasher struct {
	sem chan struct{}
}

// New returns a Hasher that allows at most maxConcurrent Hash/Verify calls
// to run their Argon2id computation at once; further calls block until a
// slot frees up or ctx is cancelled.
func New(maxConcurrent int) *Hasher {
	return &Hasher{sem: make(chan struct{}, maxConcurrent)}
}

func (h *Hasher) acquire(ctx context.Context) error {
	select {
	case h.sem <- struct{}{}:
		return nil
	case <-ctx.Done():
		return ctx.Err()
	}
}

func (h *Hasher) release() { <-h.sem }

// Hash derives and PHC-encodes an Argon2id hash of password with a fresh
// random salt.
func (h *Hasher) Hash(ctx context.Context, password string) (string, error) {
	if err := h.acquire(ctx); err != nil {
		return "", err
	}
	defer h.release()

	salt := make([]byte, saltLen)
	if _, err := rand.Read(salt); err != nil {
		return "", fmt.Errorf("passhash: generate salt: %w", err)
	}
	key := argon2.IDKey([]byte(password), salt, argonTime, argonMemory, argonThreads, keyLen)
	return encode(salt, key), nil
}

// Verify reports whether password matches encoded, comparing in constant
// time. A malformed encoded hash is reported as an error (a data problem,
// not a wrong-password result) before the semaphore is acquired, so a
// corrupt row can't be used to bypass the concurrency limit for free.
func (h *Hasher) Verify(ctx context.Context, encoded, password string) (bool, error) {
	p, salt, hash, err := decode(encoded)
	if err != nil {
		return false, err
	}
	if err := h.acquire(ctx); err != nil {
		return false, err
	}
	defer h.release()

	computed := argon2.IDKey([]byte(password), salt, p.time, p.memory, p.threads, uint32(len(hash)))
	return subtle.ConstantTimeCompare(computed, hash) == 1, nil
}

type params struct {
	time, memory uint32
	threads      uint8
}

func encode(salt, key []byte) string {
	return fmt.Sprintf("$argon2id$v=%d$m=%d,t=%d,p=%d$%s$%s",
		argon2.Version, argonMemory, argonTime, argonThreads,
		base64.RawStdEncoding.EncodeToString(salt),
		base64.RawStdEncoding.EncodeToString(key))
}

// decode parses a PHC-format Argon2id hash: $argon2id$v=19$m=..,t=..,p=..$salt$hash
func decode(encoded string) (params, []byte, []byte, error) {
	parts := strings.Split(encoded, "$")
	if len(parts) != 6 || parts[0] != "" || parts[1] != "argon2id" {
		return params{}, nil, nil, ErrMalformedHash
	}
	var version int
	if _, err := fmt.Sscanf(parts[2], "v=%d", &version); err != nil || version != argon2.Version {
		return params{}, nil, nil, ErrMalformedHash
	}
	var m, t, th int
	var extra string
	// A trailing %s (matched against "" on a clean string) catches garbage
	// appended after the p=.. field, which Sscanf would otherwise silently
	// ignore since it stops at the format string's end, not the input's.
	if n, err := fmt.Sscanf(parts[3], "m=%d,t=%d,p=%d%s", &m, &t, &th, &extra); (err != nil && err != io.EOF) || n < 3 || extra != "" {
		return params{}, nil, nil, ErrMalformedHash
	}
	// Reject anything but exactly this package's own parameters: a hash
	// encoded under different Argon2id cost parameters (an old rotation, a
	// tampered row) must fail closed rather than be verified under whatever
	// parameters happen to be embedded in it.
	if m != argonMemory || t != argonTime || th != argonThreads {
		return params{}, nil, nil, ErrMalformedHash
	}
	salt, err := base64.RawStdEncoding.DecodeString(parts[4])
	if err != nil || len(salt) != saltLen {
		return params{}, nil, nil, ErrMalformedHash
	}
	hash, err := base64.RawStdEncoding.DecodeString(parts[5])
	if err != nil || len(hash) != keyLen {
		return params{}, nil, nil, ErrMalformedHash
	}
	return params{time: uint32(t), memory: uint32(m), threads: uint8(th)}, salt, hash, nil
}
