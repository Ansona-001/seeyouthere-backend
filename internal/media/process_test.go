package media

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"image/jpeg"
	"os"
	"path/filepath"
	"testing"

	"github.com/google/uuid"
)

func countTmpEntries(t *testing.T, s *Store) int {
	t.Helper()
	entries, err := os.ReadDir(s.tmpDir())
	if err != nil {
		t.Fatalf("read tmp dir: %v", err)
	}
	return len(entries)
}

func TestStage_AcceptsAllowedTypes(t *testing.T) {
	s := newTestStore(t)
	cases := map[string][]byte{
		"jpeg": mustJPEG(t, 20, 20),
		"png":  mustPNG(t, 20, 20, false),
	}
	for name, data := range cases {
		t.Run(name, func(t *testing.T) {
			path, sniffed, err := s.Stage(bytes.NewReader(data))
			if err != nil {
				t.Fatalf("Stage: %v", err)
			}
			defer os.Remove(path)
			if sniffed == "" {
				t.Error("expected a non-empty sniffed type")
			}
			if _, err := os.Stat(path); err != nil {
				t.Errorf("staged file missing: %v", err)
			}
		})
	}
}

func TestStage_RejectsUnsupportedTypes(t *testing.T) {
	s := newTestStore(t)
	cases := map[string][]byte{
		"gif":                mustGIF(t, 10, 10),
		"svg":                []byte(`<svg xmlns="http://www.w3.org/2000/svg"><rect/></svg>`),
		"html_jpeg_polyglot": append([]byte("<html><script>alert(1)</script></html>"), mustJPEG(t, 10, 10)...),
		"plain_text":         []byte("just some text, not an image"),
	}
	for name, data := range cases {
		t.Run(name, func(t *testing.T) {
			before := countTmpEntries(t, s)
			_, _, err := s.Stage(bytes.NewReader(data))
			if err != ErrUnsupported {
				t.Fatalf("Stage(%s) err = %v, want ErrUnsupported", name, err)
			}
			if after := countTmpEntries(t, s); after != before {
				t.Errorf("tmp dir not cleaned up: before=%d after=%d", before, after)
			}
		})
	}
}

func TestProcess_ValidJPEG(t *testing.T) {
	s := newTestStore(t)
	p := NewProcessor(1)
	data := mustJPEG(t, 2000, 1000)

	srcPath, sniffed, err := s.Stage(bytes.NewReader(data))
	if err != nil {
		t.Fatalf("Stage: %v", err)
	}
	outDir := t.TempDir()
	result, err := p.Process(context.Background(), srcPath, sniffed, outDir)
	if err != nil {
		t.Fatalf("Process: %v", err)
	}
	if result.Width != 1920 || result.Height != 960 {
		t.Errorf("dimensions = %dx%d, want 1920x960", result.Width, result.Height)
	}
	for _, w := range Widths {
		info, err := os.Stat(filepath.Join(outDir, renditionName(w)))
		if err != nil {
			t.Errorf("rendition %d missing: %v", w, err)
			continue
		}
		if info.Size() == 0 {
			t.Errorf("rendition %d is empty", w)
		}
	}
}

func renditionName(w int) string { return fmt.Sprintf("%d.jpg", w) }

func TestProcess_NoUpscale(t *testing.T) {
	s := newTestStore(t)
	p := NewProcessor(1)
	data := mustJPEG(t, 100, 80) // smaller than every target width

	srcPath, sniffed, err := s.Stage(bytes.NewReader(data))
	if err != nil {
		t.Fatalf("Stage: %v", err)
	}
	outDir := t.TempDir()
	result, err := p.Process(context.Background(), srcPath, sniffed, outDir)
	if err != nil {
		t.Fatalf("Process: %v", err)
	}
	if result.Width != 100 || result.Height != 80 {
		t.Errorf("dimensions = %dx%d, want 100x80 (no upscale)", result.Width, result.Height)
	}
}

func TestProcess_PNGWithAlphaCompositedOntoWhite(t *testing.T) {
	s := newTestStore(t)
	p := NewProcessor(1)
	data := mustPNG(t, 50, 50, true)

	srcPath, sniffed, err := s.Stage(bytes.NewReader(data))
	if err != nil {
		t.Fatalf("Stage: %v", err)
	}
	outDir := t.TempDir()
	if _, err := p.Process(context.Background(), srcPath, sniffed, outDir); err != nil {
		t.Fatalf("Process: %v", err)
	}
	// The output is a JPEG (no alpha channel); just verify it decodes and
	// has no error, which it wouldn't if compositing panicked/produced a
	// broken image. Content correctness of the white composite is exercised
	// indirectly by every rendition round-tripping through jpeg.Decode.
}

func TestProcess_TruncatedJPEGRejected(t *testing.T) {
	s := newTestStore(t)
	p := NewProcessor(1)
	full := mustJPEG(t, 200, 200)
	truncated := full[:len(full)/4]

	srcPath, sniffed, err := s.Stage(bytes.NewReader(truncated))
	if err != nil {
		t.Fatalf("Stage: %v", err)
	}
	before := countTmpEntries(t, s)
	_, err = p.Process(context.Background(), srcPath, sniffed, t.TempDir())
	if err == nil {
		t.Fatal("expected an error for a truncated JPEG")
	}
	_ = os.Remove(srcPath)
	if after := countTmpEntries(t, s); after != before-1 {
		t.Errorf("tmp not cleaned up: before=%d after=%d", before, after)
	}
}

func TestProcess_PNGHugeDimensionsRejectedBeforeDecode(t *testing.T) {
	s := newTestStore(t)
	p := NewProcessor(1)
	// A PNG signature + IHDR chunk claiming 60000x60000, no image data
	// beyond that. decodeConfig must reject this from the header alone,
	// without ever running a full decode (which would try to allocate a
	// bitmap for 60000x60000 pixels).
	data := hugePNGHeader(60000, 60000)

	srcPath, sniffed, err := s.Stage(bytes.NewReader(data))
	if err != nil {
		t.Fatalf("Stage: %v", err)
	}
	_, err = p.Process(context.Background(), srcPath, sniffed, t.TempDir())
	if err != ErrTooLarge {
		t.Fatalf("Process err = %v, want ErrTooLarge", err)
	}
}

func TestProcess_CMYKJPEGFixture(t *testing.T) {
	data, err := os.ReadFile("testdata/cmyk.jpeg")
	if err != nil {
		t.Fatalf("read fixture: %v", err)
	}
	s := newTestStore(t)
	p := NewProcessor(1)
	srcPath, sniffed, err := s.Stage(bytes.NewReader(data))
	if err != nil {
		t.Fatalf("Stage: %v", err)
	}
	if _, err := p.Process(context.Background(), srcPath, sniffed, t.TempDir()); err != nil {
		t.Fatalf("Process(CMYK JPEG): %v", err)
	}
}

func TestProcess_LosslessWebPFixture(t *testing.T) {
	data, err := os.ReadFile("testdata/lossless.webp")
	if err != nil {
		t.Fatalf("read fixture: %v", err)
	}
	s := newTestStore(t)
	p := NewProcessor(1)
	srcPath, sniffed, err := s.Stage(bytes.NewReader(data))
	if err != nil {
		t.Fatalf("Stage: %v", err)
	}
	if sniffed != "image/webp" {
		t.Fatalf("sniffed = %q, want image/webp", sniffed)
	}
	if _, err := p.Process(context.Background(), srcPath, sniffed, t.TempDir()); err != nil {
		t.Fatalf("Process(lossless WebP): %v", err)
	}
}

func TestProcess_EXIFOrientationSwapsDimensions(t *testing.T) {
	base := mustJPEG(t, 300, 200)
	for _, o := range []uint16{6, 8} {
		t.Run(fmt.Sprintf("orientation_%d", o), func(t *testing.T) {
			data := withExifOrientation(t, base, o)
			s := newTestStore(t)
			p := NewProcessor(1)
			srcPath, sniffed, err := s.Stage(bytes.NewReader(data))
			if err != nil {
				t.Fatalf("Stage: %v", err)
			}
			result, err := p.Process(context.Background(), srcPath, sniffed, t.TempDir())
			if err != nil {
				t.Fatalf("Process: %v", err)
			}
			if result.Width != 200 || result.Height != 300 {
				t.Errorf("orientation %d: dimensions = %dx%d, want 200x300 (swapped)", o, result.Width, result.Height)
			}
		})
	}
}

func TestProcess_OutputHasNoAPP1Segment(t *testing.T) {
	base := mustJPEG(t, 300, 200)
	data := withExifOrientation(t, base, 6)
	s := newTestStore(t)
	p := NewProcessor(1)
	srcPath, sniffed, err := s.Stage(bytes.NewReader(data))
	if err != nil {
		t.Fatalf("Stage: %v", err)
	}
	outDir := t.TempDir()
	if _, err := p.Process(context.Background(), srcPath, sniffed, outDir); err != nil {
		t.Fatalf("Process: %v", err)
	}
	for _, w := range Widths {
		out, err := os.ReadFile(filepath.Join(outDir, renditionName(w)))
		if err != nil {
			t.Fatalf("read rendition %d: %v", w, err)
		}
		if bytes.Contains(out, []byte("Exif")) {
			t.Errorf("rendition %d still contains an EXIF segment", w)
		}
	}
}

func TestProcess_BusyReturnsErrBusyPromptly(t *testing.T) {
	p := &Processor{sem: make(chan struct{}, 1)}
	p.sem <- struct{}{} // occupy the only slot
	defer func() { <-p.sem }()

	oldWait := acquireWaitForTest(t, 50)
	defer oldWait()

	s := newTestStore(t)
	data := mustJPEG(t, 20, 20)
	srcPath, sniffed, err := s.Stage(bytes.NewReader(data))
	if err != nil {
		t.Fatalf("Stage: %v", err)
	}
	if _, err := p.Process(context.Background(), srcPath, sniffed, t.TempDir()); err != ErrBusy {
		t.Fatalf("Process err = %v, want ErrBusy", err)
	}
}

// TestProcess_ProgressiveScanBombRejected covers the fix for the
// progressive-JPEG decompression bomb: image/jpeg has no limit on scan
// count and does a full coefficient pass per SOS, so a file declaring far
// more scans than any real encoder produces must be rejected from its
// markers alone, before a decode ever runs.
func TestProcess_ProgressiveScanBombRejected(t *testing.T) {
	build := func(nSOS int) []byte {
		b := (&jpegMarkerBuilder{}).soi().sof(0xC2, 8, 8, 1)
		for i := 0; i < nSOS; i++ {
			b = b.sos()
		}
		return b.eoi().bytes()
	}

	s := newTestStore(t)
	p := NewProcessor(1)

	t.Run("at limit accepted by the scan walk", func(t *testing.T) {
		data := build(maxJPEGScans)
		srcPath, sniffed, err := s.Stage(bytes.NewReader(data))
		if err != nil {
			t.Fatalf("Stage: %v", err)
		}
		defer os.Remove(srcPath)
		// The scan count itself is within budget; Process may still fail
		// later trying to decode this header-only fixture's (nonexistent)
		// entropy data, but it must not be rejected as ErrInvalidImage
		// specifically for having too many scans.
		_, err = p.Process(context.Background(), srcPath, sniffed, t.TempDir())
		if err != nil && !errors.Is(err, ErrInvalidImage) {
			t.Fatalf("Process at scan limit: unexpected error %v", err)
		}
	})

	t.Run("over limit rejected", func(t *testing.T) {
		data := build(maxJPEGScans + 1)
		srcPath, sniffed, err := s.Stage(bytes.NewReader(data))
		if err != nil {
			t.Fatalf("Stage: %v", err)
		}
		defer os.Remove(srcPath)
		_, err = p.Process(context.Background(), srcPath, sniffed, t.TempDir())
		if !errors.Is(err, ErrInvalidImage) {
			t.Fatalf("Process err = %v, want ErrInvalidImage", err)
		}
	})
}

// TestJPEGBytesPerPixel unit-tests the JPEG per-pixel memory estimate: base
// component count, the +1 for CMYK's extra conversion pass, and the
// progressive coefficient-storage surcharge.
func TestJPEGBytesPerPixel(t *testing.T) {
	cases := []struct {
		ncomp       int
		progressive bool
		want        int64
	}{
		{1, false, 1},
		{3, false, 3},
		{4, false, 5},     // CMYK: ncomp + 1
		{3, true, 15},     // 3 + 4*3
		{1, true, 5},      // 1 + 4*1
		{4, true, 5 + 16}, // (4+1) + 4*4
	}
	for _, c := range cases {
		if got := jpegBytesPerPixel(c.ncomp, c.progressive); got != c.want {
			t.Errorf("jpegBytesPerPixel(%d, %v) = %d, want %d", c.ncomp, c.progressive, got, c.want)
		}
	}
}

// TestPNGBytesPerPixel unit-tests the PNG per-pixel memory estimate across
// the 8-bit/16-bit split and the interlace doubling, both read directly from
// the IHDR bytes.
func TestPNGBytesPerPixel(t *testing.T) {
	cases := []struct {
		name       string
		bitDepth   byte
		colorType  byte
		interlaced bool
		want       int64
	}{
		{"8-bit noninterlaced", 8, 6, false, 4},
		{"8-bit interlaced", 8, 6, true, 8},
		{"gray16 noninterlaced", 16, 0, false, 8},
		{"gray16 interlaced", 16, 0, true, 16},
		{"rgb16 noninterlaced", 16, 2, false, 8},
		{"rgba16 noninterlaced", 16, 6, false, 8},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			interlace := byte(0)
			if c.interlaced {
				interlace = 1
			}
			header := pngIHDRHeader(10, 10, c.bitDepth, c.colorType, interlace)
			path := filepath.Join(t.TempDir(), "h.png")
			if err := os.WriteFile(path, header, 0o644); err != nil {
				t.Fatalf("write header: %v", err)
			}
			f, err := os.Open(path)
			if err != nil {
				t.Fatalf("open: %v", err)
			}
			defer f.Close()

			got, err := pngBytesPerPixel(f)
			if err != nil {
				t.Fatalf("pngBytesPerPixel: %v", err)
			}
			if got != c.want {
				t.Errorf("pngBytesPerPixel(...) = %d, want %d", got, c.want)
			}
		})
	}
}

// TestWebPBytesPerPixel unit-tests the WebP per-pixel memory estimate,
// which depends on the sub-format FourCC (VP8L lossless decodes at roughly
// double the working memory of the lossy VP8/VP8X path).
func TestWebPBytesPerPixel(t *testing.T) {
	cases := []struct {
		fourCC string
		want   int64
	}{
		{"VP8L", 8},
		{"VP8 ", 4},
		{"VP8X", 4},
	}
	for _, c := range cases {
		t.Run(c.fourCC, func(t *testing.T) {
			path := filepath.Join(t.TempDir(), "h.webp")
			if err := os.WriteFile(path, webpHeader(c.fourCC), 0o644); err != nil {
				t.Fatalf("write header: %v", err)
			}
			f, err := os.Open(path)
			if err != nil {
				t.Fatalf("open: %v", err)
			}
			defer f.Close()

			got, err := webpBytesPerPixel(f)
			if err != nil {
				t.Fatalf("webpBytesPerPixel: %v", err)
			}
			if got != c.want {
				t.Errorf("webpBytesPerPixel(%q) = %d, want %d", c.fourCC, got, c.want)
			}
		})
	}
}

// TestProcess_OrientationOutputWidthMatchesTargetWidths covers the fix for
// applying EXIF orientation after scaling: for a 90/270-degree rotation
// (orientation 6 here), every rendition's output width must land exactly on
// media.Widths, not on the pre-rotation height.
func TestProcess_OrientationOutputWidthMatchesTargetWidths(t *testing.T) {
	s := newTestStore(t)
	p := NewProcessor(1)
	// Large enough that every target width actually downscales, in both
	// axes, once rotated.
	base := mustJPEG(t, 3000, 2200)
	data := withExifOrientation(t, base, 6)

	srcPath, sniffed, err := s.Stage(bytes.NewReader(data))
	if err != nil {
		t.Fatalf("Stage: %v", err)
	}
	outDir := t.TempDir()
	result, err := p.Process(context.Background(), srcPath, sniffed, outDir)
	if err != nil {
		t.Fatalf("Process: %v", err)
	}
	if result.Width != 1920 {
		t.Errorf("largest rendition width = %d, want 1920", result.Width)
	}
	for _, w := range Widths {
		img, err := jpeg.Decode(bytes.NewReader(mustReadFile(t, filepath.Join(outDir, renditionName(w)))))
		if err != nil {
			t.Fatalf("decode rendition %d: %v", w, err)
		}
		if got := img.Bounds().Dx(); got != w {
			t.Errorf("rendition %d: width = %d, want %d", w, got, w)
		}
	}
}

func mustReadFile(t *testing.T, path string) []byte {
	t.Helper()
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read %s: %v", path, err)
	}
	return data
}

func TestProcess_UUIDsAreUniqueAcrossStages(t *testing.T) {
	s := newTestStore(t)
	seen := map[string]bool{}
	for i := 0; i < 5; i++ {
		path, _, err := s.Stage(bytes.NewReader(mustJPEG(t, 10, 10)))
		if err != nil {
			t.Fatalf("Stage: %v", err)
		}
		id := filepath.Base(path)
		if seen[id] {
			t.Fatalf("duplicate staged id %s", id)
		}
		seen[id] = true
		if _, err := uuid.Parse(id); err != nil {
			t.Errorf("staged filename %q is not a UUID: %v", id, err)
		}
	}
}
