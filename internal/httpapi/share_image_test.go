package httpapi

import (
	"context"
	"crypto/sha256"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgconn"

	"github.com/ansonarose/seeyouthere-backend/internal/content"
	"github.com/ansonarose/seeyouthere-backend/internal/store"
)

func strPtr(s string) *string { return &s }

func TestValidateShareImage(t *testing.T) {
	id := uuid.Must(uuid.NewV7())
	cases := []struct {
		name     string
		req      shareImageReq
		wantPath string // "" = valid
		wantCode string
		wantID   *uuid.UUID
	}{
		{"hero", shareImageReq{Source: "hero"}, "", "", nil},
		{"site", shareImageReq{Source: "site"}, "", "", nil},
		{"custom", shareImageReq{Source: "custom", MediaID: strPtr(id.String())}, "", "", &id},
		{"empty source", shareImageReq{Source: ""}, "share_image.source", "invalid", nil},
		{"unknown source", shareImageReq{Source: "banner"}, "share_image.source", "invalid", nil},
		{"source is case-sensitive", shareImageReq{Source: "Hero"}, "share_image.source", "invalid", nil},
		{"custom without media id", shareImageReq{Source: "custom"}, "share_image.media_id", "required", nil},
		{"custom with empty media id", shareImageReq{Source: "custom", MediaID: strPtr("")}, "share_image.media_id", "required", nil},
		{"custom with malformed media id", shareImageReq{Source: "custom", MediaID: strPtr("not-a-uuid")}, "share_image.media_id", "invalid_media_id", nil},
		{"hero with media id", shareImageReq{Source: "hero", MediaID: strPtr(id.String())}, "share_image.media_id", "not_allowed", nil},
		{"site with media id", shareImageReq{Source: "site", MediaID: strPtr(id.String())}, "share_image.media_id", "not_allowed", nil},
		{"site with empty media id", shareImageReq{Source: "site", MediaID: strPtr("")}, "share_image.media_id", "not_allowed", nil},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got, issue := validateShareImage(tc.req)
			if tc.wantPath == "" {
				if issue != nil {
					t.Fatalf("issue = %+v, want none", *issue)
				}
			} else if issue == nil || issue.Path != tc.wantPath || issue.Code != tc.wantCode {
				t.Fatalf("issue = %+v, want %s/%s", issue, tc.wantPath, tc.wantCode)
			}
			if (got == nil) != (tc.wantID == nil) || (got != nil && *got != *tc.wantID) {
				t.Fatalf("media id = %v, want %v", got, tc.wantID)
			}
		})
	}
}

func TestResolveShareImage(t *testing.T) {
	hero := uuid.Must(uuid.NewV7())
	custom := uuid.Must(uuid.NewV7())
	missing := uuid.Must(uuid.NewV7())
	media := map[string]mediaItemResp{
		hero.String():   {Src: "/media/e/hero", Width: 1600, Height: 900},
		custom.String(): {Src: "/media/e/custom", Width: 1200, Height: 630},
	}
	cases := []struct {
		name    string
		source  string
		share   *uuid.UUID
		heroID  *uuid.UUID
		wantSrc string // "" = nil
	}{
		{"hero uses the hero image", "hero", nil, &hero, "/media/e/hero"},
		{"hero without a hero image", "hero", nil, nil, ""},
		{"hero image not approved", "hero", nil, &missing, ""},
		{"site ignores the hero image", "site", nil, &hero, ""},
		{"custom uses its image", "custom", &custom, &hero, "/media/e/custom"},
		{"custom image deleted", "custom", nil, &hero, ""},
		{"custom image not approved", "custom", &missing, &hero, ""},
		{"unknown source", "other", &custom, &hero, ""},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := resolveShareImage(tc.source, tc.share, tc.heroID, media)
			if tc.wantSrc == "" {
				if got != nil {
					t.Fatalf("got %+v, want nil", *got)
				}
				return
			}
			if got == nil || got.Src != tc.wantSrc {
				t.Fatalf("got %+v, want src %s", got, tc.wantSrc)
			}
		})
	}
}

// createHostMediaRow inserts an approved host media row (no files) for tests
// that only need the database side.
func createHostMediaRow(t *testing.T, ctx context.Context, q *store.Queries, eventID, userID uuid.UUID) uuid.UUID {
	t.Helper()
	id := uuid.Must(uuid.NewV7())
	if _, err := q.CreateHostMedia(ctx, store.CreateHostMediaParams{
		ID: id, UserID: userID, EventID: eventID, StorageKey: eventID.String() + "/" + id.String(),
		SizeBytes: 100, Width: 1200, Height: 630,
	}); err != nil {
		t.Fatalf("create host media: %v", err)
	}
	return id
}

func TestEventShareImageConstraints(t *testing.T) {
	pool := dbTestPool(t)
	ctx := context.Background()
	tx, err := pool.Begin(ctx)
	if err != nil {
		t.Fatalf("begin: %v", err)
	}
	defer func() { _ = tx.Rollback(ctx) }()
	q := store.New(tx)

	owner := uuid.Must(uuid.NewV7())
	if _, err := q.UpsertUserByEmail(ctx, store.UpsertUserByEmailParams{ID: owner, Email: "share-" + owner.String() + "@example.invalid"}); err != nil {
		t.Fatalf("create user: %v", err)
	}
	eventA := createTestEvent(t, ctx, q, "birthday", "classic", &owner)
	eventB := createTestEvent(t, ctx, q, "birthday", "classic", &owner)
	mediaA := createHostMediaRow(t, ctx, q, eventA.ID, owner)
	mediaB := createHostMediaRow(t, ctx, q, eventB.ID, owner)

	if eventA.ShareImage != "hero" || eventA.ShareMediaID != nil {
		t.Fatalf("new event share image = %q/%v, want hero/nil", eventA.ShareImage, eventA.ShareMediaID)
	}

	setShare := func(eventID uuid.UUID, source string, mediaID *uuid.UUID) error {
		sp, err := tx.Begin(ctx) // savepoint, so a failure doesn't abort the outer tx
		if err != nil {
			t.Fatalf("savepoint: %v", err)
		}
		if _, err := sp.Exec(ctx, "UPDATE events SET share_image = $1, share_media_id = $2 WHERE id = $3", source, mediaID, eventID); err != nil {
			_ = sp.Rollback(ctx)
			return err
		}
		return sp.Commit(ctx)
	}

	cases := []struct {
		name           string
		source         string
		mediaID        *uuid.UUID
		wantCode       string // "" = ok
		wantConstraint string
	}{
		{"custom with own media", "custom", &mediaA, "", ""},
		{"custom without media (deleted image)", "custom", nil, "", ""},
		{"hero", "hero", nil, "", ""},
		{"custom with another event's media", "custom", &mediaB, "23503", "events_share_media_fkey"},
		{"custom with unknown media", "custom", ptrUUID(uuid.Must(uuid.NewV7())), "23503", "events_share_media_fkey"},
		{"site with media", "site", &mediaA, "23514", "events_share_media_check"},
		{"hero with media", "hero", &mediaA, "23514", "events_share_media_check"},
		{"unknown source", "banner", nil, "23514", "events_share_image_check"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			err := setShare(eventA.ID, tc.source, tc.mediaID)
			if tc.wantCode == "" {
				if err != nil {
					t.Fatalf("err = %v, want nil", err)
				}
				return
			}
			var pgErr *pgconn.PgError
			if !errors.As(err, &pgErr) || pgErr.Code != tc.wantCode || pgErr.ConstraintName != tc.wantConstraint {
				t.Fatalf("err = %v, want %s on %s", err, tc.wantCode, tc.wantConstraint)
			}
		})
	}

	t.Run("deleting the media clears share_media_id only", func(t *testing.T) {
		if err := setShare(eventA.ID, "custom", &mediaA); err != nil {
			t.Fatalf("set share: %v", err)
		}
		if _, err := tx.Exec(ctx, "DELETE FROM media WHERE id = $1", mediaA); err != nil {
			t.Fatalf("delete media: %v", err)
		}
		var source string
		var mediaID *uuid.UUID
		if err := tx.QueryRow(ctx, "SELECT share_image, share_media_id FROM events WHERE id = $1", eventA.ID).Scan(&source, &mediaID); err != nil {
			t.Fatalf("read event: %v", err)
		}
		if source != "custom" || mediaID != nil {
			t.Fatalf("share = %q/%v, want custom/nil", source, mediaID)
		}
	})

	t.Run("purging an event with a share image", func(t *testing.T) {
		mediaB2 := createHostMediaRow(t, ctx, q, eventB.ID, owner)
		if err := setShare(eventB.ID, "custom", &mediaB2); err != nil {
			t.Fatalf("set share: %v", err)
		}
		if _, err := tx.Exec(ctx, "UPDATE events SET deleted_at = now() WHERE id = $1", eventB.ID); err != nil {
			t.Fatalf("soft delete: %v", err)
		}
		n, err := q.PurgeEvents(ctx, []uuid.UUID{eventB.ID})
		if err != nil || n != 1 {
			t.Fatalf("purge = %d, %v; want 1, nil", n, err)
		}
		var left int
		if err := tx.QueryRow(ctx, "SELECT count(*) FROM media WHERE event_id = $1", eventB.ID).Scan(&left); err != nil {
			t.Fatalf("count media: %v", err)
		}
		if left != 0 {
			t.Fatalf("%d media rows left after purge, want 0", left)
		}
	})
}

func ptrUUID(id uuid.UUID) *uuid.UUID { return &id }

// patchAs sends PATCH /v1/events/{id} as userID (uuid.Nil = anonymous) with
// an optional draft cookie.
func patchAs(s *Server, eventID, userID uuid.UUID, draftToken, body string) *httptest.ResponseRecorder {
	req := requestWithIP(http.MethodPatch, "/", nextTestIP(), []byte(body))
	if userID != uuid.Nil {
		req = req.WithContext(context.WithValue(req.Context(), ctxUserID, userID))
	}
	if draftToken != "" {
		req.AddCookie(&http.Cookie{Name: draftCookieName, Value: draftToken})
	}
	req.Header.Set("Content-Type", "application/json")
	req = withRouteID(req, eventID)
	rec := httptest.NewRecorder()
	s.handlePatchEvent(rec, req)
	return rec
}

type patchedEvent struct {
	Event struct {
		Version    int32                    `json:"version"`
		ShareImage shareImageResp           `json:"share_image"`
		Media      map[string]mediaItemResp `json:"media"`
	} `json:"event"`
}

func decodePatched(t *testing.T, rec *httptest.ResponseRecorder) patchedEvent {
	t.Helper()
	var out patchedEvent
	if err := json.Unmarshal(rec.Body.Bytes(), &out); err != nil {
		t.Fatalf("decode response: %v; body = %s", err, rec.Body.String())
	}
	return out
}

func eventVersion(t *testing.T, ctx context.Context, s *Server, eventID uuid.UUID) int32 {
	t.Helper()
	var v int32
	if err := s.pool.QueryRow(ctx, "SELECT version FROM events WHERE id = $1", eventID).Scan(&v); err != nil {
		t.Fatalf("read version: %v", err)
	}
	return v
}

func mediaDetached(t *testing.T, ctx context.Context, s *Server, mediaID uuid.UUID) bool {
	t.Helper()
	var detached bool
	if err := s.pool.QueryRow(ctx, "SELECT detached_at IS NOT NULL FROM media WHERE id = $1", mediaID).Scan(&detached); err != nil {
		t.Fatalf("read media %s: %v", mediaID, err)
	}
	return detached
}

func shareBody(version int32, share string) string {
	return `{"version":` + strconv.Itoa(int(version)) + `,"share_image":` + share + `}`
}

func TestHandlePatchEvent_ShareImageValidation(t *testing.T) {
	pool := dbTestPool(t)
	rdb := dbTestRedis(t)
	f := newEventTestFixture(t, pool, rdb)
	ctx := context.Background()

	other := createTestEvent(t, ctx, f.s.q, "birthday", "classic", &f.ownerID)
	t.Cleanup(func() {
		if _, err := pool.Exec(context.Background(), "DELETE FROM events WHERE id = $1", other.ID); err != nil {
			t.Logf("cleanup event %s: %v", other.ID, err)
		}
	})
	otherMedia := createHostMediaRow(t, ctx, f.s.q, other.ID, f.ownerID)
	rejected := createHostMediaRow(t, ctx, f.s.q, f.eventID, f.ownerID)
	if _, err := f.s.q.RejectMediaAdmin(ctx, rejected); err != nil {
		t.Fatalf("reject media: %v", err)
	}

	cases := []struct {
		name     string
		share    string
		wantCode string // error code at top level
		wantPath string
		wantSub  string // issue code
	}{
		{"unknown source", `{"source":"banner"}`, "validation_failed", "share_image.source", "invalid"},
		{"missing source", `{}`, "validation_failed", "share_image.source", "invalid"},
		{"custom without media id", `{"source":"custom"}`, "validation_failed", "share_image.media_id", "required"},
		{"custom with null media id", `{"source":"custom","media_id":null}`, "validation_failed", "share_image.media_id", "required"},
		{"hero with media id", `{"source":"hero","media_id":"` + rejected.String() + `"}`, "validation_failed", "share_image.media_id", "not_allowed"},
		{"site with media id", `{"source":"site","media_id":"` + rejected.String() + `"}`, "validation_failed", "share_image.media_id", "not_allowed"},
		{"malformed media id", `{"source":"custom","media_id":"nope"}`, "validation_failed", "share_image.media_id", "invalid_media_id"},
		{"unknown media id", `{"source":"custom","media_id":"` + uuid.Must(uuid.NewV7()).String() + `"}`, "validation_failed", "share_image.media_id", "invalid_media_id"},
		{"another event's media", `{"source":"custom","media_id":"` + otherMedia.String() + `"}`, "validation_failed", "share_image.media_id", "invalid_media_id"},
		{"rejected media", `{"source":"custom","media_id":"` + rejected.String() + `"}`, "validation_failed", "share_image.media_id", "invalid_media_id"},
		{"unknown field", `{"source":"hero","extra":1}`, "invalid_json", "", ""},
		{"wrong type", `"hero"`, "invalid_json", "", ""},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			rec := patchAs(f.s, f.eventID, f.ownerID, "", shareBody(1, tc.share))
			if rec.Code != http.StatusBadRequest {
				t.Fatalf("status = %d, want 400; body = %s", rec.Code, rec.Body.String())
			}
			body := rec.Body.String()
			if !strings.Contains(body, `"code":"`+tc.wantCode+`"`) {
				t.Fatalf("body = %s, want code %s", body, tc.wantCode)
			}
			if tc.wantPath != "" && !containsAll(body, `"path":"`+tc.wantPath+`"`, `"code":"`+tc.wantSub+`"`) {
				t.Fatalf("body = %s, want issue %s/%s", body, tc.wantPath, tc.wantSub)
			}
		})
	}
	if v := eventVersion(t, ctx, f.s, f.eventID); v != 1 {
		t.Fatalf("version = %d after rejected patches, want 1", v)
	}

	t.Run("viewer is forbidden", func(t *testing.T) {
		rec := patchAs(f.s, f.eventID, f.viewerID, "", shareBody(1, `{"source":"site"}`))
		if rec.Code != http.StatusForbidden {
			t.Fatalf("status = %d, want 403; body = %s", rec.Code, rec.Body.String())
		}
	})
	t.Run("stranger gets 404", func(t *testing.T) {
		rec := patchAs(f.s, f.eventID, f.strangerID, "", shareBody(1, `{"source":"site"}`))
		if rec.Code != http.StatusNotFound {
			t.Fatalf("status = %d, want 404; body = %s", rec.Code, rec.Body.String())
		}
	})
	t.Run("stale share-only patch conflicts", func(t *testing.T) {
		rec := patchAs(f.s, f.eventID, f.ownerID, "", shareBody(7, `{"source":"site"}`))
		if rec.Code != http.StatusConflict || !containsAll(rec.Body.String(), `"version_conflict"`, `"current_version":1`) {
			t.Fatalf("status = %d, body = %s; want 409 version_conflict current_version 1", rec.Code, rec.Body.String())
		}
	})
}

func TestHandlePatchEvent_ShareImageAttachDetach(t *testing.T) {
	pool := dbTestPool(t)
	rdb := dbTestRedis(t)
	f := newEventTestFixture(t, pool, rdb)
	ctx := context.Background()

	heroMedia := createHostMediaRow(t, ctx, f.s.q, f.eventID, f.ownerID)
	shareMedia := createHostMediaRow(t, ctx, f.s.q, f.eventID, f.ownerID)
	spare := createHostMediaRow(t, ctx, f.s.q, f.eventID, f.ownerID)

	heroContent := `[{"id":"hero1","type":"hero","title":"Sam","subtitle":"","image":{"media_id":"` + heroMedia.String() + `","alt":""}}]`

	// Content referencing only the hero image: the other two are detached.
	rec := patchAs(f.s, f.eventID, f.ownerID, "", `{"version":1,"content":`+heroContent+`}`)
	if rec.Code != http.StatusOK {
		t.Fatalf("content save status = %d; body = %s", rec.Code, rec.Body.String())
	}
	if got := decodePatched(t, rec).Event.ShareImage; got.Source != "hero" || got.MediaID != nil {
		t.Fatalf("share_image = %+v, want hero/nil", got)
	}
	if mediaDetached(t, ctx, f.s, heroMedia) || !mediaDetached(t, ctx, f.s, shareMedia) || !mediaDetached(t, ctx, f.s, spare) {
		t.Fatal("after content save: want hero attached, share and spare detached")
	}

	// Editor picks a custom image: it is re-attached, the response carries it.
	rec = patchAs(f.s, f.eventID, f.editorID, "", shareBody(2, `{"source":"custom","media_id":"`+shareMedia.String()+`"}`))
	if rec.Code != http.StatusOK {
		t.Fatalf("custom status = %d; body = %s", rec.Code, rec.Body.String())
	}
	got := decodePatched(t, rec)
	if got.Event.Version != 3 || got.Event.ShareImage.Source != "custom" || got.Event.ShareImage.MediaID == nil || *got.Event.ShareImage.MediaID != shareMedia {
		t.Fatalf("event = %+v, want version 3 custom %s", got.Event, shareMedia)
	}
	if _, ok := got.Event.Media[shareMedia.String()]; !ok {
		t.Fatalf("media map %v lacks the share image", got.Event.Media)
	}
	if mediaDetached(t, ctx, f.s, shareMedia) || mediaDetached(t, ctx, f.s, heroMedia) || !mediaDetached(t, ctx, f.s, spare) {
		t.Fatal("after custom: want share and hero attached, spare detached")
	}

	// The share image can't be deleted while in use.
	t.Run("media delete is blocked", func(t *testing.T) {
		f.s.media = newTestMediaStore(t)
		req := requestAs(http.MethodDelete, "/", f.ownerID)
		req = withRouteParam(req, "id", f.eventID.String())
		req = withRouteParam(req, "mediaID", shareMedia.String())
		rec := httptest.NewRecorder()
		f.s.handleDeleteEventMedia(rec, req)
		if rec.Code != http.StatusConflict || !containsAll(rec.Body.String(), `"media_in_use"`, "link preview") {
			t.Fatalf("status = %d, body = %s; want 409 media_in_use", rec.Code, rec.Body.String())
		}
	})

	// A content save that doesn't reference the share image keeps it attached.
	plainContent := `[{"id":"hero1","type":"hero","title":"Sam","subtitle":"","image":null}]`
	rec = patchAs(f.s, f.eventID, f.ownerID, "", `{"version":3,"content":`+plainContent+`}`)
	if rec.Code != http.StatusOK {
		t.Fatalf("content save status = %d; body = %s", rec.Code, rec.Body.String())
	}
	if got := decodePatched(t, rec).Event.ShareImage; got.Source != "custom" || got.MediaID == nil || *got.MediaID != shareMedia {
		t.Fatalf("share_image = %+v after content save, want unchanged custom", got)
	}
	if mediaDetached(t, ctx, f.s, shareMedia) || !mediaDetached(t, ctx, f.s, heroMedia) {
		t.Fatal("after content save: want share attached, old hero detached")
	}

	// Content and share image in one request against a stale version: 409, no change.
	rec = patchAs(f.s, f.eventID, f.ownerID, "", `{"version":3,"content":`+heroContent+`,"share_image":{"source":"site"}}`)
	if rec.Code != http.StatusConflict || !containsAll(rec.Body.String(), `"version_conflict"`, `"current_version":4`) {
		t.Fatalf("status = %d, body = %s; want 409 current_version 4", rec.Code, rec.Body.String())
	}

	// Back to the site image: the custom image is detached again.
	rec = patchAs(f.s, f.eventID, f.ownerID, "", shareBody(4, `{"source":"site","media_id":null}`))
	if rec.Code != http.StatusOK {
		t.Fatalf("site status = %d; body = %s", rec.Code, rec.Body.String())
	}
	if got := decodePatched(t, rec).Event.ShareImage; got.Source != "site" || got.MediaID != nil {
		t.Fatalf("share_image = %+v, want site/nil", got)
	}
	if !mediaDetached(t, ctx, f.s, shareMedia) {
		t.Fatal("after site: want the former share image detached")
	}

	// A patch without share_image leaves it alone.
	rec = patchAs(f.s, f.eventID, f.ownerID, "", `{"version":5}`)
	if rec.Code != http.StatusOK {
		t.Fatalf("empty patch status = %d; body = %s", rec.Code, rec.Body.String())
	}
	if got := decodePatched(t, rec).Event.ShareImage; got.Source != "site" {
		t.Fatalf("share_image = %+v after unrelated patch, want site", got)
	}
}

func TestHandlePatchEvent_ShareImageAnonDraft(t *testing.T) {
	pool := dbTestPool(t)
	rdb := dbTestRedis(t)
	f := newEventTestFixture(t, pool, rdb)
	ctx := context.Background()

	draft := createTestEvent(t, ctx, f.s.q, "birthday", "classic", nil)
	t.Cleanup(func() {
		if _, err := pool.Exec(context.Background(), "DELETE FROM events WHERE id = $1", draft.ID); err != nil {
			t.Logf("cleanup draft %s: %v", draft.ID, err)
		}
	})
	draftToken, err := randomToken()
	if err != nil {
		t.Fatalf("randomToken: %v", err)
	}
	sum := sha256.Sum256([]byte(draftToken))
	if err := f.s.q.CreateAnonDraft(ctx, store.CreateAnonDraftParams{
		ID: uuid.Must(uuid.NewV7()), CookieHash: sum[:], EventID: draft.ID, ExpiresAt: time.Now().Add(anonDraftTTL),
	}); err != nil {
		t.Fatalf("create anon draft: %v", err)
	}
	mediaID := uuid.Must(uuid.NewV7())
	if _, err := f.s.q.CreateAnonDraftMedia(ctx, store.CreateAnonDraftMediaParams{
		ID: mediaID, StorageKey: draft.ID.String() + "/" + mediaID.String(), SizeBytes: 100, Width: 1200, Height: 630,
		EventID: draft.ID, CookieHash: sum[:],
	}); err != nil {
		t.Fatalf("create draft media: %v", err)
	}
	otherToken, err := randomToken()
	if err != nil {
		t.Fatalf("randomToken: %v", err)
	}
	body := shareBody(1, `{"source":"custom","media_id":"`+mediaID.String()+`"}`)

	cases := []struct {
		name       string
		token      string
		wantStatus int
	}{
		{"no cookie", "", http.StatusNotFound},
		{"wrong cookie", otherToken, http.StatusNotFound},
		{"own cookie", draftToken, http.StatusOK},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			rec := patchAs(f.s, draft.ID, uuid.Nil, tc.token, body)
			if rec.Code != tc.wantStatus {
				t.Fatalf("status = %d, want %d; body = %s", rec.Code, tc.wantStatus, rec.Body.String())
			}
			if tc.wantStatus == http.StatusOK {
				if got := decodePatched(t, rec).Event.ShareImage; got.Source != "custom" || got.MediaID == nil || *got.MediaID != mediaID {
					t.Fatalf("share_image = %+v, want custom %s", got, mediaID)
				}
			}
		})
	}
}

func TestHandleGetPublicEvent_ShareImage(t *testing.T) {
	pool := dbTestPool(t)
	rdb := dbTestRedis(t)
	f := newPublicEventFixture(t, pool, rdb, publicEventOpts{})
	ctx := context.Background()

	heroMedia := createHostMediaRow(t, ctx, f.s.q, f.eventID, f.ownerID)
	customMedia := createHostMediaRow(t, ctx, f.s.q, f.eventID, f.ownerID)
	rejectedMedia := createHostMediaRow(t, ctx, f.s.q, f.eventID, f.ownerID)
	if _, err := f.s.q.RejectMediaAdmin(ctx, rejectedMedia); err != nil {
		t.Fatalf("reject media: %v", err)
	}

	occRow, err := f.s.q.GetOccasion(ctx, "birthday")
	if err != nil {
		t.Fatalf("get occasion: %v", err)
	}
	occ, err := content.ParseOccasion(occRow)
	if err != nil {
		t.Fatalf("parse occasion: %v", err)
	}
	contentWithHero := func(withImage bool) []byte {
		image := "null"
		if withImage {
			image = `{"media_id":"` + heroMedia.String() + `","alt":""}`
		}
		saved, err := content.ValidateContent([]byte(`[{"id":"hero1","type":"hero","title":"Sam's Party","subtitle":"","image":`+image+`}]`), occ)
		if err != nil {
			t.Fatalf("validate content: %v", err)
		}
		return saved.JSON
	}

	cases := []struct {
		name      string
		heroImage bool
		source    string
		mediaID   *uuid.UUID
		wantID    *uuid.UUID // nil = share_image null
	}{
		{"hero with an image", true, "hero", nil, &heroMedia},
		{"hero without an image", false, "hero", nil, nil},
		{"site", true, "site", nil, nil},
		{"custom", true, "custom", &customMedia, &customMedia},
		{"custom without a hero image", false, "custom", &customMedia, &customMedia},
		{"custom rejected", true, "custom", &rejectedMedia, nil},
		{"custom deleted", true, "custom", nil, nil},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if _, err := pool.Exec(ctx, "UPDATE events SET content = $1, share_image = $2, share_media_id = $3 WHERE id = $4",
				contentWithHero(tc.heroImage), tc.source, tc.mediaID, f.eventID); err != nil {
				t.Fatalf("set event: %v", err)
			}
			req := requestWithIP(http.MethodGet, "/", nextTestIP(), nil)
			req = withSlugParam(req, f.slug)
			rec := httptest.NewRecorder()
			f.s.handleGetPublicEvent(rec, req)
			if rec.Code != http.StatusOK {
				t.Fatalf("status = %d, want 200; body = %s", rec.Code, rec.Body.String())
			}
			var out struct {
				Event struct {
					ShareImage *mediaItemResp `json:"share_image"`
				} `json:"event"`
			}
			if err := json.Unmarshal(rec.Body.Bytes(), &out); err != nil {
				t.Fatalf("decode: %v", err)
			}
			if !strings.Contains(rec.Body.String(), `"share_image":`) {
				t.Fatalf("body lacks share_image: %s", rec.Body.String())
			}
			if tc.wantID == nil {
				if out.Event.ShareImage != nil {
					t.Fatalf("share_image = %+v, want null", *out.Event.ShareImage)
				}
				return
			}
			want := "/media/" + f.eventID.String() + "/" + tc.wantID.String()
			if out.Event.ShareImage == nil || out.Event.ShareImage.Src != want ||
				out.Event.ShareImage.Width != 1200 || out.Event.ShareImage.Height != 630 {
				t.Fatalf("share_image = %+v, want src %s 1200x630", out.Event.ShareImage, want)
			}
		})
	}
}
