package httpapi

import (
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"slices"
	"strings"
	"time"
	"unicode"
	"unicode/utf8"

	"github.com/go-chi/chi/v5"
	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"

	"github.com/ansonarose/seeyouthere-backend/internal/jobs"
	"github.com/ansonarose/seeyouthere-backend/internal/store"
)

var (
	errAdminUserNotFound = errors.New("admin: user not found")
	errLastSuperAdmin    = errors.New("admin: cannot remove the last super_admin")
	errCannotReverseBan  = errors.New("admin: only a super admin can reverse a ban")
)

var validUserStatuses = map[string]bool{"active": true, "suspended": true, "banned": true}
var grantableRoles = map[string]bool{"support": true, "moderator": true, "super_admin": true}

// validateAdminReason trims s and rejects control characters, matching the
// admin note/reason fields' 500-rune cap (§4.11). Empty is allowed: a reason
// is optional context, not a required justification field.
func validateAdminReason(s string, max int) (string, bool) {
	s = strings.TrimSpace(s)
	if utf8.RuneCountInString(s) > max {
		return "", false
	}
	for _, r := range s {
		if unicode.IsControl(r) {
			return "", false
		}
	}
	return s, true
}

// adminUserSummaryResp is the §4.11 user list/detail shape.
type adminUserSummaryResp struct {
	ID        uuid.UUID `json:"id"`
	Email     string    `json:"email"`
	Name      string    `json:"name"`
	Status    string    `json:"status"`
	Roles     []string  `json:"roles"`
	CreatedAt time.Time `json:"created_at"`
}

// --- GET /v1/admin/overview ---

func (s *Server) handleAdminOverview(w http.ResponseWriter, r *http.Request) {
	row, err := s.q.AdminOverviewCounts(r.Context())
	if err != nil {
		serverError(w, r, fmt.Errorf("admin overview counts: %w", err))
		return
	}
	writeJSON(w, http.StatusOK, map[string]int64{
		"open_reports":        row.OpenReports,
		"pending_photos":      row.PendingGuestPhotos,
		"new_users_7d":        row.NewUsers7d,
		"events_published_7d": row.EventsPublished7d,
	})
}

// --- GET /v1/admin/users?q&status&cursor&limit ---

// handleListAdminUsers picks between three query strategies (§13 phaseA
// notes): an exact address for a q that parses as one email (unpaginated,
// at most one row), a prefix tsquery over email+name text, or a plain
// newest-first browse — each with an optional status filter.
func (s *Server) handleListAdminUsers(w http.ResponseWriter, r *http.Request) {
	query := r.URL.Query()
	limitN := clampLimit(query.Get("limit"), 25, 100)

	var status *string
	if v := strings.TrimSpace(query.Get("status")); v != "" {
		if !validUserStatuses[v] {
			writeError(w, http.StatusBadRequest, "validation_failed", "Unknown status filter.")
			return
		}
		status = &v
	}

	ctx := r.Context()
	qtext := strings.TrimSpace(query.Get("q"))
	items := []adminUserSummaryResp{}
	var nextCursor *string

	switch {
	case qtext != "" && isExactEmail(qtext):
		email, _ := normalizeEmail(qtext)
		rows, err := s.q.SearchUsersAdminByEmail(ctx, store.SearchUsersAdminByEmailParams{Email: email, Status: status})
		if err != nil {
			serverError(w, r, fmt.Errorf("search users by email: %w", err))
			return
		}
		for _, row := range rows {
			items = append(items, adminUserSummaryResp{row.ID, row.Email, row.Name, row.Status, row.Roles, row.CreatedAt})
		}

	case qtext != "":
		prefix, ok := buildPrefixQuery(qtext)
		if !ok {
			break // no indexable tokens in q: nothing can match, empty result
		}
		createdAt, id, ok := decodeAdminCursor(w, r)
		if !ok {
			return
		}
		rows, err := s.q.SearchUsersAdminText(ctx, store.SearchUsersAdminTextParams{
			PrefixQuery: prefix, Status: status, CursorCreatedAt: createdAt, CursorID: id, Lim: int32(limitN),
		})
		if err != nil {
			serverError(w, r, fmt.Errorf("search users text: %w", err))
			return
		}
		for _, row := range rows {
			items = append(items, adminUserSummaryResp{row.ID, row.Email, row.Name, row.Status, row.Roles, row.CreatedAt})
		}
		if len(rows) == limitN {
			nc := encodeCursor(rows[len(rows)-1].CreatedAt, rows[len(rows)-1].ID)
			nextCursor = &nc
		}

	case status != nil:
		createdAt, id, ok := decodeAdminCursor(w, r)
		if !ok {
			return
		}
		rows, err := s.q.SearchUsersAdminByStatus(ctx, store.SearchUsersAdminByStatusParams{
			Status: *status, CursorCreatedAt: createdAt, CursorID: id, Lim: int32(limitN),
		})
		if err != nil {
			serverError(w, r, fmt.Errorf("search users by status: %w", err))
			return
		}
		for _, row := range rows {
			items = append(items, adminUserSummaryResp{row.ID, row.Email, row.Name, row.Status, row.Roles, row.CreatedAt})
		}
		if len(rows) == limitN {
			nc := encodeCursor(rows[len(rows)-1].CreatedAt, rows[len(rows)-1].ID)
			nextCursor = &nc
		}

	default:
		createdAt, id, ok := decodeAdminCursor(w, r)
		if !ok {
			return
		}
		rows, err := s.q.SearchUsersAdmin(ctx, store.SearchUsersAdminParams{CursorCreatedAt: createdAt, CursorID: id, Lim: int32(limitN)})
		if err != nil {
			serverError(w, r, fmt.Errorf("search users: %w", err))
			return
		}
		for _, row := range rows {
			items = append(items, adminUserSummaryResp{row.ID, row.Email, row.Name, row.Status, row.Roles, row.CreatedAt})
		}
		if len(rows) == limitN {
			nc := encodeCursor(rows[len(rows)-1].CreatedAt, rows[len(rows)-1].ID)
			nextCursor = &nc
		}
	}

	writeJSON(w, http.StatusOK, map[string]any{"users": items, "next_cursor": nextCursor})
}

// isExactEmail reports whether q parses as a single, well-formed address
// (as opposed to free text that happens to contain an "@").
func isExactEmail(q string) bool {
	_, ok := normalizeEmail(q)
	return ok
}

// --- GET /v1/admin/users/{id} ---

func (s *Server) handleGetAdminUser(w http.ResponseWriter, r *http.Request) {
	targetID, err := parseUUIDParam(r, "id")
	if err != nil {
		writeError(w, http.StatusNotFound, "not_found", "No such user.")
		return
	}
	ctx := r.Context()
	row, err := s.q.GetUserAdmin(ctx, targetID)
	if errors.Is(err, pgx.ErrNoRows) {
		writeError(w, http.StatusNotFound, "not_found", "No such user.")
		return
	}
	if err != nil {
		serverError(w, r, fmt.Errorf("get user admin: %w", err))
		return
	}

	createdAt, cid := firstPageCursor()
	auditRows, err := s.q.ListAuditForTarget(ctx, store.ListAuditForTargetParams{
		TargetType: "user", TargetID: targetID.String(), CursorCreatedAt: createdAt, CursorID: cid, Lim: 20,
	})
	if err != nil {
		serverError(w, r, fmt.Errorf("list audit for target: %w", err))
		return
	}
	audit, err := s.buildAuditEntries(ctx, auditRows)
	if err != nil {
		serverError(w, r, err)
		return
	}

	writeJSON(w, http.StatusOK, map[string]any{
		"user": adminUserSummaryResp{row.ID, row.Email, row.Name, row.Status, row.Roles, row.CreatedAt},
		"counts": map[string]int64{
			"events":   row.LiveEvents,
			"sessions": row.ActiveSessions,
		},
		"audit": audit,
	})
}

// --- POST /v1/admin/users/{id}/status ---

// handleSetAdminUserStatus (§4.11): moderator may set active/suspended,
// only super_admin may ban. Nobody may act on themselves, and only a
// super_admin may act on a user who already holds any admin role.
// Suspend and ban revoke every session; ban additionally takes down all of
// the user's live events (media_visibility per event, matching the same
// takedown path an explicit per-event admin takedown uses).
func (s *Server) handleSetAdminUserStatus(w http.ResponseWriter, r *http.Request) {
	targetID, err := parseUUIDParam(r, "id")
	if err != nil {
		writeError(w, http.StatusNotFound, "not_found", "No such user.")
		return
	}
	callerID, _ := userIDFrom(r.Context())
	if targetID == callerID {
		writeError(w, http.StatusForbidden, "forbidden", "You can't change your own status.")
		return
	}

	var body struct {
		Status string `json:"status"`
		Reason string `json:"reason"`
	}
	if !decodeJSON(w, r, &body) {
		return
	}
	if !validUserStatuses[body.Status] {
		writeError(w, http.StatusBadRequest, "validation_failed", "Unknown status.")
		return
	}
	reason, ok := validateAdminReason(body.Reason, 500)
	if !ok {
		writeError(w, http.StatusBadRequest, "validation_failed", "Check the reason field.")
		return
	}
	callerRoles := adminRolesFrom(r.Context())
	if body.Status == "banned" && !hasMinRole(callerRoles, "super_admin") {
		writeError(w, http.StatusForbidden, "forbidden", "Only a super admin can ban a user.")
		return
	}

	ctx := r.Context()
	targetRoles, err := s.q.ListUserRoles(ctx, targetID)
	if err != nil {
		serverError(w, r, fmt.Errorf("list user roles: %w", err))
		return
	}
	if len(targetRoles) > 0 && !hasMinRole(callerRoles, "super_admin") {
		writeError(w, http.StatusForbidden, "forbidden", "Only a super admin can act on a user who holds a role.")
		return
	}

	var revokedHashes [][]byte
	err = s.inTx(ctx, func(tx pgx.Tx, q *store.Queries) error {
		// Changing a super_admin to a non-active status must not leave zero
		// active super_admins. This has to run, locked, before SetUserStatus
		// touches the row: SetUserStatus's own count of active super_admins
		// would otherwise already exclude the target (its status having
		// just flipped), always seeing count-1 and refusing every such
		// change. Locking roles for the duration means two concurrent status
		// changes on two different super_admins can't both pass the check
		// (mirrors the last-super_admin guard in handleAdminUserRole).
		if body.Status != "active" && slices.Contains(targetRoles, "super_admin") {
			if err := q.LockRoles(ctx); err != nil {
				return fmt.Errorf("lock roles: %w", err)
			}
			count, err := q.CountSuperAdmins(ctx)
			if err != nil {
				return fmt.Errorf("count super admins: %w", err)
			}
			if count <= 1 {
				return errLastSuperAdmin
			}
		}

		row, err := q.SetUserStatus(ctx, store.SetUserStatusParams{Status: body.Status, TargetID: targetID})
		if errors.Is(err, pgx.ErrNoRows) {
			return errAdminUserNotFound
		}
		if err != nil {
			return fmt.Errorf("set user status: %w", err)
		}
		// A moderator/support caller may set active or suspended, but never
		// on a user a super_admin already banned: that would let a
		// lower-ranked role silently reverse the ban. Only super_admin may
		// move a banned user out of that status.
		if row.OldStatus == "banned" && !hasMinRole(callerRoles, "super_admin") {
			return errCannotReverseBan
		}
		if body.Status != "active" {
			hashes, err := q.DeleteAllUserSessions(ctx, targetID)
			if err != nil {
				return fmt.Errorf("delete user sessions: %w", err)
			}
			revokedHashes = hashes
		}
		if body.Status == "banned" {
			eventIDs, err := q.TakeDownEventsByOwner(ctx, targetID)
			if err != nil {
				return fmt.Errorf("take down events by owner: %w", err)
			}
			for _, eventID := range eventIDs {
				if _, err := s.jobs.InsertTx(ctx, tx, jobs.MediaVisibilityArgs{EventID: eventID}, nil); err != nil {
					return fmt.Errorf("enqueue media visibility: %w", err)
				}
			}
		}
		after := map[string]string{"status": row.NewStatus}
		if reason != "" {
			after["reason"] = reason
		}
		return s.writeAudit(ctx, q, r, auditEntry{
			ActorID: &callerID, Action: "user.status", TargetType: "user", TargetID: targetID.String(),
			Before: map[string]string{"status": row.OldStatus}, After: after,
		})
	})
	if err != nil {
		if errors.Is(err, errAdminUserNotFound) {
			writeError(w, http.StatusNotFound, "not_found", "No such user.")
			return
		}
		if errors.Is(err, errCannotReverseBan) {
			writeError(w, http.StatusForbidden, "forbidden", "Only a super admin can reverse a ban.")
			return
		}
		if errors.Is(err, errLastSuperAdmin) {
			writeError(w, http.StatusConflict, "last_super_admin", "This user is the last super admin; grant the role to someone else first.")
			return
		}
		serverError(w, r, err)
		return
	}
	if len(revokedHashes) > 0 {
		if err := s.sessions.RevokeHashes(ctx, revokedHashes); err != nil {
			slog.WarnContext(ctx, "revoke session cache entries", "err", err, "user_id", targetID)
		}
	}
	writeJSON(w, http.StatusOK, map[string]bool{"ok": true})
}

// --- POST /v1/admin/users/{id}/sessions/revoke ---

func (s *Server) handleAdminRevokeUserSessions(w http.ResponseWriter, r *http.Request) {
	targetID, err := parseUUIDParam(r, "id")
	if err != nil {
		writeError(w, http.StatusNotFound, "not_found", "No such user.")
		return
	}
	callerID, _ := userIDFrom(r.Context())
	if targetID == callerID {
		writeError(w, http.StatusForbidden, "forbidden", "You can't revoke your own sessions here.")
		return
	}

	ctx := r.Context()
	// Mirror the guard in handleSetAdminUserStatus (lines 267-275): a target
	// who holds any admin role can only have their sessions revoked by a
	// super_admin, so support/moderator can't kick a super_admin off every
	// session of theirs.
	targetRoles, err := s.q.ListUserRoles(ctx, targetID)
	if err != nil {
		serverError(w, r, fmt.Errorf("list user roles: %w", err))
		return
	}
	if len(targetRoles) > 0 && !hasMinRole(adminRolesFrom(ctx), "super_admin") {
		writeError(w, http.StatusForbidden, "forbidden", "Only a super admin can act on a user who holds a role.")
		return
	}

	var hashes [][]byte
	err = s.inTx(ctx, func(_ pgx.Tx, q *store.Queries) error {
		h, err := q.DeleteAllUserSessions(ctx, targetID)
		if err != nil {
			return fmt.Errorf("delete user sessions: %w", err)
		}
		hashes = h
		return s.writeAudit(ctx, q, r, auditEntry{
			ActorID: &callerID, Action: "user.sessions_revoke", TargetType: "user", TargetID: targetID.String(),
			After: map[string]int{"revoked": len(h)},
		})
	})
	if err != nil {
		serverError(w, r, err)
		return
	}
	if len(hashes) > 0 {
		if err := s.sessions.RevokeHashes(ctx, hashes); err != nil {
			slog.WarnContext(ctx, "revoke session cache entries", "err", err, "user_id", targetID)
		}
	}
	writeJSON(w, http.StatusOK, map[string]int{"revoked": len(hashes)})
}

// --- POST/DELETE /v1/admin/users/{id}/roles/{role} ---

// handleAdminUserRole grants or revokes one role. Revoking a user's last
// super_admin role is blocked under an advisory lock (LockRoles) so two
// concurrent revokes on two different super_admins can't both succeed and
// leave zero. Both directions are idempotent: granting an already-held role
// or revoking one not held succeeds without writing a redundant audit row.
func (s *Server) handleAdminUserRole(w http.ResponseWriter, r *http.Request) {
	targetID, err := parseUUIDParam(r, "id")
	if err != nil {
		writeError(w, http.StatusNotFound, "not_found", "No such user.")
		return
	}
	role := chi.URLParam(r, "role")
	if !grantableRoles[role] {
		writeError(w, http.StatusBadRequest, "validation_failed", "Unknown role.")
		return
	}
	callerID, _ := userIDFrom(r.Context())
	if targetID == callerID {
		writeError(w, http.StatusForbidden, "forbidden", "You can't change your own roles.")
		return
	}

	ctx := r.Context()
	if _, err := s.q.GetUserAdmin(ctx, targetID); errors.Is(err, pgx.ErrNoRows) {
		writeError(w, http.StatusNotFound, "not_found", "No such user.")
		return
	} else if err != nil {
		serverError(w, r, fmt.Errorf("get user admin: %w", err))
		return
	}

	grant := r.Method == http.MethodPost
	err = s.inTx(ctx, func(_ pgx.Tx, q *store.Queries) error {
		if err := q.LockRoles(ctx); err != nil {
			return fmt.Errorf("lock roles: %w", err)
		}
		if grant {
			n, err := q.GrantRole(ctx, store.GrantRoleParams{UserID: targetID, Role: role})
			if err != nil {
				return fmt.Errorf("grant role: %w", err)
			}
			if n == 0 {
				return nil // already held
			}
			return s.writeAudit(ctx, q, r, auditEntry{
				ActorID: &callerID, Action: "user.role_grant", TargetType: "user", TargetID: targetID.String(),
				After: map[string]string{"role": role},
			})
		}

		if role == "super_admin" {
			targetRoles, err := q.ListUserRoles(ctx, targetID)
			if err != nil {
				return fmt.Errorf("list user roles: %w", err)
			}
			count, err := q.CountSuperAdmins(ctx)
			if err != nil {
				return fmt.Errorf("count super admins: %w", err)
			}
			if slices.Contains(targetRoles, "super_admin") && count <= 1 {
				return errLastSuperAdmin
			}
		}
		n, err := q.RevokeRole(ctx, store.RevokeRoleParams{UserID: targetID, Role: role})
		if err != nil {
			return fmt.Errorf("revoke role: %w", err)
		}
		if n == 0 {
			return nil // wasn't held
		}
		return s.writeAudit(ctx, q, r, auditEntry{
			ActorID: &callerID, Action: "user.role_revoke", TargetType: "user", TargetID: targetID.String(),
			Before: map[string]string{"role": role},
		})
	})
	if err != nil {
		if errors.Is(err, errLastSuperAdmin) {
			writeError(w, http.StatusConflict, "last_super_admin", "This user is the last super admin; grant the role to someone else first.")
			return
		}
		serverError(w, r, err)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

// --- POST /v1/admin/users/{id}/mfa/reset ---

func (s *Server) handleAdminResetUserMFA(w http.ResponseWriter, r *http.Request) {
	targetID, err := parseUUIDParam(r, "id")
	if err != nil {
		writeError(w, http.StatusNotFound, "not_found", "No such user.")
		return
	}
	callerID, _ := userIDFrom(r.Context())
	if targetID == callerID {
		writeError(w, http.StatusForbidden, "forbidden", "You can't reset your own two-factor authentication here.")
		return
	}

	ctx := r.Context()
	err = s.inTx(ctx, func(_ pgx.Tx, q *store.Queries) error {
		n, err := q.ResetUserTOTP(ctx, targetID)
		if err != nil {
			return fmt.Errorf("reset user totp: %w", err)
		}
		if n == 0 {
			return errAdminUserNotFound
		}
		return s.writeAudit(ctx, q, r, auditEntry{
			ActorID: &callerID, Action: "user.mfa_reset", TargetType: "user", TargetID: targetID.String(),
		})
	})
	if err != nil {
		if errors.Is(err, errAdminUserNotFound) {
			writeError(w, http.StatusNotFound, "not_found", "No such user.")
			return
		}
		serverError(w, r, err)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}
