package jobs

import (
	"context"
	"errors"
	"slices"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/riverqueue/river"

	"github.com/ansonarose/seeyouthere-backend/internal/mail"
	"github.com/ansonarose/seeyouthere-backend/internal/ratelimit"
	"github.com/ansonarose/seeyouthere-backend/internal/store"
)

// fakeDeleter is a mediaDeleter that records DeleteEventFiles calls, whether
// the event's row still existed at that moment (files must go first), and
// can be told to fail for chosen events.
type fakeDeleter struct {
	pool *pgxpool.Pool

	mu         sync.Mutex
	failFor    map[uuid.UUID]bool
	calls      []uuid.UUID
	rowPresent map[uuid.UUID]bool
}

func (d *fakeDeleter) DeleteMedia(uuid.UUID, uuid.UUID) error { return nil }

func (d *fakeDeleter) DeleteEventFiles(ctx context.Context, eventID uuid.UUID) error {
	var exists bool
	if err := d.pool.QueryRow(ctx, "SELECT EXISTS (SELECT 1 FROM events WHERE id = $1)", eventID).Scan(&exists); err != nil {
		return err
	}
	d.mu.Lock()
	defer d.mu.Unlock()
	d.calls = append(d.calls, eventID)
	if d.rowPresent == nil {
		d.rowPresent = map[uuid.UUID]bool{}
	}
	d.rowPresent[eventID] = exists
	if d.failFor[eventID] {
		return errors.New("simulated file delete failure")
	}
	return nil
}

type retentionFixture struct {
	pool    *pgxpool.Pool
	q       *store.Queries
	deleter *fakeDeleter
	worker  *CleanupWorker
	sender  *recordingSender
}

func newRetentionFixture(t *testing.T) retentionFixture {
	t.Helper()
	pool := dbPool(t)
	q := store.New(pool)
	sender := &recordingSender{}
	deleter := &fakeDeleter{pool: pool}
	// A real, never-started client: InsertManyTx needs it to know the
	// retention_reminder kind and to apply the args' InsertOpts.
	client, err := NewClient(pool, sender, sender, q, nil, nil, nil, "", "https://seeyouthere.at")
	if err != nil {
		t.Fatalf("NewClient: %v", err)
	}
	return retentionFixture{
		pool: pool, q: q, deleter: deleter, sender: sender,
		worker: &CleanupWorker{Queries: q, Pool: pool, Jobs: client, Media: deleter},
	}
}

func (f retentionFixture) newUser(t *testing.T) (uuid.UUID, string) {
	t.Helper()
	id := uuid.Must(uuid.NewV7())
	email := "jobs-retention-" + id.String() + "@example.invalid"
	if _, err := f.q.UpsertUserByEmail(context.Background(), store.UpsertUserByEmailParams{ID: id, Email: email}); err != nil {
		t.Fatalf("create user: %v", err)
	}
	t.Cleanup(func() {
		if _, err := f.pool.Exec(context.Background(), "DELETE FROM users WHERE id = $1", id); err != nil {
			t.Logf("cleanup user %s: %v", id, err)
		}
	})
	return id, email
}

type retentionEvent struct {
	owner *uuid.UUID
	title string
	tz    string
	// endsAt is the stored end; the start is two hours earlier.
	endsAt time.Time
	// createdAnchor keeps the retention_from CreateEvent derived
	// (greatest(ends_at, now())). By default it is set to endsAt, which
	// stands in for an event whose end passed on its own while it was live.
	createdAnchor bool
}

// newEvent inserts a draft event (and removes it, with its reports and river
// jobs, in t.Cleanup). A nil owner makes it an anonymous draft.
func (f retentionFixture) newEvent(t *testing.T, e retentionEvent) uuid.UUID {
	t.Helper()
	ctx := context.Background()
	tmpl, err := f.q.GetPublishedTemplateBySlug(ctx, store.GetPublishedTemplateBySlugParams{Slug: "classic", AllowPremium: false})
	if err != nil {
		t.Fatalf("get template: %v", err)
	}
	if e.title == "" {
		e.title = "Retention test"
	}
	content := mustRawContent(t, []map[string]any{{"id": "dt1", "type": "datetime", "timezone": e.tz}})
	starts := e.endsAt.Add(-2 * time.Hour)
	id := uuid.Must(uuid.NewV7())
	if _, err := f.q.CreateEvent(ctx, store.CreateEventParams{
		ID: id, OwnerID: e.owner, OccasionSlug: "birthday", Title: e.title, Content: content,
		Overrides: []byte("{}"), StartsAt: &starts, EndsAt: &e.endsAt,
		TemplateID: tmpl.ID, TemplateVersion: tmpl.Version,
	}); err != nil {
		t.Fatalf("create event: %v", err)
	}
	t.Cleanup(func() {
		bg := context.Background()
		if _, err := f.pool.Exec(bg, "DELETE FROM events WHERE id = $1", id); err != nil {
			t.Logf("cleanup event %s: %v", id, err)
		}
		if _, err := f.pool.Exec(bg, "DELETE FROM river_job WHERE kind = 'retention_reminder' AND args->>'event_id' = $1", id.String()); err != nil {
			t.Logf("cleanup jobs %s: %v", id, err)
		}
	})
	if !e.createdAnchor {
		f.exec(t, "UPDATE events SET retention_from = ends_at WHERE id = $1", id)
	}
	return id
}

// remind stamps the event as claimed at claimed and, when sent is non-nil,
// as delivered at sent.
func (f retentionFixture) remind(t *testing.T, id uuid.UUID, claimed time.Time, sent *time.Time) {
	t.Helper()
	f.exec(t, "UPDATE events SET retention_reminded_at = $2, retention_reminder_sent_at = $3 WHERE id = $1", id, claimed, sent)
}

// report files a report with the given status against the event.
func (f retentionFixture) report(t *testing.T, id uuid.UUID, status string) {
	t.Helper()
	f.exec(t, "INSERT INTO reports (id, event_id, reporter_ip_hash, reason, status) VALUES ($1, $2, $3, 'spam', $4)",
		uuid.Must(uuid.NewV7()), id, []byte(id.String()), status)
}

func (f retentionFixture) exec(t *testing.T, sql string, args ...any) {
	t.Helper()
	if _, err := f.pool.Exec(context.Background(), sql, args...); err != nil {
		t.Fatalf("exec %q: %v", sql, err)
	}
}

func (f retentionFixture) eventExists(t *testing.T, id uuid.UUID) bool {
	t.Helper()
	var ok bool
	if err := f.pool.QueryRow(context.Background(), "SELECT EXISTS (SELECT 1 FROM events WHERE id = $1)", id).Scan(&ok); err != nil {
		t.Fatalf("event exists: %v", err)
	}
	return ok
}

type reminderState struct {
	retentionFrom *time.Time
	reminded      *time.Time
	sent          *time.Time
	deleted       *time.Time
}

func (f retentionFixture) state(t *testing.T, id uuid.UUID) reminderState {
	t.Helper()
	var s reminderState
	if err := f.pool.QueryRow(context.Background(),
		"SELECT retention_from, retention_reminded_at, retention_reminder_sent_at, deleted_at FROM events WHERE id = $1", id,
	).Scan(&s.retentionFrom, &s.reminded, &s.sent, &s.deleted); err != nil {
		t.Fatalf("read state: %v", err)
	}
	return s
}

func (f retentionFixture) reminderJobs(t *testing.T, id uuid.UUID) (n int, queue string, maxAttempts int) {
	t.Helper()
	rows, err := f.pool.Query(context.Background(),
		"SELECT queue, max_attempts FROM river_job WHERE kind = 'retention_reminder' AND args->>'event_id' = $1", id.String())
	if err != nil {
		t.Fatalf("query jobs: %v", err)
	}
	defer rows.Close()
	for rows.Next() {
		n++
		if err := rows.Scan(&queue, &maxAttempts); err != nil {
			t.Fatalf("scan job: %v", err)
		}
	}
	if err := rows.Err(); err != nil {
		t.Fatalf("jobs rows: %v", err)
	}
	return n, queue, maxAttempts
}

const day = 24 * time.Hour

func TestRemindRetention(t *testing.T) {
	f := newRetentionFixture(t)
	ctx := context.Background()
	now := time.Now()
	owner, _ := f.newUser(t)

	due := f.newEvent(t, retentionEvent{owner: &owner, endsAt: now.Add(-28 * day)})
	tooRecent := f.newEvent(t, retentionEvent{owner: &owner, endsAt: now.Add(-26 * day)})
	takenDown := f.newEvent(t, retentionEvent{owner: &owner, endsAt: now.Add(-28 * day)})
	f.exec(t, "UPDATE events SET status = 'taken_down', slug = 'rt-' || id::text WHERE id = $1", takenDown)
	ownerless := f.newEvent(t, retentionEvent{endsAt: now.Add(-28 * day)})
	softDeleted := f.newEvent(t, retentionEvent{owner: &owner, endsAt: now.Add(-28 * day)})
	f.exec(t, "UPDATE events SET deleted_at = now() WHERE id = $1", softDeleted)
	// Back-dated at creation: its end is long past but its retention clock
	// started now, so it must not be reminded.
	backDated := f.newEvent(t, retentionEvent{owner: &owner, endsAt: now.Add(-60 * day), createdAnchor: true})
	openReport := f.newEvent(t, retentionEvent{owner: &owner, endsAt: now.Add(-28 * day)})
	f.report(t, openReport, "open")
	reviewing := f.newEvent(t, retentionEvent{owner: &owner, endsAt: now.Add(-28 * day)})
	f.report(t, reviewing, "reviewing")
	dismissed := f.newEvent(t, retentionEvent{owner: &owner, endsAt: now.Add(-28 * day)})
	f.report(t, dismissed, "dismissed")

	if _, err := f.worker.remindRetention(ctx, now); err != nil {
		t.Fatalf("remindRetention: %v", err)
	}
	// Second run: the claim is the dedupe, so nothing new is enqueued.
	if _, err := f.worker.remindRetention(ctx, now); err != nil {
		t.Fatalf("remindRetention (second): %v", err)
	}

	for name, id := range map[string]uuid.UUID{"due": due, "dismissed report": dismissed} {
		n, queue, maxAttempts := f.reminderJobs(t, id)
		if n != 1 || queue != "email" || maxAttempts != 12 {
			t.Errorf("%s: jobs = %d on %q with max_attempts %d, want 1 on email with 12", name, n, queue, maxAttempts)
		}
	}
	// The job args carry the claim stamp, so a superseded claim can be told apart.
	var stamp time.Time
	if err := f.pool.QueryRow(ctx,
		"SELECT (args->>'claimed_at')::timestamptz FROM river_job WHERE kind = 'retention_reminder' AND args->>'event_id' = $1",
		due.String()).Scan(&stamp); err != nil {
		t.Fatalf("read job stamp: %v", err)
	}
	if s := f.state(t, due); s.reminded == nil || !s.reminded.Equal(stamp) {
		t.Errorf("job claimed_at = %v, retention_reminded_at = %v, want equal", stamp, s.reminded)
	}

	for name, id := range map[string]uuid.UUID{
		"ended 26 days ago": tooRecent, "taken down": takenDown, "ownerless": ownerless, "soft-deleted": softDeleted,
		"back-dated at creation": backDated, "open report": openReport, "reviewing report": reviewing,
	} {
		if n, _, _ := f.reminderJobs(t, id); n != 0 {
			t.Errorf("%s: %d reminder jobs, want 0", name, n)
		}
		if s := f.state(t, id); s.reminded != nil {
			t.Errorf("%s: retention_reminded_at set, want NULL", name)
		}
	}
}

func TestCreateEvent_RetentionFrom(t *testing.T) {
	f := newRetentionFixture(t)
	now := time.Now()
	owner, _ := f.newUser(t)

	// A past end starts the retention window now; a future end keeps its own.
	past := f.newEvent(t, retentionEvent{owner: &owner, endsAt: now.Add(-60 * day), createdAnchor: true})
	future := f.newEvent(t, retentionEvent{owner: &owner, endsAt: now.Add(10 * day), createdAnchor: true})

	if s := f.state(t, past); s.retentionFrom == nil || time.Since(*s.retentionFrom).Abs() > time.Minute {
		t.Errorf("back-dated event retention_from = %v, want about now", s.retentionFrom)
	}
	var endsAt time.Time
	if err := f.pool.QueryRow(context.Background(), "SELECT ends_at FROM events WHERE id = $1", future).Scan(&endsAt); err != nil {
		t.Fatalf("read ends_at: %v", err)
	}
	if s := f.state(t, future); s.retentionFrom == nil || !s.retentionFrom.Equal(endsAt) {
		t.Errorf("future event retention_from = %v, want ends_at %v", s.retentionFrom, endsAt)
	}
}

func TestExpireEndedEvents(t *testing.T) {
	f := newRetentionFixture(t)
	ctx := context.Background()
	now := time.Now()
	owner, _ := f.newUser(t)

	ended := now.Add(-31 * day)
	at := func(ago time.Duration) *time.Time { t := now.Add(-ago); return &t }

	ready := f.newEvent(t, retentionEvent{owner: &owner, endsAt: ended})
	f.remind(t, ready, now.Add(-4*day), at(4*day))
	sentTooRecently := f.newEvent(t, retentionEvent{owner: &owner, endsAt: ended})
	f.remind(t, sentTooRecently, now.Add(-4*day), at(2*day))
	// Claimed 4 days ago but the email never went out: kept until the fallback.
	undelivered := f.newEvent(t, retentionEvent{owner: &owner, endsAt: ended})
	f.remind(t, undelivered, now.Add(-4*day), nil)
	undeliveredLong := f.newEvent(t, retentionEvent{owner: &owner, endsAt: ended})
	f.remind(t, undeliveredLong, now.Add(-11*day), nil)
	undeliveredFallbackNotYet := f.newEvent(t, retentionEvent{owner: &owner, endsAt: ended})
	f.remind(t, undeliveredFallbackNotYet, now.Add(-9*day), nil)
	neverReminded := f.newEvent(t, retentionEvent{owner: &owner, endsAt: ended})
	notEndedLongEnough := f.newEvent(t, retentionEvent{owner: &owner, endsAt: now.Add(-29 * day)})
	f.remind(t, notEndedLongEnough, now.Add(-4*day), at(4*day))
	openReport := f.newEvent(t, retentionEvent{owner: &owner, endsAt: ended})
	f.remind(t, openReport, now.Add(-4*day), at(4*day))
	f.report(t, openReport, "open")
	dismissed := f.newEvent(t, retentionEvent{owner: &owner, endsAt: ended})
	f.remind(t, dismissed, now.Add(-4*day), at(4*day))
	f.report(t, dismissed, "dismissed")
	// The end is long past but the clock restarted when the end was edited
	// back, so a stale reminder state cannot expire it.
	backDated := f.newEvent(t, retentionEvent{owner: &owner, endsAt: now.Add(-60 * day), createdAnchor: true})
	f.remind(t, backDated, now.Add(-4*day), at(4*day))

	ids, err := f.worker.expireEndedEvents(ctx, now)
	if err != nil {
		t.Fatalf("expireEndedEvents: %v", err)
	}
	for name, id := range map[string]uuid.UUID{
		"delivered 4d ago": ready, "undelivered, claimed 11d ago": undeliveredLong, "dismissed report": dismissed,
	} {
		if !slices.Contains(ids, id) {
			t.Errorf("%s: not expired, want expired; got %v", name, ids)
		}
	}
	for name, id := range map[string]uuid.UUID{
		"delivered 2d ago": sentTooRecently, "undelivered, claimed 4d ago": undelivered,
		"undelivered, claimed 9d ago": undeliveredFallbackNotYet, "never reminded": neverReminded,
		"ended 29d ago": notEndedLongEnough, "open report": openReport, "back-dated": backDated,
	} {
		if slices.Contains(ids, id) {
			t.Errorf("%s: expired, want kept", name)
		}
	}
}

func TestPurgeWithFiles(t *testing.T) {
	f := newRetentionFixture(t)
	ctx := context.Background()
	now := time.Now()
	owner, _ := f.newUser(t)
	future := now.Add(30 * day)
	sent := now.Add(-4 * day)

	// Soft-deleted 31 days ago (still-future end) -> purgeable.
	userDeletedOld := f.newEvent(t, retentionEvent{owner: &owner, endsAt: future})
	f.exec(t, "UPDATE events SET deleted_at = $2 WHERE id = $1", userDeletedOld, now.Add(-31*day))
	// Soft-deleted yesterday with a future end -> kept.
	userDeletedRecent := f.newEvent(t, retentionEvent{owner: &owner, endsAt: future})
	f.exec(t, "UPDATE events SET deleted_at = $2 WHERE id = $1", userDeletedRecent, now.Add(-day))
	// Owner deleted it yesterday although its end passed long ago and it was
	// never reminded: it keeps the normal deleted_at grace.
	ownerDeletedPastEnd := f.newEvent(t, retentionEvent{owner: &owner, endsAt: now.Add(-60 * day)})
	f.exec(t, "UPDATE events SET deleted_at = $2 WHERE id = $1", ownerDeletedPastEnd, now.Add(-day))
	// Expired by retention (ended 31d ago, reminded 4d ago, soft-deleted just now) -> purgeable.
	retentionExpired := f.newEvent(t, retentionEvent{owner: &owner, endsAt: now.Add(-31 * day)})
	f.remind(t, retentionExpired, now.Add(-4*day), &sent)
	f.exec(t, "UPDATE events SET deleted_at = now() WHERE id = $1", retentionExpired)
	// Reminded only 2 days ago, so not yet retention-expired -> kept.
	remindedRecently := f.newEvent(t, retentionEvent{owner: &owner, endsAt: now.Add(-31 * day)})
	f.remind(t, remindedRecently, now.Add(-2*day), nil)
	f.exec(t, "UPDATE events SET deleted_at = now() WHERE id = $1", remindedRecently)
	// Taken down and deleted long ago -> kept as moderation evidence.
	takenDown := f.newEvent(t, retentionEvent{owner: &owner, endsAt: future})
	f.exec(t, "UPDATE events SET deleted_at = $2, status = 'taken_down', slug = 'rt-' || id::text WHERE id = $1", takenDown, now.Add(-40*day))
	// Deleted long ago but with an open report -> kept for moderation.
	openReport := f.newEvent(t, retentionEvent{owner: &owner, endsAt: future})
	f.exec(t, "UPDATE events SET deleted_at = $2 WHERE id = $1", openReport, now.Add(-40*day))
	f.report(t, openReport, "open")
	// Not deleted at all -> kept.
	live := f.newEvent(t, retentionEvent{owner: &owner, endsAt: now.Add(-40 * day)})
	f.remind(t, live, now.Add(-10*day), &sent)
	// Purgeable, but its files fail to delete.
	failing := f.newEvent(t, retentionEvent{owner: &owner, endsAt: future})
	f.exec(t, "UPDATE events SET deleted_at = $2 WHERE id = $1", failing, now.Add(-31*day))
	f.deleter.failFor = map[uuid.UUID]bool{failing: true}

	listed, err := f.q.ListPurgeableEvents(ctx, store.ListPurgeableEventsParams{
		DeletedBefore: now.Add(-deletedEventGrace), RemindedBefore: now.Add(-retentionReminderLead),
		EndedBefore: now.Add(-retentionAfterEnd), Lim: 1000,
	})
	if err != nil {
		t.Fatalf("ListPurgeableEvents: %v", err)
	}
	for name, want := range map[string]struct {
		id   uuid.UUID
		want bool
	}{
		"deleted 31d ago":                {userDeletedOld, true},
		"deleted 1d ago":                 {userDeletedRecent, false},
		"owner-deleted, end 60d ago":     {ownerDeletedPastEnd, false},
		"retention-expired":              {retentionExpired, true},
		"ended 31d ago, reminded 2d ago": {remindedRecently, false},
		"taken down":                     {takenDown, false},
		"deleted 40d ago, open report":   {openReport, false},
		"not deleted":                    {live, false},
	} {
		if got := slices.Contains(listed, want.id); got != want.want {
			t.Errorf("%s: purgeable = %v, want %v", name, got, want.want)
		}
	}

	// PurgeEvents re-checks the report even for an id handed to it directly.
	if n, err := f.q.PurgeEvents(ctx, []uuid.UUID{openReport}); err != nil || n != 0 {
		t.Fatalf("PurgeEvents(open report) = %d, %v, want 0 rows", n, err)
	}
	if !f.eventExists(t, openReport) {
		t.Error("event with an open report was hard-deleted")
	}

	// Purge only this test's events, so an old dev database is untouched.
	ids := []uuid.UUID{userDeletedOld, retentionExpired, failing}
	purged, failed, err := f.worker.purgeWithFiles(ctx, ids, f.q.PurgeEvents)
	if err != nil {
		t.Fatalf("purgeWithFiles: %v", err)
	}
	if purged != 2 || failed != 1 {
		t.Errorf("purged = %d, failed = %d, want 2 and 1", purged, failed)
	}
	for id, want := range map[uuid.UUID]bool{userDeletedOld: false, retentionExpired: false, failing: true} {
		if got := f.eventExists(t, id); got != want {
			t.Errorf("event %s exists = %v, want %v", id, got, want)
		}
	}
	if len(f.deleter.calls) != len(ids) {
		t.Errorf("DeleteEventFiles called %d times, want %d", len(f.deleter.calls), len(ids))
	}
	for id, present := range f.deleter.rowPresent {
		if !present {
			t.Errorf("event %s: row already gone when its files were deleted (rows must go second)", id)
		}
	}

	t.Run("cancelled context deletes nothing more", func(t *testing.T) {
		cctx, cancel := context.WithCancel(ctx)
		cancel()
		purged, _, err := f.worker.purgeWithFiles(cctx, []uuid.UUID{failing}, f.q.PurgeEvents)
		if !errors.Is(err, context.Canceled) || purged != 0 {
			t.Errorf("purged = %d, err = %v, want 0 and context.Canceled", purged, err)
		}
	})
}

func TestPurgeWithFiles_ExpiredAnonDrafts(t *testing.T) {
	f := newRetentionFixture(t)
	ctx := context.Background()
	now := time.Now()

	newDraft := func(expires time.Time) uuid.UUID {
		id := f.newEvent(t, retentionEvent{endsAt: now.Add(30 * day)})
		if err := f.q.CreateAnonDraft(ctx, store.CreateAnonDraftParams{
			ID: uuid.Must(uuid.NewV7()), CookieHash: []byte("retention-test-cookie-" + id.String()),
			EventID: id, ExpiresAt: expires,
		}); err != nil {
			t.Fatalf("create anon draft: %v", err)
		}
		return id
	}
	expired := newDraft(now.Add(-time.Hour))
	fresh := newDraft(now.Add(time.Hour))

	ids, err := f.q.ListExpiredAnonDraftEvents(ctx, 1000)
	if err != nil {
		t.Fatalf("ListExpiredAnonDraftEvents: %v", err)
	}
	if !slices.Contains(ids, expired) || slices.Contains(ids, fresh) {
		t.Fatalf("listed = %v, want it to contain only the expired draft", ids)
	}

	purged, failed, err := f.worker.purgeWithFiles(ctx, []uuid.UUID{expired}, f.q.DeleteExpiredAnonDraftEvents)
	if err != nil || purged != 1 || failed != 0 {
		t.Fatalf("purged = %d, failed = %d, err = %v, want 1, 0, nil", purged, failed, err)
	}
	if f.eventExists(t, expired) {
		t.Error("expired draft row still exists")
	}
	if !f.eventExists(t, fresh) {
		t.Error("unexpired draft was deleted")
	}
	if len(f.deleter.calls) != 1 || f.deleter.calls[0] != expired || !f.deleter.rowPresent[expired] {
		t.Errorf("file deletes = %v (row present: %v), want one for the expired draft before its row went", f.deleter.calls, f.deleter.rowPresent)
	}
}

// failOnceSender fails its first Send, then records like recordingSender.
type failOnceSender struct {
	recordingSender
	failed atomic.Bool
}

func (s *failOnceSender) Send(ctx context.Context, m mail.Message) error {
	if !s.failed.Swap(true) {
		return errors.New("smtp: simulated failure")
	}
	return s.recordingSender.Send(ctx, m)
}

func TestRetentionReminderWorker(t *testing.T) {
	f := newRetentionFixture(t)
	ctx := context.Background()
	rdb := rdbClient(t)
	limiter := ratelimit.New(rdb)
	const globalKey = "rl:" + retentionGlobalLimitKey
	rdb.Del(ctx, globalKey)
	t.Cleanup(func() { rdb.Del(context.Background(), globalKey) })
	owner, ownerEmail := f.newUser(t)

	endsAt := time.Date(2026, 1, 10, 23, 30, 0, 0, time.UTC)
	// reminded 26 days after the end: deletion is ended+30d.
	remindedOnTime := endsAt.Add(26 * day)
	// reminded 29 days after the end: deletion is reminded+3d.
	remindedLate := endsAt.Add(29 * day)
	sentAt := remindedOnTime.Add(time.Minute)

	tests := []struct {
		name     string
		tz       string
		title    string
		reminded *time.Time
		sent     *time.Time
		stamp    *time.Time // claim stamp in the job args; default = reminded
		mutate   string     // optional SQL run with the event id as $1
		wantSend bool
		wantDate string
	}{
		{name: "event timezone", tz: "Pacific/Auckland", reminded: &remindedOnTime, wantSend: true, wantDate: "Tuesday, 10 February 2026"},
		{name: "no timezone falls back to UTC", tz: "", reminded: &remindedOnTime, wantSend: true, wantDate: "Monday, 9 February 2026"},
		{name: "unknown timezone falls back to UTC", tz: "Not/AZone", reminded: &remindedOnTime, wantSend: true, wantDate: "Monday, 9 February 2026"},
		{name: "Local falls back to UTC", tz: "Local", reminded: &remindedOnTime, wantSend: true, wantDate: "Monday, 9 February 2026"},
		{name: "late reminder pushes the date", tz: "UTC", reminded: &remindedLate, wantSend: true, wantDate: "Wednesday, 11 February 2026"},
		{name: "CR and LF stripped from title", tz: "UTC", title: "Party\r\nBcc: evil@example.invalid", reminded: &remindedOnTime, wantSend: true, wantDate: "Monday, 9 February 2026"},
		{name: "not reminded", tz: "UTC", reminded: nil},
		{name: "stale claim stamp", tz: "UTC", reminded: &remindedOnTime, stamp: &remindedLate},
		{name: "already sent", tz: "UTC", reminded: &remindedOnTime, sent: &sentAt},
		{name: "soft-deleted event", tz: "UTC", reminded: &remindedOnTime, mutate: "UPDATE events SET deleted_at = now() WHERE id = $1"},
		{name: "taken-down event", tz: "UTC", reminded: &remindedOnTime, mutate: "UPDATE events SET status = 'taken_down', slug = 'rt-' || id::text WHERE id = $1"},
		{name: "owner deleted", tz: "UTC", reminded: &remindedOnTime, mutate: "UPDATE users SET deleted_at = now() WHERE id = (SELECT owner_id FROM events WHERE id = $1)"},
		// What an editor save that moves ends_at does (UpdateEventContent).
		{name: "re-dated to the future", tz: "UTC", reminded: &remindedOnTime, mutate: "UPDATE events SET starts_at = now() + interval '10 days', ends_at = now() + interval '10 days' + interval '2 hours', retention_from = now() + interval '10 days' + interval '2 hours', retention_reminded_at = NULL, retention_reminder_sent_at = NULL WHERE id = $1"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			// Each case gets its own owner, so the owner_deleted case can't
			// affect the others (or their rate-limit counters).
			caseOwner := owner
			if tt.name == "owner deleted" {
				caseOwner, _ = f.newUser(t)
			}
			id := f.newEvent(t, retentionEvent{owner: &caseOwner, tz: tt.tz, title: tt.title, endsAt: endsAt})
			args := RetentionReminderArgs{EventID: id}
			if tt.reminded != nil {
				f.remind(t, id, *tt.reminded, tt.sent)
				args.ClaimedAt = *tt.reminded
			}
			if tt.stamp != nil {
				args.ClaimedAt = *tt.stamp
			}
			if tt.mutate != "" {
				f.exec(t, tt.mutate, id)
			}

			sender := &recordingSender{}
			w := &RetentionReminderWorker{Queries: f.q, Sender: sender, Limiter: limiter, SiteURL: "https://seeyouthere.at"}
			t.Cleanup(func() { rdb.Del(ctx, "rl:retention:owner:"+caseOwner.String()) })
			if err := w.Work(ctx, &river.Job[RetentionReminderArgs]{Args: args}); err != nil {
				t.Fatalf("Work: %v", err)
			}
			msgs := sender.messages()
			if !tt.wantSend {
				if len(msgs) != 0 {
					t.Fatalf("sent %d messages, want none", len(msgs))
				}
				return
			}
			if len(msgs) != 1 {
				t.Fatalf("sent %d messages, want 1", len(msgs))
			}
			m := msgs[0]
			if m.To != ownerEmail {
				t.Errorf("To = %q, want owner %q", m.To, ownerEmail)
			}
			if !strings.Contains(m.Text, "on or after "+tt.wantDate+".") {
				t.Errorf("text = %q, want it to contain %q", m.Text, "on or after "+tt.wantDate+".")
			}
			if strings.ContainsAny(m.Subject, "\r\n") {
				t.Errorf("subject %q contains CR/LF", m.Subject)
			}
			if tt.title != "" && !strings.Contains(m.Subject, "PartyBcc: evil@example.invalid") {
				t.Errorf("subject = %q, want CR/LF removed but text kept", m.Subject)
			}
			if s := f.state(t, id); s.sent == nil {
				t.Error("retention_reminder_sent_at not set after a successful send")
			}
		})
	}

	t.Run("missing event", func(t *testing.T) {
		sender := &recordingSender{}
		w := &RetentionReminderWorker{Queries: f.q, Sender: sender, Limiter: limiter, SiteURL: "https://seeyouthere.at"}
		if err := w.Work(ctx, &river.Job[RetentionReminderArgs]{Args: RetentionReminderArgs{EventID: uuid.Must(uuid.NewV7()), ClaimedAt: remindedOnTime}}); err != nil {
			t.Fatalf("Work: %v", err)
		}
		if n := len(sender.messages()); n != 0 {
			t.Errorf("sent %d messages, want none", n)
		}
	})

	t.Run("duplicate job sends once", func(t *testing.T) {
		caseOwner, _ := f.newUser(t)
		t.Cleanup(func() { rdb.Del(ctx, "rl:retention:owner:"+caseOwner.String()) })
		id := f.newEvent(t, retentionEvent{owner: &caseOwner, tz: "UTC", endsAt: endsAt})
		f.remind(t, id, remindedOnTime, nil)
		sender := &recordingSender{}
		w := &RetentionReminderWorker{Queries: f.q, Sender: sender, Limiter: limiter, SiteURL: "https://seeyouthere.at"}
		job := &river.Job[RetentionReminderArgs]{Args: RetentionReminderArgs{EventID: id, ClaimedAt: remindedOnTime}}
		for range 3 {
			if err := w.Work(ctx, job); err != nil {
				t.Fatalf("Work: %v", err)
			}
		}
		if n := len(sender.messages()); n != 1 {
			t.Errorf("sent %d messages for 3 runs of one claim, want 1", n)
		}
	})

	t.Run("failed send is retried and counted once", func(t *testing.T) {
		caseOwner, _ := f.newUser(t)
		key := "retention:owner:" + caseOwner.String()
		t.Cleanup(func() { rdb.Del(ctx, "rl:"+key) })
		id := f.newEvent(t, retentionEvent{owner: &caseOwner, tz: "UTC", endsAt: endsAt})
		f.remind(t, id, remindedOnTime, nil)
		sender := &failOnceSender{}
		w := &RetentionReminderWorker{Queries: f.q, Sender: sender, Limiter: limiter, SiteURL: "https://seeyouthere.at"}
		job := &river.Job[RetentionReminderArgs]{Args: RetentionReminderArgs{EventID: id, ClaimedAt: remindedOnTime}}

		if err := w.Work(ctx, job); err == nil {
			t.Fatal("Work with a failing sender returned nil, want an error for River to retry")
		}
		if s := f.state(t, id); s.sent != nil {
			t.Error("retention_reminder_sent_at set after a failed send, want NULL so the retry can send")
		}
		if got := rdb.Get(ctx, "rl:"+key).Val(); got != "0" {
			t.Errorf("owner counter after failed send = %q, want 0 (refunded)", got)
		}
		if err := w.Work(ctx, job); err != nil {
			t.Fatalf("retry: %v", err)
		}
		if n := len(sender.messages()); n != 1 {
			t.Errorf("sent %d messages after retry, want 1", n)
		}
		if s := f.state(t, id); s.sent == nil {
			t.Error("retention_reminder_sent_at not set after the retry succeeded")
		}
	})

	t.Run("per-owner limit snoozes instead of dropping", func(t *testing.T) {
		caseOwner, _ := f.newUser(t)
		key := "rl:retention:owner:" + caseOwner.String()
		t.Cleanup(func() { rdb.Del(ctx, key) })
		sender := &recordingSender{}
		w := &RetentionReminderWorker{Queries: f.q, Sender: sender, Limiter: limiter, SiteURL: "https://seeyouthere.at"}
		rdb.Del(ctx, globalKey)

		// An owner with more due events than the limit: the extra jobs snooze.
		now := time.Now()
		const extra = 2
		var snoozed []uuid.UUID
		sentCount := 0
		for i := 0; i < maxRetentionRemindersPerOwnerPerDay+extra; i++ {
			id := f.newEvent(t, retentionEvent{owner: &caseOwner, tz: "UTC", endsAt: now.Add(-31 * day)})
			f.remind(t, id, now.Add(-4*day), nil)
			err := w.Work(ctx, &river.Job[RetentionReminderArgs]{Args: RetentionReminderArgs{EventID: id, ClaimedAt: now.Add(-4 * day)}})
			var snooze *river.JobSnoozeError
			switch {
			case err == nil:
				sentCount++
			case errors.As(err, &snooze):
				if snooze.Duration != 24*time.Hour {
					t.Errorf("snooze = %v, want 24h", snooze.Duration)
				}
				snoozed = append(snoozed, id)
			default:
				t.Fatalf("Work: %v", err)
			}
		}
		if n := len(sender.messages()); n != maxRetentionRemindersPerOwnerPerDay || sentCount != n {
			t.Errorf("sent %d (completed %d), want %d", n, sentCount, maxRetentionRemindersPerOwnerPerDay)
		}
		if len(snoozed) != extra {
			t.Fatalf("snoozed %d jobs, want %d", len(snoozed), extra)
		}
		for _, id := range snoozed {
			if s := f.state(t, id); s.sent != nil || s.reminded == nil {
				t.Errorf("snoozed event: claim = %v, sent = %v, want the claim kept and nothing marked sent", s.reminded, s.sent)
			}
		}

		// The claim was delivered nowhere, so expiry waits: not at day 4 of
		// the claim, but via the fallback at day 10.
		expired, err := f.worker.expireEndedEvents(ctx, now)
		if err != nil {
			t.Fatalf("expireEndedEvents: %v", err)
		}
		for _, id := range snoozed {
			if slices.Contains(expired, id) {
				t.Errorf("snoozed event %s expired 4 days after the claim, want kept", id)
			}
		}
		expired, err = f.worker.expireEndedEvents(ctx, now.Add(7*day))
		if err != nil {
			t.Fatalf("expireEndedEvents (fallback): %v", err)
		}
		for _, id := range snoozed {
			if !slices.Contains(expired, id) {
				t.Errorf("snoozed event %s not expired 11 days after the claim, want expired via the fallback", id)
			}
		}

		// Once the event is gone, a snoozed job finds no row and completes.
		if err := w.Work(ctx, &river.Job[RetentionReminderArgs]{Args: RetentionReminderArgs{EventID: snoozed[0], ClaimedAt: now.Add(-4 * day)}}); err != nil {
			t.Errorf("job for an expired event = %v, want nil", err)
		}
	})

	t.Run("global limit snoozes and leaves the owner quota alone", func(t *testing.T) {
		caseOwner, _ := f.newUser(t)
		ownerKey := "rl:retention:owner:" + caseOwner.String()
		t.Cleanup(func() { rdb.Del(ctx, ownerKey) })
		id := f.newEvent(t, retentionEvent{owner: &caseOwner, tz: "UTC", endsAt: endsAt})
		f.remind(t, id, remindedOnTime, nil)
		if err := rdb.Set(ctx, globalKey, maxRetentionRemindersPerHour, time.Hour).Err(); err != nil {
			t.Fatalf("seed limiter: %v", err)
		}
		sender := &recordingSender{}
		w := &RetentionReminderWorker{Queries: f.q, Sender: sender, Limiter: limiter, SiteURL: "https://seeyouthere.at"}
		job := &river.Job[RetentionReminderArgs]{Args: RetentionReminderArgs{EventID: id, ClaimedAt: remindedOnTime}}

		var snooze *river.JobSnoozeError
		if err := w.Work(ctx, job); !errors.As(err, &snooze) {
			t.Fatalf("Work at the global limit = %v, want a snooze", err)
		}
		if n := len(sender.messages()); n != 0 {
			t.Errorf("sent %d messages at the global limit, want none", n)
		}
		if s := f.state(t, id); s.sent != nil || s.reminded == nil {
			t.Errorf("claim = %v, sent = %v, want the claim kept and nothing marked sent", s.reminded, s.sent)
		}
		if n, err := rdb.Exists(ctx, ownerKey).Result(); err != nil || n != 0 {
			t.Errorf("owner counter exists = %d (err %v), want it untouched", n, err)
		}

		// One global slot left: the same job now sends.
		if err := rdb.Set(ctx, globalKey, maxRetentionRemindersPerHour-1, time.Hour).Err(); err != nil {
			t.Fatalf("seed limiter: %v", err)
		}
		if err := w.Work(ctx, job); err != nil {
			t.Fatalf("Work: %v", err)
		}
		if n := len(sender.messages()); n != 1 {
			t.Errorf("sent %d messages with one global slot left, want 1", n)
		}
	})

	t.Run("owner denial refunds the global slot", func(t *testing.T) {
		caseOwner, _ := f.newUser(t)
		ownerKey := "rl:retention:owner:" + caseOwner.String()
		t.Cleanup(func() { rdb.Del(ctx, ownerKey) })
		id := f.newEvent(t, retentionEvent{owner: &caseOwner, tz: "UTC", endsAt: endsAt})
		f.remind(t, id, remindedOnTime, nil)
		if err := rdb.Set(ctx, ownerKey, maxRetentionRemindersPerOwnerPerDay, time.Hour).Err(); err != nil {
			t.Fatalf("seed limiter: %v", err)
		}
		rdb.Del(ctx, globalKey)
		before := rdb.Get(ctx, globalKey).Val()
		w := &RetentionReminderWorker{Queries: f.q, Sender: &recordingSender{}, Limiter: limiter, SiteURL: "https://seeyouthere.at"}
		var snooze *river.JobSnoozeError
		if err := w.Work(ctx, &river.Job[RetentionReminderArgs]{Args: RetentionReminderArgs{EventID: id, ClaimedAt: remindedOnTime}}); !errors.As(err, &snooze) {
			t.Fatalf("Work at the owner limit = %v, want a snooze", err)
		}
		if after := rdb.Get(ctx, globalKey).Val(); after != before && after != "0" {
			t.Errorf("global counter %q -> %q, want it refunded", before, after)
		}
	})

	t.Run("timed-out send is treated as possibly delivered", func(t *testing.T) {
		caseOwner, _ := f.newUser(t)
		ownerKey := "retention:owner:" + caseOwner.String()
		t.Cleanup(func() { rdb.Del(ctx, "rl:"+ownerKey) })
		id := f.newEvent(t, retentionEvent{owner: &caseOwner, tz: "UTC", endsAt: endsAt})
		f.remind(t, id, remindedOnTime, nil)
		sender := &errSender{err: context.DeadlineExceeded}
		w := &RetentionReminderWorker{Queries: f.q, Sender: sender, Limiter: limiter, SiteURL: "https://seeyouthere.at"}
		job := &river.Job[RetentionReminderArgs]{Args: RetentionReminderArgs{EventID: id, ClaimedAt: remindedOnTime}}

		if err := w.Work(ctx, job); err != nil {
			t.Fatalf("Work after a send timeout = %v, want nil (no retry)", err)
		}
		if s := f.state(t, id); s.sent == nil {
			t.Error("retention_reminder_sent_at cleared after a timeout, want it kept")
		}
		if got := rdb.Get(ctx, "rl:"+ownerKey).Val(); got != "1" {
			t.Errorf("owner counter = %q, want 1 (not refunded)", got)
		}
		// A duplicate run finds the mark and does not try to send again.
		if err := w.Work(ctx, job); err != nil {
			t.Fatalf("second Work: %v", err)
		}
		if sender.calls.Load() != 1 {
			t.Errorf("Send called %d times, want 1", sender.calls.Load())
		}
	})
}

// errSender always fails with err and counts the calls.
type errSender struct {
	err   error
	calls atomic.Int32
}

func (s *errSender) Send(context.Context, mail.Message) error {
	s.calls.Add(1)
	return s.err
}

func TestClaimAnonDrafts_RetentionFrom(t *testing.T) {
	f := newRetentionFixture(t)
	ctx := context.Background()
	now := time.Now()
	owner, _ := f.newUser(t)

	newDraft := func(endsAt time.Time) uuid.UUID {
		id := f.newEvent(t, retentionEvent{endsAt: endsAt})
		if err := f.q.CreateAnonDraft(ctx, store.CreateAnonDraftParams{
			ID: uuid.Must(uuid.NewV7()), CookieHash: []byte("retention-claim-cookie"),
			EventID: id, ExpiresAt: now.Add(time.Hour),
		}); err != nil {
			t.Fatalf("create anon draft: %v", err)
		}
		return id
	}
	// Both drafts share one cookie, as one browser's drafts do.
	pastDraft := newDraft(now.Add(-60 * day))
	stamp := now.Add(-5 * day)
	f.remind(t, pastDraft, stamp, &stamp)
	futureDraft := newDraft(now.Add(20 * day))
	var futureBefore time.Time
	if err := f.pool.QueryRow(ctx, "SELECT retention_from FROM events WHERE id = $1", futureDraft).Scan(&futureBefore); err != nil {
		t.Fatalf("read retention_from: %v", err)
	}

	ids, err := f.q.ClaimAnonDrafts(ctx, store.ClaimAnonDraftsParams{UserID: owner, CookieHash: []byte("retention-claim-cookie")})
	if err != nil {
		t.Fatalf("ClaimAnonDrafts: %v", err)
	}
	if !slices.Contains(ids, pastDraft) || !slices.Contains(ids, futureDraft) {
		t.Fatalf("claimed %v, want both drafts", ids)
	}

	if s := f.state(t, pastDraft); s.retentionFrom == nil || time.Since(*s.retentionFrom).Abs() > time.Minute {
		t.Errorf("past draft retention_from = %v, want about now", s.retentionFrom)
	} else if s.reminded != nil || s.sent != nil {
		t.Errorf("past draft reminder state = %v / %v, want cleared with the moved anchor", s.reminded, s.sent)
	}
	if s := f.state(t, futureDraft); s.retentionFrom == nil || !s.retentionFrom.Equal(futureBefore) {
		t.Errorf("future draft retention_from = %v, want unchanged %v", s.retentionFrom, futureBefore)
	}
}
