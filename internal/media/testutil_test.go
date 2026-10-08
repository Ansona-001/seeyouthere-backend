package media

import (
	"bytes"
	"context"
	"encoding/binary"
	"fmt"
	"hash/crc32"
	"image"
	"image/color"
	"image/gif"
	"image/jpeg"
	"image/png"
	"io"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"
)

// solidImage returns a w x h image where each quadrant has a distinct
// colour, so orientation transforms can be checked by corner colour.
func solidImage(w, h int) *image.NRGBA {
	img := image.NewNRGBA(image.Rect(0, 0, w, h))
	tl, tr := color.NRGBA{255, 0, 0, 255}, color.NRGBA{0, 255, 0, 255}
	bl, br := color.NRGBA{0, 0, 255, 255}, color.NRGBA{255, 255, 0, 255}
	for y := 0; y < h; y++ {
		for x := 0; x < w; x++ {
			switch {
			case x < w/2 && y < h/2:
				img.Set(x, y, tl)
			case x >= w/2 && y < h/2:
				img.Set(x, y, tr)
			case x < w/2 && y >= h/2:
				img.Set(x, y, bl)
			default:
				img.Set(x, y, br)
			}
		}
	}
	return img
}

func encodeJPEGFixture(w, h int) ([]byte, error) {
	var buf bytes.Buffer
	if err := jpeg.Encode(&buf, solidImage(w, h), &jpeg.Options{Quality: 90}); err != nil {
		return nil, err
	}
	return buf.Bytes(), nil
}

func mustJPEG(t *testing.T, w, h int) []byte {
	t.Helper()
	data, err := encodeJPEGFixture(w, h)
	if err != nil {
		t.Fatalf("encode jpeg fixture: %v", err)
	}
	return data
}

func mustPNG(t *testing.T, w, h int, alpha bool) []byte {
	t.Helper()
	img := image.NewNRGBA(image.Rect(0, 0, w, h))
	for y := 0; y < h; y++ {
		for x := 0; x < w; x++ {
			a := uint8(255)
			if alpha {
				a = uint8((x + y) % 256)
			}
			img.Set(x, y, color.NRGBA{uint8(x % 256), uint8(y % 256), 128, a})
		}
	}
	var buf bytes.Buffer
	if err := png.Encode(&buf, img); err != nil {
		t.Fatalf("encode png fixture: %v", err)
	}
	return buf.Bytes()
}

func mustGIF(t *testing.T, w, h int) []byte {
	t.Helper()
	pal := color.Palette{color.White, color.Black}
	img := image.NewPaletted(image.Rect(0, 0, w, h), pal)
	var buf bytes.Buffer
	if err := gif.Encode(&buf, img, nil); err != nil {
		t.Fatalf("encode gif fixture: %v", err)
	}
	return buf.Bytes()
}

// exifApp1Segment builds a minimal APP1/EXIF segment carrying only the
// orientation tag (0x0112), matching what exifOrientation parses.
func exifApp1Segment(orientation uint16) []byte {
	payload := []byte("Exif\x00\x00")
	payload = append(payload, 'I', 'I') // little-endian TIFF
	payload = append(payload, 0x2A, 0x00)
	payload = append(payload, 0x08, 0x00, 0x00, 0x00) // IFD0 offset = 8
	payload = append(payload, 0x01, 0x00)             // 1 entry
	payload = append(payload, 0x12, 0x01)             // tag 0x0112
	payload = append(payload, 0x03, 0x00)             // type SHORT
	payload = append(payload, 0x01, 0x00, 0x00, 0x00) // count 1
	val := make([]byte, 4)
	binary.LittleEndian.PutUint16(val, orientation)
	payload = append(payload, val...)
	payload = append(payload, 0x00, 0x00, 0x00, 0x00) // next IFD = none

	segLen := len(payload) + 2
	seg := []byte{0xFF, 0xE1, byte(segLen >> 8), byte(segLen & 0xFF)}
	return append(seg, payload...)
}

// withExifOrientation inserts an EXIF orientation APP1 segment right after
// a JPEG's SOI marker.
func withExifOrientation(t *testing.T, jpegBytes []byte, orientation uint16) []byte {
	t.Helper()
	if len(jpegBytes) < 2 || jpegBytes[0] != 0xFF || jpegBytes[1] != 0xD8 {
		t.Fatal("not a JPEG (missing SOI marker)")
	}
	seg := exifApp1Segment(orientation)
	out := make([]byte, 0, len(jpegBytes)+len(seg))
	out = append(out, jpegBytes[:2]...)
	out = append(out, seg...)
	out = append(out, jpegBytes[2:]...)
	return out
}

// hugePNGHeader builds a valid PNG signature + IHDR chunk claiming w x h
// pixels, with no further chunks. png.DecodeConfig reads only the IHDR, so
// this lets the pixel-budget check be tested without allocating a real
// 60000x60000 bitmap.
func hugePNGHeader(w, h int) []byte {
	sig := []byte{0x89, 0x50, 0x4E, 0x47, 0x0D, 0x0A, 0x1A, 0x0A}
	data := make([]byte, 13)
	binary.BigEndian.PutUint32(data[0:4], uint32(w))
	binary.BigEndian.PutUint32(data[4:8], uint32(h))
	data[8] = 8  // bit depth
	data[9] = 2  // color type: truecolor (RGB)
	data[10] = 0 // compression method
	data[11] = 0 // filter method
	data[12] = 0 // interlace method

	chunkType := []byte("IHDR")
	crc := crc32.ChecksumIEEE(append(append([]byte{}, chunkType...), data...))

	var buf bytes.Buffer
	buf.Write(sig)
	length := make([]byte, 4)
	binary.BigEndian.PutUint32(length, uint32(len(data)))
	buf.Write(length)
	buf.Write(chunkType)
	buf.Write(data)
	crcBytes := make([]byte, 4)
	binary.BigEndian.PutUint32(crcBytes, crc)
	buf.Write(crcBytes)
	return buf.Bytes()
}

// pngIHDRHeader is hugePNGHeader generalized to a configurable color type,
// bit depth and interlace method, for exercising the pixel-budget math for
// 16-bit and interlaced PNGs without needing a full valid image body.
func pngIHDRHeader(w, h int, bitDepth, colorType, interlace byte) []byte {
	sig := []byte{0x89, 0x50, 0x4E, 0x47, 0x0D, 0x0A, 0x1A, 0x0A}
	data := make([]byte, 13)
	binary.BigEndian.PutUint32(data[0:4], uint32(w))
	binary.BigEndian.PutUint32(data[4:8], uint32(h))
	data[8] = bitDepth
	data[9] = colorType
	data[10] = 0 // compression method
	data[11] = 0 // filter method
	data[12] = interlace

	chunkType := []byte("IHDR")
	crc := crc32.ChecksumIEEE(append(append([]byte{}, chunkType...), data...))

	var buf bytes.Buffer
	buf.Write(sig)
	length := make([]byte, 4)
	binary.BigEndian.PutUint32(length, uint32(len(data)))
	buf.Write(length)
	buf.Write(chunkType)
	buf.Write(data)
	crcBytes := make([]byte, 4)
	binary.BigEndian.PutUint32(crcBytes, crc)
	buf.Write(crcBytes)
	return buf.Bytes()
}

// webpHeader builds a minimal RIFF/WEBP container with the given
// sub-format FourCC at the fixed offset webpBytesPerPixel reads.
func webpHeader(fourCC string) []byte {
	buf := make([]byte, 20)
	copy(buf[0:4], "RIFF")
	binary.LittleEndian.PutUint32(buf[4:8], 12)
	copy(buf[8:12], "WEBP")
	copy(buf[12:16], fourCC)
	return buf
}

// jpegMarkerBuilder assembles a synthetic JPEG byte-for-byte from its
// markers, for tests that need precise control over marker structure
// (scan count, SOF type) without a real encoder in the loop.
type jpegMarkerBuilder struct {
	buf bytes.Buffer
}

func (b *jpegMarkerBuilder) soi() *jpegMarkerBuilder {
	b.buf.Write([]byte{0xFF, 0xD8})
	return b
}

func (b *jpegMarkerBuilder) eoi() *jpegMarkerBuilder {
	b.buf.Write([]byte{0xFF, 0xD9})
	return b
}

// sof writes a start-of-frame segment (marker e.g. 0xC0 baseline, 0xC2
// progressive) for an ncomp-component image of the given dimensions.
func (b *jpegMarkerBuilder) sof(marker byte, w, h, ncomp int) *jpegMarkerBuilder {
	body := []byte{8, byte(h >> 8), byte(h), byte(w >> 8), byte(w), byte(ncomp)}
	for i := 0; i < ncomp; i++ {
		body = append(body, byte(i+1), 0x11, 0x00)
	}
	length := len(body) + 2
	b.buf.Write([]byte{0xFF, marker, byte(length >> 8), byte(length)})
	b.buf.Write(body)
	return b
}

// sos writes a single-component start-of-scan header with empty entropy
// data (the next bytes written are whatever marker follows).
func (b *jpegMarkerBuilder) sos() *jpegMarkerBuilder {
	body := []byte{1, 1, 0, 0, 0x3F, 0}
	length := len(body) + 2
	b.buf.Write([]byte{0xFF, 0xDA, byte(length >> 8), byte(length)})
	b.buf.Write(body)
	return b
}

func (b *jpegMarkerBuilder) bytes() []byte { return b.buf.Bytes() }

// acquireWaitForTest shrinks the semaphore-acquire timeout for the duration
// of a test, restoring it on return.
func acquireWaitForTest(t *testing.T, ms int) func() {
	t.Helper()
	old := acquireWait
	acquireWait = time.Duration(ms) * time.Millisecond
	return func() { acquireWait = old }
}

func newTestStore(t *testing.T) *Store {
	t.Helper()
	s, _ := newTestStoreBlobs(t)
	return s
}

// newTestStoreBlobs returns a Store backed by an in-memory fake, plus the
// fake for call counters, failure injection and direct inspection.
func newTestStoreBlobs(t *testing.T) (*Store, *memBlobs) {
	t.Helper()
	b := newMemBlobs()
	s, err := NewStore(t.TempDir(), b)
	if err != nil {
		t.Fatalf("NewStore: %v", err)
	}
	return s, b
}

// memBlobs is an in-memory Blobs with call counters and failure injection.
type memBlobs struct {
	mu       sync.Mutex
	objs     map[string]memObj
	pageSize int

	puts, gets, deletes, lists int // call counts
	deleteBatches              [][]string

	putErr    func(key string) error // returned instead of storing, when non-nil
	getErr    error
	deleteErr error
	listErr   error
}

type memObj struct {
	data []byte
	mod  time.Time
}

func newMemBlobs() *memBlobs {
	return &memBlobs{objs: map[string]memObj{}, pageSize: listPageSize}
}

func (m *memBlobs) Put(_ context.Context, key string, body io.ReadSeeker, size int64) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.puts++
	if !ValidKey(key) {
		return errInvalidKey(key)
	}
	if m.putErr != nil {
		if err := m.putErr(key); err != nil {
			return err
		}
	}
	data, err := io.ReadAll(body)
	if err != nil {
		return err
	}
	if int64(len(data)) != size {
		return fmt.Errorf("size mismatch: %d != %d", len(data), size)
	}
	m.objs[key] = memObj{data: data, mod: time.Now()}
	return nil
}

func memETag(o memObj) string { return fmt.Sprintf("\"%d\"", len(o.data)) }

func (m *memBlobs) Get(_ context.Context, key, ifNoneMatch string) (*Object, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.gets++
	if m.getErr != nil {
		return nil, m.getErr
	}
	o, ok := m.objs[key]
	if !ok {
		return nil, ErrNotFound
	}
	if ifNoneMatch != "" && ifNoneMatch == memETag(o) {
		return nil, ErrNotModified
	}
	return &Object{Body: io.NopCloser(bytes.NewReader(o.data)), Size: int64(len(o.data)), ETag: memETag(o)}, nil
}

func (m *memBlobs) Delete(_ context.Context, keys []string) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.deletes++
	if m.deleteErr != nil {
		return m.deleteErr
	}
	m.deleteBatches = append(m.deleteBatches, slices.Clone(keys))
	for _, k := range keys {
		delete(m.objs, k)
	}
	return nil
}

func (m *memBlobs) List(_ context.Context, prefix string, fn func([]ObjectInfo) error) error {
	m.mu.Lock()
	m.lists++
	if m.listErr != nil {
		m.mu.Unlock()
		return m.listErr
	}
	var all []ObjectInfo
	for k, o := range m.objs {
		if strings.HasPrefix(k, prefix) {
			all = append(all, ObjectInfo{Key: k, Size: int64(len(o.data)), LastModified: o.mod})
		}
	}
	m.mu.Unlock() // fn may call back into the fake (Delete)
	slices.SortFunc(all, func(a, b ObjectInfo) int { return strings.Compare(a.Key, b.Key) })
	for len(all) > 0 {
		n := min(m.pageSize, len(all))
		if err := fn(all[:n]); err != nil {
			return err
		}
		all = all[n:]
	}
	return nil
}

func (m *memBlobs) has(key string) bool {
	m.mu.Lock()
	defer m.mu.Unlock()
	_, ok := m.objs[key]
	return ok
}

func (m *memBlobs) count() int {
	m.mu.Lock()
	defer m.mu.Unlock()
	return len(m.objs)
}

// seed stores an object last modified age ago.
func (m *memBlobs) seed(key string, size int, age time.Duration) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.objs[key] = memObj{data: make([]byte, size), mod: time.Now().Add(-age)}
}
