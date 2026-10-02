package httpapi

import (
	"context"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/ansonarose/seeyouthere-backend/internal/store"
)

// --- GET /v1/public/events/{slug}: gate matrix (§4.4, §16) ---

func TestHandleGetPublicEvent_GateMatrix(t *testing.T) {
	pool := dbTestPool(t)
	rdb := dbTestRedis(t)

	t.Run("unknown slug is 404", func(t *testing.T) {
		f := newPublicEventFixture(t, pool, rdb, publicEventOpts{})
		req := requestWithIP(http.MethodGet, "/", nextTestIP(), nil)
		req = withSlugParam(req, "no-such-event-at-all")
		rec := httptest.NewRecorder()
		f.s.handleGetPublicEvent(rec, req)
		if rec.Code != http.StatusNotFound {
			t.Fatalf("status = %d, want 404; body = %s", rec.Code, rec.Body.String())
		}
	})

	t.Run("unpublished draft is 404 even with the right slug", func(t *testing.T) {
		f := newPublicEventFixture(t, pool, rdb, publicEventOpts{unpublished: true})
		req := requestWithIP(http.MethodGet, "/", nextTestIP(), nil)
		req = withSlugParam(req, f.slug)
		rec := httptest.NewRecorder()
		f.s.handleGetPublicEvent(rec, req)
		if rec.Code != http.StatusNotFound {
			t.Fatalf("status = %d, want 404; body = %s", rec.Code, rec.Body.String())
		}
	})

	t.Run("public event is visible with no gate", func(t *testing.T) {
		f := newPublicEventFixture(t, pool, rdb, publicEventOpts{visibility: "public"})
		req := requestWithIP(http.MethodGet, "/", nextTestIP(), nil)
		req = withSlugParam(req, f.slug)
		rec := httptest.NewRecorder()
		f.s.handleGetPublicEvent(rec, req)
		if rec.Code != http.StatusOK {
			t.Fatalf("status = %d, want 200; body = %s", rec.Code, rec.Body.String())
		}
	})

	t.Run("unlisted event is visible with no gate (unguessable URL, not indexable)", func(t *testing.T) {
		f := newPublicEventFixture(t, pool, rdb, publicEventOpts{visibility: "unlisted"})
		req := requestWithIP(http.MethodGet, "/", nextTestIP(), nil)
		req = withSlugParam(req, f.slug)
		rec := httptest.NewRecorder()
		f.s.handleGetPublicEvent(rec, req)
		if rec.Code != http.StatusOK {
			t.Fatalf("status = %d, want 200; body = %s", rec.Code, rec.Body.String())
		}
		if !containsAll(rec.Body.String(), `"indexable":false`) {
			t.Errorf("body = %s, want indexable:false for unlisted", rec.Body.String())
		}
	})

	t.Run("password event without cookie is 401", func(t *testing.T) {
		f := newPublicEventFixture(t, pool, rdb, publicEventOpts{password: "correct-horse-battery"})
		req := requestWithIP(http.MethodGet, "/", nextTestIP(), nil)
		req = withSlugParam(req, f.slug)
		rec := httptest.NewRecorder()
		f.s.handleGetPublicEvent(rec, req)
		if rec.Code != http.StatusUnauthorized || !containsAll(rec.Body.String(), `"password_required"`) {
			t.Fatalf("status = %d, body = %s, want 401 password_required", rec.Code, rec.Body.String())
		}
	})

	t.Run("password event with correct cookie is 200", func(t *testing.T) {
		f := newPublicEventFixture(t, pool, rdb, publicEventOpts{password: "correct-horse-battery"})
		ctx := context.Background()
		row, err := f.s.q.GetPublicEventBySlug(ctx, &f.slug)
		if err != nil {
			t.Fatalf("get public event: %v", err)
		}
		cookieVal := f.s.tokens.AccessCookie(f.eventID, *row.PasswordHash, time.Now().Add(accessCookieTTL))
		req := requestWithIP(http.MethodGet, "/", nextTestIP(), nil)
		req = withSlugParam(req, f.slug)
		req.AddCookie(&http.Cookie{Name: eventCookieName(pwCookiePrefix, f.eventID), Value: cookieVal})
		rec := httptest.NewRecorder()
		f.s.handleGetPublicEvent(rec, req)
		if rec.Code != http.StatusOK {
			t.Fatalf("status = %d, want 200; body = %s", rec.Code, rec.Body.String())
		}
	})

	t.Run("password event with tampered cookie is 401", func(t *testing.T) {
		f := newPublicEventFixture(t, pool, rdb, publicEventOpts{password: "correct-horse-battery"})
		ctx := context.Background()
		row, err := f.s.q.GetPublicEventBySlug(ctx, &f.slug)
		if err != nil {
			t.Fatalf("get public event: %v", err)
		}
		cookieVal := f.s.tokens.AccessCookie(f.eventID, *row.PasswordHash, time.Now().Add(accessCookieTTL))
		tampered := []byte(cookieVal)
		tampered[0] ^= 0x01
		req := requestWithIP(http.MethodGet, "/", nextTestIP(), nil)
		req = withSlugParam(req, f.slug)
		req.AddCookie(&http.Cookie{Name: eventCookieName(pwCookiePrefix, f.eventID), Value: string(tampered)})
		rec := httptest.NewRecorder()
		f.s.handleGetPublicEvent(rec, req)
		if rec.Code != http.StatusUnauthorized {
			t.Fatalf("status = %d, want 401; body = %s", rec.Code, rec.Body.String())
		}
	})

	t.Run("password event cookie invalidated by password change", func(t *testing.T) {
		f := newPublicEventFixture(t, pool, rdb, publicEventOpts{password: "correct-horse-battery"})
		ctx := context.Background()
		row, err := f.s.q.GetPublicEventBySlug(ctx, &f.slug)
		if err != nil {
			t.Fatalf("get public event: %v", err)
		}
		cookieVal := f.s.tokens.AccessCookie(f.eventID, *row.PasswordHash, time.Now().Add(accessCookieTTL))

		newHash, err := f.s.hasher.Hash(ctx, "a-completely-different-password")
		if err != nil {
			t.Fatalf("hash: %v", err)
		}
		var currentVersion int32
		if err := f.s.pool.QueryRow(ctx, "SELECT version FROM events WHERE id = $1", f.eventID).Scan(&currentVersion); err != nil {
			t.Fatalf("read current version: %v", err)
		}
		visibility := "password"
		if _, err := f.s.q.UpdateEventSettings(ctx, store.UpdateEventSettingsParams{
			Visibility: &visibility, PasswordHash: &newHash, EventID: f.eventID, UserID: f.ownerID, Version: currentVersion,
		}); err != nil {
			t.Fatalf("change password: %v", err)
		}

		req := requestWithIP(http.MethodGet, "/", nextTestIP(), nil)
		req = withSlugParam(req, f.slug)
		req.AddCookie(&http.Cookie{Name: eventCookieName(pwCookiePrefix, f.eventID), Value: cookieVal})
		rec := httptest.NewRecorder()
		f.s.handleGetPublicEvent(rec, req)
		if rec.Code != http.StatusUnauthorized {
			t.Fatalf("status = %d, want 401 (old cookie must be invalidated by the password change); body = %s", rec.Code, rec.Body.String())
		}
	})

	t.Run("invite_only without cookie is 403", func(t *testing.T) {
		f := newPublicEventFixture(t, pool, rdb, publicEventOpts{visibility: "invite_only"})
		req := requestWithIP(http.MethodGet, "/", nextTestIP(), nil)
		req = withSlugParam(req, f.slug)
		rec := httptest.NewRecorder()
		f.s.handleGetPublicEvent(rec, req)
		if rec.Code != http.StatusForbidden || !containsAll(rec.Body.String(), `"invite_required"`) {
			t.Fatalf("status = %d, body = %s, want 403 invite_required", rec.Code, rec.Body.String())
		}
	})

	t.Run("invite_only with a valid guest cookie is 200", func(t *testing.T) {
		f := newPublicEventFixture(t, pool, rdb, publicEventOpts{visibility: "invite_only"})
		_, inviteToken := f.testGuest(t, "Priya", 2)
		req := requestWithIP(http.MethodGet, "/", nextTestIP(), nil)
		req = withSlugParam(req, f.slug)
		req.AddCookie(&http.Cookie{Name: eventCookieName(inviteCookiePrefix, f.eventID), Value: inviteToken})
		rec := httptest.NewRecorder()
		f.s.handleGetPublicEvent(rec, req)
		if rec.Code != http.StatusOK {
			t.Fatalf("status = %d, want 200; body = %s", rec.Code, rec.Body.String())
		}
		if !containsAll(rec.Body.String(), `"name":"Priya"`, `"household_size":2`) {
			t.Errorf("body = %s, want viewer.guest populated", rec.Body.String())
		}
	})

	t.Run("invite_only with a rotated (stale) guest cookie is 403", func(t *testing.T) {
		f := newPublicEventFixture(t, pool, rdb, publicEventOpts{visibility: "invite_only"})
		guestID, staleToken := f.testGuest(t, "Priya", 2)
		ctx := context.Background()
		if _, err := f.s.q.RotateGuestToken(ctx, store.RotateGuestTokenParams{GuestID: guestID, EventID: f.eventID, UserID: f.ownerID}); err != nil {
			t.Fatalf("rotate guest token: %v", err)
		}
		req := requestWithIP(http.MethodGet, "/", nextTestIP(), nil)
		req = withSlugParam(req, f.slug)
		req.AddCookie(&http.Cookie{Name: eventCookieName(inviteCookiePrefix, f.eventID), Value: staleToken})
		rec := httptest.NewRecorder()
		f.s.handleGetPublicEvent(rec, req)
		if rec.Code != http.StatusForbidden {
			t.Fatalf("status = %d, want 403 (rotated token must stop working); body = %s", rec.Code, rec.Body.String())
		}
	})

	t.Run("invite_only with another event's guest cookie is 403", func(t *testing.T) {
		f1 := newPublicEventFixture(t, pool, rdb, publicEventOpts{visibility: "invite_only"})
		f2 := newPublicEventFixture(t, pool, rdb, publicEventOpts{visibility: "invite_only"})
		_, otherToken := f2.testGuest(t, "Cross Event Guest", 1)

		req := requestWithIP(http.MethodGet, "/", nextTestIP(), nil)
		req = withSlugParam(req, f1.slug)
		// Attached under f1's cookie name (that's what a real browser would
		// send for this event) but signed for f2's guest.
		req.AddCookie(&http.Cookie{Name: eventCookieName(inviteCookiePrefix, f1.eventID), Value: otherToken})
		rec := httptest.NewRecorder()
		f1.s.handleGetPublicEvent(rec, req)
		if rec.Code != http.StatusForbidden {
			t.Fatalf("status = %d, want 403; body = %s", rec.Code, rec.Body.String())
		}
	})
}

// --- POST /v1/public/invites/accept (§16: token round trip, tamper, wrong version, uniform 404) ---

func TestHandleAcceptInvite(t *testing.T) {
	pool := dbTestPool(t)
	rdb := dbTestRedis(t)
	f := newPublicEventFixture(t, pool, rdb, publicEventOpts{visibility: "invite_only"})
	_, validToken := f.testGuest(t, "Priya", 2)

	post := func(ip, body string) *httptest.ResponseRecorder {
		req := requestWithIP(http.MethodPost, "/", ip, []byte(body))
		req.Header.Set("Content-Type", "application/json")
		rec := httptest.NewRecorder()
		f.s.handleAcceptInvite(rec, req)
		return rec
	}

	t.Run("valid token succeeds and sets the cookie", func(t *testing.T) {
		rec := post(nextTestIP(), `{"token":"`+validToken+`"}`)
		if rec.Code != http.StatusOK || !containsAll(rec.Body.String(), `"slug":"`+f.slug+`"`) {
			t.Fatalf("status = %d, body = %s, want 200 with slug", rec.Code, rec.Body.String())
		}
		found := false
		for _, c := range rec.Result().Cookies() {
			if c.Name == eventCookieName(inviteCookiePrefix, f.eventID) && c.Value == validToken {
				found = true
			}
		}
		if !found {
			t.Error("expected the invite cookie to be set to the accepted token")
		}
	})

	t.Run("malformed token is a uniform 404", func(t *testing.T) {
		rec := post(nextTestIP(), `{"token":"not-a-real-token"}`)
		if rec.Code != http.StatusNotFound || !containsAll(rec.Body.String(), `"invalid_link"`) {
			t.Fatalf("status = %d, body = %s, want 404 invalid_link", rec.Code, rec.Body.String())
		}
	})

	t.Run("tampered token is 404", func(t *testing.T) {
		tampered := []byte(validToken)
		tampered[len(tampered)-1] ^= 0x01
		rec := post(nextTestIP(), `{"token":"`+string(tampered)+`"}`)
		if rec.Code != http.StatusNotFound {
			t.Fatalf("status = %d, want 404; body = %s", rec.Code, rec.Body.String())
		}
	})

	t.Run("rotated (stale) token is 404", func(t *testing.T) {
		guestID, staleToken := f.testGuest(t, "Rotated Guest", 1)
		ctx := context.Background()
		if _, err := f.s.q.RotateGuestToken(ctx, store.RotateGuestTokenParams{GuestID: guestID, EventID: f.eventID, UserID: f.ownerID}); err != nil {
			t.Fatalf("rotate: %v", err)
		}
		rec := post(nextTestIP(), `{"token":"`+staleToken+`"}`)
		if rec.Code != http.StatusNotFound {
			t.Fatalf("status = %d, want 404 for a rotated token; body = %s", rec.Code, rec.Body.String())
		}
	})

	t.Run("rate limited after 30 requests in the window", func(t *testing.T) {
		ip := nextTestIP()
		var last *httptest.ResponseRecorder
		for i := 0; i < 31; i++ {
			last = post(ip, `{"token":"not-a-real-token"}`)
		}
		if last.Code != http.StatusTooManyRequests {
			t.Fatalf("31st request status = %d, want 429", last.Code)
		}
	})
}

// --- POST /v1/public/rsvp-links/accept ---

func TestHandleAcceptRSVPLink(t *testing.T) {
	pool := dbTestPool(t)
	rdb := dbTestRedis(t)
	f := newPublicEventFixture(t, pool, rdb, publicEventOpts{})
	ctx := context.Background()

	openRSVP, err := f.s.q.CreateOpenRSVP(ctx, store.CreateOpenRSVPParams{
		ID: uuid.Must(uuid.NewV7()), EventID: f.eventID, Name: "Open RSVP", Attending: "yes", Count: 1, Answers: []byte("{}"),
	})
	if err != nil {
		t.Fatalf("create open rsvp: %v", err)
	}
	validToken := f.s.tokens.RSVPToken(openRSVP.ID, openRSVP.EditTokenVersion)

	guestID, _ := f.testGuest(t, "Guest With RSVP", 2)
	guestRSVP, err := f.s.q.UpsertGuestRSVP(ctx, store.UpsertGuestRSVPParams{
		ID: uuid.Must(uuid.NewV7()), EventID: f.eventID, GuestID: guestID, Name: "Guest With RSVP", Attending: "yes", Count: 1, Answers: []byte("{}"),
	})
	if err != nil {
		t.Fatalf("create guest rsvp: %v", err)
	}
	guestOwnedToken := f.s.tokens.RSVPToken(guestRSVP.ID, guestRSVP.EditTokenVersion)

	post := func(ip, body string) *httptest.ResponseRecorder {
		req := requestWithIP(http.MethodPost, "/", ip, []byte(body))
		req.Header.Set("Content-Type", "application/json")
		rec := httptest.NewRecorder()
		f.s.handleAcceptRSVPLink(rec, req)
		return rec
	}

	t.Run("valid open-RSVP token succeeds", func(t *testing.T) {
		rec := post(nextTestIP(), `{"token":"`+validToken+`"}`)
		if rec.Code != http.StatusOK || !containsAll(rec.Body.String(), `"slug":"`+f.slug+`"`) {
			t.Fatalf("status = %d, body = %s, want 200 with slug", rec.Code, rec.Body.String())
		}
	})

	t.Run("a guest-owned RSVP's token is rejected here (the guest cookie is authoritative for it)", func(t *testing.T) {
		rec := post(nextTestIP(), `{"token":"`+guestOwnedToken+`"}`)
		if rec.Code != http.StatusNotFound {
			t.Fatalf("status = %d, want 404; body = %s", rec.Code, rec.Body.String())
		}
	})

	t.Run("malformed token is a uniform 404", func(t *testing.T) {
		rec := post(nextTestIP(), `{"token":"garbage"}`)
		if rec.Code != http.StatusNotFound || !containsAll(rec.Body.String(), `"invalid_link"`) {
			t.Fatalf("status = %d, body = %s, want 404 invalid_link", rec.Code, rec.Body.String())
		}
	})
}

// --- POST /v1/public/events/{slug}/unlock ---

func TestHandleUnlockPublicEvent(t *testing.T) {
	pool := dbTestPool(t)
	rdb := dbTestRedis(t)

	t.Run("correct password succeeds and sets the cookie", func(t *testing.T) {
		f := newPublicEventFixture(t, pool, rdb, publicEventOpts{password: "swordfish-1234"})
		req := requestWithIP(http.MethodPost, "/", nextTestIP(), []byte(`{"password":"swordfish-1234"}`))
		req.Header.Set("Content-Type", "application/json")
		req = withSlugParam(req, f.slug)
		rec := httptest.NewRecorder()
		f.s.handleUnlockPublicEvent(rec, req)
		if rec.Code != http.StatusNoContent {
			t.Fatalf("status = %d, want 204; body = %s", rec.Code, rec.Body.String())
		}
		found := false
		for _, c := range rec.Result().Cookies() {
			if c.Name == eventCookieName(pwCookiePrefix, f.eventID) {
				found = true
			}
		}
		if !found {
			t.Error("expected the access cookie to be set")
		}
	})

	t.Run("wrong password is 401", func(t *testing.T) {
		f := newPublicEventFixture(t, pool, rdb, publicEventOpts{password: "swordfish-1234"})
		req := requestWithIP(http.MethodPost, "/", nextTestIP(), []byte(`{"password":"wrong-password"}`))
		req.Header.Set("Content-Type", "application/json")
		req = withSlugParam(req, f.slug)
		rec := httptest.NewRecorder()
		f.s.handleUnlockPublicEvent(rec, req)
		if rec.Code != http.StatusUnauthorized || !containsAll(rec.Body.String(), `"wrong_password"`) {
			t.Fatalf("status = %d, body = %s, want 401 wrong_password", rec.Code, rec.Body.String())
		}
	})

	t.Run("a non-password event 404s rather than confirming its visibility", func(t *testing.T) {
		f := newPublicEventFixture(t, pool, rdb, publicEventOpts{visibility: "public"})
		req := requestWithIP(http.MethodPost, "/", nextTestIP(), []byte(`{"password":"anything"}`))
		req.Header.Set("Content-Type", "application/json")
		req = withSlugParam(req, f.slug)
		rec := httptest.NewRecorder()
		f.s.handleUnlockPublicEvent(rec, req)
		if rec.Code != http.StatusNotFound {
			t.Fatalf("status = %d, want 404; body = %s", rec.Code, rec.Body.String())
		}
	})

	t.Run("unknown slug is 404", func(t *testing.T) {
		f := newPublicEventFixture(t, pool, rdb, publicEventOpts{visibility: "public"})
		req := requestWithIP(http.MethodPost, "/", nextTestIP(), []byte(`{"password":"anything"}`))
		req.Header.Set("Content-Type", "application/json")
		req = withSlugParam(req, "no-such-event-anywhere")
		rec := httptest.NewRecorder()
		f.s.handleUnlockPublicEvent(rec, req)
		if rec.Code != http.StatusNotFound {
			t.Fatalf("status = %d, want 404; body = %s", rec.Code, rec.Body.String())
		}
	})
}

// --- GET /v1/public/events/{slug}/photos: pending never leaks (§16) ---

func TestHandleListPublicPhotos_OnlyApprovedAreServed(t *testing.T) {
	pool := dbTestPool(t)
	rdb := dbTestRedis(t)
	f := newPublicEventFixture(t, pool, rdb, publicEventOpts{photosOpen: true})
	ctx := context.Background()

	pendingID := uuid.Must(uuid.NewV7())
	if _, err := f.s.q.CreateGuestMedia(ctx, store.CreateGuestMediaParams{
		ID: pendingID, StorageKey: f.eventID.String() + "/" + pendingID.String(), SizeBytes: 100, Width: 480, Height: 320, EventID: f.eventID,
	}); err != nil {
		t.Fatalf("create pending guest media: %v", err)
	}
	approvedID := uuid.Must(uuid.NewV7())
	if _, err := f.s.q.CreateGuestMedia(ctx, store.CreateGuestMediaParams{
		ID: approvedID, StorageKey: f.eventID.String() + "/" + approvedID.String(), SizeBytes: 100, Width: 480, Height: 320, EventID: f.eventID,
	}); err != nil {
		t.Fatalf("create guest media to approve: %v", err)
	}
	if _, err := f.s.q.SetGuestMediaModeration(ctx, store.SetGuestMediaModerationParams{
		Status: "approved", MediaID: approvedID, EventID: f.eventID, UserID: f.ownerID,
	}); err != nil {
		t.Fatalf("approve media: %v", err)
	}

	req := requestWithIP(http.MethodGet, "/", nextTestIP(), nil)
	req = withSlugParam(req, f.slug)
	rec := httptest.NewRecorder()
	f.s.handleListPublicPhotos(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200; body = %s", rec.Code, rec.Body.String())
	}
	body := rec.Body.String()
	if !containsAll(body, approvedID.String()) {
		t.Errorf("body = %s, want approved photo %s listed", body, approvedID)
	}
	if containsAll(body, pendingID.String()) {
		t.Errorf("body = %s, pending photo %s must never be publicly listed", body, pendingID)
	}
}
