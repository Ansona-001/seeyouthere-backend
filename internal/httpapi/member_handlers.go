package httpapi

import (
	"errors"
	"fmt"
	"net/http"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"

	"github.com/ansonarose/seeyouthere-backend/internal/jobs"
	"github.com/ansonarose/seeyouthere-backend/internal/store"
)

// maxEventMembers is the co-host cap (§4.6), checked against event_members
// alone (the owner isn't a member row). Enforced under LockEventForEditor so
// concurrent adds can't both squeeze past it (§14: members is one of the
// exact per-event caps).
const maxEventMembers = 10

var (
	errMemberLimit   = errors.New("member limit exceeded")
	errCannotAddSelf = errors.New("cannot add self as a member")
)

// --- response shape (§4.6) ---

type memberResp struct {
	UserID uuid.UUID `json:"user_id"`
	Email  string    `json:"email"`
	Name   string    `json:"name"`
	Role   string    `json:"role"`
}

// --- GET /v1/events/{id}/members ---

func (s *Server) handleListMembers(w http.ResponseWriter, r *http.Request) {
	eventID, err := parseUUIDParam(r, "id")
	if err != nil {
		writeError(w, http.StatusNotFound, "not_found", "No such event.")
		return
	}
	userID, ok := userIDFrom(r.Context())
	if !ok {
		writeError(w, http.StatusUnauthorized, "unauthorized", "Log in to continue.")
		return
	}

	rows, err := s.q.ListEventMembers(r.Context(), store.ListEventMembersParams{EventID: eventID, UserID: userID})
	if err != nil {
		serverError(w, r, fmt.Errorf("list event members: %w", err))
		return
	}
	// The query's outer WHERE filters every row (including the always-present
	// owner row) by event_role(...) IS NOT NULL, so an empty result means the
	// caller has no access at all (a non-member, or the event doesn't exist),
	// not "an event with zero co-hosts" (the owner row would still show up
	// for anyone who does have access).
	if len(rows) == 0 {
		writeError(w, http.StatusNotFound, "not_found", "No such event.")
		return
	}

	members := make([]memberResp, len(rows))
	for i, m := range rows {
		members[i] = memberResp{UserID: m.UserID, Email: m.Email, Name: m.Name, Role: m.Role}
	}
	writeJSON(w, http.StatusOK, map[string]any{"members": members})
}

// --- POST /v1/events/{id}/members ---

// handleAddMember adds or re-invites a co-host by email. The response is
// {"ok":true} on every success path regardless of whether the address
// already had an account (UpsertUserByEmail creates it transparently), so
// this endpoint never lets a caller learn whether an address is registered
// (§12 enumeration).
func (s *Server) handleAddMember(w http.ResponseWriter, r *http.Request) {
	eventID, err := parseUUIDParam(r, "id")
	if err != nil {
		writeError(w, http.StatusNotFound, "not_found", "No such event.")
		return
	}
	ctx := r.Context()
	userID, ok := userIDFrom(ctx)
	if !ok {
		writeError(w, http.StatusUnauthorized, "unauthorized", "Log in to continue.")
		return
	}
	if !s.allow(w, r, limit{"members:add:" + userID.String(), 20, 24 * time.Hour}) {
		return
	}

	var body struct {
		Email string `json:"email"`
		Role  string `json:"role"`
	}
	if !decodeJSON(w, r, &body) {
		return
	}
	email, ok := normalizeEmail(body.Email)
	if !ok {
		writeError(w, http.StatusBadRequest, "invalid_email", "Enter a valid email address.")
		return
	}
	if body.Role != "editor" && body.Role != "viewer" {
		writeError(w, http.StatusBadRequest, "validation_failed", "role must be editor or viewer.")
		return
	}

	row, ok, err := s.loadMemberEvent(ctx, userID, eventID)
	if err != nil {
		serverError(w, r, err)
		return
	}
	if !ok {
		writeError(w, http.StatusNotFound, "not_found", "No such event.")
		return
	}
	if row.Role != "owner" {
		writeError(w, http.StatusForbidden, "forbidden", "Only the owner can manage co-hosts.")
		return
	}

	caller, err := s.q.GetUserByID(ctx, userID)
	if err != nil {
		serverError(w, r, fmt.Errorf("get user: %w", err))
		return
	}
	if email == caller.Email {
		writeError(w, http.StatusBadRequest, "cannot_add_self", "You can't add yourself as a co-host.")
		return
	}

	err = s.inTx(ctx, func(tx pgx.Tx, q *store.Queries) error {
		if _, err := q.LockEventForEditor(ctx, store.LockEventForEditorParams{EventID: eventID, UserID: userID}); err != nil {
			return err
		}

		target, err := q.UpsertUserByEmail(ctx, store.UpsertUserByEmailParams{ID: uuid.Must(uuid.NewV7()), Email: email})
		if err != nil {
			return fmt.Errorf("upsert user: %w", err)
		}
		if target.ID == userID {
			// The owner's own address changed case/whitespace only, or a
			// race with the pre-check above; treat it the same either way.
			return errCannotAddSelf
		}

		existing, err := q.ListEventMembers(ctx, store.ListEventMembersParams{EventID: eventID, UserID: userID})
		if err != nil {
			return fmt.Errorf("list event members: %w", err)
		}
		alreadyMember := false
		for _, m := range existing {
			if m.UserID == target.ID {
				alreadyMember = true
				break
			}
		}
		if !alreadyMember {
			count, err := q.CountEventMembers(ctx, eventID)
			if err != nil {
				return fmt.Errorf("count event members: %w", err)
			}
			if count >= maxEventMembers {
				return errMemberLimit
			}
		}

		inserted, err := q.UpsertEventMember(ctx, store.UpsertEventMemberParams{
			MemberID: target.ID, Role: body.Role, EventID: eventID, OwnerID: userID,
		})
		if errors.Is(err, pgx.ErrNoRows) {
			// The owner check or self-exclusion in UpsertEventMember's own
			// WHERE failed: the event was deleted, or the owner changed,
			// between the checks above and here.
			return pgx.ErrNoRows
		}
		if err != nil {
			return fmt.Errorf("upsert event member: %w", err)
		}

		// Only a fresh add gets an email; re-adding or changing the role of
		// an existing member must not re-notify them every time.
		if inserted {
			if _, err := s.jobs.InsertTx(ctx, tx, jobs.NotifyCohostAddedArgs{EventID: eventID, UserID: target.ID}, nil); err != nil {
				return fmt.Errorf("enqueue notify_cohost_added: %w", err)
			}
		}
		return nil
	})
	switch {
	case errors.Is(err, errMemberLimit):
		writeError(w, http.StatusConflict, "member_limit", "This event already has the maximum number of co-hosts.")
		return
	case errors.Is(err, errCannotAddSelf):
		writeError(w, http.StatusBadRequest, "cannot_add_self", "You can't add yourself as a co-host.")
		return
	case errors.Is(err, pgx.ErrNoRows):
		writeError(w, http.StatusNotFound, "not_found", "No such event.")
		return
	case err != nil:
		serverError(w, r, fmt.Errorf("add member: %w", err))
		return
	}
	writeJSON(w, http.StatusCreated, map[string]any{"ok": true})
}

// --- PATCH /v1/events/{id}/members/{userID} ---

func (s *Server) handleUpdateMemberRole(w http.ResponseWriter, r *http.Request) {
	eventID, err := parseUUIDParam(r, "id")
	if err != nil {
		writeError(w, http.StatusNotFound, "not_found", "No such event.")
		return
	}
	memberID, err := parseUUIDParam(r, "userID")
	if err != nil {
		writeError(w, http.StatusNotFound, "not_found", "No such member.")
		return
	}
	ctx := r.Context()
	userID, ok := userIDFrom(ctx)
	if !ok {
		writeError(w, http.StatusUnauthorized, "unauthorized", "Log in to continue.")
		return
	}

	var body struct {
		Role string `json:"role"`
	}
	if !decodeJSON(w, r, &body) {
		return
	}
	if body.Role != "editor" && body.Role != "viewer" {
		writeError(w, http.StatusBadRequest, "validation_failed", "role must be editor or viewer.")
		return
	}

	row, ok, err := s.loadMemberEvent(ctx, userID, eventID)
	if err != nil {
		serverError(w, r, err)
		return
	}
	if !ok {
		writeError(w, http.StatusNotFound, "not_found", "No such event.")
		return
	}
	if row.Role != "owner" {
		writeError(w, http.StatusForbidden, "forbidden", "Only the owner can manage co-hosts.")
		return
	}

	n, err := s.q.UpdateEventMemberRole(ctx, store.UpdateEventMemberRoleParams{
		Role: body.Role, EventID: eventID, MemberID: memberID, OwnerID: userID,
	})
	if err != nil {
		serverError(w, r, fmt.Errorf("update member role: %w", err))
		return
	}
	if n == 0 {
		writeError(w, http.StatusNotFound, "not_found", "No such member.")
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"ok": true})
}

// --- DELETE /v1/events/{id}/members/{userID} ---

// handleDeleteMember covers both the owner removing anyone and a member
// removing themself (leave): DeleteEventMember's own WHERE clause encodes
// both cases, so no separate role check is needed here.
func (s *Server) handleDeleteMember(w http.ResponseWriter, r *http.Request) {
	eventID, err := parseUUIDParam(r, "id")
	if err != nil {
		writeError(w, http.StatusNotFound, "not_found", "No such member.")
		return
	}
	memberID, err := parseUUIDParam(r, "userID")
	if err != nil {
		writeError(w, http.StatusNotFound, "not_found", "No such member.")
		return
	}
	userID, ok := userIDFrom(r.Context())
	if !ok {
		writeError(w, http.StatusUnauthorized, "unauthorized", "Log in to continue.")
		return
	}

	n, err := s.q.DeleteEventMember(r.Context(), store.DeleteEventMemberParams{EventID: eventID, MemberID: memberID, UserID: userID})
	if err != nil {
		serverError(w, r, fmt.Errorf("delete event member: %w", err))
		return
	}
	if n == 0 {
		writeError(w, http.StatusNotFound, "not_found", "No such member.")
		return
	}
	w.WriteHeader(http.StatusNoContent)
}
