// Package media implements the image upload pipeline: sniffing, bounded
// decode, scaling to three fixed widths, JPEG re-encoding, and on-disk
// storage under MEDIA_ROOT. Every upload is re-encoded from scratch, so no
// original bytes (and no EXIF/GPS metadata, no polyglot payload) are ever
// served back to a browser. Everything CPU/RAM-heavy runs behind a
// process-wide semaphore (see Processor): on a 1 vCPU host, resizing on
// demand or without a concurrency limit is itself a denial-of-service vector.
package media

import "errors"

// Widths are the three renditions written for every image (host media and
// guest photos alike): 480/1080/1920 px wide, named "<w>.jpg".
var Widths = [3]int{480, 1080, 1920}

// Area is one of the three visibility states media files live under.
type Area string

const (
	AreaPublic     Area = "public"
	AreaPending    Area = "pending"
	AreaQuarantine Area = "quarantine"
)

// Result is the outcome of processing one image: the dimensions of the
// largest rendition actually written (never upscaled past the source) and
// the total bytes across all three JPEG files.
type Result struct {
	Width  int
	Height int
	Bytes  int64
}

var (
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
