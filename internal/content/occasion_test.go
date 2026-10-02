package content

import (
	"testing"

	"github.com/ansonarose/seeyouthere-backend/internal/store"
)

func validOccasionRow() store.Occasion {
	return store.Occasion{
		Slug: "wedding",
		Name: "Wedding",
		DefaultBlocks: []byte(`[
			{"id":"hero1","type":"hero","title":"{name_1} & {name_2}","subtitle":"","image":null},
			{"id":"dt1","type":"datetime","heading":"","start_local":"2027-06-01T16:00","end_local":"","timezone":"Europe/London","all_day":false}
		]`),
		OptionalBlocks: []byte(`["text","image","gallery"]`),
		RsvpFields:     []byte(`[{"key":"dietary","label":"Dietary requirements","type":"text","required":false,"max_length":200,"options":[],"min":null,"max":null,"help":""}]`),
		Copy:           []byte(`{"title_template":"{name_1} & {name_2}","tagline":"","rsvp_heading":"","rsvp_body":"","share_message":""}`),
		SetupQuestions: []byte(`[{"key":"name_1","label":"First name","type":"text","required":true,"target":"title_var"},{"key":"name_2","label":"Second name","type":"text","required":true,"target":"title_var"}]`),
	}
}

func TestParseOccasion_Valid(t *testing.T) {
	occ, err := ParseOccasion(validOccasionRow())
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if occ.Slug != "wedding" || len(occ.RSVPFields) != 1 || len(occ.SetupQuestions) != 2 {
		t.Errorf("unexpected occasion: %+v", occ)
	}
}

func TestParseOccasion_UnusedPlaceholder(t *testing.T) {
	row := validOccasionRow()
	row.SetupQuestions = []byte(`[{"key":"name_3","label":"Third","type":"text","required":false,"target":"title_var"}]`)
	if _, err := ParseOccasion(row); !hasIssueCode(err, "unused_placeholder") {
		t.Fatalf("expected unused_placeholder, got %v", err)
	}
}

func TestParseOccasion_UnknownBlockTypeInDefaults(t *testing.T) {
	row := validOccasionRow()
	row.DefaultBlocks = []byte(`[{"id":"x1","type":"bogus"}]`)
	if _, err := ParseOccasion(row); err == nil {
		t.Fatal("expected error for unknown block type in default_blocks")
	}
}

func TestParseOccasion_DuplicateSetupQuestionKey(t *testing.T) {
	row := validOccasionRow()
	row.SetupQuestions = []byte(`[
		{"key":"name_1","label":"A","type":"text","required":true,"target":"title_var"},
		{"key":"name_1","label":"B","type":"text","required":true,"target":"title_var"}
	]`)
	if _, err := ParseOccasion(row); !hasIssueCode(err, "duplicate_key") {
		t.Fatalf("expected duplicate_key, got %v", err)
	}
}

// TestSeedCatalog_Shapes mirrors the §2.2 seed catalog's per-occasion rsvp
// field additions and setup-question targets so a drift in either breaks
// this test before it breaks production. content is pure Go with no DB
// access by design; a full round trip that loads the actual migrated seed
// rows through ListActiveOccasions and ParseOccasion belongs to whichever
// B1 package first queries the catalog (see the handback report).
func TestSeedCatalog_Shapes(t *testing.T) {
	occasions := []struct {
		slug           string
		extraFields    []string // beyond the universal dietary + message
		defaultBlocks  string
		optionalBlocks string
	}{
		{"wedding", []string{"song_request"},
			`[{"id":"h1","type":"hero","title":"{name_1} & {name_2}","subtitle":"","image":null}]`,
			`["text","datetime","location","rsvp"]`},
		{"baby-shower", []string{"guess"},
			`[{"id":"h1","type":"hero","title":"{name_1}'s Baby Shower","subtitle":"","image":null}]`,
			`["text","datetime","location","rsvp"]`},
		{"birthday", []string{"bringing"},
			`[{"id":"h1","type":"hero","title":"{name_1}'s Birthday","subtitle":"","image":null}]`,
			`["text","datetime","location","rsvp"]`},
	}
	for _, o := range occasions {
		t.Run(o.slug, func(t *testing.T) {
			fields := `[{"key":"dietary","label":"Dietary requirements","type":"text","required":false,"max_length":200,"options":[],"min":null,"max":null,"help":""},` +
				`{"key":"message","label":"Message","type":"textarea","required":false,"max_length":500,"options":[],"min":null,"max":null,"help":""}`
			for _, k := range o.extraFields {
				var extra string
				switch k {
				case "guess":
					extra = `{"key":"guess","label":"Guess","type":"select","required":false,"max_length":0,"options":["boy","girl","surprise"],"min":null,"max":null,"help":""}`
				default:
					extra = `{"key":"` + k + `","label":"` + k + `","type":"text","required":false,"max_length":120,"options":[],"min":null,"max":null,"help":""}`
				}
				fields += "," + extra
			}
			fields += "]"

			row := store.Occasion{
				Slug:           o.slug,
				Name:           o.slug,
				DefaultBlocks:  []byte(o.defaultBlocks),
				OptionalBlocks: []byte(o.optionalBlocks),
				RsvpFields:     []byte(fields),
				Copy:           []byte(`{"title_template":"{name_1}","tagline":"","rsvp_heading":"","rsvp_body":"","share_message":""}`),
				SetupQuestions: []byte(`[{"key":"name_1","label":"Name","type":"text","required":true,"target":"title_var"}]`),
			}
			if _, err := ParseOccasion(row); err != nil {
				t.Fatalf("occasion %s failed to parse: %v", o.slug, err)
			}
		})
	}
}
