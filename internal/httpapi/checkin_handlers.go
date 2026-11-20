package httpapi

import (
	"errors"
	"fmt"
	"net/http"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"

	"github.com/ansonarose/seeyouthere-backend/internal/content"
	"github.com/ansonarose/seeyouthere-backend/internal/store"
	"github.com/ansonarose/seeyouthere-backend/internal/token"
)

// Door check-in for hosted events. Every endpoint is open to any event member
// (owner, editor or viewer: door staff are viewers); the SQL repeats that check
// in its WHERE clause and answers "no row" for no access, which maps to 404.
const (
	maxCheckInCount = 50
	checkInLimit    = 600
	checkInWindow   = 10 * time.Minute
)

// writeInvalidPass is the one answer for every pass failure (malformed, forged,
// stale, no access, unpublished), so a scanner learns nothing about why a pass
// was refused.
func writeInvalidPass(w http.ResponseWriter) {
	writeError(w, http.StatusNotFound, "invalid_pass", "This pass isn't valid.")
}

// defaultAdmitCount is how many people a pass admits when the door doesn't say:
// the guest's RSVP count while they are attending yes or maybe (never more than
// their household and at least one), one person for a "no" (a decline is not an
// invitation to bring the household; the door can pick a larger count), otherwise
// the whole household.
func defaultAdmitCount(attending *string, rsvpCount *int32, householdSize int32) int32 {
	if attending != nil && *attending == "no" {
		return 1
	}
	if attending == nil || rsvpCount == nil || (*attending != "yes" && *attending != "maybe") {
		return householdSize
	}
	return min(max(*rsvpCount, 1), householdSize)
}

type checkInGuestResp struct {
	ID            uuid.UUID `json:"id"`
	Name          string    `json:"name"`
	HouseholdSize int32     `json:"household_size"`
}

type checkInRSVPResp struct {
	Attending string `json:"attending"`
	Count     int32  `json:"count"`
}

type checkInResp struct {
	Result string `json:"result"`
	// Source is which kind of pass was scanned: "guest" (an invited guest's pass) or "registration"
	// (the pass of an open registration, whose guest.household_size is the registered party size).
	Source          string           `json:"source"`
	EventID         uuid.UUID        `json:"event_id"`
	EventTitle      string           `json:"event_title"`
	Guest           checkInGuestResp `json:"guest"`
	RSVP            *checkInRSVPResp `json:"rsvp"`
	CheckedInAt     time.Time        `json:"checked_in_at"`
	CheckedInCount  int32            `json:"checked_in_count"`
	CheckedInByName *string          `json:"checked_in_by_name"`
}

// --- POST /v1/check-ins ---

func (s *Server) handleCheckIn(w http.ResponseWriter, r *http.Request) {
	userID, ok := userIDFrom(r.Context())
	if !ok {
		writeError(w, http.StatusUnauthorized, "unauthorized", "Log in to continue.")
		return
	}
	if !s.allow(w, r, limit{"checkin:user:" + userID.String(), checkInLimit, checkInWindow}) {
		return
	}

	var body struct {
		Token   string     `json:"token"`
		EventID *uuid.UUID `json:"event_id"`
		Count   *int32     `json:"count"`
	}
	if !decodeJSON(w, r, &body) {
		return
	}

	ctx := r.Context()
	guestID, mac, err := token.Split(body.Token)
	if err != nil {
		writeInvalidPass(w)
		return
	}
	pg, err := s.q.GetPassGuest(ctx, store.GetPassGuestParams{GuestID: guestID, UserID: userID})
	if errors.Is(err, pgx.ErrNoRows) {
		// Not a guest pass: it may be a registration's (the id is an RSVP id).
		s.checkInRegistration(w, r, userID, guestID, mac, body.EventID, body.Count)
		return
	}
	if err != nil {
		serverError(w, r, fmt.Errorf("get pass guest: %w", err))
		return
	}
	if !s.tokens.CheckPass(guestID, pg.TokenVersion, mac) {
		writeInvalidPass(w)
		return
	}
	if body.EventID != nil && *body.EventID != pg.EventID {
		writeError(w, http.StatusConflict, "wrong_event", "This pass is for a different event.")
		return
	}

	count := defaultAdmitCount(pg.RsvpAttending, pg.RsvpCount, pg.HouseholdSize)
	if body.Count != nil {
		count = *body.Count
		if count < 1 || count > pg.HouseholdSize {
			writeErrorDetails(w, http.StatusBadRequest, "validation_failed", "Check the highlighted fields.",
				[]content.Issue{{Path: "count", Code: "invalid", Message: fmt.Sprintf("Must be between 1 and %d.", pg.HouseholdSize)}})
			return
		}
	}

	resp := checkInResp{
		Source:  "guest",
		EventID: pg.EventID, EventTitle: pg.EventTitle,
		Guest: checkInGuestResp{ID: pg.ID, Name: pg.Name, HouseholdSize: pg.HouseholdSize},
	}
	if pg.RsvpAttending != nil && pg.RsvpCount != nil {
		resp.RSVP = &checkInRSVPResp{Attending: *pg.RsvpAttending, Count: *pg.RsvpCount}
	}

	done, err := s.q.CheckInGuest(ctx, store.CheckInGuestParams{
		Count: count, UserID: userID, GuestID: guestID, EventID: pg.EventID, TokenVersion: pg.TokenVersion,
	})
	switch {
	case err == nil:
		resp.Result = "checked_in"
		resp.CheckedInAt, resp.CheckedInCount, resp.CheckedInByName = done.CheckedInAt, done.CheckedInCount, done.CheckedInByName
		writeJSON(w, http.StatusOK, resp)
		return
	case !errors.Is(err, pgx.ErrNoRows):
		serverError(w, r, fmt.Errorf("check in guest: %w", err))
		return
	}

	// No row: someone else admitted the guest first, or the pass went stale or
	// the event was unpublished between the lookup and the update.
	cur, err := s.q.GetCheckIn(ctx, store.GetCheckInParams{GuestID: guestID, EventID: pg.EventID, UserID: userID})
	if errors.Is(err, pgx.ErrNoRows) {
		writeInvalidPass(w)
		return
	}
	if err != nil {
		serverError(w, r, fmt.Errorf("get check-in: %w", err))
		return
	}
	if cur.TokenVersion != pg.TokenVersion || cur.EventStatus != "published" || cur.CheckedInAt == nil {
		writeInvalidPass(w)
		return
	}
	resp.Result = "already_checked_in"
	resp.CheckedInAt, resp.CheckedInCount, resp.CheckedInByName = *cur.CheckedInAt, cur.CheckedInCount, cur.CheckedInByName
	writeJSON(w, http.StatusOK, resp)
}

// checkInRegistration admits a scanned registration pass (an open RSVP on a hosted event). Every
// failure before the admission answers like a bad guest pass, so a scanner cannot tell which kind of
// pass an id belongs to or why it was refused.
func (s *Server) checkInRegistration(w http.ResponseWriter, r *http.Request, userID, rsvpID uuid.UUID, mac []byte, wantEvent *uuid.UUID, wantCount *int32) {
	ctx := r.Context()
	pr, err := s.q.GetPassRSVP(ctx, store.GetPassRSVPParams{RsvpID: rsvpID, UserID: userID})
	if errors.Is(err, pgx.ErrNoRows) {
		writeInvalidPass(w)
		return
	}
	if err != nil {
		serverError(w, r, fmt.Errorf("get pass rsvp: %w", err))
		return
	}
	// Only a 'yes' answer holds a pass.
	if pr.Attending != "yes" || !s.tokens.CheckRSVPPass(rsvpID, pr.EditTokenVersion, mac) {
		writeInvalidPass(w)
		return
	}
	if wantEvent != nil && *wantEvent != pr.EventID {
		writeError(w, http.StatusConflict, "wrong_event", "This pass is for a different event.")
		return
	}

	count := pr.Count
	if wantCount != nil {
		count = *wantCount
		if count < 1 || count > pr.Count {
			writeErrorDetails(w, http.StatusBadRequest, "validation_failed", "Check the highlighted fields.",
				[]content.Issue{{Path: "count", Code: "invalid", Message: fmt.Sprintf("Must be between 1 and %d.", pr.Count)}})
			return
		}
	}

	resp := checkInResp{
		Source:  "registration",
		EventID: pr.EventID, EventTitle: pr.EventTitle,
		Guest: checkInGuestResp{ID: pr.ID, Name: pr.Name, HouseholdSize: pr.Count},
		RSVP:  &checkInRSVPResp{Attending: pr.Attending, Count: pr.Count},
	}
	done, err := s.q.CheckInRSVP(ctx, store.CheckInRSVPParams{
		Count: count, UserID: userID, RsvpID: rsvpID, EventID: pr.EventID, EditTokenVersion: pr.EditTokenVersion,
	})
	switch {
	case err == nil:
		resp.Result = "checked_in"
		resp.CheckedInAt, resp.CheckedInCount, resp.CheckedInByName = done.CheckedInAt, done.CheckedInCount, done.CheckedInByName
		writeJSON(w, http.StatusOK, resp)
		return
	case !errors.Is(err, pgx.ErrNoRows):
		serverError(w, r, fmt.Errorf("check in rsvp: %w", err))
		return
	}

	// No row: someone else admitted the registration first, or the pass went stale, the answer
	// changed or the event was unpublished between the lookup and the update.
	cur, err := s.q.GetRSVPCheckIn(ctx, store.GetRSVPCheckInParams{RsvpID: rsvpID, EventID: pr.EventID, UserID: userID})
	if errors.Is(err, pgx.ErrNoRows) {
		writeInvalidPass(w)
		return
	}
	if err != nil {
		serverError(w, r, fmt.Errorf("get rsvp check-in: %w", err))
		return
	}
	if cur.EditTokenVersion != pr.EditTokenVersion || cur.Attending != "yes" || cur.EventStatus != "published" || cur.CheckedInAt == nil {
		writeInvalidPass(w)
		return
	}
	resp.Result = "already_checked_in"
	resp.RSVP.Count = cur.Count
	resp.Guest.HouseholdSize = cur.Count
	resp.CheckedInAt, resp.CheckedInCount, resp.CheckedInByName = *cur.CheckedInAt, cur.CheckedInCount, cur.CheckedInByName
	writeJSON(w, http.StatusOK, resp)
}

// --- PUT /v1/events/{id}/guests/{guestID}/check-in ---

func (s *Server) handleSetGuestCheckIn(w http.ResponseWriter, r *http.Request) {
	eventID, err := parseUUIDParam(r, "id")
	if err != nil {
		writeError(w, http.StatusNotFound, "not_found", "No such event.")
		return
	}
	guestID, err := parseUUIDParam(r, "guestID")
	if err != nil {
		writeError(w, http.StatusNotFound, "not_found", "No such guest.")
		return
	}
	userID, ok := userIDFrom(r.Context())
	if !ok {
		writeError(w, http.StatusUnauthorized, "unauthorized", "Log in to continue.")
		return
	}
	if !s.allow(w, r, limit{"checkin:set:user:" + userID.String(), checkInLimit, checkInWindow}) {
		return
	}

	var body struct {
		Count *int32 `json:"count"`
	}
	if !decodeJSON(w, r, &body) {
		return
	}
	if body.Count == nil || *body.Count < 0 || *body.Count > maxCheckInCount {
		writeErrorDetails(w, http.StatusBadRequest, "validation_failed", "Check the highlighted fields.",
			[]content.Issue{{Path: "count", Code: "invalid", Message: "Must be between 0 and 50."}})
		return
	}

	ctx := r.Context()
	set, err := s.q.SetGuestCheckIn(ctx, store.SetGuestCheckInParams{
		Count: *body.Count, UserID: userID, GuestID: guestID, EventID: eventID,
	})
	if errors.Is(err, pgx.ErrNoRows) {
		// No access, not a hosted event, no such guest, count above the household, or a viewer undoing.
		writeError(w, http.StatusNotFound, "not_found", "No such guest.")
		return
	}
	if err != nil {
		serverError(w, r, fmt.Errorf("set guest check-in: %w", err))
		return
	}

	writeJSON(w, http.StatusOK, map[string]any{"guest": guestResp{
		ID: set.ID, Name: set.Name, HouseholdSize: set.HouseholdSize,
		CheckedInAt: set.CheckedInAt, CheckedInCount: set.CheckedInCount,
	}})
}

// --- PUT /v1/events/{id}/rsvps/{rsvpID}/check-in ---

type setRSVPCheckInResp struct {
	ID             uuid.UUID  `json:"id"`
	Name           string     `json:"name"`
	Count          int32      `json:"count"`
	CheckedInAt    *time.Time `json:"checked_in_at"`
	CheckedInCount int32      `json:"checked_in_count"`
}

func (s *Server) handleSetRSVPCheckIn(w http.ResponseWriter, r *http.Request) {
	eventID, err := parseUUIDParam(r, "id")
	if err != nil {
		writeError(w, http.StatusNotFound, "not_found", "No such event.")
		return
	}
	rsvpID, err := parseUUIDParam(r, "rsvpID")
	if err != nil {
		writeError(w, http.StatusNotFound, "not_found", "No such registration.")
		return
	}
	userID, ok := userIDFrom(r.Context())
	if !ok {
		writeError(w, http.StatusUnauthorized, "unauthorized", "Log in to continue.")
		return
	}
	if !s.allow(w, r, limit{"checkin:set:user:" + userID.String(), checkInLimit, checkInWindow}) {
		return
	}

	var body struct {
		Count *int32 `json:"count"`
	}
	if !decodeJSON(w, r, &body) {
		return
	}
	if body.Count == nil || *body.Count < 0 || *body.Count > maxCheckInCount {
		writeErrorDetails(w, http.StatusBadRequest, "validation_failed", "Check the highlighted fields.",
			[]content.Issue{{Path: "count", Code: "invalid", Message: "Must be between 0 and 50."}})
		return
	}

	set, err := s.q.SetRSVPCheckIn(r.Context(), store.SetRSVPCheckInParams{
		Count: *body.Count, UserID: userID, RsvpID: rsvpID, EventID: eventID,
	})
	if errors.Is(err, pgx.ErrNoRows) {
		// No access, not a hosted event, no such registration, count above the party, a decline being
		// admitted, or a viewer undoing.
		writeError(w, http.StatusNotFound, "not_found", "No such registration.")
		return
	}
	if err != nil {
		serverError(w, r, fmt.Errorf("set rsvp check-in: %w", err))
		return
	}
	writeJSON(w, http.StatusOK, setRSVPCheckInResp{
		ID: set.ID, Name: set.Name, Count: set.Count,
		CheckedInAt: set.CheckedInAt, CheckedInCount: set.CheckedInCount,
	})
}

// --- GET /v1/events/{id}/registrations ---

// registrationResp is one open registration of a hosted event. Email is nil for viewers (door staff),
// who see who registered but never contact details; the list also omits answers.
type registrationResp struct {
	ID             uuid.UUID  `json:"id"`
	Name           string     `json:"name"`
	Email          *string    `json:"email"`
	Attending      string     `json:"attending"`
	Count          int32      `json:"count"`
	CheckedInAt    *time.Time `json:"checked_in_at"`
	CheckedInCount int32      `json:"checked_in_count"`
}

func (s *Server) handleListRegistrations(w http.ResponseWriter, r *http.Request) {
	eventID, err := parseUUIDParam(r, "id")
	if err != nil {
		writeError(w, http.StatusNotFound, "not_found", "No such event.")
		return
	}
	row, userID, ok := s.loadGuestEvent(w, r, eventID)
	if !ok {
		return
	}

	limitN := clampLimit(r.URL.Query().Get("limit"), 25, 100)
	var cursorCreatedAt time.Time
	var cursorID uuid.UUID
	if c := r.URL.Query().Get("cursor"); c != "" {
		t, id, err := decodeCursor(c)
		if err != nil {
			writeError(w, http.StatusBadRequest, "invalid_cursor", "Invalid cursor.")
			return
		}
		cursorCreatedAt, cursorID = t, id
	}
	pattern := buildLikePattern(r.URL.Query().Get("q"))
	// Viewers (door staff) get names and counts only: no contact details, and no searching by email.
	seesContacts := row.Role == "owner" || row.Role == "editor"

	rows, err := s.q.ListRegistrations(r.Context(), store.ListRegistrationsParams{
		EventID: eventID, UserID: userID, Pattern: pattern, MatchEmail: seesContacts,
		CursorCreatedAt: cursorCreatedAt, CursorID: cursorID, Lim: int32(limitN),
	})
	if err != nil {
		serverError(w, r, fmt.Errorf("list registrations: %w", err))
		return
	}

	items := make([]registrationResp, len(rows))
	for i, rr := range rows {
		items[i] = registrationResp{
			ID: rr.ID, Name: rr.Name, Attending: rr.Attending, Count: rr.Count,
			CheckedInAt: rr.CheckedInAt, CheckedInCount: rr.CheckedInCount,
		}
		if seesContacts {
			items[i].Email = rr.Email
		}
	}
	var nextCursor *string
	if len(rows) == limitN {
		nc := encodeCursor(rows[len(rows)-1].CreatedAt, rows[len(rows)-1].ID)
		nextCursor = &nc
	}
	writeJSON(w, http.StatusOK, map[string]any{"registrations": items, "next_cursor": nextCursor})
}

// --- GET /v1/events/{id}/check-ins/summary ---

func (s *Server) handleCheckInSummary(w http.ResponseWriter, r *http.Request) {
	eventID, err := parseUUIDParam(r, "id")
	if err != nil {
		writeError(w, http.StatusNotFound, "not_found", "No such event.")
		return
	}
	userID, ok := userIDFrom(r.Context())
	if !ok {
		writeError(w, http.StatusUnauthorized, "unauthorized", "Log in to continue.")
		return
	}
	sum, err := s.q.CheckInSummary(r.Context(), store.CheckInSummaryParams{EventID: eventID, UserID: userID})
	if errors.Is(err, pgx.ErrNoRows) {
		writeError(w, http.StatusNotFound, "not_found", "No such event.")
		return
	}
	if err != nil {
		serverError(w, r, fmt.Errorf("check-in summary: %w", err))
		return
	}
	writeJSON(w, http.StatusOK, map[string]int64{
		"guests": sum.Guests, "invited_people": sum.InvitedPeople, "registrations": sum.Registrations,
		"expected_people": sum.ExpectedPeople, "checked_in_passes": sum.CheckedInPasses,
		"checked_in_people": sum.CheckedInPeople,
	})
}
