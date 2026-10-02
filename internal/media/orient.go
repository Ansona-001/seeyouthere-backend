package media

import "image"

// applyOrientation returns img transformed per the EXIF orientation tag
// (1-8; anything else is treated as 1, no transform). Applied once, after
// scaling, per process.go's comment on why that's cheaper.
func applyOrientation(img *image.RGBA, orientation int) *image.RGBA {
	switch orientation {
	case 2:
		return flipH(img)
	case 3:
		return rotate180(img)
	case 4:
		return flipV(img)
	case 5:
		return rotate90(flipH(img))
	case 6:
		return rotate90(img)
	case 7:
		return rotate270(flipH(img))
	case 8:
		return rotate270(img)
	default:
		return img
	}
}

// rotate90 rotates 90 degrees clockwise.
func rotate90(src *image.RGBA) *image.RGBA {
	b := src.Bounds()
	w, h := b.Dx(), b.Dy()
	dst := image.NewRGBA(image.Rect(0, 0, h, w))
	for y := 0; y < h; y++ {
		for x := 0; x < w; x++ {
			dst.SetRGBA(h-1-y, x, src.RGBAAt(b.Min.X+x, b.Min.Y+y))
		}
	}
	return dst
}

// rotate270 rotates 90 degrees counter-clockwise (270 clockwise).
func rotate270(src *image.RGBA) *image.RGBA {
	b := src.Bounds()
	w, h := b.Dx(), b.Dy()
	dst := image.NewRGBA(image.Rect(0, 0, h, w))
	for y := 0; y < h; y++ {
		for x := 0; x < w; x++ {
			dst.SetRGBA(y, w-1-x, src.RGBAAt(b.Min.X+x, b.Min.Y+y))
		}
	}
	return dst
}

func rotate180(src *image.RGBA) *image.RGBA {
	b := src.Bounds()
	w, h := b.Dx(), b.Dy()
	dst := image.NewRGBA(image.Rect(0, 0, w, h))
	for y := 0; y < h; y++ {
		for x := 0; x < w; x++ {
			dst.SetRGBA(w-1-x, h-1-y, src.RGBAAt(b.Min.X+x, b.Min.Y+y))
		}
	}
	return dst
}

func flipH(src *image.RGBA) *image.RGBA {
	b := src.Bounds()
	w, h := b.Dx(), b.Dy()
	dst := image.NewRGBA(image.Rect(0, 0, w, h))
	for y := 0; y < h; y++ {
		for x := 0; x < w; x++ {
			dst.SetRGBA(w-1-x, y, src.RGBAAt(b.Min.X+x, b.Min.Y+y))
		}
	}
	return dst
}

func flipV(src *image.RGBA) *image.RGBA {
	b := src.Bounds()
	w, h := b.Dx(), b.Dy()
	dst := image.NewRGBA(image.Rect(0, 0, w, h))
	for y := 0; y < h; y++ {
		for x := 0; x < w; x++ {
			dst.SetRGBA(x, h-1-y, src.RGBAAt(b.Min.X+x, b.Min.Y+y))
		}
	}
	return dst
}
