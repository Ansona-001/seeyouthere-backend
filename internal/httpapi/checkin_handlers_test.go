package httpapi

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/ansonarose/seeyouthere-backend/internal/store"
	"github.com/ansonarose/seeyouthere-backend/internal/token"
)

func TestDefaultAdmitCount(t *testing.T) {
	yes, maybe, no, other := "yes", "maybe", "no", "pending"
	n := func(v int32) *int32 { return &v }
	tests := []struct {
		name      string
		attending *string
		count     *int32
		household int32
		want      int32
	}{
		{"no rsvp admits household", nil, nil, 4, 4},
		{"attending without count admits household", &yes, nil, 4, 4},
		{"count without attending admits household", nil, n(2), 4, 4},
		{"yes uses rsvp count", &yes, n(3), 4, 3},
		{"maybe uses rsvp count", &maybe, n(2), 4, 2},
		{"yes capped at household", &yes, n(9), 4, 4},
		{"yes at household boundary", &yes, n(4), 4, 4},
		{"yes with zero count admits at least one", &yes, n(0), 4, 1},
		{"no admits one", &no, n(0), 4, 1},
		{"no with stale count admits one", &no, n(2), 4, 1},
		{"no with household of one", &no, nil, 1, 1},
		{"unknown attending admits household", &other, n(1), 4, 4},
		{"household of one", &yes, n(1), 1, 1},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			if got := defaultAdmitCount(tc.attending, tc.count, tc.household); got != tc.want {
				t.Errorf("defaultAdmitCount = %d, want %d", got, tc.want)
			}
		})
	}
}

// hostedFixture is an eventTestFixture whose event is a published hosted
// event (owner, editor, viewer = door staff, stranger).
type hostedFixture struct {
	eventTestFixture
	pool *pgxpool.Pool
}

func newHostedFixture(t *testing.T) hostedFixture {
	t.Helper()
	pool := dbTestPool(t)
	rdb := dbTestRedis(t)
	f := newEventTestFixture(t, pool, rdb)
	keys, err := token.NewKeys(testAuthSecret())
	if err != nil {
		t.Fatalf("token.NewKeys: %v", err)
	}
	f.s.tokens = keys
	h := hostedFixture{eventTestFixture: f, pool: pool}
	h.exec(t, "UPDATE events SET kind = 'hosted', rsvp_mode = 'invite_only', status = 'published', published_at = now(), slug = 'ck-' || replace(id::text, '-', '') WHERE id = $1", f.eventID)
	h.exec(t, "UPDATE users SET name = 'Door Viewer' WHERE id = $1", f.viewerID)
	h.exec(t, "UPDATE users SET name = 'Event Owner' WHERE id = $1", f.ownerID)
	return h
}

func (h hostedFixture) exec(t *testing.T, sql string, args ...any) {
	t.Helper()
	if _, err := h.pool.Exec(context.Background(), sql, args...); err != nil {
		t.Fatalf("exec %q: %v", sql, err)
	}
}

// otherEvent creates another event owned by the fixture's owner. state is one
// of "hosted", "hosted-draft", "invite" (published invite-kind).
func (h hostedFixture) otherEvent(t *testing.T, state string) uuid.UUID {
	t.Helper()
	ev := createTestEvent(t, context.Background(), h.s.q, "birthday", "classic", &h.ownerID)
	t.Cleanup(func() { _, _ = h.pool.Exec(context.Background(), "DELETE FROM events WHERE id = $1", ev.ID) })
	switch state {
	case "hosted":
		h.exec(t, "UPDATE events SET kind = 'hosted', rsvp_mode = 'invite_only', status = 'published', published_at = now(), slug = 'ck-' || replace(id::text, '-', '') WHERE id = $1", ev.ID)
	case "hosted-draft":
		h.exec(t, "UPDATE events SET kind = 'hosted', rsvp_mode = 'invite_only' WHERE id = $1", ev.ID)
	case "invite":
		h.exec(t, "UPDATE events SET status = 'published', published_at = now(), slug = 'ck-' || replace(id::text, '-', '') WHERE id = $1", ev.ID)
	default:
		t.Fatalf("unknown state %q", state)
	}
	return ev.ID
}

func (h hostedFixture) addGuest(t *testing.T, eventID uuid.UUID, name string, household int32) store.CreateGuestRow {
	t.Helper()
	g, err := h.s.q.CreateGuest(context.Background(), store.CreateGuestParams{
		ID: uuid.Must(uuid.NewV7()), EventID: eventID, Name: name, HouseholdSize: household, UserID: h.ownerID,
	})
	if err != nil {
		t.Fatalf("create guest: %v", err)
	}
	return g
}

func (h hostedFixture) rsvp(t *testing.T, eventID, guestID uuid.UUID, attending string, count int32) {
	t.Helper()
	if _, err := h.s.q.UpsertGuestRSVP(context.Background(), store.UpsertGuestRSVPParams{
		ID: uuid.Must(uuid.NewV7()), EventID: eventID, GuestID: guestID, Name: "rsvp", Attending: attending, Count: count, Answers: []byte("{}"),
	}); err != nil {
		t.Fatalf("upsert rsvp: %v", err)
	}
}

func (h hostedFixture) pass(g store.CreateGuestRow) string {
	return h.s.tokens.PassToken(g.ID, g.TokenVersion)
}

type guestCheckInState struct {
	At    *time.Time
	Count int32
	By    *uuid.UUID
}

func (h hostedFixture) state(t *testing.T, guestID uuid.UUID) guestCheckInState {
	t.Helper()
	var s guestCheckInState
	if err := h.pool.QueryRow(context.Background(),
		"SELECT checked_in_at, checked_in_count, checked_in_by FROM guests WHERE id = $1", guestID).Scan(&s.At, &s.Count, &s.By); err != nil {
		t.Fatalf("read check-in state: %v", err)
	}
	return s
}

func jsonReq(method string, userID uuid.UUID, body string) *http.Request {
	req := requestAs(method, "/", userID)
	req.Body = newJSONBody(body)
	req.Header.Set("Content-Type", "application/json")
	return req
}

func (h hostedFixture) scan(userID uuid.UUID, body string) *httptest.ResponseRecorder {
	rec := httptest.NewRecorder()
	h.s.handleCheckIn(rec, jsonReq(http.MethodPost, userID, body))
	return rec
}

func scanBody(tok string, extra string) string {
	b, _ := json.Marshal(tok)
	return `{"token":` + string(b) + extra + `}`
}

func decodeCheckIn(t *testing.T, rec *httptest.ResponseRecorder) checkInResp {
	t.Helper()
	var resp checkInResp
	if err := json.Unmarshal(rec.Body.Bytes(), &resp); err != nil {
		t.Fatalf("decode %q: %v", rec.Body.String(), err)
	}
	return resp
}

func TestHandleCheckIn_InvalidPassIsUniform(t *testing.T) {
	h := newHostedFixture(t)
	g := h.addGuest(t, h.eventID, "Ada", 3)
	validPass := h.pass(g)

	id, mac, err := token.Split(validPass)
	if err != nil {
		t.Fatalf("split: %v", err)
	}
	badMAC := append([]byte(nil), mac...)
	badMAC[0] ^= 0x01
	forged := base64.RawURLEncoding.EncodeToString(append(id[:], badMAC...))

	inviteEvent := h.otherEvent(t, "invite")
	inviteGuest := h.addGuest(t, inviteEvent, "Inv", 2)
	draftEvent := h.otherEvent(t, "hosted-draft")
	draftGuest := h.addGuest(t, draftEvent, "Dra", 2)
	deletedEvent := h.otherEvent(t, "hosted")
	deletedGuest := h.addGuest(t, deletedEvent, "Del", 2)
	h.exec(t, "UPDATE events SET deleted_at = now() WHERE id = $1", deletedEvent)

	rotated := h.addGuest(t, h.eventID, "Rot", 2)
	rotatedPass := h.pass(rotated)
	if _, err := h.s.q.RotateGuestToken(context.Background(), store.RotateGuestTokenParams{GuestID: rotated.ID, EventID: h.eventID, UserID: h.ownerID}); err != nil {
		t.Fatalf("rotate: %v", err)
	}

	missing := h.s.tokens.PassToken(uuid.Must(uuid.NewV7()), 1)

	tests := []struct {
		name string
		user uuid.UUID
		tok  string
	}{
		{"empty token", h.ownerID, ""},
		{"garbage", h.ownerID, "not-a-token"},
		{"too long", h.ownerID, validPass + "A"},
		{"too short", h.ownerID, validPass[:len(validPass)-1]},
		{"bad mac", h.ownerID, forged},
		{"invite token used as pass", h.ownerID, h.s.tokens.GuestToken(g.ID, g.TokenVersion)},
		{"unknown guest", h.ownerID, missing},
		{"non-member", h.strangerID, validPass},
		{"invite-kind event", h.ownerID, h.pass(inviteGuest)},
		{"draft hosted event", h.ownerID, h.pass(draftGuest)},
		{"deleted hosted event", h.ownerID, h.pass(deletedGuest)},
		{"rotated pass", h.ownerID, rotatedPass},
	}
	var first string
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			rec := h.scan(tc.user, scanBody(tc.tok, ""))
			wantStatusCode(t, rec, http.StatusNotFound, "invalid_pass")
			if first == "" {
				first = rec.Body.String()
			} else if rec.Body.String() != first {
				t.Errorf("body = %s, want identical to %s", rec.Body.String(), first)
			}
		})
	}
	for _, id := range []uuid.UUID{g.ID, inviteGuest.ID, draftGuest.ID, deletedGuest.ID, rotated.ID} {
		if st := h.state(t, id); st.At != nil || st.Count != 0 {
			t.Errorf("guest %s was checked in by an invalid pass: %+v", id, st)
		}
	}
}

func TestHandleCheckIn_RequestValidation(t *testing.T) {
	h := newHostedFixture(t)
	g := h.addGuest(t, h.eventID, "Ada", 4)
	pass := h.pass(g)

	t.Run("unauthenticated", func(t *testing.T) {
		wantStatusCode(t, h.scan(uuid.Nil, scanBody(pass, "")), http.StatusUnauthorized, "unauthorized")
	})
	t.Run("malformed json", func(t *testing.T) {
		wantStatusCode(t, h.scan(h.ownerID, `{"token":`), http.StatusBadRequest, "")
	})
	t.Run("unknown field", func(t *testing.T) {
		wantStatusCode(t, h.scan(h.ownerID, `{"token":"x","admin":true}`), http.StatusBadRequest, "")
	})
	t.Run("wrong json type for count", func(t *testing.T) {
		wantStatusCode(t, h.scan(h.ownerID, scanBody(pass, `,"count":"2"`)), http.StatusBadRequest, "")
	})
	t.Run("oversized body", func(t *testing.T) {
		wantStatusCode(t, h.scan(h.ownerID, `{"token":"`+strings.Repeat("a", maxBodyBytes+1)+`"}`), http.StatusRequestEntityTooLarge, "body_too_large")
	})
	t.Run("wrong content type", func(t *testing.T) {
		req := jsonReq(http.MethodPost, h.ownerID, scanBody(pass, ""))
		req.Header.Set("Content-Type", "text/plain")
		rec := httptest.NewRecorder()
		h.s.handleCheckIn(rec, req)
		wantStatusCode(t, rec, http.StatusUnsupportedMediaType, "unsupported_media_type")
	})

	for _, count := range []string{"0", "-1", "5", "51", "2147483647"} {
		t.Run("count "+count+" rejected", func(t *testing.T) {
			wantStatusCode(t, h.scan(h.ownerID, scanBody(pass, `,"count":`+count)), http.StatusBadRequest, "validation_failed")
			if st := h.state(t, g.ID); st.At != nil {
				t.Fatalf("guest checked in despite invalid count: %+v", st)
			}
		})
	}
	t.Run("count null means default", func(t *testing.T) {
		rec := h.scan(h.ownerID, scanBody(pass, `,"count":null`))
		wantStatusCode(t, rec, http.StatusOK, "")
		if got := decodeCheckIn(t, rec).CheckedInCount; got != 4 {
			t.Errorf("count = %d, want 4 (household default)", got)
		}
	})
}

func TestHandleCheckIn_Admit(t *testing.T) {
	h := newHostedFixture(t)

	t.Run("viewer admits, name and event echoed", func(t *testing.T) {
		g := h.addGuest(t, h.eventID, "Ada", 3)
		rec := h.scan(h.viewerID, scanBody(h.pass(g), ""))
		wantStatusCode(t, rec, http.StatusOK, "")
		resp := decodeCheckIn(t, rec)
		if resp.Result != "checked_in" || resp.EventID != h.eventID || resp.Guest.ID != g.ID || resp.Guest.Name != "Ada" || resp.Guest.HouseholdSize != 3 {
			t.Errorf("resp = %+v", resp)
		}
		if resp.CheckedInByName == nil || *resp.CheckedInByName != "Door Viewer" {
			t.Errorf("checked_in_by_name = %v, want Door Viewer", resp.CheckedInByName)
		}
		st := h.state(t, g.ID)
		if st.At == nil || st.Count != 3 || st.By == nil || *st.By != h.viewerID {
			t.Errorf("db state = %+v", st)
		}
	})

	t.Run("editor and owner admit", func(t *testing.T) {
		for _, u := range []uuid.UUID{h.editorID, h.ownerID} {
			g := h.addGuest(t, h.eventID, "Role", 1)
			wantStatusCode(t, h.scan(u, scanBody(h.pass(g), "")), http.StatusOK, "")
		}
	})

	defaults := []struct {
		name      string
		household int32
		attending string // "" = no rsvp
		rsvpCount int32
		want      int32
	}{
		{"no rsvp admits household", 4, "", 0, 4},
		{"yes admits rsvp count", 4, "yes", 3, 3},
		{"maybe admits rsvp count", 4, "maybe", 2, 2},
		{"yes full household", 4, "yes", 4, 4},
		{"no admits one", 4, "no", 0, 1},
	}
	for _, tc := range defaults {
		t.Run("default count: "+tc.name, func(t *testing.T) {
			g := h.addGuest(t, h.eventID, "Def", tc.household)
			if tc.attending != "" {
				h.rsvp(t, h.eventID, g.ID, tc.attending, tc.rsvpCount)
			}
			rec := h.scan(h.viewerID, scanBody(h.pass(g), ""))
			wantStatusCode(t, rec, http.StatusOK, "")
			resp := decodeCheckIn(t, rec)
			if resp.CheckedInCount != tc.want {
				t.Errorf("checked_in_count = %d, want %d", resp.CheckedInCount, tc.want)
			}
			if tc.attending == "" && resp.RSVP != nil {
				t.Errorf("rsvp = %+v, want nil", resp.RSVP)
			}
			if tc.attending != "" && (resp.RSVP == nil || resp.RSVP.Attending != tc.attending || resp.RSVP.Count != tc.rsvpCount) {
				t.Errorf("rsvp = %+v, want %s/%d", resp.RSVP, tc.attending, tc.rsvpCount)
			}
		})
	}

	t.Run("rsvp count above a shrunk household is capped", func(t *testing.T) {
		g := h.addGuest(t, h.eventID, "Shrunk", 4)
		h.rsvp(t, h.eventID, g.ID, "yes", 4)
		h.exec(t, "UPDATE guests SET household_size = 2 WHERE id = $1", g.ID)
		rec := h.scan(h.viewerID, scanBody(h.pass(g), ""))
		wantStatusCode(t, rec, http.StatusOK, "")
		if got := decodeCheckIn(t, rec).CheckedInCount; got != 2 {
			t.Errorf("count = %d, want 2 (capped at household)", got)
		}
	})

	for _, count := range []int32{1, 2, 4} {
		t.Run("explicit count "+itoa(int(count)), func(t *testing.T) {
			g := h.addGuest(t, h.eventID, "Exp", 4)
			h.rsvp(t, h.eventID, g.ID, "yes", 3)
			rec := h.scan(h.viewerID, scanBody(h.pass(g), `,"count":`+itoa(int(count))))
			wantStatusCode(t, rec, http.StatusOK, "")
			if got := decodeCheckIn(t, rec).CheckedInCount; got != count {
				t.Errorf("count = %d, want %d", got, count)
			}
		})
	}
}

func TestHandleCheckIn_EventIDMismatch(t *testing.T) {
	h := newHostedFixture(t)
	g := h.addGuest(t, h.eventID, "Ada", 2)
	pass := h.pass(g)
	other := h.otherEvent(t, "hosted")

	rec := h.scan(h.ownerID, scanBody(pass, `,"event_id":"`+other.String()+`"`))
	wantStatusCode(t, rec, http.StatusConflict, "wrong_event")
	if st := h.state(t, g.ID); st.At != nil {
		t.Fatalf("guest checked in despite wrong event: %+v", st)
	}
	wantStatusCode(t, h.scan(h.ownerID, scanBody(pass, `,"event_id":"not-a-uuid"`)), http.StatusBadRequest, "")
	wantStatusCode(t, h.scan(h.ownerID, scanBody(pass, `,"event_id":"`+h.eventID.String()+`"`)), http.StatusOK, "")
}

func TestHandleCheckIn_RepeatScanKeepsOriginal(t *testing.T) {
	h := newHostedFixture(t)
	g := h.addGuest(t, h.eventID, "Ada", 4)
	pass := h.pass(g)

	first := h.scan(h.viewerID, scanBody(pass, `,"count":2`))
	wantStatusCode(t, first, http.StatusOK, "")
	f := decodeCheckIn(t, first)
	if f.Result != "checked_in" {
		t.Fatalf("first result = %q", f.Result)
	}

	// A second scan by someone else, even asking for a different count.
	for _, extra := range []string{"", `,"count":4`} {
		rec := h.scan(h.ownerID, scanBody(pass, extra))
		wantStatusCode(t, rec, http.StatusOK, "")
		r := decodeCheckIn(t, rec)
		if r.Result != "already_checked_in" {
			t.Errorf("result = %q, want already_checked_in", r.Result)
		}
		if !r.CheckedInAt.Equal(f.CheckedInAt) || r.CheckedInCount != 2 {
			t.Errorf("repeat changed admission: at %v count %d, want %v / 2", r.CheckedInAt, r.CheckedInCount, f.CheckedInAt)
		}
		if r.CheckedInByName == nil || *r.CheckedInByName != "Door Viewer" {
			t.Errorf("checked_in_by_name = %v, want the original scanner", r.CheckedInByName)
		}
	}
	st := h.state(t, g.ID)
	if st.By == nil || *st.By != h.viewerID || st.Count != 2 {
		t.Errorf("db state = %+v, want viewer / 2", st)
	}
}

func TestHandleCheckIn_ConcurrentScansAdmitOnce(t *testing.T) {
	h := newHostedFixture(t)
	g := h.addGuest(t, h.eventID, "Ada", 4)
	pass := h.pass(g)

	const n = 8
	users := []uuid.UUID{h.ownerID, h.editorID, h.viewerID}
	results := make([]string, n)
	codes := make([]int, n)
	var wg sync.WaitGroup
	start := make(chan struct{})
	for i := range n {
		wg.Add(1)
		go func() {
			defer wg.Done()
			<-start
			rec := h.scan(users[i%len(users)], scanBody(pass, ""))
			codes[i] = rec.Code
			var resp checkInResp
			if err := json.Unmarshal(rec.Body.Bytes(), &resp); err == nil {
				results[i] = resp.Result
			}
		}()
	}
	close(start)
	wg.Wait()

	admitted, repeats := 0, 0
	for i := range n {
		if codes[i] != http.StatusOK {
			t.Fatalf("request %d status = %d", i, codes[i])
		}
		switch results[i] {
		case "checked_in":
			admitted++
		case "already_checked_in":
			repeats++
		default:
			t.Errorf("request %d result = %q", i, results[i])
		}
	}
	if admitted != 1 || repeats != n-1 {
		t.Errorf("checked_in = %d, already_checked_in = %d, want 1 and %d", admitted, repeats, n-1)
	}
}

func TestCheckIn_RateLimited(t *testing.T) {
	h := newHostedFixture(t)
	g := h.addGuest(t, h.eventID, "Ada", 2)
	ctx := context.Background()

	t.Run("scan", func(t *testing.T) {
		if ok, err := h.s.limiter.AllowN(ctx, "checkin:user:"+h.viewerID.String(), checkInLimit, checkInLimit, checkInWindow); err != nil || !ok {
			t.Fatalf("prime limiter: ok=%v err=%v", ok, err)
		}
		rec := h.scan(h.viewerID, scanBody(h.pass(g), ""))
		wantStatusCode(t, rec, http.StatusTooManyRequests, "rate_limited")
		if rec.Header().Get("Retry-After") == "" {
			t.Error("missing Retry-After")
		}
		if st := h.state(t, g.ID); st.At != nil {
			t.Errorf("rate-limited scan admitted the guest: %+v", st)
		}
		// Other users are unaffected.
		wantStatusCode(t, h.scan(h.ownerID, scanBody(h.pass(g), "")), http.StatusOK, "")
	})
	t.Run("set", func(t *testing.T) {
		if ok, err := h.s.limiter.AllowN(ctx, "checkin:set:user:"+h.viewerID.String(), checkInLimit, checkInLimit, checkInWindow); err != nil || !ok {
			t.Fatalf("prime limiter: ok=%v err=%v", ok, err)
		}
		rec := h.setCheckIn(h.viewerID, h.eventID, g.ID, `{"count":1}`)
		wantStatusCode(t, rec, http.StatusTooManyRequests, "rate_limited")
	})
}

// --- PUT /v1/events/{id}/guests/{guestID}/check-in ---

func (h hostedFixture) setCheckIn(userID, eventID, guestID uuid.UUID, body string) *httptest.ResponseRecorder {
	req := withRouteParam(withRouteID(jsonReq(http.MethodPut, userID, body), eventID), "guestID", guestID.String())
	rec := httptest.NewRecorder()
	h.s.handleSetGuestCheckIn(rec, req)
	return rec
}

func decodeSetCheckIn(t *testing.T, rec *httptest.ResponseRecorder) guestResp {
	t.Helper()
	var out struct {
		Guest guestResp `json:"guest"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &out); err != nil {
		t.Fatalf("decode %q: %v", rec.Body.String(), err)
	}
	return out.Guest
}

func TestHandleSetGuestCheckIn_Flow(t *testing.T) {
	h := newHostedFixture(t)
	g := h.addGuest(t, h.eventID, "Ada", 4)

	rec := h.setCheckIn(h.viewerID, h.eventID, g.ID, `{"count":2}`)
	wantStatusCode(t, rec, http.StatusOK, "")
	got := decodeSetCheckIn(t, rec)
	if got.ID != g.ID || got.Name != "Ada" || got.HouseholdSize != 4 || got.CheckedInAt == nil || got.CheckedInCount != 2 {
		t.Fatalf("set resp = %+v", got)
	}
	st := h.state(t, g.ID)
	if st.By == nil || *st.By != h.viewerID {
		t.Fatalf("checked_in_by = %v, want viewer", st.By)
	}
	firstAt := *st.At

	// Adjust by another member: time and original scanner are kept.
	wantStatusCode(t, h.setCheckIn(h.ownerID, h.eventID, g.ID, `{"count":3}`), http.StatusOK, "")
	st = h.state(t, g.ID)
	if st.Count != 3 || !st.At.Equal(firstAt) || st.By == nil || *st.By != h.viewerID {
		t.Errorf("after adjust: %+v, want count 3 with original time/by", st)
	}

	// Idempotent repeat.
	wantStatusCode(t, h.setCheckIn(h.ownerID, h.eventID, g.ID, `{"count":3}`), http.StatusOK, "")
	if st2 := h.state(t, g.ID); st2.Count != 3 || !st2.At.Equal(firstAt) {
		t.Errorf("idempotent repeat changed state: %+v", st2)
	}

	// Upper boundary: the full household.
	wantStatusCode(t, h.setCheckIn(h.editorID, h.eventID, g.ID, `{"count":4}`), http.StatusOK, "")

	// Undo.
	rec = h.setCheckIn(h.editorID, h.eventID, g.ID, `{"count":0}`)
	wantStatusCode(t, rec, http.StatusOK, "")
	if u := decodeSetCheckIn(t, rec); u.CheckedInAt != nil || u.CheckedInCount != 0 {
		t.Errorf("undo resp = %+v", u)
	}
	if st3 := h.state(t, g.ID); st3.At != nil || st3.Count != 0 || st3.By != nil {
		t.Errorf("undo state = %+v, want cleared", st3)
	}
	// Undo of a guest who isn't checked in is harmless.
	wantStatusCode(t, h.setCheckIn(h.ownerID, h.eventID, g.ID, `{"count":0}`), http.StatusOK, "")

	// A scan after an undo admits again.
	scan := h.scan(h.viewerID, scanBody(h.pass(g), ""))
	if r := decodeCheckIn(t, scan); r.Result != "checked_in" {
		t.Errorf("scan after undo result = %q, want checked_in", r.Result)
	}
}

func TestHandleSetGuestCheckIn_UndoRoles(t *testing.T) {
	h := newHostedFixture(t)
	g := h.addGuest(t, h.eventID, "Ada", 4)

	// Viewers (door staff) may admit and adjust upwards, not undo.
	wantStatusCode(t, h.setCheckIn(h.viewerID, h.eventID, g.ID, `{"count":2}`), http.StatusOK, "")
	wantStatusCode(t, h.setCheckIn(h.viewerID, h.eventID, g.ID, `{"count":0}`), http.StatusNotFound, "not_found")
	if st := h.state(t, g.ID); st.At == nil || st.Count != 2 {
		t.Fatalf("viewer undo changed state: %+v", st)
	}

	for _, tc := range []struct {
		name string
		user uuid.UUID
	}{{"owner", h.ownerID}, {"editor", h.editorID}} {
		t.Run(tc.name+" undo", func(t *testing.T) {
			wantStatusCode(t, h.setCheckIn(h.viewerID, h.eventID, g.ID, `{"count":2}`), http.StatusOK, "")
			rec := h.setCheckIn(tc.user, h.eventID, g.ID, `{"count":0}`)
			wantStatusCode(t, rec, http.StatusOK, "")
			if u := decodeSetCheckIn(t, rec); u.CheckedInAt != nil || u.CheckedInCount != 0 || u.Name != "Ada" {
				t.Errorf("undo resp = %+v", u)
			}
			if st := h.state(t, g.ID); st.At != nil || st.Count != 0 || st.By != nil {
				t.Errorf("undo state = %+v, want cleared", st)
			}
		})
	}
}

func TestHandleSetGuestCheckIn_Rejections(t *testing.T) {
	h := newHostedFixture(t)
	g := h.addGuest(t, h.eventID, "Ada", 4)
	inviteEvent := h.otherEvent(t, "invite")
	inviteGuest := h.addGuest(t, inviteEvent, "Inv", 2)
	otherEvent := h.otherEvent(t, "hosted")
	missing := uuid.Must(uuid.NewV7())

	tests := []struct {
		name    string
		user    uuid.UUID
		event   uuid.UUID
		guest   uuid.UUID
		body    string
		status  int
		code    string
		touched uuid.UUID // guest that must stay untouched
	}{
		{"unauthenticated", uuid.Nil, h.eventID, g.ID, `{"count":1}`, http.StatusUnauthorized, "unauthorized", g.ID},
		{"non-member", h.strangerID, h.eventID, g.ID, `{"count":1}`, http.StatusNotFound, "not_found", g.ID},
		{"invite-kind event", h.ownerID, inviteEvent, inviteGuest.ID, `{"count":1}`, http.StatusNotFound, "not_found", inviteGuest.ID},
		{"guest of a different event", h.ownerID, otherEvent, g.ID, `{"count":1}`, http.StatusNotFound, "not_found", g.ID},
		{"unknown guest", h.ownerID, h.eventID, missing, `{"count":1}`, http.StatusNotFound, "not_found", g.ID},
		{"unknown event", h.ownerID, missing, g.ID, `{"count":1}`, http.StatusNotFound, "not_found", g.ID},
		{"count above household", h.ownerID, h.eventID, g.ID, `{"count":5}`, http.StatusNotFound, "not_found", g.ID},
		{"count max valid but above household", h.ownerID, h.eventID, g.ID, `{"count":50}`, http.StatusNotFound, "not_found", g.ID},
		{"count over api max", h.ownerID, h.eventID, g.ID, `{"count":51}`, http.StatusBadRequest, "validation_failed", g.ID},
		{"negative count", h.ownerID, h.eventID, g.ID, `{"count":-1}`, http.StatusBadRequest, "validation_failed", g.ID},
		{"missing count", h.ownerID, h.eventID, g.ID, `{}`, http.StatusBadRequest, "validation_failed", g.ID},
		{"null count", h.ownerID, h.eventID, g.ID, `{"count":null}`, http.StatusBadRequest, "validation_failed", g.ID},
		{"string count", h.ownerID, h.eventID, g.ID, `{"count":"1"}`, http.StatusBadRequest, "", g.ID},
		{"malformed json", h.ownerID, h.eventID, g.ID, `{"count":`, http.StatusBadRequest, "", g.ID},
		{"unknown field", h.ownerID, h.eventID, g.ID, `{"count":1,"x":1}`, http.StatusBadRequest, "", g.ID},
		{"oversized body", h.ownerID, h.eventID, g.ID, `{"count":1,"x":"` + strings.Repeat("a", maxBodyBytes) + `"}`, http.StatusRequestEntityTooLarge, "body_too_large", g.ID},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			wantStatusCode(t, h.setCheckIn(tc.user, tc.event, tc.guest, tc.body), tc.status, tc.code)
			if st := h.state(t, tc.touched); st.At != nil || st.Count != 0 {
				t.Errorf("guest state changed by rejected request: %+v", st)
			}
		})
	}

	t.Run("bad uuid params", func(t *testing.T) {
		req := withRouteParam(withRouteParam(jsonReq(http.MethodPut, h.ownerID, `{"count":1}`), "id", "nope"), "guestID", g.ID.String())
		rec := httptest.NewRecorder()
		h.s.handleSetGuestCheckIn(rec, req)
		wantStatusCode(t, rec, http.StatusNotFound, "not_found")
		req = withRouteParam(withRouteID(jsonReq(http.MethodPut, h.ownerID, `{"count":1}`), h.eventID), "guestID", "nope")
		rec = httptest.NewRecorder()
		h.s.handleSetGuestCheckIn(rec, req)
		wantStatusCode(t, rec, http.StatusNotFound, "not_found")
	})
}

// --- GET /v1/events/{id}/check-ins/summary ---

func (h hostedFixture) summary(userID, eventID uuid.UUID) *httptest.ResponseRecorder {
	rec := httptest.NewRecorder()
	h.s.handleCheckInSummary(rec, withRouteID(requestAs(http.MethodGet, "/", userID), eventID))
	return rec
}

func TestHandleCheckInSummary(t *testing.T) {
	h := newHostedFixture(t)

	read := func(t *testing.T, rec *httptest.ResponseRecorder) map[string]int64 {
		t.Helper()
		wantStatusCode(t, rec, http.StatusOK, "")
		var m map[string]int64
		if err := json.Unmarshal(rec.Body.Bytes(), &m); err != nil {
			t.Fatalf("decode: %v", err)
		}
		return m
	}
	want := func(t *testing.T, got map[string]int64, guests, invited, expected, inGuests, inPeople int64) {
		t.Helper()
		exp := map[string]int64{"guests": guests, "invited_people": invited, "registrations": 0, "expected_people": expected, "checked_in_passes": inGuests, "checked_in_people": inPeople}
		if len(got) != len(exp) {
			t.Errorf("summary = %v, want keys of %v", got, exp)
		}
		for k, v := range exp {
			if got[k] != v {
				t.Errorf("%s = %d, want %d (summary %v)", k, got[k], v, got)
			}
		}
	}

	t.Run("empty event is all zeros", func(t *testing.T) {
		want(t, read(t, h.summary(h.viewerID, h.eventID)), 0, 0, 0, 0, 0)
	})

	a := h.addGuest(t, h.eventID, "A", 4) // yes 3, admitted 2
	b := h.addGuest(t, h.eventID, "B", 2) // maybe 2
	c := h.addGuest(t, h.eventID, "C", 3) // no
	h.addGuest(t, h.eventID, "D", 1)      // no rsvp
	h.rsvp(t, h.eventID, a.ID, "yes", 3)
	h.rsvp(t, h.eventID, b.ID, "maybe", 2)
	h.rsvp(t, h.eventID, c.ID, "no", 0)
	wantStatusCode(t, h.setCheckIn(h.viewerID, h.eventID, a.ID, `{"count":2}`), http.StatusOK, "")

	// Guests of another hosted event must not leak into the totals.
	other := h.otherEvent(t, "hosted")
	h.addGuest(t, other, "Z", 9)

	t.Run("totals", func(t *testing.T) {
		for _, u := range []uuid.UUID{h.ownerID, h.editorID, h.viewerID} {
			want(t, read(t, h.summary(u, h.eventID)), 4, 10, 5, 1, 2)
		}
	})
	t.Run("totals follow check-in changes", func(t *testing.T) {
		wantStatusCode(t, h.setCheckIn(h.viewerID, h.eventID, b.ID, `{"count":2}`), http.StatusOK, "")
		want(t, read(t, h.summary(h.viewerID, h.eventID)), 4, 10, 5, 2, 4)
		wantStatusCode(t, h.setCheckIn(h.editorID, h.eventID, b.ID, `{"count":0}`), http.StatusOK, "")
		want(t, read(t, h.summary(h.viewerID, h.eventID)), 4, 10, 5, 1, 2)
	})

	inviteEvent := h.otherEvent(t, "invite")
	tests := []struct {
		name   string
		user   uuid.UUID
		event  uuid.UUID
		status int
		code   string
	}{
		{"unauthenticated", uuid.Nil, h.eventID, http.StatusUnauthorized, "unauthorized"},
		{"non-member", h.strangerID, h.eventID, http.StatusNotFound, "not_found"},
		{"invite-kind event", h.ownerID, inviteEvent, http.StatusNotFound, "not_found"},
		{"unknown event", h.ownerID, uuid.Must(uuid.NewV7()), http.StatusNotFound, "not_found"},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			wantStatusCode(t, h.summary(tc.user, tc.event), tc.status, tc.code)
		})
	}
	t.Run("bad uuid", func(t *testing.T) {
		rec := httptest.NewRecorder()
		h.s.handleCheckInSummary(rec, withRouteParam(requestAs(http.MethodGet, "/", h.ownerID), "id", "nope"))
		wantStatusCode(t, rec, http.StatusNotFound, "not_found")
	})
}
