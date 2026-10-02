package content

import (
	"strings"
	"testing"
)

func rsvpWithQuestions(questions []map[string]any) map[string]any {
	return map[string]any{
		"id": "r1", "type": "rsvp", "heading": "", "body": "", "deadline_local": "",
		"capacity": nil, "max_party_size": nil, "fields": []any{}, "questions": questions,
	}
}

func TestFieldDefs_KeyFormat(t *testing.T) {
	occ := sampleOccasion(t)
	cases := []struct {
		name string
		key  string
		ok   bool
	}{
		{"valid host key", "q_shirt", true},
		{"missing prefix", "shirt", false},
		{"16 chars after prefix ok", "q_" + strings.Repeat("a", 16), true},
		{"17 chars after prefix rejected", "q_" + strings.Repeat("a", 17), false},
		{"uppercase rejected", "q_Shirt", false},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			q := []map[string]any{{"key": c.key, "label": "Q", "type": "text", "required": false, "max_length": 50, "options": []any{}, "min": nil, "max": nil, "help": ""}}
			_, err := ValidateContent(mustJSON(t, []map[string]any{rsvpWithQuestions(q)}), occ)
			if (err == nil) != c.ok {
				t.Errorf("key %q: err = %v, want ok=%v", c.key, err, c.ok)
			}
		})
	}
}

func TestFieldDefs_MaxCount(t *testing.T) {
	occ := sampleOccasion(t)
	mk := func(n int) []map[string]any {
		var out []map[string]any
		for i := 0; i < n; i++ {
			out = append(out, map[string]any{
				"key": "q_" + string(rune('a'+i)), "label": "Q", "type": "text", "required": false,
				"max_length": 50, "options": []any{}, "min": nil, "max": nil, "help": "",
			})
		}
		return out
	}
	if _, err := ValidateContent(mustJSON(t, []map[string]any{rsvpWithQuestions(mk(5))}), occ); err != nil {
		t.Errorf("5 questions should be valid: %v", err)
	}
	if _, err := ValidateContent(mustJSON(t, []map[string]any{rsvpWithQuestions(mk(6))}), occ); err == nil {
		t.Error("6 questions should be invalid")
	}
}

func TestFieldDefs_DuplicateKey(t *testing.T) {
	occ := sampleOccasion(t)
	q := []map[string]any{
		{"key": "q_a", "label": "A", "type": "text", "required": false, "max_length": 50, "options": []any{}, "min": nil, "max": nil, "help": ""},
		{"key": "q_a", "label": "B", "type": "text", "required": false, "max_length": 50, "options": []any{}, "min": nil, "max": nil, "help": ""},
	}
	_, err := ValidateContent(mustJSON(t, []map[string]any{rsvpWithQuestions(q)}), occ)
	if !hasIssueCode(err, "duplicate_key") {
		t.Fatalf("expected duplicate_key, got %v", err)
	}
}

func TestFieldDefs_SelectOptionsBounds(t *testing.T) {
	occ := sampleOccasion(t)
	one := []map[string]any{{"key": "q_a", "label": "A", "type": "select", "required": false, "max_length": 0, "options": []string{"x"}, "min": nil, "max": nil, "help": ""}}
	if _, err := ValidateContent(mustJSON(t, []map[string]any{rsvpWithQuestions(one)}), occ); err == nil {
		t.Error("1 option should be invalid")
	}
	two := []map[string]any{{"key": "q_a", "label": "A", "type": "select", "required": false, "max_length": 0, "options": []string{"x", "y"}, "min": nil, "max": nil, "help": ""}}
	if _, err := ValidateContent(mustJSON(t, []map[string]any{rsvpWithQuestions(two)}), occ); err != nil {
		t.Errorf("2 options should be valid: %v", err)
	}
}

func TestFieldDefs_NumberRangeBounds(t *testing.T) {
	occ := sampleOccasion(t)
	tooLow := -1_000_001
	q := []map[string]any{{"key": "q_a", "label": "A", "type": "number", "required": false, "max_length": 0, "options": []any{}, "min": tooLow, "max": nil, "help": ""}}
	if _, err := ValidateContent(mustJSON(t, []map[string]any{rsvpWithQuestions(q)}), occ); !hasIssueCode(err, "out_of_range") {
		t.Fatalf("expected out_of_range, got %v", err)
	}
}

func TestFieldDefs_WrongShapeForType(t *testing.T) {
	occ := sampleOccasion(t)
	q := []map[string]any{{"key": "q_a", "label": "A", "type": "boolean", "required": false, "max_length": 10, "options": []any{}, "min": nil, "max": nil, "help": ""}}
	if _, err := ValidateContent(mustJSON(t, []map[string]any{rsvpWithQuestions(q)}), occ); !hasIssueCode(err, "invalid_field") {
		t.Fatalf("expected invalid_field, got %v", err)
	}
}
