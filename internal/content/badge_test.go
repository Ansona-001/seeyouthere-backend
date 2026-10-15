package content

import (
	"encoding/json"
	"testing"
)

func TestBadgeFromAnswers(t *testing.T) {
	tests := []struct {
		name    string
		answers map[string]string
		want    string
	}{
		{"couple", map[string]string{"name_1": "alex", "name_2": "Sam"}, "A&S"},
		{"accented and non-Latin initials", map[string]string{"name_1": " élodie", "name_2": "Дмитрий"}, "É&Д"},
		{"single host", map[string]string{"host": "sam & co"}, "S"},
		{"one of two names is not enough", map[string]string{"name_1": "Alex"}, ""},
		{"birthday name does not replace a numeral", map[string]string{"name": "Sam"}, ""},
		{"punctuation only", map[string]string{"host": "!!"}, ""},
		{"no answers", nil, ""},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			if got := badgeFromAnswers(tc.answers); got != tc.want {
				t.Errorf("badgeFromAnswers = %q, want %q", got, tc.want)
			}
		})
	}
}

func TestBuildInitialContent_HeroBadge(t *testing.T) {
	build := func(defaultBadge string, answers map[string]string) string {
		t.Helper()
		occ := sampleOccasion(t)
		var blocks []map[string]any
		if err := json.Unmarshal(occ.DefaultBlocks, &blocks); err != nil {
			t.Fatal(err)
		}
		for _, b := range blocks {
			if b["type"] == "hero" && defaultBadge != "" {
				b["badge"] = defaultBadge
			}
		}
		occ.DefaultBlocks = mustJSON(t, blocks)
		raw, err := BuildInitialContent(occ, answers)
		if err != nil {
			t.Fatalf("BuildInitialContent: %v", err)
		}
		var out []map[string]any
		if err := json.Unmarshal(raw, &out); err != nil {
			t.Fatal(err)
		}
		for _, b := range out {
			if b["type"] == "hero" {
				got, _ := b["badge"].(string)
				return got
			}
		}
		t.Fatal("no hero block")
		return ""
	}

	names := map[string]string{"name_1": "Ada", "name_2": "Grace"}
	if got := build("A&S", names); got != "A&G" {
		t.Errorf("badge = %q, want A&G (initials from the names)", got)
	}
	if got := build("", names); got != "" {
		t.Errorf("badge = %q, want none when the occasion has no default badge", got)
	}
}
