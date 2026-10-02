// Package ratelimit implements fixed-window counters in Valkey.
package ratelimit

import (
	"context"
	"time"

	"github.com/redis/go-redis/v9"
)

type Limiter struct {
	rdb *redis.Client
}

func New(rdb *redis.Client) *Limiter { return &Limiter{rdb: rdb} }

// Allow counts one hit against key and reports whether it is within limit for the window.
func (l *Limiter) Allow(ctx context.Context, key string, limit int64, window time.Duration) (bool, error) {
	ok, err := l.AllowN(ctx, key, 1, limit, window)
	return ok, err
}

// AllowN counts n hits at once against key (a single request that represents
// several logical actions, e.g. one send-invites call queuing several
// emails) and reports whether the window's running total, including these n,
// stays within limit. On "no" it uncounts n, so a rejected batch doesn't
// permanently consume quota it never actually used.
func (l *Limiter) AllowN(ctx context.Context, key string, n, limit int64, window time.Duration) (bool, error) {
	key = "rl:" + key
	var incr *redis.IntCmd
	_, err := l.rdb.TxPipelined(ctx, func(p redis.Pipeliner) error {
		incr = p.IncrBy(ctx, key, n)
		p.ExpireNX(ctx, key, window)
		return nil
	})
	if err != nil {
		return false, err
	}
	if incr.Val() > limit {
		l.rdb.DecrBy(ctx, key, n)
		return false, nil
	}
	return true, nil
}

// Uncount removes n previously counted hits from key. It's for a caller that
// ran several AllowN checks against different keys for the same batch and
// needs to roll back the ones that already succeeded once a later check in
// that batch fails (AllowN only ever compensates its own key, never a
// sibling's).
func (l *Limiter) Uncount(ctx context.Context, key string, n int64) {
	l.rdb.DecrBy(ctx, "rl:"+key, n)
}
