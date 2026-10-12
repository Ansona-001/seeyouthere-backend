package content

import (
	"bytes"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func readMigration(name string) ([]byte, error) {
	return os.ReadFile(filepath.Join("..", "database", "migrations", name))
}

// fuzzMaxDuration bounds one ValidateManifest + resolve pass. The real cost
// is microseconds; this only catches accidental super-linear behaviour.
const fuzzMaxDuration = 5 * time.Second

// FuzzValidateManifest feeds arbitrary bytes to the manifest validator and,
// for every accepted manifest, checks the guarantees the rest of the system
// builds on: the canonical form is a fixed point, and every resolved theme
// holds only hex colours and allowlisted enums.
//
//	go test -run xxx -fuzz FuzzValidateManifest -fuzztime 60s ./internal/content/
func FuzzValidateManifest(f *testing.F) {
	for _, s := range advSeedManifests(f) {
		f.Add(s.raw)
	}
	for _, fx := range v2Fixtures() {
		raw := advMarshal(f, fx)
		f.Add(raw)
		if m, err := ValidateManifest(raw); err == nil {
			f.Add(advMarshal(f, m)) // canonical form
		}
	}
	for _, v := range advVariants(f) {
		f.Add(v.raw)
	}
	good := string(advMarshal(f, oliveGrove()))
	for _, raw := range []string{
		``, `null`, `{}`, `[]`, `{"schema":1}`, `{"schema":2}`, `{"schema":3}`,
		`{"schema":2,"layers":[{"kind":"image","opacity":0.1,"src":"x"}]}`,
		`{"schema":2,"layers":[null]}`, `{"schema":2,"palettes":[null],"fonts":[null]}`,
		`{"schema":2,"card":{"radius":1e3}}`, `{"schema":2,"card":{"radius":9223372036854775808}}`,
		`{"schema":2,"layers":[{"kind":"paper","glow_opacity":1e999}]}`,
		strings.Repeat("[", 3000) + strings.Repeat("]", 3000),
		strings.Replace(good, `"layout":"split"`, `"LAYOUT":"split","layout":"split"`, 1),
		strings.Replace(good, `"#F3EEE3"`, `"#f3eee3"`, 1),
		strings.Replace(good, `"#F3EEE3"`, `"#F3EEE3;"`, 1),
		strings.Replace(good, `"art5"`, `"art9"`, 1),
		strings.Replace(good, `"olive_branches"`, `"olive_branches "`, 1),
		strings.Replace(good, `"double_hairline"`, `"double_hairline\u0000"`, 1),
		strings.Replace(good, `"Ivory"`, "\"\xe2\x80\xaeIvory\"", 1),
		strings.Replace(good, `"paper"`, `"PAPER"`, 1),
		strings.Replace(good, `"inset":12`, `"inset":12.5`, 1),
		strings.Replace(good, `"schema":2`, `"schema":2,"schema":1`, 1),
		good + `{"trailing":true}`,
	} {
		f.Add([]byte(raw))
	}

	f.Fuzz(func(t *testing.T, data []byte) {
		start := time.Now()
		m, err := ValidateManifest(data)
		if err != nil {
			ve, ok := err.(*ValidationError)
			if !ok {
				t.Fatalf("error is %T, want *ValidationError", err)
			}
			if n := len(ve.Issues); n == 0 || n > maxIssues {
				t.Fatalf("%d issues", n)
			}
			for _, is := range ve.Issues {
				if is.Code == "" || is.Path == "" || is.Message == "" || len(is.Message) > 200 || len(is.Path) > 200 {
					t.Fatalf("malformed issue %+v", is)
				}
			}
			return
		}
		if m.Schema != 1 && m.Schema != 2 {
			t.Fatalf("accepted schema %d", m.Schema)
		}

		canon, err := json.Marshal(m)
		if err != nil {
			t.Fatalf("marshal accepted manifest: %v", err)
		}
		if len(canon) > maxManifestCanonicalBytes {
			t.Fatalf("accepted a manifest whose canonical form is %d bytes (cap %d)", len(canon), maxManifestCanonicalBytes)
		}
		m2, err := ValidateManifest(canon)
		if err != nil {
			t.Fatalf("canonical form no longer validates: %v\n%.600s", err, canon)
		}
		canon2, err := json.Marshal(m2)
		if err != nil || !bytes.Equal(canon, canon2) {
			t.Fatalf("canonical form is not idempotent:\n%.600s\n%.600s", canon, canon2)
		}
		if _, err := ValidateStoredManifest(canon); err != nil {
			t.Fatalf("canonical form no longer validates on the stored path: %v", err)
		}

		for i, p := range m.Palettes {
			ov, err := json.Marshal(Overrides{Palette: p.ID, Font: m.Fonts[i%len(m.Fonts)].ID})
			if err != nil {
				t.Fatal(err)
			}
			th := ResolveTheme(m, ov, "/media/abc/background")
			if m.Schema == 2 {
				assertThemeSafe(t, th)
			} else if th.Engine != 0 || th.Layers != nil || th.Ornament != nil || th.Card != nil || th.Motion != "" {
				t.Fatalf("schema 1 theme carries v2 fields: %+v", th)
			}
			j, err := json.Marshal(th)
			if err != nil {
				t.Fatal(err)
			}
			advWalkTheme(t, j)
		}
		if d := time.Since(start); d > fuzzMaxDuration {
			t.Fatalf("took %v", d)
		}
	})
}
