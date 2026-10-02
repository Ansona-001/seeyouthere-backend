package httpapi

import (
	"testing"
	"time"

	"github.com/google/uuid"
)

func TestCursor_RoundTrip(t *testing.T) {
	want := time.Now().Truncate(time.Microsecond).UTC()
	id := uuid.Must(uuid.NewV7())

	s := encodeCursor(want, id)
	gotTime, gotID, err := decodeCursor(s)
	if err != nil {
		t.Fatalf("decodeCursor: %v", err)
	}
	if !gotTime.Equal(want) {
		t.Errorf("time = %v, want %v", gotTime, want)
	}
	if gotID != id {
		t.Errorf("id = %v, want %v", gotID, id)
	}
}

func TestCursor_Invalid(t *testing.T) {
	cases := []string{"", "not-base64url!!!", "AAAA", "0000000000000000000000"}
	for _, c := range cases {
		if _, _, err := decodeCursor(c); err != errInvalidCursor {
			t.Errorf("decodeCursor(%q) err = %v, want errInvalidCursor", c, err)
		}
	}
}

func TestCursor_TamperedRejectedOrDifferent(t *testing.T) {
	s := encodeCursor(time.Now(), uuid.Must(uuid.NewV7()))
	tampered := s[:len(s)-1] + "x"
	if tampered == s {
		t.Skip("could not construct a distinct tampered cursor")
	}
	gotTime, gotID, err := decodeCursor(tampered)
	// A cursor has no integrity check (it's not a secret, just an opaque
	// position marker), so tampering either fails to decode or decodes to
	// a different position -- either way it must never equal the original.
	if err == nil && gotTime.Equal(mustDecodeTime(t, s)) && gotID == mustDecodeID(t, s) {
		t.Error("tampered cursor decoded to the same position as the original")
	}
}

func mustDecodeTime(t *testing.T, s string) time.Time {
	tm, _, err := decodeCursor(s)
	if err != nil {
		t.Fatalf("decodeCursor: %v", err)
	}
	return tm
}

func mustDecodeID(t *testing.T, s string) uuid.UUID {
	_, id, err := decodeCursor(s)
	if err != nil {
		t.Fatalf("decodeCursor: %v", err)
	}
	return id
}
