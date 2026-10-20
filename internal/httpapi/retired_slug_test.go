package httpapi

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/ansonarose/seeyouthere-backend/internal/ratelimit"
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

// TestRetiredSlugs_OwnerExemption covers who the 30-day hold applies to: the user who owned the
// deleted event may take the slug back at once, everyone else (another user, an anonymous caller)
// is still held off, a tombstone whose owner is unknown holds against everyone, an event deleted
// while taken down or under an open report, or whose owner is deleted, suspended or banned, gives no
// exemption, the cleanup query drops only expired tombstones, and deleting a re-claimed slug again
// refreshes both the owner and the 30 days.
func TestRetiredSlugs_OwnerExemption(t *testing.T) {
	pool := dbTestPool(t)
	ctx := context.Background()
	tx, err := pool.Begin(ctx)
	if err != nil {
		t.Fatalf("begin: %v", err)
	}
	defer func() { _ = tx.Rollback(ctx) }()
	q := store.New(tx)

	newUser := func(label string) uuid.UUID {
		t.Helper()
		id := uuid.Must(uuid.NewV7())
		if _, err := q.UpsertUserByEmail(ctx, store.UpsertUserByEmailParams{ID: id, Email: "retired-" + label + "-" + id.String() + "@example.invalid"}); err != nil {
			t.Fatalf("create %s user: %v", label, err)
		}
		return id
	}
	owner, other := newUser("owner"), newUser("other")
	suffix := owner.String()[:8]

	// publish gives a new event of user's the slug and publishes it (status may be overridden).
	publish := func(user uuid.UUID, slug, status string) uuid.UUID {
		t.Helper()
		ev := createTestEvent(t, ctx, q, "birthday", "classic", &user)
		if _, err := tx.Exec(ctx, "UPDATE events SET slug = $2, status = $3, published_at = now() WHERE id = $1", ev.ID, slug, status); err != nil {
			t.Fatalf("publish event with %s: %v", slug, err)
		}
		return ev.ID
	}
	hardDelete := func(id uuid.UUID) {
		t.Helper()
		if _, err := tx.Exec(ctx, "DELETE FROM events WHERE id = $1", id); err != nil {
			t.Fatalf("delete event %s: %v", id, err)
		}
	}
	takenFor := func(slug string, caller *uuid.UUID) bool {
		t.Helper()
		got, err := q.SlugTaken(ctx, store.SlugTakenParams{Slug: slug, UserID: caller})
		if err != nil {
			t.Fatalf("SlugTaken(%s): %v", slug, err)
		}
		return got
	}
	tombstone := func(slug string) (ownerID *uuid.UUID, fresh bool) {
		t.Helper()
		// now() is the transaction start, so a tombstone written in this test has retired_at = now().
		if err := tx.QueryRow(ctx, "SELECT owner_id, retired_at = now() FROM retired_slugs WHERE slug = $1", slug).Scan(&ownerID, &fresh); err != nil {
			t.Fatalf("read tombstone %s: %v", slug, err)
		}
		return ownerID, fresh
	}
	retired := func(slug string) int {
		t.Helper()
		var n int
		if err := tx.QueryRow(ctx, "SELECT count(*) FROM retired_slugs WHERE slug = $1", slug).Scan(&n); err != nil {
			t.Fatalf("count retired_slugs: %v", err)
		}
		return n
	}
	// wantHold checks SlugTaken for the owner, the other user and an anonymous caller.
	wantHold := func(slug string, forOwner, forOther, forAnon bool) {
		t.Helper()
		if got := takenFor(slug, &owner); got != forOwner {
			t.Errorf("%s taken for its owner = %v, want %v", slug, got, forOwner)
		}
		if got := takenFor(slug, &other); got != forOther {
			t.Errorf("%s taken for another user = %v, want %v", slug, got, forOther)
		}
		if got := takenFor(slug, nil); got != forAnon {
			t.Errorf("%s taken for an anonymous caller = %v, want %v", slug, got, forAnon)
		}
	}

	// Owner exempt, others held.
	{
		slug := "own-" + suffix
		hardDelete(publish(owner, slug, "published"))
		if got, fresh := tombstone(slug); got == nil || *got != owner || !fresh {
			t.Errorf("tombstone owner = %v, fresh = %v; want %s, true", got, fresh, owner)
		}
		wantHold(slug, false, true, true)

		if _, err := tx.Exec(ctx, "UPDATE retired_slugs SET retired_at = now() - interval '31 days' WHERE slug = $1", slug); err != nil {
			t.Fatalf("backdate tombstone: %v", err)
		}
		wantHold(slug, false, false, false)
	}

	// Unknown owner holds against everyone.
	{
		slug := "legacy-" + suffix
		if _, err := tx.Exec(ctx, "INSERT INTO retired_slugs (slug) VALUES ($1)", slug); err != nil {
			t.Fatalf("insert legacy tombstone: %v", err)
		}
		wantHold(slug, true, true, true)
	}

	// Live and soft-deleted events still hold for their owner.
	{
		slug := "live-" + suffix
		id := publish(owner, slug, "published")
		wantHold(slug, true, true, true)
		if _, err := tx.Exec(ctx, "UPDATE events SET deleted_at = now() WHERE id = $1", id); err != nil {
			t.Fatalf("soft delete: %v", err)
		}
		wantHold(slug, true, true, true)
	}

	// Taken down event gives no exemption.
	{
		slug := "down-" + suffix
		hardDelete(publish(owner, slug, "taken_down"))
		if got, _ := tombstone(slug); got != nil {
			t.Errorf("tombstone owner = %v, want NULL", *got)
		}
		wantHold(slug, true, true, true)
	}

	// Open report gives no exemption.
	{
		slug := "report-" + suffix
		id := publish(owner, slug, "published")
		if _, err := tx.Exec(ctx, `INSERT INTO reports (id, event_id, reporter_ip_hash, reason)
			VALUES ($1, $2, '\x01'::bytea, 'spam')`, uuid.Must(uuid.NewV7()), id); err != nil {
			t.Fatalf("insert report: %v", err)
		}
		hardDelete(id)
		if got, _ := tombstone(slug); got != nil {
			t.Errorf("tombstone owner = %v, want NULL", *got)
		}
		wantHold(slug, true, true, true)
	}

	// An owner whose account is deleted, suspended or banned is not recorded.
	for _, c := range []struct{ name, update string }{
		{"deleted", "UPDATE users SET deleted_at = now() WHERE id = $1"},
		{"suspended", "UPDATE users SET status = 'suspended' WHERE id = $1"},
		{"banned", "UPDATE users SET status = 'banned' WHERE id = $1"},
	} {
		gone := newUser(c.name)
		slug := c.name + "-" + suffix
		id := publish(gone, slug, "published")
		if _, err := tx.Exec(ctx, c.update, gone); err != nil {
			t.Fatalf("mark owner %s: %v", c.name, err)
		}
		hardDelete(id)
		if got, _ := tombstone(slug); got != nil {
			t.Errorf("%s owner: tombstone owner = %v, want NULL", c.name, *got)
		}
		if !takenFor(slug, &gone) {
			t.Errorf("%s owner: slug not held against them, want held", c.name)
		}
	}

	// Expired tombstones are deleted by the cleanup query, fresh ones are kept.
	{
		expired, fresh := "exp-old-"+suffix, "exp-fresh-"+suffix
		if _, err := tx.Exec(ctx, `INSERT INTO retired_slugs (slug, retired_at, owner_id) VALUES
			($1, now() - interval '30 days 1 second', $3), ($2, now() - interval '29 days', $3)`, expired, fresh, owner); err != nil {
			t.Fatalf("insert tombstones: %v", err)
		}
		n, err := q.DeleteExpiredRetiredSlugs(ctx)
		if err != nil {
			t.Fatalf("DeleteExpiredRetiredSlugs: %v", err)
		}
		if n < 1 {
			t.Errorf("DeleteExpiredRetiredSlugs deleted %d rows, want at least the expired one", n)
		}
		if retired(expired) != 0 || retired(fresh) != 1 {
			t.Errorf("after cleanup: expired rows = %d, fresh rows = %d; want 0, 1", retired(expired), retired(fresh))
		}
	}

	// Re-delete refreshes owner and retired_at.
	{
		slug := "again-" + suffix
		hardDelete(publish(owner, slug, "published"))
		// The owner's hold lapses, another user claims the slug, publishes and deletes in turn.
		if _, err := tx.Exec(ctx, "UPDATE retired_slugs SET retired_at = now() - interval '31 days' WHERE slug = $1", slug); err != nil {
			t.Fatalf("backdate tombstone: %v", err)
		}
		hardDelete(publish(other, slug, "published"))
		if got, fresh := tombstone(slug); got == nil || *got != other || !fresh {
			t.Errorf("tombstone owner = %v, fresh = %v; want %s, true", got, fresh, other)
		}
		wantHold(slug, true, false, true)
	}
}

// TestRetiredSlug_OwnerReclaimsThroughHandlers: right after the owner's published event is hard-deleted,
// the slug check reports the link available to that owner and "taken" to another user, the settings
// write lets the owner put it on a new event while another user gets slug_taken, and the owner can
// publish it again.
func TestRetiredSlug_OwnerReclaimsThroughHandlers(t *testing.T) {
	pool := dbTestPool(t)
	rdb := dbTestRedis(t)
	ctx := context.Background()
	tx, err := pool.Begin(ctx)
	if err != nil {
		t.Fatalf("begin: %v", err)
	}
	defer func() { _ = tx.Rollback(ctx) }()
	q := store.New(tx)
	s := &Server{q: q, rdb: rdb, limiter: ratelimit.New(rdb), cfg: testConfig()}

	newUser := func(label string) uuid.UUID {
		t.Helper()
		id := uuid.Must(uuid.NewV7())
		if _, err := q.UpsertUserByEmail(ctx, store.UpsertUserByEmailParams{ID: id, Email: "reclaim-" + label + "-" + id.String() + "@example.invalid"}); err != nil {
			t.Fatalf("create %s user: %v", label, err)
		}
		return id
	}
	owner, other := newUser("owner"), newUser("other")
	slug := "reclaim-" + owner.String()[:8]

	deleted := createTestEvent(t, ctx, q, "birthday", "classic", &owner)
	if _, err := tx.Exec(ctx, "UPDATE events SET slug = $2, status = 'published', published_at = now() WHERE id = $1", deleted.ID, slug); err != nil {
		t.Fatalf("publish event: %v", err)
	}
	if _, err := tx.Exec(ctx, "DELETE FROM events WHERE id = $1", deleted.ID); err != nil {
		t.Fatalf("delete event: %v", err)
	}

	check := func(user uuid.UUID) string {
		t.Helper()
		req := withRouteParam(requestAs(http.MethodGet, "/", user), "slug", slug)
		rec := httptest.NewRecorder()
		s.handleCheckSlug(rec, req)
		if rec.Code != http.StatusOK {
			t.Fatalf("check slug: status = %d, want 200; body = %s", rec.Code, rec.Body.String())
		}
		return rec.Body.String()
	}
	if body := check(other); !containsAll(body, `"available":false`, `"reason":"taken"`) {
		t.Errorf("check as another user: body = %s, want available false, reason taken", body)
	}
	if body := check(owner); !containsAll(body, `"available":true`) {
		t.Errorf("check as the owner: body = %s, want available true", body)
	}

	post := func(handler http.HandlerFunc, user, eventID uuid.UUID, body string) *httptest.ResponseRecorder {
		t.Helper()
		req := withRouteID(requestAs(http.MethodPost, "/", user), eventID)
		req.Header.Set("Content-Type", "application/json")
		req.Body = newJSONBody(body)
		rec := httptest.NewRecorder()
		handler(rec, req)
		return rec
	}
	settingsBody := `{"version": 1, "slug": "` + slug + `"}`

	otherDraft := createTestEvent(t, ctx, q, "birthday", "classic", &other)
	if rec := post(s.handleUpdateEventSettings, other, otherDraft.ID, settingsBody); rec.Code != http.StatusConflict || !containsAll(rec.Body.String(), `"slug_taken"`) {
		t.Errorf("settings as another user: status = %d, body = %s; want 409 slug_taken", rec.Code, rec.Body.String())
	}

	ownerDraft := createTestEvent(t, ctx, q, "birthday", "classic", &owner)
	if rec := post(s.handleUpdateEventSettings, owner, ownerDraft.ID, settingsBody); rec.Code != http.StatusOK || !containsAll(rec.Body.String(), `"slug":"`+slug+`"`) {
		t.Fatalf("settings as the owner: status = %d, body = %s; want 200 with the slug", rec.Code, rec.Body.String())
	}
	if rec := post(s.handlePublishEvent, owner, ownerDraft.ID, `{"version": 2}`); rec.Code != http.StatusOK || !containsAll(rec.Body.String(), `"status":"published"`) {
		t.Errorf("publish as the owner: status = %d, body = %s; want 200 published", rec.Code, rec.Body.String())
	}
}

// seedTombstone commits a fresh tombstone owned by owner and removes it when the test ends.
func seedTombstone(t *testing.T, pool *pgxpool.Pool, owner uuid.UUID) string {
	t.Helper()
	slug := "ts-" + strings.ReplaceAll(uuid.Must(uuid.NewV7()).String(), "-", "")
	if _, err := pool.Exec(context.Background(), "INSERT INTO retired_slugs (slug, owner_id) VALUES ($1, $2)", slug, owner); err != nil {
		t.Fatalf("insert tombstone: %v", err)
	}
	t.Cleanup(func() { _, _ = pool.Exec(context.Background(), "DELETE FROM retired_slugs WHERE slug = $1", slug) })
	return slug
}

// tombstoneOwner returns a committed tombstone's owner_id (nil for NULL).
func tombstoneOwner(t *testing.T, pool *pgxpool.Pool, slug string) *uuid.UUID {
	t.Helper()
	var owner *uuid.UUID
	if err := pool.QueryRow(context.Background(), "SELECT owner_id FROM retired_slugs WHERE slug = $1", slug).Scan(&owner); err != nil {
		t.Fatalf("read tombstone %s: %v", slug, err)
	}
	return owner
}

// TestAdminDeleteEvent_RetiredSlugOwner: an admin deleting a live, unreported published event leaves
// its slug held against the owner too, while an admin delete of an event the owner had already
// deleted keeps the owner's exemption. Strangers and anonymous callers are held either way.
func TestAdminDeleteEvent_RetiredSlugOwner(t *testing.T) {
	for _, c := range []struct {
		name      string
		soft      bool
		ownerHeld bool // also means the tombstone owner is NULL
	}{
		{name: "live event", soft: false, ownerHeld: true},
		{name: "owner already deleted it", soft: true, ownerHeld: false},
	} {
		t.Run(c.name, func(t *testing.T) {
			a, blobs := newAdminAPIWithBlobs(t)
			ctx := context.Background()
			id, slug, _ := a.seedEventState(t, blobs, deleteEventCase{owner: true, publish: true, soft: c.soft})
			t.Cleanup(func() { _, _ = a.pool.Exec(context.Background(), "DELETE FROM retired_slugs WHERE slug = $1", slug) })
			var owner uuid.UUID
			if err := a.pool.QueryRow(ctx, "SELECT owner_id FROM events WHERE id = $1", id).Scan(&owner); err != nil {
				t.Fatalf("read owner: %v", err)
			}

			rec := a.do(http.MethodDelete, "/v1/admin/events/"+id.String(), a.superTok, `{"confirm":"`+slug+`"}`)
			if rec.Code != http.StatusNoContent {
				t.Fatalf("status = %d, body = %s, want 204", rec.Code, rec.Body.String())
			}

			if got := tombstoneOwner(t, a.pool, slug); (got == nil) != c.ownerHeld || (got != nil && *got != owner) {
				t.Errorf("tombstone owner = %v, want NULL = %v (owner %s)", got, c.ownerHeld, owner)
			}
			q := store.New(a.pool)
			stranger := uuid.Must(uuid.NewV7())
			for _, tc := range []struct {
				who    string
				caller *uuid.UUID
				want   bool
			}{{"owner", &owner, c.ownerHeld}, {"stranger", &stranger, true}, {"anonymous", nil, true}} {
				got, err := q.SlugTaken(ctx, store.SlugTakenParams{Slug: slug, UserID: tc.caller})
				if err != nil {
					t.Fatalf("SlugTaken: %v", err)
				}
				if got != tc.want {
					t.Errorf("taken for %s = %v, want %v", tc.who, got, tc.want)
				}
			}
		})
	}
}

// TestHandleDeleteAccount_ClearsRetiredSlugOwner: deleting an account clears that user's tombstone
// exemptions in the same transaction and leaves other users' alone.
func TestHandleDeleteAccount_ClearsRetiredSlugOwner(t *testing.T) {
	pool := dbTestPool(t)
	f := newAccountTestFixture(t, pool, dbTestRedis(t))
	mine, theirs := seedTombstone(t, pool, f.userA), seedTombstone(t, pool, f.userB)

	req := requestAs(http.MethodDelete, "/v1/me", f.userA)
	req.Body = newJSONBody(`{"confirm_email":"` + f.emailA + `"}`)
	req.Header.Set("Content-Type", "application/json")
	rec := httptest.NewRecorder()
	f.s.handleDeleteAccount(rec, req)
	if rec.Code != http.StatusNoContent {
		t.Fatalf("status = %d, want 204; body = %s", rec.Code, rec.Body.String())
	}
	if got := tombstoneOwner(t, pool, mine); got != nil {
		t.Errorf("deleted user's tombstone owner = %v, want NULL", *got)
	}
	if got := tombstoneOwner(t, pool, theirs); got == nil || *got != f.userB {
		t.Errorf("other user's tombstone owner = %v, want %s", got, f.userB)
	}
}

// TestHandleSetAdminUserStatus_ClearsRetiredSlugOwner: suspending or banning a user clears their
// tombstone exemptions; reactivating does not touch them.
func TestHandleSetAdminUserStatus_ClearsRetiredSlugOwner(t *testing.T) {
	pool := dbTestPool(t)
	f := newAdminHandlerFixture(t, pool, dbTestRedis(t))
	setStatus := func(status string) {
		t.Helper()
		req := jsonRequest(http.MethodPost, "/", f.superAdminID, f.superAdminSessID, []string{"super_admin"}, `{"status":"`+status+`","reason":""}`)
		req = withRouteID(req, f.plainUserID)
		rec := httptest.NewRecorder()
		f.s.handleSetAdminUserStatus(rec, req)
		if rec.Code != http.StatusOK {
			t.Fatalf("set %s: status = %d, body = %s", status, rec.Code, rec.Body.String())
		}
	}

	slug := seedTombstone(t, pool, f.plainUserID)
	setStatus("suspended")
	if got := tombstoneOwner(t, pool, slug); got != nil {
		t.Errorf("after suspend: tombstone owner = %v, want NULL", *got)
	}

	slug = seedTombstone(t, pool, f.plainUserID)
	setStatus("active")
	if got := tombstoneOwner(t, pool, slug); got == nil || *got != f.plainUserID {
		t.Errorf("after reactivate: tombstone owner = %v, want %s", got, f.plainUserID)
	}
	setStatus("banned")
	if got := tombstoneOwner(t, pool, slug); got != nil {
		t.Errorf("after ban: tombstone owner = %v, want NULL", *got)
	}
}

// TestAdminDeleteUser_ClearsRetiredSlugOwner: an admin deleting a user clears their tombstone
// exemptions in the same transaction.
func TestAdminDeleteUser_ClearsRetiredSlugOwner(t *testing.T) {
	a := newAdminAPI(t, dbTestPool(t), dbTestRedis(t))
	target, email := a.newTarget(t, "del-ts")
	slug := seedTombstone(t, a.pool, target)

	rec := a.do(http.MethodDelete, "/v1/admin/users/"+target.String(), a.superTok, `{"confirm_email":"`+email+`"}`)
	wantStatusCode(t, rec, http.StatusAccepted, "")
	if got := tombstoneOwner(t, a.pool, slug); got != nil {
		t.Errorf("tombstone owner = %v, want NULL", *got)
	}
}
