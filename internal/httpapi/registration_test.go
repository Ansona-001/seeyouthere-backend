package httpapi

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgconn"

	"github.com/ansonarose/seeyouthere-backend/internal/token"
)

// Open registration on a hosted event: PUT /v1/public/events/{slug}/rsvp.

func openHostedFixture(t *testing.T) *publicEventFixture {
	t.Helper()
	pool := dbTestPool(t)
	rdb := dbTestRedis(t)
	f := newPublicEventFixture(t, pool, rdb, publicEventOpts{rsvpMode: "open"})
	if _, err := pool.Exec(context.Background(), "UPDATE events SET kind = 'hosted' WHERE id = $1", f.eventID); err != nil {
		t.Fatalf("make hosted: %v", err)
	}
	f.withJobs(t)
	giveRSVPConfirmMailBudget(t, f, rdb)
	return f
}

func uniqueEmail(prefix string) string {
	return prefix + "-" + uuid.Must(uuid.NewV7()).String() + "@example.invalid"
}

func registerBody(name, email, attending string, count int) string {
	b, _ := json.Marshal(map[string]any{
		"name": name, "email": email, "attending": attending, "count": count, "answers": map[string]any{}, "website": "",
	})
	return string(b)
}

func (f *publicEventFixture) putRSVP(body string, cookies ...*http.Cookie) *httptest.ResponseRecorder {
	req := putRSVPRequest(nextTestIP(), f.slug, body)
	for _, c := range cookies {
		req.AddCookie(c)
	}
	rec := httptest.NewRecorder()
	f.s.handlePutPublicRSVP(rec, req)
	return rec
}

func rsvpCookieFrom(rec *httptest.ResponseRecorder, eventID uuid.UUID) *http.Cookie {
	for _, c := range rec.Result().Cookies() {
		if c.Name == eventCookieName(rsvpCookiePrefix, eventID) {
			return c
		}
	}
	return nil
}

// checkEmailBody is the one response every registration attempt gets, whatever happened to it.
const checkEmailBody = `{"status":"check_email"}`

// register submits a registration and asserts the uniform 202: no cookie, no RSVP data.
func (f *publicEventFixture) register(t *testing.T, body string) string {
	t.Helper()
	rec := f.putRSVP(body)
	wantStatusCode(t, rec, http.StatusAccepted, "")
	if len(rec.Result().Cookies()) != 0 {
		t.Errorf("cookie set on a registration: %v", rec.Result().Cookies())
	}
	if got := strings.TrimSpace(rec.Body.String()); got != checkEmailBody {
		t.Errorf("body = %s, want %s", got, checkEmailBody)
	}
	return rec.Body.String()
}

// acceptLinkCookie plays the emailed link: it opens the registration's edit token through
// handleAcceptRSVPLink and returns the RSVP cookie that sets.
func (f *publicEventFixture) acceptLinkCookie(t *testing.T, email string) *http.Cookie {
	t.Helper()
	var id uuid.UUID
	var version int32
	if err := f.s.pool.QueryRow(context.Background(),
		"SELECT id, edit_token_version FROM rsvps WHERE event_id = $1 AND email = $2", f.eventID, strings.ToLower(strings.TrimSpace(email))).Scan(&id, &version); err != nil {
		t.Fatalf("read registration: %v", err)
	}
	req := requestWithIP(http.MethodPost, "/", nextTestIP(), []byte(`{"token":"`+f.s.tokens.RSVPToken(id, version)+`"}`))
	req.Header.Set("Content-Type", "application/json")
	rec := httptest.NewRecorder()
	f.s.handleAcceptRSVPLink(rec, req)
	wantStatusCode(t, rec, http.StatusOK, "")
	c := rsvpCookieFrom(rec, f.eventID)
	if c == nil {
		t.Fatal("accepting the emailed link set no rsvp cookie")
	}
	return c
}

func (f *publicEventFixture) registrationCount(t *testing.T) int {
	t.Helper()
	var n int
	if err := f.s.pool.QueryRow(context.Background(), "SELECT count(*) FROM rsvps WHERE event_id = $1", f.eventID).Scan(&n); err != nil {
		t.Fatalf("count rsvps: %v", err)
	}
	return n
}

func (f *publicEventFixture) getPublicWithCookies(t *testing.T, cookies ...*http.Cookie) (publicViewerPass, string) {
	t.Helper()
	req := withSlugParam(requestWithIP(http.MethodGet, "/", nextTestIP(), nil), f.slug)
	for _, c := range cookies {
		req.AddCookie(c)
	}
	rec := httptest.NewRecorder()
	f.s.handleGetPublicEvent(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, body = %s", rec.Code, rec.Body.String())
	}
	var out publicViewerPass
	if err := json.Unmarshal(rec.Body.Bytes(), &out); err != nil {
		t.Fatalf("decode: %v", err)
	}
	return out, rec.Body.String()
}

func TestHandlePutPublicRSVP_HostedRegistration(t *testing.T) {
	tests := []struct {
		attending string
		count     int
		wantPass  bool
	}{
		{"yes", 3, true},
		{"maybe", 2, false},
		{"no", 0, false},
	}
	for _, tc := range tests {
		t.Run(tc.attending, func(t *testing.T) {
			f := openHostedFixture(t)
			email := uniqueEmail("Reg.Mixed")
			f.register(t, registerBody("Jordan", "  "+strings.ToUpper(email)+" ", tc.attending, tc.count))

			var registration bool
			var stored string
			if err := f.s.pool.QueryRow(context.Background(),
				"SELECT registration, email FROM rsvps WHERE event_id = $1", f.eventID).Scan(&registration, &stored); err != nil {
				t.Fatalf("read row: %v", err)
			}
			if !registration || stored != strings.ToLower(email) {
				t.Errorf("registration = %v, email = %q, want true and %q", registration, stored, strings.ToLower(email))
			}
			if n := countRiverJobsForEventRSVPs(t, f.s.pool, "send_rsvp_confirmation", f.eventID); n != 1 {
				t.Errorf("confirmation jobs = %d, want 1", n)
			}

			// Without the emailed link there is no cookie, so nothing about the registration is visible.
			if out, _ := f.getPublicWithCookies(t); out.Viewer.Pass != nil || out.Viewer.RSVP != nil {
				t.Errorf("registration visible before the emailed link was opened: %+v", out.Viewer)
			}
			cookie := f.acceptLinkCookie(t, email)

			out, body := f.getPublicWithCookies(t, cookie)
			if tc.wantPass != (out.Viewer.Pass != nil) {
				t.Fatalf("pass present = %v, want %v: %s", out.Viewer.Pass != nil, tc.wantPass, body)
			}
			if out.Viewer.RSVP == nil {
				t.Fatalf("viewer.rsvp missing: %s", body)
			}
			if !tc.wantPass {
				return
			}
			p := out.Viewer.Pass
			if p.Admits != int32(tc.count) || p.CheckedInAt != nil {
				t.Errorf("pass = %+v, want admits %d, not checked in", p, tc.count)
			}
			id, mac, err := token.Split(p.Token)
			if err != nil || id != out.Viewer.RSVP.ID {
				t.Fatalf("pass token id = %v err = %v, want %v", id, err, out.Viewer.RSVP.ID)
			}
			if !f.s.tokens.CheckRSVPPass(id, 1, mac) || f.s.tokens.CheckRSVP(id, 1, mac) {
				t.Error("pass token must verify as an RSVP pass and not as the edit token")
			}
			if strings.Contains(body, cookie.Value) {
				t.Error("response leaks the edit token")
			}

			// Moving away from yes revokes the pass; coming back restores it.
			wantStatusCode(t, f.putRSVP(registerBody("Jordan", email, "maybe", 2), cookie), http.StatusOK, "")
			if out, _ := f.getPublicWithCookies(t, cookie); out.Viewer.Pass != nil {
				t.Error("pass still present after changing to maybe")
			}
			wantStatusCode(t, f.putRSVP(registerBody("Jordan", email, "yes", 1), cookie), http.StatusOK, "")
			if out, _ := f.getPublicWithCookies(t, cookie); out.Viewer.Pass == nil || out.Viewer.Pass.Admits != 1 {
				t.Errorf("pass after returning to yes = %+v", out.Viewer.Pass)
			}
		})
	}
}

func TestHandlePutPublicRSVP_HostedEmailRequired(t *testing.T) {
	f := openHostedFixture(t)
	tests := []struct {
		name string
		body string
	}{
		{"missing", `{"name":"Jordan","attending":"yes","count":1,"answers":{},"website":""}`},
		{"null", `{"name":"Jordan","email":null,"attending":"yes","count":1,"answers":{},"website":""}`},
		{"blank", registerBody("Jordan", "   ", "yes", 1)},
		{"malformed", registerBody("Jordan", "not-an-email", "yes", 1)},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			rec := f.putRSVP(tc.body)
			wantStatusCode(t, rec, http.StatusBadRequest, "validation_failed")
			if !strings.Contains(rec.Body.String(), `"email"`) {
				t.Errorf("body = %s, want an email issue", rec.Body.String())
			}
			if rsvpCookieFrom(rec, f.eventID) != nil {
				t.Error("cookie set on a rejected registration")
			}
		})
	}
	if n := f.registrationCount(t); n != 0 {
		t.Errorf("rows = %d, want 0", n)
	}
}

func TestHandlePutPublicRSVP_HostedDuplicateEmail(t *testing.T) {
	f := openHostedFixture(t)
	email := uniqueEmail("dup")
	firstBody := f.register(t, registerBody("First", email, "yes", 2))
	var firstID uuid.UUID
	if err := f.s.pool.QueryRow(context.Background(), "SELECT id FROM rsvps WHERE event_id = $1", f.eventID).Scan(&firstID); err != nil {
		t.Fatalf("read id: %v", err)
	}

	for _, variant := range []string{strings.ToUpper(email), " " + email + " ", email} {
		body := f.register(t, registerBody("Someone Else", variant, "yes", 1))
		if body != firstBody {
			t.Errorf("duplicate body = %q, want byte-identical to %q", body, firstBody)
		}
		for _, leak := range []string{firstID.String(), "First", "token", "rsvp\":"} {
			if strings.Contains(body, leak) {
				t.Errorf("duplicate response leaks %q: %s", leak, body)
			}
		}
	}
	if n := f.registrationCount(t); n != 1 {
		t.Errorf("rows = %d, want 1", n)
	}
	var jobsForFirst int
	if err := f.s.pool.QueryRow(context.Background(),
		`SELECT count(*) FROM river_job WHERE kind = 'send_rsvp_confirmation' AND args->>'rsvp_id' = $1`, firstID.String()).Scan(&jobsForFirst); err != nil {
		t.Fatalf("count jobs: %v", err)
	}
	if jobsForFirst != 4 { // the sign-up plus one resend per duplicate attempt
		t.Errorf("confirmation jobs for the existing registration = %d, want 4", jobsForFirst)
	}
}

func TestHandlePutPublicRSVP_HostedGuestListEmail(t *testing.T) {
	f := openHostedFixture(t)
	email := uniqueEmail("onlist")
	gid, _ := f.testGuest(t, "Listed", 2)
	if _, err := f.s.pool.Exec(context.Background(), "UPDATE guests SET email = $2 WHERE id = $1", gid, email); err != nil {
		t.Fatalf("set guest email: %v", err)
	}
	f.register(t, registerBody("Impostor", strings.ToUpper(email), "yes", 1))
	if n := f.registrationCount(t); n != 0 {
		t.Errorf("rows = %d, want 0", n)
	}
	if n := countRiverJobsForEventRSVPs(t, f.s.pool, "send_rsvp_confirmation", f.eventID); n != 0 {
		t.Errorf("confirmation jobs = %d, want 0", n)
	}
	if n := countRiverJobsByEvent(t, f.s.pool, "notify_rsvps", f.eventID); n != 0 {
		t.Errorf("notify jobs = %d, want 0", n)
	}
}

func TestHandlePutPublicRSVP_HostedGuestCookieIsNotARegistration(t *testing.T) {
	f := openHostedFixture(t)
	_, inv := f.testGuest(t, "Priya", 3)
	// Email is optional for an invited guest.
	req := putRSVPRequest(nextTestIP(), f.slug, `{"name":"Priya","attending":"yes","count":2,"answers":{},"website":""}`)
	req.AddCookie(&http.Cookie{Name: eventCookieName(inviteCookiePrefix, f.eventID), Value: inv})
	rec := httptest.NewRecorder()
	f.s.handlePutPublicRSVP(rec, req)
	wantStatusCode(t, rec, http.StatusOK, "")
	var registration bool
	if err := f.s.pool.QueryRow(context.Background(), "SELECT registration FROM rsvps WHERE event_id = $1", f.eventID).Scan(&registration); err != nil {
		t.Fatalf("read: %v", err)
	}
	if registration {
		t.Error("guest RSVP flagged as registration")
	}
}

func TestHandlePutPublicRSVP_HostedEditRules(t *testing.T) {
	f := openHostedFixture(t)
	email := uniqueEmail("edit")
	f.register(t, registerBody("Jordan", email, "yes", 2))
	cookie := f.acceptLinkCookie(t, email)
	storedEmail := func() string {
		var e string
		_ = f.s.pool.QueryRow(context.Background(), "SELECT email FROM rsvps WHERE event_id = $1", f.eventID).Scan(&e)
		return e
	}

	t.Run("email is locked", func(t *testing.T) {
		// Another registrant's address is just a different address: the lock answers first.
		other := uniqueEmail("taken")
		f.register(t, registerBody("Taken", other, "yes", 1))
		for _, changed := range []string{other, uniqueEmail("fresh")} {
			rec := f.putRSVP(registerBody("Jordan", changed, "yes", 2), cookie)
			wantStatusCode(t, rec, http.StatusBadRequest, "validation_failed")
			if !strings.Contains(rec.Body.String(), `"email"`) {
				t.Errorf("body = %s", rec.Body.String())
			}
		}
		if storedEmail() != strings.ToLower(email) {
			t.Errorf("stored email changed to %q", storedEmail())
		}
	})
	t.Run("same email in another case is an edit and sends no new mail", func(t *testing.T) {
		before := countRiverJobsForEventRSVPs(t, f.s.pool, "send_rsvp_confirmation", f.eventID)
		wantStatusCode(t, f.putRSVP(registerBody("Jordan R", strings.ToUpper(email), "yes", 1), cookie), http.StatusOK, "")
		if after := countRiverJobsForEventRSVPs(t, f.s.pool, "send_rsvp_confirmation", f.eventID); after != before {
			t.Errorf("confirmation jobs %d -> %d, want unchanged", before, after)
		}
	})
	t.Run("frozen after check-in", func(t *testing.T) {
		if _, err := f.s.pool.Exec(context.Background(),
			"UPDATE rsvps SET checked_in_at = now(), checked_in_count = 1 WHERE event_id = $1 AND email = $2", f.eventID, strings.ToLower(email)); err != nil {
			t.Fatalf("check in: %v", err)
		}
		for _, attending := range []string{"yes", "no"} {
			wantStatusCode(t, f.putRSVP(registerBody("Jordan", email, attending, 1), cookie), http.StatusConflict, "checked_in")
		}
		var attending string
		_ = f.s.pool.QueryRow(context.Background(), "SELECT attending FROM rsvps WHERE event_id = $1 AND email = $2", f.eventID, strings.ToLower(email)).Scan(&attending)
		if attending != "yes" {
			t.Errorf("attending = %q after refused edits, want yes", attending)
		}
	})
}

func TestWriteRSVPWriteError_RegistrationErrors(t *testing.T) {
	tests := []struct {
		name   string
		err    error
		status int
		code   string
	}{
		{"unique violation on the email key", fmt.Errorf("create open rsvp: %w", &pgconn.PgError{Code: "23505", ConstraintName: "rsvps_event_registration_email_key"}), http.StatusConflict, "email_registered"},
		{"locked email", errRegistrationEmailLocked, http.StatusBadRequest, "validation_failed"},
		{"checked in", errRegistrationCheckedIn, http.StatusConflict, "checked_in"},
		{"closed", errRSVPClosed, http.StatusConflict, "rsvp_closed"},
		{"other unique violation is not mapped", &pgconn.PgError{Code: "23505", ConstraintName: "other_key"}, http.StatusInternalServerError, ""},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			rec := httptest.NewRecorder()
			writeRSVPWriteError(rec, httptest.NewRequest(http.MethodPut, "/", nil), tc.err)
			if rec.Code != tc.status || (tc.code != "" && !strings.Contains(rec.Body.String(), `"`+tc.code+`"`)) {
				t.Errorf("status = %d body = %s, want %d %q", rec.Code, rec.Body.String(), tc.status, tc.code)
			}
		})
	}
}

func TestHandlePutPublicRSVP_HostedCapacityRaceIsExact(t *testing.T) {
	f := openHostedFixture(t)
	f.setRSVPContent(t, intPtr(5), "", 1, 4)

	const n = 15
	codes := make([]int, n)
	var wg sync.WaitGroup
	for i := range n {
		wg.Add(1)
		go func() {
			defer wg.Done()
			codes[i] = f.putRSVP(registerBody(fmt.Sprintf("Guest %d", i), uniqueEmail("cap"), "yes", 1)).Code
		}()
	}
	wg.Wait()
	// Every attempt gets the same 202, whether or not it won a spot; the rows tell them apart.
	for _, c := range codes {
		if c != http.StatusAccepted {
			t.Errorf("unexpected status %d", c)
		}
	}
	if rows := f.registrationCount(t); rows != 5 {
		t.Errorf("registrations = %d, want exactly 5", rows)
	}
	var heads int
	_ = f.s.pool.QueryRow(context.Background(), "SELECT coalesce(sum(count), 0) FROM rsvps WHERE event_id = $1 AND attending = 'yes'", f.eventID).Scan(&heads)
	if heads != 5 {
		t.Errorf("heads = %d, want exactly 5", heads)
	}
}

func TestHandlePutPublicRSVP_HostedConcurrentSameEmail(t *testing.T) {
	f := openHostedFixture(t)
	email := uniqueEmail("race")
	const n = 4 // below the per-email limit of 5
	codes := make([]int, n)
	var wg sync.WaitGroup
	for i := range n {
		wg.Add(1)
		go func() {
			defer wg.Done()
			codes[i] = f.putRSVP(registerBody("Racer", email, "yes", 1)).Code
		}()
	}
	wg.Wait()
	ok := 0
	for _, c := range codes {
		if c == http.StatusAccepted {
			ok++
		} else {
			t.Errorf("unexpected status %d", c)
		}
	}
	// Every racer gets the same 202, including one that loses the unique index.
	if ok != n || f.registrationCount(t) != 1 {
		t.Errorf("accepted = %d, rows = %d, want %d and exactly one registration", ok, f.registrationCount(t), n)
	}
	if jobs := countRiverJobsForEventRSVPs(t, f.s.pool, "send_rsvp_confirmation", f.eventID); jobs < 1 || jobs > n {
		t.Errorf("confirmation jobs = %d, want between 1 and %d", jobs, n)
	}
}

func TestHandlePutPublicRSVP_HostedRegistrationBound(t *testing.T) {
	f := openHostedFixture(t)
	if _, err := f.s.pool.Exec(context.Background(),
		`INSERT INTO rsvps (id, event_id, name, email, attending, count, answers, registration)
		 SELECT gen_random_uuid(), $1, 'Bulk', 'bulk' || g || '@example.invalid', 'yes', 1, '{}', true
		 FROM generate_series(1, $2::int) g`, f.eventID, maxRegistrationsPerEvent-1); err != nil {
		t.Fatalf("seed registrations: %v", err)
	}
	f.register(t, registerBody("Last", uniqueEmail("last"), "yes", 1))
	// Over the bound the attempt is answered like any other registration, with nothing created.
	f.register(t, registerBody("Over", uniqueEmail("over"), "yes", 1))
	if n := f.registrationCount(t); n != maxRegistrationsPerEvent {
		t.Errorf("rows = %d, want %d", n, maxRegistrationsPerEvent)
	}
}

func TestHandlePutPublicRSVP_HostedPerEmailRateLimit(t *testing.T) {
	f := openHostedFixture(t)
	email := uniqueEmail("rl")
	for range 5 {
		f.register(t, registerBody("Spammer", email, "yes", 1))
	}
	rec := f.putRSVP(registerBody("Spammer", email, "yes", 1))
	wantStatusCode(t, rec, http.StatusTooManyRequests, "rate_limited")
	// Another address on the same event is unaffected.
	f.register(t, registerBody("Other", uniqueEmail("rl2"), "yes", 1))
	if n := countRiverJobsForEventRSVPs(t, f.s.pool, "send_rsvp_confirmation", f.eventID); n != 6 {
		t.Errorf("confirmation jobs = %d, want 6 (1 sign-up and 4 resends for the throttled address, 1 for the other)", n)
	}
}

func TestHandlePutPublicRSVP_InviteKindUnchanged(t *testing.T) {
	pool := dbTestPool(t)
	rdb := dbTestRedis(t)
	f := newPublicEventFixture(t, pool, rdb, publicEventOpts{rsvpMode: "open"})
	f.withJobs(t)
	giveRSVPConfirmMailBudget(t, f, rdb)
	email := uniqueEmail("same")

	for range 2 {
		wantStatusCode(t, f.putRSVP(registerBody("Jordan", email, "yes", 1)), http.StatusOK, "")
	}
	wantStatusCode(t, f.putRSVP(`{"name":"No Email","attending":"yes","count":1,"answers":{},"website":""}`), http.StatusOK, "")

	var rows, regs int
	if err := pool.QueryRow(context.Background(),
		"SELECT count(*), count(*) FILTER (WHERE registration) FROM rsvps WHERE event_id = $1", f.eventID).Scan(&rows, &regs); err != nil {
		t.Fatalf("count: %v", err)
	}
	if rows != 3 || regs != 0 {
		t.Errorf("rows = %d, registrations = %d, want 3 and 0", rows, regs)
	}
}
