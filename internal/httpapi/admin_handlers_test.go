package httpapi

import (
	"context"
	"crypto/hmac"
	"crypto/sha1"
	"encoding/base32"
	"encoding/binary"
	"encoding/json"
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
	"github.com/ansonarose/seeyouthere-backend/internal/media"
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
	mediaStore, err := media.NewStore(t.TempDir())
	if err != nil {
		t.Fatalf("new media store: %v", err)
	}
	tokens, err := token.NewKeys([]byte("0123456789012345678901234567890123456789"))
	if err != nil {
		t.Fatalf("new token keys: %v", err)
	}
	limiter := ratelimit.New(rdb)
	jobClient, err := jobs.NewClient(pool, noopSender{}, noopSender{}, q, mediaStore, tokens, limiter, "", "https://seeyouthere.at")
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
