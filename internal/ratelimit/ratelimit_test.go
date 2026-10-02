package ratelimit

import (
	"context"
	"os"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/redis/go-redis/v9"
)

func testRedis(t *testing.T) *redis.Client {
	t.Helper()
	url := os.Getenv("VALKEY_URL")
	if url == "" {
		url = "redis://localhost:6380/0"
	}
	opts, err := redis.ParseURL(url)
	if err != nil {
		t.Fatalf("parse VALKEY_URL: %v", err)
	}
	rdb := redis.NewClient(opts)
	if err := rdb.Ping(context.Background()).Err(); err != nil {
		t.Skip("valkey not reachable: " + err.Error())
	}
	t.Cleanup(func() { _ = rdb.Close() })
	return rdb
}

func TestAllow_WithinAndOverLimit(t *testing.T) {
	rdb := testRedis(t)
	l := New(rdb)
	ctx := context.Background()
	key := "test:allow:" + uuid.Must(uuid.NewV7()).String()
	t.Cleanup(func() { rdb.Del(context.Background(), "rl:"+key) })

	for i := 0; i < 3; i++ {
		ok, err := l.Allow(ctx, key, 3, time.Minute)
		if err != nil {
			t.Fatalf("Allow: %v", err)
		}
		if !ok {
			t.Fatalf("hit %d: expected allowed within limit 3", i+1)
		}
	}
	ok, err := l.Allow(ctx, key, 3, time.Minute)
	if err != nil {
		t.Fatalf("Allow: %v", err)
	}
	if ok {
		t.Fatal("4th hit should exceed limit 3")
	}
}

func TestAllowN_BatchCountsAllAtOnceAndUncountsOnRejection(t *testing.T) {
	rdb := testRedis(t)
	l := New(rdb)
	ctx := context.Background()
	key := "test:allown:" + uuid.Must(uuid.NewV7()).String()
	t.Cleanup(func() { rdb.Del(context.Background(), "rl:"+key) })

	// A batch of 5 against a limit of 10 is allowed and counts all 5 at once.
	ok, err := l.AllowN(ctx, key, 5, 10, time.Minute)
	if err != nil {
		t.Fatalf("AllowN: %v", err)
	}
	if !ok {
		t.Fatal("first batch of 5 should be allowed under limit 10")
	}

	// A second batch of 5 reaches exactly the limit and is still allowed.
	ok, err = l.AllowN(ctx, key, 5, 10, time.Minute)
	if err != nil {
		t.Fatalf("AllowN: %v", err)
	}
	if !ok {
		t.Fatal("second batch of 5 should be allowed, reaching the limit exactly")
	}

	// A third batch of 1 would push the total to 11 and must be rejected...
	ok, err = l.AllowN(ctx, key, 1, 10, time.Minute)
	if err != nil {
		t.Fatalf("AllowN: %v", err)
	}
	if ok {
		t.Fatal("batch that would exceed the limit should be rejected")
	}

	// ...and the rejection must have uncounted itself: the counter should
	// still read exactly 10, so a fresh batch of exactly the remaining
	// headroom (0 here) behaves consistently rather than drifting upward
	// on every rejected attempt.
	val, err := rdb.Get(ctx, "rl:"+key).Int64()
	if err != nil {
		t.Fatalf("get counter: %v", err)
	}
	if val != 10 {
		t.Fatalf("counter after rejected batch = %d, want 10 (rejection should uncount itself)", val)
	}
}
