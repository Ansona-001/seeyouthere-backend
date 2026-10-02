package jobs

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"log/slog"
	"strings"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/riverqueue/river"
	"github.com/riverqueue/river/rivertype"

	"github.com/ansonarose/seeyouthere-backend/internal/mail"
	"github.com/ansonarose/seeyouthere-backend/internal/ratelimit"
	"github.com/ansonarose/seeyouthere-backend/internal/store"
	"github.com/ansonarose/seeyouthere-backend/internal/token"
)

// maxInvitesPerRecipientPerDay bounds how many invite emails any single
// address can receive per day, keyed on the address itself rather than the
// guest row: deleting and recreating a guest, or spreading one victim's
// address across many events/guest rows, must not reset or bypass this.
const maxInvitesPerRecipientPerDay = 3

// SendInviteArgs sends one guest's invite link by email. It never carries a
// raw token (decision 1 in the build-out plan): the link is derived from the
// guest's current token_version at send time, so a guest whose link was
// rotated after this job was enqueued gets an email with the *new* link, and
// a rotated-then-restored version never resurrects an old, possibly leaked
// token.
type SendInviteArgs struct {
	GuestID uuid.UUID `json:"guest_id"`
}

func (SendInviteArgs) Kind() string { return "send_invite" }

// requiredUniqueStates is the minimal ByState set River accepts (see
// river.UniqueOpts.ByState), used here to de-duplicate concurrent or
// double-clicked send-invite requests for the same guest while a job is
// still in flight.
var requiredUniqueStates = []rivertype.JobState{
	rivertype.JobStateAvailable,
	rivertype.JobStatePending,
	rivertype.JobStateRunning,
	rivertype.JobStateScheduled,
}

func (SendInviteArgs) InsertOpts() river.InsertOpts {
	return river.InsertOpts{
		// Retrying for days is pointless once the guest, email or event has
		// changed; the worker no-ops in every one of those cases anyway.
		MaxAttempts: 5,
		Queue:       "email",
		UniqueOpts: river.UniqueOpts{
			ByArgs:  true,
			ByState: requiredUniqueStates,
		},
	}
}

// SendInviteWorker sends one guest's invite email. Tokens and SiteURL are
// the same subkeys and origin the HTTP API uses, so a link built here is
// byte-for-byte what the host's own "copy link" button would produce.
type SendInviteWorker struct {
	river.WorkerDefaults[SendInviteArgs]
	Queries *store.Queries
	Sender  mail.Sender
	Tokens  *token.Keys
	Limiter *ratelimit.Limiter
	SiteURL string
}

func (w *SendInviteWorker) Timeout(*river.Job[SendInviteArgs]) time.Duration { return 30 * time.Second }

// Work re-checks the guest, its email and the event's status before sending:
// any of them may have changed between the send-invites request and this
// job running (guest deleted, email cleared, event unpublished or taken
// down), and every such case is a silent no-op rather than a failure.
func (w *SendInviteWorker) Work(ctx context.Context, job *river.Job[SendInviteArgs]) error {
	g, err := w.Queries.GetGuestForInviteEmail(ctx, job.Args.GuestID)
	if errors.Is(err, pgx.ErrNoRows) {
		return nil
	}
	if err != nil {
		return fmt.Errorf("send_invite: load guest: %w", err)
	}
	if g.Email == nil || strings.TrimSpace(*g.Email) == "" {
		return nil
	}
	if g.EventStatus != "published" || g.EventSlug == nil {
		return nil
	}

	// Cap per-recipient, keyed on the address itself (HMAC-free but still
	// unreversible-enough: a keyless SHA-256 digest, matching how other
	// per-secret rate-limit keys in this codebase are derived), so it
	// survives the guest row being deleted and recreated, and catches the
	// same address invited across multiple events/guest rows.
	recipient := strings.ToLower(strings.TrimSpace(*g.Email))
	sum := sha256.Sum256([]byte(recipient))
	rcptKey := "invites:rcpt:" + hex.EncodeToString(sum[:])
	allowed, err := w.Limiter.AllowN(ctx, rcptKey, 1, maxInvitesPerRecipientPerDay, 24*time.Hour)
	if err != nil {
		return fmt.Errorf("send_invite: rate limit: %w", err)
	}
	if !allowed {
		slog.InfoContext(ctx, "send_invite: recipient rate limited", "guest_id", g.ID)
		return nil
	}

	link := w.Tokens.GuestToken(g.ID, g.TokenVersion)
	inviteURL := fmt.Sprintf("%s/%s/invite#%s", w.SiteURL, *g.EventSlug, link)
	reportURL := fmt.Sprintf("%s/%s#report", w.SiteURL, *g.EventSlug)

	host := "The host"
	if g.OwnerName != nil && strings.TrimSpace(*g.OwnerName) != "" {
		host = stripCRLF(*g.OwnerName)
	}
	title := stripCRLF(g.EventTitle)

	var when string
	if g.EventStartsAt != nil {
		when = g.EventStartsAt.UTC().Format("Monday, January 2, 2006")
	}

	var b strings.Builder
	fmt.Fprintf(&b, "%s invited you to %s.\n\n", host, title)
	if when != "" {
		fmt.Fprintf(&b, "When: %s\n\n", when)
	}
	fmt.Fprintf(&b, "View the invitation and RSVP:\n%s\n\n", inviteURL)
	fmt.Fprintf(&b, "Report this page: %s\n", reportURL)

	// A failed send must not burn the recipient's daily quota: MaxAttempts
	// retries the same job up to 5 times, and without refunding here a
	// misconfigured Sender would consume all 5 of a victim's slots without
	// ever delivering anything, blocking their other invite/confirmation
	// emails for 24h.
	if err := w.Sender.Send(ctx, mail.Message{
		To:      *g.Email,
		Subject: fmt.Sprintf("You're invited: %s", title),
		Text:    b.String(),
	}); err != nil {
		w.Limiter.Uncount(ctx, rcptKey, 1)
		return err
	}
	return nil
}
