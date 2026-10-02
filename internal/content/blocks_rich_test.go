package content

import (
	"encoding/json"
	"strings"
	"testing"
)

func person(name string) map[string]any {
	return map[string]any{
		"name": name, "role": "", "photo": nil,
		"family_label": "", "family_names": "", "place": "", "bio": "",
	}
}

func peopleBlock(id string, people ...map[string]any) map[string]any {
	return map[string]any{"id": id, "type": "people", "kicker": "", "heading": "", "people": people}
}

func videoBlock(id, provider, videoID, hash, aspect string) map[string]any {
	return map[string]any{
		"id": id, "type": "video", "kicker": "", "heading": "",
		"provider": provider, "video_id": videoID, "vimeo_hash": hash, "aspect": aspect,
		"caption": "", "poster_media_id": "",
	}
}

func wish(message, author string) map[string]any {
	return map[string]any{"message": message, "author": author}
}

func wishesBlock(id string, items ...map[string]any) map[string]any {
	return map[string]any{"id": id, "type": "wishes", "kicker": "", "heading": "", "items": items}
}

func richOccasion(t *testing.T) Occasion {
	t.Helper()
	occ := sampleOccasion(t)
	occ.OptionalBlocks = append(occ.OptionalBlocks, "people", "video", "wishes")
	return occ
}

// --- people ---

func TestValidateContent_People_Valid(t *testing.T) {
	occ := richOccasion(t)
	content := []map[string]any{peopleBlock("p1", person("Ada"))}
	saved, err := ValidateContent(mustJSON(t, content), occ)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	var out []json.RawMessage
	if err := json.Unmarshal(saved.JSON, &out); err != nil || len(out) != 1 {
		t.Fatalf("canonical JSON = %s, err %v", saved.JSON, err)
	}
}

func TestValidateContent_People_CountBounds(t *testing.T) {
	occ := richOccasion(t)
	if _, err := ValidateContent(mustJSON(t, []map[string]any{peopleBlock("p1")}), occ); !hasIssueCode(err, "invalid_length") {
		t.Fatalf("0 people should be invalid_length, got %v", err)
	}
	var seven []map[string]any
	for i := 0; i < 7; i++ {
		seven = append(seven, person("Name"))
	}
	if _, err := ValidateContent(mustJSON(t, []map[string]any{peopleBlock("p1", seven...)}), occ); !hasIssueCode(err, "invalid_length") {
		t.Fatalf("7 people should be invalid_length, got %v", err)
	}
	var six []map[string]any
	for i := 0; i < 6; i++ {
		six = append(six, person("Name"))
	}
	if _, err := ValidateContent(mustJSON(t, []map[string]any{peopleBlock("p1", six...)}), occ); err != nil {
		t.Errorf("6 people should be valid: %v", err)
	}
}

func TestValidateContent_People_NameRequiredAndBounds(t *testing.T) {
	occ := richOccasion(t)
	missing := person("")
	if _, err := ValidateContent(mustJSON(t, []map[string]any{peopleBlock("p1", missing)}), occ); !hasIssueCode(err, "required") {
		t.Fatalf("expected required, got %v", err)
	}
	tooLong := person(strings.Repeat("a", 81))
	if _, err := ValidateContent(mustJSON(t, []map[string]any{peopleBlock("p1", tooLong)}), occ); !hasIssueCode(err, "too_long") {
		t.Fatalf("expected too_long, got %v", err)
	}
	atLimit := person(strings.Repeat("a", 80))
	if _, err := ValidateContent(mustJSON(t, []map[string]any{peopleBlock("p1", atLimit)}), occ); err != nil {
		t.Errorf("80-char name should be valid: %v", err)
	}
}

func TestValidateContent_People_BioAllowsNewlineNameDoesNot(t *testing.T) {
	occ := richOccasion(t)
	p := person("Ada")
	p["bio"] = "line1\nline2"
	if _, err := ValidateContent(mustJSON(t, []map[string]any{peopleBlock("p1", p)}), occ); err != nil {
		t.Errorf("newline in bio should be valid: %v", err)
	}
	p2 := person("line1\nline2")
	if _, err := ValidateContent(mustJSON(t, []map[string]any{peopleBlock("p1", p2)}), occ); !hasIssueCode(err, "invalid_text") {
		t.Errorf("newline in name should be invalid, got %v", err)
	}
}

func TestValidateContent_People_BidiOverrideInFamilyNames(t *testing.T) {
	occ := richOccasion(t)
	p := person("Ada")
	p["family_names"] = "Mr \u202eevil"
	if _, err := ValidateContent(mustJSON(t, []map[string]any{peopleBlock("p1", p)}), occ); !hasIssueCode(err, "invalid_text") {
		t.Fatalf("expected invalid_text for bidi override, got %v", err)
	}
}

func TestValidateContent_People_PhotoMediaRef(t *testing.T) {
	occ := richOccasion(t)
	validID := "018f0000-0000-7000-8000-000000000001"
	p := person("Ada")
	p["photo"] = map[string]any{"media_id": validID, "alt": ""}
	saved, err := ValidateContent(mustJSON(t, []map[string]any{peopleBlock("p1", p)}), occ)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if len(saved.MediaIDs) != 1 || saved.MediaIDs[0].String() != validID {
		t.Fatalf("MediaIDs = %v", saved.MediaIDs)
	}

	bad := person("Ada")
	bad["photo"] = map[string]any{"media_id": "not-a-uuid", "alt": ""}
	if _, err := ValidateContent(mustJSON(t, []map[string]any{peopleBlock("p1", bad)}), occ); !hasIssueCode(err, "invalid_media_id") {
		t.Fatalf("expected invalid_media_id, got %v", err)
	}
}

func TestValidateContent_People_TooManyBlocks(t *testing.T) {
	occ := richOccasion(t)
	content := []map[string]any{
		peopleBlock("p1", person("A")), peopleBlock("p2", person("B")), peopleBlock("p3", person("C")),
	}
	if _, err := ValidateContent(mustJSON(t, content), occ); !hasIssueCode(err, "too_many_people") {
		t.Fatalf("expected too_many_people, got %v", err)
	}
}

// --- video ---

func TestValidateContent_Video_YoutubeID(t *testing.T) {
	occ := richOccasion(t)
	cases := []struct {
		name string
		id   string
		ok   bool
	}{
		{"11 chars ok", "dQw4w9WgXcQ", true},
		{"10 chars rejected", "dQw4w9WgXc", false},
		{"12 chars rejected", "dQw4w9WgXcQQ", false},
		{"angle bracket rejected", "dQw4w9WgX<Q", false},
		{"slash rejected", "dQw4w9WgX/Q", false},
		{"question mark rejected", "dQw4w9WgX?Q", false},
		{"percent rejected", "dQw4w9WgX%Q", false},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			content := []map[string]any{videoBlock("v1", "youtube", c.id, "", "")}
			_, err := ValidateContent(mustJSON(t, content), occ)
			if (err == nil) != c.ok {
				t.Errorf("id %q: err = %v, want ok=%v", c.id, err, c.ok)
			}
		})
	}
}

func TestValidateContent_Video_VimeoID(t *testing.T) {
	occ := richOccasion(t)
	cases := []struct {
		name string
		id   string
		ok   bool
	}{
		{"9 digits ok", "123456789", true},
		{"leading zero rejected", "0123", false},
		{"13 digits rejected", "1234567890123", false},
		{"letters rejected", "12345abc", false},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			content := []map[string]any{videoBlock("v1", "vimeo", c.id, "", "")}
			_, err := ValidateContent(mustJSON(t, content), occ)
			if (err == nil) != c.ok {
				t.Errorf("id %q: err = %v, want ok=%v", c.id, err, c.ok)
			}
		})
	}
}

func TestValidateContent_Video_VimeoHash(t *testing.T) {
	occ := richOccasion(t)
	cases := []struct {
		name string
		hash string
		ok   bool
	}{
		{"valid 8 hex", "0123abcd", true},
		{"valid 16 hex", "0123456789abcdef", true},
		{"uppercase rejected", "0123ABCD", false},
		{"7 chars rejected", "0123abc", false},
		{"17 chars rejected", "0123456789abcdef0", false},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			content := []map[string]any{videoBlock("v1", "vimeo", "123456789", c.hash, "")}
			_, err := ValidateContent(mustJSON(t, content), occ)
			if (err == nil) != c.ok {
				t.Errorf("hash %q: err = %v, want ok=%v", c.hash, err, c.ok)
			}
		})
	}
}

func TestValidateContent_Video_VimeoHashWithYoutubeRejected(t *testing.T) {
	occ := richOccasion(t)
	content := []map[string]any{videoBlock("v1", "youtube", "dQw4w9WgXcQ", "0123abcd", "")}
	if _, err := ValidateContent(mustJSON(t, content), occ); !hasIssueCode(err, "invalid_value") {
		t.Fatalf("expected invalid_value, got %v", err)
	}
}

func TestValidateContent_Video_Provider(t *testing.T) {
	occ := richOccasion(t)
	if _, err := ValidateContent(mustJSON(t, []map[string]any{videoBlock("v1", "", "dQw4w9WgXcQ", "", "")}), occ); !hasIssueCode(err, "required") {
		t.Fatalf("expected required for empty provider, got %v", err)
	}
	if _, err := ValidateContent(mustJSON(t, []map[string]any{videoBlock("v1", "dailymotion", "dQw4w9WgXcQ", "", "")}), occ); !hasIssueCode(err, "invalid_value") {
		t.Fatalf("expected invalid_value for unknown provider, got %v", err)
	}
}

func TestValidateContent_Video_AspectNormalisesAndValidates(t *testing.T) {
	occ := richOccasion(t)
	content := []map[string]any{videoBlock("v1", "youtube", "dQw4w9WgXcQ", "", "")}
	saved, err := ValidateContent(mustJSON(t, content), occ)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	var out []map[string]any
	if err := json.Unmarshal(saved.JSON, &out); err != nil {
		t.Fatalf("unmarshal canonical: %v", err)
	}
	if out[0]["aspect"] != "16:9" {
		t.Errorf("aspect = %v, want 16:9", out[0]["aspect"])
	}

	bad := []map[string]any{videoBlock("v1", "youtube", "dQw4w9WgXcQ", "", "21:9")}
	if _, err := ValidateContent(mustJSON(t, bad), occ); !hasIssueCode(err, "invalid_value") {
		t.Fatalf("expected invalid_value for 21:9 aspect, got %v", err)
	}
}

func TestValidateContent_Video_PosterMediaRef(t *testing.T) {
	occ := richOccasion(t)
	validID := "018f0000-0000-7000-8000-000000000001"
	b := videoBlock("v1", "youtube", "dQw4w9WgXcQ", "", "")
	b["poster_media_id"] = validID
	saved, err := ValidateContent(mustJSON(t, []map[string]any{b}), occ)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if len(saved.MediaIDs) != 1 || saved.MediaIDs[0].String() != validID {
		t.Fatalf("MediaIDs = %v", saved.MediaIDs)
	}
}

func TestValidateContent_Video_TooManyBlocks(t *testing.T) {
	occ := richOccasion(t)
	content := []map[string]any{
		videoBlock("v1", "youtube", "dQw4w9WgXcQ", "", ""),
		videoBlock("v2", "youtube", "dQw4w9WgXcQ", "", ""),
		videoBlock("v3", "youtube", "dQw4w9WgXcQ", "", ""),
		videoBlock("v4", "youtube", "dQw4w9WgXcQ", "", ""),
	}
	if _, err := ValidateContent(mustJSON(t, content), occ); !hasIssueCode(err, "too_many_video") {
		t.Fatalf("expected too_many_video, got %v", err)
	}
}

// --- wishes ---

func TestValidateContent_Wishes_CountBounds(t *testing.T) {
	occ := richOccasion(t)
	if _, err := ValidateContent(mustJSON(t, []map[string]any{wishesBlock("w1")}), occ); !hasIssueCode(err, "invalid_length") {
		t.Fatalf("0 items should be invalid_length, got %v", err)
	}
	var thirtyOne []map[string]any
	for i := 0; i < 31; i++ {
		thirtyOne = append(thirtyOne, wish("Congrats!", "A friend"))
	}
	if _, err := ValidateContent(mustJSON(t, []map[string]any{wishesBlock("w1", thirtyOne...)}), occ); !hasIssueCode(err, "invalid_length") {
		t.Fatalf("31 items should be invalid_length, got %v", err)
	}
	var thirty []map[string]any
	for i := 0; i < 30; i++ {
		thirty = append(thirty, wish("Congrats!", "A friend"))
	}
	if _, err := ValidateContent(mustJSON(t, []map[string]any{wishesBlock("w1", thirty...)}), occ); err != nil {
		t.Errorf("30 items should be valid: %v", err)
	}
}

func TestValidateContent_Wishes_MessageAndAuthorBounds(t *testing.T) {
	occ := richOccasion(t)
	tooLong := wish(strings.Repeat("a", 501), "Author")
	if _, err := ValidateContent(mustJSON(t, []map[string]any{wishesBlock("w1", tooLong)}), occ); !hasIssueCode(err, "too_long") {
		t.Fatalf("expected too_long for 501-rune message, got %v", err)
	}
	atLimit := wish(strings.Repeat("a", 500), "Author")
	if _, err := ValidateContent(mustJSON(t, []map[string]any{wishesBlock("w1", atLimit)}), occ); err != nil {
		t.Errorf("500-rune message should be valid: %v", err)
	}
	missingAuthor := wish("Congrats!", "")
	if _, err := ValidateContent(mustJSON(t, []map[string]any{wishesBlock("w1", missingAuthor)}), occ); !hasIssueCode(err, "required") {
		t.Fatalf("expected required for missing author, got %v", err)
	}
	longAuthor := wish("Congrats!", strings.Repeat("a", 81))
	if _, err := ValidateContent(mustJSON(t, []map[string]any{wishesBlock("w1", longAuthor)}), occ); !hasIssueCode(err, "too_long") {
		t.Fatalf("expected too_long for 81-rune author, got %v", err)
	}
}

func TestValidateContent_Wishes_NewlineInAuthorRejected(t *testing.T) {
	occ := richOccasion(t)
	w := wish("Congrats!", "line1\nline2")
	if _, err := ValidateContent(mustJSON(t, []map[string]any{wishesBlock("w1", w)}), occ); !hasIssueCode(err, "invalid_text") {
		t.Fatalf("expected invalid_text for newline in author, got %v", err)
	}
}

func TestValidateContent_Wishes_TooManyBlocks(t *testing.T) {
	occ := richOccasion(t)
	w := wish("Congrats!", "A friend")
	content := []map[string]any{wishesBlock("w1", w), wishesBlock("w2", w)}
	if _, err := ValidateContent(mustJSON(t, content), occ); !hasIssueCode(err, "too_many_wishes") {
		t.Fatalf("expected too_many_wishes, got %v", err)
	}
}

// --- kicker ---

func TestValidateContent_Kicker_LengthAndNewline(t *testing.T) {
	occ := richOccasion(t)
	cases := []struct {
		name    string
		block   map[string]any
		wantErr bool
	}{
		{"text 60 ok", map[string]any{"id": "t1", "type": "text", "kicker": strings.Repeat("a", 60), "heading": "", "body": "b"}, false},
		{"text 61 fails", map[string]any{"id": "t1", "type": "text", "kicker": strings.Repeat("a", 61), "heading": "", "body": "b"}, true},
		{"text newline fails", map[string]any{"id": "t1", "type": "text", "kicker": "a\nb", "heading": "", "body": "b"}, true},
		{"hero 60 ok", map[string]any{"id": "h1", "type": "hero", "kicker": strings.Repeat("a", 60), "title": "T", "subtitle": "", "image": nil}, false},
		{"hero 61 fails", map[string]any{"id": "h1", "type": "hero", "kicker": strings.Repeat("a", 61), "title": "T", "subtitle": "", "image": nil}, true},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			_, err := ValidateContent(mustJSON(t, []map[string]any{c.block}), occ)
			if (err != nil) != c.wantErr {
				t.Errorf("err = %v, wantErr %v", err, c.wantErr)
			}
		})
	}
}

// --- gallery.display ---

func galleryBlockWithDisplay(display string) map[string]any {
	return map[string]any{
		"id": "g1", "type": "gallery", "heading": "", "display": display,
		"images": []map[string]any{{"media_id": "018f0000-0000-7000-8000-000000000001", "alt": ""}},
	}
}

func TestValidateContent_GalleryDisplay(t *testing.T) {
	occ := richOccasion(t)
	occ.OptionalBlocks = append(occ.OptionalBlocks, "gallery")

	saved, err := ValidateContent(mustJSON(t, []map[string]any{galleryBlockWithDisplay("")}), occ)
	if err != nil {
		t.Fatalf("empty display should normalise to grid: %v", err)
	}
	var out []map[string]any
	if err := json.Unmarshal(saved.JSON, &out); err != nil || out[0]["display"] != "grid" {
		t.Fatalf("display = %v, want grid (err %v)", out, err)
	}

	if _, err := ValidateContent(mustJSON(t, []map[string]any{galleryBlockWithDisplay("grid")}), occ); err != nil {
		t.Errorf("grid should be valid: %v", err)
	}
	if _, err := ValidateContent(mustJSON(t, []map[string]any{galleryBlockWithDisplay("carousel")}), occ); err != nil {
		t.Errorf("carousel should be valid: %v", err)
	}
	if _, err := ValidateContent(mustJSON(t, []map[string]any{galleryBlockWithDisplay("slideshow")}), occ); !hasIssueCode(err, "invalid_value") {
		t.Fatalf("slideshow should be rejected, got %v", err)
	}
}

// --- Saved.EndsAt ---

func TestValidateContent_EndsAt(t *testing.T) {
	occ := sampleOccasion(t)

	t.Run("empty end_local gives nil", func(t *testing.T) {
		content := []map[string]any{datetimeBlock("dt1", "2027-06-01T16:00", "", "Europe/London")}
		saved, err := ValidateContent(mustJSON(t, content), occ)
		if err != nil {
			t.Fatalf("unexpected error: %v", err)
		}
		if saved.EndsAt != nil {
			t.Errorf("EndsAt = %v, want nil", saved.EndsAt)
		}
	})

	t.Run("DST-spanning range gives correct real duration", func(t *testing.T) {
		// America/New_York, 2027-03-14: US clocks spring forward at 02:00.
		content := []map[string]any{datetimeBlock("dt1", "2027-03-13T20:00", "2027-03-14T04:00", "America/New_York")}
		saved, err := ValidateContent(mustJSON(t, content), occ)
		if err != nil {
			t.Fatalf("unexpected error: %v", err)
		}
		if saved.StartsAt == nil || saved.EndsAt == nil {
			t.Fatal("expected both StartsAt and EndsAt to be set")
		}
		got := saved.EndsAt.Sub(*saved.StartsAt)
		want := 7 * 60 * 60 * 1e9 // 7 real hours in nanoseconds, because of the DST jump
		if got.Nanoseconds() != int64(want) {
			t.Errorf("duration = %v, want 7h (DST jump)", got)
		}
	})

	t.Run("UTC range", func(t *testing.T) {
		content := []map[string]any{datetimeBlock("dt1", "2027-06-01T16:00", "2027-06-01T20:00", "UTC")}
		saved, err := ValidateContent(mustJSON(t, content), occ)
		if err != nil {
			t.Fatalf("unexpected error: %v", err)
		}
		if saved.EndsAt == nil || saved.EndsAt.Sub(*saved.StartsAt) != 4*60*60*1e9 {
			t.Errorf("EndsAt-StartsAt = %v, want 4h", saved.EndsAt.Sub(*saved.StartsAt))
		}
	})

	t.Run("invalid datetime block does not panic and yields no Saved", func(t *testing.T) {
		content := []map[string]any{datetimeBlock("dt1", "not-a-date", "", "Europe/London")}
		if _, err := ValidateContent(mustJSON(t, content), occ); err == nil {
			t.Error("expected validation error for invalid datetime block")
		}
	})
}

// --- backward compatibility (§0 / §10 of the rich-blocks doc) ---
//
// Stored rows created before kicker/display/people/video/wishes existed
// have canonical JSON without those fields. ParseStored must accept them
// unchanged, and re-validating already-canonical output must be a fixed
// point (no field added a second time, nothing renormalised differently).

func TestValidateContent_BackwardCompat_PreChangeFixturePasses(t *testing.T) {
	occ := sampleOccasion(t)
	// Deliberately omits "kicker" (hero/text/datetime/gallery) and
	// "display" (gallery): the exact shape stored before this change.
	fixture := `[
		{"id":"h1","type":"hero","title":"Ada & Grace","subtitle":"","image":null},
		{"id":"dt1","type":"datetime","heading":"","start_local":"2027-06-01T16:00","end_local":"","timezone":"Europe/London","all_day":false},
		{"id":"loc1","type":"location","heading":"","name":"Venue","address":"","map_url":"","notes":""}
	]`
	saved, err := ParseStored(json.RawMessage(fixture), occ)
	if err != nil {
		t.Fatalf("pre-change fixture should still validate: %v", err)
	}
	if saved.Title != "Ada & Grace" {
		t.Errorf("title = %q", saved.Title)
	}
	if saved.EndsAt != nil {
		t.Errorf("EndsAt = %v, want nil (no end_local)", saved.EndsAt)
	}

	// Fixed point: re-validating the canonical output changes nothing.
	again, err := ValidateContent(saved.JSON, occ)
	if err != nil {
		t.Fatalf("re-validating canonical output should succeed: %v", err)
	}
	if string(again.JSON) != string(saved.JSON) {
		t.Errorf("canonical output is not a fixed point:\nfirst:  %s\nsecond: %s", saved.JSON, again.JSON)
	}
}

func TestValidateContent_BackwardCompat_GalleryWithoutDisplay(t *testing.T) {
	occ := sampleOccasion(t)
	occ.OptionalBlocks = append(occ.OptionalBlocks, "gallery")
	fixture := `[{"id":"g1","type":"gallery","heading":"","images":[{"media_id":"018f0000-0000-7000-8000-000000000001","alt":""}]}]`
	saved, err := ParseStored(json.RawMessage(fixture), occ)
	if err != nil {
		t.Fatalf("gallery without display should still validate: %v", err)
	}
	var out []map[string]any
	if err := json.Unmarshal(saved.JSON, &out); err != nil || out[0]["display"] != "grid" {
		t.Fatalf("display = %v, want grid (err %v)", out, err)
	}
}
