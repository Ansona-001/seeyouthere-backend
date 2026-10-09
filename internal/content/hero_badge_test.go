package content

import (
	"encoding/json"
	"errors"
	"strings"
	"testing"
)

func heroWithBadge(badge any) map[string]any {
	b := heroBlock("h1", "T")
	b["badge"] = badge
	return b
}

func TestValidateContent_HeroBadge(t *testing.T) {
	occ := sampleOccasion(t)
	tests := []struct {
		name    string
		badge   string
		wantErr string // issue code, "" = valid
	}{
		{"empty", "", ""},
		{"one digit", "6", ""},
		{"two digits", "50", ""},
		{"ampersand", "A&T", ""},
		{"middle dot", "R·J", ""},
		{"M&L", "M&L", ""},
		{"plus", "AB+C", ""},
		{"hyphen", "A-B", ""},
		{"unicode letters", "Ünï", ""},
		{"four runes", "1234", ""},
		{"five runes", "12345", "too_long"},
		{"markup", "<b>", "invalid_text"},
		{"quote", `A"B`, "invalid_text"},
		{"apostrophe", "A'B", "invalid_text"},
		{"inner space", "a b", "invalid_text"},
		{"leading space", " 6", "invalid_text"},
		{"space only", " ", "invalid_text"},
		{"NUL", "A\x00", "invalid_text"},
		{"RLO", "A\u202eB", "invalid_text"},
		{"ZWJ", "A\u200dB", "invalid_text"},
		{"emoji", "🎉", "invalid_text"},
		{"newline", "A\nB", "invalid_text"},
		{"line separator", "A B", "invalid_text"},
		{"combining mark", "é", "invalid_text"},
		{"slash", "A/B", "invalid_text"},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			raw := mustJSON(t, []map[string]any{heroWithBadge(tc.badge)})
			res, err := ValidateContent(raw, occ)
			if tc.wantErr == "" {
				if err != nil {
					t.Fatalf("unexpected error: %v", err)
				}
				var out []map[string]any
				if err := json.Unmarshal(res.JSON, &out); err != nil {
					t.Fatal(err)
				}
				got, _ := out[0]["badge"].(string)
				if got != tc.badge {
					t.Errorf("badge round trip = %q, want %q", got, tc.badge)
				}
				if again, err := ValidateContent(res.JSON, occ); err != nil || string(again.JSON) != string(res.JSON) {
					t.Errorf("not a fixed point: err=%v", err)
				}
				return
			}
			var ve *ValidationError
			if !errors.As(err, &ve) {
				t.Fatalf("err = %v, want ValidationError", err)
			}
			found := false
			for _, is := range ve.Issues {
				if is.Path == "content[0].badge" && is.Code == tc.wantErr {
					found = true
				}
			}
			if !found {
				t.Errorf("issues = %+v, want content[0].badge %s", ve.Issues, tc.wantErr)
			}
		})
	}
}

func TestValidateContent_HeroBadge_WrongType(t *testing.T) {
	raw := mustJSON(t, []map[string]any{heroWithBadge(6)})
	if _, err := ValidateContent(raw, sampleOccasion(t)); err == nil {
		t.Fatal("numeric badge should be rejected")
	}
}

// Content stored before the badge existed must validate and re-serialise
// byte-identically (no "badge" key is added).
func TestValidateContent_HeroBadge_OldContentUnchanged(t *testing.T) {
	occ := sampleOccasion(t)
	raw := mustJSON(t, []map[string]any{heroBlock("h1", "T")})
	res, err := ValidateContent(raw, occ)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(res.JSON), "badge") {
		t.Errorf("canonical output of badge-less content contains badge: %s", res.JSON)
	}
	// Explicit empty badge canonicalises to the same bytes as no badge.
	res2, err := ValidateContent(mustJSON(t, []map[string]any{heroWithBadge("")}), occ)
	if err != nil {
		t.Fatal(err)
	}
	if string(res2.JSON) != string(res.JSON) {
		t.Errorf("empty badge differs from absent:\n%s\n%s", res2.JSON, res.JSON)
	}
}
