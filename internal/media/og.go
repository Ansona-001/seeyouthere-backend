package media

import (
	"bytes"
	"context"
	"crypto/sha256"
	"fmt"
	"image"
	"image/jpeg"
	"io"

	"github.com/google/uuid"
	xdraw "golang.org/x/image/draw"
)

// The link-preview ("og") image: the picture itself, in its own aspect ratio, shrunk (never enlarged) to fit
// inside 1200x630. Nothing is cropped, padded or stretched.
const (
	OGWidth  = 1200
	OGHeight = 630

	// ogSourceWidth is the stored rendition the card is built from.
	ogSourceWidth = 1080
	// ogMaxSourceBytes and ogMaxSourceHeight bound the decode: the 1080 rendition is our own output, so
	// anything larger means a corrupt object, not a big upload.
	ogMaxSourceBytes  = 12 << 20
	ogMaxSourceHeight = 8000
	ogJPEGQuality     = 85
)

// OpenOG builds the link-preview image for a stored media item from its 1080 px
// rendition and returns it as an in-memory JPEG. The ETag is derived from the
// source object's ETag and size, so a matching ifNoneMatch answers
// ErrNotModified without decoding anything. A missing source is ErrNotFound.
func (s *Store) OpenOG(ctx context.Context, eventID, mediaID uuid.UUID, ifNoneMatch string) (*Object, error) {
	src, err := s.blobs.Get(ctx, mediaKey(eventID, mediaID, ogSourceWidth), "")
	if err != nil {
		return nil, err
	}
	defer src.Body.Close()

	sum := sha256.Sum256([]byte(fmt.Sprintf("og3|%s|%d", src.ETag, src.Size)))
	etag := fmt.Sprintf(`"og-%x"`, sum[:8])
	if ifNoneMatch != "" && ifNoneMatch == etag {
		return nil, ErrNotModified
	}

	raw, err := io.ReadAll(io.LimitReader(src.Body, ogMaxSourceBytes+1))
	if err != nil {
		return nil, fmt.Errorf("read source rendition: %w", err)
	}
	if len(raw) > ogMaxSourceBytes {
		return nil, fmt.Errorf("source rendition too large for og card (%d bytes)", len(raw))
	}
	cfg, err := jpeg.DecodeConfig(bytes.NewReader(raw))
	if err != nil {
		return nil, fmt.Errorf("decode source rendition header: %w", err)
	}
	if cfg.Width < 1 || cfg.Height < 1 || cfg.Width > ogSourceWidth || cfg.Height > ogMaxSourceHeight {
		return nil, fmt.Errorf("source rendition has unexpected size %dx%d", cfg.Width, cfg.Height)
	}
	img, err := jpeg.Decode(bytes.NewReader(raw))
	if err != nil {
		return nil, fmt.Errorf("decode source rendition: %w", err)
	}

	var out bytes.Buffer
	if err := jpeg.Encode(&out, fitOG(img), &jpeg.Options{Quality: ogJPEGQuality}); err != nil {
		return nil, fmt.Errorf("encode og image: %w", err)
	}
	return &Object{Body: io.NopCloser(bytes.NewReader(out.Bytes())), Size: int64(out.Len()), ETag: etag}, nil
}

// fitOG returns src shrunk, if needed, to fit inside OGWidth x OGHeight, keeping its own aspect ratio. A
// picture already inside the box is returned as is: never enlarged, cropped or padded.
func fitOG(src image.Image) image.Image {
	sb := src.Bounds()
	fw, fh := sb.Dx(), sb.Dy()
	if fw > OGWidth {
		fw, fh = OGWidth, OGWidth*sb.Dy()/sb.Dx()
	}
	if fh > OGHeight {
		fw, fh = OGHeight*sb.Dx()/sb.Dy(), OGHeight
	}
	if fw < 1 {
		fw = 1
	}
	if fh < 1 {
		fh = 1
	}
	if fw == sb.Dx() && fh == sb.Dy() {
		return src
	}
	out := image.NewRGBA(image.Rect(0, 0, fw, fh))
	xdraw.CatmullRom.Scale(out, out.Bounds(), src, sb, xdraw.Src, nil)
	return out
}
