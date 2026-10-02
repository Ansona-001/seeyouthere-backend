package httpapi

import (
	"context"
	"encoding/csv"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"mime"
	"net/http"
	"strconv"
	"strings"
	"time"
	"unicode"
	"unicode/utf8"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"

	"github.com/ansonarose/seeyouthere-backend/internal/content"
	"github.com/ansonarose/seeyouthere-backend/internal/jobs"
	"github.com/ansonarose/seeyouthere-backend/internal/ratelimit"
	"github.com/ansonarose/seeyouthere-backend/internal/store"
)

// Guest limits (§4.6, §14: guests are one of the exact per-event caps, so
// creation and CSV import both run inside LockEventForEditor).
const (
	maxGuestsPerEvent  = 2000
	maxSearchQueryRune = 80
	maxSendInviteIDs   = 500

	maxGuestCSVBytes    = 256 << 10
	maxGuestCSVDataRows = 1000
	maxCSVIssues        = 50
)

var (
	errGuestLimit = errors.New("guest limit exceeded")
	errDailyLimit = errors.New("daily invite limit exceeded")
)

// bomPrefix is a leading UTF-8 BOM (U+FEFF), written via rune conversion
// rather than a literal in the source so no editor or tool normalises it away.
var bomPrefix = string(rune(0xFEFF))

// --- validation helpers ---

// validateGuestName trims s and requires 1-120 runes with no control
// characters (guests.guests_name_len_check; a guest name is a single-line
// field, so even \n is rejected).
func validateGuestName(s string) (string, bool) {
	s = strings.TrimSpace(s)
	n := utf8.RuneCountInString(s)
	if n < 1 || n > 120 {
		return "", false
	}
	for _, r := range s {
		if unicode.IsControl(r) {
			return "", false
		}
	}
	return s, true
}

// normalizePhone strips spaces and ()-. , keeps a leading +, and requires
// the result to match guests.guests_phone_check (§4.6).
func normalizePhone(raw string) (string, bool) {
	var b strings.Builder
	for i, r := range strings.TrimSpace(raw) {
		switch {
		case r == '+' && i == 0:
			b.WriteRune(r)
		case r >= '0' && r <= '9':
			b.WriteRune(r)
		case r == ' ' || r == '(' || r == ')' || r == '-' || r == '.':
			// dropped
		default:
			return "", false
		}
	}
	s := b.String()
	digits := strings.TrimPrefix(s, "+")
	if len(digits) < 6 || len(digits) > 15 {
		return "", false
	}
	for _, r := range digits {
		if r < '0' || r > '9' {
			return "", false
		}
	}
	return s, true
}

// validateHouseholdSize enforces guests.household_size's CHECK (1..50).
func validateHouseholdSize(n int32) bool { return n >= 1 && n <= 50 }

// buildLikePattern turns free-text search into a %-wrapped ILIKE pattern
// with %, _ and the escape character itself escaped, so the caller's text
// can never be read as a wildcard, and truncates to maxSearchQueryRune runes.
func buildLikePattern(q string) *string {
	q = strings.TrimSpace(q)
	if q == "" {
		return nil
	}
	if utf8.RuneCountInString(q) > maxSearchQueryRune {
		runes := []rune(q)
		q = string(runes[:maxSearchQueryRune])
	}
	var b strings.Builder
	for _, r := range q {
		if r == '\\' || r == '%' || r == '_' {
			b.WriteByte('\\')
		}
		b.WriteRune(r)
	}
	pattern := "%" + b.String() + "%"
	return &pattern
}

// --- response shape (§4.1 Guest) ---

type guestRSVPResp struct {
	Attending string    `json:"attending"`
	Count     int32     `json:"count"`
	UpdatedAt time.Time `json:"updated_at"`
}

type guestResp struct {
	ID            uuid.UUID      `json:"id"`
	Name          string         `json:"name"`
	Email         *string        `json:"email"`
	Phone         *string        `json:"phone"`
	HouseholdSize int32          `json:"household_size"`
	InvitedAt     *time.Time     `json:"invited_at"`
	CreatedAt     time.Time      `json:"created_at"`
	RSVP          *guestRSVPResp `json:"rsvp"`
	InviteURL     *string        `json:"invite_url"`
}

// buildGuestResp assembles the host-facing Guest shape (§4.1). invite_url is
// populated only for a caller who can act on it (owner/editor) and only once
// the event has a slug; it's derived fresh from the guest's current
// token_version, so a rotated link never shows the caller a stale one.
func (s *Server) buildGuestResp(role string, slug *string, id uuid.UUID, name string, email, phone *string, householdSize, tokenVersion int32, invitedAt *time.Time, createdAt time.Time, rsvp *guestRSVPResp) guestResp {
	var url *string
	if slug != nil && (role == "owner" || role == "editor") {
		link := s.tokens.GuestToken(id, tokenVersion)
		u := fmt.Sprintf("%s/%s/invite#%s", s.cfg.SiteURL, *slug, link)
		url = &u
	}
	return guestResp{
		ID: id, Name: name, Email: email, Phone: phone, HouseholdSize: householdSize,
		InvitedAt: invitedAt, CreatedAt: createdAt, RSVP: rsvp, InviteURL: url,
	}
}

// loadGuestEvent resolves the caller's access to eventID for the guest
// endpoints: 404 for no role, else the row (slug + role) the caller needs to
// authorise a write or build invite_url.
func (s *Server) loadGuestEvent(w http.ResponseWriter, r *http.Request, eventID uuid.UUID) (eventRow, uuid.UUID, bool) {
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

// requireGuestWriteRole is loadGuestEvent plus the owner/editor check every
// guest-mutating endpoint needs; the SQL for the mutation itself repeats the
// same check in its WHERE clause as the authoritative guard.
func (s *Server) requireGuestWriteRole(w http.ResponseWriter, r *http.Request, eventID uuid.UUID) (eventRow, uuid.UUID, bool) {
	row, userID, ok := s.loadGuestEvent(w, r, eventID)
	if !ok {
		return eventRow{}, uuid.Nil, false
	}
	if row.Role != "owner" && row.Role != "editor" {
		writeError(w, http.StatusForbidden, "forbidden", "You don't have access to manage guests.")
		return eventRow{}, uuid.Nil, false
	}
	return row, userID, true
}

// --- GET /v1/events/{id}/guests ---

func (s *Server) handleListGuests(w http.ResponseWriter, r *http.Request) {
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

	ctx := r.Context()
	rows, err := s.q.ListGuests(ctx, store.ListGuestsParams{
		EventID: eventID, UserID: userID, Pattern: pattern,
		CursorCreatedAt: cursorCreatedAt, CursorID: cursorID, Lim: int32(limitN),
	})
	if err != nil {
		serverError(w, r, fmt.Errorf("list guests: %w", err))
		return
	}

	guests := make([]guestResp, len(rows))
	for i, g := range rows {
		var rsvp *guestRSVPResp
		if g.RsvpAttending != nil {
			rsvp = &guestRSVPResp{Attending: *g.RsvpAttending, Count: *g.RsvpCount, UpdatedAt: *g.RsvpUpdatedAt}
		}
		guests[i] = s.buildGuestResp(row.Role, row.Event.Slug, g.ID, g.Name, g.Email, g.Phone, g.HouseholdSize, g.TokenVersion, g.InvitedAt, g.CreatedAt, rsvp)
	}
	var nextCursor *string
	if len(rows) == limitN {
		nc := encodeCursor(rows[len(rows)-1].CreatedAt, rows[len(rows)-1].ID)
		nextCursor = &nc
	}
	writeJSON(w, http.StatusOK, map[string]any{"guests": guests, "next_cursor": nextCursor})
}

// --- POST /v1/events/{id}/guests ---

func (s *Server) handleCreateGuest(w http.ResponseWriter, r *http.Request) {
	eventID, err := parseUUIDParam(r, "id")
	if err != nil {
		writeError(w, http.StatusNotFound, "not_found", "No such event.")
		return
	}
	row, userID, ok := s.requireGuestWriteRole(w, r, eventID)
	if !ok {
		return
	}
	if !s.allow(w, r, limit{"guests:create:" + userID.String(), 300, time.Hour}) {
		return
	}

	var body struct {
		Name          string  `json:"name"`
		Email         *string `json:"email"`
		Phone         *string `json:"phone"`
		HouseholdSize *int32  `json:"household_size"`
	}
	if !decodeJSON(w, r, &body) {
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
	phone, ok := normalizeOptionalPhone(body.Phone)
	if !ok {
		writeErrorDetails(w, http.StatusBadRequest, "validation_failed", "Check the highlighted fields.",
			[]content.Issue{{Path: "phone", Code: "invalid", Message: "Enter a valid phone number."}})
		return
	}
	if body.HouseholdSize == nil {
		writeErrorDetails(w, http.StatusBadRequest, "validation_failed", "Check the highlighted fields.",
			[]content.Issue{{Path: "household_size", Code: "required", Message: "This field is required."}})
		return
	}
	if !validateHouseholdSize(*body.HouseholdSize) {
		writeErrorDetails(w, http.StatusBadRequest, "validation_failed", "Check the highlighted fields.",
			[]content.Issue{{Path: "household_size", Code: "invalid", Message: "Must be between 1 and 50."}})
		return
	}

	ctx := r.Context()
	var created store.CreateGuestRow
	err = s.inTx(ctx, func(_ pgx.Tx, q *store.Queries) error {
		if _, err := q.LockEventForEditor(ctx, store.LockEventForEditorParams{EventID: eventID, UserID: userID}); err != nil {
			return err
		}
		count, err := q.CountGuests(ctx, eventID)
		if err != nil {
			return fmt.Errorf("count guests: %w", err)
		}
		if count >= maxGuestsPerEvent {
			return errGuestLimit
		}
		g, err := q.CreateGuest(ctx, store.CreateGuestParams{
			ID: uuid.Must(uuid.NewV7()), EventID: eventID, Name: name, Email: email, Phone: phone,
			HouseholdSize: *body.HouseholdSize, UserID: userID,
		})
		if err != nil {
			return err
		}
		created = g
		return nil
	})
	switch {
	case errors.Is(err, errGuestLimit):
		writeError(w, http.StatusConflict, "guest_limit", "This event has reached its guest limit.")
		return
	case isUniqueViolation(err, "guests_event_email_key"):
		writeError(w, http.StatusConflict, "guest_exists", "A guest with this email is already on the list.")
		return
	case errors.Is(err, pgx.ErrNoRows):
		writeError(w, http.StatusNotFound, "not_found", "No such event.")
		return
	case err != nil:
		serverError(w, r, fmt.Errorf("create guest: %w", err))
		return
	}

	resp := s.buildGuestResp(row.Role, row.Event.Slug, created.ID, created.Name, created.Email, created.Phone,
		created.HouseholdSize, created.TokenVersion, created.InvitedAt, created.CreatedAt, nil)
	writeJSON(w, http.StatusCreated, map[string]any{"guest": resp})
}

// --- PATCH /v1/events/{id}/guests/{guestID} ---

func (s *Server) handleUpdateGuest(w http.ResponseWriter, r *http.Request) {
	eventID, err := parseUUIDParam(r, "id")
	if err != nil {
		writeError(w, http.StatusNotFound, "not_found", "No such guest.")
		return
	}
	guestID, err := parseUUIDParam(r, "guestID")
	if err != nil {
		writeError(w, http.StatusNotFound, "not_found", "No such guest.")
		return
	}
	row, userID, ok := s.requireGuestWriteRole(w, r, eventID)
	if !ok {
		return
	}

	// Presence (not just non-nil) matters for email/phone: an omitted key
	// leaves the field unchanged, an explicit null clears it. A plain struct
	// can't tell those apart (both decode to a nil pointer), so the body is
	// decoded into a raw map first.
	var raw map[string]json.RawMessage
	if !decodeJSON(w, r, &raw) {
		return
	}
	allowedFields := map[string]bool{"name": true, "email": true, "phone": true, "household_size": true}
	for k := range raw {
		if !allowedFields[k] {
			writeError(w, http.StatusBadRequest, "invalid_json", "Unknown field.")
			return
		}
	}

	var namePtr *string
	if v, present := raw["name"]; present {
		var s string
		if err := json.Unmarshal(v, &s); err != nil {
			writeErrorDetails(w, http.StatusBadRequest, "validation_failed", "Check the highlighted fields.",
				[]content.Issue{{Path: "name", Code: "invalid", Message: "Enter a name up to 120 characters."}})
			return
		}
		name, ok := validateGuestName(s)
		if !ok {
			writeErrorDetails(w, http.StatusBadRequest, "validation_failed", "Check the highlighted fields.",
				[]content.Issue{{Path: "name", Code: "invalid", Message: "Enter a name up to 120 characters."}})
			return
		}
		namePtr = &name
	}

	setEmail, emailPtr, ok := decodeNullableField(raw, "email", func(s string) (string, bool) { return normalizeEmail(s) })
	if !ok {
		writeErrorDetails(w, http.StatusBadRequest, "validation_failed", "Check the highlighted fields.",
			[]content.Issue{{Path: "email", Code: "invalid", Message: "Enter a valid email address."}})
		return
	}
	setPhone, phonePtr, ok := decodeNullableField(raw, "phone", normalizePhone)
	if !ok {
		writeErrorDetails(w, http.StatusBadRequest, "validation_failed", "Check the highlighted fields.",
			[]content.Issue{{Path: "phone", Code: "invalid", Message: "Enter a valid phone number."}})
		return
	}

	var householdPtr *int32
	if v, present := raw["household_size"]; present {
		var n int32
		if err := json.Unmarshal(v, &n); err != nil || !validateHouseholdSize(n) {
			writeErrorDetails(w, http.StatusBadRequest, "validation_failed", "Check the highlighted fields.",
				[]content.Issue{{Path: "household_size", Code: "invalid", Message: "Must be between 1 and 50."}})
			return
		}
		householdPtr = &n
	}

	ctx := r.Context()
	updated, err := s.q.UpdateGuest(ctx, store.UpdateGuestParams{
		Name: namePtr, HouseholdSize: householdPtr,
		SetEmail: setEmail, Email: emailPtr, SetPhone: setPhone, Phone: phonePtr,
		GuestID: guestID, EventID: eventID, UserID: userID,
	})
	switch {
	case isUniqueViolation(err, "guests_event_email_key"):
		writeError(w, http.StatusConflict, "guest_exists", "A guest with this email is already on the list.")
		return
	case errors.Is(err, pgx.ErrNoRows):
		writeError(w, http.StatusNotFound, "not_found", "No such guest.")
		return
	case err != nil:
		serverError(w, r, fmt.Errorf("update guest: %w", err))
		return
	}

	resp := s.buildGuestResp(row.Role, row.Event.Slug, updated.ID, updated.Name, updated.Email, updated.Phone,
		updated.HouseholdSize, updated.TokenVersion, updated.InvitedAt, updated.CreatedAt, nil)
	writeJSON(w, http.StatusOK, map[string]any{"guest": resp})
}

// decodeNullableField reports whether key is present in raw and, if so,
// its normalised value: present=true, value=nil for an explicit JSON null
// (clear the field), present=true, value=&v for a valid string, and
// ok=false for anything else (wrong type or a normalize failure).
func decodeNullableField(raw map[string]json.RawMessage, key string, normalize func(string) (string, bool)) (present bool, value *string, ok bool) {
	v, exists := raw[key]
	if !exists {
		return false, nil, true
	}
	var s *string
	if err := json.Unmarshal(v, &s); err != nil {
		return true, nil, false
	}
	if s == nil {
		return true, nil, true
	}
	norm, ok := normalize(*s)
	if !ok {
		return true, nil, false
	}
	return true, &norm, true
}

// --- DELETE /v1/events/{id}/guests/{guestID} ---

func (s *Server) handleDeleteGuest(w http.ResponseWriter, r *http.Request) {
	eventID, err := parseUUIDParam(r, "id")
	if err != nil {
		writeError(w, http.StatusNotFound, "not_found", "No such guest.")
		return
	}
	guestID, err := parseUUIDParam(r, "guestID")
	if err != nil {
		writeError(w, http.StatusNotFound, "not_found", "No such guest.")
		return
	}
	_, userID, ok := s.requireGuestWriteRole(w, r, eventID)
	if !ok {
		return
	}

	n, err := s.q.DeleteGuest(r.Context(), store.DeleteGuestParams{GuestID: guestID, EventID: eventID, UserID: userID})
	if err != nil {
		serverError(w, r, fmt.Errorf("delete guest: %w", err))
		return
	}
	if n == 0 {
		writeError(w, http.StatusNotFound, "not_found", "No such guest.")
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

// --- POST /v1/events/{id}/guests/{guestID}/rotate-link ---

func (s *Server) handleRotateGuestToken(w http.ResponseWriter, r *http.Request) {
	eventID, err := parseUUIDParam(r, "id")
	if err != nil {
		writeError(w, http.StatusNotFound, "not_found", "No such guest.")
		return
	}
	guestID, err := parseUUIDParam(r, "guestID")
	if err != nil {
		writeError(w, http.StatusNotFound, "not_found", "No such guest.")
		return
	}
	row, userID, ok := s.requireGuestWriteRole(w, r, eventID)
	if !ok {
		return
	}

	updated, err := s.q.RotateGuestToken(r.Context(), store.RotateGuestTokenParams{GuestID: guestID, EventID: eventID, UserID: userID})
	if errors.Is(err, pgx.ErrNoRows) {
		writeError(w, http.StatusNotFound, "not_found", "No such guest.")
		return
	}
	if err != nil {
		serverError(w, r, fmt.Errorf("rotate guest token: %w", err))
		return
	}

	resp := s.buildGuestResp(row.Role, row.Event.Slug, updated.ID, updated.Name, updated.Email, updated.Phone,
		updated.HouseholdSize, updated.TokenVersion, updated.InvitedAt, updated.CreatedAt, nil)
	writeJSON(w, http.StatusOK, map[string]any{"guest": resp})
}

// --- POST /v1/events/{id}/guests/import ---

// csvRowIssue is one entry of the 422 invalid_csv response's details (§4.6):
// distinct from content.Issue, whose Path addresses a content block rather
// than a spreadsheet cell.
type csvRowIssue struct {
	Row   int    `json:"row"`
	Field string `json:"field"`
	Code  string `json:"code"`
}

// csvGuestRow is one validated, ready-to-insert row.
type csvGuestRow struct {
	name, email, phone string
	householdSize      int32
}

// recordIsValidUTF8 reports whether every cell in record is valid UTF-8.
// Postgres rejects invalid UTF-8 bytes at the wire level (SQLSTATE 22021),
// which would otherwise turn a malformed CSV cell into a 500 instead of a
// row-level validation issue.
func recordIsValidUTF8(record []string) bool {
	for _, cell := range record {
		if !utf8.ValidString(cell) {
			return false
		}
	}
	return true
}

func (s *Server) handleImportGuests(w http.ResponseWriter, r *http.Request) {
	eventID, err := parseUUIDParam(r, "id")
	if err != nil {
		writeError(w, http.StatusNotFound, "not_found", "No such event.")
		return
	}
	_, userID, ok := s.requireGuestWriteRole(w, r, eventID)
	if !ok {
		return
	}
	if !s.allow(w, r, limit{"guests:import:" + userID.String(), 20, time.Hour}) {
		return
	}

	mt, _, _ := mime.ParseMediaType(r.Header.Get("Content-Type"))
	if mt != "text/csv" {
		writeError(w, http.StatusUnsupportedMediaType, "unsupported_media_type", "Send a CSV file with Content-Type: text/csv.")
		return
	}

	body := http.MaxBytesReader(w, r.Body, maxGuestCSVBytes)
	reader := csv.NewReader(body)
	reader.FieldsPerRecord = -1 // validated by hand below, so a short row reports a clear per-row error

	header, err := reader.Read()
	if err != nil {
		var tooBig *http.MaxBytesError
		if errors.As(err, &tooBig) {
			writeError(w, http.StatusRequestEntityTooLarge, "body_too_large", "That file is too large.")
			return
		}
		writeErrorDetails(w, http.StatusUnprocessableEntity, "invalid_csv", "Couldn't read that file.",
			[]csvRowIssue{{Row: 1, Field: "", Code: "invalid_header"}})
		return
	}

	// Strip a leading UTF-8 BOM from the first header cell: some spreadsheet
	// tools (notably Excel's "CSV UTF-8" export) prepend U+FEFF, which would
	// otherwise make the first column's name compare unequal to "name".
	if len(header) > 0 {
		header[0] = strings.TrimPrefix(header[0], bomPrefix)
	}
	col := map[string]int{}
	for i, h := range header {
		col[strings.ToLower(strings.TrimSpace(h))] = i
	}
	if _, ok := col["name"]; !ok {
		writeErrorDetails(w, http.StatusUnprocessableEntity, "invalid_csv", "The file needs a \"name\" column.",
			[]csvRowIssue{{Row: 1, Field: "name", Code: "missing_column"}})
		return
	}
	cell := func(record []string, key string) string {
		i, ok := col[key]
		if !ok || i >= len(record) {
			return ""
		}
		return strings.TrimSpace(record[i])
	}

	// capped goes true once issues has reached maxCSVIssues; the loop below
	// checks it before every read so a wide CSV of bad rows stops accumulating
	// (and stops reading) instead of growing the response without bound.
	var issues []csvRowIssue
	capped := false
	addIssue := func(row int, field, code string) {
		if len(issues) < maxCSVIssues {
			issues = append(issues, csvRowIssue{Row: row, Field: field, Code: code})
		}
		if len(issues) >= maxCSVIssues {
			capped = true
		}
	}

	var validRows []csvGuestRow
	rowNum := 1 // header is row 1; the first data row is row 2
	for !capped {
		record, err := reader.Read()
		if err == io.EOF {
			break
		}
		rowNum++
		if err != nil {
			var tooBig *http.MaxBytesError
			if errors.As(err, &tooBig) {
				writeError(w, http.StatusRequestEntityTooLarge, "body_too_large", "That file is too large.")
				return
			}
			// Only a genuine CSV syntax error (unterminated quote, wrong
			// field count, etc.) becomes a per-row issue; any other Read
			// error (a dropped or stalled connection under
			// http.MaxBytesReader, for example) aborts the request
			// immediately instead of looping on it.
			var parseErr *csv.ParseError
			if !errors.As(err, &parseErr) {
				writeError(w, http.StatusBadRequest, "invalid_csv", "Could not read the file.")
				return
			}
			addIssue(rowNum, "", "unreadable_row")
			continue
		}
		if rowNum-1 > maxGuestCSVDataRows {
			writeErrorDetails(w, http.StatusUnprocessableEntity, "invalid_csv", "That file has too many rows (max 1,000).",
				[]csvRowIssue{{Row: rowNum, Field: "", Code: "too_many_rows"}})
			return
		}
		if !recordIsValidUTF8(record) {
			addIssue(rowNum, "", "invalid_encoding")
			continue
		}

		name, ok := validateGuestName(cell(record, "name"))
		if !ok {
			addIssue(rowNum, "name", "invalid")
			continue
		}
		var email string
		if raw := cell(record, "email"); raw != "" {
			e, ok := normalizeEmail(raw)
			if !ok {
				addIssue(rowNum, "email", "invalid")
				continue
			}
			email = e
		}
		var phone string
		if raw := cell(record, "phone"); raw != "" {
			p, ok := normalizePhone(raw)
			if !ok {
				addIssue(rowNum, "phone", "invalid")
				continue
			}
			phone = p
		}
		householdSize := int32(1)
		if raw := cell(record, "household_size"); raw != "" {
			n, err := strconv.Atoi(raw)
			if err != nil || !validateHouseholdSize(int32(n)) {
				addIssue(rowNum, "household_size", "invalid")
				continue
			}
			householdSize = int32(n)
		}
		validRows = append(validRows, csvGuestRow{name: name, email: email, phone: phone, householdSize: householdSize})
	}

	if len(issues) > 0 {
		writeErrorDetails(w, http.StatusUnprocessableEntity, "invalid_csv", "Fix the highlighted rows and try again.", issues)
		return
	}
	if len(validRows) == 0 {
		writeJSON(w, http.StatusOK, map[string]any{"imported": 0, "skipped_existing": 0})
		return
	}

	ids := make([]uuid.UUID, len(validRows))
	names := make([]string, len(validRows))
	emails := make([]string, len(validRows))
	phones := make([]string, len(validRows))
	sizes := make([]int32, len(validRows))
	for i, row := range validRows {
		ids[i] = uuid.Must(uuid.NewV7())
		names[i] = row.name
		emails[i] = row.email
		phones[i] = row.phone
		sizes[i] = row.householdSize
	}

	ctx := r.Context()
	var insertedIDs []uuid.UUID
	err = s.inTx(ctx, func(_ pgx.Tx, q *store.Queries) error {
		if _, err := q.LockEventForEditor(ctx, store.LockEventForEditorParams{EventID: eventID, UserID: userID}); err != nil {
			return err
		}
		count, err := q.CountGuests(ctx, eventID)
		if err != nil {
			return fmt.Errorf("count guests: %w", err)
		}
		if count+int64(len(validRows)) > maxGuestsPerEvent {
			return errGuestLimit
		}
		ids, err := q.InsertGuestsBatch(ctx, store.InsertGuestsBatchParams{
			EventID: eventID, Ids: ids, Names: names, Emails: emails, Phones: phones, HouseholdSizes: sizes, UserID: userID,
		})
		if err != nil {
			return err
		}
		insertedIDs = ids
		return nil
	})
	switch {
	case errors.Is(err, errGuestLimit):
		writeError(w, http.StatusConflict, "guest_limit", "This event has reached its guest limit.")
		return
	case errors.Is(err, pgx.ErrNoRows):
		writeError(w, http.StatusNotFound, "not_found", "No such event.")
		return
	case err != nil:
		serverError(w, r, fmt.Errorf("import guests: %w", err))
		return
	}

	writeJSON(w, http.StatusOK, map[string]any{
		"imported":         len(insertedIDs),
		"skipped_existing": len(validRows) - len(insertedIDs),
	})
}

// --- POST /v1/events/{id}/guests/send-invites ---

func (s *Server) handleSendInvites(w http.ResponseWriter, r *http.Request) {
	eventID, err := parseUUIDParam(r, "id")
	if err != nil {
		writeError(w, http.StatusNotFound, "not_found", "No such event.")
		return
	}
	row, userID, ok := s.requireGuestWriteRole(w, r, eventID)
	if !ok {
		return
	}
	if row.Event.Status != "published" {
		writeError(w, http.StatusConflict, "event_not_published", "Publish this event before sending invites.")
		return
	}

	var body struct {
		GuestIDs     []uuid.UUID `json:"guest_ids"`
		AllUninvited bool        `json:"all_uninvited"`
	}
	if !decodeJSON(w, r, &body) {
		return
	}
	if len(body.GuestIDs) > maxSendInviteIDs {
		writeError(w, http.StatusBadRequest, "validation_failed", "Send to at most 500 guests per request.")
		return
	}
	if (len(body.GuestIDs) == 0) == !body.AllUninvited {
		writeError(w, http.StatusBadRequest, "validation_failed", "Provide either guest_ids or all_uninvited.")
		return
	}

	ctx := r.Context()
	user, err := s.q.GetUserByID(ctx, userID)
	if err != nil {
		serverError(w, r, fmt.Errorf("get user: %w", err))
		return
	}
	userCap := int64(500)
	if time.Since(user.CreatedAt) < 7*24*time.Hour {
		userCap = 100
	}

	// The three daily caps (per user, per event, global) are checked against
	// the *actual* number of guests MarkGuestsInvited stamps, not a guess
	// made before it runs: a request's guest_ids/all_uninvited can include
	// guests skipped by the 24h resend cooldown, and charging for those
	// would reject legitimate small requests (especially against the
	// 100/day new-account cap) for no reason. Checking after the count is
	// known, inside the same transaction as the stamp and the job inserts,
	// keeps the caps exact for what's actually sent while still rolling
	// everything back together if a cap is hit.
	capChecks := []struct {
		key    string
		max    int64
		window time.Duration
	}{
		{"invites:send:user:" + userID.String(), userCap, 24 * time.Hour},
		{"invites:send:event:" + eventID.String(), maxGuestsPerEvent, 24 * time.Hour},
		// Keyed per UTC calendar day (not a rolling window from first use),
		// so the cap actually matches "daily" and self-expires with the TTL.
		{"invites:send:global:" + time.Now().UTC().Format("2006-01-02"), int64(s.cfg.MailDailyInviteCap), 48 * time.Hour},
	}

	var invitedIDs []uuid.UUID
	err = s.inTx(ctx, func(tx pgx.Tx, q *store.Queries) error {
		ids, err := q.MarkGuestsInvited(ctx, store.MarkGuestsInvitedParams{
			EventID: eventID, GuestIds: body.GuestIDs, AllUninvited: body.AllUninvited, UserID: userID,
		})
		if err != nil {
			return fmt.Errorf("mark guests invited: %w", err)
		}
		n := int64(len(ids))
		if n == 0 {
			invitedIDs = ids
			return nil
		}

		var applied int
		for i, c := range capChecks {
			ok, err := s.limiter.AllowN(ctx, c.key, n, c.max, c.window)
			if err != nil {
				uncountApplied(ctx, s.limiter, capChecks[:applied], n)
				return fmt.Errorf("rate limit: %w", err)
			}
			if !ok {
				uncountApplied(ctx, s.limiter, capChecks[:applied], n)
				return errDailyLimit
			}
			applied = i + 1
		}

		for _, id := range ids {
			if _, err := s.jobs.InsertTx(ctx, tx, jobs.SendInviteArgs{GuestID: id}, nil); err != nil {
				uncountApplied(ctx, s.limiter, capChecks[:applied], n)
				return fmt.Errorf("enqueue send_invite: %w", err)
			}
		}
		invitedIDs = ids
		return nil
	})
	if errors.Is(err, errDailyLimit) {
		writeError(w, http.StatusTooManyRequests, "daily_limit", "Daily invite limit reached. Try again tomorrow.")
		return
	}
	if err != nil {
		serverError(w, r, err)
		return
	}
	writeJSON(w, http.StatusAccepted, map[string]any{"queued": len(invitedIDs)})
}

// uncountApplied rolls back the checks that already succeeded in this batch
// before a later one failed, so a rejected send-invites call never leaves
// the caps permanently overcharged for quota it never actually used.
func uncountApplied(ctx context.Context, l *ratelimit.Limiter, applied []struct {
	key    string
	max    int64
	window time.Duration
}, n int64) {
	for _, c := range applied {
		l.Uncount(ctx, c.key, n)
	}
}

// --- shared helpers ---

// normalizeOptionalEmail treats a nil or blank pointer as "not provided".
func normalizeOptionalEmail(p *string) (*string, bool) {
	if p == nil || strings.TrimSpace(*p) == "" {
		return nil, true
	}
	e, ok := normalizeEmail(*p)
	if !ok {
		return nil, false
	}
	return &e, true
}

// normalizeOptionalPhone treats a nil or blank pointer as "not provided".
func normalizeOptionalPhone(p *string) (*string, bool) {
	if p == nil || strings.TrimSpace(*p) == "" {
		return nil, true
	}
	ph, ok := normalizePhone(*p)
	if !ok {
		return nil, false
	}
	return &ph, true
}

// isUniqueViolation reports whether err is a 23505 unique-violation on the
// named constraint.
func isUniqueViolation(err error, constraint string) bool {
	var pgErr *pgconn.PgError
	return errors.As(err, &pgErr) && pgErr.Code == "23505" && pgErr.ConstraintName == constraint
}
