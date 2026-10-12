package content

import (
	"encoding/json"
	"regexp"
	"strings"
	"testing"
)

var strictHexRe = regexp.MustCompile(`^#[0-9A-F]{6}$`)

// assertThemeSafe checks the invariant the frontend relies on: every colour
// in a resolved v2 theme is a #RRGGBB hex, and every other string is an
// allowlisted enum or a server-built value.
func assertThemeSafe(t *testing.T, th Theme) {
	t.Helper()
	hex := func(where, v string) {
		t.Helper()
		if !strictHexRe.MatchString(v) {
			t.Errorf("%s = %q, want an upper-case #RRGGBB hex", where, v)
		}
	}
	optHex := func(where, v string) {
		t.Helper()
		if v != "" {
			hex(where, v)
		}
	}
	enum := func(where, v string, set map[string]bool) {
		t.Helper()
		if !set[v] {
			t.Errorf("%s = %q is not an allowlisted value", where, v)
		}
	}
	if th.Engine != 2 {
		t.Errorf("engine = %d, want 2", th.Engine)
	}
	for name, v := range map[string]string{
		"palette.background": th.Palette.Background, "palette.surface": th.Palette.Surface,
		"palette.text": th.Palette.Text, "palette.muted": th.Palette.Muted,
		"palette.accent": th.Palette.Accent, "palette.accent_text": th.Palette.AccentText,
		"accent_ink": th.AccentInk,
	} {
		hex(name, v)
	}
	for _, c := range th.Art {
		hex("art", c)
	}
	for _, c := range th.Foil {
		hex("foil", c)
	}
	for i, l := range th.Layers {
		w := "layers[" + string(rune('0'+i)) + "]"
		enum(w+".kind", l.Kind, layerKinds)
		enum(w+".region", l.Region, validRegions)
		optHex(w+".tone", l.Tone)
		optHex(w+".glow", l.Glow)
		optHex(w+".vignette", l.Vignette)
		optHex(w+".color", l.Color)
		for _, c := range l.Colors {
			hex(w+".colors", c)
		}
		if l.Blend != "" {
			enum(w+".blend", l.Blend, validBlends)
		}
		if l.Paint != "" {
			enum(w+".paint", l.Paint, map[string]bool{paintFoil: true})
		}
		if l.Kind != kindImage && l.Src != "" {
			t.Errorf("%s.src = %q on a %s layer, want empty", w, l.Src, l.Kind)
		}
		if l.Density != "" {
			enum(w+".density", l.Density, validDensities)
		}
		for name, p := range map[string]*float64{"opacity": l.Opacity, "glow_opacity": l.GlowOpacity, "vignette_opacity": l.VignetteOpacity} {
			if p != nil && !(*p >= 0 && *p <= 1) {
				t.Errorf("%s.%s = %v, want 0-1", w, name, *p)
			}
		}
		if l.Inset != nil && (*l.Inset < 0 || *l.Inset > 32) {
			t.Errorf("%s.inset = %d, want 0-32", w, *l.Inset)
		}
		switch l.Kind {
		case kindBlock:
			enum(w+".edge", l.Edge, validEdges)
			hex(w+".color", l.Color)
		case kindPattern:
			enum(w+".pattern", l.Pattern, validPatterns)
			enum(w+".origin", l.Origin, validOrigins)
			enum(w+".mask", l.Mask, validMasks)
		case kindTexture:
			enum(w+".texture", l.Texture, validTextureV2)
		case kindArt:
			if _, ok := artRegistry[l.Art]; !ok {
				t.Errorf("%s.art = %q", w, l.Art)
			}
			enum(w+".placement", l.Placement, allArtPlacements)
		case kindFrame:
			enum(w+".frame", l.Frame, validFrames)
		case kindImage:
			if l.Opacity == nil || *l.Opacity > maxImageOpacity {
				t.Errorf("%s.opacity = %v, want at most %g", w, l.Opacity, maxImageOpacity)
			}
			if !regexp.MustCompile(`^/media/[A-Za-z0-9/_-]+$`).MatchString(l.Src) {
				t.Errorf("%s.src = %q", w, l.Src)
			}
		}
	}
	o := th.Ornament
	if o == nil || th.Card == nil {
		t.Fatal("ornament and card must be set")
	}
	enum("ornament.hero", o.Hero, validHeroOrnaments)
	enum("ornament.divider", o.Divider, validDividers)
	enum("ornament.badge", o.Badge, validBadges)
	enum("ornament.ampersand", o.Ampersand, validAmpersands)
	hex("ornament.hero_ink", o.HeroInk)
	enum("card.style", th.Card.Style, validCardStyles)
	enum("card.border", th.Card.Border, validCardBorders)
	enum("card.fields", th.Card.Fields, validCardFields)
	enum("card.buttons", th.Card.Buttons, validCardButtons)
	if th.Card.Radius == nil || *th.Card.Radius < 0 || *th.Card.Radius > 32 {
		t.Errorf("card.radius = %v", th.Card.Radius)
	}
	enum("motion", th.Motion, validMotions)
	enum("layout", th.Layout, validLayouts)
	enum("hero_style", th.HeroStyle, validHeroStyleV2)
	enum("decoration", th.Decoration, v1Dividers)
	enum("surface", th.Surface, map[string]bool{"plain": true, "card": true, "glass": true})
	enum("texture", th.Texture, map[string]bool{"none": true})
}

func TestResolveThemeV2_FixturesEveryPaletteAndFont(t *testing.T) {
	for name, fx := range v2Fixtures() {
		t.Run(name, func(t *testing.T) {
			m, err := ValidateManifest(mustJSON(t, fx))
			if err != nil {
				t.Fatalf("fixture invalid: %v", err)
			}
			for _, p := range m.Palettes {
				for _, f := range m.Fonts {
					ov, err := ValidateOverrides(mustJSON(t, Overrides{Palette: p.ID, Font: f.ID}), m)
					if err != nil {
						t.Fatal(err)
					}
					th := ResolveTheme(m, ov, "/media/abc/background")
					assertThemeSafe(t, th)
					if th.Palette != p.Colors {
						t.Errorf("palette %s not selected: %+v", p.ID, th.Palette)
					}
					if th.Fonts.Heading != m.Fonts[0].Heading {
						t.Errorf("fonts = %+v", th.Fonts)
					}
				}
			}
			// The default (no overrides) resolves too.
			assertThemeSafe(t, ResolveTheme(m, nil, ""))
		})
	}
}

func TestResolveThemeV2_PaletteSwitchRecoloursArt(t *testing.T) {
	m, err := ValidateManifest(mustJSON(t, oliveGrove()))
	if err != nil {
		t.Fatal(err)
	}
	ivory := ResolveTheme(m, []byte(`{"palette":"ivory","font":""}`), "")
	sage := ResolveTheme(m, []byte(`{"palette":"sage","font":""}`), "")
	// layers[2] is the olive_branches art: art1..art4 of the palette.
	for i, p := range []struct {
		th  Theme
		art []string
	}{{ivory, m.Palettes[0].Art}, {sage, m.Palettes[1].Art}} {
		got := p.th.Layers[2].Colors
		want := p.art[:4]
		if strings.Join(got, ",") != strings.Join(want, ",") {
			t.Errorf("palette %d: art colours = %v, want %v", i, got, want)
		}
	}
	if ivory.Layers[2].Colors[0] == sage.Layers[2].Colors[0] {
		t.Error("switching palette must recolour the art")
	}
	// paper: tone background, glow art5, vignette art6, opacities default 1.
	pl := ivory.Layers[0]
	p0 := m.Palettes[0]
	if pl.Tone != p0.Colors.Background || pl.Glow != p0.Art[4] || pl.Vignette != p0.Art[5] ||
		pl.GlowOpacity == nil || *pl.GlowOpacity != 1 {
		t.Errorf("paper = %+v", pl)
	}
	if ivory.Art[0] != p0.Art[0] || ivory.Foil[0] != p0.Foil[0] {
		t.Errorf("art/foil not carried: %v %v", ivory.Art, ivory.Foil)
	}
	// The resolved theme must not alias the manifest's slices.
	ivory.Art[0] = "#000000"
	ivory.Layers[2].Colors[0] = "#000000"
	if m.Palettes[0].Art[0] == "#000000" || m.Layers[2].Colors[0] == "#000000" {
		t.Error("resolved theme aliases the manifest")
	}
}

func TestResolveThemeV2_AccentInk(t *testing.T) {
	m, err := ValidateManifest(mustJSON(t, oliveGrove()))
	if err != nil {
		t.Fatal(err)
	}
	if th := ResolveTheme(m, []byte(`{"palette":"ivory","font":""}`), ""); th.AccentInk != "#77592A" {
		t.Errorf("explicit accent_ink = %q", th.AccentInk)
	}
	// sage has no accent_ink: computed (accent when >= 4.5:1, else text).
	th := ResolveTheme(m, []byte(`{"palette":"sage","font":""}`), "")
	if th.AccentInk != m.Palettes[1].Colors.Accent {
		t.Errorf("computed accent_ink = %q, want the accent %q", th.AccentInk, m.Palettes[1].Colors.Accent)
	}
	// An accent that reads at 3:1 but not 4.5:1 falls back to text in v2.
	raw := mutated(t, oliveGrove(), func(m obj) {
		c := colorsAt(m, 1)
		c["accent"] = "#868686"
		c["accent_text"] = "#000000"
	})
	weak, err := ValidateManifest(raw)
	if err != nil {
		t.Fatal(err)
	}
	th = ResolveTheme(weak, []byte(`{"palette":"sage","font":""}`), "")
	if th.AccentInk != th.Palette.Text {
		t.Errorf("weak accent: accent_ink = %q, want text %q", th.AccentInk, th.Palette.Text)
	}
}

func TestResolveThemeV2_BlockImageAndFallbacks(t *testing.T) {
	m, err := ValidateManifest(mustJSON(t, sprinkles()))
	if err != nil {
		t.Fatal(err)
	}
	th := ResolveTheme(m, nil, "")
	assertThemeSafe(t, th)
	if th.Layers[1].Kind != kindBlock || th.Layers[1].Color != m.Palettes[0].Colors.Accent {
		t.Errorf("block = %+v, want accent colour", th.Layers[1])
	}
	// color_block hero: hero_ink defaults to accent_text.
	if th.Ornament.HeroInk != m.Palettes[0].Colors.AccentText {
		t.Errorf("hero_ink = %q", th.Ornament.HeroInk)
	}
	if th.HeroStyle != "color_block" || th.Decoration != "none" || th.Surface != "card" || th.Texture != "none" {
		t.Errorf("fallback fields = %+v", th)
	}
	if th.Background != nil {
		t.Errorf("v2 theme background = %+v, want nil", th.Background)
	}

	// An image layer gets the server-built src, or is dropped without an asset.
	withImage, err := ValidateManifest(mutated(t, oliveGrove(), func(m obj) {
		m["layers"] = append(layersOf(m), obj{"kind": "image", "opacity": 0.3})
	}))
	if err != nil {
		t.Fatal(err)
	}
	n := len(withImage.Layers)
	if got := ResolveTheme(withImage, nil, "/media/p/background").Layers; len(got) != n || got[n-1].Src != "/media/p/background" {
		t.Errorf("image layer with asset = %+v", got)
	}
	if got := ResolveTheme(withImage, nil, "").Layers; len(got) != n-1 {
		t.Errorf("image layer without asset should be dropped, got %d layers", len(got))
	}

	// v1 dividers become the decoration fallback; glass and none map through.
	olive, _ := ValidateManifest(mustJSON(t, oliveGrove()))
	if d := ResolveTheme(olive, nil, "").Decoration; d != "none" {
		t.Errorf("olive_sprig fallback decoration = %q, want none", d)
	}
	for _, c := range []struct{ divider, style, wantDeco, wantSurface string }{
		{"heart", "glass", "heart", "glass"},
		{"line", "none", "line", "plain"},
		{"wave", "sticker", "none", "card"},
	} {
		mm, err := ValidateManifest(mutated(t, oliveGrove(), func(m obj) {
			m["ornament"].(obj)["divider"] = c.divider
			m["card"] = obj{"style": c.style}
		}))
		if err != nil {
			t.Fatalf("%+v: %v", c, err)
		}
		th := ResolveTheme(mm, nil, "")
		if th.Decoration != c.wantDeco || th.Surface != c.wantSurface {
			t.Errorf("%+v: decoration=%q surface=%q", c, th.Decoration, th.Surface)
		}
	}
}

func TestResolveThemeV2_ExplicitHeroInkToken(t *testing.T) {
	m, err := ValidateManifest(mutated(t, oliveGrove(), func(m obj) { m["ornament"].(obj)["hero_ink"] = "art4" }))
	if err != nil {
		t.Fatal(err)
	}
	if th := ResolveTheme(m, nil, ""); th.Ornament.HeroInk != m.Palettes[0].Art[3] {
		t.Errorf("hero_ink = %q, want art4 %q", th.Ornament.HeroInk, m.Palettes[0].Art[3])
	}
}

func TestResolveThemeV2_JSONShape(t *testing.T) {
	m, _ := ValidateManifest(mustJSON(t, oliveGrove()))
	b, err := json.Marshal(ResolveTheme(m, nil, ""))
	if err != nil {
		t.Fatal(err)
	}
	var top map[string]json.RawMessage
	if err := json.Unmarshal(b, &top); err != nil {
		t.Fatal(err)
	}
	for _, k := range []string{"engine", "layers", "art", "foil", "ornament", "card", "motion", "accent_ink", "palette", "fonts", "background"} {
		if _, ok := top[k]; !ok {
			t.Errorf("v2 theme JSON lacks %q", k)
		}
	}
	// A v1 theme carries none of the engine-2 keys.
	v1, _ := ValidateManifest(mustJSON(t, validManifestMap()))
	b, _ = json.Marshal(ResolveTheme(v1, nil, ""))
	for _, k := range []string{`"engine"`, `"layers"`, `"art"`, `"foil"`, `"ornament"`, `"card"`, `"motion"`} {
		if strings.Contains(string(b), k) {
			t.Errorf("v1 theme JSON contains %s: %s", k, b)
		}
	}
}

// A hand-built schema-2 manifest that skipped validation must still resolve
// to hex-only output without panicking (ResolveTheme's "never fails" rule).
func TestResolveThemeV2_UnvalidatedManifestDoesNotPanic(t *testing.T) {
	m := Manifest{
		Schema: 2, Layout: "centered", HeroStyle: "text_only",
		Palettes: []Palette{{ID: "p", Name: "P", Colors: PaletteColors{
			Background: "#FFFFFF", Surface: "#FFFFFF", Text: "#000000", Muted: "#333333", Accent: "#000000", AccentText: "#FFFFFF"}}},
		Fonts:    []FontPair{{ID: "f", Heading: "lora", Body: "lora"}},
		Defaults: ManifestDefaults{Palette: "p", Font: "f"},
		Layers:   []Layer{{Kind: kindPaper, Tone: "art8", Glow: "bogus"}, {Kind: kindArt, Colors: []string{"art3"}}},
	}
	th := ResolveTheme(m, nil, "")
	for _, c := range []string{th.Layers[0].Tone, th.Layers[0].Glow, th.Layers[1].Colors[0]} {
		if !strictHexRe.MatchString(c) {
			t.Errorf("unresolvable token produced %q, want a hex fallback", c)
		}
	}
}
