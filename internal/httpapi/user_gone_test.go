package httpapi

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/ansonarose/seeyouthere-backend/internal/store"
)

// newDisposableUser creates a live user and removes it (and anything it owns)
// when the test ends.
func newDisposableUser(t *testing.T, pool *pgxpool.Pool, label string) uuid.UUID {
	t.Helper()
	ctx := context.Background()
	id := uuid.Must(uuid.NewV7())
	if _, err := store.New(pool).UpsertUserByEmail(ctx, store.UpsertUserByEmailParams{
		ID: id, Email: label + "-" + id.String() + "@example.invalid",
	}); err != nil {
		t.Fatalf("create %s user: %v", label, err)
	}
	t.Cleanup(func() {
		_, _ = pool.Exec(context.Background(), "DELETE FROM events WHERE owner_id = $1", id)
		_, _ = pool.Exec(context.Background(), "DELETE FROM users WHERE id = $1", id)
	})
	return id
}

// makeUserGone puts the user in the given unusable state.
func makeUserGone(t *testing.T, pool *pgxpool.Pool, q *store.Queries, id uuid.UUID, state string) {
	t.Helper()
	ctx := context.Background()
	switch state {
	case "deleted":
		if err := q.SoftDeleteUser(ctx, id); err != nil {
			t.Fatalf("soft delete user: %v", err)
		}
	case "banned":
		if _, err := pool.Exec(ctx, "UPDATE users SET status = 'banned' WHERE id = $1", id); err != nil {
			t.Fatalf("ban user: %v", err)
		}
	default:
		t.Fatalf("unknown state %q", state)
	}
}

func ownedEventCount(t *testing.T, pool *pgxpool.Pool, id uuid.UUID) int {
	t.Helper()
	var n int
	if err := pool.QueryRow(context.Background(), "SELECT count(*) FROM events WHERE owner_id = $1", id).Scan(&n); err != nil {
		t.Fatalf("count owned events: %v", err)
	}
	return n
}

func createEventRequest(userID uuid.UUID) *http.Request {
	req := httptest.NewRequest(http.MethodPost, "/", newJSONBody(`{"occasion_slug":"birthday","answers":{"name":"Sam"}}`))
	req.Header.Set("Content-Type", "application/json")
	return req.WithContext(contextWithUser(req.Context(), userID))
}

// blockedThenDeleted holds an in-flight admin delete's row lock on the user,
// starts fn, proves fn waits for the lock, then commits the delete.
func blockedThenDeleted(t *testing.T, pool *pgxpool.Pool, q *store.Queries, userID uuid.UUID, fn func()) {
	t.Helper()
	ctx := context.Background()
	tx, err := pool.Begin(ctx)
	if err != nil {
		t.Fatalf("begin: %v", err)
	}
	defer tx.Rollback(ctx) //nolint:errcheck
	qtx := q.WithTx(tx)
	if _, err := qtx.GetUserForAdminDelete(ctx, userID); err != nil {
		t.Fatalf("lock user for delete: %v", err)
	}
	done := make(chan struct{})
	go func() {
		defer close(done)
		fn()
	}()
	select {
	case <-done:
		t.Fatal("request finished while the admin delete held the user lock")
	case <-time.After(300 * time.Millisecond):
	}
	if err := qtx.SoftDeleteUser(ctx, userID); err != nil {
		t.Fatalf("soft delete: %v", err)
	}
	if err := tx.Commit(ctx); err != nil {
		t.Fatalf("commit delete: %v", err)
	}
	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("request still blocked after the delete committed")
	}
}

func TestHandleCreateEvent_GoneUserCreatesNothing(t *testing.T) {
	pool := dbTestPool(t)
	rdb := dbTestRedis(t)
	f := newEventTestFixture(t, pool, rdb)

	for _, state := range []string{"deleted", "banned"} {
		t.Run(state, func(t *testing.T) {
			id := newDisposableUser(t, pool, "create-"+state)
			makeUserGone(t, pool, f.s.q, id, state)
			rec := httptest.NewRecorder()
			f.s.handleCreateEvent(rec, createEventRequest(id))
			wantStatusCode(t, rec, http.StatusUnauthorized, "unauthorized")
			if n := ownedEventCount(t, pool, id); n != 0 {
				t.Fatalf("gone user owns %d events", n)
			}
		})
	}

	t.Run("live user still creates", func(t *testing.T) {
		id := newDisposableUser(t, pool, "create-live")
		rec := httptest.NewRecorder()
		f.s.handleCreateEvent(rec, createEventRequest(id))
		wantStatusCode(t, rec, http.StatusCreated, "")
		if n := ownedEventCount(t, pool, id); n != 1 {
			t.Fatalf("owned events = %d, want 1", n)
		}
	})
}

func TestHandleCreateEvent_RacingAdminDeleteCreatesNothing(t *testing.T) {
	pool := dbTestPool(t)
	rdb := dbTestRedis(t)
	f := newEventTestFixture(t, pool, rdb)
	id := newDisposableUser(t, pool, "create-race")

	rec := httptest.NewRecorder()
	blockedThenDeleted(t, pool, f.s.q, id, func() { f.s.handleCreateEvent(rec, createEventRequest(id)) })
	wantStatusCode(t, rec, http.StatusUnauthorized, "unauthorized")
	if n := ownedEventCount(t, pool, id); n != 0 {
		t.Fatalf("deleted user owns %d events", n)
	}
}

func TestClaimAnonDrafts_GoneUserClaimsNothing(t *testing.T) {
	pool := dbTestPool(t)
	rdb := dbTestRedis(t)
	f := newEventTestFixture(t, pool, rdb)
	ctx := context.Background()

	newDraft := func(t *testing.T, cookie []byte) uuid.UUID {
		t.Helper()
		draft := createTestEvent(t, ctx, f.s.q, "birthday", "classic", nil)
		t.Cleanup(func() { _, _ = pool.Exec(context.Background(), "DELETE FROM events WHERE id = $1", draft.ID) })
		if err := f.s.q.CreateAnonDraft(ctx, store.CreateAnonDraftParams{
			ID: uuid.Must(uuid.NewV7()), CookieHash: cookie, EventID: draft.ID, ExpiresAt: time.Now().Add(time.Hour),
		}); err != nil {
			t.Fatalf("create anon draft: %v", err)
		}
		return draft.ID
	}
	assertUnclaimed := func(t *testing.T, draftID uuid.UUID) {
		t.Helper()
		var owner *uuid.UUID
		if err := pool.QueryRow(ctx, "SELECT owner_id FROM events WHERE id = $1", draftID).Scan(&owner); err != nil || owner != nil {
			t.Fatalf("draft owner = %v, %v; want unclaimed", owner, err)
		}
		var n int
		if err := pool.QueryRow(ctx, "SELECT count(*) FROM anon_drafts WHERE event_id = $1", draftID).Scan(&n); err != nil || n != 1 {
			t.Fatalf("anon_drafts rows = %d, %v; want 1 (claim rolled back)", n, err)
		}
	}

	for _, state := range []string{"deleted", "banned"} {
		t.Run(state, func(t *testing.T) {
			id := newDisposableUser(t, pool, "claim-"+state)
			cookie := []byte("claim-gone-" + state + "-cookie-0123456789")
			draftID := newDraft(t, cookie)
			makeUserGone(t, pool, f.s.q, id, state)
			if _, err := f.s.claimAnonDrafts(ctx, id, cookie); !errors.Is(err, errUserGone) {
				t.Fatalf("claim error = %v, want errUserGone", err)
			}
			assertUnclaimed(t, draftID)
		})
	}

	t.Run("racing admin delete", func(t *testing.T) {
		id := newDisposableUser(t, pool, "claim-race")
		cookie := []byte("claim-race-cookie-0123456789abc")
		draftID := newDraft(t, cookie)
		var claimErr error
		blockedThenDeleted(t, pool, f.s.q, id, func() { _, claimErr = f.s.claimAnonDrafts(ctx, id, cookie) })
		if !errors.Is(claimErr, errUserGone) {
			t.Fatalf("claim error = %v, want errUserGone", claimErr)
		}
		assertUnclaimed(t, draftID)
	})
}

func TestHandleAddMember_GoneTargetAddsNothing(t *testing.T) {
	pool := dbTestPool(t)
	rdb := dbTestRedis(t)
	f := newEventTestFixture(t, pool, rdb)
	withGuestFixtureJobs(t, f, pool)

	// A banned account is not a valid co-host, and nothing is attached to it.
	target := newDisposableUser(t, pool, "member-banned")
	makeUserGone(t, pool, f.s.q, target, "banned")
	row, err := f.s.q.GetUserByID(context.Background(), target)
	if err != nil {
		t.Fatalf("get target: %v", err)
	}

	req := httptest.NewRequest(http.MethodPost, "/", newJSONBody(`{"email":"`+row.Email+`","role":"viewer"}`))
	req.Header.Set("Content-Type", "application/json")
	req = req.WithContext(contextWithUser(req.Context(), f.ownerID))
	req = withRouteID(req, f.eventID)
	rec := httptest.NewRecorder()
	f.s.handleAddMember(rec, req)
	wantStatusCode(t, rec, http.StatusNotFound, "not_found")

	var n int
	if err := pool.QueryRow(context.Background(), "SELECT count(*) FROM event_members WHERE user_id = $1", target).Scan(&n); err != nil || n != 0 {
		t.Fatalf("memberships for the gone target = %d, %v; want 0", n, err)
	}
}
