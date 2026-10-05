package jobs

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/riverqueue/river"
	"github.com/riverqueue/river/rivertype"

	"github.com/ansonarose/seeyouthere-backend/internal/mail"
	"github.com/ansonarose/seeyouthere-backend/internal/ratelimit"
	"github.com/ansonarose/seeyouthere-backend/internal/store"
)

const (
	// retentionAfterEnd is how long after an event ends it is deleted.
	retentionAfterEnd = 30 * 24 * time.Hour
	// retentionReminderLead is how long before deletion the owner is
	// reminded: an event is reminded once it ended retentionAfterEnd -
	// retentionReminderLead ago, and deleted once it has also been reminded
	// that long ago.
	retentionReminderLead = 3 * 24 * time.Hour
	// retentionReminderFallback is how long after the claim an event expires
	// even though its reminder was never delivered (undeliverable address,
	// persistent SMTP failure), so it cannot be kept forever.
	retentionReminderFallback = 10 * 24 * time.Hour
	// deletedEventGrace is how long a soft-deleted event is kept before its
	// rows and files are hard-deleted.
	deletedEventGrace = 30 * 24 * time.Hour

	// maxRetentionRemindersPerOwnerPerDay caps reminder emails per owner so
	// an account holding many ended events cannot turn the job into a mail
	// flood; a denied event still expires through the fallback path.
	maxRetentionRemindersPerOwnerPerDay = 20
	// maxRetentionRemindersPerHour caps all reminder emails together: the
	// SMTP account is shared with login codes, and a burst of due events
	// (for example the legacy backfill) must not exhaust it.
	maxRetentionRemindersPerHour = 60
	retentionGlobalLimitKey      = "retention:global"

	// retentionBatchSize bounds how many events one cleanup run reminds and
	// how many it expires; the remainder is picked up on the next run.
	retentionBatchSize = 100
	// eventPurgeBatchSize and eventPurgeBatchesPerRun bound the hard-delete
	// work per run: each event costs a recursive directory removal, so the
	// batches are small and the number of batches capped.
	eventPurgeBatchSize     = 50
	eventPurgeBatchesPerRun = 4
)

// jobInserter is the part of *river.Client the cleanup worker needs; a
// consumer-side interface so tests can run without a started client.
type jobInserter interface {
	InsertManyTx(ctx context.Context, tx pgx.Tx, params []river.InsertManyParams) ([]*rivertype.JobInsertResult, error)
}

// mediaDeleter is the part of *media.Store the cleanup worker needs.
type mediaDeleter interface {
	DeleteMedia(eventID, mediaID uuid.UUID) error
	DeleteEventFiles(ctx context.Context, eventID uuid.UUID) error
}

// RetentionReminderArgs emails an event's owner that the event will be
// deleted soon. The recipient, title and dates are re-read at send time, so a
// deleted or re-dated event sends nothing. ClaimedAt is the claim stamp
// (events.retention_reminded_at) the job was enqueued for: a re-date clears
// the stamp and a later claim replaces it, so a job for a superseded claim
// finds no matching row and sends nothing. Enqueued by CleanupWorker in the
// same transaction that claims the reminder, which is what makes it one per
// claim.
type RetentionReminderArgs struct {
	EventID   uuid.UUID `json:"event_id"`
	ClaimedAt time.Time `json:"claimed_at"`
}

func (RetentionReminderArgs) Kind() string { return "retention_reminder" }

func (RetentionReminderArgs) InsertOpts() river.InsertOpts {
	// The deletion is days away, so retry generously with River's backoff
	// rather than losing the only reminder to a brief SMTP outage.
	return river.InsertOpts{Queue: "email", MaxAttempts: 12}
}

type RetentionReminderWorker struct {
	river.WorkerDefaults[RetentionReminderArgs]
	Queries *store.Queries
	Sender  mail.Sender
	Limiter *ratelimit.Limiter
	SiteURL string
}

func (w *RetentionReminderWorker) Timeout(*river.Job[RetentionReminderArgs]) time.Duration {
	return 30 * time.Second
}

// Work sends the reminder at most once per claim. The claim is marked as sent
// before the mail goes out: a retry or duplicate job then finds the mark and
// sends nothing. A send that failed definitely before delivery removes the
// mark again so the retry can deliver it. A send that ended on a context
// error (timeout or shutdown) may still have been delivered by the SMTP
// goroutine, so the mark and the quota stay and the job completes: a possibly
// lost email is better than up to 12 duplicates. The same goes for a crash
// between the mark and the send; the event then expires through the fallback
// instead of being kept forever. Sending first and marking after would
// double-send whenever the mark fails.
//
// A rate-limit denial snoozes the job instead of dropping it: River does not
// count a snooze as an attempt, the claim stays, and the job runs again once
// the limiter window has passed. If the event is expired or deleted in the
// meantime the next run finds no row and completes.
func (w *RetentionReminderWorker) Work(ctx context.Context, job *river.Job[RetentionReminderArgs]) error {
	args := job.Args
	row, err := w.Queries.GetEventForRetentionReminder(ctx, store.GetEventForRetentionReminderParams{
		EventID: args.EventID, ClaimedAt: args.ClaimedAt,
	})
	if errors.Is(err, pgx.ErrNoRows) {
		return nil
	}
	if err != nil {
		return fmt.Errorf("retention_reminder: load event: %w", err)
	}

	allowed, err := w.Limiter.AllowN(ctx, retentionGlobalLimitKey, 1, maxRetentionRemindersPerHour, time.Hour)
	if err != nil {
		return fmt.Errorf("retention_reminder: rate limit: %w", err)
	}
	if !allowed {
		slog.WarnContext(ctx, "retention_reminder: global rate limited, snoozing", "event_id", args.EventID)
		return river.JobSnooze(time.Hour)
	}
	ownerKey := "retention:owner:" + row.OwnerID.String()
	allowed, err = w.Limiter.AllowN(ctx, ownerKey, 1, maxRetentionRemindersPerOwnerPerDay, 24*time.Hour)
	if err != nil {
		w.Limiter.Uncount(ctx, retentionGlobalLimitKey, 1)
		return fmt.Errorf("retention_reminder: rate limit: %w", err)
	}
	if !allowed {
		w.Limiter.Uncount(ctx, retentionGlobalLimitKey, 1)
		slog.WarnContext(ctx, "retention_reminder: owner rate limited, snoozing", "event_id", args.EventID, "owner_id", row.OwnerID)
		return river.JobSnooze(24 * time.Hour)
	}
	refund := func(ctx context.Context) {
		w.Limiter.Uncount(ctx, retentionGlobalLimitKey, 1)
		w.Limiter.Uncount(ctx, ownerKey, 1)
	}

	marked, err := w.Queries.MarkRetentionReminderSent(ctx, store.MarkRetentionReminderSentParams{
		EventID: args.EventID, ClaimedAt: args.ClaimedAt,
	})
	if err != nil || marked == 0 {
		refund(ctx)
		if err != nil {
			return fmt.Errorf("retention_reminder: mark sent: %w", err)
		}
		return nil
	}

	deleteAt := row.RetentionFrom.Add(retentionAfterEnd)
	if earliest := row.RetentionRemindedAt.Add(retentionReminderLead); earliest.After(deleteAt) {
		deleteAt = earliest
	}
	loc := time.UTC
	if row.Timezone != "" && row.Timezone != "Local" {
		if l, err := time.LoadLocation(row.Timezone); err == nil {
			loc = l
		}
	}

	title := stripCRLF(row.Title)
	err = w.Sender.Send(ctx, mail.Message{
		To:      row.OwnerEmail,
		Subject: fmt.Sprintf("Your event will be deleted soon: %s", title),
		Text: fmt.Sprintf(
			"Your event \"%s\" has ended. Events are deleted 30 days after they end, together with their "+
				"guest list, responses and photos. Yours will be deleted on or after %s.\n\n"+
				"Sign in to save anything you want to keep before then:\n%s/login\n\n"+
				"See You There\n",
			title, deleteAt.In(loc).Format("Monday, 2 January 2006"), w.SiteURL,
		),
	})
	if err != nil {
		if errors.Is(err, context.DeadlineExceeded) || errors.Is(err, context.Canceled) {
			// The SMTP goroutine may still deliver after the context ends.
			slog.WarnContext(ctx, "retention_reminder: send outcome unknown, not retrying", "event_id", args.EventID, "error", err)
			return nil
		}
		// Undo the mark and the quota hits even if ctx is already done, so
		// the retry can send and a failing SMTP server does not use up the
		// allowances.
		cleanup, cancel := context.WithTimeout(context.WithoutCancel(ctx), 5*time.Second)
		defer cancel()
		if _, uerr := w.Queries.UnmarkRetentionReminderSent(cleanup, store.UnmarkRetentionReminderSentParams{
			EventID: args.EventID, ClaimedAt: args.ClaimedAt,
		}); uerr != nil {
			slog.ErrorContext(ctx, "retention_reminder: unmark after failed send", "event_id", args.EventID, "error", uerr)
		}
		refund(cleanup)
		return fmt.Errorf("retention_reminder: send: %w", err)
	}
	return nil
}

// remindRetention claims owned events whose retention clock
// (events.retention_from) is far enough back to be due a reminder and
// enqueues one retention_reminder job per claim, all in one transaction: a
// claim without its job (or the reverse) can't happen.
func (w *CleanupWorker) remindRetention(ctx context.Context, now time.Time) (int, error) {
	var n int
	err := pgx.BeginTxFunc(ctx, w.Pool, pgx.TxOptions{}, func(tx pgx.Tx) error {
		claims, err := w.Queries.WithTx(tx).ClaimRetentionReminders(ctx, store.ClaimRetentionRemindersParams{
			EndedBefore: now.Add(-(retentionAfterEnd - retentionReminderLead)),
			Lim:         retentionBatchSize,
		})
		if err != nil {
			return fmt.Errorf("claim: %w", err)
		}
		if len(claims) == 0 {
			return nil
		}
		params := make([]river.InsertManyParams, len(claims))
		for i, c := range claims {
			params[i] = river.InsertManyParams{Args: RetentionReminderArgs{EventID: c.ID, ClaimedAt: c.ClaimedAt}}
		}
		if _, err := w.Jobs.InsertManyTx(ctx, tx, params); err != nil {
			return fmt.Errorf("enqueue: %w", err)
		}
		n = len(claims)
		return nil
	})
	if err != nil {
		return 0, fmt.Errorf("remind retention: %w", err)
	}
	return n, nil
}

// expireEndedEvents soft-deletes events whose retention clock is 30+ days old
// and whose reminder was delivered at least retentionReminderLead ago, or
// claimed at least retentionReminderFallback ago without ever being delivered.
func (w *CleanupWorker) expireEndedEvents(ctx context.Context, now time.Time) ([]uuid.UUID, error) {
	ids, err := w.Queries.ExpireEndedEvents(ctx, store.ExpireEndedEventsParams{
		EndedBefore:    now.Add(-retentionAfterEnd),
		RemindedBefore: now.Add(-retentionReminderLead),
		FallbackBefore: now.Add(-retentionReminderFallback),
		Lim:            retentionBatchSize,
	})
	if err != nil {
		return nil, fmt.Errorf("expire ended events: %w", err)
	}
	return ids, nil
}

// purgeWithFiles hard-deletes the given events, files first: each event's
// media files are removed, then deleteRows runs once for the events whose
// files are gone. An event whose files could not be removed keeps its row
// (counted in failed, logged here) so the next run retries it; a crash after
// the files but before the rows is the same retry. err is non-nil only for a
// cancelled context or a failed row delete.
func (w *CleanupWorker) purgeWithFiles(ctx context.Context, ids []uuid.UUID, deleteRows func(context.Context, []uuid.UUID) (int64, error)) (purged int64, failed int, err error) {
	done := make([]uuid.UUID, 0, len(ids))
	var ctxErr error
	for _, id := range ids {
		if ctxErr = ctx.Err(); ctxErr != nil {
			break
		}
		if err := w.Media.DeleteEventFiles(ctx, id); err != nil {
			failed++
			slog.ErrorContext(ctx, "purge event files", "event_id", id, "error", err)
			continue
		}
		done = append(done, id)
	}
	if len(done) > 0 {
		purged, err = deleteRows(ctx, done)
		if err != nil {
			return 0, failed, fmt.Errorf("delete event rows: %w", err)
		}
	}
	if ctxErr != nil {
		return purged, failed, fmt.Errorf("purge events: %w", ctxErr)
	}
	return purged, failed, nil
}
