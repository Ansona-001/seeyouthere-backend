package media

import (
	"bufio"
	"context"
	"fmt"
	"image"
	"image/color"
	stddraw "image/draw"
	"image/jpeg"
	"image/png"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"time"

	"github.com/google/uuid"
	xdraw "golang.org/x/image/draw"
	"golang.org/x/image/webp"
)

// allowedSniffed is the fixed set of image types this pipeline accepts,
// checked against the sniffed bytes (never the caller-declared Content-Type
// alone: that header is checked by the caller against this same value, but
// the sniff always wins).
var allowedSniffed = map[string]bool{
	"image/jpeg": true,
	"image/png":  true,
	"image/webp": true,
}

// pixelBudget bounds decoded-bitmap size before a full decode runs, so a
// small file with huge declared dimensions (a "decompression bomb") is
// rejected from its header alone. 128 MiB matches the plan's per-image
// memory budget on a 1 vCPU/1-2 GiB host.
const (
	maxDimension = 12000
	pixelBudget  = 128 << 20
	// maxJPEGScans bounds the number of SOS (start-of-scan) segments a
	// progressive JPEG may declare. Go's image/jpeg decoder makes a full
	// coefficient pass per scan and has no limit of its own, so a file with
	// thousands of tiny scans is a CPU decompression bomb even though its
	// declared dimensions look small. 32 comfortably covers every scan
	// pattern encoders produce in practice.
	maxJPEGScans = 32
)

// jpegMeta is what scanJPEGMarkers extracts from a JPEG's markers without
// running a full decode: the component count and encoding mode needed to
// size the pixel budget, having already verified the scan count is sane.
type jpegMeta struct {
	ncomp       int
	progressive bool
}

// isSOFMarker reports whether m is one of the nine actual start-of-frame
// markers (0xC0-0xCF minus DHT/JPG-extension/DAC, which share the range but
// aren't frame headers).
func isSOFMarker(m byte) bool {
	if m < 0xC0 || m > 0xCF {
		return false
	}
	return m != 0xC4 && m != 0xC8 && m != 0xCC
}

// nextRealMarker scans forward for the next JPEG marker, transparently
// skipping byte-stuffed 0xFF00 and restart markers (0xFFD0-0xFFD7): both
// only ever appear inside entropy-coded scan data, so this single function
// works whether the reader is currently between segments or mid-scan.
func nextRealMarker(br *bufio.Reader) (byte, error) {
	for {
		b, err := br.ReadByte()
		if err != nil {
			return 0, err
		}
		if b != 0xFF {
			continue
		}
		var m byte
		for {
			m, err = br.ReadByte()
			if err != nil {
				return 0, err
			}
			if m != 0xFF {
				break
			}
		}
		switch {
		case m == 0x00:
			continue // stuffed 0xFF byte inside entropy data
		case m >= 0xD0 && m <= 0xD7:
			continue // restart marker inside entropy data
		default:
			return m, nil
		}
	}
}

// skipLengthPrefixed discards a standard length-prefixed marker segment
// (the 2-byte length includes itself).
func skipLengthPrefixed(br *bufio.Reader) error {
	hi, err := br.ReadByte()
	if err != nil {
		return err
	}
	lo, err := br.ReadByte()
	if err != nil {
		return err
	}
	length := int(hi)<<8 | int(lo)
	if length < 2 {
		return ErrInvalidImage
	}
	_, err = io.CopyN(io.Discard, br, int64(length-2))
	return err
}

// readSOFComponentCount parses a start-of-frame segment far enough to
// return its component count (1 = grayscale, 3 = YCbCr/RGB, 4 = CMYK/YCCK).
func readSOFComponentCount(br *bufio.Reader) (int, error) {
	hi, err := br.ReadByte()
	if err != nil {
		return 0, err
	}
	lo, err := br.ReadByte()
	if err != nil {
		return 0, err
	}
	length := int(hi)<<8 | int(lo)
	if length < 8 { // 2(len) + 1(precision) + 2(height) + 2(width) + 1(ncomp)
		return 0, ErrInvalidImage
	}
	rest := make([]byte, length-2)
	if _, err := io.ReadFull(br, rest); err != nil {
		return 0, err
	}
	ncomp := int(rest[5])
	if ncomp <= 0 || ncomp > 4 {
		return 0, ErrInvalidImage
	}
	return ncomp, nil
}

// scanJPEGMarkers walks r's markers from SOI to EOI without decoding any
// entropy-coded scan data, counting SOS segments and recording the frame's
// component count and encoding mode (baseline vs. progressive). It rejects
// anything that doesn't parse as well-formed JPEG structure, and any file
// declaring more than maxJPEGScans scans, as ErrInvalidImage: both truncated
// input and a progressive-scan bomb look the same to the caller.
func scanJPEGMarkers(r io.Reader) (jpegMeta, error) {
	br := bufio.NewReaderSize(r, 8192)

	first, err := nextRealMarker(br)
	if err != nil || first != 0xD8 {
		return jpegMeta{}, ErrInvalidImage
	}

	var meta jpegMeta
	nSOS := 0
	for {
		marker, err := nextRealMarker(br)
		if err != nil {
			return jpegMeta{}, ErrInvalidImage
		}
		switch {
		case marker == 0xD9: // EOI
			if meta.ncomp == 0 {
				return jpegMeta{}, ErrInvalidImage
			}
			return meta, nil
		case marker == 0xDA: // SOS
			nSOS++
			if nSOS > maxJPEGScans {
				return jpegMeta{}, ErrInvalidImage
			}
			if err := skipLengthPrefixed(br); err != nil {
				return jpegMeta{}, ErrInvalidImage
			}
		case isSOFMarker(marker):
			ncomp, err := readSOFComponentCount(br)
			if err != nil {
				return jpegMeta{}, ErrInvalidImage
			}
			meta.ncomp = ncomp
			meta.progressive = marker == 0xC2 || marker == 0xC6 || marker == 0xCA
		case marker == 0x01: // TEM, standalone (no length)
		default:
			if err := skipLengthPrefixed(br); err != nil {
				return jpegMeta{}, ErrInvalidImage
			}
		}
	}
}

// jpegBytesPerPixel estimates the decoder's peak per-pixel memory: the
// source component buffers, an extra byte per pixel for image/jpeg's CMYK
// conversion pass, and (for progressive scans) the accumulated per-component
// coefficient storage the decoder keeps across scans.
func jpegBytesPerPixel(ncomp int, progressive bool) int64 {
	bpp := int64(ncomp)
	if ncomp == 4 {
		bpp++
	}
	if progressive {
		bpp += 4 * int64(ncomp)
	}
	return bpp
}

// pngBytesPerPixel reads the IHDR bit depth (file offset 24) and interlace
// method (offset 28) directly, since image/png exposes neither. 16-bit images
// decode to twice the bytes per pixel, and interlaced ones need a second
// buffer for the passes.
func pngBytesPerPixel(f *os.File) (int64, error) {
	var ihdr [5]byte
	if _, err := f.ReadAt(ihdr[:], 24); err != nil {
		return 0, err
	}
	bpp := int64(4)
	if ihdr[0] == 16 {
		bpp = 8
	}
	if ihdr[4] == 1 {
		bpp *= 2
	}
	return bpp, nil
}

// webpBytesPerPixel reads the sub-format FourCC directly from the file
// (offset 12, right after the 12-byte RIFF/WEBP header) since it decides
// which of two very different decode paths (lossless VP8L vs. lossy VP8/
// VP8X) the webp package takes, and lossless decoding needs roughly double
// the working memory of the lossy path.
func webpBytesPerPixel(f *os.File) (int64, error) {
	buf := make([]byte, 4)
	if _, err := f.ReadAt(buf, 12); err != nil {
		return 0, err
	}
	if string(buf) == "VP8L" {
		return 8, nil
	}
	return 4, nil
}

// Stage reads all of r (which the caller must already wrap in an
// http.MaxBytesReader) into a fresh file under MEDIA_ROOT/tmp, sniffing the
// first 512 bytes to identify the image type. The returned tmpPath is on
// the same filesystem as every Area directory, so Commit can rename it
// atomically. On any error the temp file is removed before returning.
func (s *Store) Stage(r io.Reader) (tmpPath, sniffed string, err error) {
	id := uuid.Must(uuid.NewV7())
	tmpPath = filepath.Join(s.tmpDir(), id.String())

	f, err := os.OpenFile(tmpPath, os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0o644)
	if err != nil {
		return "", "", fmt.Errorf("media: create temp file: %w", err)
	}
	remove := func() { _ = os.Remove(tmpPath) }

	head := make([]byte, 512)
	n, readErr := io.ReadFull(r, head)
	if readErr != nil && readErr != io.EOF && readErr != io.ErrUnexpectedEOF {
		f.Close()
		remove()
		return "", "", fmt.Errorf("media: read upload: %w", readErr)
	}
	head = head[:n]
	sniffed = http.DetectContentType(head)
	if !allowedSniffed[sniffed] {
		f.Close()
		remove()
		return "", "", ErrUnsupported
	}

	if _, err := f.Write(head); err != nil {
		f.Close()
		remove()
		return "", "", fmt.Errorf("media: write upload: %w", err)
	}
	if _, err := io.Copy(f, r); err != nil {
		f.Close()
		remove()
		return "", "", fmt.Errorf("media: write upload: %w", err)
	}
	if err := f.Close(); err != nil {
		remove()
		return "", "", fmt.Errorf("media: write upload: %w", err)
	}
	return tmpPath, sniffed, nil
}

// Processor bounds concurrent image decode/scale/encode work: each call to
// Process acquires one slot, waiting up to 20s before giving up with ErrBusy.
type Processor struct {
	sem chan struct{}
}

// NewProcessor returns a Processor allowing at most concurrency images to be
// processed at once.
func NewProcessor(concurrency int) *Processor {
	return &Processor{sem: make(chan struct{}, concurrency)}
}

// acquireWait is how long Process waits for a free semaphore slot before
// reporting ErrBusy (with a Retry-After the caller can act on). A var, not
// a const, so tests can shrink it instead of blocking for the full 20s.
var acquireWait = 20 * time.Second

func (p *Processor) acquire(ctx context.Context) error {
	timer := time.NewTimer(acquireWait)
	defer timer.Stop()
	select {
	case p.sem <- struct{}{}:
		return nil
	case <-timer.C:
		return ErrBusy
	case <-ctx.Done():
		return ctx.Err()
	}
}

func (p *Processor) release() { <-p.sem }

// Process decodes the image at srcPath (whose sniffed type is already
// known), scales it down to (at most) the three Widths, and writes
// "<w>.jpg" for each into outDir, which the caller must create first and
// which Store.Commit later moves into place as a unit. It never upscales:
// a source narrower than a given width is written at its own size under
// that width's filename.
func (p *Processor) Process(ctx context.Context, srcPath, sniffed string, outDir string) (Result, error) {
	f, err := os.Open(srcPath)
	if err != nil {
		return Result{}, fmt.Errorf("media: open source: %w", err)
	}
	defer f.Close()

	cfg, err := decodeConfig(sniffed, f)
	if err != nil {
		return Result{}, fmt.Errorf("%w: %v", ErrInvalidImage, err)
	}
	if cfg.Width <= 0 || cfg.Height <= 0 || cfg.Width > maxDimension || cfg.Height > maxDimension {
		return Result{}, ErrTooLarge
	}

	var bpp int64
	switch sniffed {
	case "image/jpeg":
		// Rejects a progressive-scan bomb (thousands of tiny SOS segments,
		// each a full coefficient pass for image/jpeg) before any decode
		// runs, and supplies the component count/encoding mode the budget
		// below needs.
		if _, err := f.Seek(0, io.SeekStart); err != nil {
			return Result{}, fmt.Errorf("media: rewind source: %w", err)
		}
		meta, err := scanJPEGMarkers(f)
		if err != nil {
			return Result{}, fmt.Errorf("%w: %v", ErrInvalidImage, err)
		}
		bpp = jpegBytesPerPixel(meta.ncomp, meta.progressive)
	case "image/png":
		bpp, err = pngBytesPerPixel(f)
		if err != nil {
			return Result{}, fmt.Errorf("%w: %v", ErrInvalidImage, err)
		}
	case "image/webp":
		bpp, err = webpBytesPerPixel(f)
		if err != nil {
			return Result{}, fmt.Errorf("%w: %v", ErrInvalidImage, err)
		}
	default:
		return Result{}, ErrUnsupported
	}
	if int64(cfg.Width)*int64(cfg.Height)*bpp > pixelBudget {
		return Result{}, ErrTooLarge
	}

	if err := p.acquire(ctx); err != nil {
		return Result{}, err
	}
	defer p.release()

	if _, err := f.Seek(0, io.SeekStart); err != nil {
		return Result{}, fmt.Errorf("media: rewind source: %w", err)
	}
	img, err := decodeImage(sniffed, f)
	if err != nil {
		return Result{}, fmt.Errorf("%w: %v", ErrInvalidImage, err)
	}

	orientation := 1
	if sniffed == "image/jpeg" {
		head := make([]byte, 65536)
		if _, err := f.Seek(0, io.SeekStart); err == nil {
			n, _ := io.ReadFull(f, head)
			orientation = exifOrientation(head[:n])
		}
	}

	// Widths is smallest-to-largest; walk it largest-to-smallest so each
	// rendition is scaled from the previous (larger) one instead of the
	// full-size original. Orientation is corrected once, right after the
	// first (largest) pass: rotating the ~1920px image is far cheaper than
	// rotating the full-size decode, and every smaller rendition then
	// inherits the correction by descending from it.
	var totalBytes int64
	var result Result
	current := img
	for i := len(Widths) - 1; i >= 0; i-- {
		w := Widths[i]
		var rendition *image.RGBA
		if i == len(Widths)-1 {
			// A 90/270-degree rotation (orientation 5-8) swaps width and
			// height, so scaling to w BEFORE rotating would leave the
			// rotated output w pixels tall, not wide. Scale by the target
			// height instead so the post-rotation width lands on w, same
			// as every non-rotated rendition.
			if orientation >= 5 {
				rendition = scaleDownToHeight(current, w)
			} else {
				rendition = scaleDown(current, w)
			}
			rendition = applyOrientation(rendition, orientation)
			result.Width, result.Height = rendition.Bounds().Dx(), rendition.Bounds().Dy()
		} else {
			rendition = scaleDown(current, w)
		}
		current = rendition
		n, err := encodeJPEG(filepath.Join(outDir, fmt.Sprintf("%d.jpg", w)), rendition)
		if err != nil {
			return Result{}, err
		}
		totalBytes += n
	}
	result.Bytes = totalBytes
	return result, nil
}

func decodeConfig(sniffed string, r io.Reader) (image.Config, error) {
	switch sniffed {
	case "image/jpeg":
		return jpeg.DecodeConfig(r)
	case "image/png":
		return png.DecodeConfig(r)
	case "image/webp":
		return webp.DecodeConfig(r)
	default:
		return image.Config{}, ErrUnsupported
	}
}

func decodeImage(sniffed string, r io.Reader) (image.Image, error) {
	switch sniffed {
	case "image/jpeg":
		return jpeg.Decode(r)
	case "image/png":
		return png.Decode(r)
	case "image/webp":
		return webp.Decode(r)
	default:
		return nil, ErrUnsupported
	}
}

// scaleDown returns img scaled to targetW wide, never upscaling. Alpha (from
// PNG/WebP) is composited onto an opaque white background as part of the
// same pass, since the JPEG output has no alpha channel of its own. Sources
// more than 4x the target are pre-shrunk with a cheap filter before the
// higher-quality (and much slower) CatmullRom pass.
func scaleDown(img image.Image, targetW int) *image.RGBA {
	srcW := img.Bounds().Dx()
	if targetW <= 0 || targetW > srcW {
		targetW = srcW
	}
	if srcW > targetW*4 {
		img = compositeResize(img, targetW*2, xdraw.ApproxBiLinear)
	}
	return compositeResize(img, targetW, xdraw.CatmullRom)
}

func compositeResize(img image.Image, targetW int, interp xdraw.Interpolator) *image.RGBA {
	b := img.Bounds()
	srcW, srcH := b.Dx(), b.Dy()
	if targetW <= 0 || targetW > srcW {
		targetW = srcW
	}
	targetH := int(float64(srcH)*float64(targetW)/float64(srcW) + 0.5)
	if targetH < 1 {
		targetH = 1
	}
	dst := image.NewRGBA(image.Rect(0, 0, targetW, targetH))
	stddraw.Draw(dst, dst.Bounds(), image.NewUniform(color.White), image.Point{}, stddraw.Src)
	interp.Scale(dst, dst.Bounds(), img, b, xdraw.Over, nil)
	return dst
}

// scaleDownToHeight is scaleDown's mirror image: it bounds the target
// dimension by height instead of width. Used only for the largest rendition
// when a 90/270-degree EXIF rotation is about to swap the axes, so the
// rotated result's width lands on the intended target width.
func scaleDownToHeight(img image.Image, targetH int) *image.RGBA {
	srcH := img.Bounds().Dy()
	if targetH <= 0 || targetH > srcH {
		targetH = srcH
	}
	if srcH > targetH*4 {
		img = compositeResizeHeight(img, targetH*2, xdraw.ApproxBiLinear)
	}
	return compositeResizeHeight(img, targetH, xdraw.CatmullRom)
}

func compositeResizeHeight(img image.Image, targetH int, interp xdraw.Interpolator) *image.RGBA {
	b := img.Bounds()
	srcW, srcH := b.Dx(), b.Dy()
	if targetH <= 0 || targetH > srcH {
		targetH = srcH
	}
	targetW := int(float64(srcW)*float64(targetH)/float64(srcH) + 0.5)
	if targetW < 1 {
		targetW = 1
	}
	dst := image.NewRGBA(image.Rect(0, 0, targetW, targetH))
	stddraw.Draw(dst, dst.Bounds(), image.NewUniform(color.White), image.Point{}, stddraw.Src)
	interp.Scale(dst, dst.Bounds(), img, b, xdraw.Over, nil)
	return dst
}

const jpegQuality = 82

func encodeJPEG(path string, img image.Image) (int64, error) {
	f, err := os.OpenFile(path, os.O_CREATE|os.O_TRUNC|os.O_WRONLY, 0o644)
	if err != nil {
		return 0, fmt.Errorf("media: create rendition: %w", err)
	}
	defer f.Close()
	if err := jpeg.Encode(f, img, &jpeg.Options{Quality: jpegQuality}); err != nil {
		return 0, fmt.Errorf("media: encode rendition: %w", err)
	}
	info, err := f.Stat()
	if err != nil {
		return 0, fmt.Errorf("media: stat rendition: %w", err)
	}
	return info.Size(), nil
}
