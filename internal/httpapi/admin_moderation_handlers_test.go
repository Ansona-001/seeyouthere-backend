package httpapi

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/ansonarose/seeyouthere-backend/internal/media"
	"github.com/ansonarose/seeyouthere-backend/internal/store"
)

// TestHandleListAdminMedia_IncludesGuestName covers a regression where
// ListGuestMediaAdmin didn't join guests, so the moderation queue always
// showed "a guest" instead of the uploader's actual name.
func TestHandleListAdminMedia_IncludesGuestName(t *testing.T) {
	pool := dbTestPool(t)
	rdb := dbTestRedis(t)
	f := newMediaTestFixture(t, pool, rdb)
	ctx := context.Background()

	slug := "admin-media-" + strings.ReplaceAll(f.eventID.String(), "-", "")
	settings, err := f.s.q.UpdateEventSettings(ctx, store.UpdateEventSettingsParams{
		Slug: &slug, EventID: f.eventID, UserID: f.ownerID, Version: 1,
	})
	if err != nil {
		t.Fatalf("set slug: %v", err)
	}
	if _, err := f.s.q.PublishEvent(ctx, store.PublishEventParams{EventID: f.eventID, UserID: f.ownerID, Version: settings.Version}); err != nil {
		t.Fatalf("publish event: %v", err)
	}

	guest, err := f.s.q.CreateGuest(ctx, store.CreateGuestParams{
		ID: uuid.Must(uuid.NewV7()), EventID: f.eventID, UserID: f.ownerID, Name: "Pat Admin Test",
		HouseholdSize: 1,
	})
	if err != nil {
		t.Fatalf("create guest: %v", err)
	}

	mediaID := uuid.Must(uuid.NewV7())
	outDir, result := processTestUpload(t, f.s, testJPEG(t))
	if err := f.s.media.Commit(ctx, outDir, f.eventID, mediaID); err != nil {
		t.Fatalf("commit: %v", err)
	}
	if _, err := f.s.q.CreateGuestMedia(ctx, store.CreateGuestMediaParams{
		ID: mediaID, StorageKey: f.eventID.String() + "/" + mediaID.String(), SizeBytes: result.Bytes,
		Width: int32(result.Width), Height: int32(result.Height), EventID: f.eventID,
		GuestID: &guest.ID,
	}); err != nil {
		t.Fatalf("create guest media: %v", err)
	}

	req := httptest.NewRequest(http.MethodGet, "/?status=pending", nil)
	rec := httptest.NewRecorder()
	f.s.handleListAdminMedia(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, body = %s", rec.Code, rec.Body.String())
	}
	if !containsAll(rec.Body.String(), `"guest_name":"Pat Admin Test"`) {
		t.Fatalf("body = %s, want guest_name \"Pat Admin Test\"", rec.Body.String())
	}
}

// --- admin delete event (driven through the real router) ---

// deleteEventCase builds one event in a given state, with a guest, an RSVP, a
// media row and a stored object, and returns what the delete must confirm.
type deleteEventCase struct {
	name     string
	owner    bool   // owned by a fresh user (else an anonymous draft)
	publish  bool   // published with a slug
	takeDown bool   // status taken_down (needs a slug)
	soft     bool   // soft-deleted already
	reported bool   // has an open report
	confirm  string // "" means the slug
}

func (a adminAPI) seedEventState(t *testing.T, blobs *testBlobs, c deleteEventCase) (id uuid.UUID, confirm string, ownerEmail string) {
	t.Helper()
	ctx := context.Background()
	var owner *uuid.UUID
	if c.owner {
		oid, email := a.newTarget(t, "evt-owner")
		owner, ownerEmail = &oid, email
	}
	ev := createTestEvent(t, ctx, a.f.s.q, "birthday", "classic", owner)
	id = ev.ID
	t.Cleanup(func() { _, _ = a.pool.Exec(context.Background(), "DELETE FROM events WHERE id = $1", id) })

	exec := func(sql string, args ...any) {
		t.Helper()
		if _, err := a.pool.Exec(ctx, sql, args...); err != nil {
			t.Fatalf("exec %q: %v", sql, err)
		}
	}
	confirm = "delete"
	switch {
	case c.takeDown:
		slug := "evt-td-" + id.String()[:8]
		exec("UPDATE events SET status = 'taken_down', slug = $2 WHERE id = $1", id, slug)
		confirm = slug
	case c.publish:
		slug := "evt-pub-" + id.String()[:8]
		exec("UPDATE events SET status = 'published', slug = $2, published_at = now() WHERE id = $1", id, slug)
		confirm = slug
	}
	if c.soft {
		exec("UPDATE events SET deleted_at = now() - interval '1 day' WHERE id = $1", id)
	}
	guestID, rsvpID, mediaID := uuid.Must(uuid.NewV7()), uuid.Must(uuid.NewV7()), uuid.Must(uuid.NewV7())
	exec("INSERT INTO guests (id, event_id, name, email) VALUES ($1, $2, 'Guest', 'guest-"+id.String()+"@example.test')", guestID, id)
	exec("INSERT INTO rsvps (id, event_id, guest_id, name, attending) VALUES ($1, $2, $3, 'Guest', 'yes')", rsvpID, id, guestID)
	key := "e/" + id.String() + "/" + mediaID.String() + ".jpg"
	exec(`INSERT INTO media (id, event_id, storage_key, content_type, size_bytes, width, height, uploaded_by, moderation_status)
		VALUES ($1, $2, $3, 'image/jpeg', 100, 10, 10, 'host', 'approved')`, mediaID, id, key)
	if err := blobs.inner.Put(ctx, key, bytes.NewReader([]byte("img")), 3); err != nil {
		t.Fatalf("put object: %v", err)
	}
	if c.reported {
		exec("INSERT INTO reports (id, event_id, reporter_ip_hash, reason, status) VALUES ($1, $2, $3, 'spam', 'open')",
			uuid.Must(uuid.NewV7()), id, []byte(id.String()))
	}
	if c.confirm != "" {
		confirm = c.confirm
	}
	return id, confirm, ownerEmail
}

func eventChildCount(t *testing.T, pool *pgxpool.Pool, id uuid.UUID) (events, guests, rsvps, media, reports int) {
	t.Helper()
	err := pool.QueryRow(context.Background(), `SELECT
		(SELECT count(*) FROM events WHERE id = $1), (SELECT count(*) FROM guests WHERE event_id = $1),
		(SELECT count(*) FROM rsvps WHERE event_id = $1), (SELECT count(*) FROM media WHERE event_id = $1),
		(SELECT count(*) FROM reports WHERE event_id = $1)`, id).Scan(&events, &guests, &rsvps, &media, &reports)
	if err != nil {
		t.Fatalf("count event rows: %v", err)
	}
	return
}

func newAdminAPIWithBlobs(t *testing.T) (adminAPI, *testBlobs) {
	t.Helper()
	a := newAdminAPI(t, dbTestPool(t), dbTestRedis(t))
	st, blobs := newTestMediaStoreBlobs(t)
	a.f.s.media = st
	return a, blobs
}

func TestAdminDeleteEvent_Success(t *testing.T) {
	cases := []deleteEventCase{
		{name: "published", owner: true, publish: true, reported: true},
		{name: "taken down", owner: true, takeDown: true, reported: true},
		{name: "soft-deleted", owner: true, publish: true, soft: true},
		{name: "owned draft without a slug", owner: true},
		{name: "anonymous draft", owner: false},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			a, blobs := newAdminAPIWithBlobs(t)
			id, confirm, ownerEmail := a.seedEventState(t, blobs, c)
			if objectCount(t, blobs, id) != 1 {
				t.Fatal("seed object missing")
			}

			rec := a.do(http.MethodDelete, "/v1/admin/events/"+id.String(), a.superTok, `{"confirm":"`+confirm+`","reason":"policy"}`)
			if rec.Code != http.StatusNoContent || rec.Body.Len() != 0 {
				t.Fatalf("status = %d, body = %q, want empty 204", rec.Code, rec.Body.String())
			}

			if ev, g, r, m, rp := eventChildCount(t, a.pool, id); ev+g+r+m+rp != 0 {
				t.Errorf("rows left: events=%d guests=%d rsvps=%d media=%d reports=%d", ev, g, r, m, rp)
			}
			if n := objectCount(t, blobs, id); n != 0 {
				t.Errorf("%d objects left", n)
			}
			// Gone for good: a second delete is a 404.
			wantStatusCode(t, a.do(http.MethodDelete, "/v1/admin/events/"+id.String(), a.superTok, `{"confirm":"`+confirm+`"}`), http.StatusNotFound, "not_found")

			rows := a.auditRows(t, "event", id)
			if len(rows) != 1 || rows[0].Action != "event.delete" {
				t.Fatalf("audit rows = %+v", rows)
			}
			var before map[string]any
			if err := json.Unmarshal(rows[0].Before, &before); err != nil {
				t.Fatalf("before json: %v", err)
			}
			wantReports := float64(0)
			if c.reported {
				wantReports = 1
			}
			if before["was_soft_deleted"] != c.soft || before["open_reports"] != wantReports ||
				before["rsvps"] != float64(1) || before["guests"] != float64(1) || before["media"] != float64(1) {
				t.Errorf("audit before = %s", rows[0].Before)
			}
			if c.owner && before["owner_id"] == nil {
				t.Errorf("audit before lacks owner_id: %s", rows[0].Before)
			}
			if !strings.Contains(string(rows[0].After), `"reason": "policy"`) {
				t.Errorf("audit after = %s, want the reason", rows[0].After)
			}
			for _, field := range []string{string(rows[0].Before), string(rows[0].After)} {
				if (ownerEmail != "" && strings.Contains(field, ownerEmail)) || strings.Contains(field, "Sam") || strings.Contains(field, "@") {
					t.Errorf("audit row leaks PII: %s", field)
				}
			}

			// The slug is free the moment the delete commits.
			if confirm != "delete" {
				again := createTestEvent(t, context.Background(), a.f.s.q, "birthday", "classic", nil)
				t.Cleanup(func() { _, _ = a.pool.Exec(context.Background(), "DELETE FROM events WHERE id = $1", again.ID) })
				if _, err := a.pool.Exec(context.Background(), "UPDATE events SET slug = $2 WHERE id = $1", again.ID, confirm); err != nil {
					t.Errorf("slug %q not reusable: %v", confirm, err)
				}
			}
		})
	}
}

func TestAdminDeleteEvent_Rejections(t *testing.T) {
	a, blobs := newAdminAPIWithBlobs(t)
	id, slug, _ := a.seedEventState(t, blobs, deleteEventCase{owner: true, publish: true, reported: true})
	draftID, _, _ := a.seedEventState(t, blobs, deleteEventCase{owner: false})
	missing := uuid.Must(uuid.NewV7())
	path := "/v1/admin/events/" + id.String()
	ok := `{"confirm":"` + slug + `"}`

	tests := []struct {
		name        string
		token       string
		path        string
		contentType string
		body        string
		status      int
		code        string
		touches     uuid.UUID // the event that must stay intact
	}{
		{"moderator is forbidden", a.moderatorT, path, "application/json", ok, http.StatusForbidden, "forbidden", id},
		{"malformed id", a.superTok, "/v1/admin/events/nope", "application/json", ok, http.StatusNotFound, "not_found", id},
		{"missing event", a.superTok, "/v1/admin/events/" + missing.String(), "application/json", ok, http.StatusNotFound, "not_found", id},
		{"wrong slug", a.superTok, path, "application/json", `{"confirm":"` + slug + `x"}`, http.StatusBadRequest, "confirm_mismatch", id},
		{"slug in other case", a.superTok, path, "application/json", `{"confirm":"` + strings.ToUpper(slug) + `"}`, http.StatusBadRequest, "confirm_mismatch", id},
		{"slug with spaces", a.superTok, path, "application/json", `{"confirm":" ` + slug + ` "}`, http.StatusBadRequest, "confirm_mismatch", id},
		{"literal delete for an event with a slug", a.superTok, path, "application/json", `{"confirm":"delete"}`, http.StatusBadRequest, "confirm_mismatch", id},
		{"empty confirm", a.superTok, path, "application/json", `{"confirm":""}`, http.StatusBadRequest, "confirm_mismatch", id},
		{"empty object", a.superTok, path, "application/json", `{}`, http.StatusBadRequest, "confirm_mismatch", id},
		{"slug for a draft without one", a.superTok, "/v1/admin/events/" + draftID.String(), "application/json", `{"confirm":"` + slug + `"}`, http.StatusBadRequest, "confirm_mismatch", draftID},
		{"unknown field", a.superTok, path, "application/json", `{"confirm":"` + slug + `","hard":true}`, http.StatusBadRequest, "invalid_json", id},
		{"reason too long", a.superTok, path, "application/json", `{"confirm":"` + slug + `","reason":"` + strings.Repeat("x", 501) + `"}`, http.StatusBadRequest, "validation_failed", id},
		{"not json", a.superTok, path, "text/plain", ok, http.StatusUnsupportedMediaType, "unsupported_media_type", id},
		{"no body", a.superTok, path, "", "", http.StatusUnsupportedMediaType, "unsupported_media_type", id},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			wantStatusCode(t, a.doCT(http.MethodDelete, tt.path, tt.token, tt.contentType, tt.body), tt.status, tt.code)
			if ev, g, r, m, _ := eventChildCount(t, a.pool, tt.touches); ev != 1 || g != 1 || r != 1 || m != 1 {
				t.Errorf("rows changed: events=%d guests=%d rsvps=%d media=%d", ev, g, r, m)
			}
			if n := objectCount(t, blobs, tt.touches); n != 1 {
				t.Errorf("objects = %d, want 1", n)
			}
		})
	}
	if len(a.auditRows(t, "event", id)) != 0 || len(a.auditRows(t, "event", draftID)) != 0 {
		t.Error("rejected deletes wrote audit rows")
	}
	if blobs.deletes != 0 {
		t.Errorf("storage delete called %d times by rejected requests", blobs.deletes)
	}
}

func TestAdminDeleteEvent_RateLimited(t *testing.T) {
	a, blobs := newAdminAPIWithBlobs(t)
	id, slug, _ := a.seedEventState(t, blobs, deleteEventCase{owner: true, publish: true})

	a.exhaust(t, "admin:event:delete:"+a.f.superAdminID.String(), 30, time.Hour)
	rec := a.do(http.MethodDelete, "/v1/admin/events/"+id.String(), a.superTok, `{"confirm":"`+slug+`"}`)
	wantStatusCode(t, rec, http.StatusTooManyRequests, "rate_limited")
	if ev, _, _, _, _ := eventChildCount(t, a.pool, id); ev != 1 {
		t.Error("event deleted despite the rate limit")
	}
}

func TestAdminDeleteEvent_StorageFailureKeepsEverythingAndRetrySucceeds(t *testing.T) {
	a, blobs := newAdminAPIWithBlobs(t)
	id, slug, _ := a.seedEventState(t, blobs, deleteEventCase{owner: true, publish: true, reported: true})
	path := "/v1/admin/events/" + id.String()
	body := `{"confirm":"` + slug + `"}`

	blobs.deleteErr = fmt.Errorf("delete: %w", media.ErrStorageUnavailable)
	rec := a.do(http.MethodDelete, path, a.superTok, body)
	blobs.deleteErr = nil
	wantStatusCode(t, rec, http.StatusServiceUnavailable, "storage_unavailable")
	if rec.Header().Get("Retry-After") == "" {
		t.Error("503 without Retry-After")
	}
	if ev, g, r, m, rp := eventChildCount(t, a.pool, id); ev != 1 || g != 1 || r != 1 || m != 1 || rp != 1 {
		t.Errorf("rows changed by a failed delete: events=%d guests=%d rsvps=%d media=%d reports=%d", ev, g, r, m, rp)
	}
	if n := len(a.auditRows(t, "event", id)); n != 0 {
		t.Errorf("failed delete left %d audit rows", n)
	}

	wantStatusCode(t, a.do(http.MethodDelete, path, a.superTok, body), http.StatusNoContent, "")
	if ev, _, _, _, _ := eventChildCount(t, a.pool, id); ev != 0 {
		t.Error("retry did not delete the event")
	}
	if n := objectCount(t, blobs, id); n != 0 {
		t.Errorf("%d objects left after the retry", n)
	}
	if n := len(a.auditRows(t, "event", id)); n != 1 {
		t.Errorf("audit rows = %d, want exactly 1", n)
	}
}

func TestAdminChangeEventSlug(t *testing.T) {
	a, blobs := newAdminAPIWithBlobs(t)
	ctx := context.Background()
	id, oldSlug, _ := a.seedEventState(t, blobs, deleteEventCase{owner: true, publish: true})
	// A second published event whose slug the first must not be able to take. (seedEventState slugs share the
	// UUIDv7 time prefix, so this one is set by hand.)
	otherOwner, _ := a.newTarget(t, "slug-other")
	other := createTestEvent(t, ctx, a.f.s.q, "birthday", "classic", &otherOwner)
	otherID, otherSlug := other.ID, "other-"+strings.ReplaceAll(other.ID.String(), "-", "")
	t.Cleanup(func() { _, _ = a.pool.Exec(context.Background(), "DELETE FROM events WHERE id = $1", otherID) })
	if _, err := a.pool.Exec(ctx, "UPDATE events SET status = 'published', slug = $2, published_at = now() WHERE id = $1", otherID, otherSlug); err != nil {
		t.Fatalf("publish other event: %v", err)
	}
	draftID, _, _ := a.seedEventState(t, blobs, deleteEventCase{owner: false})
	path := "/v1/admin/events/" + id.String() + "/slug"
	newSlug := "moved-" + id.String()[:8]
	t.Cleanup(func() {
		_, _ = a.pool.Exec(context.Background(), "DELETE FROM retired_slugs WHERE slug = ANY($1)", []string{oldSlug, newSlug})
	})

	slugOf := func(eventID uuid.UUID) string {
		t.Helper()
		var slug *string
		if err := a.pool.QueryRow(ctx, "SELECT slug FROM events WHERE id = $1", eventID).Scan(&slug); err != nil || slug == nil {
			t.Fatalf("read slug: %v", err)
		}
		return *slug
	}
	if err := a.pool.QueryRow(ctx, "SELECT slug FROM events WHERE id = $1", id).Scan(new(string)); err != nil {
		t.Fatalf("seed: %v", err)
	}

	rejections := []struct {
		name   string
		token  string
		path   string
		body   string
		status int
		code   string
	}{
		{"moderator is forbidden", a.moderatorT, path, `{"slug":"` + newSlug + `"}`, http.StatusForbidden, "forbidden"},
		{"malformed id", a.superTok, "/v1/admin/events/nope/slug", `{"slug":"` + newSlug + `"}`, http.StatusNotFound, "not_found"},
		{"missing event", a.superTok, "/v1/admin/events/" + uuid.Must(uuid.NewV7()).String() + "/slug", `{"slug":"` + newSlug + `"}`, http.StatusNotFound, "not_found"},
		{"slug-less draft", a.superTok, "/v1/admin/events/" + draftID.String() + "/slug", `{"slug":"` + newSlug + `"}`, http.StatusNotFound, "not_found"},
		{"invalid format", a.superTok, path, `{"slug":"No Spaces!"}`, http.StatusBadRequest, "invalid_slug"},
		{"too short", a.superTok, path, `{"slug":"ab"}`, http.StatusBadRequest, "invalid_slug"},
		{"double hyphen", a.superTok, path, `{"slug":"a--b-c"}`, http.StatusBadRequest, "invalid_slug"},
		{"taken by another event", a.superTok, path, `{"slug":"` + otherSlug + `"}`, http.StatusConflict, "slug_taken"},
		{"unknown field", a.superTok, path, `{"slug":"` + newSlug + `","x":1}`, http.StatusBadRequest, "invalid_json"},
	}
	for _, tt := range rejections {
		t.Run(tt.name, func(t *testing.T) {
			wantStatusCode(t, a.do(http.MethodPatch, tt.path, tt.token, tt.body), tt.status, tt.code)
			if got := slugOf(id); got != oldSlug {
				t.Errorf("slug changed to %q by a rejected request", got)
			}
		})
	}
	if got := slugOf(otherID); got != otherSlug {
		t.Errorf("another event's slug changed to %q", got)
	}
	if len(a.auditRows(t, "event", id)) != 0 {
		t.Error("rejected requests wrote audit rows")
	}

	t.Run("blocked term", func(t *testing.T) {
		if _, err := a.pool.Exec(ctx, "INSERT INTO slug_blocklist (term, reason) VALUES ('zzblocked', 'reserved') ON CONFLICT DO NOTHING"); err != nil {
			t.Fatalf("seed blocklist: %v", err)
		}
		t.Cleanup(func() {
			_, _ = a.pool.Exec(context.Background(), "DELETE FROM slug_blocklist WHERE term = 'zzblocked'")
		})
		wantStatusCode(t, a.do(http.MethodPatch, path, a.superTok, `{"slug":"our-zzblocked-day"}`), http.StatusConflict, "slug_blocked")
	})

	t.Run("a link retired in the last 30 days stays unavailable", func(t *testing.T) {
		held := "held-" + id.String()[:8]
		if _, err := a.pool.Exec(ctx, "INSERT INTO retired_slugs (slug) VALUES ($1)", held); err != nil {
			t.Fatalf("seed retired slug: %v", err)
		}
		t.Cleanup(func() { _, _ = a.pool.Exec(context.Background(), "DELETE FROM retired_slugs WHERE slug = $1", held) })
		wantStatusCode(t, a.do(http.MethodPatch, path, a.superTok, `{"slug":"`+held+`"}`), http.StatusConflict, "slug_taken")
	})

	t.Run("changes a published event's link, holds the old one and audits it", func(t *testing.T) {
		rec := a.do(http.MethodPatch, path, a.superTok, `{"slug":"`+strings.ToUpper(newSlug)+`"}`)
		if rec.Code != http.StatusOK {
			t.Fatalf("status = %d, body = %s", rec.Code, rec.Body.String())
		}
		if got := slugOf(id); got != newSlug {
			t.Fatalf("slug = %q, want %q (normalised)", got, newSlug)
		}
		var held int
		if err := a.pool.QueryRow(ctx, "SELECT count(*) FROM retired_slugs WHERE slug = $1 AND owner_id IS NULL", oldSlug).Scan(&held); err != nil || held != 1 {
			t.Errorf("old slug held = %d (err %v), want 1", held, err)
		}
		rows := a.auditRows(t, "event", id)
		if len(rows) != 1 || rows[0].Action != "event.slug" {
			t.Fatalf("audit rows = %+v", rows)
		}
		if !strings.Contains(string(rows[0].Before), oldSlug) || !strings.Contains(string(rows[0].After), newSlug) {
			t.Errorf("audit before/after = %s / %s", rows[0].Before, rows[0].After)
		}
		// The old link is now held back from everyone, including through the host's own check.
		taken, err := a.f.s.q.SlugTaken(ctx, store.SlugTakenParams{Slug: oldSlug})
		if err != nil || !taken {
			t.Errorf("old slug taken = %v (err %v), want true", taken, err)
		}
	})

	t.Run("the immutability trigger still blocks any other slug change", func(t *testing.T) {
		_, err := a.pool.Exec(ctx, "UPDATE events SET slug = $2 WHERE id = $1", id, "sneaky-"+id.String()[:8])
		if err == nil {
			t.Fatal("a plain UPDATE changed a published slug")
		}
		if got := slugOf(id); got != newSlug {
			t.Errorf("slug = %q, want %q", got, newSlug)
		}
	})
}
