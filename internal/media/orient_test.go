package media

import (
	"image"
	"image/color"
	"testing"
)

// cornerImage returns a 2x2 image with a distinct colour in each corner,
// named for its position, so a rotation/flip can be checked by asking
// "which named colour is now at (0,0)?".
func cornerImage() *image.RGBA {
	img := image.NewRGBA(image.Rect(0, 0, 2, 2))
	img.Set(0, 0, color.RGBA{1, 0, 0, 255}) // top-left
	img.Set(1, 0, color.RGBA{2, 0, 0, 255}) // top-right
	img.Set(0, 1, color.RGBA{3, 0, 0, 255}) // bottom-left
	img.Set(1, 1, color.RGBA{4, 0, 0, 255}) // bottom-right
	return img
}

func at(img *image.RGBA, x, y int) uint8 { return img.RGBAAt(x, y).R }

func TestFlipH(t *testing.T) {
	out := flipH(cornerImage())
	if at(out, 0, 0) != 2 || at(out, 1, 0) != 1 || at(out, 0, 1) != 4 || at(out, 1, 1) != 3 {
		t.Errorf("flipH corners wrong: %d %d / %d %d", at(out, 0, 0), at(out, 1, 0), at(out, 0, 1), at(out, 1, 1))
	}
}

func TestFlipV(t *testing.T) {
	out := flipV(cornerImage())
	if at(out, 0, 0) != 3 || at(out, 1, 0) != 4 || at(out, 0, 1) != 1 || at(out, 1, 1) != 2 {
		t.Errorf("flipV corners wrong: %d %d / %d %d", at(out, 0, 0), at(out, 1, 0), at(out, 0, 1), at(out, 1, 1))
	}
}

func TestRotate180(t *testing.T) {
	out := rotate180(cornerImage())
	if at(out, 0, 0) != 4 || at(out, 1, 0) != 3 || at(out, 0, 1) != 2 || at(out, 1, 1) != 1 {
		t.Errorf("rotate180 corners wrong: %d %d / %d %d", at(out, 0, 0), at(out, 1, 0), at(out, 0, 1), at(out, 1, 1))
	}
}

func TestRotate90_ClockwiseSwapsDimensionsAndCorners(t *testing.T) {
	// A wide (3x2) image rotated 90 clockwise becomes tall (2x3): the
	// original bottom-left pixel ends up at the new top-left.
	src := image.NewRGBA(image.Rect(0, 0, 3, 2))
	src.Set(0, 1, color.RGBA{9, 0, 0, 255}) // bottom-left
	out := rotate90(src)
	if out.Bounds().Dx() != 2 || out.Bounds().Dy() != 3 {
		t.Fatalf("rotate90 size = %v, want 2x3", out.Bounds())
	}
	if at(out, 0, 0) != 9 {
		t.Errorf("rotate90: original bottom-left should land at new top-left, got %d", at(out, 0, 0))
	}
}

func TestRotate270_CounterClockwise(t *testing.T) {
	src := image.NewRGBA(image.Rect(0, 0, 3, 2))
	src.Set(2, 0, color.RGBA{7, 0, 0, 255}) // top-right
	out := rotate270(src)
	if out.Bounds().Dx() != 2 || out.Bounds().Dy() != 3 {
		t.Fatalf("rotate270 size = %v, want 2x3", out.Bounds())
	}
	if at(out, 0, 0) != 7 {
		t.Errorf("rotate270: original top-right should land at new top-left, got %d", at(out, 0, 0))
	}
}

func TestApplyOrientation_IdentityForUnknown(t *testing.T) {
	src := cornerImage()
	for _, o := range []int{0, 1, 9, -1} {
		if applyOrientation(src, o) != src {
			t.Errorf("orientation %d should be a no-op", o)
		}
	}
}

func TestApplyOrientation_AllCasesRun(t *testing.T) {
	// Every defined case (2-8) must run without panicking and must
	// produce an image whose pixel count matches the source's.
	src := cornerImage()
	for o := 2; o <= 8; o++ {
		out := applyOrientation(src, o)
		if out.Bounds().Dx()*out.Bounds().Dy() != src.Bounds().Dx()*src.Bounds().Dy() {
			t.Errorf("orientation %d changed pixel count", o)
		}
	}
}
