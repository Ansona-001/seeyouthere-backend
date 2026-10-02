package content

import (
	"encoding/json"
	"fmt"
	"strings"

	"github.com/ansonarose/seeyouthere-backend/internal/store"
)

// Occasion is a catalog occasion's parsed, validated definition: the shape
// used to build new drafts and to validate/render events of that occasion.
type Occasion struct {
	Slug           string
	DefaultBlocks  json.RawMessage
	OptionalBlocks []string
	RSVPFields     []Field
	Copy           Copy
	SetupQuestions []SetupQuestion
}

type Copy struct {
	TitleTemplate string `json:"title_template"`
	Tagline       string `json:"tagline"`
	RSVPHeading   string `json:"rsvp_heading"`
	RSVPBody      string `json:"rsvp_body"`
	ShareMessage  string `json:"share_message"`
}

// setupQuestionTargets is where a setup answer is written: either into the
// title template (title_var) or directly onto a field of a default block.
var setupQuestionTargets = map[string]bool{
	"title_var": true, "datetime.start_local": true, "datetime.timezone": true,
	"location.name": true, "location.address": true, "hero.subtitle": true,
}

var setupQuestionTypes = map[string]bool{"text": true, "date_time": true, "timezone": true, "place": true}

type SetupQuestion struct {
	Key       string `json:"key"`
	Label     string `json:"label"`
	Type      string `json:"type"`
	Required  bool   `json:"required"`
	MaxLength int    `json:"max_length,omitempty"`
	Target    string `json:"target"`
}

// ParseOccasion decodes and validates one catalog row into an Occasion. It
// treats the row as trusted (admin/seed authored) but still validates it
// strictly so a malformed seed fails fast instead of corrupting every event
// built from it.
func ParseOccasion(o store.Occasion) (Occasion, error) {
	iss := &issues{}
	occ := Occasion{Slug: o.Slug, DefaultBlocks: json.RawMessage(o.DefaultBlocks)}

	var optional []string
	if err := strictUnmarshal(o.OptionalBlocks, &optional); err != nil {
		iss.add("optional_blocks", "invalid_json", "must be an array of block type strings")
	}
	for _, t := range optional {
		if !knownBlockTypes[t] {
			iss.add("optional_blocks", "invalid_type", fmt.Sprintf("unknown block type %q", t))
			continue
		}
		occ.OptionalBlocks = append(occ.OptionalBlocks, t)
	}

	fields, err := parseFieldDefs(json.RawMessage(o.RsvpFields), "rsvp_fields", occasionFieldKeyRe, 32)
	if err != nil {
		iss.add("rsvp_fields", "invalid", err.Error())
	}
	occ.RSVPFields = fields

	if err := strictUnmarshal(o.Copy, &occ.Copy); err != nil {
		iss.add("copy", "invalid_json", "must be a copy object")
	} else {
		if strings.TrimSpace(occ.Copy.TitleTemplate) == "" {
			iss.add("copy.title_template", "required", "This field is required.")
		}
	}

	var questions []SetupQuestion
	if err := strictUnmarshal(o.SetupQuestions, &questions); err != nil {
		iss.add("setup_questions", "invalid_json", "must be an array")
	}
	seenKey := make(map[string]bool, len(questions))
	for i, q := range questions {
		path := fmt.Sprintf("setup_questions[%d]", i)
		if !occasionFieldKeyRe.MatchString(q.Key) {
			iss.add(path+".key", "invalid_key", "invalid setup question key")
			continue
		}
		if seenKey[q.Key] {
			iss.add(path+".key", "duplicate_key", "setup question keys must be unique")
			continue
		}
		seenKey[q.Key] = true
		if !setupQuestionTypes[q.Type] {
			iss.add(path+".type", "invalid_type", "unknown setup question type")
		}
		if !setupQuestionTargets[q.Target] {
			iss.add(path+".target", "invalid_target", "unknown setup question target")
		}
		if q.Target == "title_var" && !strings.Contains(occ.Copy.TitleTemplate, "{"+q.Key+"}") {
			iss.add(path+".target", "unused_placeholder", "title_template has no {"+q.Key+"} placeholder")
		}
	}
	occ.SetupQuestions = questions

	if err := iss.err(); err != nil {
		return Occasion{}, err
	}

	// The default blocks are validated against the occasion they describe
	// (they must only use the occasion's own default/optional block types).
	if _, err := ValidateContent(occ.DefaultBlocks, occ); err != nil {
		return Occasion{}, fmt.Errorf("default_blocks: %w", err)
	}
	return occ, nil
}

// setupAnswerMaxRunes bounds every setup-question answer, regardless of type,
// since they only ever substitute into short template placeholders or block fields.
const setupAnswerMaxRunes = 60

// BuildInitialContent builds a new draft's content from occ's default blocks,
// substituting the host's setup-question answers into the title template and
// the target block fields, then validates the result exactly like a save.
func BuildInitialContent(occ Occasion, answers map[string]string) (json.RawMessage, error) {
	var blocks []map[string]any
	if err := json.Unmarshal(occ.DefaultBlocks, &blocks); err != nil {
		return nil, fmt.Errorf("default blocks: %w", err)
	}

	title := occ.Copy.TitleTemplate
	sets := make(map[string]string, len(occ.SetupQuestions))
	iss := &issues{}
	for _, q := range occ.SetupQuestions {
		raw, present := answers[q.Key]
		ans := normalizeText(raw)
		if !present || ans == "" {
			if q.Required {
				iss.add("answers."+q.Key, "required", "This answer is required.")
			}
			continue
		}
		if code, msg, ok := validateText(ans, setupAnswerMaxRunes, false); !ok {
			iss.add("answers."+q.Key, code, msg)
			continue
		}
		if q.Target == "title_var" {
			title = strings.ReplaceAll(title, "{"+q.Key+"}", ans)
		} else {
			sets[q.Target] = ans
		}
	}
	if err := iss.err(); err != nil {
		return nil, err
	}

	for _, b := range blocks {
		switch b["type"] {
		case "hero":
			b["title"] = title
			if v, ok := sets["hero.subtitle"]; ok {
				b["subtitle"] = v
			}
		case "datetime":
			if v, ok := sets["datetime.start_local"]; ok {
				b["start_local"] = v
			}
			if v, ok := sets["datetime.timezone"]; ok {
				b["timezone"] = v
			}
		case "location":
			if v, ok := sets["location.name"]; ok {
				b["name"] = v
			}
			if v, ok := sets["location.address"]; ok {
				b["address"] = v
			}
		}
	}

	built, err := json.Marshal(blocks)
	if err != nil {
		return nil, fmt.Errorf("marshal built content: %w", err)
	}
	saved, err := ValidateContent(built, occ)
	if err != nil {
		return nil, err
	}
	return saved.JSON, nil
}
