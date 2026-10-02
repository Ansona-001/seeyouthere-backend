package media

import (
	"fmt"
	"os"
	"path/filepath"

	"github.com/google/uuid"
)

// minFreeBytes is the free-space floor below which new uploads are refused
// (507 storage_full) so Postgres always keeps operating headroom on the
// same volume/host.
const minFreeBytes = 2 << 30 // 2 GiB

// Store is the only code that touches paths under MEDIA_ROOT. root layout:
//
//	tmp/                                   staged uploads, mid-processing output
//	public/<event>/<media>/{480,1080,1920}.jpg
//	pending/<event>/<media>/{480,1080,1920}.jpg
//	quarantine/<event>/<media>/{480,1080,1920}.jpg
//	public/templates/<template>/<version>/background/{480,1080,1920}.jpg
type Store struct {
	root string
}

// NewStore creates (if missing) tmp/ and the three Area directories under
// root and verifies root is writable.
func NewStore(root string) (*Store, error) {
	s := &Store{root: root}
	for _, dir := range []string{"tmp", string(AreaPublic), string(AreaPending), string(AreaQuarantine)} {
		if err := os.MkdirAll(filepath.Join(root, dir), 0o755); err != nil {
			return nil, fmt.Errorf("media: create %s: %w", dir, err)
		}
	}
	probe := filepath.Join(s.tmpDir(), ".write-check")
	if err := os.WriteFile(probe, []byte("ok"), 0o644); err != nil {
		return nil, fmt.Errorf("media: root not writable: %w", err)
	}
	_ = os.Remove(probe)
	return s, nil
}

func (s *Store) tmpDir() string { return filepath.Join(s.root, "tmp") }

// NewOutputDir creates a fresh, empty directory under tmp/ (the same
// filesystem as every Area) for Processor.Process to write renditions into.
// Commit later renames it into place; if the caller never commits it (an
// error before or during the DB insert), it must os.RemoveAll it itself.
func (s *Store) NewOutputDir() (string, error) {
	dir, err := os.MkdirTemp(s.tmpDir(), "out-")
	if err != nil {
		return "", fmt.Errorf("media: create output dir: %w", err)
	}
	return dir, nil
}

func (s *Store) areaDir(area Area, eventID, mediaID uuid.UUID) string {
	return filepath.Join(s.root, string(area), eventID.String(), mediaID.String())
}

func (s *Store) templateDir(templateID uuid.UUID, version int32) string {
	return filepath.Join(s.root, string(AreaPublic), "templates", templateID.String(), fmt.Sprint(version), "background")
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

// Commit moves outDir (produced by Processor.Process) into place for
// eventID/mediaID under area, via os.Rename: both are on the same
// filesystem, so this is atomic and near-instant regardless of file size.
func (s *Store) Commit(outDir string, area Area, eventID, mediaID uuid.UUID) error {
	dst := s.areaDir(area, eventID, mediaID)
	if err := os.MkdirAll(filepath.Dir(dst), 0o755); err != nil {
		return fmt.Errorf("media: create event dir: %w", err)
	}
	if err := os.Rename(outDir, dst); err != nil {
		return fmt.Errorf("media: commit: %w", err)
	}
	return nil
}

// CommitTemplateAsset moves outDir into place as a template version's
// background asset and returns the assets_path stored on the row
// (relative to MEDIA_ROOT/public, e.g. "templates/<id>/<version>").
//
// Re-uploading a background to a version that already has one is a normal
// admin workflow, but os.Rename fails on most platforms when dst is a
// non-empty directory. So dst is swapped into place: outDir is first
// staged as a sibling of dst (same filesystem, so the final renames stay
// cheap), any existing dst is displaced to another sibling, the staged
// content is renamed into dst, and only then is the displaced copy removed.
// If no prior asset exists, this degenerates to a plain rename. A failure
// at any point leaves dst as either the old asset or, on success, the new
// one — never missing or half-written.
func (s *Store) CommitTemplateAsset(outDir string, templateID uuid.UUID, version int32) (string, error) {
	dst := s.templateDir(templateID, version)
	if err := os.MkdirAll(filepath.Dir(dst), 0o755); err != nil {
		return "", fmt.Errorf("media: create template dir: %w", err)
	}

	staged := dst + ".staging-" + uuid.NewString()
	if err := os.Rename(outDir, staged); err != nil {
		return "", fmt.Errorf("media: stage template asset: %w", err)
	}

	displaced := dst + ".old-" + uuid.NewString()
	hadExisting := true
	if err := os.Rename(dst, displaced); err != nil {
		if !os.IsNotExist(err) {
			_ = os.RemoveAll(staged)
			return "", fmt.Errorf("media: displace existing template asset: %w", err)
		}
		hadExisting = false
	}

	if err := os.Rename(staged, dst); err != nil {
		// Restore the previous asset so dst is left in its original state,
		// not missing.
		if hadExisting {
			_ = os.Rename(displaced, dst)
		}
		return "", fmt.Errorf("media: commit template asset: %w", err)
	}
	if hadExisting {
		_ = os.RemoveAll(displaced)
	}
	return filepath.ToSlash(filepath.Join("templates", templateID.String(), fmt.Sprint(version))), nil
}

// MoveMedia relocates one media item's directory between areas (used when a
// guest photo is approved/rejected). Missing source is not an error: the
// caller may be retrying after a partial failure.
func (s *Store) MoveMedia(from, to Area, eventID, mediaID uuid.UUID) error {
	src := s.areaDir(from, eventID, mediaID)
	dst := s.areaDir(to, eventID, mediaID)
	return moveDir(src, dst)
}

// MoveEvent relocates an entire event's media directory between areas
// (takedown/restore, account/host delete). It is idempotent: a missing
// source is not an error, and if the destination already has some media for
// this event (e.g. a retried job), files are merged in per media id rather
// than the whole move failing.
func (s *Store) MoveEvent(from, to Area, eventID uuid.UUID) error {
	src := filepath.Join(s.root, string(from), eventID.String())
	dst := filepath.Join(s.root, string(to), eventID.String())

	entries, err := os.ReadDir(src)
	if os.IsNotExist(err) {
		return nil
	}
	if err != nil {
		return fmt.Errorf("media: list event dir: %w", err)
	}
	if err := os.MkdirAll(dst, 0o755); err != nil {
		return fmt.Errorf("media: create event dir: %w", err)
	}
	for _, e := range entries {
		if err := moveDir(filepath.Join(src, e.Name()), filepath.Join(dst, e.Name())); err != nil {
			return err
		}
	}
	_ = os.Remove(src) // only succeeds once empty; harmless otherwise
	return nil
}

// moveDir renames src to dst. A missing src is not an error. If dst already
// exists (a retry, or MoveEvent merging into a non-empty destination), src
// is removed after copying anything dst is missing, rather than failing outright.
func moveDir(src, dst string) error {
	if _, err := os.Stat(src); os.IsNotExist(err) {
		return nil
	}
	if err := os.Rename(src, dst); err == nil {
		return nil
	}
	// Rename failed, most likely because dst already exists. Merge file by
	// file and then remove src.
	entries, err := os.ReadDir(src)
	if err != nil {
		return fmt.Errorf("media: list %s: %w", src, err)
	}
	if err := os.MkdirAll(dst, 0o755); err != nil {
		return fmt.Errorf("media: create %s: %w", dst, err)
	}
	for _, e := range entries {
		s, d := filepath.Join(src, e.Name()), filepath.Join(dst, e.Name())
		if e.IsDir() {
			if err := moveDir(s, d); err != nil {
				return err
			}
			continue
		}
		if err := os.Rename(s, d); err != nil {
			return fmt.Errorf("media: move %s: %w", s, err)
		}
	}
	return os.RemoveAll(src)
}

// DeleteMedia removes a media item's directory from every area. A missing
// directory is not an error.
func (s *Store) DeleteMedia(eventID, mediaID uuid.UUID) error {
	for _, area := range []Area{AreaPublic, AreaPending, AreaQuarantine} {
		if err := os.RemoveAll(s.areaDir(area, eventID, mediaID)); err != nil {
			return fmt.Errorf("media: delete %s: %w", area, err)
		}
	}
	return nil
}

// Open returns the file for one rendition, searching every area (a host or
// admin may need to preview pending/quarantined media). width must be one
// of Widths.
func (s *Store) Open(eventID, mediaID uuid.UUID, width int) (*os.File, error) {
	for _, area := range []Area{AreaPublic, AreaPending, AreaQuarantine} {
		path := filepath.Join(s.areaDir(area, eventID, mediaID), fmt.Sprintf("%d.jpg", width))
		f, err := os.Open(path)
		if err == nil {
			return f, nil
		}
		if !os.IsNotExist(err) {
			return nil, fmt.Errorf("media: open: %w", err)
		}
	}
	return nil, os.ErrNotExist
}
