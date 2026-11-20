package httpapi

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/ansonarose/seeyouthere-backend/internal/store"
)

// Door check-in and the registration list for open registrations on a hosted event.

type testRegistration struct {
	ID      uuid.UUID
	Email   string
	Version int32
}

func (h hostedFixture) addRegistration(t *testing.T, eventID uuid.UUID, name, attending string, count int32) testRegistration {
	t.Helper()
	email := strings.ToLower(strings.ReplaceAll(name, " ", ".")) + "-" + uuid.Must(uuid.NewV7()).String() + "@example.invalid"
	r, err := h.s.q.CreateOpenRSVP(context.Background(), store.CreateOpenRSVPParams{
		ID: uuid.Must(uuid.NewV7()), EventID: eventID, Name: name, Email: &email,
		Attending: attending, Count: count, Answers: []byte("{}"), Registration: true,
	})
	if err != nil {
		t.Fatalf("create registration: %v", err)
	}
	return testRegistration{ID: r.ID, Email: email, Version: r.EditTokenVersion}
}

func (h hostedFixture) regPass(r testRegistration) string {
	return h.s.tokens.RSVPPassToken(r.ID, r.Version)
}

type rsvpCheckInState struct {
	At    *time.Time
	Count int32
	By    *uuid.UUID
}

func (h hostedFixture) regState(t *testing.T, id uuid.UUID) rsvpCheckInState {
	t.Helper()
	var s rsvpCheckInState
	if err := h.pool.QueryRow(context.Background(),
		"SELECT checked_in_at, checked_in_count, checked_in_by FROM rsvps WHERE id = $1", id).Scan(&s.At, &s.Count, &s.By); err != nil {
		t.Fatalf("read check-in state: %v", err)
	}
	return s
}

func TestHandleCheckIn_RegistrationInvalidPassIsUniform(t *testing.T) {
	h := newHostedFixture(t)
	g := h.addGuest(t, h.eventID, "Ada", 3)
	reg := h.addRegistration(t, h.eventID, "Reg Yes", "yes", 3)
	pass := h.regPass(reg)

	maybe := h.addRegistration(t, h.eventID, "Reg Maybe", "maybe", 2)
	no := h.addRegistration(t, h.eventID, "Reg No", "no", 0)
	bumped := h.addRegistration(t, h.eventID, "Reg Bumped", "yes", 2)
	bumpedPass := h.regPass(bumped)
	h.exec(t, "UPDATE rsvps SET edit_token_version = edit_token_version + 1 WHERE id = $1", bumped.ID)
	deleted := h.addRegistration(t, h.eventID, "Reg Deleted", "yes", 2)
	deletedPass := h.regPass(deleted)
	h.exec(t, "DELETE FROM rsvps WHERE id = $1", deleted.ID)

	draftEvent := h.otherEvent(t, "hosted-draft")
	draftReg := h.addRegistration(t, draftEvent, "Draft", "yes", 1)
	inviteEvent := h.otherEvent(t, "invite")
	inviteReg := h.addRegistration(t, inviteEvent, "Inv", "yes", 1)
	deletedEvent := h.otherEvent(t, "hosted")
	deletedEventReg := h.addRegistration(t, deletedEvent, "DelEv", "yes", 1)
	h.exec(t, "UPDATE events SET deleted_at = now() WHERE id = $1", deletedEvent)

	// An ordinary open RSVP (not a registration) never has a pass.
	plainEmail := "plain@example.invalid"
	plain, err := h.s.q.CreateOpenRSVP(context.Background(), store.CreateOpenRSVPParams{
		ID: uuid.Must(uuid.NewV7()), EventID: h.eventID, Name: "Plain", Email: &plainEmail, Attending: "yes", Count: 1, Answers: []byte("{}"),
	})
	if err != nil {
		t.Fatalf("create plain rsvp: %v", err)
	}

	tests := []struct {
		name string
		user uuid.UUID
		tok  string
	}{
		{"non-member", h.strangerID, pass},
		{"unpublished hosted event", h.ownerID, h.regPass(draftReg)},
		{"invite-kind event", h.ownerID, h.regPass(inviteReg)},
		{"deleted event", h.ownerID, h.regPass(deletedEventReg)},
		{"deleted rsvp", h.ownerID, deletedPass},
		{"maybe", h.ownerID, h.regPass(maybe)},
		{"no", h.ownerID, h.regPass(no)},
		{"version bumped", h.ownerID, bumpedPass},
		{"unknown id", h.ownerID, h.s.tokens.RSVPPassToken(uuid.Must(uuid.NewV7()), 1)},
		{"guest-pass MAC on the rsvp id", h.ownerID, h.s.tokens.PassToken(reg.ID, 1)},
		{"rsvp-pass MAC on a guest id", h.ownerID, h.s.tokens.RSVPPassToken(g.ID, g.TokenVersion)},
		{"edit token as pass", h.ownerID, h.s.tokens.RSVPToken(reg.ID, reg.Version)},
		{"invite token as pass", h.ownerID, h.s.tokens.GuestToken(reg.ID, reg.Version)},
		{"non-registration rsvp", h.ownerID, h.s.tokens.RSVPPassToken(plain.ID, plain.EditTokenVersion)},
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
	// The identical body is also the guest-pass failure body.
	if rec := h.scan(h.ownerID, scanBody("not-a-token", "")); rec.Body.String() != first {
		t.Errorf("guest-path failure body %s differs from %s", rec.Body.String(), first)
	}
	for _, id := range []uuid.UUID{reg.ID, maybe.ID, no.ID, bumped.ID, draftReg.ID, inviteReg.ID, deletedEventReg.ID, plain.ID} {
		if st := h.regState(t, id); st.At != nil || st.Count != 0 {
			t.Errorf("rsvp %s was checked in by an invalid pass: %+v", id, st)
		}
	}
}

func TestHandleCheckIn_RegistrationAdmit(t *testing.T) {
	h := newHostedFixture(t)

	t.Run("default admits the registered party", func(t *testing.T) {
		reg := h.addRegistration(t, h.eventID, "Party Of Four", "yes", 4)
		rec := h.scan(h.viewerID, scanBody(h.regPass(reg), ""))
		wantStatusCode(t, rec, http.StatusOK, "")
		resp := decodeCheckIn(t, rec)
		if resp.Result != "checked_in" || resp.Source != "registration" || resp.CheckedInCount != 4 ||
			resp.Guest.Name != "Party Of Four" || resp.Guest.HouseholdSize != 4 || resp.Guest.ID != reg.ID ||
			resp.RSVP == nil || resp.RSVP.Attending != "yes" || resp.RSVP.Count != 4 ||
			resp.CheckedInByName == nil || *resp.CheckedInByName != "Door Viewer" || resp.EventID != h.eventID {
			t.Errorf("resp = %+v", resp)
		}
		if st := h.regState(t, reg.ID); st.At == nil || st.Count != 4 || st.By == nil || *st.By != h.viewerID {
			t.Errorf("state = %+v", st)
		}
	})
	t.Run("explicit count", func(t *testing.T) {
		reg := h.addRegistration(t, h.eventID, "Partial", "yes", 4)
		rec := h.scan(h.editorID, scanBody(h.regPass(reg), `,"count":2`))
		wantStatusCode(t, rec, http.StatusOK, "")
		if resp := decodeCheckIn(t, rec); resp.CheckedInCount != 2 {
			t.Errorf("checked_in_count = %d, want 2", resp.CheckedInCount)
		}
	})
	for _, extra := range []string{`,"count":0`, `,"count":5`, `,"count":-1`} {
		t.Run("bad count"+extra, func(t *testing.T) {
			reg := h.addRegistration(t, h.eventID, "Bad Count", "yes", 4)
			wantStatusCode(t, h.scan(h.ownerID, scanBody(h.regPass(reg), extra)), http.StatusBadRequest, "validation_failed")
			if st := h.regState(t, reg.ID); st.At != nil {
				t.Errorf("rejected count admitted the registration: %+v", st)
			}
		})
	}
	t.Run("event_id mismatch is wrong_event", func(t *testing.T) {
		reg := h.addRegistration(t, h.eventID, "Wrong Event", "yes", 2)
		rec := h.scan(h.ownerID, scanBody(h.regPass(reg), `,"event_id":"`+uuid.Must(uuid.NewV7()).String()+`"`))
		wantStatusCode(t, rec, http.StatusConflict, "wrong_event")
		wantStatusCode(t, h.scan(h.ownerID, scanBody(h.regPass(reg), `,"event_id":"`+h.eventID.String()+`"`)), http.StatusOK, "")
	})
	t.Run("repeat scan keeps the original", func(t *testing.T) {
		reg := h.addRegistration(t, h.eventID, "Twice", "yes", 4)
		first := decodeCheckIn(t, h.scan(h.viewerID, scanBody(h.regPass(reg), `,"count":3`)))
		rec := h.scan(h.ownerID, scanBody(h.regPass(reg), `,"count":1`))
		wantStatusCode(t, rec, http.StatusOK, "")
		again := decodeCheckIn(t, rec)
		if again.Result != "already_checked_in" || again.Source != "registration" || again.CheckedInCount != 3 ||
			!again.CheckedInAt.Equal(first.CheckedInAt) || again.CheckedInByName == nil || *again.CheckedInByName != "Door Viewer" {
			t.Errorf("repeat = %+v, first = %+v", again, first)
		}
	})
	t.Run("rotating the edit link revokes the pass", func(t *testing.T) {
		reg := h.addRegistration(t, h.eventID, "Rotated", "yes", 2)
		pass := h.regPass(reg)
		h.exec(t, "UPDATE rsvps SET edit_token_version = edit_token_version + 1 WHERE id = $1", reg.ID)
		wantStatusCode(t, h.scan(h.ownerID, scanBody(pass, "")), http.StatusNotFound, "invalid_pass")
	})
	t.Run("changing the answer away from yes revokes the pass", func(t *testing.T) {
		reg := h.addRegistration(t, h.eventID, "Changed Mind", "yes", 2)
		pass := h.regPass(reg)
		h.exec(t, "UPDATE rsvps SET attending = 'no', count = 0 WHERE id = $1", reg.ID)
		wantStatusCode(t, h.scan(h.ownerID, scanBody(pass, "")), http.StatusNotFound, "invalid_pass")
	})
	t.Run("guest source", func(t *testing.T) {
		g := h.addGuest(t, h.eventID, "Guest", 2)
		if resp := decodeCheckIn(t, h.scan(h.ownerID, scanBody(h.pass(g), ""))); resp.Source != "guest" {
			t.Errorf("source = %q, want guest", resp.Source)
		}
	})
}

func TestHandleCheckIn_RegistrationConcurrentScansAdmitOnce(t *testing.T) {
	h := newHostedFixture(t)
	reg := h.addRegistration(t, h.eventID, "Crowd", "yes", 4)
	pass := h.regPass(reg)

	const n = 8
	users := []uuid.UUID{h.ownerID, h.editorID, h.viewerID}
	results := make([]string, n)
	var wg sync.WaitGroup
	start := make(chan struct{})
	for i := range n {
		wg.Add(1)
		go func() {
			defer wg.Done()
			<-start
			rec := h.scan(users[i%len(users)], scanBody(pass, ""))
			var resp checkInResp
			if rec.Code == http.StatusOK && json.Unmarshal(rec.Body.Bytes(), &resp) == nil {
				results[i] = resp.Result
			}
		}()
	}
	close(start)
	wg.Wait()
	admitted, repeats := 0, 0
	for _, r := range results {
		switch r {
		case "checked_in":
			admitted++
		case "already_checked_in":
			repeats++
		}
	}
	if admitted != 1 || repeats != n-1 {
		t.Errorf("checked_in = %d, already_checked_in = %d, want 1 and %d", admitted, repeats, n-1)
	}
}

func TestCheckIn_RegistrationRateLimited(t *testing.T) {
	h := newHostedFixture(t)
	reg := h.addRegistration(t, h.eventID, "Limited", "yes", 2)
	ctx := context.Background()
	if ok, err := h.s.limiter.AllowN(ctx, "checkin:user:"+h.viewerID.String(), checkInLimit, checkInLimit, checkInWindow); err != nil || !ok {
		t.Fatalf("prime limiter: ok=%v err=%v", ok, err)
	}
	wantStatusCode(t, h.scan(h.viewerID, scanBody(h.regPass(reg), "")), http.StatusTooManyRequests, "rate_limited")
	if st := h.regState(t, reg.ID); st.At != nil {
		t.Errorf("rate-limited scan admitted the registration: %+v", st)
	}

	if ok, err := h.s.limiter.AllowN(ctx, "checkin:set:user:"+h.viewerID.String(), checkInLimit, checkInLimit, checkInWindow); err != nil || !ok {
		t.Fatalf("prime limiter: ok=%v err=%v", ok, err)
	}
	wantStatusCode(t, h.setRSVPCheckIn(h.viewerID, h.eventID, reg.ID, `{"count":1}`), http.StatusTooManyRequests, "rate_limited")
}

// --- PUT /v1/events/{id}/rsvps/{rsvpID}/check-in ---

func (h hostedFixture) setRSVPCheckIn(userID, eventID, rsvpID uuid.UUID, body string) *httptest.ResponseRecorder {
	req := withRouteParam(withRouteID(jsonReq(http.MethodPut, userID, body), eventID), "rsvpID", rsvpID.String())
	rec := httptest.NewRecorder()
	h.s.handleSetRSVPCheckIn(rec, req)
	return rec
}

func TestHandleSetRSVPCheckIn(t *testing.T) {
	h := newHostedFixture(t)

	t.Run("set, adjust and undo keep the first admission", func(t *testing.T) {
		reg := h.addRegistration(t, h.eventID, "Walk In", "yes", 4)
		rec := h.setRSVPCheckIn(h.viewerID, h.eventID, reg.ID, `{"count":2}`)
		wantStatusCode(t, rec, http.StatusOK, "")
		var out setRSVPCheckInResp
		if err := json.Unmarshal(rec.Body.Bytes(), &out); err != nil {
			t.Fatalf("decode: %v", err)
		}
		if out.ID != reg.ID || out.Name != "Walk In" || out.Count != 4 || out.CheckedInCount != 2 || out.CheckedInAt == nil {
			t.Errorf("resp = %+v", out)
		}
		first := h.regState(t, reg.ID)
		if first.By == nil || *first.By != h.viewerID {
			t.Errorf("checked_in_by = %v, want the viewer", first.By)
		}
		wantStatusCode(t, h.setRSVPCheckIn(h.editorID, h.eventID, reg.ID, `{"count":4}`), http.StatusOK, "")
		adj := h.regState(t, reg.ID)
		if adj.Count != 4 || adj.At == nil || !adj.At.Equal(*first.At) || adj.By == nil || *adj.By != h.viewerID {
			t.Errorf("after adjust = %+v, first = %+v", adj, first)
		}
		rec = h.setRSVPCheckIn(h.ownerID, h.eventID, reg.ID, `{"count":0}`)
		wantStatusCode(t, rec, http.StatusOK, "")
		if st := h.regState(t, reg.ID); st.At != nil || st.Count != 0 || st.By != nil {
			t.Errorf("after undo = %+v", st)
		}
	})
	t.Run("viewer cannot undo", func(t *testing.T) {
		reg := h.addRegistration(t, h.eventID, "Staff Undo", "yes", 2)
		wantStatusCode(t, h.setRSVPCheckIn(h.viewerID, h.eventID, reg.ID, `{"count":1}`), http.StatusOK, "")
		wantStatusCode(t, h.setRSVPCheckIn(h.viewerID, h.eventID, reg.ID, `{"count":0}`), http.StatusNotFound, "not_found")
		if st := h.regState(t, reg.ID); st.At == nil || st.Count != 1 {
			t.Errorf("viewer undo changed state: %+v", st)
		}
		wantStatusCode(t, h.setRSVPCheckIn(h.editorID, h.eventID, reg.ID, `{"count":0}`), http.StatusOK, "")
	})

	reg := h.addRegistration(t, h.eventID, "Rejections", "yes", 2)
	maybe := h.addRegistration(t, h.eventID, "Maybe", "maybe", 2)
	other := h.otherEvent(t, "hosted")
	otherReg := h.addRegistration(t, other, "Elsewhere", "yes", 2)
	inviteEvent := h.otherEvent(t, "invite")
	inviteReg := h.addRegistration(t, inviteEvent, "Invite Kind", "yes", 2)
	tests := []struct {
		name   string
		user   uuid.UUID
		event  uuid.UUID
		rsvp   uuid.UUID
		body   string
		status int
		code   string
	}{
		{"unauthenticated", uuid.Nil, h.eventID, reg.ID, `{"count":1}`, http.StatusUnauthorized, "unauthorized"},
		{"non-member", h.strangerID, h.eventID, reg.ID, `{"count":1}`, http.StatusNotFound, "not_found"},
		{"rsvp of another event", h.ownerID, h.eventID, otherReg.ID, `{"count":1}`, http.StatusNotFound, "not_found"},
		{"invite-kind event", h.ownerID, inviteEvent, inviteReg.ID, `{"count":1}`, http.StatusNotFound, "not_found"},
		{"unknown rsvp", h.ownerID, h.eventID, uuid.Must(uuid.NewV7()), `{"count":1}`, http.StatusNotFound, "not_found"},
		{"count above the party", h.ownerID, h.eventID, reg.ID, `{"count":3}`, http.StatusNotFound, "not_found"},
		{"admitting a maybe", h.ownerID, h.eventID, maybe.ID, `{"count":1}`, http.StatusNotFound, "not_found"},
		{"missing count", h.ownerID, h.eventID, reg.ID, `{}`, http.StatusBadRequest, "validation_failed"},
		{"negative count", h.ownerID, h.eventID, reg.ID, `{"count":-1}`, http.StatusBadRequest, "validation_failed"},
		{"count over 50", h.ownerID, h.eventID, reg.ID, `{"count":51}`, http.StatusBadRequest, "validation_failed"},
		{"unknown field", h.ownerID, h.eventID, reg.ID, `{"count":1,"x":1}`, http.StatusBadRequest, ""},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			rec := h.setRSVPCheckIn(tc.user, tc.event, tc.rsvp, tc.body)
			if tc.code == "" {
				if rec.Code != tc.status {
					t.Fatalf("status = %d, want %d", rec.Code, tc.status)
				}
			} else {
				wantStatusCode(t, rec, tc.status, tc.code)
			}
		})
	}
	for _, id := range []uuid.UUID{reg.ID, maybe.ID, otherReg.ID, inviteReg.ID} {
		if st := h.regState(t, id); st.At != nil {
			t.Errorf("rsvp %s checked in by a rejected request: %+v", id, st)
		}
	}
	t.Run("bad uuids", func(t *testing.T) {
		rec := httptest.NewRecorder()
		h.s.handleSetRSVPCheckIn(rec, withRouteParam(withRouteID(jsonReq(http.MethodPut, h.ownerID, `{"count":1}`), h.eventID), "rsvpID", "nope"))
		wantStatusCode(t, rec, http.StatusNotFound, "not_found")
	})
}

// --- GET /v1/events/{id}/registrations ---

func (h hostedFixture) listRegistrations(userID, eventID uuid.UUID, query url.Values) *httptest.ResponseRecorder {
	req := withRouteID(requestAs(http.MethodGet, "/", userID), eventID)
	req.URL.RawQuery = query.Encode()
	rec := httptest.NewRecorder()
	h.s.handleListRegistrations(rec, req)
	return rec
}

type registrationList struct {
	Registrations []registrationResp `json:"registrations"`
	NextCursor    *string            `json:"next_cursor"`
}

func decodeRegistrations(t *testing.T, rec *httptest.ResponseRecorder) registrationList {
	t.Helper()
	wantStatusCode(t, rec, http.StatusOK, "")
	var out registrationList
	if err := json.Unmarshal(rec.Body.Bytes(), &out); err != nil {
		t.Fatalf("decode %q: %v", rec.Body.String(), err)
	}
	return out
}

func TestHandleListRegistrations(t *testing.T) {
	h := newHostedFixture(t)
	a := h.addRegistration(t, h.eventID, "Alice Zephyr", "yes", 2)
	b := h.addRegistration(t, h.eventID, "Bob Quill", "maybe", 1)
	c := h.addRegistration(t, h.eventID, "Cara Moss", "no", 0)
	h.exec(t, "UPDATE rsvps SET checked_in_at = now(), checked_in_count = 1, checked_in_by = $2 WHERE id = $1", a.ID, h.viewerID)
	// Neither guest RSVPs nor other events' registrations belong in the list.
	g := h.addGuest(t, h.eventID, "Guest Person", 2)
	h.rsvp(t, h.eventID, g.ID, "yes", 2)
	otherEvent := h.otherEvent(t, "hosted")
	h.addRegistration(t, otherEvent, "Alice Elsewhere", "yes", 1)

	t.Run("owner sees emails, in sign-up order", func(t *testing.T) {
		out := decodeRegistrations(t, h.listRegistrations(h.ownerID, h.eventID, url.Values{}))
		if len(out.Registrations) != 3 || out.NextCursor != nil {
			t.Fatalf("list = %+v", out)
		}
		for i, want := range []testRegistration{a, b, c} {
			got := out.Registrations[i]
			if got.ID != want.ID || got.Email == nil || *got.Email != want.Email {
				t.Errorf("row %d = %+v, want %s %s", i, got, want.ID, want.Email)
			}
		}
		first := out.Registrations[0]
		if first.Name != "Alice Zephyr" || first.Attending != "yes" || first.Count != 2 || first.CheckedInAt == nil || first.CheckedInCount != 1 {
			t.Errorf("first = %+v", first)
		}
	})
	t.Run("viewer gets no emails and cannot search by them", func(t *testing.T) {
		rec := h.listRegistrations(h.viewerID, h.eventID, url.Values{})
		out := decodeRegistrations(t, rec)
		if len(out.Registrations) != 3 {
			t.Fatalf("rows = %d, want 3", len(out.Registrations))
		}
		for _, r := range out.Registrations {
			if r.Email != nil {
				t.Errorf("viewer sees email %q", *r.Email)
			}
		}
		if strings.Contains(rec.Body.String(), "example.invalid") {
			t.Errorf("viewer response leaks an email: %s", rec.Body.String())
		}
		if !strings.Contains(rec.Body.String(), `"email":null`) {
			t.Errorf("want explicit null email: %s", rec.Body.String())
		}
		for _, q := range []string{a.Email, "example.invalid", "@example", "alice.zephyr"} {
			if out := decodeRegistrations(t, h.listRegistrations(h.viewerID, h.eventID, url.Values{"q": {q}})); len(out.Registrations) != 0 {
				t.Errorf("viewer email search %q matched %d rows", q, len(out.Registrations))
			}
		}
		if out := decodeRegistrations(t, h.listRegistrations(h.viewerID, h.eventID, url.Values{"q": {"quill"}})); len(out.Registrations) != 1 || out.Registrations[0].ID != b.ID {
			t.Errorf("viewer name search = %+v", out.Registrations)
		}
	})
	t.Run("owner and editor search by email and name", func(t *testing.T) {
		for _, u := range []uuid.UUID{h.ownerID, h.editorID} {
			out := decodeRegistrations(t, h.listRegistrations(u, h.eventID, url.Values{"q": {strings.ToUpper(c.Email)}}))
			if len(out.Registrations) != 1 || out.Registrations[0].ID != c.ID || out.Registrations[0].Email == nil {
				t.Errorf("user %s email search = %+v", u, out.Registrations)
			}
			if out := decodeRegistrations(t, h.listRegistrations(u, h.eventID, url.Values{"q": {"moss"}})); len(out.Registrations) != 1 {
				t.Errorf("user %s name search matched %d", u, len(out.Registrations))
			}
		}
	})
	t.Run("search treats wildcards literally", func(t *testing.T) {
		for _, q := range []string{"%", "_", `\`} {
			if out := decodeRegistrations(t, h.listRegistrations(h.ownerID, h.eventID, url.Values{"q": {q}})); len(out.Registrations) != 0 {
				t.Errorf("q=%q matched %d rows", q, len(out.Registrations))
			}
		}
	})
	t.Run("pagination", func(t *testing.T) {
		page1 := decodeRegistrations(t, h.listRegistrations(h.ownerID, h.eventID, url.Values{"limit": {"2"}}))
		if len(page1.Registrations) != 2 || page1.NextCursor == nil {
			t.Fatalf("page 1 = %+v", page1)
		}
		page2 := decodeRegistrations(t, h.listRegistrations(h.ownerID, h.eventID, url.Values{"limit": {"2"}, "cursor": {*page1.NextCursor}}))
		if len(page2.Registrations) != 1 || page2.Registrations[0].ID != c.ID || page2.NextCursor != nil {
			t.Errorf("page 2 = %+v", page2)
		}
		for limit, want := range map[string]int{"0": 3, "-4": 3, "x": 3, "1000": 3, "1": 1} {
			if out := decodeRegistrations(t, h.listRegistrations(h.ownerID, h.eventID, url.Values{"limit": {limit}})); len(out.Registrations) != want {
				t.Errorf("limit=%s returned %d rows, want %d", limit, len(out.Registrations), want)
			}
		}
	})
	inviteEvent := h.otherEvent(t, "invite")
	tests := []struct {
		name   string
		user   uuid.UUID
		event  uuid.UUID
		query  url.Values
		status int
		code   string
	}{
		{"unauthenticated", uuid.Nil, h.eventID, nil, http.StatusUnauthorized, "unauthorized"},
		{"non-member", h.strangerID, h.eventID, nil, http.StatusNotFound, "not_found"},
		{"unknown event", h.ownerID, uuid.Must(uuid.NewV7()), nil, http.StatusNotFound, "not_found"},
		{"bad cursor", h.ownerID, h.eventID, url.Values{"cursor": {"garbage"}}, http.StatusBadRequest, "invalid_cursor"},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			wantStatusCode(t, h.listRegistrations(tc.user, tc.event, tc.query), tc.status, tc.code)
		})
	}
	t.Run("event without registrations is empty", func(t *testing.T) {
		out := decodeRegistrations(t, h.listRegistrations(h.ownerID, inviteEvent, url.Values{}))
		if len(out.Registrations) != 0 || out.NextCursor != nil {
			t.Errorf("list = %+v", out)
		}
		if !strings.Contains(h.listRegistrations(h.ownerID, inviteEvent, url.Values{}).Body.String(), `"registrations":[]`) {
			t.Error("want an empty array, not null")
		}
	})
	t.Run("bad uuid", func(t *testing.T) {
		rec := httptest.NewRecorder()
		h.s.handleListRegistrations(rec, withRouteParam(requestAs(http.MethodGet, "/", h.ownerID), "id", "nope"))
		wantStatusCode(t, rec, http.StatusNotFound, "not_found")
	})
}

func TestHandleCheckInSummary_WithRegistrations(t *testing.T) {
	h := newHostedFixture(t)
	g := h.addGuest(t, h.eventID, "Guest", 4)
	h.rsvp(t, h.eventID, g.ID, "yes", 3)
	wantStatusCode(t, h.setCheckIn(h.viewerID, h.eventID, g.ID, `{"count":2}`), http.StatusOK, "")

	yes1 := h.addRegistration(t, h.eventID, "Reg One", "yes", 2)
	h.addRegistration(t, h.eventID, "Reg Two", "yes", 3)
	h.addRegistration(t, h.eventID, "Reg Maybe", "maybe", 5) // not counted: cannot be admitted
	h.addRegistration(t, h.eventID, "Reg No", "no", 0)
	wantStatusCode(t, h.scan(h.viewerID, scanBody(h.regPass(yes1), `,"count":1`)), http.StatusOK, "")
	// Another event's registrations must not leak in.
	h.addRegistration(t, h.otherEvent(t, "hosted"), "Elsewhere", "yes", 9)

	rec := h.summary(h.viewerID, h.eventID)
	wantStatusCode(t, rec, http.StatusOK, "")
	var got map[string]int64
	if err := json.Unmarshal(rec.Body.Bytes(), &got); err != nil {
		t.Fatalf("decode: %v", err)
	}
	want := map[string]int64{
		"guests": 1, "invited_people": 4, "registrations": 2,
		"expected_people": 3 + 2 + 3, "checked_in_passes": 2, "checked_in_people": 2 + 1,
	}
	for k, v := range want {
		if got[k] != v {
			t.Errorf("%s = %d, want %d (summary %v)", k, got[k], v, got)
		}
	}
}
