package media

import (
	"context"
	"fmt"
	"log/slog"
	"os"
	"path/filepath"
	"time"

	"github.com/google/uuid"
)

// minFreeBytes is the free-space floor below which new uploads are refused
// (507 storage_full) so Postgres always keeps operating headroom on the
// same volume/host.
const minFreeBytes = 2 << 30 // 2 GiB

// cleanupTimeout bounds the best-effort delete after a failed Commit, which
// runs on a context detached from the (possibly cancelled) request.
const cleanupTimeout = 10 * time.Second

// Store owns upload staging on local disk and the object keys in Blobs.
// root holds only tmp/ (staged uploads and mid-processing output); the
// renditions themselves live in blobs under:
//
//	e/<event>/<media>/{480,1080,1920}.jpg
//	t/<template>/<version>/{480,1080,1920}.jpg
type Store struct {
	root  string
	blobs Blobs
}

// NewStore creates (if missing) tmp/ under root and verifies root is
// writable.
func NewStore(root string, blobs Blobs) (*Store, error) {
	s := &Store{root: root, blobs: blobs}
	if err := os.MkdirAll(s.tmpDir(), 0o755); err != nil {
		return nil, fmt.Errorf("media: create tmp: %w", err)
	}
	probe := filepath.Join(s.tmpDir(), ".write-check")
	if err := os.WriteFile(probe, []byte("ok"), 0o644); err != nil {
		return nil, fmt.Errorf("media: root not writable: %w", err)
	}
	_ = os.Remove(probe)
	return s, nil
}

func (s *Store) tmpDir() string { return filepath.Join(s.root, "tmp") }

// NewOutputDir creates a fresh, empty directory under tmp/ for
// Processor.Process to write renditions into. Commit consumes it; if the
// caller never commits it (an error before Commit), it must os.RemoveAll it
// itself.
func (s *Store) NewOutputDir() (string, error) {
	dir, err := os.MkdirTemp(s.tmpDir(), "out-")
	if err != nil {
		return "", fmt.Errorf("media: create output dir: %w", err)
	}
	return dir, nil
}

// CheckFree reports ErrDiskFull when the filesystem holding root has less
// than minFreeBytes available.
func (s *Store) CheckFree() error {
	free, err := freeBytes(s.root)
	if err != nil {
		return fmt.Errorf("media: check free space: %w", err)
	}
	if free < minFreeBytes {
		return ErrDiskFull
	}
	return nil
}

// putRenditions uploads the three renditions in outDir one at a time,
// streaming each file, so memory stays at about one copy buffer.
func (s *Store) putRenditions(ctx context.Context, outDir string, key func(width int) string) error {
	for _, w := range Widths {
		if err := s.putFile(ctx, filepath.Join(outDir, fmt.Sprintf("%d.jpg", w)), key(w)); err != nil {
			return err
		}
	}
	return nil
}

func (s *Store) putFile(ctx context.Context, path, key string) error {
	f, err := os.Open(path)
	if err != nil {
		return fmt.Errorf("media: open rendition: %w", err)
	}
	defer f.Close()
	info, err := f.Stat()
	if err != nil {
		return fmt.Errorf("media: stat rendition: %w", err)
	}
	if err := s.blobs.Put(ctx, key, f, info.Size()); err != nil {
		return fmt.Errorf("media: put rendition: %w", err)
	}
	return nil
}

// Commit uploads the renditions in outDir (produced by Processor.Process)
// as eventID/mediaID's objects. It takes ownership of outDir and always
// removes it. If any upload fails, all three keys are deleted best-effort
// (on a context detached from ctx) so no partial media is left behind.
func (s *Store) Commit(ctx context.Context, outDir string, eventID, mediaID uuid.UUID) error {
	defer func() { _ = os.RemoveAll(outDir) }()
	err := s.putRenditions(ctx, outDir, func(w int) string { return mediaKey(eventID, mediaID, w) })
	if err != nil {
		cctx, cancel := context.WithTimeout(context.WithoutCancel(ctx), cleanupTimeout)
		defer cancel()
		_ = s.blobs.Delete(cctx, mediaKeys(eventID, mediaID))
		return err
	}
	return nil
}

// CommitTemplateAsset uploads the renditions in outDir as a template
// version's background, overwriting any existing objects in place (callers
// only do this for unpublished versions), and returns the assets_path stored
// on the row ("templates/<id>/<version>"). It takes ownership of outDir and
// always removes it. A failure part-way may leave a mix of old and new
// renditions; the caller treats the upload as failed and the admin retries.
func (s *Store) CommitTemplateAsset(ctx context.Context, outDir string, templateID uuid.UUID, version int32) (string, error) {
	defer func() { _ = os.RemoveAll(outDir) }()
	err := s.putRenditions(ctx, outDir, func(w int) string { return templateKey(templateID, version, w) })
	if err != nil {
		return "", err
	}
	return fmt.Sprintf("templates/%s/%d", templateID, version), nil
}

// Open returns one stored rendition of a media item. width must be one of
// Widths, otherwise ErrNotFound. ifNoneMatch is forwarded to the backend
// (ErrNotModified on a match). The caller must close Object.Body.
func (s *Store) Open(ctx context.Context, eventID, mediaID uuid.UUID, width int, ifNoneMatch string) (*Object, error) {
	if !validWidth(width) {
		return nil, ErrNotFound
	}
	return s.blobs.Get(ctx, mediaKey(eventID, mediaID, width), ifNoneMatch)
}

// OpenTemplateAsset is Open for a template version's background.
func (s *Store) OpenTemplateAsset(ctx context.Context, templateID uuid.UUID, version int32, width int, ifNoneMatch string) (*Object, error) {
	if !validWidth(width) {
		return nil, ErrNotFound
	}
	return s.blobs.Get(ctx, templateKey(templateID, version, width), ifNoneMatch)
}

func mediaKeys(eventID uuid.UUID, mediaIDs ...uuid.UUID) []string {
	keys := make([]string, 0, len(mediaIDs)*len(Widths))
	for _, id := range mediaIDs {
		for _, w := range Widths {
			keys = append(keys, mediaKey(eventID, id, w))
		}
	}
	return keys
}

// DeleteMedia removes every rendition of the given media items. Missing
// objects are not an error, so it is idempotent and safe to retry.
func (s *Store) DeleteMedia(ctx context.Context, eventID uuid.UUID, mediaIDs ...uuid.UUID) error {
	if len(mediaIDs) == 0 {
		return nil
	}
	if err := s.blobs.Delete(ctx, mediaKeys(eventID, mediaIDs...)); err != nil {
		return fmt.Errorf("media: delete media of event %s: %w", eventID, err)
	}
	return nil
}

// DeleteEventFiles removes every object under the event's prefix, page by
// page, which also sweeps orphans no row knows about. It must run before the
// event row is deleted: once the row is gone nothing else knows the event id
// to clean up (Reconcile is only the safety net). A failure part-way returns
// an error; a retry re-lists and finishes the job.
func (s *Store) DeleteEventFiles(ctx context.Context, eventID uuid.UUID) error {
	var skipped int
	err := s.blobs.List(ctx, fmt.Sprintf("e/%s/", eventID), func(page []ObjectInfo) error {
		keys := make([]string, 0, len(page))
		for _, o := range page {
			// A key the backend can't address would fail the whole batch; skip it.
			if !ValidKey(o.Key) {
				skipped++
				continue
			}
			keys = append(keys, o.Key)
		}
		if len(keys) == 0 {
			return nil
		}
		return s.blobs.Delete(ctx, keys)
	})
	if err != nil {
		return fmt.Errorf("media: delete event files %s: %w", eventID, err)
	}
	if skipped > 0 {
		slog.WarnContext(ctx, "media delete event files skipped invalid keys", "event_id", eventID, "count", skipped)
	}
	return nil
}
