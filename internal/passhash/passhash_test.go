package passhash

import (
	"context"
	"encoding/base64"
	"fmt"
	"strings"
	"testing"
	"time"
)

func TestHashAndVerify(t *testing.T) {
	h := New(2)
	ctx := context.Background()

	encoded, err := h.Hash(ctx, "correct horse battery staple")
	if err != nil {
		t.Fatalf("Hash: %v", err)
	}
	if !strings.HasPrefix(encoded, "$argon2id$v=19$m=19456,t=2,p=1$") {
		t.Fatalf("unexpected encoding: %s", encoded)
	}

	ok, err := h.Verify(ctx, encoded, "correct horse battery staple")
	if err != nil || !ok {
		t.Fatalf("Verify(correct) = %v, %v; want true, nil", ok, err)
	}

	ok, err = h.Verify(ctx, encoded, "wrong password")
	if err != nil || ok {
		t.Fatalf("Verify(wrong) = %v, %v; want false, nil", ok, err)
	}
}

func TestVerify_MalformedHash(t *testing.T) {
	h := New(2)
	ctx := context.Background()
	cases := []string{
		"",
		"not-a-hash",
		"$argon2i$v=19$m=19456,t=2,p=1$c2FsdA$aGFzaA",  // wrong algorithm
		"$argon2id$v=18$m=19456,t=2,p=1$c2FsdA$aGFzaA", // wrong version
		"$argon2id$v=19$m=oops$c2FsdA$aGFzaA",          // unparseable params
		"$argon2id$v=19$m=19456,t=2,p=1$$aGFzaA",       // empty salt
	}
	for _, c := range cases {
		if _, err := h.Verify(ctx, c, "password"); err != ErrMalformedHash {
			t.Errorf("Verify(%q) err = %v, want ErrMalformedHash", c, err)
		}
	}
}

// TestVerify_ParamAndLengthMismatchesRejected covers the fix that a decoded
// PHC string must match this package's own Argon2id parameters and exact
// salt/key lengths exactly, and that trailing garbage after a valid p=..
// field is caught instead of silently ignored by Sscanf.
func TestVerify_ParamAndLengthMismatchesRejected(t *testing.T) {
	h := New(2)
	ctx := context.Background()

	salt16 := base64.RawStdEncoding.EncodeToString(make([]byte, saltLen))
	hash32 := base64.RawStdEncoding.EncodeToString(make([]byte, keyLen))
	saltShort := base64.RawStdEncoding.EncodeToString(make([]byte, saltLen-1))
	saltLong := base64.RawStdEncoding.EncodeToString(make([]byte, saltLen+1))
	hashShort := base64.RawStdEncoding.EncodeToString(make([]byte, keyLen-1))
	hashLong := base64.RawStdEncoding.EncodeToString(make([]byte, keyLen+1))

	cases := map[string]string{
		"wrong memory param":        fmt.Sprintf("$argon2id$v=19$m=%d,t=%d,p=%d$%s$%s", argonMemory+1, argonTime, argonThreads, salt16, hash32),
		"wrong time param":          fmt.Sprintf("$argon2id$v=19$m=%d,t=%d,p=%d$%s$%s", argonMemory, argonTime+1, argonThreads, salt16, hash32),
		"wrong threads param":       fmt.Sprintf("$argon2id$v=19$m=%d,t=%d,p=%d$%s$%s", argonMemory, argonTime, argonThreads+1, salt16, hash32),
		"trailing garbage after p=": fmt.Sprintf("$argon2id$v=19$m=%d,t=%d,p=%dJUNK$%s$%s", argonMemory, argonTime, argonThreads, salt16, hash32),
		"salt too short":            fmt.Sprintf("$argon2id$v=19$m=%d,t=%d,p=%d$%s$%s", argonMemory, argonTime, argonThreads, saltShort, hash32),
		"salt too long":             fmt.Sprintf("$argon2id$v=19$m=%d,t=%d,p=%d$%s$%s", argonMemory, argonTime, argonThreads, saltLong, hash32),
		"hash too short":            fmt.Sprintf("$argon2id$v=19$m=%d,t=%d,p=%d$%s$%s", argonMemory, argonTime, argonThreads, salt16, hashShort),
		"hash too long":             fmt.Sprintf("$argon2id$v=19$m=%d,t=%d,p=%d$%s$%s", argonMemory, argonTime, argonThreads, salt16, hashLong),
	}
	for name, encoded := range cases {
		t.Run(name, func(t *testing.T) {
			if _, err := h.Verify(ctx, encoded, "password"); err != ErrMalformedHash {
				t.Errorf("Verify(%s) err = %v, want ErrMalformedHash", name, err)
			}
		})
	}

	// The exact-length, exact-param encoding of this package's own output
	// must still verify normally (sanity check that the stricter parsing
	// didn't also reject legitimate hashes).
	valid := fmt.Sprintf("$argon2id$v=19$m=%d,t=%d,p=%d$%s$%s", argonMemory, argonTime, argonThreads, salt16, hash32)
	if _, err := h.Verify(ctx, valid, "password"); err != nil {
		t.Errorf("Verify(valid-shaped hash) err = %v, want nil", err)
	}
}

func TestVerify_DifferentSaltsProduceDifferentHashes(t *testing.T) {
	h := New(2)
	ctx := context.Background()
	a, err := h.Hash(ctx, "same password")
	if err != nil {
		t.Fatalf("Hash: %v", err)
	}
	b, err := h.Hash(ctx, "same password")
	if err != nil {
		t.Fatalf("Hash: %v", err)
	}
	if a == b {
		t.Error("two hashes of the same password should differ (random salt)")
	}
}

func TestSemaphore_HonoursContextCancellation(t *testing.T) {
	h := New(1)
	ctx := context.Background()

	// Occupy the only slot directly (bypassing Hash, to avoid depending on
	// how long a real Argon2id computation takes).
	if err := h.acquire(ctx); err != nil {
		t.Fatalf("acquire: %v", err)
	}
	defer h.release()

	cancelCtx, cancel := context.WithTimeout(context.Background(), 50*time.Millisecond)
	defer cancel()

	start := time.Now()
	_, err := h.Hash(cancelCtx, "password")
	if err == nil {
		t.Fatal("expected an error when the semaphore can't be acquired before the deadline")
	}
	if elapsed := time.Since(start); elapsed > time.Second {
		t.Fatalf("Hash blocked for %v; should have returned promptly on cancellation", elapsed)
	}
}
