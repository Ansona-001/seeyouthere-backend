// Package jobs defines River background jobs: email delivery, periodic
// cleanup, and deletion of rejected event media.
package jobs

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"strings"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/riverqueue/river"
	"github.com/riverqueue/river/riverdriver/riverpgxv5"
	"github.com/riverqueue/river/rivertype"

	"github.com/ansonarose/seeyouthere-backend/internal/mail"
	"github.com/ansonarose/seeyouthere-backend/internal/media"
	"github.com/ansonarose/seeyouthere-backend/internal/ratelimit"
	"github.com/ansonarose/seeyouthere-backend/internal/store"
	"github.com/ansonarose/seeyouthere-backend/internal/token"
)

// stripCRLF removes CR and LF from s, so a user-controlled string (an
// event title) can never inject extra header lines into an outgoing
// email's subject.
func stripCRLF(s string) string {
	return strings.Map(func(r rune) rune {
		if r == '\r' || r == '\n' {
			return -1
		}
		return r
	}, s)
}

type Client = river.Client[pgx.Tx]

// SendEmailArgs delivers one email. Retried with backoff by River on failure.
type SendEmailArgs struct {
	mail.Message
}

func (SendEmailArgs) Kind() string { return "send_email" }

func (SendEmailArgs) InsertOpts() river.InsertOpts {
	// Login codes are useless after 10 minutes, so don't keep retrying for days.
	return river.InsertOpts{MaxAttempts: 5, Queue: "email"}
}

type SendEmailWorker struct {
	river.WorkerDefaults[SendEmailArgs]
	Sender mail.Sender
}

func (w *SendEmailWorker) Work(ctx context.Context, job *river.Job[SendEmailArgs]) error {
	return w.Sender.Send(ctx, job.Args.Message)
}

func (w *SendEmailWorker) Timeout(*river.Job[SendEmailArgs]) time.Duration { return 30 * time.Second }

// CleanupArgs runs the hourly housekeeping: expired sessions, expired
// anonymous drafts, orphaned media rows, and event retention (reminders,
// expiry and hard deletes).
type CleanupArgs struct{}

func (CleanupArgs) Kind() string { return "cleanup" }

// InsertOpts puts cleanup on the single-worker maintenance queue, so two runs
// (a slow one plus the next hourly tick) never overlap.
func (CleanupArgs) InsertOpts() river.InsertOpts {
	return river.InsertOpts{Queue: "maintenance"}
}

// anonDraftBatchSize is how many expired anonymous drafts one batch removes
// (files, then rows).
const anonDraftBatchSize = 500

// maxAnonDraftBatchesPerRun bounds how many batches one cleanup run
// processes, so a large backlog can't turn an hourly job into an unbounded
// loop; any remainder is picked up on the next run.
const maxAnonDraftBatchesPerRun = 20

// mediaCleanupBatchSize matches the LIMIT baked into DeleteDetachedMedia and
// DeleteRejectedMedia; mediaCleanupBatchesPerRun bounds how many batches of
// each this job processes per run, same reasoning as anonDraftBatchSize.
const mediaCleanupBatchSize = 500
const mediaCleanupBatchesPerRun = 10

type CleanupWorker struct {
	river.WorkerDefaults[CleanupArgs]
	Queries *store.Queries
	Pool    *pgxpool.Pool
	Jobs    jobInserter
	Media   mediaDeleter
}

// Timeout is well above River's 1 minute default: a purge batch removes
// directory trees and can be slow on a busy disk.
func (w *CleanupWorker) Timeout(*river.Job[CleanupArgs]) time.Duration { return 10 * time.Minute }

func (w *CleanupWorker) Work(ctx context.Context, _ *river.Job[CleanupArgs]) error {
	now := time.Now()

	n, err := w.Queries.DeleteExpiredSessions(ctx)
	if err != nil {
		return err
	}
	slog.InfoContext(ctx, "cleanup", "expired_sessions", n)

	// Expired anonymous drafts: media files first, then the rows (a failed
	// file delete keeps the row so the next run retries).
	var totalDrafts int64
	var draftFailures int
	for i := 0; i < maxAnonDraftBatchesPerRun; i++ {
		ids, err := w.Queries.ListExpiredAnonDraftEvents(ctx, anonDraftBatchSize)
		if err != nil {
			return fmt.Errorf("list expired anon drafts: %w", err)
		}
		purged, failed, err := w.purgeWithFiles(ctx, ids, w.Queries.DeleteExpiredAnonDraftEvents)
		totalDrafts += purged
		draftFailures += failed
		if err != nil {
			return fmt.Errorf("delete expired anon drafts: %w", err)
		}
		if failed > 0 || len(ids) < anonDraftBatchSize {
			break
		}
	}
	slog.InfoContext(ctx, "cleanup", "expired_anon_drafts", totalDrafts, "anon_draft_file_failures", draftFailures)

	// Host media detached from an event's content for 7+ days: the row is
	// deleted first (DeleteDetachedMedia re-checks detached_at atomically),
	// then its files. A failed or interrupted file delete just leaves an
	// orphan media_reconcile removes later.
	var totalDetached int64
	for i := 0; i < mediaCleanupBatchesPerRun; i++ {
		rows, err := w.Queries.DeleteDetachedMedia(ctx)
		if err != nil {
			return fmt.Errorf("delete detached media: %w", err)
		}
		deleteDetachedFiles(ctx, w.Media, rows)
		totalDetached += int64(len(rows))
		if len(rows) < mediaCleanupBatchSize {
			break
		}
	}
	slog.InfoContext(ctx, "cleanup", "detached_media", totalDetached)

	// Rejected guest photos: their files were already deleted at reject
	// time, so this only needs to purge the rows.
	var totalRejected int64
	for i := 0; i < mediaCleanupBatchesPerRun; i++ {
		n, err := w.Queries.DeleteRejectedMedia(ctx)
		if err != nil {
			return fmt.Errorf("delete rejected media: %w", err)
		}
		totalRejected += n
		if n < mediaCleanupBatchSize {
			break
		}
	}
	slog.InfoContext(ctx, "cleanup", "rejected_media_rows", totalRejected)

	// Event retention, on events.retention_from (the end, but never earlier
	// than the last edit that moved it): remind owners 27+ days after it,
	// soft-delete 30+ days after it once the reminder was delivered 3+ days
	// ago (or claimed 10+ days ago), then hard-delete (files first) events
	// soft-deleted 30+ days ago, or retention-expired ones right away.
	reminded, err := w.remindRetention(ctx, now)
	if err != nil {
		return err
	}
	expired, err := w.expireEndedEvents(ctx, now)
	if err != nil {
		return err
	}
	for _, id := range expired {
		slog.InfoContext(ctx, "event expired", "event_id", id)
	}

	var totalPurged int64
	var purgeFailures int
	for i := 0; i < eventPurgeBatchesPerRun; i++ {
		listed, purged, failed, err := w.purgeEventBatch(ctx, now)
		totalPurged += purged
		purgeFailures += failed
		if err != nil {
			return fmt.Errorf("purge events: %w", err)
		}
		if failed > 0 || listed < eventPurgeBatchSize {
			break
		}
	}
	slog.InfoContext(ctx, "cleanup", "retention_reminders", reminded, "expired_events", len(expired),
		"purged_events", totalPurged, "purge_file_failures", purgeFailures)
	return nil
}

// MediaVisibilityArgs asks for the files of an event's rejected media to be
// deleted. It is enqueued in the same transaction as the rejection. Jobs are
// unique per event while one is queued or running, so a burst of rejects
// queues one event-wide sweep, not one per reject.
type MediaVisibilityArgs struct {
	EventID uuid.UUID `json:"event_id"`
}

func (MediaVisibilityArgs) Kind() string { return "media_visibility" }

func (MediaVisibilityArgs) InsertOpts() river.InsertOpts {
	return river.InsertOpts{
		UniqueOpts: river.UniqueOpts{
			ByArgs: true,
			ByState: []rivertype.JobState{
				rivertype.JobStateAvailable,
				rivertype.JobStatePending,
				rivertype.JobStateRunning,
				rivertype.JobStateRetryable,
				rivertype.JobStateScheduled,
			},
		},
	}
}

// deleteDetachedFiles removes the files of just-deleted detached media rows,
// one DeleteMedia call per event. Failures are logged, not returned: the rows
// are already gone, so a retry could not find them, and media_reconcile sweeps
// whatever objects are left.
func deleteDetachedFiles(ctx context.Context, m mediaDeleter, rows []store.DeleteDetachedMediaRow) {
	byEvent := make(map[uuid.UUID][]uuid.UUID)
	for _, row := range rows {
		byEvent[row.EventID] = append(byEvent[row.EventID], row.ID)
	}
	for eventID, ids := range byEvent {
		if err := m.DeleteMedia(ctx, eventID, ids...); err != nil {
			slog.WarnContext(ctx, "delete detached media files", "event_id", eventID, "count", len(ids), "error", err)
		}
	}
}

// MediaVisibilityWorker deletes the files of an event's rejected media. A
// taken-down or (soft-)deleted event needs nothing: the media serve check
// already hides its files, so they stay in place in case it is restored. A
// missing event row (already hard-deleted, e.g. an expired anon draft purged
// before this job ran) is not an error: media_reconcile cleans up any
// orphaned objects later. Files for a rejected row are deleted synchronously
// when it is rejected; this only cleans up a straggler left by a crash.
type MediaVisibilityWorker struct {
	river.WorkerDefaults[MediaVisibilityArgs]
	Queries *store.Queries
	Media   mediaDeleter
}

func (w *MediaVisibilityWorker) Work(ctx context.Context, job *river.Job[MediaVisibilityArgs]) error {
	state, err := w.Queries.GetEventMediaState(ctx, job.Args.EventID)
	if errors.Is(err, pgx.ErrNoRows) {
		return nil
	}
	if err != nil {
		return fmt.Errorf("media visibility: get event state: %w", err)
	}
	if state.DeletedAt != nil || state.Status == "taken_down" {
		return nil
	}

	ids, err := w.Queries.ListRejectedMediaIDsByEvent(ctx, job.Args.EventID)
	if err != nil {
		return fmt.Errorf("media visibility: list rejected media: %w", err)
	}
	if len(ids) == 0 {
		return nil
	}
	if err := w.Media.DeleteMedia(ctx, job.Args.EventID, ids...); err != nil {
		return fmt.Errorf("media visibility: delete rejected media: %w", err)
	}
	return nil
}

// MediaReconcileArgs periodically runs internal/media's Reconcile: it
// removes stored objects that have no matching live database row (a crash
// between Store.Commit and the row insert, or a media row that was
// hard-deleted or rejected) plus stale tmp/ entries, and checks the bucket's
// size against the configured ceiling. Event purges and anonymous-draft
// deletes remove files before rows, so this is the safety net for crashes and
// older leftovers, not the primary cleanup. Never enqueued manually.
type MediaReconcileArgs struct{}

func (MediaReconcileArgs) Kind() string { return "media_reconcile" }

func (MediaReconcileArgs) InsertOpts() river.InsertOpts {
	return river.InsertOpts{Queue: "maintenance"}
}

// mediaReconciler is the part of *media.Store the reconcile worker needs.
type mediaReconciler interface {
	Reconcile(ctx context.Context, live media.LiveMediaFunc) (media.ReconcileStats, error)
}

// reconcileDriftPercent is how far the bucket's byte total may exceed the
// database's before it is worth a warning: leftovers from crashed uploads
// and in-flight uploads are normal, a large gap means leaking objects.
const reconcileDriftPercent = 5

type MediaReconcileWorker struct {
	river.WorkerDefaults[MediaReconcileArgs]
	Queries *store.Queries
	Media   mediaReconciler
	// TotalQuotaBytes is the global storage ceiling (MEDIA_TOTAL_QUOTA_MB).
	TotalQuotaBytes int64
}

func (w *MediaReconcileWorker) Timeout(*river.Job[MediaReconcileArgs]) time.Duration {
	return 10 * time.Minute
}

func (w *MediaReconcileWorker) Work(ctx context.Context, _ *river.Job[MediaReconcileArgs]) error {
	live := func(ctx context.Context, ids []uuid.UUID) (map[uuid.UUID]bool, error) {
		found, err := w.Queries.LiveMediaIDs(ctx, ids)
		if err != nil {
			return nil, fmt.Errorf("live media ids: %w", err)
		}
		out := make(map[uuid.UUID]bool, len(found))
		for _, id := range found {
			out[id] = true
		}
		return out, nil
	}
	stats, err := w.Media.Reconcile(ctx, live)
	if err != nil {
		return fmt.Errorf("media reconcile: %w", err)
	}

	if w.TotalQuotaBytes > 0 && stats.Bytes >= w.TotalQuotaBytes {
		slog.ErrorContext(ctx, "media storage at ceiling", "bytes", stats.Bytes, "ceiling_bytes", w.TotalQuotaBytes)
	}
	usage, err := w.Queries.MediaStorageUsage(ctx, store.MediaStorageUsageParams{})
	if err != nil {
		return fmt.Errorf("media reconcile: storage usage: %w", err)
	}
	if stats.Bytes > usage.TotalBytes+usage.TotalBytes*reconcileDriftPercent/100 {
		slog.WarnContext(ctx, "media storage exceeds database total", "bytes", stats.Bytes, "db_bytes", usage.TotalBytes)
	}
	return nil
}

// NotifyReportArgs alerts an admin that a visitor reported an event page.
// Enqueued (InsertTx, in the same transaction as the write) only when
// CreateReport actually inserted a row: a duplicate report from the same
// reporter while one is already open is silently ignored, so it never
// re-triggers this job either. Unique by event id within a 1-hour period,
// so a burst of reports against the same event sends one alert, not one
// per report.
type NotifyReportArgs struct {
	EventID uuid.UUID `json:"event_id"`
}

func (NotifyReportArgs) Kind() string { return "notify_report" }

func (NotifyReportArgs) InsertOpts() river.InsertOpts {
	return river.InsertOpts{
		Queue:      "email",
		UniqueOpts: river.UniqueOpts{ByArgs: true, ByPeriod: time.Hour},
	}
}

// NotifyReportWorker emails ADMIN_ALERT_EMAIL with a link into the admin
// moderation queue. It never includes reporter data (not even which reason
// was chosen): the reporter's IP is only ever stored as a keyed hash, and
// this job's args carry nothing more than the event id. A missing event
// (deleted since the report was filed) or an unset AdminAlertEmail are both
// treated as done, not an error.
type NotifyReportWorker struct {
	river.WorkerDefaults[NotifyReportArgs]
	Queries         *store.Queries
	Sender          mail.Sender
	AdminAlertEmail string
	SiteURL         string
}

func (w *NotifyReportWorker) Work(ctx context.Context, job *river.Job[NotifyReportArgs]) error {
	if w.AdminAlertEmail == "" {
		return nil
	}
	event, err := w.Queries.GetEventForNotify(ctx, job.Args.EventID)
	if errors.Is(err, pgx.ErrNoRows) {
		return nil
	}
	if err != nil {
		return fmt.Errorf("notify report: get event: %w", err)
	}
	title := stripCRLF(event.Title)
	return w.Sender.Send(ctx, mail.Message{
		To:      w.AdminAlertEmail,
		Subject: "New report: " + title,
		Text: fmt.Sprintf("A visitor reported \"%s\".\n\n"+
			"Review it in the admin moderation queue:\n%s/admin/reports\n\n"+
			"See You There\n", title, w.SiteURL),
	})
}

func (w *NotifyReportWorker) Timeout(*river.Job[NotifyReportArgs]) time.Duration {
	return 30 * time.Second
}

// calendarSender sends the calendar-invite variant of send_rsvp_confirmation
// (an attending="yes" response); pass the same value as sender to reuse the
// primary SMTP identity.
func NewClient(pool *pgxpool.Pool, sender, calendarSender mail.Sender, queries *store.Queries, mediaStore *media.Store, tokens *token.Keys, limiter *ratelimit.Limiter, adminAlertEmail, siteURL string, mediaTotalQuotaBytes int64) (*Client, error) {
	workers := river.NewWorkers()
	cleanup := &CleanupWorker{Queries: queries, Pool: pool, Media: mediaStore}
	river.AddWorker(workers, &SendEmailWorker{Sender: sender})
	river.AddWorker(workers, cleanup)
	river.AddWorker(workers, &RetentionReminderWorker{Queries: queries, Sender: sender, Limiter: limiter, SiteURL: siteURL})
	river.AddWorker(workers, &MediaVisibilityWorker{Queries: queries, Media: mediaStore})
	river.AddWorker(workers, &MediaReconcileWorker{Queries: queries, Media: mediaStore, TotalQuotaBytes: mediaTotalQuotaBytes})
	river.AddWorker(workers, &NotifyReportWorker{Queries: queries, Sender: sender, AdminAlertEmail: adminAlertEmail, SiteURL: siteURL})
	river.AddWorker(workers, &SendInviteWorker{Queries: queries, Sender: sender, Tokens: tokens, Limiter: limiter, SiteURL: siteURL})
	river.AddWorker(workers, &NotifyCohostAddedWorker{Queries: queries, Sender: sender, SiteURL: siteURL})
	river.AddWorker(workers, &NotifyTakedownWorker{Queries: queries, Sender: sender, SiteURL: siteURL})
	river.AddWorker(workers, &SendRSVPConfirmationWorker{Queries: queries, Sender: sender, CalendarSender: calendarSender, Tokens: tokens, Limiter: limiter, SiteURL: siteURL})
	river.AddWorker(workers, &NotifyRSVPsWorker{Queries: queries, Sender: sender, SiteURL: siteURL})

	client, err := river.NewClient(riverpgxv5.New(pool), &river.Config{
		Logger: slog.Default(),
		Queues: map[string]river.QueueConfig{
			// Worker counts are deliberately small (§7 of the build-out
			// plan): this shares a 1 vCPU host with the API's own request
			// handling, and email queue capacity is bounded by SMTP
			// throughput, not memory we'd gain from more workers.
			river.QueueDefault: {MaxWorkers: 2},
			"email":            {MaxWorkers: 3},
			"maintenance":      {MaxWorkers: 1},
		},
		Workers: workers,
		PeriodicJobs: []*river.PeriodicJob{
			river.NewPeriodicJob(
				river.PeriodicInterval(time.Hour),
				func() (river.JobArgs, *river.InsertOpts) { return CleanupArgs{}, nil },
				&river.PeriodicJobOpts{RunOnStart: true},
			),
			river.NewPeriodicJob(
				river.PeriodicInterval(24*time.Hour),
				func() (river.JobArgs, *river.InsertOpts) { return MediaReconcileArgs{}, nil },
				&river.PeriodicJobOpts{RunOnStart: false},
			),
		},
	})
	if err != nil {
		return nil, err
	}
	// The cleanup worker enqueues reminder jobs through the client that runs
	// it, so it is wired once the client exists (before Start).
	cleanup.Jobs = client
	return client, nil
}
