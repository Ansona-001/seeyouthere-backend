package httpapi

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"

	"github.com/ansonarose/seeyouthere-backend/internal/store"
)

// A host must not give an email that already registered a second pass as a guest.

func TestGuestHandlers_RegisteredEmailConflict(t *testing.T) {
	h := newHostedFixture(t)
	reg := h.addRegistration(t, h.eventID, "Reg One", "yes", 1)
	other := "other-" + uuid.Must(uuid.NewV7()).String() + "@example.invalid"
	guest := h.addGuest(t, h.eventID, "Plain Guest", 1)

	t.Run("create rejects a registered email", func(t *testing.T) {
		req := newGuestBody(t, "Dup", strings.ToUpper(reg.Email), "", 1)
		req = req.WithContext(contextWithUser(req.Context(), h.ownerID))
		req = withRouteID(req, h.eventID)
		rec := httptest.NewRecorder()
		h.s.handleCreateGuest(rec, req)
		wantStatusCode(t, rec, http.StatusConflict, "already_registered")
	})

	t.Run("create allows an unregistered email", func(t *testing.T) {
		req := newGuestBody(t, "Fine", other, "", 1)
		req = req.WithContext(contextWithUser(req.Context(), h.ownerID))
		req = withRouteID(req, h.eventID)
		rec := httptest.NewRecorder()
		h.s.handleCreateGuest(rec, req)
		if rec.Code != http.StatusCreated {
			t.Fatalf("status = %d, want 201; body = %s", rec.Code, rec.Body.String())
		}
	})

	patch := func(email string) *httptest.ResponseRecorder {
		req := httptest.NewRequest(http.MethodPatch, "/", newJSONBody(`{"email":`+jsonStringOrNull(email)+`}`))
		req.Header.Set("Content-Type", "application/json")
		req = req.WithContext(contextWithUser(req.Context(), h.ownerID))
		req = withRouteID(req, h.eventID)
		req = withRouteParam(req, "guestID", guest.ID.String())
		rec := httptest.NewRecorder()
		h.s.handleUpdateGuest(rec, req)
		return rec
	}

	t.Run("update rejects a registered email", func(t *testing.T) {
		wantStatusCode(t, patch(reg.Email), http.StatusConflict, "already_registered")
	})
	t.Run("update to a free email and clearing still work", func(t *testing.T) {
		if rec := patch("free-" + other); rec.Code != http.StatusOK {
			t.Fatalf("status = %d, want 200; body = %s", rec.Code, rec.Body.String())
		}
		if rec := patch(""); rec.Code != http.StatusOK {
			t.Fatalf("clear status = %d, want 200; body = %s", rec.Code, rec.Body.String())
		}
	})

	t.Run("import skips registered emails and reports them as skipped", func(t *testing.T) {
		free := "imp-" + other
		csv := "name,email\nRegistered," + reg.Email + "\nFree," + free + "\nNo Email,\n"
		req := httptest.NewRequest(http.MethodPost, "/", newJSONBody(csv))
		req.Header.Set("Content-Type", "text/csv")
		req = req.WithContext(contextWithUser(req.Context(), h.ownerID))
		req = withRouteID(req, h.eventID)
		rec := httptest.NewRecorder()
		h.s.handleImportGuests(rec, req)
		if rec.Code != http.StatusOK {
			t.Fatalf("status = %d, want 200; body = %s", rec.Code, rec.Body.String())
		}
		if !containsAll(rec.Body.String(), `"imported":2`, `"skipped_existing":1`) {
			t.Errorf("body = %s", rec.Body.String())
		}
		var n int
		if err := h.pool.QueryRow(context.Background(),
			"SELECT count(*) FROM guests WHERE event_id = $1 AND email = $2", h.eventID, reg.Email).Scan(&n); err != nil {
			t.Fatalf("count: %v", err)
		}
		if n != 0 {
			t.Errorf("guest rows for the registered email = %d, want 0", n)
		}
	})
}

// A concurrent door scan can check a registration in after the handler's pre-check; the UPDATE
// itself must then touch no row.
func TestUpdateOpenRSVP_NoRowOnceCheckedIn(t *testing.T) {
	h := newHostedFixture(t)
	reg := h.addRegistration(t, h.eventID, "Scanned", "yes", 2)
	params := store.UpdateOpenRSVPParams{
		Name: "Renamed", Email: &reg.Email, Attending: "no", Count: 0, Answers: []byte("{}"),
		ID: reg.ID, EventID: h.eventID, EditTokenVersion: reg.Version,
	}
	ctx := context.Background()

	if _, err := h.s.q.UpdateOpenRSVP(ctx, params); err != nil {
		t.Fatalf("update before check-in: %v", err)
	}
	h.exec(t, "UPDATE rsvps SET checked_in_at = now(), checked_in_count = 2 WHERE id = $1", reg.ID)
	if _, err := h.s.q.UpdateOpenRSVP(ctx, params); !errors.Is(err, pgx.ErrNoRows) {
		t.Fatalf("update after check-in err = %v, want pgx.ErrNoRows", err)
	}
	var name, attending string
	if err := h.pool.QueryRow(ctx, "SELECT name, attending FROM rsvps WHERE id = $1", reg.ID).Scan(&name, &attending); err != nil {
		t.Fatalf("read: %v", err)
	}
	if name != "Renamed" || attending != "no" {
		t.Errorf("checked-in row changed after check-in: name=%q attending=%q", name, attending)
	}
}

func TestExportRSVPsBatch_RoleEnforcedInSQL(t *testing.T) {
	h := newHostedFixture(t)
	h.addRegistration(t, h.eventID, "Exported", "yes", 1)
	ctx := context.Background()

	for _, tc := range []struct {
		name string
		user uuid.UUID
		want int
	}{
		{"owner", h.ownerID, 1},
		{"viewer", h.viewerID, 0},
		{"stranger", h.strangerID, 0},
	} {
		t.Run(tc.name, func(t *testing.T) {
			rows, err := h.s.q.ExportRSVPsBatch(ctx, store.ExportRSVPsBatchParams{
				EventID: h.eventID, UserID: tc.user, CursorCreatedAt: time.Time{}, CursorID: uuid.Nil, Lim: 10,
			})
			if err != nil {
				t.Fatalf("ExportRSVPsBatch: %v", err)
			}
			if len(rows) != tc.want {
				t.Errorf("rows = %d, want %d", len(rows), tc.want)
			}
		})
	}
}
