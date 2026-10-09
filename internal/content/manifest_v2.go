package content

import (
	"fmt"
	"math"
	"strings"
	"unicode"
	"unicode/utf8"
)

// Layer is one entry of a schema-2 manifest's `layers` stack. One flat struct
// serves every kind; a field foreign to its kind is invalid_field. Colour
// fields hold tokens in a manifest and hex colours in a resolved Theme.
type Layer struct {
	Kind            string   `json:"kind"`
	Region          string   `json:"region,omitempty"`
	Tone            string   `json:"tone,omitempty"`
	Glow            string   `json:"glow,omitempty"`
	GlowOpacity     *float64 `json:"glow_opacity,omitempty"`
	Vignette        string   `json:"vignette,omitempty"`
	VignetteOpacity *float64 `json:"vignette_opacity,omitempty"`
	Edge            string   `json:"edge,omitempty"`
	Pattern         string   `json:"pattern,omitempty"`
	Texture         string   `json:"texture,omitempty"`
	Art             string   `json:"art,omitempty"`
	Frame           string   `json:"frame,omitempty"`
	Color           string   `json:"color,omitempty"`
	Colors          []string `json:"colors,omitempty"`
	Paint           string   `json:"paint,omitempty"`
	Origin          string   `json:"origin,omitempty"`
	Mask            string   `json:"mask,omitempty"`
	Blend           string   `json:"blend,omitempty"`
	Placement       string   `json:"placement,omitempty"`
	Density         string   `json:"density,omitempty"`
	Opacity         *float64 `json:"opacity,omitempty"`
	Inset           *int     `json:"inset,omitempty"`
	// Src is never accepted from a manifest; the server sets it on a
	// resolved image layer.
	Src string `json:"src,omitempty"`
}

// Ornament selects the decorative pieces of a schema-2 theme. In a manifest
// HeroInk is a colour token or ""; in a resolved Theme it is a hex colour.
type Ornament struct {
	Hero      string `json:"hero"`
	Divider   string `json:"divider"`
	Badge     string `json:"badge"`
	Ampersand string `json:"ampersand"`
	HeroInk   string `json:"hero_ink"`
}

// Card styles the RSVP/details card of a schema-2 theme.
type Card struct {
	Style   string `json:"style"`
	Border  string `json:"border"`
	Radius  *int   `json:"radius"`
	Fields  string `json:"fields"`
	Buttons string `json:"buttons"`
}

const (
	minNameRunes = 1
	maxNameRunes = 40

	// Layer opacity caps for the layers that can sit behind text (see
	// checkPaletteContrastV2): an image is limited like the v1 background.
	maxImageOpacity = 0.5

	// Contrast floors (WCAG): readable text, and large text / non-text UI.
	minTextContrastV2 = 4.5
	minLargeContrast  = 3.0

	defaultFrameInset = 12
	defaultCardRadius = 24
)

func ptrFloat(v float64) *float64 { return &v }

// round3 rounds an already range-checked opacity to 3 decimals so the
// canonical JSON never carries long or exponent forms (1e-7).
func round3(v *float64) {
	if v != nil {
		*v = math.Round(*v*1000) / 1000
	}
}
func ptrInt(v int) *int { return &v }

// outOfRange is written so a NaN fails the bounds check.
func outOfRange(v, lo, hi float64) bool { return !(v >= lo && v <= hi) }

// checkSchemaFields enforces that a manifest only uses the fields of its own
// schema: v2 fields in schema 1, or the v1-only decoration/surface/texture/
// background in schema 2, are schema_mismatch.
func checkSchemaFields(iss *issues, m *Manifest) {
	switch m.Schema {
	case 1:
		for _, f := range []struct {
			name string
			set  bool
		}{
			{"layers", m.Layers != nil}, {"ornament", m.Ornament != nil},
			{"card", m.Card != nil}, {"motion", m.Motion != ""},
		} {
			if f.set {
				iss.add(f.name, "schema_mismatch", "this field needs schema 2")
			}
		}
		for i, p := range m.Palettes {
			if p.AccentInk != "" || p.Art != nil || p.Foil != nil {
				iss.add(fmt.Sprintf("palettes[%d]", i), "schema_mismatch", "accent_ink, art and foil need schema 2")
			}
		}
	case 2:
		for _, f := range []struct {
			name string
			set  bool
		}{
			{"decoration", m.Decoration != ""}, {"surface", m.Surface != ""},
			{"texture", m.Texture != ""}, {"background", m.Background != nil},
		} {
			if f.set {
				iss.add(f.name, "schema_mismatch", "this field is only valid in schema 1")
			}
		}
	}
}

// validateV2 validates the schema-2 part of a manifest and normalises its
// defaults in place so the canonical form is explicit and idempotent.
// layout, hero_style, heading_scale, palette ids, fonts and defaults are
// validated by ValidateManifest for both schemas.
func validateV2(iss *issues, m *Manifest) {
	// Palettes: per-palette fields first (hex normalisation), contrast last.
	palOK := make([]bool, len(m.Palettes))
	for i := range m.Palettes {
		palOK[i] = validatePaletteV2(iss, fmt.Sprintf("palettes[%d]", i), &m.Palettes[i])
	}

	// Layers.
	if len(m.Layers) > maxLayers {
		iss.add("layers", "too_many", fmt.Sprintf("at most %d layers", maxLayers))
	}
	for i := range m.Layers {
		validateLayer(iss, fmt.Sprintf("layers[%d]", i), i, &m.Layers[i], m)
	}
	validateLayerCounts(iss, m)

	// Ornament, card, motion: defaults then enums.
	if m.Ornament == nil {
		m.Ornament = &Ornament{}
	}
	validateOrnament(iss, m.Ornament)
	if m.Card == nil {
		m.Card = &Card{}
	}
	validateCard(iss, m.Card)
	if m.Motion == "" {
		m.Motion = "none"
	}
	if !validMotions[m.Motion] {
		iss.add("motion", "invalid_value", "unknown motion")
	}

	// Cross references.
	hasBlock := false
	for _, l := range m.Layers {
		if l.Kind == kindBlock {
			hasBlock = true
		}
	}
	colorBlock := m.HeroStyle == "color_block"
	if colorBlock && !hasBlock {
		iss.add("layers", "invalid_combination", "hero style color_block needs a block layer")
	}
	if hasBlock && !colorBlock {
		iss.add("layers", "invalid_combination", "a block layer needs hero style color_block")
	}

	needArt := maxArtTokenIndex(m)
	foil := usesFoil(m)
	for i := range m.Palettes {
		p := &m.Palettes[i]
		path := fmt.Sprintf("palettes[%d]", i)
		if len(p.Art) < needArt {
			iss.add(path+".art", "missing_palette_color", fmt.Sprintf("needs at least %d art colours", needArt))
		}
		if foil && len(p.Foil) == 0 {
			iss.add(path+".foil", "missing_palette_color", "this theme uses foil, so every palette needs foil colours")
		}
	}

	// Per-palette contrast.
	glass := m.Card.Style == "glass"
	for i := range m.Palettes {
		if palOK[i] {
			checkPaletteContrastV2(iss, i, &m.Palettes[i], m, glass)
		}
	}
}

// validatePaletteV2 validates one schema-2 palette's name, hex colours and
// the optional accent_ink/art/foil, upper-casing every hex. It reports
// whether all colours are usable for the contrast checks.
func validatePaletteV2(iss *issues, path string, p *Palette) bool {
	checkNameV2(iss, path+".name", p.Name)

	ok := normalizePaletteHex(iss, path+".colors", &p.Colors)
	if p.AccentInk != "" {
		if hexColorRe.MatchString(p.AccentInk) {
			p.AccentInk = strings.ToUpper(p.AccentInk)
		} else {
			iss.add(path+".accent_ink", "invalid_color", "must be a #RRGGBB hex colour")
			ok = false
		}
	}
	if len(p.Art) > maxPaletteArt {
		iss.add(path+".art", "too_many", fmt.Sprintf("at most %d art colours", maxPaletteArt))
	}
	if !normalizeHexList(iss, path+".art", p.Art) {
		ok = false
	}
	switch {
	case len(p.Foil) == 0:
	case len(p.Foil) < minFoilStops:
		iss.add(path+".foil", "invalid_length", fmt.Sprintf("needs %d-%d colours", minFoilStops, maxFoilStops))
	case len(p.Foil) > maxFoilStops:
		iss.add(path+".foil", "too_many", fmt.Sprintf("needs %d-%d colours", minFoilStops, maxFoilStops))
	}
	if !normalizeHexList(iss, path+".foil", p.Foil) {
		ok = false
	}
	return ok
}

// checkNameV2 validates a schema-2 display name (palette or font pair):
// 1-40 runes, no control, format or line/paragraph-separator characters, not
// blank and no leading or trailing whitespace.
func checkNameV2(iss *issues, path, name string) {
	if n := utf8.RuneCountInString(name); n < minNameRunes || n > maxNameRunes {
		iss.add(path, "invalid_length", fmt.Sprintf("must be %d-%d characters", minNameRunes, maxNameRunes))
		return
	}
	for _, r := range name {
		if unicode.IsControl(r) || unicode.Is(unicode.Cf, r) || unicode.Is(unicode.Zl, r) || unicode.Is(unicode.Zp, r) {
			iss.add(path, "invalid_value", "contains characters that aren't allowed")
			return
		}
	}
	if strings.TrimSpace(name) != name {
		iss.add(path, "invalid_value", "must not start or end with whitespace")
	}
}

// normalizeHexList validates and upper-cases a list of hex colours in place.
func normalizeHexList(iss *issues, path string, list []string) bool {
	ok := true
	for i, c := range list {
		if !hexColorRe.MatchString(c) {
			iss.add(fmt.Sprintf("%s[%d]", path, i), "invalid_color", "must be a #RRGGBB hex colour")
			ok = false
			continue
		}
		list[i] = strings.ToUpper(c)
	}
	return ok
}

// layerFieldNames returns the names of the fields that are set on l, other
// than "kind".
func layerFieldNames(l *Layer) []string {
	set := []struct {
		name string
		on   bool
	}{
		{"region", l.Region != ""}, {"tone", l.Tone != ""}, {"glow", l.Glow != ""},
		{"glow_opacity", l.GlowOpacity != nil}, {"vignette", l.Vignette != ""},
		{"vignette_opacity", l.VignetteOpacity != nil}, {"edge", l.Edge != ""},
		{"pattern", l.Pattern != ""}, {"texture", l.Texture != ""}, {"art", l.Art != ""},
		{"frame", l.Frame != ""}, {"color", l.Color != ""}, {"colors", l.Colors != nil},
		{"paint", l.Paint != ""}, {"origin", l.Origin != ""}, {"mask", l.Mask != ""},
		{"blend", l.Blend != ""}, {"placement", l.Placement != ""}, {"density", l.Density != ""},
		{"opacity", l.Opacity != nil}, {"inset", l.Inset != nil}, {"src", l.Src != ""},
	}
	out := make([]string, 0, 8)
	for _, f := range set {
		if f.on {
			out = append(out, f.name)
		}
	}
	return out
}

// validateLayer validates one layer (index i, JSON path `path`) and fills
// its defaults in place. Cross-layer rules (counts, block vs hero style,
// palette-dependent checks) live in validateV2.
func validateLayer(iss *issues, path string, i int, l *Layer, _ *Manifest) {
	if !layerKinds[l.Kind] {
		iss.add(path+".kind", "invalid_value", "unknown layer kind")
		return
	}

	// Per-kind field presence.
	allowed := layerFieldSets[l.Kind]
	for _, f := range layerFieldNames(l) {
		if allowed[f] {
			continue
		}
		if f == "src" {
			iss.add(path+".src", "invalid_field", "src is set by the server")
			continue
		}
		iss.add(path+"."+f, "invalid_field", "not valid for a "+l.Kind+" layer")
	}

	// Region.
	switch {
	case l.Region == "":
		l.Region = regionPage
		if l.Kind == kindBlock {
			l.Region = regionHero
		}
	case !validRegions[l.Region]:
		iss.add(path+".region", "invalid_value", "unknown region")
	case l.Kind == kindBlock && l.Region != regionHero,
		(l.Kind == kindPaper || l.Kind == kindFrame) && l.Region != regionPage:
		iss.add(path+".region", "invalid_combination", "this region isn't allowed for a "+l.Kind+" layer")
	}

	if l.Blend != "" && !validBlends[l.Blend] {
		iss.add(path+".blend", "invalid_value", "unknown blend")
	}

	switch l.Kind {
	case kindPaper:
		if i != 0 {
			iss.add(path, "invalid_combination", "a paper layer must be the first layer")
		}
		if l.Tone == "" {
			l.Tone = "background"
		}
		checkToken(iss, path+".tone", l.Tone, true)
		if l.Tone != "background" && l.Tone != "surface" {
			if _, known := colourTokens[l.Tone]; known {
				iss.add(path+".tone", "invalid_value", "must be background or surface")
			}
		}
		l.GlowOpacity = validateWash(iss, path, "glow", l.Glow, l.GlowOpacity)
		l.VignetteOpacity = validateWash(iss, path, "vignette", l.Vignette, l.VignetteOpacity)
	case kindBlock:
		if l.Edge == "" {
			l.Edge = "straight"
		}
		if !validEdges[l.Edge] {
			iss.add(path+".edge", "invalid_value", "unknown edge")
		}
	case kindPattern:
		if !validPatterns[l.Pattern] {
			iss.add(path+".pattern", "invalid_value", "unknown pattern")
		}
		checkToken(iss, path+".color", l.Color, true)
		checkOpacity(iss, path+".opacity", l.Opacity, 0.01, 0.35)
		round3(l.Opacity)
		if l.Origin == "" {
			l.Origin = "center"
		}
		if !validOrigins[l.Origin] {
			iss.add(path+".origin", "invalid_value", "unknown origin")
		}
		if l.Mask == "" {
			l.Mask = "none"
		}
		if !validMasks[l.Mask] {
			iss.add(path+".mask", "invalid_value", "unknown mask")
		}
	case kindTexture:
		validateTexture(iss, path, l)
	case kindArt:
		validateArt(iss, path, l)
	case kindFrame:
		if !validFrames[l.Frame] {
			iss.add(path+".frame", "invalid_value", "unknown frame")
		}
		if l.Inset == nil {
			l.Inset = ptrInt(defaultFrameInset)
		} else if outOfRange(float64(*l.Inset), 0, 32) {
			iss.add(path+".inset", "out_of_range", "must be between 0 and 32")
		}
		switch {
		case l.Paint != "" && l.Color != "":
			iss.add(path, "invalid_combination", "use either color or paint, not both")
		case l.Paint == "" && l.Color == "":
			iss.add(path+".color", "missing_value", "needs a color or paint")
		case l.Paint != "":
			checkPaint(iss, path+".paint", l.Paint)
		default:
			checkToken(iss, path+".color", l.Color, true)
		}
	case kindImage:
		checkOpacity(iss, path+".opacity", l.Opacity, 0.05, maxImageOpacity)
		round3(l.Opacity)
	}
}

// validateWash validates a paper layer's glow or vignette colour token and
// its opacity (field name+"_opacity"), returning the opacity with its default
// (1) applied.
func validateWash(iss *issues, path, name, token string, opacity *float64) *float64 {
	if token == "" {
		if opacity != nil {
			iss.add(path+"."+name+"_opacity", "invalid_field", "needs a "+name+" colour")
		}
		return opacity
	}
	checkToken(iss, path+"."+name, token, true)
	if opacity == nil {
		return ptrFloat(1)
	}
	if outOfRange(*opacity, 0, 1) {
		iss.add(path+"."+name+"_opacity", "out_of_range", "must be between 0 and 1")
	}
	round3(opacity)
	return opacity
}

func validateTexture(iss *issues, path string, l *Layer) {
	if !validTextureV2[l.Texture] {
		iss.add(path+".texture", "invalid_value", "unknown texture")
	}
	checkOpacity(iss, path+".opacity", l.Opacity, 0.02, 0.4)
	round3(l.Opacity)
	if l.Blend == "" {
		l.Blend = "multiply"
	}
	switch {
	case l.Texture == "watercolour":
		if len(l.Colors) != 3 {
			iss.add(path+".colors", "invalid_combination", "a watercolour texture needs exactly 3 colours")
		}
	case len(l.Colors) > 0:
		iss.add(path+".colors", "invalid_combination", "only a watercolour texture takes colours")
	}
	for j, c := range l.Colors {
		checkToken(iss, fmt.Sprintf("%s.colors[%d]", path, j), c, true)
	}
}

func validateArt(iss *issues, path string, l *Layer) {
	spec, known := artRegistry[l.Art]
	if !known {
		iss.add(path+".art", "invalid_value", "unknown art")
	}
	switch {
	case !allArtPlacements[l.Placement]:
		iss.add(path+".placement", "invalid_value", "unknown placement")
	case known && !spec.placements[l.Placement]:
		iss.add(path+".placement", "invalid_combination", "this art doesn't support that placement")
	}
	checkOptionalOpacity(iss, path, l)

	if l.Paint != "" {
		checkPaint(iss, path+".paint", l.Paint)
		if known && !spec.foil {
			iss.add(path+".paint", "invalid_combination", "this art can't be painted with foil")
		}
		if len(l.Colors) > 0 {
			iss.add(path, "invalid_combination", "use either colors or paint, not both")
		}
	} else if known {
		switch {
		case len(l.Colors) > spec.maxColors:
			iss.add(path+".colors", "too_many", fmt.Sprintf("this art takes at most %d colours", spec.maxColors))
		case len(l.Colors) < spec.minColors:
			iss.add(path+".colors", "invalid_length", fmt.Sprintf("this art needs at least %d colours", spec.minColors))
		}
	}
	for j, c := range l.Colors {
		checkToken(iss, fmt.Sprintf("%s.colors[%d]", path, j), c, true)
	}

	switch {
	case known && !spec.density:
		if l.Density != "" {
			iss.add(path+".density", "invalid_field", "this art has no density")
		}
	case known:
		if l.Density == "" {
			l.Density = "normal"
		}
		if !validDensities[l.Density] {
			iss.add(path+".density", "invalid_value", "unknown density")
		}
	}
}

// checkOptionalOpacity validates an art layer's opacity (default 1).
func checkOptionalOpacity(iss *issues, path string, l *Layer) {
	if l.Opacity == nil {
		l.Opacity = ptrFloat(1)
		return
	}
	checkOpacity(iss, path+".opacity", l.Opacity, 0.1, 1)
	round3(l.Opacity)
}

// checkOpacity validates a required opacity in [lo, hi].
func checkOpacity(iss *issues, path string, v *float64, lo, hi float64) {
	switch {
	case v == nil:
		iss.add(path, "missing_value", "is required")
	case outOfRange(*v, lo, hi):
		iss.add(path, "out_of_range", fmt.Sprintf("must be between %g and %g", lo, hi))
	}
}

func checkPaint(iss *issues, path, paint string) {
	if paint != paintFoil {
		iss.add(path, "invalid_value", `must be "foil"`)
	}
}

// checkToken validates a colour token. An empty token is missing_value when
// required and accepted otherwise.
func checkToken(iss *issues, path, token string, required bool) {
	if token == "" {
		if required {
			iss.add(path, "missing_value", "is required")
		}
		return
	}
	if _, ok := colourTokens[token]; !ok {
		iss.add(path, "unknown_token", "unknown colour token")
	}
}

// validateLayerCounts enforces the per-kind and blend caps once every layer
// has had its defaults applied.
func validateLayerCounts(iss *issues, m *Manifest) {
	seen := make(map[string]int, len(maxLayersPerKind))
	blends := 0
	for i, l := range m.Layers {
		if limit, ok := maxLayersPerKind[l.Kind]; ok {
			seen[l.Kind]++
			if seen[l.Kind] > limit {
				iss.add(fmt.Sprintf("layers[%d]", i), "too_many", fmt.Sprintf("at most %d %s layer(s)", limit, l.Kind))
			}
		}
		if l.Blend != "" && l.Blend != "normal" {
			blends++
			if blends == maxBlendLayers+1 {
				iss.add(fmt.Sprintf("layers[%d].blend", i), "too_many", fmt.Sprintf("at most %d layers may blend", maxBlendLayers))
			}
		}
	}
}

func validateOrnament(iss *issues, o *Ornament) {
	for _, f := range []struct {
		name  string
		val   *string
		valid map[string]bool
	}{
		{"hero", &o.Hero, validHeroOrnaments}, {"divider", &o.Divider, validDividers},
		{"badge", &o.Badge, validBadges}, {"ampersand", &o.Ampersand, validAmpersands},
	} {
		if *f.val == "" {
			*f.val = "none"
		}
		if !f.valid[*f.val] {
			iss.add("ornament."+f.name, "invalid_value", "unknown "+f.name+" ornament")
		}
	}
	checkToken(iss, "ornament.hero_ink", o.HeroInk, false)
}

func validateCard(iss *issues, c *Card) {
	if c.Style == "" {
		c.Style = "soft"
	}
	if c.Border == "" {
		c.Border = "hairline"
	}
	if c.Fields == "" {
		c.Fields = "boxed"
	}
	if c.Buttons == "" {
		c.Buttons = "accent"
	}
	if !validCardStyles[c.Style] {
		iss.add("card.style", "invalid_value", "unknown card style")
	}
	if !validCardBorders[c.Border] {
		iss.add("card.border", "invalid_value", "unknown card border")
	}
	if !validCardFields[c.Fields] {
		iss.add("card.fields", "invalid_value", "unknown field style")
	}
	if !validCardButtons[c.Buttons] {
		iss.add("card.buttons", "invalid_value", "unknown button style")
	}
	switch {
	case c.Radius == nil && c.Style == "chamfered":
		c.Radius = ptrInt(0)
	case c.Radius == nil:
		c.Radius = ptrInt(defaultCardRadius)
	case outOfRange(float64(*c.Radius), 0, 32):
		iss.add("card.radius", "out_of_range", "must be between 0 and 32")
	case c.Style == "chamfered" && *c.Radius != 0:
		iss.add("card.radius", "invalid_combination", "a chamfered card has no radius")
	}
}

// maxArtTokenIndex returns the highest artN any token in the manifest needs.
func maxArtTokenIndex(m *Manifest) int {
	need := 0
	note := func(tok string) {
		if n := colourTokens[tok]; n > need {
			need = n
		}
	}
	for _, l := range m.Layers {
		for _, t := range [...]string{l.Tone, l.Glow, l.Vignette, l.Color} {
			note(t)
		}
		for _, t := range l.Colors {
			note(t)
		}
	}
	if m.Ornament != nil {
		note(m.Ornament.HeroInk)
	}
	return need
}

// usesFoil reports whether anything in the manifest renders with the
// palette's foil colours.
func usesFoil(m *Manifest) bool {
	for _, l := range m.Layers {
		if l.Paint == paintFoil || (l.Kind == kindTexture && l.Texture == "foil") {
			return true
		}
	}
	if o := m.Ornament; o != nil && (o.Hero == "foil_numeral" || o.Badge == "foil_seal") {
		return true
	}
	if c := m.Card; c != nil && (c.Border == "foil" || c.Border == "foil_inset" || c.Buttons == "foil") {
		return true
	}
	return m.Motion == "foil_sheen"
}

// checkPaletteContrastV2 applies the schema-2 contrast rules to palette i.
// All its colours are already valid, upper-cased hex.
func checkPaletteContrastV2(iss *issues, i int, p *Palette, m *Manifest, glass bool) {
	path := fmt.Sprintf("palettes[%d]", i)
	c := p.Colors

	low := func(at, fg, bg string, floor float64, msg string) {
		if contrastRatio(fg, bg) < floor {
			iss.add(at, "low_contrast", msg)
		}
	}
	low(path+".colors", c.Text, c.Background, minTextContrastV2, "text on background doesn't meet 4.5:1 contrast")
	low(path+".colors", c.Text, c.Surface, minTextContrastV2, "text on surface doesn't meet 4.5:1 contrast")
	low(path+".colors", c.AccentText, c.Accent, minTextContrastV2, "accent text on accent doesn't meet 4.5:1 contrast")
	low(path+".colors", c.Muted, c.Background, minTextContrastV2, "muted text on background doesn't meet 4.5:1 contrast")
	low(path+".colors", c.Muted, c.Surface, minTextContrastV2, "muted text on surface doesn't meet 4.5:1 contrast")

	var blends []string
	if glass {
		blends = []string{
			blendHex(c.Surface, c.Background, glassAlphaWide),
			blendHex(c.Surface, c.Background, glassAlphaNarrow),
		}
		for _, b := range blends {
			low(path+".colors", c.Text, b, minTextContrastV2, "text over the glass card doesn't meet 4.5:1 contrast")
			low(path+".colors", c.Muted, b, minTextContrastV2, "muted text over the glass card doesn't meet 4.5:1 contrast")
		}
	}

	if p.AccentInk != "" {
		low(path+".accent_ink", p.AccentInk, c.Background, minTextContrastV2, "accent ink on background doesn't meet 4.5:1 contrast")
		low(path+".accent_ink", p.AccentInk, c.Surface, minTextContrastV2, "accent ink on surface doesn't meet 4.5:1 contrast")
		for _, b := range blends {
			low(path+".accent_ink", p.AccentInk, b, minTextContrastV2, "accent ink over the glass card doesn't meet 4.5:1 contrast")
		}
	}

	// Hero: the ink drawn over the hero backdrop (the accent block for
	// color_block, else the page background).
	backdrop := c.Background
	if m.HeroStyle == "color_block" {
		backdrop = c.Accent
	}
	heroInk := resolveHeroInk(*p, m, glass)
	low("ornament.hero_ink", heroInk, backdrop, minLargeContrast,
		fmt.Sprintf("hero ink on the hero doesn't meet 3:1 contrast in palette %d", i))
	if m.Ornament.Hero == "foil_numeral" {
		for _, stop := range p.Foil {
			low("ornament.hero", stop, backdrop, minLargeContrast,
				fmt.Sprintf("foil numeral doesn't meet 3:1 contrast on the hero in palette %d", i))
		}
	}
	if m.Card.Buttons == "foil" {
		for _, stop := range p.Foil {
			low("card.buttons", c.AccentText, stop, minTextContrastV2,
				fmt.Sprintf("button text doesn't meet 4.5:1 contrast on the foil in palette %d", i))
		}
	}

	checkLayerContrastV2(iss, i, p, paletteInk(*p, glass), m)
}

// checkLayerContrastV2 checks that the layers that tint the area behind
// text can't make text or muted text unreadable in palette i. The worst case
// is assumed: the wash or pattern colour fully covers the text at its
// opacity, over both the background and the surface colour (text sits on
// either). Image layers are bounded by their opacity cap instead.
func checkLayerContrastV2(iss *issues, i int, p *Palette, ink string, m *Manifest) {
	c := p.Colors
	bases := [...]string{c.Background, c.Surface}
	check := func(path, what, tok string, opacity *float64) {
		if _, known := colourTokens[tok]; !known || opacity == nil || outOfRange(*opacity, 0, 1) {
			return // already reported as invalid
		}
		col := tokenHex(*p, ink, tok)
		for _, base := range bases {
			b := blendHex(col, base, *opacity)
			if contrastRatio(c.Text, b) < minTextContrastV2 {
				iss.add(path, "low_contrast", fmt.Sprintf("text over the %s doesn't meet 4.5:1 contrast in palette %d", what, i))
			}
			if contrastRatio(c.Muted, b) < minTextContrastV2 {
				iss.add(path, "low_contrast", fmt.Sprintf("muted text over the %s doesn't meet 4.5:1 contrast in palette %d", what, i))
			}
		}
	}
	for li := range m.Layers {
		l := &m.Layers[li]
		path := fmt.Sprintf("layers[%d]", li)
		switch l.Kind {
		case kindPaper:
			check(path+".glow", "paper glow", l.Glow, l.GlowOpacity)
			check(path+".vignette", "paper vignette", l.Vignette, l.VignetteOpacity)
		case kindPattern:
			check(path+".color", "pattern", l.Color, l.Opacity)
		}
	}
}

// paletteInk is a schema-2 palette's accent ink: its explicit accent_ink, or
// the accent when it reads at 4.5:1 on every background it can appear on,
// else the text colour.
func paletteInk(p Palette, glass bool) string {
	if p.AccentInk != "" {
		return p.AccentInk
	}
	return accentInkMin(p.Colors, glass, minTextContrastV2)
}

// resolveHeroInk resolves ornament.hero_ink for p: the token's colour, or
// the default (accent_text on a color_block hero, else the accent ink).
func resolveHeroInk(p Palette, m *Manifest, glass bool) string {
	ink := paletteInk(p, glass)
	if tok := m.Ornament.HeroInk; tok != "" {
		return tokenHex(p, ink, tok)
	}
	if m.HeroStyle == "color_block" {
		return p.Colors.AccentText
	}
	return ink
}

// tokenHex resolves a colour token against palette p (ink is p's resolved
// accent ink). A token that cannot resolve falls back to the text colour so
// a resolved theme only ever holds hex colours.
func tokenHex(p Palette, ink, tok string) string {
	c := p.Colors
	switch tok {
	case "background":
		return c.Background
	case "surface":
		return c.Surface
	case "text":
		return c.Text
	case "muted":
		return c.Muted
	case "accent":
		return c.Accent
	case "accent_text":
		return c.AccentText
	case "accent_ink":
		return ink
	}
	if n := colourTokens[tok]; n >= 1 && n <= len(p.Art) {
		return p.Art[n-1]
	}
	return c.Text
}

// UsesBackgroundAsset reports whether the manifest needs the template
// version's uploaded background asset: a schema-1 background or any schema-2
// image layer.
func (m Manifest) UsesBackgroundAsset() bool {
	if m.Background != nil {
		return true
	}
	for _, l := range m.Layers {
		if l.Kind == kindImage {
			return true
		}
	}
	return false
}
