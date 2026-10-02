package content

import (
	"encoding/json"
	"fmt"
)

// EffectiveFields returns the full set of questions a guest must answer for
// an event's rsvp block: the occasion fields it enables (with the per-event
// required override) followed by the host's own questions.
func EffectiveFields(occ Occasion, b RSVPBlock) []Field {
	out := make([]Field, 0, len(b.Fields)+len(b.Questions))
	for _, ref := range b.Fields {
		for _, f := range occ.RSVPFields {
			if f.Key == ref.Key {
				f.Required = ref.Required
				out = append(out, f)
				break
			}
		}
	}
	out = append(out, b.Questions...)
	return out
}

// ValidateAnswers decodes and validates a guest's rsvp.answers object against
// the effective fields for the event. Unknown keys are rejected. When
// attending is "no" every answer is dropped (required is not enforced) and
// the canonical result is "{}", matching the invariant that a "no" RSVP
// carries no answers.
func ValidateAnswers(fields []Field, raw json.RawMessage, attending string) ([]byte, error) {
	if attending == "no" {
		return []byte("{}"), nil
	}

	byKey := make(map[string]Field, len(fields))
	for _, f := range fields {
		byKey[f.Key] = f
	}

	var in map[string]json.RawMessage
	if len(raw) > 0 {
		if err := json.Unmarshal(raw, &in); err != nil {
			return nil, single("answers", "invalid_json", "must be an object")
		}
	}

	iss := &issues{}
	out := make(map[string]any, len(in))
	for key, rawVal := range in {
		f, ok := byKey[key]
		if !ok {
			iss.add("answers."+key, "unknown_field", "Not a field on this event.")
			continue
		}
		v, ok := validateAnswerValue(iss, "answers."+key, f, rawVal)
		if ok {
			out[key] = v
		}
	}
	for _, f := range fields {
		if f.Required {
			if _, present := in[f.Key]; !present {
				iss.add("answers."+f.Key, "required", "This field is required.")
			}
		}
	}
	if err := iss.err(); err != nil {
		return nil, err
	}

	canon, err := json.Marshal(out)
	if err != nil {
		return nil, fmt.Errorf("marshal answers: %w", err)
	}
	return canon, nil
}

func validateAnswerValue(iss *issues, path string, f Field, raw json.RawMessage) (any, bool) {
	switch f.Type {
	case "text", "textarea":
		var s string
		if err := json.Unmarshal(raw, &s); err != nil {
			iss.add(path, "invalid_value", "must be a string")
			return nil, false
		}
		s = normalizeText(s)
		maxLen := f.MaxLength
		if maxLen == 0 {
			maxLen = textDefaultLen
		}
		allowNewline := f.Type == "textarea"
		if code, msg, ok := validateText(s, maxLen, allowNewline); !ok {
			iss.add(path, code, msg)
			return nil, false
		}
		if s == "" {
			return nil, false
		}
		return s, true

	case "select":
		var s string
		if err := json.Unmarshal(raw, &s); err != nil || !contains(f.Options, s) {
			iss.add(path, "invalid_value", "must be one of the offered options")
			return nil, false
		}
		return s, true

	case "multiselect":
		var vals []string
		if err := json.Unmarshal(raw, &vals); err != nil {
			iss.add(path, "invalid_value", "must be an array of options")
			return nil, false
		}
		seen := make(map[string]bool, len(vals))
		out := make([]string, 0, len(vals))
		for _, v := range vals {
			if !contains(f.Options, v) {
				iss.add(path, "invalid_value", "must be one of the offered options")
				return nil, false
			}
			if seen[v] {
				continue
			}
			seen[v] = true
			out = append(out, v)
		}
		if len(out) == 0 {
			return nil, false
		}
		return out, true

	case "boolean":
		var b bool
		if err := json.Unmarshal(raw, &b); err != nil {
			iss.add(path, "invalid_value", "must be a boolean")
			return nil, false
		}
		return b, true

	case "number":
		var n float64
		if err := json.Unmarshal(raw, &n); err != nil || n != float64(int64(n)) {
			iss.add(path, "invalid_value", "must be an integer")
			return nil, false
		}
		v := int64(n)
		if f.Min != nil && v < int64(*f.Min) {
			iss.add(path, "out_of_range", fmt.Sprintf("must be at least %d", *f.Min))
			return nil, false
		}
		if f.Max != nil && v > int64(*f.Max) {
			iss.add(path, "out_of_range", fmt.Sprintf("must be at most %d", *f.Max))
			return nil, false
		}
		return v, true

	default:
		iss.add(path, "invalid_field", "unsupported field type")
		return nil, false
	}
}

func contains(opts []string, v string) bool {
	for _, o := range opts {
		if o == v {
			return true
		}
	}
	return false
}
