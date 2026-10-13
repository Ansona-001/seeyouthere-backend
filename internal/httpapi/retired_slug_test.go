package httpapi

import (
	"context"
	"testing"

	"github.com/google/uuid"

	"github.com/ansonarose/seeyouthere-backend/internal/store"
)

// TestRetiredSlugs covers the tombstone left when a published event is hard-deleted: the slug stays
// taken for 30 days (a printed invitation link must not be claimable by someone else the moment the
// event's data is purged), a never-published event leaves none, and an old tombstone expires.
func TestRetiredSlugs(t *testing.T) {
	pool := dbTestPool(t)
	ctx := context.Background()
	tx, err := pool.Begin(ctx)
	if err != nil {
		t.Fatalf("begin: %v", err)
	}
	defer func() { _ = tx.Rollback(ctx) }()
	q := store.New(tx)

	owner := uuid.Must(uuid.NewV7())
	if _, err := q.UpsertUserByEmail(ctx, store.UpsertUserByEmailParams{ID: owner, Email: "retired-" + owner.String() + "@example.invalid"}); err != nil {
		t.Fatalf("create user: %v", err)
	}
	suffix := owner.String()[:8]
	published := createTestEvent(t, ctx, q, "birthday", "classic", &owner)
	draft := createTestEvent(t, ctx, q, "birthday", "classic", &owner)
	pubSlug, draftSlug := "retire-pub-"+suffix, "retire-draft-"+suffix

	if _, err := tx.Exec(ctx, "UPDATE events SET slug = $2, status = 'published', published_at = now() WHERE id = $1", published.ID, pubSlug); err != nil {
		t.Fatalf("publish event: %v", err)
	}
	if _, err := tx.Exec(ctx, "UPDATE events SET slug = $2 WHERE id = $1", draft.ID, draftSlug); err != nil {
		t.Fatalf("set draft slug: %v", err)
	}

	taken := func(slug string) bool {
		t.Helper()
		got, err := q.SlugTaken(ctx, store.SlugTakenParams{Slug: slug})
		if err != nil {
			t.Fatalf("SlugTaken(%s): %v", slug, err)
		}
		return got
	}
	retired := func(slug string) int {
		t.Helper()
		var n int
		if err := tx.QueryRow(ctx, "SELECT count(*) FROM retired_slugs WHERE slug = $1", slug).Scan(&n); err != nil {
			t.Fatalf("count retired_slugs: %v", err)
		}
		return n
	}

	for _, id := range []uuid.UUID{published.ID, draft.ID} {
		if _, err := tx.Exec(ctx, "DELETE FROM events WHERE id = $1", id); err != nil {
			t.Fatalf("delete event %s: %v", id, err)
		}
	}

	if retired(pubSlug) != 1 || !taken(pubSlug) {
		t.Errorf("published slug after hard delete: retired rows = %d, taken = %v; want 1, true", retired(pubSlug), taken(pubSlug))
	}
	if retired(draftSlug) != 0 || taken(draftSlug) {
		t.Errorf("never-published slug after hard delete: retired rows = %d, taken = %v; want 0, false", retired(draftSlug), taken(draftSlug))
	}

	if _, err := tx.Exec(ctx, "UPDATE retired_slugs SET retired_at = now() - interval '31 days' WHERE slug = $1", pubSlug); err != nil {
		t.Fatalf("backdate tombstone: %v", err)
	}
	if taken(pubSlug) {
		t.Error("slug retired 31 days ago is still taken, want free again")
	}
}
