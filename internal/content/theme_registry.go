package content

// Every allowlist the schema-2 (theme engine v2) validator consults lives in
// this file, as map lookups on exact strings: no trimming, no case folding.
// A manifest string never reaches CSS or markup unless it is a key of one of
// these maps, a validated hex colour or a bounded number. The art registry
// is mirrored on the frontend (theme-engine/art/registry.ts).

// maxArtKeys is a hard cap on the art registry; theme_registry_test.go
// enforces it so the frontend bundle of art components stays bounded.
const maxArtKeys = 32

// artSpec describes what one art registry key accepts.
type artSpec struct {
	minColors, maxColors int
	foil                 bool // may be painted with paint:"foil"
	density              bool // accepts a density value
	placements           map[string]bool
}

func placements(keys ...string) map[string]bool {
	m := make(map[string]bool, len(keys))
	for _, k := range keys {
		m[k] = true
	}
	return m
}

var artRegistry = map[string]artSpec{
	"olive_branches":   {3, 4, false, false, placements("corners", "top_corners", "bottom_corners")},
	"botanical_wash":   {2, 4, false, false, placements("corners", "top_corners")},
	"confetti":         {2, 6, false, true, placements("hero", "edges", "scatter")},
	"balloons":         {2, 4, false, false, placements("hero_top", "top_corners")},
	"stepped_arches":   {0, 1, true, false, placements("hero")},
	"deco_fans":        {0, 1, true, false, placements("bottom_corners", "corners")},
	"clouds":           {1, 3, false, true, placements("top", "scatter")},
	"open_door_plants": {2, 3, false, false, placements("hero", "bottom_corners")},
	"terrazzo_chips":   {2, 6, false, true, placements("scatter", "edges")},
	"riso_shapes":      {2, 3, false, false, placements("corners", "scatter")},
	"mirror_ball":      {1, 2, true, false, placements("hero_top")},
	"sparkles":         {1, 2, true, true, placements("scatter", "hero")},
	"printers_corners": {1, 1, true, false, placements("corners")},
	"daisies":          {2, 3, false, true, placements("corners", "edges")},
}

// allArtPlacements is the union of every key's placements, so an unknown
// placement (invalid_value) can be told apart from a known one the key does
// not support (invalid_combination).
var allArtPlacements = func() map[string]bool {
	m := map[string]bool{}
	for _, s := range artRegistry {
		for p := range s.placements {
			m[p] = true
		}
	}
	return m
}()

const (
	kindPaper   = "paper"
	kindBlock   = "block"
	kindPattern = "pattern"
	kindTexture = "texture"
	kindArt     = "art"
	kindFrame   = "frame"
	kindImage   = "image"

	regionPage = "page"
	regionHero = "hero"

	paintFoil = "foil"
)

// layerFieldSets lists, per layer kind, the fields (besides "kind") the kind
// accepts. Any other field that is set is invalid_field. "src" is in no set:
// only the server fills it.
var layerFieldSets = map[string]map[string]bool{
	kindPaper:   fieldSet("region", "tone", "glow", "glow_opacity", "vignette", "vignette_opacity"),
	kindBlock:   fieldSet("region", "edge"),
	kindPattern: fieldSet("region", "pattern", "color", "opacity", "origin", "mask", "blend"),
	kindTexture: fieldSet("region", "texture", "opacity", "blend", "colors"),
	kindArt:     fieldSet("region", "art", "placement", "colors", "paint", "opacity", "density", "blend"),
	kindFrame:   fieldSet("region", "frame", "inset", "color", "paint", "blend"),
	kindImage:   fieldSet("region", "opacity", "blend"),
}

func fieldSet(keys ...string) map[string]bool {
	m := make(map[string]bool, len(keys))
	for _, k := range keys {
		m[k] = true
	}
	return m
}

// maxLayersPerKind caps how often each kind may appear; maxLayers caps the
// whole stack and maxBlendLayers the layers with blend != normal.
var maxLayersPerKind = map[string]int{
	kindPaper: 1, kindBlock: 1, kindPattern: 3, kindTexture: 2, kindArt: 3, kindFrame: 1, kindImage: 1,
}

const (
	maxLayers      = 7
	maxBlendLayers = 3
	maxPaletteArt  = 8
	maxFoilStops   = 5
	minFoilStops   = 3
)

var (
	layerKinds       = fieldSet(kindPaper, kindBlock, kindPattern, kindTexture, kindArt, kindFrame, kindImage)
	validRegions     = fieldSet(regionPage, regionHero)
	validEdges       = fieldSet("straight", "scallop", "wave")
	validPatterns    = fieldSet("grid", "dots", "halftone", "sunburst", "pinstripe", "gingham", "stripes")
	validOrigins     = fieldSet("center", "top", "bottom", "top_left", "top_right", "bottom_left", "bottom_right")
	validMasks       = fieldSet("none", "radial", "corner", "fade_bottom")
	validTextureV2   = fieldSet("grain", "fibers", "linen", "wood", "watercolour", "foil", "marble", "velvet")
	validBlends      = fieldSet("normal", "multiply", "screen", "soft-light", "overlay")
	validDensities   = fieldSet("sparse", "normal", "dense")
	validFrames      = fieldSet("single_hairline", "double_hairline", "double_hairline_deco_corners", "scallop")
	validHeroStyleV2 = fieldSet("full_bleed", "framed", "text_only", "spotlight", "color_block")

	validHeroOrnaments = fieldSet("none", "wreath_monogram", "sticker_numeral", "foil_numeral", "ring", "cloud", "doorway", "monogram_rule")
	validDividers      = fieldSet("none", "line", "floral", "dots", "heart", "olive_sprig", "squiggle", "deco_diamond", "wave", "sparkle_rule")
	validBadges        = fieldSet("none", "starburst_sticker", "wax_seal", "foil_seal")
	validAmpersands    = fieldSet("none", "script")

	validCardStyles  = fieldSet("none", "soft", "glass", "reply_card", "sticker", "chamfered")
	validCardBorders = fieldSet("none", "hairline", "ink", "foil", "foil_inset")
	validCardFields  = fieldSet("boxed", "underline")
	validCardButtons = fieldSet("accent", "foil")
	validMotions     = fieldSet("none", "draw_on", "pop_and_settle", "foil_sheen")

	// v1Dividers are the ornament dividers the v1 DecorationDivider renders;
	// the fallback v1 `decoration` of a v2 theme is the divider if it is one.
	v1Dividers = fieldSet("none", "line", "floral", "dots", "heart")
)

// colourTokens maps a colour token to the art index it needs (0 = a named
// palette colour). artN requires every palette to carry at least N art
// colours.
var colourTokens = map[string]int{
	"background": 0, "surface": 0, "text": 0, "muted": 0, "accent": 0,
	"accent_text": 0, "accent_ink": 0,
	"art1": 1, "art2": 2, "art3": 3, "art4": 4, "art5": 5, "art6": 6, "art7": 7, "art8": 8,
}
