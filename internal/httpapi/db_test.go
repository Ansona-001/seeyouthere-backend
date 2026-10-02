package httpapi

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"os"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/ansonarose/seeyouthere-backend/internal/config"
	"github.com/ansonarose/seeyouthere-backend/internal/store"
)

// dbTestPool opens a pool for DB-backed tests, skipping when DATABASE_URL
// isn't set (per CLAUDE.md: these hit the real local Postgres from
// `docker compose up`, and are skipped otherwise, e.g. in an environment
// without it).
func dbTestPool(t *testing.T) *pgxpool.Pool {
	t.Helper()
	url := os.Getenv("DATABASE_URL")
	if url == "" {
		t.Skip("DATABASE_URL not set")
	}
	pool, err := pgxpool.New(context.Background(), url)
	if err != nil {
		t.Fatalf("open pool: %v", err)
	}
	t.Cleanup(pool.Close)
	return pool
}

func TestInTx_CommitsOnSuccessRollsBackOnError(t *testing.T) {
	pool := dbTestPool(t)
	s := &Server{pool: pool, q: store.New(pool)}
	ctx := context.Background()

	userID := uuid.Must(uuid.NewV7())
	email := "intx-test-" + userID.String() + "@example.invalid"

	// A failing fn must roll back: the row must not exist afterward.
	forced := errors.New("forced rollback")
	err := s.inTx(ctx, func(_ pgx.Tx, q *store.Queries) error {
		if _, err := q.UpsertUserByEmail(ctx, store.UpsertUserByEmailParams{ID: userID, Email: email}); err != nil {
			return err
		}
		return forced
	})
	if !errors.Is(err, forced) {
		t.Fatalf("err = %v, want %v", err, forced)
	}
	if _, err := s.q.GetUserByID(ctx, userID); !errors.Is(err, pgx.ErrNoRows) {
		t.Errorf("row should not exist after rollback, GetUserByID err = %v", err)
	}

	// A succeeding fn must commit.
	err = s.inTx(ctx, func(_ pgx.Tx, q *store.Queries) error {
		_, err := q.UpsertUserByEmail(ctx, store.UpsertUserByEmailParams{ID: userID, Email: email})
		return err
	})
	if err != nil {
		t.Fatalf("inTx: %v", err)
	}
	t.Cleanup(func() { _ = s.q.SoftDeleteUser(ctx, userID) })

	if _, err := s.q.GetUserByID(ctx, userID); err != nil {
		t.Errorf("row should exist after commit: %v", err)
	}
}

// adminTestFixture creates a user and session inside tx (rolled back by the
// caller) and returns their ids.
func adminTestFixture(t *testing.T, ctx context.Context, q *store.Queries) (userID, sessionID uuid.UUID) {
	t.Helper()
	userID = uuid.Must(uuid.NewV7())
	if _, err := q.UpsertUserByEmail(ctx, store.UpsertUserByEmailParams{
		ID: userID, Email: "admin-test-" + userID.String() + "@example.invalid",
	}); err != nil {
		t.Fatalf("create user: %v", err)
	}
	sess, err := q.CreateSession(ctx, store.CreateSessionParams{
		ID: uuid.Must(uuid.NewV7()), UserID: userID, TokenHash: []byte(userID.String()),
		ExpiresAt: time.Now().Add(time.Hour), UserAgent: "test",
	})
	if err != nil {
		t.Fatalf("create session: %v", err)
	}
	return userID, sess.ID
}

func requestWithSession(userID, sessionID uuid.UUID) *http.Request {
	req := httptest.NewRequest(http.MethodGet, "/", nil)
	ctx := context.WithValue(req.Context(), ctxUserID, userID)
	ctx = context.WithValue(ctx, ctxSessionID, sessionID)
	return req.WithContext(ctx)
}

func TestRequireAdmin_RoleAndMFAGates(t *testing.T) {
	pool := dbTestPool(t)
	ctx := context.Background()
	tx, err := pool.Begin(ctx)
	if err != nil {
		t.Fatalf("begin: %v", err)
	}
	t.Cleanup(func() { _ = tx.Rollback(ctx) })
	q := store.New(tx)

	userID, sessionID := adminTestFixture(t, ctx, q)

	s := &Server{q: q, cfg: config.Config{AdminMFATTL: 12 * time.Hour}}
	called := false
	h := s.requireAdmin("moderator")(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		called = true
		w.WriteHeader(http.StatusOK)
	}))

	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, requestWithSession(userID, sessionID))
	if rec.Code != http.StatusForbidden || called {
		t.Fatalf("no role: status = %d, called = %v", rec.Code, called)
	}

	if _, err := q.GrantRole(ctx, store.GrantRoleParams{UserID: userID, Role: "moderator"}); err != nil {
		t.Fatalf("grant role: %v", err)
	}

	rec = httptest.NewRecorder()
	h.ServeHTTP(rec, requestWithSession(userID, sessionID))
	if rec.Code != http.StatusForbidden || called {
		t.Fatalf("role but no mfa: status = %d, called = %v", rec.Code, called)
	}

	if err := q.SetSessionMFAVerified(ctx, store.SetSessionMFAVerifiedParams{SessionID: sessionID, UserID: userID}); err != nil {
		t.Fatalf("set mfa verified: %v", err)
	}

	rec = httptest.NewRecorder()
	h.ServeHTTP(rec, requestWithSession(userID, sessionID))
	if rec.Code != http.StatusOK || !called {
		t.Fatalf("role + mfa: status = %d, called = %v", rec.Code, called)
	}
}

func TestRequireAdmin_UnauthenticatedRejected(t *testing.T) {
	s := &Server{}
	h := s.requireAdmin("support")(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		t.Error("handler should not run without a session")
	}))
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/", nil))
	if rec.Code != http.StatusUnauthorized {
		t.Errorf("status = %d, want 401", rec.Code)
	}
}

func TestWriteAudit(t *testing.T) {
	pool := dbTestPool(t)
	ctx := context.Background()
	tx, err := pool.Begin(ctx)
	if err != nil {
		t.Fatalf("begin: %v", err)
	}
	t.Cleanup(func() { _ = tx.Rollback(ctx) })
	q := store.New(tx)

	// audit_log.actor_id has a foreign key to users, so the actor must be a
	// real row (audit_log_actor_id_fkey).
	actorID, _ := adminTestFixture(t, ctx, q)
	targetID := uuid.Must(uuid.NewV7())
	s := &Server{}
	req := httptest.NewRequest(http.MethodPost, "/", nil)

	err = s.writeAudit(ctx, q, req, auditEntry{
		ActorID:    &actorID,
		Action:     "test.action",
		TargetType: "user",
		TargetID:   targetID.String(),
		Before:     map[string]string{"status": "active"},
		After:      map[string]string{"status": "suspended"},
	})
	if err != nil {
		t.Fatalf("writeAudit: %v", err)
	}

	rows, err := q.ListAuditForTarget(ctx, store.ListAuditForTargetParams{
		TargetType: "user", TargetID: targetID.String(),
		CursorCreatedAt: time.Date(9999, 12, 31, 0, 0, 0, 0, time.UTC),
		CursorID:        uuid.Max,
		Lim:             10,
	})
	if err != nil {
		t.Fatalf("ListAuditForTarget: %v", err)
	}
	if len(rows) != 1 {
		t.Fatalf("expected 1 audit row, got %d", len(rows))
	}
	row := rows[0]
	if row.Action != "test.action" || row.TargetType != "user" || row.TargetID != targetID.String() {
		t.Errorf("unexpected row: %+v", row)
	}
	if row.ActorID == nil || *row.ActorID != actorID {
		t.Errorf("actor_id = %v, want %v", row.ActorID, actorID)
	}
	if len(row.Before) == 0 || len(row.After) == 0 {
		t.Error("expected non-empty before/after JSON")
	}
}
