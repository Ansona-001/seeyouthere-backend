package httpapi

import (
	"context"
	"encoding/csv"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/redis/go-redis/v9"

	"github.com/ansonarose/seeyouthere-backend/internal/content"
	"github.com/ansonarose/seeyouthere-backend/internal/store"
)

// rsvpContentJSON builds a minimal content document (hero + datetime + rsvp
// block) with a configurable capacity, deadline and max party size, reusing
// testEventContentJSON's shape but with the knobs the RSVP-write tests need.
func rsvpContentJSON(capacity *int, deadlineLocal string, maxPartySize int) string {
	cap := "null"
	if capacity != nil {
		cap = fmt.Sprintf("%d", *capacity)
	}
	return fmt.Sprintf(`[
		{"id":"hero1","type":"hero","title":"Sam's Party","subtitle":""},
		{"id":"dt1","type":"datetime","heading":"","start_local":"2027-06-01T18:00","end_local":"","timezone":"America/New_York","all_day":false},
		{"id":"rsvp1","type":"rsvp","heading":"RSVP","body":"","deadline_local":%q,"capacity":%s,"max_party_size":%d,"fields":[{"key":"dietary","required":false}],"questions":[]}
	]`, deadlineLocal, cap, maxPartySize)
}

// setRSVPContent overwrites f's event content with rsvpContentJSON's shape,
// bumping the event's version (the fixture leaves it at 4: create, content
// update, settings update, publish). Returns the new version for chained
// calls within the same test.
func (f *publicEventFixture) setRSVPContent(t *testing.T, capacity *int, deadlineLocal string, maxPartySize int, version int32) int32 {
	t.Helper()
	ctx := context.Background()
	occRow, err := f.s.q.GetOccasion(ctx, "birthday")
	if err != nil {
		t.Fatalf("get occasion: %v", err)
	}
	occ, err := content.ParseOccasion(occRow)
	if err != nil {
		t.Fatalf("parse occasion: %v", err)
	}
	saved, err := content.ValidateContent([]byte(rsvpContentJSON(capacity, deadlineLocal, maxPartySize)), occ)
	if err != nil {
		t.Fatalf("validate content: %v", err)
	}
	row, err := f.s.q.UpdateEventContent(ctx, store.UpdateEventContentParams{
		Content: saved.JSON, Title: saved.Title, StartsAt: saved.StartsAt, EndsAt: saved.EffectiveEnd(),
		EventID: f.eventID, Version: version, UserID: f.ownerID,
	})
	if err != nil {
		t.Fatalf("update content: %v", err)
	}
	return row.Version
}

func putRSVPRequest(ip, slug, body string) *http.Request {
	req := requestWithIP(http.MethodPut, "/", ip, []byte(body))
	req.Header.Set("Content-Type", "application/json")
	return withSlugParam(req, slug)
}

// countRiverJobsByEvent counts jobs of kind whose args carry event_id
// (notify_rsvps).
func countRiverJobsByEvent(t *testing.T, pool *pgxpool.Pool, kind string, eventID uuid.UUID) int {
	t.Helper()
	var n int
	if err := pool.QueryRow(context.Background(),
		"SELECT count(*) FROM river_job WHERE kind = $1 AND args->>'event_id' = $2",
		kind, eventID.String(),
	).Scan(&n); err != nil {
		t.Fatalf("count %s jobs: %v", kind, err)
	}
	return n
}

// countRiverJobsForEventRSVPs counts jobs of kind whose args carry rsvp_id,
// for any RSVP row belonging to eventID (send_rsvp_confirmation).
func countRiverJobsForEventRSVPs(t *testing.T, pool *pgxpool.Pool, kind string, eventID uuid.UUID) int {
	t.Helper()
	var n int
	if err := pool.QueryRow(context.Background(),
		`SELECT count(*) FROM river_job j
		 WHERE j.kind = $1 AND (j.args->>'rsvp_id')::uuid IN (SELECT id FROM rsvps WHERE event_id = $2)`,
		kind, eventID,
	).Scan(&n); err != nil {
		t.Fatalf("count %s jobs: %v", kind, err)
	}
	return n
}

// --- PUT /v1/public/events/{slug}/rsvp ---

// giveRSVPConfirmMailBudget gives f's server a generous
// MailDailyInviteCap (send_rsvp_confirmation's global cap shares its scale,
// rsvp_handlers.go) and clears the global counter's key, which — like
// invites:send:global (see TestHandleSendInvites_QueuesJobsForGuestsWithEmail)
// — is shared across the whole test binary run rather than scoped to this
// fixture's ids, so a previous test's usage would otherwise leak in.
func giveRSVPConfirmMailBudget(t *testing.T, f *publicEventFixture, rdb *redis.Client) {
	t.Helper()
	f.s.cfg.MailDailyInviteCap = 1000
	today := time.Now().UTC().Format("2006-01-02")
	rdb.Del(context.Background(), "rl:rsvp_confirm:global:"+today)
}

func TestHandlePutPublicRSVP_NewOpenRSVP(t *testing.T) {
	pool := dbTestPool(t)
	rdb := dbTestRedis(t)
	f := newPublicEventFixture(t, pool, rdb, publicEventOpts{})
	f.withJobs(t)
	giveRSVPConfirmMailBudget(t, f, rdb)

	req := putRSVPRequest(nextTestIP(), f.slug, `{"name":"Jordan","email":"jordan@example.invalid","attending":"yes","count":2,"answers":{"dietary":"none"},"website":""}`)
	rec := httptest.NewRecorder()
	f.s.handlePutPublicRSVP(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200; body = %s", rec.Code, rec.Body.String())
	}
	if !containsAll(rec.Body.String(), `"name":"Jordan"`, `"count":2`) {
		t.Errorf("body = %s", rec.Body.String())
	}
	cookies := rec.Result().Cookies()
	found := false
	for _, c := range cookies {
		if c.Name == eventCookieName(rsvpCookiePrefix, f.eventID) {
			found = true
		}
	}
	if !found {
		t.Error("expected a syt_rsvp_ cookie on a new open RSVP")
	}
	if n := countRiverJobsForEventRSVPs(t, pool, "send_rsvp_confirmation", f.eventID); n != 1 {
		t.Errorf("send_rsvp_confirmation jobs = %d, want 1", n)
	}
	if n := countRiverJobsByEvent(t, pool, "notify_rsvps", f.eventID); n != 1 {
		t.Errorf("notify_rsvps jobs = %d, want 1 (event's notify_rsvps defaults to on)", n)
	}
}

func TestHandlePutPublicRSVP_Honeypot(t *testing.T) {
	pool := dbTestPool(t)
	rdb := dbTestRedis(t)
	f := newPublicEventFixture(t, pool, rdb, publicEventOpts{})
	f.withJobs(t)

	req := putRSVPRequest(nextTestIP(), f.slug, `{"name":"Bot","attending":"yes","count":1,"answers":{},"website":"http://spam.example"}`)
	rec := httptest.NewRecorder()
	f.s.handlePutPublicRSVP(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200; body = %s", rec.Code, rec.Body.String())
	}
	var n int
	if err := pool.QueryRow(context.Background(), "SELECT count(*) FROM rsvps WHERE event_id = $1", f.eventID).Scan(&n); err != nil {
		t.Fatalf("count rsvps: %v", err)
	}
	if n != 0 {
		t.Errorf("rsvp rows = %d, want 0 (honeypot must write nothing)", n)
	}
}

func TestHandlePutPublicRSVP_InviteOnlyWithoutCookieIsForbidden(t *testing.T) {
	pool := dbTestPool(t)
	rdb := dbTestRedis(t)
	f := newPublicEventFixture(t, pool, rdb, publicEventOpts{rsvpMode: "invite_only"})
	f.withJobs(t)

	req := putRSVPRequest(nextTestIP(), f.slug, `{"name":"Nobody","attending":"yes","count":1,"answers":{},"website":""}`)
	rec := httptest.NewRecorder()
	f.s.handlePutPublicRSVP(rec, req)
	if rec.Code != http.StatusForbidden || !containsAll(rec.Body.String(), `"invite_required"`) {
		t.Fatalf("status = %d, body = %s, want 403 invite_required", rec.Code, rec.Body.String())
	}
}

func TestHandlePutPublicRSVP_GuestIdentityUpsertAndHouseholdCap(t *testing.T) {
	pool := dbTestPool(t)
	rdb := dbTestRedis(t)
	f := newPublicEventFixture(t, pool, rdb, publicEventOpts{rsvpMode: "invite_only"})
	f.withJobs(t)
	_, inviteToken := f.testGuest(t, "The Smiths", 3)

	withCookie := func(body string) *httptest.ResponseRecorder {
		req := putRSVPRequest(nextTestIP(), f.slug, body)
		req.AddCookie(&http.Cookie{Name: eventCookieName(inviteCookiePrefix, f.eventID), Value: inviteToken})
		rec := httptest.NewRecorder()
		f.s.handlePutPublicRSVP(rec, req)
		return rec
	}

	t.Run("count over household_size is party_too_large", func(t *testing.T) {
		rec := withCookie(`{"name":"Smith","attending":"yes","count":4,"answers":{},"website":""}`)
		if rec.Code != http.StatusBadRequest || !containsAll(rec.Body.String(), `"party_too_large"`) {
			t.Fatalf("status = %d, body = %s, want 400 party_too_large", rec.Code, rec.Body.String())
		}
	})

	t.Run("create then update upserts the same row", func(t *testing.T) {
		rec := withCookie(`{"name":"Smith Family","attending":"yes","count":3,"answers":{"dietary":"vegetarian"},"website":""}`)
		if rec.Code != http.StatusOK {
			t.Fatalf("create: status = %d, body = %s", rec.Code, rec.Body.String())
		}
		var firstID string
		if !extractJSONString(t, rec.Body.String(), "id", &firstID) {
			t.Fatal("missing id in response")
		}

		rec = withCookie(`{"name":"Smith Family","attending":"maybe","count":2,"answers":{},"website":""}`)
		if rec.Code != http.StatusOK {
			t.Fatalf("update: status = %d, body = %s", rec.Code, rec.Body.String())
		}
		var secondID string
		extractJSONString(t, rec.Body.String(), "id", &secondID)
		if firstID != secondID {
			t.Errorf("update should reuse the same rsvp row: first=%s second=%s", firstID, secondID)
		}

		var count int
		if err := pool.QueryRow(context.Background(), "SELECT count(*) FROM rsvps WHERE event_id = $1", f.eventID).Scan(&count); err != nil {
			t.Fatalf("count rsvps: %v", err)
		}
		if count != 1 {
			t.Errorf("rsvp rows = %d, want 1 (upsert, not a second row)", count)
		}
	})
}

// TestHandlePutPublicRSVP_EventMailCapSkipsEnqueueButSavesRSVP covers the
// per-event send_rsvp_confirmation cap (rsvp_handlers.go): an open event
// with no invite gate can otherwise be used to mail arbitrary addresses at
// volume, well past what any legitimate event needs. Once the event's daily
// confirmation budget is exhausted, a new RSVP must still be written — the
// mail cap is never allowed to fail the request — but no confirmation job
// gets enqueued.
func TestHandlePutPublicRSVP_EventMailCapSkipsEnqueueButSavesRSVP(t *testing.T) {
	pool := dbTestPool(t)
	rdb := dbTestRedis(t)
	f := newPublicEventFixture(t, pool, rdb, publicEventOpts{})
	f.withJobs(t)
	giveRSVPConfirmMailBudget(t, f, rdb) // generous global cap; this test is about the per-event one

	ctx := context.Background()
	today := time.Now().UTC().Format("2006-01-02")
	eventKey := "rsvp_confirm:event:" + f.eventID.String() + ":" + today
	if ok, err := f.s.limiter.AllowN(ctx, eventKey, maxRSVPConfirmationsPerEventPerDay, maxRSVPConfirmationsPerEventPerDay, 24*time.Hour); err != nil || !ok {
		t.Fatalf("pre-fill event cap: ok=%v err=%v", ok, err)
	}

	req := putRSVPRequest(nextTestIP(), f.slug, `{"name":"Jordan","email":"jordan@example.invalid","attending":"yes","count":1,"answers":{},"website":""}`)
	rec := httptest.NewRecorder()
	f.s.handlePutPublicRSVP(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200; body = %s", rec.Code, rec.Body.String())
	}

	var n int
	if err := pool.QueryRow(ctx, "SELECT count(*) FROM rsvps WHERE event_id = $1", f.eventID).Scan(&n); err != nil {
		t.Fatalf("count rsvps: %v", err)
	}
	if n != 1 {
		t.Errorf("rsvp rows = %d, want 1 (the write must succeed despite the mail cap)", n)
	}
	if n := countRiverJobsForEventRSVPs(t, pool, "send_rsvp_confirmation", f.eventID); n != 0 {
		t.Errorf("send_rsvp_confirmation jobs = %d, want 0 (event mail cap exhausted)", n)
	}
}

// TestHandlePutPublicRSVP_GlobalMailCapSkipsEnqueueButSavesRSVP covers the
// global send_rsvp_confirmation cap, shared in scale with
// MAIL_DAILY_INVITE_CAP: once it's exhausted, further RSVPs across any event
// still save but stop enqueueing confirmation emails.
func TestHandlePutPublicRSVP_GlobalMailCapSkipsEnqueueButSavesRSVP(t *testing.T) {
	pool := dbTestPool(t)
	rdb := dbTestRedis(t)
	f := newPublicEventFixture(t, pool, rdb, publicEventOpts{})
	f.withJobs(t)

	f.s.cfg.MailDailyInviteCap = 1
	today := time.Now().UTC().Format("2006-01-02")
	rdb.Del(context.Background(), "rl:rsvp_confirm:global:"+today)

	rec := httptest.NewRecorder()
	f.s.handlePutPublicRSVP(rec, putRSVPRequest(nextTestIP(), f.slug, `{"name":"First","email":"first@example.invalid","attending":"yes","count":1,"answers":{},"website":""}`))
	if rec.Code != http.StatusOK {
		t.Fatalf("first: status = %d, body = %s", rec.Code, rec.Body.String())
	}
	if n := countRiverJobsForEventRSVPs(t, pool, "send_rsvp_confirmation", f.eventID); n != 1 {
		t.Fatalf("send_rsvp_confirmation jobs after first = %d, want 1", n)
	}

	rec = httptest.NewRecorder()
	f.s.handlePutPublicRSVP(rec, putRSVPRequest(nextTestIP(), f.slug, `{"name":"Second","email":"second@example.invalid","attending":"yes","count":1,"answers":{},"website":""}`))
	if rec.Code != http.StatusOK {
		t.Fatalf("second: status = %d, body = %s", rec.Code, rec.Body.String())
	}

	var n int
	if err := pool.QueryRow(context.Background(), "SELECT count(*) FROM rsvps WHERE event_id = $1", f.eventID).Scan(&n); err != nil {
		t.Fatalf("count rsvps: %v", err)
	}
	if n != 2 {
		t.Errorf("rsvp rows = %d, want 2 (both writes must succeed despite the mail cap)", n)
	}
	if n := countRiverJobsForEventRSVPs(t, pool, "send_rsvp_confirmation", f.eventID); n != 1 {
		t.Errorf("send_rsvp_confirmation jobs = %d, want 1 (global mail cap exhausted after the first)", n)
	}
}

func TestHandlePutPublicRSVP_UnknownAnswerFieldIsValidationFailed(t *testing.T) {
	pool := dbTestPool(t)
	rdb := dbTestRedis(t)
	f := newPublicEventFixture(t, pool, rdb, publicEventOpts{})
	f.withJobs(t)

	req := putRSVPRequest(nextTestIP(), f.slug, `{"name":"Jordan","attending":"yes","count":1,"answers":{"not_a_real_field":"x"},"website":""}`)
	rec := httptest.NewRecorder()
	f.s.handlePutPublicRSVP(rec, req)
	if rec.Code != http.StatusBadRequest || !containsAll(rec.Body.String(), `"validation_failed"`) {
		t.Fatalf("status = %d, body = %s, want 400 validation_failed", rec.Code, rec.Body.String())
	}
}

func TestHandlePutPublicRSVP_ClosedStateCapacityAndLoweringException(t *testing.T) {
	pool := dbTestPool(t)
	rdb := dbTestRedis(t)
	f := newPublicEventFixture(t, pool, rdb, publicEventOpts{})
	f.withJobs(t)
	f.setRSVPContent(t, intPtr(1), "", 4, 4)

	// First "yes" of 1 fills the only spot; capture its syt_rsvp_ cookie so
	// later requests in this test edit the same row rather than creating a
	// new anonymous one each time.
	rec := httptest.NewRecorder()
	f.s.handlePutPublicRSVP(rec, putRSVPRequest(nextTestIP(), f.slug, `{"name":"First","attending":"yes","count":1,"answers":{},"website":""}`))
	if rec.Code != http.StatusOK {
		t.Fatalf("first yes: status = %d, body = %s", rec.Code, rec.Body.String())
	}
	var firstCookie *http.Cookie
	for _, c := range rec.Result().Cookies() {
		if c.Name == eventCookieName(rsvpCookiePrefix, f.eventID) {
			firstCookie = c
		}
	}
	if firstCookie == nil {
		t.Fatal("expected a syt_rsvp_ cookie on the first RSVP")
	}

	// A second, brand-new "yes" is capacity_reached: the only spot is taken.
	rec = httptest.NewRecorder()
	f.s.handlePutPublicRSVP(rec, putRSVPRequest(nextTestIP(), f.slug, `{"name":"Second","attending":"yes","count":1,"answers":{},"website":""}`))
	if rec.Code != http.StatusConflict || !containsAll(rec.Body.String(), `"capacity_reached"`, `"spots_left":0`) {
		t.Fatalf("second yes: status = %d, body = %s, want 409 capacity_reached spots_left:0", rec.Code, rec.Body.String())
	}

	// The first RSVP switching to "no" is always allowed, even at capacity
	// (lowering its own commitment can never make things worse).
	req := putRSVPRequest(nextTestIP(), f.slug, `{"name":"First","attending":"no","count":0,"answers":{},"website":""}`)
	req.AddCookie(firstCookie)
	rec = httptest.NewRecorder()
	f.s.handlePutPublicRSVP(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("first switches to no: status = %d, body = %s", rec.Code, rec.Body.String())
	}

	// The spot is now free: a brand-new "yes" succeeds.
	rec = httptest.NewRecorder()
	f.s.handlePutPublicRSVP(rec, putRSVPRequest(nextTestIP(), f.slug, `{"name":"Third","attending":"yes","count":1,"answers":{},"website":""}`))
	if rec.Code != http.StatusOK {
		t.Fatalf("third yes after the spot freed up: status = %d, body = %s", rec.Code, rec.Body.String())
	}

	// The first RSVP trying to switch back to "yes" is capacity_reached
	// again: that would be an increase while the event is full.
	req = putRSVPRequest(nextTestIP(), f.slug, `{"name":"First","attending":"yes","count":1,"answers":{},"website":""}`)
	req.AddCookie(firstCookie)
	rec = httptest.NewRecorder()
	f.s.handlePutPublicRSVP(rec, req)
	if rec.Code != http.StatusConflict || !containsAll(rec.Body.String(), `"capacity_reached"`) {
		t.Fatalf("first re-increases to yes while full: status = %d, body = %s, want 409 capacity_reached", rec.Code, rec.Body.String())
	}
}

func TestHandlePutPublicRSVP_CapacityRaceIsExact(t *testing.T) {
	pool := dbTestPool(t)
	rdb := dbTestRedis(t)
	f := newPublicEventFixture(t, pool, rdb, publicEventOpts{})
	f.withJobs(t)
	f.setRSVPContent(t, intPtr(10), "", 1, 4)

	const n = 20
	results := make([]int, n)
	var wg sync.WaitGroup
	for i := 0; i < n; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			body := fmt.Sprintf(`{"name":"Guest %d","attending":"yes","count":1,"answers":{},"website":""}`, i)
			req := putRSVPRequest(nextTestIP(), f.slug, body)
			rec := httptest.NewRecorder()
			f.s.handlePutPublicRSVP(rec, req)
			results[i] = rec.Code
		}(i)
	}
	wg.Wait()

	var successes, conflicts int
	for _, code := range results {
		switch code {
		case http.StatusOK:
			successes++
		case http.StatusConflict:
			conflicts++
		default:
			t.Errorf("unexpected status %d", code)
		}
	}
	if successes != 10 {
		t.Errorf("successes = %d, want 10", successes)
	}
	if conflicts != 10 {
		t.Errorf("conflicts = %d, want 10", conflicts)
	}

	var heads int
	if err := pool.QueryRow(context.Background(),
		"SELECT coalesce(sum(count), 0) FROM rsvps WHERE event_id = $1 AND attending = 'yes'", f.eventID,
	).Scan(&heads); err != nil {
		t.Fatalf("sum heads: %v", err)
	}
	if heads != 10 {
		t.Errorf("heads = %d, want exactly 10 (no overselling)", heads)
	}
}

// extractJSONString is a tiny helper for pulling one top-level-ish string
// field's value out of a response body in tests that don't need full
// decoding. Returns false if the key wasn't found.
func extractJSONString(t *testing.T, body, key string, out *string) bool {
	t.Helper()
	marker := `"` + key + `":"`
	i := strings.Index(body, marker)
	if i < 0 {
		return false
	}
	rest := body[i+len(marker):]
	j := strings.IndexByte(rest, '"')
	if j < 0 {
		return false
	}
	*out = rest[:j]
	return true
}

func intPtr(n int) *int { return &n }

// --- host endpoints: GET /rsvps, /rsvps/summary, DELETE /rsvps/{id} ---

func TestHandleListRSVPsAndSummary_IDORMatrix(t *testing.T) {
	pool := dbTestPool(t)
	rdb := dbTestRedis(t)
	f := newEventTestFixture(t, pool, rdb)
	ctx := context.Background()

	if _, err := f.s.q.CreateOpenRSVP(ctx, store.CreateOpenRSVPParams{
		ID: uuid.Must(uuid.NewV7()), EventID: f.eventID, Name: "Guest", Attending: "yes", Count: 1, Answers: []byte("{}"),
	}); err != nil {
		t.Fatalf("create open rsvp: %v", err)
	}

	cases := []struct {
		name       string
		userID     uuid.UUID
		wantStatus int
	}{
		{"owner", f.ownerID, http.StatusOK},
		{"editor", f.editorID, http.StatusOK},
		{"viewer", f.viewerID, http.StatusOK},
		{"stranger", f.strangerID, http.StatusNotFound},
	}
	for _, c := range cases {
		t.Run("list/"+c.name, func(t *testing.T) {
			req := requestAs(http.MethodGet, "/", c.userID)
			req = withRouteID(req, f.eventID)
			rec := httptest.NewRecorder()
			f.s.handleListRSVPs(rec, req)
			if rec.Code != c.wantStatus {
				t.Fatalf("status = %d, want %d; body = %s", rec.Code, c.wantStatus, rec.Body.String())
			}
		})
		t.Run("summary/"+c.name, func(t *testing.T) {
			req := requestAs(http.MethodGet, "/", c.userID)
			req = withRouteID(req, f.eventID)
			rec := httptest.NewRecorder()
			f.s.handleRSVPSummary(rec, req)
			if rec.Code != c.wantStatus {
				t.Fatalf("status = %d, want %d; body = %s", rec.Code, c.wantStatus, rec.Body.String())
			}
			if c.wantStatus == http.StatusOK && !containsAll(rec.Body.String(), `"yes":1`, `"total":1`) {
				t.Errorf("body = %s, want yes:1 total:1", rec.Body.String())
			}
		})
	}
}

func TestHandleDeleteRSVP_IDORMatrix(t *testing.T) {
	pool := dbTestPool(t)
	rdb := dbTestRedis(t)

	newRSVP := func(t *testing.T, f eventTestFixture) uuid.UUID {
		t.Helper()
		row, err := f.s.q.CreateOpenRSVP(context.Background(), store.CreateOpenRSVPParams{
			ID: uuid.Must(uuid.NewV7()), EventID: f.eventID, Name: "Guest", Attending: "yes", Count: 1, Answers: []byte("{}"),
		})
		if err != nil {
			t.Fatalf("create open rsvp: %v", err)
		}
		return row.ID
	}

	cases := []struct {
		name       string
		userID     func(f eventTestFixture) uuid.UUID
		wantStatus int
	}{
		{"owner", func(f eventTestFixture) uuid.UUID { return f.ownerID }, http.StatusNoContent},
		{"editor", func(f eventTestFixture) uuid.UUID { return f.editorID }, http.StatusNoContent},
		{"viewer", func(f eventTestFixture) uuid.UUID { return f.viewerID }, http.StatusForbidden},
		{"stranger", func(f eventTestFixture) uuid.UUID { return f.strangerID }, http.StatusNotFound},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			f := newEventTestFixture(t, pool, rdb)
			rsvpID := newRSVP(t, f)
			req := requestAs(http.MethodDelete, "/", c.userID(f))
			req = withRouteParam(withRouteID(req, f.eventID), "rsvpID", rsvpID.String())
			rec := httptest.NewRecorder()
			f.s.handleDeleteRSVP(rec, req)
			if rec.Code != c.wantStatus {
				t.Fatalf("status = %d, want %d; body = %s", rec.Code, c.wantStatus, rec.Body.String())
			}
		})
	}
}

// --- GET /v1/events/{id}/rsvps.csv ---

func TestHandleExportRSVPsCSV_EscapingAndStreaming(t *testing.T) {
	pool := dbTestPool(t)
	rdb := dbTestRedis(t)
	f := newPublicEventFixture(t, pool, rdb, publicEventOpts{})
	ctx := context.Background()

	// One row with a formula-injection payload in a free-text answer.
	if _, err := f.s.q.CreateOpenRSVP(ctx, store.CreateOpenRSVPParams{
		ID: uuid.Must(uuid.NewV7()), EventID: f.eventID, Name: "=cmd|' /C calc'!A1",
		Attending: "yes", Count: 1, Answers: []byte(`{"dietary":"=SUM(1+1)"}`),
	}); err != nil {
		t.Fatalf("create open rsvp: %v", err)
	}

	req := requestAs(http.MethodGet, "/", f.ownerID)
	req = withRouteID(req, f.eventID)
	rec := httptest.NewRecorder()
	f.s.handleExportRSVPsCSV(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200; body = %s", rec.Code, rec.Body.String())
	}
	if ct := rec.Header().Get("Content-Type"); !strings.HasPrefix(ct, "text/csv") {
		t.Errorf("Content-Type = %q", ct)
	}
	if cd := rec.Header().Get("Content-Disposition"); !strings.Contains(cd, "rsvps.csv") {
		t.Errorf("Content-Disposition = %q", cd)
	}

	body := rec.Body.String()
	if !strings.HasPrefix(body, "\xef\xbb\xbf") {
		t.Error("expected a UTF-8 BOM at the start of the CSV")
	}
	body = strings.TrimPrefix(body, "\xef\xbb\xbf")

	rows, err := csv.NewReader(strings.NewReader(body)).ReadAll()
	if err != nil {
		t.Fatalf("parse csv: %v", err)
	}
	if len(rows) != 2 {
		t.Fatalf("rows = %d, want 2 (header + 1 data row); rows = %v", len(rows), rows)
	}
	header := rows[0]
	if header[0] != "name" || header[4] != "guest" {
		t.Errorf("header = %v", header)
	}
	dataRow := rows[1]
	if !strings.HasPrefix(dataRow[0], "'=") {
		t.Errorf("name cell = %q, want a leading quote guarding the formula-like value", dataRow[0])
	}
	// The dietary answer column is appended after the fixed columns.
	dietaryCol := len(header) - 1
	if !strings.HasPrefix(dataRow[dietaryCol], "'=") {
		t.Errorf("dietary cell = %q, want a leading quote guarding the formula-like value", dataRow[dietaryCol])
	}
}

// TestHandleExportRSVPsCSV_HeaderLabelEscaping covers a host-defined RSVP
// question (rsvp.questions) whose label is a formula-injection payload:
// f.Label is host/editor-controlled (unlike the fixed columns), so the
// header row must run it through csvSafe exactly like any data cell.
func TestHandleExportRSVPsCSV_HeaderLabelEscaping(t *testing.T) {
	pool := dbTestPool(t)
	rdb := dbTestRedis(t)
	f := newPublicEventFixture(t, pool, rdb, publicEventOpts{})
	ctx := context.Background()

	occRow, err := f.s.q.GetOccasion(ctx, "birthday")
	if err != nil {
		t.Fatalf("get occasion: %v", err)
	}
	occ, err := content.ParseOccasion(occRow)
	if err != nil {
		t.Fatalf("parse occasion: %v", err)
	}
	contentJSON := `[
		{"id":"hero1","type":"hero","title":"Sam's Party","subtitle":""},
		{"id":"dt1","type":"datetime","heading":"","start_local":"2027-06-01T18:00","end_local":"","timezone":"America/New_York","all_day":false},
		{"id":"rsvp1","type":"rsvp","heading":"RSVP","body":"","deadline_local":"","capacity":null,"max_party_size":4,"fields":[],
			"questions":[{"key":"q_shirt","label":"=cmd|' /C calc'!A1","type":"text","max_length":50}]}
	]`
	saved, err := content.ValidateContent([]byte(contentJSON), occ)
	if err != nil {
		t.Fatalf("validate content: %v", err)
	}
	if _, err := f.s.q.UpdateEventContent(ctx, store.UpdateEventContentParams{
		Content: saved.JSON, Title: saved.Title, StartsAt: saved.StartsAt, EndsAt: saved.EffectiveEnd(),
		EventID: f.eventID, Version: 4, UserID: f.ownerID,
	}); err != nil {
		t.Fatalf("update content: %v", err)
	}

	req := requestAs(http.MethodGet, "/", f.ownerID)
	req = withRouteID(req, f.eventID)
	rec := httptest.NewRecorder()
	f.s.handleExportRSVPsCSV(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200; body = %s", rec.Code, rec.Body.String())
	}

	body := strings.TrimPrefix(rec.Body.String(), "\xef\xbb\xbf")
	rows, err := csv.NewReader(strings.NewReader(body)).ReadAll()
	if err != nil {
		t.Fatalf("parse csv: %v", err)
	}
	header := rows[0]
	labelCol := header[len(header)-1]
	if !strings.HasPrefix(labelCol, "'=") {
		t.Errorf("question label header cell = %q, want a leading quote guarding the formula-like value", labelCol)
	}
}

func TestHandleExportRSVPsCSV_IDORMatrix(t *testing.T) {
	pool := dbTestPool(t)
	rdb := dbTestRedis(t)
	f := newEventTestFixture(t, pool, rdb)

	cases := []struct {
		name       string
		userID     uuid.UUID
		wantStatus int
	}{
		{"owner", f.ownerID, http.StatusOK},
		{"viewer", f.viewerID, http.StatusOK},
		{"stranger", f.strangerID, http.StatusNotFound},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			req := requestAs(http.MethodGet, "/", c.userID)
			req = withRouteID(req, f.eventID)
			rec := httptest.NewRecorder()
			f.s.handleExportRSVPsCSV(rec, req)
			if rec.Code != c.wantStatus {
				t.Fatalf("status = %d, want %d; body = %s", rec.Code, c.wantStatus, rec.Body.String())
			}
		})
	}
}
