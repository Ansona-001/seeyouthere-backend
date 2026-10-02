package httpapi

import (
	"encoding/base64"
	"encoding/binary"
	"errors"
	"time"

	"github.com/google/uuid"
)

// errInvalidCursor is mapped to 400 invalid_cursor by every list handler.
var errInvalidCursor = errors.New("invalid cursor")

// encodeCursor builds the opaque keyset cursor for a (created_at, id) page
// boundary: base64url(unix_micro[8] || uuid[16]).
func encodeCursor(t time.Time, id uuid.UUID) string {
	buf := make([]byte, 8+16)
	binary.BigEndian.PutUint64(buf[:8], uint64(t.UnixMicro()))
	copy(buf[8:], id[:])
	return base64.RawURLEncoding.EncodeToString(buf)
}

// decodeCursor parses a cursor produced by encodeCursor.
func decodeCursor(s string) (time.Time, uuid.UUID, error) {
	raw, err := base64.RawURLEncoding.DecodeString(s)
	if err != nil || len(raw) != 8+16 {
		return time.Time{}, uuid.Nil, errInvalidCursor
	}
	micros := int64(binary.BigEndian.Uint64(raw[:8]))
	id, err := uuid.FromBytes(raw[8:])
	if err != nil {
		return time.Time{}, uuid.Nil, errInvalidCursor
	}
	return time.UnixMicro(micros).UTC(), id, nil
}
