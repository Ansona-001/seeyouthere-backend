package content

import (
	"bytes"
	"encoding/json"
	"fmt"
	"reflect"
	"strings"
	"testing"
)

// layerContrastBase is a one-palette schema-2 manifest on a white page whose
// art ramp is [black, white], so layer contrast rules can be hit exactly.
// surface lets a case make the surface darker than the background.
func layerContrastBase(surface string, layers ...obj) func() obj {
	return func() obj {
		m := oliveGrove()
		p := v2Palette("plain", "Plain", [6]string{"#FFFFFF", surface, "#000000", "#595959", "#000000", "#FFFFFF"},
			[]string{"#000000", "#FFFFFF"}, goldFoil)
		p["accent_ink"] = "#595959"
		m["palettes"] = []obj{p}
		m["defaults"] = obj{"palette": "plain", "font": "bodoni"}
		setLayers(m, layers...)
		return m
	}
}

func TestManifestV2_PaperTone(t *testing.T) {
	for _, tc := range []struct {
		tone string
		code string // "" = accepted
	}{
		{"", ""}, {"background", ""}, {"surface", ""},
		{"accent", "invalid_value"}, {"text", "invalid_value"}, {"accent_text", "invalid_value"},
		{"accent_ink", "invalid_value"}, {"muted", "invalid_value"}, {"art1", "invalid_value"},
		{"bogus", "unknown_token"}, {"#FFFFFF", "unknown_token"},
	} {
		t.Run("tone="+tc.tone, func(t *testing.T) {
			l := obj{"kind": "paper"}
			if tc.tone != "" {
				l["tone"] = tc.tone
			}
			_, err := ValidateManifest(mutated(t, layerContrastBase("#FFFFFF", l)(), func(obj) {}))
			if tc.code == "" {
				if err != nil {
					t.Fatalf("want accepted, got %v", err)
				}
				return
			}
			if !hasIssue(err, tc.code, "layers[0].tone") {
				t.Fatalf("want %s at layers[0].tone, got %v", tc.code, err)
			}
		})
	}
}

func TestManifestV2_LayerContrast(t *testing.T) {
	paper := func(extra obj) obj {
		l := obj{"kind": "paper"}
		for k, v := range extra {
			l[k] = v
		}
		return l
	}
	pattern := func(color string, op float64, region string) obj {
		l := obj{"kind": "pattern", "pattern": "dots", "color": color, "opacity": op}
		if region != "" {
			l["region"] = region
		}
		return l
	}
	cases := []struct {
		name    string
		surface string
		layers  []obj
		path    string // "" = accepted; else a low_contrast issue at this path
	}{
		// Glow and vignette are blended over both background and surface.
		{"glow white at full opacity", "#FFFFFF", []obj{paper(obj{"glow": "art2"})}, ""},
		{"glow black faint", "#FFFFFF", []obj{paper(obj{"glow": "art1", "glow_opacity": 0.1})}, ""},
		{"glow black strong", "#FFFFFF", []obj{paper(obj{"glow": "art1", "glow_opacity": 0.3})}, "layers[0].glow"},
		{"glow black default opacity 1", "#FFFFFF", []obj{paper(obj{"glow": "art1"})}, "layers[0].glow"},
		{"vignette black faint", "#FFFFFF", []obj{paper(obj{"vignette": "art1", "vignette_opacity": 0.1})}, ""},
		{"vignette black strong", "#FFFFFF", []obj{paper(obj{"vignette": "art1", "vignette_opacity": 0.3})}, "layers[0].vignette"},
		{"glow ok on background, fails on a darker surface", "#D9D9D9", []obj{paper(obj{"glow": "art1", "glow_opacity": 0.1})}, "layers[0].glow"},
		{"vignette checked against the surface even when tone is background", "#D9D9D9", []obj{paper(obj{"tone": "background", "vignette": "art1", "vignette_opacity": 0.1})}, "layers[0].vignette"},
		{"glow zero opacity is invisible", "#D9D9D9", []obj{paper(obj{"glow": "art1", "glow_opacity": 0})}, ""},
		// Patterns: the colour fully covering, scaled by its opacity.
		{"pattern white strong", "#FFFFFF", []obj{paper(nil), pattern("art2", 0.35, "")}, ""},
		{"pattern black faint", "#FFFFFF", []obj{paper(nil), pattern("art1", 0.1, "")}, ""},
		{"pattern black strong", "#FFFFFF", []obj{paper(nil), pattern("art1", 0.35, "")}, "layers[1].color"},
		{"pattern black strong in the hero", "#FFFFFF", []obj{paper(nil), pattern("art1", 0.35, "hero")}, "layers[1].color"},
		{"pattern black faint, fails on a darker surface", "#D9D9D9", []obj{paper(nil), pattern("art1", 0.1, "")}, "layers[1].color"},
		{"pattern text colour at the minimum opacity", "#FFFFFF", []obj{paper(nil), pattern("text", 0.01, "")}, ""},
		// Image layers are bounded by their opacity cap instead.
		{"image at the cap", "#FFFFFF", []obj{paper(nil), {"kind": "image", "opacity": 0.5}}, ""},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			_, err := ValidateManifest(mutated(t, layerContrastBase(c.surface, c.layers...)(), func(obj) {}))
			if c.path == "" {
				if err != nil {
					t.Fatalf("want accepted, got %v", err)
				}
				return
			}
			if !hasIssue(err, "low_contrast", c.path) {
				t.Fatalf("want low_contrast at %s, got %v", c.path, err)
			}
			if len(err.Error()) > 400 {
				t.Errorf("error is not short: %d bytes", len(err.Error()))
			}
		})
	}
}

func TestManifestV2_ImageOpacityCap(t *testing.T) {
	for _, tc := range []struct {
		op   float64
		code string
	}{
		{0.05, ""}, {0.5, ""}, {0.501, "out_of_range"}, {0.51, "out_of_range"}, {1, "out_of_range"}, {0.049, "out_of_range"},
	} {
		t.Run(fmt.Sprint(tc.op), func(t *testing.T) {
			base := layerContrastBase("#FFFFFF", obj{"kind": "paper"}, obj{"kind": "image", "opacity": tc.op})
			_, err := ValidateManifest(mutated(t, base(), func(obj) {}))
			if tc.code == "" {
				if err != nil {
					t.Fatalf("want accepted, got %v", err)
				}
				return
			}
			if !hasIssue(err, tc.code, "layers[1].opacity") {
				t.Fatalf("want %s at layers[1].opacity, got %v", tc.code, err)
			}
		})
	}
}

func TestManifestV2_OpacityRounding(t *testing.T) {
	base := layerContrastBase("#FFFFFF",
		obj{"kind": "paper", "glow": "art2", "glow_opacity": 0.1234567, "vignette": "art2", "vignette_opacity": 1e-7},
		obj{"kind": "texture", "texture": "grain", "opacity": 0.0200004},
		obj{"kind": "image", "opacity": 0.1234999},
	)
	m, err := ValidateManifest(mutated(t, base(), func(obj) {}))
	if err != nil {
		t.Fatal(err)
	}
	canon := advMarshal(t, m)
	for _, bad := range []string{"e-", "E-", "e+", "0.1234567", "0.0200004"} {
		if bytes.Contains(canon, []byte(bad)) {
			t.Errorf("canonical form contains %q: %s", bad, canon)
		}
	}
	if got := *m.Layers[0].GlowOpacity; got != 0.123 {
		t.Errorf("glow_opacity = %v, want 0.123", got)
	}
	if got := *m.Layers[0].VignetteOpacity; got != 0 {
		t.Errorf("vignette_opacity = %v, want 0", got)
	}
	// Bounds are checked on the unrounded value first: 0.0199999 is below
	// the texture minimum even though it would round up to it.
	bad := layerContrastBase("#FFFFFF", obj{"kind": "paper"}, obj{"kind": "texture", "texture": "grain", "opacity": 0.0199999})
	if _, err := ValidateManifest(mutated(t, bad(), func(obj) {})); !hasIssue(err, "out_of_range", "layers[1].opacity") {
		t.Errorf("0.0199999 texture opacity: want out_of_range, got %v", err)
	}
	// And the canonical form is a fixed point.
	m2, err := ValidateManifest(canon)
	if err != nil || !bytes.Equal(canon, advMarshal(t, m2)) {
		t.Errorf("canonical form is not idempotent: %v", err)
	}
}

func TestManifestV2_Names(t *testing.T) {
	for _, tc := range []struct {
		name string
		val  string
		code string // "" = accepted
	}{
		{"plain", "Bodoni and Montserrat", ""},
		{"40 runes", strings.Repeat("é", 40), ""},
		{"41 runes", strings.Repeat("a", 41), "invalid_length"},
		{"empty", "", "invalid_length"},
		{"whitespace only", "   ", "invalid_value"},
		{"leading space", " Pop", "invalid_value"},
		{"trailing space", "Pop ", "invalid_value"},
		{"trailing newline", "Pop\n", "invalid_value"},
		{"line separator U+2028", "Pop Two", "invalid_value"},
		{"paragraph separator U+2029", "Pop Two", "invalid_value"},
		{"NUL", "Pop\x00", "invalid_value"},
		{"tab inside", "Pop\tTwo", "invalid_value"},
		{"bidi override", "\xe2\x80\xaePop", "invalid_value"},
		{"zero width space", "Po\xe2\x80\x8bp", "invalid_value"},
		{"inner space is fine", "Pop Two", ""},
	} {
		for _, target := range []struct{ label, path string }{{"font", "fonts[0].name"}, {"palette", "palettes[0].name"}} {
			t.Run(target.label+"/"+tc.name, func(t *testing.T) {
				_, err := ValidateManifest(mutated(t, oliveGrove(), func(m obj) {
					if target.label == "font" {
						m["fonts"].([]any)[0].(obj)["name"] = tc.val
					} else {
						palAt(m, 0)["name"] = tc.val
					}
				}))
				if tc.code == "" {
					if err != nil {
						t.Fatalf("want accepted, got %v", err)
					}
					return
				}
				if !hasIssue(err, tc.code, target.path) {
					t.Fatalf("want %s at %s, got %v", tc.code, target.path, err)
				}
			})
		}
	}

	// Schema 1 is unchanged: long and padded font names are still accepted.
	v1 := validManifestMap()
	v1["fonts"] = []map[string]any{{"id": "classic", "name": "  " + strings.Repeat("x", 500) + " ", "heading": "playfair_display", "body": "lora"}}
	if _, err := ValidateManifest(mustJSON(t, v1)); err != nil {
		t.Errorf("schema 1 font name rule changed: %v", err)
	}
}

func TestValidateManifest_RejectsTrailingDataAndBadUTF8(t *testing.T) {
	v1 := mustJSON(t, validManifestMap())
	v2 := mustJSON(t, oliveGrove())
	for name, good := range map[string][]byte{"v1": v1, "v2": v2} {
		t.Run(name+"/trailing whitespace is fine", func(t *testing.T) {
			if _, err := ValidateManifest(append(append([]byte{}, good...), " \n\t "...)); err != nil {
				t.Fatal(err)
			}
		})
		for _, tail := range []string{`{}`, ` {"a":1}`, `x`, `,`, `[]`, `null`, "\x00"} {
			t.Run(fmt.Sprintf("%s/trailing %q", name, tail), func(t *testing.T) {
				_, err := ValidateManifest(append(append([]byte{}, good...), tail...))
				if !hasIssue(err, "invalid_json", "manifest") {
					t.Fatalf("want invalid_json, got %v", err)
				}
			})
		}
		t.Run(name+"/invalid UTF-8", func(t *testing.T) {
			raw := bytes.Replace(good, []byte(`"Ivory"`), []byte("\"Iv\xffory\""), 1)
			if bytes.Equal(raw, good) {
				raw = bytes.Replace(good, []byte(`"Classic"`), []byte("\"Cl\xffassic\""), 1)
			}
			if bytes.Equal(raw, good) {
				t.Fatal("fixture lacks the name to corrupt")
			}
			if _, err := ValidateManifest(raw); !hasIssue(err, "invalid_json", "manifest") {
				t.Fatalf("want invalid_json, got %v", err)
			}
			if _, err := ValidateStoredManifest(raw); !hasIssue(err, "invalid_json", "manifest") {
				t.Fatalf("stored: want invalid_json, got %v", err)
			}
		})
	}

	m, err := ValidateManifest(v2)
	if err != nil {
		t.Fatal(err)
	}
	for _, raw := range []string{`{"palette":"ivory"} {"font":"bodoni"}`, `{"palette":"ivory"}x`, `{"palette":"ivory"}]`} {
		if _, err := ValidateOverrides([]byte(raw), m); !hasIssue(err, "invalid_json", "overrides") {
			t.Errorf("overrides %q: want invalid_json, got %v", raw, err)
		}
	}
	if _, err := ValidateOverrides([]byte(`{"palette":"ivory"}  `), m); err != nil {
		t.Errorf("trailing whitespace in overrides: %v", err)
	}
}

// padCanonical returns a schema-1 manifest whose canonical form is exactly n
// bytes (font names are unbounded in schema 1, so it can be sized exactly).
func padCanonical(t *testing.T, n int) []byte {
	t.Helper()
	m, err := ValidateManifest(advMarshal(t, validManifestMap()))
	if err != nil {
		t.Fatal(err)
	}
	var canonObj obj
	if err := json.Unmarshal(advMarshal(t, m), &canonObj); err != nil {
		t.Fatal(err)
	}
	return advPadTo(t, canonObj, n)
}

func TestValidateManifest_CanonicalSizeBoundary(t *testing.T) {
	t.Run("exactly 20 KiB accepted and idempotent", func(t *testing.T) {
		raw := padCanonical(t, maxManifestCanonicalBytes)
		m, err := ValidateManifest(raw)
		if err != nil {
			t.Fatalf("want accepted, got %v", err)
		}
		canon := advMarshal(t, m)
		if len(canon) != len(raw) {
			t.Fatalf("canonical form is %d bytes, input %d", len(canon), len(raw))
		}
		m2, err := ValidateManifest(canon)
		if err != nil || !bytes.Equal(canon, advMarshal(t, m2)) {
			t.Fatalf("not idempotent: %v", err)
		}
		// What PostgreSQL hands back loads through both paths.
		stored := pgJSONB(canon)
		if _, err := ValidateStoredManifest(stored); err != nil {
			t.Errorf("stored read: %v", err)
		}
	})
	t.Run("20 KiB + 1 rejected", func(t *testing.T) {
		_, err := ValidateManifest(padCanonical(t, maxManifestCanonicalBytes+1))
		if !hasIssue(err, "too_large", "manifest") {
			t.Fatalf("want too_large, got %v", err)
		}
		if n := len(err.(*ValidationError).Issues); n != 1 {
			t.Errorf("%d issues, want 1", n)
		}
	})
	t.Run("stored read path is looser", func(t *testing.T) {
		big := padCanonical(t, maxManifestCanonicalBytes+1)
		if _, err := ValidateStoredManifest(big); err != nil {
			t.Errorf("stored read of a 20 KiB + 1 manifest: %v", err)
		}
		spaced := append(advMarshal(t, oliveGrove()), bytes.Repeat([]byte(" "), 30<<10)...)
		if _, err := ValidateManifest(spaced); !hasIssue(err, "too_large", "manifest") {
			t.Errorf("write path must cap raw input at 24 KiB, got %v", err)
		}
		if _, err := ValidateStoredManifest(spaced); err != nil {
			t.Errorf("stored read must allow jsonb-sized text: %v", err)
		}
		over := bytes.Repeat([]byte(" "), maxStoredBytes+1)
		if _, err := ValidateStoredManifest(over); !hasIssue(err, "too_large", "manifest") {
			t.Errorf("stored read cap: got %v", err)
		}
	})
}

// layerFieldNames is the allowlist gate for per-kind field checks; a Layer
// field it forgets would silently bypass the invalid_field check.
func TestLayerFieldNamesCoversEveryLayerField(t *testing.T) {
	var l Layer
	rv := reflect.ValueOf(&l).Elem()
	want := map[string]bool{}
	for i := 0; i < rv.NumField(); i++ {
		f := rv.Type().Field(i)
		name := strings.Split(f.Tag.Get("json"), ",")[0]
		if name == "" || name == "-" {
			t.Fatalf("field %s has no json name", f.Name)
		}
		if name == "kind" {
			continue
		}
		want[name] = true
		fv := rv.Field(i)
		switch f.Type.Kind() {
		case reflect.String:
			fv.SetString("x")
		case reflect.Slice:
			fv.Set(reflect.MakeSlice(f.Type, 1, 1))
		case reflect.Ptr:
			fv.Set(reflect.New(f.Type.Elem()))
		default:
			t.Fatalf("field %s has unsupported kind %s: extend layerFieldNames and this test", f.Name, f.Type.Kind())
		}
	}
	got := map[string]bool{}
	for _, n := range layerFieldNames(&l) {
		got[n] = true
	}
	for n := range want {
		if !got[n] {
			t.Errorf("layerFieldNames misses %q", n)
		}
	}
	for n := range got {
		if !want[n] {
			t.Errorf("layerFieldNames reports unknown field %q", n)
		}
	}
	// Every field of every kind's allowed set must be a real field too.
	for kind, set := range layerFieldSets {
		for n := range set {
			if !want[n] {
				t.Errorf("layerFieldSets[%s] allows unknown field %q", kind, n)
			}
		}
	}
}

func TestV2SeedThemes_StoredRoundTrip(t *testing.T) {
	for slug, raw := range loadV2Seeds(t) {
		t.Run(slug, func(t *testing.T) {
			m, err := ValidateManifest(raw)
			if err != nil {
				t.Fatal(err)
			}
			canon := advMarshal(t, m)
			if len(canon) > maxManifestCanonicalBytes {
				t.Fatalf("canonical form is %d bytes, cap %d", len(canon), maxManifestCanonicalBytes)
			}
			stored := pgJSONB(canon)
			ms, err := ValidateStoredManifest(stored)
			if err != nil {
				t.Fatalf("stored read: %v", err)
			}
			if !bytes.Equal(canon, advMarshal(t, ms)) {
				t.Error("canonical form changed through the jsonb text round trip")
			}
			// Written again from the stored text it is the same manifest.
			if m2, err := ValidateManifest(stored); err != nil || !bytes.Equal(canon, advMarshal(t, m2)) {
				t.Errorf("write path on stored text: %v", err)
			}
		})
	}
}
