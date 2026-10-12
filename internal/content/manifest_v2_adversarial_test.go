package content

// Adversarial layer for the schema-2 manifest validator and resolver: every
// enum, token, hex and numeric slot is attacked with hostile values, the
// cross-field rules are exercised one by one, and every accepted manifest is
// held to the idempotence, determinism and resolved-theme invariants the
// frontend relies on. Helpers are prefixed adv/Adv to stay clear of the
// first-pass tests in manifest_v2_test.go and theme_v2_test.go.

import (
	"bytes"
	"encoding/json"
	"fmt"
	"math"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"sync"
	"testing"
)

// --- helpers ---

func advMarshal(tb testing.TB, v any) []byte {
	tb.Helper()
	b, err := json.Marshal(v)
	if err != nil {
		tb.Fatalf("marshal: %v", err)
	}
	return b
}

// advMutate deep-copies base through JSON and applies fn to the copy.
func advMutate(tb testing.TB, base obj, fn func(m obj)) []byte {
	tb.Helper()
	var m obj
	if err := json.Unmarshal(advMarshal(tb, base), &m); err != nil {
		tb.Fatalf("copy: %v", err)
	}
	if fn != nil {
		fn(m)
	}
	return advMarshal(tb, m)
}

// advValidate runs ValidateManifest and turns a panic into a test failure.
func advValidate(t *testing.T, raw []byte) (m Manifest, err error) {
	t.Helper()
	defer func() {
		if r := recover(); r != nil {
			t.Fatalf("ValidateManifest panicked: %v (input %.200q)", r, raw)
		}
	}()
	return ValidateManifest(raw)
}

func advIssues(err error) []Issue {
	if ve, ok := err.(*ValidationError); ok {
		return ve.Issues
	}
	return nil
}

func advDump(err error) string {
	if err == nil {
		return "<nil>"
	}
	var sb strings.Builder
	for i, is := range advIssues(err) {
		if i == 8 {
			sb.WriteString(" ...")
			break
		}
		fmt.Fprintf(&sb, " [%s@%s]", is.Code, is.Path)
	}
	if sb.Len() == 0 {
		return fmt.Sprintf("non-validation error %T: %v", err, err)
	}
	return sb.String()
}

// advWant fails unless err holds an issue with this code (and path, when
// given).
func advWant(t *testing.T, err error, code, path string) {
	t.Helper()
	if err == nil {
		t.Fatalf("want %s@%s, got no error", code, path)
	}
	if _, ok := err.(*ValidationError); !ok {
		t.Fatalf("want a *ValidationError, got %T: %v", err, err)
	}
	if path == "" && hasIssueCode(err, code) {
		return
	}
	if path != "" && hasIssue(err, code, path) {
		return
	}
	t.Fatalf("want %s@%s, got%s", code, path, advDump(err))
}

// advCanon asserts raw is accepted and that its canonical form is stable
// (validate -> marshal -> validate -> marshal is byte-identical), returning
// the canonical bytes.
func advCanon(t *testing.T, raw []byte) []byte {
	t.Helper()
	m, err := advValidate(t, raw)
	if err != nil {
		t.Fatalf("want accepted, got%s", advDump(err))
	}
	c1 := advMarshal(t, m)
	m2, err := advValidate(t, c1)
	if err != nil {
		t.Fatalf("canonical form no longer validates:%s\n%.400s", advDump(err), c1)
	}
	c2 := advMarshal(t, m2)
	if !bytes.Equal(c1, c2) {
		t.Fatalf("canonical form is not idempotent:\n%.600s\n%.600s", c1, c2)
	}
	// Validating the same input twice is deterministic.
	m3, err := advValidate(t, raw)
	if err != nil || !bytes.Equal(c1, advMarshal(t, m3)) {
		t.Fatalf("validation is not deterministic")
	}
	return c1
}

// advCase is a table row: mutate base, then expect either acceptance
// (code == "") or an issue with code (and path when set).
type advCase struct {
	name string
	base func() obj
	fn   func(m obj)
	code string
	path string
}

func advRun(t *testing.T, cases []advCase) {
	t.Helper()
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			base := c.base
			if base == nil {
				base = oliveGrove
			}
			raw := advMutate(t, base(), c.fn)
			if c.code == "" {
				advCanon(t, raw)
				return
			}
			_, err := advValidate(t, raw)
			advWant(t, err, c.code, c.path)
		})
	}
}

// Slot setters for the JSON-round-tripped maps.
func advTop(k string) func(m obj, v string) { return func(m obj, v string) { m[k] = v } }
func advL(i int, k string) func(m obj, v string) {
	return func(m obj, v string) { layerAt(m, i)[k] = v }
}
func advIn(parent, k string) func(m obj, v string) {
	return func(m obj, v string) { m[parent].(obj)[k] = v }
}
func advFont(i int, k string) func(m obj, v string) {
	return func(m obj, v string) { m["fonts"].([]any)[i].(obj)[k] = v }
}
func advPalField(i int, k string) func(m obj, v string) {
	return func(m obj, v string) { palAt(m, i)[k] = v }
}
func advPalColor(i int, k string) func(m obj, v string) {
	return func(m obj, v string) { colorsAt(m, i)[k] = v }
}
func advPalList(i int, k string, j int) func(m obj, v string) {
	return func(m obj, v string) { palAt(m, i)[k].([]any)[j] = v }
}

// advSolo trims a v2 manifest to its first palette, so a contrast case does
// not depend on the other palettes.
func advSolo(m obj) {
	// The paper wash colours come from the palette's art ramp; a contrast
	// case sets its own palette colours, so drop the wash (it has its own
	// contrast tests) to keep the baseline independent of the art colours.
	if ls, ok := m["layers"].([]any); ok && len(ls) > 0 {
		if l, ok := ls[0].(obj); ok && l["kind"] == "paper" {
			delete(l, "glow")
			delete(l, "vignette")
		}
	}
	m["palettes"] = []any{palAt(m, 0)}
	m["defaults"] = obj{"palette": palAt(m, 0)["id"], "font": m["defaults"].(obj)["font"]}
}

// --- hostile value generators ---

func advTitle(s string) string {
	if s == "" {
		return s
	}
	return strings.ToUpper(s[:1]) + s[1:]
}

// advHomoglyph swaps the first Latin letter that has a Cyrillic twin.
func advHomoglyph(s string) string {
	twin := map[rune]rune{'a': 'а', 'e': 'е', 'o': 'о', 'c': 'с', 'p': 'р', 'x': 'х'}
	done := false
	out := strings.Map(func(r rune) rune {
		if t, ok := twin[r]; ok && !done {
			done = true
			return t
		}
		return r
	}, s)
	if !done {
		return "а" + s
	}
	return out
}

func advHostileEnums(valid string) []string {
	cands := []string{
		"", "definitely_unknown", advTitle(valid), strings.ToUpper(valid),
		valid + " ", " " + valid, valid + "\t", valid + "\n", "\u00a0" + valid,
		valid + ";background:url(//x)", "grain;background:url(//x)",
		"url(javascript:alert(1))", "javascript:alert(1)",
		"</style><script>alert(1)</script>", "expression(1)",
		advHomoglyph(valid), valid + "\x00", valid + "\u200b", "../" + valid,
		"__proto__", `"` + valid + `"`, valid + `\`,
		strings.Repeat("a", 10<<10),
	}
	return advDedupe(cands, valid)
}

func advHostileTokens(valid string) []string {
	cands := append(advHostileEnums(valid),
		"art0", "art9", "art10", "art-1", "art01", "ART1", "art1 ", " art1", "art 1",
		"accent ", "Accent_Text", "accent_ink ", "#FFFFFF", "#FFF", "red", "var(--x)", "rgb(0,0,0)",
	)
	return advDedupe(cands, valid)
}

func advHostileHex() []string {
	return []string{
		"", "#FFF", "#GGGGGG", "red", "#FFFFFF;", "#FFFFFF\n", "#FFFFFF\r\n", "rgb()", "rgb(0,0,0)", "var(--x)",
		"#FFFFFFFF", "#FFFFF", "FFFFFF", "##FFFFF", " #FFFFFF", "#FFFFFF ", "\t#FFFFFF", "#FFFFFF\t",
		"#１２３４５６", // fullwidth digits
		"#٠٠٠٠٠٠", // Arabic-Indic digits
		"#АВСDEF", // Cyrillic A, V, S lookalikes
		"#FFFFFF\x00", "#FFFFFF\u200b", "#ffffff\u0301",
		"url(javascript:alert(1))", "</style><script>", "expression(1)", "#FFFFFF;background:url(//x)",
		"#" + strings.Repeat("F", 10<<10), strings.Repeat("a", 10<<10),
		"transparent", "currentColor", "#FFFFFFFFFFFF",
	}
}

func advDedupe(in []string, drop string) []string {
	seen := map[string]bool{drop: true}
	out := make([]string, 0, len(in))
	for _, s := range in {
		if !seen[s] {
			seen[s] = true
			out = append(out, s)
		}
	}
	return out
}

func advLabel(v string) string { return fmt.Sprintf("%.24q", v) }

// --- (1) enum matrix ---

type advSlot struct {
	name      string
	base      func() obj
	valid     string // a value the slot accepts (also the seed of the variants)
	set       func(m obj, v string)
	path      string
	code      string // code for hostile non-empty values (default invalid_value)
	emptyOK   bool   // "" is normalised to a default and accepted
	emptyCode string // code for "" when not emptyOK (default = code)
	emptyPath string // path for that issue (default = path)
}

func advEnumSlots() []advSlot {
	oliveS, spr, gilt := oliveGrove, sprinkles, giltNoir
	v1 := func() obj { return validManifestMap() }
	return []advSlot{
		// v2 envelope.
		{name: "layout", base: oliveS, valid: "split", set: advTop("layout"), path: "layout"},
		{name: "hero_style", base: oliveS, valid: "text_only", set: advTop("hero_style"), path: "hero_style"},
		{name: "heading_scale", base: oliveS, valid: "display", set: advTop("heading_scale"), path: "heading_scale", emptyOK: true},
		{name: "motion", base: oliveS, valid: "draw_on", set: advTop("motion"), path: "motion", emptyOK: true},
		// layer kinds.
		{name: "layers0.kind", base: oliveS, valid: "paper", set: advL(0, "kind"), path: "layers[0].kind"},
		{name: "layers1.kind", base: oliveS, valid: "texture", set: advL(1, "kind"), path: "layers[1].kind"},
		{name: "layers2.kind", base: oliveS, valid: "art", set: advL(2, "kind"), path: "layers[2].kind"},
		{name: "layers4.kind", base: oliveS, valid: "frame", set: advL(4, "kind"), path: "layers[4].kind"},
		{name: "block.kind", base: spr, valid: "block", set: advL(1, "kind"), path: "layers[1].kind"},
		// region on every kind that can carry one.
		{name: "paper.region", base: oliveS, valid: "page", set: advL(0, "region"), path: "layers[0].region", emptyOK: true},
		{name: "texture.region", base: oliveS, valid: "page", set: advL(1, "region"), path: "layers[1].region", emptyOK: true},
		{name: "art.region", base: oliveS, valid: "hero", set: advL(2, "region"), path: "layers[2].region", emptyOK: true},
		{name: "frame.region", base: oliveS, valid: "page", set: advL(4, "region"), path: "layers[4].region", emptyOK: true},
		{name: "block.region", base: spr, valid: "hero", set: advL(1, "region"), path: "layers[1].region", emptyOK: true},
		{name: "pattern.region", base: spr, valid: "page", set: advL(2, "region"), path: "layers[2].region", emptyOK: true},
		// block / pattern / texture / art / frame.
		{name: "block.edge", base: spr, valid: "scallop", set: advL(1, "edge"), path: "layers[1].edge", emptyOK: true},
		{name: "pattern.pattern", base: spr, valid: "halftone", set: advL(2, "pattern"), path: "layers[2].pattern"},
		{name: "pattern.origin", base: spr, valid: "top_right", set: advL(2, "origin"), path: "layers[2].origin", emptyOK: true},
		{name: "pattern.mask", base: spr, valid: "corner", set: advL(2, "mask"), path: "layers[2].mask", emptyOK: true},
		{name: "pattern.blend", base: spr, valid: "overlay", set: advL(2, "blend"), path: "layers[2].blend", emptyOK: true},
		{name: "texture.texture", base: oliveS, valid: "fibers", set: advL(1, "texture"), path: "layers[1].texture"},
		{name: "texture.blend", base: oliveS, valid: "multiply", set: advL(1, "blend"), path: "layers[1].blend", emptyOK: true},
		{name: "art.art", base: oliveS, valid: "olive_branches", set: advL(2, "art"), path: "layers[2].art"},
		{name: "art.placement", base: oliveS, valid: "corners", set: advL(2, "placement"), path: "layers[2].placement"},
		{name: "art.blend", base: oliveS, valid: "soft-light", set: advL(2, "blend"), path: "layers[2].blend", emptyOK: true},
		{name: "art.density", base: spr, valid: "dense", set: advL(3, "density"), path: "layers[3].density", emptyOK: true},
		{name: "art.paint", base: gilt, valid: "foil", set: advL(2, "paint"), path: "layers[2].paint", emptyOK: true},
		{name: "frame.frame", base: oliveS, valid: "double_hairline", set: advL(4, "frame"), path: "layers[4].frame"},
		{name: "frame.paint", base: oliveS, valid: "foil", set: advL(4, "paint"), path: "layers[4].paint", emptyCode: "missing_value", emptyPath: "layers[4].color"},
		{name: "frame.blend", base: oliveS, valid: "screen", set: advL(4, "blend"), path: "layers[4].blend", emptyOK: true},
		// ornament, card.
		{name: "ornament.hero", base: oliveS, valid: "wreath_monogram", set: advIn("ornament", "hero"), path: "ornament.hero", emptyOK: true},
		{name: "ornament.divider", base: oliveS, valid: "olive_sprig", set: advIn("ornament", "divider"), path: "ornament.divider", emptyOK: true},
		{name: "ornament.badge", base: oliveS, valid: "wax_seal", set: advIn("ornament", "badge"), path: "ornament.badge", emptyOK: true},
		{name: "ornament.ampersand", base: oliveS, valid: "script", set: advIn("ornament", "ampersand"), path: "ornament.ampersand", emptyOK: true},
		{name: "card.style", base: oliveS, valid: "reply_card", set: advIn("card", "style"), path: "card.style", emptyOK: true},
		{name: "card.border", base: oliveS, valid: "foil_inset", set: advIn("card", "border"), path: "card.border", emptyOK: true},
		{name: "card.fields", base: oliveS, valid: "underline", set: advIn("card", "fields"), path: "card.fields", emptyOK: true},
		{name: "card.buttons", base: oliveS, valid: "accent", set: advIn("card", "buttons"), path: "card.buttons", emptyOK: true},
		// fonts.
		{name: "font.heading", base: oliveS, valid: "bodoni_moda", set: advFont(0, "heading"), path: "fonts[0].heading"},
		{name: "font.body", base: oliveS, valid: "montserrat", set: advFont(0, "body"), path: "fonts[0].body"},
		{name: "font.accent", base: oliveS, valid: "pinyon_script", set: advFont(0, "accent"), path: "fonts[0].accent", emptyOK: true},
		// schema 1 slots.
		{name: "v1.layout", base: v1, valid: "centered", set: advTop("layout"), path: "layout"},
		{name: "v1.hero_style", base: v1, valid: "framed", set: advTop("hero_style"), path: "hero_style"},
		{name: "v1.decoration", base: v1, valid: "line", set: advTop("decoration"), path: "decoration"},
		{name: "v1.surface", base: v1, valid: "card", set: advTop("surface"), path: "surface", emptyOK: true},
		{name: "v1.texture", base: v1, valid: "grid", set: advTop("texture"), path: "texture", emptyOK: true},
		{name: "v1.heading_scale", base: v1, valid: "display", set: advTop("heading_scale"), path: "heading_scale", emptyOK: true},
		{name: "v1.font.heading", base: v1, valid: "playfair_display", set: advFont(0, "heading"), path: "fonts[0].heading"},
		{name: "v1.font.body", base: v1, valid: "lora", set: advFont(0, "body"), path: "fonts[0].body"},
		{name: "v1.font.accent", base: v1, valid: "birthstone", set: advFont(0, "accent"), path: "fonts[0].accent", emptyOK: true},
	}
}

func advTokenSlots() []advSlot {
	s := advTokenSlotList()
	for i := range s {
		s[i].code = "unknown_token"
	}
	return s
}

func advTokenSlotList() []advSlot {
	oliveS, spr := oliveGrove, sprinkles
	return []advSlot{
		{name: "paper.tone", base: oliveS, valid: "background", set: advL(0, "tone"), path: "layers[0].tone", emptyOK: true},
		{name: "paper.glow", base: oliveS, valid: "art5", set: advL(0, "glow"), path: "layers[0].glow", emptyOK: true},
		{name: "paper.vignette", base: oliveS, valid: "art6", set: advL(0, "vignette"), path: "layers[0].vignette", emptyOK: true},
		{name: "pattern.color", base: spr, valid: "accent_text", set: advL(2, "color"), path: "layers[2].color", emptyCode: "missing_value"},
		{name: "art.colors0", base: oliveS, valid: "art1", set: func(m obj, v string) { layerAt(m, 2)["colors"].([]any)[0] = v }, path: "layers[2].colors[0]", emptyCode: "missing_value"},
		{name: "art.colors3", base: oliveS, valid: "art4", set: func(m obj, v string) { layerAt(m, 2)["colors"].([]any)[3] = v }, path: "layers[2].colors[3]", emptyCode: "missing_value"},
		{name: "frame.color", base: oliveS, valid: "accent", set: func(m obj, v string) {
			delete(layerAt(m, 4), "paint")
			layerAt(m, 4)["color"] = v
		}, path: "layers[4].color", emptyCode: "missing_value"},
		{name: "watercolour.colors2", base: oliveS, valid: "art3", set: func(m obj, v string) {
			layersOf(m)[1] = obj{"kind": "texture", "texture": "watercolour", "opacity": 0.1, "colors": []any{"art1", "art2", v}}
		}, path: "layers[1].colors[2]", emptyCode: "missing_value"},
		{name: "ornament.hero_ink", base: oliveS, valid: "accent", set: advIn("ornament", "hero_ink"), path: "ornament.hero_ink", emptyOK: true},
	}
}

func advHexSlots() []advSlot {
	oliveS := oliveGrove
	v1 := func() obj { return validManifestMap() }
	s := []advSlot{
		{name: "accent_ink", base: oliveS, valid: "#77592A", set: advPalField(0, "accent_ink"), path: "palettes[0].accent_ink", emptyOK: true},
		{name: "art0", base: oliveS, valid: "#5F6E55", set: advPalList(0, "art", 0), path: "palettes[0].art[0]"},
		{name: "art5", base: oliveS, valid: "#E5DCC8", set: advPalList(0, "art", 5), path: "palettes[0].art[5]"},
		{name: "foil0", base: oliveS, valid: "#8E6B2E", set: advPalList(0, "foil", 0), path: "palettes[0].foil[0]"},
		{name: "foil4", base: oliveS, valid: "#E6CC8B", set: advPalList(0, "foil", 4), path: "palettes[0].foil[4]"},
		{name: "palette2.art0", base: oliveS, valid: "#8A6B5E", set: advPalList(2, "art", 0), path: "palettes[2].art[0]"},
	}
	for _, c := range []struct{ k, v string }{
		{"background", "#F3EEE3"}, {"surface", "#FBF8F1"}, {"text", "#2C382D"},
		{"muted", "#5A6457"}, {"accent", "#2F4A37"}, {"accent_text", "#FBF8F1"},
	} {
		s = append(s,
			advSlot{name: "colors." + c.k, base: oliveS, valid: c.v, set: advPalColor(0, c.k), path: "palettes[0].colors." + c.k},
			advSlot{name: "v1.colors." + c.k, base: v1, valid: map[string]string{
				"background": "#FBF8F3", "surface": "#FFFFFF", "text": "#1F1B16", "muted": "#6B645C", "accent": "#8C6A3F", "accent_text": "#FFFFFF",
			}[c.k], set: advPalColor(0, c.k), path: "palettes[0].colors." + c.k},
		)
	}
	for i := range s {
		s[i].code = "invalid_color"
	}
	return s
}

func advRunSlots(t *testing.T, slots []advSlot, hostile func(valid string) []string) {
	t.Helper()
	for _, s := range slots {
		t.Run(s.name, func(t *testing.T) {
			code := s.code
			if code == "" {
				code = "invalid_value"
			}
			emptyCode := s.emptyCode
			if emptyCode == "" {
				emptyCode = code
			}
			base := s.base
			// The slot's own valid value must be accepted, or the matrix is
			// testing nothing.
			advCanon(t, advMutate(t, base(), func(m obj) { s.set(m, s.valid) }))

			for _, v := range hostile(s.valid) {
				t.Run(advLabel(v), func(t *testing.T) {
					raw := advMutate(t, base(), func(m obj) { s.set(m, v) })
					if v == "" && s.emptyOK {
						advCanon(t, raw)
						return
					}
					want, wantPath := code, s.path
					if v == "" {
						want = emptyCode
						if s.emptyPath != "" {
							wantPath = s.emptyPath
						}
					}
					_, err := advValidate(t, raw)
					advWant(t, err, want, wantPath)
					issues := advIssues(err)
					if len(issues) > maxIssues {
						t.Errorf("%d issues, cap is %d", len(issues), maxIssues)
					}
					for _, is := range issues {
						if len(v) >= 8 && strings.Contains(is.Message+is.Path, v) {
							t.Errorf("issue echoes the hostile input: %s", is.Path)
						}
						if len(is.Message) > 200 || len(is.Path) > 200 {
							t.Errorf("oversized issue %s", is.Path)
						}
					}
				})
			}
		})
	}
}

func TestAdv_EnumMatrix(t *testing.T)  { advRunSlots(t, advEnumSlots(), advHostileEnums) }
func TestAdv_TokenMatrix(t *testing.T) { advRunSlots(t, advTokenSlots(), advHostileTokens) }
func TestAdv_HexMatrix(t *testing.T) {
	advRunSlots(t, advHexSlots(), func(string) []string { return advHostileHex() })
}

// Lower-case hex is accepted and upper-cased in the canonical form.
func TestAdv_HexLowercaseCanonicalised(t *testing.T) {
	for _, s := range advHexSlots() {
		t.Run(s.name, func(t *testing.T) {
			lower := strings.ToLower(s.valid)
			canon := advCanon(t, advMutate(t, s.base(), func(m obj) { s.set(m, lower) }))
			if bytes.Contains(canon, []byte(`"`+lower+`"`)) && lower != s.valid {
				t.Errorf("canonical form kept lower-case %s", lower)
			}
			if !bytes.Contains(canon, []byte(`"`+s.valid+`"`)) {
				t.Errorf("canonical form lost %s", s.valid)
			}
		})
	}
}

// Invalid UTF-8 and lone surrogate escapes in string slots never panic.
func TestAdv_InvalidUTF8AndSurrogates(t *testing.T) {
	good := string(advMutate(t, oliveGrove(), nil))
	for _, c := range []struct{ name, from, to string }{
		{"layout raw 0xFF", `"layout":"split"`, "\"layout\":\"spl\xffit\""},
		{"layout lone surrogate", `"layout":"split"`, `"layout":"spl\ud800it"`},
		{"layout overlong NUL", `"layout":"split"`, "\"layout\":\"split\xc0\x80\""},
		{"kind raw 0xFE", `"kind":"paper"`, "\"kind\":\"pap\xfeer\""},
		{"palette name raw 0xFF", `"name":"Ivory"`, "\"name\":\"Iv\xffory\""},
		{"hex raw 0xFF", `"#F3EEE3"`, "\"#F3EE\xffE\""},
	} {
		t.Run(c.name, func(t *testing.T) {
			if !strings.Contains(good, c.from) {
				t.Fatalf("fixture lacks %s", c.from)
			}
			raw := strings.Replace(good, c.from, c.to, 1)
			_, err := advValidate(t, []byte(raw))
			if err == nil {
				t.Fatalf("hostile bytes accepted")
			}
		})
	}
}

// --- (3) numeric matrix ---

type advNum struct {
	name     string
	base     func() obj
	set      func(m obj, v any)
	path     string
	lo, hi   float64
	integer  bool
	required bool // null/absent is missing_value rather than a default
}

func advLayerNum(i int, k string) func(m obj, v any) {
	return func(m obj, v any) { layerAt(m, i)[k] = v }
}

func advNumSlots() []advNum {
	withImage := func() obj {
		m := oliveGrove()
		m["layers"] = append(m["layers"].([]obj), obj{"kind": "image", "opacity": 0.3})
		return m
	}
	oliveWash := func() obj {
		m := oliveGrove()
		m["layers"].([]obj)[0]["glow_opacity"] = 0.5
		m["layers"].([]obj)[0]["vignette_opacity"] = 0.5
		return m
	}
	return []advNum{
		{name: "glow_opacity", base: oliveWash, set: advLayerNum(0, "glow_opacity"), path: "layers[0].glow_opacity", lo: 0, hi: 1},
		{name: "vignette_opacity", base: oliveWash, set: advLayerNum(0, "vignette_opacity"), path: "layers[0].vignette_opacity", lo: 0, hi: 1},
		{name: "texture.opacity", base: oliveGrove, set: advLayerNum(1, "opacity"), path: "layers[1].opacity", lo: 0.02, hi: 0.4, required: true},
		{name: "art.opacity", base: func() obj {
			m := oliveGrove()
			m["layers"].([]obj)[2]["opacity"] = 0.5
			return m
		}, set: advLayerNum(2, "opacity"), path: "layers[2].opacity", lo: 0.1, hi: 1},
		{name: "pattern.opacity", base: sprinkles, set: advLayerNum(2, "opacity"), path: "layers[2].opacity", lo: 0.01, hi: 0.35, required: true},
		{name: "image.opacity", base: withImage, set: advLayerNum(5, "opacity"), path: "layers[5].opacity", lo: 0.05, hi: 0.5, required: true},
		{name: "frame.inset", base: oliveGrove, set: advLayerNum(4, "inset"), path: "layers[4].inset", lo: 0, hi: 32, integer: true},
		{name: "card.radius", base: oliveGrove, set: func(m obj, v any) { m["card"].(obj)["radius"] = v }, path: "card.radius", lo: 0, hi: 32, integer: true},
	}
}

type advLit struct {
	lit string
	val float64
}

func advFloatLits(lo, hi float64) []advLit {
	vals := []float64{
		-1e308, -1, -0.01, 0, 5e-324, 1e-308, lo, math.Nextafter(lo, -1), lo - 1e-9, lo + 1e-9,
		(lo + hi) / 2, hi, math.Nextafter(hi, 2), hi + 1e-9, hi - 1e-9, 1.01, 2, 1e308,
	}
	out := make([]advLit, 0, len(vals)+1)
	for _, v := range vals {
		out = append(out, advLit{strconv.FormatFloat(v, 'g', -1, 64), v})
	}
	// Underflow parses to 0 without an error.
	out = append(out, advLit{"1e-999", 0})
	return out
}

func advIntLits() []advLit {
	var out []advLit
	for _, v := range []int64{math.MinInt64, -2147483649, -1, 0, 1, 31, 32, 33, 2147483648, math.MaxInt64} {
		out = append(out, advLit{strconv.FormatInt(v, 10), float64(v)})
	}
	out = append(out, advLit{"-0", 0})
	return out
}

func TestAdv_NumericMatrix(t *testing.T) {
	for _, s := range advNumSlots() {
		t.Run(s.name, func(t *testing.T) {
			lits := advFloatLits(s.lo, s.hi)
			if s.integer {
				lits = advIntLits()
			}
			for _, l := range lits {
				t.Run(l.lit, func(t *testing.T) {
					raw := advMutate(t, s.base(), func(m obj) { s.set(m, json.Number(l.lit)) })
					if l.val >= s.lo && l.val <= s.hi {
						advCanon(t, raw)
						return
					}
					_, err := advValidate(t, raw)
					advWant(t, err, "out_of_range", s.path)
				})
			}

			// Wrong JSON types are invalid_json, never a panic or a coercion.
			badTypes := []struct {
				name string
				v    any
			}{
				{"string 0.5", "0.5"}, {"string 1", "1"}, {"empty string", ""}, {"true", true}, {"false", false},
				{"empty array", []any{}}, {"array of number", []any{0.5}}, {"empty object", obj{}},
				{"1e999", json.Number("1e999")}, {"-1e999", json.Number("-1e999")},
			}
			if s.integer {
				for _, lit := range []string{"1.5", "1e3", "1E1", "1.0", "32.0", "-0.0", "0.5e1",
					"9223372036854775808", "-9223372036854775809", "18446744073709551616"} {
					badTypes = append(badTypes, struct {
						name string
						v    any
					}{"int slot " + lit, json.Number(lit)})
				}
			}
			for _, b := range badTypes {
				t.Run("bad/"+b.name, func(t *testing.T) {
					raw := advMutate(t, s.base(), func(m obj) { s.set(m, b.v) })
					_, err := advValidate(t, raw)
					advWant(t, err, "invalid_json", "manifest")
				})
			}

			// null decodes to "absent": missing_value where the field is
			// required, the default elsewhere.
			t.Run("null", func(t *testing.T) {
				raw := advMutate(t, s.base(), func(m obj) { s.set(m, nil) })
				if s.required {
					_, err := advValidate(t, raw)
					advWant(t, err, "missing_value", s.path)
					return
				}
				advCanon(t, raw)
			})
		})
	}
}

// --- (4) arrays, nesting, ids ---

func TestAdv_ArraysAndCounts(t *testing.T) {
	sevenLayers := func(m obj) {
		setLayers(m,
			obj{"kind": "paper"}, obj{"kind": "pattern", "pattern": "dots", "color": "text", "opacity": 0.1},
			obj{"kind": "pattern", "pattern": "grid", "color": "text", "opacity": 0.1},
			obj{"kind": "texture", "texture": "grain", "opacity": 0.1},
			obj{"kind": "texture", "texture": "linen", "opacity": 0.1},
			obj{"kind": "art", "art": "printers_corners", "placement": "corners", "colors": []string{"art1"}},
			obj{"kind": "frame", "frame": "single_hairline", "color": "accent"})
	}
	artN := func(n int) func(m obj) {
		return func(m obj) {
			cols := make([]any, n)
			for i := range cols {
				// Light, so a paper wash in the art colours stays readable.
				cols[i] = fmt.Sprintf("#%02X%02X%02X", 240+i, 241+i, 242+i)
			}
			for pi := range m["palettes"].([]any) {
				palAt(m, pi)["art"] = cols
			}
		}
	}
	foilN := func(n int) func(m obj) {
		return func(m obj) {
			cols := make([]any, n)
			for i := range cols {
				cols[i] = fmt.Sprintf("#%02X%02X%02X", 200+i, 160+i, 80+i)
			}
			palAt(m, 0)["foil"] = cols
		}
	}
	advRun(t, []advCase{
		{name: "7 layers accepted", fn: sevenLayers},
		{name: "8 layers", fn: func(m obj) {
			sevenLayers(m)
			m["layers"] = append(layersOf(m), obj{"kind": "image", "opacity": 0.1})
		}, code: "too_many", path: "layers"},
		{name: "0 layers accepted", fn: func(m obj) { m["layers"] = []any{} }},
		{name: "null layers accepted", fn: func(m obj) { m["layers"] = nil }},
		{name: "null layer element", fn: func(m obj) { m["layers"] = []any{nil} }, code: "invalid_value", path: "layers[0].kind"},
		{name: "layer without kind", fn: func(m obj) { setLayers(m, obj{}) }, code: "invalid_value", path: "layers[0].kind"},
		{name: "kind null", fn: func(m obj) { setLayers(m, obj{"kind": nil}) }, code: "invalid_value", path: "layers[0].kind"},
		{name: "art colours 7 on confetti", fn: func(m obj) {
			setLayers(m, obj{"kind": "paper"}, obj{"kind": "art", "art": "confetti", "placement": "hero", "colors": []string{"art1", "art2", "art3", "art4", "art5", "art6", "art1"}})
		}, code: "too_many", path: "layers[1].colors"},
		{name: "art colours 6 on confetti accepted", fn: func(m obj) {
			setLayers(m, obj{"kind": "paper"}, obj{"kind": "art", "art": "confetti", "placement": "hero", "colors": []string{"art1", "art2", "art3", "art4", "art5", "art6"}})
		}},
		{name: "palette art 8 accepted", fn: artN(8)},
		{name: "palette art 9", fn: artN(9), code: "too_many", path: "palettes[0].art"},
		{name: "palette art 0 with art tokens", fn: artN(0), code: "missing_palette_color", path: "palettes[0].art"},
		{name: "palette art [] explicit, no tokens", fn: func(m obj) {
			artN(0)(m)
			setLayers(m, obj{"kind": "paper"})
		}},
		{name: "foil 2", fn: foilN(2), code: "invalid_length", path: "palettes[0].foil"},
		{name: "foil 3 accepted", fn: foilN(3)},
		{name: "foil 5 accepted", fn: foilN(5)},
		{name: "foil 6", fn: foilN(6), code: "too_many", path: "palettes[0].foil"},
		{name: "foil 1", fn: foilN(1), code: "invalid_length", path: "palettes[0].foil"},
		{name: "foil [] with foil users", fn: foilN(0), code: "missing_palette_color", path: "palettes[0].foil"},
		{name: "palettes 0", fn: func(m obj) { m["palettes"] = []any{} }, code: "invalid_length", path: "palettes"},
		{name: "palettes null", fn: func(m obj) { m["palettes"] = nil }, code: "invalid_length", path: "palettes"},
		{name: "palettes 9", fn: func(m obj) {
			ps := make([]any, 9)
			for i := range ps {
				p := json.RawMessage(advMarshal(t, palAt(m, 0)))
				var cp obj
				_ = json.Unmarshal(p, &cp)
				cp["id"] = fmt.Sprintf("p%d", i)
				ps[i] = cp
			}
			m["palettes"] = ps
			m["defaults"].(obj)["palette"] = "p0"
		}, code: "invalid_length", path: "palettes"},
		{name: "fonts 0", fn: func(m obj) { m["fonts"] = []any{} }, code: "invalid_length", path: "fonts"},
		{name: "fonts null", fn: func(m obj) { m["fonts"] = nil }, code: "invalid_length", path: "fonts"},
		{name: "fonts 7", fn: func(m obj) {
			fs := make([]any, 7)
			for i := range fs {
				fs[i] = obj{"id": fmt.Sprintf("f%d", i), "name": "F", "heading": "lora", "body": "lora"}
			}
			m["fonts"] = fs
			m["defaults"].(obj)["font"] = "f0"
		}, code: "invalid_length", path: "fonts"},
		// shape errors
		{name: "ornament as array", fn: func(m obj) { m["ornament"] = []any{} }, code: "invalid_json", path: "manifest"},
		{name: "card as array", fn: func(m obj) { m["card"] = []any{1} }, code: "invalid_json", path: "manifest"},
		{name: "card as string", fn: func(m obj) { m["card"] = "soft" }, code: "invalid_json", path: "manifest"},
		{name: "layer as array", fn: func(m obj) { m["layers"] = []any{[]any{}} }, code: "invalid_json", path: "manifest"},
		{name: "layer as string", fn: func(m obj) { m["layers"] = []any{"paper"} }, code: "invalid_json", path: "manifest"},
		{name: "palette as array", fn: func(m obj) { m["palettes"] = []any{[]any{}} }, code: "invalid_json", path: "manifest"},
		{name: "palette null", fn: func(m obj) { m["palettes"] = []any{nil} }, code: "invalid_id", path: "palettes[0].id"},
		{name: "colors as array", fn: func(m obj) { palAt(m, 0)["colors"] = []any{} }, code: "invalid_json", path: "manifest"},
		{name: "colors null", fn: func(m obj) { palAt(m, 0)["colors"] = nil }, code: "invalid_color", path: "palettes[0].colors.background"},
		{name: "defaults as array", fn: func(m obj) { m["defaults"] = []any{} }, code: "invalid_json", path: "manifest"},
		{name: "font as array", fn: func(m obj) { m["fonts"] = []any{[]any{}} }, code: "invalid_json", path: "manifest"},
		{name: "layers as object", fn: func(m obj) { m["layers"] = obj{} }, code: "invalid_json", path: "manifest"},
		{name: "layers as string", fn: func(m obj) { m["layers"] = "x" }, code: "invalid_json", path: "manifest"},
		{name: "palettes as object", fn: func(m obj) { m["palettes"] = obj{} }, code: "invalid_json", path: "manifest"},
		{name: "fonts as object", fn: func(m obj) { m["fonts"] = obj{} }, code: "invalid_json", path: "manifest"},
		{name: "art list as object", fn: func(m obj) { palAt(m, 0)["art"] = obj{} }, code: "invalid_json", path: "manifest"},
		{name: "foil as object", fn: func(m obj) { palAt(m, 0)["foil"] = obj{} }, code: "invalid_json", path: "manifest"},
		{name: "foil as string", fn: func(m obj) { palAt(m, 0)["foil"] = "#FFFFFF" }, code: "invalid_json", path: "manifest"},
		{name: "layer colors as object", fn: func(m obj) { layerAt(m, 2)["colors"] = obj{} }, code: "invalid_json", path: "manifest"},
		{name: "layer colors as string", fn: func(m obj) { layerAt(m, 2)["colors"] = "art1" }, code: "invalid_json", path: "manifest"},
		{name: "colour element not a string", fn: func(m obj) { layerAt(m, 2)["colors"] = []any{1, 2, 3} }, code: "invalid_json", path: "manifest"},
		{name: "colour element null", fn: func(m obj) { layerAt(m, 2)["colors"] = []any{nil, "art1", "art2"} }, code: "missing_value", path: "layers[2].colors[0]"},
		{name: "foil element null", fn: func(m obj) { palAt(m, 0)["foil"] = []any{nil, "#C9A963", "#F3E2AA"} }, code: "invalid_color", path: "palettes[0].foil[0]"},
		{name: "hex as number", fn: func(m obj) { colorsAt(m, 0)["text"] = 123456 }, code: "invalid_json", path: "manifest"},
		{name: "hex as null", fn: func(m obj) { colorsAt(m, 0)["text"] = nil }, code: "invalid_color", path: "palettes[0].colors.text"},
		{name: "layout as number", fn: func(m obj) { m["layout"] = 1 }, code: "invalid_json", path: "manifest"},
		{name: "layout null", fn: func(m obj) { m["layout"] = nil }, code: "invalid_value", path: "layout"},
		{name: "ornament null accepted", fn: func(m obj) { m["ornament"] = nil }},
		{name: "card null accepted", fn: func(m obj) { m["card"] = nil }},
		{name: "motion null accepted", fn: func(m obj) { m["motion"] = nil }},
		{name: "ornament hero_ink as number", fn: func(m obj) { m["ornament"].(obj)["hero_ink"] = 1 }, code: "invalid_json", path: "manifest"},
	})
}

func TestAdv_NestingAndShapesNeverPanic(t *testing.T) {
	deep := func(n int, open, shut string) string { return strings.Repeat(open, n) + strings.Repeat(shut, n) }
	d5k, d10k, d12k := deep(5000, "[", "]"), deep(10000, "[", "]"), deep(12000, "[", "]")
	dObj := strings.Repeat(`{"a":`, 4000) + "1" + strings.Repeat("}", 4000)
	inputs := map[string]string{
		"top level array 5k":             d5k,
		"top level array 10k":            d10k,
		"top level array 12k":            d12k,
		"unknown field holding 10k":      `{"schema":2,"x":` + d10k + `}`,
		"unknown field holding 12k":      `{"schema":2,"x":` + d12k + `}`,
		"layers holding 10k":             `{"schema":2,"layers":` + d10k + `}`,
		"layers holding 5k":              `{"schema":2,"layers":` + d5k + `}`,
		"layer extra field 10k":          `{"schema":2,"layers":[{"kind":"paper","x":` + d10k + `}]}`,
		"palette name holding array":     `{"schema":2,"palettes":[{"id":"a","name":` + d5k + `}]}`,
		"nested objects 4k":              `{"schema":2,"x":` + dObj + `}`,
		"nested objects as top":          dObj,
		"ornament holding 10k":           `{"schema":2,"ornament":{"hero":` + d10k + `}}`,
		"card holding 10k":               `{"schema":2,"card":` + d10k + `}`,
		"defaults holding 10k":           `{"schema":2,"defaults":{"palette":` + d10k + `}}`,
		"unterminated 10k":               strings.Repeat("[", 10000),
		"unterminated objects":           strings.Repeat(`{"a":`, 5000),
		"empty":                          "",
		"whitespace":                     "   \n\t",
		"null":                           "null",
		"true":                           "true",
		"number":                         "1",
		"string":                         `"x"`,
		"array":                          "[]",
		"empty object":                   "{}",
		"only schema 2":                  `{"schema":2}`,
		"only schema 1":                  `{"schema":1}`,
		"truncated":                      `{"schema":2,"layout":"spl`,
		"BOM prefix":                     string(rune(0xFEFF)) + `{"schema":2}`,
		"NUL byte":                       "{\"schema\":2}\x00",
		"huge int":                       `{"schema":` + strings.Repeat("9", 5000) + `}`,
		"huge float exponent":            `{"schema":2,"layers":[{"kind":"image","opacity":1e` + strings.Repeat("9", 4000) + `}]}`,
		"many empty objects as palettes": `{"schema":2,"palettes":[` + strings.Repeat(`{},`, 5000) + `{}]}`,
	}
	for name, in := range inputs {
		t.Run(name, func(t *testing.T) {
			if len(in) > maxManifestBytes {
				// Not under test here: the size cap fires first.
				_, err := advValidate(t, []byte(in))
				advWant(t, err, "too_large", "manifest")
				return
			}
			_, err := advValidate(t, []byte(in))
			if err == nil {
				t.Fatalf("accepted")
			}
			if _, ok := err.(*ValidationError); !ok {
				t.Fatalf("error is %T, want *ValidationError", err)
			}
			if n := len(advIssues(err)); n == 0 || n > maxIssues {
				t.Fatalf("%d issues", n)
			}
		})
	}

	t.Run("12k deep within cap is rejected as invalid_json", func(t *testing.T) {
		in := []byte(`{"schema":2,"x":` + d10k + `}`)
		if len(in) > maxManifestBytes {
			t.Fatalf("test input is %d bytes, over the cap", len(in))
		}
		_, err := advValidate(t, in)
		advWant(t, err, "invalid_json", "manifest")
	})
}

func TestAdv_ManyEntriesStayBounded(t *testing.T) {
	// Thousands of cheap entries must be rejected with a bounded issue list.
	rep := func(item string, n int) string {
		return "[" + strings.TrimSuffix(strings.Repeat(item+",", n), ",") + "]"
	}
	cases := []struct {
		name, raw, code, path string
	}{
		{"2000 empty palettes", `{"schema":2,"layout":"split","hero_style":"text_only","palettes":` + rep(`{"id":"A!"}`, 1500) + `}`, "invalid_length", "palettes"},
		{"2000 fonts", `{"schema":2,"layout":"split","hero_style":"text_only","fonts":` + rep(`{"id":"A!"}`, 1500) + `}`, "invalid_length", "fonts"},
		{"2000 layers", `{"schema":2,"layout":"split","hero_style":"text_only","layers":` + rep(`{"kind":"x"}`, 1500) + `}`, "too_many", "layers"},
		{"2000 valid-ish image layers", `{"schema":2,"layout":"split","hero_style":"text_only","layers":` + rep(`{"kind":"image","opacity":0.1}`, 700) + `}`, "too_many", "layers"},
		{"duplicate ids flood", `{"schema":2,"layout":"split","hero_style":"text_only","palettes":` + rep(`{"id":"a"}`, 1500) + `}`, "duplicate_id", "palettes[1].id"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			if len(c.raw) > maxManifestBytes {
				t.Fatalf("test input is %d bytes, over the cap", len(c.raw))
			}
			_, err := advValidate(t, []byte(c.raw))
			advWant(t, err, c.code, c.path)
			if n := len(advIssues(err)); n > maxIssues {
				t.Errorf("%d issues, cap is %d", n, maxIssues)
			}
		})
	}
}

func TestAdv_IDsAndDuplicates(t *testing.T) {
	badIDs := []string{
		"", "A", "Ivory", "a b", "a/b", "a.b", "a:b", "a\n", "a\x00", "é", "а" /* Cyrillic */, "a;b", "<b>",
		strings.Repeat("a", 25), strings.Repeat("a", 10<<10), " a", "a ", "../a", "a\u200b",
	}
	for _, id := range badIDs {
		t.Run("palette/"+advLabel(id), func(t *testing.T) {
			raw := advMutate(t, oliveGrove(), func(m obj) { palAt(m, 1)["id"] = id })
			_, err := advValidate(t, raw)
			advWant(t, err, "invalid_id", "palettes[1].id")
		})
		t.Run("font/"+advLabel(id), func(t *testing.T) {
			raw := advMutate(t, oliveGrove(), func(m obj) { m["fonts"].([]any)[0].(obj)["id"] = id })
			_, err := advValidate(t, raw)
			advWant(t, err, "invalid_id", "fonts[0].id")
		})
	}
	for _, id := range []string{"a", "a-b_c0", strings.Repeat("a", 24), "0", "_", "-"} {
		t.Run("valid/"+id, func(t *testing.T) {
			advCanon(t, advMutate(t, oliveGrove(), func(m obj) { palAt(m, 1)["id"] = id }))
		})
	}

	advRun(t, []advCase{
		{name: "duplicate palette ids", fn: func(m obj) { palAt(m, 1)["id"] = palAt(m, 0)["id"] }, code: "duplicate_id", path: "palettes[1].id"},
		{name: "duplicate palette ids case differs ok", fn: func(m obj) { palAt(m, 1)["id"] = "ivory2" }},
		{name: "duplicate font ids", fn: func(m obj) {
			f := m["fonts"].([]any)
			cp := obj{}
			for k, v := range f[0].(obj) {
				cp[k] = v
			}
			m["fonts"] = append(f, cp)
		}, code: "duplicate_id", path: "fonts[1].id"},
		{name: "defaults palette unknown", fn: func(m obj) { m["defaults"].(obj)["palette"] = "nope" }, code: "invalid_value", path: "defaults.palette"},
		{name: "defaults palette case", fn: func(m obj) { m["defaults"].(obj)["palette"] = "Ivory" }, code: "invalid_value", path: "defaults.palette"},
		{name: "defaults palette trailing space", fn: func(m obj) { m["defaults"].(obj)["palette"] = "ivory " }, code: "invalid_value", path: "defaults.palette"},
		{name: "defaults palette empty", fn: func(m obj) { m["defaults"].(obj)["palette"] = "" }, code: "invalid_value", path: "defaults.palette"},
		{name: "defaults font unknown", fn: func(m obj) { m["defaults"].(obj)["font"] = "nope" }, code: "invalid_value", path: "defaults.font"},
		{name: "defaults font empty", fn: func(m obj) { m["defaults"].(obj)["font"] = "" }, code: "invalid_value", path: "defaults.font"},
		{name: "defaults palette refers to invalid id", fn: func(m obj) {
			palAt(m, 0)["id"] = "A!"
			m["defaults"].(obj)["palette"] = "A!"
		}, code: "invalid_id", path: "palettes[0].id"},
		{name: "heading-only font as body", fn: func(m obj) { m["fonts"].([]any)[0].(obj)["body"] = "pinyon_script" }, code: "invalid_value", path: "fonts[0].body"},
		{name: "limelight as body", fn: func(m obj) { m["fonts"].([]any)[0].(obj)["body"] = "limelight" }, code: "invalid_value", path: "fonts[0].body"},
		{name: "bagel_fat_one as body", fn: func(m obj) { m["fonts"].([]any)[0].(obj)["body"] = "bagel_fat_one" }, code: "invalid_value", path: "fonts[0].body"},
		{name: "gloock is not a manifest font", fn: func(m obj) { m["fonts"].([]any)[0].(obj)["heading"] = "gloock" }, code: "invalid_value", path: "fonts[0].heading"},
		{name: "heading-only font as heading and accent", fn: func(m obj) {
			f := m["fonts"].([]any)[0].(obj)
			f["heading"], f["accent"] = "limelight", "bagel_fat_one"
		}},
	})
}

// Palette names: bounded, no control or format (bidi, zero-width) characters.
func TestAdv_PaletteNames(t *testing.T) {
	cases := []struct {
		name, val, code string
	}{
		{"empty", "", "invalid_length"},
		{"1 rune", "I", ""},
		{"40 runes", strings.Repeat("a", 40), ""},
		{"41 runes", strings.Repeat("a", 41), "invalid_length"},
		{"40 multibyte runes", strings.Repeat("é", 40), ""},
		{"40 emoji", strings.Repeat("\U0001f600", 40), ""},
		{"41 emoji", strings.Repeat("\U0001f600", 41), "invalid_length"},
		{"10 KB", strings.Repeat("a", 10<<10), "invalid_length"},
		{"NUL", "a\x00b", "invalid_value"},
		{"newline", "a\nb", "invalid_value"},
		{"tab", "a\tb", "invalid_value"},
		{"DEL", "a\x7fb", "invalid_value"},
		{"C1 control", "a\u0085b", "invalid_value"},
		{"RLO bidi override", "a\u202eb", "invalid_value"},
		{"LRI bidi isolate", "a\u2066b", "invalid_value"},
		{"zero width space", "a\u200bb", "invalid_value"},
		{"zero width joiner", "a\u200db", "invalid_value"},
		{"BOM / ZWNBSP", "a" + string(rune(0xFEFF)) + "b", "invalid_value"},
		{"tag character", "a\U000E0041b", "invalid_value"},
		{"soft hyphen", "a\u00adb", "invalid_value"},
		{"script tag as text", "<script>x</script>", ""}, // rendered as React text
		{"quotes and semicolon as text", `a";b'`, ""},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			raw := advMutate(t, oliveGrove(), func(m obj) { palAt(m, 0)["name"] = c.val })
			if c.code == "" {
				advCanon(t, raw)
				return
			}
			_, err := advValidate(t, raw)
			advWant(t, err, c.code, "palettes[0].name")
		})
	}
	t.Run("name missing", func(t *testing.T) {
		raw := advMutate(t, oliveGrove(), func(m obj) { delete(palAt(m, 0), "name") })
		_, err := advValidate(t, raw)
		advWant(t, err, "invalid_length", "palettes[0].name")
	})
}

// --- duplicate and case-folded keys ---

func TestAdv_DuplicateAndCaseFoldedKeys(t *testing.T) {
	good := string(advMutate(t, oliveGrove(), nil))
	swap := func(from, to string) string {
		if !strings.Contains(good, from) {
			t.Fatalf("fixture lacks %s", from)
		}
		return strings.Replace(good, from, to, 1)
	}
	t.Run("duplicate key, last wins, bad last is rejected", func(t *testing.T) {
		raw := swap(`"layout":"split"`, `"layout":"split","layout":"nope"`)
		_, err := advValidate(t, []byte(raw))
		advWant(t, err, "invalid_value", "layout")
	})
	t.Run("duplicate key, bad first is overwritten by a good last", func(t *testing.T) {
		raw := swap(`"layout":"split"`, `"layout":"nope","layout":"split"`)
		canon := advCanon(t, []byte(raw))
		if !bytes.Contains(canon, []byte(`"layout":"split"`)) || bytes.Contains(canon, []byte("nope")) {
			t.Errorf("canonical form must carry only the winning value: %.200s", canon)
		}
	})
	t.Run("duplicate src cannot smuggle through", func(t *testing.T) {
		raw := swap(`"kind":"paper"`, `"kind":"paper","src":"x"`)
		_, err := advValidate(t, []byte(raw))
		advWant(t, err, "invalid_field", "layers[0].src")
		raw = swap(`"kind":"paper"`, `"kind":"paper","src":"x","src":""`)
		canon := advCanon(t, []byte(raw))
		if bytes.Contains(canon, []byte(`"src"`)) {
			t.Errorf("empty src leaked into canonical form")
		}
	})
	t.Run("duplicate schema, last wins", func(t *testing.T) {
		raw := swap(`"schema":2`, `"schema":3,"schema":2`)
		advCanon(t, []byte(raw))
		raw = swap(`"schema":2`, `"schema":2,"schema":3`)
		_, err := advValidate(t, []byte(raw))
		advWant(t, err, "unsupported_schema", "schema")
	})
	t.Run("duplicate layers array, last wins", func(t *testing.T) {
		raw := swap(`"layers":[`, `"layers":[{"kind":"image","opacity":0.1}],"layers":[`)
		// encoding/json decodes the second array into the first one's slice
		// elements, so fields of the first array can survive into the second
		// (a merge, not a replacement). Whatever comes out must be a valid,
		// stable manifest or a rejection.
		if _, err := advValidate(t, []byte(raw)); err == nil {
			advCanon(t, []byte(raw))
		}
	})
	t.Run("duplicate palette colour, last wins and is validated", func(t *testing.T) {
		raw := swap(`"text":"#2C382D"`, `"text":"#2C382D","text":"red"`)
		_, err := advValidate(t, []byte(raw))
		advWant(t, err, "invalid_color", "palettes[0].colors.text")
	})
	t.Run("case-folded keys decode to the field and are canonicalised", func(t *testing.T) {
		// encoding/json matches object keys case-insensitively, so LAYOUT is
		// layout. The result is still validated, and the stored form is the
		// re-marshalled one with the exact lower-case key.
		raw := swap(`"layout":"split"`, `"LAYOUT":"split"`)
		canon := advCanon(t, []byte(raw))
		if bytes.Contains(canon, []byte("LAYOUT")) {
			t.Errorf("canonical form kept the upper-case key")
		}
		raw = swap(`"layout":"split"`, `"LAYOUT":"nope"`)
		_, err := advValidate(t, []byte(raw))
		advWant(t, err, "invalid_value", "layout")
		raw = swap(`"kind":"paper"`, `"KIND":"paper","SRC":"x"`)
		_, err = advValidate(t, []byte(raw))
		advWant(t, err, "invalid_field", "layers[0].src")
	})
	t.Run("unicode-folded key is unknown", func(t *testing.T) {
		// The Kelvin sign (U+212A) folds to k; encoding/json's case folding
		// is ASCII-plus-simple-fold, so it must either match or be rejected,
		// never be accepted as an unvalidated extra field.
		raw := swap(`"kind":"paper"`, `"\u212aind":"paper"`)
		if _, err := advValidate(t, []byte(raw)); err == nil {
			// Accepted only if it folded onto kind and validated as such.
			advCanon(t, []byte(raw))
		}
	})
	t.Run("trailing data after the object", func(t *testing.T) {
		// json.Decoder.Decode reads one value and ignores the rest. Whatever
		// trails must never reach the stored canonical form.
		for _, tail := range []string{" ", "\n{}", `{"schema":9}`, "garbage", "\x00", "]"} {
			raw := good + tail
			if m, err := advValidate(t, []byte(raw)); err == nil {
				c := advMarshal(t, m)
				if bytes.Contains(c, []byte("garbage")) || bytes.Contains(c, []byte("schema\":9")) {
					t.Errorf("trailing data %q leaked into canonical form", tail)
				}
				advCanon(t, []byte(raw))
			}
		}
	})
}

// --- (5) unknown fields at every depth, foreign fields, src ---

func TestAdv_UnknownFieldsAtEveryDepth(t *testing.T) {
	type site struct {
		name string
		base func() obj
		fn   func(m obj, k string)
	}
	sites := []site{
		{"top", oliveGrove, func(m obj, k string) { m[k] = 1 }},
		{"palette", oliveGrove, func(m obj, k string) { palAt(m, 0)[k] = 1 }},
		{"palette.colors", oliveGrove, func(m obj, k string) { colorsAt(m, 0)[k] = "#FFFFFF" }},
		{"font", oliveGrove, func(m obj, k string) { m["fonts"].([]any)[0].(obj)[k] = "x" }},
		{"defaults", oliveGrove, func(m obj, k string) { m["defaults"].(obj)[k] = "x" }},
		{"layer paper", oliveGrove, func(m obj, k string) { layerAt(m, 0)[k] = 1 }},
		{"layer art", oliveGrove, func(m obj, k string) { layerAt(m, 2)[k] = 1 }},
		{"layer frame", oliveGrove, func(m obj, k string) { layerAt(m, 4)[k] = 1 }},
		{"ornament", oliveGrove, func(m obj, k string) { m["ornament"].(obj)[k] = "x" }},
		{"card", oliveGrove, func(m obj, k string) { m["card"].(obj)[k] = "x" }},
		{"v1 top", func() obj { return validManifestMap() }, func(m obj, k string) { m[k] = 1 }},
		{"v1 palette", func() obj { return validManifestMap() }, func(m obj, k string) { palAt(m, 0)[k] = 1 }},
		{"v1 background", func() obj {
			m := validManifestMap()
			m["background"] = obj{"asset": "background", "opacity": 0.2}
			return m
		}, func(m obj, k string) { m["background"].(obj)[k] = 1 }},
	}
	for _, s := range sites {
		for _, k := range []string{"extra", "", "__proto__", "constructor", "Src2", "<script>", "layers2", "opacity_", " kind", "kind "} {
			t.Run(s.name+"/"+advLabel(k), func(t *testing.T) {
				raw := advMutate(t, s.base(), func(m obj) { s.fn(m, k) })
				_, err := advValidate(t, raw)
				advWant(t, err, "invalid_json", "manifest")
			})
		}
	}
}

func TestAdv_FieldsForeignToKind(t *testing.T) {
	// A representative value for every layer field (any non-zero value counts
	// as "set").
	vals := map[string]any{
		"region": "page", "tone": "background", "glow": "art1", "glow_opacity": 0.5,
		"vignette": "art1", "vignette_opacity": 0.5, "edge": "straight", "pattern": "dots",
		"texture": "grain", "art": "clouds", "frame": "single_hairline", "color": "accent",
		"colors": []string{"art1"}, "paint": "foil", "origin": "center", "mask": "none",
		"blend": "multiply", "placement": "top", "density": "normal", "opacity": 0.5, "inset": 5,
		"src": "/media/x/background",
	}
	for kind, allowed := range layerFieldSets {
		for field, v := range vals {
			if allowed[field] {
				continue
			}
			t.Run(kind+"/"+field, func(t *testing.T) {
				raw := advMutate(t, oliveGrove(), func(m obj) {
					l := obj{"kind": kind, field: v}
					if kind == kindPaper {
						setLayers(m, l)
					} else {
						setLayers(m, obj{"kind": "paper"}, l)
					}
				})
				idx := 1
				if kind == kindPaper {
					idx = 0
				}
				_, err := advValidate(t, raw)
				advWant(t, err, "invalid_field", fmt.Sprintf("layers[%d].%s", idx, field))
			})
		}
	}
	// Zero values are still "set" when they are pointers.
	t.Run("opacity 0 on a paper layer", func(t *testing.T) {
		raw := advMutate(t, oliveGrove(), func(m obj) { setLayers(m, obj{"kind": "paper", "opacity": 0}) })
		_, err := advValidate(t, raw)
		advWant(t, err, "invalid_field", "layers[0].opacity")
	})
	t.Run("inset 0 on a pattern layer", func(t *testing.T) {
		raw := advMutate(t, oliveGrove(), func(m obj) {
			setLayers(m, obj{"kind": "paper"}, obj{"kind": "pattern", "pattern": "dots", "color": "text", "opacity": 0.1, "inset": 0})
		})
		_, err := advValidate(t, raw)
		advWant(t, err, "invalid_field", "layers[1].inset")
	})
	t.Run("empty colors array on a paper layer", func(t *testing.T) {
		raw := advMutate(t, oliveGrove(), func(m obj) { setLayers(m, obj{"kind": "paper", "colors": []any{}}) })
		_, err := advValidate(t, raw)
		advWant(t, err, "invalid_field", "layers[0].colors")
	})
	t.Run("glow_opacity without glow", func(t *testing.T) {
		raw := advMutate(t, oliveGrove(), func(m obj) { setLayers(m, obj{"kind": "paper", "glow_opacity": 0.5}) })
		_, err := advValidate(t, raw)
		advWant(t, err, "invalid_field", "layers[0].glow_opacity")
	})
	t.Run("vignette_opacity without vignette", func(t *testing.T) {
		raw := advMutate(t, oliveGrove(), func(m obj) { setLayers(m, obj{"kind": "paper", "vignette_opacity": 0.5}) })
		_, err := advValidate(t, raw)
		advWant(t, err, "invalid_field", "layers[0].vignette_opacity")
	})
	t.Run("src on an image layer", func(t *testing.T) {
		for _, src := range []string{"/media/x/background", "https://evil.example/x.png", "javascript:alert(1)", "data:text/html,<script>", "//x", "x"} {
			raw := advMutate(t, oliveGrove(), func(m obj) {
				m["layers"] = append(layersOf(m), obj{"kind": "image", "opacity": 0.3, "src": src})
			})
			_, err := advValidate(t, raw)
			advWant(t, err, "invalid_field", "layers[5].src")
		}
	})
	t.Run("empty src is dropped, never stored", func(t *testing.T) {
		raw := advMutate(t, oliveGrove(), func(m obj) {
			m["layers"] = append(layersOf(m), obj{"kind": "image", "opacity": 0.3, "src": ""})
		})
		if canon := advCanon(t, raw); bytes.Contains(canon, []byte(`"src"`)) {
			t.Errorf("canonical form contains src")
		}
	})
	t.Run("src on every layer of a valid stack", func(t *testing.T) {
		for i := range oliveGrove()["layers"].([]obj) {
			raw := advMutate(t, oliveGrove(), func(m obj) { layerAt(m, i)["src"] = "/media/x/background" })
			_, err := advValidate(t, raw)
			advWant(t, err, "invalid_field", fmt.Sprintf("layers[%d].src", i))
		}
	})
}

// --- (6) cross-field combinations ---

func advPattern(blend string) obj {
	l := obj{"kind": "pattern", "pattern": "dots", "color": "text", "opacity": 0.1}
	if blend != "" {
		l["blend"] = blend
	}
	return l
}

func advTexture(tex, blend string) obj {
	l := obj{"kind": "texture", "texture": tex, "opacity": 0.1}
	if blend != "" {
		l["blend"] = blend
	}
	return l
}

// advNoFoil is a v2 manifest that uses no foil anywhere and whose palettes
// carry none: the base for the foil cross-reference cases.
func advNoFoil() obj {
	m := oliveGrove()
	for _, p := range m["palettes"].([]obj) {
		delete(p, "foil")
	}
	m["layers"].([]obj)[4] = obj{"kind": "frame", "frame": "double_hairline", "color": "accent"}
	m["card"] = obj{"style": "reply_card", "border": "hairline", "radius": 3, "fields": "underline", "buttons": "accent"}
	m["motion"] = "none"
	return m
}

func TestAdv_CrossFieldLayers(t *testing.T) {
	advRun(t, []advCase{
		{name: "paper at index 1", fn: func(m obj) { setLayers(m, advTexture("grain", ""), obj{"kind": "paper"}) }, code: "invalid_combination", path: "layers[1]"},
		{name: "paper last", fn: func(m obj) {
			setLayers(m, advTexture("grain", ""), advTexture("linen", ""), obj{"kind": "paper"})
		}, code: "invalid_combination", path: "layers[2]"},
		{name: "two papers", fn: func(m obj) { setLayers(m, obj{"kind": "paper"}, obj{"kind": "paper"}) }, code: "too_many", path: "layers[1]"},
		{name: "two frames", fn: func(m obj) {
			setLayers(m, obj{"kind": "frame", "frame": "single_hairline", "color": "text"}, obj{"kind": "frame", "frame": "scallop", "color": "text"})
		}, code: "too_many", path: "layers[1]"},
		{name: "three textures", fn: func(m obj) {
			setLayers(m, advTexture("grain", ""), advTexture("linen", ""), advTexture("wood", ""))
		}, code: "too_many", path: "layers[2]"},
		{name: "four patterns", fn: func(m obj) {
			setLayers(m, advPattern(""), advPattern(""), advPattern(""), advPattern(""))
		}, code: "too_many", path: "layers[3]"},
		{name: "four art layers", fn: func(m obj) {
			a := obj{"kind": "art", "art": "printers_corners", "placement": "corners", "colors": []string{"art1"}}
			setLayers(m, a, a, a, a)
		}, code: "too_many", path: "layers[3]"},
		{name: "two image layers", fn: func(m obj) {
			setLayers(m, obj{"kind": "image", "opacity": 0.1}, obj{"kind": "image", "opacity": 0.1})
		}, code: "too_many", path: "layers[1]"},
		{name: "two block layers", base: sprinkles, fn: func(m obj) {
			setLayers(m, obj{"kind": "block"}, obj{"kind": "block"})
		}, code: "too_many", path: "layers[1]"},
		{name: "3 blend layers accepted", fn: func(m obj) {
			setLayers(m, advTexture("grain", "multiply"), advTexture("linen", "screen"), advPattern("overlay"))
		}},
		{name: "4 blend layers", fn: func(m obj) {
			setLayers(m, advTexture("grain", "multiply"), advTexture("linen", "screen"), advPattern("overlay"), advPattern("soft-light"))
		}, code: "too_many", path: "layers[3].blend"},
		{name: "5 blend layers reports once", fn: func(m obj) {
			setLayers(m, advTexture("grain", "multiply"), advTexture("linen", "screen"), advPattern("overlay"), advPattern("soft-light"), advPattern("multiply"))
		}, code: "too_many", path: "layers[3].blend"},
		{name: "normal blends are not counted", fn: func(m obj) {
			setLayers(m, advTexture("grain", "normal"), advTexture("linen", "normal"), advPattern("normal"), advPattern("normal"), advPattern("multiply"))
		}},
		{name: "block without color_block", base: sprinkles, fn: func(m obj) { m["hero_style"] = "framed" }, code: "invalid_combination", path: "layers"},
		{name: "color_block without block", base: sprinkles, fn: func(m obj) {
			setLayers(m, obj{"kind": "paper"}, advPattern(""))
		}, code: "invalid_combination", path: "layers"},
		{name: "color_block with no layers at all", base: sprinkles, fn: func(m obj) { delete(m, "layers") }, code: "invalid_combination", path: "layers"},
		{name: "block region page", base: sprinkles, fn: func(m obj) { layerAt(m, 1)["region"] = "page" }, code: "invalid_combination", path: "layers[1].region"},
		{name: "block region hero explicit accepted", base: sprinkles, fn: func(m obj) { layerAt(m, 1)["region"] = "hero" }},
		{name: "paper region hero", fn: func(m obj) { layerAt(m, 0)["region"] = "hero" }, code: "invalid_combination", path: "layers[0].region"},
		{name: "frame region hero", fn: func(m obj) { layerAt(m, 4)["region"] = "hero" }, code: "invalid_combination", path: "layers[4].region"},
		{name: "pattern/art/texture/image may be hero", fn: func(m obj) {
			setLayers(m, obj{"kind": "paper"},
				obj{"kind": "pattern", "pattern": "dots", "color": "text", "opacity": 0.1, "region": "hero"},
				obj{"kind": "texture", "texture": "grain", "opacity": 0.1, "region": "hero"},
				obj{"kind": "art", "art": "printers_corners", "placement": "corners", "colors": []string{"art1"}, "region": "hero"},
				obj{"kind": "image", "opacity": 0.2, "region": "hero"})
		}},
		{name: "hero_style color_block in v1", base: func() obj { return validManifestMap() }, fn: func(m obj) { m["hero_style"] = "color_block" }, code: "invalid_value", path: "hero_style"},
		{name: "v2 hero_style spotlight accepted", fn: func(m obj) { m["hero_style"] = "spotlight" }},
		// texture colours
		{name: "watercolour with 2 colours", fn: func(m obj) {
			setLayers(m, obj{"kind": "texture", "texture": "watercolour", "opacity": 0.1, "colors": []string{"art1", "art2"}})
		}, code: "invalid_combination", path: "layers[0].colors"},
		{name: "watercolour with 4 colours", fn: func(m obj) {
			setLayers(m, obj{"kind": "texture", "texture": "watercolour", "opacity": 0.1, "colors": []string{"art1", "art2", "art3", "art4"}})
		}, code: "invalid_combination", path: "layers[0].colors"},
		{name: "watercolour with no colours", fn: func(m obj) {
			setLayers(m, obj{"kind": "texture", "texture": "watercolour", "opacity": 0.1})
		}, code: "invalid_combination", path: "layers[0].colors"},
		{name: "watercolour with 3 colours accepted", fn: func(m obj) {
			setLayers(m, obj{"kind": "texture", "texture": "watercolour", "opacity": 0.1, "colors": []string{"art1", "art2", "art3"}})
		}},
		{name: "grain with colours", fn: func(m obj) {
			setLayers(m, obj{"kind": "texture", "texture": "grain", "opacity": 0.1, "colors": []string{"art1"}})
		}, code: "invalid_combination", path: "layers[0].colors"},
		// frame colour xor paint
		{name: "frame with colour and paint", fn: func(m obj) { layerAt(m, 4)["color"] = "accent" }, code: "invalid_combination", path: "layers[4]"},
		{name: "frame with neither", fn: func(m obj) { delete(layerAt(m, 4), "paint") }, code: "missing_value", path: "layers[4].color"},
		{name: "frame inset omitted defaults to 12", fn: func(m obj) { delete(layerAt(m, 4), "inset") }},
		// art
		{name: "art with colours and paint", base: giltNoir, fn: func(m obj) { layerAt(m, 2)["colors"] = []string{"art1"} }, code: "invalid_combination", path: "layers[2]"},
		{name: "art paint on a non-foil key", fn: func(m obj) { delete(layerAt(m, 2), "colors"); layerAt(m, 2)["paint"] = "foil" }, code: "invalid_combination", path: "layers[2].paint"},
		{name: "art key unknown, placement fine", fn: func(m obj) { layerAt(m, 2)["art"] = "nope" }, code: "invalid_value", path: "layers[2].art"},
		{name: "art density on balloons", base: sprinkles, fn: func(m obj) { layerAt(m, 4)["density"] = "dense" }, code: "invalid_field", path: "layers[4].density"},
		{name: "art density on olive_branches", fn: func(m obj) { layerAt(m, 2)["density"] = "normal" }, code: "invalid_field", path: "layers[2].density"},
		{name: "art density bad value on confetti", base: sprinkles, fn: func(m obj) { layerAt(m, 3)["density"] = "extreme" }, code: "invalid_value", path: "layers[3].density"},
		{name: "art placement allowed elsewhere but not here", fn: func(m obj) { layerAt(m, 2)["placement"] = "hero" }, code: "invalid_combination", path: "layers[2].placement"},
		{name: "art placement unknown anywhere", fn: func(m obj) { layerAt(m, 2)["placement"] = "everywhere" }, code: "invalid_value", path: "layers[2].placement"},
		{name: "art placement empty", fn: func(m obj) { layerAt(m, 2)["placement"] = "" }, code: "invalid_value", path: "layers[2].placement"},
		{name: "art colours max+1 on olive_branches", fn: func(m obj) {
			layerAt(m, 2)["colors"] = []string{"art1", "art2", "art3", "art4", "art5"}
		}, code: "too_many", path: "layers[2].colors"},
		{name: "art colours min-1 on olive_branches", fn: func(m obj) { layerAt(m, 2)["colors"] = []string{"art1", "art2"} }, code: "invalid_length", path: "layers[2].colors"},
		{name: "art colours none on olive_branches", fn: func(m obj) { delete(layerAt(m, 2), "colors") }, code: "invalid_length", path: "layers[2].colors"},
	})
}

// Every art registry key against every placement, colour count, paint and
// density value, with an independent oracle taken from the spec table.
func TestAdv_ArtRegistryMatrix(t *testing.T) {
	type row struct {
		min, max     int
		foil, dens   bool
		placementSet []string
	}
	spec := map[string]row{
		"olive_branches":   {3, 4, false, false, []string{"corners", "top_corners", "bottom_corners"}},
		"botanical_wash":   {2, 4, false, false, []string{"corners", "top_corners"}},
		"confetti":         {2, 6, false, true, []string{"hero", "edges", "scatter"}},
		"balloons":         {2, 4, false, false, []string{"hero_top", "top_corners"}},
		"stepped_arches":   {0, 1, true, false, []string{"hero"}},
		"deco_fans":        {0, 1, true, false, []string{"bottom_corners", "corners"}},
		"clouds":           {1, 3, false, true, []string{"top", "scatter"}},
		"open_door_plants": {2, 3, false, false, []string{"hero", "bottom_corners"}},
		"terrazzo_chips":   {2, 6, false, true, []string{"scatter", "edges"}},
		"riso_shapes":      {2, 3, false, false, []string{"corners", "scatter"}},
		"mirror_ball":      {1, 2, true, false, []string{"hero_top"}},
		"sparkles":         {1, 2, true, true, []string{"scatter", "hero"}},
		"printers_corners": {1, 1, true, false, []string{"corners"}},
		"daisies":          {2, 3, false, true, []string{"corners", "edges"}},
	}
	if len(spec) != len(artRegistry) {
		t.Fatalf("registry has %d keys, spec table %d", len(artRegistry), len(spec))
	}
	allPlacements := map[string]bool{}
	for _, r := range spec {
		for _, p := range r.placementSet {
			allPlacements[p] = true
		}
	}
	toks := func(n int) []string {
		out := make([]string, n)
		for i := range out {
			out[i] = fmt.Sprintf("art%d", i%8+1)
		}
		return out
	}
	layer := func(m obj, l obj) { setLayers(m, obj{"kind": "paper"}, l) }
	keys := make([]string, 0, len(spec))
	for k := range spec {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	for _, key := range keys {
		r := spec[key]
		got := artRegistry[key]
		if got.minColors != r.min || got.maxColors != r.max || got.foil != r.foil || got.density != r.dens || len(got.placements) != len(r.placementSet) {
			t.Errorf("%s: registry %+v differs from the spec row %+v", key, got, r)
		}
		okPl := map[string]bool{}
		for _, p := range r.placementSet {
			okPl[p] = true
		}
		t.Run(key, func(t *testing.T) {
			for pl := range allPlacements {
				for n := r.min - 1; n <= r.max+1; n++ {
					if n < 0 {
						continue
					}
					l := obj{"kind": "art", "art": key, "placement": pl}
					if n > 0 {
						l["colors"] = toks(n)
					}
					raw := advMutate(t, oliveGrove(), func(m obj) { layer(m, l) })
					wantOK := okPl[pl] && n >= r.min && n <= r.max
					if wantOK {
						advCanon(t, raw)
						continue
					}
					_, err := advValidate(t, raw)
					switch {
					case !okPl[pl]:
						advWant(t, err, "invalid_combination", "layers[1].placement")
					case n > r.max:
						advWant(t, err, "too_many", "layers[1].colors")
					default:
						advWant(t, err, "invalid_length", "layers[1].colors")
					}
				}
			}
			// paint
			pl := r.placementSet[0]
			raw := advMutate(t, giltNoir(), func(m obj) {
				layer(m, obj{"kind": "art", "art": key, "placement": pl, "paint": "foil"})
				m["card"].(obj)["buttons"] = "accent"
			})
			if r.foil {
				// A foil-capable key painted with foil is fine when the
				// colour-free form is allowed, or an explicit hex is not
				// needed. min colours don't apply to paint.
				advCanon(t, raw)
			} else {
				_, err := advValidate(t, raw)
				advWant(t, err, "invalid_combination", "layers[1].paint")
			}
			// density
			for _, d := range []string{"sparse", "normal", "dense"} {
				l := obj{"kind": "art", "art": key, "placement": pl, "density": d}
				if r.min > 0 {
					l["colors"] = toks(r.min)
				}
				raw := advMutate(t, oliveGrove(), func(m obj) { layer(m, l) })
				if r.dens {
					advCanon(t, raw)
				} else {
					_, err := advValidate(t, raw)
					advWant(t, err, "invalid_field", "layers[1].density")
				}
			}
			// opacity of art: 0.1..1
			for _, op := range []float64{0.1, 0.5, 1} {
				l := obj{"kind": "art", "art": key, "placement": pl, "opacity": op}
				if r.min > 0 {
					l["colors"] = toks(r.min)
				}
				advCanon(t, advMutate(t, oliveGrove(), func(m obj) { layer(m, l) }))
			}
		})
	}
}

func TestAdv_CrossFieldFoil(t *testing.T) {
	users := map[string]func(m obj){
		"frame paint foil": func(m obj) { layerAt(m, 4)["paint"] = "foil"; delete(layerAt(m, 4), "color") },
		"foil texture":     func(m obj) { layerAt(m, 1)["texture"] = "foil" },
		"art paint foil": func(m obj) {
			setLayers(m, obj{"kind": "paper"}, obj{"kind": "art", "art": "stepped_arches", "placement": "hero", "paint": "foil"})
		},
		"card border foil":      func(m obj) { m["card"].(obj)["border"] = "foil" },
		"card border inset":     func(m obj) { m["card"].(obj)["border"] = "foil_inset" },
		"card buttons foil":     func(m obj) { m["card"].(obj)["buttons"] = "foil" },
		"ornament foil_numeral": func(m obj) { m["ornament"].(obj)["hero"] = "foil_numeral" },
		"ornament foil_seal":    func(m obj) { m["ornament"].(obj)["badge"] = "foil_seal" },
		"motion foil_sheen":     func(m obj) { m["motion"] = "foil_sheen" },
	}
	names := make([]string, 0, len(users))
	for n := range users {
		names = append(names, n)
	}
	sort.Strings(names)
	for _, name := range names {
		use := users[name]
		t.Run(name+" without palette foil", func(t *testing.T) {
			raw := advMutate(t, advNoFoil(), use)
			_, err := advValidate(t, raw)
			for i := 0; i < 3; i++ {
				advWant(t, err, "missing_palette_color", fmt.Sprintf("palettes[%d].foil", i))
			}
		})
		t.Run(name+" with foil only on palette 0", func(t *testing.T) {
			// Foil colours must be bright enough for the foil-specific
			// contrast rules, so give palette 0 the dark gilt palette's foil.
			raw := advMutate(t, advNoFoil(), func(m obj) {
				use(m)
				palAt(m, 0)["foil"] = []string{"#8E6B2E", "#C9A963", "#F3E2AA", "#B48A43", "#E6CC8B"}
			})
			_, err := advValidate(t, raw)
			if hasIssue(err, "missing_palette_color", "palettes[0].foil") {
				t.Fatalf("palette 0 has foil, must not be reported:%s", advDump(err))
			}
			advWant(t, err, "missing_palette_color", "palettes[1].foil")
			advWant(t, err, "missing_palette_color", "palettes[2].foil")
		})
	}
	t.Run("foil colours present but unused is fine", func(t *testing.T) {
		raw := advMutate(t, advNoFoil(), func(m obj) {
			for pi := range m["palettes"].([]any) {
				palAt(m, pi)["foil"] = []string{"#8E6B2E", "#C9A963", "#F3E2AA"}
			}
		})
		advCanon(t, raw)
	})
	// artN needs N art colours in every palette.
	for n := 1; n <= 8; n++ {
		t.Run(fmt.Sprintf("art%d needs %d art colours", n, n), func(t *testing.T) {
			tok := fmt.Sprintf("art%d", n)
			mk := func(have int) []byte {
				return advMutate(t, oliveGrove(), func(m obj) {
					cols := []any{"#101010", "#202020", "#303030", "#404040", "#505050", "#606060", "#707070", "#808080"}
					for pi := range m["palettes"].([]any) {
						palAt(m, pi)["art"] = cols[:have]
					}
					setLayers(m, obj{"kind": "paper", "glow": tok, "glow_opacity": 0.02})
				})
			}
			advCanon(t, mk(n))
			if n > 1 {
				_, err := advValidate(t, mk(n-1))
				advWant(t, err, "missing_palette_color", "palettes[0].art")
				advWant(t, err, "missing_palette_color", "palettes[2].art")
			}
			// Only one short palette.
			raw := advMutate(t, oliveGrove(), func(m obj) {
				cols := []any{"#101010", "#202020", "#303030", "#404040", "#505050", "#606060", "#707070", "#808080"}
				palAt(m, 0)["art"] = cols[:n]
				palAt(m, 1)["art"] = cols[:n]
				palAt(m, 2)["art"] = cols[:n-1]
				setLayers(m, obj{"kind": "paper", "glow": tok, "glow_opacity": 0.02})
			})
			_, err := advValidate(t, raw)
			advWant(t, err, "missing_palette_color", "palettes[2].art")
			if hasIssue(err, "missing_palette_color", "palettes[0].art") {
				t.Errorf("palette 0 has enough art colours")
			}
		})
	}
	// The token needing the most art colours in any slot is the one counted.
	slots := map[string]func(m obj, tok string){
		"paper.tone":     func(m obj, tok string) { setLayers(m, obj{"kind": "paper", "tone": tok}) },
		"paper.glow":     func(m obj, tok string) { setLayers(m, obj{"kind": "paper", "glow": tok, "glow_opacity": 0.02}) },
		"paper.vignette": func(m obj, tok string) { setLayers(m, obj{"kind": "paper", "vignette": tok}) },
		"pattern.color": func(m obj, tok string) {
			setLayers(m, obj{"kind": "paper"}, obj{"kind": "pattern", "pattern": "dots", "color": tok, "opacity": 0.1})
		},
		"frame.color": func(m obj, tok string) { setLayers(m, obj{"kind": "frame", "frame": "single_hairline", "color": tok}) },
		"texture.colors": func(m obj, tok string) {
			setLayers(m, obj{"kind": "texture", "texture": "watercolour", "opacity": 0.1, "colors": []string{"art1", "art1", tok}})
		},
		"hero_ink": func(m obj, tok string) { setLayers(m, obj{"kind": "paper"}); m["ornament"].(obj)["hero_ink"] = tok },
	}
	for name, set := range slots {
		t.Run(name+" art8 vs 7 art colours", func(t *testing.T) {
			raw := advMutate(t, oliveGrove(), func(m obj) {
				for pi := range m["palettes"].([]any) {
					palAt(m, pi)["art"] = []string{"#101010", "#202020", "#303030", "#404040", "#505050", "#606060", "#707070"}
				}
				set(m, "art8")
			})
			_, err := advValidate(t, raw)
			advWant(t, err, "missing_palette_color", "palettes[0].art")
		})
	}
}

func TestAdv_CrossFieldCard(t *testing.T) {
	advRun(t, []advCase{
		{name: "chamfered with radius 1", fn: func(m obj) { m["card"] = obj{"style": "chamfered", "radius": 1} }, code: "invalid_combination", path: "card.radius"},
		{name: "chamfered with radius 32", fn: func(m obj) { m["card"] = obj{"style": "chamfered", "radius": 32} }, code: "invalid_combination", path: "card.radius"},
		{name: "chamfered with radius 0", fn: func(m obj) { m["card"] = obj{"style": "chamfered", "radius": 0} }},
		{name: "chamfered radius omitted", fn: func(m obj) { m["card"] = obj{"style": "chamfered"} }},
		{name: "chamfered radius null", fn: func(m obj) { m["card"] = obj{"style": "chamfered", "radius": nil} }},
		{name: "chamfered radius out of range wins", fn: func(m obj) { m["card"] = obj{"style": "chamfered", "radius": 33} }, code: "out_of_range", path: "card.radius"},
		{name: "soft with radius 0", fn: func(m obj) { m["card"] = obj{"style": "soft", "radius": 0} }},
		{name: "none card keeps defaults", fn: func(m obj) { m["card"] = obj{"style": "none"} }},
		{name: "empty card object", fn: func(m obj) { m["card"] = obj{} }},
		{name: "empty ornament object", fn: func(m obj) { m["ornament"] = obj{} }},
	})
}

func TestAdv_CrossFieldContrast(t *testing.T) {
	need := func(t *testing.T, fg, bg string, floor float64, wantPass bool) {
		t.Helper()
		r := contrastRatio(fg, bg)
		if wantPass && r < floor || !wantPass && r >= floor {
			t.Fatalf("precondition: contrast(%s,%s)=%.3f, wantPass=%v vs %.1f", fg, bg, r, wantPass, floor)
		}
	}
	white := func(m obj) {
		advSolo(m)
		c := colorsAt(m, 0)
		c["background"], c["surface"] = "#FFFFFF", "#FFFFFF"
		c["text"], c["muted"], c["accent"], c["accent_text"] = "#000000", "#767676", "#000000", "#FFFFFF"
		palAt(m, 0)["accent_ink"] = "#767676"
	}
	t.Run("preconditions", func(t *testing.T) {
		need(t, "#767676", "#FFFFFF", 4.5, true)
		need(t, "#777777", "#FFFFFF", 4.5, false)
		need(t, "#949494", "#FFFFFF", 3, true)
		need(t, "#959595", "#FFFFFF", 3, false)
	})

	advRun(t, []advCase{
		{name: "white baseline accepted", fn: white},
		{name: "muted 4.54 on bg+surface accepted", fn: func(m obj) { white(m); colorsAt(m, 0)["muted"] = "#767676" }},
		{name: "muted 4.48 on bg", fn: func(m obj) { white(m); colorsAt(m, 0)["muted"] = "#777777" }, code: "low_contrast", path: "palettes[0].colors"},
		{name: "muted fine on bg, fails on surface only", fn: func(m obj) {
			white(m)
			colorsAt(m, 0)["surface"] = "#D0D0D0"
		}, code: "low_contrast", path: "palettes[0].colors"},
		{name: "text 4.48 on bg", fn: func(m obj) { white(m); colorsAt(m, 0)["text"] = "#777777"; colorsAt(m, 0)["muted"] = "#000000" }, code: "low_contrast", path: "palettes[0].colors"},
		{name: "accent_text vs accent 4.48", fn: func(m obj) {
			white(m)
			colorsAt(m, 0)["accent"], colorsAt(m, 0)["accent_text"] = "#777777", "#FFFFFF"
		}, code: "low_contrast", path: "palettes[0].colors"},
		{name: "accent_ink 4.54 accepted", fn: func(m obj) { white(m) }},
		{name: "accent_ink 4.48 vs background", fn: func(m obj) { white(m); palAt(m, 0)["accent_ink"] = "#777777" }, code: "low_contrast", path: "palettes[0].accent_ink"},
		{name: "accent_ink fine on background, fails on surface", fn: func(m obj) {
			white(m)
			colorsAt(m, 0)["surface"] = "#D0D0D0"
			colorsAt(m, 0)["muted"] = "#000000"
		}, code: "low_contrast", path: "palettes[0].accent_ink"},
		{name: "glass: text/muted fail only on the blend", fn: func(m obj) {
			white(m)
			colorsAt(m, 0)["background"], colorsAt(m, 0)["surface"] = "#000000", "#FFFFFF"
			colorsAt(m, 0)["text"], colorsAt(m, 0)["muted"] = "#767676", "#767676"
			colorsAt(m, 0)["accent"], colorsAt(m, 0)["accent_text"] = "#000000", "#FFFFFF"
			delete(palAt(m, 0), "accent_ink")
			m["card"] = obj{"style": "glass"}
		}, code: "low_contrast", path: "palettes[0].colors"},
		{name: "glass: same colours accepted when the card is soft", fn: func(m obj) {
			white(m)
			colorsAt(m, 0)["background"], colorsAt(m, 0)["surface"] = "#000000", "#FFFFFF"
			colorsAt(m, 0)["text"], colorsAt(m, 0)["muted"] = "#767676", "#767676"
			delete(palAt(m, 0), "accent_ink")
			m["card"] = obj{"style": "soft"}
		}},
		{name: "glass: accent_ink fails only on the blend", fn: func(m obj) {
			white(m)
			colorsAt(m, 0)["background"], colorsAt(m, 0)["surface"] = "#000000", "#FFFFFF"
			colorsAt(m, 0)["text"], colorsAt(m, 0)["muted"] = "#000000", "#000000"
			palAt(m, 0)["accent_ink"] = "#767676"
			m["card"] = obj{"style": "glass"}
		}, code: "low_contrast", path: "palettes[0].accent_ink"},
		{name: "glass: accent_ink accepted on a soft card", fn: func(m obj) {
			white(m)
			colorsAt(m, 0)["background"], colorsAt(m, 0)["surface"] = "#000000", "#FFFFFF"
			colorsAt(m, 0)["text"], colorsAt(m, 0)["muted"] = "#767676", "#767676"
			palAt(m, 0)["accent_ink"] = "#767676"
			m["card"] = obj{"style": "soft"}
		}},
		// hero ink
		{name: "hero_ink explicit 3.03 accepted", fn: func(m obj) {
			white(m)
			palAt(m, 0)["art"] = []string{"#949494", "#959595", "#000000", "#000000", "#000000", "#000000"}
			m["ornament"] = obj{"hero_ink": "art1"}
		}},
		{name: "hero_ink explicit 2.99 on page hero", fn: func(m obj) {
			white(m)
			palAt(m, 0)["art"] = []string{"#949494", "#959595", "#000000", "#000000", "#000000", "#000000"}
			m["ornament"] = obj{"hero_ink": "art2"}
		}, code: "low_contrast", path: "ornament.hero_ink"},
		{name: "hero_ink equals background", fn: func(m obj) { m["ornament"].(obj)["hero_ink"] = "background" }, code: "low_contrast", path: "ornament.hero_ink"},
		{name: "hero_ink default on color_block fails when accent_text == accent", base: sprinkles, fn: func(m obj) {
			advSolo(m)
			colorsAt(m, 0)["accent_text"] = colorsAt(m, 0)["accent"].(string)
		}, code: "low_contrast", path: "ornament.hero_ink"},
		{name: "hero_ink on color_block is judged against accent, not background", base: sprinkles, fn: func(m obj) {
			m["ornament"].(obj)["hero_ink"] = "background"
		}, code: ""},
		{name: "hero_ink accent on color_block", base: sprinkles, fn: func(m obj) { m["ornament"].(obj)["hero_ink"] = "accent" }, code: "low_contrast", path: "ornament.hero_ink"},
		{name: "hero_ink low only in one palette", fn: func(m obj) {
			// palette 1 (sage) art1 vs its background is the only weak pair.
			palAt(m, 1)["art"].([]any)[0] = "#E9EEE2"
			m["ornament"].(obj)["hero_ink"] = "art1"
		}, code: "low_contrast", path: "ornament.hero_ink"},
		// foil numeral and foil buttons
		{name: "foil_numeral stops 3.03 accepted", base: giltNoir, fn: func(m obj) {
			white(m)
			palAt(m, 0)["foil"] = []string{"#949494", "#949494", "#949494"}
			m["card"] = obj{"style": "soft", "buttons": "accent"}
			m["layers"] = []any{obj{"kind": "paper"}}
		}},
		{name: "foil_numeral one stop 2.99", base: giltNoir, fn: func(m obj) {
			white(m)
			palAt(m, 0)["foil"] = []string{"#949494", "#959595", "#949494"}
			m["card"] = obj{"style": "soft", "buttons": "accent"}
			m["layers"] = []any{obj{"kind": "paper"}}
		}, code: "low_contrast", path: "ornament.hero"},
		{name: "foil buttons 4.54 on every stop accepted", base: giltNoir, fn: func(m obj) {
			white(m)
			palAt(m, 0)["foil"] = []string{"#767676", "#767676", "#767676"}
			m["layers"] = []any{obj{"kind": "paper"}}
		}},
		{name: "foil buttons one stop 4.48", base: giltNoir, fn: func(m obj) {
			white(m)
			palAt(m, 0)["foil"] = []string{"#767676", "#777777", "#767676"}
			m["layers"] = []any{obj{"kind": "paper"}}
		}, code: "low_contrast", path: "card.buttons"},
		{name: "foil buttons: accent button text irrelevant when buttons=accent", base: giltNoir, fn: func(m obj) {
			white(m)
			palAt(m, 0)["foil"] = []string{"#777777", "#777777", "#777777"}
			m["card"] = obj{"style": "soft", "buttons": "accent"}
			m["ornament"] = obj{"hero": "none", "badge": "none"}
			m["layers"] = []any{obj{"kind": "paper"}}
		}},
	})
}

// --- (7) schema matrix ---

func TestAdv_SchemaMatrix(t *testing.T) {
	setSchema := func(lit string) func(m obj) { return func(m obj) { m["schema"] = json.Number(lit) } }
	v1With := func(k string, v any) advCase {
		return advCase{name: "schema 1 + " + k, base: func() obj { return validManifestMap() }, fn: func(m obj) { m[k] = v }, code: "schema_mismatch", path: k}
	}
	advRun(t, []advCase{
		v1With("layers", []any{}),
		v1With("layers", []any{obj{"kind": "paper"}}),
		v1With("ornament", obj{}),
		v1With("card", obj{}),
		v1With("motion", "draw_on"),
		v1With("motion", "x"),
		{name: "schema 1 + palette accent_ink", base: func() obj { return validManifestMap() }, fn: func(m obj) { palAt(m, 0)["accent_ink"] = "#000000" }, code: "schema_mismatch", path: "palettes[0]"},
		{name: "schema 1 + palette art", base: func() obj { return validManifestMap() }, fn: func(m obj) { palAt(m, 0)["art"] = []string{"#000000"} }, code: "schema_mismatch", path: "palettes[0]"},
		{name: "schema 1 + palette foil", base: func() obj { return validManifestMap() }, fn: func(m obj) { palAt(m, 0)["foil"] = []string{"#000000"} }, code: "schema_mismatch", path: "palettes[0]"},
		{name: "schema 1 + null v2 fields accepted", base: func() obj { return validManifestMap() }, fn: func(m obj) {
			m["layers"], m["ornament"], m["card"] = nil, nil, nil
		}},
		{name: "schema 1 + empty motion accepted", base: func() obj { return validManifestMap() }, fn: func(m obj) { m["motion"] = "" }},
		{name: "schema 2 + decoration", fn: func(m obj) { m["decoration"] = "line" }, code: "schema_mismatch", path: "decoration"},
		{name: "schema 2 + surface", fn: func(m obj) { m["surface"] = "plain" }, code: "schema_mismatch", path: "surface"},
		{name: "schema 2 + texture", fn: func(m obj) { m["texture"] = "none" }, code: "schema_mismatch", path: "texture"},
		{name: "schema 2 + background object", fn: func(m obj) { m["background"] = obj{} }, code: "schema_mismatch", path: "background"},
		{name: "schema 2 + background null accepted", fn: func(m obj) { m["background"] = nil }},
		{name: "schema 2 + background omitted accepted", fn: func(m obj) { delete(m, "background") }},
		{name: "schema 2 + decoration empty accepted", fn: func(m obj) { m["decoration"] = "" }},
		{name: "schema 0", fn: setSchema("0"), code: "unsupported_schema", path: "schema"},
		{name: "schema 3", fn: setSchema("3"), code: "unsupported_schema", path: "schema"},
		{name: "schema -1", fn: setSchema("-1"), code: "unsupported_schema", path: "schema"},
		{name: "schema 99", fn: setSchema("99"), code: "unsupported_schema", path: "schema"},
		{name: "schema max int", fn: setSchema("9223372036854775807"), code: "unsupported_schema", path: "schema"},
		{name: "schema -0", fn: setSchema("-0"), code: "unsupported_schema", path: "schema"},
		{name: "schema missing", fn: func(m obj) { delete(m, "schema") }, code: "unsupported_schema", path: "schema"},
		{name: "schema null", fn: func(m obj) { m["schema"] = nil }, code: "unsupported_schema", path: "schema"},
		{name: "schema string 2", fn: func(m obj) { m["schema"] = "2" }, code: "invalid_json", path: "manifest"},
		{name: "schema string 1", fn: func(m obj) { m["schema"] = "1" }, code: "invalid_json", path: "manifest"},
		{name: "schema 2.0", fn: setSchema("2.0"), code: "invalid_json", path: "manifest"},
		{name: "schema 2.5", fn: setSchema("2.5"), code: "invalid_json", path: "manifest"},
		{name: "schema 1e0", fn: setSchema("1e0"), code: "invalid_json", path: "manifest"},
		{name: "schema 2e0", fn: setSchema("2e0"), code: "invalid_json", path: "manifest"},
		{name: "schema true", fn: func(m obj) { m["schema"] = true }, code: "invalid_json", path: "manifest"},
		{name: "schema array", fn: func(m obj) { m["schema"] = []any{2} }, code: "invalid_json", path: "manifest"},
		{name: "schema overflow", fn: setSchema("9223372036854775808"), code: "invalid_json", path: "manifest"},
		{name: "schema 3 with garbage elsewhere stays a single issue", fn: func(m obj) { m["schema"] = 3; m["layout"] = "nope" }, code: "unsupported_schema", path: "schema"},
	})
	t.Run("schema 3 reports exactly one issue", func(t *testing.T) {
		raw := advMutate(t, oliveGrove(), func(m obj) { m["schema"] = 3; m["layout"] = "nope" })
		_, err := advValidate(t, raw)
		if n := len(advIssues(err)); n != 1 {
			t.Errorf("%d issues, want 1", n)
		}
	})
}

// --- (8) size ---

// advPadTo returns base with fonts[0].name padded so the compact JSON is
// exactly n bytes.
func advPadTo(t *testing.T, base obj, n int) []byte {
	t.Helper()
	build := func(pad int) []byte {
		return advMutate(t, base, func(m obj) { m["fonts"].([]any)[0].(obj)["name"] = strings.Repeat("x", pad) })
	}
	l1 := len(build(1))
	out := build(1 + n - l1)
	if len(out) != n {
		t.Fatalf("padding produced %d bytes, want %d", len(out), n)
	}
	return out
}

// advPadWS returns base as compact JSON padded with trailing whitespace to
// exactly n bytes: it grows the raw input without growing the canonical form.
func advPadWS(t *testing.T, base obj, n int) []byte {
	t.Helper()
	raw := advMarshal(t, base)
	if len(raw) > n {
		t.Fatalf("base is %d bytes, over %d", len(raw), n)
	}
	return append(raw, bytes.Repeat([]byte(" "), n-len(raw))...)
}

func TestAdv_SizeBoundary(t *testing.T) {
	for name, base := range map[string]func() obj{"v2 olive": oliveGrove, "v1": func() obj { return validManifestMap() }} {
		t.Run(name+"/exactly 24 KiB accepted", func(t *testing.T) {
			raw := advPadWS(t, base(), maxManifestBytes)
			if len(raw) != 24576 {
				t.Fatalf("len = %d", len(raw))
			}
			if _, err := advValidate(t, raw); err != nil {
				t.Fatalf("exactly 24 KiB must be accepted:%s", advDump(err))
			}
		})
		t.Run(name+"/24 KiB + 1 rejected before decoding", func(t *testing.T) {
			raw := advPadWS(t, base(), maxManifestBytes+1)
			_, err := advValidate(t, raw)
			advWant(t, err, "too_large", "manifest")
			if n := len(advIssues(err)); n != 1 {
				t.Errorf("%d issues, want exactly the size issue", n)
			}
		})
		t.Run(name+"/24 KiB - 1 accepted", func(t *testing.T) {
			if _, err := advValidate(t, advPadWS(t, base(), maxManifestBytes-1)); err != nil {
				t.Fatalf("%s", advDump(err))
			}
		})
	}
	t.Run("size check precedes decoding", func(t *testing.T) {
		// Not JSON at all: at the cap it is invalid_json, one byte over it
		// is too_large, proving the cap runs first.
		_, err := advValidate(t, bytes.Repeat([]byte("{"), maxManifestBytes))
		advWant(t, err, "invalid_json", "manifest")
		_, err = advValidate(t, bytes.Repeat([]byte("{"), maxManifestBytes+1))
		advWant(t, err, "too_large", "manifest")
		_, err = advValidate(t, bytes.Repeat([]byte{0xff}, 1<<20))
		advWant(t, err, "too_large", "manifest")
	})
	t.Run("whitespace padding counts", func(t *testing.T) {
		base := advMutate(t, oliveGrove(), nil)
		raw := append(append([]byte{}, base...), bytes.Repeat([]byte(" "), maxManifestBytes-len(base))...)
		if _, err := advValidate(t, raw); err != nil {
			t.Fatalf("%s", advDump(err))
		}
		_, err := advValidate(t, append(raw, ' '))
		advWant(t, err, "too_large", "manifest")
	})
}

// pgJSONB renders compact JSON the way PostgreSQL prints a jsonb value: one
// space after every ':' and ',' outside strings.
func pgJSONB(compact []byte) []byte {
	var out bytes.Buffer
	inStr, esc := false, false
	for _, c := range compact {
		out.WriteByte(c)
		switch {
		case esc:
			esc = false
		case inStr && c == '\\':
			esc = true
		case c == '"':
			inStr = !inStr
		case !inStr && (c == ':' || c == ','):
			out.WriteByte(' ')
		}
	}
	return out.Bytes()
}

// Regression for two read-back bugs. The 24 KiB cap is checked against the
// raw input, but what is stored is the canonical re-marshalled form (defaults
// written out, so it can be bigger than the input), and what is read back is
// PostgreSQL's jsonb text for it (about 8-10% bigger again). The write path
// therefore also caps the canonical form at maxManifestCanonicalBytes, and the
// read path (ValidateStoredManifest) is looser than the write path.
func TestAdv_Bug_CanonicalFormCanExceedTheReadCap(t *testing.T) {
	// A schema-1 manifest that omits its defaults (surface, texture,
	// heading_scale) has a canonical form larger than the input; font names
	// are unbounded in schema 1, so the input can be sized exactly. Inputs
	// near and over the canonical cap must be rejected on write, and every
	// accepted one must validate again from its own canonical form.
	base := validManifestMap()
	for _, k := range []string{"surface", "texture", "heading_scale"} {
		delete(base, k)
	}
	for _, n := range []int{maxManifestCanonicalBytes - 200, maxManifestCanonicalBytes - 90, maxManifestCanonicalBytes, maxManifestBytes - 1, maxManifestBytes} {
		raw := advPadTo(t, base, n)
		m, err := ValidateManifest(raw)
		if err != nil {
			if n >= maxManifestCanonicalBytes {
				continue // rejected on write is correct
			}
			t.Fatalf("%d-byte input: %v", n, err)
		}
		canon := advMarshal(t, m)
		if len(canon) > maxManifestCanonicalBytes {
			t.Errorf("%d-byte input accepted but its canonical form is %d bytes (cap %d)", n, len(canon), maxManifestCanonicalBytes)
		}
		if _, err := ValidateManifest(canon); err != nil {
			t.Errorf("an accepted manifest's canonical form (%d bytes) no longer validates: %v", len(canon), err)
		}
	}
}

func TestAdv_Bug_JSONBTextCanExceedTheReadCap(t *testing.T) {
	// A canonical manifest of exactly the canonical cap, with PostgreSQL's
	// ", " and ": " spacing, must still load.
	m, err := ValidateManifest(advMarshal(t, validManifestMap()))
	if err != nil {
		t.Fatalf("setup: %v", err)
	}
	var canonObj obj
	if err := json.Unmarshal(advMarshal(t, m), &canonObj); err != nil {
		t.Fatal(err)
	}
	raw := advPadTo(t, canonObj, maxManifestCanonicalBytes)
	m2, err := ValidateManifest(raw)
	if err != nil {
		t.Fatalf("a canonical manifest of exactly %d bytes must be accepted: %v", maxManifestCanonicalBytes, err)
	}
	got := advMarshal(t, m2)
	if len(got) != len(raw) {
		t.Fatalf("setup: canonical form is %d bytes, input %d", len(got), len(raw))
	}
	stored := pgJSONB(got)
	if len(stored) <= len(got) {
		t.Fatalf("setup: jsonb text (%d) is not larger than the canonical form (%d)", len(stored), len(got))
	}
	if _, err := ValidateStoredManifest(stored); err != nil {
		t.Errorf("accepted on write (%d bytes) but unreadable as PostgreSQL jsonb text (%d bytes): %v", len(got), len(stored), err)
	}
	// And the stored read path has its own, looser bound.
	if _, err := ValidateStoredManifest(bytes.Repeat([]byte(" "), maxStoredBytes+1)); !hasIssueCode(err, "too_large") {
		t.Errorf("stored read cap: want too_large, got %v", err)
	}
}

// --- (9)/(10) idempotence, determinism, resolved-theme invariants ---

var advBanned = []string{"<", ">", ";", "url(", "javascript:", "expression(", "\"", "'", "`", "\\", "{", "}", "\x00"}
var advArtTokenRe = regexp.MustCompile(`art\d`)
var advHexPathRe = regexp.MustCompile(`^theme\.(palette\.[a-z_]+|accent_ink|layers\[\d+\]\.(tone|glow|vignette|color)|ornament\.hero_ink)$`)

// advWalkTheme walks a resolved theme's JSON generically.
func advWalkTheme(t *testing.T, themeJSON []byte) {
	t.Helper()
	var root any
	if err := json.Unmarshal(themeJSON, &root); err != nil {
		t.Fatalf("theme is not JSON: %v", err)
	}
	var walk func(path string, v any)
	walk = func(path string, v any) {
		switch x := v.(type) {
		case map[string]any:
			if kind, _ := x["kind"].(string); kind != "" {
				if _, has := x["src"]; has && kind != kindImage {
					t.Errorf("%s: src on a %s layer", path, kind)
				}
			}
			for k, c := range x {
				if advArtTokenRe.MatchString(k) {
					t.Errorf("%s: key %q looks like an art token", path, k)
				}
				if s, ok := c.(string); ok && s != "" && advHexPathRe.MatchString(path+"."+k) && !strictHexRe.MatchString(s) {
					t.Errorf("%s.%s = %q, want an upper-case #RRGGBB hex", path, k, s)
				}
				if list, ok := c.([]any); ok && (k == "colors" || k == "art" || k == "foil") {
					for i, e := range list {
						if s, ok := e.(string); !ok || !strictHexRe.MatchString(s) {
							t.Errorf("%s.%s[%d] = %v, want hex", path, k, i, e)
						}
					}
				}
				walk(path+"."+k, c)
			}
		case []any:
			for i, c := range x {
				walk(fmt.Sprintf("%s[%d]", path, i), c)
			}
		case string:
			low := strings.ToLower(x)
			for _, b := range advBanned {
				if strings.Contains(low, b) {
					t.Errorf("%s = %.60q contains banned %q", path, x, b)
				}
			}
			if advArtTokenRe.MatchString(x) {
				t.Errorf("%s = %.60q holds an unresolved art token", path, x)
			}
		}
	}
	walk("theme", root)
}

// advCheckResolved resolves m for every palette x font x background x
// override shape, asserting determinism and the theme invariants.
func advCheckResolved(t *testing.T, m Manifest) {
	t.Helper()
	overrides := [][]byte{nil, []byte(`{}`), []byte(`{"palette":"nope","font":"nope"}`), []byte(`not json`), []byte(`{"palette":"` + strings.Repeat("a", 100) + `"}`)}
	for _, p := range m.Palettes {
		for _, f := range m.Fonts {
			overrides = append(overrides, advMarshal(t, Overrides{Palette: p.ID, Font: f.ID}))
		}
	}
	for _, ov := range overrides {
		for _, bg := range []string{"", "/media/abc/background"} {
			th := ResolveTheme(m, ov, bg)
			j1 := advMarshal(t, th)
			j2 := advMarshal(t, ResolveTheme(m, ov, bg))
			if !bytes.Equal(j1, j2) {
				t.Fatalf("resolving twice differs:\n%.400s\n%.400s", j1, j2)
			}
			if m.Schema == 2 {
				assertThemeSafe(t, th)
			}
			advWalkTheme(t, j1)
		}
	}
}

func TestAdv_Fixtures_IdempotentDeterministicAndSafe(t *testing.T) {
	for name, fx := range v2Fixtures() {
		t.Run(name, func(t *testing.T) {
			raw := advMarshal(t, fx)
			canon := advCanon(t, raw)
			m, err := ValidateManifest(canon)
			if err != nil {
				t.Fatal(err)
			}
			advCheckResolved(t, m)
			// Whitespace/indentation of the input never changes the canonical form.
			var ind bytes.Buffer
			if err := json.Indent(&ind, raw, "", "  "); err != nil {
				t.Fatal(err)
			}
			if !bytes.Equal(canon, advCanon(t, ind.Bytes())) {
				t.Errorf("indented input canonicalises differently")
			}
		})
	}
	t.Run("seeded v1 manifests", func(t *testing.T) {
		seeds := advSeedManifests(t)
		if len(seeds) == 0 {
			t.Fatal("no seeded manifests found")
		}
		for _, s := range seeds {
			t.Run(fmt.Sprintf("%s_v%d", s.slug, s.version), func(t *testing.T) {
				canon := advCanon(t, s.raw)
				m, err := ValidateManifest(canon)
				if err != nil {
					t.Fatal(err)
				}
				advCheckResolved(t, m)
			})
		}
	})
}

func TestAdv_AllValidVariants(t *testing.T) {
	variants := advVariants(t)
	if len(variants) < 200 {
		t.Fatalf("only %d variants", len(variants))
	}
	for _, v := range variants {
		t.Run(v.name, func(t *testing.T) {
			canon := advCanon(t, v.raw)
			m, err := ValidateManifest(canon)
			if err != nil {
				t.Fatal(err)
			}
			advCheckResolved(t, m)
		})
	}
}

// Resolving is a pure function of its inputs: concurrent resolves of one
// shared Manifest agree (run with -race for the data-race half).
func TestAdv_ResolveIsPureUnderConcurrency(t *testing.T) {
	for name, fx := range v2Fixtures() {
		t.Run(name, func(t *testing.T) {
			m, err := ValidateManifest(advMarshal(t, fx))
			if err != nil {
				t.Fatal(err)
			}
			want := advMarshal(t, ResolveTheme(m, nil, "/media/abc/background"))
			var wg sync.WaitGroup
			errs := make(chan string, 16)
			for i := 0; i < 16; i++ {
				wg.Add(1)
				go func() {
					defer wg.Done()
					for j := 0; j < 20; j++ {
						b, err := json.Marshal(ResolveTheme(m, nil, "/media/abc/background"))
						if err != nil || !bytes.Equal(b, want) {
							errs <- "concurrent resolve differs"
							return
						}
					}
				}()
			}
			wg.Wait()
			close(errs)
			for e := range errs {
				t.Error(e)
			}
		})
	}
}

// Resolving never mutates the manifest it was given.
func TestAdv_ResolveDoesNotMutateManifest(t *testing.T) {
	for name, fx := range v2Fixtures() {
		t.Run(name, func(t *testing.T) {
			m, err := ValidateManifest(advMarshal(t, fx))
			if err != nil {
				t.Fatal(err)
			}
			before := advMarshal(t, m)
			for _, p := range m.Palettes {
				th := ResolveTheme(m, advMarshal(t, Overrides{Palette: p.ID}), "/media/abc/background")
				// Scribble over everything the theme exposes by pointer/slice.
				for i := range th.Layers {
					th.Layers[i].Tone = "#000000"
					th.Layers[i].Colors = append(th.Layers[i].Colors, "x")
				}
				if len(th.Art) > 0 {
					th.Art[0] = "x"
				}
				if len(th.Foil) > 0 {
					th.Foil[0] = "x"
				}
			}
			if after := advMarshal(t, m); !bytes.Equal(before, after) {
				t.Errorf("mutating a resolved theme changed the manifest (shared pointer or slice):\n%.300s\n%.300s", before, after)
			}
		})
	}
}

// The allowlists equal the spec lists (catches silent drift).
func TestAdv_AllowlistsMatchSpec(t *testing.T) {
	sets := []struct {
		name string
		got  map[string]bool
		want []string
	}{
		{"layer kinds", layerKinds, []string{"paper", "block", "pattern", "texture", "art", "frame", "image"}},
		{"regions", validRegions, []string{"page", "hero"}},
		{"edges", validEdges, []string{"straight", "scallop", "wave"}},
		{"patterns", validPatterns, []string{"grid", "dots", "halftone", "sunburst", "pinstripe", "gingham", "stripes"}},
		{"origins", validOrigins, []string{"center", "top", "bottom", "top_left", "top_right", "bottom_left", "bottom_right"}},
		{"masks", validMasks, []string{"none", "radial", "corner", "fade_bottom"}},
		{"textures", validTextureV2, []string{"grain", "fibers", "linen", "wood", "watercolour", "foil", "marble", "velvet"}},
		{"blends", validBlends, []string{"normal", "multiply", "screen", "soft-light", "overlay"}},
		{"densities", validDensities, []string{"sparse", "normal", "dense"}},
		{"frames", validFrames, []string{"single_hairline", "double_hairline", "double_hairline_deco_corners", "scallop"}},
		{"v2 hero styles", validHeroStyleV2, []string{"full_bleed", "framed", "text_only", "spotlight", "color_block"}},
		{"v1 hero styles", validHeroStyles, []string{"full_bleed", "framed", "text_only", "spotlight"}},
		{"layouts", validLayouts, []string{"centered", "split", "stacked"}},
		{"hero ornaments", validHeroOrnaments, []string{"none", "wreath_monogram", "sticker_numeral", "foil_numeral", "ring", "cloud", "doorway", "monogram_rule"}},
		{"dividers", validDividers, []string{"none", "line", "floral", "dots", "heart", "olive_sprig", "squiggle", "deco_diamond", "wave", "sparkle_rule"}},
		{"badges", validBadges, []string{"none", "starburst_sticker", "wax_seal", "foil_seal"}},
		{"ampersands", validAmpersands, []string{"none", "script"}},
		{"card styles", validCardStyles, []string{"none", "soft", "glass", "reply_card", "sticker", "chamfered"}},
		{"card borders", validCardBorders, []string{"none", "hairline", "ink", "foil", "foil_inset"}},
		{"card fields", validCardFields, []string{"boxed", "underline"}},
		{"card buttons", validCardButtons, []string{"accent", "foil"}},
		{"motions", validMotions, []string{"none", "draw_on", "pop_and_settle", "foil_sheen"}},
		{"v1 dividers", v1Dividers, []string{"none", "line", "floral", "dots", "heart"}},
	}
	for _, s := range sets {
		t.Run(s.name, func(t *testing.T) {
			want := map[string]bool{}
			for _, w := range s.want {
				want[w] = true
			}
			for k := range s.got {
				if !want[k] {
					t.Errorf("allowlist has %q, not in the spec", k)
				}
			}
			for k := range want {
				if !s.got[k] {
					t.Errorf("spec value %q missing from the allowlist", k)
				}
			}
			for k := range s.got {
				if k == "" || k != strings.TrimSpace(k) || k != strings.ToLower(k) || strings.ContainsAny(k, `;:()<>"'\/ `) {
					t.Errorf("allowlist key %q is not a plain lower-case token", k)
				}
			}
		})
	}
	t.Run("colour tokens", func(t *testing.T) {
		want := []string{"background", "surface", "text", "muted", "accent", "accent_text", "accent_ink",
			"art1", "art2", "art3", "art4", "art5", "art6", "art7", "art8"}
		if len(colourTokens) != len(want) {
			t.Errorf("%d tokens, want %d", len(colourTokens), len(want))
		}
		for _, w := range want {
			if _, ok := colourTokens[w]; !ok {
				t.Errorf("token %q missing", w)
			}
		}
		for i := 1; i <= 8; i++ {
			if colourTokens[fmt.Sprintf("art%d", i)] != i {
				t.Errorf("art%d needs %d", i, colourTokens[fmt.Sprintf("art%d", i)])
			}
		}
	})
	t.Run("fonts", func(t *testing.T) {
		for _, f := range []string{"bodoni_moda", "pinyon_script", "bagel_fat_one", "limelight", "josefin_sans"} {
			if !allowedFonts[f] {
				t.Errorf("%s not allowed", f)
			}
		}
		for _, f := range []string{"pinyon_script", "bagel_fat_one", "limelight", "great_vibes", "birthstone"} {
			if !headingOnlyFonts[f] {
				t.Errorf("%s should be heading-only", f)
			}
		}
		if allowedFonts["gloock"] {
			t.Error("gloock must not be a manifest font")
		}
		for f := range headingOnlyFonts {
			if !allowedFonts[f] {
				t.Errorf("heading-only %s is not an allowed font", f)
			}
		}
	})
}

// --- seeds / variants shared with the fuzz target and benchmarks ---

// advSeedManifests loads every seeded schema-1 manifest from the migrations
// (the helper the golden test uses needs a *testing.T; this one takes a TB so
// the fuzz target can use it too).
func advSeedManifests(tb testing.TB) []seededManifest {
	tb.Helper()
	var out []seededManifest
	slugs := map[string]string{}
	for _, name := range []string{"00003_seed_catalog.sql", "00004_rich_blocks.sql"} {
		b, err := readMigration(name)
		if err != nil {
			tb.Fatalf("read %s: %v", name, err)
		}
		sql := strings.ReplaceAll(string(b), "\r\n", "\n")
		if i := strings.Index(sql, "-- +goose Down"); i >= 0 {
			sql = sql[:i]
		}
		for _, m := range seedTemplateRe.FindAllStringSubmatch(sql, -1) {
			slugs[m[1]] = m[2]
		}
		for _, m := range seedVersionRe.FindAllStringSubmatch(sql, -1) {
			slug, ok := slugs[m[1]]
			if !ok {
				tb.Fatalf("%s: template id %s has no seeded slug", name, m[1])
			}
			v, _ := strconv.Atoi(m[2])
			out = append(out, seededManifest{slug: slug, version: v, raw: []byte(strings.ReplaceAll(m[3], "''", "'"))})
		}
	}
	return out
}

type advVariant struct {
	name string
	raw  []byte
}

// advUniversal is a valid schema-2 manifest with 8 art colours and a foil
// ramp per palette, no foil users and a single paper layer: every variant
// changes exactly one thing, so each must be accepted.
func advUniversal() obj {
	m := giltNoir()
	art := []string{"#C9A45C", "#8E7440", "#E3C779", "#7FB59F", "#F4EEDD", "#BDB39A", "#B66A73", "#D4AF6A"}
	for _, p := range m["palettes"].([]obj) {
		p["art"] = art
	}
	m["ornament"] = obj{"hero": "none", "divider": "none", "badge": "none", "ampersand": "none", "hero_ink": ""}
	m["card"] = obj{"style": "soft", "border": "hairline", "radius": 24, "fields": "boxed", "buttons": "accent"}
	m["motion"] = "none"
	m["layers"] = []obj{{"kind": "paper"}}
	return m
}

func advSortedKeys(set map[string]bool) []string {
	out := make([]string, 0, len(set))
	for k := range set {
		out = append(out, k)
	}
	sort.Strings(out)
	return out
}

// advVariants returns every single-change variant of the universal manifest:
// each enum value in each slot, each colour token in each token slot, every
// registry combination, boundary numerics, fonts, palette/foil/art counts.
func advVariants(tb testing.TB) []advVariant {
	tb.Helper()
	var out []advVariant
	add := func(name string, fn func(m obj)) {
		out = append(out, advVariant{name, advMutate(tb, advUniversal(), fn)})
	}
	stack := func(ls ...obj) func(m obj) {
		return func(m obj) {
			all := make([]obj, 0, len(ls)+1)
			all = append(all, obj{"kind": "paper"})
			all = append(all, ls...)
			setLayers(m, all...)
		}
	}
	tokens := make([]string, 0, len(colourTokens))
	for k := range colourTokens {
		tokens = append(tokens, k)
	}
	sort.Strings(tokens)

	for _, tk := range tokens {
		// Only background and surface are valid paper tones; a wash or
		// pattern in an arbitrary token is only readable at a faint opacity.
		if tk == "background" || tk == "surface" {
			add("tone/"+tk, func(m obj) { setLayers(m, obj{"kind": "paper", "tone": tk}) })
		}
		add("glow/"+tk, func(m obj) { setLayers(m, obj{"kind": "paper", "glow": tk, "glow_opacity": 0.02}) })
		add("vignette/"+tk, func(m obj) { setLayers(m, obj{"kind": "paper", "vignette": tk, "vignette_opacity": 0.02}) })
		add("pattern.color/"+tk, stack(obj{"kind": "pattern", "pattern": "dots", "color": tk, "opacity": 0.01}))
		add("frame.color/"+tk, stack(obj{"kind": "frame", "frame": "single_hairline", "color": tk}))
		add("art.colors/"+tk, stack(obj{"kind": "art", "art": "printers_corners", "placement": "corners", "colors": []string{tk}}))
		add("watercolour.colors/"+tk, stack(obj{"kind": "texture", "texture": "watercolour", "opacity": 0.2, "colors": []string{tk, tk, tk}}))
	}
	for _, tk := range []string{"text", "muted", "accent", "accent_ink"} {
		add("hero_ink/"+tk, func(m obj) { m["ornament"].(obj)["hero_ink"] = tk })
	}
	for _, op := range []float64{0, 1} {
		add(fmt.Sprintf("glow_opacity/%g", op), func(m obj) { setLayers(m, obj{"kind": "paper", "glow": "surface", "glow_opacity": op}) })
		add(fmt.Sprintf("vignette_opacity/%g", op), func(m obj) { setLayers(m, obj{"kind": "paper", "vignette": "surface", "vignette_opacity": op}) })
	}
	for _, p := range advSortedKeys(validPatterns) {
		add("pattern/"+p, stack(obj{"kind": "pattern", "pattern": p, "color": "surface", "opacity": 0.2}))
	}
	for _, v := range advSortedKeys(validOrigins) {
		add("origin/"+v, stack(obj{"kind": "pattern", "pattern": "dots", "color": "surface", "opacity": 0.2, "origin": v}))
	}
	for _, v := range advSortedKeys(validMasks) {
		add("mask/"+v, stack(obj{"kind": "pattern", "pattern": "dots", "color": "surface", "opacity": 0.2, "mask": v}))
	}
	for _, op := range []float64{0.01, 0.35} {
		add(fmt.Sprintf("pattern.opacity/%g", op), stack(obj{"kind": "pattern", "pattern": "dots", "color": "surface", "opacity": op}))
	}
	for _, tx := range advSortedKeys(validTextureV2) {
		l := obj{"kind": "texture", "texture": tx, "opacity": 0.2}
		if tx == "watercolour" {
			l["colors"] = []string{"art1", "art2", "art3"}
		}
		add("texture/"+tx, stack(l))
	}
	for _, op := range []float64{0.02, 0.4} {
		add(fmt.Sprintf("texture.opacity/%g", op), stack(obj{"kind": "texture", "texture": "grain", "opacity": op}))
	}
	for _, b := range advSortedKeys(validBlends) {
		add("texture.blend/"+b, stack(obj{"kind": "texture", "texture": "grain", "opacity": 0.2, "blend": b}))
		add("pattern.blend/"+b, stack(obj{"kind": "pattern", "pattern": "dots", "color": "surface", "opacity": 0.2, "blend": b}))
		add("art.blend/"+b, stack(obj{"kind": "art", "art": "printers_corners", "placement": "corners", "colors": []string{"art1"}, "blend": b}))
		add("frame.blend/"+b, stack(obj{"kind": "frame", "frame": "single_hairline", "color": "text", "blend": b}))
		add("image.blend/"+b, stack(obj{"kind": "image", "opacity": 0.3, "blend": b}))
	}
	for _, r := range advSortedKeys(validRegions) {
		for _, l := range []obj{
			{"kind": "pattern", "pattern": "dots", "color": "surface", "opacity": 0.2},
			{"kind": "texture", "texture": "grain", "opacity": 0.2},
			{"kind": "art", "art": "printers_corners", "placement": "corners", "colors": []string{"art1"}},
			{"kind": "image", "opacity": 0.3},
		} {
			l := l
			l["region"] = r
			add("region/"+r+"/"+l["kind"].(string), stack(l))
		}
	}
	for _, f := range advSortedKeys(validFrames) {
		add("frame/"+f+"/color", stack(obj{"kind": "frame", "frame": f, "color": "accent"}))
		add("frame/"+f+"/foil", stack(obj{"kind": "frame", "frame": f, "paint": "foil"}))
	}
	for _, in := range []int{0, 1, 12, 31, 32} {
		add(fmt.Sprintf("frame.inset/%d", in), stack(obj{"kind": "frame", "frame": "single_hairline", "color": "text", "inset": in}))
	}
	for _, e := range advSortedKeys(validEdges) {
		add("block.edge/"+e, func(m obj) {
			m["hero_style"] = "color_block"
			setLayers(m, obj{"kind": "paper"}, obj{"kind": "block", "edge": e})
		})
	}
	for _, op := range []float64{0.05, 0.5} {
		add(fmt.Sprintf("image.opacity/%g", op), stack(obj{"kind": "image", "opacity": op}))
	}
	// art registry cross product, valid cells only.
	for _, key := range func() []string {
		ks := make([]string, 0, len(artRegistry))
		for k := range artRegistry {
			ks = append(ks, k)
		}
		sort.Strings(ks)
		return ks
	}() {
		spec := artRegistry[key]
		for _, pl := range advSortedKeys(spec.placements) {
			for n := spec.minColors; n <= spec.maxColors; n++ {
				l := obj{"kind": "art", "art": key, "placement": pl}
				if n > 0 {
					cols := make([]string, n)
					for i := range cols {
						cols[i] = fmt.Sprintf("art%d", i+1)
					}
					l["colors"] = cols
				}
				add(fmt.Sprintf("art/%s/%s/%dcolors", key, pl, n), stack(l))
			}
			if spec.foil {
				add(fmt.Sprintf("art/%s/%s/foil", key, pl), stack(obj{"kind": "art", "art": key, "placement": pl, "paint": "foil"}))
			}
			if spec.density {
				for _, d := range advSortedKeys(validDensities) {
					cols := []string{}
					for i := 0; i < spec.minColors; i++ {
						cols = append(cols, fmt.Sprintf("art%d", i+1))
					}
					add(fmt.Sprintf("art/%s/%s/density_%s", key, pl, d), stack(obj{"kind": "art", "art": key, "placement": pl, "density": d, "colors": cols}))
				}
			}
		}
	}
	for _, op := range []float64{0.1, 1} {
		add(fmt.Sprintf("art.opacity/%g", op), stack(obj{"kind": "art", "art": "printers_corners", "placement": "corners", "colors": []string{"art1"}, "opacity": op}))
	}
	// whole-manifest enums.
	for _, v := range advSortedKeys(validHeroStyleV2) {
		add("hero_style/"+v, func(m obj) {
			m["hero_style"] = v
			if v == "color_block" {
				setLayers(m, obj{"kind": "paper"}, obj{"kind": "block"})
			}
		})
	}
	for _, v := range advSortedKeys(validLayouts) {
		add("layout/"+v, func(m obj) { m["layout"] = v })
	}
	for _, v := range []string{"regular", "display", ""} {
		add("heading_scale/"+v, func(m obj) { m["heading_scale"] = v })
	}
	for _, v := range advSortedKeys(validMotions) {
		add("motion/"+v, func(m obj) { m["motion"] = v })
	}
	for _, v := range advSortedKeys(validHeroOrnaments) {
		add("ornament.hero/"+v, func(m obj) { m["ornament"].(obj)["hero"] = v })
	}
	for _, v := range advSortedKeys(validDividers) {
		add("ornament.divider/"+v, func(m obj) { m["ornament"].(obj)["divider"] = v })
	}
	for _, v := range advSortedKeys(validBadges) {
		add("ornament.badge/"+v, func(m obj) { m["ornament"].(obj)["badge"] = v })
	}
	for _, v := range advSortedKeys(validAmpersands) {
		add("ornament.ampersand/"+v, func(m obj) { m["ornament"].(obj)["ampersand"] = v })
	}
	for _, v := range advSortedKeys(validCardStyles) {
		add("card.style/"+v, func(m obj) { delete(m["card"].(obj), "radius"); m["card"].(obj)["style"] = v })
	}
	for _, v := range advSortedKeys(validCardBorders) {
		add("card.border/"+v, func(m obj) { m["card"].(obj)["border"] = v })
	}
	for _, v := range advSortedKeys(validCardFields) {
		add("card.fields/"+v, func(m obj) { m["card"].(obj)["fields"] = v })
	}
	for _, v := range advSortedKeys(validCardButtons) {
		add("card.buttons/"+v, func(m obj) { m["card"].(obj)["buttons"] = v })
	}
	for _, r := range []int{0, 1, 32} {
		add(fmt.Sprintf("card.radius/%d", r), func(m obj) { m["card"].(obj)["radius"] = r })
	}
	// fonts.
	fonts := advSortedKeys(allowedFonts)
	for _, f := range fonts {
		add("font.heading/"+f, func(m obj) { m["fonts"].([]any)[0].(obj)["heading"] = f })
		add("font.accent/"+f, func(m obj) { m["fonts"].([]any)[0].(obj)["accent"] = f })
		if !headingOnlyFonts[f] {
			add("font.body/"+f, func(m obj) { m["fonts"].([]any)[0].(obj)["body"] = f })
		}
	}
	// palettes, fonts, art and foil counts.
	for _, n := range []int{1, 2, 8} {
		add(fmt.Sprintf("palettes/%d", n), func(m obj) {
			ps := make([]any, n)
			for i := range ps {
				var cp obj
				_ = json.Unmarshal(advMarshal(tb, palAt(m, 0)), &cp)
				cp["id"] = fmt.Sprintf("p%d", i)
				ps[i] = cp
			}
			m["palettes"] = ps
			m["defaults"].(obj)["palette"] = "p0"
		})
	}
	for _, n := range []int{1, 6} {
		add(fmt.Sprintf("fonts/%d", n), func(m obj) {
			fs := make([]any, n)
			for i := range fs {
				fs[i] = obj{"id": fmt.Sprintf("f%d", i), "name": "F", "heading": "lora", "body": "lora"}
			}
			m["fonts"] = fs
			m["defaults"].(obj)["font"] = "f0"
		})
	}
	for n := 0; n <= 8; n++ {
		add(fmt.Sprintf("palette.art/%d", n), func(m obj) {
			cols := []string{"#C9A45C", "#8E7440", "#E3C779", "#7FB59F", "#F4EEDD", "#BDB39A", "#B66A73", "#D4AF6A"}
			for pi := range m["palettes"].([]any) {
				palAt(m, pi)["art"] = cols[:n]
			}
		})
	}
	for n := 3; n <= 5; n++ {
		add(fmt.Sprintf("palette.foil/%d", n), func(m obj) {
			cols := []string{"#E3C779", "#C9A45C", "#F3E2AA", "#B48A43", "#D9B769"}
			for pi := range m["palettes"].([]any) {
				palAt(m, pi)["foil"] = cols[:n]
			}
		})
	}
	add("palette.foil/absent", func(m obj) {
		for pi := range m["palettes"].([]any) {
			delete(palAt(m, pi), "foil")
		}
	})
	add("palette.accent_ink/absent", func(m obj) {
		for pi := range m["palettes"].([]any) {
			delete(palAt(m, pi), "accent_ink")
		}
	})
	add("palette.accent_ink/present", func(m obj) { palAt(m, 0)["accent_ink"] = "#E3C779" })
	add("palette.name/40 multibyte", func(m obj) { palAt(m, 0)["name"] = strings.Repeat("é", 40) })
	add("lowercase hex", func(m obj) {
		c := colorsAt(m, 0)
		for k, v := range c {
			c[k] = strings.ToLower(v.(string))
		}
	})
	add("empty layers", func(m obj) { m["layers"] = []any{} })
	add("no layers", func(m obj) { delete(m, "layers") })
	add("explicit nulls", func(m obj) { m["ornament"], m["card"], m["background"] = nil, nil, nil })
	add("image layer", stack(obj{"kind": "image", "opacity": 0.3}))
	add("seven layers", stack(
		obj{"kind": "pattern", "pattern": "dots", "color": "surface", "opacity": 0.2},
		obj{"kind": "pattern", "pattern": "grid", "color": "surface", "opacity": 0.2},
		obj{"kind": "texture", "texture": "grain", "opacity": 0.2},
		obj{"kind": "texture", "texture": "linen", "opacity": 0.2},
		obj{"kind": "art", "art": "printers_corners", "placement": "corners", "colors": []string{"art1"}},
		obj{"kind": "frame", "frame": "single_hairline", "color": "text"},
	))
	// foil users (the universal palettes carry foil).
	add("foil all users", func(m obj) {
		m["ornament"] = obj{"hero": "foil_numeral", "badge": "foil_seal"}
		m["card"] = obj{"style": "chamfered", "border": "foil_inset", "buttons": "foil"}
		m["motion"] = "foil_sheen"
		setLayers(m, obj{"kind": "paper"}, obj{"kind": "texture", "texture": "foil", "opacity": 0.2},
			obj{"kind": "frame", "frame": "double_hairline", "paint": "foil"})
	})
	return out
}

// BUG (reported, latent): resolveV2 copies each Layer by value, so the
// resolved theme's Opacity/GlowOpacity/VignetteOpacity/Inset pointers (and
// Card.Radius) alias the manifest's. Nothing mutates a Theme today; a future
// caller that does (or a cached Manifest shared across requests) would corrupt
// the manifest.
func TestAdv_Bug_ThemeSharesPointersWithManifest(t *testing.T) {
	for name, fx := range v2Fixtures() {
		t.Run(name, func(t *testing.T) {
			m, err := ValidateManifest(advMarshal(t, fx))
			if err != nil {
				t.Fatal(err)
			}
			before := advMarshal(t, m)
			th := ResolveTheme(m, nil, "/media/abc/background")
			for i := range th.Layers {
				if th.Layers[i].Opacity != nil {
					*th.Layers[i].Opacity = 7
				}
				if th.Layers[i].GlowOpacity != nil {
					*th.Layers[i].GlowOpacity = 7
				}
				if th.Layers[i].Inset != nil {
					*th.Layers[i].Inset = 99
				}
			}
			if th.Card != nil && th.Card.Radius != nil {
				*th.Card.Radius = 99
			}
			if after := advMarshal(t, m); !bytes.Equal(before, after) {
				t.Errorf("writing through the resolved theme changed the manifest")
			}
		})
	}
}
