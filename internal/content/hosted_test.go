package content

import (
	"encoding/json"
	"slices"
	"testing"
)

func TestHostedOccasion_DefaultSections(t *testing.T) {
	occ := sampleOccasion(t)
	occ.OptionalBlocks = []string{"text", "schedule", "countdown", "location", "rsvp"}
	occ.RSVPFields = append(occ.RSVPFields, Field{Key: "organisation", Label: "Organisation", Type: "text", MaxLength: 120})
	original := string(occ.DefaultBlocks)

	hosted := HostedOccasion(occ)
	if string(occ.DefaultBlocks) != original {
		t.Fatal("HostedOccasion modified its argument")
	}

	built, err := BuildInitialContent(hosted, map[string]string{"name_1": "Spring", "name_2": "Gala"})
	if err != nil {
		t.Fatalf("BuildInitialContent: %v", err)
	}
	var blocks []struct {
		ID      string `json:"id"`
		Type    string `json:"type"`
		Heading string `json:"heading"`
	}
	if err := json.Unmarshal(built, &blocks); err != nil {
		t.Fatalf("decode: %v", err)
	}
	var types, headings []string
	for _, b := range blocks {
		types = append(types, b.Type)
		headings = append(headings, b.Heading)
	}
	wantTypes := []string{"hero", "text", "datetime", "countdown", "schedule", "location", "rsvp"}
	if !slices.Equal(types, wantTypes) {
		t.Errorf("block types = %v, want %v", types, wantTypes)
	}
	wantHeadings := []string{"", "About", "Date & time", "Countdown", "Programme", "Event details", "Register"}
	if !slices.Equal(headings, wantHeadings) {
		t.Errorf("headings = %v, want %v", headings, wantHeadings)
	}
}
