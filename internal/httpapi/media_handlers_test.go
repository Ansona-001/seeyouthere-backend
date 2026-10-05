package httpapi

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/go-chi/chi/v5"
	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/redis/go-redis/v9"

	"github.com/ansonarose/seeyouthere-backend/internal/content"
	"github.com/ansonarose/seeyouthere-backend/internal/media"
	"github.com/ansonarose/seeyouthere-backend/internal/store"
	"github.com/ansonarose/seeyouthere-backend/internal/token"
)

// newMediaTestFixture builds on newEventTestFixture (owner/editor/viewer/
// stranger + one draft event) and wires the media-specific dependencies
// (Store, Processor, token.Keys) that only media_handlers.go needs.
func newMediaTestFixture(t *testing.T, pool *pgxpool.Pool, rdb *redis.Client) eventTestFixture {
	t.Helper()
	f := newEventTestFixture(t, pool, rdb)
	mediaStore, err := media.NewStore(t.TempDir())
	if err != nil {
		t.Fatalf("media.NewStore: %v", err)
	}
	tokens, err := token.NewKeys(testAuthSecret())
	if err != nil {
		t.Fatalf("token.NewKeys: %v", err)
	}
	f.s.media = mediaStore
	f.s.images = media.NewProcessor(1)
	f.s.tokens = tokens
	return f
}

// uploadRequest builds a multipart-free raw-body upload request (the API
// takes the image bytes directly as the body, per §4.8) with a route id
// param and, for an authenticated caller, a user id in context. Content-Type
// defaults to image/jpeg, matching every real client (browser fetch/XHR
// always sets one from the File's type): tests exercising the declared-type
// check set their own via req.Header.Set after calling this.
func uploadRequest(userID uuid.UUID, eventID uuid.UUID, body []byte, ip string) *http.Request {
	req := requestWithIP(http.MethodPost, "/", ip, body)
	req.Header.Set("Content-Type", "image/jpeg")
	if userID != uuid.Nil {
		req = req.WithContext(context.WithValue(req.Context(), ctxUserID, userID))
	}
	rctx := chi.NewRouteContext()
	rctx.URLParams.Add("id", eventID.String())
	return req.WithContext(context.WithValue(req.Context(), chi.RouteCtxKey, rctx))
}

func TestHandleUploadEventMedia_AuthzMatrix(t *testing.T) {
	pool := dbTestPool(t)
	rdb := dbTestRedis(t)
	f := newMediaTestFixture(t, pool, rdb)
	img := testJPEG(t)

	cases := []struct {
		name       string
		userID     uuid.UUID
		wantStatus int
	}{
		{"owner", f.ownerID, http.StatusCreated},
		{"editor", f.editorID, http.StatusCreated},
		{"viewer", f.viewerID, http.StatusForbidden},
		{"stranger", f.strangerID, http.StatusNotFound},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			req := uploadRequest(c.userID, f.eventID, img, nextTestIP())
			rec := httptest.NewRecorder()
			f.s.handleUploadEventMedia(rec, req)
			if rec.Code != c.wantStatus {
				t.Fatalf("status = %d, want %d; body = %s", rec.Code, c.wantStatus, rec.Body.String())
			}
		})
	}

	t.Run("anon without a draft cookie is 404", func(t *testing.T) {
		req := uploadRequest(uuid.Nil, f.eventID, img, nextTestIP())
		rec := httptest.NewRecorder()
		f.s.handleUploadEventMedia(rec, req)
		if rec.Code != http.StatusNotFound {
			t.Fatalf("status = %d, want 404; body = %s", rec.Code, rec.Body.String())
		}
	})

	t.Run("anon with a valid draft cookie for its own draft succeeds", func(t *testing.T) {
		ctx := context.Background()
		q := f.s.q
		draft := createTestEvent(t, ctx, q, "birthday", "classic", nil)
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
		if err := q.CreateAnonDraft(ctx, store.CreateAnonDraftParams{
			ID: uuid.Must(uuid.NewV7()), CookieHash: sum[:], EventID: draft.ID, ExpiresAt: time.Now().Add(anonDraftTTL),
		}); err != nil {
			t.Fatalf("create anon draft: %v", err)
		}

		req := uploadRequest(uuid.Nil, draft.ID, img, nextTestIP())
		req.AddCookie(&http.Cookie{Name: draftCookieName, Value: draftToken})
		rec := httptest.NewRecorder()
		f.s.handleUploadEventMedia(rec, req)
		if rec.Code != http.StatusCreated {
			t.Fatalf("status = %d, want 201; body = %s", rec.Code, rec.Body.String())
		}
		if !containsAll(rec.Body.String(), `"uploaded_by":"host"`) {
			t.Errorf("body = %s, want uploaded_by host", rec.Body.String())
		}
	})
}

func TestHandleUploadEventMedia_RejectsUnsupportedType(t *testing.T) {
	pool := dbTestPool(t)
	rdb := dbTestRedis(t)
	f := newMediaTestFixture(t, pool, rdb)

	req := uploadRequest(f.ownerID, f.eventID, []byte("this is definitely not an image, just plain text padding to survive the 512-byte sniff window without matching any known signature"), nextTestIP())
	rec := httptest.NewRecorder()
	f.s.handleUploadEventMedia(rec, req)
	if rec.Code != http.StatusUnsupportedMediaType || !containsAll(rec.Body.String(), `"unsupported_image"`) {
		t.Fatalf("status = %d, body = %s, want 415 unsupported_image", rec.Code, rec.Body.String())
	}
}

func TestHandleUploadEventMedia_TooLargeBody(t *testing.T) {
	pool := dbTestPool(t)
	rdb := dbTestRedis(t)
	f := newMediaTestFixture(t, pool, rdb)

	oversized := bytes.Repeat([]byte{0}, maxHostUploadBytes+1)
	req := uploadRequest(f.ownerID, f.eventID, oversized, nextTestIP())
	rec := httptest.NewRecorder()
	f.s.handleUploadEventMedia(rec, req)
	if rec.Code != http.StatusRequestEntityTooLarge && rec.Code != http.StatusUnsupportedMediaType {
		t.Fatalf("status = %d, want 413 or 415 for an oversized/unsniffable body; body = %s", rec.Code, rec.Body.String())
	}
}

// TestHandleUploadEventMedia_QuotaEnforced pre-fills an event's host media
// count to the cap via direct inserts (no image pipeline needed for those:
// only the count matters) and checks that one more real upload is rejected
// with 409 media_quota rather than ever touching disk.
func TestHandleUploadEventMedia_QuotaEnforced(t *testing.T) {
	pool := dbTestPool(t)
	rdb := dbTestRedis(t)
	f := newMediaTestFixture(t, pool, rdb)
	ctx := context.Background()

	for i := 0; i < maxEventMediaFiles; i++ {
		id := uuid.Must(uuid.NewV7())
		if _, err := f.s.q.CreateHostMedia(ctx, store.CreateHostMediaParams{
			ID: id, UserID: f.ownerID, EventID: f.eventID, StorageKey: f.eventID.String() + "/" + id.String(),
			SizeBytes: 100, Width: 100, Height: 100,
		}); err != nil {
			t.Fatalf("prefill host media %d: %v", i, err)
		}
	}

	req := uploadRequest(f.ownerID, f.eventID, testJPEG(t), nextTestIP())
	rec := httptest.NewRecorder()
	f.s.handleUploadEventMedia(rec, req)
	if rec.Code != http.StatusConflict || !containsAll(rec.Body.String(), `"media_quota"`) {
		t.Fatalf("status = %d, body = %s, want 409 media_quota", rec.Code, rec.Body.String())
	}
}

// TestLockAnonDraftEvent_SerializesConcurrentUploads replicates, at the
// transaction level, exactly what handleUploadEventMedia's anon-draft branch
// does inside s.inTx (lock, read usage, insert) for two concurrent
// transactions against a draft one file below its cap. It can't exercise
// this race through the real HTTP handler: media.NewProcessor(1) only
// allows one image-processing job at a time, so one upload's pipeline
// always finishes (and its transaction commits) well before the other
// starts its own transaction, and the race window never opens. Each
// goroutine here sleeps briefly between its usage check and its insert to
// force the two transactions to overlap in time; without LockAnonDraftEvent
// serialising them first, both would read the same under-cap usage count
// during that overlap and both would insert, leaving the draft over its cap.
// With the lock, the second transaction blocks at LockAnonDraftEvent until
// the first commits, so it always sees the first's insert and is rejected.
func TestLockAnonDraftEvent_SerializesConcurrentUploads(t *testing.T) {
	pool := dbTestPool(t)
	rdb := dbTestRedis(t)
	f := newMediaTestFixture(t, pool, rdb)
	ctx := context.Background()
	q := f.s.q

	draft := createTestEvent(t, ctx, q, "birthday", "classic", nil)
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
	cookieHash := sum[:]
	if err := q.CreateAnonDraft(ctx, store.CreateAnonDraftParams{
		ID: uuid.Must(uuid.NewV7()), CookieHash: cookieHash, EventID: draft.ID, ExpiresAt: time.Now().Add(anonDraftTTL),
	}); err != nil {
		t.Fatalf("create anon draft: %v", err)
	}

	for i := 0; i < maxAnonDraftMediaFiles-1; i++ {
		id := uuid.Must(uuid.NewV7())
		if _, err := q.CreateAnonDraftMedia(ctx, store.CreateAnonDraftMediaParams{
			ID: id, StorageKey: draft.ID.String() + "/" + id.String(), SizeBytes: 100,
			Width: 100, Height: 100, EventID: draft.ID, CookieHash: cookieHash,
		}); err != nil {
			t.Fatalf("prefill anon draft media %d: %v", i, err)
		}
	}

	attempt := func() error {
		return f.s.inTx(ctx, func(_ pgx.Tx, txq *store.Queries) error {
			if _, err := txq.LockAnonDraftEvent(ctx, store.LockAnonDraftEventParams{EventID: draft.ID, CookieHash: cookieHash}); err != nil {
				return err
			}
			usage, err := txq.MediaUsage(ctx, store.MediaUsageParams{EventID: draft.ID, UploadedBy: "host"})
			if err != nil {
				return fmt.Errorf("media usage: %w", err)
			}
			if usage.Files >= maxAnonDraftMediaFiles {
				return errMediaQuota
			}
			// Widen the window for a concurrent, unlocked transaction to read
			// the same pre-insert usage count.
			time.Sleep(50 * time.Millisecond)
			id := uuid.Must(uuid.NewV7())
			_, err = txq.CreateAnonDraftMedia(ctx, store.CreateAnonDraftMediaParams{
				ID: id, StorageKey: draft.ID.String() + "/" + id.String(), SizeBytes: 100,
				Width: 100, Height: 100, EventID: draft.ID, CookieHash: cookieHash,
			})
			return err
		})
	}

	var wg sync.WaitGroup
	errs := make(chan error, 2)
	for i := 0; i < 2; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			errs <- attempt()
		}()
	}
	wg.Wait()
	close(errs)

	succeeded, quotaRejected := 0, 0
	for err := range errs {
		switch {
		case err == nil:
			succeeded++
		case errors.Is(err, errMediaQuota):
			quotaRejected++
		default:
			t.Fatalf("unexpected error: %v", err)
		}
	}
	if succeeded != 1 || quotaRejected != 1 {
		t.Fatalf("succeeded = %d, quotaRejected = %d, want exactly one of each (cap respected)", succeeded, quotaRejected)
	}

	usage, err := q.MediaUsage(ctx, store.MediaUsageParams{EventID: draft.ID, UploadedBy: "host"})
	if err != nil {
		t.Fatalf("media usage: %v", err)
	}
	if usage.Files != int64(maxAnonDraftMediaFiles) {
		t.Fatalf("final file count = %d, want exactly the cap (%d)", usage.Files, maxAnonDraftMediaFiles)
	}
}

func TestHandleGetEventMediaFile(t *testing.T) {
	pool := dbTestPool(t)
	rdb := dbTestRedis(t)
	f := newMediaTestFixture(t, pool, rdb)

	uploadReq := uploadRequest(f.ownerID, f.eventID, testJPEG(t), nextTestIP())
	uploadRec := httptest.NewRecorder()
	f.s.handleUploadEventMedia(uploadRec, uploadReq)
	if uploadRec.Code != http.StatusCreated {
		t.Fatalf("upload status = %d, body = %s", uploadRec.Code, uploadRec.Body.String())
	}
	var uploaded struct {
		Media struct{ ID uuid.UUID } `json:"media"`
	}
	if err := json.Unmarshal(uploadRec.Body.Bytes(), &uploaded); err != nil {
		t.Fatalf("parse upload response: %v", err)
	}

	getFile := func(userID uuid.UUID, width string) *httptest.ResponseRecorder {
		req := requestAs(http.MethodGet, "/?w="+width, userID)
		req = withRouteParam(req, "id", f.eventID.String())
		req = withRouteParam(req, "mediaID", uploaded.Media.ID.String())
		rec := httptest.NewRecorder()
		f.s.handleGetEventMediaFile(rec, req)
		return rec
	}

	t.Run("a member can fetch it", func(t *testing.T) {
		rec := getFile(f.ownerID, "480")
		if rec.Code != http.StatusOK {
			t.Fatalf("status = %d, want 200; body = %s", rec.Code, rec.Body.String())
		}
		if ct := rec.Header().Get("X-Content-Type-Options"); ct != "nosniff" {
			t.Errorf("X-Content-Type-Options = %q, want nosniff", ct)
		}
	})

	t.Run("a stranger gets 404", func(t *testing.T) {
		rec := getFile(f.strangerID, "480")
		if rec.Code != http.StatusNotFound {
			t.Fatalf("status = %d, want 404; body = %s", rec.Code, rec.Body.String())
		}
	})

	t.Run("an invalid width is 400", func(t *testing.T) {
		rec := getFile(f.ownerID, "999")
		if rec.Code != http.StatusBadRequest {
			t.Fatalf("status = %d, want 400; body = %s", rec.Code, rec.Body.String())
		}
	})
}

// processTestUpload runs s.processUpload directly (bypassing the HTTP
// handler) to build a committed-but-unrecorded output directory, for tests
// that need real files on disk under a guest-media row they insert by hand.
func processTestUpload(t *testing.T, s *Server, img []byte) (outDir string, result media.Result) {
	t.Helper()
	req := httptest.NewRequest(http.MethodPost, "/", nil)
	req.Header.Set("Content-Type", "image/jpeg")
	rec := httptest.NewRecorder()
	outDir, result, ok := s.processUpload(rec, req, bytes.NewReader(img))
	if !ok {
		t.Fatalf("processUpload failed: status %d, body %s", rec.Code, rec.Body.String())
	}
	return outDir, result
}

func TestSetMediaModeration_ApproveMovesToPublicRejectDeletesFiles(t *testing.T) {
	pool := dbTestPool(t)
	rdb := dbTestRedis(t)
	f := newMediaTestFixture(t, pool, rdb)
	ctx := context.Background()

	// CreateGuestMedia (unlike CreateHostMedia) only inserts against a
	// published event, so the fixture's draft needs a slug and a publish
	// before any guest photo can be created against it.
	slug := "media-mod-fixture-" + strings.ReplaceAll(f.eventID.String(), "-", "")
	settings, err := f.s.q.UpdateEventSettings(ctx, store.UpdateEventSettingsParams{
		Slug: &slug, EventID: f.eventID, UserID: f.ownerID, Version: 1,
	})
	if err != nil {
		t.Fatalf("set slug: %v", err)
	}
	if _, err := f.s.q.PublishEvent(ctx, store.PublishEventParams{EventID: f.eventID, UserID: f.ownerID, Version: settings.Version}); err != nil {
		t.Fatalf("publish event: %v", err)
	}

	// newPendingUpload takes the calling subtest's own *testing.T (never the
	// outer function's): calling t.Fatalf on a *different* T than the one
	// whose t.Run is currently executing panics with "FailNow on a parent
	// test", so this must not close over the outer t.
	newPendingUpload := func(t *testing.T) uuid.UUID {
		t.Helper()
		id := uuid.Must(uuid.NewV7())
		outDir, result := processTestUpload(t, f.s, testJPEG(t))
		if err := f.s.media.Commit(outDir, media.AreaPending, f.eventID, id); err != nil {
			t.Fatalf("commit: %v", err)
		}
		if _, err := f.s.q.CreateGuestMedia(ctx, store.CreateGuestMediaParams{
			ID: id, StorageKey: f.eventID.String() + "/" + id.String(), SizeBytes: result.Bytes,
			Width: int32(result.Width), Height: int32(result.Height), EventID: f.eventID,
		}); err != nil {
			t.Fatalf("create guest media: %v", err)
		}
		return id
	}

	t.Run("approve moves pending files to public and flips status", func(t *testing.T) {
		mediaID := newPendingUpload(t)
		req := requestAs(http.MethodPost, "/", f.ownerID)
		req = withRouteParam(req, "id", f.eventID.String())
		req = withRouteParam(req, "mediaID", mediaID.String())
		rec := httptest.NewRecorder()
		f.s.handleApproveMedia(rec, req)
		if rec.Code != http.StatusOK || !containsAll(rec.Body.String(), `"moderation_status":"approved"`) {
			t.Fatalf("status = %d, body = %s, want 200 approved", rec.Code, rec.Body.String())
		}
		opened, err := f.s.media.Open(f.eventID, mediaID, 480)
		if err != nil {
			t.Errorf("expected the file to be openable (moved to public), got: %v", err)
		} else {
			opened.Close()
		}
	})

	t.Run("reject deletes the files and flips status", func(t *testing.T) {
		mediaID := newPendingUpload(t)
		req := requestAs(http.MethodPost, "/", f.ownerID)
		req = withRouteParam(req, "id", f.eventID.String())
		req = withRouteParam(req, "mediaID", mediaID.String())
		rec := httptest.NewRecorder()
		f.s.handleRejectMedia(rec, req)
		if rec.Code != http.StatusOK || !containsAll(rec.Body.String(), `"moderation_status":"rejected"`) {
			t.Fatalf("status = %d, body = %s, want 200 rejected", rec.Code, rec.Body.String())
		}
		if _, err := f.s.media.Open(f.eventID, mediaID, 480); err == nil {
			t.Error("expected the file to be gone after rejection")
		}
	})

	t.Run("a stranger cannot moderate", func(t *testing.T) {
		mediaID := newPendingUpload(t)
		req := requestAs(http.MethodPost, "/", f.strangerID)
		req = withRouteParam(req, "id", f.eventID.String())
		req = withRouteParam(req, "mediaID", mediaID.String())
		rec := httptest.NewRecorder()
		f.s.handleApproveMedia(rec, req)
		if rec.Code != http.StatusNotFound {
			t.Fatalf("status = %d, want 404; body = %s", rec.Code, rec.Body.String())
		}
	})
}

func TestHandleDeleteEventMedia_InUseIsBlocked(t *testing.T) {
	pool := dbTestPool(t)
	rdb := dbTestRedis(t)
	f := newMediaTestFixture(t, pool, rdb)
	ctx := context.Background()

	uploadReq := uploadRequest(f.ownerID, f.eventID, testJPEG(t), nextTestIP())
	uploadRec := httptest.NewRecorder()
	f.s.handleUploadEventMedia(uploadRec, uploadReq)
	if uploadRec.Code != http.StatusCreated {
		t.Fatalf("upload status = %d, body = %s", uploadRec.Code, uploadRec.Body.String())
	}
	var uploaded struct {
		Media struct{ ID uuid.UUID } `json:"media"`
	}
	if err := json.Unmarshal(uploadRec.Body.Bytes(), &uploaded); err != nil {
		t.Fatalf("parse upload response: %v", err)
	}

	occRow, err := f.s.q.GetOccasion(ctx, "birthday")
	if err != nil {
		t.Fatalf("get occasion: %v", err)
	}
	occ, err := content.ParseOccasion(occRow)
	if err != nil {
		t.Fatalf("parse occasion: %v", err)
	}
	contentJSON := `[{"id":"img1","type":"image","media_id":"` + uploaded.Media.ID.String() + `","alt":"","caption":""}]`
	saved, err := content.ValidateContent([]byte(contentJSON), occ)
	if err != nil {
		t.Fatalf("validate content: %v", err)
	}
	if _, err := f.s.q.UpdateEventContent(ctx, store.UpdateEventContentParams{
		Content: saved.JSON, Title: saved.Title, StartsAt: saved.StartsAt, EndsAt: saved.EffectiveEnd(), EventID: f.eventID, Version: 1, UserID: f.ownerID,
	}); err != nil {
		t.Fatalf("update content: %v", err)
	}

	del := func(userID uuid.UUID) *httptest.ResponseRecorder {
		req := requestAs(http.MethodDelete, "/", userID)
		req = withRouteParam(req, "id", f.eventID.String())
		req = withRouteParam(req, "mediaID", uploaded.Media.ID.String())
		rec := httptest.NewRecorder()
		f.s.handleDeleteEventMedia(rec, req)
		return rec
	}

	if rec := del(f.ownerID); rec.Code != http.StatusConflict || !containsAll(rec.Body.String(), `"media_in_use"`) {
		t.Fatalf("status = %d, body = %s, want 409 media_in_use while referenced", rec.Code, rec.Body.String())
	}

	// Clear the content, then deletion must succeed.
	if _, err := f.s.q.UpdateEventContent(ctx, store.UpdateEventContentParams{
		Content: []byte(`[]`), Title: "Untitled event", EventID: f.eventID, Version: 2, UserID: f.ownerID,
	}); err != nil {
		t.Fatalf("clear content: %v", err)
	}
	if rec := del(f.ownerID); rec.Code != http.StatusNoContent {
		t.Fatalf("status = %d, want 204 once unreferenced; body = %s", rec.Code, rec.Body.String())
	}
}

func TestHandleUploadGuestPhoto(t *testing.T) {
	pool := dbTestPool(t)
	rdb := dbTestRedis(t)

	upload := func(f *publicEventFixture, body []byte, ip string, inviteToken string) *httptest.ResponseRecorder {
		req := requestWithIP(http.MethodPost, "/", ip, body)
		req.Header.Set("Content-Type", "image/jpeg")
		req = withSlugParam(req, f.slug)
		if inviteToken != "" {
			req.AddCookie(&http.Cookie{Name: eventCookieName(inviteCookiePrefix, f.eventID), Value: inviteToken})
		}
		rec := httptest.NewRecorder()
		f.s.handleUploadGuestPhoto(rec, req)
		return rec
	}

	t.Run("open photos accepts a valid image as pending", func(t *testing.T) {
		f := newPublicEventFixture(t, pool, rdb, publicEventOpts{photosOpen: true})
		rec := upload(f, testJPEG(t), nextTestIP(), "")
		if rec.Code != http.StatusCreated || !containsAll(rec.Body.String(), `"moderation_status":"pending"`) {
			t.Fatalf("status = %d, body = %s, want 201 pending", rec.Code, rec.Body.String())
		}
	})

	t.Run("photos closed is 409", func(t *testing.T) {
		f := newPublicEventFixture(t, pool, rdb, publicEventOpts{photosOpen: false})
		rec := upload(f, testJPEG(t), nextTestIP(), "")
		if rec.Code != http.StatusConflict || !containsAll(rec.Body.String(), `"photos_closed"`) {
			t.Fatalf("status = %d, body = %s, want 409 photos_closed", rec.Code, rec.Body.String())
		}
	})

	t.Run("invite_only rsvp_mode requires a guest cookie", func(t *testing.T) {
		f := newPublicEventFixture(t, pool, rdb, publicEventOpts{photosOpen: true, rsvpMode: "invite_only"})
		rec := upload(f, testJPEG(t), nextTestIP(), "")
		if rec.Code != http.StatusForbidden || !containsAll(rec.Body.String(), `"invite_required"`) {
			t.Fatalf("status = %d, body = %s, want 403 invite_required", rec.Code, rec.Body.String())
		}

		_, inviteToken := f.testGuest(t, "Photo Guest", 1)
		rec = upload(f, testJPEG(t), nextTestIP(), inviteToken)
		if rec.Code != http.StatusCreated {
			t.Fatalf("status = %d, want 201 with a valid invite cookie; body = %s", rec.Code, rec.Body.String())
		}
	})

	t.Run("unsupported type is 415", func(t *testing.T) {
		f := newPublicEventFixture(t, pool, rdb, publicEventOpts{photosOpen: true})
		rec := upload(f, []byte(strings.Repeat("not an image", 50)), nextTestIP(), "")
		if rec.Code != http.StatusUnsupportedMediaType {
			t.Fatalf("status = %d, want 415; body = %s", rec.Code, rec.Body.String())
		}
	})

	t.Run("a password-gated event still requires the access cookie", func(t *testing.T) {
		f := newPublicEventFixture(t, pool, rdb, publicEventOpts{photosOpen: true, password: "letmein12345"})
		rec := upload(f, testJPEG(t), nextTestIP(), "")
		if rec.Code != http.StatusUnauthorized {
			t.Fatalf("status = %d, want 401 password_required; body = %s", rec.Code, rec.Body.String())
		}
	})
}
