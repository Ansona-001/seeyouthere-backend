package httpapi

import (
	"context"
	"crypto/sha256"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/redis/go-redis/v9"

	"github.com/ansonarose/seeyouthere-backend/internal/auth"
	"github.com/ansonarose/seeyouthere-backend/internal/config"
	"github.com/ansonarose/seeyouthere-backend/internal/jobs"
	"github.com/ansonarose/seeyouthere-backend/internal/media"
	"github.com/ansonarose/seeyouthere-backend/internal/ratelimit"
	"github.com/ansonarose/seeyouthere-backend/internal/store"
)

func TestValidateUserName(t *testing.T) {
	tests := []struct {
		name  string
		input string
		want  string
		ok    bool
	}{
		{"trims whitespace", "  Sam  ", "Sam", true},
		{"empty rejected", "   ", "", false},
		{"exactly 80 runes ok", strings.Repeat("a", 80), strings.Repeat("a", 80), true},
		{"81 runes rejected", strings.Repeat("a", 81), "", false},
		{"control character rejected", "Sam\nSmith", "", false},
		{"tab rejected", "Sam\tSmith", "", false},
		{"unicode ok", "Søren Æ", "Søren Æ", true},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			got, ok := validateUserName(tc.input)
			if ok != tc.ok || got != tc.want {
				t.Errorf("validateUserName(%q) = (%q, %v), want (%q, %v)", tc.input, got, ok, tc.want, tc.ok)
			}
		})
	}
}

// requestAsSession builds a request carrying both the user id and session id
// that loadSession would place in context, needed for handlers that read
// sessionIDFrom (the "current" session flag, revoke-others).
func requestAsSession(method, path string, userID, sessionID uuid.UUID) *http.Request {
	req := httptest.NewRequest(method, path, nil)
	ctx := context.WithValue(req.Context(), ctxUserID, userID)
	ctx = context.WithValue(ctx, ctxSessionID, sessionID)
	return req.WithContext(ctx)
}

// accountTestFixture wires a *Server bound to the real pool (handlePatchMe,
// handleDeleteAccount etc. use s.q directly or s.inTx, which opens its own
// transaction from s.pool), plus two independent users with a couple of real
// sessions each. It cleans up every row it creates.
type accountTestFixture struct {
	s              *Server
	userA, userB   uuid.UUID
	emailA, emailB string
	sessA1, sessA2 uuid.UUID
	sessB1         uuid.UUID
}

func newAccountTestFixture(t *testing.T, pool *pgxpool.Pool, rdb *redis.Client) *accountTestFixture {
	t.Helper()
	ctx := context.Background()
	q := store.New(pool)

	newUser := func(label string) (uuid.UUID, string) {
		id := uuid.Must(uuid.NewV7())
		email := label + "-" + id.String() + "@example.invalid"
		if _, err := q.UpsertUserByEmail(ctx, store.UpsertUserByEmailParams{ID: id, Email: email}); err != nil {
			t.Fatalf("create %s user: %v", label, err)
		}
		return id, email
	}
	userA, emailA := newUser("account-a")
	userB, emailB := newUser("account-b")

	newSession := func(userID uuid.UUID, label string) uuid.UUID {
		hash := sha256.Sum256([]byte(label + "-" + userID.String()))
		row, err := q.CreateSession(ctx, store.CreateSessionParams{
			ID: uuid.Must(uuid.NewV7()), UserID: userID, TokenHash: hash[:],
			ExpiresAt: time.Now().Add(time.Hour), UserAgent: "test-agent",
		})
		if err != nil {
			t.Fatalf("create session: %v", err)
		}
		return row.ID
	}
	sessA1 := newSession(userA, "a1")
	sessA2 := newSession(userA, "a2")
	sessB1 := newSession(userB, "b1")

	t.Cleanup(func() {
		cleanupCtx := context.Background()
		if _, err := pool.Exec(cleanupCtx, "DELETE FROM users WHERE id = ANY($1)", []uuid.UUID{userA, userB}); err != nil {
			t.Logf("cleanup users: %v", err)
		}
	})

	s := &Server{
		pool:     pool,
		q:        q,
		rdb:      rdb,
		limiter:  ratelimit.New(rdb),
		sessions: auth.NewSessions(q, rdb, time.Hour),
		cfg:      config.Config{SiteURL: "https://seeuthere.at"},
	}
	return &accountTestFixture{
		s: s, userA: userA, userB: userB, emailA: emailA, emailB: emailB,
		sessA1: sessA1, sessA2: sessA2, sessB1: sessB1,
	}
}

// --- PATCH /v1/me ---

func TestHandlePatchMe(t *testing.T) {
	pool := dbTestPool(t)
	rdb := dbTestRedis(t)
	f := newAccountTestFixture(t, pool, rdb)

	t.Run("valid name saves", func(t *testing.T) {
		req := requestAs(http.MethodPatch, "/v1/me", f.userA)
		req.Body = newJSONBody(`{"name":"Sam Rivera"}`)
		req.Header.Set("Content-Type", "application/json")
		rec := httptest.NewRecorder()
		f.s.handlePatchMe(rec, req)
		if rec.Code != http.StatusOK {
			t.Fatalf("status = %d, want 200; body = %s", rec.Code, rec.Body.String())
		}
		if !strings.Contains(rec.Body.String(), "Sam Rivera") {
			t.Errorf("body = %s, want it to contain the new name", rec.Body.String())
		}
	})

	t.Run("empty name rejected", func(t *testing.T) {
		req := requestAs(http.MethodPatch, "/v1/me", f.userA)
		req.Body = newJSONBody(`{"name":"   "}`)
		req.Header.Set("Content-Type", "application/json")
		rec := httptest.NewRecorder()
		f.s.handlePatchMe(rec, req)
		if rec.Code != http.StatusBadRequest {
			t.Fatalf("status = %d, want 400; body = %s", rec.Code, rec.Body.String())
		}
	})

	t.Run("too-long name rejected", func(t *testing.T) {
		req := requestAs(http.MethodPatch, "/v1/me", f.userA)
		req.Body = newJSONBody(`{"name":"` + strings.Repeat("a", 81) + `"}`)
		req.Header.Set("Content-Type", "application/json")
		rec := httptest.NewRecorder()
		f.s.handlePatchMe(rec, req)
		if rec.Code != http.StatusBadRequest {
			t.Fatalf("status = %d, want 400; body = %s", rec.Code, rec.Body.String())
		}
	})
}

// --- GET /v1/me/sessions ---

func TestHandleListSessions(t *testing.T) {
	pool := dbTestPool(t)
	rdb := dbTestRedis(t)
	f := newAccountTestFixture(t, pool, rdb)

	req := requestAsSession(http.MethodGet, "/v1/me/sessions", f.userA, f.sessA1)
	rec := httptest.NewRecorder()
	f.s.handleListSessions(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200; body = %s", rec.Code, rec.Body.String())
	}
	body := rec.Body.String()
	if !strings.Contains(body, f.sessA1.String()) || !strings.Contains(body, f.sessA2.String()) {
		t.Fatalf("body missing expected session ids: %s", body)
	}
	if strings.Contains(body, f.sessB1.String()) {
		t.Fatalf("body leaked another user's session: %s", body)
	}
	// sessA1 is the "current" session for this request; the JSON encoder
	// preserves field order, so the specific session's object contains its
	// own "current":true immediately after its own id, not any other row's.
	idx := strings.Index(body, f.sessA1.String())
	end := idx + 300
	if end > len(body) {
		end = len(body)
	}
	if idx == -1 || !strings.Contains(body[idx:end], `"current":true`) {
		t.Errorf("expected current:true near sessA1 in body: %s", body)
	}
}

// --- DELETE /v1/me/sessions/{id} ---

func TestHandleDeleteSession_AuthzAndRevoke(t *testing.T) {
	pool := dbTestPool(t)
	rdb := dbTestRedis(t)
	f := newAccountTestFixture(t, pool, rdb)
	ctx := context.Background()

	t.Run("cannot revoke another user's session", func(t *testing.T) {
		req := requestAsSession(http.MethodDelete, "/v1/me/sessions/"+f.sessB1.String(), f.userA, f.sessA1)
		req = withRouteID(req, f.sessB1)
		rec := httptest.NewRecorder()
		f.s.handleDeleteSession(rec, req)
		if rec.Code != http.StatusNotFound {
			t.Fatalf("status = %d, want 404; body = %s", rec.Code, rec.Body.String())
		}
		// The other user's session must still exist afterward.
		rows, err := f.s.q.ListUserSessions(ctx, f.userB)
		if err != nil {
			t.Fatalf("list sessions: %v", err)
		}
		found := false
		for _, r := range rows {
			if r.ID == f.sessB1 {
				found = true
			}
		}
		if !found {
			t.Error("sessB1 was deleted by a request from userA")
		}
	})

	t.Run("owner can revoke their own session", func(t *testing.T) {
		req := requestAsSession(http.MethodDelete, "/v1/me/sessions/"+f.sessA2.String(), f.userA, f.sessA1)
		req = withRouteID(req, f.sessA2)
		rec := httptest.NewRecorder()
		f.s.handleDeleteSession(rec, req)
		if rec.Code != http.StatusNoContent {
			t.Fatalf("status = %d, want 204; body = %s", rec.Code, rec.Body.String())
		}
		rows, err := f.s.q.ListUserSessions(ctx, f.userA)
		if err != nil {
			t.Fatalf("list sessions: %v", err)
		}
		for _, r := range rows {
			if r.ID == f.sessA2 {
				t.Error("sessA2 still present after delete")
			}
		}
	})

	t.Run("unknown session id is a 404", func(t *testing.T) {
		req := requestAsSession(http.MethodDelete, "/v1/me/sessions/"+uuid.Must(uuid.NewV7()).String(), f.userA, f.sessA1)
		req = withRouteID(req, uuid.Must(uuid.NewV7()))
		rec := httptest.NewRecorder()
		f.s.handleDeleteSession(rec, req)
		if rec.Code != http.StatusNotFound {
			t.Fatalf("status = %d, want 404; body = %s", rec.Code, rec.Body.String())
		}
	})
}

// --- POST /v1/me/sessions/revoke-others ---

func TestHandleRevokeOtherSessions(t *testing.T) {
	pool := dbTestPool(t)
	rdb := dbTestRedis(t)
	f := newAccountTestFixture(t, pool, rdb)
	ctx := context.Background()

	req := requestAsSession(http.MethodPost, "/v1/me/sessions/revoke-others", f.userA, f.sessA1)
	rec := httptest.NewRecorder()
	f.s.handleRevokeOtherSessions(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200; body = %s", rec.Code, rec.Body.String())
	}
	if !strings.Contains(rec.Body.String(), `"revoked":1`) {
		t.Fatalf("body = %s, want revoked:1", rec.Body.String())
	}

	rows, err := f.s.q.ListUserSessions(ctx, f.userA)
	if err != nil {
		t.Fatalf("list sessions: %v", err)
	}
	if len(rows) != 1 || rows[0].ID != f.sessA1 {
		t.Fatalf("sessions after revoke-others = %+v, want only sessA1", rows)
	}

	// userB's session must be untouched.
	bRows, err := f.s.q.ListUserSessions(ctx, f.userB)
	if err != nil {
		t.Fatalf("list userB sessions: %v", err)
	}
	if len(bRows) != 1 || bRows[0].ID != f.sessB1 {
		t.Fatalf("userB sessions = %+v, want only sessB1 untouched", bRows)
	}
}

// --- DELETE /v1/me ---

func TestHandleDeleteAccount(t *testing.T) {
	pool := dbTestPool(t)
	rdb := dbTestRedis(t)
	ctx := context.Background()
	q := store.New(pool)

	t.Run("confirm mismatch", func(t *testing.T) {
		f := newAccountTestFixture(t, pool, rdb)
		req := requestAs(http.MethodDelete, "/v1/me", f.userA)
		req.Body = newJSONBody(`{"confirm_email":"wrong@example.invalid"}`)
		req.Header.Set("Content-Type", "application/json")
		rec := httptest.NewRecorder()
		f.s.handleDeleteAccount(rec, req)
		if rec.Code != http.StatusBadRequest {
			t.Fatalf("status = %d, want 400; body = %s", rec.Code, rec.Body.String())
		}
		if _, err := q.GetUserByID(ctx, f.userA); err != nil {
			t.Errorf("user should still exist after a mismatched confirmation: %v", err)
		}
	})

	t.Run("holding an admin role blocks deletion", func(t *testing.T) {
		f := newAccountTestFixture(t, pool, rdb)
		if _, err := q.GrantRole(ctx, store.GrantRoleParams{UserID: f.userA, Role: "support"}); err != nil {
			t.Fatalf("grant role: %v", err)
		}
		req := requestAs(http.MethodDelete, "/v1/me", f.userA)
		req.Body = newJSONBody(`{"confirm_email":"` + f.emailA + `"}`)
		req.Header.Set("Content-Type", "application/json")
		rec := httptest.NewRecorder()
		f.s.handleDeleteAccount(rec, req)
		if rec.Code != http.StatusConflict {
			t.Fatalf("status = %d, want 409; body = %s", rec.Code, rec.Body.String())
		}
		if _, err := q.GetUserByID(ctx, f.userA); err != nil {
			t.Errorf("user should still exist when blocked by an admin role: %v", err)
		}
		if _, err := q.RevokeRole(ctx, store.RevokeRoleParams{UserID: f.userA, Role: "support"}); err != nil {
			t.Fatalf("cleanup role: %v", err)
		}
	})

	t.Run("happy path scrubs PII, soft-deletes owned events, leaves co-hosted events alone", func(t *testing.T) {
		f := newAccountTestFixture(t, pool, rdb)

		owned := createTestEvent(t, ctx, q, "birthday", "classic", &f.userA)
		cohosted := createTestEvent(t, ctx, q, "birthday", "classic", &f.userB)
		t.Cleanup(func() {
			cleanupCtx := context.Background()
			if _, err := pool.Exec(cleanupCtx, "DELETE FROM events WHERE id = ANY($1)", []uuid.UUID{owned.ID, cohosted.ID}); err != nil {
				t.Logf("cleanup events: %v", err)
			}
		})
		if _, err := q.UpsertEventMember(ctx, store.UpsertEventMemberParams{
			MemberID: f.userA, Role: "editor", EventID: cohosted.ID, OwnerID: f.userB,
		}); err != nil {
			t.Fatalf("add co-host: %v", err)
		}

		mediaStore, err := media.NewStore(t.TempDir())
		if err != nil {
			t.Fatalf("media.NewStore: %v", err)
		}
		jobClient, err := jobs.NewClient(pool, noopSender{}, noopSender{}, q, mediaStore, f.s.tokens, f.s.limiter, "", f.s.cfg.SiteURL)
		if err != nil {
			t.Fatalf("jobs.NewClient: %v", err)
		}
		f.s.jobs = jobClient

		req := requestAs(http.MethodDelete, "/v1/me", f.userA)
		req.Body = newJSONBody(`{"confirm_email":"` + f.emailA + `"}`)
		req.Header.Set("Content-Type", "application/json")
		rec := httptest.NewRecorder()
		f.s.handleDeleteAccount(rec, req)
		if rec.Code != http.StatusNoContent {
			t.Fatalf("status = %d, want 204; body = %s", rec.Code, rec.Body.String())
		}

		// PII scrubbed, row soft-deleted: GetUserByID (deleted_at IS NULL) must miss.
		if _, err := q.GetUserByID(ctx, f.userA); err == nil {
			t.Error("deleted user should not be visible via GetUserByID")
		}

		// All sessions gone.
		rows, err := q.ListUserSessions(ctx, f.userA)
		if err != nil {
			t.Fatalf("list sessions: %v", err)
		}
		if len(rows) != 0 {
			t.Errorf("sessions after delete = %+v, want none", rows)
		}

		// The owned event was soft-deleted.
		var deletedAt *time.Time
		if err := pool.QueryRow(ctx, "SELECT deleted_at FROM events WHERE id = $1", owned.ID).Scan(&deletedAt); err != nil {
			t.Fatalf("query owned event: %v", err)
		}
		if deletedAt == nil {
			t.Error("owned event should be soft-deleted")
		}

		// The co-hosted event (owned by userB) is untouched: still live...
		var cohostedDeletedAt *time.Time
		if err := pool.QueryRow(ctx, "SELECT deleted_at FROM events WHERE id = $1", cohosted.ID).Scan(&cohostedDeletedAt); err != nil {
			t.Fatalf("query cohosted event: %v", err)
		}
		if cohostedDeletedAt != nil {
			t.Error("co-hosted event should not be touched by the co-host's deletion")
		}
		// ...and userA's membership row on it is gone.
		members, err := q.ListEventMembers(ctx, store.ListEventMembersParams{EventID: cohosted.ID, UserID: f.userB})
		if err != nil {
			t.Fatalf("list event members: %v", err)
		}
		for _, m := range members {
			if m.UserID == f.userA {
				t.Error("userA's membership on the co-hosted event should have been removed")
			}
		}

		// A media_visibility job was enqueued for the owned event.
		var n int
		if err := pool.QueryRow(ctx,
			"SELECT count(*) FROM river_job WHERE kind = 'media_visibility' AND args->>'event_id' = $1",
			owned.ID.String(),
		).Scan(&n); err != nil {
			t.Fatalf("count media_visibility jobs: %v", err)
		}
		if n != 1 {
			t.Errorf("media_visibility jobs for owned event = %d, want 1", n)
		}
	})
}
