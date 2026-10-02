package media

import "testing"

func TestExifOrientation_Parses(t *testing.T) {
	for _, want := range []int{1, 2, 3, 4, 5, 6, 7, 8} {
		seg := exifApp1Segment(uint16(want))
		// exifOrientation scans for "Exif\x00\x00" directly, so the raw
		// APP1 payload (without the marker/length prefix) is enough.
		head := seg[4:]
		if got := exifOrientation(head); got != want {
			t.Errorf("orientation %d: got %d", want, got)
		}
	}
}

func TestExifOrientation_NoExifDefaultsToOne(t *testing.T) {
	if got := exifOrientation([]byte("not a jpeg header at all")); got != 1 {
		t.Errorf("got %d, want 1", got)
	}
	if got := exifOrientation(nil); got != 1 {
		t.Errorf("got %d, want 1", got)
	}
}

func TestExifOrientation_OutOfRangeValueIgnored(t *testing.T) {
	seg := exifApp1Segment(99)
	if got := exifOrientation(seg[4:]); got != 1 {
		t.Errorf("out-of-range orientation should default to 1, got %d", got)
	}
}

func TestExifOrientation_BigEndianTIFF(t *testing.T) {
	// Build the same segment but with a big-endian ("MM") TIFF header.
	payload := []byte("Exif\x00\x00")
	payload = append(payload, 'M', 'M')
	payload = append(payload, 0x00, 0x2A)
	payload = append(payload, 0x00, 0x00, 0x00, 0x08) // IFD0 offset = 8
	payload = append(payload, 0x00, 0x01)             // 1 entry
	payload = append(payload, 0x01, 0x12)             // tag 0x0112
	payload = append(payload, 0x00, 0x03)             // type SHORT
	payload = append(payload, 0x00, 0x00, 0x00, 0x01) // count 1
	payload = append(payload, 0x00, 0x06, 0x00, 0x00) // value 6, big-endian in first 2 bytes
	payload = append(payload, 0x00, 0x00, 0x00, 0x00) // next IFD

	if got := exifOrientation(payload); got != 6 {
		t.Errorf("got %d, want 6", got)
	}
}
