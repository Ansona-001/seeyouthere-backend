package httpapi

import (
	"encoding/base32"
	"errors"
	"fmt"
	"net/http"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/ansonarose/seeyouthere-backend/internal/store"
	"github.com/ansonarose/seeyouthere-backend/internal/totp"
)

// mfaIssuer names the account in an authenticator app (§4.11).
const mfaIssuer = "See You There"

// --- POST /v1/admin/mfa/enroll ---

// handleAdminMFAEnroll starts (or restarts) TOTP enrolment for the caller.
// Any admin role may call it and the MFA step-up itself is not required
// (loadAdminContext, not requireAdmin): a user who hasn't verified yet must
// still be able to reach this endpoint. The secret is only sealed and
// persisted as unconfirmed here; it becomes usable for step-up only once
// handleAdminMFAVerify confirms a code against it.
func (s *Server) handleAdminMFAEnroll(w http.ResponseWriter, r *http.Request) {
	admin, ok := s.loadAdminContext(w, r)
	if !ok {
		return
	}
	ctx := r.Context()

	existing, err := s.q.GetUserTOTP(ctx, admin.UserID)
	if err != nil {
		serverError(w, r, fmt.Errorf("get user totp: %w", err))
		return
	}
	if existing.TotpConfirmedAt != nil {
		writeError(w, http.StatusConflict, "already_enrolled", "Two-factor authentication is already set up.")
		return
	}

	secret, err := totp.NewSecret()
	if err != nil {
		serverError(w, r, fmt.Errorf("generate totp secret: %w", err))
		return
	}
	sealed, err := s.totp.Seal(admin.UserID, secret)
	if err != nil {
		serverError(w, r, fmt.Errorf("seal totp secret: %w", err))
		return
	}

	err = s.inTx(ctx, func(tx pgx.Tx, q *store.Queries) error {
		n, err := q.SetUserTOTPSecret(ctx, store.SetUserTOTPSecretParams{TotpSecret: &sealed, UserID: admin.UserID})
		if err != nil {
			return fmt.Errorf("set totp secret: %w", err)
		}
		if n == 0 {
			return errAlreadyEnrolled
		}
		return s.writeAudit(ctx, q, r, auditEntry{
			ActorID: &admin.UserID, Action: "admin.mfa_enroll_started",
			TargetType: "user", TargetID: admin.UserID.String(),
		})
	})
	if err != nil {
		if errors.Is(err, errAlreadyEnrolled) {
			writeError(w, http.StatusConflict, "already_enrolled", "Two-factor authentication is already set up.")
			return
		}
		serverError(w, r, err)
		return
	}

	secretB32 := base32.StdEncoding.WithPadding(base32.NoPadding).EncodeToString(secret)
	writeJSON(w, http.StatusOK, map[string]string{
		"secret":      secretB32,
		"otpauth_uri": totp.URI(mfaIssuer, admin.Email, secret),
	})
}

var errAlreadyEnrolled = errors.New("admin: already enrolled")

// --- POST /v1/admin/mfa/verify ---

// handleAdminMFAVerify confirms a pending enrolment's TOTP code, or (once
// already confirmed) simply steps up the current session for ADMIN_MFA_TTL.
// Replay-safe: ConfirmUserTOTPStep only succeeds when the matched step is
// strictly newer than totp_last_step, compare-and-set in the same statement
// that records it, so a captured code can't be reused even by a second,
// concurrent request.
func (s *Server) handleAdminMFAVerify(w http.ResponseWriter, r *http.Request) {
	admin, ok := s.loadAdminContext(w, r)
	if !ok {
		return
	}
	sessionID, ok := sessionIDFrom(r.Context())
	if !ok {
		writeError(w, http.StatusUnauthorized, "unauthorized", "Log in to continue.")
		return
	}
	ctx := r.Context()

	if !s.allow(w, r, limit{"admin:mfa:verify:" + admin.UserID.String(), 5, 15 * time.Minute}) {
		return
	}

	var body struct {
		Code string `json:"code"`
	}
	if !decodeJSON(w, r, &body) {
		return
	}
	if !validCode(body.Code) {
		writeError(w, http.StatusBadRequest, "invalid_code", "Enter the 6-digit code from your authenticator app.")
		return
	}

	row, err := s.q.GetUserTOTP(ctx, admin.UserID)
	if err != nil {
		serverError(w, r, fmt.Errorf("get user totp: %w", err))
		return
	}
	if row.TotpSecret == nil {
		writeError(w, http.StatusBadRequest, "invalid_code", "Set up two-factor authentication first.")
		return
	}
	secret, err := s.totp.Open(admin.UserID, *row.TotpSecret)
	if err != nil {
		serverError(w, r, fmt.Errorf("open totp secret: %w", err))
		return
	}
	step, ok := totp.Verify(secret, body.Code, time.Now(), row.TotpLastStep)
	if !ok {
		writeError(w, http.StatusBadRequest, "invalid_code", "That code is incorrect or has expired.")
		return
	}

	err = s.inTx(ctx, func(tx pgx.Tx, q *store.Queries) error {
		n, err := q.ConfirmUserTOTPStep(ctx, store.ConfirmUserTOTPStepParams{Step: step, UserID: admin.UserID, ExpectedSecret: *row.TotpSecret})
		if err != nil {
			return fmt.Errorf("confirm totp step: %w", err)
		}
		if n == 0 {
			return errCodeReplayed
		}
		if err := q.SetSessionMFAVerified(ctx, store.SetSessionMFAVerifiedParams{SessionID: sessionID, UserID: admin.UserID}); err != nil {
			return fmt.Errorf("set session mfa verified: %w", err)
		}
		return s.writeAudit(ctx, q, r, auditEntry{
			ActorID: &admin.UserID, Action: "admin.mfa_verified",
			TargetType: "user", TargetID: admin.UserID.String(),
		})
	})
	if err != nil {
		if errors.Is(err, errCodeReplayed) {
			writeError(w, http.StatusBadRequest, "invalid_code", "That code is incorrect or has expired.")
			return
		}
		serverError(w, r, err)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

var errCodeReplayed = errors.New("admin: totp code already used")
