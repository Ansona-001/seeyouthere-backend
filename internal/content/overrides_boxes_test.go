package content

import "testing"

func TestOverridesBoxes(t *testing.T) {
	v1, err := ValidateManifest(mustJSON(t, validManifestMap()))
	if err != nil {
		t.Fatalf("v1 manifest: %v", err)
	}
	v2, err := ValidateManifest(mustJSON(t, oliveGrove()))
	if err != nil {
		t.Fatalf("v2 manifest: %v", err)
	}

	t.Run("off is accepted for a schema-2 template and kept in the canonical form", func(t *testing.T) {
		canon, err := ValidateOverrides([]byte(`{"palette":"","font":"","boxes":"off"}`), v2)
		if err != nil {
			t.Fatalf("ValidateOverrides: %v", err)
		}
		if th := ResolveTheme(v2, canon, ""); th.Card == nil || th.Card.Style != "open" || th.Card.Border != "none" {
			t.Errorf("card = %+v, want open/none", th.Card)
		}
	})

	t.Run("default and absent keep the template's own card", func(t *testing.T) {
		for _, raw := range []string{`{"palette":"","font":"","boxes":""}`, `{"palette":"","font":""}`, ``} {
			canon, err := ValidateOverrides([]byte(raw), v2)
			if err != nil {
				t.Fatalf("ValidateOverrides(%q): %v", raw, err)
			}
			th := ResolveTheme(v2, canon, "")
			if th.Card == nil || th.Card.Style == "open" {
				t.Errorf("%q: card = %+v, want the template's style", raw, th.Card)
			}
		}
	})

	t.Run("other values and schema-1 templates are rejected", func(t *testing.T) {
		if _, err := ValidateOverrides([]byte(`{"boxes":"on"}`), v2); !hasIssue(err, "invalid_value", "overrides.boxes") {
			t.Errorf("boxes=on: err = %v", err)
		}
		if _, err := ValidateOverrides([]byte(`{"boxes":"off"}`), v1); !hasIssue(err, "invalid_value", "overrides.boxes") {
			t.Errorf("schema 1 off: err = %v", err)
		}
	})
}
