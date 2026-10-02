package content

import "testing"

func validManifestMap() map[string]any {
	return map[string]any{
		"schema": 1, "layout": "centered", "hero_style": "framed", "decoration": "line",
		"palettes": []map[string]any{
			{"id": "ivory", "name": "Ivory", "colors": map[string]any{
				"background": "#FBF8F3", "surface": "#FFFFFF", "text": "#1F1B16",
				"muted": "#6B645C", "accent": "#8C6A3F", "accent_text": "#FFFFFF",
			}},
		},
		"fonts": []map[string]any{
			{"id": "classic", "name": "Classic", "heading": "playfair_display", "body": "lora"},
		},
		"defaults":   map[string]any{"palette": "ivory", "font": "classic"},
		"background": nil,
	}
}

func TestValidateManifest_Valid(t *testing.T) {
	m, err := ValidateManifest(mustJSON(t, validManifestMap()))
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if m.Palettes[0].Colors.Background != "#FBF8F3" {
		t.Errorf("unexpected normalised colour: %+v", m.Palettes[0].Colors)
	}
}

func TestValidateManifest_ContrastFailure(t *testing.T) {
	m := validManifestMap()
	m["palettes"] = []map[string]any{
		{"id": "bad", "name": "Bad", "colors": map[string]any{
			// text and background nearly identical: fails 4.5:1 contrast.
			"background": "#FFFFFF", "surface": "#FFFFFF", "text": "#FEFEFE",
			"muted": "#EEEEEE", "accent": "#8C6A3F", "accent_text": "#FFFFFF",
		}},
	}
	m["defaults"] = map[string]any{"palette": "bad", "font": "classic"}
	if _, err := ValidateManifest(mustJSON(t, m)); !hasIssueCode(err, "low_contrast") {
		t.Fatalf("expected low_contrast, got %v", err)
	}
}

func TestValidateManifest_UnknownFont(t *testing.T) {
	m := validManifestMap()
	m["fonts"] = []map[string]any{{"id": "classic", "name": "Classic", "heading": "comic_sans", "body": "lora"}}
	if _, err := ValidateManifest(mustJSON(t, m)); !hasIssueCode(err, "invalid_value") {
		t.Fatalf("expected invalid_value for unknown font, got %v", err)
	}
}

func TestValidateManifest_GreatVibesBodyRejected(t *testing.T) {
	m := validManifestMap()
	m["fonts"] = []map[string]any{{"id": "classic", "name": "Classic", "heading": "playfair_display", "body": "great_vibes"}}
	if _, err := ValidateManifest(mustJSON(t, m)); err == nil {
		t.Fatal("expected error for great_vibes as body font")
	}
}

func TestValidateManifest_DuplicateIDs(t *testing.T) {
	m := validManifestMap()
	p := m["palettes"].([]map[string]any)[0]
	m["palettes"] = []map[string]any{p, p}
	if _, err := ValidateManifest(mustJSON(t, m)); !hasIssueCode(err, "duplicate_id") {
		t.Fatalf("expected duplicate_id, got %v", err)
	}
}

func TestValidateManifest_DefaultsMustExist(t *testing.T) {
	m := validManifestMap()
	m["defaults"] = map[string]any{"palette": "nope", "font": "classic"}
	if _, err := ValidateManifest(mustJSON(t, m)); !hasIssueCode(err, "invalid_value") {
		t.Fatalf("expected invalid_value for unknown default palette, got %v", err)
	}
}

func TestValidateManifest_PaletteCountBounds(t *testing.T) {
	base := validManifestMap()
	p := base["palettes"].([]map[string]any)[0]
	var nine []map[string]any
	for i := 0; i < 9; i++ {
		cp := map[string]any{"id": p["id"].(string) + string(rune('a'+i)), "name": "P", "colors": p["colors"]}
		nine = append(nine, cp)
	}
	base["palettes"] = nine
	if _, err := ValidateManifest(mustJSON(t, base)); !hasIssueCode(err, "invalid_length") {
		t.Fatalf("expected invalid_length for 9 palettes, got %v", err)
	}
}

func TestValidateManifest_BackgroundShape(t *testing.T) {
	m := validManifestMap()
	m["background"] = map[string]any{"asset": "background", "opacity": 0.3}
	if _, err := ValidateManifest(mustJSON(t, m)); err != nil {
		t.Errorf("valid background should pass: %v", err)
	}
	m["background"] = map[string]any{"asset": "logo", "opacity": 0.3}
	if _, err := ValidateManifest(mustJSON(t, m)); err == nil {
		t.Error("wrong asset name should fail")
	}
	m["background"] = map[string]any{"asset": "background", "opacity": 0.9}
	if _, err := ValidateManifest(mustJSON(t, m)); err == nil {
		t.Error("opacity out of range should fail")
	}
}

func TestValidateOverrides(t *testing.T) {
	m, err := ValidateManifest(mustJSON(t, validManifestMap()))
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if _, err := ValidateOverrides([]byte(`{"palette":"ivory","font":"classic"}`), m); err != nil {
		t.Errorf("valid overrides should pass: %v", err)
	}
	if _, err := ValidateOverrides([]byte(`{"palette":"","font":""}`), m); err != nil {
		t.Errorf("empty overrides should pass: %v", err)
	}
	if _, err := ValidateOverrides([]byte(`{"palette":"nope","font":""}`), m); err == nil {
		t.Error("unknown palette override should fail")
	}
}

func TestResolveTheme(t *testing.T) {
	m, err := ValidateManifest(mustJSON(t, validManifestMap()))
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	theme := ResolveTheme(m, []byte(`{"palette":"","font":""}`), "")
	if theme.Palette.Background != "#FBF8F3" {
		t.Errorf("expected default palette, got %+v", theme.Palette)
	}
	if theme.Fonts.Heading != "playfair_display" {
		t.Errorf("expected default font, got %+v", theme.Fonts)
	}
	if theme.Background != nil {
		t.Errorf("expected nil background, got %+v", theme.Background)
	}
}

// --- rich-blocks theme knobs (§4 of the rich-blocks doc) ---

func TestValidateManifest_ExistingManifestsNormaliseToPlainDefaults(t *testing.T) {
	// A manifest with no surface/texture/heading_scale/accent (today's
	// shape) must still validate and normalise to the values that match
	// today's rendered look, with fonts.accent falling back to heading.
	m, err := ValidateManifest(mustJSON(t, validManifestMap()))
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if m.Surface != "plain" || m.Texture != "none" || m.HeadingScale != "regular" {
		t.Errorf("got surface=%q texture=%q heading_scale=%q, want plain/none/regular", m.Surface, m.Texture, m.HeadingScale)
	}
	theme := ResolveTheme(m, nil, "")
	if theme.Fonts.Accent != theme.Fonts.Heading {
		t.Errorf("Fonts.Accent = %q, want fallback to heading %q", theme.Fonts.Accent, theme.Fonts.Heading)
	}
	if theme.AccentInk != m.Palettes[0].Colors.Accent {
		t.Errorf("AccentInk = %q, want the palette's accent (Ivory passes contrast)", theme.AccentInk)
	}
}

func TestValidateManifest_UnknownSurfaceTextureHeadingScale(t *testing.T) {
	cases := []struct {
		field, value string
	}{{"surface", "translucent"}, {"texture", "stripes"}, {"heading_scale", "huge"}}
	for _, c := range cases {
		t.Run(c.field, func(t *testing.T) {
			m := validManifestMap()
			m[c.field] = c.value
			if _, err := ValidateManifest(mustJSON(t, m)); !hasIssueCode(err, "invalid_value") {
				t.Fatalf("expected invalid_value for %s=%q, got %v", c.field, c.value, err)
			}
		})
	}
}

func TestValidateManifest_ValidSurfaceTextureHeadingScale(t *testing.T) {
	m := validManifestMap()
	m["surface"] = "card"
	m["texture"] = "dots"
	m["heading_scale"] = "display"
	got, err := ValidateManifest(mustJSON(t, m))
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if got.Surface != "card" || got.Texture != "dots" || got.HeadingScale != "display" {
		t.Errorf("got %+v", got)
	}
}

func TestValidateManifest_UnknownAccentFont(t *testing.T) {
	m := validManifestMap()
	m["fonts"] = []map[string]any{{"id": "classic", "name": "Classic", "heading": "playfair_display", "body": "lora", "accent": "comic_sans"}}
	if _, err := ValidateManifest(mustJSON(t, m)); !hasIssueCode(err, "invalid_value") {
		t.Fatalf("expected invalid_value for unknown accent font, got %v", err)
	}
}

func TestValidateManifest_HeadingOnlyFontsRejectedAsBodyAcceptedElsewhere(t *testing.T) {
	for _, f := range []string{"great_vibes", "birthstone"} {
		t.Run(f+"_as_body", func(t *testing.T) {
			m := validManifestMap()
			m["fonts"] = []map[string]any{{"id": "classic", "name": "Classic", "heading": "playfair_display", "body": f}}
			if _, err := ValidateManifest(mustJSON(t, m)); err == nil {
				t.Fatalf("expected error for %s as body font", f)
			}
		})
		t.Run(f+"_as_heading_and_accent", func(t *testing.T) {
			m := validManifestMap()
			m["fonts"] = []map[string]any{{"id": "classic", "name": "Classic", "heading": f, "body": "lora", "accent": f}}
			if _, err := ValidateManifest(mustJSON(t, m)); err != nil {
				t.Errorf("%s should be valid as heading/accent: %v", f, err)
			}
		})
	}
}

func TestValidateManifest_GlassContrast(t *testing.T) {
	// text #767676 on surface #FFFFFF over background #000000 passes both
	// solid checks (text/background, text/surface) but fails the composited
	// blend, so this must only be caught when surface == "glass".
	palette := map[string]any{"id": "bad", "name": "Bad", "colors": map[string]any{
		"background": "#000000", "surface": "#FFFFFF", "text": "#767676",
		"muted": "#767676", "accent": "#8C6A3F", "accent_text": "#FFFFFF",
	}}
	m := validManifestMap()
	m["palettes"] = []map[string]any{palette}
	m["defaults"] = map[string]any{"palette": "bad", "font": "classic"}

	if _, err := ValidateManifest(mustJSON(t, m)); err != nil {
		t.Fatalf("plain surface should pass: %v", err)
	}
	m["surface"] = "glass"
	if _, err := ValidateManifest(mustJSON(t, m)); !hasIssueCode(err, "low_contrast") {
		t.Fatalf("glass surface should fail the blended contrast check, got %v", err)
	}
}

func TestResolveTheme_AccentInkFallsBackToTextOnLowContrast(t *testing.T) {
	// Confetti "Sunshine": accent/background contrast 1.60:1, below the
	// 3:1 non-text-UI threshold, so AccentInk must fall back to text.
	m := validManifestMap()
	m["palettes"] = []map[string]any{{"id": "sunshine", "name": "Sunshine", "colors": map[string]any{
		"background": "#FFF7E0", "surface": "#FFFFFF", "text": "#4A3B00",
		"muted": "#8A7A3D", "accent": "#F2C12E", "accent_text": "#4A3B00",
	}}}
	m["defaults"] = map[string]any{"palette": "sunshine", "font": "classic"}
	valid, err := ValidateManifest(mustJSON(t, m))
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	theme := ResolveTheme(valid, nil, "")
	if theme.AccentInk != theme.Palette.Text {
		t.Errorf("AccentInk = %q, want fallback to text %q", theme.AccentInk, theme.Palette.Text)
	}
}
