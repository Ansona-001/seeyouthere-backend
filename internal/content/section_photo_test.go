package content

import (
	"encoding/json"
	"strings"
	"testing"

	"github.com/google/uuid"
)

func TestSectionPhoto(t *testing.T) {
	occ := sampleOccasion(t)
	mediaID := uuid.NewString()
	hero := `{"id":"h1","type":"hero","kicker":"","title":"Sandra & Alan","subtitle":"","image":null}`
	text := func(extra string) string {
		return `{"id":"t1","type":"text","kicker":"","heading":"Hi","body":"x"` + extra + `}`
	}
	validate := func(blocks ...string) (Saved, error) {
		return ValidateContent(json.RawMessage("["+strings.Join(blocks, ",")+"]"), occ)
	}

	t.Run("a section can carry a photo, normalised and kept in the canonical form", func(t *testing.T) {
		saved, err := validate(hero, text(`,"photo":{"media_id":"`+mediaID+`"}`))
		if err != nil {
			t.Fatalf("validate: %v", err)
		}
		var got []map[string]any
		if err := json.Unmarshal(saved.JSON, &got); err != nil {
			t.Fatal(err)
		}
		p, _ := got[1]["photo"].(map[string]any)
		if p["media_id"] != mediaID || p["fade"] != "medium" {
			t.Errorf("photo = %v", p)
		}
		found := false
		for _, id := range saved.MediaIDs {
			found = found || id.String() == mediaID
		}
		if !found {
			t.Errorf("photo media %s missing from MediaIDs %v", mediaID, saved.MediaIDs)
		}
		if again, err := ParseStored(saved.JSON, occ); err != nil || string(again.JSON) != string(saved.JSON) {
			t.Errorf("canonical form is not a fixed point: %v", err)
		}
	})

	t.Run("absent and null photo leave the block unchanged", func(t *testing.T) {
		for _, extra := range []string{``, `,"photo":null`} {
			saved, err := validate(hero, text(extra))
			if err != nil {
				t.Fatalf("validate %q: %v", extra, err)
			}
			if strings.Contains(string(saved.JSON), `"photo"`) {
				t.Errorf("unexpected photo in %s", saved.JSON)
			}
		}
	})

	for _, tc := range []struct {
		name, photo, field string
	}{
		{"missing media id", `{"alt":"x"}`, "photo.media_id"},
		{"bad media id", `{"media_id":"nope"}`, "photo.media_id"},
		{"unknown strength", `{"media_id":"` + mediaID + `","fade":"huge"}`, "photo.fade"},
		{"unknown field", `{"media_id":"` + mediaID + `","x":1}`, "photo"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			_, err := validate(hero, text(`,"photo":`+tc.photo))
			if err == nil || !strings.Contains(err.Error(), tc.field) && !strings.Contains(strings.ToLower(err.Error()), "photo") {
				t.Fatalf("err = %v, want an issue on %s", err, tc.field)
			}
		})
	}

	t.Run("a photo on the date block does not disturb the derived start time", func(t *testing.T) {
		dt := `{"id":"d1","type":"datetime","kicker":"","heading":"","start_local":"2027-06-01T16:00","end_local":"","timezone":"Europe/London","all_day":false,"photo":{"media_id":"` + mediaID + `"}}`
		saved, err := validate(hero, dt)
		if err != nil {
			t.Fatalf("validate: %v", err)
		}
		if saved.StartsAt == nil {
			t.Error("StartsAt is nil")
		}
	})

	t.Run("hero cannot carry a photo", func(t *testing.T) {
		h := `{"id":"h1","type":"hero","kicker":"","title":"A","subtitle":"","image":null,"photo":{"media_id":"` + mediaID + `"}}`
		if _, err := validate(h, text(``)); err == nil {
			t.Fatal("expected an error")
		}
	})
}
