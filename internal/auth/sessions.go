package auth

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"errors"
	"fmt"
	"log/slog"
	"net/netip"
	"strings"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/redis/go-redis/v9"

	"github.com/ansonarose/seeyouthere-backend/internal/store"
)

var ErrNoSession = errors.New("no active session")

// How long Valkey may answer for a session before Postgres is asked again.
// Keeps suspensions and expiry effective within a few minutes.
const sessionCacheTTL = 5 * time.Minute

// Session identifies an authenticated request: the session row itself (for
// MFA step-up checks, "current session" in the sessions list, ...) and the
// user it belongs to.
type Session struct {
	ID     uuid.UUID
	UserID uuid.UUID
}

// Sessions are stored in Postgres (source of truth) and mirrored in Valkey.
type Sessions struct {
	q   *store.Queries
	rdb *redis.Client
	ttl time.Duration
}

func NewSessions(q *store.Queries, rdb *redis.Client, ttl time.Duration) *Sessions {
	return &Sessions{q: q, rdb: rdb, ttl: ttl}
}

// Create starts a session and returns the raw token for the cookie.
func (s *Sessions) Create(ctx context.Context, userID uuid.UUID, ip *netip.Addr, userAgent string) (string, time.Time, error) {
	raw := make([]byte, 32)
	if _, err := rand.Read(raw); err != nil {
		return "", time.Time{}, err
	}
	token := base64.RawURLEncoding.EncodeToString(raw)
	expires := time.Now().Add(s.ttl)

	if len(userAgent) > 512 {
		userAgent = userAgent[:512]
	}
	_, err := s.q.CreateSession(ctx, store.CreateSessionParams{
		ID:        uuid.Must(uuid.NewV7()),
		UserID:    userID,
		TokenHash: hashToken(token),
		ExpiresAt: expires,
		Ip:        ip,
		UserAgent: userAgent,
	})
	if err != nil {
		return "", time.Time{}, fmt.Errorf("create session: %w", err)
	}
	return token, expires, nil
}

// Lookup returns the Session for an active session token.
func (s *Sessions) Lookup(ctx context.Context, token string) (Session, error) {
	if token == "" {
		return Session{}, ErrNoSession
	}
	hash := hashToken(token)
	key := sessionKey(hash)

	if cached, err := s.rdb.Get(ctx, key).Result(); err == nil {
		if sess, ok := parseCachedSession(cached); ok {
			return sess, nil
		}
	} else if !errors.Is(err, redis.Nil) {
		return Session{}, fmt.Errorf("session cache: %w", err)
	}

	row, err := s.q.GetActiveSession(ctx, hash)
	if errors.Is(err, pgx.ErrNoRows) {
		return Session{}, ErrNoSession
	}
	if err != nil {
		return Session{}, fmt.Errorf("load session: %w", err)
	}
	sess := Session{ID: row.ID, UserID: row.UserID}

	ttl := min(time.Until(row.ExpiresAt), sessionCacheTTL)
	if ttl > 0 {
		s.rdb.Set(ctx, key, cacheValue(sess), ttl)
	}

	// A cache miss means it's been a while since this session was seen
	// (or it's brand new); touch it so sessions.last_seen_at stays roughly
	// current for the account's session list. TouchSession itself only
	// writes when last_seen_at is more than 5 minutes stale.
	if err := s.q.TouchSession(ctx, row.ID); err != nil {
		slog.WarnContext(ctx, "touch session", "err", err, "session_id", row.ID)
	}
	return sess, nil
}

// Revoke ends the session for token. Missing sessions are not an error.
// Postgres is deleted first, then the Valkey mirror (matching RevokeHashes):
// deleting the cache entry first would leave a window where a concurrent
// Lookup, on a cache miss, re-reads the still-live DB row and re-populates
// the cache with a session this call is trying to kill.
func (s *Sessions) Revoke(ctx context.Context, token string) error {
	hash := hashToken(token)
	if err := s.q.DeleteSessionByTokenHash(ctx, hash); err != nil {
		return fmt.Errorf("delete session: %w", err)
	}
	if err := s.rdb.Del(ctx, sessionKey(hash)).Err(); err != nil {
		return fmt.Errorf("session cache: %w", err)
	}
	return nil
}

// RevokeHashes drops the Valkey mirror entries for token hashes already
// deleted from Postgres by the caller (e.g. DeleteAllUserSessions
// RETURNING token_hash). It does not touch the database itself: by the
// time a caller has hashes to pass here, the DB row is already gone.
func (s *Sessions) RevokeHashes(ctx context.Context, hashes [][]byte) error {
	if len(hashes) == 0 {
		return nil
	}
	keys := make([]string, len(hashes))
	for i, h := range hashes {
		keys[i] = sessionKey(h)
	}
	if err := s.rdb.Del(ctx, keys...).Err(); err != nil {
		return fmt.Errorf("session cache: %w", err)
	}
	return nil
}

func hashToken(token string) []byte {
	sum := sha256.Sum256([]byte(token))
	return sum[:]
}

func sessionKey(hash []byte) string { return "session:" + hex.EncodeToString(hash) }

// cacheValue and parseCachedSession encode a Session as "<user_id> <session_id>".
func cacheValue(s Session) string { return s.UserID.String() + " " + s.ID.String() }

func parseCachedSession(v string) (Session, bool) {
	userStr, sessStr, ok := strings.Cut(v, " ")
	if !ok {
		return Session{}, false
	}
	userID, err := uuid.Parse(userStr)
	if err != nil {
		return Session{}, false
	}
	sessID, err := uuid.Parse(sessStr)
	if err != nil {
		return Session{}, false
	}
	return Session{ID: sessID, UserID: userID}, true
}
