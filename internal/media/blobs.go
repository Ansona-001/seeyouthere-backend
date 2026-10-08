package media

import (
	"context"
	"io"
	"time"
)

// Blobs is the object storage behind Store: a flat key space of JPEG
// renditions. DiskBlobs (local disk, dev and tests) and the R2 adapter in
// internal/media/r2 implement it. Keys come only from mediaKey/templateKey
// and must satisfy ValidKey; implementations reject anything else.
type Blobs interface {
	// Put stores size bytes from body as image/jpeg, replacing any existing
	// object. body is seekable so a retrying implementation can rewind it.
	Put(ctx context.Context, key string, body io.ReadSeeker, size int64) error
	// Get opens an object. When ifNoneMatch equals the object's ETag it
	// returns ErrNotModified; a missing object is ErrNotFound.
	Get(ctx context.Context, key, ifNoneMatch string) (*Object, error)
	// Delete removes any number of keys (chunking as the backend requires).
	// Missing keys are not an error.
	Delete(ctx context.Context, keys []string) error
	// List calls fn with key-ordered pages of at most 1000 objects whose
	// keys start with prefix. An error from fn stops the listing and is
	// returned.
	List(ctx context.Context, prefix string, fn func([]ObjectInfo) error) error
}

// Object is an open stored object. The caller must close Body.
type Object struct {
	Body io.ReadCloser
	Size int64
	ETag string
}

// ObjectInfo describes one listed object.
type ObjectInfo struct {
	Key          string
	Size         int64
	LastModified time.Time
}

const maxKeyLen = 201

// ValidKey reports whether key is acceptable to every Blobs implementation:
// 1-201 bytes of [a-z0-9/._-], starting with [a-z0-9], with no ".." and no
// "//". It is what makes path traversal impossible on disk and keeps R2
// signing inputs canonical.
func ValidKey(key string) bool {
	if key == "" || len(key) > maxKeyLen || !validKeyChars(key) {
		return false
	}
	c := key[0]
	return c >= 'a' && c <= 'z' || c >= '0' && c <= '9'
}

// ValidPrefix is ValidKey for List prefixes: the same alphabet and
// restrictions, but the empty prefix (everything) is allowed.
func ValidPrefix(prefix string) bool {
	return prefix == "" || ValidKey(prefix)
}

func validKeyChars(s string) bool {
	for i := 0; i < len(s); i++ {
		c := s[i]
		switch {
		case c >= 'a' && c <= 'z', c >= '0' && c <= '9', c == '-', c == '_':
		case c == '/' || c == '.':
			if i > 0 && s[i-1] == c {
				return false // "//" or ".."
			}
		default:
			return false
		}
	}
	return true
}
