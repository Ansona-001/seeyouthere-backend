package httpapi

import (
	"context"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"regexp"
	"strconv"
	"sync"
	"syscall"
	"time"

	"github.com/go-chi/chi/v5"
	"github.com/go-chi/chi/v5/middleware"
	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"

	"github.com/ansonarose/seeyouthere-backend/internal/media"
	"github.com/ansonarose/seeyouthere-backend/internal/store"
)

// Serving and object-store budgets (r2-design §6). The daily budgets keep
// the free tier of the object store safe: reads are class-B operations,
// writes (three renditions per upload) are class-A.
const (
	mediaViewPerIPLimit = 3000 // per client IP per hour, bounds scraping
	mediaReadsPerDay    = 300000
	// Host and admin previews have their own budget, so public traffic that
	// exhausts mediaReadsPerDay cannot also switch off moderation previews.
	mediaPreviewReadsPerDay = 50000
	mediaWritesPerDay       = 30000
	writesPerUpload         = int64(len(media.Widths))

	// mediaStreams bounds concurrent object streams so slow clients can't pin
	// unbounded connections to the object store on a small host.
	mediaStreams     = 32
	mediaStreamWait  = 5 * time.Second
	mediaStreamLimit = 60 * time.Second // write deadline for one streamed image

	storageRetryAfter = "30"

	publicMediaCache  = "public, max-age=3600"
	previewMediaCache = "private, max-age=300"

	// limiterWarnEvery spaces out the warning logged while the rate limiter is
	// unreachable and media requests fail open.
	limiterWarnEvery = time.Minute

	// uploadTxTimeout bounds the insert transaction of an upload and the best-effort
	// cleanup after a failed one; both run detached from the request context.
	uploadTxTimeout = 10 * time.Second
)

// readBudget is a daily class-B (object read) budget in the rate limiter.
type readBudget struct {
	key string
	max int64
}

var (
	publicReads  = readBudget{"r2:reads:day", mediaReadsPerDay}
	previewReads = readBudget{"r2:reads:preview:day", mediaPreviewReadsPerDay}
)

// ifNoneMatchRE is a single quoted entity tag, optionally weak: no list, no
// "*", no spaces, only printable ASCII other than the quote (RFC 9110 etagc).
var ifNoneMatchRE = regexp.MustCompile(`^(W/)?"[\x21\x23-\x7E]{1,126}"$`)

// copyBufs pools the 32 KiB copy buffers used to stream objects to clients.
var copyBufs = sync.Pool{New: func() any {
	b := make([]byte, 32<<10)
	return &b
}}

// renditionFiles is the closed set of file names the public routes accept.
var renditionFiles = map[string]int{"480.jpg": 480, "1080.jpg": 1080, "1920.jpg": 1920}

// parseCanonicalUUID accepts only the canonical lowercase hyphenated form, so
// every object has exactly one URL.
func parseCanonicalUUID(s string) (uuid.UUID, bool) {
	id, err := uuid.Parse(s)
	if err != nil || id.String() != s {
		return uuid.Nil, false
	}
	return id, true
}

// notFoundFile is the single 404 every non-servable reason gets, so the
// response never reveals whether a media item exists, is pending, rejected or
// taken down.
func notFoundFile(w http.ResponseWriter) {
	writeError(w, http.StatusNotFound, "not_found", "No such file.")
}

// mediaErrorHeaders makes every error response from a /media route
// uncacheable; successful responses overwrite Cache-Control.
func mediaErrorHeaders(w http.ResponseWriter) {
	h := w.Header()
	h.Set("Cache-Control", "no-store")
	h.Set("X-Content-Type-Options", "nosniff")
}

// GET /media/{event_id}/{media_id}/{480|1080|1920}.jpg
//
// Public: anyone with the link can view an approved photo of a live event.
// The DB gate runs before any object-store call, so probing non-servable ids
// costs no storage operations.
func (s *Server) handleMediaFile(w http.ResponseWriter, r *http.Request) {
	mediaErrorHeaders(w)
	eventID, ok1 := parseCanonicalUUID(chi.URLParam(r, "event_id"))
	mediaID, ok2 := parseCanonicalUUID(chi.URLParam(r, "media_id"))
	width, ok3 := renditionFiles[chi.URLParam(r, "file")]
	if !ok1 || !ok2 || !ok3 {
		notFoundFile(w)
		return
	}
	ctx := r.Context()
	if !s.allowView(w, r) {
		return
	}
	if _, err := s.q.GetServableMedia(ctx, store.GetServableMediaParams{MediaID: mediaID, EventID: eventID}); err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			notFoundFile(w)
			return
		}
		serverError(w, r, fmt.Errorf("get servable media: %w", err))
		return
	}
	s.serveObject(w, r, func(ctx context.Context, inm string) (*media.Object, error) {
		return s.media.Open(ctx, eventID, mediaID, width, inm)
	}, publicMediaCache, publicReads)
}

// GET /media/templates/{template_id}/{version}/background/{480|1080|1920}.jpg
func (s *Server) handleTemplateMediaFile(w http.ResponseWriter, r *http.Request) {
	mediaErrorHeaders(w)
	templateID, ok1 := parseCanonicalUUID(chi.URLParam(r, "template_id"))
	width, ok2 := renditionFiles[chi.URLParam(r, "file")]
	rawVersion := chi.URLParam(r, "version")
	v, err := strconv.ParseInt(rawVersion, 10, 32)
	if !ok1 || !ok2 || err != nil || v < 1 || strconv.FormatInt(v, 10) != rawVersion {
		notFoundFile(w)
		return
	}
	version := int32(v)
	ctx := r.Context()
	if !s.allowView(w, r) {
		return
	}
	if _, err := s.q.GetTemplateAssetsPath(ctx, store.GetTemplateAssetsPathParams{TemplateID: templateID, Version: version}); err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			notFoundFile(w)
			return
		}
		serverError(w, r, fmt.Errorf("get template assets path: %w", err))
		return
	}
	s.serveObject(w, r, func(ctx context.Context, inm string) (*media.Object, error) {
		return s.media.OpenTemplateAsset(ctx, templateID, version, width, inm)
	}, publicMediaCache, publicReads)
}

// allowView applies the per-client-IP view limit. A limiter outage fails
// open: serving an image is cheap and the DB gate already ran, so Valkey being
// down must not take every photo down with it.
func (s *Server) allowView(w http.ResponseWriter, r *http.Request) bool {
	ctx := r.Context()
	ok, err := s.limiter.Allow(ctx, "media:view:ip:"+ipRateKey(clientIPFrom(ctx)), mediaViewPerIPLimit, time.Hour)
	if err != nil {
		s.warnLimiterDown(ctx, err)
		return true
	}
	if !ok {
		w.Header().Set("Retry-After", "3600")
		writeError(w, http.StatusTooManyRequests, "rate_limited", "Too many requests. Please wait and try again.")
		return false
	}
	return true
}

// warnLimiterDown logs a rate-limiter failure at most once per
// limiterWarnEvery, however many requests hit it meanwhile.
func (s *Server) warnLimiterDown(ctx context.Context, err error) {
	now := time.Now().UnixNano()
	last := s.limiterWarnAt.Load()
	if now-last < int64(limiterWarnEvery) || !s.limiterWarnAt.CompareAndSwap(last, now) {
		return
	}
	slog.WarnContext(ctx, "media rate limiter unavailable, failing open", "err", err, "request_id", middleware.GetReqID(ctx))
}

// serveObject streams one stored rendition. It is shared by the public
// routes and the host/admin previews; callers have already authorised the
// request. It bounds concurrent streams, charges the given daily read budget
// (so a busy refusal costs nothing), and answers with only headers we control
// (the stored object's own metadata is never reflected).
func (s *Server) serveObject(w http.ResponseWriter, r *http.Request,
	open func(ctx context.Context, ifNoneMatch string) (*media.Object, error), cacheControl string, budget readBudget) {
	ctx := r.Context()

	timer := time.NewTimer(mediaStreamWait)
	defer timer.Stop()
	select {
	case s.mediaSem <- struct{}{}:
		defer func() { <-s.mediaSem }()
	case <-timer.C:
		w.Header().Set("Retry-After", "5")
		writeError(w, http.StatusServiceUnavailable, "busy", "The server is busy. Try again shortly.")
		return
	case <-ctx.Done():
		return
	}

	ok, err := s.limiter.AllowN(ctx, budget.key, 1, budget.max, 24*time.Hour)
	switch {
	case err != nil:
		s.warnLimiterDown(ctx, err) // fail open, see allowView
	case !ok:
		slog.WarnContext(ctx, "daily media read budget exhausted", "budget", budget.key, "request_id", middleware.GetReqID(ctx))
		w.Header().Set("Retry-After", "3600")
		writeError(w, http.StatusServiceUnavailable, "storage_unavailable", "Photos are temporarily unavailable. Please try again later.")
		return
	}

	obj, err := open(ctx, forwardableETag(r.Header.Get("If-None-Match")))
	switch {
	case err == nil:
	case errors.Is(err, media.ErrNotModified):
		// The backend validated the client's ETag; we only need to restate it.
		h := w.Header()
		setMediaHeaders(h, cacheControl)
		if etag := forwardableETag(r.Header.Get("If-None-Match")); etag != "" {
			h.Set("ETag", etag)
		}
		w.WriteHeader(http.StatusNotModified)
		return
	case errors.Is(err, media.ErrNotFound):
		slog.WarnContext(ctx, "media object missing", "path", r.URL.Path, "request_id", middleware.GetReqID(ctx))
		notFoundFile(w)
		return
	default:
		writeStorageError(w, r, err)
		return
	}
	defer obj.Body.Close()

	h := w.Header()
	setMediaHeaders(h, cacheControl)
	if obj.ETag != "" {
		h.Set("ETag", obj.ETag)
	}
	if obj.Size >= 0 {
		h.Set("Content-Length", strconv.FormatInt(obj.Size, 10))
	}
	_ = http.NewResponseController(w).SetWriteDeadline(time.Now().Add(mediaStreamLimit))
	w.WriteHeader(http.StatusOK)

	bp := copyBufs.Get().(*[]byte)
	defer copyBufs.Put(bp)
	if _, err := io.CopyBuffer(w, obj.Body, *bp); err != nil {
		// Headers are out; nothing more can be sent. A client that went away
		// is routine, anything else is a degraded backend worth a warning.
		if ctx.Err() != nil || errors.Is(err, context.Canceled) || errors.Is(err, syscall.EPIPE) || errors.Is(err, syscall.ECONNRESET) {
			slog.DebugContext(ctx, "media stream aborted", "request_id", middleware.GetReqID(ctx))
			return
		}
		slog.WarnContext(ctx, "media stream failed", "err", err, "request_id", middleware.GetReqID(ctx))
	}
}

// setMediaHeaders sets the headers shared by 200 and 304 media responses. The
// content type is always ours, never the stored object's.
func setMediaHeaders(h http.Header, cacheControl string) {
	h.Set("Content-Type", "image/jpeg")
	h.Set("Cache-Control", cacheControl)
	h.Set("X-Content-Type-Options", "nosniff")
	h.Set("Content-Security-Policy", "default-src 'none'")
	h.Set("Cross-Origin-Resource-Policy", "same-site")
}

// forwardableETag returns the client's If-None-Match only when it is exactly
// one quoted entity tag (optionally weak). Lists, "*", unquoted or oversized
// values are dropped, as if the header were absent, so attacker-controlled
// text never reaches the object store.
func forwardableETag(v string) string {
	if !ifNoneMatchRE.MatchString(v) {
		return ""
	}
	return v
}

// writeStorageError maps an object-store failure to 503 storage_unavailable
// with Retry-After. Exhausted retries are expected degradation (Warn); other
// failures such as rejected credentials need a human (Error). A cancelled
// request is routine and only logged at Debug, a timed-out one at Warn.
func writeStorageError(w http.ResponseWriter, r *http.Request, err error) {
	ctx := r.Context()
	switch {
	case errors.Is(err, context.Canceled):
		slog.DebugContext(ctx, "storage call cancelled", "request_id", middleware.GetReqID(ctx))
	case errors.Is(err, context.DeadlineExceeded):
		slog.WarnContext(ctx, "storage call timed out", "err", err, "request_id", middleware.GetReqID(ctx))
	case errors.Is(err, media.ErrStorageUnavailable):
		slog.WarnContext(ctx, "object storage unavailable", "err", err, "request_id", middleware.GetReqID(ctx))
	default:
		slog.ErrorContext(ctx, "object storage failed", "err", err, "request_id", middleware.GetReqID(ctx))
	}
	w.Header().Set("Retry-After", storageRetryAfter)
	writeError(w, http.StatusServiceUnavailable, "storage_unavailable",
		"Photo storage is temporarily unavailable. Please try again in a minute.")
}
