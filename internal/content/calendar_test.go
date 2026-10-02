package content

import (
	"testing"
	"time"
)

func locationBlock(id, name, address string) map[string]any {
	return map[string]any{
		"id": id, "type": "location", "heading": "", "name": name, "address": address,
		"map_url": "", "notes": "",
	}
}

func TestExtractCalendarInfo(t *testing.T) {
	loc, err := time.LoadLocation("America/New_York")
	if err != nil {
		t.Fatal(err)
	}
	wantStart := time.Date(2026, 6, 20, 18, 0, 0, 0, loc)
	wantEnd := time.Date(2026, 6, 20, 21, 0, 0, 0, loc)

	t.Run("datetime and location present", func(t *testing.T) {
		raw := mustJSON(t, []map[string]any{
			datetimeBlock("dt1", "2026-06-20T18:00", "2026-06-20T21:00", "America/New_York"),
			locationBlock("loc1", "The Grand Hall", "123 Main St"),
		})
		start, end, location := ExtractCalendarInfo(raw)
		if start == nil || !start.Equal(wantStart) {
			t.Errorf("start = %v, want %v", start, wantStart)
		}
		if end == nil || !end.Equal(wantEnd) {
			t.Errorf("end = %v, want %v", end, wantEnd)
		}
		if location != "The Grand Hall, 123 Main St" {
			t.Errorf("location = %q", location)
		}
	})

	t.Run("missing location block", func(t *testing.T) {
		raw := mustJSON(t, []map[string]any{
			datetimeBlock("dt1", "2026-06-20T18:00", "2026-06-20T21:00", "America/New_York"),
		})
		start, end, location := ExtractCalendarInfo(raw)
		if start == nil || end == nil {
			t.Errorf("start/end = %v/%v, want both set", start, end)
		}
		if location != "" {
			t.Errorf("location = %q, want empty", location)
		}
	})

	t.Run("missing datetime block", func(t *testing.T) {
		raw := mustJSON(t, []map[string]any{
			locationBlock("loc1", "The Grand Hall", "123 Main St"),
		})
		start, end, location := ExtractCalendarInfo(raw)
		if start != nil || end != nil {
			t.Errorf("start/end = %v/%v, want both nil", start, end)
		}
		if location != "The Grand Hall, 123 Main St" {
			t.Errorf("location = %q", location)
		}
	})

	t.Run("malformed JSON", func(t *testing.T) {
		start, end, location := ExtractCalendarInfo([]byte("not json"))
		if start != nil || end != nil || location != "" {
			t.Errorf("got (%v, %v, %q), want all zero", start, end, location)
		}
	})

	t.Run("malformed datetime block tolerated", func(t *testing.T) {
		raw := mustJSON(t, []map[string]any{
			{"id": "dt1", "type": "datetime", "start_local": "not-a-time", "timezone": "America/New_York"},
			locationBlock("loc1", "The Grand Hall", ""),
		})
		start, end, location := ExtractCalendarInfo(raw)
		if start != nil || end != nil {
			t.Errorf("start/end = %v/%v, want both nil", start, end)
		}
		if location != "The Grand Hall" {
			t.Errorf("location = %q", location)
		}
	})

	t.Run("multiple location blocks uses first", func(t *testing.T) {
		raw := mustJSON(t, []map[string]any{
			locationBlock("loc1", "First Venue", "1 First St"),
			locationBlock("loc2", "Second Venue", "2 Second St"),
		})
		_, _, location := ExtractCalendarInfo(raw)
		if location != "First Venue, 1 First St" {
			t.Errorf("location = %q, want first block", location)
		}
	})

	t.Run("name or address only", func(t *testing.T) {
		raw := mustJSON(t, []map[string]any{locationBlock("loc1", "", "123 Main St")})
		_, _, location := ExtractCalendarInfo(raw)
		if location != "123 Main St" {
			t.Errorf("location = %q", location)
		}
	})
}
