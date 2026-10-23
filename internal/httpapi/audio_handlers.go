package httpapi

import (
	"errors"
	"fmt"
	"log/slog"
	"mime"
	"net/http"
	"os"
	"time"

	"github.com/go-chi/chi/v5/middleware"
	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"

	"github.com/ansonarose/seeyouthere-backend/internal/media"
	"github.com/ansonarose/seeyouthere-backend/internal/store"
)

const (
	// audioBodyMargin is how much larger than media.MaxAudioBytes the raw
	// upload may be: SanitizeMP3 drops a leading ID3v2 tag (up to 1 MiB, often
	// cover art) and a 128-byte ID3v1 tag, and itself caps the stored output
	// at MaxAudioBytes.
	audioBodyMargin = 1<<20 + 256

	audioUploadsPerHour = 10

	// audioUploads bounds concurrent audio uploads (temp file + sanitise);
	// audioUploadWait is how long one waits for a slot before a 503.
	tooLargeAudioMsg = "That file is too large. Please use an MP3 under 5 MB."

	audioUploads    = 2
	audioUploadWait = 3 * time.Second
)

// errAudioForbidden signals that CreateEventAudio found the caller no longer
// an owner or editor of the event (the role check lives in the query).
var errAudioForbidden = errors.New("not an event editor")

type audioResp struct {
	ID         uuid.UUID `json:"id"`
	Src        string    `json:"src"`
	SizeBytes  int64     `json:"size_bytes"`
	DurationMs int32     `json:"duration_ms"`
	CreatedAt  time.Time `json:"created_at"`
}

func audioSrc(eventID, audioID uuid.UUID) string {
	return fmt.Sprintf("/media/%s/%s/audio.mp3", eventID, audioID)
}

func newAudioResp(a store.EventAudio) audioResp {
	return audioResp{
		ID: a.ID, Src: audioSrc(a.EventID, a.ID), SizeBytes: a.SizeBytes,
		DurationMs: a.DurationMs, CreatedAt: a.CreatedAt,
	}
}

// discardAudio best-effort deletes an audio object, on a context detached
// from the request. Anything it misses is swept by the storage reconcile job.
func (s *Server) discardAudio(r *http.Request, eventID, audioID uuid.UUID, msg string) {
	ctx, cancel := insertTxCtx(r)
	defer cancel()
	if err := s.media.DeleteAudio(ctx, eventID, audioID); err != nil {
		slog.WarnContext(ctx, msg, "err", err, "audio_id", audioID, "request_id", middleware.GetReqID(ctx))
	}
}

// --- POST /v1/events/{id}/audio ---

// handleUploadEventAudio stores the event's music track (the raw request body
// is the MP3), replacing any previous one. Files first, DB row second, as for
// photos: the sanitised MP3 is committed to the object store, then one
// transaction swaps the row; any failure afterwards deletes the new object,
// and the replaced object is deleted only after the swap committed. It does
// not change music_source; the host opts in with PATCH.
func (s *Server) handleUploadEventAudio(w http.ResponseWriter, r *http.Request) {
	eventID, err := parseUUIDParam(r, "id")
	if err != nil {
		writeError(w, http.StatusNotFound, "not_found", "No such event.")
		return
	}
	ctx := r.Context()
	userID, authed := userIDFrom(ctx)
	if !authed {
		writeError(w, http.StatusUnauthorized, "unauthorized", "Log in to continue.")
		return
	}
	if !s.allow(w, r, limit{"audio:upload:user:" + userID.String(), audioUploadsPerHour, time.Hour}) {
		return
	}

	row, ok, err := s.loadMemberEvent(ctx, userID, eventID)
	if err != nil {
		serverError(w, r, err)
		return
	}
	if !ok {
		writeError(w, http.StatusNotFound, "not_found", "No such event.")
		return
	}
	if row.Role != "owner" && row.Role != "editor" {
		writeError(w, http.StatusForbidden, "forbidden", "You don't have access to add music.")
		return
	}
	if row.Event.Status == "taken_down" {
		writeError(w, http.StatusConflict, "event_taken_down", "This event has been taken down.")
		return
	}
	if !s.precheckStorage(w, r, row.Event.OwnerID, nil, false) {
		return
	}

	declared, _, err := mime.ParseMediaType(r.Header.Get("Content-Type"))
	if err != nil || declared != mp3ContentType {
		writeError(w, http.StatusUnsupportedMediaType, "unsupported_audio", "Choose an MP3 file.")
		return
	}
	if err := s.media.CheckFree(); err != nil {
		writeError(w, http.StatusInsufficientStorage, "storage_full", "Storage is nearly full. Try again later.")
		return
	}

	timer := time.NewTimer(audioUploadWait)
	defer timer.Stop()
	select {
	case s.audioUploadSem <- struct{}{}:
		defer func() { <-s.audioUploadSem }()
	case <-timer.C:
		w.Header().Set("Retry-After", "5")
		writeError(w, http.StatusServiceUnavailable, "busy", "The server is busy. Try again shortly.")
		return
	case <-ctx.Done():
		return
	}

	tmp, err := s.media.NewTempFile()
	if err != nil {
		serverError(w, r, fmt.Errorf("create audio temp file: %w", err))
		return
	}
	// CommitAudio removes the file itself; this covers every earlier exit.
	defer os.Remove(tmp.Name())
	body := http.MaxBytesReader(w, r.Body, media.MaxAudioBytes+audioBodyMargin)
	res, sanErr := media.SanitizeMP3(body, tmp)
	closeErr := tmp.Close()
	if sanErr != nil {
		var tooBig *http.MaxBytesError
		switch {
		case errors.As(sanErr, &tooBig):
			writeError(w, http.StatusRequestEntityTooLarge, "body_too_large", tooLargeAudioMsg)
		case errors.Is(sanErr, media.ErrAudioTooLarge):
			writeError(w, http.StatusRequestEntityTooLarge, "body_too_large", tooLargeAudioMsg)
		case errors.Is(sanErr, media.ErrAudioTooLong):
			writeError(w, http.StatusUnprocessableEntity, "audio_too_long",
				fmt.Sprintf("That track is longer than %d minutes. Please trim it and try again.", int(media.MaxAudioDuration/time.Minute)))
		case errors.Is(sanErr, media.ErrInvalidAudio):
			writeError(w, http.StatusUnprocessableEntity, "invalid_audio",
				"That doesn't look like a standard MP3 file. Please export it as a standard MP3 and try again. If it has large cover art, remove it first.")
		default:
			serverError(w, r, fmt.Errorf("sanitize audio: %w", sanErr))
		}
		return
	}
	if closeErr != nil {
		serverError(w, r, fmt.Errorf("close audio temp file: %w", closeErr))
		return
	}
	if !s.chargeUploadWrites(w, r) {
		return
	}

	audioID := uuid.Must(uuid.NewV7())
	if err := s.media.CommitAudio(ctx, tmp.Name(), eventID, audioID); err != nil {
		writeStorageError(w, r, fmt.Errorf("commit audio: %w", err))
		return
	}

	txCtx, cancel := insertTxCtx(r)
	defer cancel()
	var (
		created store.EventAudio
		old     *store.DeleteEventAudioByEventRow
	)
	txErr := s.inTx(txCtx, func(_ pgx.Tx, q *store.Queries) error {
		// Serialises this swap against other uploads, deletes and takedowns of
		// the event, so concurrent uploads leave exactly one row.
		lockRow, err := q.LockEventForEditor(txCtx, store.LockEventForEditorParams{EventID: eventID, UserID: userID})
		if err != nil {
			return err
		}
		if lockRow.Status == "taken_down" {
			return errEventTakenDown
		}
		var oldSize int64
		switch prev, err := q.GetEventAudio(txCtx, eventID); {
		case err == nil:
			oldSize = prev.SizeBytes
		case !errors.Is(err, pgx.ErrNoRows):
			return fmt.Errorf("get event audio: %w", err)
		}
		// The old file is deleted in this transaction, so only growth counts.
		if net := res.Bytes - oldSize; net > 0 {
			if err := s.lockAndCheckStorage(txCtx, q, lockRow.OwnerID, nil, net); err != nil {
				return err
			}
		}
		switch d, err := q.DeleteEventAudioByEvent(txCtx, eventID); {
		case err == nil:
			old = &d
		case !errors.Is(err, pgx.ErrNoRows):
			return fmt.Errorf("delete event audio: %w", err)
		}
		created, err = q.CreateEventAudio(txCtx, store.CreateEventAudioParams{
			ID: audioID, EventID: eventID, UserID: userID, SizeBytes: res.Bytes, DurationMs: res.DurationMS,
		})
		if errors.Is(err, pgx.ErrNoRows) {
			return errAudioForbidden
		}
		return err
	})
	if txErr != nil {
		s.discardAudio(r, eventID, audioID, "discard uploaded audio failed")
		var pgErr *pgconn.PgError
		switch {
		case s.writeStorageCapError(w, r, txErr, false):
		case errors.Is(txErr, errEventTakenDown):
			writeError(w, http.StatusConflict, "event_taken_down", "This event has been taken down.")
		case errors.Is(txErr, errAudioForbidden):
			writeError(w, http.StatusForbidden, "forbidden", "You don't have access to add music.")
		case errors.Is(txErr, pgx.ErrNoRows):
			writeError(w, http.StatusNotFound, "not_found", "No such event.")
		case errors.As(txErr, &pgErr) && pgErr.Code == "23505":
			writeError(w, http.StatusConflict, "conflict", "The music was changed at the same time. Please try again.")
		default:
			serverError(w, r, fmt.Errorf("create event audio: %w", txErr))
		}
		return
	}
	if old != nil {
		s.discardAudio(r, eventID, old.ID, "delete replaced audio failed")
	}
	writeJSON(w, http.StatusCreated, map[string]any{"audio": newAudioResp(created)})
}

// --- DELETE /v1/events/{id}/audio ---

// handleDeleteEventAudio removes the event's uploaded track. If the event was
// playing it (music_source 'custom') the source falls back to 'none', not to
// the template's music. The event version is deliberately not bumped: it
// guards content edits, music_source is only changed by this and PATCH, and
// the host UI refetches the event after deleting, so an open editor's next
// save still carries a valid version.
func (s *Server) handleDeleteEventAudio(w http.ResponseWriter, r *http.Request) {
	eventID, err := parseUUIDParam(r, "id")
	if err != nil {
		writeError(w, http.StatusNotFound, "not_found", "No such event.")
		return
	}
	ctx := r.Context()
	userID, authed := userIDFrom(ctx)
	if !authed {
		writeError(w, http.StatusUnauthorized, "unauthorized", "Log in to continue.")
		return
	}
	if !s.allow(w, r, limit{"audio:delete:user:" + userID.String(), 60, time.Hour}) {
		return
	}

	var audioID uuid.UUID
	err = s.inTx(ctx, func(_ pgx.Tx, q *store.Queries) error {
		// LockEventForEditor is the owner/editor check and serialises this
		// with uploads and PATCH.
		if _, err := q.LockEventForEditor(ctx, store.LockEventForEditorParams{EventID: eventID, UserID: userID}); err != nil {
			return err
		}
		a, err := q.GetEventAudio(ctx, eventID)
		if err != nil {
			return err
		}
		audioID = a.ID
		if err := q.ClearCustomMusic(ctx, eventID); err != nil {
			return fmt.Errorf("clear custom music: %w", err)
		}
		_, err = q.DeleteEventAudio(ctx, store.DeleteEventAudioParams{EventID: eventID, AudioID: a.ID, UserID: userID})
		return err
	})
	if errors.Is(err, pgx.ErrNoRows) {
		s.writeAudioDeleteMiss(w, r, eventID, userID)
		return
	}
	if err != nil {
		serverError(w, r, fmt.Errorf("delete event audio: %w", err))
		return
	}
	s.discardAudio(r, eventID, audioID, "delete audio object failed")
	w.WriteHeader(http.StatusNoContent)
}

// writeAudioDeleteMiss tells a member without edit rights (403) from an
// event that is not theirs or has no audio (404).
func (s *Server) writeAudioDeleteMiss(w http.ResponseWriter, r *http.Request, eventID, userID uuid.UUID) {
	row, ok, err := s.loadMemberEvent(r.Context(), userID, eventID)
	switch {
	case err != nil:
		serverError(w, r, err)
	case ok && row.Role != "owner" && row.Role != "editor":
		writeError(w, http.StatusForbidden, "forbidden", "You don't have access to change this event's music.")
	default:
		writeError(w, http.StatusNotFound, "not_found", "No music to delete.")
	}
}
