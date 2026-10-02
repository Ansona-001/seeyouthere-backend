package httpapi

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/google/uuid"

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
	if err := f.s.media.Commit(outDir, media.AreaPending, f.eventID, mediaID); err != nil {
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
