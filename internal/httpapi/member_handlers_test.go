package httpapi

import (
	"context"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/google/uuid"

	"github.com/ansonarose/seeyouthere-backend/internal/store"
)

func TestHandleListMembers_IDORMatrix(t *testing.T) {
	pool := dbTestPool(t)
	rdb := dbTestRedis(t)
	f := newEventTestFixture(t, pool, rdb)

	cases := []struct {
		name       string
		userID     uuid.UUID
		wantStatus int
	}{
		{"owner", f.ownerID, http.StatusOK},
		{"editor", f.editorID, http.StatusOK},
		{"viewer", f.viewerID, http.StatusOK},
		{"stranger", f.strangerID, http.StatusNotFound},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			req := httptest.NewRequest(http.MethodGet, "/", nil)
			req = req.WithContext(contextWithUser(req.Context(), c.userID))
			req = withRouteID(req, f.eventID)
			rec := httptest.NewRecorder()
			f.s.handleListMembers(rec, req)
			if rec.Code != c.wantStatus {
				t.Fatalf("status = %d, want %d; body = %s", rec.Code, c.wantStatus, rec.Body.String())
			}
			if c.wantStatus == http.StatusOK && !containsAll(rec.Body.String(), `"role":"owner"`) {
				t.Errorf("body = %s, want the owner listed", rec.Body.String())
			}
		})
	}
}

func TestHandleAddMember_EnumerationSafeSelfAndLimit(t *testing.T) {
	pool := dbTestPool(t)
	rdb := dbTestRedis(t)
	f := newEventTestFixture(t, pool, rdb)
	withGuestFixtureJobs(t, f, pool)
	ctx := context.Background()

	owner, err := f.s.q.GetUserByID(ctx, f.ownerID)
	if err != nil {
		t.Fatalf("get owner: %v", err)
	}

	t.Run("non-owner forbidden", func(t *testing.T) {
		req := httptest.NewRequest(http.MethodPost, "/", newJSONBody(`{"email":"nobody@example.invalid","role":"viewer"}`))
		req.Header.Set("Content-Type", "application/json")
		req = req.WithContext(contextWithUser(req.Context(), f.editorID))
		req = withRouteID(req, f.eventID)
		rec := httptest.NewRecorder()
		f.s.handleAddMember(rec, req)
		if rec.Code != http.StatusForbidden {
			t.Fatalf("status = %d, want 403; body = %s", rec.Code, rec.Body.String())
		}
	})

	t.Run("cannot add self", func(t *testing.T) {
		req := httptest.NewRequest(http.MethodPost, "/", newJSONBody(`{"email":"`+owner.Email+`","role":"editor"}`))
		req.Header.Set("Content-Type", "application/json")
		req = req.WithContext(contextWithUser(req.Context(), f.ownerID))
		req = withRouteID(req, f.eventID)
		rec := httptest.NewRecorder()
		f.s.handleAddMember(rec, req)
		if rec.Code != http.StatusBadRequest || !containsAll(rec.Body.String(), `"cannot_add_self"`) {
			t.Fatalf("status = %d, body = %s, want 400 cannot_add_self", rec.Code, rec.Body.String())
		}
	})

	// The response must be identical whether the address already has an
	// account (the fixture's stranger) or is brand new: both succeed with
	// {"ok":true} and neither reveals which case it was.
	newEmail := "brandnew-" + f.eventID.String()[:8] + "@example.invalid"
	strangerRow, err := f.s.q.GetUserByID(ctx, f.strangerID)
	if err != nil {
		t.Fatalf("get stranger: %v", err)
	}

	var bodies []string
	for _, email := range []string{newEmail, strangerRow.Email} {
		t.Run("add "+email, func(t *testing.T) {
			req := httptest.NewRequest(http.MethodPost, "/", newJSONBody(`{"email":"`+email+`","role":"viewer"}`))
			req.Header.Set("Content-Type", "application/json")
			req = req.WithContext(contextWithUser(req.Context(), f.ownerID))
			req = withRouteID(req, f.eventID)
			rec := httptest.NewRecorder()
			f.s.handleAddMember(rec, req)
			if rec.Code != http.StatusCreated {
				t.Fatalf("status = %d, want 201; body = %s", rec.Code, rec.Body.String())
			}
			bodies = append(bodies, rec.Body.String())
		})
	}
	if len(bodies) == 2 && bodies[0] != bodies[1] {
		t.Errorf("responses differ by account existence: %q vs %q", bodies[0], bodies[1])
	}
	t.Cleanup(func() {
		pool.Exec(context.Background(), "DELETE FROM users WHERE email = $1", newEmail)
	})

	t.Run("member limit", func(t *testing.T) {
		// The fixture already has 1 co-host (editorID as editor) plus the
		// viewer added just above (2), plus the stranger just added (3).
		// Fill up to the cap with fresh accounts, then confirm the next one
		// is rejected.
		var fillEmails []string
		for len(fillEmails) < maxEventMembers {
			count, err := f.s.q.CountEventMembers(ctx, f.eventID)
			if err != nil {
				t.Fatalf("count members: %v", err)
			}
			if count >= maxEventMembers {
				break
			}
			email := "fill-" + uuid.Must(uuid.NewV7()).String() + "@example.invalid"
			fillEmails = append(fillEmails, email)
			req := httptest.NewRequest(http.MethodPost, "/", newJSONBody(`{"email":"`+email+`","role":"viewer"}`))
			req.Header.Set("Content-Type", "application/json")
			req = req.WithContext(contextWithUser(req.Context(), f.ownerID))
			req = withRouteID(req, f.eventID)
			rec := httptest.NewRecorder()
			f.s.handleAddMember(rec, req)
			if rec.Code != http.StatusCreated {
				t.Fatalf("fill status = %d, want 201; body = %s", rec.Code, rec.Body.String())
			}
		}
		t.Cleanup(func() {
			for _, email := range fillEmails {
				pool.Exec(context.Background(), "DELETE FROM users WHERE email = $1", email)
			}
		})

		overflowEmail := "overflow-" + f.eventID.String()[:8] + "@example.invalid"
		req := httptest.NewRequest(http.MethodPost, "/", newJSONBody(`{"email":"`+overflowEmail+`","role":"viewer"}`))
		req.Header.Set("Content-Type", "application/json")
		req = req.WithContext(contextWithUser(req.Context(), f.ownerID))
		req = withRouteID(req, f.eventID)
		rec := httptest.NewRecorder()
		f.s.handleAddMember(rec, req)
		if rec.Code != http.StatusConflict || !containsAll(rec.Body.String(), `"member_limit"`) {
			t.Fatalf("status = %d, body = %s, want 409 member_limit", rec.Code, rec.Body.String())
		}
		pool.Exec(context.Background(), "DELETE FROM users WHERE email = $1", overflowEmail)
	})
}

func TestHandleUpdateMemberRole_OwnerOnly(t *testing.T) {
	pool := dbTestPool(t)
	rdb := dbTestRedis(t)
	f := newEventTestFixture(t, pool, rdb)

	t.Run("non-owner forbidden", func(t *testing.T) {
		req := httptest.NewRequest(http.MethodPatch, "/", newJSONBody(`{"role":"viewer"}`))
		req.Header.Set("Content-Type", "application/json")
		req = req.WithContext(contextWithUser(req.Context(), f.editorID))
		req = withRouteID(req, f.eventID)
		req = withRouteParam(req, "userID", f.viewerID.String())
		rec := httptest.NewRecorder()
		f.s.handleUpdateMemberRole(rec, req)
		if rec.Code != http.StatusForbidden {
			t.Fatalf("status = %d, want 403; body = %s", rec.Code, rec.Body.String())
		}
	})

	t.Run("owner promotes viewer to editor", func(t *testing.T) {
		req := httptest.NewRequest(http.MethodPatch, "/", newJSONBody(`{"role":"editor"}`))
		req.Header.Set("Content-Type", "application/json")
		req = req.WithContext(contextWithUser(req.Context(), f.ownerID))
		req = withRouteID(req, f.eventID)
		req = withRouteParam(req, "userID", f.viewerID.String())
		rec := httptest.NewRecorder()
		f.s.handleUpdateMemberRole(rec, req)
		if rec.Code != http.StatusOK {
			t.Fatalf("status = %d, want 200; body = %s", rec.Code, rec.Body.String())
		}

		rows, err := f.s.q.ListEventMembers(context.Background(), store.ListEventMembersParams{EventID: f.eventID, UserID: f.ownerID})
		if err != nil {
			t.Fatalf("list members: %v", err)
		}
		found := false
		for _, m := range rows {
			if m.UserID == f.viewerID && m.Role == "editor" {
				found = true
			}
		}
		if !found {
			t.Errorf("viewer role not updated to editor: %+v", rows)
		}
	})

	t.Run("unknown member not found", func(t *testing.T) {
		req := httptest.NewRequest(http.MethodPatch, "/", newJSONBody(`{"role":"viewer"}`))
		req.Header.Set("Content-Type", "application/json")
		req = req.WithContext(contextWithUser(req.Context(), f.ownerID))
		req = withRouteID(req, f.eventID)
		req = withRouteParam(req, "userID", f.strangerID.String())
		rec := httptest.NewRecorder()
		f.s.handleUpdateMemberRole(rec, req)
		if rec.Code != http.StatusNotFound {
			t.Fatalf("status = %d, want 404; body = %s", rec.Code, rec.Body.String())
		}
	})
}

func TestHandleDeleteMember_OwnerOrSelf(t *testing.T) {
	pool := dbTestPool(t)
	rdb := dbTestRedis(t)

	t.Run("owner removes editor", func(t *testing.T) {
		f := newEventTestFixture(t, pool, rdb)
		req := httptest.NewRequest(http.MethodDelete, "/", nil)
		req = req.WithContext(contextWithUser(req.Context(), f.ownerID))
		req = withRouteID(req, f.eventID)
		req = withRouteParam(req, "userID", f.editorID.String())
		rec := httptest.NewRecorder()
		f.s.handleDeleteMember(rec, req)
		if rec.Code != http.StatusNoContent {
			t.Fatalf("status = %d, want 204; body = %s", rec.Code, rec.Body.String())
		}
	})

	t.Run("member removes self", func(t *testing.T) {
		f := newEventTestFixture(t, pool, rdb)
		req := httptest.NewRequest(http.MethodDelete, "/", nil)
		req = req.WithContext(contextWithUser(req.Context(), f.viewerID))
		req = withRouteID(req, f.eventID)
		req = withRouteParam(req, "userID", f.viewerID.String())
		rec := httptest.NewRecorder()
		f.s.handleDeleteMember(rec, req)
		if rec.Code != http.StatusNoContent {
			t.Fatalf("status = %d, want 204; body = %s", rec.Code, rec.Body.String())
		}
	})

	t.Run("stranger cannot remove another member", func(t *testing.T) {
		f := newEventTestFixture(t, pool, rdb)
		req := httptest.NewRequest(http.MethodDelete, "/", nil)
		req = req.WithContext(contextWithUser(req.Context(), f.strangerID))
		req = withRouteID(req, f.eventID)
		req = withRouteParam(req, "userID", f.viewerID.String())
		rec := httptest.NewRecorder()
		f.s.handleDeleteMember(rec, req)
		if rec.Code != http.StatusNotFound {
			t.Fatalf("status = %d, want 404; body = %s", rec.Code, rec.Body.String())
		}
	})
}
