package httpapi

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"time"

	"github.com/go-chi/chi/v5"
	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"

	"github.com/ansonarose/seeyouthere-backend/internal/content"
	"github.com/ansonarose/seeyouthere-backend/internal/store"
	"github.com/ansonarose/seeyouthere-backend/internal/token"
)

// publicPhotosPageSize is the fixed page size for the public guest-photo
// wall (§4.4), both embedded in the event page and on its own endpoint.
const publicPhotosPageSize = 24

// --- response shapes (§4.1 PublicEvent) ---

type photoItemResp struct {
	ID     uuid.UUID `json:"id"`
	Src    string    `json:"src"`
	Width  int32     `json:"width"`
	Height int32     `json:"height"`
}

type rsvpResp struct {
	ID        uuid.UUID       `json:"id"`
	Name      string          `json:"name"`
	Email     *string         `json:"email"`
	Attending string          `json:"attending"`
	Count     int32           `json:"count"`
	Answers   json.RawMessage `json:"answers"`
	GuestID   *uuid.UUID      `json:"guest_id"`
	CreatedAt time.Time       `json:"created_at"`
	UpdatedAt time.Time       `json:"updated_at"`
}

type publicViewerGuestResp struct {
	Name          string `json:"name"`
	HouseholdSize int32  `json:"household_size"`
}

// publicRsvpConfigResp is §4.1's PublicEvent.event.rsvp. Fields is the
// occasion's enabled fields resolved to full definitions
// (content.EffectiveFields) only — never merged with the rsvp block's own
// host questions; the frontend concatenates the two itself (phaseC-notes.md).
type publicRsvpConfigResp struct {
	Enabled      bool            `json:"enabled"`
	Open         bool            `json:"open"`
	ClosedReason *string         `json:"closed_reason"`
	SpotsLeft    *int32          `json:"spots_left"`
	Fields       []content.Field `json:"fields"`
	MaxPartySize int             `json:"max_party_size"`
}

type publicEventResp struct {
	ID           uuid.UUID  `json:"id"`
	Slug         string     `json:"slug"`
	Title        string     `json:"title"`
	OccasionSlug string     `json:"occasion_slug"`
	Visibility   string     `json:"visibility"`
	RsvpMode     string     `json:"rsvp_mode"`
	StartsAt     *time.Time `json:"starts_at"`
	// EndsAt mirrors eventResp.EndsAt (event_handlers.go): derived from the
	// content's datetime block, nil if absent or invalid.
	EndsAt         *time.Time               `json:"ends_at"`
	Indexable      bool                     `json:"indexable"`
	RemoveBranding bool                     `json:"remove_branding"`
	Content        json.RawMessage          `json:"content"`
	Theme          content.Theme            `json:"theme"`
	Media          map[string]mediaItemResp `json:"media"`
	RSVP           publicRsvpConfigResp     `json:"rsvp"`
}

// --- visibility gate (§4.4) ---

// errGatePasswordRequired and errGateInviteRequired are returned by
// checkVisibilityGate; callers map them to the exact 401/403 in §4.4.
var (
	errGatePasswordRequired = errors.New("password required")
	errGateInviteRequired   = errors.New("invite required")
)

// checkPasswordCookie reports whether the request carries a current,
// unexpired syt_pw_<event> cookie for a password-gated event. passwordHash
// is nil only if the DB's own CHECK constraint were somehow violated, in
// which case the gate fails closed.
func (s *Server) checkPasswordCookie(r *http.Request, eventID uuid.UUID, passwordHash *string) bool {
	if passwordHash == nil {
		return false
	}
	val, ok := eventCookie(r, pwCookiePrefix, eventID)
	if !ok {
		return false
	}
	return s.tokens.CheckAccessCookie(val, eventID, *passwordHash, time.Now())
}

// checkInviteCookie validates the syt_inv_<event> cookie against eventID and
// returns the guest it names. ok is false if the cookie is absent, malformed,
// names a different event, or was signed under a token_version the guest no
// longer holds (rotated via rotate-link).
func (s *Server) checkInviteCookie(ctx context.Context, r *http.Request, eventID uuid.UUID) (store.GetGuestForTokenRow, bool) {
	val, ok := eventCookie(r, inviteCookiePrefix, eventID)
	if !ok {
		return store.GetGuestForTokenRow{}, false
	}
	id, mac, err := token.Split(val)
	if err != nil {
		return store.GetGuestForTokenRow{}, false
	}
	guest, err := s.q.GetGuestForToken(ctx, id)
	if err != nil {
		return store.GetGuestForTokenRow{}, false
	}
	if guest.EventID != eventID || !s.tokens.CheckGuest(id, guest.TokenVersion, mac) {
		return store.GetGuestForTokenRow{}, false
	}
	return guest, true
}

// checkRSVPCookie validates the syt_rsvp_<event> cookie against eventID and
// returns the open RSVP it names. ok is false if the cookie is absent,
// malformed, names a different event or a guest-owned RSVP (the guest
// cookie is authoritative for those), or was signed under a stale
// edit_token_version.
func (s *Server) checkRSVPCookie(ctx context.Context, r *http.Request, eventID uuid.UUID) (store.GetRSVPForTokenRow, bool) {
	val, ok := eventCookie(r, rsvpCookiePrefix, eventID)
	if !ok {
		return store.GetRSVPForTokenRow{}, false
	}
	id, mac, err := token.Split(val)
	if err != nil {
		return store.GetRSVPForTokenRow{}, false
	}
	rsvp, err := s.q.GetRSVPForToken(ctx, id)
	if err != nil {
		return store.GetRSVPForTokenRow{}, false
	}
	if rsvp.EventID != eventID || rsvp.GuestID != nil || !s.tokens.CheckRSVP(id, rsvp.EditTokenVersion, mac) {
		return store.GetRSVPForTokenRow{}, false
	}
	return rsvp, true
}

// checkVisibilityGate applies §4.4's gate order for an already-resolved,
// published event row: password visibility needs a valid access cookie,
// invite_only needs a valid guest cookie for this event. Returns nil when
// the caller may proceed. Shared by every /v1/public/events/{slug}/*
// handler, including the RSVP write endpoints (B3).
func (s *Server) checkVisibilityGate(ctx context.Context, r *http.Request, row store.GetPublicEventBySlugRow) error {
	switch row.Visibility {
	case "password":
		if !s.checkPasswordCookie(r, row.ID, row.PasswordHash) {
			return errGatePasswordRequired
		}
	case "invite_only":
		if _, ok := s.checkInviteCookie(ctx, r, row.ID); !ok {
			return errGateInviteRequired
		}
	}
	return nil
}

func writeGateError(w http.ResponseWriter, err error) {
	switch {
	case errors.Is(err, errGatePasswordRequired):
		writeError(w, http.StatusUnauthorized, "password_required", "This event needs a password.")
	case errors.Is(err, errGateInviteRequired):
		writeError(w, http.StatusForbidden, "invite_required", "This event needs an invitation.")
	default:
		writeError(w, http.StatusNotFound, "not_found", "No such event.")
	}
}

// resolvePublicEvent loads a live, published event by slug and applies the
// visibility gate, writing the appropriate error response (uniform 404 for
// hidden/draft/taken_down/deleted/unknown, per §4.4) and returning ok=false
// on any failure. Shared by every /v1/public/events/{slug}/* handler,
// including the RSVP write endpoints B3 adds.
func (s *Server) resolvePublicEvent(w http.ResponseWriter, r *http.Request, slug string) (store.GetPublicEventBySlugRow, bool) {
	norm, ok := content.NormalizeSlug(slug)
	if !ok {
		writeError(w, http.StatusNotFound, "not_found", "No such event.")
		return store.GetPublicEventBySlugRow{}, false
	}
	row, err := s.q.GetPublicEventBySlug(r.Context(), &norm)
	if errors.Is(err, pgx.ErrNoRows) {
		writeError(w, http.StatusNotFound, "not_found", "No such event.")
		return store.GetPublicEventBySlugRow{}, false
	}
	if err != nil {
		serverError(w, r, fmt.Errorf("get public event: %w", err))
		return store.GetPublicEventBySlugRow{}, false
	}
	if err := s.checkVisibilityGate(r.Context(), r, row); err != nil {
		writeGateError(w, err)
		return store.GetPublicEventBySlugRow{}, false
	}
	return row, true
}

// --- viewer resolution ---

// loadPublicViewer resolves the visitor's own guest identity and RSVP, if
// any: an invite cookie takes priority (it also carries household_size, so
// the RSVP form can enforce the party-size cap), otherwise an open-RSVP
// cookie for this event. Neither is required.
func (s *Server) loadPublicViewer(ctx context.Context, eventID uuid.UUID, r *http.Request) (*publicViewerGuestResp, *rsvpResp, error) {
	if guest, ok := s.checkInviteCookie(ctx, r, eventID); ok {
		viewer := &publicViewerGuestResp{Name: guest.Name, HouseholdSize: guest.HouseholdSize}
		row, err := s.q.GetGuestRSVP(ctx, store.GetGuestRSVPParams{EventID: eventID, GuestID: guest.ID})
		if errors.Is(err, pgx.ErrNoRows) {
			return viewer, nil, nil
		}
		if err != nil {
			return nil, nil, fmt.Errorf("get guest rsvp: %w", err)
		}
		return viewer, &rsvpResp{
			ID: row.ID, Name: row.Name, Email: row.Email, Attending: row.Attending, Count: row.Count,
			Answers: row.Answers, GuestID: row.GuestID, CreatedAt: row.CreatedAt, UpdatedAt: row.UpdatedAt,
		}, nil
	}

	if tok, ok := s.checkRSVPCookie(ctx, r, eventID); ok {
		row, err := s.q.GetRSVPForEvent(ctx, store.GetRSVPForEventParams{ID: tok.ID, EventID: eventID})
		if errors.Is(err, pgx.ErrNoRows) {
			return nil, nil, nil
		}
		if err != nil {
			return nil, nil, fmt.Errorf("get rsvp for event: %w", err)
		}
		return nil, &rsvpResp{
			ID: row.ID, Name: row.Name, Email: row.Email, Attending: row.Attending, Count: row.Count,
			Answers: row.Answers, GuestID: row.GuestID, CreatedAt: row.CreatedAt, UpdatedAt: row.UpdatedAt,
		}, nil
	}

	return nil, nil, nil
}

// --- rsvp config, theme and media (shared build-up for the page response) ---

// buildPublicRsvpConfig derives the public-facing RSVP state from the
// event's rsvp block: whether RSVPs are enabled at all, whether new "yes"
// responses are currently accepted, and (when capacity is set) how many
// spots remain. This mirrors, but is simpler than, the write-time "closed"
// check in B3 (which also excludes the RSVP being edited).
func (s *Server) buildPublicRsvpConfig(ctx context.Context, eventID uuid.UUID, occ content.Occasion, block *content.RSVPBlock) (publicRsvpConfigResp, error) {
	if block == nil {
		return publicRsvpConfigResp{Fields: []content.Field{}}, nil
	}

	cfg := publicRsvpConfigResp{
		Enabled:      true,
		Fields:       content.EffectiveFields(occ, *block),
		MaxPartySize: block.MaxPartySize,
	}

	if block.Deadline != nil && time.Now().After(*block.Deadline) {
		reason := "deadline"
		cfg.ClosedReason = &reason
		return cfg, nil
	}
	if block.Capacity != nil {
		heads, err := s.q.SumYesHeads(ctx, store.SumYesHeadsParams{EventID: eventID, ExcludeID: nil})
		if err != nil {
			return publicRsvpConfigResp{}, fmt.Errorf("sum yes heads: %w", err)
		}
		left := int32(*block.Capacity) - heads
		if left < 0 {
			left = 0
		}
		cfg.SpotsLeft = &left
		if left <= 0 {
			reason := "capacity"
			cfg.ClosedReason = &reason
			return cfg, nil
		}
	}
	cfg.Open = true
	return cfg, nil
}

// buildPublicMedia loads the approved renditions for every media id the
// content actually references, the same restriction buildEventResponse
// applies for the host view: a public visitor never sees pending or
// rejected guest photos through the content itself.
func (s *Server) buildPublicMedia(ctx context.Context, eventID uuid.UUID, mediaIDs []uuid.UUID) (map[string]mediaItemResp, error) {
	media := map[string]mediaItemResp{}
	if len(mediaIDs) == 0 {
		return media, nil
	}
	rows, err := s.q.ApprovedMediaByIDs(ctx, store.ApprovedMediaByIDsParams{EventID: eventID, MediaIds: uniqueUUIDs(mediaIDs)})
	if err != nil {
		return nil, fmt.Errorf("load media: %w", err)
	}
	for _, m := range rows {
		media[m.ID.String()] = mediaItemResp{Src: fmt.Sprintf("/media/%s/%s", eventID, m.ID), Width: m.Width, Height: m.Height}
	}
	return media, nil
}

// listApprovedPhotosPage loads one page of the public guest-photo wall.
func (s *Server) listApprovedPhotosPage(ctx context.Context, eventID uuid.UUID, cursorCreatedAt time.Time, cursorID uuid.UUID) ([]photoItemResp, *string, error) {
	rows, err := s.q.ListApprovedGuestPhotos(ctx, store.ListApprovedGuestPhotosParams{
		EventID: eventID, CursorCreatedAt: cursorCreatedAt, CursorID: cursorID, Lim: publicPhotosPageSize,
	})
	if err != nil {
		return nil, nil, fmt.Errorf("list approved guest photos: %w", err)
	}
	items := make([]photoItemResp, len(rows))
	for i, row := range rows {
		items[i] = photoItemResp{ID: row.ID, Src: fmt.Sprintf("/media/%s/%s", eventID, row.ID), Width: row.Width, Height: row.Height}
	}
	var next *string
	if len(rows) == publicPhotosPageSize {
		nc := encodeCursor(rows[len(rows)-1].CreatedAt, rows[len(rows)-1].ID)
		next = &nc
	}
	return items, next, nil
}

// --- GET /v1/public/events/{slug} ---

func (s *Server) handleGetPublicEvent(w http.ResponseWriter, r *http.Request) {
	row, ok := s.resolvePublicEvent(w, r, chi.URLParam(r, "slug"))
	if !ok {
		return
	}
	ctx := r.Context()

	occ, err := content.ParseOccasion(row.Occasion)
	if err != nil {
		serverError(w, r, fmt.Errorf("parse occasion for event %s: %w", row.ID, err))
		return
	}
	saved, err := content.ParseStored(row.Content, occ)
	if err != nil {
		serverError(w, r, fmt.Errorf("parse stored content for event %s: %w", row.ID, err))
		return
	}
	manifest, err := content.ValidateStoredManifest(row.Manifest)
	if err != nil {
		serverError(w, r, fmt.Errorf("parse manifest for event %s: %w", row.ID, err))
		return
	}
	backgroundSrc := ""
	if manifest.UsesBackgroundAsset() && row.AssetsPath != "" {
		backgroundSrc = "/media/" + row.AssetsPath + "/background"
	}
	theme := content.ResolveTheme(manifest, row.Overrides, backgroundSrc)

	media, err := s.buildPublicMedia(ctx, row.ID, saved.MediaIDs)
	if err != nil {
		serverError(w, r, err)
		return
	}
	rsvpCfg, err := s.buildPublicRsvpConfig(ctx, row.ID, occ, saved.RSVP)
	if err != nil {
		serverError(w, r, err)
		return
	}
	guest, viewerRSVP, err := s.loadPublicViewer(ctx, row.ID, r)
	if err != nil {
		serverError(w, r, err)
		return
	}
	photos, nextCursor, err := s.listApprovedPhotosPage(ctx, row.ID, farFutureCursorTime, uuid.Max)
	if err != nil {
		serverError(w, r, err)
		return
	}

	resp := publicEventResp{
		ID: row.ID, Slug: *row.Slug, Title: row.Title, OccasionSlug: row.OccasionSlug,
		Visibility: row.Visibility, RsvpMode: row.RsvpMode, StartsAt: row.StartsAt,
		EndsAt:    saved.EndsAt,
		Indexable: row.Visibility == "public", RemoveBranding: row.RemoveBranding,
		Content: saved.JSON, Theme: theme, Media: media, RSVP: rsvpCfg,
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"event":  resp,
		"viewer": map[string]any{"guest": guest, "rsvp": viewerRSVP},
		"photos": map[string]any{"items": photos, "next_cursor": nextCursor},
	})
}

// farFutureCursorTime is the "no cursor yet" sentinel for the descending
// (created_at, id) keyset used by the public photo wall, matching the
// pattern in handleListEvents.
var farFutureCursorTime = time.Date(9999, 12, 31, 0, 0, 0, 0, time.UTC)

// --- GET /v1/public/events/{slug}/photos ---

func (s *Server) handleListPublicPhotos(w http.ResponseWriter, r *http.Request) {
	row, ok := s.resolvePublicEvent(w, r, chi.URLParam(r, "slug"))
	if !ok {
		return
	}

	cursorCreatedAt, cursorID := farFutureCursorTime, uuid.Max
	if c := r.URL.Query().Get("cursor"); c != "" {
		t, id, err := decodeCursor(c)
		if err != nil {
			writeError(w, http.StatusBadRequest, "invalid_cursor", "Invalid cursor.")
			return
		}
		cursorCreatedAt, cursorID = t, id
	}

	items, next, err := s.listApprovedPhotosPage(r.Context(), row.ID, cursorCreatedAt, cursorID)
	if err != nil {
		serverError(w, r, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"items": items, "next_cursor": next})
}

// --- POST /v1/public/events/{slug}/unlock ---

func (s *Server) handleUnlockPublicEvent(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	norm, ok := content.NormalizeSlug(chi.URLParam(r, "slug"))
	if !ok {
		writeError(w, http.StatusNotFound, "not_found", "No such event.")
		return
	}
	row, err := s.q.GetPublicEventBySlug(ctx, &norm)
	if errors.Is(err, pgx.ErrNoRows) {
		writeError(w, http.StatusNotFound, "not_found", "No such event.")
		return
	}
	if err != nil {
		serverError(w, r, fmt.Errorf("get public event: %w", err))
		return
	}
	// A non-password event has nothing to unlock; answering not_found (as if
	// the slug didn't resolve) rather than some other code keeps this
	// endpoint from confirming a slug's visibility to a caller who doesn't
	// already have it right.
	if row.Visibility != "password" || row.PasswordHash == nil {
		writeError(w, http.StatusNotFound, "not_found", "No such event.")
		return
	}

	// Rate-limited before hashing (Argon2id is deliberately expensive):
	// per IP+event bounds one visitor's retries, per-event bounds a
	// distributed attempt that rotates IPs, and per-IP-across-events bounds
	// one visitor spreading guesses across many slugs to dodge both.
	ip := ipRateKey(clientIPFrom(ctx))
	eventKey := "unlock:event:" + row.ID.String()
	ipKey := "unlock:ip:" + ip
	if !s.allow(w, r,
		limit{"unlock:ip:" + ip + ":" + row.ID.String(), 10, 15 * time.Minute},
		limit{eventKey, 300, time.Hour},
		limit{ipKey, 30, 15 * time.Minute},
	) {
		return
	}

	var body struct {
		Password string `json:"password"`
	}
	if !decodeJSON(w, r, &body) {
		return
	}

	match, err := s.hasher.Verify(ctx, *row.PasswordHash, body.Password)
	if err != nil {
		serverError(w, r, fmt.Errorf("verify password: %w", err))
		return
	}
	if !match {
		writeError(w, http.StatusUnauthorized, "wrong_password", "That password is incorrect.")
		return
	}
	// A successful verify wasn't an abusive attempt: give the per-event and
	// per-IP-across-events budgets back so a legitimate visitor who
	// mistypes once isn't left closer to locked out on other events.
	s.limiter.Uncount(ctx, eventKey, 1)
	s.limiter.Uncount(ctx, ipKey, 1)

	cookieVal := s.tokens.AccessCookie(row.ID, *row.PasswordHash, time.Now().Add(accessCookieTTL))
	s.setEventCookie(w, pwCookiePrefix, row.ID, cookieVal, accessCookieTTL)
	w.WriteHeader(http.StatusNoContent)
}

// --- POST /v1/public/invites/accept ---

// handleAcceptInvite exchanges a guest invite token (read from the request
// body only — §0: tokens never arrive in query strings) for the syt_inv_
// cookie. The token itself, already verified against the guest's current
// token_version, becomes the cookie value: it's exactly the credential the
// gate re-checks on every later request, so there's nothing else to derive.
func (s *Server) handleAcceptInvite(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	ip := ipRateKey(clientIPFrom(ctx))
	if !s.allow(w, r, limit{"invite:accept:ip:" + ip, 30, 10 * time.Minute}) {
		return
	}

	var body struct {
		Token string `json:"token"`
	}
	if !decodeJSON(w, r, &body) {
		return
	}

	id, mac, err := token.Split(body.Token)
	if err != nil {
		writeError(w, http.StatusNotFound, "invalid_link", "This link isn't valid.")
		return
	}
	guest, err := s.q.GetGuestForToken(ctx, id)
	if errors.Is(err, pgx.ErrNoRows) {
		writeError(w, http.StatusNotFound, "invalid_link", "This link isn't valid.")
		return
	}
	if err != nil {
		serverError(w, r, fmt.Errorf("get guest for token: %w", err))
		return
	}
	if guest.EventSlug == nil || guest.EventStatus != "published" || !s.tokens.CheckGuest(id, guest.TokenVersion, mac) {
		writeError(w, http.StatusNotFound, "invalid_link", "This link isn't valid.")
		return
	}

	s.setEventCookie(w, inviteCookiePrefix, guest.EventID, body.Token, inviteCookieTTL)
	writeJSON(w, http.StatusOK, map[string]string{"slug": *guest.EventSlug})
}

// --- POST /v1/public/rsvp-links/accept ---

// handleAcceptRSVPLink is handleAcceptInvite's counterpart for an open
// (non-guest) RSVP's edit link.
func (s *Server) handleAcceptRSVPLink(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	ip := ipRateKey(clientIPFrom(ctx))
	if !s.allow(w, r, limit{"rsvp_link:accept:ip:" + ip, 30, 10 * time.Minute}) {
		return
	}

	var body struct {
		Token string `json:"token"`
	}
	if !decodeJSON(w, r, &body) {
		return
	}

	id, mac, err := token.Split(body.Token)
	if err != nil {
		writeError(w, http.StatusNotFound, "invalid_link", "This link isn't valid.")
		return
	}
	rsvp, err := s.q.GetRSVPForToken(ctx, id)
	if errors.Is(err, pgx.ErrNoRows) {
		writeError(w, http.StatusNotFound, "invalid_link", "This link isn't valid.")
		return
	}
	if err != nil {
		serverError(w, r, fmt.Errorf("get rsvp for token: %w", err))
		return
	}
	if rsvp.EventSlug == nil || rsvp.EventStatus != "published" || rsvp.GuestID != nil || !s.tokens.CheckRSVP(id, rsvp.EditTokenVersion, mac) {
		writeError(w, http.StatusNotFound, "invalid_link", "This link isn't valid.")
		return
	}

	s.setEventCookie(w, rsvpCookiePrefix, rsvp.EventID, body.Token, rsvpCookieTTL)
	writeJSON(w, http.StatusOK, map[string]string{"slug": *rsvp.EventSlug})
}
