package httpapi

import (
	"errors"
	"fmt"
	"io"
	"mime"
	"net/http"
	"os"
	"path/filepath"
	"regexp"
	"strconv"
	"time"

	"github.com/go-chi/chi/v5"
	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"

	"github.com/ansonarose/seeyouthere-backend/internal/content"
	"github.com/ansonarose/seeyouthere-backend/internal/media"
	"github.com/ansonarose/seeyouthere-backend/internal/store"
)

// Upload limits (§6.2, §4.8/§4.4): the host/anon path allows slightly larger
// files than the public guest path, and Caddy's own request_body max_size in
// production is a backstop above both.
const (
	maxHostUploadBytes  = 10 << 20
	maxGuestUploadBytes = 8 << 20

	maxEventMediaFiles     = 150
	maxEventMediaBytes     = 300 << 20
	maxAnonDraftMediaFiles = 10
	maxEventGuestPhotos    = 2000
)

// errMediaQuota signals a quota check that failed inside a transaction (as
// opposed to the fast, non-authoritative pre-check made before the CPU-heavy
// upload pipeline runs).
var errMediaQuota = errors.New("media quota exceeded")

// errEventTakenDown signals that LockEventForEditor found the event taken
// down after the upload pipeline already ran, so the file must be discarded
// rather than recorded as public.
var errEventTakenDown = errors.New("event taken down")

// acceptedImageContentTypes is the declared Content-Type an upload must
// carry; the sniffed type inside processUpload/Stage is still what actually
// decides the image format, but rejecting an obviously wrong header early
// avoids running the CPU-heavy pipeline against a mislabelled body.
var acceptedImageContentTypes = map[string]bool{
	"image/jpeg": true, "image/png": true, "image/webp": true,
}

func mediaSrc(eventID, mediaID uuid.UUID) string {
	return fmt.Sprintf("/media/%s/%s", eventID, mediaID)
}

// mediaResp is the §4.1 Media shape.
type mediaResp struct {
	ID               uuid.UUID `json:"id"`
	Src              string    `json:"src"`
	Width            int32     `json:"width"`
	Height           int32     `json:"height"`
	SizeBytes        int64     `json:"size_bytes"`
	UploadedBy       string    `json:"uploaded_by"`
	ModerationStatus string    `json:"moderation_status"`
	GuestName        *string   `json:"guest_name"`
	CreatedAt        time.Time `json:"created_at"`
}

// extendUploadDeadline gives upload requests a much longer deadline than the
// default 15s /v1 group timeout: staging, the free-space check and image
// processing (bounded by the processor's own 20s semaphore wait, plus
// decode/scale/encode time) can legitimately run longer than that under
// load. Routes using this must not also sit inside middleware.Timeout.
func extendUploadDeadline(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		rc := http.NewResponseController(w)
		deadline := time.Now().Add(120 * time.Second)
		_ = rc.SetReadDeadline(deadline)
		_ = rc.SetWriteDeadline(deadline)
		next.ServeHTTP(w, r)
	})
}

// processUpload runs Stage and Process against body (which the caller has
// already wrapped in http.MaxBytesReader with the right limit) and maps
// every failure mode the pipeline defines to the exact response §4.4/§4.8
// promise. On success it returns a committed-but-not-yet-recorded output
// directory the caller must pass to Store.Commit (or os.RemoveAll on any
// later failure before the DB insert).
func (s *Server) processUpload(w http.ResponseWriter, r *http.Request, body io.Reader) (outDir string, result media.Result, ok bool) {
	declared, _, err := mime.ParseMediaType(r.Header.Get("Content-Type"))
	if err != nil || !acceptedImageContentTypes[declared] {
		writeError(w, http.StatusUnsupportedMediaType, "unsupported_image",
			"Choose a JPEG, PNG or WebP image. On iPhone, pick \"Most Compatible\" when saving the photo.")
		return "", media.Result{}, false
	}

	if err := s.media.CheckFree(); err != nil {
		writeError(w, http.StatusInsufficientStorage, "storage_full", "Storage is nearly full. Try again later.")
		return "", media.Result{}, false
	}

	srcPath, sniffed, err := s.media.Stage(body)
	if errors.Is(err, media.ErrUnsupported) {
		writeError(w, http.StatusUnsupportedMediaType, "unsupported_image",
			"Choose a JPEG, PNG or WebP image. On iPhone, pick \"Most Compatible\" when saving the photo.")
		return "", media.Result{}, false
	}
	if err != nil {
		var tooBig *http.MaxBytesError
		if errors.As(err, &tooBig) {
			writeError(w, http.StatusRequestEntityTooLarge, "body_too_large", "That image is too large.")
			return "", media.Result{}, false
		}
		serverError(w, r, fmt.Errorf("stage upload: %w", err))
		return "", media.Result{}, false
	}
	defer os.Remove(srcPath)

	outDir, err = s.media.NewOutputDir()
	if err != nil {
		serverError(w, r, fmt.Errorf("create output dir: %w", err))
		return "", media.Result{}, false
	}

	result, err = s.images.Process(r.Context(), srcPath, sniffed, outDir)
	if err == nil {
		return outDir, result, true
	}
	os.RemoveAll(outDir)
	switch {
	case errors.Is(err, media.ErrBusy):
		w.Header().Set("Retry-After", "5")
		writeError(w, http.StatusServiceUnavailable, "busy", "The server is busy processing images. Try again shortly.")
	case errors.Is(err, media.ErrTooLarge):
		writeError(w, http.StatusUnprocessableEntity, "image_too_large", "That image is too large to process.")
	case errors.Is(err, media.ErrInvalidImage):
		writeError(w, http.StatusUnprocessableEntity, "invalid_image", "That image couldn't be processed.")
	default:
		serverError(w, r, fmt.Errorf("process upload: %w", err))
	}
	return "", media.Result{}, false
}

// --- POST /v1/events/{id}/media ---

// handleUploadEventMedia covers §4.8's owner/editor/anon-draft upload path.
// A fast, non-authoritative quota check runs before the CPU-heavy pipeline
// (Stage/Process) so an event that's already over its cap fails cheaply; the
// authoritative check runs again inside the insert transaction, after the
// files are already committed to disk (files first, DB row second, per the
// package comment on media.sql: any failure past this point deletes them).
func (s *Server) handleUploadEventMedia(w http.ResponseWriter, r *http.Request) {
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

	if authed {
		if !s.allow(w, r, limit{"media:upload:user:" + userID.String(), 60, time.Hour}) {
			return
		}
	} else {
		ip := ipRateKey(clientIPFrom(ctx))
		if !s.allow(w, r,
			limit{"media:upload:anon:" + ip, 20, time.Hour},
			limit{"media:upload:anon:global", 500, 24 * time.Hour},
		) {
			return
		}
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
	if authed && row.Role != "owner" && row.Role != "editor" {
		writeError(w, http.StatusForbidden, "forbidden", "You don't have access to add media.")
		return
	}
	if row.Event.Status == "taken_down" {
		writeError(w, http.StatusConflict, "event_taken_down", "This event has been taken down.")
		return
	}

	fileCap := int64(maxEventMediaFiles)
	if !authed {
		fileCap = maxAnonDraftMediaFiles
	}
	usage, err := s.q.MediaUsage(ctx, store.MediaUsageParams{EventID: eventID, UploadedBy: "host"})
	if err != nil {
		serverError(w, r, fmt.Errorf("media usage: %w", err))
		return
	}
	if usage.Files >= fileCap || usage.Bytes >= maxEventMediaBytes {
		writeError(w, http.StatusConflict, "media_quota", "This event has reached its media limit.")
		return
	}

	body := http.MaxBytesReader(w, r.Body, maxHostUploadBytes)
	outDir, result, ok := s.processUpload(w, r, body)
	if !ok {
		return
	}

	mediaID := uuid.Must(uuid.NewV7())
	if err := s.media.Commit(outDir, media.AreaPublic, eventID, mediaID); err != nil {
		os.RemoveAll(outDir)
		serverError(w, r, fmt.Errorf("commit media: %w", err))
		return
	}
	storageKey := eventID.String() + "/" + mediaID.String()

	var resp mediaResp
	txErr := s.inTx(ctx, func(_ pgx.Tx, q *store.Queries) error {
		// Serialises the quota check and this insert against any other
		// concurrent upload/takedown for the same event, so two uploads
		// racing the fast pre-check above can't both slip past the cap, and
		// a takedown that commits first is seen here before the file is
		// recorded as public.
		if authed {
			lockRow, err := q.LockEventForEditor(ctx, store.LockEventForEditorParams{EventID: eventID, UserID: userID})
			if err != nil {
				return err
			}
			if lockRow.Status == "taken_down" {
				return errEventTakenDown
			}
		} else {
			if _, err := q.LockAnonDraftEvent(ctx, store.LockAnonDraftEventParams{EventID: eventID, CookieHash: cookieHash}); err != nil {
				return err
			}
		}
		usage, err := q.MediaUsage(ctx, store.MediaUsageParams{EventID: eventID, UploadedBy: "host"})
		if err != nil {
			return fmt.Errorf("media usage: %w", err)
		}
		if usage.Files >= fileCap || usage.Bytes >= maxEventMediaBytes {
			return errMediaQuota
		}
		if authed {
			row, err := q.CreateHostMedia(ctx, store.CreateHostMediaParams{
				ID: mediaID, UserID: userID, EventID: eventID, StorageKey: storageKey,
				SizeBytes: result.Bytes, Width: int32(result.Width), Height: int32(result.Height),
			})
			if err != nil {
				return err
			}
			resp = mediaResp{
				ID: row.ID, Src: mediaSrc(eventID, row.ID), Width: row.Width, Height: row.Height,
				SizeBytes: row.SizeBytes, UploadedBy: row.UploadedBy, ModerationStatus: row.ModerationStatus,
				CreatedAt: row.CreatedAt,
			}
			return nil
		}
		row, err := q.CreateAnonDraftMedia(ctx, store.CreateAnonDraftMediaParams{
			ID: mediaID, StorageKey: storageKey, SizeBytes: result.Bytes,
			Width: int32(result.Width), Height: int32(result.Height), EventID: eventID, CookieHash: cookieHash,
		})
		if err != nil {
			return err
		}
		resp = mediaResp{
			ID: row.ID, Src: mediaSrc(eventID, row.ID), Width: row.Width, Height: row.Height,
			SizeBytes: row.SizeBytes, UploadedBy: row.UploadedBy, ModerationStatus: row.ModerationStatus,
			CreatedAt: row.CreatedAt,
		}
		return nil
	})
	if txErr != nil {
		_ = s.media.DeleteMedia(eventID, mediaID)
		switch {
		case errors.Is(txErr, errMediaQuota):
			writeError(w, http.StatusConflict, "media_quota", "This event has reached its media limit.")
		case errors.Is(txErr, errEventTakenDown):
			writeError(w, http.StatusConflict, "event_taken_down", "This event has been taken down.")
		case errors.Is(txErr, pgx.ErrNoRows):
			// Role, ownership or status changed between the pre-check above
			// and the insert (event taken down, draft claimed, etc.): the
			// same 404 a fresh request against the new state would get.
			writeError(w, http.StatusNotFound, "not_found", "No such event.")
		default:
			serverError(w, r, fmt.Errorf("create media: %w", txErr))
		}
		return
	}
	writeJSON(w, http.StatusCreated, map[string]any{"media": resp})
}

// --- GET /v1/events/{id}/media ---

func (s *Server) handleListEventMedia(w http.ResponseWriter, r *http.Request) {
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

	limitN := clampLimit(r.URL.Query().Get("limit"), 25, 100)
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

	var uploadedBy, status *string
	if v := r.URL.Query().Get("uploaded_by"); v != "" {
		if v != "host" && v != "guest" {
			writeError(w, http.StatusBadRequest, "validation_failed", "Unknown uploaded_by filter.")
			return
		}
		uploadedBy = &v
	}
	if v := r.URL.Query().Get("status"); v != "" {
		if v != "pending" && v != "approved" && v != "rejected" {
			writeError(w, http.StatusBadRequest, "validation_failed", "Unknown status filter.")
			return
		}
		status = &v
	}

	ctx := r.Context()
	rows, err := s.q.ListEventMedia(ctx, store.ListEventMediaParams{
		EventID: eventID, UserID: userID, UploadedBy: uploadedBy, Status: status,
		CursorCreatedAt: cursorCreatedAt, CursorID: cursorID, Lim: int32(limitN),
	})
	if err != nil {
		serverError(w, r, fmt.Errorf("list event media: %w", err))
		return
	}

	items := make([]mediaResp, len(rows))
	for i, row := range rows {
		items[i] = mediaResp{
			ID: row.ID, Src: mediaSrc(eventID, row.ID), Width: row.Width, Height: row.Height,
			SizeBytes: row.SizeBytes, UploadedBy: row.UploadedBy, ModerationStatus: row.ModerationStatus,
			GuestName: row.GuestName, CreatedAt: row.CreatedAt,
		}
	}
	var nextCursor *string
	if len(rows) == limitN {
		nc := encodeCursor(rows[len(rows)-1].CreatedAt, rows[len(rows)-1].ID)
		nextCursor = &nc
	}
	writeJSON(w, http.StatusOK, map[string]any{"media": items, "next_cursor": nextCursor})
}

// --- GET /v1/events/{id}/media/{mediaID}/file?w=480 ---

// handleGetEventMediaFile is the only way a host or co-host previews a
// pending (unapproved) or quarantined guest photo: Store.Open searches every
// area, and GetEventMedia's WHERE (any member) is the sole authorization
// check.
func (s *Server) handleGetEventMediaFile(w http.ResponseWriter, r *http.Request) {
	eventID, err := parseUUIDParam(r, "id")
	if err != nil {
		writeError(w, http.StatusNotFound, "not_found", "No such file.")
		return
	}
	mediaID, err := parseUUIDParam(r, "mediaID")
	if err != nil {
		writeError(w, http.StatusNotFound, "not_found", "No such file.")
		return
	}
	userID, ok := userIDFrom(r.Context())
	if !ok {
		writeError(w, http.StatusUnauthorized, "unauthorized", "Log in to continue.")
		return
	}

	width, err := strconv.Atoi(r.URL.Query().Get("w"))
	if err != nil || (width != 480 && width != 1080) {
		writeError(w, http.StatusBadRequest, "validation_failed", "w must be 480 or 1080.")
		return
	}

	ctx := r.Context()
	if _, err := s.q.GetEventMedia(ctx, store.GetEventMediaParams{MediaID: mediaID, EventID: eventID, UserID: userID}); err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			writeError(w, http.StatusNotFound, "not_found", "No such file.")
			return
		}
		serverError(w, r, fmt.Errorf("get event media: %w", err))
		return
	}

	f, err := s.media.Open(eventID, mediaID, width)
	if err != nil {
		if os.IsNotExist(err) {
			writeError(w, http.StatusNotFound, "not_found", "No such file.")
			return
		}
		serverError(w, r, fmt.Errorf("open media file: %w", err))
		return
	}
	defer f.Close()
	info, err := f.Stat()
	if err != nil {
		serverError(w, r, fmt.Errorf("stat media file: %w", err))
		return
	}

	w.Header().Set("X-Content-Type-Options", "nosniff")
	w.Header().Set("Cache-Control", "private, max-age=300")
	http.ServeContent(w, r, "", info.ModTime(), f)
}

// --- POST /v1/events/{id}/media/{mediaID}/approve and /reject ---

func (s *Server) handleApproveMedia(w http.ResponseWriter, r *http.Request) {
	s.setMediaModeration(w, r, "approved")
}

func (s *Server) handleRejectMedia(w http.ResponseWriter, r *http.Request) {
	s.setMediaModeration(w, r, "rejected")
}

func (s *Server) setMediaModeration(w http.ResponseWriter, r *http.Request, newStatus string) {
	eventID, err := parseUUIDParam(r, "id")
	if err != nil {
		writeError(w, http.StatusNotFound, "not_found", "No such media.")
		return
	}
	mediaID, err := parseUUIDParam(r, "mediaID")
	if err != nil {
		writeError(w, http.StatusNotFound, "not_found", "No such media.")
		return
	}
	userID, ok := userIDFrom(r.Context())
	if !ok {
		writeError(w, http.StatusUnauthorized, "unauthorized", "Log in to continue.")
		return
	}

	ctx := r.Context()
	row, err := s.q.SetGuestMediaModeration(ctx, store.SetGuestMediaModerationParams{
		Status: newStatus, MediaID: mediaID, EventID: eventID, UserID: userID,
	})
	if errors.Is(err, pgx.ErrNoRows) {
		writeError(w, http.StatusNotFound, "not_found", "No such media.")
		return
	}
	if err != nil {
		serverError(w, r, fmt.Errorf("set media moderation: %w", err))
		return
	}

	var moveErr error
	switch {
	case newStatus == "approved" && row.OldStatus != "approved":
		moveErr = s.media.MoveMedia(media.AreaPending, media.AreaPublic, eventID, mediaID)
	case newStatus == "rejected" && row.OldStatus != "rejected":
		// Files are gone as of this call; the row itself is purged by the
		// cleanup job 7 days later (media.sql: DeleteRejectedMedia).
		moveErr = s.media.DeleteMedia(eventID, mediaID)
	}
	if moveErr != nil {
		serverError(w, r, fmt.Errorf("move media files: %w", moveErr))
		return
	}

	writeJSON(w, http.StatusOK, map[string]any{"media": mediaResp{
		ID: row.ID, Src: mediaSrc(eventID, row.ID), Width: row.Width, Height: row.Height,
		SizeBytes: row.SizeBytes, UploadedBy: row.UploadedBy, ModerationStatus: row.ModerationStatus,
		CreatedAt: row.CreatedAt,
	}})
}

// --- DELETE /v1/events/{id}/media/{mediaID} ---

func (s *Server) handleDeleteEventMedia(w http.ResponseWriter, r *http.Request) {
	eventID, err := parseUUIDParam(r, "id")
	if err != nil {
		writeError(w, http.StatusNotFound, "not_found", "No such media.")
		return
	}
	mediaID, err := parseUUIDParam(r, "mediaID")
	if err != nil {
		writeError(w, http.StatusNotFound, "not_found", "No such media.")
		return
	}
	userID, ok := userIDFrom(r.Context())
	if !ok {
		writeError(w, http.StatusUnauthorized, "unauthorized", "Log in to continue.")
		return
	}

	ctx := r.Context()
	row, ok, err := s.loadMemberEvent(ctx, userID, eventID)
	if err != nil {
		serverError(w, r, err)
		return
	}
	if !ok {
		writeError(w, http.StatusNotFound, "not_found", "No such media.")
		return
	}
	if row.Role != "owner" && row.Role != "editor" {
		writeError(w, http.StatusForbidden, "forbidden", "You don't have access to remove media.")
		return
	}

	occ, err := s.loadOccasion(ctx, s.q, row.Event.OccasionSlug)
	if err != nil {
		serverError(w, r, err)
		return
	}
	if saved, err := content.ParseStored(row.Event.Content, occ); err == nil {
		for _, id := range saved.MediaIDs {
			if id == mediaID {
				writeError(w, http.StatusConflict, "media_in_use", "This image is used in the event content.")
				return
			}
		}
	}

	if _, err := s.q.DeleteEventMedia(ctx, store.DeleteEventMediaParams{MediaID: mediaID, EventID: eventID, UserID: userID}); err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			writeError(w, http.StatusNotFound, "not_found", "No such media.")
			return
		}
		serverError(w, r, fmt.Errorf("delete event media: %w", err))
		return
	}
	if err := s.media.DeleteMedia(eventID, mediaID); err != nil {
		serverError(w, r, fmt.Errorf("delete media files: %w", err))
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

// --- POST /v1/public/events/{slug}/photos ---

// handleUploadGuestPhoto covers §4.4's public guest-photo upload: it re-runs
// the same visibility gate the public GET enforces, additionally requires an
// invite cookie when rsvp_mode is invite_only, and requires the event's
// guest_photos block to have open=true. Uploads start in AreaPending and
// moderation_status "pending"; they're never served until a host approves
// them.
func (s *Server) handleUploadGuestPhoto(w http.ResponseWriter, r *http.Request) {
	rawSlug := chi.URLParam(r, "slug")
	slug, ok := content.NormalizeSlug(rawSlug)
	if !ok {
		writeError(w, http.StatusNotFound, "not_found", "No such event.")
		return
	}

	ctx := r.Context()
	event, err := s.q.GetPublicEventBySlug(ctx, &slug)
	if errors.Is(err, pgx.ErrNoRows) {
		writeError(w, http.StatusNotFound, "not_found", "No such event.")
		return
	}
	if err != nil {
		serverError(w, r, fmt.Errorf("get public event: %w", err))
		return
	}

	if err := s.checkVisibilityGate(ctx, r, event); err != nil {
		writeGateError(w, err)
		return
	}

	var guestID *uuid.UUID
	if event.RsvpMode == "invite_only" {
		guest, ok := s.checkInviteCookie(ctx, r, event.ID)
		if !ok {
			writeError(w, http.StatusForbidden, "invite_required", "This event is invite-only.")
			return
		}
		guestID = &guest.ID
	}

	occ, err := s.loadOccasion(ctx, s.q, event.OccasionSlug)
	if err != nil {
		serverError(w, r, err)
		return
	}
	saved, err := content.ParseStored(event.Content, occ)
	if err != nil {
		serverError(w, r, fmt.Errorf("parse stored content for event %s: %w", event.ID, err))
		return
	}
	if saved.Photos == nil || !saved.Photos.Open {
		writeError(w, http.StatusConflict, "photos_closed", "Photo uploads aren't open for this event.")
		return
	}

	usage, err := s.q.MediaUsage(ctx, store.MediaUsageParams{EventID: event.ID, UploadedBy: "guest"})
	if err != nil {
		serverError(w, r, fmt.Errorf("media usage: %w", err))
		return
	}
	if usage.Files >= maxEventGuestPhotos {
		writeError(w, http.StatusConflict, "photo_limit", "This event has reached its photo limit.")
		return
	}

	// Rate-limited only now, after every gate/quota check that doesn't need
	// the CPU-heavy pipeline: charging the budget any earlier would let
	// someone burn it against a slug they only know but can't actually
	// upload to (still gated, still invite-only, already closed). The
	// per-IP-across-events and global caps bound an attacker who spreads
	// uploads across many slugs to dodge the per-event limit.
	ip := ipRateKey(clientIPFrom(ctx))
	if !s.allow(w, r,
		limit{"media:guest:ip:" + ip + ":" + event.ID.String(), 20, time.Hour},
		limit{"media:guest:event:" + event.ID.String(), 300, 24 * time.Hour},
		limit{"media:guest:ip:" + ip, 60, time.Hour},
		limit{"media:guest:global", 2000, 24 * time.Hour},
	) {
		return
	}

	body := http.MaxBytesReader(w, r.Body, maxGuestUploadBytes)
	outDir, result, ok := s.processUpload(w, r, body)
	if !ok {
		return
	}

	mediaID := uuid.Must(uuid.NewV7())
	if err := s.media.Commit(outDir, media.AreaPending, event.ID, mediaID); err != nil {
		os.RemoveAll(outDir)
		serverError(w, r, fmt.Errorf("commit media: %w", err))
		return
	}
	storageKey := event.ID.String() + "/" + mediaID.String()

	var moderationStatus string
	txErr := s.inTx(ctx, func(_ pgx.Tx, q *store.Queries) error {
		// Locks the event row so the quota check below and this insert are
		// exact under concurrent uploads (§14: "photos" is one of the exact
		// caps), and re-confirms the event is still published.
		if _, err := q.LockEventForRSVP(ctx, event.ID); err != nil {
			return err
		}
		usage, err := q.MediaUsage(ctx, store.MediaUsageParams{EventID: event.ID, UploadedBy: "guest"})
		if err != nil {
			return fmt.Errorf("media usage: %w", err)
		}
		if usage.Files >= maxEventGuestPhotos {
			return errMediaQuota
		}
		row, err := q.CreateGuestMedia(ctx, store.CreateGuestMediaParams{
			ID: mediaID, StorageKey: storageKey, SizeBytes: result.Bytes,
			Width: int32(result.Width), Height: int32(result.Height), GuestID: guestID, EventID: event.ID,
		})
		if err != nil {
			return err
		}
		moderationStatus = row.ModerationStatus
		return nil
	})
	if txErr != nil {
		_ = s.media.DeleteMedia(event.ID, mediaID)
		switch {
		case errors.Is(txErr, errMediaQuota):
			writeError(w, http.StatusConflict, "photo_limit", "This event has reached its photo limit.")
		case errors.Is(txErr, pgx.ErrNoRows):
			writeError(w, http.StatusNotFound, "not_found", "No such event.")
		default:
			serverError(w, r, fmt.Errorf("create guest media: %w", txErr))
		}
		return
	}
	writeJSON(w, http.StatusCreated, map[string]any{"id": mediaID, "moderation_status": moderationStatus})
}

// --- GET /media/* (dev convenience; §4.12) ---

// mediaPathRe matches only the exact rendition paths §4.12 allows through:
// an event/media id pair, or a template version's background asset, each at
// one of the three fixed widths. Anything else (directory listings, other
// extensions, path traversal attempts) 404s.
var mediaPathRe = regexp.MustCompile(`^/media/([0-9a-f-]{36}/[0-9a-f-]{36}|templates/[0-9a-f-]{36}/[0-9]+/background)/(480|1080|1920)\.jpg$`)

// devMediaHandler serves MEDIA_ROOT/public directly, for local development
// where there's no Caddy in front of the API to do it. It's only mounted
// when the server isn't running in production: in production Caddy answers
// these requests before they reach Next or the API (deploy/Caddyfile), and
// pending/ and quarantine/ are never served by either.
func (s *Server) devMediaHandler() http.HandlerFunc {
	fs := http.StripPrefix("/media", http.FileServer(http.Dir(filepath.Join(s.cfg.MediaRoot, "public"))))
	return func(w http.ResponseWriter, r *http.Request) {
		if !mediaPathRe.MatchString(r.URL.Path) {
			writeError(w, http.StatusNotFound, "not_found", "No such file.")
			return
		}
		w.Header().Set("X-Content-Type-Options", "nosniff")
		w.Header().Set("Cache-Control", "public, max-age=31536000, immutable")
		fs.ServeHTTP(w, r)
	}
}
