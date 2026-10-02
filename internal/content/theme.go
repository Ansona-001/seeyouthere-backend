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
	for _, p := range m.Palettes {
		if p.ID == paletteID {
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
	t.AccentInk = accentInk(t.Palette, t.Surface)
	return t
}

// accentInk implements Theme.AccentInk: the palette's accent colour, unless
// it fails 3:1 contrast (WCAG large-text/non-text UI) against any
// background it can render over, in which case the palette's text colour is
// used instead. Pure function, no failure path — every existing palette
// resolves to its accent except Confetti "Sunshine" (1.60:1), which falls
// back to text.
func accentInk(p PaletteColors, surface string) string {
	const minInk = 3.0
	backgrounds := []string{p.Background, p.Surface}
	if surface == "glass" {
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
