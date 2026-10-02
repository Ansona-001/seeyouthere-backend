package jobs

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"strings"
	"sync"
	"testing"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/redis/go-redis/v9"
	"github.com/riverqueue/river"

	"github.com/ansonarose/seeyouthere-backend/internal/content"
	"github.com/ansonarose/seeyouthere-backend/internal/mail"
	"github.com/ansonarose/seeyouthere-backend/internal/ratelimit"
	"github.com/ansonarose/seeyouthere-backend/internal/store"
	"github.com/ansonarose/seeyouthere-backend/internal/token"
)

// recordingSender is a fake mail.Sender that records every message it was
// asked to send, so tests can assert which of two senders (Sender vs
// CalendarSender) actually got used.
type recordingSender struct {
	mu   sync.Mutex
	sent []mail.Message
}

func (s *recordingSender) Send(_ context.Context, m mail.Message) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.sent = append(s.sent, m)
	return nil
}

func (s *recordingSender) messages() []mail.Message {
	s.mu.Lock()
	defer s.mu.Unlock()
	return append([]mail.Message(nil), s.sent...)
}

// failingSender is a fake mail.Sender that always errors, for exercising the
// rate-limit refund path on a failed send.
type failingSender struct{}

func (failingSender) Send(context.Context, mail.Message) error {
	return errors.New("smtp: simulated failure")
}

func dbPool(t *testing.T) *pgxpool.Pool {
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

func rdbClient(t *testing.T) *redis.Client {
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

// rsvpConfirmationFixture creates one throwaway published event (with a
// datetime block fixed at 2026-06-20 18:00-21:00 America/New_York and a
// location block) and an owner user, then returns everything a test needs
// to build RSVPs and a worker against it. Every row it creates is deleted
// in t.Cleanup.
type rsvpConfirmationFixture struct {
	q       *store.Queries
	pool    *pgxpool.Pool
	eventID uuid.UUID
	slug    string
	title   string
}

func newRSVPConfirmationFixture(t *testing.T, ctx context.Context) rsvpConfirmationFixture {
	t.Helper()
	pool := dbPool(t)
	q := store.New(pool)

	ownerID := uuid.Must(uuid.NewV7())
	email := "jobs-rsvp-test-" + ownerID.String() + "@example.invalid"
	if _, err := q.UpsertUserByEmail(ctx, store.UpsertUserByEmailParams{ID: ownerID, Email: email}); err != nil {
		t.Fatalf("create owner: %v", err)
	}
	// Registered immediately so a t.Fatalf anywhere below still cleans up
	// the user; the event's own cleanup (registered once it exists) runs
	// first (t.Cleanup is LIFO), so the owner_id FK never blocks it.
	t.Cleanup(func() {
		if _, err := pool.Exec(context.Background(), "DELETE FROM users WHERE id = $1", ownerID); err != nil {
			t.Logf("cleanup user %s: %v", ownerID, err)
		}
	})

	occRow, err := q.GetOccasion(ctx, "birthday")
	if err != nil {
		t.Fatalf("get occasion: %v", err)
	}
	occ, err := content.ParseOccasion(occRow)
	if err != nil {
		t.Fatalf("parse occasion: %v", err)
	}
	tmpl, err := q.GetPublishedTemplateBySlug(ctx, store.GetPublishedTemplateBySlugParams{Slug: "classic", AllowPremium: false})
	if err != nil {
		t.Fatalf("get template: %v", err)
	}

	title := "Sam's Test Party " + ownerID.String()[:8]
	raw := mustRawContent(t, []map[string]any{
		{"id": "hero1", "type": "hero", "kicker": "", "title": title, "subtitle": "", "image": nil},
		{
			"id": "dt1", "type": "datetime", "kicker": "", "heading": "",
			"start_local": "2026-06-20T18:00", "end_local": "2026-06-20T21:00",
			"timezone": "America/New_York", "all_day": false,
		},
		{
			"id": "loc1", "type": "location", "kicker": "", "heading": "",
			"name": "Test Venue", "address": "1 Test St", "map_url": "", "notes": "",
		},
	})
	saved, err := content.ValidateContent(raw, occ)
	if err != nil {
		t.Fatalf("validate content: %v", err)
	}

	eventID := uuid.Must(uuid.NewV7())
	event, err := q.CreateEvent(ctx, store.CreateEventParams{
		ID: eventID, OwnerID: &ownerID, OccasionSlug: "birthday", Title: saved.Title,
		Content: saved.JSON, Overrides: []byte("{}"), StartsAt: saved.StartsAt,
		TemplateID: tmpl.ID, TemplateVersion: tmpl.Version,
	})
	if err != nil {
		t.Fatalf("create event: %v", err)
	}
	t.Cleanup(func() {
		if _, err := pool.Exec(context.Background(), "DELETE FROM events WHERE id = $1", event.ID); err != nil {
			t.Logf("cleanup event %s: %v", event.ID, err)
		}
	})

	slug := "jobs-rsvp-test-" + eventID.String()[:8]
	if _, err := pool.Exec(ctx, `UPDATE events SET status = 'published', slug = $2, published_at = now() WHERE id = $1`, event.ID, slug); err != nil {
		t.Fatalf("publish event: %v", err)
	}

	return rsvpConfirmationFixture{q: q, pool: pool, eventID: event.ID, slug: slug, title: saved.Title}
}

func mustRawContent(t *testing.T, blocks []map[string]any) []byte {
	t.Helper()
	b, err := json.Marshal(blocks)
	if err != nil {
		t.Fatalf("marshal content: %v", err)
	}
	return b
}

func newWorker(f rsvpConfirmationFixture, sender, calendarSender mail.Sender) *SendRSVPConfirmationWorker {
	keys, err := token.NewKeys([]byte(strings.Repeat("a", 32)))
	if err != nil {
		panic(err)
	}
	return &SendRSVPConfirmationWorker{
		Queries:        f.q,
		Sender:         sender,
		CalendarSender: calendarSender,
		Tokens:         keys,
		Limiter:        nil, // set per test via rdb-backed limiter
		SiteURL:        "https://seeuthere.at",
	}
}

func TestSendRSVPConfirmationWorker_Yes_UsesCalendarSenderWithICS(t *testing.T) {
	ctx := context.Background()
	f := newRSVPConfirmationFixture(t, ctx)
	rdb := rdbClient(t)

	email := "guest-yes-" + uuid.Must(uuid.NewV7()).String() + "@example.invalid"
	rsvp, err := f.q.CreateOpenRSVP(ctx, store.CreateOpenRSVPParams{
		ID: uuid.Must(uuid.NewV7()), EventID: f.eventID, Name: "Yes Guest", Email: &email,
		Attending: "yes", Count: 2, Answers: []byte("{}"),
	})
	if err != nil {
		t.Fatalf("create rsvp: %v", err)
	}
	t.Cleanup(func() { _, _ = f.pool.Exec(context.Background(), "DELETE FROM rsvps WHERE id = $1", rsvp.ID) })

	sender := &recordingSender{}
	calendarSender := &recordingSender{}
	w := newWorker(f, sender, calendarSender)
	w.Limiter = ratelimit.New(rdb)

	if err := w.Work(ctx, &river.Job[SendRSVPConfirmationArgs]{Args: SendRSVPConfirmationArgs{RSVPID: rsvp.ID}}); err != nil {
		t.Fatalf("Work() error = %v", err)
	}

	if len(sender.messages()) != 0 {
		t.Errorf("regular Sender got %d messages, want 0", len(sender.messages()))
	}
	msgs := calendarSender.messages()
	if len(msgs) != 1 {
		t.Fatalf("CalendarSender got %d messages, want 1", len(msgs))
	}
	msg := msgs[0]
	if len(msg.Attachments) != 1 {
		t.Fatalf("got %d attachments, want 1", len(msg.Attachments))
	}
	att := msg.Attachments[0]
	if att.Filename != "event.ics" {
		t.Errorf("attachment filename = %q", att.Filename)
	}
	icsText := string(att.Content)
	if !strings.Contains(icsText, "SUMMARY:"+f.title) {
		t.Errorf("ics missing SUMMARY for %q:\n%s", f.title, icsText)
	}
	if !strings.Contains(icsText, "DTSTART:20260620T220000Z") { // 18:00 EDT == 22:00 UTC
		t.Errorf("ics missing expected DTSTART:\n%s", icsText)
	}
	if !strings.Contains(icsText, "LOCATION:Test Venue\\, 1 Test St") {
		t.Errorf("ics missing expected LOCATION:\n%s", icsText)
	}
}

func TestSendRSVPConfirmationWorker_No_UsesRegularSenderNoAttachment(t *testing.T) {
	ctx := context.Background()
	f := newRSVPConfirmationFixture(t, ctx)
	rdb := rdbClient(t)

	email := "guest-no-" + uuid.Must(uuid.NewV7()).String() + "@example.invalid"
	rsvp, err := f.q.CreateOpenRSVP(ctx, store.CreateOpenRSVPParams{
		ID: uuid.Must(uuid.NewV7()), EventID: f.eventID, Name: "No Guest", Email: &email,
		Attending: "no", Count: 0, Answers: []byte("{}"),
	})
	if err != nil {
		t.Fatalf("create rsvp: %v", err)
	}
	t.Cleanup(func() { _, _ = f.pool.Exec(context.Background(), "DELETE FROM rsvps WHERE id = $1", rsvp.ID) })

	sender := &recordingSender{}
	calendarSender := &recordingSender{}
	w := newWorker(f, sender, calendarSender)
	w.Limiter = ratelimit.New(rdb)

	if err := w.Work(ctx, &river.Job[SendRSVPConfirmationArgs]{Args: SendRSVPConfirmationArgs{RSVPID: rsvp.ID}}); err != nil {
		t.Fatalf("Work() error = %v", err)
	}

	if len(calendarSender.messages()) != 0 {
		t.Errorf("CalendarSender got %d messages, want 0", len(calendarSender.messages()))
	}
	msgs := sender.messages()
	if len(msgs) != 1 {
		t.Fatalf("regular Sender got %d messages, want 1", len(msgs))
	}
	if len(msgs[0].Attachments) != 0 {
		t.Errorf("got %d attachments, want 0", len(msgs[0].Attachments))
	}
	if strings.Contains(msgs[0].Text, "Add this to your calendar") {
		t.Errorf("no/maybe body should not mention the calendar invite:\n%s", msgs[0].Text)
	}
}

func TestSendRSVPConfirmationWorker_Yes_FallsBackWhenCalendarSenderNil(t *testing.T) {
	ctx := context.Background()
	f := newRSVPConfirmationFixture(t, ctx)
	rdb := rdbClient(t)

	email := "guest-yes-fallback-" + uuid.Must(uuid.NewV7()).String() + "@example.invalid"
	rsvp, err := f.q.CreateOpenRSVP(ctx, store.CreateOpenRSVPParams{
		ID: uuid.Must(uuid.NewV7()), EventID: f.eventID, Name: "Yes Fallback Guest", Email: &email,
		Attending: "yes", Count: 1, Answers: []byte("{}"),
	})
	if err != nil {
		t.Fatalf("create rsvp: %v", err)
	}
	t.Cleanup(func() { _, _ = f.pool.Exec(context.Background(), "DELETE FROM rsvps WHERE id = $1", rsvp.ID) })

	sender := &recordingSender{}
	w := newWorker(f, sender, nil) // no separate calendar identity configured
	w.Limiter = ratelimit.New(rdb)

	if err := w.Work(ctx, &river.Job[SendRSVPConfirmationArgs]{Args: SendRSVPConfirmationArgs{RSVPID: rsvp.ID}}); err != nil {
		t.Fatalf("Work() error = %v", err)
	}

	msgs := sender.messages()
	if len(msgs) != 1 {
		t.Fatalf("regular Sender got %d messages, want 1", len(msgs))
	}
	if len(msgs[0].Attachments) != 1 {
		t.Fatalf("got %d attachments, want 1 (even via fallback sender)", len(msgs[0].Attachments))
	}
}

// TestSendRSVPConfirmationWorker_FailedSendRefundsRecipientRateLimit checks
// that a Sender error doesn't permanently consume the recipient's daily
// quota: MaxAttempts retries this job up to 5 times, so without a refund on
// failure a misconfigured Sender would burn all maxRSVPConfirmationsPerRecipientPerDay
// slots on retries alone, without ever delivering anything, and lock the
// recipient out of their other confirmation emails for 24h.
func TestSendRSVPConfirmationWorker_FailedSendRefundsRecipientRateLimit(t *testing.T) {
	ctx := context.Background()
	f := newRSVPConfirmationFixture(t, ctx)
	rdb := rdbClient(t)

	email := "guest-refund-" + uuid.Must(uuid.NewV7()).String() + "@example.invalid"
	rsvp, err := f.q.CreateOpenRSVP(ctx, store.CreateOpenRSVPParams{
		ID: uuid.Must(uuid.NewV7()), EventID: f.eventID, Name: "Refund Guest", Email: &email,
		Attending: "no", Count: 0, Answers: []byte("{}"),
	})
	if err != nil {
		t.Fatalf("create rsvp: %v", err)
	}
	t.Cleanup(func() { _, _ = f.pool.Exec(context.Background(), "DELETE FROM rsvps WHERE id = $1", rsvp.ID) })

	failWorker := newWorker(f, failingSender{}, failingSender{})
	failWorker.Limiter = ratelimit.New(rdb)

	// Exhaust more than the daily cap's worth of attempts against a Sender
	// that always errors. If the rate limit were consumed (not refunded) on
	// each failure, this alone would use up the recipient's whole quota.
	for i := 0; i < maxRSVPConfirmationsPerRecipientPerDay+2; i++ {
		if err := failWorker.Work(ctx, &river.Job[SendRSVPConfirmationArgs]{Args: SendRSVPConfirmationArgs{RSVPID: rsvp.ID}}); err == nil {
			t.Fatalf("attempt %d: Work() error = nil, want the simulated send failure", i)
		}
	}

	// A working Sender should still be able to deliver: the quota must have
	// been refunded on every failed attempt above, not exhausted by them.
	okSender := &recordingSender{}
	okWorker := newWorker(f, okSender, okSender)
	okWorker.Limiter = ratelimit.New(rdb)
	if err := okWorker.Work(ctx, &river.Job[SendRSVPConfirmationArgs]{Args: SendRSVPConfirmationArgs{RSVPID: rsvp.ID}}); err != nil {
		t.Fatalf("Work() error = %v", err)
	}
	if msgs := okSender.messages(); len(msgs) != 1 {
		t.Fatalf("got %d messages after recovery, want 1 (quota should not have been exhausted by prior failures)", len(msgs))
	}
}
