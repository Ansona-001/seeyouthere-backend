package content

import (
	"encoding/json"
	"testing"
)

// sampleOccasion returns a valid Occasion modelled on the wedding seed
// (§2.2 of the build-out plan): a hero+datetime+location+rsvp default set,
// several optional blocks, two rsvp fields and one setup question per target.
func sampleOccasion(t *testing.T) Occasion {
	t.Helper()
	defaults := `[
		{"id":"hero1","type":"hero","title":"{name_1} & {name_2}","subtitle":"","image":null},
		{"id":"dt1","type":"datetime","heading":"","start_local":"2027-06-01T16:00","end_local":"","timezone":"Europe/London","all_day":false},
		{"id":"loc1","type":"location","heading":"","name":"TBD","address":"","map_url":"","notes":""},
		{"id":"rsvp1","type":"rsvp","heading":"RSVP","body":"","deadline_local":"","capacity":null,"max_party_size":4,"fields":[{"key":"dietary","required":false}],"questions":[]}
	]`
	occ := Occasion{
		Slug:           "wedding",
		DefaultBlocks:  json.RawMessage(defaults),
		OptionalBlocks: []string{"text", "image", "gallery", "schedule", "dress_code", "links", "faq", "countdown", "guest_photos"},
		RSVPFields: []Field{
			{Key: "dietary", Label: "Dietary requirements", Type: "text", MaxLength: 200},
			{Key: "message", Label: "Message", Type: "textarea", MaxLength: 500},
		},
		Copy: Copy{
			TitleTemplate: "{name_1} & {name_2}",
			Tagline:       "We're getting married!",
			RSVPHeading:   "Will you join us?",
			RSVPBody:      "Please let us know by the date below.",
			ShareMessage:  "You're invited!",
		},
		SetupQuestions: []SetupQuestion{
			{Key: "name_1", Label: "First partner's name", Type: "text", Required: true, Target: "title_var"},
			{Key: "name_2", Label: "Second partner's name", Type: "text", Required: true, Target: "title_var"},
		},
	}
	return occ
}

func mustJSON(t *testing.T, v any) json.RawMessage {
	t.Helper()
	b, err := json.Marshal(v)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	return b
}

func hasIssueCode(err error, code string) bool {
	ve, ok := err.(*ValidationError)
	if !ok {
		return false
	}
	for _, i := range ve.Issues {
		if i.Code == code {
			return true
		}
	}
	return false
}
