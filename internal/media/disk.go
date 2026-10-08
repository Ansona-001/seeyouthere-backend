package media

import (
	"context"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path"
	"strconv"
	"strings"

	"github.com/google/uuid"
)

// tmpMarker tags in-progress Put files so List and Get never expose them.
const tmpMarker = ".tmp-"

// listPageSize is the largest page List hands to its callback.
const listPageSize = 1000

// DiskBlobs stores objects as files under one directory. All access goes
// through os.Root, so a key can never resolve outside the directory, even
// through symlinks. It is the dev/test backend; production uses R2.
type DiskBlobs struct {
	root *os.Root
}

var _ Blobs = (*DiskBlobs)(nil)

// NewDiskBlobs creates dir if missing and opens it as the object root.
func NewDiskBlobs(dir string) (*DiskBlobs, error) {
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return nil, fmt.Errorf("media: create object dir: %w", err)
	}
	root, err := os.OpenRoot(dir)
	if err != nil {
		return nil, fmt.Errorf("media: open object dir: %w", err)
	}
	return &DiskBlobs{root: root}, nil
}

// Close releases the root directory handle.
func (d *DiskBlobs) Close() error { return d.root.Close() }

func errInvalidKey(key string) error {
	return fmt.Errorf("media: invalid object key %q", key)
}

func diskETag(info fs.FileInfo) string {
	return `"` + strconv.FormatInt(info.Size(), 16) + "-" + strconv.FormatInt(info.ModTime().UnixNano(), 16) + `"`
}

// Put writes body to a temp file next to key and renames it into place, so
// readers see the old object or the new one, never a partial write.
func (d *DiskBlobs) Put(ctx context.Context, key string, body io.ReadSeeker, size int64) error {
	if !ValidKey(key) {
		return errInvalidKey(key)
	}
	if err := ctx.Err(); err != nil {
		return err
	}
	if err := d.root.MkdirAll(path.Dir(key), 0o755); err != nil {
		return fmt.Errorf("media: create object dir: %w", err)
	}
	tmp := key + tmpMarker + uuid.NewString()
	f, err := d.root.OpenFile(tmp, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o644)
	if err != nil {
		return fmt.Errorf("media: create temp object: %w", err)
	}
	n, err := io.Copy(f, io.LimitReader(body, size+1))
	if cerr := f.Close(); err == nil {
		err = cerr
	}
	if err == nil && n != size {
		err = fmt.Errorf("body is %d bytes, want %d", n, size)
	}
	if err == nil {
		err = d.root.Rename(tmp, key)
	}
	if err != nil {
		_ = d.root.Remove(tmp)
		return fmt.Errorf("media: put object: %w", err)
	}
	return nil
}

// Get opens key. The ETag is derived from size and mtime.
func (d *DiskBlobs) Get(ctx context.Context, key, ifNoneMatch string) (*Object, error) {
	if !ValidKey(key) {
		return nil, errInvalidKey(key)
	}
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	f, err := d.root.Open(key)
	if err != nil {
		if errors.Is(err, fs.ErrNotExist) {
			return nil, ErrNotFound
		}
		return nil, fmt.Errorf("media: open object: %w", err)
	}
	info, err := f.Stat()
	if err != nil {
		_ = f.Close()
		return nil, fmt.Errorf("media: stat object: %w", err)
	}
	if info.IsDir() || strings.Contains(key, tmpMarker) {
		_ = f.Close()
		return nil, ErrNotFound
	}
	etag := diskETag(info)
	if ifNoneMatch != "" && ifNoneMatch == etag {
		_ = f.Close()
		return nil, ErrNotModified
	}
	return &Object{Body: f, Size: info.Size(), ETag: etag}, nil
}

// Delete removes keys, ignoring missing ones, and prunes directories left
// empty (best effort).
func (d *DiskBlobs) Delete(ctx context.Context, keys []string) error {
	for _, key := range keys {
		if !ValidKey(key) {
			return errInvalidKey(key)
		}
	}
	for _, key := range keys {
		if err := ctx.Err(); err != nil {
			return err
		}
		if err := d.root.Remove(key); err != nil && !errors.Is(err, fs.ErrNotExist) {
			return fmt.Errorf("media: delete object: %w", err)
		}
		for dir := path.Dir(key); dir != "."; dir = path.Dir(dir) {
			if d.root.Remove(dir) != nil {
				break // not empty, or already gone
			}
		}
	}
	return nil
}

// List walks the keys under prefix in lexical order, in pages of at most
// listPageSize.
func (d *DiskBlobs) List(ctx context.Context, prefix string, fn func([]ObjectInfo) error) error {
	if !ValidPrefix(prefix) {
		return errInvalidKey(prefix)
	}
	start := "."
	if i := strings.LastIndexByte(prefix, '/'); i > 0 {
		start = prefix[:i]
	}
	page := make([]ObjectInfo, 0, listPageSize)
	flush := func() error {
		if len(page) == 0 {
			return nil
		}
		err := fn(page)
		page = page[:0]
		return err
	}
	err := fs.WalkDir(d.root.FS(), start, func(p string, e fs.DirEntry, err error) error {
		if err != nil {
			if errors.Is(err, fs.ErrNotExist) {
				return nil // removed under us (e.g. pruned after a delete)
			}
			return err
		}
		if err := ctx.Err(); err != nil {
			return err
		}
		if e.IsDir() {
			if p != "." && !strings.HasPrefix(p+"/", prefix) && !strings.HasPrefix(prefix, p+"/") {
				return fs.SkipDir
			}
			return nil
		}
		if !strings.HasPrefix(p, prefix) || strings.Contains(p, tmpMarker) {
			return nil
		}
		info, err := e.Info()
		if err != nil {
			if errors.Is(err, fs.ErrNotExist) {
				return nil
			}
			return err
		}
		page = append(page, ObjectInfo{Key: p, Size: info.Size(), LastModified: info.ModTime()})
		if len(page) == listPageSize {
			return flush()
		}
		return nil
	})
	if err != nil {
		return fmt.Errorf("media: list objects: %w", err)
	}
	return flush()
}
