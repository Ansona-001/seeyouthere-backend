package httpapi

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgconn"

	"github.com/ansonarose/seeyouthere-backend/internal/store"
	"github.com/ansonarose/seeyouthere-backend/internal/token"
)

func createKindRequestFor(userID uuid.UUID, occasion, kindField string) *http.Request {
	answers := `{"name":"Sam","date":"2099-06-12T18:00","venue":"The Hall"}`
	if occasion == hostedOccasionSlug {
		answers = `{"title":"Spring Gala","date":"2099-06-12T18:00","venue":"The Hall"}`
	}
	body := `{"occasion_slug":"` + occasion + `","answers":` + answers + kindField + `}`
	return jsonReq(http.MethodPost, userID, body)
}

func TestHandleCreateEvent_Kind(t *testing.T) {
	pool := dbTestPool(t)
	rdb := dbTestRedis(t)
	f := newEventTestFixture(t, pool, rdb)

	tests := []struct {
		name     string
		occasion string
		field    string
		status   int
		wantKind string
		wantRSVP string
	}{
		{"default is invite", "birthday", "", http.StatusCreated, "invite", "open"},
		{"empty string is invite", "birthday", `,"kind":""`, http.StatusCreated, "invite", "open"},
		{"explicit invite", "birthday", `,"kind":"invite"`, http.StatusCreated, "invite", "open"},
		{"hosted stage event starts open", hostedOccasionSlug, `,"kind":"hosted"`, http.StatusCreated, "hosted", "open"},
		{"hosted is limited to the stage occasion", "birthday", `,"kind":"hosted"`, http.StatusBadRequest, "", ""},
		{"stage event can still be a plain invite", hostedOccasionSlug, "", http.StatusCreated, "invite", "open"},
		{"unknown kind", "birthday", `,"kind":"party"`, http.StatusBadRequest, "", ""},
		{"wrong case", "birthday", `,"kind":"Hosted"`, http.StatusBadRequest, "", ""},
		{"padded", "birthday", `,"kind":" hosted"`, http.StatusBadRequest, "", ""},
		{"non-string kind", "birthday", `,"kind":1`, http.StatusBadRequest, "", ""},
		{"null kind falls back to invite", "birthday", `,"kind":null`, http.StatusCreated, "invite", "open"},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			id := newDisposableUser(t, pool, "create-kind")
			rec := httptest.NewRecorder()
			f.s.handleCreateEvent(rec, createKindRequestFor(id, tc.occasion, tc.field))
			wantStatusCode(t, rec, tc.status, "")
			if tc.status != http.StatusCreated {
				var n int
				if err := pool.QueryRow(context.Background(), "SELECT count(*) FROM events WHERE owner_id = $1", id).Scan(&n); err != nil || n != 0 {
					t.Errorf("events created for a rejected request: n=%d err=%v", n, err)
				}
				return
			}
			var out struct {
				Event eventResp `json:"event"`
			}
			if err := json.Unmarshal(rec.Body.Bytes(), &out); err != nil {
				t.Fatalf("decode %s: %v", rec.Body.String(), err)
			}
			if out.Event.Kind != tc.wantKind || out.Event.RsvpMode != tc.wantRSVP {
				t.Errorf("kind/rsvp_mode = %q/%q, want %q/%q", out.Event.Kind, out.Event.RsvpMode, tc.wantKind, tc.wantRSVP)
			}
		})
	}
}

func TestHandleListEvents_KindFilter(t *testing.T) {
	pool := dbTestPool(t)
	rdb := dbTestRedis(t)
	f := newEventTestFixture(t, pool, rdb)
	id := newDisposableUser(t, pool, "list-kind")

	for _, tc := range []struct{ occasion, field string }{
		{"birthday", ""},
		{hostedOccasionSlug, `,"kind":"hosted"`},
		{hostedOccasionSlug, `,"kind":"hosted"`},
	} {
		rec := httptest.NewRecorder()
		f.s.handleCreateEvent(rec, createKindRequestFor(id, tc.occasion, tc.field))
		wantStatusCode(t, rec, http.StatusCreated, "")
	}

	list := func(query string) (int, []eventSummaryResp) {
		rec := httptest.NewRecorder()
		f.s.handleListEvents(rec, requestAs(http.MethodGet, "/v1/events"+query, id))
		var out struct {
			Events []eventSummaryResp `json:"events"`
		}
		if rec.Code == http.StatusOK {
			if err := json.Unmarshal(rec.Body.Bytes(), &out); err != nil {
				t.Fatalf("decode %s: %v", rec.Body.String(), err)
			}
		}
		return rec.Code, out.Events
	}

	for _, tc := range []struct {
		name, query string
		status      int
		wantKind    string
		wantN       int
	}{
		{"no filter lists everything", "", http.StatusOK, "", 3},
		{"invites only", "?kind=invite", http.StatusOK, "invite", 1},
		{"hosted only", "?kind=hosted", http.StatusOK, "hosted", 2},
		{"unknown kind is rejected", "?kind=party", http.StatusBadRequest, "", 0},
		{"wrong case is rejected", "?kind=Hosted", http.StatusBadRequest, "", 0},
	} {
		t.Run(tc.name, func(t *testing.T) {
			status, events := list(tc.query)
			if status != tc.status {
				t.Fatalf("status = %d, want %d", status, tc.status)
			}
			if len(events) != tc.wantN {
				t.Fatalf("got %d events, want %d", len(events), tc.wantN)
			}
			for _, e := range events {
				if tc.wantKind != "" && e.Kind != tc.wantKind {
					t.Errorf("event kind = %q, want %q", e.Kind, tc.wantKind)
				}
			}
		})
	}
}

func TestHandleCreateEvent_EntryPasses(t *testing.T) {
	pool := dbTestPool(t)
	rdb := dbTestRedis(t)
	f := newEventTestFixture(t, pool, rdb)

	create := func(occasion, kindField string) (int, eventResp) {
		id := newDisposableUser(t, pool, "create-passes")
		rec := httptest.NewRecorder()
		f.s.handleCreateEvent(rec, createKindRequestFor(id, occasion, kindField))
		var out struct {
			Event eventResp `json:"event"`
		}
		if rec.Code == http.StatusCreated {
			if err := json.Unmarshal(rec.Body.Bytes(), &out); err != nil {
				t.Fatalf("decode %s: %v", rec.Body.String(), err)
			}
		}
		return rec.Code, out.Event
	}

	t.Run("hosted defaults to passes on", func(t *testing.T) {
		code, ev := create(hostedOccasionSlug, `,"kind":"hosted"`)
		if code != http.StatusCreated || !ev.EntryPasses {
			t.Fatalf("status = %d, entry_passes = %v, want 201 and true", code, ev.EntryPasses)
		}
	})
	t.Run("hosted can start with passes off", func(t *testing.T) {
		code, ev := create(hostedOccasionSlug, `,"kind":"hosted","entry_passes":false`)
		if code != http.StatusCreated || ev.EntryPasses {
			t.Fatalf("status = %d, entry_passes = %v, want 201 and false", code, ev.EntryPasses)
		}
	})
	t.Run("invites cannot set it", func(t *testing.T) {
		if code, _ := create("birthday", `,"entry_passes":false`); code != http.StatusBadRequest {
			t.Fatalf("status = %d, want 400", code)
		}
	})
}

func TestHostedEvent_DBInvariants(t *testing.T) {
	h := newHostedFixture(t)
	ctx := context.Background()

	wantCheckViolation := func(t *testing.T, err error, constraint string) {
		t.Helper()
		var pgErr *pgconn.PgError
		if !errors.As(err, &pgErr) || pgErr.Code != "23514" || (constraint != "" && pgErr.ConstraintName != constraint) {
			t.Fatalf("err = %v, want check violation %s", err, constraint)
		}
	}
	t.Run("unknown kind", func(t *testing.T) {
		_, err := h.pool.Exec(ctx, "UPDATE events SET kind = 'bogus' WHERE id = $1", h.eventID)
		wantCheckViolation(t, err, "")
	})
	// No checked_in_count <= household_size CHECK on purpose: it would block lowering household_size
	// after check-in. The check-in queries enforce the cap instead.
	t.Run("negative check-in count", func(t *testing.T) {
		g := h.addGuest(t, h.eventID, "Bob", 2)
		_, err := h.pool.Exec(ctx, "UPDATE guests SET checked_in_count = -1 WHERE id = $1", g.ID)
		wantCheckViolation(t, err, "")
	})
}

func (h hostedFixture) settings(userID, eventID uuid.UUID, body string) *httptest.ResponseRecorder {
	rec := httptest.NewRecorder()
	h.s.handleUpdateEventSettings(rec, withRouteID(jsonReq(http.MethodPatch, userID, body), eventID))
	return rec
}

func (h hostedFixture) version(t *testing.T, eventID uuid.UUID) int {
	t.Helper()
	var v int
	if err := h.pool.QueryRow(context.Background(), "SELECT version FROM events WHERE id = $1", eventID).Scan(&v); err != nil {
		t.Fatalf("read version: %v", err)
	}
	return v
}

func TestHandleUpdateEventSettings_EntryPasses(t *testing.T) {
	h := newHostedFixture(t)
	invite := h.otherEvent(t, "invite")
	body := func(v int) string { return `{"version":` + itoa(v) + `,"entry_passes":false}` }

	t.Run("hosted owner can switch passes off", func(t *testing.T) {
		wantStatusCode(t, h.settings(h.ownerID, h.eventID, body(h.version(t, h.eventID))), http.StatusOK, "")
		var on bool
		_ = h.pool.QueryRow(context.Background(), "SELECT entry_passes FROM events WHERE id = $1", h.eventID).Scan(&on)
		if on {
			t.Error("entry_passes still true after switching off")
		}
	})
	t.Run("invites have no entry passes", func(t *testing.T) {
		wantStatusCode(t, h.settings(h.ownerID, invite, body(h.version(t, invite))), http.StatusBadRequest, "")
	})
}

func TestHandleUpdateEventSettings_HostedAcceptsOpenRSVP(t *testing.T) {
	h := newHostedFixture(t)
	invite := h.otherEvent(t, "invite")

	body := func(v int, mode string) string {
		return `{"version":` + itoa(v) + `,"rsvp_mode":"` + mode + `"}`
	}
	t.Run("hosted open is fine and saved", func(t *testing.T) {
		wantStatusCode(t, h.settings(h.ownerID, h.eventID, body(h.version(t, h.eventID), "open")), http.StatusOK, "")
		var mode string
		_ = h.pool.QueryRow(context.Background(), "SELECT rsvp_mode FROM events WHERE id = $1", h.eventID).Scan(&mode)
		if mode != "open" {
			t.Errorf("rsvp_mode = %q, want open", mode)
		}
	})
	t.Run("hosted invite_only is fine", func(t *testing.T) {
		wantStatusCode(t, h.settings(h.ownerID, h.eventID, body(h.version(t, h.eventID), "invite_only")), http.StatusOK, "")
	})
	t.Run("hosted unknown mode is 400", func(t *testing.T) {
		wantStatusCode(t, h.settings(h.ownerID, h.eventID, body(h.version(t, h.eventID), "everyone")), http.StatusBadRequest, "validation_failed")
	})
	t.Run("invite-kind open is fine", func(t *testing.T) {
		wantStatusCode(t, h.settings(h.ownerID, invite, body(h.version(t, invite), "open")), http.StatusOK, "")
	})
}

// --- public event: entry pass ---

func publicHostedFixture(t *testing.T, kind string) *publicEventFixture {
	t.Helper()
	pool := dbTestPool(t)
	rdb := dbTestRedis(t)
	f := newPublicEventFixture(t, pool, rdb, publicEventOpts{rsvpMode: "invite_only"})
	if kind == "hosted" {
		if _, err := pool.Exec(context.Background(), "UPDATE events SET kind = 'hosted' WHERE id = $1", f.eventID); err != nil {
			t.Fatalf("make hosted: %v", err)
		}
	}
	return f
}

type publicViewerPass struct {
	Event struct {
		Kind string `json:"kind"`
	} `json:"event"`
	Viewer struct {
		Guest *struct {
			Name          string `json:"name"`
			HouseholdSize int32  `json:"household_size"`
		} `json:"guest"`
		Pass *entryPassResp `json:"pass"`
		RSVP *rsvpResp      `json:"rsvp"`
	} `json:"viewer"`
}

func getPublicWithCookie(t *testing.T, f *publicEventFixture, cookieVal string) (publicViewerPass, string) {
	t.Helper()
	req := withSlugParam(requestWithIP(http.MethodGet, "/", nextTestIP(), nil), f.slug)
	if cookieVal != "" {
		req.AddCookie(&http.Cookie{Name: eventCookieName(inviteCookiePrefix, f.eventID), Value: cookieVal})
	}
	rec := httptest.NewRecorder()
	f.s.handleGetPublicEvent(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, body = %s", rec.Code, rec.Body.String())
	}
	var out publicViewerPass
	if err := json.Unmarshal(rec.Body.Bytes(), &out); err != nil {
		t.Fatalf("decode: %v", err)
	}
	return out, rec.Body.String()
}

func upsertTestRSVP(t *testing.T, f *publicEventFixture, guestID uuid.UUID, attending string, count int32) {
	t.Helper()
	if _, err := f.s.q.UpsertGuestRSVP(context.Background(), store.UpsertGuestRSVPParams{
		ID: uuid.Must(uuid.NewV7()), EventID: f.eventID, GuestID: guestID, Name: "x", Attending: attending, Count: count, Answers: []byte("{}"),
	}); err != nil {
		t.Fatalf("upsert rsvp: %v", err)
	}
}

func TestHandleGetPublicEvent_EntryPassesSwitchedOff(t *testing.T) {
	f := publicHostedFixture(t, "hosted")
	_, inv := f.testGuest(t, "Priya", 3)
	if out, _ := getPublicWithCookie(t, f, inv); out.Viewer.Pass == nil {
		t.Fatal("entry passes default to on: pass missing")
	}
	if _, err := f.s.pool.Exec(context.Background(), "UPDATE events SET entry_passes = false WHERE id = $1", f.eventID); err != nil {
		t.Fatalf("switch passes off: %v", err)
	}
	out, body := getPublicWithCookie(t, f, inv)
	if out.Viewer.Pass != nil {
		t.Errorf("pass = %+v, want none once the host switched entry passes off", out.Viewer.Pass)
	}
	if out.Viewer.Guest == nil {
		t.Errorf("guest identity should still resolve: %s", body)
	}
}

func TestHandleGetPublicEvent_EntryPass(t *testing.T) {
	t.Run("hosted without rsvp has a pass admitting the household", func(t *testing.T) {
		f := publicHostedFixture(t, "hosted")
		gid, inv := f.testGuest(t, "Priya", 3)
		out, body := getPublicWithCookie(t, f, inv)
		if out.Event.Kind != "hosted" {
			t.Errorf("event.kind = %q", out.Event.Kind)
		}
		if out.Viewer.Guest == nil || out.Viewer.Pass == nil {
			t.Fatalf("pass missing: %s", body)
		}
		p := out.Viewer.Pass
		if p.Admits != 3 || p.CheckedInAt != nil {
			t.Errorf("pass = %+v, want admits 3, not checked in", p)
		}
		// The pass token verifies as a pass for this guest and is not the invite token.
		id, mac, err := token.Split(p.Token)
		if err != nil || id != gid {
			t.Fatalf("pass token id = %v err = %v, want %v", id, err, gid)
		}
		if !f.s.tokens.CheckPass(gid, 1, mac) {
			t.Error("pass token does not verify via CheckPass")
		}
		if f.s.tokens.CheckGuest(gid, 1, mac) {
			t.Error("pass token verifies as an invite token")
		}
		if p.Token == inv {
			t.Error("pass token equals the invite token")
		}
		if strings.Contains(body, inv) {
			t.Error("response leaks the invite token")
		}
		if strings.Contains(body, `"guest":{"name":"Priya","household_size":3,"pass"`) {
			t.Error("guest still carries the pass")
		}
	})

	rsvps := []struct {
		attending string
		count     int32
		wantPass  bool
		wantAdmit int32
	}{
		{"yes", 2, true, 2},
		{"maybe", 1, true, 1},
		{"no", 0, false, 0},
	}
	for _, tc := range rsvps {
		t.Run("hosted rsvp "+tc.attending, func(t *testing.T) {
			f := publicHostedFixture(t, "hosted")
			gid, inv := f.testGuest(t, "Priya", 3)
			upsertTestRSVP(t, f, gid, tc.attending, tc.count)
			out, body := getPublicWithCookie(t, f, inv)
			if out.Viewer.Guest == nil || out.Viewer.RSVP == nil {
				t.Fatalf("viewer incomplete: %s", body)
			}
			if tc.wantPass != (out.Viewer.Pass != nil) {
				t.Fatalf("pass present = %v, want %v: %s", out.Viewer.Pass != nil, tc.wantPass, body)
			}
			if tc.wantPass && out.Viewer.Pass.Admits != tc.wantAdmit {
				t.Errorf("admits = %d, want %d", out.Viewer.Pass.Admits, tc.wantAdmit)
			}
		})
	}

	t.Run("flipping rsvp from no to yes brings the pass back", func(t *testing.T) {
		f := publicHostedFixture(t, "hosted")
		gid, inv := f.testGuest(t, "Priya", 2)
		upsertTestRSVP(t, f, gid, "no", 0)
		if out, _ := getPublicWithCookie(t, f, inv); out.Viewer.Pass != nil {
			t.Fatal("pass present for a no")
		}
		upsertTestRSVP(t, f, gid, "yes", 2)
		if out, _ := getPublicWithCookie(t, f, inv); out.Viewer.Pass == nil {
			t.Fatal("pass missing after yes")
		}
	})

	t.Run("invite-kind event never has a pass", func(t *testing.T) {
		f := publicHostedFixture(t, "invite")
		gid, inv := f.testGuest(t, "Priya", 3)
		for _, rsvp := range []string{"", "yes", "maybe"} {
			if rsvp != "" {
				upsertTestRSVP(t, f, gid, rsvp, 1)
			}
			out, body := getPublicWithCookie(t, f, inv)
			if out.Event.Kind != "invite" {
				t.Errorf("event.kind = %q", out.Event.Kind)
			}
			if out.Viewer.Guest == nil || out.Viewer.Pass != nil {
				t.Errorf("rsvp %q: pass leaked on invite event: %s", rsvp, body)
			}
		}
	})

	t.Run("no cookie means no viewer and no pass", func(t *testing.T) {
		f := publicHostedFixture(t, "hosted")
		f.testGuest(t, "Priya", 3)
		out, body := getPublicWithCookie(t, f, "")
		if out.Viewer.Guest != nil || out.Viewer.Pass != nil {
			t.Errorf("anonymous viewer got guest/pass: %s", body)
		}
	})

	t.Run("rotated invite cookie loses the pass", func(t *testing.T) {
		f := publicHostedFixture(t, "hosted")
		gid, inv := f.testGuest(t, "Priya", 3)
		if _, err := f.s.q.RotateGuestToken(context.Background(), store.RotateGuestTokenParams{GuestID: gid, EventID: f.eventID, UserID: f.ownerID}); err != nil {
			t.Fatalf("rotate: %v", err)
		}
		if out, body := getPublicWithCookie(t, f, inv); out.Viewer.Guest != nil || out.Viewer.Pass != nil {
			t.Errorf("stale cookie still yields viewer/pass: %s", body)
		}
	})

	t.Run("pass token is not accepted as the invite cookie", func(t *testing.T) {
		f := publicHostedFixture(t, "hosted")
		_, inv := f.testGuest(t, "Priya", 3)
		out, _ := getPublicWithCookie(t, f, inv)
		pass := out.Viewer.Pass.Token
		if out2, body := getPublicWithCookie(t, f, pass); out2.Viewer.Guest != nil {
			t.Errorf("pass token accepted as invite cookie: %s", body)
		}
	})

	t.Run("checked_in_at is reflected", func(t *testing.T) {
		f := publicHostedFixture(t, "hosted")
		gid, inv := f.testGuest(t, "Priya", 3)
		if _, err := f.s.pool.Exec(context.Background(), "UPDATE guests SET checked_in_at = now(), checked_in_count = 2 WHERE id = $1", gid); err != nil {
			t.Fatalf("check in: %v", err)
		}
		out, body := getPublicWithCookie(t, f, inv)
		if out.Viewer.Pass == nil || out.Viewer.Pass.CheckedInAt == nil {
			t.Errorf("checked_in_at not reflected: %s", body)
		}
	})
}

// --- guest list: viewers see no contact details ---

func (h hostedFixture) listGuests(userID uuid.UUID, q string) *httptest.ResponseRecorder {
	req := withRouteID(requestAs(http.MethodGet, "/", userID), h.eventID)
	req.URL.RawQuery = url.Values{"q": {q}}.Encode()
	rec := httptest.NewRecorder()
	h.s.handleListGuests(rec, req)
	return rec
}

func TestHandleListGuests_ViewerSeesNoContacts(t *testing.T) {
	h := newHostedFixture(t)
	email, phone := "secret.person@example.invalid", "+15551234567"
	g, err := h.s.q.CreateGuest(context.Background(), store.CreateGuestParams{
		ID: uuid.Must(uuid.NewV7()), EventID: h.eventID, Name: "Zed Quill", Email: &email, Phone: &phone, HouseholdSize: 2, UserID: h.ownerID,
	})
	if err != nil {
		t.Fatalf("create guest: %v", err)
	}

	list := func(t *testing.T, user uuid.UUID, q string) []guestResp {
		t.Helper()
		rec := h.listGuests(user, q)
		wantStatusCode(t, rec, http.StatusOK, "")
		var out struct {
			Guests []guestResp `json:"guests"`
		}
		if err := json.Unmarshal(rec.Body.Bytes(), &out); err != nil {
			t.Fatalf("decode: %v", err)
		}
		if user == h.viewerID && (strings.Contains(rec.Body.String(), email) || strings.Contains(rec.Body.String(), phone) || strings.Contains(rec.Body.String(), "#")) {
			t.Errorf("viewer response leaks contact details or invite link: %s", rec.Body.String())
		}
		return out.Guests
	}

	t.Run("viewer gets name but no email, phone or invite url", func(t *testing.T) {
		gs := list(t, h.viewerID, "")
		if len(gs) != 1 || gs[0].ID != g.ID || gs[0].Name != "Zed Quill" || gs[0].HouseholdSize != 2 {
			t.Fatalf("guests = %+v", gs)
		}
		if gs[0].Email != nil || gs[0].Phone != nil || gs[0].InviteURL != nil {
			t.Errorf("contacts = %v %v %v, want nil", gs[0].Email, gs[0].Phone, gs[0].InviteURL)
		}
	})
	t.Run("viewer can search by name", func(t *testing.T) {
		if gs := list(t, h.viewerID, "quill"); len(gs) != 1 {
			t.Errorf("name search returned %d guests", len(gs))
		}
	})
	for _, q := range []string{email, "secret.person", "secret", "example.invalid", "@example"} {
		t.Run("viewer search by email "+q+" finds nothing", func(t *testing.T) {
			if gs := list(t, h.viewerID, q); len(gs) != 0 {
				t.Errorf("viewer email search matched %d guests", len(gs))
			}
		})
		t.Run("owner and editor search by email "+q+" finds the guest", func(t *testing.T) {
			for _, u := range []uuid.UUID{h.ownerID, h.editorID} {
				gs := list(t, u, q)
				if len(gs) != 1 || gs[0].Email == nil || *gs[0].Email != email || gs[0].Phone == nil {
					t.Errorf("user %s: guests = %+v", u, gs)
				}
			}
		})
	}
	t.Run("viewer wildcard does not probe emails", func(t *testing.T) {
		if gs := list(t, h.viewerID, "%@%"); len(gs) != 0 {
			t.Errorf("wildcard search matched %d guests", len(gs))
		}
	})
	t.Run("non-member and unauthenticated", func(t *testing.T) {
		wantStatusCode(t, h.listGuests(h.strangerID, ""), http.StatusNotFound, "not_found")
		wantStatusCode(t, h.listGuests(uuid.Nil, ""), http.StatusUnauthorized, "unauthorized")
	})
}
