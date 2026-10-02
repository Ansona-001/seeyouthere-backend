package jobs

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/riverqueue/river"

	"github.com/ansonarose/seeyouthere-backend/internal/mail"
	"github.com/ansonarose/seeyouthere-backend/internal/store"
)

// NotifyCohostAddedArgs tells a newly added co-host they now have access to
// an event. It carries only ids, not the recipient's email: GetCohostNotification
// re-reads the membership (and the user's and event's live rows) at send time,
// so a membership removed before the job runs is a no-op rather than an email
// to someone who's since lost access.
type NotifyCohostAddedArgs struct {
	EventID uuid.UUID `json:"event_id"`
	UserID  uuid.UUID `json:"user_id"`
}

func (NotifyCohostAddedArgs) Kind() string { return "notify_cohost_added" }

func (NotifyCohostAddedArgs) InsertOpts() river.InsertOpts {
	// Backstop against a double notification for the same membership within
	// a day: the handler already only enqueues this on a fresh add, but a
	// concurrent duplicate request could otherwise still slip two jobs in.
	return river.InsertOpts{
		Queue:      "email",
		UniqueOpts: river.UniqueOpts{ByArgs: true, ByPeriod: 24 * time.Hour},
	}
}

type NotifyCohostAddedWorker struct {
	river.WorkerDefaults[NotifyCohostAddedArgs]
	Queries *store.Queries
	Sender  mail.Sender
	SiteURL string
}

func (w *NotifyCohostAddedWorker) Timeout(*river.Job[NotifyCohostAddedArgs]) time.Duration {
	return 30 * time.Second
}

func (w *NotifyCohostAddedWorker) Work(ctx context.Context, job *river.Job[NotifyCohostAddedArgs]) error {
	row, err := w.Queries.GetCohostNotification(ctx, store.GetCohostNotificationParams{
		EventID: job.Args.EventID, UserID: job.Args.UserID,
	})
	if errors.Is(err, pgx.ErrNoRows) {
		return nil
	}
	if err != nil {
		return fmt.Errorf("notify_cohost_added: load notification: %w", err)
	}

	title := stripCRLF(row.EventTitle)
	text := fmt.Sprintf(
		"You've been added as a co-host of %s.\n\n"+
			"Sign in to see it:\n%s/login\n\n"+
			"See You There\n",
		title, w.SiteURL,
	)
	return w.Sender.Send(ctx, mail.Message{
		To:      row.Email,
		Subject: fmt.Sprintf("You're a co-host of %s", title),
		Text:    text,
	})
}

// NotifyTakedownArgs tells an event's owner that an admin took the page
// down or restored it (§4.11/§7). Action is "takedown" or "restore"; it
// carries no reason text (that's logged in audit_log, not emailed), so a
// held-back moderation note is never accidentally sent to the person it's
// about.
type NotifyTakedownArgs struct {
	EventID uuid.UUID `json:"event_id"`
	Action  string    `json:"action"`
}

func (NotifyTakedownArgs) Kind() string { return "notify_takedown" }

func (NotifyTakedownArgs) InsertOpts() river.InsertOpts {
	return river.InsertOpts{Queue: "email"}
}

// NotifyTakedownWorker emails the event's current owner. A deleted/
// ownerless event (GetEventForNotify requires a live owner_id) is a no-op:
// nobody to tell, and the caller already checked OwnerID != nil before
// enqueuing this for a takedown, since a slug-less draft can never be
// taken down in the first place.
type NotifyTakedownWorker struct {
	river.WorkerDefaults[NotifyTakedownArgs]
	Queries *store.Queries
	Sender  mail.Sender
	SiteURL string
}

func (w *NotifyTakedownWorker) Timeout(*river.Job[NotifyTakedownArgs]) time.Duration {
	return 30 * time.Second
}

func (w *NotifyTakedownWorker) Work(ctx context.Context, job *river.Job[NotifyTakedownArgs]) error {
	event, err := w.Queries.GetEventForNotify(ctx, job.Args.EventID)
	if errors.Is(err, pgx.ErrNoRows) {
		return nil
	}
	if err != nil {
		return fmt.Errorf("notify_takedown: load event: %w", err)
	}
	title := stripCRLF(event.Title)

	var subject, text string
	switch job.Args.Action {
	case "restore":
		subject = fmt.Sprintf("Your page is back up: %s", title)
		text = fmt.Sprintf(
			"Your page \"%s\" has been restored by a See You There moderator and is visible again.\n\n"+
				"Sign in to see it:\n%s/login\n\n"+
				"See You There\n",
			title, w.SiteURL,
		)
	default: // "takedown"
		subject = fmt.Sprintf("Your page was taken down: %s", title)
		text = fmt.Sprintf(
			"Your page \"%s\" has been taken down by a See You There moderator, usually in response to a "+
				"report. It's no longer visible to guests.\n\n"+
				"Sign in for more details:\n%s/login\n\n"+
				"See You There\n",
			title, w.SiteURL,
		)
	}
	return w.Sender.Send(ctx, mail.Message{To: event.OwnerEmail, Subject: subject, Text: text})
}
