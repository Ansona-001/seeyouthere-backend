package content

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"regexp"
)

// Field describes one RSVP question: either a built-in occasion field
// (occasion.rsvp_fields) or a host-defined question (rsvp.questions).
type Field struct {
	Key       string   `json:"key"`
	Label     string   `json:"label"`
	Type      string   `json:"type"`
	Required  bool     `json:"required"`
	MaxLength int      `json:"max_length,omitempty"`
	Options   []string `json:"options,omitempty"`
	Min       *int     `json:"min,omitempty"`
	Max       *int     `json:"max,omitempty"`
	Help      string   `json:"help,omitempty"`
}

const (
	fieldLabelMax   = 120
	fieldHelpMax    = 200
	fieldOptionMax  = 80
	fieldNumberAbs  = 1_000_000
	textMaxLenMax   = 2000
	textDefaultLen  = 200
	textareaDefault = 1000
)

var fieldTypes = map[string]bool{
	"text": true, "textarea": true, "select": true, "multiselect": true, "boolean": true, "number": true,
}

// occasionFieldKeyRe matches occasion-defined field keys (dietary, message, ...).
var occasionFieldKeyRe = regexp.MustCompile(`^[a-z][a-z0-9_]{0,31}$`)

// hostQuestionKeyRe matches host-defined RSVP question keys (§3.1 rsvp.questions).
var hostQuestionKeyRe = regexp.MustCompile(`^q_[a-z0-9_]{1,16}$`)

// parseFieldDefs decodes and validates a JSON array of Field definitions,
// enforcing the key pattern, per-type shape and limits from §3.2. maxCount
// bounds the number of fields (rsvp.questions allows 0..5; occasion
// rsvp_fields has no separate cap but content size limits still apply).
func parseFieldDefs(raw json.RawMessage, path string, keyRe *regexp.Regexp, maxCount int) ([]Field, error) {
	if len(raw) == 0 {
		return nil, nil
	}
	var in []json.RawMessage
	if err := strictUnmarshal(raw, &in); err != nil {
		return nil, single(path, "invalid_json", "must be an array")
	}
	if len(in) > maxCount {
		return nil, single(path, "too_many", fmt.Sprintf("at most %d allowed", maxCount))
	}

	iss := &issues{}
	seen := make(map[string]bool, len(in))
	out := make([]Field, 0, len(in))
	for i, rawField := range in {
		fp := fmt.Sprintf("%s[%d]", path, i)
		var f Field
		if err := strictUnmarshal(rawField, &f); err != nil {
			iss.add(fp, "invalid_json", "must be a field definition object")
			continue
		}
		if !keyRe.MatchString(f.Key) {
			iss.add(fp+".key", "invalid_key", "Invalid field key.")
			continue
		}
		if seen[f.Key] {
			iss.add(fp+".key", "duplicate_key", "Field keys must be unique.")
			continue
		}
		seen[f.Key] = true

		f.Label = checkField(iss, fp+".label", f.Label, fieldLabelMax, true, false)
		f.Help = checkField(iss, fp+".help", f.Help, fieldHelpMax, false, false)

		if !fieldTypes[f.Type] {
			iss.add(fp+".type", "invalid_type", "Unknown field type.")
			continue
		}
		validateFieldShape(iss, fp, &f)
		out = append(out, f)
	}
	if err := iss.err(); err != nil {
		return nil, err
	}
	return out, nil
}

// validateFieldShape enforces the type-specific fields and rejects
// fields that don't belong to the field's type (e.g. options on a number field).
func validateFieldShape(iss *issues, path string, f *Field) {
	switch f.Type {
	case "text", "textarea":
		if len(f.Options) > 0 || f.Min != nil || f.Max != nil {
			iss.add(path, "invalid_field", "options/min/max aren't valid for this type.")
			return
		}
		def := textDefaultLen
		if f.Type == "textarea" {
			def = textareaDefault
		}
		if f.MaxLength == 0 {
			f.MaxLength = def
		}
		if f.MaxLength < 1 || f.MaxLength > textMaxLenMax {
			iss.add(path+".max_length", "out_of_range", fmt.Sprintf("must be between 1 and %d", textMaxLenMax))
		}
	case "select", "multiselect":
		if f.MaxLength != 0 || f.Min != nil || f.Max != nil {
			iss.add(path, "invalid_field", "max_length/min/max aren't valid for this type.")
			return
		}
		validateOptions(iss, path+".options", f.Options)
	case "boolean":
		if f.MaxLength != 0 || len(f.Options) > 0 || f.Min != nil || f.Max != nil {
			iss.add(path, "invalid_field", "max_length/options/min/max aren't valid for this type.")
		}
	case "number":
		if f.MaxLength != 0 || len(f.Options) > 0 {
			iss.add(path, "invalid_field", "max_length/options aren't valid for this type.")
			return
		}
		if f.Min != nil && (*f.Min < -fieldNumberAbs || *f.Min > fieldNumberAbs) {
			iss.add(path+".min", "out_of_range", "must be within +/-1,000,000")
		}
		if f.Max != nil && (*f.Max < -fieldNumberAbs || *f.Max > fieldNumberAbs) {
			iss.add(path+".max", "out_of_range", "must be within +/-1,000,000")
		}
		if f.Min != nil && f.Max != nil && *f.Min > *f.Max {
			iss.add(path+".max", "out_of_range", "must be greater than or equal to min")
		}
	}
}

func validateOptions(iss *issues, path string, opts []string) {
	if len(opts) < 2 || len(opts) > 20 {
		iss.add(path, "out_of_range", "must have between 2 and 20 options")
		return
	}
	seen := make(map[string]bool, len(opts))
	for i, o := range opts {
		norm := normalizeText(o)
		if norm == "" {
			iss.add(fmt.Sprintf("%s[%d]", path, i), "required", "Option text is required.")
			continue
		}
		if code, msg, ok := validateText(norm, fieldOptionMax, false); !ok {
			iss.add(fmt.Sprintf("%s[%d]", path, i), code, msg)
			continue
		}
		if seen[norm] {
			iss.add(fmt.Sprintf("%s[%d]", path, i), "duplicate_option", "Options must be unique.")
			continue
		}
		seen[norm] = true
		opts[i] = norm
	}
}

// strictUnmarshal decodes data into dst, rejecting unknown JSON fields and
// any data after the first JSON value.
func strictUnmarshal(data []byte, dst any) error {
	dec := json.NewDecoder(bytes.NewReader(data))
	dec.DisallowUnknownFields()
	if err := dec.Decode(dst); err != nil {
		return err
	}
	var extra json.RawMessage
	if err := dec.Decode(&extra); !errors.Is(err, io.EOF) {
		return errors.New("unexpected data after the JSON value")
	}
	return nil
}
