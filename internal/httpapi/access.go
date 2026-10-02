package httpapi

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/ansonarose/seeyouthere-backend/internal/store"
)

// roleRank orders admin roles so requireAdmin("support") also admits
// moderator and super_admin (§4.11: support < moderator < super_admin).
var roleRank = map[string]int{"support": 1, "moderator": 2, "super_admin": 3}

func hasMinRole(roles []string, minRole string) bool {
	need := roleRank[minRole]
	for _, r := range roles {
		// roleRank[r] == 0 means r isn't a recognised admin role (map's
		// zero value for a missing key), which must never satisfy any
		// minRole: without this guard, need == 0 for an unranked minRole
		// would let an unranked/unknown role through too.
		if rank := roleRank[r]; rank > 0 && rank >= need {
			return true
		}
	}
	return false
}

func adminRolesFrom(ctx context.Context) []string {
	roles, _ := ctx.Value(ctxAdminRoles).([]string)
	return roles
}

// loadAdminContext resolves the caller's session to their admin roles and
// MFA step-up state, requiring at least the lowest admin role (support) but
// not the MFA step-up itself: the two MFA enrol/verify endpoints call this
// directly instead of going through requireAdmin, since §4.11 exempts them
// from the MFA-TTL gate (a user who isn't verified yet has to be able to
// reach them). On failure a response has already been written and ok is
// false.
func (s *Server) loadAdminContext(w http.ResponseWriter, r *http.Request) (store.GetAdminContextRow, bool) {
	userID, ok := userIDFrom(r.Context())
	sessionID, sessOK := sessionIDFrom(r.Context())
	if !ok || !sessOK {
		writeError(w, http.StatusUnauthorized, "unauthorized", "Log in to continue.")
		return store.GetAdminContextRow{}, false
	}

	admin, err := s.q.GetAdminContext(r.Context(), store.GetAdminContextParams{
		SessionID: sessionID,
		UserID:    userID,
	})
	switch {
	case errors.Is(err, pgx.ErrNoRows):
		writeError(w, http.StatusForbidden, "forbidden", "You don't have access to this.")
		return store.GetAdminContextRow{}, false
	case err != nil:
		serverError(w, r, err)
		return store.GetAdminContextRow{}, false
	}
	if !hasMinRole(admin.Roles, "support") {
		writeError(w, http.StatusForbidden, "forbidden", "You don't have access to this.")
		return store.GetAdminContextRow{}, false
	}
	return admin, true
}

// requireAdmin builds middleware that admits a session only if its user
// holds at least minRole and has stepped up with TOTP within ADMIN_MFA_TTL.
// Roles and MFA state are read from Postgres on every admin request (§4.11:
// admin traffic is tiny, so there's no point caching it and risking staleness
// after a role revoke or ban).
func (s *Server) requireAdmin(minRole string) func(http.Handler) http.Handler {
	// A typo'd or removed minRole would make roleRank[minRole] == 0, which
	// hasMinRole never satisfies: every request would be silently forbidden
	// forever rather than failing loudly at startup, so fail fast here
	// instead of shipping a route no admin role can ever pass.
	if roleRank[minRole] == 0 {
		panic(fmt.Sprintf("httpapi: requireAdmin: unknown minRole %q", minRole))
	}
	return func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			admin, ok := s.loadAdminContext(w, r)
			if !ok {
				return
			}
			if !hasMinRole(admin.Roles, minRole) {
				writeError(w, http.StatusForbidden, "forbidden", "You don't have access to this.")
				return
			}
			if admin.MfaVerifiedAt == nil || time.Since(*admin.MfaVerifiedAt) > s.cfg.AdminMFATTL {
				writeError(w, http.StatusForbidden, "mfa_required", "Verify your two-factor code to continue.")
				return
			}

			ctx := context.WithValue(r.Context(), ctxAdminRoles, admin.Roles)
			next.ServeHTTP(w, r.WithContext(ctx))
		})
	}
}
