package httpapi

import (
	"context"
	"crypto/sha256"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/ansonarose/seeyouthere-backend/internal/media"
	"github.com/ansonarose/seeyouthere-backend/internal/store"
)

// seedHostMedia inserts a host media row of the given size for owner into
// eventID, with no objects behind it (only the byte count matters to quota).
func seedHostMedia(t *testing.T, s *Server, ownerID, eventID uuid.UUID, size int64) uuid.UUID {
	t.Helper()
	id := uuid.Must(uuid.NewV7())
	if _, err := s.q.CreateHostMedia(context.Background(), store.CreateHostMediaParams{
		ID: id, UserID: ownerID, EventID: eventID, StorageKey: eventID.String() + "/" + id.String(),
		SizeBytes: size, Width: 100, Height: 100,
	}); err != nil {
		t.Fatalf("seed host media: %v", err)
	}
	return id
}

// objectCount lists the objects stored under the event's prefix.
func objectCount(t *testing.T, b *testBlobs, eventID uuid.UUID) int {
	t.Helper()
	n := 0
	err := b.inner.List(context.Background(), "e/"+eventID.String()+"/", func(page []media.ObjectInfo) error {
		n += len(page)
		return nil
	})
	if err != nil {
		t.Fatalf("list objects: %v", err)
	}
	return n
}

func mediaRowCount(t *testing.T, pool *pgxpool.Pool, eventID uuid.UUID) int {
	t.Helper()
	var n int
	if err := pool.QueryRow(context.Background(), "SELECT count(*) FROM media WHERE event_id = $1", eventID).Scan(&n); err != nil {
		t.Fatalf("count media: %v", err)
	}
	return n
}

func hostUpload(f eventTestFixture, userID, eventID uuid.UUID, img []byte) *httptest.ResponseRecorder {
	rec := httptest.NewRecorder()
	f.s.handleUploadEventMedia(rec, uploadRequest(userID, eventID, img, nextTestIP()))
	return rec
}

const quotaMessage200 = "You've used your 200 MB of photo storage. Delete some photos or past events to upload more."

func TestUploadHost_StorageQuota(t *testing.T) {
	pool := dbTestPool(t)
	rdb := dbTestRedis(t)
	img := testJPEG(t)

	t.Run("pre-check refuses an owner at the cap before any storage call", func(t *testing.T) {
		f, blobs := newMediaTestFixtureBlobs(t, pool, rdb)
		seedHostMedia(t, f.s, f.ownerID, f.eventID, testUserQuotaBytes)
		rec := hostUpload(f, f.ownerID, f.eventID, img)
		if rec.Code != http.StatusConflict || !strings.Contains(rec.Body.String(), `"storage_quota"`) ||
			!strings.Contains(rec.Body.String(), quotaMessage200) {
			t.Fatalf("status = %d, body = %s, want 409 storage_quota with the 200 MB message", rec.Code, rec.Body.String())
		}
		if blobs.calls() != 0 {
			t.Errorf("storage was called %d times for a refused upload", blobs.calls())
		}
	})

	t.Run("in-tx check refuses an upload that does not fit and removes its objects", func(t *testing.T) {
		f, blobs := newMediaTestFixtureBlobs(t, pool, rdb)
		seedHostMedia(t, f.s, f.ownerID, f.eventID, testUserQuotaBytes-1) // pre-check passes, the upload won't fit
		rec := hostUpload(f, f.ownerID, f.eventID, img)
		if rec.Code != http.StatusConflict || !strings.Contains(rec.Body.String(), `"storage_quota"`) {
			t.Fatalf("status = %d, body = %s, want 409 storage_quota", rec.Code, rec.Body.String())
		}
		if blobs.puts != 3 {
			t.Errorf("puts = %d, want 3 (the objects were uploaded before the tx refused)", blobs.puts)
		}
		if n := objectCount(t, blobs, f.eventID); n != 0 {
			t.Errorf("%d objects left after a refused upload, want 0", n)
		}
		if n := mediaRowCount(t, pool, f.eventID); n != 1 {
			t.Errorf("media rows = %d, want only the seeded one", n)
		}
	})

	t.Run("co-hosts are charged to the event owner", func(t *testing.T) {
		f, blobs := newMediaTestFixtureBlobs(t, pool, rdb)
		seedHostMedia(t, f.s, f.ownerID, f.eventID, testUserQuotaBytes)
		rec := hostUpload(f, f.editorID, f.eventID, img)
		if rec.Code != http.StatusConflict || !strings.Contains(rec.Body.String(), `"storage_quota"`) {
			t.Fatalf("status = %d, body = %s, want 409 storage_quota for the owner's usage", rec.Code, rec.Body.String())
		}
		if blobs.calls() != 0 {
			t.Errorf("storage was called for a refused upload")
		}
	})

	t.Run("usage under the cap succeeds and counts toward it", func(t *testing.T) {
		f, _ := newMediaTestFixtureBlobs(t, pool, rdb)
		_, size := uploadHostMedia(t, f, f.ownerID, f.eventID)
		f.s.cfg.MediaUserQuotaBytes = size + size/2 // room for one, not two
		rec := hostUpload(f, f.ownerID, f.eventID, img)
		if rec.Code != http.StatusConflict || !strings.Contains(rec.Body.String(), `"storage_quota"`) {
			t.Fatalf("second upload: status = %d, body = %s, want 409", rec.Code, rec.Body.String())
		}
	})

	t.Run("soft-deleted events free the owner's quota but still count globally", func(t *testing.T) {
		f, _ := newMediaTestFixtureBlobs(t, pool, rdb)
		ctx := context.Background()
		gone := createTestEvent(t, ctx, f.s.q, "birthday", "classic", &f.ownerID)
		t.Cleanup(func() { _, _ = pool.Exec(context.Background(), "DELETE FROM events WHERE id = $1", gone.ID) })
		before, err := f.s.q.MediaStorageUsage(ctx, store.MediaStorageUsageParams{OwnerID: &f.ownerID})
		if err != nil {
			t.Fatalf("usage: %v", err)
		}
		seedHostMedia(t, f.s, f.ownerID, gone.ID, testUserQuotaBytes)
		if _, err := pool.Exec(ctx, "UPDATE events SET deleted_at = now() WHERE id = $1", gone.ID); err != nil {
			t.Fatalf("soft delete: %v", err)
		}

		after, err := f.s.q.MediaStorageUsage(ctx, store.MediaStorageUsageParams{OwnerID: &f.ownerID})
		if err != nil {
			t.Fatalf("usage: %v", err)
		}
		if after.OwnerBytes != before.OwnerBytes {
			t.Errorf("owner bytes %d -> %d, soft-deleted media must not count per user", before.OwnerBytes, after.OwnerBytes)
		}
		if after.TotalBytes != before.TotalBytes+testUserQuotaBytes {
			t.Errorf("total bytes %d -> %d, soft-deleted media must still count globally", before.TotalBytes, after.TotalBytes)
		}

		// Per-user: the owner is not blocked by the soft-deleted event.
		if rec := hostUpload(f, f.ownerID, f.eventID, img); rec.Code != http.StatusCreated {
			t.Fatalf("owner upload: status = %d, body = %s, want 201", rec.Code, rec.Body.String())
		}
		// Global: a ceiling that only leaves room without the soft-deleted bytes refuses.
		f.s.cfg.MediaTotalQuotaBytes = before.TotalBytes + testUserQuotaBytes/2
		rec := hostUpload(f, f.ownerID, f.eventID, img)
		if rec.Code != http.StatusInsufficientStorage || !strings.Contains(rec.Body.String(), `"storage_full"`) {
			t.Fatalf("status = %d, body = %s, want 507 storage_full", rec.Code, rec.Body.String())
		}
	})

	t.Run("global ceiling is 507 and leaves no objects", func(t *testing.T) {
		f, blobs := newMediaTestFixtureBlobs(t, pool, rdb)
		usage, err := f.s.q.MediaStorageUsage(context.Background(), store.MediaStorageUsageParams{OwnerID: &f.ownerID})
		if err != nil {
			t.Fatalf("usage: %v", err)
		}
		f.s.cfg.MediaTotalQuotaBytes = usage.TotalBytes + 1 // pre-check passes, the upload can't fit
		rec := hostUpload(f, f.ownerID, f.eventID, img)
		if rec.Code != http.StatusInsufficientStorage || !strings.Contains(rec.Body.String(), "Photo storage is full right now") {
			t.Fatalf("status = %d, body = %s, want 507 storage_full", rec.Code, rec.Body.String())
		}
		if n := objectCount(t, blobs, f.eventID); n != 0 {
			t.Errorf("%d objects left, want 0", n)
		}

		f.s.cfg.MediaTotalQuotaBytes = usage.TotalBytes // pre-check itself refuses
		before := blobs.calls()
		rec = hostUpload(f, f.ownerID, f.eventID, img)
		if rec.Code != http.StatusInsufficientStorage || blobs.calls() != before {
			t.Fatalf("pre-check: status = %d, calls changed = %v", rec.Code, blobs.calls() != before)
		}
	})
}

func TestUploadHost_AnonDraftSkipsPerUserCap(t *testing.T) {
	pool := dbTestPool(t)
	rdb := dbTestRedis(t)
	ctx := context.Background()
	f := newMediaTestFixture(t, pool, rdb)
	img := testJPEG(t)

	draft := createTestEvent(t, ctx, f.s.q, "birthday", "classic", nil)
	t.Cleanup(func() { _, _ = pool.Exec(context.Background(), "DELETE FROM events WHERE id = $1", draft.ID) })
	token, err := randomToken()
	if err != nil {
		t.Fatalf("randomToken: %v", err)
	}
	sum := sha256.Sum256([]byte(token))
	if err := f.s.q.CreateAnonDraft(ctx, store.CreateAnonDraftParams{
		ID: uuid.Must(uuid.NewV7()), CookieHash: sum[:], EventID: draft.ID, ExpiresAt: time.Now().Add(anonDraftTTL),
	}); err != nil {
		t.Fatalf("create anon draft: %v", err)
	}
	upload := func() *httptest.ResponseRecorder {
		req := uploadRequest(uuid.Nil, draft.ID, img, nextTestIP())
		req.AddCookie(&http.Cookie{Name: draftCookieName, Value: token})
		rec := httptest.NewRecorder()
		f.s.handleUploadEventMedia(rec, req)
		return rec
	}

	f.s.cfg.MediaUserQuotaBytes = 1 // would refuse any owned upload
	if rec := upload(); rec.Code != http.StatusCreated {
		t.Fatalf("status = %d, body = %s, want 201: drafts have no owner cap", rec.Code, rec.Body.String())
	}

	usage, err := f.s.q.MediaStorageUsage(ctx, store.MediaStorageUsageParams{})
	if err != nil {
		t.Fatalf("usage: %v", err)
	}
	if usage.OwnerBytes != 0 {
		t.Errorf("owner bytes for a NULL owner = %d, want 0", usage.OwnerBytes)
	}
	f.s.cfg.MediaTotalQuotaBytes = usage.TotalBytes
	if rec := upload(); rec.Code != http.StatusInsufficientStorage {
		t.Fatalf("status = %d, body = %s, want 507: drafts still count toward the global ceiling", rec.Code, rec.Body.String())
	}
}

func publishTestEvent(t *testing.T, f eventTestFixture) {
	t.Helper()
	ctx := context.Background()
	slug := "media-quota-" + strings.ReplaceAll(f.eventID.String(), "-", "")
	settings, err := f.s.q.UpdateEventSettings(ctx, store.UpdateEventSettingsParams{
		Slug: &slug, EventID: f.eventID, UserID: f.ownerID, Version: 1,
	})
	if err != nil {
		t.Fatalf("set slug: %v", err)
	}
	if _, err := f.s.q.PublishEvent(ctx, store.PublishEventParams{EventID: f.eventID, UserID: f.ownerID, Version: settings.Version}); err != nil {
		t.Fatalf("publish event: %v", err)
	}
}

// seedPendingGuest inserts a pending guest photo of the given size into
// eventID, with no objects behind it (only the byte count matters to quota).
func seedPendingGuest(t *testing.T, pool *pgxpool.Pool, eventID uuid.UUID, size int64) uuid.UUID {
	t.Helper()
	id := uuid.Must(uuid.NewV7())
	if _, err := pool.Exec(context.Background(),
		`INSERT INTO media (id, owner_id, event_id, storage_key, content_type, size_bytes, width, height, uploaded_by, moderation_status)
		 VALUES ($1, NULL, $2, $3, 'image/jpeg', $4, 10, 10, 'guest', 'pending')`,
		id, eventID, eventID.String()+"/"+id.String(), size); err != nil {
		t.Fatalf("seed pending guest media: %v", err)
	}
	return id
}

// setPendingGuestCap lowers the per-event pending allowance for one test.
func setPendingGuestCap(t *testing.T, n int64) {
	t.Helper()
	old := maxEventPendingGuestBytes
	maxEventPendingGuestBytes = n
	t.Cleanup(func() { maxEventPendingGuestBytes = old })
}

func guestUpload(f *publicEventFixture, img []byte) *httptest.ResponseRecorder {
	req := requestWithIP(http.MethodPost, "/", nextTestIP(), img)
	req.Header.Set("Content-Type", "image/jpeg")
	req = withSlugParam(req, f.slug)
	rec := httptest.NewRecorder()
	f.s.handleUploadGuestPhoto(rec, req)
	return rec
}

// txCheck runs lockAndCheckStorage in a transaction that is rolled back.
func txCheck(t *testing.T, s *Server, ownerID, guestEventID *uuid.UUID, add int64) error {
	t.Helper()
	errRollback := errors.New("rollback")
	var got error
	err := s.inTx(context.Background(), func(_ pgx.Tx, q *store.Queries) error {
		got = s.lockAndCheckStorage(context.Background(), q, ownerID, guestEventID, add)
		return errRollback
	})
	if !errors.Is(err, errRollback) {
		t.Fatalf("tx: %v", err)
	}
	return got
}

// A guest upload is not charged to the owner's cap: it succeeds (pending)
// even when the owner is already at the cap. (Replaces the old behaviour
// where an owner at the cap made guest uploads fail with photo_limit.)
func TestUploadGuest_NotChargedToOwnerCap(t *testing.T) {
	pool := dbTestPool(t)
	rdb := dbTestRedis(t)
	f := newPublicEventFixture(t, pool, rdb, publicEventOpts{photosOpen: true})
	f.useCountingBlobs(t)
	seedHostMedia(t, f.s, f.ownerID, f.eventID, testUserQuotaBytes)

	rec := guestUpload(f, testJPEG(t))
	if rec.Code != http.StatusCreated || !strings.Contains(rec.Body.String(), `"pending"`) {
		t.Fatalf("status = %d, body = %s, want 201 pending", rec.Code, rec.Body.String())
	}
}

func TestPendingGuestPhotosDoNotReduceOwnerHeadroom(t *testing.T) {
	pool := dbTestPool(t)
	rdb := dbTestRedis(t)
	f := newMediaTestFixture(t, pool, rdb)
	publishTestEvent(t, f)
	ctx := context.Background()

	f.s.cfg.MediaUserQuotaBytes = 1000
	before, err := f.s.q.MediaStorageUsage(ctx, store.MediaStorageUsageParams{OwnerID: &f.ownerID, EventID: &f.eventID})
	if err != nil {
		t.Fatalf("usage: %v", err)
	}
	pending := seedPendingGuest(t, pool, f.eventID, 5000)
	after, err := f.s.q.MediaStorageUsage(ctx, store.MediaStorageUsageParams{OwnerID: &f.ownerID, EventID: &f.eventID})
	if err != nil {
		t.Fatalf("usage: %v", err)
	}
	if after.OwnerBytes != before.OwnerBytes {
		t.Errorf("owner_bytes %d -> %d, pending guest photo must not be charged to the owner", before.OwnerBytes, after.OwnerBytes)
	}
	if after.TotalBytes != before.TotalBytes+5000 {
		t.Errorf("total_bytes %d -> %d, pending guest photo must count toward the global ceiling", before.TotalBytes, after.TotalBytes)
	}
	if after.PendingGuestBytes != before.PendingGuestBytes+5000 {
		t.Errorf("pending_guest_bytes %d -> %d, want +5000", before.PendingGuestBytes, after.PendingGuestBytes)
	}

	// The host can still use the whole cap.
	room := 1000 - before.OwnerBytes
	if err := txCheck(t, f.s, &f.ownerID, nil, room); err != nil {
		t.Errorf("host upload filling the cap: %v, want nil", err)
	}
	if err := txCheck(t, f.s, &f.ownerID, nil, room+1); !errors.Is(err, errStorageQuota) {
		t.Errorf("host upload over the cap: %v, want errStorageQuota", err)
	}

	// Once rejected, the photo counts nowhere.
	if _, err := pool.Exec(ctx, "UPDATE media SET moderation_status = 'rejected' WHERE id = $1", pending); err != nil {
		t.Fatalf("reject: %v", err)
	}
	rejected, err := f.s.q.MediaStorageUsage(ctx, store.MediaStorageUsageParams{OwnerID: &f.ownerID, EventID: &f.eventID})
	if err != nil {
		t.Fatalf("usage: %v", err)
	}
	if rejected.TotalBytes != before.TotalBytes || rejected.PendingGuestBytes != before.PendingGuestBytes {
		t.Errorf("rejected photo still counted: %+v vs %+v", rejected, before)
	}
}

func TestUploadGuest_PendingAllowancePerEvent(t *testing.T) {
	pool := dbTestPool(t)
	rdb := dbTestRedis(t)
	f := newPublicEventFixture(t, pool, rdb, publicEventOpts{photosOpen: true})
	blobs := f.useCountingBlobs(t)
	const allowance = 1000
	setPendingGuestCap(t, allowance)

	t.Run("exactly at the allowance is accepted, one byte more is refused", func(t *testing.T) {
		seedPendingGuest(t, pool, f.eventID, allowance-10)
		if err := txCheck(t, f.s, nil, &f.eventID, 10); err != nil {
			t.Errorf("exactly at the allowance: %v, want nil", err)
		}
		if err := txCheck(t, f.s, nil, &f.eventID, 11); !errors.Is(err, errGuestPendingFull) {
			t.Errorf("one byte over: %v, want errGuestPendingFull", err)
		}
	})

	t.Run("allowance already reached refuses the upload with photo_limit and no leftovers", func(t *testing.T) {
		seedPendingGuest(t, pool, f.eventID, 10) // now exactly at the allowance
		rec := guestUpload(f, testJPEG(t))
		if rec.Code != http.StatusConflict || !strings.Contains(rec.Body.String(), `"photo_limit"`) ||
			!strings.Contains(rec.Body.String(), "This event isn't accepting more photos right now.") {
			t.Fatalf("status = %d, body = %s, want 409 photo_limit", rec.Code, rec.Body.String())
		}
		if n := objectCount(t, blobs, f.eventID); n != 0 {
			t.Errorf("%d objects left after a refused guest upload, want 0", n)
		}
		if n := mediaRowCount(t, pool, f.eventID); n != 2 {
			t.Errorf("media rows = %d, want only the 2 seeded ones", n)
		}
	})

	t.Run("in-transaction check refuses when the pre-check passed", func(t *testing.T) {
		// An allowance the pre-check (used >= cap) passes with nothing used,
		// but any real upload exceeds.
		setPendingGuestCap(t, 1)
		other := newPublicEventFixture(t, pool, rdb, publicEventOpts{photosOpen: true})
		other.useCountingBlobs(t)
		rec := guestUpload(other, testJPEG(t))
		if rec.Code != http.StatusConflict || !strings.Contains(rec.Body.String(), `"photo_limit"`) {
			t.Fatalf("status = %d, body = %s, want 409 photo_limit", rec.Code, rec.Body.String())
		}
		if n := mediaRowCount(t, pool, other.eventID); n != 0 {
			t.Errorf("media rows = %d, want 0", n)
		}
	})

	t.Run("approved photos no longer use the allowance", func(t *testing.T) {
		if _, err := pool.Exec(context.Background(),
			"UPDATE media SET moderation_status = 'approved' WHERE event_id = $1", f.eventID); err != nil {
			t.Fatalf("approve: %v", err)
		}
		if err := txCheck(t, f.s, nil, &f.eventID, allowance); err != nil {
			t.Errorf("after approval: %v, want nil", err)
		}
	})
}

func TestStorageCheck_GlobalCeilingCountsPendingGuestPhotos(t *testing.T) {
	pool := dbTestPool(t)
	rdb := dbTestRedis(t)
	f := newMediaTestFixture(t, pool, rdb)
	publishTestEvent(t, f)
	before, err := f.s.q.MediaStorageUsage(context.Background(), store.MediaStorageUsageParams{})
	if err != nil {
		t.Fatalf("usage: %v", err)
	}
	seedPendingGuest(t, pool, f.eventID, 500)
	f.s.cfg.MediaTotalQuotaBytes = before.TotalBytes + 500 // full only if the pending photo counts
	if err := txCheck(t, f.s, nil, &f.eventID, 1); !errors.Is(err, errStorageFull) {
		t.Errorf("guest upload at the ceiling: %v, want errStorageFull", err)
	}
	if err := txCheck(t, f.s, &f.ownerID, nil, 1); !errors.Is(err, errStorageFull) {
		t.Errorf("host upload at the ceiling: %v, want errStorageFull", err)
	}
}

func TestSetMediaModeration_ApprovalChargesOwnerCap(t *testing.T) {
	pool := dbTestPool(t)
	rdb := dbTestRedis(t)
	f := newMediaTestFixture(t, pool, rdb)
	publishTestEvent(t, f)
	ctx := context.Background()

	moderate := func(h func(http.ResponseWriter, *http.Request), eventID, id uuid.UUID) *httptest.ResponseRecorder {
		req := requestAs(http.MethodPost, "/", f.ownerID)
		req = withRouteParam(withRouteParam(req, "id", eventID.String()), "mediaID", id.String())
		rec := httptest.NewRecorder()
		h(rec, req)
		return rec
	}
	status := func(id uuid.UUID) string {
		var st string
		if err := pool.QueryRow(ctx, "SELECT moderation_status FROM media WHERE id = $1", id).Scan(&st); err != nil {
			t.Fatalf("status: %v", err)
		}
		return st
	}
	usage, err := f.s.q.MediaStorageUsage(ctx, store.MediaStorageUsageParams{OwnerID: &f.ownerID})
	if err != nil {
		t.Fatalf("usage: %v", err)
	}
	base := usage.OwnerBytes
	// Cap leaves room for 1000 more bytes beyond what the owner already uses.
	f.s.cfg.MediaUserQuotaBytes = base + 1000

	hostRow := seedHostMedia(t, f.s, f.ownerID, f.eventID, 600)
	pending := seedPendingGuest(t, pool, f.eventID, 600)

	t.Run("approval over the cap is refused with storage_quota and rolled back", func(t *testing.T) {
		rec := moderate(f.s.handleApproveMedia, f.eventID, pending)
		if rec.Code != http.StatusConflict || !strings.Contains(rec.Body.String(), `"storage_quota"`) ||
			!strings.Contains(rec.Body.String(), "so this photo can't be approved") {
			t.Fatalf("status = %d, body = %s, want 409 storage_quota", rec.Code, rec.Body.String())
		}
		if st := status(pending); st != "pending" {
			t.Errorf("status = %q, want pending", st)
		}
	})

	t.Run("approval succeeds after the owner frees space", func(t *testing.T) {
		if _, err := pool.Exec(ctx, "DELETE FROM media WHERE id = $1", hostRow); err != nil {
			t.Fatalf("delete host media: %v", err)
		}
		rec := moderate(f.s.handleApproveMedia, f.eventID, pending)
		if rec.Code != http.StatusOK || !strings.Contains(rec.Body.String(), `"approved"`) {
			t.Fatalf("status = %d, body = %s, want 200 approved", rec.Code, rec.Body.String())
		}
		if st := status(pending); st != "approved" {
			t.Errorf("status = %q, want approved", st)
		}
		// Approving an already approved photo changes nothing (404, as before).
		if rec := moderate(f.s.handleApproveMedia, f.eventID, pending); rec.Code != http.StatusNotFound {
			t.Errorf("re-approve status = %d, want 404", rec.Code)
		}
		after, err := f.s.q.MediaStorageUsage(ctx, store.MediaStorageUsageParams{OwnerID: &f.ownerID})
		if err != nil {
			t.Fatalf("usage: %v", err)
		}
		if after.OwnerBytes != base+600 {
			t.Errorf("owner_bytes = %d, want %d", after.OwnerBytes, base+600)
		}
	})

	t.Run("approval landing exactly on the cap is allowed", func(t *testing.T) {
		exact := seedPendingGuest(t, pool, f.eventID, 400)
		if rec := moderate(f.s.handleApproveMedia, f.eventID, exact); rec.Code != http.StatusOK {
			t.Fatalf("status = %d, body = %s, want 200", rec.Code, rec.Body.String())
		}
	})

	t.Run("reject always works, even over the cap", func(t *testing.T) {
		over := seedPendingGuest(t, pool, f.eventID, 5000)
		if rec := moderate(f.s.handleApproveMedia, f.eventID, over); rec.Code != http.StatusConflict {
			t.Fatalf("approve status = %d, want 409", rec.Code)
		}
		rec := moderate(f.s.handleRejectMedia, f.eventID, over)
		if rec.Code != http.StatusOK || !strings.Contains(rec.Body.String(), `"rejected"`) {
			t.Fatalf("reject status = %d, body = %s, want 200", rec.Code, rec.Body.String())
		}
	})

	t.Run("non-members cannot approve", func(t *testing.T) {
		p := seedPendingGuest(t, pool, f.eventID, 1)
		req := requestAs(http.MethodPost, "/", f.strangerID)
		req = withRouteParam(withRouteParam(req, "id", f.eventID.String()), "mediaID", p.String())
		rec := httptest.NewRecorder()
		f.s.handleApproveMedia(rec, req)
		if rec.Code != http.StatusNotFound {
			t.Errorf("status = %d, want 404", rec.Code)
		}
		if st := status(p); st != "pending" {
			t.Errorf("status = %q, want pending", st)
		}
	})
}

// Two approvals of pending photos in different events of one owner, each
// fitting alone but not together: exactly one succeeds.
func TestSetMediaModeration_ConcurrentApprovalsRespectTheCap(t *testing.T) {
	pool := dbTestPool(t)
	rdb := dbTestRedis(t)
	f := newMediaTestFixture(t, pool, rdb)
	ctx := context.Background()
	second := createTestEvent(t, ctx, f.s.q, "birthday", "classic", &f.ownerID)
	t.Cleanup(func() { _, _ = pool.Exec(context.Background(), "DELETE FROM events WHERE id = $1", second.ID) })

	usage, err := f.s.q.MediaStorageUsage(ctx, store.MediaStorageUsageParams{OwnerID: &f.ownerID})
	if err != nil {
		t.Fatalf("usage: %v", err)
	}
	f.s.cfg.MediaUserQuotaBytes = usage.OwnerBytes + 1500
	ids := map[uuid.UUID]uuid.UUID{
		f.eventID: seedPendingGuest(t, pool, f.eventID, 1000),
		second.ID: seedPendingGuest(t, pool, second.ID, 1000),
	}

	codes := make(chan int, 2)
	var wg sync.WaitGroup
	for eventID, mediaID := range ids {
		wg.Add(1)
		go func() {
			defer wg.Done()
			req := requestAs(http.MethodPost, "/", f.ownerID)
			req = withRouteParam(withRouteParam(req, "id", eventID.String()), "mediaID", mediaID.String())
			rec := httptest.NewRecorder()
			f.s.handleApproveMedia(rec, req)
			codes <- rec.Code
		}()
	}
	wg.Wait()
	close(codes)
	ok, refused := 0, 0
	for c := range codes {
		switch c {
		case http.StatusOK:
			ok++
		case http.StatusConflict:
			refused++
		default:
			t.Fatalf("unexpected status %d", c)
		}
	}
	if ok != 1 || refused != 1 {
		t.Fatalf("ok = %d, refused = %d, want exactly one of each", ok, refused)
	}
}

// useCountingBlobs swaps f.s.media for a store over a counting backend.
func (f *publicEventFixture) useCountingBlobs(t *testing.T) *testBlobs {
	t.Helper()
	st, blobs := newTestMediaStoreBlobs(t)
	f.s.media = st
	return blobs
}

func TestUpload_CommitFailureIs503(t *testing.T) {
	pool := dbTestPool(t)
	rdb := dbTestRedis(t)
	img := testJPEG(t)

	for _, c := range []struct {
		name  string
		after int // successful Puts before the failure
	}{{"first rendition fails", 0}, {"second rendition fails", 1}} {
		t.Run(c.name, func(t *testing.T) {
			f, blobs := newMediaTestFixtureBlobs(t, pool, rdb)
			blobs.putErr = fmt.Errorf("put: %w", media.ErrStorageUnavailable)
			blobs.putErrAfter = c.after
			rec := hostUpload(f, f.ownerID, f.eventID, img)
			if rec.Code != http.StatusServiceUnavailable || !strings.Contains(rec.Body.String(), `"storage_unavailable"`) {
				t.Fatalf("status = %d, body = %s, want 503 storage_unavailable", rec.Code, rec.Body.String())
			}
			if rec.Header().Get("Retry-After") != "30" {
				t.Errorf("Retry-After = %q, want 30", rec.Header().Get("Retry-After"))
			}
			if n := mediaRowCount(t, pool, f.eventID); n != 0 {
				t.Errorf("media rows = %d, want none after a failed commit", n)
			}
			if n := objectCount(t, blobs, f.eventID); n != 0 {
				t.Errorf("%d objects left after a failed commit, want 0", n)
			}
		})
	}
}

func TestUpload_DailyWriteBudget(t *testing.T) {
	pool := dbTestPool(t)
	rdb := dbTestRedis(t)
	f, blobs := newMediaTestFixtureBlobs(t, pool, rdb)

	before, err := rdb.Get(context.Background(), "rl:r2:writes:day").Int64()
	if err != nil && err.Error() != "redis: nil" {
		t.Fatalf("read counter: %v", err)
	}
	uploadHostMedia(t, f, f.ownerID, f.eventID)
	after, _ := rdb.Get(context.Background(), "rl:r2:writes:day").Int64()
	if after-before != 3 {
		t.Errorf("write budget moved by %d, want 3 (one per rendition)", after-before)
	}

	exhaustBudget(t, rdb, "r2:writes:day", mediaWritesPerDay, 2) // 3 more would not fit
	puts := blobs.puts
	rec := hostUpload(f, f.ownerID, f.eventID, testJPEG(t))
	if rec.Code != http.StatusServiceUnavailable || !strings.Contains(rec.Body.String(), `"uploads_paused"`) ||
		!strings.Contains(rec.Body.String(), "Photo uploads are paused for today. Please try again tomorrow.") {
		t.Fatalf("status = %d, body = %s, want 503 uploads_paused", rec.Code, rec.Body.String())
	}
	if blobs.puts != puts {
		t.Errorf("storage was written to while the budget was exhausted")
	}
}

// TestUpload_ConcurrentUploadsOfOneOwnerRespectTheCap runs the quota section
// of the insert transaction for two different events of the same owner at
// once, each alone within the cap but not together. Each goroutine pauses
// between check and insert so that, without the advisory lock, both would see
// the same usage and both insert.
func TestUpload_ConcurrentUploadsOfOneOwnerRespectTheCap(t *testing.T) {
	pool := dbTestPool(t)
	rdb := dbTestRedis(t)
	f := newMediaTestFixture(t, pool, rdb)
	ctx := context.Background()
	second := createTestEvent(t, ctx, f.s.q, "birthday", "classic", &f.ownerID)
	t.Cleanup(func() { _, _ = pool.Exec(context.Background(), "DELETE FROM events WHERE id = $1", second.ID) })

	const size = 1000
	f.s.cfg.MediaUserQuotaBytes = size + size/2

	attempt := func(eventID uuid.UUID) error {
		return f.s.inTx(ctx, func(_ pgx.Tx, q *store.Queries) error {
			lock, err := q.LockEventForEditor(ctx, store.LockEventForEditorParams{EventID: eventID, UserID: f.ownerID})
			if err != nil {
				return err
			}
			if err := f.s.lockAndCheckStorage(ctx, q, lock.OwnerID, nil, size); err != nil {
				return err
			}
			time.Sleep(100 * time.Millisecond)
			id := uuid.Must(uuid.NewV7())
			_, err = q.CreateHostMedia(ctx, store.CreateHostMediaParams{
				ID: id, UserID: f.ownerID, EventID: eventID, StorageKey: eventID.String() + "/" + id.String(),
				SizeBytes: size, Width: 10, Height: 10,
			})
			return err
		})
	}

	var wg sync.WaitGroup
	errs := make(chan error, 2)
	for _, id := range []uuid.UUID{f.eventID, second.ID} {
		wg.Add(1)
		go func() {
			defer wg.Done()
			errs <- attempt(id)
		}()
	}
	wg.Wait()
	close(errs)
	ok, refused := 0, 0
	for err := range errs {
		switch {
		case err == nil:
			ok++
		case errors.Is(err, errStorageQuota):
			refused++
		default:
			t.Fatalf("unexpected error: %v", err)
		}
	}
	if ok != 1 || refused != 1 {
		t.Fatalf("ok = %d, refused = %d, want exactly one of each", ok, refused)
	}
}

// TestUpload_ConcurrentHTTPUploadsOfOneOwner is the same scenario through the
// real handlers.
func TestUpload_ConcurrentHTTPUploadsOfOneOwner(t *testing.T) {
	pool := dbTestPool(t)
	rdb := dbTestRedis(t)
	f, _ := newMediaTestFixtureBlobs(t, pool, rdb)
	ctx := context.Background()
	second := createTestEvent(t, ctx, f.s.q, "birthday", "classic", &f.ownerID)
	t.Cleanup(func() { _, _ = pool.Exec(context.Background(), "DELETE FROM events WHERE id = $1", second.ID) })

	img := testJPEG(t)
	_, size := uploadHostMedia(t, f, f.ownerID, f.eventID)
	if _, err := pool.Exec(ctx, "DELETE FROM media WHERE event_id = $1", f.eventID); err != nil {
		t.Fatalf("reset media: %v", err)
	}
	f.s.cfg.MediaUserQuotaBytes = size + size/2

	codes := make(chan int, 2)
	var wg sync.WaitGroup
	for _, id := range []uuid.UUID{f.eventID, second.ID} {
		wg.Add(1)
		go func() {
			defer wg.Done()
			codes <- hostUpload(f, f.ownerID, id, img).Code
		}()
	}
	wg.Wait()
	close(codes)
	created, conflict := 0, 0
	for c := range codes {
		switch c {
		case http.StatusCreated:
			created++
		case http.StatusConflict:
			conflict++
		default:
			t.Fatalf("unexpected status %d", c)
		}
	}
	if created != 1 || conflict != 1 {
		t.Fatalf("created = %d, conflict = %d, want exactly one 201 and one 409", created, conflict)
	}
}

func TestDeleteEventMedia_FilesFirst(t *testing.T) {
	pool := dbTestPool(t)
	rdb := dbTestRedis(t)
	f, blobs := newMediaTestFixtureBlobs(t, pool, rdb)
	mediaID, _ := uploadHostMedia(t, f, f.ownerID, f.eventID)

	del := func(id uuid.UUID) *httptest.ResponseRecorder {
		req := requestAs(http.MethodDelete, "/", f.ownerID)
		req = withRouteParam(withRouteParam(req, "id", f.eventID.String()), "mediaID", id.String())
		rec := httptest.NewRecorder()
		f.s.handleDeleteEventMedia(rec, req)
		return rec
	}

	t.Run("unknown media is 404 without a storage call", func(t *testing.T) {
		before := blobs.calls()
		if rec := del(uuid.Must(uuid.NewV7())); rec.Code != http.StatusNotFound {
			t.Fatalf("status = %d, want 404", rec.Code)
		}
		if blobs.calls() != before {
			t.Errorf("storage was called for a media id that does not exist")
		}
	})

	t.Run("storage failure is 503 and keeps the row", func(t *testing.T) {
		blobs.deleteErr = fmt.Errorf("delete: %w", media.ErrStorageUnavailable)
		rec := del(mediaID)
		blobs.deleteErr = nil
		if rec.Code != http.StatusServiceUnavailable || !strings.Contains(rec.Body.String(), `"storage_unavailable"`) {
			t.Fatalf("status = %d, body = %s, want 503 storage_unavailable", rec.Code, rec.Body.String())
		}
		if n := mediaRowCount(t, pool, f.eventID); n != 1 {
			t.Fatalf("media rows = %d, want the row kept so the delete can be retried", n)
		}
	})

	t.Run("retry succeeds and removes objects and row", func(t *testing.T) {
		if rec := del(mediaID); rec.Code != http.StatusNoContent {
			t.Fatalf("status = %d, body = %s, want 204", rec.Code, rec.Body.String())
		}
		if n := mediaRowCount(t, pool, f.eventID); n != 0 {
			t.Errorf("media rows = %d, want 0", n)
		}
		if n := objectCount(t, blobs, f.eventID); n != 0 {
			t.Errorf("%d objects left, want 0", n)
		}
	})
}

func TestSetMediaModeration_RejectEnqueuesJobAndSurvivesStorageFailure(t *testing.T) {
	pool := dbTestPool(t)
	rdb := dbTestRedis(t)
	f, blobs := newMediaTestFixtureBlobs(t, pool, rdb)
	ctx := context.Background()
	publishTestEvent(t, f)

	newPending := func(t *testing.T) uuid.UUID {
		t.Helper()
		id := uuid.Must(uuid.NewV7())
		outDir, result := processTestUpload(t, f.s, testJPEG(t))
		if err := f.s.media.Commit(ctx, outDir, f.eventID, id); err != nil {
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
	moderate := func(h func(http.ResponseWriter, *http.Request), id uuid.UUID) *httptest.ResponseRecorder {
		req := requestAs(http.MethodPost, "/", f.ownerID)
		req = withRouteParam(withRouteParam(req, "id", f.eventID.String()), "mediaID", id.String())
		rec := httptest.NewRecorder()
		h(rec, req)
		return rec
	}
	jobCount := func() int {
		var n int
		if err := pool.QueryRow(ctx, "SELECT count(*) FROM river_job WHERE kind = 'media_visibility' AND args->>'event_id' = $1", f.eventID.String()).Scan(&n); err != nil {
			t.Fatalf("count jobs: %v", err)
		}
		return n
	}

	t.Run("approve is DB only", func(t *testing.T) {
		id := newPending(t)
		before, jobs := blobs.calls(), jobCount()
		if rec := moderate(f.s.handleApproveMedia, id); rec.Code != http.StatusOK {
			t.Fatalf("status = %d, body = %s", rec.Code, rec.Body.String())
		}
		if blobs.calls() != before || jobCount() != jobs {
			t.Errorf("approve touched storage or enqueued a job")
		}
	})

	t.Run("reject with failing storage is still 200 and the job is enqueued", func(t *testing.T) {
		id := newPending(t)
		jobs := jobCount()
		blobs.deleteErr = fmt.Errorf("delete: %w", media.ErrStorageUnavailable)
		rec := moderate(f.s.handleRejectMedia, id)
		blobs.deleteErr = nil
		if rec.Code != http.StatusOK || !strings.Contains(rec.Body.String(), `"moderation_status":"rejected"`) {
			t.Fatalf("status = %d, body = %s, want 200 rejected", rec.Code, rec.Body.String())
		}
		if jobCount() != jobs+1 {
			t.Errorf("media_visibility jobs = %d, want %d", jobCount(), jobs+1)
		}
		if _, err := f.s.media.Open(ctx, f.eventID, id, 480, ""); err != nil {
			t.Errorf("objects should remain for the job to delete, got %v", err)
		}
	})
}

// TestUpload_JunkDoesNotChargeWriteBudget: the daily write budget is spent
// only by uploads that passed validation and processing, so junk requests
// cannot drain it for everyone.
func TestUpload_JunkDoesNotChargeWriteBudget(t *testing.T) {
	pool := dbTestPool(t)
	rdb := dbTestRedis(t)
	f, _ := newMediaTestFixtureBlobs(t, pool, rdb)
	counter := func() int64 {
		n, _ := rdb.Get(context.Background(), "rl:r2:writes:day").Int64()
		return n
	}

	tests := []struct {
		name        string
		body        []byte
		contentType string
		wantStatus  int
	}{
		{"wrong content type", testJPEG(t), "text/plain", http.StatusUnsupportedMediaType},
		{"no content type", testJPEG(t), "", http.StatusUnsupportedMediaType},
		{"not an image", []byte("definitely not an image"), "image/jpeg", http.StatusUnsupportedMediaType},
		{"empty body", nil, "image/jpeg", http.StatusUnsupportedMediaType},
		{"truncated jpeg", testJPEG(t)[:64], "image/jpeg", http.StatusUnprocessableEntity},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			before := counter()
			req := uploadRequest(f.ownerID, f.eventID, tt.body, nextTestIP())
			req.Header.Set("Content-Type", tt.contentType)
			rec := httptest.NewRecorder()
			f.s.handleUploadEventMedia(rec, req)
			if rec.Code != tt.wantStatus {
				t.Fatalf("status = %d, want %d; body = %s", rec.Code, tt.wantStatus, rec.Body.String())
			}
			if after := counter(); after != before {
				t.Errorf("write budget moved from %d to %d on a rejected upload", before, after)
			}
		})
	}
}

// Several events of one owner, each under the per-event allowance, together
// over the owner's pending allowance: refused, regardless of the personal cap.
func TestUploadGuest_PendingAllowancePerOwner(t *testing.T) {
	pool := dbTestPool(t)
	rdb := dbTestRedis(t)
	f := newMediaTestFixture(t, pool, rdb)
	ctx := context.Background()
	setPendingGuestCap(t, 1000)
	// The owner allowance follows the personal cap; make it small and the
	// personal cap's own headroom irrelevant to the pending check.
	f.s.cfg.MediaUserQuotaBytes = 1500

	events := []uuid.UUID{f.eventID}
	for range 2 {
		ev := createTestEvent(t, ctx, f.s.q, "birthday", "classic", &f.ownerID)
		t.Cleanup(func() { _, _ = pool.Exec(context.Background(), "DELETE FROM events WHERE id = $1", ev.ID) })
		events = append(events, ev.ID)
	}
	other := newMediaTestFixture(t, pool, rdb)
	otherEvent := createTestEvent(t, ctx, f.s.q, "birthday", "classic", &other.ownerID)
	t.Cleanup(func() { _, _ = pool.Exec(context.Background(), "DELETE FROM events WHERE id = $1", otherEvent.ID) })

	var firstPending uuid.UUID
	for i, id := range events[:2] {
		p := seedPendingGuest(t, pool, id, 700)
		if i == 0 {
			firstPending = p
		}
	}
	// 1400 pending across two events; each event is below 1000.
	third := events[2]
	if err := txCheck(t, f.s, &f.ownerID, &third, 100); err != nil {
		t.Errorf("1500 pending in total: %v, want nil (exactly at the allowance)", err)
	}
	if err := txCheck(t, f.s, &f.ownerID, &third, 101); !errors.Is(err, errGuestPendingFull) {
		t.Errorf("1501 pending in total: %v, want errGuestPendingFull", err)
	}
	// The pre-check is per-event only (no owner passed).
	if err := txCheck(t, f.s, nil, &third, 101); err != nil {
		t.Errorf("per-event check alone: %v, want nil", err)
	}
	// Another owner is unaffected.
	if err := txCheck(t, f.s, &other.ownerID, &otherEvent.ID, 900); err != nil {
		t.Errorf("different owner: %v, want nil", err)
	}
	// The allowance is independent of the personal cap: pending bytes are not
	// charged to it and a full personal cap does not shrink the allowance.
	seedHostMedia(t, f.s, f.ownerID, f.eventID, 1500)
	if err := txCheck(t, f.s, &f.ownerID, &third, 100); err != nil {
		t.Errorf("owner at personal cap: %v, want nil", err)
	}
	if err := txCheck(t, f.s, &f.ownerID, nil, 1); !errors.Is(err, errStorageQuota) {
		t.Errorf("host upload at personal cap: %v, want errStorageQuota", err)
	}
	// Approving (or rejecting) a pending photo frees allowance.
	if _, err := pool.Exec(ctx, "UPDATE media SET moderation_status = 'approved' WHERE id = $1", firstPending); err != nil {
		t.Fatalf("approve: %v", err)
	}
	if err := txCheck(t, f.s, &f.ownerID, &third, 800); err != nil {
		t.Errorf("after approve: %v, want nil", err)
	}
	// An event without an owner skips the per-owner check.
	if err := txCheck(t, f.s, nil, &third, 900); err != nil {
		t.Errorf("nil owner: %v, want nil", err)
	}
}

// End to end: the guest upload handler refuses with photo_limit once the
// owner's other events hold the allowance.
func TestUploadGuest_OwnerPendingAllowanceHTTP(t *testing.T) {
	pool := dbTestPool(t)
	rdb := dbTestRedis(t)
	f := newPublicEventFixture(t, pool, rdb, publicEventOpts{photosOpen: true})
	f.useCountingBlobs(t)
	ctx := context.Background()
	ev := createTestEvent(t, ctx, f.s.q, "birthday", "classic", &f.ownerID)
	t.Cleanup(func() { _, _ = pool.Exec(context.Background(), "DELETE FROM events WHERE id = $1", ev.ID) })
	seedPendingGuest(t, pool, ev.ID, 5000)
	f.s.cfg.MediaUserQuotaBytes = 5000 // allowance fully used by the other event

	rec := guestUpload(f, testJPEG(t))
	if rec.Code != http.StatusConflict || !strings.Contains(rec.Body.String(), `"photo_limit"`) {
		t.Fatalf("status = %d, body = %s, want 409 photo_limit", rec.Code, rec.Body.String())
	}
	if mediaRowCount(t, pool, f.eventID) != 0 {
		t.Errorf("refused upload left a media row")
	}
}

// Approving an unknown or foreign media id answers 404 without waiting for
// the global quota lock, which another transaction holds here.
func TestSetMediaModeration_ApproveNotFoundDoesNotTakeQuotaLock(t *testing.T) {
	pool := dbTestPool(t)
	rdb := dbTestRedis(t)
	f := newMediaTestFixture(t, pool, rdb)
	ctx := context.Background()

	holder, err := pool.Begin(ctx)
	if err != nil {
		t.Fatalf("begin: %v", err)
	}
	t.Cleanup(func() { _ = holder.Rollback(context.Background()) })
	if err := store.New(holder).LockMediaStorageQuota(ctx); err != nil {
		t.Fatalf("hold quota lock: %v", err)
	}

	foreign := createTestEvent(t, ctx, f.s.q, "birthday", "classic", &f.strangerID)
	t.Cleanup(func() { _, _ = pool.Exec(context.Background(), "DELETE FROM events WHERE id = $1", foreign.ID) })
	foreignMedia := seedPendingGuest(t, pool, foreign.ID, 10)

	for name, mediaID := range map[string]uuid.UUID{
		"nonexistent": uuid.Must(uuid.NewV7()),
		"foreign":     foreignMedia,
	} {
		t.Run(name, func(t *testing.T) {
			done := make(chan int, 1)
			go func() {
				req := requestAs(http.MethodPost, "/", f.ownerID)
				req = withRouteParam(withRouteParam(req, "id", f.eventID.String()), "mediaID", mediaID.String())
				rec := httptest.NewRecorder()
				f.s.handleApproveMedia(rec, req)
				done <- rec.Code
			}()
			select {
			case code := <-done:
				if code != http.StatusNotFound {
					t.Errorf("status = %d, want 404", code)
				}
			case <-time.After(3 * time.Second):
				t.Fatal("approve of a missing media id blocked on the quota lock")
			}
		})
	}
}
