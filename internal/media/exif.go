package media

import (
	"bytes"
	"encoding/binary"
)

// exifOrientation returns the EXIF orientation tag (1-8) found in the first
// 64 KiB of a JPEG file, or 1 (no transform needed) if it's absent or
// unparseable. Only tag 0x0112 is read; every other EXIF/TIFF field
// (including GPS) is ignored and, since we always re-encode from the
// decoded pixels, never makes it into the stored file.
func exifOrientation(head []byte) int {
	const maxHead = 65536
	if len(head) > maxHead {
		head = head[:maxHead]
	}
	idx := bytes.Index(head, []byte("Exif\x00\x00"))
	if idx < 0 {
		return 1
	}
	tiff := head[idx+6:]
	if len(tiff) < 8 {
		return 1
	}

	var order binary.ByteOrder
	switch string(tiff[:2]) {
	case "II":
		order = binary.LittleEndian
	case "MM":
		order = binary.BigEndian
	default:
		return 1
	}

	ifdOffset := int(order.Uint32(tiff[4:8]))
	if ifdOffset < 0 || ifdOffset+2 > len(tiff) {
		return 1
	}
	count := int(order.Uint16(tiff[ifdOffset : ifdOffset+2]))
	base := ifdOffset + 2
	for i := 0; i < count; i++ {
		off := base + i*12
		if off+12 > len(tiff) {
			break
		}
		tag := order.Uint16(tiff[off : off+2])
		if tag != 0x0112 {
			continue
		}
		typ := order.Uint16(tiff[off+2 : off+4])
		if typ != 3 { // SHORT
			return 1
		}
		val := int(order.Uint16(tiff[off+8 : off+10]))
		if val < 1 || val > 8 {
			return 1
		}
		return val
	}
	return 1
}
