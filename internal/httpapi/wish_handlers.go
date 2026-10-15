package httpapi

import (
	"errors"
	"fmt"
	"net/http"
	"time"

	"github.com/go-chi/chi/v5"
	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"

	"github.com/ansonarose/seeyouthere-backend/internal/content"
	"github.com/ansonarose/seeyouthere-backend/internal/store"
)

const (
	// maxWishesPerEvent caps all wishes per event, whatever their status, so
	// published spam can't grow without bound; the host frees room by deleting.
	maxWishesPerEvent = 500
	// publicWishesLimit is how many approved wishes the public page embeds.
	publicWishesLimit = 30
)

var errWishLimit = errors.New("wish limit reached")

var validWishStatus = map[string]bool{"pending": true, "approved": true, "hidden": true}

// publicWishResp is one approved wish on the public page.
type publicWishResp struct {
	ID      uuid.UUID `json:"id"`
	Author  string    `json:"author"`
	Message string    `json:"message"`
}

// wishResp is a wish as the host's moderation queue sees it.
type wishResp struct {
	ID        uuid.UUID `json:"id"`
	Author    string    `json:"author"`
	Message   string    `json:"message"`
	Status    string    `json:"status"`
	CreatedAt time.Time `json:"created_at"`
	UpdatedAt time.Time `json:"updated_at"`
}

// listApprovedWishes loads the public wall. Callers only invoke it when the
// event has a wishes block, so other events cost no query.
func (s *Server) listApprovedWishes(r *http.Request, eventID uuid.UUID) ([]publicWishResp, error) {
	rows, err := s.q.ListApprovedWishes(r.Context(), store.ListApprovedWishesParams{EventID: eventID, Lim: publicWishesLimit})
	if err != nil {
		return nil, fmt.Errorf("list approved wishes: %w", err)
	}
	out := make([]publicWishResp, len(rows))
	for i, row := range rows {
		out[i] = publicWishResp{ID: row.ID, Author: row.Author, Message: row.Message}
	}
	return out, nil
}

// --- POST /v1/public/events/{slug}/wishes ---

// handleCreateWish publishes a guest wish immediately (status approved); the
// host deletes or hides what they don't want. It re-runs the public
// visibility gate, requires an invite cookie for invite_only events, and
// requires the wishes block to be open.
func (s *Server) handleCreateWish(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()

	var body struct {
		Author  string `json:"author"`
		Message string `json:"message"`
		Website string `json:"website"`
	}
	if !decodeJSON(w, r, &body) {
		return
	}
	// Honeypot: answer exactly like success before touching the gate, limits
	// or DB, so a bot learns nothing. Nothing is written.
	if body.Website != "" {
		writeJSON(w, http.StatusCreated, map[string]string{"status": "approved"})
		return
	}

	row, ok := s.resolvePublicEvent(w, r, chi.URLParam(r, "slug"))
	if !ok {
		return
	}
	if row.RsvpMode == "invite_only" {
		if _, ok := s.checkInviteCookie(ctx, r, row.ID); !ok {
			writeGateError(w, errGateInviteRequired)
			return
		}
	}

	ip := ipRateKey(clientIPFrom(ctx))
	if !s.allow(w, r,
		limit{"wish:ip_event:" + ip + ":" + row.ID.String(), 5, time.Hour},
		limit{"wish:ip:" + ip, 20, time.Hour},
		limit{"wish:event:" + row.ID.String(), 200, time.Hour},
	) {
		return
	}

	var issues []content.Issue
	author, ok := content.CleanText(body.Author, content.WishAuthorMax, false)
	if !ok {
		issues = append(issues, content.Issue{Path: "author", Code: "invalid",
			Message: fmt.Sprintf("Enter your name, up to %d characters.", content.WishAuthorMax)})
	}
	message, ok := content.CleanText(body.Message, content.WishMessageMax, true)
	if !ok {
		issues = append(issues, content.Issue{Path: "message", Code: "invalid",
			Message: fmt.Sprintf("Write a wish, up to %d characters.", content.WishMessageMax)})
	}
	if len(issues) > 0 {
		writeErrorDetails(w, http.StatusBadRequest, "validation_failed", "Check the highlighted fields.", issues)
		return
	}

	occ, err := s.loadOccasion(ctx, s.q, row.OccasionSlug)
	if err != nil {
		serverError(w, r, err)
		return
	}
	saved, err := content.ParseStored(row.Content, occ)
	if err != nil {
		serverError(w, r, fmt.Errorf("parse stored content for event %s: %w", row.ID, err))
		return
	}
	if saved.Wishes == nil || !saved.Wishes.Open {
		writeError(w, http.StatusConflict, "wishes_closed", "This event isn't accepting wishes.")
		return
	}

	err = s.inTx(ctx, func(_ pgx.Tx, q *store.Queries) error {
		// The event row lock makes the cap exact under concurrent submissions
		// and re-confirms the event is still published.
		if _, err := q.LockEventForRSVP(ctx, row.ID); err != nil {
			return err
		}
		if _, err := q.CreateWish(ctx, store.CreateWishParams{
			ID: uuid.Must(uuid.NewV7()), EventID: row.ID, Author: author, Message: message,
			MaxPerEvent: maxWishesPerEvent,
		}); err != nil {
			if errors.Is(err, pgx.ErrNoRows) {
				return errWishLimit
			}
			return fmt.Errorf("create wish: %w", err)
		}
		return nil
	})
	switch {
	case errors.Is(err, errWishLimit):
		writeError(w, http.StatusConflict, "wish_limit", "This event has reached its wish limit.")
	case errors.Is(err, pgx.ErrNoRows):
		writeError(w, http.StatusNotFound, "not_found", "No such event.")
	case err != nil:
		serverError(w, r, fmt.Errorf("submit wish for event %s: %w", row.ID, err))
	default:
		writeJSON(w, http.StatusCreated, map[string]string{"status": "approved"})
	}
}

// --- GET /v1/events/{id}/wishes ---

func (s *Server) handleListEventWishes(w http.ResponseWriter, r *http.Request) {
	eventID, err := parseUUIDParam(r, "id")
	if err != nil {
		writeError(w, http.StatusNotFound, "not_found", "No such event.")
		return
	}
	_, userID, ok := s.loadRSVPEvent(w, r, eventID)
	if !ok {
		return
	}

	status := r.URL.Query().Get("status")
	if !validWishStatus[status] {
		writeErrorDetails(w, http.StatusBadRequest, "validation_failed", "Check the highlighted fields.",
			[]content.Issue{{Path: "status", Code: "invalid", Message: "Must be pending, approved or hidden."}})
		return
	}
	limitN := clampLimit(r.URL.Query().Get("limit"), 25, 100)
	cursorCreatedAt, cursorID := farFutureCursorTime, uuid.Max
	if c := r.URL.Query().Get("cursor"); c != "" {
		t, id, err := decodeCursor(c)
		if err != nil {
			writeError(w, http.StatusBadRequest, "invalid_cursor", "Invalid cursor.")
			return
		}
		cursorCreatedAt, cursorID = t, id
	}

	rows, err := s.q.ListEventWishes(r.Context(), store.ListEventWishesParams{
		EventID: eventID, UserID: userID, Status: status,
		CursorCreatedAt: cursorCreatedAt, CursorID: cursorID, Lim: int32(limitN),
	})
	if err != nil {
		serverError(w, r, fmt.Errorf("list event wishes: %w", err))
		return
	}
	items := make([]wishResp, len(rows))
	for i, row := range rows {
		items[i] = wishResp{ID: row.ID, Author: row.Author, Message: row.Message, Status: row.Status,
			CreatedAt: row.CreatedAt, UpdatedAt: row.UpdatedAt}
	}
	var next *string
	if len(rows) == limitN {
		nc := encodeCursor(rows[len(rows)-1].CreatedAt, rows[len(rows)-1].ID)
		next = &nc
	}
	writeJSON(w, http.StatusOK, map[string]any{"wishes": items, "next_cursor": next})
}

// --- POST /v1/events/{id}/wishes/{wishID}/approve and /hide ---

func (s *Server) handleApproveWish(w http.ResponseWriter, r *http.Request) {
	s.setWishStatus(w, r, "approved")
}

func (s *Server) handleHideWish(w http.ResponseWriter, r *http.Request) {
	s.setWishStatus(w, r, "hidden")
}

// wishParams parses the event and wish ids and applies the owner/editor
// pre-check; the SQL repeats the role check as the authoritative guard.
func (s *Server) wishParams(w http.ResponseWriter, r *http.Request) (eventID, wishID, userID uuid.UUID, ok bool) {
	eventID, err := parseUUIDParam(r, "id")
	if err != nil {
		writeError(w, http.StatusNotFound, "not_found", "No such event.")
		return
	}
	wishID, err = parseUUIDParam(r, "wishID")
	if err != nil {
		writeError(w, http.StatusNotFound, "not_found", "No such wish.")
		return
	}
	_, userID, ok = s.requireRSVPWriteRole(w, r, eventID)
	return eventID, wishID, userID, ok
}

func (s *Server) setWishStatus(w http.ResponseWriter, r *http.Request, status string) {
	eventID, wishID, userID, ok := s.wishParams(w, r)
	if !ok {
		return
	}
	row, err := s.q.SetWishStatus(r.Context(), store.SetWishStatusParams{
		Status: status, WishID: wishID, EventID: eventID, UserID: userID,
	})
	if errors.Is(err, pgx.ErrNoRows) {
		writeError(w, http.StatusNotFound, "not_found", "No such wish.")
		return
	}
	if err != nil {
		serverError(w, r, fmt.Errorf("set wish status: %w", err))
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"wish": wishResp{
		ID: row.ID, Author: row.Author, Message: row.Message, Status: row.Status,
		CreatedAt: row.CreatedAt, UpdatedAt: row.UpdatedAt,
	}})
}

// --- DELETE /v1/events/{id}/wishes/{wishID} ---

func (s *Server) handleDeleteWish(w http.ResponseWriter, r *http.Request) {
	eventID, wishID, userID, ok := s.wishParams(w, r)
	if !ok {
		return
	}
	n, err := s.q.DeleteWish(r.Context(), store.DeleteWishParams{WishID: wishID, EventID: eventID, UserID: userID})
	if err != nil {
		serverError(w, r, fmt.Errorf("delete wish: %w", err))
		return
	}
	if n == 0 {
		writeError(w, http.StatusNotFound, "not_found", "No such wish.")
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

// --- DELETE /v1/events/{id}/wishes?status=approved|pending|hidden ---

// handleDeleteWishesByStatus clears one status in a single statement. The
// status is required so a bare DELETE can never wipe every wish.
func (s *Server) handleDeleteWishesByStatus(w http.ResponseWriter, r *http.Request) {
	eventID, err := parseUUIDParam(r, "id")
	if err != nil {
		writeError(w, http.StatusNotFound, "not_found", "No such event.")
		return
	}
	_, userID, ok := s.requireRSVPWriteRole(w, r, eventID)
	if !ok {
		return
	}
	status := r.URL.Query().Get("status")
	if !validWishStatus[status] {
		writeErrorDetails(w, http.StatusBadRequest, "validation_failed", "Check the highlighted fields.",
			[]content.Issue{{Path: "status", Code: "invalid", Message: "Must be pending, approved or hidden."}})
		return
	}
	n, err := s.q.DeleteWishesByStatus(r.Context(), store.DeleteWishesByStatusParams{EventID: eventID, UserID: userID, Status: status})
	if err != nil {
		serverError(w, r, fmt.Errorf("delete wishes by status: %w", err))
		return
	}
	writeJSON(w, http.StatusOK, map[string]int64{"deleted": n})
}
