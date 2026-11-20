package jobs

import (
	"context"
	"strings"
	"testing"

	"github.com/google/uuid"
	"github.com/riverqueue/river"

	"github.com/ansonarose/seeyouthere-backend/internal/ratelimit"
	"github.com/ansonarose/seeyouthere-backend/internal/store"
	"github.com/ansonarose/seeyouthere-backend/internal/token"
)

const hostedInviteCopy = "Your entry pass is on your invitation page. Show the QR code at the door."

// TestSendInviteWorker_HostedCopy checks that only a hosted event's invite
// email tells the guest about the QR entry pass, and that the invite link is
// still present either way.
func TestSendInviteWorker_HostedCopy(t *testing.T) {
	pool := dbPool(t)
	rdb := rdbClient(t)
	q := store.New(pool)
	ctx := context.Background()

	tokens, err := token.NewKeys(make([]byte, 32))
	if err != nil {
		t.Fatalf("token.NewKeys: %v", err)
	}
	tmpl, err := q.GetPublishedTemplateBySlug(ctx, store.GetPublishedTemplateBySlugParams{Slug: "classic", AllowPremium: false})
	if err != nil {
		t.Fatalf("get template: %v", err)
	}

	for _, tc := range []struct {
		kind       string
		wantCopy   bool
		wantStatus string
	}{
		{kind: "hosted", wantCopy: true, wantStatus: "published"},
		{kind: "invite", wantCopy: false, wantStatus: "published"},
		{kind: "hosted", wantCopy: false, wantStatus: "draft"}, // unpublished: silent no-op, nothing sent
	} {
		t.Run(tc.kind+"/"+tc.wantStatus, func(t *testing.T) {
			ownerID := uuid.Must(uuid.NewV7())
			if _, err := q.UpsertUserByEmail(ctx, store.UpsertUserByEmailParams{
				ID: ownerID, Email: "jobs-invite-owner-" + ownerID.String() + "@example.invalid",
			}); err != nil {
				t.Fatalf("create owner: %v", err)
			}
			eventID := uuid.Must(uuid.NewV7())
			if _, err := q.CreateEvent(ctx, store.CreateEventParams{
				ID: eventID, OwnerID: &ownerID, OccasionSlug: "birthday", Kind: tc.kind, Title: "Invite copy test",
				Content: []byte("[]"), Overrides: []byte("{}"), TemplateID: tmpl.ID, TemplateVersion: tmpl.Version,
			}); err != nil {
				t.Fatalf("create event: %v", err)
			}
			t.Cleanup(func() {
				_, _ = pool.Exec(context.Background(), "DELETE FROM events WHERE id = $1", eventID)
				_, _ = pool.Exec(context.Background(), "DELETE FROM users WHERE id = $1", ownerID)
			})
			slug := "invite-copy-" + strings.ReplaceAll(eventID.String(), "-", "")
			if tc.wantStatus == "published" {
				if _, err := pool.Exec(ctx, "UPDATE events SET status = 'published', published_at = now(), slug = $2 WHERE id = $1", eventID, slug); err != nil {
					t.Fatalf("publish event: %v", err)
				}
			} else if _, err := pool.Exec(ctx, "UPDATE events SET slug = $2 WHERE id = $1", eventID, slug); err != nil {
				t.Fatalf("set slug: %v", err)
			}

			email := "guest-" + eventID.String() + "@example.invalid"
			g, err := q.CreateGuest(ctx, store.CreateGuestParams{
				ID: uuid.Must(uuid.NewV7()), EventID: eventID, Name: "Pat", Email: &email, HouseholdSize: 2, UserID: ownerID,
			})
			if err != nil {
				t.Fatalf("create guest: %v", err)
			}

			sender := &recordingSender{}
			w := &SendInviteWorker{
				Queries: q, Sender: sender, Tokens: tokens, Limiter: ratelimit.New(rdb), SiteURL: "https://seeyouthere.at",
			}
			if err := w.Work(ctx, &river.Job[SendInviteArgs]{Args: SendInviteArgs{GuestID: g.ID}}); err != nil {
				t.Fatalf("Work: %v", err)
			}

			msgs := sender.messages()
			if tc.wantStatus != "published" {
				if len(msgs) != 0 {
					t.Fatalf("sent %d emails for an unpublished event, want 0", len(msgs))
				}
				return
			}
			if len(msgs) != 1 {
				t.Fatalf("sent %d emails, want 1", len(msgs))
			}
			body := msgs[0].Text
			if got := strings.Contains(body, hostedInviteCopy); got != tc.wantCopy {
				t.Errorf("hosted copy present = %v, want %v; body = %q", got, tc.wantCopy, body)
			}
			if !strings.Contains(body, "https://seeyouthere.at/"+slug+"/invite#"+tokens.GuestToken(g.ID, g.TokenVersion)) {
				t.Errorf("invite link missing from body = %q", body)
			}
			// The email carries the invite link only: the pass token must never be mailed.
			if strings.Contains(body, tokens.PassToken(g.ID, g.TokenVersion)) {
				t.Errorf("body leaks the entry pass token: %q", body)
			}
		})
	}
}
