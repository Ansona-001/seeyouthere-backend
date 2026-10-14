package jobs

import (
	"context"
	"errors"
	"log/slog"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/riverqueue/river"
	"github.com/riverqueue/river/rivertype"
)

func (f retentionFixture) purgeWorker() *AdminPurgeEventWorker {
	return &AdminPurgeEventWorker{Pool: f.pool, Queries: f.q, Media: f.deleter}
}

func purgeJob(eventID uuid.UUID, attempt, maxAttempts int, keepIfHeld bool) *river.Job[AdminPurgeEventArgs] {
	return &river.Job[AdminPurgeEventArgs]{
		JobRow: &rivertype.JobRow{Attempt: attempt, MaxAttempts: maxAttempts},
		Args:   AdminPurgeEventArgs{EventID: eventID, KeepIfHeld: keepIfHeld},
	}
}

func (f retentionFixture) rowCount(t *testing.T, table string, eventID uuid.UUID) int {
	t.Helper()
	var n int
	if err := f.pool.QueryRow(context.Background(), "SELECT count(*) FROM "+table+" WHERE event_id = $1", eventID).Scan(&n); err != nil {
		t.Fatalf("count %s: %v", table, err)
	}
	return n
}

func TestAdminPurgeEventWorker(t *testing.T) {
	now := time.Now()
	tests := []struct {
		name string
		// setup prepares the event and returns its id.
		setup       func(t *testing.T, f retentionFixture, owner uuid.UUID) uuid.UUID
		failFiles   bool
		keepIfHeld  bool // an owner's delete: moderation holds keep the event
		attempt     int
		wantErr     bool
		wantGone    bool
		wantFiles   int  // DeleteEventFiles calls
		wantRowSeen bool // the row still existed when its files were deleted
		wantErrLog  bool
	}{
		{
			name: "soft-deleted event: files first, then rows and children",
			setup: func(t *testing.T, f retentionFixture, owner uuid.UUID) uuid.UUID {
				id := f.newEvent(t, retentionEvent{owner: &owner, endsAt: now.Add(24 * time.Hour)})
				f.insertMedia(t, id, "approved")
				f.exec(t, "UPDATE events SET deleted_at = now() WHERE id = $1", id)
				return id
			},
			wantGone: true, wantFiles: 1, wantRowSeen: true,
		},
		{
			name: "taken-down event with an open report is still purged, grace period ignored",
			setup: func(t *testing.T, f retentionFixture, owner uuid.UUID) uuid.UUID {
				id := f.newEvent(t, retentionEvent{owner: &owner, endsAt: now.Add(24 * time.Hour)})
				f.report(t, id, "open")
				f.exec(t, "UPDATE events SET status = 'taken_down', slug = $2, deleted_at = now() WHERE id = $1", id, "purge-td-"+id.String()[:8])
				return id
			},
			wantGone: true, wantFiles: 1, wantRowSeen: true,
		},
		{
			name: "owner delete: a plain soft-deleted event is purged at once",
			setup: func(t *testing.T, f retentionFixture, owner uuid.UUID) uuid.UUID {
				id := f.newEvent(t, retentionEvent{owner: &owner, endsAt: now.Add(24 * time.Hour)})
				f.insertMedia(t, id, "approved")
				f.exec(t, "UPDATE events SET deleted_at = now() WHERE id = $1", id)
				return id
			},
			keepIfHeld: true,
			wantGone:   true, wantFiles: 1, wantRowSeen: true,
		},
		{
			name: "owner delete: an open report keeps the event and its files",
			setup: func(t *testing.T, f retentionFixture, owner uuid.UUID) uuid.UUID {
				id := f.newEvent(t, retentionEvent{owner: &owner, endsAt: now.Add(24 * time.Hour)})
				f.insertMedia(t, id, "approved")
				f.report(t, id, "open")
				f.exec(t, "UPDATE events SET deleted_at = now() WHERE id = $1", id)
				return id
			},
			keepIfHeld: true,
			wantGone:   false, wantFiles: 0,
		},
		{
			name: "owner delete: a taken-down event is kept as evidence",
			setup: func(t *testing.T, f retentionFixture, owner uuid.UUID) uuid.UUID {
				id := f.newEvent(t, retentionEvent{owner: &owner, endsAt: now.Add(24 * time.Hour)})
				f.insertMedia(t, id, "approved")
				f.exec(t, "UPDATE events SET status = 'taken_down', slug = $2, deleted_at = now() WHERE id = $1", id, "purge-keep-"+id.String()[:8])
				return id
			},
			keepIfHeld: true,
			wantGone:   false, wantFiles: 0,
		},
		{
			name: "anonymous draft purged when soft-deleted",
			setup: func(t *testing.T, f retentionFixture, _ uuid.UUID) uuid.UUID {
				id := f.newEvent(t, retentionEvent{endsAt: now.Add(24 * time.Hour)})
				f.exec(t, "UPDATE events SET deleted_at = now() WHERE id = $1", id)
				return id
			},
			wantGone: true, wantFiles: 1, wantRowSeen: true,
		},
		{
			name: "missing row is done without touching files",
			setup: func(*testing.T, retentionFixture, uuid.UUID) uuid.UUID {
				return uuid.Must(uuid.NewV7())
			},
			wantGone: true, wantFiles: 0,
		},
		{
			name: "live event is left alone and logged",
			setup: func(t *testing.T, f retentionFixture, owner uuid.UUID) uuid.UUID {
				return f.newEvent(t, retentionEvent{owner: &owner, endsAt: now.Add(24 * time.Hour)})
			},
			wantGone: false, wantFiles: 0, wantErrLog: true,
		},
		{
			name: "file failure keeps the row and returns an error",
			setup: func(t *testing.T, f retentionFixture, owner uuid.UUID) uuid.UUID {
				id := f.newEvent(t, retentionEvent{owner: &owner, endsAt: now.Add(24 * time.Hour)})
				f.insertMedia(t, id, "approved")
				f.exec(t, "UPDATE events SET deleted_at = now() WHERE id = $1", id)
				return id
			},
			failFiles: true, wantErr: true, wantGone: false, wantFiles: 1, wantRowSeen: true,
		},
		{
			name: "last failed attempt logs an error naming the event",
			setup: func(t *testing.T, f retentionFixture, owner uuid.UUID) uuid.UUID {
				id := f.newEvent(t, retentionEvent{owner: &owner, endsAt: now.Add(24 * time.Hour)})
				f.exec(t, "UPDATE events SET deleted_at = now() WHERE id = $1", id)
				return id
			},
			failFiles: true, attempt: 25, wantErr: true, wantGone: false, wantFiles: 1, wantRowSeen: true, wantErrLog: true,
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			f := newRetentionFixture(t)
			owner, _ := f.newUser(t)
			id := tt.setup(t, f, owner)
			if tt.failFiles {
				f.deleter.failFor = map[uuid.UUID]bool{id: true}
			}
			mediaBefore := f.rowCount(t, "media", id)

			logs := &logCapture{}
			prev := slog.Default()
			slog.SetDefault(slog.New(logs))
			t.Cleanup(func() { slog.SetDefault(prev) })

			attempt := tt.attempt
			if attempt == 0 {
				attempt = 1
			}
			err := f.purgeWorker().Work(context.Background(), purgeJob(id, attempt, 25, tt.keepIfHeld))
			if (err != nil) != tt.wantErr {
				t.Fatalf("Work error = %v, wantErr %v", err, tt.wantErr)
			}
			if tt.wantErr && !errors.Is(err, ErrPurgeFiles) {
				t.Errorf("error = %v, want it to wrap ErrPurgeFiles", err)
			}
			if got := f.eventExists(t, id); got == tt.wantGone {
				t.Errorf("event exists = %v, want %v", got, !tt.wantGone)
			}
			if tt.wantGone && f.rowCount(t, "media", id) != 0 {
				t.Error("media rows survived the purge")
			}
			if !tt.wantGone && f.rowCount(t, "media", id) != mediaBefore {
				t.Error("media rows changed although the event was kept")
			}
			if len(f.deleter.calls) != tt.wantFiles {
				t.Errorf("DeleteEventFiles calls = %d, want %d", len(f.deleter.calls), tt.wantFiles)
			}
			if tt.wantRowSeen && !f.deleter.rowPresent[id] {
				t.Error("files were deleted after the row: they must go first")
			}
			if got := logs.has(slog.LevelError, "admin purge event"); got != tt.wantErrLog {
				t.Errorf("error logged = %v, want %v", got, tt.wantErrLog)
			}
			if tt.wantErrLog && tt.attempt > 0 {
				logs.mu.Lock()
				defer logs.mu.Unlock()
				found := false
				for _, r := range logs.records {
					r.Attrs(func(a slog.Attr) bool {
						if a.Key == "event_id" && strings.Contains(a.Value.String(), id.String()) {
							found = true
						}
						return true
					})
				}
				if !found {
					t.Error("error log does not name the event")
				}
			}
		})
	}
}

func TestAdminPurgeEventWorker_RetryAndRerun(t *testing.T) {
	f := newRetentionFixture(t)
	owner, _ := f.newUser(t)
	id := f.newEvent(t, retentionEvent{owner: &owner, endsAt: time.Now().Add(24 * time.Hour)})
	f.exec(t, "UPDATE events SET deleted_at = now() WHERE id = $1", id)
	w := f.purgeWorker()
	ctx := context.Background()

	f.deleter.failFor = map[uuid.UUID]bool{id: true}
	if err := w.Work(ctx, purgeJob(id, 1, 25, false)); err == nil {
		t.Fatal("first attempt with failing storage returned nil")
	}
	if !f.eventExists(t, id) {
		t.Fatal("row deleted although its files could not be")
	}

	f.deleter.failFor = nil
	if err := w.Work(ctx, purgeJob(id, 2, 25, false)); err != nil {
		t.Fatalf("retry: %v", err)
	}
	if f.eventExists(t, id) {
		t.Fatal("row survived the successful retry")
	}

	calls := len(f.deleter.calls)
	if err := w.Work(ctx, purgeJob(id, 1, 25, false)); err != nil {
		t.Fatalf("second run: %v", err)
	}
	if len(f.deleter.calls) != calls {
		t.Error("second run touched files of an already purged event")
	}
}

func TestAdminPurgeEventArgs_UniquePerEventWhileInFlight(t *testing.T) {
	opts := AdminPurgeEventArgs{}.InsertOpts()
	if opts.Queue != "maintenance" {
		t.Errorf("queue = %q, want maintenance", opts.Queue)
	}
	if !opts.UniqueOpts.ByArgs {
		t.Error("jobs must be unique by args")
	}
	for _, st := range []rivertype.JobState{rivertype.JobStateAvailable, rivertype.JobStateRunning, rivertype.JobStateRetryable} {
		found := false
		for _, s := range opts.UniqueOpts.ByState {
			found = found || s == st
		}
		if !found {
			t.Errorf("uniqueness does not cover state %s", st)
		}
	}
	if got := (&AdminPurgeEventWorker{}).Timeout(nil); got != 2*time.Minute {
		t.Errorf("timeout = %v, want 2m", got)
	}
}

// TestEventScopedWorkers_NoopForDeletedEvent proves that jobs queued before an
// admin delete (invites, RSVP confirmations, digests, co-host and takedown
// notices) send nothing once their event is soft-deleted, and still send
// nothing after the purge removed it and its children.
func TestEventScopedWorkers_NoopForDeletedEvent(t *testing.T) {
	f := newRetentionFixture(t)
	ctx := context.Background()
	owner, _ := f.newUser(t)
	cohost, _ := f.newUser(t)
	id := f.newEvent(t, retentionEvent{owner: &owner, endsAt: time.Now().Add(48 * time.Hour)})
	f.exec(t, "UPDATE events SET status = 'published', slug = $2, published_at = now(), notify_rsvps = true WHERE id = $1",
		id, "purge-noop-"+id.String()[:8])

	guestID, rsvpID := uuid.Must(uuid.NewV7()), uuid.Must(uuid.NewV7())
	f.exec(t, "INSERT INTO guests (id, event_id, name, email) VALUES ($1, $2, 'G', 'guest@example.invalid')",
		guestID, id)
	f.exec(t, "INSERT INTO rsvps (id, event_id, guest_id, name, email, attending) VALUES ($1, $2, $3, 'G', 'guest@example.invalid', 'yes')",
		rsvpID, id, guestID)
	f.exec(t, "INSERT INTO event_members (event_id, user_id, role) VALUES ($1, $2, 'editor')", id, cohost)

	sender := &recordingSender{}
	run := func(t *testing.T, phase string) {
		t.Helper()
		invite := &SendInviteWorker{Queries: f.q, Sender: sender, SiteURL: "https://seeyouthere.at"}
		rsvp := &SendRSVPConfirmationWorker{Queries: f.q, Sender: sender, CalendarSender: sender, SiteURL: "https://seeyouthere.at"}
		digest := &NotifyRSVPsWorker{Queries: f.q, Sender: sender, SiteURL: "https://seeyouthere.at"}
		added := &NotifyCohostAddedWorker{Queries: f.q, Sender: sender, SiteURL: "https://seeyouthere.at"}
		down := &NotifyTakedownWorker{Queries: f.q, Sender: sender, SiteURL: "https://seeyouthere.at"}
		jobMeta := &rivertype.JobRow{Attempt: 1, MaxAttempts: 5}
		checks := map[string]error{
			"invite": invite.Work(ctx, &river.Job[SendInviteArgs]{JobRow: jobMeta, Args: SendInviteArgs{GuestID: guestID}}),
			"rsvp":   rsvp.Work(ctx, &river.Job[SendRSVPConfirmationArgs]{JobRow: jobMeta, Args: SendRSVPConfirmationArgs{RSVPID: rsvpID}}),
			"digest": digest.Work(ctx, &river.Job[NotifyRSVPsArgs]{JobRow: jobMeta, Args: NotifyRSVPsArgs{EventID: id}}),
			"cohost": added.Work(ctx, &river.Job[NotifyCohostAddedArgs]{JobRow: jobMeta, Args: NotifyCohostAddedArgs{EventID: id, UserID: cohost}}),
			"down":   down.Work(ctx, &river.Job[NotifyTakedownArgs]{JobRow: jobMeta, Args: NotifyTakedownArgs{EventID: id, Action: "takedown"}}),
		}
		for name, err := range checks {
			if err != nil {
				t.Errorf("%s: %s worker returned %v, want nil", phase, name, err)
			}
		}
		if msgs := sender.messages(); len(msgs) != 0 {
			t.Errorf("%s: %d mails sent for a deleted event, want none", phase, len(msgs))
		}
	}

	f.exec(t, "UPDATE events SET deleted_at = now() WHERE id = $1", id)
	run(t, "soft-deleted")

	if err := f.purgeWorker().Work(ctx, purgeJob(id, 1, 25, false)); err != nil {
		t.Fatalf("purge: %v", err)
	}
	if f.eventExists(t, id) {
		t.Fatal("event survived the purge")
	}
	for _, table := range []string{"guests", "rsvps", "event_members"} {
		if n := f.rowCount(t, table, id); n != 0 {
			t.Errorf("%s rows left after purge: %d", table, n)
		}
	}
	run(t, "purged")
}
