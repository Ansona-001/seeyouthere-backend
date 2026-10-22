package content

import (
	"encoding/json"
	"math"
	"testing"
	"time"
)

func mapLocationBlock(id, mapURL string) map[string]any {
	return map[string]any{
		"id": id, "type": "location", "heading": "", "name": "Venue", "address": "", "map_url": mapURL, "notes": "",
	}
}

func TestMapCoordinates(t *testing.T) {
	cases := []struct {
		name     string
		url      string
		lat, lon float64
		ok       bool
	}{
		{"google viewport", "https://www.google.com/maps/place/Burj+Khalifa/@25.1972,55.2744,17z/data=!4m6", 25.1972, 55.2744, true},
		{"google pin beats viewport", "https://www.google.com/maps/place/X/@25.2,55.3,17z/data=!3d25.1972!4d55.2744", 25.1972, 55.2744, true},
		{"google embed", "https://www.google.com/maps/embed?pb=!1m18!1m12!1m3!1d3613.5!2d55.2744!3d25.1972!2m3", 25.1972, 55.2744, true},
		{"google q pair", "https://maps.google.com/?q=51.5007,-0.1246", 51.5007, -0.1246, true},
		{"google query pair", "https://www.google.com/maps/search/?api=1&query=40.7580,-73.9855", 40.758, -73.9855, true},
		{"apple ll", "https://maps.apple.com/?ll=-33.8568,151.2153&q=Opera", -33.8568, 151.2153, true},
		{"osm hash", "https://www.openstreetmap.org/#map=17/27.7172/85.3240", 27.7172, 85.324, true},
		{"osm mlat", "https://www.openstreetmap.org/?mlat=28.6139&mlon=77.2090", 28.6139, 77.209, true},
		{"address only", "https://www.google.com/maps/search/?api=1&query=Burj+Khalifa+Dubai", 0, 0, false},
		{"short link", "https://maps.app.goo.gl/AbCdEf123", 0, 0, false},
		{"null island", "https://www.google.com/maps/@0,0,5z", 0, 0, false},
		{"latitude out of range", "https://www.google.com/maps/@95.5,55.2,5z", 0, 0, false},
		{"longitude out of range", "https://www.google.com/maps/@25.5,195.2,5z", 0, 0, false},
		{"garbage q", "https://maps.google.com/?q=1,2,3,4", 0, 0, false},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			lat, lon, ok := mapCoordinates(c.url)
			if ok != c.ok || (ok && (math.Abs(lat-c.lat) > 1e-9 || math.Abs(lon-c.lon) > 1e-9)) {
				t.Errorf("mapCoordinates(%q) = %v, %v, %v; want %v, %v, %v", c.url, lat, lon, ok, c.lat, c.lon, c.ok)
			}
		})
	}
}

func TestVenueTimezone(t *testing.T) {
	cases := []struct {
		name   string
		blocks []map[string]any
		want   string
	}{
		{"dubai", []map[string]any{mapLocationBlock("l1", "https://www.google.com/maps/@25.1972,55.2744,17z")}, "Asia/Dubai"},
		{"london", []map[string]any{mapLocationBlock("l1", "https://maps.google.com/?q=51.5007,-0.1246")}, "Europe/London"},
		{"new york", []map[string]any{mapLocationBlock("l1", "https://www.google.com/maps/@40.758,-73.9855,15z")}, "America/New_York"},
		{"india", []map[string]any{mapLocationBlock("l1", "https://www.openstreetmap.org/?mlat=28.6139&mlon=77.2090")}, "Asia/Kolkata"},
		{"nepal quarter hour offset", []map[string]any{mapLocationBlock("l1", "https://www.openstreetmap.org/#map=17/27.7172/85.3240")}, "Asia/Kathmandu"},
		{"open water has no zone", []map[string]any{mapLocationBlock("l1", "https://www.google.com/maps/@0.5,-30.5,5z")}, ""},
		{"no map link", []map[string]any{mapLocationBlock("l1", "")}, ""},
		{"address-only link", []map[string]any{mapLocationBlock("l1", "https://www.google.com/maps/search/?api=1&query=Somewhere")}, ""},
		{"unsupported host is ignored", []map[string]any{mapLocationBlock("l1", "https://evil.example/@25.1972,55.2744")}, ""},
		{"first block without coordinates is skipped", []map[string]any{
			mapLocationBlock("l1", "https://maps.app.goo.gl/short"),
			mapLocationBlock("l2", "https://www.google.com/maps/@25.1972,55.2744,17z"),
		}, "Asia/Dubai"},
		{"no location block", []map[string]any{heroBlock("h1", "A & B")}, ""},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			var in []json.RawMessage
			for _, b := range c.blocks {
				in = append(in, mustJSON(t, b))
			}
			if got := venueTimezone(in); got != c.want {
				t.Errorf("venueTimezone = %q, want %q", got, c.want)
			}
		})
	}
}

func TestValidateContent_VenueTimezoneOverridesClientZone(t *testing.T) {
	occ := sampleOccasion(t)
	content := []map[string]any{
		heroBlock("hero1", "Ada & Grace"),
		datetimeBlock("dt1", "2027-06-01T16:00", "2027-06-01T20:00", "Europe/London"),
		mapLocationBlock("loc1", "https://www.google.com/maps/@25.1972,55.2744,17z"),
	}
	saved, err := ValidateContent(mustJSON(t, content), occ)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	var out []map[string]any
	if err := json.Unmarshal(saved.JSON, &out); err != nil {
		t.Fatal(err)
	}
	if got := out[1]["timezone"]; got != "Asia/Dubai" {
		t.Errorf("saved timezone = %v, want Asia/Dubai", got)
	}
	// 16:00 in Dubai (UTC+4) is 12:00 UTC; the end is four hours later.
	wantStart := time.Date(2027, 6, 1, 12, 0, 0, 0, time.UTC)
	if saved.StartsAt == nil || !saved.StartsAt.Equal(wantStart) {
		t.Errorf("StartsAt = %v, want %v", saved.StartsAt, wantStart)
	}
	if saved.EndsAt == nil || !saved.EndsAt.Equal(wantStart.Add(4*time.Hour)) {
		t.Errorf("EndsAt = %v, want %v", saved.EndsAt, wantStart.Add(4*time.Hour))
	}
}

func TestValidateContent_VenueTimezoneAcceptsUnsetClientZone(t *testing.T) {
	occ := sampleOccasion(t)
	// With a venue zone, a client that sends no usable zone is still accepted.
	for _, tz := range []string{"", "Local", "Not/AZone"} {
		content := []map[string]any{
			datetimeBlock("dt1", "2027-06-01T16:00", "", tz),
			mapLocationBlock("loc1", "https://maps.google.com/?q=51.5007,-0.1246"),
		}
		saved, err := ValidateContent(mustJSON(t, content), occ)
		if err != nil {
			t.Fatalf("tz %q: unexpected error: %v", tz, err)
		}
		// 16:00 London in June (BST, UTC+1) is 15:00 UTC.
		want := time.Date(2027, 6, 1, 15, 0, 0, 0, time.UTC)
		if saved.StartsAt == nil || !saved.StartsAt.Equal(want) {
			t.Errorf("tz %q: StartsAt = %v, want %v", tz, saved.StartsAt, want)
		}
	}
}

func TestValidateContent_NoVenueCoordinatesKeepsClientZone(t *testing.T) {
	occ := sampleOccasion(t)
	for _, mapURL := range []string{"", "https://maps.app.goo.gl/short", "https://www.google.com/maps/search/?api=1&query=Somewhere"} {
		content := []map[string]any{
			datetimeBlock("dt1", "2027-06-01T16:00", "", "Asia/Tokyo"),
			mapLocationBlock("loc1", mapURL),
		}
		saved, err := ValidateContent(mustJSON(t, content), occ)
		if err != nil {
			t.Fatalf("map %q: unexpected error: %v", mapURL, err)
		}
		want := time.Date(2027, 6, 1, 7, 0, 0, 0, time.UTC) // 16:00 JST
		if saved.StartsAt == nil || !saved.StartsAt.Equal(want) {
			t.Errorf("map %q: StartsAt = %v, want %v", mapURL, saved.StartsAt, want)
		}
	}
	// Without a usable venue zone the client's zone is still validated.
	content := []map[string]any{datetimeBlock("dt1", "2027-06-01T16:00", "", "Local"), mapLocationBlock("loc1", "")}
	if _, err := ValidateContent(mustJSON(t, content), occ); !hasIssueCode(err, "invalid_timezone") {
		t.Fatalf("expected invalid_timezone, got %v", err)
	}
}
