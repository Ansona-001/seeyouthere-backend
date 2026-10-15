package httpapi

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgxpool"
)

func submitWish(f *publicEventFixture, slug, body, ip, inviteToken string) *httptest.ResponseRecorder {
	req := requestWithIP(http.MethodPost, "/", ip, []byte(body))
	req.Header.Set("Content-Type", "application/json")
	req = withSlugParam(req, slug)
	if inviteToken != "" {
		req.AddCookie(&http.Cookie{Name: eventCookieName(inviteCookiePrefix, f.eventID), Value: inviteToken})
	}
	rec := httptest.NewRecorder()
	f.s.handleCreateWish(rec, req)
	return rec
}

func countWishes(t *testing.T, pool *pgxpool.Pool, eventID uuid.UUID) int {
	t.Helper()
	var n int
	if err := pool.QueryRow(context.Background(), "SELECT count(*) FROM event_wishes WHERE event_id = $1", eventID).Scan(&n); err != nil {
		t.Fatalf("count wishes: %v", err)
	}
	return n
}

func seedWish(t *testing.T, pool *pgxpool.Pool, eventID uuid.UUID, author, status string, createdAt time.Time) uuid.UUID {
	t.Helper()
	id := uuid.Must(uuid.NewV7())
	if _, err := pool.Exec(context.Background(),
		"INSERT INTO event_wishes (id, event_id, author, message, status, created_at) VALUES ($1, $2, $3, 'msg', $4, $5)",
		id, eventID, author, status, createdAt); err != nil {
		t.Fatalf("seed wish: %v", err)
	}
	return id
}

func TestHandleCreateWish(t *testing.T) {
	pool := dbTestPool(t)
	rdb := dbTestRedis(t)
	const valid = `{"author":"Sam","message":"Congrats!"}`

	t.Run("success publishes an approved wish", func(t *testing.T) {
		f := newPublicEventFixture(t, pool, rdb, publicEventOpts{wishes: "open"})
		rec := submitWish(f, f.slug, `{"author":"  Sam ","message":" Line one\r\nline two "}`, nextTestIP(), "")
		if rec.Code != http.StatusCreated || !containsAll(rec.Body.String(), `"status":"approved"`) {
			t.Fatalf("status = %d, body = %s, want 201 approved", rec.Code, rec.Body.String())
		}
		var author, message, status string
		if err := pool.QueryRow(context.Background(),
			"SELECT author, message, status FROM event_wishes WHERE event_id = $1", f.eventID).Scan(&author, &message, &status); err != nil {
			t.Fatalf("read wish: %v", err)
		}
		if author != "Sam" || message != "Line one\nline two" || status != "approved" {
			t.Fatalf("stored (%q, %q, %q)", author, message, status)
		}
	})

	t.Run("honeypot answers 201 and writes nothing", func(t *testing.T) {
		f := newPublicEventFixture(t, pool, rdb, publicEventOpts{wishes: "open"})
		rec := submitWish(f, f.slug, `{"author":"Bot","message":"spam","website":"http://x"}`, nextTestIP(), "")
		if rec.Code != http.StatusCreated || !containsAll(rec.Body.String(), `"status":"approved"`) {
			t.Fatalf("status = %d, body = %s", rec.Code, rec.Body.String())
		}
		if n := countWishes(t, pool, f.eventID); n != 0 {
			t.Fatalf("wishes = %d, want 0", n)
		}
	})

	t.Run("unknown slug and draft are 404", func(t *testing.T) {
		f := newPublicEventFixture(t, pool, rdb, publicEventOpts{wishes: "open"})
		if rec := submitWish(f, "no-such-event-slug", valid, nextTestIP(), ""); rec.Code != http.StatusNotFound {
			t.Fatalf("unknown slug status = %d, want 404", rec.Code)
		}
		draft := newPublicEventFixture(t, pool, rdb, publicEventOpts{wishes: "open", unpublished: true})
		if rec := submitWish(draft, draft.slug, valid, nextTestIP(), ""); rec.Code != http.StatusNotFound {
			t.Fatalf("draft status = %d, want 404", rec.Code)
		}
	})

	t.Run("taken-down event is 404", func(t *testing.T) {
		f := newPublicEventFixture(t, pool, rdb, publicEventOpts{wishes: "open"})
		if _, err := pool.Exec(context.Background(), "UPDATE events SET status = 'taken_down' WHERE id = $1", f.eventID); err != nil {
			t.Fatal(err)
		}
		if rec := submitWish(f, f.slug, valid, nextTestIP(), ""); rec.Code != http.StatusNotFound {
			t.Fatalf("status = %d, want 404; body = %s", rec.Code, rec.Body.String())
		}
	})

	t.Run("password gate is 401", func(t *testing.T) {
		f := newPublicEventFixture(t, pool, rdb, publicEventOpts{wishes: "open", password: "letmein12345"})
		rec := submitWish(f, f.slug, valid, nextTestIP(), "")
		if rec.Code != http.StatusUnauthorized || !containsAll(rec.Body.String(), `"password_required"`) {
			t.Fatalf("status = %d, body = %s", rec.Code, rec.Body.String())
		}
	})

	t.Run("invite_only needs an invite cookie", func(t *testing.T) {
		f := newPublicEventFixture(t, pool, rdb, publicEventOpts{wishes: "open", rsvpMode: "invite_only"})
		rec := submitWish(f, f.slug, valid, nextTestIP(), "")
		if rec.Code != http.StatusForbidden || !containsAll(rec.Body.String(), `"invite_required"`) {
			t.Fatalf("status = %d, body = %s", rec.Code, rec.Body.String())
		}
		_, tok := f.testGuest(t, "Wish Guest", 1)
		if rec := submitWish(f, f.slug, valid, nextTestIP(), tok); rec.Code != http.StatusCreated {
			t.Fatalf("with invite status = %d, body = %s", rec.Code, rec.Body.String())
		}
	})

	t.Run("missing or closed block is 409", func(t *testing.T) {
		for _, mode := range []string{"", "closed"} {
			f := newPublicEventFixture(t, pool, rdb, publicEventOpts{wishes: mode})
			rec := submitWish(f, f.slug, valid, nextTestIP(), "")
			if rec.Code != http.StatusConflict || !containsAll(rec.Body.String(), `"wishes_closed"`) {
				t.Fatalf("mode %q: status = %d, body = %s", mode, rec.Code, rec.Body.String())
			}
			if n := countWishes(t, pool, f.eventID); n != 0 {
				t.Fatalf("mode %q: wishes = %d, want 0", mode, n)
			}
		}
	})

	t.Run("validation", func(t *testing.T) {
		f := newPublicEventFixture(t, pool, rdb, publicEventOpts{wishes: "open"})
		tests := []struct {
			name, body, wantCode, wantField string
		}{
			{"empty author", `{"author":"","message":"hi"}`, "validation_failed", `"author"`},
			{"blank author", `{"author":"   ","message":"hi"}`, "validation_failed", `"author"`},
			{"empty message", `{"author":"Sam","message":""}`, "validation_failed", `"message"`},
			{"blank message", `{"author":"Sam","message":" \n "}`, "validation_failed", `"message"`},
			{"author 81 chars", `{"author":"` + strings.Repeat("a", 81) + `","message":"hi"}`, "validation_failed", `"author"`},
			{"message 501 chars", `{"author":"Sam","message":"` + strings.Repeat("a", 501) + `"}`, "validation_failed", `"message"`},
			{"control char in author", `{"author":"Sa\u0000m","message":"hi"}`, "validation_failed", `"author"`},
			{"control char in message", `{"author":"Sam","message":"h\u0007i"}`, "validation_failed", `"message"`},
			{"bidi override", `{"author":"Sam","message":"a\u202eb"}`, "validation_failed", `"message"`},
			{"newline in author", `{"author":"Sa\nm","message":"hi"}`, "validation_failed", `"author"`},
			{"both invalid", `{"author":"","message":""}`, "validation_failed", `"message"`},
			{"unknown field", `{"author":"Sam","message":"hi","extra":1}`, "", ""},
			{"not json", `nope`, "", ""},
		}
		for _, tt := range tests {
			t.Run(tt.name, func(t *testing.T) {
				rec := submitWish(f, f.slug, tt.body, nextTestIP(), "")
				if rec.Code != http.StatusBadRequest {
					t.Fatalf("status = %d, body = %s, want 400", rec.Code, rec.Body.String())
				}
				if tt.wantCode != "" && !containsAll(rec.Body.String(), tt.wantCode, tt.wantField) {
					t.Fatalf("body = %s, want %s and %s", rec.Body.String(), tt.wantCode, tt.wantField)
				}
			})
		}
		if n := countWishes(t, pool, f.eventID); n != 0 {
			t.Fatalf("wishes = %d, want 0", n)
		}
		// Boundary lengths are accepted: 80-rune author, 500-rune message.
		body := `{"author":"` + strings.Repeat("é", 80) + `","message":"` + strings.Repeat("é", 500) + `"}`
		if rec := submitWish(f, f.slug, body, nextTestIP(), ""); rec.Code != http.StatusCreated {
			t.Fatalf("boundary status = %d, body = %s", rec.Code, rec.Body.String())
		}
	})

	t.Run("sixth request from one IP is rate limited", func(t *testing.T) {
		f := newPublicEventFixture(t, pool, rdb, publicEventOpts{wishes: "open"})
		ip := nextTestIP()
		for i := 1; i <= 6; i++ {
			rec := submitWish(f, f.slug, valid, ip, "")
			want := http.StatusCreated
			if i == 6 {
				want = http.StatusTooManyRequests
			}
			if rec.Code != want {
				t.Fatalf("request %d: status = %d, body = %s, want %d", i, rec.Code, rec.Body.String(), want)
			}
		}
		if n := countWishes(t, pool, f.eventID); n != 5 {
			t.Fatalf("wishes = %d, want 5", n)
		}
	})

	t.Run("cap counts wishes of every status", func(t *testing.T) {
		f := newPublicEventFixture(t, pool, rdb, publicEventOpts{wishes: "open"})
		if _, err := pool.Exec(context.Background(),
			`INSERT INTO event_wishes (id, event_id, author, message, status)
			 SELECT gen_random_uuid(), $1, 'a', 'm', (ARRAY['pending','approved','hidden'])[g % 3 + 1]
			 FROM generate_series(1, $2) g`, f.eventID, maxWishesPerEvent); err != nil {
			t.Fatalf("seed: %v", err)
		}
		rec := submitWish(f, f.slug, valid, nextTestIP(), "")
		if rec.Code != http.StatusConflict || !containsAll(rec.Body.String(), `"wish_limit"`) {
			t.Fatalf("status = %d, body = %s", rec.Code, rec.Body.String())
		}
		if n := countWishes(t, pool, f.eventID); n != maxWishesPerEvent {
			t.Fatalf("wishes = %d, want %d", n, maxWishesPerEvent)
		}
	})

	t.Run("one below the cap still accepts", func(t *testing.T) {
		f := newPublicEventFixture(t, pool, rdb, publicEventOpts{wishes: "open"})
		if _, err := pool.Exec(context.Background(),
			`INSERT INTO event_wishes (id, event_id, author, message, status)
			 SELECT gen_random_uuid(), $1, 'a', 'm', 'hidden' FROM generate_series(1, $2)`, f.eventID, maxWishesPerEvent-1); err != nil {
			t.Fatalf("seed: %v", err)
		}
		if rec := submitWish(f, f.slug, valid, nextTestIP(), ""); rec.Code != http.StatusCreated {
			t.Fatalf("status = %d, body = %s", rec.Code, rec.Body.String())
		}
	})

	t.Run("new wish is visible in the public GET immediately", func(t *testing.T) {
		f := newPublicEventFixture(t, pool, rdb, publicEventOpts{wishes: "open"})
		if rec := submitWish(f, f.slug, `{"author":"Visible","message":"hello"}`, nextTestIP(), ""); rec.Code != http.StatusCreated {
			t.Fatalf("submit status = %d, body = %s", rec.Code, rec.Body.String())
		}
		req := withSlugParam(requestWithIP(http.MethodGet, "/", nextTestIP(), nil), f.slug)
		rec := httptest.NewRecorder()
		f.s.handleGetPublicEvent(rec, req)
		if rec.Code != http.StatusOK || !containsAll(rec.Body.String(), `"author":"Visible"`, `"message":"hello"`) {
			t.Fatalf("status = %d, body = %s", rec.Code, rec.Body.String())
		}
	})
}

func TestHandleGetPublicEvent_Wishes(t *testing.T) {
	pool := dbTestPool(t)
	rdb := dbTestRedis(t)

	get := func(f *publicEventFixture) []map[string]any {
		req := withSlugParam(requestWithIP(http.MethodGet, "/", nextTestIP(), nil), f.slug)
		rec := httptest.NewRecorder()
		f.s.handleGetPublicEvent(rec, req)
		if rec.Code != http.StatusOK {
			t.Fatalf("status = %d, body = %s", rec.Code, rec.Body.String())
		}
		var resp struct {
			Wishes []map[string]any `json:"wishes"`
		}
		if err := json.Unmarshal(rec.Body.Bytes(), &resp); err != nil {
			t.Fatal(err)
		}
		if resp.Wishes == nil {
			t.Fatalf("wishes missing or null in %s", rec.Body.String())
		}
		return resp.Wishes
	}

	t.Run("no wishes block is an empty list", func(t *testing.T) {
		f := newPublicEventFixture(t, pool, rdb, publicEventOpts{})
		seedWish(t, pool, f.eventID, "Stale", "approved", time.Now())
		if got := get(f); len(got) != 0 {
			t.Fatalf("wishes = %v, want []", got)
		}
	})

	for _, mode := range []string{"open", "closed"} {
		t.Run(mode+" block lists only approved, newest first, max 30", func(t *testing.T) {
			f := newPublicEventFixture(t, pool, rdb, publicEventOpts{wishes: mode})
			base := time.Now().Add(-time.Hour)
			seedWish(t, pool, f.eventID, "Pending", "pending", base.Add(90*time.Minute))
			seedWish(t, pool, f.eventID, "Hidden", "hidden", base.Add(91*time.Minute))
			for i := 0; i < 35; i++ {
				seedWish(t, pool, f.eventID, fmt.Sprintf("Guest %02d", i), "approved", base.Add(time.Duration(i)*time.Second))
			}
			got := get(f)
			if len(got) != 30 {
				t.Fatalf("len = %d, want 30", len(got))
			}
			if got[0]["author"] != "Guest 34" || got[29]["author"] != "Guest 05" {
				t.Fatalf("order: first %v last %v", got[0]["author"], got[29]["author"])
			}
			for _, w := range got {
				if w["id"] == nil || w["message"] == nil || len(w) != 3 {
					t.Fatalf("unexpected wish shape %v", w)
				}
			}
		})
	}
}

func TestWishHostEndpoints(t *testing.T) {
	pool := dbTestPool(t)
	rdb := dbTestRedis(t)
	f := newEventTestFixture(t, pool, rdb)
	other := newEventTestFixture(t, pool, rdb)

	hostReq := func(method, target string, userID *uuid.UUID, eventID uuid.UUID, wishID *uuid.UUID) *http.Request {
		req := httptest.NewRequest(method, target, nil)
		if userID != nil {
			req = req.WithContext(context.WithValue(req.Context(), ctxUserID, *userID))
		}
		req = withRouteID(req, eventID)
		if wishID != nil {
			req = withRouteParam(req, "wishID", wishID.String())
		}
		return req
	}

	base := time.Now().Add(-time.Hour).UTC().Truncate(time.Microsecond)
	var pendingIDs []uuid.UUID
	for i := 0; i < 3; i++ {
		pendingIDs = append(pendingIDs, seedWish(t, pool, f.eventID, fmt.Sprintf("P%d", i), "pending", base.Add(time.Duration(i)*time.Minute)))
	}
	seedWish(t, pool, f.eventID, "Approved", "approved", base)
	foreign := seedWish(t, pool, other.eventID, "Foreign", "pending", base)

	type listResp struct {
		Wishes     []wishResp `json:"wishes"`
		NextCursor *string    `json:"next_cursor"`
	}
	list := func(userID *uuid.UUID, eventID uuid.UUID, query string) (*httptest.ResponseRecorder, listResp) {
		rec := httptest.NewRecorder()
		f.s.handleListEventWishes(rec, hostReq(http.MethodGet, "/?"+query, userID, eventID, nil))
		var out listResp
		if rec.Code == http.StatusOK {
			if err := json.Unmarshal(rec.Body.Bytes(), &out); err != nil {
				t.Fatal(err)
			}
		}
		return rec, out
	}

	t.Run("list auth and validation", func(t *testing.T) {
		tests := []struct {
			name    string
			user    *uuid.UUID
			event   uuid.UUID
			query   string
			want    int
			wantStr string
		}{
			{"unauthenticated", nil, f.eventID, "status=pending", http.StatusUnauthorized, ""},
			{"non-member", &f.strangerID, f.eventID, "status=pending", http.StatusNotFound, ""},
			{"unknown event", &f.ownerID, uuid.Must(uuid.NewV7()), "status=pending", http.StatusNotFound, ""},
			{"missing status", &f.ownerID, f.eventID, "", http.StatusBadRequest, "validation_failed"},
			{"bad status", &f.ownerID, f.eventID, "status=rejected", http.StatusBadRequest, "validation_failed"},
			{"bad cursor", &f.ownerID, f.eventID, "status=pending&cursor=%21%21", http.StatusBadRequest, "invalid_cursor"},
			{"viewer may read", &f.viewerID, f.eventID, "status=pending", http.StatusOK, ""},
			{"editor may read", &f.editorID, f.eventID, "status=approved", http.StatusOK, ""},
		}
		for _, tt := range tests {
			t.Run(tt.name, func(t *testing.T) {
				rec, _ := list(tt.user, tt.event, tt.query)
				if rec.Code != tt.want || (tt.wantStr != "" && !strings.Contains(rec.Body.String(), tt.wantStr)) {
					t.Fatalf("status = %d, body = %s, want %d %s", rec.Code, rec.Body.String(), tt.want, tt.wantStr)
				}
			})
		}
	})

	t.Run("list paginates newest first by status", func(t *testing.T) {
		rec, page1 := list(&f.ownerID, f.eventID, "status=pending&limit=2")
		if rec.Code != http.StatusOK || len(page1.Wishes) != 2 || page1.NextCursor == nil {
			t.Fatalf("page1: status %d, %d wishes, cursor %v", rec.Code, len(page1.Wishes), page1.NextCursor)
		}
		if page1.Wishes[0].Author != "P2" || page1.Wishes[1].Author != "P1" || page1.Wishes[0].Status != "pending" {
			t.Fatalf("page1 = %+v", page1.Wishes)
		}
		rec, page2 := list(&f.ownerID, f.eventID, "status=pending&limit=2&cursor="+*page1.NextCursor)
		if rec.Code != http.StatusOK || len(page2.Wishes) != 1 || page2.NextCursor != nil || page2.Wishes[0].Author != "P0" {
			t.Fatalf("page2: status %d, %+v, cursor %v", rec.Code, page2.Wishes, page2.NextCursor)
		}
		_, approved := list(&f.ownerID, f.eventID, "status=approved")
		if len(approved.Wishes) != 1 || approved.Wishes[0].Author != "Approved" {
			t.Fatalf("approved = %+v", approved.Wishes)
		}
		_, hidden := list(&f.ownerID, f.eventID, "status=hidden")
		if hidden.Wishes == nil || len(hidden.Wishes) != 0 {
			t.Fatalf("hidden = %+v, want empty non-nil", hidden.Wishes)
		}
	})

	type action struct {
		name    string
		handler func(http.ResponseWriter, *http.Request)
		method  string
	}
	actions := []action{
		{"approve", f.s.handleApproveWish, http.MethodPost},
		{"hide", f.s.handleHideWish, http.MethodPost},
		{"delete", f.s.handleDeleteWish, http.MethodDelete},
	}
	for _, a := range actions {
		t.Run(a.name+" auth matrix", func(t *testing.T) {
			missing := uuid.Must(uuid.NewV7())
			tests := []struct {
				name  string
				user  *uuid.UUID
				event uuid.UUID
				wish  uuid.UUID
				want  int
			}{
				{"unauthenticated", nil, f.eventID, pendingIDs[0], http.StatusUnauthorized},
				{"non-member", &f.strangerID, f.eventID, pendingIDs[0], http.StatusNotFound},
				{"viewer", &f.viewerID, f.eventID, pendingIDs[0], http.StatusForbidden},
				{"unknown wish", &f.ownerID, f.eventID, missing, http.StatusNotFound},
				{"wish of another event", &f.ownerID, f.eventID, foreign, http.StatusNotFound},
			}
			for _, tt := range tests {
				t.Run(tt.name, func(t *testing.T) {
					rec := httptest.NewRecorder()
					a.handler(rec, hostReq(a.method, "/", tt.user, tt.event, &tt.wish))
					if rec.Code != tt.want {
						t.Fatalf("status = %d, body = %s, want %d", rec.Code, rec.Body.String(), tt.want)
					}
				})
			}
		})
	}

	var status string
	statusOf := func(id uuid.UUID) string {
		if err := pool.QueryRow(context.Background(), "SELECT status FROM event_wishes WHERE id = $1", id).Scan(&status); err != nil {
			t.Fatal(err)
		}
		return status
	}

	t.Run("approve and hide as editor and owner", func(t *testing.T) {
		for _, step := range []struct {
			user    uuid.UUID
			handler func(http.ResponseWriter, *http.Request)
			want    string
		}{
			{f.editorID, f.s.handleApproveWish, "approved"},
			{f.ownerID, f.s.handleHideWish, "hidden"},
		} {
			rec := httptest.NewRecorder()
			step.handler(rec, hostReq(http.MethodPost, "/", &step.user, f.eventID, &pendingIDs[0]))
			if rec.Code != http.StatusOK || !containsAll(rec.Body.String(), `"wish":`, `"status":"`+step.want+`"`, `"author":"P0"`) {
				t.Fatalf("status = %d, body = %s", rec.Code, rec.Body.String())
			}
			if got := statusOf(pendingIDs[0]); got != step.want {
				t.Fatalf("db status = %s, want %s", got, step.want)
			}
		}
		if got := statusOf(foreign); got != "pending" {
			t.Fatalf("foreign wish status = %s, want pending", got)
		}
	})

	t.Run("delete is 204 then 404", func(t *testing.T) {
		rec := httptest.NewRecorder()
		f.s.handleDeleteWish(rec, hostReq(http.MethodDelete, "/", &f.ownerID, f.eventID, &pendingIDs[1]))
		if rec.Code != http.StatusNoContent {
			t.Fatalf("first delete = %d, body = %s", rec.Code, rec.Body.String())
		}
		rec = httptest.NewRecorder()
		f.s.handleDeleteWish(rec, hostReq(http.MethodDelete, "/", &f.ownerID, f.eventID, &pendingIDs[1]))
		if rec.Code != http.StatusNotFound {
			t.Fatalf("second delete = %d, want 404", rec.Code)
		}
	})

	t.Run("bulk delete by status", func(t *testing.T) {
		bulk := func(user *uuid.UUID, query string) *httptest.ResponseRecorder {
			rec := httptest.NewRecorder()
			f.s.handleDeleteWishesByStatus(rec, hostReq(http.MethodDelete, "/?"+query, user, f.eventID, nil))
			return rec
		}
		tests := []struct {
			name  string
			user  *uuid.UUID
			query string
			want  int
		}{
			{"unauthenticated", nil, "status=pending", http.StatusUnauthorized},
			{"non-member", &f.strangerID, "status=pending", http.StatusNotFound},
			{"viewer", &f.viewerID, "status=pending", http.StatusForbidden},
			{"missing status", &f.ownerID, "", http.StatusBadRequest},
			{"all refused", &f.ownerID, "status=all", http.StatusBadRequest},
		}
		for _, tt := range tests {
			t.Run(tt.name, func(t *testing.T) {
				if rec := bulk(tt.user, tt.query); rec.Code != tt.want {
					t.Fatalf("status = %d, body = %s, want %d", rec.Code, rec.Body.String(), tt.want)
				}
			})
		}
		// State here: pendingIDs[0] hidden, [1] deleted, [2] pending, plus one approved.
		if n := countWishes(t, pool, f.eventID); n != 3 {
			t.Fatalf("wishes after rejected calls = %d, want 3", n)
		}

		for _, step := range []struct {
			user   *uuid.UUID
			status string
			want   string
			left   int
		}{
			{&f.editorID, "pending", `"deleted":1`, 2},
			{&f.ownerID, "pending", `"deleted":0`, 2},
			{&f.ownerID, "hidden", `"deleted":1`, 1},
			{&f.ownerID, "approved", `"deleted":1`, 0},
			{&f.ownerID, "approved", `"deleted":0`, 0},
		} {
			rec := bulk(step.user, "status="+step.status)
			if rec.Code != http.StatusOK || !containsAll(rec.Body.String(), step.want) {
				t.Fatalf("%s: status = %d, body = %s, want %s", step.status, rec.Code, rec.Body.String(), step.want)
			}
			if n := countWishes(t, pool, f.eventID); n != step.left {
				t.Fatalf("%s: wishes = %d, want %d", step.status, n, step.left)
			}
			if got := statusOf(foreign); got != "pending" {
				t.Fatalf("foreign wish status = %s, want pending", got)
			}
		}
	})

	t.Run("taken-down event blocks moderation", func(t *testing.T) {
		pendingID := seedWish(t, pool, f.eventID, "TD", "pending", base)
		if _, err := pool.Exec(context.Background(), "UPDATE events SET status = 'taken_down', slug = 'wish-td-' || replace(id::text, '-', '') WHERE id = $1", f.eventID); err != nil {
			t.Fatal(err)
		}
		rec := httptest.NewRecorder()
		f.s.handleApproveWish(rec, hostReq(http.MethodPost, "/", &f.ownerID, f.eventID, &pendingID))
		if rec.Code == http.StatusOK {
			t.Fatalf("approve on taken-down event succeeded: %s", rec.Body.String())
		}
		if got := statusOf(pendingID); got != "pending" {
			t.Fatalf("status = %s, want pending", got)
		}
		rec = httptest.NewRecorder()
		f.s.handleDeleteWishesByStatus(rec, hostReq(http.MethodDelete, "/?status=pending", &f.ownerID, f.eventID, nil))
		if rec.Code != http.StatusOK || !containsAll(rec.Body.String(), `"deleted":0`) {
			t.Fatalf("bulk delete on taken-down event: status = %d, body = %s", rec.Code, rec.Body.String())
		}
		if got := statusOf(pendingID); got != "pending" {
			t.Fatalf("status after bulk delete = %s, want pending", got)
		}
	})
}
