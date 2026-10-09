package content

import (
	"bytes"
	"encoding/json"
	"fmt"
	"math"
	"strings"
	"testing"
)

// --- fixtures ---

type obj = map[string]any

func v2Palette(id, name string, c [6]string, art, foil []string) obj {
	p := obj{"id": id, "name": name, "colors": obj{
		"background": c[0], "surface": c[1], "text": c[2], "muted": c[3], "accent": c[4], "accent_text": c[5],
	}}
	if art != nil {
		p["art"] = art
	}
	if foil != nil {
		p["foil"] = foil
	}
	return p
}

var goldFoil = []string{"#8E6B2E", "#C9A963", "#F3E2AA", "#B48A43", "#E6CC8B"}
var brightFoil = []string{"#E3C779", "#C9A45C", "#F3E2AA", "#B48A43", "#D9B769"}

func oliveGrove() obj {
	ivory := v2Palette("ivory", "Ivory", [6]string{"#F3EEE3", "#FBF8F1", "#2C382D", "#5A6457", "#2F4A37", "#FBF8F1"},
		[]string{"#5F6E55", "#C9D3B8", "#DCE2CF", "#4A5531", "#FFFDF8", "#E5DCC8"}, goldFoil)
	ivory["accent_ink"] = "#77592A"
	sage := v2Palette("sage", "Sage", [6]string{"#E9EEE2", "#F6F8F2", "#25301F", "#4F5A47", "#3C5A3A", "#F6F8F2"},
		[]string{"#6A7B5C", "#CAD6BC", "#DDE5D2", "#4F5E32", "#FAFCF6", "#DDE3CF"}, goldFoil)
	blush := v2Palette("blush", "Blush", [6]string{"#F7EAE6", "#FDF7F4", "#3A2623", "#6A4F4A", "#7A3B3F", "#FDF7F4"},
		[]string{"#8A6B5E", "#EBCFC6", "#F3DFD8", "#6B4A3A", "#FFFBF9", "#EBD9D2"}, goldFoil)
	return obj{
		"schema": 2, "layout": "split", "hero_style": "text_only", "heading_scale": "display", "background": nil,
		"palettes": []obj{ivory, sage, blush},
		"fonts":    []obj{{"id": "bodoni", "name": "Bodoni and Montserrat", "heading": "bodoni_moda", "body": "montserrat", "accent": "pinyon_script"}},
		"defaults": obj{"palette": "ivory", "font": "bodoni"},
		"layers": []obj{
			{"kind": "paper", "tone": "background", "glow": "art5", "vignette": "art6"},
			{"kind": "texture", "texture": "fibers", "opacity": 0.06, "blend": "multiply"},
			{"kind": "art", "art": "olive_branches", "placement": "corners", "colors": []string{"art1", "art2", "art3", "art4"}},
			{"kind": "texture", "texture": "grain", "opacity": 0.1, "blend": "multiply"},
			{"kind": "frame", "frame": "double_hairline", "inset": 12, "paint": "foil"},
		},
		"ornament": obj{"hero": "wreath_monogram", "divider": "olive_sprig", "badge": "none", "ampersand": "script", "hero_ink": ""},
		"card":     obj{"style": "reply_card", "border": "foil_inset", "radius": 3, "fields": "underline", "buttons": "accent"},
		"motion":   "draw_on",
	}
}

func sprinkles() obj {
	pal := func(id, name string, c [6]string, art []string) obj { return v2Palette(id, name, c, art, nil) }
	return obj{
		"schema": 2, "layout": "split", "hero_style": "color_block", "heading_scale": "display", "background": nil,
		"palettes": []obj{
			pal("cobalt", "Cobalt", [6]string{"#FFF4DC", "#FFFFFF", "#1B2250", "#4A4F72", "#2B4BE0", "#FFFFFF"},
				[]string{"#FFC21A", "#FF4F7B", "#16C79A", "#FF8A3D", "#FFFFFF"}),
			pal("berry", "Berry", [6]string{"#FFF0F4", "#FFFFFF", "#3B1030", "#6B3A5E", "#B3124F", "#FFFFFF"},
				[]string{"#FFC21A", "#2B4BE0", "#16C79A", "#FF8A3D", "#FFFFFF"}),
			pal("mint", "Mint", [6]string{"#E8F8F1", "#FFFFFF", "#0F3B2E", "#3E6B5C", "#0B6B50", "#FFFFFF"},
				[]string{"#FFC21A", "#FF4F7B", "#2B4BE0", "#FF8A3D", "#FFFFFF"}),
		},
		"fonts":    []obj{{"id": "pop", "name": "Pop", "heading": "bagel_fat_one", "body": "figtree", "accent": "figtree"}},
		"defaults": obj{"palette": "cobalt", "font": "pop"},
		"layers": []obj{
			{"kind": "paper", "tone": "background", "glow": "art1", "glow_opacity": 0.4},
			{"kind": "block", "edge": "scallop"},
			{"kind": "pattern", "pattern": "halftone", "color": "accent_text", "opacity": 0.26, "region": "hero", "origin": "top_right", "mask": "corner"},
			{"kind": "art", "art": "confetti", "placement": "hero", "colors": []string{"art1", "art2", "art3", "art4", "art5"}, "density": "dense"},
			{"kind": "art", "art": "balloons", "placement": "hero_top", "colors": []string{"art2", "art1", "art3"}},
			{"kind": "texture", "texture": "grain", "opacity": 0.12, "blend": "multiply"},
		},
		"ornament": obj{"hero": "sticker_numeral", "divider": "squiggle", "badge": "starburst_sticker", "ampersand": "none", "hero_ink": ""},
		"card":     obj{"style": "sticker", "border": "ink", "radius": 28, "fields": "boxed", "buttons": "accent"},
		"motion":   "pop_and_settle",
	}
}

func giltNoir() obj {
	pal := func(id, name string, c [6]string, art []string) obj { return v2Palette(id, name, c, art, brightFoil) }
	return obj{
		"schema": 2, "layout": "split", "hero_style": "text_only", "heading_scale": "display", "background": nil,
		"palettes": []obj{
			pal("noir", "Noir", [6]string{"#0E0D0A", "#1A1812", "#F4EEDD", "#BDB39A", "#C9A45C", "#0E0D0A"}, []string{"#C9A45C", "#8E7440"}),
			pal("emerald", "Emerald", [6]string{"#0B1F1A", "#12302A", "#EAF3EE", "#A9C4BA", "#C9A45C", "#0B1F1A"}, []string{"#C9A45C", "#7FB59F"}),
			pal("oxblood", "Oxblood", [6]string{"#1F0B0E", "#2E1216", "#F6ECEC", "#CDB0B3", "#D4AF6A", "#1F0B0E"}, []string{"#D4AF6A", "#B66A73"}),
		},
		"fonts":    []obj{{"id": "deco", "name": "Deco", "heading": "limelight", "body": "josefin_sans", "accent": "josefin_sans"}},
		"defaults": obj{"palette": "noir", "font": "deco"},
		"layers": []obj{
			{"kind": "paper", "tone": "background", "glow": "art1", "glow_opacity": 0.26},
			{"kind": "pattern", "pattern": "sunburst", "color": "art1", "opacity": 0.15, "origin": "top"},
			{"kind": "art", "art": "stepped_arches", "placement": "hero", "paint": "foil"},
			{"kind": "pattern", "pattern": "pinstripe", "color": "text", "opacity": 0.03},
			{"kind": "texture", "texture": "grain", "opacity": 0.16, "blend": "screen"},
			{"kind": "art", "art": "deco_fans", "placement": "bottom_corners", "colors": []string{"art1"}},
			{"kind": "frame", "frame": "double_hairline_deco_corners", "inset": 10, "paint": "foil"},
		},
		"ornament": obj{"hero": "foil_numeral", "divider": "deco_diamond", "badge": "foil_seal", "ampersand": "none", "hero_ink": ""},
		"card":     obj{"style": "chamfered", "border": "foil", "fields": "underline", "buttons": "foil"},
		"motion":   "foil_sheen",
	}
}

func v2Fixtures() map[string]obj {
	return map[string]obj{"olive_grove": oliveGrove(), "sprinkles": sprinkles(), "gilt_noir": giltNoir()}
}

// mutated deep-copies base (via JSON) and applies fn to the copy.
func mutated(t *testing.T, base obj, fn func(m obj)) []byte {
	t.Helper()
	var m obj
	if err := json.Unmarshal(mustJSON(t, base), &m); err != nil {
		t.Fatalf("copy: %v", err)
	}
	fn(m)
	return mustJSON(t, m)
}

// Typed accessors for the JSON-round-tripped maps used inside mutators.
func layersOf(m obj) []any     { return m["layers"].([]any) }
func layerAt(m obj, i int) obj { return layersOf(m)[i].(obj) }
func palAt(m obj, i int) obj   { return m["palettes"].([]any)[i].(obj) }
func colorsAt(m obj, i int) obj {
	return palAt(m, i)["colors"].(obj)
}
func setLayers(m obj, ls ...obj) {
	out := make([]any, len(ls))
	for i, l := range ls {
		out[i] = l
	}
	m["layers"] = out
}

// --- valid fixtures ---

func TestManifestV2_FixturesValidateAndRoundTrip(t *testing.T) {
	for name, fx := range v2Fixtures() {
		t.Run(name, func(t *testing.T) {
			m, err := ValidateManifest(mustJSON(t, fx))
			if err != nil {
				t.Fatalf("fixture invalid: %v", err)
			}
			canon, err := json.Marshal(m)
			if err != nil {
				t.Fatal(err)
			}
			m2, err := ValidateManifest(canon)
			if err != nil {
				t.Fatalf("canonical form no longer validates: %v", err)
			}
			canon2, _ := json.Marshal(m2)
			if !bytes.Equal(canon, canon2) {
				t.Errorf("canonical form is not idempotent:\n%s\n%s", canon, canon2)
			}
			var top map[string]json.RawMessage
			if err := json.Unmarshal(canon, &top); err != nil {
				t.Fatal(err)
			}
			for _, banned := range []string{"decoration", "surface", "texture"} {
				if _, ok := top[banned]; ok {
					t.Errorf("canonical v2 form has top-level %s", banned)
				}
			}
			if strings.Contains(string(canon), `"src"`) {
				t.Errorf("canonical v2 form contains src: %s", canon)
			}
		})
	}
}

func TestManifestV2_DefaultsAreWrittenExplicitly(t *testing.T) {
	fx := oliveGrove()
	raw := mutated(t, fx, func(m obj) {
		delete(m, "ornament")
		delete(m, "card")
		delete(m, "motion")
		delete(m, "heading_scale")
		setLayers(m, obj{"kind": "paper"}, obj{"kind": "frame", "frame": "single_hairline", "color": "accent"})
	})
	m, err := ValidateManifest(raw)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if m.Ornament == nil || *m.Ornament != (Ornament{Hero: "none", Divider: "none", Badge: "none", Ampersand: "none"}) {
		t.Errorf("ornament defaults = %+v", m.Ornament)
	}
	if m.Card == nil || m.Card.Style != "soft" || m.Card.Border != "hairline" || *m.Card.Radius != 24 ||
		m.Card.Fields != "boxed" || m.Card.Buttons != "accent" {
		t.Errorf("card defaults = %+v", m.Card)
	}
	if m.Motion != "none" || m.HeadingScale != "regular" {
		t.Errorf("motion=%q heading_scale=%q", m.Motion, m.HeadingScale)
	}
	paper, frame := m.Layers[0], m.Layers[1]
	if paper.Region != "page" || paper.Tone != "background" {
		t.Errorf("paper defaults = %+v", paper)
	}
	if frame.Inset == nil || *frame.Inset != 12 || frame.Region != "page" {
		t.Errorf("frame defaults = %+v", frame)
	}
}

func TestManifestV2_ChamferedDefaultsToZeroRadius(t *testing.T) {
	m, err := ValidateManifest(mustJSON(t, giltNoir()))
	if err != nil {
		t.Fatal(err)
	}
	if m.Card.Radius == nil || *m.Card.Radius != 0 {
		t.Errorf("chamfered radius = %v, want 0", m.Card.Radius)
	}
}

func TestManifestV2_LowercaseHexIsUpperCased(t *testing.T) {
	raw := mutated(t, oliveGrove(), func(m obj) {
		colorsAt(m, 0)["background"] = "#f3eee3"
		palAt(m, 0)["accent_ink"] = "#77592a"
		palAt(m, 0)["art"].([]any)[0] = "#5f6e55"
		palAt(m, 0)["foil"].([]any)[0] = "#8e6b2e"
	})
	m, err := ValidateManifest(raw)
	if err != nil {
		t.Fatal(err)
	}
	p := m.Palettes[0]
	if p.Colors.Background != "#F3EEE3" || p.AccentInk != "#77592A" || p.Art[0] != "#5F6E55" || p.Foil[0] != "#8E6B2E" {
		t.Errorf("not upper-cased: %+v", p)
	}
}

func TestManifestV2_UsesBackgroundAsset(t *testing.T) {
	m, err := ValidateManifest(mustJSON(t, oliveGrove()))
	if err != nil {
		t.Fatal(err)
	}
	if m.UsesBackgroundAsset() {
		t.Error("manifest without an image layer must not need the asset")
	}
	withImage := mutated(t, oliveGrove(), func(m obj) {
		ls := append(layersOf(m), obj{"kind": "image", "opacity": 0.3})
		m["layers"] = ls
	})
	m, err = ValidateManifest(withImage)
	if err != nil {
		t.Fatal(err)
	}
	if !m.UsesBackgroundAsset() {
		t.Error("image layer must need the background asset")
	}
	v1, err := ValidateManifest(mustJSON(t, validManifestMap()))
	if err != nil || v1.UsesBackgroundAsset() {
		t.Errorf("v1 without background: err=%v uses=%v", err, v1.UsesBackgroundAsset())
	}
	bg := validManifestMap()
	bg["background"] = obj{"asset": "background", "opacity": 0.3}
	v1, err = ValidateManifest(mustJSON(t, bg))
	if err != nil || !v1.UsesBackgroundAsset() {
		t.Errorf("v1 with background: err=%v uses=%v", err, v1.UsesBackgroundAsset())
	}
}

// --- failures ---

type v2Case struct {
	name string
	base func() obj
	fn   func(m obj)
	code string
	path string // optional: an issue with this code must also have this path
}

func runV2Cases(t *testing.T, cases []v2Case) {
	t.Helper()
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			base := c.base
			if base == nil {
				base = oliveGrove
			}
			_, err := ValidateManifest(mutated(t, base(), c.fn))
			if !hasIssueCode(err, c.code) {
				t.Fatalf("want issue %q, got %v", c.code, err)
			}
			if c.path != "" && !hasIssue(err, c.code, c.path) {
				t.Fatalf("want issue %q at %q, got %+v", c.code, c.path, err)
			}
		})
	}
}

func hasIssue(err error, code, path string) bool {
	ve, ok := err.(*ValidationError)
	if !ok {
		return false
	}
	for _, i := range ve.Issues {
		if i.Code == code && i.Path == path {
			return true
		}
	}
	return false
}

func TestManifestV2_SchemaAndEnvelope(t *testing.T) {
	runV2Cases(t, []v2Case{
		{name: "v2 with decoration", fn: func(m obj) { m["decoration"] = "line" }, code: "schema_mismatch", path: "decoration"},
		{name: "v2 with surface", fn: func(m obj) { m["surface"] = "card" }, code: "schema_mismatch", path: "surface"},
		{name: "v2 with texture", fn: func(m obj) { m["texture"] = "grid" }, code: "schema_mismatch", path: "texture"},
		{name: "v2 with background", fn: func(m obj) { m["background"] = obj{"asset": "background", "opacity": 0.2} }, code: "schema_mismatch", path: "background"},
		{name: "schema 3", fn: func(m obj) { m["schema"] = 3 }, code: "unsupported_schema"},
		{name: "schema 0", fn: func(m obj) { m["schema"] = 0 }, code: "unsupported_schema"},
		{name: "unknown top-level field", fn: func(m obj) { m["extra"] = 1 }, code: "invalid_json"},
		{name: "unknown layer field", fn: func(m obj) { layerAt(m, 0)["extra"] = 1 }, code: "invalid_json"},
		{name: "unknown card field", fn: func(m obj) { m["card"].(obj)["extra"] = 1 }, code: "invalid_json"},
		{name: "unknown ornament field", fn: func(m obj) { m["ornament"].(obj)["extra"] = 1 }, code: "invalid_json"},
		{name: "unknown palette field", fn: func(m obj) { palAt(m, 0)["extra"] = 1 }, code: "invalid_json"},
		{name: "inset as 1e3", fn: func(m obj) { layerAt(m, 4)["inset"] = json.Number("1e3") }, code: "invalid_json"},
		{name: "opacity as string", fn: func(m obj) { layerAt(m, 1)["opacity"] = "0.5" }, code: "invalid_json"},
		{name: "opacity as bool", fn: func(m obj) { layerAt(m, 1)["opacity"] = true }, code: "invalid_json"},
		{name: "layers as object", fn: func(m obj) { m["layers"] = obj{} }, code: "invalid_json"},
		{name: "card as array", fn: func(m obj) { m["card"] = []any{} }, code: "invalid_json"},
		{name: "int overflow", fn: func(m obj) { layerAt(m, 4)["inset"] = json.Number("9223372036854775808") }, code: "invalid_json"},
		{name: "v2 with hero_style color_block but no block", fn: func(m obj) { m["hero_style"] = "color_block" }, code: "invalid_combination"},
	})
}

func TestManifestV2_SchemaMismatchFromV1(t *testing.T) {
	cases := []struct {
		name string
		fn   func(m obj)
		path string
	}{
		{"layers", func(m obj) { m["layers"] = []any{} }, "layers"},
		{"ornament", func(m obj) { m["ornament"] = obj{} }, "ornament"},
		{"card", func(m obj) { m["card"] = obj{} }, "card"},
		{"motion", func(m obj) { m["motion"] = "none" }, "motion"},
		{"palette foil", func(m obj) { m["palettes"].([]any)[0].(obj)["foil"] = []string{"#111111", "#222222", "#333333"} }, "palettes[0]"},
		{"palette art", func(m obj) { m["palettes"].([]any)[0].(obj)["art"] = []string{"#111111"} }, "palettes[0]"},
		{"palette accent_ink", func(m obj) { m["palettes"].([]any)[0].(obj)["accent_ink"] = "#111111" }, "palettes[0]"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			raw := mutated(t, validManifestMap(), c.fn)
			_, err := ValidateManifest(raw)
			if !hasIssue(err, "schema_mismatch", c.path) {
				t.Fatalf("want schema_mismatch at %s, got %v", c.path, err)
			}
		})
	}
	// color_block is a v2 hero style only.
	v1 := validManifestMap()
	v1["hero_style"] = "color_block"
	if _, err := ValidateManifest(mustJSON(t, v1)); !hasIssueCode(err, "invalid_value") {
		t.Errorf("v1 color_block: want invalid_value, got %v", err)
	}
}

func TestManifestV2_TooLarge(t *testing.T) {
	base := mustJSON(t, oliveGrove())
	pad := func(n int) []byte {
		return append(append([]byte{}, base...), bytes.Repeat([]byte(" "), n-len(base))...)
	}

	if _, err := ValidateManifest(pad(maxManifestBytes)); err != nil {
		t.Errorf("exactly 24 KiB must be accepted: %v", err)
	}
	_, err := ValidateManifest(pad(maxManifestBytes + 1))
	if !hasIssue(err, "too_large", "manifest") {
		t.Errorf("24 KiB + 1: want too_large, got %v", err)
	}
	// The cap applies to schema 1 too.
	v1 := mustJSON(t, validManifestMap())
	v1 = append(v1, bytes.Repeat([]byte(" "), maxManifestBytes)...)
	if _, err := ValidateManifest(v1); !hasIssueCode(err, "too_large") {
		t.Errorf("oversized v1: want too_large, got %v", err)
	}
}

func TestManifestV2_DeepNestingDoesNotPanic(t *testing.T) {
	deep := append(bytes.Repeat([]byte("["), 10_000), bytes.Repeat([]byte("]"), 10_000)...)
	for _, raw := range [][]byte{
		[]byte(`{"schema":2,"layers":` + string(deep) + `}`),
		[]byte(`{"schema":2,"x":` + string(deep) + `}`),
		deep,
	} {
		if _, err := ValidateManifest(raw); err == nil {
			t.Error("deeply nested input must be rejected")
		}
	}
}

func TestManifestV2_Palettes(t *testing.T) {
	long := strings.Repeat("a", 41)
	runV2Cases(t, []v2Case{
		{name: "name empty", fn: func(m obj) { palAt(m, 0)["name"] = "" }, code: "invalid_length", path: "palettes[0].name"},
		{name: "name 41 runes", fn: func(m obj) { palAt(m, 0)["name"] = long }, code: "invalid_length"},
		{name: "name control char", fn: func(m obj) { palAt(m, 0)["name"] = "Iv\u0000ory" }, code: "invalid_value", path: "palettes[0].name"},
		{name: "name bidi override", fn: func(m obj) { palAt(m, 0)["name"] = "Iv" + string(rune(0x202E)) + "ory" }, code: "invalid_value"},
		{name: "name zero-width", fn: func(m obj) { palAt(m, 0)["name"] = "Iv" + string(rune(0x200B)) + "ory" }, code: "invalid_value"},
		{name: "accent_ink bad hex", fn: func(m obj) { palAt(m, 0)["accent_ink"] = "#FFF" }, code: "invalid_color", path: "palettes[0].accent_ink"},
		{name: "accent_ink low contrast", fn: func(m obj) { palAt(m, 0)["accent_ink"] = "#C9C0A8" }, code: "low_contrast", path: "palettes[0].accent_ink"},
		{name: "art 9 colours", fn: func(m obj) {
			palAt(m, 0)["art"] = []string{"#111111", "#111111", "#111111", "#111111", "#111111", "#111111", "#111111", "#111111", "#111111"}
		}, code: "too_many", path: "palettes[0].art"},
		{name: "art bad hex", fn: func(m obj) { palAt(m, 0)["art"].([]any)[2] = "red" }, code: "invalid_color", path: "palettes[0].art[2]"},
		{name: "foil 2 stops", fn: func(m obj) { palAt(m, 0)["foil"] = []string{"#111111", "#222222"} }, code: "invalid_length"},
		{name: "foil 6 stops", fn: func(m obj) {
			palAt(m, 0)["foil"] = []string{"#111111", "#222222", "#333333", "#444444", "#555555", "#666666"}
		}, code: "too_many"},
		{name: "foil bad hex", fn: func(m obj) { palAt(m, 0)["foil"].([]any)[1] = "#GGGGGG" }, code: "invalid_color"},
		{name: "foil used but a palette lacks it", fn: func(m obj) { delete(palAt(m, 1), "foil") }, code: "missing_palette_color", path: "palettes[1].foil"},
		{name: "art token beyond a palette's art", fn: func(m obj) { palAt(m, 2)["art"] = []string{"#111111", "#222222", "#333333"} }, code: "missing_palette_color", path: "palettes[2].art"},
		{name: "palette colour not hex", fn: func(m obj) { colorsAt(m, 0)["text"] = "var(--x)" }, code: "invalid_color"},
		{name: "palette colour with newline", fn: func(m obj) { colorsAt(m, 0)["text"] = "#FFFFFF\n" }, code: "invalid_color"},
		{name: "palette colour 8 digits", fn: func(m obj) { colorsAt(m, 0)["text"] = "#FFFFFFFF" }, code: "invalid_color"},
		{name: "ten palettes", fn: func(m obj) {
			p := palAt(m, 0)
			ps := make([]any, 9)
			for i := range ps {
				c := obj{}
				for k, v := range p {
					c[k] = v
				}
				c["id"] = fmt.Sprintf("p%d", i)
				ps[i] = c
			}
			m["palettes"] = ps
		}, code: "invalid_length"},
	})
}

func TestManifestV2_Contrast(t *testing.T) {
	runV2Cases(t, []v2Case{
		{name: "text on background", fn: func(m obj) { colorsAt(m, 0)["text"] = "#EDE8DC" }, code: "low_contrast", path: "palettes[0].colors"},
		{name: "muted on background (4.5 not 3)", fn: func(m obj) { colorsAt(m, 0)["muted"] = "#8A9486" }, code: "low_contrast", path: "palettes[0].colors"},
		{name: "muted on surface only", fn: func(m obj) {
			colorsAt(m, 0)["muted"] = "#767B72"
			colorsAt(m, 0)["background"] = "#F3EEE3"
			colorsAt(m, 0)["surface"] = "#E4DFD0"
		}, code: "low_contrast"},
		{name: "accent text on accent", fn: func(m obj) { colorsAt(m, 0)["accent_text"] = "#6F8A74" }, code: "low_contrast"},
		{name: "glass blend", base: giltNoir, fn: func(m obj) {
			m["card"] = obj{"style": "glass", "border": "hairline", "fields": "boxed", "buttons": "accent"}
			c := colorsAt(m, 0)
			c["background"], c["surface"], c["text"], c["muted"] = "#000000", "#FFFFFF", "#767676", "#767676"
			c["accent_text"] = "#000000"
		}, code: "low_contrast"},
		{name: "glass blend vs explicit accent_ink", fn: func(m obj) {
			m["card"] = obj{"style": "glass"}
			c := colorsAt(m, 0)
			c["background"], c["surface"], c["text"], c["muted"] = "#101010", "#FFFFFF", "#2C382D", "#2C382D"
			palAt(m, 0)["accent_ink"] = "#767676"
		}, code: "low_contrast"},
		{name: "hero_ink on page hero", fn: func(m obj) { m["ornament"].(obj)["hero_ink"] = "art5" }, code: "low_contrast", path: "ornament.hero_ink"},
		{name: "hero_ink on color_block hero", base: sprinkles, fn: func(m obj) { m["ornament"].(obj)["hero_ink"] = "accent" }, code: "low_contrast", path: "ornament.hero_ink"},
		{name: "foil_numeral stop too faint", base: giltNoir, fn: func(m obj) {
			// Light page + pale foil stop: below 3:1 on the hero backdrop.
			c := colorsAt(m, 0)
			c["background"], c["surface"], c["text"], c["muted"], c["accent"], c["accent_text"] = "#FBF8F1", "#FFFFFF", "#1E1B14", "#4D473A", "#5A4A20", "#FFFFFF"
			palAt(m, 0)["foil"] = []string{"#F3E2AA", "#E6CC8B", "#F8EBC0"}
			m["card"] = obj{"style": "soft"}
			m["ornament"].(obj)["badge"] = "none"
		}, code: "low_contrast", path: "ornament.hero"},
		{name: "foil buttons text contrast", base: giltNoir, fn: func(m obj) {
			colorsAt(m, 0)["accent_text"] = "#C9A45C" // equals a foil stop
			colorsAt(m, 0)["accent"] = "#0E0D0A"
		}, code: "low_contrast", path: "card.buttons"},
	})
}

func TestManifestV2_OrnamentCardMotion(t *testing.T) {
	runV2Cases(t, []v2Case{
		{name: "ornament hero unknown", fn: func(m obj) { m["ornament"].(obj)["hero"] = "laurel" }, code: "invalid_value", path: "ornament.hero"},
		{name: "ornament divider unknown", fn: func(m obj) { m["ornament"].(obj)["divider"] = "zigzag" }, code: "invalid_value", path: "ornament.divider"},
		{name: "ornament badge unknown", fn: func(m obj) { m["ornament"].(obj)["badge"] = "ribbon" }, code: "invalid_value", path: "ornament.badge"},
		{name: "ornament ampersand unknown", fn: func(m obj) { m["ornament"].(obj)["ampersand"] = "fancy" }, code: "invalid_value", path: "ornament.ampersand"},
		{name: "hero_ink hex in token slot", fn: func(m obj) { m["ornament"].(obj)["hero_ink"] = "#FFFFFF" }, code: "unknown_token", path: "ornament.hero_ink"},
		{name: "hero_ink art9", fn: func(m obj) { m["ornament"].(obj)["hero_ink"] = "art9" }, code: "unknown_token"},
		{name: "hero_ink art7 beyond palette art", fn: func(m obj) { m["ornament"].(obj)["hero_ink"] = "art7" }, code: "missing_palette_color"},
		{name: "card style unknown", fn: func(m obj) { m["card"].(obj)["style"] = "neon" }, code: "invalid_value", path: "card.style"},
		{name: "card border unknown", fn: func(m obj) { m["card"].(obj)["border"] = "dashed" }, code: "invalid_value", path: "card.border"},
		{name: "card fields unknown", fn: func(m obj) { m["card"].(obj)["fields"] = "pill" }, code: "invalid_value", path: "card.fields"},
		{name: "card buttons unknown", fn: func(m obj) { m["card"].(obj)["buttons"] = "ghost" }, code: "invalid_value", path: "card.buttons"},
		{name: "card radius 33", fn: func(m obj) { m["card"].(obj)["radius"] = 33 }, code: "out_of_range", path: "card.radius"},
		{name: "card radius -1", fn: func(m obj) { m["card"].(obj)["radius"] = -1 }, code: "out_of_range"},
		{name: "chamfered with radius", base: giltNoir, fn: func(m obj) { m["card"].(obj)["radius"] = 4 }, code: "invalid_combination", path: "card.radius"},
		{name: "foil border without foil", base: sprinkles, fn: func(m obj) { m["card"].(obj)["border"] = "foil" }, code: "missing_palette_color"},
		{name: "foil_inset border without foil", base: sprinkles, fn: func(m obj) { m["card"].(obj)["border"] = "foil_inset" }, code: "missing_palette_color"},
		{name: "foil buttons without foil", base: sprinkles, fn: func(m obj) { m["card"].(obj)["buttons"] = "foil" }, code: "missing_palette_color"},
		{name: "foil_sheen without foil", base: sprinkles, fn: func(m obj) { m["motion"] = "foil_sheen" }, code: "missing_palette_color"},
		{name: "foil_numeral without foil", base: sprinkles, fn: func(m obj) { m["ornament"].(obj)["hero"] = "foil_numeral" }, code: "missing_palette_color"},
		{name: "motion unknown", fn: func(m obj) { m["motion"] = "wobble" }, code: "invalid_value", path: "motion"},
	})
}

// --- layers ---

func TestManifestV2_Layers(t *testing.T) {
	art := func(extra obj) obj {
		l := obj{"kind": "art", "art": "balloons", "placement": "hero_top", "colors": []string{"art1", "art2"}}
		for k, v := range extra {
			l[k] = v
		}
		return l
	}
	withLayers := func(ls ...obj) func(m obj) { return func(m obj) { setLayers(m, ls...) } }
	paper := obj{"kind": "paper"}
	cases := []v2Case{
		// kind / envelope
		{name: "unknown kind", fn: withLayers(obj{"kind": "sparkle"}), code: "invalid_value", path: "layers[0].kind"},
		{name: "empty kind", fn: withLayers(obj{"kind": ""}), code: "invalid_value"},
		{name: "src in manifest", fn: withLayers(paper, obj{"kind": "image", "opacity": 0.2, "src": "/media/x/background"}), code: "invalid_field", path: "layers[1].src"},
		{name: "src on a paper", fn: withLayers(obj{"kind": "paper", "src": "x"}), code: "invalid_field", path: "layers[0].src"},
		{name: "8 layers", fn: func(m obj) {
			ls := []any{paper}
			for i := 0; i < 7; i++ {
				ls = append(ls, obj{"kind": "pattern", "pattern": "dots", "color": "text", "opacity": 0.1})
			}
			m["layers"] = ls
		}, code: "too_many", path: "layers"},
		{name: "region unknown", fn: withLayers(obj{"kind": "paper", "region": "footer"}), code: "invalid_value", path: "layers[0].region"},
		{name: "blend unknown", fn: withLayers(paper, obj{"kind": "texture", "texture": "grain", "opacity": 0.1, "blend": "difference"}), code: "invalid_value", path: "layers[1].blend"},
		{name: "4 blend layers", fn: withLayers(paper,
			obj{"kind": "texture", "texture": "grain", "opacity": 0.1},
			obj{"kind": "texture", "texture": "fibers", "opacity": 0.1},
			obj{"kind": "pattern", "pattern": "dots", "color": "text", "opacity": 0.1, "blend": "overlay"},
			obj{"kind": "pattern", "pattern": "grid", "color": "text", "opacity": 0.1, "blend": "overlay"}),
			code: "too_many", path: "layers[4].blend"},

		// paper
		{name: "paper not first", fn: withLayers(obj{"kind": "texture", "texture": "grain", "opacity": 0.1}, paper), code: "invalid_combination", path: "layers[1]"},
		{name: "two papers", fn: withLayers(paper, paper), code: "too_many", path: "layers[1]"},
		{name: "paper region hero", fn: withLayers(obj{"kind": "paper", "region": "hero"}), code: "invalid_combination", path: "layers[0].region"},
		{name: "paper tone unknown token", fn: withLayers(obj{"kind": "paper", "tone": "art9"}), code: "unknown_token", path: "layers[0].tone"},
		{name: "paper glow hex in token slot", fn: withLayers(obj{"kind": "paper", "glow": "#FFFFFF"}), code: "unknown_token"},
		{name: "paper glow trailing space", fn: withLayers(obj{"kind": "paper", "glow": "accent "}), code: "unknown_token"},
		{name: "paper glow_opacity without glow", fn: withLayers(obj{"kind": "paper", "glow_opacity": 0.5}), code: "invalid_field", path: "layers[0].glow_opacity"},
		{name: "paper glow_opacity 1.01", fn: withLayers(obj{"kind": "paper", "glow": "art1", "glow_opacity": 1.01}), code: "out_of_range", path: "layers[0].glow_opacity"},
		{name: "paper vignette_opacity -0.01", fn: withLayers(obj{"kind": "paper", "vignette": "art1", "vignette_opacity": -0.01}), code: "out_of_range"},
		{name: "paper with edge", fn: withLayers(obj{"kind": "paper", "edge": "wave"}), code: "invalid_field", path: "layers[0].edge"},

		// block
		{name: "block without color_block", fn: withLayers(paper, obj{"kind": "block"}), code: "invalid_combination"},
		{name: "block edge unknown", base: sprinkles, fn: func(m obj) { layerAt(m, 1)["edge"] = "zigzag" }, code: "invalid_value", path: "layers[1].edge"},
		{name: "block region page", base: sprinkles, fn: func(m obj) { layerAt(m, 1)["region"] = "page" }, code: "invalid_combination", path: "layers[1].region"},
		{name: "block with colour", base: sprinkles, fn: func(m obj) { layerAt(m, 1)["color"] = "accent" }, code: "invalid_field", path: "layers[1].color"},
		{name: "two blocks", base: sprinkles, fn: func(m obj) { m["layers"] = append(layersOf(m), obj{"kind": "block"}) }, code: "too_many"},
		{name: "block blend", base: sprinkles, fn: func(m obj) { layerAt(m, 1)["blend"] = "multiply" }, code: "invalid_field"},
		{name: "color_block without a block", base: sprinkles, fn: func(m obj) {
			setLayers(m, obj{"kind": "paper"})
		}, code: "invalid_combination", path: "layers"},

		// pattern
		{name: "pattern unknown", fn: withLayers(paper, obj{"kind": "pattern", "pattern": "plaid", "color": "text", "opacity": 0.1}), code: "invalid_value", path: "layers[1].pattern"},
		{name: "pattern empty", fn: withLayers(paper, obj{"kind": "pattern", "pattern": "", "color": "text", "opacity": 0.1}), code: "invalid_value"},
		{name: "pattern no color", fn: withLayers(paper, obj{"kind": "pattern", "pattern": "dots", "opacity": 0.1}), code: "missing_value", path: "layers[1].color"},
		{name: "pattern no opacity", fn: withLayers(paper, obj{"kind": "pattern", "pattern": "dots", "color": "text"}), code: "missing_value", path: "layers[1].opacity"},
		{name: "pattern opacity 0.36", fn: withLayers(paper, obj{"kind": "pattern", "pattern": "dots", "color": "text", "opacity": 0.36}), code: "out_of_range"},
		{name: "pattern opacity 0.3500001", fn: withLayers(paper, obj{"kind": "pattern", "pattern": "dots", "color": "text", "opacity": 0.3500001}), code: "out_of_range"},
		{name: "pattern opacity 0.009", fn: withLayers(paper, obj{"kind": "pattern", "pattern": "dots", "color": "text", "opacity": 0.009}), code: "out_of_range"},
		{name: "pattern opacity 1e308", fn: withLayers(paper, obj{"kind": "pattern", "pattern": "dots", "color": "text", "opacity": 1e308}), code: "out_of_range"},
		{name: "pattern opacity -1e308", fn: withLayers(paper, obj{"kind": "pattern", "pattern": "dots", "color": "text", "opacity": -1e308}), code: "out_of_range"},
		{name: "pattern origin unknown", fn: withLayers(paper, obj{"kind": "pattern", "pattern": "dots", "color": "text", "opacity": 0.1, "origin": "middle"}), code: "invalid_value", path: "layers[1].origin"},
		{name: "pattern mask unknown", fn: withLayers(paper, obj{"kind": "pattern", "pattern": "dots", "color": "text", "opacity": 0.1, "mask": "diagonal"}), code: "invalid_value", path: "layers[1].mask"},
		{name: "pattern colour is a hex", fn: withLayers(paper, obj{"kind": "pattern", "pattern": "dots", "color": "#FFFFFF", "opacity": 0.1}), code: "unknown_token"},
		{name: "pattern with colors", fn: withLayers(paper, obj{"kind": "pattern", "pattern": "dots", "color": "text", "opacity": 0.1, "colors": []string{"text"}}), code: "invalid_field"},
		{name: "4 patterns", fn: withLayers(paper,
			obj{"kind": "pattern", "pattern": "dots", "color": "text", "opacity": 0.1},
			obj{"kind": "pattern", "pattern": "grid", "color": "text", "opacity": 0.1},
			obj{"kind": "pattern", "pattern": "stripes", "color": "text", "opacity": 0.1},
			obj{"kind": "pattern", "pattern": "gingham", "color": "text", "opacity": 0.1}),
			code: "too_many", path: "layers[4]"},

		// texture
		{name: "texture unknown", fn: withLayers(paper, obj{"kind": "texture", "texture": "Grain", "opacity": 0.1}), code: "invalid_value", path: "layers[1].texture"},
		{name: "texture trailing space", fn: withLayers(paper, obj{"kind": "texture", "texture": "grain ", "opacity": 0.1}), code: "invalid_value"},
		{name: "texture opacity 0.41", fn: withLayers(paper, obj{"kind": "texture", "texture": "grain", "opacity": 0.41}), code: "out_of_range"},
		{name: "texture opacity 0.01", fn: withLayers(paper, obj{"kind": "texture", "texture": "grain", "opacity": 0.01}), code: "out_of_range"},
		{name: "texture no opacity", fn: withLayers(paper, obj{"kind": "texture", "texture": "grain"}), code: "missing_value"},
		{name: "watercolour with 2 colours", fn: withLayers(paper, obj{"kind": "texture", "texture": "watercolour", "opacity": 0.1, "colors": []string{"art1", "art2"}}), code: "invalid_combination", path: "layers[1].colors"},
		{name: "watercolour with none", fn: withLayers(paper, obj{"kind": "texture", "texture": "watercolour", "opacity": 0.1}), code: "invalid_combination"},
		{name: "grain with colours", fn: withLayers(paper, obj{"kind": "texture", "texture": "grain", "opacity": 0.1, "colors": []string{"art1"}}), code: "invalid_combination"},
		{name: "watercolour bad token", fn: withLayers(paper, obj{"kind": "texture", "texture": "watercolour", "opacity": 0.1, "colors": []string{"art1", "art2", "blue"}}), code: "unknown_token", path: "layers[1].colors[2]"},
		{name: "foil texture without palette foil", base: sprinkles, fn: withLayers(paper, obj{"kind": "texture", "texture": "foil", "opacity": 0.1}), code: "missing_palette_color"},
		{name: "3 textures", fn: withLayers(paper,
			obj{"kind": "texture", "texture": "grain", "opacity": 0.1},
			obj{"kind": "texture", "texture": "fibers", "opacity": 0.1},
			obj{"kind": "texture", "texture": "linen", "opacity": 0.1}), code: "too_many"},
		{name: "texture with pattern field", fn: withLayers(paper, obj{"kind": "texture", "texture": "grain", "opacity": 0.1, "pattern": "dots"}), code: "invalid_field", path: "layers[1].pattern"},

		// art
		{name: "art unknown", fn: withLayers(paper, art(obj{"art": "unicorns"})), code: "invalid_value", path: "layers[1].art"},
		{name: "art empty", fn: withLayers(paper, art(obj{"art": ""})), code: "invalid_value"},
		{name: "placement unknown", fn: withLayers(paper, art(obj{"placement": "everywhere"})), code: "invalid_value", path: "layers[1].placement"},
		{name: "placement not allowed for key", fn: withLayers(paper, art(obj{"placement": "scatter"})), code: "invalid_combination", path: "layers[1].placement"},
		{name: "density on balloons", fn: withLayers(paper, art(obj{"density": "dense"})), code: "invalid_field", path: "layers[1].density"},
		{name: "density unknown", fn: withLayers(paper, obj{"kind": "art", "art": "confetti", "placement": "hero", "colors": []string{"art1", "art2"}, "density": "heavy"}), code: "invalid_value"},
		{name: "too few colours", fn: withLayers(paper, art(obj{"colors": []string{"art1"}})), code: "invalid_length", path: "layers[1].colors"},
		{name: "too many colours", fn: withLayers(paper, art(obj{"colors": []string{"art1", "art2", "art3", "art4", "art1"}})), code: "too_many"},
		{name: "7 colours on confetti", fn: withLayers(paper, obj{"kind": "art", "art": "confetti", "placement": "hero",
			"colors": []string{"art1", "art2", "art3", "art4", "art5", "art6", "art1"}}), code: "too_many"},
		{name: "paint on non-foil art", fn: withLayers(paper, obj{"kind": "art", "art": "balloons", "placement": "hero_top", "paint": "foil"}), code: "invalid_combination", path: "layers[1].paint"},
		{name: "paint unknown", fn: withLayers(paper, obj{"kind": "art", "art": "stepped_arches", "placement": "hero", "paint": "gold"}), code: "invalid_value"},
		{name: "colors and paint", fn: withLayers(paper, obj{"kind": "art", "art": "sparkles", "placement": "scatter", "paint": "foil", "colors": []string{"art1"}}), code: "invalid_combination"},
		{name: "art opacity 0.05", fn: withLayers(paper, art(obj{"opacity": 0.05})), code: "out_of_range"},
		{name: "art opacity 1.01", fn: withLayers(paper, art(obj{"opacity": 1.01})), code: "out_of_range"},
		{name: "art colour token unknown", fn: withLayers(paper, art(obj{"colors": []string{"art1", "art0"}})), code: "unknown_token", path: "layers[1].colors[1]"},
		{name: "art paint foil without palette foil", base: sprinkles, fn: withLayers(paper, obj{"kind": "art", "art": "sparkles", "placement": "scatter", "paint": "foil"}), code: "missing_palette_color"},
		{name: "4 art layers", fn: withLayers(paper, art(nil), art(nil), art(nil), art(nil)), code: "too_many"},
		{name: "art with edge", fn: withLayers(paper, art(obj{"edge": "wave"})), code: "invalid_field"},

		// frame
		{name: "frame unknown", fn: withLayers(paper, obj{"kind": "frame", "frame": "ornate", "color": "accent"}), code: "invalid_value", path: "layers[1].frame"},
		{name: "frame no colour or paint", fn: withLayers(paper, obj{"kind": "frame", "frame": "single_hairline"}), code: "missing_value", path: "layers[1].color"},
		{name: "frame both colour and paint", fn: withLayers(paper, obj{"kind": "frame", "frame": "single_hairline", "color": "accent", "paint": "foil"}), code: "invalid_combination"},
		{name: "frame inset 33", fn: withLayers(paper, obj{"kind": "frame", "frame": "single_hairline", "color": "accent", "inset": 33}), code: "out_of_range", path: "layers[1].inset"},
		{name: "frame inset -1", fn: withLayers(paper, obj{"kind": "frame", "frame": "single_hairline", "color": "accent", "inset": -1}), code: "out_of_range"},
		{name: "frame region hero", fn: withLayers(paper, obj{"kind": "frame", "frame": "single_hairline", "color": "accent", "region": "hero"}), code: "invalid_combination"},
		{name: "two frames", fn: withLayers(paper,
			obj{"kind": "frame", "frame": "single_hairline", "color": "accent"},
			obj{"kind": "frame", "frame": "scallop", "color": "accent"}), code: "too_many", path: "layers[2]"},
		{name: "frame colour hex", fn: withLayers(paper, obj{"kind": "frame", "frame": "single_hairline", "color": "#112233"}), code: "unknown_token"},

		// image
		{name: "image no opacity", fn: withLayers(paper, obj{"kind": "image"}), code: "missing_value", path: "layers[1].opacity"},
		{name: "image opacity 0.04", fn: withLayers(paper, obj{"kind": "image", "opacity": 0.04}), code: "out_of_range"},
		{name: "image opacity 1.1", fn: withLayers(paper, obj{"kind": "image", "opacity": 1.1}), code: "out_of_range"},
		{name: "two images", fn: withLayers(paper, obj{"kind": "image", "opacity": 0.2}, obj{"kind": "image", "opacity": 0.2}), code: "too_many"},
		{name: "image with colour", fn: withLayers(paper, obj{"kind": "image", "opacity": 0.2, "color": "text"}), code: "invalid_field"},
	}
	runV2Cases(t, cases)
}

// Hostile strings in enum and token slots must be rejected as invalid_value /
// unknown_token and never accepted.
func TestManifestV2_HostileStrings(t *testing.T) {
	hostile := []string{
		"", "Grain", "grain ", " grain", "grain;background:url(//x)", "url(javascript:alert(1))",
		"</style><script>alert(1)</script>", "expression(1)", "grаin" /* Cyrillic a */, "gra\u0000in",
		strings.Repeat("a", 10<<10),
	}
	enumSlots := map[string]func(m obj, v string){
		"texture": func(m obj, v string) {
			setLayers(m, obj{"kind": "paper"}, obj{"kind": "texture", "texture": v, "opacity": 0.1})
		},
		"pattern": func(m obj, v string) {
			setLayers(m, obj{"kind": "paper"}, obj{"kind": "pattern", "pattern": v, "color": "text", "opacity": 0.1})
		},
		"frame": func(m obj, v string) {
			setLayers(m, obj{"kind": "paper"}, obj{"kind": "frame", "frame": v, "color": "text"})
		},
		"art": func(m obj, v string) {
			setLayers(m, obj{"kind": "paper"}, obj{"kind": "art", "art": v, "placement": "corners"})
		},
		"placement": func(m obj, v string) {
			setLayers(m, obj{"kind": "paper"}, obj{"kind": "art", "art": "olive_branches", "placement": v})
		},
		"ornament.hero":  func(m obj, v string) { m["ornament"].(obj)["hero"] = v },
		"ornament.badge": func(m obj, v string) { m["ornament"].(obj)["badge"] = v },
		"card.style":     func(m obj, v string) { m["card"].(obj)["style"] = v },
		"card.border":    func(m obj, v string) { m["card"].(obj)["border"] = v },
		"motion":         func(m obj, v string) { m["motion"] = v },
		"layout":         func(m obj, v string) { m["layout"] = v },
		"hero_style":     func(m obj, v string) { m["hero_style"] = v },
		"font heading":   func(m obj, v string) { m["fonts"].([]any)[0].(obj)["heading"] = v },
	}
	for slot, set := range enumSlots {
		for i, v := range hostile {
			// "" is the default for the optional slots (ornament/card/motion),
			// so it is not an error there.
			if v == "" && (strings.HasPrefix(slot, "ornament.") || strings.HasPrefix(slot, "card.") || slot == "motion") {
				continue
			}
			t.Run(fmt.Sprintf("%s/%d", slot, i), func(t *testing.T) {
				_, err := ValidateManifest(mutated(t, oliveGrove(), func(m obj) { set(m, v) }))
				if !hasIssueCode(err, "invalid_value") {
					t.Fatalf("want invalid_value, got %v", err)
				}
			})
		}
	}
	tokenSlots := map[string]func(m obj, v string){
		"tone": func(m obj, v string) { setLayers(m, obj{"kind": "paper", "tone": v}) },
		"glow": func(m obj, v string) { setLayers(m, obj{"kind": "paper", "glow": v}) },
		"color": func(m obj, v string) {
			setLayers(m, obj{"kind": "paper"}, obj{"kind": "pattern", "pattern": "dots", "color": v, "opacity": 0.1})
		},
		"hero_ink": func(m obj, v string) { m["ornament"].(obj)["hero_ink"] = v },
		"colors[0]": func(m obj, v string) {
			setLayers(m, obj{"kind": "paper"}, obj{"kind": "art", "art": "olive_branches", "placement": "corners", "colors": []string{v, "art1", "art2"}})
		},
	}
	tokens := []string{"art0", "art9", "art10", "#FFFFFF", "#FFF", "red", "var(--x)", "accent ", " accent", "Accent", "rgb(0,0,0)", "url(x)", "</style>", "ac\u0000cent"}
	for slot, set := range tokenSlots {
		for i, v := range tokens {
			t.Run(fmt.Sprintf("%s/%d", slot, i), func(t *testing.T) {
				_, err := ValidateManifest(mutated(t, oliveGrove(), func(m obj) { set(m, v) }))
				if !hasIssueCode(err, "unknown_token") {
					t.Fatalf("want unknown_token for %q, got %v", v, err)
				}
			})
		}
	}
}

func TestManifestV2_HexInvalid(t *testing.T) {
	for _, v := range []string{"#FFF", "#GGGGGG", "red", "#FFFFFF;", "#FFFFFF\n", "rgb(0,0,0)", "var(--x)", "#FFFFFFFF", ""} {
		t.Run(fmt.Sprintf("%q", v), func(t *testing.T) {
			_, err := ValidateManifest(mutated(t, oliveGrove(), func(m obj) { palAt(m, 0)["foil"].([]any)[0] = v }))
			if !hasIssueCode(err, "invalid_color") {
				t.Fatalf("want invalid_color, got %v", err)
			}
		})
	}
}

// NaN can't be produced by JSON, but the bounds helpers must still reject it
// if a future caller feeds one in.
func TestManifestV2_NaNFailsBounds(t *testing.T) {
	nan := math.NaN()
	if !outOfRange(nan, 0, 1) {
		t.Error("outOfRange must reject NaN")
	}
	iss := &issues{}
	l := &Layer{Kind: kindPattern, Pattern: "dots", Color: "text", Opacity: &nan}
	validateLayer(iss, "layers[1]", 1, l, nil)
	if !hasIssueCode(iss.err(), "out_of_range") {
		t.Errorf("NaN opacity: want out_of_range, got %v", iss.err())
	}
	inf := math.Inf(1)
	l = &Layer{Kind: kindImage, Opacity: &inf}
	iss = &issues{}
	validateLayer(iss, "layers[1]", 1, l, nil)
	if !hasIssueCode(iss.err(), "out_of_range") {
		t.Errorf("+Inf opacity: want out_of_range, got %v", iss.err())
	}
}

func TestManifestV2_ImageLayerOK(t *testing.T) {
	raw := mutated(t, oliveGrove(), func(m obj) {
		m["layers"] = append(layersOf(m), obj{"kind": "image", "opacity": 0.3, "blend": "soft-light"})
	})
	if _, err := ValidateManifest(raw); err != nil {
		t.Fatalf("image layer should be valid: %v", err)
	}
}

// --- fonts ---

func TestManifestV2_NewFonts(t *testing.T) {
	for _, f := range []string{"bodoni_moda", "josefin_sans"} {
		for _, role := range []string{"heading", "body", "accent"} {
			t.Run(f+"/"+role, func(t *testing.T) {
				m := validManifestMap()
				pair := obj{"id": "classic", "name": "Classic", "heading": "playfair_display", "body": "lora"}
				pair[role] = f
				m["fonts"] = []obj{pair}
				if _, err := ValidateManifest(mustJSON(t, m)); err != nil {
					t.Errorf("%s as %s should be valid: %v", f, role, err)
				}
			})
		}
	}
	for _, f := range []string{"pinyon_script", "bagel_fat_one", "limelight"} {
		t.Run(f+"/heading_accent_ok", func(t *testing.T) {
			m := validManifestMap()
			m["fonts"] = []obj{{"id": "classic", "name": "Classic", "heading": f, "body": "lora", "accent": f}}
			if _, err := ValidateManifest(mustJSON(t, m)); err != nil {
				t.Errorf("%s should be valid as heading/accent: %v", f, err)
			}
		})
		t.Run(f+"/body_rejected", func(t *testing.T) {
			m := validManifestMap()
			m["fonts"] = []obj{{"id": "classic", "name": "Classic", "heading": "lora", "body": f}}
			if _, err := ValidateManifest(mustJSON(t, m)); !hasIssueCode(err, "invalid_value") {
				t.Errorf("%s as body: want invalid_value, got %v", f, err)
			}
		})
	}
	// Gloock is not a manifest font.
	m := validManifestMap()
	m["fonts"] = []obj{{"id": "classic", "name": "Classic", "heading": "gloock", "body": "lora"}}
	if _, err := ValidateManifest(mustJSON(t, m)); !hasIssueCode(err, "invalid_value") {
		t.Errorf("gloock: want invalid_value, got %v", err)
	}
}

// --- registry ---

func TestArtRegistry(t *testing.T) {
	if n := len(artRegistry); n > maxArtKeys {
		t.Fatalf("art registry has %d keys, hard cap is %d", n, maxArtKeys)
	}
	if len(artRegistry) != 14 {
		t.Errorf("art registry has %d keys, spec lists 14", len(artRegistry))
	}
	for key, spec := range artRegistry {
		t.Run(key, func(t *testing.T) {
			if !manifestIDRe.MatchString(key) {
				t.Errorf("key %q is not a safe identifier", key)
			}
			if spec.minColors < 0 || spec.maxColors < spec.minColors || spec.maxColors > maxPaletteArt {
				t.Errorf("bad colour range %d-%d", spec.minColors, spec.maxColors)
			}
			if len(spec.placements) == 0 {
				t.Error("no placements")
			}
			// Every placement of every key validates with the minimum colours.
			for pl := range spec.placements {
				colors := make([]string, spec.minColors)
				for i := range colors {
					colors[i] = fmt.Sprintf("art%d", i+1)
				}
				l := obj{"kind": "art", "art": key, "placement": pl, "colors": colors}
				raw := mutated(t, oliveGrove(), func(m obj) { setLayers(m, obj{"kind": "paper"}, l) })
				if _, err := ValidateManifest(raw); err != nil {
					t.Errorf("placement %s: %v", pl, err)
				}
				// And maxColors is accepted, maxColors+1 is not.
				colors = make([]string, spec.maxColors+1)
				for i := range colors {
					colors[i] = fmt.Sprintf("art%d", i%6+1)
				}
				l["colors"] = colors[:spec.maxColors]
				if spec.maxColors > 0 {
					raw = mutated(t, oliveGrove(), func(m obj) { setLayers(m, obj{"kind": "paper"}, l) })
					if _, err := ValidateManifest(raw); err != nil {
						t.Errorf("placement %s max colours: %v", pl, err)
					}
				}
				l["colors"] = colors
				raw = mutated(t, oliveGrove(), func(m obj) { setLayers(m, obj{"kind": "paper"}, l) })
				if _, err := ValidateManifest(raw); !hasIssueCode(err, "too_many") {
					t.Errorf("placement %s: %d colours should be too_many, got %v", pl, len(colors), err)
				}
			}
			// foil paint accepted exactly when the key is foil-capable.
			pl := ""
			for p := range spec.placements {
				pl = p
				break
			}
			raw := mutated(t, oliveGrove(), func(m obj) {
				setLayers(m, obj{"kind": "paper"}, obj{"kind": "art", "art": key, "placement": pl, "paint": "foil"})
			})
			_, err := ValidateManifest(raw)
			if spec.foil && err != nil {
				t.Errorf("foil-capable key rejected foil: %v", err)
			}
			if !spec.foil && !hasIssueCode(err, "invalid_combination") {
				t.Errorf("non-foil key accepted foil: %v", err)
			}
			// density accepted exactly when the key supports it.
			colors := make([]string, spec.minColors)
			for i := range colors {
				colors[i] = fmt.Sprintf("art%d", i+1)
			}
			raw = mutated(t, oliveGrove(), func(m obj) {
				setLayers(m, obj{"kind": "paper"}, obj{"kind": "art", "art": key, "placement": pl, "colors": colors, "density": "sparse"})
			})
			_, err = ValidateManifest(raw)
			if spec.density && err != nil {
				t.Errorf("density key rejected density: %v", err)
			}
			if !spec.density && !hasIssueCode(err, "invalid_field") {
				t.Errorf("non-density key accepted density: %v", err)
			}
		})
	}
}

// --- v1 behaviour ---

func TestManifestV1_UnchangedByV2(t *testing.T) {
	// A v1 manifest's canonical bytes carry none of the v2 keys.
	m, err := ValidateManifest(mustJSON(t, validManifestMap()))
	if err != nil {
		t.Fatal(err)
	}
	canon, _ := json.Marshal(m)
	for _, key := range []string{`"layers"`, `"ornament"`, `"card"`, `"motion"`, `"accent_ink"`, `"art"`, `"foil"`} {
		if strings.Contains(string(canon), key) {
			t.Errorf("v1 canonical form contains %s: %s", key, canon)
		}
	}
	for _, key := range []string{`"decoration":"line"`, `"surface":"plain"`, `"texture":"none"`, `"heading_scale":"regular"`, `"background":null`} {
		if !strings.Contains(string(canon), key) {
			t.Errorf("v1 canonical form lost %s: %s", key, canon)
		}
	}
	// schema 3 and 0 are unsupported for v1-shaped input too.
	for _, s := range []int{0, 3} {
		bad := validManifestMap()
		bad["schema"] = s
		if _, err := ValidateManifest(mustJSON(t, bad)); !hasIssueCode(err, "unsupported_schema") {
			t.Errorf("schema %d: want unsupported_schema, got %v", s, err)
		}
	}
}
