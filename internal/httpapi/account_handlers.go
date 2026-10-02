package httpapi

import (
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"strings"
	"time"
	"unicode"
	"unicode/utf8"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/riverqueue/river"

	"github.com/ansonarose/seeyouthere-backend/internal/jobs"
	"github.com/ansonarose/seeyouthere-backend/internal/store"
)

// validateUserName trims s and requires 1-80 runes with no control
// characters (users.users_name_len_check; §4.9 caps a display name at 80).
func validateUserName(s string) (string, bool) {
	s = strings.TrimSpace(s)
	n := utf8.RuneCountInString(s)
	if n < 1 || n > 80 {
		return "", false
	}
	for _, r := range s {
		if unicode.IsControl(r) {
			return "", false
		}
	}
	return s, true
}

// --- PATCH /v1/me ---

func (s *Server) handlePatchMe(w http.ResponseWriter, r *http.Request) {
	userID, _ := userIDFrom(r.Context())
	if !s.allow(w, r, limit{"me:patch:" + userID.String(), 30, time.Hour}) {
		return
	}

	var body struct {
		Name string `json:"name"`
	}
	if !decodeJSON(w, r, &body) {
		return
	}
	name, ok := validateUserName(body.Name)
	if !ok {
		writeError(w, http.StatusBadRequest, "validation_failed", "Name must be 1-80 characters with no control characters.")
		return
	}

	updated, err := s.q.UpdateUserName(r.Context(), store.UpdateUserNameParams{Name: name, UserID: userID})
	if errors.Is(err, pgx.ErrNoRows) {
		// The session was valid, but the user row is gone (deleted
		// concurrently); nothing left to update.
		writeError(w, http.StatusNotFound, "not_found", "No such account.")
		return
	}
	if err != nil {
		serverError(w, r, fmt.Errorf("update user name: %w", err))
		return
	}
	roles, err := s.q.ListUserRoles(r.Context(), userID)
	if err != nil {
		serverError(w, r, fmt.Errorf("list user roles: %w", err))
		return
	}
	verified, err := s.currentSessionMFAVerified(r.Context(), userID, roles)
	if err != nil {
		serverError(w, r, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"user": newUserResponse(updated, roles, verified)})
}

// --- GET /v1/me/sessions ---

type sessionResp struct {
	ID         uuid.UUID `json:"id"`
	CreatedAt  time.Time `json:"created_at"`
	LastSeenAt time.Time `json:"last_seen_at"`
	UserAgent  string    `json:"user_agent"`
	IP         string    `json:"ip"`
	Current    bool      `json:"current"`
}

func (s *Server) handleListSessions(w http.ResponseWriter, r *http.Request) {
	userID, _ := userIDFrom(r.Context())
	currentID, _ := sessionIDFrom(r.Context())

	rows, err := s.q.ListUserSessions(r.Context(), userID)
	if err != nil {
		serverError(w, r, fmt.Errorf("list user sessions: %w", err))
		return
	}
	sessions := make([]sessionResp, len(rows))
	for i, row := range rows {
		ip := ""
		if row.Ip != nil {
			ip = row.Ip.String()
		}
		sessions[i] = sessionResp{
			ID:         row.ID,
			CreatedAt:  row.CreatedAt,
			LastSeenAt: row.LastSeenAt,
			UserAgent:  row.UserAgent,
			IP:         ip,
			Current:    row.ID == currentID,
		}
	}
	writeJSON(w, http.StatusOK, map[string]any{"sessions": sessions})
}

// --- DELETE /v1/me/sessions/{id} ---

// handleDeleteSession revokes exactly one of the caller's own sessions:
// DeleteUserSession's WHERE clause is scoped to user_id = @user_id, so a
// stranger's session id always yields zero rows (404), never 403 (§12 IDOR).
func (s *Server) handleDeleteSession(w http.ResponseWriter, r *http.Request) {
	id, err := parseUUIDParam(r, "id")
	if err != nil {
		writeError(w, http.StatusNotFound, "not_found", "No such session.")
		return
	}
	userID, _ := userIDFrom(r.Context())

	hash, err := s.q.DeleteUserSession(r.Context(), store.DeleteUserSessionParams{ID: id, UserID: userID})
	if errors.Is(err, pgx.ErrNoRows) {
		writeError(w, http.StatusNotFound, "not_found", "No such session.")
		return
	}
	if err != nil {
		serverError(w, r, fmt.Errorf("delete user session: %w", err))
		return
	}
	// Postgres (source of truth) is already updated; a failure here only
	// means the Valkey mirror keeps answering for up to its 5-minute TTL
	// (§14), so it's logged, not surfaced as a failed revoke.
	if err := s.sessions.RevokeHashes(r.Context(), [][]byte{hash}); err != nil {
		slog.WarnContext(r.Context(), "revoke session cache", "err", err, "user_id", userID)
	}
	w.WriteHeader(http.StatusNoContent)
}

// --- POST /v1/me/sessions/revoke-others ---

func (s *Server) handleRevokeOtherSessions(w http.ResponseWriter, r *http.Request) {
	userID, _ := userIDFrom(r.Context())
	currentID, ok := sessionIDFrom(r.Context())
	if !ok {
		// requireUser + loadSession guarantee a session id whenever
		// userIDFrom succeeds; this would only trip on a wiring bug.
		serverError(w, r, fmt.Errorf("revoke-others: no session id in context for user %s", userID))
		return
	}

	hashes, err := s.q.DeleteOtherUserSessions(r.Context(), store.DeleteOtherUserSessionsParams{UserID: userID, CurrentID: currentID})
	if err != nil {
		serverError(w, r, fmt.Errorf("delete other user sessions: %w", err))
		return
	}
	if err := s.sessions.RevokeHashes(r.Context(), hashes); err != nil {
		slog.WarnContext(r.Context(), "revoke other sessions cache", "err", err, "user_id", userID)
	}
	writeJSON(w, http.StatusOK, map[string]any{"revoked": len(hashes)})
}

// --- DELETE /v1/me ---

// handleDeleteAccount scrubs the account's PII immediately and soft-deletes
// every event it owns (§4.9, §14: "account deletion"). Co-hosted events
// (this user holding only an event_members row) are left untouched: only
// the membership row is removed, never the event itself.
func (s *Server) handleDeleteAccount(w http.ResponseWriter, r *http.Request) {
	userID, _ := userIDFrom(r.Context())
	ctx := r.Context()

	if !s.allow(w, r, limit{"me:delete:" + userID.String(), 5, time.Hour}) {
		return
	}

	var body struct {
		ConfirmEmail string `json:"confirm_email"`
	}
	if !decodeJSON(w, r, &body) {
		return
	}

	user, err := s.q.GetUserByID(ctx, userID)
	if errors.Is(err, pgx.ErrNoRows) {
		// Already gone (e.g. a concurrent delete from another tab/device):
		// the caller's goal is already accomplished.
		s.clearSessionCookie(w)
		w.WriteHeader(http.StatusNoContent)
		return
	}
	if err != nil {
		serverError(w, r, fmt.Errorf("get user: %w", err))
		return
	}

	roles, err := s.q.ListUserRoles(ctx, userID)
	if err != nil {
		serverError(w, r, fmt.Errorf("list user roles: %w", err))
		return
	}
	if len(roles) > 0 {
		writeError(w, http.StatusConflict, "has_admin_role", "Remove your admin roles before deleting your account.")
		return
	}

	if body.ConfirmEmail != user.Email {
		writeError(w, http.StatusBadRequest, "confirm_mismatch", "That email doesn't match your account.")
		return
	}

	var sessionHashes [][]byte
	err = s.inTx(ctx, func(tx pgx.Tx, q *store.Queries) error {
		eventIDs, err := q.SoftDeleteEventsByOwner(ctx, userID)
		if err != nil {
			return fmt.Errorf("soft delete owned events: %w", err)
		}
		if err := q.DeleteMembershipsByUser(ctx, userID); err != nil {
			return fmt.Errorf("delete memberships: %w", err)
		}
		hashes, err := q.DeleteAllUserSessions(ctx, userID)
		if err != nil {
			return fmt.Errorf("delete sessions: %w", err)
		}
		sessionHashes = hashes
		if err := q.SoftDeleteUser(ctx, userID); err != nil {
			return fmt.Errorf("soft delete user: %w", err)
		}

		if len(eventIDs) > 0 {
			params := make([]river.InsertManyParams, len(eventIDs))
			for i, eventID := range eventIDs {
				params[i] = river.InsertManyParams{Args: jobs.MediaVisibilityArgs{EventID: eventID}}
			}
			if _, err := s.jobs.InsertManyTx(ctx, tx, params); err != nil {
				return fmt.Errorf("enqueue media_visibility: %w", err)
			}
		}
		return nil
	})
	if err != nil {
		serverError(w, r, fmt.Errorf("delete account: %w", err))
		return
	}

	// As with individual/other-session revocation: Postgres already
	// committed the deletion, so a Valkey failure here is logged, not
	// surfaced as a failed account deletion (§14).
	if err := s.sessions.RevokeHashes(ctx, sessionHashes); err != nil {
		slog.WarnContext(ctx, "revoke account session cache", "err", err, "user_id", userID)
	}
	s.clearSessionCookie(w)
	w.WriteHeader(http.StatusNoContent)
}
