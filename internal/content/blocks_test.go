package content

import (
	"encoding/json"
	"strings"
	"testing"
)

func heroBlock(id, title string) map[string]any {
	return map[string]any{"id": id, "type": "hero", "title": title, "subtitle": "", "image": nil}
}

func datetimeBlock(id, start, end, tz string) map[string]any {
	return map[string]any{
		"id": id, "type": "datetime", "heading": "", "start_local": start, "end_local": end,
		"timezone": tz, "all_day": false,
	}
}

func rsvpBlock(id, deadline string) map[string]any {
	return map[string]any{
		"id": id, "type": "rsvp", "heading": "", "body": "", "deadline_local": deadline,
		"capacity": nil, "max_party_size": nil, "fields": []any{}, "questions": []any{},
	}
}

func TestValidateContent_Valid(t *testing.T) {
	occ := sampleOccasion(t)
	raw := mustJSON(t, []map[string]any{
		heroBlock("hero1", "Ada & Grace"),
		datetimeBlock("dt1", "2027-06-01T16:00", "", "Europe/London"),
	})
	saved, err := ValidateContent(raw, occ)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if saved.Title != "Ada & Grace" {
		t.Errorf("title = %q", saved.Title)
	}
	if saved.StartsAt == nil {
		t.Fatal("expected StartsAt to be set")
	}
	var out []json.RawMessage
	if err := json.Unmarshal(saved.JSON, &out); err != nil || len(out) != 2 {
		t.Fatalf("canonical JSON = %s, err %v", saved.JSON, err)
	}
}

func TestValidateContent_TitleFallback(t *testing.T) {
	occ := sampleOccasion(t)
	cases := []struct {
		name    string
		content []map[string]any
		want    string
	}{
		{"hero wins", []map[string]any{heroBlock("h", "Hero Title"), {"id": "t1", "type": "text", "heading": "Heading", "body": "body"}}, "Hero Title"},
		{"first heading fallback", []map[string]any{{"id": "t1", "type": "text", "heading": "First Heading", "body": "b"}, {"id": "t2", "type": "text", "heading": "Second", "body": "b"}}, "First Heading"},
		{"untitled fallback", []map[string]any{{"id": "t1", "type": "text", "heading": "", "body": "b"}}, "Untitled event"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			saved, err := ValidateContent(mustJSON(t, c.content), occ)
			if err != nil {
				t.Fatalf("unexpected error: %v", err)
			}
			if saved.Title != c.want {
				t.Errorf("title = %q, want %q", saved.Title, c.want)
			}
		})
	}
}

func TestValidateContent_LengthBounds(t *testing.T) {
	occ := sampleOccasion(t)
	one := []map[string]any{heroBlock("h", "T")}
	if _, err := ValidateContent(mustJSON(t, one), occ); err != nil {
		t.Errorf("1 block should be valid: %v", err)
	}
	if _, err := ValidateContent(mustJSON(t, []map[string]any{}), occ); err == nil {
		t.Error("0 blocks should be invalid")
	}

	var forty []map[string]any
	for i := 0; i < 40; i++ {
		forty = append(forty, map[string]any{"id": idFor(i), "type": "text", "heading": "", "body": "b"})
	}
	if _, err := ValidateContent(mustJSON(t, forty), occ); err != nil {
		t.Errorf("40 blocks should be valid: %v", err)
	}
	fortyOne := append(forty, map[string]any{"id": "extra", "type": "text", "heading": "", "body": "b"})
	if _, err := ValidateContent(mustJSON(t, fortyOne), occ); err == nil {
		t.Error("41 blocks should be invalid")
	}
}

func idFor(i int) string { return "b" + string(rune('a'+i%26)) + string(rune('0'+i/26)) }

func TestValidateContent_Cardinality(t *testing.T) {
	occ := sampleOccasion(t)
	two := []map[string]any{heroBlock("h1", "A"), heroBlock("h2", "B")}
	_, err := ValidateContent(mustJSON(t, two), occ)
	if !hasIssueCode(err, "too_many_hero") {
		t.Fatalf("expected too_many_hero, got %v", err)
	}
}

func TestValidateContent_UnknownAndDisallowedType(t *testing.T) {
	occ := sampleOccasion(t)
	unknown := []map[string]any{{"id": "x", "type": "bogus"}}
	_, err := ValidateContent(mustJSON(t, unknown), occ)
	if !hasIssueCode(err, "invalid_type") {
		t.Fatalf("expected invalid_type, got %v", err)
	}

	// "faq" is not in this occasion's default or optional blocks list for this test's occ... actually it is.
	notAllowed := []map[string]any{{"id": "x", "type": "hero", "title": "T", "subtitle": "", "image": nil}}
	occNoHero := sampleOccasion(t)
	occNoHero.OptionalBlocks = nil
	occNoHero.DefaultBlocks = mustJSON(t, []map[string]any{{"id": "d1", "type": "text", "heading": "", "body": "b"}})
	_, err = ValidateContent(mustJSON(t, notAllowed), occNoHero)
	if !hasIssueCode(err, "block_not_allowed") {
		t.Fatalf("expected block_not_allowed, got %v", err)
	}
}

func TestValidateContent_BlockIDs(t *testing.T) {
	occ := sampleOccasion(t)
	dup := []map[string]any{
		{"id": "same", "type": "text", "heading": "", "body": "a"},
		{"id": "same", "type": "text", "heading": "", "body": "b"},
	}
	if _, err := ValidateContent(mustJSON(t, dup), occ); !hasIssueCode(err, "duplicate_id") {
		t.Fatalf("expected duplicate_id, got %v", err)
	}

	bad := []map[string]any{{"id": "has a space", "type": "text", "heading": "", "body": "a"}}
	if _, err := ValidateContent(mustJSON(t, bad), occ); !hasIssueCode(err, "invalid_id") {
		t.Fatalf("expected invalid_id, got %v", err)
	}

	longID := strings.Repeat("a", 25)
	badLong := []map[string]any{{"id": longID, "type": "text", "heading": "", "body": "a"}}
	if _, err := ValidateContent(mustJSON(t, badLong), occ); !hasIssueCode(err, "invalid_id") {
		t.Fatalf("expected invalid_id for 25-char id, got %v", err)
	}
	okID := strings.Repeat("a", 24)
	badLong2 := []map[string]any{{"id": okID, "type": "text", "heading": "", "body": "a"}}
	if _, err := ValidateContent(mustJSON(t, badLong2), occ); err != nil {
		t.Errorf("24-char id should be valid: %v", err)
	}
}

func TestValidateContent_CountdownRequiresDatetime(t *testing.T) {
	occ := sampleOccasion(t)
	without := []map[string]any{{"id": "c1", "type": "countdown", "heading": ""}}
	if _, err := ValidateContent(mustJSON(t, without), occ); !hasIssueCode(err, "requires_datetime") {
		t.Fatalf("expected requires_datetime, got %v", err)
	}

	with := []map[string]any{
		datetimeBlock("dt1", "2027-06-01T16:00", "", "Europe/London"),
		{"id": "c1", "type": "countdown", "heading": ""},
	}
	if _, err := ValidateContent(mustJSON(t, with), occ); err != nil {
		t.Errorf("countdown with datetime should be valid: %v", err)
	}
}

func TestValidateContent_RSVPDeadline(t *testing.T) {
	occ := sampleOccasion(t)

	noDatetime := []map[string]any{rsvpBlock("r1", "2027-05-01T00:00")}
	if _, err := ValidateContent(mustJSON(t, noDatetime), occ); !hasIssueCode(err, "requires_datetime") {
		t.Fatalf("expected requires_datetime, got %v", err)
	}

	withDatetime := []map[string]any{
		datetimeBlock("dt1", "2027-06-01T16:00", "", "Europe/London"),
		rsvpBlock("r1", "2027-05-01T00:00"),
	}
	saved, err := ValidateContent(mustJSON(t, withDatetime), occ)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if saved.RSVP == nil || saved.RSVP.Deadline == nil {
		t.Fatal("expected resolved deadline")
	}

	badDeadline := []map[string]any{
		datetimeBlock("dt1", "2027-06-01T16:00", "", "Europe/London"),
		rsvpBlock("r1", "not-a-date"),
	}
	if _, err := ValidateContent(mustJSON(t, badDeadline), occ); !hasIssueCode(err, "invalid_datetime") {
		t.Fatalf("expected invalid_datetime, got %v", err)
	}
}

func TestValidateContent_RSVPUnknownField(t *testing.T) {
	occ := sampleOccasion(t)
	r := map[string]any{
		"id": "r1", "type": "rsvp", "heading": "", "body": "", "deadline_local": "",
		"capacity": nil, "max_party_size": nil,
		"fields":    []map[string]any{{"key": "not_a_field", "required": false}},
		"questions": []any{},
	}
	if _, err := ValidateContent(mustJSON(t, []map[string]any{r}), occ); !hasIssueCode(err, "unknown_field") {
		t.Fatalf("expected unknown_field, got %v", err)
	}
}

func TestValidateContent_DatetimeBounds(t *testing.T) {
	occ := sampleOccasion(t)
	cases := []struct {
		name       string
		start, end string
		wantErr    bool
	}{
		{"no end ok", "2027-06-01T16:00", "", false},
		{"end after start ok", "2027-06-01T16:00", "2027-06-01T20:00", false},
		{"end before start fails", "2027-06-01T16:00", "2027-06-01T10:00", true},
		{"end exactly 14 days ok", "2027-06-01T16:00", "2027-06-15T16:00", false},
		{"end over 14 days fails", "2027-06-01T16:00", "2027-06-16T16:00", true},
		{"bad start format fails", "2027-06-01 16:00", "", true},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			content := []map[string]any{datetimeBlock("dt1", c.start, c.end, "Europe/London")}
			_, err := ValidateContent(mustJSON(t, content), occ)
			if (err != nil) != c.wantErr {
				t.Errorf("err = %v, wantErr %v", err, c.wantErr)
			}
		})
	}
}

func TestValidateContent_TimezoneRejectsLocal(t *testing.T) {
	occ := sampleOccasion(t)
	content := []map[string]any{datetimeBlock("dt1", "2027-06-01T16:00", "", "Local")}
	if _, err := ValidateContent(mustJSON(t, content), occ); !hasIssueCode(err, "invalid_timezone") {
		t.Fatalf("expected invalid_timezone, got %v", err)
	}
}

func TestValidateContent_MediaRefs(t *testing.T) {
	occ := sampleOccasion(t)
	validID := "018f0000-0000-7000-8000-000000000001"
	hero := map[string]any{"id": "h1", "type": "hero", "title": "T", "subtitle": "", "image": map[string]any{"media_id": validID, "alt": ""}}
	saved, err := ValidateContent(mustJSON(t, []map[string]any{hero}), occ)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if len(saved.MediaIDs) != 1 || saved.MediaIDs[0].String() != validID {
		t.Fatalf("MediaIDs = %v", saved.MediaIDs)
	}

	heroBad := map[string]any{"id": "h1", "type": "hero", "title": "T", "subtitle": "", "image": map[string]any{"media_id": "not-a-uuid", "alt": ""}}
	if _, err := ValidateContent(mustJSON(t, []map[string]any{heroBad}), occ); !hasIssueCode(err, "invalid_media_id") {
		t.Fatalf("expected invalid_media_id, got %v", err)
	}
}

func TestValidateContent_TooManyMediaRefs(t *testing.T) {
	occ := sampleOccasion(t)
	occ.OptionalBlocks = append(occ.OptionalBlocks, "gallery")
	var images []map[string]any
	for i := 0; i < 24; i++ {
		images = append(images, map[string]any{"media_id": "018f0000-0000-7000-8000-" + padID(i), "alt": ""})
	}
	g1 := map[string]any{"id": "g1", "type": "gallery", "heading": "", "images": images}
	g2 := map[string]any{"id": "g2", "type": "gallery", "heading": "", "images": images}
	g3 := map[string]any{"id": "g3", "type": "gallery", "heading": "", "images": images}
	_, err := ValidateContent(mustJSON(t, []map[string]any{g1, g2, g3}), occ)
	if !hasIssueCode(err, "too_many_media") {
		t.Fatalf("expected too_many_media, got %v", err)
	}
}

func padID(i int) string {
	s := "000000000000"
	is := itoa(i)
	return s[:len(s)-len(is)] + is
}

func itoa(i int) string {
	if i == 0 {
		return "0"
	}
	var b []byte
	for i > 0 {
		b = append([]byte{byte('0' + i%10)}, b...)
		i /= 10
	}
	return string(b)
}

func TestValidateContent_CanonicalTooLarge(t *testing.T) {
	occ := sampleOccasion(t)
	occ.OptionalBlocks = append(occ.OptionalBlocks, "text")
	bigBody := strings.Repeat("a", 5000)
	var blocks []map[string]any
	for i := 0; i < 40; i++ {
		blocks = append(blocks, map[string]any{"id": idFor(i), "type": "text", "heading": "", "body": bigBody})
	}
	_, err := ValidateContent(mustJSON(t, blocks), occ)
	if !hasIssueCode(err, "too_large") {
		t.Fatalf("expected too_large, got %v", err)
	}
}

func TestValidateContent_BidiAndControlChars(t *testing.T) {
	occ := sampleOccasion(t)
	for _, bad := range []string{"\u202Ename", "with\x07bell", "with\x00null"} {
		content := []map[string]any{{"id": "t1", "type": "text", "heading": bad, "body": "b"}}
		if _, err := ValidateContent(mustJSON(t, content), occ); !hasIssueCode(err, "invalid_text") {
			t.Errorf("input %q: expected invalid_text, got %v", bad, err)
		}
	}
	// Newline allowed in a multi-line body...
	content := []map[string]any{{"id": "t1", "type": "text", "heading": "", "body": "line1\nline2"}}
	if _, err := ValidateContent(mustJSON(t, content), occ); err != nil {
		t.Errorf("newline in multi-line body should be valid: %v", err)
	}
	// ...but not in a single-line heading.
	content2 := []map[string]any{{"id": "t1", "type": "text", "heading": "line1\nline2", "body": "b"}}
	if _, err := ValidateContent(mustJSON(t, content2), occ); !hasIssueCode(err, "invalid_text") {
		t.Errorf("newline in heading should be invalid, got %v", err)
	}
}

func TestValidateContent_LinksURLRules(t *testing.T) {
	occ := sampleOccasion(t)
	occ.OptionalBlocks = append(occ.OptionalBlocks, "links")
	cases := []struct {
		name string
		url  string
		ok   bool
	}{
		{"https ok", "https://example.com/rsvp", true},
		{"http rejected", "http://example.com", false},
		{"javascript rejected", "javascript:alert(1)", false},
		{"userinfo rejected", "https://user:pass@example.com", false},
		{"ip host rejected", "https://192.168.1.1", false},
		{"port 443 ok", "https://example.com:443/path", true},
		{"port 8443 rejected", "https://example.com:8443", false},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			links := map[string]any{"id": "l1", "type": "links", "heading": "", "body": "", "items": []map[string]any{{"label": "Site", "url": c.url}}}
			_, err := ValidateContent(mustJSON(t, []map[string]any{links}), occ)
			if (err == nil) != c.ok {
				t.Errorf("url %q: err = %v, want ok=%v", c.url, err, c.ok)
			}
		})
	}
}

func TestValidateContent_MapURLAllowlist(t *testing.T) {
	occ := sampleOccasion(t)
	cases := []struct {
		name string
		url  string
		ok   bool
	}{
		{"maps.google.com ok", "https://maps.google.com/?q=1", true},
		{"google maps path ok", "https://www.google.com/maps/place/x", true},
		{"google without maps path rejected", "https://www.google.com/search?q=x", false},
		{"openstreetmap ok", "https://www.openstreetmap.org/way/1", true},
		{"unrelated host rejected", "https://example.com/maps", false},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			loc := map[string]any{"id": "loc1", "type": "location", "heading": "", "name": "Venue", "address": "", "map_url": c.url, "notes": ""}
			_, err := ValidateContent(mustJSON(t, []map[string]any{loc}), occ)
			if (err == nil) != c.ok {
				t.Errorf("map_url %q: err = %v, want ok=%v", c.url, err, c.ok)
			}
		})
	}
}

func TestParseStored_RoundTrip(t *testing.T) {
	occ := sampleOccasion(t)
	raw := mustJSON(t, []map[string]any{heroBlock("h1", "Ada & Grace")})
	saved, err := ValidateContent(raw, occ)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	reparsed, err := ParseStored(saved.JSON, occ)
	if err != nil {
		t.Fatalf("ParseStored: %v", err)
	}
	if reparsed.Title != saved.Title || string(reparsed.JSON) != string(saved.JSON) {
		t.Errorf("ParseStored not idempotent: %+v vs %+v", reparsed, saved)
	}
}

func TestBuildInitialContent(t *testing.T) {
	occ := sampleOccasion(t)
	raw, err := BuildInitialContent(occ, map[string]string{"name_1": "Ada", "name_2": "Grace"})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	saved, err := ValidateContent(raw, occ)
	if err != nil {
		t.Fatalf("built content should validate: %v", err)
	}
	if saved.Title != "Ada & Grace" {
		t.Errorf("title = %q", saved.Title)
	}
}

func TestBuildInitialContent_RequiredAnswerMissing(t *testing.T) {
	occ := sampleOccasion(t)
	if _, err := BuildInitialContent(occ, map[string]string{"name_1": "Ada"}); !hasIssueCode(err, "required") {
		t.Fatalf("expected required, got %v", err)
	}
}
