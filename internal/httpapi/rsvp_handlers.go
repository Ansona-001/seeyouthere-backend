package httpapi

import (
	"context"
	"encoding/csv"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/go-chi/chi/v5"
	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/riverqueue/river"

	"github.com/ansonarose/seeyouthere-backend/internal/content"
	"github.com/ansonarose/seeyouthere-backend/internal/jobs"
	"github.com/ansonarose/seeyouthere-backend/internal/store"
)

var validAttending = map[string]bool{"yes": true, "no": true, "maybe": true}

// maxRSVPConfirmationsPerEventPerDay bounds send_rsvp_confirmation per event
// per UTC day: an open event with no invite gate can otherwise be used to
// mail arbitrary addresses at volume (the per-IP/per-event write caps allow
// far more RSVPs than any legitimate event needs confirmation emails for),
// none of which counts toward the per-recipient cap in internal/jobs/rsvps.go
// or MAIL_DAILY_INVITE_CAP.
const maxRSVPConfirmationsPerEventPerDay = 200

var (
	// errRSVPClosed covers every "no new commitment accepted" case: no rsvp
	// block at all, the deadline has passed, or (folded into the same
	// sentinel as errRSVPCapacityReached below) capacity is full. Switching
	// to no/maybe or lowering the head count is never blocked by this (§4.4):
	// the write-time check only applies these rules to a change that would
	// *increase* the caller's own committed heads.
	errRSVPClosed       = errors.New("rsvp closed")
	errPartyTooLarge    = errors.New("party too large")
	errRSVPEditConflict = errors.New("rsvp edit token invalidated concurrently")
)

// errRSVPCapacityReached carries the remaining spots for the 409's details.
type errRSVPCapacityReached struct{ spotsLeft int32 }

func (e *errRSVPCapacityReached) Error() string { return "rsvp capacity reached" }

// --- shared authorisation helpers (host endpoints, §4.5) ---

// loadRSVPEvent resolves the caller's access to eventID for the RSVP list/
// summary/export endpoints: any member (owner, editor or viewer) may read.
func (s *Server) loadRSVPEvent(w http.ResponseWriter, r *http.Request, eventID uuid.UUID) (eventRow, uuid.UUID, bool) {
	userID, ok := userIDFrom(r.Context())
	if !ok {
		writeError(w, http.StatusUnauthorized, "unauthorized", "Log in to continue.")
		return eventRow{}, uuid.Nil, false
	}
	row, ok, err := s.loadMemberEvent(r.Context(), userID, eventID)
	if err != nil {
		serverError(w, r, err)
		return eventRow{}, uuid.Nil, false
	}
	if !ok {
		writeError(w, http.StatusNotFound, "not_found", "No such event.")
		return eventRow{}, uuid.Nil, false
	}
	return row, userID, true
}

// requireRSVPWriteRole is loadRSVPEvent plus the owner/editor check DELETE
// needs; the SQL repeats the same check in its WHERE clause as the
// authoritative guard.
func (s *Server) requireRSVPWriteRole(w http.ResponseWriter, r *http.Request, eventID uuid.UUID) (eventRow, uuid.UUID, bool) {
	row, userID, ok := s.loadRSVPEvent(w, r, eventID)
	if !ok {
		return eventRow{}, uuid.Nil, false
	}
	if row.Role != "owner" && row.Role != "editor" {
		writeError(w, http.StatusForbidden, "forbidden", "You don't have access to manage RSVPs.")
		return eventRow{}, uuid.Nil, false
	}
	return row, userID, true
}

// --- GET /v1/events/{id}/rsvps ---

func (s *Server) handleListRSVPs(w http.ResponseWriter, r *http.Request) {
	eventID, err := parseUUIDParam(r, "id")
	if err != nil {
		writeError(w, http.StatusNotFound, "not_found", "No such event.")
		return
	}
	_, userID, ok := s.loadRSVPEvent(w, r, eventID)
	if !ok {
		return
	}

	var attendingFilter *string
	if v := r.URL.Query().Get("attending"); v != "" {
		if !validAttending[v] {
			writeError(w, http.StatusBadRequest, "validation_failed", "Invalid attending filter.")
			return
		}
		attendingFilter = &v
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

	rows, err := s.q.ListRSVPs(r.Context(), store.ListRSVPsParams{
		EventID: eventID, UserID: userID, Attending: attendingFilter,
		CursorCreatedAt: cursorCreatedAt, CursorID: cursorID, Lim: int32(limitN),
	})
	if err != nil {
		serverError(w, r, fmt.Errorf("list rsvps: %w", err))
		return
	}

	items := make([]rsvpResp, len(rows))
	for i, row := range rows {
		items[i] = rsvpResp{
			ID: row.ID, Name: row.Name, Email: row.Email, Attending: row.Attending, Count: row.Count,
			Answers: row.Answers, GuestID: row.GuestID, CreatedAt: row.CreatedAt, UpdatedAt: row.UpdatedAt,
		}
	}
	var nextCursor *string
	if len(rows) == limitN {
		nc := encodeCursor(rows[len(rows)-1].CreatedAt, rows[len(rows)-1].ID)
		nextCursor = &nc
	}
	writeJSON(w, http.StatusOK, map[string]any{"rsvps": items, "next_cursor": nextCursor})
}

// --- GET /v1/events/{id}/rsvps/summary ---

func (s *Server) handleRSVPSummary(w http.ResponseWriter, r *http.Request) {
	eventID, err := parseUUIDParam(r, "id")
	if err != nil {
		writeError(w, http.StatusNotFound, "not_found", "No such event.")
		return
	}
	row, userID, ok := s.loadRSVPEvent(w, r, eventID)
	if !ok {
		return
	}
	ctx := r.Context()

	sum, err := s.q.RSVPSummary(ctx, store.RSVPSummaryParams{EventID: eventID, UserID: userID})
	if errors.Is(err, pgx.ErrNoRows) {
		// loadRSVPEvent already confirmed access, so this would only happen
		// if the event was deleted between the two checks.
		writeError(w, http.StatusNotFound, "not_found", "No such event.")
		return
	}
	if err != nil {
		serverError(w, r, fmt.Errorf("rsvp summary: %w", err))
		return
	}

	occ, err := s.loadOccasion(ctx, s.q, row.Event.OccasionSlug)
	if err != nil {
		serverError(w, r, err)
		return
	}
	saved, err := content.ParseStored(row.Event.Content, occ)
	if err != nil {
		serverError(w, r, fmt.Errorf("parse stored content for event %s: %w", eventID, err))
		return
	}
	var capacity *int32
	if saved.RSVP != nil && saved.RSVP.Capacity != nil {
		c := int32(*saved.RSVP.Capacity)
		capacity = &c
	}

	writeJSON(w, http.StatusOK, map[string]any{
		"yes": sum.Yes, "no": sum.No, "maybe": sum.Maybe,
		"yes_heads": sum.YesHeads, "maybe_heads": sum.MaybeHeads, "total": sum.Total,
		"guests_total": sum.GuestsTotal, "guests_responded": sum.GuestsResponded,
		"capacity": capacity,
	})
}

// --- DELETE /v1/events/{id}/rsvps/{rsvpID} ---

func (s *Server) handleDeleteRSVP(w http.ResponseWriter, r *http.Request) {
	eventID, err := parseUUIDParam(r, "id")
	if err != nil {
		writeError(w, http.StatusNotFound, "not_found", "No such RSVP.")
		return
	}
	rsvpID, err := parseUUIDParam(r, "rsvpID")
	if err != nil {
		writeError(w, http.StatusNotFound, "not_found", "No such RSVP.")
		return
	}
	_, userID, ok := s.requireRSVPWriteRole(w, r, eventID)
	if !ok {
		return
	}

	n, err := s.q.DeleteRSVP(r.Context(), store.DeleteRSVPParams{ID: rsvpID, EventID: eventID, UserID: userID})
	if err != nil {
		serverError(w, r, fmt.Errorf("delete rsvp: %w", err))
		return
	}
	if n == 0 {
		writeError(w, http.StatusNotFound, "not_found", "No such RSVP.")
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

// --- GET /v1/events/{id}/rsvps.csv ---

// csvBatchSize matches §4.5's "batches of 500 via keyset"; csvHardCap is the
// safety net against an unbounded export regardless of how many RSVPs an
// event accumulates.
const (
	csvBatchSize = 500
	csvHardCap   = 10000
)

// csvSafe prefixes a cell that would otherwise be read as a spreadsheet
// formula (=, +, -, @) or that starts with a tab or CR with a leading
// single quote, neutralising it without changing what a human reading the
// file sees.
func csvSafe(s string) string {
	if s == "" {
		return s
	}
	switch s[0] {
	case '=', '+', '-', '@', '\t', '\r':
		return "'" + s
	}
	return s
}

// derefOrEmpty returns "" for a nil pointer, else the pointed-to string.
// Shared with admin_moderation_handlers.go.
func derefOrEmpty(p *string) string {
	if p == nil {
		return ""
	}
	return *p
}

// formatAnswerCell renders one stored answer value (already canonical JSON
// from content.ValidateAnswers) as a single CSV cell for field f's type.
// A missing answer (raw is empty) renders as an empty cell.
func formatAnswerCell(f content.Field, raw json.RawMessage) string {
	if len(raw) == 0 {
		return ""
	}
	switch f.Type {
	case "text", "textarea", "select":
		var s string
		_ = json.Unmarshal(raw, &s)
		return s
	case "multiselect":
		var vals []string
		_ = json.Unmarshal(raw, &vals)
		return strings.Join(vals, "; ")
	case "boolean":
		var b bool
		_ = json.Unmarshal(raw, &b)
		if b {
			return "yes"
		}
		return "no"
	case "number":
		var n int64
		_ = json.Unmarshal(raw, &n)
		return strconv.FormatInt(n, 10)
	default:
		return ""
	}
}

func (s *Server) handleExportRSVPsCSV(w http.ResponseWriter, r *http.Request) {
	eventID, err := parseUUIDParam(r, "id")
	if err != nil {
		writeError(w, http.StatusNotFound, "not_found", "No such event.")
		return
	}
	row, userID, ok := s.loadRSVPEvent(w, r, eventID)
	if !ok {
		return
	}
	if !s.allow(w, r, limit{"rsvps:export:" + userID.String(), 30, time.Hour}) {
		return
	}

	// Own 60s context (§4.5), independent of the 15s /v1 group timeout this
	// route deliberately sits outside of (see extendExportDeadline in
	// server.go).
	ctx, cancel := context.WithTimeout(r.Context(), 60*time.Second)
	defer cancel()

	occ, err := s.loadOccasion(ctx, s.q, row.Event.OccasionSlug)
	if err != nil {
		serverError(w, r, err)
		return
	}
	saved, err := content.ParseStored(row.Event.Content, occ)
	if err != nil {
		serverError(w, r, fmt.Errorf("parse stored content for event %s: %w", eventID, err))
		return
	}
	var fields []content.Field
	if saved.RSVP != nil {
		fields = content.EffectiveFields(occ, *saved.RSVP)
	}

	filename := eventID.String()
	if row.Event.Slug != nil {
		filename = *row.Event.Slug
	}
	w.Header().Set("Content-Type", "text/csv; charset=utf-8")
	w.Header().Set("Content-Disposition", fmt.Sprintf("attachment; filename=%q", filename+"-rsvps.csv"))

	// UTF-8 BOM so Excel recognises the encoding rather than guessing Latin-1.
	if _, err := w.Write([]byte("\xef\xbb\xbf")); err != nil {
		slog.ErrorContext(ctx, "csv export: write bom", "err", err, "event_id", eventID)
		return
	}
	cw := csv.NewWriter(w)
	header := []string{"name", "email", "attending", "count", "guest", "created_at", "updated_at"}
	for _, f := range fields {
		header = append(header, csvSafe(f.Label))
	}
	if err := cw.Write(header); err != nil {
		slog.ErrorContext(ctx, "csv export: write header", "err", err, "event_id", eventID)
		return
	}

	cursorCreatedAt, cursorID := time.Time{}, uuid.Nil
	total := 0
	for {
		rows, err := s.q.ExportRSVPsBatch(ctx, store.ExportRSVPsBatchParams{
			EventID: eventID, UserID: userID,
			CursorCreatedAt: cursorCreatedAt, CursorID: cursorID, Lim: csvBatchSize,
		})
		if err != nil {
			slog.ErrorContext(ctx, "csv export: query batch", "err", err, "event_id", eventID)
			return
		}
		for _, rr := range rows {
			guestCol := "open"
			if rr.GuestID != nil {
				guestCol = "invited"
			}
			record := []string{
				csvSafe(rr.Name), csvSafe(derefOrEmpty(rr.Email)), rr.Attending, strconv.Itoa(int(rr.Count)),
				guestCol, rr.CreatedAt.UTC().Format(time.RFC3339), rr.UpdatedAt.UTC().Format(time.RFC3339),
			}
			var answers map[string]json.RawMessage
			if len(rr.Answers) > 0 {
				_ = json.Unmarshal(rr.Answers, &answers) // stored data was validated at write time
			}
			for _, f := range fields {
				record = append(record, csvSafe(formatAnswerCell(f, answers[f.Key])))
			}
			if err := cw.Write(record); err != nil {
				slog.ErrorContext(ctx, "csv export: write row", "err", err, "event_id", eventID)
				return
			}
		}
		total += len(rows)
		cw.Flush()
		if err := cw.Error(); err != nil {
			slog.ErrorContext(ctx, "csv export: flush", "err", err, "event_id", eventID)
			return
		}
		if len(rows) < csvBatchSize || total >= csvHardCap {
			break
		}
		last := rows[len(rows)-1]
		cursorCreatedAt, cursorID = last.CreatedAt, last.ID
	}
}

// extendExportDeadline gives the CSV export its own 60s deadline (§4.5),
// independent of the default 15s /v1 group timeout. Routes using this must
// not also sit inside middleware.Timeout, matching extendUploadDeadline's
// convention in media_handlers.go.
func extendExportDeadline(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		rc := http.NewResponseController(w)
		deadline := time.Now().Add(60 * time.Second)
		_ = rc.SetReadDeadline(deadline)
		_ = rc.SetWriteDeadline(deadline)
		next.ServeHTTP(w, r)
	})
}

// --- PUT /v1/public/events/{slug}/rsvp (§4.4) ---

// rsvpWriteResult unifies the three possible write outcomes (a new guest
// upsert, an existing guest upsert, a new open RSVP, or an update to an
// existing open RSVP) into one shape the rest of the handler works with.
type rsvpWriteResult struct {
	ID               uuid.UUID
	GuestID          *uuid.UUID
	Name             string
	Email            *string
	Attending        string
	Count            int32
	Answers          []byte
	CreatedAt        time.Time
	UpdatedAt        time.Time
	EditTokenVersion int32
	IsNew            bool
}

func (s *Server) handlePutPublicRSVP(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	slug := chi.URLParam(r, "slug")

	var body struct {
		Name      string          `json:"name"`
		Email     *string         `json:"email"`
		Attending string          `json:"attending"`
		Count     int32           `json:"count"`
		Answers   json.RawMessage `json:"answers"`
		Website   string          `json:"website"`
	}
	if !decodeJSON(w, r, &body) {
		return
	}

	// Honeypot (§4.4): answer exactly like success, before anything else
	// (gate, rate limits, DB) even runs, so a bot filling this field learns
	// nothing about whether the slug or gate state is real. Nothing is
	// written.
	if body.Website != "" {
		writeJSON(w, http.StatusOK, map[string]any{"rsvp": rsvpResp{
			ID: uuid.Must(uuid.NewV7()), Name: strings.TrimSpace(body.Name), Email: body.Email,
			Attending: body.Attending, Count: body.Count, Answers: json.RawMessage(`{}`),
			CreatedAt: time.Now().UTC(), UpdatedAt: time.Now().UTC(),
		}})
		return
	}

	row, ok := s.resolvePublicEvent(w, r, slug)
	if !ok {
		return
	}
	eventID := row.ID

	ip := ipRateKey(clientIPFrom(ctx))
	if !s.allow(w, r,
		limit{"rsvp:ip_event:" + ip + ":" + eventID.String(), 10, time.Hour},
		limit{"rsvp:ip:" + ip, 30, time.Hour},
		limit{"rsvp:event:" + eventID.String(), 500, time.Hour},
	) {
		return
	}

	if !validAttending[body.Attending] {
		writeErrorDetails(w, http.StatusBadRequest, "validation_failed", "Check the highlighted fields.",
			[]content.Issue{{Path: "attending", Code: "invalid", Message: "Must be yes, no or maybe."}})
		return
	}
	name, ok := validateGuestName(body.Name)
	if !ok {
		writeErrorDetails(w, http.StatusBadRequest, "validation_failed", "Check the highlighted fields.",
			[]content.Issue{{Path: "name", Code: "invalid", Message: "Enter a name up to 120 characters."}})
		return
	}
	email, ok := normalizeOptionalEmail(body.Email)
	if !ok {
		writeErrorDetails(w, http.StatusBadRequest, "validation_failed", "Check the highlighted fields.",
			[]content.Issue{{Path: "email", Code: "invalid", Message: "Enter a valid email address."}})
		return
	}
	// attending = "no" forces count = 0 (§4.4); otherwise at least one head.
	count := body.Count
	if body.Attending == "no" {
		count = 0
	} else if count < 1 {
		writeErrorDetails(w, http.StatusBadRequest, "validation_failed", "Check the highlighted fields.",
			[]content.Issue{{Path: "count", Code: "invalid", Message: "Must be at least 1."}})
		return
	}

	// Identity resolution, in priority order (§4.4): a valid invite cookie
	// always wins; otherwise, under invite_only, there's nothing else this
	// caller may do.
	guest, hasGuest := s.checkInviteCookie(ctx, r, eventID)
	if !hasGuest && row.RsvpMode == "invite_only" {
		writeGateError(w, errGateInviteRequired)
		return
	}
	var openTok store.GetRSVPForTokenRow
	var hasOpenTok bool
	if !hasGuest {
		openTok, hasOpenTok = s.checkRSVPCookie(ctx, r, eventID)
	}

	if hasGuest && count > guest.HouseholdSize {
		writeError(w, http.StatusBadRequest, "party_too_large", "That's more than this household's invite allows.")
		return
	}

	var result rsvpWriteResult
	var newOpenRSVP bool
	err := s.inTx(ctx, func(tx pgx.Tx, q *store.Queries) error {
		locked, err := q.LockEventForRSVP(ctx, eventID)
		if errors.Is(err, pgx.ErrNoRows) {
			return errEventNotFound
		}
		if err != nil {
			return fmt.Errorf("lock event for rsvp: %w", err)
		}

		occ, err := s.loadOccasion(ctx, q, locked.OccasionSlug)
		if err != nil {
			return err
		}
		saved, err := content.ParseStored(locked.Content, occ)
		if err != nil {
			return fmt.Errorf("parse stored content for event %s: %w", eventID, err)
		}
		block := saved.RSVP
		if block == nil {
			return errRSVPClosed
		}

		if !hasGuest && count > int32(block.MaxPartySize) {
			return errPartyTooLarge
		}

		fields := content.EffectiveFields(occ, *block)
		answers, err := content.ValidateAnswers(fields, body.Answers, body.Attending)
		if err != nil {
			return err
		}

		// Load previous state (if any) to decide whether this write is an
		// "increase" that closed-state rules apply to (§4.4), and whether
		// the email changed (for send_rsvp_confirmation). Safe to read
		// without its own row lock: every writer for this event must first
		// take LockEventForRSVP, which serialises all of them.
		var existingID uuid.UUID
		var existingAttending string
		var existingCount int32
		var existingEmail *string
		isNew := true
		switch {
		case hasGuest:
			existing, err := q.GetGuestRSVP(ctx, store.GetGuestRSVPParams{EventID: eventID, GuestID: guest.ID})
			if err != nil && !errors.Is(err, pgx.ErrNoRows) {
				return fmt.Errorf("get guest rsvp: %w", err)
			}
			if err == nil {
				isNew = false
				existingID, existingAttending, existingCount, existingEmail = existing.ID, existing.Attending, existing.Count, existing.Email
			}
		case hasOpenTok:
			existing, err := q.GetRSVPForEvent(ctx, store.GetRSVPForEventParams{ID: openTok.ID, EventID: eventID})
			if errors.Is(err, pgx.ErrNoRows) {
				hasOpenTok = false // rotated or deleted since the cookie was checked; fall through to create
			} else if err != nil {
				return fmt.Errorf("get rsvp for event: %w", err)
			} else {
				isNew = false
				existingID, existingAttending, existingCount, existingEmail = existing.ID, existing.Attending, existing.Count, existing.Email
			}
		}

		prevYesCount := int32(0)
		if existingAttending == "yes" {
			prevYesCount = existingCount
		}
		increasing := body.Attending == "yes" && count > prevYesCount
		if increasing {
			if block.Deadline != nil && time.Now().After(*block.Deadline) {
				return errRSVPClosed
			}
			if block.Capacity != nil {
				var excludeID *uuid.UUID
				if !isNew {
					excludeID = &existingID
				}
				heads, err := q.SumYesHeads(ctx, store.SumYesHeadsParams{EventID: eventID, ExcludeID: excludeID})
				if err != nil {
					return fmt.Errorf("sum yes heads: %w", err)
				}
				left := int32(*block.Capacity) - heads
				if left < 0 {
					left = 0
				}
				if count > left {
					return &errRSVPCapacityReached{spotsLeft: left}
				}
			}
		}

		emailChanged := isNew || !stringPtrEqual(existingEmail, email)

		switch {
		case hasGuest:
			r, err := q.UpsertGuestRSVP(ctx, store.UpsertGuestRSVPParams{
				ID: uuid.Must(uuid.NewV7()), EventID: eventID, GuestID: guest.ID,
				Name: name, Email: email, Attending: body.Attending, Count: count, Answers: answers,
			})
			if err != nil {
				return fmt.Errorf("upsert guest rsvp: %w", err)
			}
			result = rsvpWriteResult{
				ID: r.ID, GuestID: r.GuestID, Name: r.Name, Email: r.Email, Attending: r.Attending, Count: r.Count,
				Answers: r.Answers, CreatedAt: r.CreatedAt, UpdatedAt: r.UpdatedAt, IsNew: r.Inserted,
			}
		case hasOpenTok:
			r, err := q.UpdateOpenRSVP(ctx, store.UpdateOpenRSVPParams{
				Name: name, Email: email, Attending: body.Attending, Count: count, Answers: answers,
				ID: existingID, EventID: eventID, EditTokenVersion: openTok.EditTokenVersion,
			})
			if errors.Is(err, pgx.ErrNoRows) {
				// The edit_token_version verified against the cookie no
				// longer matches: extremely rare (no host endpoint rotates
				// an open RSVP's token today), handled defensively anyway.
				return errRSVPEditConflict
			}
			if err != nil {
				return fmt.Errorf("update open rsvp: %w", err)
			}
			result = rsvpWriteResult{
				ID: r.ID, GuestID: r.GuestID, Name: r.Name, Email: r.Email, Attending: r.Attending, Count: r.Count,
				Answers: r.Answers, CreatedAt: r.CreatedAt, UpdatedAt: r.UpdatedAt, EditTokenVersion: r.EditTokenVersion,
			}
		default:
			r, err := q.CreateOpenRSVP(ctx, store.CreateOpenRSVPParams{
				ID: uuid.Must(uuid.NewV7()), EventID: eventID, Name: name, Email: email,
				Attending: body.Attending, Count: count, Answers: answers,
			})
			if err != nil {
				return fmt.Errorf("create open rsvp: %w", err)
			}
			result = rsvpWriteResult{
				ID: r.ID, GuestID: r.GuestID, Name: r.Name, Email: r.Email, Attending: r.Attending, Count: r.Count,
				Answers: r.Answers, CreatedAt: r.CreatedAt, UpdatedAt: r.UpdatedAt, EditTokenVersion: r.EditTokenVersion,
				IsNew: true,
			}
			newOpenRSVP = true
		}

		if isNew || emailChanged {
			// An open event with no invite gate lets anyone put an arbitrary
			// address into the RSVP form, so cap send_rsvp_confirmation the
			// same way send_invite is capped: per-event and globally, on the
			// same daily scale as MAIL_DAILY_INVITE_CAP. Over either cap,
			// skip the enqueue but still save the RSVP — a mail budget must
			// never fail the write.
			today := time.Now().UTC().Format("2006-01-02")
			eventOK, err := s.limiter.AllowN(ctx, "rsvp_confirm:event:"+eventID.String()+":"+today, 1, maxRSVPConfirmationsPerEventPerDay, 24*time.Hour)
			if err != nil {
				return fmt.Errorf("rsvp confirm event rate limit: %w", err)
			}
			globalOK := false
			if eventOK {
				globalOK, err = s.limiter.AllowN(ctx, "rsvp_confirm:global:"+today, 1, int64(s.cfg.MailDailyInviteCap), 48*time.Hour)
				if err != nil {
					s.limiter.Uncount(ctx, "rsvp_confirm:event:"+eventID.String()+":"+today, 1)
					return fmt.Errorf("rsvp confirm global rate limit: %w", err)
				}
			}
			if !eventOK || !globalOK {
				if eventOK && !globalOK {
					s.limiter.Uncount(ctx, "rsvp_confirm:event:"+eventID.String()+":"+today, 1)
				}
				slog.InfoContext(ctx, "send_rsvp_confirmation: mail cap reached, skipping enqueue",
					"event_id", eventID, "event_ok", eventOK, "global_ok", globalOK)
			} else if _, err := s.jobs.InsertTx(ctx, tx, jobs.SendRSVPConfirmationArgs{RSVPID: result.ID}, nil); err != nil {
				return fmt.Errorf("enqueue send_rsvp_confirmation: %w", err)
			}
		}
		if locked.NotifyRsvps {
			if _, err := s.jobs.InsertTx(ctx, tx, jobs.NotifyRSVPsArgs{EventID: eventID}, &river.InsertOpts{
				ScheduledAt: time.Now().Add(10 * time.Minute),
			}); err != nil {
				return fmt.Errorf("enqueue notify_rsvps: %w", err)
			}
		}
		return nil
	})

	if err != nil {
		writeRSVPWriteError(w, r, err)
		return
	}

	if newOpenRSVP {
		cookieVal := s.tokens.RSVPToken(result.ID, result.EditTokenVersion)
		s.setEventCookie(w, rsvpCookiePrefix, eventID, cookieVal, rsvpCookieTTL)
	}

	writeJSON(w, http.StatusOK, map[string]any{"rsvp": rsvpResp{
		ID: result.ID, Name: result.Name, Email: result.Email, Attending: result.Attending, Count: result.Count,
		Answers: result.Answers, GuestID: result.GuestID, CreatedAt: result.CreatedAt, UpdatedAt: result.UpdatedAt,
	}})
}

// stringPtrEqual reports whether two possibly-nil string pointers hold the
// same value.
func stringPtrEqual(a, b *string) bool {
	if a == nil || b == nil {
		return a == b
	}
	return *a == *b
}

// writeRSVPWriteError renders the outcome of the PUT /rsvp transaction.
func writeRSVPWriteError(w http.ResponseWriter, r *http.Request, err error) {
	var capErr *errRSVPCapacityReached
	switch {
	case errors.Is(err, errEventNotFound), errors.Is(err, pgx.ErrNoRows):
		writeError(w, http.StatusNotFound, "not_found", "No such event.")
	case errors.Is(err, errRSVPClosed):
		writeError(w, http.StatusConflict, "rsvp_closed", "RSVPs are closed for this event.")
	case errors.As(err, &capErr):
		writeErrorDetails(w, http.StatusConflict, "capacity_reached", "This event has reached capacity.",
			map[string]int32{"spots_left": capErr.spotsLeft})
	case errors.Is(err, errPartyTooLarge):
		writeError(w, http.StatusBadRequest, "party_too_large", "That's more people than this event allows per RSVP.")
	case errors.Is(err, errRSVPEditConflict):
		writeError(w, http.StatusConflict, "version_conflict", "This RSVP changed since you loaded it. Reload and try again.")
	default:
		writeValidationIssues(w, r, err, "Check the highlighted fields.")
	}
}
