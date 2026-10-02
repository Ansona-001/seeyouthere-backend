package content

import "testing"

func testFields() []Field {
	min0, max10 := 0, 10
	return []Field{
		{Key: "dietary", Type: "text", MaxLength: 50, Required: true},
		{Key: "message", Type: "textarea", MaxLength: 200},
		{Key: "guess", Type: "select", Options: []string{"boy", "girl", "surprise"}},
		{Key: "songs", Type: "multiselect", Options: []string{"rock", "pop", "jazz"}},
		{Key: "plus_one", Type: "boolean"},
		{Key: "guests", Type: "number", Min: &min0, Max: &max10},
	}
}

func TestValidateAnswers_PerType(t *testing.T) {
	fields := testFields()
	raw := []byte(`{"dietary":"vegetarian","message":"can't wait","guess":"girl","songs":["rock","pop"],"plus_one":true,"guests":3}`)
	out, err := ValidateAnswers(fields, raw, "yes")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if string(out) == "" || string(out) == "{}" {
		t.Fatalf("expected populated answers, got %s", out)
	}
}

func TestValidateAnswers_UnknownKey(t *testing.T) {
	fields := testFields()
	raw := []byte(`{"dietary":"vegetarian","not_a_field":"x"}`)
	if _, err := ValidateAnswers(fields, raw, "yes"); !hasIssueCode(err, "unknown_field") {
		t.Fatalf("expected unknown_field, got %v", err)
	}
}

func TestValidateAnswers_RequiredOnlyWhenAttending(t *testing.T) {
	fields := testFields()
	// "dietary" is required, but omitted here.
	raw := []byte(`{"message":"hi"}`)
	if _, err := ValidateAnswers(fields, raw, "yes"); !hasIssueCode(err, "required") {
		t.Fatalf("expected required for attending=yes, got %v", err)
	}
	if _, err := ValidateAnswers(fields, raw, "maybe"); !hasIssueCode(err, "required") {
		t.Fatalf("expected required for attending=maybe, got %v", err)
	}
	out, err := ValidateAnswers(fields, raw, "no")
	if err != nil {
		t.Fatalf("attending=no should never fail required checks: %v", err)
	}
	if string(out) != "{}" {
		t.Fatalf("attending=no should drop all answers, got %s", out)
	}
}

func TestValidateAnswers_SelectAndMultiselectInvalid(t *testing.T) {
	fields := testFields()
	cases := []string{
		`{"dietary":"x","guess":"unknown"}`,
		`{"dietary":"x","songs":["not-an-option"]}`,
	}
	for _, raw := range cases {
		if _, err := ValidateAnswers(fields, []byte(raw), "yes"); !hasIssueCode(err, "invalid_value") {
			t.Errorf("input %s: expected invalid_value, got %v", raw, err)
		}
	}
}

func TestValidateAnswers_NumberRange(t *testing.T) {
	fields := testFields()
	if _, err := ValidateAnswers(fields, []byte(`{"dietary":"x","guests":11}`), "yes"); !hasIssueCode(err, "out_of_range") {
		t.Fatalf("expected out_of_range, got %v", err)
	}
	if _, err := ValidateAnswers(fields, []byte(`{"dietary":"x","guests":1.5}`), "yes"); !hasIssueCode(err, "invalid_value") {
		t.Fatalf("expected invalid_value for non-integer, got %v", err)
	}
}

func TestValidateAnswers_TextTooLong(t *testing.T) {
	fields := testFields()
	long := make([]byte, 0, 60)
	for i := 0; i < 60; i++ {
		long = append(long, 'a')
	}
	raw := []byte(`{"dietary":"` + string(long) + `"}`)
	if _, err := ValidateAnswers(fields, raw, "yes"); !hasIssueCode(err, "too_long") {
		t.Fatalf("expected too_long, got %v", err)
	}
}

func TestEffectiveFields(t *testing.T) {
	occ := sampleOccasion(t)
	b := RSVPBlock{
		Fields:    []RSVPFieldRef{{Key: "dietary", Required: true}},
		Questions: []Field{{Key: "q_shirt", Label: "Shirt size", Type: "text", MaxLength: 10}},
	}
	fields := EffectiveFields(occ, b)
	if len(fields) != 2 {
		t.Fatalf("expected 2 effective fields, got %d: %+v", len(fields), fields)
	}
	if fields[0].Key != "dietary" || !fields[0].Required {
		t.Errorf("expected required dietary field, got %+v", fields[0])
	}
	if fields[1].Key != "q_shirt" {
		t.Errorf("expected host question second, got %+v", fields[1])
	}
}
