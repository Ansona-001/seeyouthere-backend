package httpapi

import (
	"context"
	"crypto/hmac"
	"crypto/sha1"
	"crypto/sha256"
	"encoding/base32"
	"encoding/binary"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/redis/go-redis/v9"

	"github.com/ansonarose/seeyouthere-backend/internal/auth"
	"github.com/ansonarose/seeyouthere-backend/internal/config"
	"github.com/ansonarose/seeyouthere-backend/internal/jobs"
	"github.com/ansonarose/seeyouthere-backend/internal/ratelimit"
	"github.com/ansonarose/seeyouthere-backend/internal/store"
	"github.com/ansonarose/seeyouthere-backend/internal/token"
	"github.com/ansonarose/seeyouthere-backend/internal/totp"
)

// --- buildPrefixQuery (pure function) ---

func TestBuildPrefixQuery(t *testing.T) {
	cases := []struct {
		in     string
		want   string
		wantOK bool
	}{
		{"", "", false},
		{"   ", "", false},
		{"ali example", "ali:* & example:*", true},
		{"a&b|c!(d)", "abcd:*", true}, // one whitespace-free field: buildPrefixQuery only splits on strings.Fields
		{"Ali Ce", "ali:* & ce:*", true},
	}
	for _, c := range cases {
		got, ok := buildPrefixQuery(c.in)
		if ok != c.wantOK || (ok && got != c.want) {
			t.Errorf("buildPrefixQuery(%q) = (%q, %v), want (%q, %v)", c.in, got, ok, c.want, c.wantOK)
		}
	}
}

// --- test fixture ---

// adminHandlerFixture wires a *Server bound to the real pool (several
// handlers under test call s.inTx / s.sessions.RevokeHashes / s.jobs.InsertTx,
// none of which can run inside an outer test transaction) plus a super_admin,
// a moderator (both MFA-verified) and a plain user with no admin role. Every
// row it creates is cleaned up.
type adminHandlerFixture struct {
	s                              *Server
	superAdminID, superAdminSessID uuid.UUID
	moderatorID, moderatorSessID   uuid.UUID
	plainUserID                    uuid.UUID
}

func newAdminHandlerFixture(t *testing.T, pool *pgxpool.Pool, rdb *redis.Client) adminHandlerFixture {
	t.Helper()
	ctx := context.Background()
	q := store.New(pool)

	newAdminUser := func(label, role string) (userID, sessionID uuid.UUID) {
		userID = uuid.Must(uuid.NewV7())
		if _, err := q.UpsertUserByEmail(ctx, store.UpsertUserByEmailParams{
			ID: userID, Email: label + "-" + userID.String() + "@example.invalid",
		}); err != nil {
			t.Fatalf("create %s user: %v", label, err)
		}
		if role != "" {
			if _, err := q.GrantRole(ctx, store.GrantRoleParams{UserID: userID, Role: role}); err != nil {
				t.Fatalf("grant role %s: %v", role, err)
			}
		}
		sess, err := q.CreateSession(ctx, store.CreateSessionParams{
			ID: uuid.Must(uuid.NewV7()), UserID: userID, TokenHash: []byte(userID.String()),
			ExpiresAt: time.Now().Add(time.Hour), UserAgent: "test",
		})
		if err != nil {
			t.Fatalf("create session for %s: %v", label, err)
		}
		if role != "" {
			if err := q.SetSessionMFAVerified(ctx, store.SetSessionMFAVerifiedParams{SessionID: sess.ID, UserID: userID}); err != nil {
				t.Fatalf("set mfa verified for %s: %v", label, err)
			}
		}
		return userID, sess.ID
	}

	superAdminID, superAdminSessID := newAdminUser("superadmin", "super_admin")
	moderatorID, moderatorSessID := newAdminUser("moderator", "moderator")
	plainUserID, _ := newAdminUser("plain", "")

	t.Cleanup(func() {
		// Soft-delete, not a hard DELETE: these users may have become an
		// audit_log actor during the test, and audit_log's append-only
		// trigger blocks even the FK's ON DELETE SET NULL action, which a
		// hard delete would trigger. SoftDeleteUser also excludes them from
		// CountSuperAdmins (deleted_at IS NULL), so a leftover super_admin
		// grant from this fixture can never pollute a later test's count.
		cleanupCtx := context.Background()
		for _, id := range []uuid.UUID{superAdminID, moderatorID, plainUserID} {
			if err := q.SoftDeleteUser(cleanupCtx, id); err != nil {
				t.Logf("cleanup user %s: %v", id, err)
			}
		}
	})

	totpKey := make([]byte, 32)
	sealer, err := totp.NewSealer(totpKey)
	if err != nil {
		t.Fatalf("new sealer: %v", err)
	}
	mediaStore := newTestMediaStore(t)
	tokens, err := token.NewKeys([]byte("0123456789012345678901234567890123456789"))
	if err != nil {
		t.Fatalf("new token keys: %v", err)
	}
	limiter := ratelimit.New(rdb)
	jobClient, err := jobs.NewClient(pool, noopSender{}, noopSender{}, q, mediaStore, tokens, limiter, "", "https://seeyouthere.at", testTotalQuotaBytes)
	if err != nil {
		t.Fatalf("jobs.NewClient: %v", err)
	}

	s := &Server{
		pool: pool, q: q, rdb: rdb, limiter: limiter,
		sessions: auth.NewSessions(q, rdb, time.Hour),
		jobs:     jobClient,
		totp:     sealer,
		media:    mediaStore,
		cfg:      config.Config{SiteURL: "https://seeyouthere.at", AdminMFATTL: 12 * time.Hour},
	}
	return adminHandlerFixture{
		s: s, superAdminID: superAdminID, superAdminSessID: superAdminSessID,
		moderatorID: moderatorID, moderatorSessID: moderatorSessID, plainUserID: plainUserID,
	}
}

// adminRequestAs builds a request carrying userID/sessionID/roles exactly as
// requireAdmin would have populated the context, for handlers tested
// directly (below their own middleware, matching the rest of this package's
// convention of unit-testing handlers with a hand-built context).
func adminRequestAs(method, path string, userID, sessionID uuid.UUID, roles []string) *http.Request {
	req := requestWithSession(userID, sessionID)
	req = httptest.NewRequest(method, path, req.Body).WithContext(
		context.WithValue(context.WithValue(context.WithValue(req.Context(), ctxUserID, userID), ctxSessionID, sessionID), ctxAdminRoles, roles),
	)
	return req
}

func jsonRequest(method, path string, userID, sessionID uuid.UUID, roles []string, body string) *http.Request {
	req := adminRequestAs(method, path, userID, sessionID, roles)
	req.Body = newJSONBody(body)
	req.Header.Set("Content-Type", "application/json")
	return req
}

// --- MFA enrol / verify ---

// totpCodeAt reproduces RFC 6238's HOTP computation so the test can produce
// a valid code from a secret it just received from the enrol response,
// without exporting internal/totp's private hotp helper.
func totpCodeAt(secret []byte, at time.Time) string {
	counter := uint64(at.Unix() / 30)
	var buf [8]byte
	binary.BigEndian.PutUint64(buf[:], counter)
	mac := hmac.New(sha1.New, secret)
	mac.Write(buf[:])
	sum := mac.Sum(nil)
	offset := sum[len(sum)-1] & 0x0f
	code := (uint32(sum[offset]&0x7f) << 24) | (uint32(sum[offset+1]) << 16) | (uint32(sum[offset+2]) << 8) | uint32(sum[offset+3])
	return fmt.Sprintf("%06d", code%1_000_000)
}

func TestAdminMFA_EnrollVerifyAndReplay(t *testing.T) {
	pool := dbTestPool(t)
	rdb := dbTestRedis(t)
	f := newAdminHandlerFixture(t, pool, rdb)
	ctx := context.Background()

	// A fresh user with a role but no TOTP secret yet.
	userID := uuid.Must(uuid.NewV7())
	q := f.s.q
	if _, err := q.UpsertUserByEmail(ctx, store.UpsertUserByEmailParams{ID: userID, Email: "mfa-" + userID.String() + "@example.invalid"}); err != nil {
		t.Fatalf("create user: %v", err)
	}
	if _, err := q.GrantRole(ctx, store.GrantRoleParams{UserID: userID, Role: "support"}); err != nil {
		t.Fatalf("grant role: %v", err)
	}
	sess, err := q.CreateSession(ctx, store.CreateSessionParams{
		ID: uuid.Must(uuid.NewV7()), UserID: userID, TokenHash: []byte(userID.String()),
		ExpiresAt: time.Now().Add(time.Hour), UserAgent: "test",
	})
	if err != nil {
		t.Fatalf("create session: %v", err)
	}
	t.Cleanup(func() { _ = q.SoftDeleteUser(context.Background(), userID) })

	// Enrol.
	rec := httptest.NewRecorder()
	f.s.handleAdminMFAEnroll(rec, requestWithSession(userID, sess.ID))
	if rec.Code != http.StatusOK {
		t.Fatalf("enroll: status = %d, body = %s", rec.Code, rec.Body.String())
	}
	var enrollResp struct {
		Secret     string `json:"secret"`
		OtpauthURI string `json:"otpauth_uri"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &enrollResp); err != nil {
		t.Fatalf("decode enroll response: %v", err)
	}
	if !strings.HasPrefix(enrollResp.OtpauthURI, "otpauth://totp/") {
		t.Errorf("otpauth_uri = %q", enrollResp.OtpauthURI)
	}
	secret, err := base32.StdEncoding.WithPadding(base32.NoPadding).DecodeString(enrollResp.Secret)
	if err != nil {
		t.Fatalf("decode secret: %v", err)
	}

	// Enrolling again while unconfirmed is allowed (restart), but re-enrolling
	// after confirmation must 409.
	code := totpCodeAt(secret, time.Now())
	rec = httptest.NewRecorder()
	f.s.handleAdminMFAVerify(rec, jsonRequestNoRoles(t, "/", userID, sess.ID, fmt.Sprintf(`{"code":%q}`, code)))
	if rec.Code != http.StatusNoContent {
		t.Fatalf("verify: status = %d, body = %s", rec.Code, rec.Body.String())
	}

	rec = httptest.NewRecorder()
	f.s.handleAdminMFAEnroll(rec, requestWithSession(userID, sess.ID))
	if rec.Code != http.StatusConflict {
		t.Fatalf("re-enroll after confirm: status = %d, want 409", rec.Code)
	}

	// Replay: the exact same code must now be rejected.
	rec = httptest.NewRecorder()
	f.s.handleAdminMFAVerify(rec, jsonRequestNoRoles(t, "/", userID, sess.ID, fmt.Sprintf(`{"code":%q}`, code)))
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("replayed code: status = %d, want 400", rec.Code)
	}
}

// jsonRequestNoRoles is jsonRequest without a roles value (the MFA handlers
// call loadAdminContext directly, not requireAdmin, so no ctxAdminRoles is
// ever set on the real request path either).
func jsonRequestNoRoles(t *testing.T, path string, userID, sessionID uuid.UUID, body string) *http.Request {
	t.Helper()
	req := requestWithSession(userID, sessionID)
	req = httptest.NewRequest(http.MethodPost, path, newJSONBody(body)).WithContext(req.Context())
	req.Header.Set("Content-Type", "application/json")
	return req
}

// --- user status ---

func TestHandleSetAdminUserStatus_SelfTargetForbidden(t *testing.T) {
	pool := dbTestPool(t)
	rdb := dbTestRedis(t)
	f := newAdminHandlerFixture(t, pool, rdb)

	req := jsonRequest(http.MethodPost, "/", f.superAdminID, f.superAdminSessID, []string{"super_admin"}, `{"status":"suspended","reason":""}`)
	req = withRouteID(req, f.superAdminID)
	rec := httptest.NewRecorder()
	f.s.handleSetAdminUserStatus(rec, req)
	if rec.Code != http.StatusForbidden {
		t.Fatalf("self-target status change: status = %d, body = %s", rec.Code, rec.Body.String())
	}
}

func TestHandleSetAdminUserStatus_ModeratorCannotBanOrActOnRoleHolder(t *testing.T) {
	pool := dbTestPool(t)
	rdb := dbTestRedis(t)
	f := newAdminHandlerFixture(t, pool, rdb)

	// Moderator tries to ban the plain user: banning requires super_admin.
	req := jsonRequest(http.MethodPost, "/", f.moderatorID, f.moderatorSessID, []string{"moderator"}, `{"status":"banned","reason":""}`)
	req = withRouteID(req, f.plainUserID)
	rec := httptest.NewRecorder()
	f.s.handleSetAdminUserStatus(rec, req)
	if rec.Code != http.StatusForbidden {
		t.Fatalf("moderator ban: status = %d, want 403", rec.Code)
	}

	// Moderator tries to suspend the super_admin (a role holder): forbidden.
	req = jsonRequest(http.MethodPost, "/", f.moderatorID, f.moderatorSessID, []string{"moderator"}, `{"status":"suspended","reason":""}`)
	req = withRouteID(req, f.superAdminID)
	rec = httptest.NewRecorder()
	f.s.handleSetAdminUserStatus(rec, req)
	if rec.Code != http.StatusForbidden {
		t.Fatalf("moderator on role holder: status = %d, want 403", rec.Code)
	}
}

func TestHandleSetAdminUserStatus_SuspendRevokesSessionsAndAudits(t *testing.T) {
	pool := dbTestPool(t)
	rdb := dbTestRedis(t)
	f := newAdminHandlerFixture(t, pool, rdb)
	ctx := context.Background()

	req := jsonRequest(http.MethodPost, "/", f.superAdminID, f.superAdminSessID, []string{"super_admin"}, `{"status":"suspended","reason":"testing"}`)
	req = withRouteID(req, f.plainUserID)
	rec := httptest.NewRecorder()
	f.s.handleSetAdminUserStatus(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("suspend: status = %d, body = %s", rec.Code, rec.Body.String())
	}

	row, err := f.s.q.GetUserAdmin(ctx, f.plainUserID)
	if err != nil {
		t.Fatalf("get user admin: %v", err)
	}
	if row.Status != "suspended" {
		t.Errorf("status = %q, want suspended", row.Status)
	}
	if row.ActiveSessions != 0 {
		t.Errorf("active_sessions = %d, want 0 (sessions should be revoked)", row.ActiveSessions)
	}

	auditRows, err := f.s.q.ListAuditForTarget(ctx, store.ListAuditForTargetParams{
		TargetType: "user", TargetID: f.plainUserID.String(), CursorCreatedAt: time.Date(9999, 12, 31, 0, 0, 0, 0, time.UTC),
		CursorID: uuid.Max, Lim: 10,
	})
	if err != nil {
		t.Fatalf("list audit: %v", err)
	}
	found := false
	for _, a := range auditRows {
		if a.Action == "user.status" {
			found = true
		}
	}
	if !found {
		t.Error("expected a user.status audit row")
	}
}

// TestHandleSetAdminUserStatus_ModeratorCannotReverseSuperAdminBan verifies
// fix for §4.11: a moderator who can freely set active/suspended on an
// ordinary user must not be able to reactivate a user a super_admin banned.
// Only checking the *new* status (as the handler used to) misses this: a
// moderator sending {"status":"active"} for a banned target was allowed
// through because "active" isn't "banned".
func TestHandleSetAdminUserStatus_ModeratorCannotReverseSuperAdminBan(t *testing.T) {
	pool := dbTestPool(t)
	rdb := dbTestRedis(t)
	f := newAdminHandlerFixture(t, pool, rdb)
	ctx := context.Background()

	req := jsonRequest(http.MethodPost, "/", f.superAdminID, f.superAdminSessID, []string{"super_admin"}, `{"status":"banned","reason":""}`)
	req = withRouteID(req, f.plainUserID)
	rec := httptest.NewRecorder()
	f.s.handleSetAdminUserStatus(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("super_admin ban: status = %d, body = %s", rec.Code, rec.Body.String())
	}

	// Moderator tries to reverse the ban by setting "active": the new
	// status alone (active) isn't privileged, so this must still be
	// blocked because the *old* status is banned.
	req = jsonRequest(http.MethodPost, "/", f.moderatorID, f.moderatorSessID, []string{"moderator"}, `{"status":"active","reason":""}`)
	req = withRouteID(req, f.plainUserID)
	rec = httptest.NewRecorder()
	f.s.handleSetAdminUserStatus(rec, req)
	if rec.Code != http.StatusForbidden {
		t.Fatalf("moderator reversing ban: status = %d, want 403, body = %s", rec.Code, rec.Body.String())
	}
	row, err := f.s.q.GetUserAdmin(ctx, f.plainUserID)
	if err != nil {
		t.Fatalf("get user admin: %v", err)
	}
	if row.Status != "banned" {
		t.Fatalf("status = %q after blocked reversal, want still banned", row.Status)
	}

	// A super_admin can reverse its own ban decision.
	req = jsonRequest(http.MethodPost, "/", f.superAdminID, f.superAdminSessID, []string{"super_admin"}, `{"status":"active","reason":""}`)
	req = withRouteID(req, f.plainUserID)
	rec = httptest.NewRecorder()
	f.s.handleSetAdminUserStatus(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("super_admin un-banning: status = %d, body = %s", rec.Code, rec.Body.String())
	}
}

// TestHandleSetAdminUserStatus_LastSuperAdminProtected verifies that
// demoting the only active super_admin out of "active" (suspend/ban) is
// blocked the same way revoking their role is, so two concurrent status
// changes can't leave zero active super_admins.
func TestHandleSetAdminUserStatus_LastSuperAdminProtected(t *testing.T) {
	pool := dbTestPool(t)
	rdb := dbTestRedis(t)
	f := newAdminHandlerFixture(t, pool, rdb)
	ctx := context.Background()

	// f.superAdminID is the only super_admin. moderatorID acts with a
	// super_admin context role (as the role tests above do) so the actor
	// itself isn't the last-super_admin question; the target is.
	req := jsonRequest(http.MethodPost, "/", f.moderatorID, f.moderatorSessID, []string{"super_admin"}, `{"status":"suspended","reason":""}`)
	req = withRouteID(req, f.superAdminID)
	rec := httptest.NewRecorder()
	f.s.handleSetAdminUserStatus(rec, req)
	if rec.Code != http.StatusConflict {
		t.Fatalf("suspend last super_admin: status = %d, want 409, body = %s", rec.Code, rec.Body.String())
	}
	row, err := f.s.q.GetUserAdmin(ctx, f.superAdminID)
	if err != nil {
		t.Fatalf("get user admin: %v", err)
	}
	if row.Status != "active" {
		t.Fatalf("status = %q after blocked suspend, want still active", row.Status)
	}

	// Grant super_admin to a second user: now suspending the first must
	// succeed, since one active super_admin remains.
	if _, err := f.s.q.GrantRole(ctx, store.GrantRoleParams{UserID: f.plainUserID, Role: "super_admin"}); err != nil {
		t.Fatalf("grant super_admin: %v", err)
	}
	req = jsonRequest(http.MethodPost, "/", f.moderatorID, f.moderatorSessID, []string{"super_admin"}, `{"status":"suspended","reason":""}`)
	req = withRouteID(req, f.superAdminID)
	rec = httptest.NewRecorder()
	f.s.handleSetAdminUserStatus(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("suspend non-last super_admin: status = %d, body = %s", rec.Code, rec.Body.String())
	}
}

// --- session revoke ---

// TestHandleAdminRevokeUserSessions_RoleHolderForbidden mirrors the
// status-change guard: support/moderator must not be able to force-revoke
// every session of a super_admin (or any role holder); only a super_admin
// may act on a role holder here.
func TestHandleAdminRevokeUserSessions_RoleHolderForbidden(t *testing.T) {
	pool := dbTestPool(t)
	rdb := dbTestRedis(t)
	f := newAdminHandlerFixture(t, pool, rdb)

	req := adminRequestAs(http.MethodPost, "/", f.moderatorID, f.moderatorSessID, []string{"moderator"})
	req = withRouteID(req, f.superAdminID)
	rec := httptest.NewRecorder()
	f.s.handleAdminRevokeUserSessions(rec, req)
	if rec.Code != http.StatusForbidden {
		t.Fatalf("moderator revoking super_admin sessions: status = %d, want 403, body = %s", rec.Code, rec.Body.String())
	}

	// A super_admin may revoke another role holder's sessions.
	req = adminRequestAs(http.MethodPost, "/", f.superAdminID, f.superAdminSessID, []string{"super_admin"})
	req = withRouteID(req, f.moderatorID)
	rec = httptest.NewRecorder()
	f.s.handleAdminRevokeUserSessions(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("super_admin revoking moderator sessions: status = %d, body = %s", rec.Code, rec.Body.String())
	}
}

// --- roles ---

func TestHandleAdminUserRole_LastSuperAdminProtected(t *testing.T) {
	pool := dbTestPool(t)
	rdb := dbTestRedis(t)
	f := newAdminHandlerFixture(t, pool, rdb)

	// f.superAdminID is (in this fixture) the only super_admin: revoking it
	// from itself... but self-target is forbidden, so grant it to the plain
	// user first, then try to revoke it from the *original* super admin while
	// there are now two, which must succeed, and then try to revoke the
	// remaining one, which must 409.
	req := adminRequestAs(http.MethodPost, "/", f.moderatorID, f.moderatorSessID, []string{"super_admin"})
	req = withRouteParam(withRouteID(req, f.plainUserID), "role", "super_admin")
	rec := httptest.NewRecorder()
	f.s.handleAdminUserRole(rec, req)
	if rec.Code != http.StatusNoContent {
		t.Fatalf("grant super_admin to plain user: status = %d, body = %s", rec.Code, rec.Body.String())
	}

	// Now revoke it from the plain user (an actor distinct from both
	// targets, so no self-target 403 muddies the last-admin check): two
	// super_admins exist, so this must succeed.
	req = adminRequestAs(http.MethodDelete, "/", f.moderatorID, f.moderatorSessID, []string{"super_admin"})
	req = withRouteParam(withRouteID(req, f.plainUserID), "role", "super_admin")
	rec = httptest.NewRecorder()
	f.s.handleAdminUserRole(rec, req)
	if rec.Code != http.StatusNoContent {
		t.Fatalf("revoke super_admin from plain user: status = %d, body = %s", rec.Code, rec.Body.String())
	}

	// Only f.superAdminID now holds super_admin: revoking it (acted on by
	// the moderator, a different user, so not a self-target 403) must 409.
	req = adminRequestAs(http.MethodDelete, "/", f.moderatorID, f.moderatorSessID, []string{"super_admin"})
	req = withRouteParam(withRouteID(req, f.superAdminID), "role", "super_admin")
	rec = httptest.NewRecorder()
	f.s.handleAdminUserRole(rec, req)
	if rec.Code != http.StatusConflict {
		t.Fatalf("revoke last super_admin: status = %d, want 409, body = %s", rec.Code, rec.Body.String())
	}
}

func TestHandleAdminUserRole_SelfTargetForbidden(t *testing.T) {
	pool := dbTestPool(t)
	rdb := dbTestRedis(t)
	f := newAdminHandlerFixture(t, pool, rdb)

	req := adminRequestAs(http.MethodPost, "/", f.superAdminID, f.superAdminSessID, []string{"super_admin"})
	req = withRouteParam(withRouteID(req, f.superAdminID), "role", "moderator")
	rec := httptest.NewRecorder()
	f.s.handleAdminUserRole(rec, req)
	if rec.Code != http.StatusForbidden {
		t.Fatalf("self role change: status = %d, want 403", rec.Code)
	}
}

// --- media moderation ---

// TestHandleAdminRejectMedia_EnqueuesMediaVisibilityJob verifies the fix
// for the media reject handler leaking approved-then-rejected files under
// public/: the durable media_visibility takedown must be enqueued inside
// the same tx as the reject, not left to the synchronous delete alone.
func TestHandleAdminRejectMedia_EnqueuesMediaVisibilityJob(t *testing.T) {
	pool := dbTestPool(t)
	rdb := dbTestRedis(t)
	f := newAdminHandlerFixture(t, pool, rdb)
	ctx := context.Background()
	q := f.s.q

	owner := f.plainUserID
	event := createTestEvent(t, ctx, q, "birthday", "classic", &owner)
	t.Cleanup(func() { _, _ = pool.Exec(context.Background(), "DELETE FROM events WHERE id = $1", event.ID) })

	mediaID := uuid.Must(uuid.NewV7())
	if _, err := q.CreateHostMedia(ctx, store.CreateHostMediaParams{
		ID: mediaID, UserID: owner, EventID: event.ID,
		StorageKey: event.ID.String() + "/" + mediaID.String(), SizeBytes: 1024, Width: 10, Height: 10,
	}); err != nil {
		t.Fatalf("create host media: %v", err)
	}

	req := adminRequestAs(http.MethodPost, "/", f.moderatorID, f.moderatorSessID, []string{"moderator"})
	req = withRouteID(req, mediaID)
	rec := httptest.NewRecorder()
	f.s.handleAdminRejectMedia(rec, req)
	if rec.Code != http.StatusOK || !containsAll(rec.Body.String(), `"moderation_status":"rejected"`) {
		t.Fatalf("status = %d, body = %s, want 200 rejected", rec.Code, rec.Body.String())
	}

	var n int
	if err := pool.QueryRow(ctx,
		"SELECT count(*) FROM river_job WHERE kind = 'media_visibility' AND args->>'event_id' = $1", event.ID.String(),
	).Scan(&n); err != nil {
		t.Fatalf("count media_visibility jobs: %v", err)
	}
	if n == 0 {
		t.Error("expected a media_visibility job to be enqueued in the reject tx")
	}
}

// --- reports ---

func TestHandleDismissReport_WritesAudit(t *testing.T) {
	pool := dbTestPool(t)
	rdb := dbTestRedis(t)
	f := newAdminHandlerFixture(t, pool, rdb)
	ctx := context.Background()
	q := f.s.q

	owner := f.plainUserID
	event := createTestEvent(t, ctx, q, "birthday", "classic", &owner)
	t.Cleanup(func() { _, _ = pool.Exec(context.Background(), "DELETE FROM events WHERE id = $1", event.ID) })

	reportID := uuid.Must(uuid.NewV7())
	if _, err := q.CreateReport(ctx, store.CreateReportParams{
		ID: reportID, EventID: event.ID, ReporterIpHash: []byte("abcdefgh"), Reason: "spam", Details: "",
	}); err != nil {
		t.Fatalf("create report: %v", err)
	}

	req := adminRequestAs(http.MethodPost, "/", f.moderatorID, f.moderatorSessID, []string{"moderator"})
	req = withRouteID(req, reportID)
	rec := httptest.NewRecorder()
	f.s.handleDismissReport(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("dismiss: status = %d, body = %s", rec.Code, rec.Body.String())
	}

	auditRows, err := q.ListAuditForTarget(ctx, store.ListAuditForTargetParams{
		TargetType: "report", TargetID: reportID.String(), CursorCreatedAt: time.Date(9999, 12, 31, 0, 0, 0, 0, time.UTC),
		CursorID: uuid.Max, Lim: 10,
	})
	if err != nil {
		t.Fatalf("list audit: %v", err)
	}
	if len(auditRows) != 1 || auditRows[0].Action != "report.dismiss" {
		t.Fatalf("audit rows = %+v", auditRows)
	}
}

// --- templates ---

func TestHandleUpdateAdminTemplate_PublishRequiresPublishedVersion(t *testing.T) {
	pool := dbTestPool(t)
	rdb := dbTestRedis(t)
	f := newAdminHandlerFixture(t, pool, rdb)
	ctx := context.Background()
	q := f.s.q

	templateID := uuid.Must(uuid.NewV7())
	if _, err := q.CreateTemplate(ctx, store.CreateTemplateParams{
		// The full UUID, not a truncated prefix: uuid.NewV7's leading bytes
		// are a millisecond timestamp, so a short prefix collides easily
		// between test runs seconds apart (which is exactly what happened
		// before this fix, as templates_slug_key duplicates).
		ID: templateID, Slug: "test-tpl-" + templateID.String(), Name: "Test", Tags: []byte("{}"), IsPremium: false,
	}); err != nil {
		t.Fatalf("create template: %v", err)
	}
	t.Cleanup(func() {
		// This test publishes a version, and template_versions_immutable
		// forbids ever deleting a published version (by design, §2.1), so
		// the row can't be hard-deleted. Marking it premium instead removes
		// it from every public/host-facing catalog query (ListPublishedTemplates
		// filters NOT is_premium), which is the only pollution that would
		// otherwise leak into other tests/dev-DB use.
		_, _ = pool.Exec(context.Background(), "UPDATE templates SET is_premium = true WHERE id = $1", templateID)
	})

	// No version at all yet: publishing must 409.
	req := jsonRequest(http.MethodPatch, "/", f.superAdminID, f.superAdminSessID, []string{"super_admin"}, `{"status":"published"}`)
	req = withRouteID(req, templateID)
	rec := httptest.NewRecorder()
	f.s.handleUpdateAdminTemplate(rec, req)
	if rec.Code != http.StatusConflict {
		t.Fatalf("publish with no versions: status = %d, body = %s", rec.Code, rec.Body.String())
	}

	// Add a draft version (unpublished): still must 409.
	manifest := testManifestJSON(t)
	req = jsonRequest(http.MethodPost, "/", f.superAdminID, f.superAdminSessID, []string{"super_admin"}, fmt.Sprintf(`{"manifest":%s}`, manifest))
	req = withRouteID(req, templateID)
	rec = httptest.NewRecorder()
	f.s.handleCreateAdminTemplateVersion(rec, req)
	if rec.Code != http.StatusCreated {
		t.Fatalf("create version: status = %d, body = %s", rec.Code, rec.Body.String())
	}

	req = jsonRequest(http.MethodPatch, "/", f.superAdminID, f.superAdminSessID, []string{"super_admin"}, `{"status":"published"}`)
	req = withRouteID(req, templateID)
	rec = httptest.NewRecorder()
	f.s.handleUpdateAdminTemplate(rec, req)
	if rec.Code != http.StatusConflict {
		t.Fatalf("publish with only a draft version: status = %d, body = %s", rec.Code, rec.Body.String())
	}

	// Publish the version itself, then the template-level publish must succeed.
	req = adminRequestAs(http.MethodPost, "/", f.superAdminID, f.superAdminSessID, []string{"super_admin"})
	req = withRouteParam(withRouteID(req, templateID), "v", "1")
	rec = httptest.NewRecorder()
	f.s.handleAdminPublishTemplateVersion(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("publish version: status = %d, body = %s", rec.Code, rec.Body.String())
	}

	req = jsonRequest(http.MethodPatch, "/", f.superAdminID, f.superAdminSessID, []string{"super_admin"}, `{"status":"published"}`)
	req = withRouteID(req, templateID)
	rec = httptest.NewRecorder()
	f.s.handleUpdateAdminTemplate(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("publish with a published version: status = %d, body = %s", rec.Code, rec.Body.String())
	}
}

// TestHandleUploadAdminTemplateBackground_ChecksVersionFirst verifies the
// fix that moves the GetTemplateVersion existence/draft check ahead of any
// filesystem work: a bad version or an already-published one must 404/409
// before an upload is staged or committed, never after.
func TestHandleUploadAdminTemplateBackground_ChecksVersionFirst(t *testing.T) {
	pool := dbTestPool(t)
	rdb := dbTestRedis(t)
	f := newAdminHandlerFixture(t, pool, rdb)
	ctx := context.Background()
	q := f.s.q

	templateID := uuid.Must(uuid.NewV7())
	if _, err := q.CreateTemplate(ctx, store.CreateTemplateParams{
		ID: templateID, Slug: "test-tpl-bg-" + templateID.String(), Name: "Test", Tags: []byte("{}"), IsPremium: false,
	}); err != nil {
		t.Fatalf("create template: %v", err)
	}
	t.Cleanup(func() {
		_, _ = pool.Exec(context.Background(), "UPDATE templates SET is_premium = true WHERE id = $1", templateID)
	})

	// Nonexistent version: 404, no upload processing attempted.
	req := adminRequestAs(http.MethodPost, "/", f.superAdminID, f.superAdminSessID, []string{"super_admin"})
	req = withRouteParam(withRouteID(req, templateID), "v", "99")
	rec := httptest.NewRecorder()
	f.s.handleUploadAdminTemplateBackground(rec, req)
	if rec.Code != http.StatusNotFound {
		t.Fatalf("nonexistent version: status = %d, body = %s, want 404", rec.Code, rec.Body.String())
	}

	// Create and publish version 1.
	manifest := testManifestJSON(t)
	req = jsonRequest(http.MethodPost, "/", f.superAdminID, f.superAdminSessID, []string{"super_admin"}, fmt.Sprintf(`{"manifest":%s}`, manifest))
	req = withRouteID(req, templateID)
	rec = httptest.NewRecorder()
	f.s.handleCreateAdminTemplateVersion(rec, req)
	if rec.Code != http.StatusCreated {
		t.Fatalf("create version: status = %d, body = %s", rec.Code, rec.Body.String())
	}
	req = adminRequestAs(http.MethodPost, "/", f.superAdminID, f.superAdminSessID, []string{"super_admin"})
	req = withRouteParam(withRouteID(req, templateID), "v", "1")
	rec = httptest.NewRecorder()
	f.s.handleAdminPublishTemplateVersion(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("publish version: status = %d, body = %s", rec.Code, rec.Body.String())
	}

	// Published version: 409, before touching the filesystem.
	req = adminRequestAs(http.MethodPost, "/", f.superAdminID, f.superAdminSessID, []string{"super_admin"})
	req = withRouteParam(withRouteID(req, templateID), "v", "1")
	rec = httptest.NewRecorder()
	f.s.handleUploadAdminTemplateBackground(rec, req)
	if rec.Code != http.StatusConflict {
		t.Fatalf("published version upload: status = %d, body = %s, want 409", rec.Code, rec.Body.String())
	}
}

// testManifestJSON returns a minimal, valid manifest per content.ValidateManifest.
func testManifestJSON(t *testing.T) string {
	t.Helper()
	return `{
		"schema": 1, "layout": "centered", "hero_style": "framed", "decoration": "none",
		"palettes": [{"id":"ivory","name":"Ivory","colors":{
			"background":"#FBF8F3","surface":"#FFFFFF","text":"#1F1B16","muted":"#6B645C",
			"accent":"#8C6A3F","accent_text":"#FFFFFF"}}],
		"fonts": [{"id":"classic","name":"Classic","heading":"playfair_display","body":"lora"}],
		"defaults": {"palette":"ivory","font":"classic"},
		"background": null
	}`
}

// --- admin delete / edit user (driven through the real router) ---

// adminAPI drives Routes() with real sessions, so role gating, route wiring
// and the cookie path are all exercised, not just the handler bodies.
type adminAPI struct {
	f                    adminHandlerFixture
	h                    http.Handler
	pool                 *pgxpool.Pool
	superTok, moderatorT string
}

func newAdminAPI(t *testing.T, pool *pgxpool.Pool, rdb *redis.Client) adminAPI {
	t.Helper()
	f := newAdminHandlerFixture(t, pool, rdb)
	a := adminAPI{f: f, h: f.s.Routes(), pool: pool}
	a.superTok = a.login(t, f.superAdminID, true)
	a.moderatorT = a.login(t, f.moderatorID, true)
	return a
}

// login starts a real session and returns its cookie token.
func (a adminAPI) login(t *testing.T, userID uuid.UUID, mfa bool) string {
	t.Helper()
	ctx := context.Background()
	tok, _, err := a.f.s.sessions.Create(ctx, userID, nil, "test")
	if err != nil {
		t.Fatalf("create session: %v", err)
	}
	if mfa {
		sess, err := a.f.s.sessions.Lookup(ctx, tok)
		if err != nil {
			t.Fatalf("lookup session: %v", err)
		}
		if err := a.f.s.q.SetSessionMFAVerified(ctx, store.SetSessionMFAVerifiedParams{SessionID: sess.ID, UserID: userID}); err != nil {
			t.Fatalf("set mfa verified: %v", err)
		}
	}
	return tok
}

// do sends a JSON request (no Content-Type when body is empty).
func (a adminAPI) do(method, path, token, body string) *httptest.ResponseRecorder {
	ct := ""
	if body != "" {
		ct = "application/json"
	}
	return a.doCT(method, path, token, ct, body)
}

func (a adminAPI) doCT(method, path, token, contentType, body string) *httptest.ResponseRecorder {
	req := httptest.NewRequest(method, path, strings.NewReader(body))
	if contentType != "" {
		req.Header.Set("Content-Type", contentType)
	}
	if token != "" {
		req.AddCookie(&http.Cookie{Name: sessionCookie, Value: token})
	}
	rec := httptest.NewRecorder()
	a.h.ServeHTTP(rec, req)
	return rec
}

// newTarget creates an ordinary user with a name; its rows are removed
// afterwards (events first, since events.owner_id is RESTRICT).
func (a adminAPI) newTarget(t *testing.T, label string) (id uuid.UUID, email string) {
	t.Helper()
	ctx := context.Background()
	id = uuid.Must(uuid.NewV7())
	email = label + "-" + id.String() + "@example.test"
	if _, err := a.f.s.q.UpsertUserByEmail(ctx, store.UpsertUserByEmailParams{ID: id, Email: email}); err != nil {
		t.Fatalf("create target: %v", err)
	}
	if _, err := a.f.s.q.UpdateUserName(ctx, store.UpdateUserNameParams{UserID: id, Name: "Target " + label}); err != nil {
		t.Fatalf("name target: %v", err)
	}
	t.Cleanup(func() {
		bg := context.Background()
		_, _ = a.pool.Exec(bg, "DELETE FROM river_job WHERE kind = 'admin_purge_event' AND args->>'event_id' IN (SELECT id::text FROM events WHERE owner_id = $1)", id)
		_, _ = a.pool.Exec(bg, "DELETE FROM events WHERE owner_id = $1", id)
		if _, err := a.pool.Exec(bg, "DELETE FROM users WHERE id = $1", id); err != nil {
			t.Logf("cleanup target %s: %v", id, err)
		}
	})
	return id, email
}

func sessionCacheKey(token string) string {
	sum := sha256.Sum256([]byte(token))
	return "session:" + hex.EncodeToString(sum[:])
}

func (a adminAPI) sessionCount(t *testing.T, userID uuid.UUID) int {
	t.Helper()
	var n int
	if err := a.pool.QueryRow(context.Background(), "SELECT count(*) FROM sessions WHERE user_id = $1", userID).Scan(&n); err != nil {
		t.Fatalf("count sessions: %v", err)
	}
	return n
}

func (a adminAPI) jobCount(t *testing.T, sql string, args ...any) int {
	t.Helper()
	var n int
	if err := a.pool.QueryRow(context.Background(), sql, args...).Scan(&n); err != nil {
		t.Fatalf("count jobs: %v", err)
	}
	return n
}

func (a adminAPI) auditRows(t *testing.T, targetType string, id uuid.UUID) []store.AuditLog {
	t.Helper()
	rows, err := a.f.s.q.ListAuditForTarget(context.Background(), store.ListAuditForTargetParams{
		TargetType: targetType, TargetID: id.String(),
		CursorCreatedAt: time.Date(9999, 12, 31, 0, 0, 0, 0, time.UTC), CursorID: uuid.Max, Lim: 20,
	})
	if err != nil {
		t.Fatalf("list audit: %v", err)
	}
	return rows
}

// exhaust uses up a rate-limit key so the next request is denied.
func (a adminAPI) exhaust(t *testing.T, key string, max int64, window time.Duration) {
	t.Helper()
	for i := int64(0); i < max; i++ {
		if _, err := a.f.s.limiter.Allow(context.Background(), key, max, window); err != nil {
			t.Fatalf("exhaust %s: %v", key, err)
		}
	}
}

func wantStatusCode(t *testing.T, rec *httptest.ResponseRecorder, status int, code string) {
	t.Helper()
	if rec.Code != status {
		t.Fatalf("status = %d, body = %s, want %d", rec.Code, rec.Body.String(), status)
	}
	if code != "" && !strings.Contains(rec.Body.String(), `"`+code+`"`) {
		t.Fatalf("body = %s, want error code %q", rec.Body.String(), code)
	}
}

func TestAdminDeleteUser_Rejections(t *testing.T) {
	pool := dbTestPool(t)
	rdb := dbTestRedis(t)
	a := newAdminAPI(t, pool, rdb)
	ctx := context.Background()

	target, email := a.newTarget(t, "del-reject")
	a.login(t, target, false)
	missing := uuid.Must(uuid.NewV7())

	assertIntact := func(t *testing.T) {
		t.Helper()
		u, err := a.f.s.q.GetUserByID(ctx, target)
		if err != nil || u.Email != email {
			t.Fatalf("target changed: %+v, %v", u, err)
		}
		if a.sessionCount(t, target) != 1 {
			t.Fatal("target sessions were revoked")
		}
	}
	banned, bannedEmail := a.newTarget(t, "del-banned")
	if _, err := pool.Exec(ctx, "UPDATE users SET status = 'banned' WHERE id = $1", banned); err != nil {
		t.Fatalf("ban: %v", err)
	}
	good := `{"confirm_email":"` + email + `"}`
	path := "/v1/admin/users/" + target.String()

	tests := []struct {
		name        string
		token       string
		path        string
		contentType string
		body        string
		status      int
		code        string
	}{
		{"moderator is forbidden", a.moderatorT, path, "application/json", good, http.StatusForbidden, "forbidden"},
		{"self is forbidden", a.superTok, "/v1/admin/users/" + a.f.superAdminID.String(), "application/json", good, http.StatusForbidden, "forbidden"},
		{"malformed id", a.superTok, "/v1/admin/users/not-a-uuid", "application/json", good, http.StatusNotFound, "not_found"},
		{"missing user", a.superTok, "/v1/admin/users/" + missing.String(), "application/json", good, http.StatusNotFound, "not_found"},
		{"role holder", a.superTok, "/v1/admin/users/" + a.f.moderatorID.String(), "application/json", `{"confirm_email":"x@example.test"}`, http.StatusConflict, "has_admin_role"},
		{"banned user", a.superTok, "/v1/admin/users/" + banned.String(), "application/json", `{"confirm_email":"` + bannedEmail + `"}`, http.StatusConflict, "user_banned"},
		{"wrong confirm email", a.superTok, path, "application/json", `{"confirm_email":"someone-else@example.test"}`, http.StatusBadRequest, "confirm_mismatch"},
		{"empty confirm email", a.superTok, path, "application/json", `{"confirm_email":""}`, http.StatusBadRequest, "confirm_mismatch"},
		{"malformed confirm email", a.superTok, path, "application/json", `{"confirm_email":"not an email"}`, http.StatusBadRequest, "confirm_mismatch"},
		{"unknown field", a.superTok, path, "application/json", `{"confirm_email":"` + email + `","force":true}`, http.StatusBadRequest, "invalid_json"},
		{"reason too long", a.superTok, path, "application/json", `{"confirm_email":"` + email + `","reason":"` + strings.Repeat("x", 501) + `"}`, http.StatusBadRequest, "validation_failed"},
		{"not json", a.superTok, path, "text/plain", good, http.StatusUnsupportedMediaType, "unsupported_media_type"},
		{"no body", a.superTok, path, "", "", http.StatusUnsupportedMediaType, "unsupported_media_type"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			wantStatusCode(t, a.doCT(http.MethodDelete, tt.path, tt.token, tt.contentType, tt.body), tt.status, tt.code)
			assertIntact(t)
		})
	}
	// The banned row keeps its address and status, so the ban stays in force.
	if u, err := a.f.s.q.GetUserByID(ctx, banned); err != nil || u.Email != bannedEmail || u.Status != "banned" {
		t.Fatalf("banned user changed: %+v, %v", u, err)
	}
	// The role holder's roles and the moderator's row are untouched too.
	if roles, err := a.f.s.q.ListUserRoles(ctx, a.f.moderatorID); err != nil || len(roles) != 1 {
		t.Fatalf("moderator roles = %v, %v", roles, err)
	}

}

func TestAdminDeleteUser_RateLimited(t *testing.T) {
	pool := dbTestPool(t)
	rdb := dbTestRedis(t)
	a := newAdminAPI(t, pool, rdb)
	target, email := a.newTarget(t, "del-rate")

	a.exhaust(t, "admin:user:delete:"+a.f.superAdminID.String(), 10, time.Hour)
	rec := a.do(http.MethodDelete, "/v1/admin/users/"+target.String(), a.superTok, `{"confirm_email":"`+email+`"}`)
	wantStatusCode(t, rec, http.StatusTooManyRequests, "rate_limited")
	if _, err := a.f.s.q.GetUserByID(context.Background(), target); err != nil {
		t.Fatalf("target deleted despite the limit: %v", err)
	}
}

func TestAdminDeleteUser_Success(t *testing.T) {
	pool := dbTestPool(t)
	rdb := dbTestRedis(t)
	a := newAdminAPI(t, pool, rdb)
	ctx := context.Background()
	q := a.f.s.q

	target, email := a.newTarget(t, "del-ok")
	other, _ := a.newTarget(t, "del-other")
	tok1 := a.login(t, target, false)
	tok2 := a.login(t, target, false)
	for _, tok := range []string{tok1, tok2} {
		if _, err := a.f.s.sessions.Lookup(ctx, tok); err != nil { // fills the Valkey mirror
			t.Fatalf("lookup: %v", err)
		}
	}
	if n, _ := rdb.Exists(ctx, sessionCacheKey(tok1), sessionCacheKey(tok2)).Result(); n != 2 {
		t.Fatalf("cached sessions = %d, want 2", n)
	}

	live := createTestEvent(t, ctx, q, "birthday", "classic", &target)
	if _, err := pool.Exec(ctx, "UPDATE events SET status = 'published', slug = $2, published_at = now() WHERE id = $1", live.ID, "del-ok-"+live.ID.String()[:8]); err != nil {
		t.Fatalf("publish: %v", err)
	}
	softDeleted := createTestEvent(t, ctx, q, "birthday", "classic", &target)
	if _, err := pool.Exec(ctx, "UPDATE events SET deleted_at = now() - interval '2 days' WHERE id = $1", softDeleted.ID); err != nil {
		t.Fatalf("soft delete: %v", err)
	}
	if _, err := pool.Exec(ctx, "INSERT INTO reports (id, event_id, reporter_ip_hash, reason, status) VALUES ($1, $2, $3, 'spam', 'open')",
		uuid.Must(uuid.NewV7()), live.ID, []byte(live.ID.String())); err != nil {
		t.Fatalf("report: %v", err)
	}
	// An event owned by someone else that the target co-hosts must survive.
	cohosted := createTestEvent(t, ctx, q, "birthday", "classic", &other)
	if _, err := pool.Exec(ctx, "INSERT INTO event_members (event_id, user_id, role) VALUES ($1, $2, 'editor')", cohosted.ID, target); err != nil {
		t.Fatalf("membership: %v", err)
	}

	var deletedAtBefore time.Time
	if err := pool.QueryRow(ctx, "SELECT deleted_at FROM events WHERE id = $1", softDeleted.ID).Scan(&deletedAtBefore); err != nil {
		t.Fatalf("read deleted_at: %v", err)
	}

	// Case and surrounding spaces in the confirmation do not matter.
	body := `{"confirm_email":"  ` + strings.ToUpper(email) + ` ","reason":"abuse"}`
	rec := a.do(http.MethodDelete, "/v1/admin/users/"+target.String(), a.superTok, body)
	wantStatusCode(t, rec, http.StatusAccepted, "")
	// Only the live event is queued; the soft-deleted one is already hidden
	// and left to the retention purge.
	if !strings.Contains(rec.Body.String(), `"events_queued":1`) {
		t.Fatalf("body = %s, want events_queued 1", rec.Body.String())
	}

	// The account is a scrubbed placeholder.
	row, err := q.GetUserAdmin(ctx, target)
	if err != nil {
		t.Fatalf("get user admin: %v", err)
	}
	if row.DeletedAt == nil || row.Name != "" || row.Email != "deleted+"+target.String()+"@deleted.invalid" {
		t.Errorf("user not scrubbed: %+v", row)
	}
	if _, err := q.GetUserByID(ctx, target); err == nil {
		t.Error("GetUserByID still finds the deleted user")
	}

	// Sessions: rows, Valkey mirror and the old cookie are all dead.
	if a.sessionCount(t, target) != 0 {
		t.Error("sessions survived")
	}
	if n, _ := rdb.Exists(ctx, sessionCacheKey(tok1), sessionCacheKey(tok2)).Result(); n != 0 {
		t.Errorf("%d Valkey session keys survived", n)
	}
	if _, err := a.f.s.sessions.Lookup(ctx, tok1); !errors.Is(err, auth.ErrNoSession) {
		t.Errorf("old token lookup error = %v, want ErrNoSession", err)
	}
	wantStatusCode(t, a.do(http.MethodGet, "/v1/me", tok1, ""), http.StatusUnauthorized, "")

	// Owned events are soft-deleted (an earlier deleted_at is kept); only the
	// one that was live gets a purge job; the co-hosted event and its owner remain.
	for _, ev := range []struct {
		id   uuid.UUID
		jobs int
	}{{live.ID, 1}, {softDeleted.ID, 0}} {
		var del *time.Time
		if err := pool.QueryRow(ctx, "SELECT deleted_at FROM events WHERE id = $1", ev.id).Scan(&del); err != nil || del == nil {
			t.Errorf("event %s deleted_at = %v, %v", ev.id, del, err)
		}
		if n := a.jobCount(t, "SELECT count(*) FROM river_job WHERE kind = 'admin_purge_event' AND args->>'event_id' = $1", ev.id.String()); n != ev.jobs {
			t.Errorf("purge jobs for %s = %d, want %d", ev.id, n, ev.jobs)
		}
	}
	var del *time.Time
	if err := pool.QueryRow(ctx, "SELECT deleted_at FROM events WHERE id = $1", softDeleted.ID).Scan(&del); err != nil || !del.Equal(deletedAtBefore) {
		t.Errorf("earlier deleted_at replaced: %v, %v", del, err)
	}
	if err := pool.QueryRow(ctx, "SELECT deleted_at FROM events WHERE id = $1", cohosted.ID).Scan(&del); err != nil || del != nil {
		t.Errorf("co-hosted event touched: deleted_at = %v, %v", del, err)
	}
	if n := a.jobCount(t, "SELECT count(*) FROM event_members WHERE user_id = $1", target); n != 0 {
		t.Errorf("memberships left: %d", n)
	}
	if n := a.jobCount(t, "SELECT count(*) FROM river_job WHERE kind = 'admin_purge_event' AND args->>'event_id' = $1", cohosted.ID.String()); n != 0 {
		t.Errorf("purge job queued for a co-hosted event")
	}
	if _, err := q.GetUserByID(ctx, other); err != nil {
		t.Errorf("other user affected: %v", err)
	}

	// Audit: counts and reason, but neither email nor name.
	rows := a.auditRows(t, "user", target)
	var found bool
	for _, r := range rows {
		if r.Action != "user.delete" {
			continue
		}
		found = true
		var before map[string]any
		if err := json.Unmarshal(r.Before, &before); err != nil {
			t.Fatalf("before json: %v", err)
		}
		if before["status"] != "active" || before["owned_events"] != float64(2) || before["open_reports"] != float64(1) {
			t.Errorf("audit before = %s", r.Before)
		}
		if !strings.Contains(string(r.After), `"reason": "abuse"`) {
			t.Errorf("audit after = %s, want the reason", r.After)
		}
		for _, field := range []string{string(r.Before), string(r.After)} {
			if strings.Contains(strings.ToLower(field), strings.ToLower(email)) || strings.Contains(field, "Target del-ok") {
				t.Errorf("audit row leaks PII: %s", field)
			}
		}
	}
	if !found {
		t.Error("no user.delete audit row")
	}

	// The detail endpoint now reports deleted_at; a second delete is 404.
	rec = a.do(http.MethodGet, "/v1/admin/users/"+target.String(), a.superTok, "")
	wantStatusCode(t, rec, http.StatusOK, "")
	var detail struct {
		DeletedAt *time.Time `json:"deleted_at"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &detail); err != nil || detail.DeletedAt == nil {
		t.Errorf("detail deleted_at = %v, %v (body %s)", detail.DeletedAt, err, rec.Body.String())
	}
	wantStatusCode(t, a.do(http.MethodGet, "/v1/admin/users/"+other.String(), a.superTok, ""), http.StatusOK, "")
	wantStatusCode(t, a.do(http.MethodDelete, "/v1/admin/users/"+target.String(), a.superTok, body), http.StatusNotFound, "not_found")

	// The address is free again.
	if _, err := q.UpsertUserByEmail(ctx, store.UpsertUserByEmailParams{ID: uuid.Must(uuid.NewV7()), Email: email}); err != nil {
		t.Errorf("address not reusable after delete: %v", err)
	}
	_, _ = pool.Exec(ctx, "DELETE FROM users WHERE email = $1", email)
}

func TestAdminDeleteUser_NoOwnedEvents(t *testing.T) {
	pool := dbTestPool(t)
	rdb := dbTestRedis(t)
	a := newAdminAPI(t, pool, rdb)
	target, email := a.newTarget(t, "del-none")

	rec := a.do(http.MethodDelete, "/v1/admin/users/"+target.String(), a.superTok, `{"confirm_email":"`+email+`"}`)
	wantStatusCode(t, rec, http.StatusAccepted, "")
	if !strings.Contains(rec.Body.String(), `"events_queued":0`) {
		t.Fatalf("body = %s, want events_queued 0", rec.Body.String())
	}
}

func TestAdminDeleteUser_RoleGrantedConcurrentlyBlocksDelete(t *testing.T) {
	pool := dbTestPool(t)
	rdb := dbTestRedis(t)
	a := newAdminAPI(t, pool, rdb)
	ctx := context.Background()
	target, email := a.newTarget(t, "del-race")

	// A role grant holds the roles lock; the delete must wait for it and then
	// see the role instead of deleting an admin.
	tx, err := pool.Begin(ctx)
	if err != nil {
		t.Fatalf("begin: %v", err)
	}
	defer tx.Rollback(ctx) //nolint:errcheck
	qtx := a.f.s.q.WithTx(tx)
	if err := qtx.LockRoles(ctx); err != nil {
		t.Fatalf("lock roles: %v", err)
	}
	if _, err := qtx.GrantRole(ctx, store.GrantRoleParams{UserID: target, Role: "support"}); err != nil {
		t.Fatalf("grant: %v", err)
	}

	done := make(chan *httptest.ResponseRecorder, 1)
	go func() {
		done <- a.do(http.MethodDelete, "/v1/admin/users/"+target.String(), a.superTok, `{"confirm_email":"`+email+`"}`)
	}()
	select {
	case rec := <-done:
		t.Fatalf("delete finished while the roles lock was held: %d %s", rec.Code, rec.Body.String())
	case <-time.After(300 * time.Millisecond):
	}
	if err := tx.Commit(ctx); err != nil {
		t.Fatalf("commit grant: %v", err)
	}
	wantStatusCode(t, <-done, http.StatusConflict, "has_admin_role")
	if _, err := a.f.s.q.GetUserByID(ctx, target); err != nil {
		t.Fatalf("role holder was deleted: %v", err)
	}
}

func TestAdminUpdateUser_Rejections(t *testing.T) {
	pool := dbTestPool(t)
	rdb := dbTestRedis(t)
	a := newAdminAPI(t, pool, rdb)
	ctx := context.Background()

	target, email := a.newTarget(t, "edit-rej")
	_, takenEmail := a.newTarget(t, "edit-taken")
	missing := uuid.Must(uuid.NewV7())
	deleted, _ := a.newTarget(t, "edit-gone")
	if err := a.f.s.q.SoftDeleteUser(ctx, deleted); err != nil {
		t.Fatalf("soft delete: %v", err)
	}
	path := "/v1/admin/users/" + target.String()

	tests := []struct {
		name        string
		token       string
		path        string
		contentType string
		body        string
		status      int
		code        string
	}{
		{"moderator cannot change email", a.moderatorT, path, "application/json", `{"email":"new@example.org"}`, http.StatusForbidden, "forbidden"},
		{"moderator cannot change email with name", a.moderatorT, path, "application/json", `{"name":"X","email":"new@example.org"}`, http.StatusForbidden, "forbidden"},
		{"moderator cannot edit a role holder", a.moderatorT, "/v1/admin/users/" + a.f.superAdminID.String(), "application/json", `{"name":"Hacked"}`, http.StatusForbidden, "forbidden"},
		{"moderator cannot edit self", a.moderatorT, "/v1/admin/users/" + a.f.moderatorID.String(), "application/json", `{"name":"Me"}`, http.StatusForbidden, "forbidden"},
		{"super admin cannot change a role holder's email", a.superTok, "/v1/admin/users/" + a.f.moderatorID.String(), "application/json", `{"email":"takeover@example.org"}`, http.StatusConflict, "has_admin_role"},
		{"super admin cannot change a role holder's name and email", a.superTok, "/v1/admin/users/" + a.f.moderatorID.String(), "application/json", `{"name":"X","email":"takeover@example.org"}`, http.StatusConflict, "has_admin_role"},
		{"super admin cannot edit self", a.superTok, "/v1/admin/users/" + a.f.superAdminID.String(), "application/json", `{"name":"Me"}`, http.StatusForbidden, "forbidden"},
		{"malformed id", a.superTok, "/v1/admin/users/nope", "application/json", `{"name":"X"}`, http.StatusNotFound, "not_found"},
		{"missing user", a.superTok, "/v1/admin/users/" + missing.String(), "application/json", `{"name":"X"}`, http.StatusNotFound, "not_found"},
		{"deleted user", a.superTok, "/v1/admin/users/" + deleted.String(), "application/json", `{"name":"X"}`, http.StatusNotFound, "not_found"},
		{"empty object", a.superTok, path, "application/json", `{}`, http.StatusBadRequest, "validation_failed"},
		{"unknown field", a.superTok, path, "application/json", `{"name":"X","status":"banned"}`, http.StatusBadRequest, "invalid_json"},
		{"empty name", a.superTok, path, "application/json", `{"name":"   "}`, http.StatusBadRequest, "validation_failed"},
		{"name too long", a.superTok, path, "application/json", `{"name":"` + strings.Repeat("a", 81) + `"}`, http.StatusBadRequest, "validation_failed"},
		{"name with control character", a.superTok, path, "application/json", `{"name":"a\u0007b"}`, http.StatusBadRequest, "validation_failed"},
		{"null name and email", a.superTok, path, "application/json", `{"name":null,"email":null}`, http.StatusBadRequest, "validation_failed"},
		{"malformed email", a.superTok, path, "application/json", `{"email":"nope"}`, http.StatusBadRequest, "validation_failed"},
		{"email with display name", a.superTok, path, "application/json", `{"email":"A <a@example.org>"}`, http.StatusBadRequest, "validation_failed"},
		{"email too long", a.superTok, path, "application/json", `{"email":"` + strings.Repeat("a", 250) + `@example.org"}`, http.StatusBadRequest, "validation_failed"},
		{"invalid TLD placeholder domain", a.superTok, path, "application/json", `{"email":"deleted+x@deleted.invalid"}`, http.StatusBadRequest, "validation_failed"},
		{"invalid TLD in any case", a.superTok, path, "application/json", `{"email":"X@Example.INVALID"}`, http.StatusBadRequest, "validation_failed"},
		{"email taken", a.superTok, path, "application/json", `{"email":"` + takenEmail + `"}`, http.StatusConflict, "email_taken"},
		{"email taken in other case", a.superTok, path, "application/json", `{"email":"` + strings.ToUpper(takenEmail) + `"}`, http.StatusConflict, "email_taken"},
		{"not json", a.superTok, path, "text/plain", `{"name":"X"}`, http.StatusUnsupportedMediaType, "unsupported_media_type"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			wantStatusCode(t, a.doCT(http.MethodPatch, tt.path, tt.token, tt.contentType, tt.body), tt.status, tt.code)
		})
	}

	u, err := a.f.s.q.GetUserByID(ctx, target)
	if err != nil || u.Email != email || u.Name != "Target edit-rej" {
		t.Fatalf("target changed by a rejected edit: %+v, %v", u, err)
	}
	if mod, err := a.f.s.q.GetUserByID(ctx, a.f.moderatorID); err != nil || mod.Name == "X" || strings.HasPrefix(mod.Email, "takeover") {
		t.Fatalf("role holder changed by a rejected edit: %+v, %v", mod, err)
	}
	if len(a.auditRows(t, "user", target)) != 0 {
		t.Error("rejected edits wrote audit rows")
	}
}

func TestAdminUpdateUser_RateLimited(t *testing.T) {
	pool := dbTestPool(t)
	rdb := dbTestRedis(t)
	a := newAdminAPI(t, pool, rdb)
	target, _ := a.newTarget(t, "edit-rate")
	path := "/v1/admin/users/" + target.String()

	t.Run("per target email limit", func(t *testing.T) {
		a.exhaust(t, "admin:user:email:target:"+target.String(), 3, 24*time.Hour)
		wantStatusCode(t, a.do(http.MethodPatch, path, a.superTok, `{"email":"rate1@example.org"}`), http.StatusTooManyRequests, "rate_limited")
		// A name-only edit is not subject to the email limits.
		wantStatusCode(t, a.do(http.MethodPatch, path, a.superTok, `{"name":"Still Fine"}`), http.StatusOK, "")
	})
	t.Run("per admin email limit", func(t *testing.T) {
		other, _ := a.newTarget(t, "edit-rate2")
		a.exhaust(t, "admin:user:email:"+a.f.superAdminID.String(), 10, time.Hour)
		wantStatusCode(t, a.do(http.MethodPatch, "/v1/admin/users/"+other.String(), a.superTok, `{"email":"rate2@example.org"}`), http.StatusTooManyRequests, "rate_limited")
	})
	t.Run("edit limit", func(t *testing.T) {
		a.exhaust(t, "admin:user:edit:"+a.f.moderatorID.String(), 60, time.Hour)
		wantStatusCode(t, a.do(http.MethodPatch, path, a.moderatorT, `{"name":"Nope"}`), http.StatusTooManyRequests, "rate_limited")
	})
}

func TestAdminUpdateUser_ModeratorEditsName(t *testing.T) {
	pool := dbTestPool(t)
	rdb := dbTestRedis(t)
	a := newAdminAPI(t, pool, rdb)
	ctx := context.Background()
	target, email := a.newTarget(t, "edit-name")
	tok := a.login(t, target, false)

	rec := a.do(http.MethodPatch, "/v1/admin/users/"+target.String(), a.moderatorT, `{"name":"  New Name  "}`)
	wantStatusCode(t, rec, http.StatusOK, "")
	var resp struct {
		User adminUserSummaryResp `json:"user"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &resp); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if resp.User.ID != target || resp.User.Name != "New Name" || resp.User.Email != email {
		t.Errorf("response user = %+v", resp.User)
	}
	// A name edit leaves sessions and mail alone.
	if a.sessionCount(t, target) != 1 {
		t.Error("name edit revoked sessions")
	}
	if _, err := a.f.s.sessions.Lookup(ctx, tok); err != nil {
		t.Errorf("session lookup after name edit: %v", err)
	}
	if n := a.jobCount(t, "SELECT count(*) FROM river_job WHERE kind = 'send_email' AND args->>'to' = $1", email); n != 0 {
		t.Errorf("name edit queued %d emails", n)
	}
	rows := a.auditRows(t, "user", target)
	if len(rows) != 1 || rows[0].Action != "user.update" || string(rows[0].After) != `{"fields": ["name"]}` {
		t.Fatalf("audit rows = %+v", rows)
	}
	if strings.Contains(string(rows[0].After), "New Name") || strings.Contains(string(rows[0].After), email) {
		t.Errorf("audit leaks PII: %s", rows[0].After)
	}
	if rows[0].ActorID == nil || *rows[0].ActorID != a.f.moderatorID {
		t.Errorf("audit actor = %v, want the moderator", rows[0].ActorID)
	}

	// A super admin may edit a role holder.
	wantStatusCode(t, a.do(http.MethodPatch, "/v1/admin/users/"+a.f.moderatorID.String(), a.superTok, `{"name":"Mod Renamed"}`), http.StatusOK, "")
}

func TestAdminUpdateUser_EmailChangeOnRoleHolderRacesGrant(t *testing.T) {
	pool := dbTestPool(t)
	rdb := dbTestRedis(t)
	a := newAdminAPI(t, pool, rdb)
	ctx := context.Background()
	target, email := a.newTarget(t, "edit-race")

	// A role grant holds the roles lock; the email change must wait for it and
	// then see the role instead of re-pointing an admin's account.
	tx, err := pool.Begin(ctx)
	if err != nil {
		t.Fatalf("begin: %v", err)
	}
	defer tx.Rollback(ctx) //nolint:errcheck
	qtx := a.f.s.q.WithTx(tx)
	if err := qtx.LockRoles(ctx); err != nil {
		t.Fatalf("lock roles: %v", err)
	}
	if _, err := qtx.GrantRole(ctx, store.GrantRoleParams{UserID: target, Role: "support"}); err != nil {
		t.Fatalf("grant: %v", err)
	}
	done := make(chan *httptest.ResponseRecorder, 1)
	go func() {
		done <- a.do(http.MethodPatch, "/v1/admin/users/"+target.String(), a.superTok, `{"email":"race-`+target.String()+`@example.org"}`)
	}()
	select {
	case rec := <-done:
		t.Fatalf("edit finished while the roles lock was held: %d %s", rec.Code, rec.Body.String())
	case <-time.After(300 * time.Millisecond):
	}
	if err := tx.Commit(ctx); err != nil {
		t.Fatalf("commit grant: %v", err)
	}
	wantStatusCode(t, <-done, http.StatusConflict, "has_admin_role")
	if u, err := a.f.s.q.GetUserByID(ctx, target); err != nil || u.Email != email {
		t.Fatalf("role holder's email changed: %+v, %v", u, err)
	}
}

func TestAdminUpdateUser_NoOpWritesNothing(t *testing.T) {
	pool := dbTestPool(t)
	rdb := dbTestRedis(t)
	a := newAdminAPI(t, pool, rdb)
	ctx := context.Background()
	target, email := a.newTarget(t, "edit-noop")
	a.login(t, target, false)
	before, err := a.f.s.q.GetUserByID(ctx, target)
	if err != nil {
		t.Fatalf("get user: %v", err)
	}

	body := `{"name":"` + before.Name + `","email":"  ` + strings.ToUpper(email) + `"}`
	wantStatusCode(t, a.do(http.MethodPatch, "/v1/admin/users/"+target.String(), a.superTok, body), http.StatusOK, "")

	after, err := a.f.s.q.GetUserByID(ctx, target)
	if err != nil {
		t.Fatalf("get user: %v", err)
	}
	if !after.UpdatedAt.Equal(before.UpdatedAt) {
		t.Error("no-op edit touched updated_at")
	}
	if a.sessionCount(t, target) != 1 {
		t.Error("no-op edit revoked sessions")
	}
	if len(a.auditRows(t, "user", target)) != 0 {
		t.Error("no-op edit wrote an audit row")
	}
	if n := a.jobCount(t, "SELECT count(*) FROM river_job WHERE kind = 'send_email' AND args->>'to' = $1", email); n != 0 {
		t.Errorf("no-op edit queued %d emails", n)
	}
}

func TestAdminUpdateUser_EmailChange(t *testing.T) {
	pool := dbTestPool(t)
	rdb := dbTestRedis(t)
	a := newAdminAPI(t, pool, rdb)
	ctx := context.Background()
	target, oldEmail := a.newTarget(t, "edit-mail")
	tok1, tok2 := a.login(t, target, false), a.login(t, target, false)
	for _, tok := range []string{tok1, tok2} {
		if _, err := a.f.s.sessions.Lookup(ctx, tok); err != nil {
			t.Fatalf("lookup: %v", err)
		}
	}
	newEmail := "changed-" + target.String() + "@example.org"
	t.Cleanup(func() {
		_, _ = pool.Exec(context.Background(), "DELETE FROM river_job WHERE kind = 'send_email' AND args->>'to' = $1", oldEmail)
	})

	rec := a.do(http.MethodPatch, "/v1/admin/users/"+target.String(), a.superTok, `{"name":"Renamed","email":"  `+strings.ToUpper(newEmail)+`"}`)
	wantStatusCode(t, rec, http.StatusOK, "")
	var resp struct {
		User adminUserSummaryResp `json:"user"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &resp); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if resp.User.Email != newEmail || resp.User.Name != "Renamed" {
		t.Errorf("response user = %+v, want email %q (lower-cased)", resp.User, newEmail)
	}

	// Every session is gone, rows and Valkey mirror alike.
	if a.sessionCount(t, target) != 0 {
		t.Error("sessions survived an email change")
	}
	if n, _ := rdb.Exists(ctx, sessionCacheKey(tok1), sessionCacheKey(tok2)).Result(); n != 0 {
		t.Errorf("%d Valkey session keys survived", n)
	}
	if _, err := a.f.s.sessions.Lookup(ctx, tok1); !errors.Is(err, auth.ErrNoSession) {
		t.Errorf("old token lookup error = %v, want ErrNoSession", err)
	}

	// Exactly one notice, to the OLD address, and the new one appears nowhere in it.
	if n := a.jobCount(t, "SELECT count(*) FROM river_job WHERE kind = 'send_email' AND args->>'to' = $1", oldEmail); n != 1 {
		t.Fatalf("notices to the old address = %d, want 1", n)
	}
	if n := a.jobCount(t, "SELECT count(*) FROM river_job WHERE kind = 'send_email' AND args::text ILIKE '%' || $1 || '%'", newEmail); n != 0 {
		t.Errorf("%d queued emails mention the new address", n)
	}
	if n := a.jobCount(t, "SELECT count(*) FROM river_job WHERE kind = 'send_email' AND args->>'to' = $1", newEmail); n != 0 {
		t.Errorf("%d emails queued to the new address", n)
	}

	rows := a.auditRows(t, "user", target)
	if len(rows) != 1 || rows[0].Action != "user.update" || string(rows[0].After) != `{"fields": ["name", "email"]}` {
		t.Fatalf("audit rows = %+v", rows)
	}
	for _, field := range []string{string(rows[0].Before), string(rows[0].After)} {
		if strings.Contains(field, oldEmail) || strings.Contains(field, newEmail) || strings.Contains(field, "Renamed") {
			t.Errorf("audit leaks PII: %s", field)
		}
	}

	// The old address is free for others; the new one signs in as this user.
	u, err := a.f.s.q.GetUserByID(ctx, target)
	if err != nil || u.Email != newEmail {
		t.Fatalf("stored user = %+v, %v", u, err)
	}
}
