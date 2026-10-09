package httpapi

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"strconv"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/go-chi/chi/v5"
	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"

	"github.com/ansonarose/seeyouthere-backend/internal/content"
	"github.com/ansonarose/seeyouthere-backend/internal/jobs"
	"github.com/ansonarose/seeyouthere-backend/internal/store"
)

// untitledEventTitle is content.ValidateContent's fallback title when no
// hero title or heading is set. Publish readiness treats it as "no title".
const untitledEventTitle = "Untitled event"

const (
	maxLiveEventsPerOwner = 100
	maxDraftsPerCookie    = 5
	anonDraftTTL          = 30 * 24 * time.Hour
	maxPatchBodyBytes     = 256 << 10
)

var validVisibilities = map[string]bool{"unlisted": true, "password": true, "invite_only": true, "public": true}
var validRsvpModes = map[string]bool{"open": true, "invite_only": true}

var (
	errTooManyEvents   = errors.New("too many live events")
	errTooManyDrafts   = errors.New("too many anonymous drafts")
	errUnknownTemplate = errors.New("unknown template")
	errEventNotFound   = errors.New("event not found")
	errEventForbidden  = errors.New("event forbidden")
)

// versionConflictError is returned by mapEventWriteFailure when a write's
// role and status checks pass but its version didn't match the row's.
type versionConflictError struct{ current int32 }

func (e *versionConflictError) Error() string { return "version conflict" }

// takenDownError is returned by mapEventWriteFailure for a taken-down event.
type takenDownError struct{}

func (e *takenDownError) Error() string { return "event taken down" }

// writeEventWriteError renders the outcome of mapEventWriteFailure (or an
// equivalent sentinel from a check made before attempting the write).
func writeEventWriteError(w http.ResponseWriter, r *http.Request, err error) {
	var vc *versionConflictError
	var td *takenDownError
	switch {
	case errors.Is(err, errEventNotFound):
		writeError(w, http.StatusNotFound, "not_found", "No such event.")
	case errors.Is(err, errEventForbidden):
		writeError(w, http.StatusForbidden, "forbidden", "You don't have access to do this.")
	case errors.As(err, &td):
		writeError(w, http.StatusConflict, "event_taken_down", "This event has been taken down.")
	case errors.As(err, &vc):
		writeErrorDetails(w, http.StatusConflict, "version_conflict",
			"This event changed since you loaded it.", map[string]int32{"current_version": vc.current})
	default:
		serverError(w, r, err)
	}
}

// mapEventWriteFailure runs after a zero-row event write to decide why:
// no role at all -> not found, a role outside allowedRoles -> forbidden,
// taken down -> conflict, otherwise the version the caller sent is stale.
// userID nil means the caller is an anonymous draft cookie, in which case
// allowedRoles is ignored (an anon draft has no roles to check).
func (s *Server) mapEventWriteFailure(ctx context.Context, q *store.Queries, userID *uuid.UUID, eventID uuid.UUID, cookieHash []byte, allowedRoles map[string]bool) error {
	var status string
	var version int32
	if userID != nil {
		st, err := q.GetEventWriteState(ctx, store.GetEventWriteStateParams{UserID: *userID, EventID: eventID})
		if errors.Is(err, pgx.ErrNoRows) {
			return errEventNotFound
		}
		if err != nil {
			return fmt.Errorf("get event write state: %w", err)
		}
		if !allowedRoles[st.Role] {
			return errEventForbidden
		}
		status, version = st.Status, st.Version
	} else {
		st, err := q.GetAnonDraftWriteState(ctx, store.GetAnonDraftWriteStateParams{EventID: eventID, CookieHash: cookieHash})
		if errors.Is(err, pgx.ErrNoRows) {
			return errEventNotFound
		}
		if err != nil {
			return fmt.Errorf("get anon draft write state: %w", err)
		}
		status, version = st.Status, st.Version
	}
	if status == "taken_down" {
		return &takenDownError{}
	}
	return &versionConflictError{current: version}
}

// eventRow is the common shape of GetEventForUserRow and GetAnonDraftEventRow.
type eventRow struct {
	Event         store.Event
	Role          string
	TemplateSlug  string
	TemplateName  string
	Manifest      []byte
	AssetsPath    string
	LatestVersion int32
}

func fromGetEventForUser(r store.GetEventForUserRow) eventRow {
	return eventRow{r.Event, r.Role, r.TemplateSlug, r.TemplateName, r.Manifest, r.AssetsPath, r.LatestVersion}
}

func fromGetAnonDraftEvent(r store.GetAnonDraftEventRow) eventRow {
	return eventRow{r.Event, r.Role, r.TemplateSlug, r.TemplateName, r.Manifest, r.AssetsPath, r.LatestVersion}
}

// loadEventRow resolves read/write access to an event for either a signed-in
// user (any member) or an anonymous draft cookie (§4.3's "S/D" auth column).
func (s *Server) loadEventRow(r *http.Request, eventID uuid.UUID) (eventRow, bool, error) {
	ctx := r.Context()
	if userID, ok := userIDFrom(ctx); ok {
		row, err := s.q.GetEventForUser(ctx, store.GetEventForUserParams{UserID: userID, EventID: eventID})
		if errors.Is(err, pgx.ErrNoRows) {
			return eventRow{}, false, nil
		}
		if err != nil {
			return eventRow{}, false, fmt.Errorf("get event: %w", err)
		}
		return fromGetEventForUser(row), true, nil
	}
	hash, ok := draftCookieHash(r)
	if !ok {
		return eventRow{}, false, nil
	}
	row, err := s.q.GetAnonDraftEvent(ctx, store.GetAnonDraftEventParams{EventID: eventID, CookieHash: hash})
	if errors.Is(err, pgx.ErrNoRows) {
		return eventRow{}, false, nil
	}
	if err != nil {
		return eventRow{}, false, fmt.Errorf("get anon draft: %w", err)
	}
	return fromGetAnonDraftEvent(row), true, nil
}

// loadMemberEvent is loadEventRow for the session-only endpoints (settings,
// publish, unpublish): no anonymous-draft branch.
func (s *Server) loadMemberEvent(ctx context.Context, userID, eventID uuid.UUID) (eventRow, bool, error) {
	row, err := s.q.GetEventForUser(ctx, store.GetEventForUserParams{UserID: userID, EventID: eventID})
	if errors.Is(err, pgx.ErrNoRows) {
		return eventRow{}, false, nil
	}
	if err != nil {
		return eventRow{}, false, fmt.Errorf("get event: %w", err)
	}
	return fromGetEventForUser(row), true, nil
}

// loadOccasion fetches and parses one occasion. Used for events, whose
// occasion_slug is fixed at creation, so any failure here is a server-side
// inconsistency (a deactivated or corrupted catalog row), not a bad request.
func (s *Server) loadOccasion(ctx context.Context, q *store.Queries, slug string) (content.Occasion, error) {
	row, err := q.GetOccasionForEvent(ctx, slug)
	if err != nil {
		return content.Occasion{}, fmt.Errorf("get occasion %s: %w", slug, err)
	}
	occ, err := content.ParseOccasion(row)
	if err != nil {
		return content.Occasion{}, fmt.Errorf("parse occasion %s: %w", slug, err)
	}
	return occ, nil
}

// --- response shapes (§4.1) ---

type templateResp struct {
	ID            uuid.UUID `json:"id"`
	Slug          string    `json:"slug"`
	Name          string    `json:"name"`
	Version       int32     `json:"version"`
	LatestVersion int32     `json:"latest_version"`
}

type mediaItemResp struct {
	Src    string `json:"src"`
	Width  int32  `json:"width"`
	Height int32  `json:"height"`
}

type eventResp struct {
	ID             uuid.UUID  `json:"id"`
	Slug           *string    `json:"slug"`
	Title          string     `json:"title"`
	OccasionSlug   string     `json:"occasion_slug"`
	Status         string     `json:"status"`
	Visibility     string     `json:"visibility"`
	HasPassword    bool       `json:"has_password"`
	RsvpMode       string     `json:"rsvp_mode"`
	RemoveBranding bool       `json:"remove_branding"`
	NotifyRsvps    bool       `json:"notify_rsvps"`
	StartsAt       *time.Time `json:"starts_at"`
	// EndsAt is derived from the content's datetime block end_local (nil if
	// absent or the block is invalid), so the frontend's add-to-calendar
	// needs no timezone maths.
	EndsAt      *time.Time               `json:"ends_at"`
	PublishedAt *time.Time               `json:"published_at"`
	CreatedAt   time.Time                `json:"created_at"`
	UpdatedAt   time.Time                `json:"updated_at"`
	Version     int32                    `json:"version"`
	Role        string                   `json:"role"`
	Template    templateResp             `json:"template"`
	Content     json.RawMessage          `json:"content"`
	Overrides   content.Overrides        `json:"overrides"`
	Theme       content.Theme            `json:"theme"`
	Media       map[string]mediaItemResp `json:"media"`
	URL         *string                  `json:"url"`
}

// buildEventResponse assembles the host view of an event: it re-derives the
// media map and resolved theme from currently-stored, already-validated
// data, so it never fails except on a server-side inconsistency.
func (s *Server) buildEventResponse(ctx context.Context, q *store.Queries, row eventRow) (eventResp, error) {
	e := row.Event

	occ, err := s.loadOccasion(ctx, q, e.OccasionSlug)
	if err != nil {
		return eventResp{}, err
	}
	saved, err := content.ParseStored(e.Content, occ)
	if err != nil {
		return eventResp{}, fmt.Errorf("parse stored content for event %s: %w", e.ID, err)
	}

	manifest, err := content.ValidateStoredManifest(row.Manifest)
	if err != nil {
		return eventResp{}, fmt.Errorf("parse manifest for event %s: %w", e.ID, err)
	}
	backgroundSrc := ""
	if manifest.UsesBackgroundAsset() && row.AssetsPath != "" {
		backgroundSrc = "/media/" + row.AssetsPath + "/background"
	}
	theme := content.ResolveTheme(manifest, e.Overrides, backgroundSrc)

	media := map[string]mediaItemResp{}
	if len(saved.MediaIDs) > 0 {
		rows, err := q.ApprovedMediaByIDs(ctx, store.ApprovedMediaByIDsParams{EventID: e.ID, MediaIds: uniqueUUIDs(saved.MediaIDs)})
		if err != nil {
			return eventResp{}, fmt.Errorf("load media for event %s: %w", e.ID, err)
		}
		for _, m := range rows {
			media[m.ID.String()] = mediaItemResp{
				Src:    fmt.Sprintf("/media/%s/%s", e.ID, m.ID),
				Width:  m.Width,
				Height: m.Height,
			}
		}
	}

	var ov content.Overrides
	if len(e.Overrides) > 0 {
		_ = json.Unmarshal(e.Overrides, &ov) // stored data was validated at save time
	}

	var url *string
	if e.Slug != nil {
		u := s.cfg.SiteURL + "/" + *e.Slug
		url = &u
	}

	return eventResp{
		ID: e.ID, Slug: e.Slug, Title: e.Title, OccasionSlug: e.OccasionSlug, Status: e.Status,
		Visibility: e.Visibility, HasPassword: e.PasswordHash != nil, RsvpMode: e.RsvpMode,
		RemoveBranding: e.RemoveBranding, NotifyRsvps: e.NotifyRsvps, StartsAt: e.StartsAt,
		EndsAt:      saved.EndsAt,
		PublishedAt: e.PublishedAt, CreatedAt: e.CreatedAt, UpdatedAt: e.UpdatedAt, Version: e.Version,
		Role: row.Role,
		Template: templateResp{
			ID: e.TemplateID, Slug: row.TemplateSlug, Name: row.TemplateName,
			Version: e.TemplateVersion, LatestVersion: row.LatestVersion,
		},
		Content: saved.JSON, Overrides: ov, Theme: theme, Media: media, URL: url,
	}, nil
}

type rsvpCountsResp struct {
	Yes      int64 `json:"yes"`
	No       int64 `json:"no"`
	Maybe    int64 `json:"maybe"`
	YesHeads int64 `json:"yes_heads"`
}

type eventSummaryResp struct {
	ID           uuid.UUID      `json:"id"`
	Slug         *string        `json:"slug"`
	Title        string         `json:"title"`
	OccasionSlug string         `json:"occasion_slug"`
	Status       string         `json:"status"`
	StartsAt     *time.Time     `json:"starts_at"`
	Role         string         `json:"role"`
	CreatedAt    time.Time      `json:"created_at"`
	RSVP         rsvpCountsResp `json:"rsvp"`
}

// --- helpers ---

func parseUUIDParam(r *http.Request, name string) (uuid.UUID, error) {
	return uuid.Parse(chi.URLParam(r, name))
}

// clampLimit parses the ?limit= query param, falling back to def on any
// parse failure and clamping to [1, max].
func clampLimit(raw string, def, max int) int {
	if raw == "" {
		return def
	}
	n, err := strconv.Atoi(raw)
	if err != nil || n < 1 {
		return def
	}
	if n > max {
		return max
	}
	return n
}

func uniqueUUIDs(ids []uuid.UUID) []uuid.UUID {
	seen := make(map[uuid.UUID]bool, len(ids))
	out := make([]uuid.UUID, 0, len(ids))
	for _, id := range ids {
		if !seen[id] {
			seen[id] = true
			out = append(out, id)
		}
	}
	return out
}

// writeValidationIssues renders a content.ValidationError (or reports err as
// unexpected if it isn't one, since every content validator only ever
// returns *content.ValidationError on a bad-input path).
func writeValidationIssues(w http.ResponseWriter, r *http.Request, err error, message string) {
	var ve *content.ValidationError
	if errors.As(err, &ve) {
		writeErrorDetails(w, http.StatusBadRequest, "validation_failed", message, ve.Issues)
		return
	}
	serverError(w, r, err)
}

// --- POST /v1/events ---

// handleCreateEvent creates a draft, either owned (signed-in) or anonymous
// (§4.3, decision 4 in the build-out plan: anonymous drafts are claimed at
// login, not published directly).
func (s *Server) handleCreateEvent(w http.ResponseWriter, r *http.Request) {
	var body struct {
		OccasionSlug string            `json:"occasion_slug"`
		TemplateSlug string            `json:"template_slug"`
		Answers      map[string]string `json:"answers"`
	}
	if !decodeJSON(w, r, &body) {
		return
	}

	ctx := r.Context()
	userID, authed := userIDFrom(ctx)
	if authed {
		if !s.allow(w, r, limit{"events:create:user:" + userID.String(), 30, time.Hour}) {
			return
		}
	} else {
		ip := ipRateKey(clientIPFrom(ctx))
		if !s.allow(w, r,
			limit{"events:create:anon:" + ip, 10, time.Hour},
			limit{"events:create:anon:global", 300, time.Hour},
		) {
			return
		}
	}

	occasionSlug := strings.TrimSpace(body.OccasionSlug)
	occRow, err := s.q.GetOccasion(ctx, occasionSlug)
	if errors.Is(err, pgx.ErrNoRows) {
		writeError(w, http.StatusBadRequest, "unknown_occasion", "Unknown occasion.")
		return
	}
	if err != nil {
		serverError(w, r, fmt.Errorf("get occasion: %w", err))
		return
	}
	occ, err := content.ParseOccasion(occRow)
	if err != nil {
		serverError(w, r, fmt.Errorf("parse occasion %s: %w", occasionSlug, err))
		return
	}

	templateSlug := strings.TrimSpace(body.TemplateSlug)
	if templateSlug == "" {
		templateSlug = "classic" // seeded, tagged for every occasion (§2.2)
	}
	tmpl, err := s.q.GetPublishedTemplateBySlug(ctx, store.GetPublishedTemplateBySlugParams{Slug: templateSlug, AllowPremium: false})
	if errors.Is(err, pgx.ErrNoRows) {
		writeError(w, http.StatusBadRequest, "unknown_template", "Unknown template.")
		return
	}
	if err != nil {
		serverError(w, r, fmt.Errorf("get template: %w", err))
		return
	}

	built, err := content.BuildInitialContent(occ, body.Answers)
	if err != nil {
		writeValidationIssues(w, r, err, "Check the highlighted answers.")
		return
	}
	saved, err := content.ParseStored(built, occ)
	if err != nil {
		serverError(w, r, fmt.Errorf("parse built content: %w", err))
		return
	}

	createParams := store.CreateEventParams{
		ID: uuid.Must(uuid.NewV7()), OccasionSlug: occasionSlug, Title: saved.Title,
		Content: saved.JSON, Overrides: []byte("{}"), StartsAt: saved.StartsAt, EndsAt: saved.EffectiveEnd(),
		TemplateID: tmpl.ID, TemplateVersion: tmpl.Version,
	}

	var draftValue string
	var createdEvent store.Event
	role := "owner"

	err = s.inTx(ctx, func(tx pgx.Tx, q *store.Queries) error {
		if authed {
			n, err := q.CountLiveEventsByOwner(ctx, userID)
			if err != nil {
				return fmt.Errorf("count live events: %w", err)
			}
			if n >= maxLiveEventsPerOwner {
				return errTooManyEvents
			}
			owner := userID
			createParams.OwnerID = &owner
		} else {
			role = "anon"
			hash, hasCookie := draftCookieHash(r)
			reuseCookie := false
			if hasCookie {
				n, err := q.CountAnonDraftsByCookie(ctx, hash)
				if err != nil {
					return fmt.Errorf("count anon drafts: %w", err)
				}
				if n >= maxDraftsPerCookie {
					return errTooManyDrafts
				}
				// Only reuse a presented cookie if it already owns at least
				// one live draft; otherwise it's an attacker-fixed or stale
				// value, and reusing it would let someone else's requests
				// land against the victim's (or a fresh, attacker-known)
				// cookie hash instead of minting the caller a real identity.
				reuseCookie = n >= 1
			}
			if !reuseCookie {
				token, err := randomToken()
				if err != nil {
					return fmt.Errorf("generate draft token: %w", err)
				}
				draftValue = token
				sum := sha256.Sum256([]byte(token))
				hash = sum[:]
			}
			event, err := q.CreateEvent(ctx, createParams)
			if errors.Is(err, pgx.ErrNoRows) {
				return errUnknownTemplate
			}
			if err != nil {
				return fmt.Errorf("create event: %w", err)
			}
			if err := q.CreateAnonDraft(ctx, store.CreateAnonDraftParams{
				ID: uuid.Must(uuid.NewV7()), CookieHash: hash, EventID: event.ID, ExpiresAt: time.Now().Add(anonDraftTTL),
			}); err != nil {
				return fmt.Errorf("create anon draft: %w", err)
			}
			createdEvent = event
			return nil
		}
		event, err := q.CreateEvent(ctx, createParams)
		if errors.Is(err, pgx.ErrNoRows) {
			return errUnknownTemplate
		}
		if err != nil {
			return fmt.Errorf("create event: %w", err)
		}
		createdEvent = event
		return nil
	})
	switch {
	case errors.Is(err, errTooManyEvents):
		writeError(w, http.StatusConflict, "too_many_events", "You've reached the limit of live events.")
		return
	case errors.Is(err, errTooManyDrafts):
		writeError(w, http.StatusConflict, "too_many_drafts", "Too many drafts from this browser. Sign in to keep going.")
		return
	case errors.Is(err, errUnknownTemplate):
		writeError(w, http.StatusBadRequest, "unknown_template", "Unknown template.")
		return
	case err != nil:
		serverError(w, r, err)
		return
	}

	if !authed && draftValue != "" {
		s.setDraftCookie(w, draftValue)
	}

	resp, err := s.buildEventResponse(ctx, s.q, eventRow{
		Event: createdEvent, Role: role, TemplateSlug: tmpl.Slug, TemplateName: tmpl.Name,
		Manifest: tmpl.Manifest, AssetsPath: tmpl.AssetsPath, LatestVersion: tmpl.Version,
	})
	if err != nil {
		serverError(w, r, err)
		return
	}
	writeJSON(w, http.StatusCreated, map[string]any{"event": resp})
}

// --- GET /v1/events ---

func (s *Server) handleListEvents(w http.ResponseWriter, r *http.Request) {
	userID, ok := userIDFrom(r.Context())
	if !ok {
		writeError(w, http.StatusUnauthorized, "unauthorized", "Log in to continue.")
		return
	}

	limit := clampLimit(r.URL.Query().Get("limit"), 25, 100)
	cursorCreatedAt := time.Date(9999, 12, 31, 0, 0, 0, 0, time.UTC)
	cursorID := uuid.Max
	if c := r.URL.Query().Get("cursor"); c != "" {
		t, id, err := decodeCursor(c)
		if err != nil {
			writeError(w, http.StatusBadRequest, "invalid_cursor", "Invalid cursor.")
			return
		}
		cursorCreatedAt, cursorID = t, id
	}

	ctx := r.Context()
	rows, err := s.q.ListEventsForUser(ctx, store.ListEventsForUserParams{
		UserID: userID, CursorCreatedAt: cursorCreatedAt, CursorID: cursorID, Lim: int32(limit),
	})
	if err != nil {
		serverError(w, r, fmt.Errorf("list events: %w", err))
		return
	}

	counts := map[uuid.UUID]store.RSVPCountsForEventsRow{}
	if len(rows) > 0 {
		ids := make([]uuid.UUID, len(rows))
		for i, row := range rows {
			ids[i] = row.ID
		}
		crows, err := s.q.RSVPCountsForEvents(ctx, store.RSVPCountsForEventsParams{EventIds: ids, UserID: userID})
		if err != nil {
			serverError(w, r, fmt.Errorf("rsvp counts: %w", err))
			return
		}
		for _, c := range crows {
			counts[c.EventID] = c
		}
	}

	summaries := make([]eventSummaryResp, len(rows))
	for i, row := range rows {
		c := counts[row.ID]
		summaries[i] = eventSummaryResp{
			ID: row.ID, Slug: row.Slug, Title: row.Title, OccasionSlug: row.OccasionSlug,
			Status: row.Status, StartsAt: row.StartsAt, Role: row.Role, CreatedAt: row.CreatedAt,
			RSVP: rsvpCountsResp{Yes: c.Yes, No: c.No, Maybe: c.Maybe, YesHeads: c.YesHeads},
		}
	}
	var nextCursor *string
	if len(rows) == limit {
		nc := encodeCursor(rows[len(rows)-1].CreatedAt, rows[len(rows)-1].ID)
		nextCursor = &nc
	}
	writeJSON(w, http.StatusOK, map[string]any{"events": summaries, "next_cursor": nextCursor})
}

// --- GET /v1/events/{id} ---

func (s *Server) handleGetEvent(w http.ResponseWriter, r *http.Request) {
	eventID, err := parseUUIDParam(r, "id")
	if err != nil {
		writeError(w, http.StatusNotFound, "not_found", "No such event.")
		return
	}
	row, ok, err := s.loadEventRow(r, eventID)
	if err != nil {
		serverError(w, r, err)
		return
	}
	if !ok {
		writeError(w, http.StatusNotFound, "not_found", "No such event.")
		return
	}
	resp, err := s.buildEventResponse(r.Context(), s.q, row)
	if err != nil {
		serverError(w, r, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"event": resp})
}

// --- PATCH /v1/events/{id} ---

func (s *Server) handlePatchEvent(w http.ResponseWriter, r *http.Request) {
	eventID, err := parseUUIDParam(r, "id")
	if err != nil {
		writeError(w, http.StatusNotFound, "not_found", "No such event.")
		return
	}

	ctx := r.Context()
	userID, authed := userIDFrom(ctx)
	var cookieHash []byte
	if !authed {
		hash, ok := draftCookieHash(r)
		if !ok {
			writeError(w, http.StatusNotFound, "not_found", "No such event.")
			return
		}
		cookieHash = hash
	}

	// Rate-limit before decoding the body: an anonymous caller controls the
	// syt_draft cookie value, so limiting on that alone lets an attacker
	// mint a fresh cookie per request to dodge the limit and force the
	// server to decode up to maxPatchBodyBytes each time. A per-IP limit
	// alongside it, checked first, bounds that regardless of cookie churn.
	if authed {
		if !s.allow(w, r, limit{"events:patch:user:" + userID.String(), 1200, time.Hour}) {
			return
		}
	} else {
		ip := ipRateKey(clientIPFrom(ctx))
		if !s.allow(w, r,
			limit{"events:patch:ip:" + ip, 1200, time.Hour},
			limit{"events:patch:draft:" + hex.EncodeToString(cookieHash), 1200, time.Hour},
		) {
			return
		}
	}

	var body struct {
		Version      int32           `json:"version"`
		Content      json.RawMessage `json:"content"`
		Overrides    json.RawMessage `json:"overrides"`
		TemplateSlug *string         `json:"template_slug"`
	}
	if !decodeJSONMax(w, r, &body, maxPatchBodyBytes) {
		return
	}
	if body.Version < 1 {
		writeErrorDetails(w, http.StatusBadRequest, "validation_failed", "version is required.",
			[]content.Issue{{Path: "version", Code: "required", Message: "This field is required."}})
		return
	}

	row, ok, err := s.loadEventRow(r, eventID)
	if err != nil {
		serverError(w, r, err)
		return
	}
	if !ok {
		writeError(w, http.StatusNotFound, "not_found", "No such event.")
		return
	}
	if authed && row.Role == "viewer" {
		writeError(w, http.StatusForbidden, "forbidden", "You don't have access to edit this event.")
		return
	}
	if row.Event.Status == "taken_down" {
		writeError(w, http.StatusConflict, "event_taken_down", "This event has been taken down.")
		return
	}

	var templateID *uuid.UUID
	var templateVersion *int32
	manifest := row.Manifest
	if body.TemplateSlug != nil {
		slug := strings.TrimSpace(*body.TemplateSlug)
		if slug == "" {
			writeError(w, http.StatusBadRequest, "unknown_template", "Unknown template.")
			return
		}
		tmpl, err := s.q.GetPublishedTemplateBySlug(ctx, store.GetPublishedTemplateBySlugParams{Slug: slug, AllowPremium: false})
		if errors.Is(err, pgx.ErrNoRows) {
			writeError(w, http.StatusBadRequest, "unknown_template", "Unknown template.")
			return
		}
		if err != nil {
			serverError(w, r, fmt.Errorf("get template: %w", err))
			return
		}
		templateID, templateVersion = &tmpl.ID, &tmpl.Version
		manifest = tmpl.Manifest
	}

	var contentJSON []byte
	var title string
	var startsAt, endsAt *time.Time
	var mediaIDs []uuid.UUID
	contentChanged := len(body.Content) > 0
	if contentChanged {
		occ, err := s.loadOccasion(ctx, s.q, row.Event.OccasionSlug)
		if err != nil {
			serverError(w, r, err)
			return
		}
		saved, err := content.ValidateContent(body.Content, occ)
		if err != nil {
			writeValidationIssues(w, r, err, "Check the highlighted fields.")
			return
		}
		if len(saved.MediaIDs) > 0 {
			unique := uniqueUUIDs(saved.MediaIDs)
			approved, err := s.q.ApprovedMediaByIDs(ctx, store.ApprovedMediaByIDsParams{EventID: eventID, MediaIds: unique})
			if err != nil {
				serverError(w, r, fmt.Errorf("check approved media: %w", err))
				return
			}
			if len(approved) != len(unique) {
				writeErrorDetails(w, http.StatusBadRequest, "validation_failed", "One or more images aren't available.",
					[]content.Issue{{Path: "content", Code: "invalid_media_id", Message: "Not a valid media reference."}})
				return
			}
		}
		contentJSON, title, startsAt, endsAt, mediaIDs = saved.JSON, saved.Title, saved.StartsAt, saved.EffectiveEnd(), saved.MediaIDs
	}

	var overridesJSON []byte
	if len(body.Overrides) > 0 {
		m, err := content.ValidateStoredManifest(manifest)
		if err != nil {
			serverError(w, r, fmt.Errorf("parse manifest: %w", err))
			return
		}
		canon, err := content.ValidateOverrides(body.Overrides, m)
		if err != nil {
			writeValidationIssues(w, r, err, "Check the theme options.")
			return
		}
		overridesJSON = canon
	}

	err = s.inTx(ctx, func(tx pgx.Tx, q *store.Queries) error {
		if authed {
			_, err := q.UpdateEventContent(ctx, store.UpdateEventContentParams{
				Content: contentJSON, Title: title, StartsAt: startsAt, EndsAt: endsAt, Overrides: overridesJSON,
				TemplateID: templateID, TemplateVersion: templateVersion,
				EventID: eventID, Version: body.Version, UserID: userID,
			})
			if err != nil {
				return err
			}
		} else {
			_, err := q.UpdateAnonDraftContent(ctx, store.UpdateAnonDraftContentParams{
				Content: contentJSON, Title: title, StartsAt: startsAt, EndsAt: endsAt, Overrides: overridesJSON,
				TemplateID: templateID, TemplateVersion: templateVersion,
				EventID: eventID, Version: body.Version, CookieHash: cookieHash,
			})
			if err != nil {
				return err
			}
		}
		if contentChanged {
			if err := q.SyncMediaDetached(ctx, store.SyncMediaDetachedParams{EventID: eventID, Referenced: mediaIDs}); err != nil {
				return fmt.Errorf("sync media detached: %w", err)
			}
		}
		return nil
	})
	if errors.Is(err, pgx.ErrNoRows) {
		var wErr error
		if authed {
			wErr = s.mapEventWriteFailure(ctx, s.q, &userID, eventID, nil, map[string]bool{"owner": true, "editor": true})
		} else {
			wErr = s.mapEventWriteFailure(ctx, s.q, nil, eventID, cookieHash, nil)
		}
		writeEventWriteError(w, r, wErr)
		return
	}
	if err != nil {
		serverError(w, r, err)
		return
	}

	updated, ok, err := s.loadEventRow(r, eventID)
	if err != nil {
		serverError(w, r, err)
		return
	}
	if !ok {
		serverError(w, r, fmt.Errorf("event %s vanished after patch", eventID))
		return
	}
	resp, err := s.buildEventResponse(ctx, s.q, updated)
	if err != nil {
		serverError(w, r, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"event": resp})
}

// --- POST /v1/events/{id}/template/upgrade ---

func (s *Server) handleUpgradeEventTemplate(w http.ResponseWriter, r *http.Request) {
	eventID, err := parseUUIDParam(r, "id")
	if err != nil {
		writeError(w, http.StatusNotFound, "not_found", "No such event.")
		return
	}
	ctx := r.Context()
	userID, authed := userIDFrom(ctx)
	var cookieHash []byte
	if !authed {
		hash, ok := draftCookieHash(r)
		if !ok {
			writeError(w, http.StatusNotFound, "not_found", "No such event.")
			return
		}
		cookieHash = hash
	}

	// Same rate-limit shape as PATCH, and checked before decoding the body
	// for the same reason: an anonymous caller controls the draft cookie.
	if authed {
		if !s.allow(w, r, limit{"events:upgrade:user:" + userID.String(), 1200, time.Hour}) {
			return
		}
	} else {
		ip := ipRateKey(clientIPFrom(ctx))
		if !s.allow(w, r,
			limit{"events:upgrade:ip:" + ip, 1200, time.Hour},
			limit{"events:upgrade:draft:" + hex.EncodeToString(cookieHash), 1200, time.Hour},
		) {
			return
		}
	}

	var body struct {
		Version int32 `json:"version"`
	}
	if !decodeJSON(w, r, &body) {
		return
	}
	if body.Version < 1 {
		writeErrorDetails(w, http.StatusBadRequest, "validation_failed", "version is required.",
			[]content.Issue{{Path: "version", Code: "required", Message: "This field is required."}})
		return
	}

	row, ok, err := s.loadEventRow(r, eventID)
	if err != nil {
		serverError(w, r, err)
		return
	}
	if !ok {
		writeError(w, http.StatusNotFound, "not_found", "No such event.")
		return
	}
	if authed && row.Role == "viewer" {
		writeError(w, http.StatusForbidden, "forbidden", "You don't have access to edit this event.")
		return
	}
	if row.Event.Status == "taken_down" {
		writeError(w, http.StatusConflict, "event_taken_down", "This event has been taken down.")
		return
	}

	templateID := row.Event.TemplateID
	latest := row.LatestVersion

	err = s.inTx(ctx, func(tx pgx.Tx, q *store.Queries) error {
		if authed {
			_, err := q.UpdateEventContent(ctx, store.UpdateEventContentParams{
				TemplateID: &templateID, TemplateVersion: &latest,
				EventID: eventID, Version: body.Version, UserID: userID,
			})
			return err
		}
		_, err := q.UpdateAnonDraftContent(ctx, store.UpdateAnonDraftContentParams{
			TemplateID: &templateID, TemplateVersion: &latest,
			EventID: eventID, Version: body.Version, CookieHash: cookieHash,
		})
		return err
	})
	if errors.Is(err, pgx.ErrNoRows) {
		var wErr error
		if authed {
			wErr = s.mapEventWriteFailure(ctx, s.q, &userID, eventID, nil, map[string]bool{"owner": true, "editor": true})
		} else {
			wErr = s.mapEventWriteFailure(ctx, s.q, nil, eventID, cookieHash, nil)
		}
		writeEventWriteError(w, r, wErr)
		return
	}
	if err != nil {
		serverError(w, r, err)
		return
	}

	updated, ok, err := s.loadEventRow(r, eventID)
	if err != nil {
		serverError(w, r, err)
		return
	}
	if !ok {
		serverError(w, r, fmt.Errorf("event %s vanished after template upgrade", eventID))
		return
	}
	resp, err := s.buildEventResponse(ctx, s.q, updated)
	if err != nil {
		serverError(w, r, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"event": resp})
}

// --- PATCH /v1/events/{id}/settings ---

func (s *Server) handleUpdateEventSettings(w http.ResponseWriter, r *http.Request) {
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
	if !s.allow(w, r, limit{"events:settings:" + userID.String(), 60, time.Hour}) {
		return
	}

	var body struct {
		Version     int32   `json:"version"`
		Slug        *string `json:"slug"`
		Visibility  *string `json:"visibility"`
		Password    *string `json:"password"`
		RsvpMode    *string `json:"rsvp_mode"`
		NotifyRsvps *bool   `json:"notify_rsvps"`
	}
	if !decodeJSON(w, r, &body) {
		return
	}
	if body.Version < 1 {
		writeErrorDetails(w, http.StatusBadRequest, "validation_failed", "version is required.",
			[]content.Issue{{Path: "version", Code: "required", Message: "This field is required."}})
		return
	}

	ctx := r.Context()
	row, ok, err := s.loadMemberEvent(ctx, userID, eventID)
	if err != nil {
		serverError(w, r, err)
		return
	}
	if !ok {
		writeError(w, http.StatusNotFound, "not_found", "No such event.")
		return
	}
	if row.Role != "owner" {
		writeError(w, http.StatusForbidden, "forbidden", "Only the owner can change settings.")
		return
	}

	var normalizedSlug *string
	if body.Slug != nil {
		norm, ok := content.NormalizeSlug(*body.Slug)
		if !ok {
			writeError(w, http.StatusBadRequest, "invalid_slug", "That's not a valid link.")
			return
		}
		if row.Event.PublishedAt != nil && (row.Event.Slug == nil || norm != *row.Event.Slug) {
			writeError(w, http.StatusConflict, "slug_locked", "The link can't be changed after publishing.")
			return
		}
		blocked, err := s.q.SlugBlocked(ctx, norm)
		if err != nil {
			serverError(w, r, fmt.Errorf("check slug blocklist: %w", err))
			return
		}
		if blocked {
			writeError(w, http.StatusConflict, "slug_blocked", "That link isn't available.")
			return
		}
		taken, err := s.q.SlugTaken(ctx, store.SlugTakenParams{Slug: norm, ExcludeEventID: &eventID})
		if err != nil {
			serverError(w, r, fmt.Errorf("check slug taken: %w", err))
			return
		}
		if taken {
			writeError(w, http.StatusConflict, "slug_taken", "That link is already taken.")
			return
		}
		normalizedSlug = &norm
	}

	newVisibility := row.Event.Visibility
	if body.Visibility != nil {
		if !validVisibilities[*body.Visibility] {
			writeError(w, http.StatusBadRequest, "validation_failed", "Unknown visibility.")
			return
		}
		newVisibility = *body.Visibility
	}
	if body.RsvpMode != nil && !validRsvpModes[*body.RsvpMode] {
		writeError(w, http.StatusBadRequest, "validation_failed", "Unknown RSVP mode.")
		return
	}
	if body.Password != nil && newVisibility != "password" {
		writeError(w, http.StatusBadRequest, "validation_failed", "password can only be set when visibility is password.")
		return
	}

	var passwordHash *string
	if body.Password != nil {
		n := utf8.RuneCountInString(*body.Password)
		if n < 8 || n > 128 {
			writeError(w, http.StatusBadRequest, "invalid_password", "Passwords must be between 8 and 128 characters.")
			return
		}
		hash, err := s.hasher.Hash(ctx, *body.Password)
		if err != nil {
			serverError(w, r, fmt.Errorf("hash password: %w", err))
			return
		}
		passwordHash = &hash
	}
	if newVisibility == "password" && passwordHash == nil && row.Event.PasswordHash == nil {
		writeError(w, http.StatusBadRequest, "password_required", "Set a password for this event.")
		return
	}

	_, err = s.q.UpdateEventSettings(ctx, store.UpdateEventSettingsParams{
		Slug: normalizedSlug, Visibility: body.Visibility, PasswordHash: passwordHash,
		RsvpMode: body.RsvpMode, NotifyRsvps: body.NotifyRsvps,
		EventID: eventID, UserID: userID, Version: body.Version,
	})
	if err != nil {
		var pgErr *pgconn.PgError
		if errors.As(err, &pgErr) && pgErr.ConstraintName == "events_slug_immutable" {
			writeError(w, http.StatusConflict, "slug_locked", "The link can't be changed after publishing.")
			return
		}
		// A race with another request claiming the same slug between the
		// SlugTaken check above and this write; the unique constraint is
		// the actual source of truth.
		if errors.As(err, &pgErr) && pgErr.Code == "23505" && pgErr.ConstraintName == "events_slug_key" {
			writeError(w, http.StatusConflict, "slug_taken", "That link is already taken.")
			return
		}
		if errors.Is(err, pgx.ErrNoRows) {
			writeEventWriteError(w, r, s.mapEventWriteFailure(ctx, s.q, &userID, eventID, nil, map[string]bool{"owner": true}))
			return
		}
		serverError(w, r, fmt.Errorf("update event settings: %w", err))
		return
	}

	updated, ok, err := s.loadMemberEvent(ctx, userID, eventID)
	if err != nil {
		serverError(w, r, err)
		return
	}
	if !ok {
		serverError(w, r, fmt.Errorf("event %s vanished after settings update", eventID))
		return
	}
	resp, err := s.buildEventResponse(ctx, s.q, updated)
	if err != nil {
		serverError(w, r, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"event": resp})
}

// --- GET /v1/slugs/{slug} ---

func (s *Server) handleCheckSlug(w http.ResponseWriter, r *http.Request) {
	userID, ok := userIDFrom(r.Context())
	if !ok {
		writeError(w, http.StatusUnauthorized, "unauthorized", "Log in to continue.")
		return
	}
	if !s.allow(w, r, limit{"slugs:check:" + userID.String(), 60, time.Minute}) {
		return
	}

	raw := chi.URLParam(r, "slug")
	ctx := r.Context()
	norm, ok := content.NormalizeSlug(raw)
	if !ok {
		writeJSON(w, http.StatusOK, map[string]any{"slug": raw, "available": false, "reason": "invalid"})
		return
	}
	blocked, err := s.q.SlugBlocked(ctx, norm)
	if err != nil {
		serverError(w, r, fmt.Errorf("check slug blocklist: %w", err))
		return
	}
	if blocked {
		writeJSON(w, http.StatusOK, map[string]any{"slug": norm, "available": false, "reason": "blocked"})
		return
	}
	taken, err := s.q.SlugTaken(ctx, store.SlugTakenParams{Slug: norm, ExcludeEventID: nil})
	if err != nil {
		serverError(w, r, fmt.Errorf("check slug taken: %w", err))
		return
	}
	if taken {
		writeJSON(w, http.StatusOK, map[string]any{"slug": norm, "available": false, "reason": "taken"})
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"slug": norm, "available": true, "reason": nil})
}

// --- POST /v1/events/{id}/publish ---

func (s *Server) handlePublishEvent(w http.ResponseWriter, r *http.Request) {
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
	if !s.allow(w, r, limit{"events:publish:" + userID.String(), 20, 24 * time.Hour}) {
		return
	}

	var body struct {
		Version int32 `json:"version"`
	}
	if !decodeJSON(w, r, &body) {
		return
	}
	if body.Version < 1 {
		writeErrorDetails(w, http.StatusBadRequest, "validation_failed", "version is required.",
			[]content.Issue{{Path: "version", Code: "required", Message: "This field is required."}})
		return
	}

	ctx := r.Context()
	row, ok, err := s.loadMemberEvent(ctx, userID, eventID)
	if err != nil {
		serverError(w, r, err)
		return
	}
	if !ok {
		writeError(w, http.StatusNotFound, "not_found", "No such event.")
		return
	}
	if row.Role != "owner" {
		writeError(w, http.StatusForbidden, "forbidden", "Only the owner can publish.")
		return
	}
	if row.Event.Status == "taken_down" {
		writeError(w, http.StatusConflict, "event_taken_down", "This event has been taken down.")
		return
	}

	var missing []string
	if row.Event.Slug == nil {
		missing = append(missing, "slug")
	}
	if row.Event.Title == "" || row.Event.Title == untitledEventTitle {
		missing = append(missing, "title")
	}
	if row.Event.StartsAt == nil {
		missing = append(missing, "datetime")
	}
	occ, err := s.loadOccasion(ctx, s.q, row.Event.OccasionSlug)
	if err != nil {
		serverError(w, r, err)
		return
	}
	if _, verr := content.ParseStored(row.Event.Content, occ); verr != nil {
		missing = append(missing, "content")
	}
	if len(missing) > 0 {
		writeErrorDetails(w, http.StatusConflict, "not_ready", "This event isn't ready to publish yet.",
			map[string][]string{"missing": missing})
		return
	}
	if row.Event.Slug != nil {
		blocked, err := s.q.SlugBlocked(ctx, *row.Event.Slug)
		if err != nil {
			serverError(w, r, fmt.Errorf("check slug blocklist: %w", err))
			return
		}
		if blocked {
			writeError(w, http.StatusConflict, "slug_blocked", "That link isn't available.")
			return
		}
	}

	_, err = s.q.PublishEvent(ctx, store.PublishEventParams{EventID: eventID, UserID: userID, Version: body.Version})
	if errors.Is(err, pgx.ErrNoRows) {
		writeEventWriteError(w, r, s.mapEventWriteFailure(ctx, s.q, &userID, eventID, nil, map[string]bool{"owner": true}))
		return
	}
	if err != nil {
		serverError(w, r, fmt.Errorf("publish event: %w", err))
		return
	}

	updated, ok, err := s.loadMemberEvent(ctx, userID, eventID)
	if err != nil {
		serverError(w, r, err)
		return
	}
	if !ok {
		serverError(w, r, fmt.Errorf("event %s vanished after publish", eventID))
		return
	}
	resp, err := s.buildEventResponse(ctx, s.q, updated)
	if err != nil {
		serverError(w, r, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"event": resp})
}

// --- POST /v1/events/{id}/unpublish ---

func (s *Server) handleUnpublishEvent(w http.ResponseWriter, r *http.Request) {
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

	var body struct {
		Version int32 `json:"version"`
	}
	if !decodeJSON(w, r, &body) {
		return
	}
	if body.Version < 1 {
		writeErrorDetails(w, http.StatusBadRequest, "validation_failed", "version is required.",
			[]content.Issue{{Path: "version", Code: "required", Message: "This field is required."}})
		return
	}

	ctx := r.Context()
	row, ok, err := s.loadMemberEvent(ctx, userID, eventID)
	if err != nil {
		serverError(w, r, err)
		return
	}
	if !ok {
		writeError(w, http.StatusNotFound, "not_found", "No such event.")
		return
	}
	if row.Role != "owner" {
		writeError(w, http.StatusForbidden, "forbidden", "Only the owner can unpublish.")
		return
	}

	_, err = s.q.UnpublishEvent(ctx, store.UnpublishEventParams{EventID: eventID, UserID: userID, Version: body.Version})
	if errors.Is(err, pgx.ErrNoRows) {
		writeEventWriteError(w, r, s.mapEventWriteFailure(ctx, s.q, &userID, eventID, nil, map[string]bool{"owner": true}))
		return
	}
	if err != nil {
		serverError(w, r, fmt.Errorf("unpublish event: %w", err))
		return
	}

	updated, ok, err := s.loadMemberEvent(ctx, userID, eventID)
	if err != nil {
		serverError(w, r, err)
		return
	}
	if !ok {
		serverError(w, r, fmt.Errorf("event %s vanished after unpublish", eventID))
		return
	}
	resp, err := s.buildEventResponse(ctx, s.q, updated)
	if err != nil {
		serverError(w, r, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"event": resp})
}

// --- DELETE /v1/events/{id} ---

func (s *Server) handleDeleteEvent(w http.ResponseWriter, r *http.Request) {
	eventID, err := parseUUIDParam(r, "id")
	if err != nil {
		writeError(w, http.StatusNotFound, "not_found", "No such event.")
		return
	}
	ctx := r.Context()

	if userID, ok := userIDFrom(ctx); ok {
		err := s.inTx(ctx, func(tx pgx.Tx, q *store.Queries) error {
			if _, err := q.SoftDeleteEvent(ctx, store.SoftDeleteEventParams{EventID: eventID, UserID: userID}); err != nil {
				return err
			}
			_, err := s.jobs.InsertTx(ctx, tx, jobs.MediaVisibilityArgs{EventID: eventID}, nil)
			return err
		})
		if errors.Is(err, pgx.ErrNoRows) {
			writeError(w, http.StatusNotFound, "not_found", "No such event.")
			return
		}
		if err != nil {
			serverError(w, r, fmt.Errorf("delete event: %w", err))
			return
		}
		w.WriteHeader(http.StatusNoContent)
		return
	}

	hash, ok := draftCookieHash(r)
	if !ok {
		writeError(w, http.StatusNotFound, "not_found", "No such event.")
		return
	}
	err = s.inTx(ctx, func(tx pgx.Tx, q *store.Queries) error {
		if _, err := q.SoftDeleteAnonDraft(ctx, store.SoftDeleteAnonDraftParams{EventID: eventID, CookieHash: hash}); err != nil {
			return err
		}
		_, err := s.jobs.InsertTx(ctx, tx, jobs.MediaVisibilityArgs{EventID: eventID}, nil)
		return err
	})
	if errors.Is(err, pgx.ErrNoRows) {
		writeError(w, http.StatusNotFound, "not_found", "No such event.")
		return
	}
	if err != nil {
		serverError(w, r, fmt.Errorf("delete anon draft: %w", err))
		return
	}
	w.WriteHeader(http.StatusNoContent)
}
