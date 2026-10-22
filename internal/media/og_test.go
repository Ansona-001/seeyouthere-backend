package media

import (
	"bytes"
	"context"
	"errors"
	"image"
	"image/color"
	"image/jpeg"
	"io"
	"os"
	"path/filepath"
	"testing"
)

func nrgbaAt(img image.Image, x, y int) color.NRGBA {
	return color.NRGBAModel.Convert(img.At(x, y)).(color.NRGBA)
}

// near reports whether each channel of got is within tol of want.
func near(got, want color.NRGBA, tol int) bool {
	d := func(a, b uint8) int {
		if a > b {
			return int(a - b)
		}
		return int(b - a)
	}
	return d(got.R, want.R) <= tol && d(got.G, want.G) <= tol && d(got.B, want.B) <= tol
}

func TestFitOG(t *testing.T) {
	for _, c := range []struct {
		name         string
		w, h         int
		wantW, wantH int
	}{
		{"small portrait is not enlarged", 400, 600, 400, 600},
		{"small landscape is not enlarged", 800, 400, 800, 400},
		{"exact card shape", 1200, 630, 1200, 630},
		{"tall portrait shrinks to the height", 1080, 1620, 420, 630},
		{"wide picture shrinks to the width", 3000, 1000, 1200, 400},
		{"square shrinks to the height", 1000, 1000, 630, 630},
		{"one pixel", 1, 1, 1, 1},
	} {
		t.Run(c.name, func(t *testing.T) {
			got := fitOG(solidImage(c.w, c.h))
			if b := got.Bounds(); b.Dx() != c.wantW || b.Dy() != c.wantH {
				t.Fatalf("size = %dx%d, want %dx%d", b.Dx(), b.Dy(), c.wantW, c.wantH)
			}
		})
	}
}

func TestFitOG_KeepsEveryCorner(t *testing.T) {
	// solidImage has a different colour in each quadrant; none may be cropped away or padded over.
	got := fitOG(solidImage(1080, 1620)) // 420x630
	b := got.Bounds()
	for _, c := range []struct {
		name string
		x, y int
		want color.NRGBA
	}{
		{"top left", 3, 3, color.NRGBA{255, 0, 0, 255}},
		{"top right", b.Dx() - 4, 3, color.NRGBA{0, 255, 0, 255}},
		{"bottom left", 3, b.Dy() - 4, color.NRGBA{0, 0, 255, 255}},
		{"bottom right", b.Dx() - 4, b.Dy() - 4, color.NRGBA{255, 255, 0, 255}},
	} {
		if g := nrgbaAt(got, c.x, c.y); !near(g, c.want, 40) {
			t.Errorf("%s = %v, want about %v", c.name, g, c.want)
		}
	}
}

func commitOGSource(t *testing.T, w, h int) func(inm string) (*Object, error) {
	t.Helper()
	store, _ := newTestStoreBlobs(t)
	ev, md := newIDs()
	outDir := t.TempDir()
	writeRenditions(t, outDir)
	jpg, err := encodeJPEGFixture(w, h)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(outDir, renditionName(1080)), jpg, 0o644); err != nil {
		t.Fatal(err)
	}
	if err := store.Commit(context.Background(), outDir, ev, md); err != nil {
		t.Fatal(err)
	}
	return func(inm string) (*Object, error) {
		return store.OpenOG(context.Background(), ev, md, inm)
	}
}

func TestStore_OpenOG(t *testing.T) {
	open := commitOGSource(t, 540, 810)

	obj, err := open("")
	if err != nil {
		t.Fatalf("OpenOG: %v", err)
	}
	body, _ := io.ReadAll(obj.Body)
	obj.Body.Close()
	if obj.Size != int64(len(body)) || obj.ETag == "" {
		t.Errorf("size %d (body %d), etag %q", obj.Size, len(body), obj.ETag)
	}
	cfg, err := jpeg.DecodeConfig(bytes.NewReader(body))
	if err != nil {
		t.Fatalf("result is not a JPEG: %v", err)
	}
	if cfg.Width != 420 || cfg.Height != 630 { // 540x810 shrunk to the box height
		t.Errorf("image = %dx%d, want 420x630", cfg.Width, cfg.Height)
	}

	// The same source gives the same ETag, and a matching If-None-Match skips the work.
	again, err := open("")
	if err != nil {
		t.Fatal(err)
	}
	again.Body.Close()
	if again.ETag != obj.ETag {
		t.Errorf("etag changed between calls: %q vs %q", again.ETag, obj.ETag)
	}
	if _, err := open(obj.ETag); !errors.Is(err, ErrNotModified) {
		t.Errorf("matching If-None-Match: err = %v, want ErrNotModified", err)
	}
	if o, err := open(`"something-else"`); err != nil {
		t.Errorf("non-matching If-None-Match: %v", err)
	} else {
		o.Body.Close()
	}
}

func TestStore_OpenOG_Errors(t *testing.T) {
	s, _ := newTestStoreBlobs(t)
	ev, md := newIDs()
	if _, err := s.OpenOG(context.Background(), ev, md, ""); !errors.Is(err, ErrNotFound) {
		t.Errorf("missing source: err = %v, want ErrNotFound", err)
	}

	// A source that is not a JPEG, or is oversized, is an error rather than a panic or a blank card.
	for name, size := range map[string][2]int{"too tall": {100, ogMaxSourceHeight + 1}} {
		t.Run(name, func(t *testing.T) {
			open := commitOGSource(t, size[0], size[1])
			if _, err := open(""); err == nil {
				t.Error("expected an error for an out-of-bounds source")
			}
		})
	}
	t.Run("not a jpeg", func(t *testing.T) {
		store, _ := newTestStoreBlobs(t)
		ev, md := newIDs()
		outDir := t.TempDir()
		writeRenditions(t, outDir) // "jpeg-1080" text, not an image
		if err := store.Commit(context.Background(), outDir, ev, md); err != nil {
			t.Fatal(err)
		}
		if _, err := store.OpenOG(context.Background(), ev, md, ""); err == nil {
			t.Error("expected an error for a corrupt source")
		}
	})
}
