package httpapi

import (
	"context"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"mime"
	"net/http"
	"os"
	"strconv"
	"time"

	"github.com/go-chi/chi/v5"
	"github.com/go-chi/chi/v5/middleware"
	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"

	"github.com/ansonarose/seeyouthere-backend/internal/content"
	"github.com/ansonarose/seeyouthere-backend/internal/jobs"
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

// maxEventPendingGuestBytes bounds the stored size (decimal 100 MB) of one
// event's guest photos awaiting moderation. Pending photos are not charged to
// the owner's personal cap (so a hostile guest cannot lock the host out of
// their own uploads); this per-event allowance is what bounds them instead.
// A guest photo counts toward the owner's cap only once the host approves it.
// A var so tests can lower it.
var maxEventPendingGuestBytes int64 = 100_000_000

// errMediaQuota signals a quota check that failed inside a transaction (as
// opposed to the fast, non-authoritative pre-check made before the CPU-heavy
// upload pipeline runs).
var errMediaQuota = errors.New("media quota exceeded")

// errStorageQuota and errStorageFull signal that an upload would push the
// event owner past their per-user storage cap (also returned when approving a
// guest photo would), or the whole bucket past the global ceiling.
// errGuestPendingFull signals that an event's pending guest photos reached
// maxEventPendingGuestBytes. They come from both the non-authoritative
// pre-check and the authoritative check inside the insert transaction.
var (
	errStorageQuota     = errors.New("storage quota exceeded")
	errStorageFull      = errors.New("storage full")
	errGuestPendingFull = errors.New("event pending guest photo allowance exceeded")
)

// errApproveStorageQuota signals that approving a guest photo would push the
// event owner past their per-user storage cap.
var errApproveStorageQuota = errors.New("approval storage quota exceeded")

// errEventNoOwner signals an approval for an event without an owner, which
// would otherwise skip the owner's cap check. Anonymous drafts cannot
// receive guest photos, so this is an invariant violation, not a user error.
var errEventNoOwner = errors.New("event has no owner")

// ownerPendingGuestAllowance bounds the pending (unmoderated) guest photos
// across all of one owner's live events, so many events cannot each park a
// full per-event allowance. It equals the per-user storage cap: an owner
// can never be asked to moderate more than they could keep. Pending photos
// still are not charged to the owner's personal cap.
func (s *Server) ownerPendingGuestAllowance() int64 { return s.cfg.MediaUserQuotaBytes }

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

// checkStorage compares the owner's and the bucket's usage plus add bytes
// against the configured caps. A nil owner (anonymous draft) only counts
// toward the global ceiling. A non-nil guestEventID marks a guest upload: the
// bytes are not charged to the owner's personal cap (they are pending
// moderation) but to that event's pending allowance and, when ownerID names
// the event's owner, to the owner's pending allowance. The unauthenticated
// pre-check passes a nil ownerID and so checks the per-event allowance only.
// Run after LockMediaStorageQuota for the
// authoritative check; with add == 0 it is the cheap pre-check, which refuses
// once a cap is already reached.
func (s *Server) checkStorage(ctx context.Context, q *store.Queries, ownerID, guestEventID *uuid.UUID, add int64) error {
	usage, err := q.MediaStorageUsage(ctx, store.MediaStorageUsageParams{OwnerID: ownerID, EventID: guestEventID})
	if err != nil {
		return fmt.Errorf("media storage usage: %w", err)
	}
	over := func(used, cap int64) bool {
		if add == 0 {
			return used >= cap
		}
		return used+add > cap
	}
	if over(usage.TotalBytes, s.cfg.MediaTotalQuotaBytes) {
		return errStorageFull
	}
	if guestEventID != nil {
		if over(usage.PendingGuestBytes, maxEventPendingGuestBytes) {
			return errGuestPendingFull
		}
		if ownerID != nil && over(usage.OwnerPendingGuestBytes, s.ownerPendingGuestAllowance()) {
			return errGuestPendingFull
		}
		return nil
	}
	if ownerID != nil && over(usage.OwnerBytes, s.cfg.MediaUserQuotaBytes) {
		return errStorageQuota
	}
	return nil
}

// lockAndCheckStorage serialises uploads by every owner on one advisory lock
// (taken after the event row lock, always last, so it cannot deadlock) and
// then runs the authoritative cap check. The lock is released at commit.
func (s *Server) lockAndCheckStorage(ctx context.Context, q *store.Queries, ownerID, guestEventID *uuid.UUID, add int64) error {
	if err := q.LockMediaStorageQuota(ctx); err != nil {
		return fmt.Errorf("lock media storage quota: %w", err)
	}
	return s.checkStorage(ctx, q, ownerID, guestEventID, add)
}

// writeStorageCapError answers errStorageQuota / errStorageFull and reports
// whether err was one of them. Guests are never told about the owner's
// quota; they only learn that the event isn't taking photos.
func (s *Server) writeStorageCapError(w http.ResponseWriter, r *http.Request, err error, guest bool) bool {
	switch {
	case errors.Is(err, errStorageFull):
		slog.WarnContext(r.Context(), "media storage full: upload refused", "request_id", middleware.GetReqID(r.Context()))
		writeError(w, http.StatusInsufficientStorage, "storage_full",
			"Photo storage is full right now, so new uploads are paused. Please try again later.")
	case (errors.Is(err, errStorageQuota) || errors.Is(err, errGuestPendingFull)) && guest:
		writeError(w, http.StatusConflict, "photo_limit", "This event isn't accepting more photos right now.")
	case errors.Is(err, errStorageQuota):
		writeError(w, http.StatusConflict, "storage_quota", fmt.Sprintf(
			"You've used your %d MB of photo storage. Delete some photos or past events to upload more.",
			s.cfg.MediaUserQuotaBytes/1_000_000))
	default:
		return false
	}
	return true
}

// precheckStorage is the fast, non-authoritative storage check made before
// the CPU-heavy pipeline. It reports whether the request may continue; on
// false the response has been written.
func (s *Server) precheckStorage(w http.ResponseWriter, r *http.Request, ownerID, guestEventID *uuid.UUID, guest bool) bool {
	err := s.checkStorage(r.Context(), s.q, ownerID, guestEventID, 0)
	if err == nil {
		return true
	}
	if !s.writeStorageCapError(w, r, err, guest) {
		serverError(w, r, err)
	}
	return false
}

// chargeUploadWrites counts one upload's object-store writes against the
// daily budget. Callers charge it only once the upload has been validated and
// processed, immediately before Commit, so junk requests that fail the cheap
// checks never drain the budget. It reports whether the upload may continue;
// on false the response has been written and the caller must discard the
// processed output.
func (s *Server) chargeUploadWrites(w http.ResponseWriter, r *http.Request) bool {
	ok, err := s.limiter.AllowN(r.Context(), "r2:writes:day", writesPerUpload, mediaWritesPerDay, 24*time.Hour)
	if err != nil {
		serverError(w, r, fmt.Errorf("rate limit: %w", err))
		return false
	}
	if !ok {
		slog.WarnContext(r.Context(), "daily media write budget exhausted", "request_id", middleware.GetReqID(r.Context()))
		w.Header().Set("Retry-After", "3600")
		writeError(w, http.StatusServiceUnavailable, "uploads_paused", "Photo uploads are paused for today. Please try again tomorrow.")
		return false
	}
	return true
}

// insertTxCtx returns the context for an upload's insert transaction: detached
// from the request so a client disconnect can't leave the commit ambiguous,
// but bounded.
func insertTxCtx(r *http.Request) (context.Context, context.CancelFunc) {
	return context.WithTimeout(context.WithoutCancel(r.Context()), uploadTxTimeout)
}

// discardMedia best-effort deletes the objects of an upload whose insert
// failed. Anything it misses is swept by the reconcile job.
func (s *Server) discardMedia(r *http.Request, eventID, mediaID uuid.UUID) {
	ctx, cancel := insertTxCtx(r)
	defer cancel()
	if err := s.media.DeleteMedia(ctx, eventID, mediaID); err != nil {
		slog.WarnContext(ctx, "discard uploaded media failed", "err", err, "media_id", mediaID, "request_id", middleware.GetReqID(ctx))
	}
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
	// Co-hosts are charged to the event owner, anonymous drafts (no owner)
	// only to the global ceiling.
	if !s.precheckStorage(w, r, row.Event.OwnerID, nil, false) {
		return
	}

	body := http.MaxBytesReader(w, r.Body, maxHostUploadBytes)
	outDir, result, ok := s.processUpload(w, r, body)
	if !ok {
		return
	}
	if !s.chargeUploadWrites(w, r) {
		_ = os.RemoveAll(outDir)
		return
	}

	mediaID := uuid.Must(uuid.NewV7())
	if err := s.media.Commit(ctx, outDir, eventID, mediaID); err != nil {
		writeStorageError(w, r, fmt.Errorf("commit media: %w", err))
		return
	}
	storageKey := eventID.String() + "/" + mediaID.String()

	txCtx, cancel := insertTxCtx(r)
	defer cancel()
	var resp mediaResp
	txErr := s.inTx(txCtx, func(_ pgx.Tx, q *store.Queries) error {
		ctx := txCtx
		var ownerID *uuid.UUID
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
			ownerID = lockRow.OwnerID
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
		if err := s.lockAndCheckStorage(ctx, q, ownerID, nil, result.Bytes); err != nil {
			return err
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
		s.discardMedia(r, eventID, mediaID)
		switch {
		case s.writeStorageCapError(w, r, txErr, false):
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
// pending (unapproved) guest photo: GetEventMedia's WHERE (any member) is the
// sole authorization check, since the object store has no notion of
// visibility.
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

	s.serveObject(w, r, func(ctx context.Context, inm string) (*media.Object, error) {
		return s.media.Open(ctx, eventID, mediaID, width, inm)
	}, previewMediaCache, previewReads)
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
	var row store.SetGuestMediaModerationRow
	err = s.inTx(ctx, func(tx pgx.Tx, q *store.Queries) error {
		var err error
		if newStatus == "approved" {
			// Approving charges the photo to the event owner, so serialise
			// with uploads in the usual order: event row, then quota lock.
			// The media row is checked and flipped before the global quota
			// lock so an unknown id answers 404 without ever waiting on it.
			lock, err := q.LockEventForEditor(ctx, store.LockEventForEditorParams{EventID: eventID, UserID: userID})
			if err != nil {
				return err
			}
			if lock.OwnerID == nil {
				return errEventNoOwner
			}
			row, err = q.SetGuestMediaModeration(ctx, store.SetGuestMediaModerationParams{
				Status: newStatus, MediaID: mediaID, EventID: eventID, UserID: userID,
			})
			if err != nil {
				return err
			}
			if err := q.LockMediaStorageQuota(ctx); err != nil {
				return fmt.Errorf("lock media storage quota: %w", err)
			}
			// The row is approved now, so the owner's usage includes it: over
			// the cap means rolling the approval back.
			usage, err := q.MediaOwnerUsage(ctx, *lock.OwnerID)
			if err != nil {
				return fmt.Errorf("media owner usage: %w", err)
			}
			if usage > s.cfg.MediaUserQuotaBytes {
				return errApproveStorageQuota
			}
			return nil
		}
		row, err = q.SetGuestMediaModeration(ctx, store.SetGuestMediaModerationParams{
			Status: newStatus, MediaID: mediaID, EventID: eventID, UserID: userID,
		})
		if err != nil {
			return err
		}
		// Visibility is DB state only, so approving touches no objects. A
		// reject enqueues the durable file deletion in the same tx; the
		// synchronous delete below is just low-latency best effort.
		if _, err := s.jobs.InsertTx(ctx, tx, jobs.MediaVisibilityArgs{EventID: eventID}, nil); err != nil {
			return fmt.Errorf("enqueue media visibility: %w", err)
		}
		return nil
	})
	if errors.Is(err, pgx.ErrNoRows) {
		writeError(w, http.StatusNotFound, "not_found", "No such media.")
		return
	}
	if errors.Is(err, errApproveStorageQuota) {
		writeError(w, http.StatusConflict, "storage_quota",
			"The event owner's photo storage is full, so this photo can't be approved. Free up space or reject it.")
		return
	}
	if err != nil {
		serverError(w, r, fmt.Errorf("set media moderation: %w", err))
		return
	}
	if newStatus == "rejected" && row.OldStatus != "rejected" {
		if err := s.media.DeleteMedia(ctx, eventID, mediaID); err != nil {
			slog.WarnContext(ctx, "delete rejected media files", "err", err, "media_id", mediaID, "request_id", middleware.GetReqID(ctx))
		}
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

	// Files first: a storage failure keeps the row so the host can retry, and
	// 404 for an unknown media item is answered before any storage call.
	if _, err := s.q.GetEventMedia(ctx, store.GetEventMediaParams{MediaID: mediaID, EventID: eventID, UserID: userID}); err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			writeError(w, http.StatusNotFound, "not_found", "No such media.")
			return
		}
		serverError(w, r, fmt.Errorf("get event media: %w", err))
		return
	}
	if err := s.media.DeleteMedia(ctx, eventID, mediaID); err != nil {
		writeStorageError(w, r, fmt.Errorf("delete media files: %w", err))
		return
	}
	if _, err := s.q.DeleteEventMedia(ctx, store.DeleteEventMediaParams{MediaID: mediaID, EventID: eventID, UserID: userID}); err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			writeError(w, http.StatusNotFound, "not_found", "No such media.")
			return
		}
		serverError(w, r, fmt.Errorf("delete event media: %w", err))
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

// --- POST /v1/public/events/{slug}/photos ---

// handleUploadGuestPhoto covers §4.4's public guest-photo upload: it re-runs
// the same visibility gate the public GET enforces, additionally requires an
// invite cookie when rsvp_mode is invite_only, and requires the event's
// guest_photos block to have open=true. Uploads start with
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

	// Pending guest photos are not charged to the owner's cap, only to the
	// event's pending allowance (same query as the global ceiling check); the
	// per-owner pending allowance is enforced only inside the insert tx, to
	// keep this unauthenticated path to one query. It runs after the rate limits so a throttled client costs no storage query.
	if !s.precheckStorage(w, r, nil, &event.ID, true) {
		return
	}

	body := http.MaxBytesReader(w, r.Body, maxGuestUploadBytes)
	outDir, result, ok := s.processUpload(w, r, body)
	if !ok {
		return
	}
	if !s.chargeUploadWrites(w, r) {
		_ = os.RemoveAll(outDir)
		return
	}

	mediaID := uuid.Must(uuid.NewV7())
	if err := s.media.Commit(ctx, outDir, event.ID, mediaID); err != nil {
		writeStorageError(w, r, fmt.Errorf("commit media: %w", err))
		return
	}
	storageKey := event.ID.String() + "/" + mediaID.String()

	txCtx, cancel := insertTxCtx(r)
	defer cancel()
	var moderationStatus string
	txErr := s.inTx(txCtx, func(_ pgx.Tx, q *store.Queries) error {
		ctx := txCtx
		// Locks the event row so the quota check below and this insert are
		// exact under concurrent uploads (§14: "photos" is one of the exact
		// caps), and re-confirms the event is still published.
		locked, err := q.LockEventForRSVP(ctx, event.ID)
		if err != nil {
			return err
		}
		usage, err := q.MediaUsage(ctx, store.MediaUsageParams{EventID: event.ID, UploadedBy: "guest"})
		if err != nil {
			return fmt.Errorf("media usage: %w", err)
		}
		if usage.Files >= maxEventGuestPhotos {
			return errMediaQuota
		}
		if err := s.lockAndCheckStorage(ctx, q, locked.OwnerID, &event.ID, result.Bytes); err != nil {
			return err
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
		s.discardMedia(r, event.ID, mediaID)
		switch {
		case s.writeStorageCapError(w, r, txErr, true):
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
