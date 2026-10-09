package content

import (
	"encoding/json"
	"os"
	"path/filepath"
	"testing"
)

// TestDumpResolvedThemes writes the resolved Theme JSON of every launch theme x
// palette (first font) to $DUMP_RESOLVED_DIR/<slug>--<palette>.json so the
// frontend can render real resolver output during visual QA. Skipped unless
// the variable is set.
func TestDumpResolvedThemes(t *testing.T) {
	dir := os.Getenv("DUMP_RESOLVED_DIR")
	if dir == "" {
		t.Skip("DUMP_RESOLVED_DIR not set")
	}
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatalf("mkdir: %v", err)
	}
	seeds := loadV2Seeds(t)
	for _, slug := range v2SeedSlugs {
		m, err := ValidateManifest(seeds[slug])
		if err != nil {
			t.Fatalf("%s: ValidateManifest: %v", slug, err)
		}
		for _, p := range m.Palettes {
			ov, err := json.Marshal(Overrides{Palette: p.ID, Font: m.Fonts[0].ID})
			if err != nil {
				t.Fatalf("marshal overrides: %v", err)
			}
			out, err := json.MarshalIndent(ResolveTheme(m, ov, ""), "", "  ")
			if err != nil {
				t.Fatalf("%s/%s: marshal theme: %v", slug, p.ID, err)
			}
			if err := os.WriteFile(filepath.Join(dir, slug+"--"+p.ID+".json"), out, 0o644); err != nil {
				t.Fatalf("write: %v", err)
			}
		}
	}
}
