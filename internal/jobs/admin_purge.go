package jobs

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/riverqueue/river"
	"github.com/riverqueue/river/rivertype"

	"github.com/ansonarose/seeyouthere-backend/internal/store"
)

// ErrPurgeFiles marks a PurgeLockedEvent failure that happened while deleting
// the event's files, before any row was touched, so callers can tell a
// storage problem from a database one.
var ErrPurgeFiles = errors.New("purge event files")

// AdminPurgeEventArgs hard-deletes one event right after it was soft-deleted:
// an admin removing it (directly, or through deleting its owner), or an owner
// deleting their own event. Enqueued in the same transaction as the soft
// delete. Unique by event while queued or running, so a repeated delete never
// queues a second purge.
type AdminPurgeEventArgs struct {
	EventID uuid.UUID `json:"event_id"`
	// KeepIfHeld is set for an owner's delete: an event taken down by moderation or with an open report
	// is kept soft-deleted (the scheduled purge honours those holds), because deleting it would destroy
	// moderation evidence. An admin delete leaves it false and overrides the holds.
	KeepIfHeld bool `json:"keep_if_held,omitempty"`
}

func (AdminPurgeEventArgs) Kind() string { return "admin_purge_event" }

// InsertOpts runs purges on the single-worker maintenance queue: each one
// removes a directory tree, and one at a time keeps the shared host calm.
func (AdminPurgeEventArgs) InsertOpts() river.InsertOpts {
	return river.InsertOpts{
		Queue: "maintenance",
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

// PurgeLockedEvent hard-deletes an event the caller's transaction holds
// LockEventForAdminDelete's row lock on: files first, then the row (children
// cascade). It deliberately skips the grace period, takedown and open-report
// guards of the scheduled purge, which are what an admin delete overrides. A
// file failure returns an error wrapping ErrPurgeFiles and leaves the row, so
// the caller rolls back and a retry (which re-lists the objects) finishes it.
func PurgeLockedEvent(ctx context.Context, q *store.Queries, files mediaDeleter, eventID uuid.UUID) error {
	if err := files.DeleteEventFiles(ctx, eventID); err != nil {
		return fmt.Errorf("%w: event %s: %w", ErrPurgeFiles, eventID, err)
	}
	n, err := q.DeleteEventAdmin(ctx, eventID)
	if err != nil {
		return fmt.Errorf("delete event row %s: %w", eventID, err)
	}
	if n != 1 {
		// The caller holds the row lock, so this means the row vanished
		// under it: files are already gone, so surface it.
		return fmt.Errorf("delete event row %s: %d rows deleted, want 1", eventID, n)
	}
	return nil
}

// AdminPurgeEventWorker purges an event right after it was soft-deleted (an admin delete or an owner's delete).
// A missing row is done (an earlier attempt or another purge got there). A row
// that is not soft-deleted is left alone and logged: the job is only ever
// enqueued together with the soft delete, so this means something restored it
// and must not be deleted on a stale job.
type AdminPurgeEventWorker struct {
	river.WorkerDefaults[AdminPurgeEventArgs]
	Pool    *pgxpool.Pool
	Queries *store.Queries
	Media   mediaDeleter
}

func (w *AdminPurgeEventWorker) Timeout(*river.Job[AdminPurgeEventArgs]) time.Duration {
	return 2 * time.Minute
}

func (w *AdminPurgeEventWorker) Work(ctx context.Context, job *river.Job[AdminPurgeEventArgs]) error {
	eventID := job.Args.EventID
	err := pgx.BeginTxFunc(ctx, w.Pool, pgx.TxOptions{}, func(tx pgx.Tx) error {
		q := w.Queries.WithTx(tx)
		row, err := q.LockEventForAdminDelete(ctx, eventID)
		if errors.Is(err, pgx.ErrNoRows) {
			return nil
		}
		if err != nil {
			return fmt.Errorf("lock event: %w", err)
		}
		if row.DeletedAt == nil {
			slog.ErrorContext(ctx, "admin purge event: event is not soft-deleted, keeping it", "event_id", eventID)
			return nil
		}
		if job.Args.KeepIfHeld {
			// LockEventForAdminDelete's report count used the statement snapshot taken before the row lock,
			// so a report committed meanwhile would be missed. FilterLockedPurgeable runs as a new statement
			// with the lock held and also rejects taken-down events.
			purgeable, err := q.FilterLockedPurgeable(ctx, []uuid.UUID{eventID})
			if err != nil {
				return fmt.Errorf("recheck moderation holds: %w", err)
			}
			if len(purgeable) == 0 {
				slog.InfoContext(ctx, "purge event: moderation hold, keeping it soft-deleted", "event_id", eventID)
				return nil
			}
		}
		return PurgeLockedEvent(ctx, q, w.Media, eventID)
	})
	if err != nil {
		err = fmt.Errorf("admin_purge_event: %w", err)
		if job.Attempt >= job.MaxAttempts {
			slog.ErrorContext(ctx, "admin purge event: giving up, event and files remain",
				"event_id", eventID, "attempt", job.Attempt, "error", err)
		}
		return err
	}
	return nil
}
