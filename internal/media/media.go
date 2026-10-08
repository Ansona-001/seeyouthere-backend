// Package media implements the image upload pipeline: sniffing, bounded
// decode, scaling to three fixed widths, JPEG re-encoding, and storage of
// the renditions as objects (Blobs: a private R2 bucket in production,
// local disk under MEDIA_ROOT/objects otherwise). Staging and processing
// always happen on local disk under MEDIA_ROOT/tmp. Every upload is
// re-encoded from scratch, so no original bytes (and no EXIF/GPS metadata,
// no polyglot payload) are ever served back to a browser. Everything
// CPU/RAM-heavy runs behind a process-wide semaphore (see Processor): on a
// 1 vCPU host, resizing on demand or without a concurrency limit is itself
// a denial-of-service vector.
package media

import (
	"errors"
	"fmt"

	"github.com/google/uuid"
)

// Widths are the three renditions written for every image (host media and
// guest photos alike): 480/1080/1920 px wide, named "<w>.jpg".
var Widths = [3]int{480, 1080, 1920}

// Result is the outcome of processing one image: the dimensions of the
// largest rendition actually written (never upscaled past the source) and
// the total bytes across all three JPEG files.
type Result struct {
	Width  int
	Height int
	Bytes  int64
}

// mediaKey is the object key of one host/guest rendition. Keys are built
// only from parsed UUIDs and a width from Widths, never from request text.
func mediaKey(eventID, mediaID uuid.UUID, width int) string {
	return fmt.Sprintf("e/%s/%s/%d.jpg", eventID, mediaID, width)
}

// templateKey is the object key of one template background rendition.
func templateKey(templateID uuid.UUID, version int32, width int) string {
	return fmt.Sprintf("t/%s/%d/%d.jpg", templateID, version, width)
}

// validWidth reports whether w is one of the stored renditions.
func validWidth(w int) bool {
	for _, x := range Widths {
		if x == w {
			return true
		}
	}
	return false
}

var (
	// ErrNotFound is returned by Blobs.Get and Store.Open when the object
	// does not exist (or the requested width is not a stored rendition).
	ErrNotFound = errors.New("media: object not found")
	// ErrNotModified is returned by Blobs.Get and Store.Open when the
	// caller's If-None-Match matches the object's ETag.
	ErrNotModified = errors.New("media: object not modified")
	// ErrStorageUnavailable is returned (wrapped) when the object store
	// could not be reached or kept failing after retries.
	ErrStorageUnavailable = errors.New("media: storage unavailable")
	// ErrUnsupported is returned when the sniffed content type isn't
	// image/jpeg, image/png or image/webp.
	ErrUnsupported = errors.New("media: unsupported image type")
	// ErrInvalidImage is returned when the bytes can't be decoded as the
	// sniffed type (truncated, corrupt, or a format polyglot).
	ErrInvalidImage = errors.New("media: invalid image")
	// ErrTooLarge is returned when the image's pixel dimensions or
	// decoded-bitmap size exceed the pre-decode budget (decompression-bomb guard).
	ErrTooLarge = errors.New("media: image too large")
	// ErrDiskFull is returned by CheckFree when MEDIA_ROOT has less than
	// the minimum free space required to accept new uploads.
	ErrDiskFull = errors.New("media: disk nearly full")
	// ErrBusy is returned when the processor's semaphore could not be
	// acquired before its wait timeout.
	ErrBusy = errors.New("media: processor busy")
)
