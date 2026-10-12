package content

import (
	"bytes"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"testing"
)

// The golden file pins, for every seeded schema-1 template version, the
// canonical stored manifest and the resolved Theme for the default and for
// every palette x font override. Theme engine changes must leave all of it
// byte-identical; a deliberate change regenerates it with UPDATE_GOLDEN=1.

const themeGoldenPath = "testdata/theme_v1_golden.json"

var (
	seedTemplateRe = regexp.MustCompile(`\('(01926a00-[0-9a-f-]+)', '([a-z0-9_-]+)', '[^']*', '[^\n]*', '[a-z]+'\)`)
	seedVersionRe  = regexp.MustCompile(`\('(01926a00-[0-9a-f-]+)', (\d+),\s*'(\{[^\n]*\})',\s*\n`)
)

type seededManifest struct {
	slug    string
	version int
	raw     []byte
}

// loadSeededManifests extracts every template_versions manifest from the
// Up section of the seed migrations, in file order.
func loadSeededManifests(t *testing.T) []seededManifest {
	t.Helper()
	var out []seededManifest
	slugs := map[string]string{}
	for _, name := range []string{"00003_seed_catalog.sql", "00004_rich_blocks.sql"} {
		b, err := os.ReadFile(filepath.Join("..", "database", "migrations", name))
		if err != nil {
			t.Fatalf("read %s: %v", name, err)
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
				t.Fatalf("%s: template id %s has no seeded slug", name, m[1])
			}
			v, _ := strconv.Atoi(m[2])
			out = append(out, seededManifest{slug: slug, version: v, raw: []byte(strings.ReplaceAll(m[3], "''", "'"))})
		}
	}
	return out
}

type goldenLine struct {
	kind string // "manifest" or "theme"
	key  string
	line string
}

// computeThemeGolden returns the manifest and theme lines, each sorted by
// (template slug, version, palette, font).
func computeThemeGolden(t *testing.T) (manifests, themes []goldenLine) {
	t.Helper()
	seeds := loadSeededManifests(t)
	sort.SliceStable(seeds, func(i, j int) bool {
		if seeds[i].slug != seeds[j].slug {
			return seeds[i].slug < seeds[j].slug
		}
		return seeds[i].version < seeds[j].version
	})
	for _, s := range seeds {
		id := fmt.Sprintf("%s/v%d", s.slug, s.version)
		m, err := ValidateManifest(s.raw)
		if err != nil {
			t.Fatalf("%s: seeded manifest no longer validates: %v", id, err)
		}
		canon, err := json.Marshal(m)
		if err != nil {
			t.Fatalf("%s: marshal manifest: %v", id, err)
		}
		manifests = append(manifests, goldenLine{"manifest", id, goldenEntry(t, id, "manifest", canon)})

		pals := make([]string, 0, len(m.Palettes))
		for _, p := range m.Palettes {
			pals = append(pals, p.ID)
		}
		fonts := make([]string, 0, len(m.Fonts))
		for _, f := range m.Fonts {
			fonts = append(fonts, f.ID)
		}
		sort.Strings(pals)
		sort.Strings(fonts)

		type combo struct{ palette, font string }
		combos := []combo{{"", ""}} // the default: no overrides
		for _, p := range pals {
			for _, f := range fonts {
				combos = append(combos, combo{p, f})
			}
		}
		for _, c := range combos {
			var overrides []byte
			label := "default"
			if c.palette != "" {
				overrides, err = ValidateOverrides(mustJSON(t, Overrides{Palette: c.palette, Font: c.font}), m)
				if err != nil {
					t.Fatalf("%s %s/%s: overrides: %v", id, c.palette, c.font, err)
				}
				label = c.palette + "/" + c.font
			}
			theme, err := json.Marshal(ResolveTheme(m, overrides, ""))
			if err != nil {
				t.Fatalf("%s: marshal theme: %v", id, err)
			}
			key := id + " " + label
			themes = append(themes, goldenLine{"theme", key, goldenEntry(t, key, "theme", theme)})
		}
	}
	return manifests, themes
}

// goldenEntry renders {"key":...,"<field>":<raw>} on one line.
func goldenEntry(t *testing.T, key, field string, raw []byte) string {
	t.Helper()
	k, err := json.Marshal(key)
	if err != nil {
		t.Fatalf("marshal key: %v", err)
	}
	return fmt.Sprintf(`{"key":%s,%q:%s}`, k, field, raw)
}

func renderThemeGolden(manifests, themes []goldenLine) []byte {
	var b bytes.Buffer
	section := func(name string, ls []goldenLine, last bool) {
		fmt.Fprintf(&b, "  %q: [\n", name)
		for i, l := range ls {
			sep := ","
			if i == len(ls)-1 {
				sep = ""
			}
			fmt.Fprintf(&b, "    %s%s\n", l.line, sep)
		}
		if last {
			b.WriteString("  ]\n")
		} else {
			b.WriteString("  ],\n")
		}
	}
	b.WriteString("{\n")
	section("manifests", manifests, false)
	section("themes", themes, true)
	b.WriteString("}\n")
	return b.Bytes()
}

// indexGolden maps "<kind> <key>" to the entry line of a rendered golden
// file, failing on duplicate keys.
func indexGolden(t *testing.T, data []byte) map[string]string {
	t.Helper()
	out := map[string]string{}
	for _, raw := range strings.Split(string(data), "\n") {
		line := strings.TrimSuffix(strings.TrimSpace(raw), ",")
		if !strings.HasPrefix(line, `{"key":`) {
			continue
		}
		var h struct {
			Key string `json:"key"`
		}
		if err := json.Unmarshal([]byte(line), &h); err != nil {
			t.Fatalf("golden line unparseable: %v: %.80s", err, line)
		}
		kind := "manifest"
		if strings.Contains(line, `,"theme":`) {
			kind = "theme"
		}
		k := kind + " " + h.Key
		if _, dup := out[k]; dup {
			t.Fatalf("duplicate golden entry %q", k)
		}
		out[k] = line
	}
	return out
}

func TestThemeV1Golden(t *testing.T) {
	manifests, themes := computeThemeGolden(t)
	got := renderThemeGolden(manifests, themes)

	if os.Getenv("UPDATE_GOLDEN") == "1" {
		if err := os.MkdirAll(filepath.Dir(themeGoldenPath), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(themeGoldenPath, got, 0o644); err != nil {
			t.Fatal(err)
		}
		t.Logf("regenerated %s (%d manifests, %d themes)", themeGoldenPath, len(manifests), len(themes))
		return
	}

	wantRaw, err := os.ReadFile(themeGoldenPath)
	if err != nil {
		t.Fatalf("read golden (regenerate with UPDATE_GOLDEN=1): %v", err)
	}
	// git may check the file out with CRLF on Windows.
	want := bytes.ReplaceAll(wantRaw, []byte("\r\n"), []byte("\n"))
	if bytes.Equal(got, want) {
		return
	}

	report := diffGolden(indexGolden(t, want), indexGolden(t, got))
	t.Errorf("schema-1 theme output differs from %s; if the change is intended regenerate with UPDATE_GOLDEN=1\n%s",
		themeGoldenPath, report)
}

// diffGolden describes entries missing from, extra to, or changed versus the
// golden. It returns a non-empty string whenever the maps differ.
func diffGolden(want, got map[string]string) string {
	var missing, extra, changed []string
	for k, w := range want {
		g, ok := got[k]
		switch {
		case !ok:
			missing = append(missing, k)
		case g != w:
			changed = append(changed, fmt.Sprintf("%s\n    golden: %s\n    actual: %s", k, w, g))
		}
	}
	for k := range got {
		if _, ok := want[k]; !ok {
			extra = append(extra, k)
		}
	}
	sort.Strings(missing)
	sort.Strings(extra)
	sort.Strings(changed)
	return fmt.Sprintf("  in golden but no longer produced (%d): %v\n  produced but not in golden (%d): %v\n  output changed (%d):\n  %s",
		len(missing), missing, len(extra), extra, len(changed), strings.Join(changed, "\n  "))
}

// TestThemeV1Golden_Coverage guards the harness itself: every seeded
// version and every palette x font combination must be generated, so the
// golden cannot silently shrink.
func TestThemeV1Golden_Coverage(t *testing.T) {
	seeds := loadSeededManifests(t)
	// 00003 seeds five templates at v1; 00004 adds heirloom v1 and v2 of four.
	if len(seeds) != 10 {
		t.Fatalf("found %d seeded template versions, want 10", len(seeds))
	}
	manifests, themes := computeThemeGolden(t)
	if len(manifests) != len(seeds) {
		t.Fatalf("got %d manifest entries, want %d", len(manifests), len(seeds))
	}
	want := 0
	for _, s := range seeds {
		m, err := ValidateManifest(s.raw)
		if err != nil {
			t.Fatal(err)
		}
		want += 1 + len(m.Palettes)*len(m.Fonts)
	}
	if len(themes) != want {
		t.Fatalf("got %d theme entries, want %d", len(themes), want)
	}
}

// TestThemeV1Golden_DetectsDrift proves the comparison reports missing,
// extra and changed entries rather than passing vacuously.
func TestThemeV1Golden_DetectsDrift(t *testing.T) {
	manifests, themes := computeThemeGolden(t)
	full := indexGolden(t, renderThemeGolden(manifests, themes))

	cases := []struct {
		name   string
		mutate func(m, th []goldenLine) ([]goldenLine, []goldenLine)
		want   string
	}{
		{"missing theme", func(m, th []goldenLine) ([]goldenLine, []goldenLine) { return m, th[1:] }, "in golden but no longer produced (1)"},
		{"extra theme", func(m, th []goldenLine) ([]goldenLine, []goldenLine) {
			extra := goldenLine{"theme", "zz/v9 default", `{"key":"zz/v9 default","theme":{}}`}
			return m, append(append([]goldenLine{}, th...), extra)
		}, "produced but not in golden (1)"},
		{"missing manifest", func(m, th []goldenLine) ([]goldenLine, []goldenLine) { return m[1:], th }, "in golden but no longer produced (1)"},
		{"changed theme", func(m, th []goldenLine) ([]goldenLine, []goldenLine) {
			c := append([]goldenLine{}, th...)
			c[0].line = strings.Replace(c[0].line, `"layout":"`, `"layout":"x`, 1)
			return m, c
		}, "output changed (1)"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			m, th := tc.mutate(manifests, themes)
			report := diffGolden(full, indexGolden(t, renderThemeGolden(m, th)))
			if !strings.Contains(report, tc.want) {
				t.Fatalf("report does not flag drift %q:\n%s", tc.want, report)
			}
		})
	}
}
