package content

import (
	"fmt"
	"math"
)

// Theme is the fully resolved design sent to the frontend, which never
// parses a manifest itself.
type Theme struct {
	Layout       string `json:"layout"`
	HeroStyle    string `json:"hero_style"`
	Decoration   string `json:"decoration"`
	Surface      string `json:"surface"`
	Texture      string `json:"texture"`
	HeadingScale string `json:"heading_scale"`
	// AccentInk is the readable colour for accent-styled text (kickers,
	// countdown numerals, the heart divider): the palette's accent when it
	// has enough contrast to read as large text/non-text UI against every
	// background it can appear on, else the palette's text colour.
	AccentInk  string           `json:"accent_ink"`
	Palette    PaletteColors    `json:"palette"`
	Fonts      ThemeFonts       `json:"fonts"`
	Background *ThemeBackground `json:"background"`

	// Schema-2 (theme engine v2) additions; all omitempty so a v1 theme's
	// JSON is byte-identical. Every colour is a resolved hex string. For a v2
	// theme the v1 fields above carry fallbacks for older renderers
	// (decoration, surface, texture) and AccentInk is the palette's ink.
	Engine   int       `json:"engine,omitempty"`
	Layers   []Layer   `json:"layers,omitempty"`
	Art      []string  `json:"art,omitempty"`
	Foil     []string  `json:"foil,omitempty"`
	Ornament *Ornament `json:"ornament,omitempty"`
	Card     *Card     `json:"card,omitempty"`
	Motion   string    `json:"motion,omitempty"`
}

type ThemeFonts struct {
	Heading string `json:"heading"`
	Body    string `json:"body"`
	// Accent is the kicker face: the pair's own accent, else its heading.
	// Never empty.
	Accent string `json:"accent"`
}

type ThemeBackground struct {
	Src     string  `json:"src"`
	Opacity float64 `json:"opacity"`
}

// ResolveTheme applies overrides (falling back to the manifest defaults) to
// produce the theme sent to the frontend. m and overridesRaw are assumed
// already valid (ValidateManifest/ValidateOverrides ran at save time), so
// this never fails; an inconsistency falls back to the manifest defaults
// defensively rather than panicking.
func ResolveTheme(m Manifest, overridesRaw []byte, backgroundSrc string) Theme {
	var o Overrides
	if len(overridesRaw) > 0 {
		_ = strictUnmarshal(overridesRaw, &o)
	}

	paletteID := o.Palette
	if paletteID == "" || !hasPaletteID(m, paletteID) {
		paletteID = m.Defaults.Palette
	}
	fontID := o.Font
	if fontID == "" || !hasFontID(m, fontID) {
		fontID = m.Defaults.Font
	}

	t := Theme{
		Layout: m.Layout, HeroStyle: m.HeroStyle, Decoration: m.Decoration,
		Surface: m.Surface, Texture: m.Texture, HeadingScale: m.HeadingScale,
	}
	var selected Palette
	for _, p := range m.Palettes {
		if p.ID == paletteID {
			selected = p
			t.Palette = p.Colors
			break
		}
	}
	for _, f := range m.Fonts {
		if f.ID == fontID {
			accent := f.Accent
			if accent == "" {
				accent = f.Heading
			}
			t.Fonts = ThemeFonts{Heading: f.Heading, Body: f.Body, Accent: accent}
			break
		}
	}
	if m.Background != nil && backgroundSrc != "" {
		t.Background = &ThemeBackground{Src: backgroundSrc, Opacity: m.Background.Opacity}
	}
	if m.Schema == 2 {
		resolveV2(&t, m, selected, backgroundSrc)
		return t
	}
	t.AccentInk = accentInk(t.Palette, t.Surface)
	return t
}

// resolveV2 fills the schema-2 part of t from manifest m and the selected
// palette p: every colour token becomes that palette's hex, the image layer
// gets its server-built src (and is dropped when there is no asset), and the
// v1 fields get fallbacks for older renderers. m is assumed valid.
func resolveV2(t *Theme, m Manifest, p Palette, backgroundSrc string) {
	glass := m.Card != nil && m.Card.Style == "glass"
	ink := paletteInk(p, glass)

	t.Engine = 2
	t.AccentInk = ink
	t.Art = append([]string(nil), p.Art...)
	t.Foil = append([]string(nil), p.Foil...)
	t.Motion = m.Motion

	var o Ornament
	var c Card
	if m.Ornament != nil {
		o = *m.Ornament
		o.HeroInk = resolveHeroInk(p, &m, glass)
	}
	if m.Card != nil {
		c = *m.Card
		c.Radius = copyPtr(c.Radius)
	}
	t.Ornament = &o
	t.Card = &c

	hex := func(tok string) string {
		if tok == "" {
			return ""
		}
		return tokenHex(p, ink, tok)
	}
	t.Layers = make([]Layer, 0, len(m.Layers))
	for _, l := range m.Layers {
		switch l.Kind {
		case kindImage:
			if backgroundSrc == "" {
				continue
			}
			l.Src = backgroundSrc
		case kindBlock:
			l.Color = "accent"
		}
		// The by-value copy still shares the manifest's pointers and slices;
		// a resolved theme must never alias manifest memory.
		l.Opacity, l.GlowOpacity, l.VignetteOpacity = copyPtr(l.Opacity), copyPtr(l.GlowOpacity), copyPtr(l.VignetteOpacity)
		l.Inset = copyPtr(l.Inset)
		l.Tone, l.Glow, l.Vignette, l.Color = hex(l.Tone), hex(l.Glow), hex(l.Vignette), hex(l.Color)
		if l.Colors != nil {
			cols := make([]string, len(l.Colors))
			for i, tok := range l.Colors {
				cols[i] = hex(tok)
			}
			l.Colors = cols
		}
		t.Layers = append(t.Layers, l)
	}

	// Fallbacks for a renderer that predates engine 2.
	t.Decoration = "none"
	if v1Dividers[o.Divider] {
		t.Decoration = o.Divider
	}
	switch c.Style {
	case "none":
		t.Surface = "plain"
	case "glass":
		t.Surface = "glass"
	default:
		t.Surface = "card"
	}
	t.Texture = "none"
}

// copyPtr returns a pointer to a copy of *p (nil stays nil).
func copyPtr[T any](p *T) *T {
	if p == nil {
		return nil
	}
	v := *p
	return &v
}

// accentInk implements Theme.AccentInk: the palette's accent colour, unless
// it fails 3:1 contrast (WCAG large-text/non-text UI) against any
// background it can render over, in which case the palette's text colour is
// used instead. Pure function, no failure path — every existing palette
// resolves to its accent except Confetti "Sunshine" (1.60:1), which falls
// back to text.
func accentInk(p PaletteColors, surface string) string {
	return accentInkMin(p, surface == "glass", 3.0)
}

// accentInkMin is accentInk with an explicit contrast floor and glass flag;
// schema 2 uses a 4.5 floor.
func accentInkMin(p PaletteColors, glass bool, minInk float64) string {
	backgrounds := []string{p.Background, p.Surface}
	if glass {
		backgrounds = append(backgrounds,
			blendHex(p.Surface, p.Background, glassAlphaWide),
			blendHex(p.Surface, p.Background, glassAlphaNarrow),
		)
	}
	for _, bg := range backgrounds {
		if contrastRatio(p.Accent, bg) < minInk {
			return p.Text
		}
	}
	return p.Accent
}

// blendHex composites fg over bg at alpha (0..1), a per-channel sRGB alpha
// blend rounded to 8 bits — the same maths the browser performs compositing
// a translucent "glass" surface (must match surface.tsx). fg and bg are
// "#RRGGBB", already validated.
func blendHex(fg, bg string, alpha float64) string {
	fr, fg2, fb := hexRGB(fg)
	br, bg2, bb := hexRGB(bg)
	mix := func(f, b float64) int {
		v := int(math.Round(f*alpha + b*(1-alpha)))
		switch {
		case v < 0:
			return 0
		case v > 255:
			return 255
		default:
			return v
		}
	}
	return fmt.Sprintf("#%02X%02X%02X", mix(fr, br), mix(fg2, bg2), mix(fb, bb))
}

// contrastRatio computes the WCAG contrast ratio between two #RRGGBB colours
// (case-insensitive, already validated by the caller).
func contrastRatio(hex1, hex2 string) float64 {
	l1, l2 := relativeLuminance(hex1), relativeLuminance(hex2)
	if l1 < l2 {
		l1, l2 = l2, l1
	}
	return (l1 + 0.05) / (l2 + 0.05)
}

func relativeLuminance(hex string) float64 {
	r, g, b := hexRGB(hex)
	lin := func(c float64) float64 {
		c /= 255
		if c <= 0.03928 {
			return c / 12.92
		}
		return math.Pow((c+0.055)/1.055, 2.4)
	}
	return 0.2126*lin(r) + 0.7152*lin(g) + 0.0722*lin(b)
}

func hexRGB(hex string) (r, g, b float64) {
	// hex is "#RRGGBB", already validated.
	v := func(a, b byte) float64 { return float64(hexNibble(a)*16 + hexNibble(b)) }
	return v(hex[1], hex[2]), v(hex[3], hex[4]), v(hex[5], hex[6])
}

func hexNibble(c byte) int {
	switch {
	case c >= '0' && c <= '9':
		return int(c - '0')
	case c >= 'a' && c <= 'f':
		return int(c-'a') + 10
	case c >= 'A' && c <= 'F':
		return int(c-'A') + 10
	default:
		return 0
	}
}
