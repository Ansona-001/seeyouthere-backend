package content

import (
	"encoding/json"
	"fmt"
	"regexp"
	"strings"
	"unicode/utf8"
)

// Manifest is a template version's design definition: schema 1 (flat theme
// knobs) or schema 2 (theme engine v2: layers, ornament, card, motion). The
// schema-2 fields are all omitempty so a schema-1 manifest's canonical bytes
// are unchanged; decoration, surface and texture are omitempty because a
// schema-2 manifest must not carry them (schema 1 always has all three).
type Manifest struct {
	Schema     int    `json:"schema"`
	Layout     string `json:"layout"`
	HeroStyle  string `json:"hero_style"`
	Decoration string `json:"decoration,omitempty"`
	// Surface, Texture and HeadingScale are optional theme knobs (rich-blocks
	// doc §4.1). "" normalises to the default ("plain" / "none" / "regular")
	// so every manifest that predates them keeps today's rendered look.
	Surface      string              `json:"surface,omitempty"`
	Texture      string              `json:"texture,omitempty"`
	HeadingScale string              `json:"heading_scale"`
	Palettes     []Palette           `json:"palettes"`
	Fonts        []FontPair          `json:"fonts"`
	Defaults     ManifestDefaults    `json:"defaults"`
	Background   *ManifestBackground `json:"background"`

	// Schema 2 only.
	Layers   []Layer   `json:"layers,omitempty"`
	Ornament *Ornament `json:"ornament,omitempty"`
	Card     *Card     `json:"card,omitempty"`
	Motion   string    `json:"motion,omitempty"`
}

type Palette struct {
	ID     string        `json:"id"`
	Name   string        `json:"name"`
	Colors PaletteColors `json:"colors"`

	// Schema 2 only: an explicit readable ink for accent-styled text, the art
	// colour ramp (art1..art8 tokens) and the foil gradient stops.
	AccentInk string   `json:"accent_ink,omitempty"`
	Art       []string `json:"art,omitempty"`
	Foil      []string `json:"foil,omitempty"`
}

// PaletteColors is the resolved 6-colour theme applied to an event page.
type PaletteColors struct {
	Background string `json:"background"`
	Surface    string `json:"surface"`
	Text       string `json:"text"`
	Muted      string `json:"muted"`
	Accent     string `json:"accent"`
	AccentText string `json:"accent_text"`
}

type FontPair struct {
	ID      string `json:"id"`
	Name    string `json:"name"`
	Heading string `json:"heading"`
	Body    string `json:"body"`
	// Accent is the kicker face for this pair; "" resolves to Heading
	// (ResolveTheme.Fonts.Accent is never empty).
	Accent string `json:"accent"`
}

type ManifestDefaults struct {
	Palette string `json:"palette"`
	Font    string `json:"font"`
}

// ManifestBackground selects the template version's single background asset.
type ManifestBackground struct {
	Asset   string  `json:"asset"`
	Opacity float64 `json:"opacity"`
}

// Overrides is an event's per-instance theme choice (events.overrides).
type Overrides struct {
	Palette string `json:"palette"`
	Font    string `json:"font"`
}

// Manifest size limits. A write (admin create/update) is held to both
// maxManifestBytes on the raw input, checked before decoding, and
// maxManifestCanonicalBytes on the canonical re-marshalled form that is actually
// stored (defaults written out can be bigger than the input). The canonical
// cap leaves room for PostgreSQL's jsonb text (", " and ": " spacing, about
// 8-10% bigger), so what is read back always fits maxStoredBytes by a wide
// margin. The DB CHECK on pg_column_size(manifest) (32768) sits above all of
// them because jsonb's binary form is 10-15% bigger than the text.
const (
	maxManifestBytes          = 24 << 10
	maxManifestCanonicalBytes = 20 << 10
	maxStoredBytes            = 48 << 10
)

var (
	manifestIDRe = regexp.MustCompile(`^[a-z0-9_-]{1,24}$`)
	hexColorRe   = regexp.MustCompile(`^#[0-9A-Fa-f]{6}$`)
)

var validLayouts = map[string]bool{"centered": true, "split": true, "stacked": true}
var validHeroStyles = map[string]bool{"full_bleed": true, "framed": true, "text_only": true, "spotlight": true}
var validDecorations = map[string]bool{"none": true, "line": true, "floral": true, "dots": true, "heart": true}

// validSurfaces, validTextures and validHeadingScales gate the optional
// theme knobs (§4.1); "" is always accepted and normalised below to the
// value that matches today's rendered look.
var validSurfaces = map[string]bool{"": true, "plain": true, "card": true, "glass": true}
var validTextures = map[string]bool{"": true, "none": true, "grid": true, "dots": true}
var validHeadingScales = map[string]bool{"": true, "regular": true, "display": true}

// glassAlphaWide and glassAlphaNarrow are the two composited opacities a
// "glass" surface renders at (below md / from md up respectively) — must
// match surface.tsx. Contrast for a glass palette is checked against both,
// because contrast is not monotonic along an alpha blend.
const (
	glassAlphaWide   = 0.55
	glassAlphaNarrow = 0.72
)

// allowedFonts is the fixed font allowlist shared with the frontend registry
// (src/lib/fonts.ts).
var allowedFonts = map[string]bool{
	"figtree": true, "bricolage_grotesque": true, "playfair_display": true, "cormorant_garamond": true,
	"dm_serif_display": true, "lora": true, "fraunces": true, "great_vibes": true,
	"birthstone": true, "montserrat": true,
	"bodoni_moda": true, "pinyon_script": true, "bagel_fat_one": true,
	"limelight": true, "josefin_sans": true,
}

// headingOnlyFonts are display scripts too narrow/decorative for body text:
// allowed as a font pair's heading or accent, rejected as body.
var headingOnlyFonts = map[string]bool{
	"great_vibes": true, "birthstone": true,
	"pinyon_script": true, "bagel_fat_one": true, "limelight": true,
}

// ValidateManifest decodes and validates a template manifest, normalising
// colours to upper-case hex and checking WCAG contrast so no combination of
// palette and font pair can render unreadable text.
//
// Schema 1 is validated exactly as it always was; every schema-2 rule lives
// in validateV2 and only runs for `schema: 2`.
func ValidateManifest(raw []byte) (Manifest, error) {
	return validateManifest(raw, maxManifestBytes, true)
}

// ValidateStoredManifest is ValidateManifest for a manifest read back from
// the database: the same rules, but sized for PostgreSQL's jsonb text (which
// is looser than the canonical form it was saved from) and without the
// canonical-size cap, so a manifest that passed on write can never fail to
// load. Use ValidateManifest for anything that arrives from a client.
func ValidateStoredManifest(raw []byte) (Manifest, error) {
	return validateManifest(raw, maxStoredBytes, false)
}

func validateManifest(raw []byte, maxRaw int, capCanonical bool) (Manifest, error) {
	if len(raw) > maxRaw {
		return Manifest{}, single("manifest", "too_large", fmt.Sprintf("manifest must be %d KiB or smaller", maxRaw>>10))
	}
	if !utf8.Valid(raw) {
		return Manifest{}, single("manifest", "invalid_json", "must be a manifest object")
	}
	var m Manifest
	if err := strictUnmarshal(raw, &m); err != nil {
		return Manifest{}, single("manifest", "invalid_json", "must be a manifest object")
	}

	iss := &issues{}
	if m.Schema != 1 && m.Schema != 2 {
		return Manifest{}, single("schema", "unsupported_schema", "only schemas 1 and 2 are supported")
	}
	v2 := m.Schema == 2
	checkSchemaFields(iss, &m)

	if !validLayouts[m.Layout] {
		iss.add("layout", "invalid_value", "unknown layout")
	}
	heroStyles := validHeroStyles
	if v2 {
		heroStyles = validHeroStyleV2
	}
	if !heroStyles[m.HeroStyle] {
		iss.add("hero_style", "invalid_value", "unknown hero style")
	}
	if !v2 {
		if !validDecorations[m.Decoration] {
			iss.add("decoration", "invalid_value", "unknown decoration")
		}
		if !validSurfaces[m.Surface] {
			iss.add("surface", "invalid_value", "unknown surface")
		}
		if !validTextures[m.Texture] {
			iss.add("texture", "invalid_value", "unknown texture")
		}
	}
	if !validHeadingScales[m.HeadingScale] {
		iss.add("heading_scale", "invalid_value", "unknown heading scale")
	}
	// Normalise "" to today's default regardless of the checks above, so a
	// caller that only wants the canonical form (e.g. a preview) sees an
	// explicit value even for an otherwise-invalid manifest. Schema 2 has no
	// surface or texture to normalise.
	if !v2 && m.Surface == "" {
		m.Surface = "plain"
	}
	if !v2 && m.Texture == "" {
		m.Texture = "none"
	}
	if m.HeadingScale == "" {
		m.HeadingScale = "regular"
	}

	if len(m.Palettes) < 1 || len(m.Palettes) > 8 {
		iss.add("palettes", "invalid_length", "must have between 1 and 8 palettes")
	}
	paletteIDs := make(map[string]bool, len(m.Palettes))
	for i := range m.Palettes {
		p := &m.Palettes[i]
		path := fmt.Sprintf("palettes[%d]", i)
		if !manifestIDRe.MatchString(p.ID) {
			iss.add(path+".id", "invalid_id", "invalid palette id")
		} else if paletteIDs[p.ID] {
			iss.add(path+".id", "duplicate_id", "palette ids must be unique")
		} else {
			paletteIDs[p.ID] = true
		}
		if !v2 {
			validatePaletteColors(iss, path+".colors", &p.Colors, m.Surface)
		}
	}

	if len(m.Fonts) < 1 || len(m.Fonts) > 6 {
		iss.add("fonts", "invalid_length", "must have between 1 and 6 font pairs")
	}
	fontIDs := make(map[string]bool, len(m.Fonts))
	for i, f := range m.Fonts {
		path := fmt.Sprintf("fonts[%d]", i)
		if !manifestIDRe.MatchString(f.ID) {
			iss.add(path+".id", "invalid_id", "invalid font id")
		} else if fontIDs[f.ID] {
			iss.add(path+".id", "duplicate_id", "font ids must be unique")
		} else {
			fontIDs[f.ID] = true
		}
		if v2 {
			checkNameV2(iss, path+".name", f.Name)
		}
		if !allowedFonts[f.Heading] {
			iss.add(path+".heading", "invalid_value", "unknown font")
		}
		if !allowedFonts[f.Body] {
			iss.add(path+".body", "invalid_value", "unknown font")
		}
		if headingOnlyFonts[f.Body] {
			iss.add(path+".body", "invalid_value", "this font is heading-only")
		}
		if f.Accent != "" && !allowedFonts[f.Accent] {
			iss.add(path+".accent", "invalid_value", "unknown font")
		}
	}

	if m.Defaults.Palette == "" || !paletteIDs[m.Defaults.Palette] {
		iss.add("defaults.palette", "invalid_value", "must reference a palette above")
	}
	if m.Defaults.Font == "" || !fontIDs[m.Defaults.Font] {
		iss.add("defaults.font", "invalid_value", "must reference a font pair above")
	}

	if v2 {
		validateV2(iss, &m)
	}

	if !v2 && m.Background != nil {
		if m.Background.Asset != "background" {
			iss.add("background.asset", "invalid_value", `must be "background"`)
		}
		if m.Background.Opacity < 0 || m.Background.Opacity > 0.5 {
			iss.add("background.opacity", "out_of_range", "must be between 0 and 0.5")
		}
	}

	if err := iss.err(); err != nil {
		return Manifest{}, err
	}
	if capCanonical {
		canon, err := json.Marshal(m)
		if err != nil {
			return Manifest{}, fmt.Errorf("marshal manifest: %w", err)
		}
		if len(canon) > maxManifestCanonicalBytes {
			return Manifest{}, single("manifest", "too_large", fmt.Sprintf("manifest must be %d KiB or smaller once saved", maxManifestCanonicalBytes>>10))
		}
	}
	return m, nil
}

// normalizePaletteHex checks that the six palette colours are #RRGGBB and
// upper-cases them in place, reporting whether all are valid.
func normalizePaletteHex(iss *issues, path string, c *PaletteColors) bool {
	fields := []struct {
		name string
		val  *string
	}{
		{"background", &c.Background}, {"surface", &c.Surface}, {"text", &c.Text},
		{"muted", &c.Muted}, {"accent", &c.Accent}, {"accent_text", &c.AccentText},
	}
	ok := true
	for _, f := range fields {
		if !hexColorRe.MatchString(*f.val) {
			iss.add(path+"."+f.name, "invalid_color", "must be a #RRGGBB hex colour")
			ok = false
			continue
		}
		*f.val = strings.ToUpper(*f.val)
	}
	return ok
}

func validatePaletteColors(iss *issues, path string, c *PaletteColors, surface string) {
	if !normalizePaletteHex(iss, path, c) {
		return
	}
	const minText = 4.5
	const minMuted = 3.0
	if r := contrastRatio(c.Text, c.Background); r < minText {
		iss.add(path, "low_contrast", "text on background doesn't meet 4.5:1 contrast")
	}
	if r := contrastRatio(c.Text, c.Surface); r < minText {
		iss.add(path, "low_contrast", "text on surface doesn't meet 4.5:1 contrast")
	}
	if r := contrastRatio(c.AccentText, c.Accent); r < minText {
		iss.add(path, "low_contrast", "accent text on accent doesn't meet 4.5:1 contrast")
	}
	if r := contrastRatio(c.Muted, c.Background); r < minMuted {
		iss.add(path, "low_contrast", "muted text on background doesn't meet 3:1 contrast")
	}

	// A "glass" surface renders translucent over the background at two
	// composited opacities (surface.tsx); text and muted must stay readable
	// against both, which a solid-surface check alone can't guarantee
	// (contrast isn't monotonic along an alpha blend).
	if surface == "glass" {
		for _, alpha := range [...]float64{glassAlphaWide, glassAlphaNarrow} {
			blend := blendHex(c.Surface, c.Background, alpha)
			if r := contrastRatio(c.Text, blend); r < minText {
				iss.add(path, "low_contrast", "text over the glass surface doesn't meet 4.5:1 contrast")
			}
			if r := contrastRatio(c.Muted, blend); r < minMuted {
				iss.add(path, "low_contrast", "muted text over the glass surface doesn't meet 3:1 contrast")
			}
		}
	}
}

// ValidateOverrides decodes and validates an event's theme overrides against
// the manifest they're pinned to, returning the canonical form.
func ValidateOverrides(raw []byte, m Manifest) ([]byte, error) {
	var o Overrides
	if len(raw) > 0 {
		if err := strictUnmarshal(raw, &o); err != nil {
			return nil, single("overrides", "invalid_json", "must be an overrides object")
		}
	}
	iss := &issues{}
	if o.Palette != "" && !hasPaletteID(m, o.Palette) {
		iss.add("overrides.palette", "invalid_value", "unknown palette for this template")
	}
	if o.Font != "" && !hasFontID(m, o.Font) {
		iss.add("overrides.font", "invalid_value", "unknown font pair for this template")
	}
	if err := iss.err(); err != nil {
		return nil, err
	}
	return json.Marshal(o)
}

func hasPaletteID(m Manifest, id string) bool {
	for _, p := range m.Palettes {
		if p.ID == id {
			return true
		}
	}
	return false
}

func hasFontID(m Manifest, id string) bool {
	for _, f := range m.Fonts {
		if f.ID == id {
			return true
		}
	}
	return false
}
