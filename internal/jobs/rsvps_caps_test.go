package jobs

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/redis/go-redis/v9"
	"github.com/riverqueue/river"
	"github.com/riverqueue/river/rivertype"

	"github.com/ansonarose/seeyouthere-backend/internal/mail"
	"github.com/ansonarose/seeyouthere-backend/internal/ratelimit"
	"github.com/ansonarose/seeyouthere-backend/internal/store"
)

// Daily caps on registration confirmations: over a cap the mail is deferred to the next UTC day, not
// dropped; the per-recipient cap still drops it.

func newRegistration(t *testing.T, f rsvpConfirmationFixture, attending string) (id uuid.UUID, email string) {
	t.Helper()
	email = "reg-cap-" + uuid.Must(uuid.NewV7()).String() + "@example.invalid"
	count := int32(1)
	if attending == "no" {
		count = 0
	}
	rsvp, err := f.q.CreateOpenRSVP(context.Background(), store.CreateOpenRSVPParams{
		ID: uuid.Must(uuid.NewV7()), EventID: f.eventID, Name: "Reg Guest", Email: &email,
		Attending: attending, Count: count, Answers: []byte("{}"), Registration: true,
	})
	if err != nil {
		t.Fatalf("create registration: %v", err)
	}
	t.Cleanup(func() { _, _ = f.pool.Exec(context.Background(), "DELETE FROM rsvps WHERE id = $1", rsvp.ID) })
	return rsvp.ID, email
}

// capWorker builds a worker whose limiter is backed by rdb and whose global registration cap is
// globalCap. The shared global counter is reset before and after the test.
func capWorker(t *testing.T, f rsvpConfirmationFixture, rdb *redis.Client, sender mail.Sender, globalCap int64) *SendRSVPConfirmationWorker {
	t.Helper()
	w := newWorker(f, sender, sender)
	w.Limiter = ratelimit.New(rdb)
	w.RegistrationDailyCap = globalCap
	today := time.Now().UTC().Format("2006-01-02")
	globalKey := "rl:" + RegistrationConfirmGlobalKey(today)
	rdb.Del(context.Background(), globalKey)
	t.Cleanup(func() { rdb.Del(context.Background(), globalKey) })
	return w
}

// recipientHash is the per-recipient limiter key suffix the worker derives from an address.
func recipientHash(email string) string {
	sum := sha256.Sum256([]byte(email))
	return hex.EncodeToString(sum[:])
}

func counter(t *testing.T, rdb *redis.Client, key string) int64 {
	t.Helper()
	n, err := rdb.Get(context.Background(), "rl:"+key).Int64()
	if errors.Is(err, redis.Nil) {
		return 0
	}
	if err != nil {
		t.Fatalf("read counter %s: %v", key, err)
	}
	return n
}

func jobFor(id uuid.UUID, resend bool, createdAt time.Time) *river.Job[SendRSVPConfirmationArgs] {
	return &river.Job[SendRSVPConfirmationArgs]{
		JobRow: &rivertype.JobRow{CreatedAt: createdAt},
		Args:   SendRSVPConfirmationArgs{RSVPID: id, Resend: resend},
	}
}

// wantSnoozedPastMidnight asserts err is a River snooze that wakes the job just after the next UTC midnight.
func wantSnoozedPastMidnight(t *testing.T, err error) {
	t.Helper()
	var snooze *rivertype.JobSnoozeError
	if !errors.As(err, &snooze) {
		t.Fatalf("Work() error = %v, want a river snooze", err)
	}
	lo := untilNextUTCDay(time.Now()) + time.Minute - 5*time.Second
	hi := untilNextUTCDay(time.Now()) + time.Minute + snoozeJitter + 5*time.Second
	if snooze.Duration < lo || snooze.Duration > hi {
		t.Errorf("snooze = %v, want between %v and %v (just after the next UTC midnight)", snooze.Duration, lo, hi)
	}
}

func TestSendRSVPConfirmationWorker_RegistrationCapDefers(t *testing.T) {
	today := time.Now().UTC().Format("2006-01-02")
	tests := []struct {
		name    string
		prefill func(t *testing.T, f rsvpConfirmationFixture, rdb *redis.Client)
	}{
		{"global registration cap", func(t *testing.T, f rsvpConfirmationFixture, rdb *redis.Client) {
			l := ratelimit.New(rdb)
			if ok, err := l.AllowN(context.Background(), RegistrationConfirmGlobalKey(today), 5, 5, 48*time.Hour); err != nil || !ok {
				t.Fatalf("pre-fill global cap: ok=%v err=%v", ok, err)
			}
		}},
		{"per-event cap", func(t *testing.T, f rsvpConfirmationFixture, rdb *redis.Client) {
			l := ratelimit.New(rdb)
			if ok, err := l.AllowN(context.Background(), RSVPConfirmEventKey(f.eventID, today), MaxRegistrationConfirmationsPerEventPerDay, MaxRegistrationConfirmationsPerEventPerDay, 24*time.Hour); err != nil || !ok {
				t.Fatalf("pre-fill event cap: ok=%v err=%v", ok, err)
			}
		}},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			ctx := context.Background()
			f := newRSVPConfirmationFixture(t, ctx)
			rdb := rdbClient(t)
			id, email := newRegistration(t, f, "yes")
			sender := &recordingSender{}
			w := capWorker(t, f, rdb, sender, 5)
			t.Cleanup(func() { rdb.Del(ctx, "rl:"+RSVPConfirmEventKey(f.eventID, today)) })
			tc.prefill(t, f, rdb)
			eventBefore := counter(t, rdb, RSVPConfirmEventKey(f.eventID, today))
			globalBefore := counter(t, rdb, RegistrationConfirmGlobalKey(today))

			wantSnoozedPastMidnight(t, w.Work(ctx, jobFor(id, false, time.Now())))
			if msgs := sender.messages(); len(msgs) != 0 {
				t.Errorf("sent %d messages while over the cap, want none", len(msgs))
			}
			// Nothing stays consumed by the deferred attempt, so the retry starts from the same counts.
			if got := counter(t, rdb, RSVPConfirmEventKey(f.eventID, today)); got != eventBefore {
				t.Errorf("event counter = %d, want %d", got, eventBefore)
			}
			if got := counter(t, rdb, RegistrationConfirmGlobalKey(today)); got != globalBefore {
				t.Errorf("global counter = %d, want %d", got, globalBefore)
			}
			if got := counter(t, rdb, "rsvp_confirm:rcpt:"+recipientHash(email)); got != 0 {
				t.Errorf("recipient counter = %d, want 0", got)
			}

			// A job that has been deferred for too long is dropped instead of snoozed again.
			old := time.Now().Add(-registrationConfirmationMaxAge - time.Hour)
			if err := w.Work(ctx, jobFor(id, false, old)); err != nil {
				t.Fatalf("Work() for an expired job = %v, want nil (dropped)", err)
			}
			if msgs := sender.messages(); len(msgs) != 0 {
				t.Errorf("sent %d messages for a dropped job, want none", len(msgs))
			}
		})
	}
}

// Once the cap has room again (the next UTC day), the same job sends.
func TestSendRSVPConfirmationWorker_RegistrationSendsAndCountsGlobalAndEvent(t *testing.T) {
	ctx := context.Background()
	f := newRSVPConfirmationFixture(t, ctx)
	rdb := rdbClient(t)
	today := time.Now().UTC().Format("2006-01-02")
	id, _ := newRegistration(t, f, "yes")
	sender := &recordingSender{}
	w := capWorker(t, f, rdb, sender, 5)
	t.Cleanup(func() { rdb.Del(ctx, "rl:"+RSVPConfirmEventKey(f.eventID, today)) })
	// The invite / RSVP confirmation counter is not touched by registration mail.
	legacyKey := "rsvp_confirm:global:" + today
	legacyBefore := counter(t, rdb, legacyKey)

	if err := w.Work(ctx, jobFor(id, false, time.Now())); err != nil {
		t.Fatalf("Work() = %v", err)
	}
	if msgs := sender.messages(); len(msgs) != 1 {
		t.Fatalf("sent %d messages, want 1", len(msgs))
	}
	if got := counter(t, rdb, RegistrationConfirmGlobalKey(today)); got != 1 {
		t.Errorf("global registration counter = %d, want 1", got)
	}
	if got := counter(t, rdb, RSVPConfirmEventKey(f.eventID, today)); got != 1 {
		t.Errorf("event counter = %d, want 1", got)
	}
	if got := counter(t, rdb, legacyKey); got != legacyBefore {
		t.Errorf("shared invite/confirmation counter moved %d -> %d, want untouched", legacyBefore, got)
	}
}

// The per-recipient cap is the anti-bombing limit: it drops the mail (a retry would only mail the same
// third party again) and gives back the counters it took.
func TestSendRSVPConfirmationWorker_RegistrationRecipientCapDrops(t *testing.T) {
	ctx := context.Background()
	f := newRSVPConfirmationFixture(t, ctx)
	rdb := rdbClient(t)
	today := time.Now().UTC().Format("2006-01-02")
	id, email := newRegistration(t, f, "yes")
	sender := &recordingSender{}
	w := capWorker(t, f, rdb, sender, 5)
	t.Cleanup(func() { rdb.Del(ctx, "rl:"+RSVPConfirmEventKey(f.eventID, today)) })
	rcptKey := "rsvp_confirm:rcpt:" + recipientHash(email)
	t.Cleanup(func() { rdb.Del(ctx, "rl:"+rcptKey) })
	if ok, err := ratelimit.New(rdb).AllowN(ctx, rcptKey, maxRSVPConfirmationsPerRecipientPerDay, maxRSVPConfirmationsPerRecipientPerDay, 24*time.Hour); err != nil || !ok {
		t.Fatalf("pre-fill recipient cap: ok=%v err=%v", ok, err)
	}

	if err := w.Work(ctx, jobFor(id, false, time.Now())); err != nil {
		t.Fatalf("Work() = %v, want nil (dropped, not snoozed or failed)", err)
	}
	if msgs := sender.messages(); len(msgs) != 0 {
		t.Errorf("sent %d messages over the recipient cap, want none", len(msgs))
	}
	if got := counter(t, rdb, RegistrationConfirmGlobalKey(today)); got != 0 {
		t.Errorf("global counter = %d, want 0 (refunded)", got)
	}
	if got := counter(t, rdb, RSVPConfirmEventKey(f.eventID, today)); got != 0 {
		t.Errorf("event counter = %d, want 0 (refunded)", got)
	}
}

// A duplicate-registration re-send skips the per-event cap so recovery works on a flooded event.
func TestSendRSVPConfirmationWorker_RegistrationResendSkipsEventCap(t *testing.T) {
	ctx := context.Background()
	f := newRSVPConfirmationFixture(t, ctx)
	rdb := rdbClient(t)
	today := time.Now().UTC().Format("2006-01-02")
	id, _ := newRegistration(t, f, "yes")
	sender := &recordingSender{}
	w := capWorker(t, f, rdb, sender, 5)
	eventKey := RSVPConfirmEventKey(f.eventID, today)
	t.Cleanup(func() { rdb.Del(ctx, "rl:"+eventKey) })
	if ok, err := ratelimit.New(rdb).AllowN(ctx, eventKey, MaxRegistrationConfirmationsPerEventPerDay, MaxRegistrationConfirmationsPerEventPerDay, 24*time.Hour); err != nil || !ok {
		t.Fatalf("pre-fill event cap: ok=%v err=%v", ok, err)
	}

	if err := w.Work(ctx, jobFor(id, true, time.Now())); err != nil {
		t.Fatalf("Work() = %v", err)
	}
	if msgs := sender.messages(); len(msgs) != 1 {
		t.Errorf("sent %d messages for a re-send, want 1", len(msgs))
	}
	if got := counter(t, rdb, eventKey); got != MaxRegistrationConfirmationsPerEventPerDay {
		t.Errorf("event counter = %d, want it untouched at %d", got, MaxRegistrationConfirmationsPerEventPerDay)
	}
}

// A failed send gives back every counter it took, so the retry is not starved by its own attempts.
func TestSendRSVPConfirmationWorker_RegistrationFailedSendRefundsAll(t *testing.T) {
	ctx := context.Background()
	f := newRSVPConfirmationFixture(t, ctx)
	rdb := rdbClient(t)
	today := time.Now().UTC().Format("2006-01-02")
	id, email := newRegistration(t, f, "no")
	w := capWorker(t, f, rdb, failingSender{}, 5)
	t.Cleanup(func() { rdb.Del(ctx, "rl:"+RSVPConfirmEventKey(f.eventID, today)) })

	for range 3 {
		if err := w.Work(ctx, jobFor(id, false, time.Now())); err == nil {
			t.Fatal("Work() = nil, want the simulated send failure")
		}
	}
	for name, key := range map[string]string{
		"global":    RegistrationConfirmGlobalKey(today),
		"event":     RSVPConfirmEventKey(f.eventID, today),
		"recipient": "rsvp_confirm:rcpt:" + recipientHash(email),
	} {
		if got := counter(t, rdb, key); got != 0 {
			t.Errorf("%s counter = %d after failed sends, want 0", name, got)
		}
	}
}

// Only registration mail is subject to the registration cap; a plain RSVP is unaffected by it.
func TestSendRSVPConfirmationWorker_PlainRSVPIgnoresRegistrationCap(t *testing.T) {
	ctx := context.Background()
	f := newRSVPConfirmationFixture(t, ctx)
	rdb := rdbClient(t)
	email := "plain-" + uuid.Must(uuid.NewV7()).String() + "@example.invalid"
	rsvp, err := f.q.CreateOpenRSVP(ctx, store.CreateOpenRSVPParams{
		ID: uuid.Must(uuid.NewV7()), EventID: f.eventID, Name: "Plain", Email: &email,
		Attending: "no", Count: 0, Answers: []byte("{}"),
	})
	if err != nil {
		t.Fatalf("create rsvp: %v", err)
	}
	t.Cleanup(func() { _, _ = f.pool.Exec(context.Background(), "DELETE FROM rsvps WHERE id = $1", rsvp.ID) })
	sender := &recordingSender{}
	w := capWorker(t, f, rdb, sender, 1)
	t.Cleanup(func() { rdb.Del(ctx, "rl:rsvp_confirm:rcpt:"+recipientHash(email)) })
	today := time.Now().UTC().Format("2006-01-02")
	if ok, err := ratelimit.New(rdb).AllowN(ctx, RegistrationConfirmGlobalKey(today), 1, 1, 48*time.Hour); err != nil || !ok {
		t.Fatalf("pre-fill global cap: ok=%v err=%v", ok, err)
	}

	if err := w.Work(ctx, jobFor(rsvp.ID, false, time.Now())); err != nil {
		t.Fatalf("Work() = %v", err)
	}
	if msgs := sender.messages(); len(msgs) != 1 {
		t.Errorf("sent %d messages, want 1", len(msgs))
	}
}

func TestUntilNextUTCDay(t *testing.T) {
	tests := []struct {
		now  time.Time
		want time.Duration
	}{
		{time.Date(2026, 11, 19, 23, 30, 0, 0, time.UTC), 30 * time.Minute},
		{time.Date(2026, 11, 19, 0, 0, 0, 0, time.UTC), 24 * time.Hour},
		{time.Date(2026, 12, 31, 12, 0, 0, 0, time.UTC), 12 * time.Hour},
		// A non-UTC instant is measured against the UTC day: 23:30 UTC on the 19th is 00:30 on the 20th in Paris.
		{time.Date(2026, 11, 20, 0, 30, 0, 0, time.FixedZone("CET", 3600)), 30 * time.Minute},
	}
	for _, tc := range tests {
		if got := untilNextUTCDay(tc.now); got != tc.want {
			t.Errorf("untilNextUTCDay(%v) = %v, want %v", tc.now, got, tc.want)
		}
	}
}

// DeleteUnconfirmedRegistrations removes only registrations that were never confirmed or used, 48h
// after sign-up, in bounded batches.
func TestDeleteUnconfirmedRegistrations(t *testing.T) {
	ctx := context.Background()
	f := newRSVPConfirmationFixture(t, ctx)

	set := func(id uuid.UUID, created string, extra string) {
		t.Helper()
		if _, err := f.pool.Exec(ctx, "UPDATE rsvps SET created_at = now() - $2::interval"+extra+" WHERE id = $1", id, created); err != nil {
			t.Fatalf("update rsvp: %v", err)
		}
	}
	exists := func(id uuid.UUID) bool {
		t.Helper()
		var n int
		if err := f.pool.QueryRow(ctx, "SELECT count(*) FROM rsvps WHERE id = $1", id).Scan(&n); err != nil {
			t.Fatalf("count: %v", err)
		}
		return n == 1
	}

	staleA, _ := newRegistration(t, f, "yes")
	staleB, _ := newRegistration(t, f, "maybe")
	staleC, _ := newRegistration(t, f, "no")
	confirmed, _ := newRegistration(t, f, "yes")
	checkedIn, _ := newRegistration(t, f, "yes")
	young, _ := newRegistration(t, f, "yes")
	justUnder, _ := newRegistration(t, f, "yes")
	for _, id := range []uuid.UUID{staleA, staleB, staleC, confirmed, checkedIn} {
		set(id, "49 hours", "")
	}
	set(justUnder, "47 hours", "")
	set(confirmed, "49 hours", ", confirmed_at = now() - interval '40 hours'")
	set(checkedIn, "49 hours", ", checked_in_at = now(), checked_in_count = 1")

	plainEmail := "plain-old-" + uuid.Must(uuid.NewV7()).String() + "@example.invalid"
	plain, err := f.q.CreateOpenRSVP(ctx, store.CreateOpenRSVPParams{
		ID: uuid.Must(uuid.NewV7()), EventID: f.eventID, Name: "Plain", Email: &plainEmail,
		Attending: "yes", Count: 1, Answers: []byte("{}"),
	})
	if err != nil {
		t.Fatalf("create rsvp: %v", err)
	}
	t.Cleanup(func() { _, _ = f.pool.Exec(context.Background(), "DELETE FROM rsvps WHERE id = $1", plain.ID) })
	set(plain.ID, "49 hours", "")

	// A batch is bounded by its limit, oldest first.
	n, err := f.q.DeleteUnconfirmedRegistrations(ctx, 2)
	if err != nil {
		t.Fatalf("delete batch: %v", err)
	}
	if n != 2 {
		t.Fatalf("first batch deleted %d rows, want 2 (the limit)", n)
	}
	n, err = f.q.DeleteUnconfirmedRegistrations(ctx, 500)
	if err != nil {
		t.Fatalf("delete: %v", err)
	}
	if n != 1 {
		t.Errorf("second batch deleted %d rows, want 1", n)
	}
	for name, id := range map[string]uuid.UUID{"stale A": staleA, "stale B": staleB, "stale C": staleC} {
		if exists(id) {
			t.Errorf("%s: unconfirmed registration older than 48h survived the purge", name)
		}
	}
	for name, id := range map[string]uuid.UUID{
		"confirmed": confirmed, "checked in": checkedIn, "young": young, "under 48h": justUnder, "plain rsvp": plain.ID,
	} {
		if !exists(id) {
			t.Errorf("%s: purged, want kept", name)
		}
	}
}
