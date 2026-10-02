package httpapi

import (
	"context"
	"crypto/sha256"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/go-chi/chi/v5"
	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/redis/go-redis/v9"

	"github.com/ansonarose/seeyouthere-backend/internal/config"
	"github.com/ansonarose/seeyouthere-backend/internal/content"
	"github.com/ansonarose/seeyouthere-backend/internal/jobs"
	mailpkg "github.com/ansonarose/seeyouthere-backend/internal/mail"
	"github.com/ansonarose/seeyouthere-backend/internal/media"
	"github.com/ansonarose/seeyouthere-backend/internal/ratelimit"
	"github.com/ansonarose/seeyouthere-backend/internal/store"
)

// noopSender discards mail; the DELETE tests below build a real, unstarted
// river.Client (needed so InsertTx recognises the media_visibility job kind)
// but never run its workers.
type noopSender struct{}

func (noopSender) Send(context.Context, mailpkg.Message) error { return nil }

// dbTestRedis opens a Valkey client for rate-limited handlers under test. The
// dev stack (`docker compose up -d --wait`) is required for these tests
// anyway (dbTestPool needs DATABASE_URL from the same stack), so this fails
// loudly rather than skipping.
func dbTestRedis(t *testing.T) *redis.Client {
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
		t.Fatalf("ping valkey: %v", err)
	}
	t.Cleanup(func() { _ = rdb.Close() })
	return rdb
}

// eventTestFixture wires a *Server bound to the real pool (needed because
// several handlers under test call s.inTx, which opens its own transaction
// from s.pool and so can't run inside an outer test transaction), plus an
// owner user, an editor, a viewer, an unrelated stranger, and one draft event
// owned by the owner. It cleans up every row it creates.
type eventTestFixture struct {
	s                           *Server
	ownerID, editorID, viewerID uuid.UUID
	strangerID                  uuid.UUID
	eventID                     uuid.UUID
}

func newEventTestFixture(t *testing.T, pool *pgxpool.Pool, rdb *redis.Client) eventTestFixture {
	t.Helper()
	ctx := context.Background()
	q := store.New(pool)

	newUser := func(label string) uuid.UUID {
		id := uuid.Must(uuid.NewV7())
		if _, err := q.UpsertUserByEmail(ctx, store.UpsertUserByEmailParams{
			ID: id, Email: label + "-" + id.String() + "@example.invalid",
		}); err != nil {
			t.Fatalf("create %s user: %v", label, err)
		}
		return id
	}

	owner := newUser("owner")
	editor := newUser("editor")
	viewer := newUser("viewer")
	stranger := newUser("stranger")

	event := createTestEvent(t, ctx, q, "birthday", "classic", &owner)

	if _, err := q.UpsertEventMember(ctx, store.UpsertEventMemberParams{
		MemberID: editor, Role: "editor", EventID: event.ID, OwnerID: owner,
	}); err != nil {
		t.Fatalf("add editor: %v", err)
	}
	if _, err := q.UpsertEventMember(ctx, store.UpsertEventMemberParams{
		MemberID: viewer, Role: "viewer", EventID: event.ID, OwnerID: owner,
	}); err != nil {
		t.Fatalf("add viewer: %v", err)
	}

	t.Cleanup(func() {
		cleanupCtx := context.Background()
		// The event first: event_members and anon_drafts cascade off it, so
		// the user deletes below never hit a lingering reference.
		if _, err := pool.Exec(cleanupCtx, "DELETE FROM events WHERE id = $1", event.ID); err != nil {
			t.Logf("cleanup event %s: %v", event.ID, err)
		}
		for _, id := range []uuid.UUID{owner, editor, viewer, stranger} {
			if _, err := pool.Exec(cleanupCtx, "DELETE FROM users WHERE id = $1", id); err != nil {
				t.Logf("cleanup user %s: %v", id, err)
			}
		}
	})

	s := &Server{
		pool:    pool,
		q:       q,
		rdb:     rdb,
		limiter: ratelimit.New(rdb),
		cfg:     config.Config{SiteURL: "https://seeyouthere.at"},
	}
	return eventTestFixture{s: s, ownerID: owner, editorID: editor, viewerID: viewer, strangerID: stranger, eventID: event.ID}
}

// createTestEvent builds a draft event for occasionSlug/templateSlug, owned
// by ownerID (nil for an anonymous draft, matching CreateEvent's contract).
func createTestEvent(t *testing.T, ctx context.Context, q *store.Queries, occasionSlug, templateSlug string, ownerID *uuid.UUID) store.Event {
	t.Helper()
	occRow, err := q.GetOccasion(ctx, occasionSlug)
	if err != nil {
		t.Fatalf("get occasion %s: %v", occasionSlug, err)
	}
	occ, err := content.ParseOccasion(occRow)
	if err != nil {
		t.Fatalf("parse occasion %s: %v", occasionSlug, err)
	}
	tmpl, err := q.GetPublishedTemplateBySlug(ctx, store.GetPublishedTemplateBySlugParams{Slug: templateSlug, AllowPremium: false})
	if err != nil {
		t.Fatalf("get template %s: %v", templateSlug, err)
	}
	// birthday's setup question "name" is required (title_template
	// "{name}'s birthday"); other seeded occasions have their own required
	// answers, so this helper is only exercised with occasionSlug "birthday".
	built, err := content.BuildInitialContent(occ, map[string]string{"name": "Sam"})
	if err != nil {
		t.Fatalf("build initial content: %v", err)
	}
	saved, err := content.ParseStored(built, occ)
	if err != nil {
		t.Fatalf("parse stored: %v", err)
	}
	event, err := q.CreateEvent(ctx, store.CreateEventParams{
		ID: uuid.Must(uuid.NewV7()), OwnerID: ownerID, OccasionSlug: occasionSlug, Title: saved.Title,
		Content: saved.JSON, Overrides: []byte("{}"), StartsAt: saved.StartsAt,
		TemplateID: tmpl.ID, TemplateVersion: tmpl.Version,
	})
	if err != nil {
		t.Fatalf("create event: %v", err)
	}
	return event
}

func requestAs(method, path string, userID uuid.UUID) *http.Request {
	req := httptest.NewRequest(method, path, nil)
	if userID != uuid.Nil {
		req = req.WithContext(context.WithValue(req.Context(), ctxUserID, userID))
	}
	return req
}

// withRouteParam attaches a chi URL param (as chi.URLParam reads it) to req.
// It reuses any *chi.Context already attached (adding to its params) rather
// than replacing it, so chained calls like
// withRouteParam(withRouteParam(req, "id", ...), "guestID", ...) both remain
// visible to the handler: chi.URLParam looks up exactly one RouteCtxKey
// value, and a fresh chi.NewRouteContext() on every call would silently
// discard whichever param was set first.
func withRouteParam(r *http.Request, name, value string) *http.Request {
	rctx, ok := r.Context().Value(chi.RouteCtxKey).(*chi.Context)
	if !ok || rctx == nil {
		rctx = chi.NewRouteContext()
		r = r.WithContext(context.WithValue(r.Context(), chi.RouteCtxKey, rctx))
	}
	rctx.URLParams.Add(name, value)
	return r
}

func withRouteID(r *http.Request, id uuid.UUID) *http.Request {
	return withRouteParam(r, "id", id.String())
}

func newJSONBody(body string) io.ReadCloser {
	return io.NopCloser(strings.NewReader(body))
}

func containsAll(s string, subs ...string) bool {
	for _, sub := range subs {
		if !strings.Contains(s, sub) {
			return false
		}
	}
	return true
}

func TestListActiveOccasions_SeededCatalogParses(t *testing.T) {
	pool := dbTestPool(t)
	ctx := context.Background()
	q := store.New(pool)

	rows, err := q.ListActiveOccasions(ctx)
	if err != nil {
		t.Fatalf("list occasions: %v", err)
	}
	if len(rows) == 0 {
		t.Fatal("expected seeded occasions, got none (did the seed migration run?)")
	}
	for _, row := range rows {
		occ, err := content.ParseOccasion(row)
		if err != nil {
			t.Errorf("ParseOccasion(%s): %v", row.Slug, err)
			continue
		}
		if occ.Copy.TitleTemplate == "" {
			t.Errorf("occasion %s: empty title_template", row.Slug)
		}
	}

	tmplRows, err := q.ListPublishedTemplates(ctx, nil)
	if err != nil {
		t.Fatalf("list templates: %v", err)
	}
	if len(tmplRows) == 0 {
		t.Fatal("expected seeded templates, got none")
	}
	for _, row := range tmplRows {
		if _, err := content.ValidateManifest(row.Manifest); err != nil {
			t.Errorf("ValidateManifest(%s): %v", row.Slug, err)
		}
	}
}

func TestHandleGetEvent_IDORMatrix(t *testing.T) {
	pool := dbTestPool(t)
	rdb := dbTestRedis(t)
	f := newEventTestFixture(t, pool, rdb)

	cases := []struct {
		name       string
		userID     uuid.UUID
		wantStatus int
	}{
		{"owner", f.ownerID, http.StatusOK},
		{"editor", f.editorID, http.StatusOK},
		{"viewer", f.viewerID, http.StatusOK},
		{"stranger", f.strangerID, http.StatusNotFound},
		{"anon without cookie", uuid.Nil, http.StatusNotFound},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			req := requestAs(http.MethodGet, "/", c.userID)
			req = withRouteID(req, f.eventID)
			rec := httptest.NewRecorder()
			f.s.handleGetEvent(rec, req)
			if rec.Code != c.wantStatus {
				t.Fatalf("status = %d, want %d; body = %s", rec.Code, c.wantStatus, rec.Body.String())
			}
		})
	}
}

func TestHandlePatchEvent_VersionConflict(t *testing.T) {
	pool := dbTestPool(t)
	rdb := dbTestRedis(t)
	f := newEventTestFixture(t, pool, rdb)

	req := requestAs(http.MethodPatch, "/", f.ownerID)
	req = withRouteID(req, f.eventID)
	req.Header.Set("Content-Type", "application/json")
	req.Body = newJSONBody(`{"version": 999}`)
	rec := httptest.NewRecorder()
	f.s.handlePatchEvent(rec, req)
	if rec.Code != http.StatusConflict {
		t.Fatalf("status = %d, want 409; body = %s", rec.Code, rec.Body.String())
	}
	if !containsAll(rec.Body.String(), `"version_conflict"`, `"current_version"`) {
		t.Errorf("body = %s, want version_conflict with current_version", rec.Body.String())
	}
}

func TestHandlePatchEvent_ViewerForbidden(t *testing.T) {
	pool := dbTestPool(t)
	rdb := dbTestRedis(t)
	f := newEventTestFixture(t, pool, rdb)

	req := requestAs(http.MethodPatch, "/", f.viewerID)
	req = withRouteID(req, f.eventID)
	req.Header.Set("Content-Type", "application/json")
	req.Body = newJSONBody(`{"version": 1}`)
	rec := httptest.NewRecorder()
	f.s.handlePatchEvent(rec, req)
	if rec.Code != http.StatusForbidden {
		t.Fatalf("status = %d, want 403; body = %s", rec.Code, rec.Body.String())
	}
}

func TestHandlePublishEvent_NotReadyThenSucceeds(t *testing.T) {
	pool := dbTestPool(t)
	rdb := dbTestRedis(t)
	ctx := context.Background()
	q := store.New(pool)
	f := newEventTestFixture(t, pool, rdb)

	// No slug/title/datetime yet: not ready.
	req := requestAs(http.MethodPost, "/", f.ownerID)
	req = withRouteID(req, f.eventID)
	req.Header.Set("Content-Type", "application/json")
	req.Body = newJSONBody(`{"version": 1}`)
	rec := httptest.NewRecorder()
	f.s.handlePublishEvent(rec, req)
	if rec.Code != http.StatusConflict {
		t.Fatalf("status = %d, want 409 not_ready; body = %s", rec.Code, rec.Body.String())
	}
	if !containsAll(rec.Body.String(), `"not_ready"`, `"slug"`) {
		t.Errorf("body = %s, want not_ready listing missing slug", rec.Body.String())
	}

	// Give the event a title + datetime via a valid content save, then a slug.
	occRow, err := q.GetOccasion(ctx, "birthday")
	if err != nil {
		t.Fatalf("get occasion: %v", err)
	}
	occ, err := content.ParseOccasion(occRow)
	if err != nil {
		t.Fatalf("parse occasion: %v", err)
	}
	readyContent := `[{"id":"hero1","type":"hero","title":"Sam's Party","subtitle":""},` +
		`{"id":"dt1","type":"datetime","heading":"","start_local":"2027-06-01T18:00","end_local":"","timezone":"America/New_York","all_day":false}]`
	saved, err := content.ValidateContent([]byte(readyContent), occ)
	if err != nil {
		t.Fatalf("validate content: %v", err)
	}
	if _, err := q.UpdateEventContent(ctx, store.UpdateEventContentParams{
		Content: saved.JSON, Title: saved.Title, StartsAt: saved.StartsAt,
		EventID: f.eventID, Version: 1, UserID: f.ownerID,
	}); err != nil {
		t.Fatalf("update content: %v", err)
	}
	slug := "sams-party-" + f.eventID.String()[:8]
	if _, err := q.UpdateEventSettings(ctx, store.UpdateEventSettingsParams{
		Slug: &slug, EventID: f.eventID, UserID: f.ownerID, Version: 2,
	}); err != nil {
		t.Fatalf("set slug: %v", err)
	}

	req = requestAs(http.MethodPost, "/", f.ownerID)
	req = withRouteID(req, f.eventID)
	req.Header.Set("Content-Type", "application/json")
	req.Body = newJSONBody(`{"version": 3}`)
	rec = httptest.NewRecorder()
	f.s.handlePublishEvent(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200; body = %s", rec.Code, rec.Body.String())
	}
	if !containsAll(rec.Body.String(), `"status":"published"`) {
		t.Errorf("body = %s, want status published", rec.Body.String())
	}
}

func TestHandleCheckSlug_Cases(t *testing.T) {
	pool := dbTestPool(t)
	rdb := dbTestRedis(t)
	ctx := context.Background()
	tx, err := pool.Begin(ctx)
	if err != nil {
		t.Fatalf("begin: %v", err)
	}
	t.Cleanup(func() { _ = tx.Rollback(ctx) })
	q := store.New(tx)

	userID := uuid.Must(uuid.NewV7())
	if _, err := q.UpsertUserByEmail(ctx, store.UpsertUserByEmailParams{
		ID: userID, Email: "slugcheck-" + userID.String() + "@example.invalid",
	}); err != nil {
		t.Fatalf("create user: %v", err)
	}
	s := &Server{q: q, rdb: rdb, limiter: ratelimit.New(rdb)}

	cases := []struct {
		name   string
		slug   string
		reason string
	}{
		{"invalid", "a", "invalid"},
		{"blocked reserved word", "admin", "blocked"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			req := requestAs(http.MethodGet, "/", userID)
			req = withRouteParam(req, "slug", c.slug)
			rec := httptest.NewRecorder()
			s.handleCheckSlug(rec, req)
			if rec.Code != http.StatusOK {
				t.Fatalf("status = %d, want 200; body = %s", rec.Code, rec.Body.String())
			}
			if !containsAll(rec.Body.String(), `"available":false`, `"reason":"`+c.reason+`"`) {
				t.Errorf("body = %s, want reason %s", rec.Body.String(), c.reason)
			}
		})
	}

	req := requestAs(http.MethodGet, "/", userID)
	req = withRouteParam(req, "slug", "totally-fresh-slug-"+userID.String()[:8])
	rec := httptest.NewRecorder()
	s.handleCheckSlug(rec, req)
	if rec.Code != http.StatusOK || !containsAll(rec.Body.String(), `"available":true`) {
		t.Fatalf("status = %d, body = %s, want available true", rec.Code, rec.Body.String())
	}
}

func TestClaimAnonDrafts(t *testing.T) {
	pool := dbTestPool(t)
	ctx := context.Background()
	q := store.New(pool)

	userID := uuid.Must(uuid.NewV7())
	if _, err := q.UpsertUserByEmail(ctx, store.UpsertUserByEmailParams{
		ID: userID, Email: "claim-" + userID.String() + "@example.invalid",
	}); err != nil {
		t.Fatalf("create user: %v", err)
	}

	draft := createTestEvent(t, ctx, q, "birthday", "classic", nil)
	t.Cleanup(func() {
		cleanupCtx := context.Background()
		if _, err := pool.Exec(cleanupCtx, "DELETE FROM events WHERE id = $1", draft.ID); err != nil {
			t.Logf("cleanup event %s: %v", draft.ID, err)
		}
		if _, err := pool.Exec(cleanupCtx, "DELETE FROM users WHERE id = $1", userID); err != nil {
			t.Logf("cleanup user %s: %v", userID, err)
		}
	})

	cookieHash := []byte("test-cookie-hash-0123456789abcd")
	if err := q.CreateAnonDraft(ctx, store.CreateAnonDraftParams{
		ID: uuid.Must(uuid.NewV7()), CookieHash: cookieHash, EventID: draft.ID, ExpiresAt: time.Now().Add(24 * time.Hour),
	}); err != nil {
		t.Fatalf("create anon draft: %v", err)
	}

	s := &Server{q: q, pool: pool}
	claimed, err := s.claimAnonDrafts(ctx, userID, cookieHash)
	if err != nil {
		t.Fatalf("claimAnonDrafts: %v", err)
	}
	if len(claimed) != 1 || claimed[0] != draft.ID {
		t.Fatalf("claimed = %v, want [%v]", claimed, draft.ID)
	}

	got, err := q.GetEventForUser(ctx, store.GetEventForUserParams{UserID: userID, EventID: draft.ID})
	if err != nil {
		t.Fatalf("get claimed event: %v", err)
	}
	if got.Role != "owner" || got.Event.OwnerID == nil || *got.Event.OwnerID != userID {
		t.Fatalf("claimed event not owned by claiming user: %+v", got)
	}

	// A second claim with the same cookie finds nothing left to claim.
	claimedAgain, err := s.claimAnonDrafts(ctx, userID, cookieHash)
	if err != nil {
		t.Fatalf("claimAnonDrafts (second): %v", err)
	}
	if len(claimedAgain) != 0 {
		t.Errorf("second claim = %v, want empty", claimedAgain)
	}
}

func TestHandleDeleteEvent_NonOwnerNotFound(t *testing.T) {
	pool := dbTestPool(t)
	rdb := dbTestRedis(t)
	f := newEventTestFixture(t, pool, rdb)

	// SoftDeleteEvent's owner_id filter rejects the editor and the stranger
	// before the handler ever reaches the media_visibility job insert, so a
	// nil jobs client on the fixture is fine for both cases: DELETE only
	// checks real ownership (404, not 403, matching the plan's IDOR policy).
	for _, userID := range []uuid.UUID{f.editorID, f.strangerID} {
		req := requestAs(http.MethodDelete, "/", userID)
		req = withRouteID(req, f.eventID)
		rec := httptest.NewRecorder()
		f.s.handleDeleteEvent(rec, req)
		if rec.Code != http.StatusNotFound {
			t.Fatalf("user %s: status = %d, want 404; body = %s", userID, rec.Code, rec.Body.String())
		}
	}
}

// TestHandleDeleteEvent_EnqueuesMediaVisibilityJob covers the regression
// where every DELETE 500'd because InsertTx enqueued a media_visibility job
// with no worker registered for that kind (an UnknownJobKindError rolls
// back the whole transaction). Both an owner-delete and an anonymous-draft
// delete must succeed and leave exactly one media_visibility job behind.
func TestHandleDeleteEvent_EnqueuesMediaVisibilityJob(t *testing.T) {
	pool := dbTestPool(t)
	rdb := dbTestRedis(t)
	f := newEventTestFixture(t, pool, rdb)
	ctx := context.Background()
	q := store.New(pool)

	mediaStore, err := media.NewStore(t.TempDir())
	if err != nil {
		t.Fatalf("media.NewStore: %v", err)
	}
	jobClient, err := jobs.NewClient(pool, noopSender{}, noopSender{}, q, mediaStore, f.s.tokens, f.s.limiter, "", f.s.cfg.SiteURL)
	if err != nil {
		t.Fatalf("jobs.NewClient: %v", err)
	}
	f.s.jobs = jobClient

	countMediaJobs := func(eventID uuid.UUID) int {
		var n int
		if err := pool.QueryRow(ctx,
			"SELECT count(*) FROM river_job WHERE kind = 'media_visibility' AND args->>'event_id' = $1",
			eventID.String(),
		).Scan(&n); err != nil {
			t.Fatalf("count media_visibility jobs: %v", err)
		}
		return n
	}

	t.Run("owner delete", func(t *testing.T) {
		req := requestAs(http.MethodDelete, "/", f.ownerID)
		req = withRouteID(req, f.eventID)
		rec := httptest.NewRecorder()
		f.s.handleDeleteEvent(rec, req)
		if rec.Code != http.StatusNoContent {
			t.Fatalf("status = %d, want 204; body = %s", rec.Code, rec.Body.String())
		}
		if n := countMediaJobs(f.eventID); n != 1 {
			t.Errorf("media_visibility jobs for %s = %d, want 1", f.eventID, n)
		}
	})

	t.Run("anonymous draft delete", func(t *testing.T) {
		draft := createTestEvent(t, ctx, q, "birthday", "classic", nil)
		t.Cleanup(func() {
			if _, err := pool.Exec(context.Background(), "DELETE FROM events WHERE id = $1", draft.ID); err != nil {
				t.Logf("cleanup draft %s: %v", draft.ID, err)
			}
		})
		token, err := randomToken()
		if err != nil {
			t.Fatalf("randomToken: %v", err)
		}
		sum := sha256.Sum256([]byte(token))
		if err := q.CreateAnonDraft(ctx, store.CreateAnonDraftParams{
			ID: uuid.Must(uuid.NewV7()), CookieHash: sum[:], EventID: draft.ID, ExpiresAt: time.Now().Add(anonDraftTTL),
		}); err != nil {
			t.Fatalf("create anon draft: %v", err)
		}

		req := httptest.NewRequest(http.MethodDelete, "/", nil)
		req.AddCookie(&http.Cookie{Name: draftCookieName, Value: token})
		req = withRouteID(req, draft.ID)
		rec := httptest.NewRecorder()
		f.s.handleDeleteEvent(rec, req)
		if rec.Code != http.StatusNoContent {
			t.Fatalf("status = %d, want 204; body = %s", rec.Code, rec.Body.String())
		}
		if n := countMediaJobs(draft.ID); n != 1 {
			t.Errorf("media_visibility jobs for %s = %d, want 1", draft.ID, n)
		}
	})
}
