package httpapi

import (
	"context"
	"net/http"
	"testing"
	"time"

	"github.com/google/uuid"
)

// Hardening of open registration on hosted events: a uniform answer for unavailable events, and
// registrations that only count toward capacity while their pass link is unconfirmed for 24 hours.

// confirmationJobsFor counts the send_rsvp_confirmation jobs queued for one RSVP.
func (f *publicEventFixture) confirmationJobsFor(t *testing.T, rsvpID uuid.UUID) int {
	t.Helper()
	var n int
	if err := f.s.pool.QueryRow(context.Background(),
		`SELECT count(*) FROM river_job WHERE kind = 'send_rsvp_confirmation' AND args->>'rsvp_id' = $1`, rsvpID.String()).Scan(&n); err != nil {
		t.Fatalf("count jobs: %v", err)
	}
	return n
}

// An email that already holds a registration or a guest-list slot is answered 202 before the closed
// and capacity checks run. If a new email got a 409 there, the response would tell the requester
// which emails are known, so all three get the same 202 and the new one creates and queues nothing.
func TestHandlePutPublicRSVP_HostedUnavailableIsUniform(t *testing.T) {
	for _, tc := range []struct {
		name  string
		close func(t *testing.T, f *publicEventFixture)
	}{
		{"past deadline", func(t *testing.T, f *publicEventFixture) { f.setRSVPContent(t, nil, "2020-01-01T00:00", 4, 4) }},
		{"capacity short", func(t *testing.T, f *publicEventFixture) { f.setRSVPContent(t, intPtr(1), "", 4, 4) }},
	} {
		t.Run(tc.name, func(t *testing.T) {
			f := openHostedFixture(t)
			existing := uniqueEmail("known")
			f.register(t, registerBody("Known", existing, "yes", 1))
			var existingID uuid.UUID
			if err := f.s.pool.QueryRow(context.Background(), "SELECT id FROM rsvps WHERE event_id = $1", f.eventID).Scan(&existingID); err != nil {
				t.Fatalf("read id: %v", err)
			}
			listed := uniqueEmail("onlist")
			gid, _ := f.testGuest(t, "Listed", 2)
			if _, err := f.s.pool.Exec(context.Background(), "UPDATE guests SET email = $2 WHERE id = $1", gid, listed); err != nil {
				t.Fatalf("set guest email: %v", err)
			}
			tc.close(t, f)

			fresh := uniqueEmail("fresh")
			bodies := map[string]string{
				"new":                   f.register(t, registerBody("Someone", fresh, "yes", 1)),
				"existing registration": f.register(t, registerBody("Someone", existing, "yes", 1)),
				"guest list":            f.register(t, registerBody("Someone", listed, "yes", 1)),
			}
			for name, b := range bodies {
				if b != bodies["new"] {
					t.Errorf("%s body = %q, want identical to the new email's %q", name, b, bodies["new"])
				}
			}
			if n := f.registrationCount(t); n != 1 {
				t.Errorf("rows = %d, want 1: the new email must not be registered", n)
			}
			var freshRows int
			_ = f.s.pool.QueryRow(context.Background(), "SELECT count(*) FROM rsvps WHERE event_id = $1 AND email = $2", f.eventID, fresh).Scan(&freshRows)
			if freshRows != 0 {
				t.Errorf("new email has %d rows, want 0", freshRows)
			}
			// The sign-up mail plus one resend for the existing registration; nothing for the others.
			if n := countRiverJobsForEventRSVPs(t, f.s.pool, "send_rsvp_confirmation", f.eventID); n != 2 {
				t.Errorf("confirmation jobs = %d, want 2", n)
			}
			if n := f.confirmationJobsFor(t, existingID); n != 2 {
				t.Errorf("jobs for the existing registration = %d, want 2", n)
			}
		})
	}
}

// The uniform 202 is only for callers with no cookie. A guest with an invite cookie and a registration
// holder editing through their link know who they are, so they still learn the event is closed or full.
func TestHandlePutPublicRSVP_HostedClosedAndFullKeep409ForCookieHolders(t *testing.T) {
	f := openHostedFixture(t)
	f.setRSVPContent(t, intPtr(2), "", 4, 4)
	holder := uniqueEmail("holder")
	f.register(t, registerBody("Holder", holder, "yes", 1))
	holderCookie := f.acceptLinkCookie(t, holder)
	f.register(t, registerBody("Other", uniqueEmail("other"), "yes", 1)) // the event is now full
	_, inv := f.testGuest(t, "Priya", 3)
	inviteCookie := &http.Cookie{Name: eventCookieName(inviteCookiePrefix, f.eventID), Value: inv}
	const guestBody = `{"name":"Priya","attending":"yes","count":2,"answers":{},"website":""}`

	wantStatusCode(t, f.putRSVP(registerBody("Holder", holder, "yes", 2), holderCookie), http.StatusConflict, "capacity_reached")
	wantStatusCode(t, f.putRSVP(guestBody, inviteCookie), http.StatusConflict, "capacity_reached")

	f.setRSVPContent(t, nil, "2020-01-01T00:00", 4, 5)
	wantStatusCode(t, f.putRSVP(registerBody("Holder", holder, "yes", 2), holderCookie), http.StatusConflict, "rsvp_closed")
	wantStatusCode(t, f.putRSVP(guestBody, inviteCookie), http.StatusConflict, "rsvp_closed")
}

// Opening the emailed link confirms a registration. An unconfirmed one holds its spot for 24 hours,
// then stops counting; a confirmed one keeps it.
func TestHandlePutPublicRSVP_HostedUnconfirmedStopsCounting(t *testing.T) {
	f := openHostedFixture(t)
	f.setRSVPContent(t, intPtr(1), "", 4, 4)
	ctx := context.Background()
	confirmedAt := func(email string) *time.Time {
		var at *time.Time
		if err := f.s.pool.QueryRow(ctx, "SELECT confirmed_at FROM rsvps WHERE event_id = $1 AND email = $2", f.eventID, email).Scan(&at); err != nil {
			t.Fatalf("read confirmed_at: %v", err)
		}
		return at
	}

	a := uniqueEmail("a")
	f.register(t, registerBody("A", a, "yes", 1))
	if confirmedAt(a) != nil {
		t.Fatal("registration confirmed before its link was opened")
	}
	// Fresh and unconfirmed: it holds the only spot.
	b := uniqueEmail("b")
	f.register(t, registerBody("B", b, "yes", 1))
	if n := f.registrationCount(t); n != 1 {
		t.Fatalf("rows = %d, want 1 while the first registration is fresh", n)
	}

	if _, err := f.s.pool.Exec(ctx, "UPDATE rsvps SET created_at = now() - interval '25 hours' WHERE event_id = $1 AND email = $2", f.eventID, a); err != nil {
		t.Fatalf("age registration: %v", err)
	}
	f.register(t, registerBody("B", b, "yes", 1))
	if n := f.registrationCount(t); n != 2 {
		t.Fatalf("rows = %d, want 2 once the unconfirmed one stopped counting", n)
	}

	// Opening the old link confirms A, and a repeat open keeps the first stamp.
	f.acceptLinkCookie(t, a)
	first := confirmedAt(a)
	if first == nil {
		t.Fatal("accepting the link did not set confirmed_at")
	}
	f.acceptLinkCookie(t, a)
	if again := confirmedAt(a); again == nil || !again.Equal(*first) {
		t.Errorf("confirmed_at changed on a second open: %v -> %v", first, again)
	}
	if confirmedAt(b) != nil {
		t.Error("an unrelated registration was confirmed")
	}

	// A is confirmed and counts again, B is fresh: the event is full.
	f.register(t, registerBody("C", uniqueEmail("c"), "yes", 1))
	if n := f.registrationCount(t); n != 2 {
		t.Errorf("rows = %d, want 2: a confirmed registration must keep counting", n)
	}
}

// Old unconfirmed registrations do not use up the per-event registration bound either.
func TestHandlePutPublicRSVP_HostedBoundIgnoresStaleUnconfirmed(t *testing.T) {
	f := openHostedFixture(t)
	if _, err := f.s.pool.Exec(context.Background(),
		`INSERT INTO rsvps (id, event_id, name, email, attending, count, answers, registration, created_at)
		 SELECT gen_random_uuid(), $1, 'Bulk', 'bulk' || g || '@example.invalid', 'yes', 1, '{}', true, now() - interval '30 hours'
		 FROM generate_series(1, $2::int) g`, f.eventID, maxRegistrationsPerEvent); err != nil {
		t.Fatalf("seed registrations: %v", err)
	}
	fresh := uniqueEmail("fresh")
	f.register(t, registerBody("Fresh", fresh, "yes", 1))
	var n int
	_ = f.s.pool.QueryRow(context.Background(), "SELECT count(*) FROM rsvps WHERE event_id = $1 AND email = $2", f.eventID, fresh).Scan(&n)
	if n != 1 {
		t.Errorf("fresh registration rows = %d, want 1: stale unconfirmed ones must not fill the bound", n)
	}
}
