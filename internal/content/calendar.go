package content

import (
	"encoding/json"
	"strings"
	"time"
)

// ExtractCalendarInfo pulls best-effort calendar details out of an event's
// already-stored content (the raw jsonb from events.content): the
// datetime block's start/end instants (via datetimeZone, the same
// timezone-parsing logic used at write time) and the first location
// block's address, joined the same way the frontend's own "add to
// calendar" feature does (name + address, comma-separated, empty parts
// dropped).
//
// This is used only to build an email calendar invite for a guest who just
// RSVP'd, so it never errors: malformed JSON, a missing datetime block or a
// missing location block all just return their zero value rather than
// blocking the confirmation email from being sent. The content was already
// validated by ValidateContent when it was saved, so a plain (non-strict)
// decode here only matters for forward compatibility with blocks added
// after this event's content was written.
func ExtractCalendarInfo(stored []byte) (start, end *time.Time, location string) {
	var in []json.RawMessage
	if err := json.Unmarshal(stored, &in); err != nil {
		return nil, nil, ""
	}
	_, start, end, _ = datetimeZone(in)

	for _, raw := range in {
		var meta blockMeta
		if err := json.Unmarshal(raw, &meta); err != nil || meta.Type != "location" {
			continue
		}
		var b locationBlockJSON
		if err := json.Unmarshal(raw, &b); err != nil {
			continue
		}
		parts := make([]string, 0, 2)
		if b.Name != "" {
			parts = append(parts, b.Name)
		}
		if b.Address != "" {
			parts = append(parts, b.Address)
		}
		location = strings.Join(parts, ", ")
		break
	}
	return start, end, location
}
