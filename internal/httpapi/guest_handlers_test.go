package httpapi

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/ansonarose/seeyouthere-backend/internal/jobs"
	"github.com/ansonarose/seeyouthere-backend/internal/media"
	"github.com/ansonarose/seeyouthere-backend/internal/store"
	"github.com/ansonarose/seeyouthere-backend/internal/token"
)

// withGuestFixtureJobs equips f.s with a token.Keys and a real (unstarted)
// River client, needed by any test that builds an invite_url or enqueues
// send_invite/notify_cohost_added (InsertTx panics on an unregistered kind
// with a nil s.jobs).
func withGuestFixtureJobs(t *testing.T, f eventTestFixture, pool *pgxpool.Pool) {
	t.Helper()
	keys, err := token.NewKeys(make([]byte, 32))
	if err != nil {
		t.Fatalf("token.NewKeys: %v", err)
	}
	f.s.tokens = keys

	mediaStore, err := media.NewStore(t.TempDir())
	if err != nil {
		t.Fatalf("media.NewStore: %v", err)
	}
	jobClient, err := jobs.NewClient(pool, noopSender{}, noopSender{}, f.s.q, mediaStore, keys, f.s.limiter, "", f.s.cfg.SiteURL)
	if err != nil {
		t.Fatalf("jobs.NewClient: %v", err)
	}
	f.s.jobs = jobClient
}

func newGuestBody(t *testing.T, name, email, phone string, householdSize int) *http.Request {
	t.Helper()
	body := `{"name":"` + name + `","household_size":` + itoa(householdSize) + `}`
	if email != "" || phone != "" {
		body = `{"name":"` + name + `","email":` + jsonStringOrNull(email) + `,"phone":` + jsonStringOrNull(phone) + `,"household_size":` + itoa(householdSize) + `}`
	}
	req := httptest.NewRequest(http.MethodPost, "/", newJSONBody(body))
	req.Header.Set("Content-Type", "application/json")
	return req
}

func jsonStringOrNull(s string) string {
	if s == "" {
		return "null"
	}
	return `"` + s + `"`
}

func itoa(n int) string {
	if n == 0 {
		return "0"
	}
	neg := n < 0
	if neg {
		n = -n
	}
	var b []byte
	for n > 0 {
		b = append([]byte{byte('0' + n%10)}, b...)
		n /= 10
	}
	if neg {
		return "-" + string(b)
	}
	return string(b)
}

func TestHandleCreateGuest_RoleAndValidation(t *testing.T) {
	pool := dbTestPool(t)
	rdb := dbTestRedis(t)
	f := newEventTestFixture(t, pool, rdb)
	withGuestFixtureJobs(t, f, pool)

	t.Run("viewer forbidden", func(t *testing.T) {
		req := newGuestBody(t, "Ann", "", "", 1)
		req = req.WithContext(contextWithUser(req.Context(), f.viewerID))
		req = withRouteID(req, f.eventID)
		rec := httptest.NewRecorder()
		f.s.handleCreateGuest(rec, req)
		if rec.Code != http.StatusForbidden {
			t.Fatalf("status = %d, want 403; body = %s", rec.Code, rec.Body.String())
		}
	})

	t.Run("stranger not found", func(t *testing.T) {
		req := newGuestBody(t, "Ann", "", "", 1)
		req = req.WithContext(contextWithUser(req.Context(), f.strangerID))
		req = withRouteID(req, f.eventID)
		rec := httptest.NewRecorder()
		f.s.handleCreateGuest(rec, req)
		if rec.Code != http.StatusNotFound {
			t.Fatalf("status = %d, want 404; body = %s", rec.Code, rec.Body.String())
		}
	})

	t.Run("owner invalid household size", func(t *testing.T) {
		req := newGuestBody(t, "Ann", "", "", 0)
		req = req.WithContext(contextWithUser(req.Context(), f.ownerID))
		req = withRouteID(req, f.eventID)
		rec := httptest.NewRecorder()
		f.s.handleCreateGuest(rec, req)
		if rec.Code != http.StatusBadRequest {
			t.Fatalf("status = %d, want 400; body = %s", rec.Code, rec.Body.String())
		}
	})

	t.Run("owner invalid email", func(t *testing.T) {
		req := newGuestBody(t, "Ann", "not-an-email", "", 1)
		req = req.WithContext(contextWithUser(req.Context(), f.ownerID))
		req = withRouteID(req, f.eventID)
		rec := httptest.NewRecorder()
		f.s.handleCreateGuest(rec, req)
		if rec.Code != http.StatusBadRequest {
			t.Fatalf("status = %d, want 400; body = %s", rec.Code, rec.Body.String())
		}
	})

	t.Run("editor creates guest with email and gets invite_url", func(t *testing.T) {
		email := "ann-" + f.eventID.String()[:8] + "@example.invalid"
		req := newGuestBody(t, "Ann", email, "+1 (555) 123-4567", 2)
		req = req.WithContext(contextWithUser(req.Context(), f.editorID))
		req = withRouteID(req, f.eventID)
		rec := httptest.NewRecorder()
		f.s.handleCreateGuest(rec, req)
		if rec.Code != http.StatusCreated {
			t.Fatalf("status = %d, want 201; body = %s", rec.Code, rec.Body.String())
		}
		body := rec.Body.String()
		if !containsAll(body, `"name":"Ann"`, `"household_size":2`) {
			t.Errorf("body = %s, want name/household_size", body)
		}
		// The event has no slug yet, so invite_url must stay null even for
		// an editor.
		if !containsAll(body, `"invite_url":null`) {
			t.Errorf("body = %s, want invite_url null without a slug", body)
		}
		t.Cleanup(func() {
			if _, err := pool.Exec(context.Background(), "DELETE FROM guests WHERE event_id = $1", f.eventID); err != nil {
				t.Logf("cleanup guests: %v", err)
			}
		})
	})

	t.Run("duplicate email conflicts", func(t *testing.T) {
		email := "dup-" + f.eventID.String()[:8] + "@example.invalid"
		req1 := newGuestBody(t, "Bea", email, "", 1)
		req1 = req1.WithContext(contextWithUser(req1.Context(), f.ownerID))
		req1 = withRouteID(req1, f.eventID)
		rec1 := httptest.NewRecorder()
		f.s.handleCreateGuest(rec1, req1)
		if rec1.Code != http.StatusCreated {
			t.Fatalf("first create status = %d; body = %s", rec1.Code, rec1.Body.String())
		}

		req2 := newGuestBody(t, "Bea Two", email, "", 1)
		req2 = req2.WithContext(contextWithUser(req2.Context(), f.ownerID))
		req2 = withRouteID(req2, f.eventID)
		rec2 := httptest.NewRecorder()
		f.s.handleCreateGuest(rec2, req2)
		if rec2.Code != http.StatusConflict {
			t.Fatalf("status = %d, want 409 guest_exists; body = %s", rec2.Code, rec2.Body.String())
		}
		if !containsAll(rec2.Body.String(), `"guest_exists"`) {
			t.Errorf("body = %s, want guest_exists", rec2.Body.String())
		}
	})
}

func TestHandleUpdateGuest_ClearEmailVsOmit(t *testing.T) {
	pool := dbTestPool(t)
	rdb := dbTestRedis(t)
	f := newEventTestFixture(t, pool, rdb)
	withGuestFixtureJobs(t, f, pool)
	ctx := context.Background()
	q := store.New(pool)

	email := "clear-" + f.eventID.String()[:8] + "@example.invalid"
	created, err := q.CreateGuest(ctx, store.CreateGuestParams{
		ID: uuid.Must(uuid.NewV7()), EventID: f.eventID, Name: "Cara", Email: &email, HouseholdSize: 1, UserID: f.ownerID,
	})
	if err != nil {
		t.Fatalf("create guest: %v", err)
	}
	t.Cleanup(func() {
		if _, err := pool.Exec(context.Background(), "DELETE FROM guests WHERE id = $1", created.ID); err != nil {
			t.Logf("cleanup guest: %v", err)
		}
	})

	// Omitting email/phone must leave them unchanged.
	req := httptest.NewRequest(http.MethodPatch, "/", newJSONBody(`{"name":"Cara Lee"}`))
	req.Header.Set("Content-Type", "application/json")
	req = req.WithContext(contextWithUser(req.Context(), f.ownerID))
	req = withRouteID(req, f.eventID)
	req = withRouteParam(req, "guestID", created.ID.String())
	rec := httptest.NewRecorder()
	f.s.handleUpdateGuest(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200; body = %s", rec.Code, rec.Body.String())
	}
	if !containsAll(rec.Body.String(), `"name":"Cara Lee"`, `"email":"`+email+`"`) {
		t.Errorf("body = %s, want unchanged email", rec.Body.String())
	}

	// An explicit null must clear it.
	req = httptest.NewRequest(http.MethodPatch, "/", newJSONBody(`{"email":null}`))
	req.Header.Set("Content-Type", "application/json")
	req = req.WithContext(contextWithUser(req.Context(), f.ownerID))
	req = withRouteID(req, f.eventID)
	req = withRouteParam(req, "guestID", created.ID.String())
	rec = httptest.NewRecorder()
	f.s.handleUpdateGuest(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200; body = %s", rec.Code, rec.Body.String())
	}
	if !containsAll(rec.Body.String(), `"email":null`) {
		t.Errorf("body = %s, want cleared email", rec.Body.String())
	}
}

func TestHandleDeleteGuest_IDORMatrix(t *testing.T) {
	pool := dbTestPool(t)
	rdb := dbTestRedis(t)
	f := newEventTestFixture(t, pool, rdb)
	ctx := context.Background()
	q := store.New(pool)

	newGuest := func() uuid.UUID {
		g, err := q.CreateGuest(ctx, store.CreateGuestParams{
			ID: uuid.Must(uuid.NewV7()), EventID: f.eventID, Name: "Del", HouseholdSize: 1, UserID: f.ownerID,
		})
		if err != nil {
			t.Fatalf("create guest: %v", err)
		}
		return g.ID
	}

	for _, c := range []struct {
		name       string
		userID     uuid.UUID
		wantStatus int
	}{
		{"viewer forbidden", f.viewerID, http.StatusForbidden},
		{"stranger not found", f.strangerID, http.StatusNotFound},
		{"owner ok", f.ownerID, http.StatusNoContent},
	} {
		t.Run(c.name, func(t *testing.T) {
			guestID := newGuest()
			t.Cleanup(func() {
				pool.Exec(context.Background(), "DELETE FROM guests WHERE id = $1", guestID)
			})
			req := httptest.NewRequest(http.MethodDelete, "/", nil)
			req = req.WithContext(contextWithUser(req.Context(), c.userID))
			req = withRouteID(req, f.eventID)
			req = withRouteParam(req, "guestID", guestID.String())
			rec := httptest.NewRecorder()
			f.s.handleDeleteGuest(rec, req)
			if rec.Code != c.wantStatus {
				t.Fatalf("status = %d, want %d; body = %s", rec.Code, c.wantStatus, rec.Body.String())
			}
		})
	}
}

func TestHandleRotateGuestToken_ChangesLink(t *testing.T) {
	pool := dbTestPool(t)
	rdb := dbTestRedis(t)
	f := newEventTestFixture(t, pool, rdb)
	withGuestFixtureJobs(t, f, pool)
	ctx := context.Background()
	q := store.New(pool)

	// Give the event a slug so invite_url is populated.
	slug := "rotate-" + f.eventID.String()[:8]
	if _, err := q.UpdateEventSettings(ctx, store.UpdateEventSettingsParams{
		Slug: &slug, EventID: f.eventID, UserID: f.ownerID, Version: 1,
	}); err != nil {
		t.Fatalf("set slug: %v", err)
	}

	g, err := q.CreateGuest(ctx, store.CreateGuestParams{
		ID: uuid.Must(uuid.NewV7()), EventID: f.eventID, Name: "Rot", HouseholdSize: 1, UserID: f.ownerID,
	})
	if err != nil {
		t.Fatalf("create guest: %v", err)
	}
	t.Cleanup(func() { pool.Exec(context.Background(), "DELETE FROM guests WHERE id = $1", g.ID) })

	rows, err := f.s.q.ListGuests(ctx, store.ListGuestsParams{EventID: f.eventID, UserID: f.ownerID, Lim: 25})
	if err != nil {
		t.Fatalf("list guests: %v", err)
	}
	firstLink := f.s.tokens.GuestToken(g.ID, rows[0].TokenVersion)

	req := httptest.NewRequest(http.MethodPost, "/", nil)
	req = req.WithContext(contextWithUser(req.Context(), f.ownerID))
	req = withRouteID(req, f.eventID)
	req = withRouteParam(req, "guestID", g.ID.String())
	rec := httptest.NewRecorder()
	f.s.handleRotateGuestToken(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200; body = %s", rec.Code, rec.Body.String())
	}
	if strings.Contains(rec.Body.String(), firstLink) {
		t.Errorf("body still contains the pre-rotation link: %s", rec.Body.String())
	}
	if !containsAll(rec.Body.String(), `"invite_url":"https://seeuthere.at/`+slug+`/invite#`) {
		t.Errorf("body = %s, want a fresh invite_url", rec.Body.String())
	}
}

func TestHandleImportGuestsCSV_Cases(t *testing.T) {
	pool := dbTestPool(t)
	rdb := dbTestRedis(t)
	f := newEventTestFixture(t, pool, rdb)
	withGuestFixtureJobs(t, f, pool)

	newImportReq := func(body string) *http.Request {
		req := httptest.NewRequest(http.MethodPost, "/", newJSONBody(body))
		req.Header.Set("Content-Type", "text/csv")
		req = req.WithContext(contextWithUser(req.Context(), f.ownerID))
		req = withRouteID(req, f.eventID)
		return req
	}

	t.Run("missing name column", func(t *testing.T) {
		rec := httptest.NewRecorder()
		f.s.handleImportGuests(rec, newImportReq("email,phone\n"))
		if rec.Code != http.StatusUnprocessableEntity {
			t.Fatalf("status = %d, want 422; body = %s", rec.Code, rec.Body.String())
		}
		if !containsAll(rec.Body.String(), `"invalid_csv"`, `"missing_column"`) {
			t.Errorf("body = %s", rec.Body.String())
		}
	})

	t.Run("bad row reports field error, nothing written", func(t *testing.T) {
		email := "csvbad-" + f.eventID.String()[:8] + "@example.invalid"
		csv := "name,email\nGood One," + email + "\nBad Two,not-an-email\n"
		rec := httptest.NewRecorder()
		f.s.handleImportGuests(rec, newImportReq(csv))
		if rec.Code != http.StatusUnprocessableEntity {
			t.Fatalf("status = %d, want 422; body = %s", rec.Code, rec.Body.String())
		}
		if !containsAll(rec.Body.String(), `"invalid_csv"`, `"row":3`, `"field":"email"`) {
			t.Errorf("body = %s", rec.Body.String())
		}
		var n int
		if err := pool.QueryRow(context.Background(), "SELECT count(*) FROM guests WHERE event_id = $1", f.eventID).Scan(&n); err != nil {
			t.Fatalf("count guests: %v", err)
		}
		if n != 0 {
			t.Errorf("guests inserted = %d, want 0 (whole import must fail atomically)", n)
		}
	})

	t.Run("too many rows rejected", func(t *testing.T) {
		var b strings.Builder
		b.WriteString("name\n")
		for i := 0; i < maxGuestCSVDataRows+1; i++ {
			b.WriteString("Guest\n")
		}
		rec := httptest.NewRecorder()
		f.s.handleImportGuests(rec, newImportReq(b.String()))
		if rec.Code != http.StatusUnprocessableEntity {
			t.Fatalf("status = %d, want 422; body = %s", rec.Code, rec.Body.String())
		}
		if !containsAll(rec.Body.String(), `"too_many_rows"`) {
			t.Errorf("body = %s", rec.Body.String())
		}
	})

	t.Run("valid csv imports and skips existing", func(t *testing.T) {
		email := "csvok-" + f.eventID.String()[:8] + "@example.invalid"
		csv := "name,email,household_size\nGood One," + email + ",2\nGood Two,,1\n"
		rec := httptest.NewRecorder()
		f.s.handleImportGuests(rec, newImportReq(csv))
		if rec.Code != http.StatusOK {
			t.Fatalf("status = %d, want 200; body = %s", rec.Code, rec.Body.String())
		}
		if !containsAll(rec.Body.String(), `"imported":2`, `"skipped_existing":0`) {
			t.Errorf("body = %s", rec.Body.String())
		}

		// Re-importing the same email is skipped, not an error.
		rec2 := httptest.NewRecorder()
		f.s.handleImportGuests(rec2, newImportReq(csv))
		if rec2.Code != http.StatusOK {
			t.Fatalf("status = %d, want 200; body = %s", rec2.Code, rec2.Body.String())
		}
		if !containsAll(rec2.Body.String(), `"imported":1`, `"skipped_existing":1`) {
			t.Errorf("second import body = %s, want the emailed row skipped", rec2.Body.String())
		}
		t.Cleanup(func() {
			pool.Exec(context.Background(), "DELETE FROM guests WHERE event_id = $1", f.eventID)
		})
	})
}

func TestHandleSendInvites_EventNotPublished(t *testing.T) {
	pool := dbTestPool(t)
	rdb := dbTestRedis(t)
	f := newEventTestFixture(t, pool, rdb)
	withGuestFixtureJobs(t, f, pool)

	req := httptest.NewRequest(http.MethodPost, "/", newJSONBody(`{"all_uninvited":true}`))
	req.Header.Set("Content-Type", "application/json")
	req = req.WithContext(contextWithUser(req.Context(), f.ownerID))
	req = withRouteID(req, f.eventID)
	rec := httptest.NewRecorder()
	f.s.handleSendInvites(rec, req)
	if rec.Code != http.StatusConflict {
		t.Fatalf("status = %d, want 409; body = %s", rec.Code, rec.Body.String())
	}
	if !containsAll(rec.Body.String(), `"event_not_published"`) {
		t.Errorf("body = %s", rec.Body.String())
	}
}

func TestHandleSendInvites_QueuesJobsForGuestsWithEmail(t *testing.T) {
	pool := dbTestPool(t)
	rdb := dbTestRedis(t)
	f := newEventTestFixture(t, pool, rdb)
	withGuestFixtureJobs(t, f, pool)
	// The fixture's cfg has no MailDailyInviteCap (zero value), which would
	// make the global send-invites cap reject every request; give it a
	// generous cap so this test exercises the queueing logic, not the cap.
	// The global counter's key is shared across test runs (unlike the
	// per-user/per-event keys, which are scoped by this run's fresh ids), so
	// clear it first to keep the test idempotent under repeated runs.
	f.s.cfg.MailDailyInviteCap = 1000
	rdb.Del(context.Background(), "rl:invites:send:global")
	ctx := context.Background()
	q := store.New(pool)

	// Publish the event directly (bypassing the full publish-readiness flow,
	// which isn't what this test is about).
	slug := "invites-" + f.eventID.String()[:8]
	if _, err := pool.Exec(ctx,
		"UPDATE events SET status = 'published', slug = $1, published_at = now(), title = 'Test', starts_at = now() WHERE id = $2",
		slug, f.eventID,
	); err != nil {
		t.Fatalf("publish event: %v", err)
	}

	withEmail, err := q.CreateGuest(ctx, store.CreateGuestParams{
		ID: uuid.Must(uuid.NewV7()), EventID: f.eventID, Name: "Has Email",
		Email: ptr("invite-" + f.eventID.String()[:8] + "@example.invalid"), HouseholdSize: 1, UserID: f.ownerID,
	})
	if err != nil {
		t.Fatalf("create guest with email: %v", err)
	}
	noEmail, err := q.CreateGuest(ctx, store.CreateGuestParams{
		ID: uuid.Must(uuid.NewV7()), EventID: f.eventID, Name: "No Email", HouseholdSize: 1, UserID: f.ownerID,
	})
	if err != nil {
		t.Fatalf("create guest without email: %v", err)
	}
	t.Cleanup(func() {
		pool.Exec(context.Background(), "DELETE FROM guests WHERE event_id = $1", f.eventID)
	})

	req := httptest.NewRequest(http.MethodPost, "/", newJSONBody(`{"all_uninvited":true}`))
	req.Header.Set("Content-Type", "application/json")
	req = req.WithContext(contextWithUser(req.Context(), f.ownerID))
	req = withRouteID(req, f.eventID)
	rec := httptest.NewRecorder()
	f.s.handleSendInvites(rec, req)
	if rec.Code != http.StatusAccepted {
		t.Fatalf("status = %d, want 202; body = %s", rec.Code, rec.Body.String())
	}
	if !containsAll(rec.Body.String(), `"queued":1`) {
		t.Errorf("body = %s, want queued 1 (only the guest with an email)", rec.Body.String())
	}

	var jobCount int
	if err := pool.QueryRow(ctx,
		"SELECT count(*) FROM river_job WHERE kind = 'send_invite' AND args->>'guest_id' = $1",
		withEmail.ID.String(),
	).Scan(&jobCount); err != nil {
		t.Fatalf("count send_invite jobs: %v", err)
	}
	if jobCount != 1 {
		t.Errorf("send_invite jobs for %s = %d, want 1", withEmail.ID, jobCount)
	}

	var noEmailJobs int
	if err := pool.QueryRow(ctx,
		"SELECT count(*) FROM river_job WHERE kind = 'send_invite' AND args->>'guest_id' = $1",
		noEmail.ID.String(),
	).Scan(&noEmailJobs); err != nil {
		t.Fatalf("count send_invite jobs: %v", err)
	}
	if noEmailJobs != 0 {
		t.Errorf("send_invite jobs for the emailless guest = %d, want 0", noEmailJobs)
	}
}

func ptr(s string) *string { return &s }

// contextWithUser is requestAs's context construction, exposed for tests
// that need to attach a user id to a request built another way (a non-nil
// body, a route param set separately, etc.).
func contextWithUser(ctx context.Context, userID uuid.UUID) context.Context {
	if userID == uuid.Nil {
		return ctx
	}
	return context.WithValue(ctx, ctxUserID, userID)
}
