package httpapi

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"net/mail"
	"net/netip"
	"strings"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"

	"github.com/ansonarose/seeyouthere-backend/internal/auth"
	"github.com/ansonarose/seeyouthere-backend/internal/jobs"
	mailpkg "github.com/ansonarose/seeyouthere-backend/internal/mail"
	"github.com/ansonarose/seeyouthere-backend/internal/store"
)

// normalizeEmail accepts a bare address only and returns it lowercased.
func normalizeEmail(raw string) (string, bool) {
	raw = strings.TrimSpace(raw)
	if raw == "" || len(raw) > 254 {
		return "", false
	}
	addr, err := mail.ParseAddress(raw)
	if err != nil || addr.Address != raw || addr.Name != "" {
		return "", false
	}
	at := strings.LastIndexByte(raw, '@')
	if at < 1 || !strings.Contains(raw[at+1:], ".") {
		return "", false
	}
	return strings.ToLower(raw), true
}

func validCode(code string) bool {
	if len(code) != 6 {
		return false
	}
	for _, c := range code {
		if c < '0' || c > '9' {
			return false
		}
	}
	return true
}

// allow applies each limit in order and writes a 429 on the first one exceeded.
func (s *Server) allow(w http.ResponseWriter, r *http.Request, limits ...limit) bool {
	for _, l := range limits {
		ok, err := s.limiter.Allow(r.Context(), l.key, l.max, l.window)
		if err != nil {
			serverError(w, r, fmt.Errorf("rate limit: %w", err))
			return false
		}
		if !ok {
			w.Header().Set("Retry-After", fmt.Sprint(int(l.window.Seconds())))
			writeError(w, http.StatusTooManyRequests, "rate_limited", "Too many requests. Please wait and try again.")
			return false
		}
	}
	return true
}

type limit struct {
	key    string
	max    int64
	window time.Duration
}

// POST /v1/auth/code {email}
// Always answers 202 for a valid address so it can't be used to discover accounts.
func (s *Server) handleRequestCode(w http.ResponseWriter, r *http.Request) {
	var body struct {
		Email string `json:"email"`
	}
	if !decodeJSON(w, r, &body) {
		return
	}
	email, ok := normalizeEmail(body.Email)
	if !ok {
		writeError(w, http.StatusBadRequest, "invalid_email", "Enter a valid email address.")
		return
	}

	ip := ipRateKey(clientIPFrom(r.Context()))
	if !s.allow(w, r,
		limit{"code:ip:" + ip, 20, time.Hour},
		limit{"code:email:" + email, 5, time.Hour},
	) {
		return
	}

	code, err := s.codes.Issue(r.Context(), email)
	if err != nil {
		serverError(w, r, err)
		return
	}
	_, err = s.jobs.Insert(r.Context(), jobs.SendEmailArgs{Message: mailpkg.Message{
		To:      email,
		Subject: code + " is your See You There login code",
		Text: fmt.Sprintf("Your login code is %s\n\n"+
			"It expires in %d minutes. If you didn't ask for it, you can ignore this email.\n\n"+
			"See You There\n", code, int(auth.CodeTTL.Minutes())),
	}}, nil)
	if err != nil {
		serverError(w, r, fmt.Errorf("enqueue login email: %w", err))
		return
	}
	writeJSON(w, http.StatusAccepted, map[string]bool{"ok": true})
}

// POST /v1/auth/verify {email, code}
func (s *Server) handleVerifyCode(w http.ResponseWriter, r *http.Request) {
	var body struct {
		Email string `json:"email"`
		Code  string `json:"code"`
	}
	if !decodeJSON(w, r, &body) {
		return
	}
	email, ok := normalizeEmail(body.Email)
	code := strings.TrimSpace(body.Code)
	if !ok || !validCode(code) {
		writeError(w, http.StatusBadRequest, "invalid_code", "That code is wrong or has expired.")
		return
	}

	ip := clientIPFrom(r.Context())
	if !s.allow(w, r, limit{"verify:ip:" + ipRateKey(ip), 30, 10 * time.Minute}) {
		return
	}

	switch err := s.codes.Verify(r.Context(), email, code); {
	case errors.Is(err, auth.ErrCodeInvalid):
		writeError(w, http.StatusBadRequest, "invalid_code", "That code is wrong or has expired.")
		return
	case errors.Is(err, auth.ErrTooManyAttempts):
		writeError(w, http.StatusTooManyRequests, "too_many_attempts", "Too many wrong codes. Request a new one.")
		return
	case err != nil:
		serverError(w, r, err)
		return
	}

	user, err := s.q.UpsertUserByEmail(r.Context(), store.UpsertUserByEmailParams{
		ID:    uuid.Must(uuid.NewV7()),
		Email: email,
	})
	if err != nil {
		serverError(w, r, fmt.Errorf("upsert user: %w", err))
		return
	}
	if user.Status != "active" {
		writeError(w, http.StatusForbidden, "account_suspended", "This account has been suspended.")
		return
	}

	var ipPtr *netip.Addr
	if ip.IsValid() {
		ipPtr = &ip
	}
	token, expires, err := s.sessions.Create(r.Context(), user.ID, ipPtr, r.UserAgent())
	if err != nil {
		serverError(w, r, err)
		return
	}
	s.setSessionCookie(w, token, expires)

	roles, err := s.q.ListUserRoles(r.Context(), user.ID)
	if err != nil {
		serverError(w, r, err)
		return
	}

	// A brand-new session has never been stepped up yet, regardless of role.
	respBody := map[string]any{"user": newUserResponse(user, roles, false)}
	// Claim any anonymous drafts from this browser (decision 4, §4.10): the
	// draft cookie carries no secret worth protecting on its own (it only
	// unlocks drafts, which become claimable at login anyway), so a failure
	// here must never fail the login itself.
	if hash, ok := draftCookieHash(r); ok {
		claimed, err := s.claimAnonDrafts(r.Context(), user.ID, hash)
		if err != nil {
			slog.ErrorContext(r.Context(), "claim anon drafts", "err", err, "user_id", user.ID)
		} else {
			respBody["claimed_event_ids"] = claimed
			s.clearDraftCookie(w)
		}
	}
	writeJSON(w, http.StatusOK, respBody)
}

// claimAnonDrafts hands every unexpired draft under cookieHash to userID and
// reassigns their media in the same transaction, so a claimed draft is never
// left with orphaned media rows.
func (s *Server) claimAnonDrafts(ctx context.Context, userID uuid.UUID, cookieHash []byte) ([]uuid.UUID, error) {
	var claimed []uuid.UUID
	err := s.inTx(ctx, func(_ pgx.Tx, q *store.Queries) error {
		ids, err := q.ClaimAnonDrafts(ctx, store.ClaimAnonDraftsParams{UserID: userID, CookieHash: cookieHash})
		if err != nil {
			return fmt.Errorf("claim anon drafts: %w", err)
		}
		if len(ids) > 0 {
			if err := q.ClaimAnonDraftMedia(ctx, store.ClaimAnonDraftMediaParams{UserID: userID, EventIds: ids}); err != nil {
				return fmt.Errorf("claim anon draft media: %w", err)
			}
		}
		claimed = ids
		return nil
	})
	if err != nil {
		return nil, err
	}
	return claimed, nil
}

// POST /v1/auth/logout
func (s *Server) handleLogout(w http.ResponseWriter, r *http.Request) {
	if c, err := r.Cookie(sessionCookie); err == nil && c.Value != "" {
		if err := s.sessions.Revoke(r.Context(), c.Value); err != nil {
			serverError(w, r, err)
			return
		}
	}
	s.clearSessionCookie(w)
	w.WriteHeader(http.StatusNoContent)
}

// GET /v1/me
func (s *Server) handleMe(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	id, _ := userIDFrom(ctx)
	user, err := s.q.GetUserByID(ctx, id)
	if err != nil {
		serverError(w, r, fmt.Errorf("get user: %w", err))
		return
	}
	roles, err := s.q.ListUserRoles(ctx, id)
	if err != nil {
		serverError(w, r, err)
		return
	}
	verified, err := s.currentSessionMFAVerified(ctx, id, roles)
	if err != nil {
		serverError(w, r, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]userResponse{"user": newUserResponse(user, roles, verified)})
}

// currentSessionMFAVerified reports whether the caller's current session has
// stepped up with TOTP within ADMIN_MFA_TTL (§4.9's "mfa" object on GET/PATCH
// /v1/me, only meaningful — and only queried — for role holders). Returns
// false, not an error, for a caller with no session in context (e.g. a
// non-cookie test harness), since that's the same "not verified" state a
// real logged-out caller would report.
func (s *Server) currentSessionMFAVerified(ctx context.Context, userID uuid.UUID, roles []string) (bool, error) {
	if len(roles) == 0 {
		return false, nil
	}
	sessionID, ok := sessionIDFrom(ctx)
	if !ok {
		return false, nil
	}
	admin, err := s.q.GetAdminContext(ctx, store.GetAdminContextParams{SessionID: sessionID, UserID: userID})
	if errors.Is(err, pgx.ErrNoRows) {
		return false, nil
	}
	if err != nil {
		return false, fmt.Errorf("get admin context: %w", err)
	}
	return admin.MfaVerifiedAt != nil && time.Since(*admin.MfaVerifiedAt) <= s.cfg.AdminMFATTL, nil
}

type mfaStatusResp struct {
	Enrolled bool `json:"enrolled"`
	Verified bool `json:"verified"`
}

type userResponse struct {
	ID        uuid.UUID      `json:"id"`
	Email     string         `json:"email"`
	Name      string         `json:"name"`
	Roles     []string       `json:"roles"`
	CreatedAt time.Time      `json:"created_at"`
	Mfa       *mfaStatusResp `json:"mfa,omitempty"`
}

// newUserResponse builds the §4.1/§4.9 user shape. mfa is included only for
// role holders (a plain user has no admin step-up state to report).
func newUserResponse(u store.User, roles []string, mfaVerified bool) userResponse {
	resp := userResponse{ID: u.ID, Email: u.Email, Name: u.Name, Roles: roles, CreatedAt: u.CreatedAt}
	if len(roles) > 0 {
		resp.Mfa = &mfaStatusResp{Enrolled: u.TotpConfirmedAt != nil, Verified: mfaVerified}
	}
	return resp
}
