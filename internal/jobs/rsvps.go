package jobs

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"log/slog"
	"math/rand/v2"
	"strings"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/riverqueue/river"

	"github.com/ansonarose/seeyouthere-backend/internal/content"
	"github.com/ansonarose/seeyouthere-backend/internal/ics"
	"github.com/ansonarose/seeyouthere-backend/internal/mail"
	"github.com/ansonarose/seeyouthere-backend/internal/ratelimit"
	"github.com/ansonarose/seeyouthere-backend/internal/store"
	"github.com/ansonarose/seeyouthere-backend/internal/token"
)

// maxRSVPConfirmationsPerRecipientPerDay bounds how many confirmation
// emails any single address can receive per day, keyed on the address
// itself rather than the RSVP row (same reasoning, and same mechanism, as
// send_invite's per-recipient cap): a submitter can put any email address
// into the RSVP form, so this is the backstop against using the RSVP
// endpoint to spam a third party via repeated new open RSVPs.
const maxRSVPConfirmationsPerRecipientPerDay = 5

const (
	// MaxRegistrationConfirmationsPerEventPerDay bounds registration confirmations per event per UTC
	// day: the shared public link of a hosted event can otherwise be used to mail arbitrary addresses
	// at volume. It sits above the expected sign-up volume of one day (an event holds at most 5000
	// registrations in total) so legitimate sign-ups are not starved. Over it the mail is deferred to
	// the next UTC day, not dropped.
	MaxRegistrationConfirmationsPerEventPerDay = 2000

	// registrationConfirmationMaxAge is how long a registration confirmation may stay deferred by the
	// daily caps. A job older than this is dropped (logged) instead of snoozed again, so a permanently
	// saturated cap cannot keep jobs alive forever. River does not count a snooze as an attempt, so
	// MaxAttempts cannot bound this; the job's age does.
	registrationConfirmationMaxAge = 3 * 24 * time.Hour
	// snoozeJitter spreads the jobs deferred by a cap over the minutes after midnight, so they do not
	// all wake at the same instant.
	snoozeJitter = 10 * time.Minute
)

// RSVPConfirmEventKey is the limiter key for the per-event daily count of RSVP confirmation emails.
func RSVPConfirmEventKey(eventID uuid.UUID, day string) string {
	return "rsvp_confirm:event:" + eventID.String() + ":" + day
}

// RegistrationConfirmGlobalKey is the limiter key for the global daily count of registration
// confirmations. It is separate from the invite / RSVP confirmation counter so sign-up volume cannot
// starve other mail.
func RegistrationConfirmGlobalKey(day string) string { return "rsvp_confirm:reg:global:" + day }

// SendRSVPConfirmationArgs emails whoever just submitted an RSVP a link back
// to it: the guest's invite link for a guest RSVP, or the open RSVP's own
// edit link otherwise. Enqueued on create, and on update only if the email
// changed (§4.4); never carries a raw token (decision 1), the same rule as
// send_invite. Resend marks a duplicate-registration link re-send, which skips the
// per-event cap (recovery must still work once an event is flooded with sign-ups).
type SendRSVPConfirmationArgs struct {
	RSVPID uuid.UUID `json:"rsvp_id"`
	Resend bool      `json:"resend,omitempty"`
}

func (SendRSVPConfirmationArgs) Kind() string { return "send_rsvp_confirmation" }

func (SendRSVPConfirmationArgs) InsertOpts() river.InsertOpts {
	return river.InsertOpts{MaxAttempts: 5, Queue: "email"}
}

type SendRSVPConfirmationWorker struct {
	river.WorkerDefaults[SendRSVPConfirmationArgs]
	Queries *store.Queries
	Sender  mail.Sender
	// CalendarSender sends the calendar-invite variant of this email (an
	// attending="yes" response, §below). Configured separately so the
	// invite can go out from a different mailbox (config.RSVPMailFrom);
	// falls back to the primary sender in cmd/api/main.go when unset.
	CalendarSender mail.Sender
	Tokens         *token.Keys
	Limiter        *ratelimit.Limiter
	SiteURL        string
	// RegistrationDailyCap is the global daily cap on registration confirmations
	// (config.MailDailyRegistrationCap).
	RegistrationDailyCap int64
}

func (w *SendRSVPConfirmationWorker) Timeout(*river.Job[SendRSVPConfirmationArgs]) time.Duration {
	return 30 * time.Second
}

// Work re-checks the RSVP, its email and the event's status before sending:
// any of them may have changed between the write and this job running (the
// RSVP deleted, the event unpublished or taken down). Every such case is a
// silent no-op, matching send_invite.
//
// A registration's mail is counted here, not at enqueue, so a cap can defer it: over the per-event
// daily cap or the global registration cap the job snoozes to just after the next UTC midnight
// (River does not count a snooze as an attempt) and is dropped only once it is
// registrationConfirmationMaxAge old. The per-recipient cap is the anti-bombing limit and drops the
// mail: retrying later would only mail the same third party again. Every counter taken for a mail
// that is not sent is refunded.
func (w *SendRSVPConfirmationWorker) Work(ctx context.Context, job *river.Job[SendRSVPConfirmationArgs]) error {
	r, err := w.Queries.GetRSVPForConfirmation(ctx, job.Args.RSVPID)
	if errors.Is(err, pgx.ErrNoRows) {
		return nil
	}
	if err != nil {
		return fmt.Errorf("send_rsvp_confirmation: load rsvp: %w", err)
	}
	if r.Email == nil || strings.TrimSpace(*r.Email) == "" {
		return nil
	}
	if r.EventStatus != "published" || r.EventSlug == nil {
		return nil
	}

	// Counters taken so far, refunded if the mail does not go out.
	var held []string
	refund := func(ctx context.Context) {
		for _, k := range held {
			w.Limiter.Uncount(ctx, k, 1)
		}
	}
	if r.Registration {
		today := time.Now().UTC().Format("2006-01-02")
		if !job.Args.Resend {
			eventKey := RSVPConfirmEventKey(r.EventID, today)
			ok, err := w.Limiter.AllowN(ctx, eventKey, 1, MaxRegistrationConfirmationsPerEventPerDay, 24*time.Hour)
			if err != nil {
				return fmt.Errorf("send_rsvp_confirmation: event rate limit: %w", err)
			}
			if !ok {
				return deferRegistrationConfirmation(ctx, job, r.ID, r.EventID, "event")
			}
			held = append(held, eventKey)
		}
		globalKey := RegistrationConfirmGlobalKey(today)
		ok, err := w.Limiter.AllowN(ctx, globalKey, 1, w.RegistrationDailyCap, 48*time.Hour)
		if err != nil {
			refund(ctx)
			return fmt.Errorf("send_rsvp_confirmation: global rate limit: %w", err)
		}
		if !ok {
			refund(ctx)
			return deferRegistrationConfirmation(ctx, job, r.ID, r.EventID, "global")
		}
		held = append(held, globalKey)
	}

	recipient := strings.ToLower(strings.TrimSpace(*r.Email))
	sum := sha256.Sum256([]byte(recipient))
	rcptKey := "rsvp_confirm:rcpt:" + hex.EncodeToString(sum[:])
	allowed, err := w.Limiter.AllowN(ctx, rcptKey, 1, maxRSVPConfirmationsPerRecipientPerDay, 24*time.Hour)
	if err != nil {
		refund(ctx)
		return fmt.Errorf("send_rsvp_confirmation: rate limit: %w", err)
	}
	if !allowed {
		refund(ctx)
		slog.InfoContext(ctx, "send_rsvp_confirmation: recipient rate limited, dropping", "rsvp_id", r.ID)
		return nil
	}
	held = append(held, rcptKey)

	var editURL string
	if r.GuestID != nil && r.GuestTokenVersion != nil {
		link := w.Tokens.GuestToken(*r.GuestID, *r.GuestTokenVersion)
		editURL = fmt.Sprintf("%s/%s/invite#%s", w.SiteURL, *r.EventSlug, link)
	} else {
		link := w.Tokens.RSVPToken(r.ID, r.EditTokenVersion)
		editURL = fmt.Sprintf("%s/%s/rsvp#%s", w.SiteURL, *r.EventSlug, link)
	}
	reportURL := fmt.Sprintf("%s/%s#report", w.SiteURL, *r.EventSlug)
	title := stripCRLF(r.EventTitle)

	var when string
	if r.EventStartsAt != nil {
		when = r.EventStartsAt.UTC().Format("Monday, January 2, 2006")
	}

	var b strings.Builder
	fmt.Fprintf(&b, "Thanks for responding to %s.\n\n", title)
	if when != "" {
		fmt.Fprintf(&b, "When: %s\n\n", when)
	}
	fmt.Fprintf(&b, "Your response: %s", r.Attending)
	if r.Attending != "no" {
		fmt.Fprintf(&b, " (%d attending)", r.Count)
	}
	if r.Registration && r.EventEntryPasses && r.Attending == "yes" {
		// The link opens the registration page, which shows the entry pass as a QR code. Anyone with
		// the link holds the pass, so the email says so.
		fmt.Fprintf(&b, "\n\nYour entry pass: %s\n\nShow it at the door. Keep this email to yourself: anyone with the link can use your pass. You can also change your response from that page.\n\n", editURL)
	} else if r.Registration && r.EventEntryPasses {
		// Only a Yes holds an entry pass; a maybe or no registration gets the same link to change its reply.
		fmt.Fprintf(&b, "\n\nWe got your reply. You can change it any time:\n%s\n\nOnly a Yes gets an entry pass. If you change your reply to Yes, your pass will be on that page. Keep this email to yourself: anyone with the link can change your reply.\n\n", editURL)
	} else {
		fmt.Fprintf(&b, "\n\nYou can change your response any time:\n%s\n\n", editURL)
	}

	// A failed send must not burn the daily quotas: MaxAttempts retries the
	// same job up to 5 times, and without refunding here a misconfigured
	// Sender would consume all 5 of a recipient's slots (and the event and
	// global counters) without ever delivering anything, blocking their
	// other confirmation emails for 24h.
	if r.Attending != "yes" {
		fmt.Fprintf(&b, "Report this page: %s\n", reportURL)
		if err := w.Sender.Send(ctx, mail.Message{
			To:      *r.Email,
			Subject: fmt.Sprintf("Your RSVP for %s", title),
			Text:    b.String(),
		}); err != nil {
			refund(ctx)
			return err
		}
		return nil
	}

	// attending == "yes": attach a calendar invite and send from the
	// (optionally separate) calendar identity. The public page has its own
	// Add-to-Calendar buttons too, so this is never the only way to get it.
	eventURL := fmt.Sprintf("%s/%s", w.SiteURL, *r.EventSlug)
	fmt.Fprintf(&b, "Add this to your calendar — see the attached invite, or visit %s#when\n\n", eventURL)
	fmt.Fprintf(&b, "Report this page: %s\n", reportURL)

	msg := mail.Message{
		To:      *r.Email,
		Subject: fmt.Sprintf("Your RSVP for %s", title),
		Text:    b.String(),
	}
	start, end, location := content.ExtractCalendarInfo(r.EventContent)
	if start != nil {
		var endTime time.Time
		if end != nil {
			endTime = *end
		}
		msg.Attachments = []mail.Attachment{{
			Filename:    "event.ics",
			ContentType: "text/calendar; charset=utf-8; method=PUBLISH",
			Content:     ics.Build(title, *start, endTime, location),
		}}
	}

	sender := w.CalendarSender
	if sender == nil {
		sender = w.Sender
	}
	if err := sender.Send(ctx, msg); err != nil {
		refund(ctx)
		return err
	}
	return nil
}

// NotifyRSVPsArgs coalesces new RSVPs into one digest email per event,
// scheduled ten minutes after the first new response (§7, §11 decision):
// enqueued with a fresh ScheduledAt on every RSVP write while the event's
// notify_rsvps is on, but InsertOpts' unique args (while
// available/scheduled) mean only the first such enqueue in a burst actually
// creates a job.
type NotifyRSVPsArgs struct {
	EventID uuid.UUID `json:"event_id"`
}

func (NotifyRSVPsArgs) Kind() string { return "notify_rsvps" }

func (NotifyRSVPsArgs) InsertOpts() river.InsertOpts {
	return river.InsertOpts{
		Queue: "email",
		UniqueOpts: river.UniqueOpts{
			ByArgs:  true,
			ByState: requiredUniqueStates,
		},
	}
}

// maxDigestListed bounds how many individual RSVPs the digest email names
// before falling back to just the total count (ListRSVPsUpdatedSince itself
// caps at 50; this is the same number, named for the digest's own use).
const maxDigestListed = 50

type NotifyRSVPsWorker struct {
	river.WorkerDefaults[NotifyRSVPsArgs]
	Queries *store.Queries
	Sender  mail.Sender
	SiteURL string
}

func (w *NotifyRSVPsWorker) Timeout(*river.Job[NotifyRSVPsArgs]) time.Duration {
	return 30 * time.Second
}

// Work claims the digest watermark first (so a failure past this point
// still moves the window forward rather than replaying the same RSVPs
// forever on retry), then loads what's new since the previous watermark and
// emails the owner one digest. No-op if the event is gone, notify_rsvps has
// since been turned off, or nothing changed in the window.
func (w *NotifyRSVPsWorker) Work(ctx context.Context, job *river.Job[NotifyRSVPsArgs]) error {
	watermark, err := w.Queries.ClaimRSVPWatermark(ctx, job.Args.EventID)
	if errors.Is(err, pgx.ErrNoRows) {
		return nil
	}
	if err != nil {
		return fmt.Errorf("notify_rsvps: claim watermark: %w", err)
	}

	event, err := w.Queries.GetEventForNotify(ctx, job.Args.EventID)
	if errors.Is(err, pgx.ErrNoRows) {
		return nil
	}
	if err != nil {
		return fmt.Errorf("notify_rsvps: load event: %w", err)
	}
	if !event.NotifyRsvps {
		return nil
	}

	rows, err := w.Queries.ListRSVPsUpdatedSince(ctx, store.ListRSVPsUpdatedSinceParams{
		EventID: job.Args.EventID, Since: watermark.PreviousNotifiedAt, Until: watermark.NotifiedAt,
	})
	if err != nil {
		return fmt.Errorf("notify_rsvps: list updated: %w", err)
	}
	if len(rows) == 0 {
		return nil
	}

	title := stripCRLF(event.Title)
	total := rows[0].Total

	var b strings.Builder
	fmt.Fprintf(&b, "%d new or updated RSVP", total)
	if total != 1 {
		b.WriteString("s")
	}
	fmt.Fprintf(&b, " for %s.\n\n", title)
	for i, r := range rows {
		if i >= maxDigestListed {
			break
		}
		name := stripCRLF(r.Name)
		if r.Attending == "no" {
			fmt.Fprintf(&b, "- %s: not attending\n", name)
		} else {
			fmt.Fprintf(&b, "- %s: %s (%d attending)\n", name, r.Attending, r.Count)
		}
	}
	if total > int64(len(rows)) {
		fmt.Fprintf(&b, "\n...and %d more.\n", total-int64(len(rows)))
	}
	fmt.Fprintf(&b, "\nSee the full list:\n%s/app/events/%s/rsvps\n", w.SiteURL, job.Args.EventID)

	return w.Sender.Send(ctx, mail.Message{
		To:      event.OwnerEmail,
		Subject: fmt.Sprintf("New RSVPs for %s", title),
		Text:    b.String(),
	})
}

// deferRegistrationConfirmation snoozes a registration confirmation that hit a daily cap to just
// after the next UTC midnight, when the day-keyed counters restart. A job that has already waited
// registrationConfirmationMaxAge is dropped instead. which names the cap ("event" or "global").
func deferRegistrationConfirmation(ctx context.Context, job *river.Job[SendRSVPConfirmationArgs], rsvpID, eventID uuid.UUID, which string) error {
	now := time.Now()
	if now.Sub(job.CreatedAt) >= registrationConfirmationMaxAge {
		slog.WarnContext(ctx, "send_rsvp_confirmation: registration mail deferred too long, dropping",
			"rsvp_id", rsvpID, "event_id", eventID, "cap", which)
		return nil
	}
	d := untilNextUTCDay(now) + time.Minute + rand.N(snoozeJitter)
	slog.InfoContext(ctx, "send_rsvp_confirmation: daily mail cap reached, deferring to next UTC day",
		"rsvp_id", rsvpID, "event_id", eventID, "cap", which, "snooze", d.Round(time.Second).String())
	return river.JobSnooze(d)
}

// untilNextUTCDay is the time from now to the next 00:00 UTC.
func untilNextUTCDay(now time.Time) time.Duration {
	n := now.UTC()
	return time.Date(n.Year(), n.Month(), n.Day()+1, 0, 0, 0, 0, time.UTC).Sub(n)
}
