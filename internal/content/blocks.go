package content

import (
	"encoding/json"
	"errors"
	"fmt"
	"regexp"
	"time"

	"github.com/google/uuid"
)

// dateTimeLayout is the wall-clock local format used by start_local,
// end_local and deadline_local: "YYYY-MM-DDTHH:MM".
const dateTimeLayout = "2006-01-02T15:04"

// idRe matches client-generated block ids.
var idRe = regexp.MustCompile(`^[A-Za-z0-9_-]{1,24}$`)

const (
	maxBlocks           = 40
	maxCanonicalBytes   = 128 << 10
	maxMediaRefs        = 60
	headingMax          = 120
	defaultMaxPartySize = 10
	// kickerMax bounds the optional single-line label shown above a block's
	// heading (§3.2 of the rich-blocks doc). Shared by every block type that
	// has a heading, plus hero.
	kickerMax = 60
)

// knownBlockTypes are the block types this schema version understands.
var knownBlockTypes = map[string]bool{
	"hero": true, "text": true, "datetime": true, "location": true, "schedule": true,
	"image": true, "gallery": true, "dress_code": true, "links": true, "faq": true,
	"countdown": true, "rsvp": true, "guest_photos": true,
	"people": true, "video": true, "wishes": true,
}

// blockCardinality caps how many blocks of a type may appear; 0 means unbounded
// (still limited overall by maxBlocks).
var blockCardinality = map[string]int{
	"hero": 1, "datetime": 1, "location": 3, "schedule": 2, "gallery": 3,
	"dress_code": 1, "links": 2, "faq": 1, "countdown": 1, "rsvp": 1, "guest_photos": 1,
	"people": 2, "video": 3, "wishes": 1,
}

type blockMeta struct {
	ID   string `json:"id"`
	Type string `json:"type"`
}

// Saved is the result of validating or re-parsing an event's content.
type Saved struct {
	JSON     []byte
	Title    string
	StartsAt *time.Time
	// EndsAt is the datetime block's end_local parsed in the block's zone,
	// or nil when end_local is empty or the block itself is invalid. Exposed
	// so handlers can return an end instant without the frontend doing any
	// timezone maths (see event_handlers.go/public_handlers.go ends_at).
	EndsAt   *time.Time
	MediaIDs []uuid.UUID
	RSVP     *RSVPBlock
	Photos   *GuestPhotosBlock
}

// EffectiveEnd is the instant the event is over for retention purposes: nil
// when the content has no datetime, else EndsAt when it is not before
// StartsAt, else StartsAt. Saved.EndsAt keeps its API meaning unchanged.
func (s Saved) EffectiveEnd() *time.Time {
	if s.StartsAt == nil {
		return nil
	}
	if s.EndsAt != nil && !s.EndsAt.Before(*s.StartsAt) {
		return s.EndsAt
	}
	return s.StartsAt
}

// RSVPFieldRef enables one occasion field within an event's rsvp block.
type RSVPFieldRef struct {
	Key      string `json:"key"`
	Required bool   `json:"required"`
}

// RSVPBlock is the resolved rsvp block, used by the RSVP and public-page handlers.
type RSVPBlock struct {
	Heading      string
	Body         string
	Deadline     *time.Time
	Capacity     *int
	MaxPartySize int
	Fields       []RSVPFieldRef
	Questions    []Field
}

// GuestPhotosBlock is the resolved guest_photos block.
type GuestPhotosBlock struct {
	Heading string
	Body    string
	Open    bool
}

// mediaRef is the {media_id, alt} shape shared by hero, image and gallery blocks.
type mediaRef struct {
	MediaID string `json:"media_id"`
	Alt     string `json:"alt"`
}

type heroBlockJSON struct {
	ID       string    `json:"id"`
	Type     string    `json:"type"`
	Kicker   string    `json:"kicker"`
	Title    string    `json:"title"`
	Subtitle string    `json:"subtitle"`
	Image    *mediaRef `json:"image"`
}

type textBlockJSON struct {
	ID      string `json:"id"`
	Type    string `json:"type"`
	Kicker  string `json:"kicker"`
	Heading string `json:"heading"`
	Body    string `json:"body"`
}

type datetimeBlockJSON struct {
	ID         string `json:"id"`
	Type       string `json:"type"`
	Kicker     string `json:"kicker"`
	Heading    string `json:"heading"`
	StartLocal string `json:"start_local"`
	EndLocal   string `json:"end_local"`
	Timezone   string `json:"timezone"`
	AllDay     bool   `json:"all_day"`
}

type locationBlockJSON struct {
	ID      string `json:"id"`
	Type    string `json:"type"`
	Kicker  string `json:"kicker"`
	Heading string `json:"heading"`
	Name    string `json:"name"`
	Address string `json:"address"`
	MapURL  string `json:"map_url"`
	Notes   string `json:"notes"`
}

type scheduleItemJSON struct {
	Time        string `json:"time"`
	Title       string `json:"title"`
	Description string `json:"description"`
}

type scheduleBlockJSON struct {
	ID      string             `json:"id"`
	Type    string             `json:"type"`
	Kicker  string             `json:"kicker"`
	Heading string             `json:"heading"`
	Items   []scheduleItemJSON `json:"items"`
}

type imageBlockJSON struct {
	ID      string `json:"id"`
	Type    string `json:"type"`
	MediaID string `json:"media_id"`
	Alt     string `json:"alt"`
	Caption string `json:"caption"`
}

type galleryBlockJSON struct {
	ID      string     `json:"id"`
	Type    string     `json:"type"`
	Kicker  string     `json:"kicker"`
	Heading string     `json:"heading"`
	Display string     `json:"display"`
	Images  []mediaRef `json:"images"`
}

type dressCodeBlockJSON struct {
	ID      string `json:"id"`
	Type    string `json:"type"`
	Kicker  string `json:"kicker"`
	Heading string `json:"heading"`
	Body    string `json:"body"`
}

type linkItemJSON struct {
	Label string `json:"label"`
	URL   string `json:"url"`
}

type linksBlockJSON struct {
	ID      string         `json:"id"`
	Type    string         `json:"type"`
	Kicker  string         `json:"kicker"`
	Heading string         `json:"heading"`
	Body    string         `json:"body"`
	Items   []linkItemJSON `json:"items"`
}

type faqItemJSON struct {
	Question string `json:"question"`
	Answer   string `json:"answer"`
}

type faqBlockJSON struct {
	ID      string        `json:"id"`
	Type    string        `json:"type"`
	Kicker  string        `json:"kicker"`
	Heading string        `json:"heading"`
	Items   []faqItemJSON `json:"items"`
}

type countdownBlockJSON struct {
	ID      string `json:"id"`
	Type    string `json:"type"`
	Kicker  string `json:"kicker"`
	Heading string `json:"heading"`
}

type rsvpBlockJSON struct {
	ID            string          `json:"id"`
	Type          string          `json:"type"`
	Kicker        string          `json:"kicker"`
	Heading       string          `json:"heading"`
	Body          string          `json:"body"`
	DeadlineLocal string          `json:"deadline_local"`
	Capacity      *int            `json:"capacity"`
	MaxPartySize  *int            `json:"max_party_size"`
	Fields        []RSVPFieldRef  `json:"fields"`
	Questions     json.RawMessage `json:"questions"`
}

type guestPhotosBlockJSON struct {
	ID      string `json:"id"`
	Type    string `json:"type"`
	Kicker  string `json:"kicker"`
	Heading string `json:"heading"`
	Body    string `json:"body"`
	Open    bool   `json:"open"`
}

// mediaRef with an optional caption-style alt is reused for a person's
// photo; personJSON, peopleBlockJSON, videoBlockJSON and wishesBlockJSON are
// the three new block types (§3.1 of the rich-blocks doc).
type personJSON struct {
	Name        string    `json:"name"`
	Role        string    `json:"role"`
	Photo       *mediaRef `json:"photo"`
	FamilyLabel string    `json:"family_label"`
	FamilyNames string    `json:"family_names"`
	Place       string    `json:"place"`
	Bio         string    `json:"bio"`
}

type peopleBlockJSON struct {
	ID      string       `json:"id"`
	Type    string       `json:"type"`
	Kicker  string       `json:"kicker"`
	Heading string       `json:"heading"`
	People  []personJSON `json:"people"`
}

type videoBlockJSON struct {
	ID            string `json:"id"`
	Type          string `json:"type"`
	Kicker        string `json:"kicker"`
	Heading       string `json:"heading"`
	Provider      string `json:"provider"`
	VideoID       string `json:"video_id"`
	VimeoHash     string `json:"vimeo_hash"`
	Aspect        string `json:"aspect"`
	Caption       string `json:"caption"`
	PosterMediaID string `json:"poster_media_id"`
}

type wishJSON struct {
	Message string `json:"message"`
	Author  string `json:"author"`
}

type wishesBlockJSON struct {
	ID      string     `json:"id"`
	Type    string     `json:"type"`
	Kicker  string     `json:"kicker"`
	Heading string     `json:"heading"`
	Items   []wishJSON `json:"items"`
}

// allowedBlockTypes is the union of an occasion's default and optional block
// types: the only types an event of this occasion may use.
func allowedBlockTypes(occ Occasion) map[string]bool {
	allowed := make(map[string]bool, len(occ.OptionalBlocks)+4)
	for _, t := range occ.OptionalBlocks {
		allowed[t] = true
	}
	var defaults []blockMeta
	_ = json.Unmarshal(occ.DefaultBlocks, &defaults)
	for _, b := range defaults {
		allowed[b.Type] = true
	}
	return allowed
}

// datetimeZone resolves the timezone, start instant and (when present) end
// instant of the content's datetime block (there is at most one, enforced
// below), used to validate the rsvp block's deadline and to derive
// events.starts_at / Saved.EndsAt. It tolerates an invalid datetime block
// (returns loc=nil); the real validation error for that block is reported by
// the main pass. end is nil whenever end_local is empty or unparseable.
func datetimeZone(in []json.RawMessage) (loc *time.Location, start, end *time.Time, present bool) {
	for _, raw := range in {
		var meta blockMeta
		if err := json.Unmarshal(raw, &meta); err != nil || meta.Type != "datetime" {
			continue
		}
		present = true
		var b datetimeBlockJSON
		if err := strictUnmarshal(raw, &b); err != nil {
			return nil, nil, nil, true
		}
		l, err := time.LoadLocation(b.Timezone)
		if err != nil || b.Timezone == "Local" {
			return nil, nil, nil, true
		}
		s, err := time.ParseInLocation(dateTimeLayout, b.StartLocal, l)
		if err != nil {
			return nil, nil, nil, true
		}
		if b.EndLocal != "" {
			if e, err := time.ParseInLocation(dateTimeLayout, b.EndLocal, l); err == nil {
				end = &e
			}
		}
		return l, &s, end, true
	}
	return nil, nil, nil, false
}

// ValidateContent decodes and validates a whole content array against occ,
// returning the canonical (re-marshalled) form and the values derived from it.
func ValidateContent(raw json.RawMessage, occ Occasion) (Saved, error) {
	var in []json.RawMessage
	if err := strictUnmarshal(raw, &in); err != nil {
		return Saved{}, single("content", "invalid_json", "must be a JSON array of blocks")
	}
	if len(in) < 1 || len(in) > maxBlocks {
		return Saved{}, single("content", "invalid_length", fmt.Sprintf("must have between 1 and %d blocks", maxBlocks))
	}

	allowed := allowedBlockTypes(occ)
	dtLoc, dtStart, dtEnd, haveDatetime := datetimeZone(in)

	iss := &issues{}
	ids := make(map[string]bool, len(in))
	counts := make(map[string]int, len(in))
	out := make([]any, len(in))
	var mediaIDs []uuid.UUID
	var heroTitle, firstHeading string
	var rsvpBlock *RSVPBlock
	var photosBlock *GuestPhotosBlock

	addMedia := func(path, raw string) {
		if raw == "" {
			return
		}
		id, err := uuid.Parse(raw)
		if err != nil {
			iss.add(path, "invalid_media_id", "Not a valid media reference.")
			return
		}
		mediaIDs = append(mediaIDs, id)
	}

	for i, rawBlock := range in {
		path := fmt.Sprintf("content[%d]", i)
		var meta blockMeta
		if err := json.Unmarshal(rawBlock, &meta); err != nil {
			iss.add(path, "invalid_json", "must be a block object")
			continue
		}
		switch {
		case !idRe.MatchString(meta.ID):
			iss.add(path+".id", "invalid_id", "Invalid block id.")
		case ids[meta.ID]:
			iss.add(path+".id", "duplicate_id", "Block ids must be unique.")
		default:
			ids[meta.ID] = true
		}
		if !knownBlockTypes[meta.Type] {
			iss.add(path+".type", "invalid_type", "Unknown block type.")
			continue
		}
		if !allowed[meta.Type] {
			iss.add(path+".type", "block_not_allowed", "This block type isn't available for the event's occasion.")
			continue
		}
		counts[meta.Type]++

		switch meta.Type {
		case "hero":
			var b heroBlockJSON
			if err := strictUnmarshal(rawBlock, &b); err != nil {
				iss.add(path, "invalid_json", "invalid hero block")
				continue
			}
			b.Kicker = checkField(iss, path+".kicker", b.Kicker, kickerMax, false, false)
			b.Title = checkField(iss, path+".title", b.Title, headingMax, true, false)
			b.Subtitle = checkField(iss, path+".subtitle", b.Subtitle, 200, false, false)
			if b.Image != nil {
				b.Image.Alt = checkField(iss, path+".image.alt", b.Image.Alt, 200, false, false)
				addMedia(path+".image.media_id", b.Image.MediaID)
			}
			if b.Title != "" {
				heroTitle = b.Title
			}
			out[i] = b

		case "text":
			var b textBlockJSON
			if err := strictUnmarshal(rawBlock, &b); err != nil {
				iss.add(path, "invalid_json", "invalid text block")
				continue
			}
			b.Kicker = checkField(iss, path+".kicker", b.Kicker, kickerMax, false, false)
			b.Heading = checkField(iss, path+".heading", b.Heading, headingMax, false, false)
			b.Body = checkField(iss, path+".body", b.Body, 5000, true, true)
			if firstHeading == "" && b.Heading != "" {
				firstHeading = b.Heading
			}
			out[i] = b

		case "datetime":
			var b datetimeBlockJSON
			if err := strictUnmarshal(rawBlock, &b); err != nil {
				iss.add(path, "invalid_json", "invalid datetime block")
				continue
			}
			b.Kicker = checkField(iss, path+".kicker", b.Kicker, kickerMax, false, false)
			b.Heading = checkField(iss, path+".heading", b.Heading, headingMax, false, false)
			if b.Timezone == "Local" {
				iss.add(path+".timezone", "invalid_timezone", "Choose a specific timezone.")
			} else if _, err := time.LoadLocation(b.Timezone); err != nil {
				iss.add(path+".timezone", "invalid_timezone", "Unknown timezone.")
			}
			start, err := time.Parse(dateTimeLayout, b.StartLocal)
			if err != nil {
				iss.add(path+".start_local", "invalid_datetime", "Must be a valid date and time.")
			} else if b.EndLocal != "" {
				end, err := time.Parse(dateTimeLayout, b.EndLocal)
				switch {
				case err != nil:
					iss.add(path+".end_local", "invalid_datetime", "Must be a valid date and time.")
				case !end.After(start):
					iss.add(path+".end_local", "invalid_range", "Must be after the start time.")
				case end.Sub(start) > 14*24*time.Hour:
					iss.add(path+".end_local", "invalid_range", "Must be within 14 days of the start.")
				}
			}
			if firstHeading == "" && b.Heading != "" {
				firstHeading = b.Heading
			}
			out[i] = b

		case "location":
			var b locationBlockJSON
			if err := strictUnmarshal(rawBlock, &b); err != nil {
				iss.add(path, "invalid_json", "invalid location block")
				continue
			}
			b.Kicker = checkField(iss, path+".kicker", b.Kicker, kickerMax, false, false)
			b.Heading = checkField(iss, path+".heading", b.Heading, headingMax, false, false)
			b.Name = checkField(iss, path+".name", b.Name, headingMax, true, false)
			b.Address = checkField(iss, path+".address", b.Address, 300, false, false)
			b.Notes = checkField(iss, path+".notes", b.Notes, 500, false, false)
			if b.MapURL != "" {
				if canon, ok := validateMapURL(b.MapURL, 2048); ok {
					b.MapURL = canon
				} else {
					iss.add(path+".map_url", "invalid_url", "Must be a supported map link.")
					b.MapURL = ""
				}
			}
			if firstHeading == "" && b.Heading != "" {
				firstHeading = b.Heading
			}
			out[i] = b

		case "schedule":
			var b scheduleBlockJSON
			if err := strictUnmarshal(rawBlock, &b); err != nil {
				iss.add(path, "invalid_json", "invalid schedule block")
				continue
			}
			b.Kicker = checkField(iss, path+".kicker", b.Kicker, kickerMax, false, false)
			b.Heading = checkField(iss, path+".heading", b.Heading, headingMax, false, false)
			if len(b.Items) < 1 || len(b.Items) > 30 {
				iss.add(path+".items", "invalid_length", "must have between 1 and 30 items")
			}
			for j := range b.Items {
				ip := fmt.Sprintf("%s.items[%d]", path, j)
				it := &b.Items[j]
				if it.Time != "" {
					if _, err := time.Parse("15:04", it.Time); err != nil {
						iss.add(ip+".time", "invalid_time", "Must be HH:MM.")
						it.Time = ""
					}
				}
				it.Title = checkField(iss, ip+".title", it.Title, headingMax, true, false)
				it.Description = checkField(iss, ip+".description", it.Description, 300, false, false)
			}
			if firstHeading == "" && b.Heading != "" {
				firstHeading = b.Heading
			}
			out[i] = b

		case "image":
			var b imageBlockJSON
			if err := strictUnmarshal(rawBlock, &b); err != nil {
				iss.add(path, "invalid_json", "invalid image block")
				continue
			}
			if b.MediaID == "" {
				iss.add(path+".media_id", "required", "This field is required.")
			}
			addMedia(path+".media_id", b.MediaID)
			b.Alt = checkField(iss, path+".alt", b.Alt, 200, false, false)
			b.Caption = checkField(iss, path+".caption", b.Caption, 200, false, false)
			out[i] = b

		case "gallery":
			var b galleryBlockJSON
			if err := strictUnmarshal(rawBlock, &b); err != nil {
				iss.add(path, "invalid_json", "invalid gallery block")
				continue
			}
			b.Kicker = checkField(iss, path+".kicker", b.Kicker, kickerMax, false, false)
			b.Heading = checkField(iss, path+".heading", b.Heading, headingMax, false, false)
			switch b.Display {
			case "":
				b.Display = "grid"
			case "grid", "carousel":
				// valid
			default:
				iss.add(path+".display", "invalid_value", "Unknown gallery display.")
				b.Display = "grid"
			}
			if len(b.Images) < 1 || len(b.Images) > 24 {
				iss.add(path+".images", "invalid_length", "must have between 1 and 24 images")
			}
			for j := range b.Images {
				ip := fmt.Sprintf("%s.images[%d]", path, j)
				if b.Images[j].MediaID == "" {
					iss.add(ip+".media_id", "required", "This field is required.")
				}
				addMedia(ip+".media_id", b.Images[j].MediaID)
				b.Images[j].Alt = checkField(iss, ip+".alt", b.Images[j].Alt, 200, false, false)
			}
			if firstHeading == "" && b.Heading != "" {
				firstHeading = b.Heading
			}
			out[i] = b

		case "dress_code":
			var b dressCodeBlockJSON
			if err := strictUnmarshal(rawBlock, &b); err != nil {
				iss.add(path, "invalid_json", "invalid dress_code block")
				continue
			}
			b.Kicker = checkField(iss, path+".kicker", b.Kicker, kickerMax, false, false)
			b.Heading = checkField(iss, path+".heading", b.Heading, headingMax, false, false)
			b.Body = checkField(iss, path+".body", b.Body, 1000, true, false)
			if firstHeading == "" && b.Heading != "" {
				firstHeading = b.Heading
			}
			out[i] = b

		case "links":
			var b linksBlockJSON
			if err := strictUnmarshal(rawBlock, &b); err != nil {
				iss.add(path, "invalid_json", "invalid links block")
				continue
			}
			b.Kicker = checkField(iss, path+".kicker", b.Kicker, kickerMax, false, false)
			b.Heading = checkField(iss, path+".heading", b.Heading, headingMax, false, false)
			b.Body = checkField(iss, path+".body", b.Body, 500, false, false)
			if len(b.Items) < 1 || len(b.Items) > 12 {
				iss.add(path+".items", "invalid_length", "must have between 1 and 12 items")
			}
			for j := range b.Items {
				ip := fmt.Sprintf("%s.items[%d]", path, j)
				b.Items[j].Label = checkField(iss, ip+".label", b.Items[j].Label, 80, true, false)
				if canon, ok := validateHTTPSURL(b.Items[j].URL, 2048); ok {
					b.Items[j].URL = canon
				} else {
					iss.add(ip+".url", "invalid_url", "Must be a valid https:// link.")
				}
			}
			if firstHeading == "" && b.Heading != "" {
				firstHeading = b.Heading
			}
			out[i] = b

		case "faq":
			var b faqBlockJSON
			if err := strictUnmarshal(rawBlock, &b); err != nil {
				iss.add(path, "invalid_json", "invalid faq block")
				continue
			}
			b.Kicker = checkField(iss, path+".kicker", b.Kicker, kickerMax, false, false)
			b.Heading = checkField(iss, path+".heading", b.Heading, headingMax, false, false)
			if len(b.Items) < 1 || len(b.Items) > 30 {
				iss.add(path+".items", "invalid_length", "must have between 1 and 30 items")
			}
			for j := range b.Items {
				ip := fmt.Sprintf("%s.items[%d]", path, j)
				b.Items[j].Question = checkField(iss, ip+".question", b.Items[j].Question, 200, false, false)
				b.Items[j].Answer = checkField(iss, ip+".answer", b.Items[j].Answer, 2000, false, false)
			}
			if firstHeading == "" && b.Heading != "" {
				firstHeading = b.Heading
			}
			out[i] = b

		case "countdown":
			var b countdownBlockJSON
			if err := strictUnmarshal(rawBlock, &b); err != nil {
				iss.add(path, "invalid_json", "invalid countdown block")
				continue
			}
			b.Kicker = checkField(iss, path+".kicker", b.Kicker, kickerMax, false, false)
			b.Heading = checkField(iss, path+".heading", b.Heading, headingMax, false, false)
			if !haveDatetime {
				iss.add(path, "requires_datetime", "Add a date & time block to use a countdown.")
			}
			if firstHeading == "" && b.Heading != "" {
				firstHeading = b.Heading
			}
			out[i] = b

		case "rsvp":
			var b rsvpBlockJSON
			if err := strictUnmarshal(rawBlock, &b); err != nil {
				iss.add(path, "invalid_json", "invalid rsvp block")
				continue
			}
			b.Kicker = checkField(iss, path+".kicker", b.Kicker, kickerMax, false, false)
			b.Heading = checkField(iss, path+".heading", b.Heading, headingMax, false, false)
			b.Body = checkField(iss, path+".body", b.Body, 1000, false, false)

			var deadline *time.Time
			if b.DeadlineLocal != "" {
				switch {
				case !haveDatetime:
					iss.add(path+".deadline_local", "requires_datetime", "Add a date & time block to set a deadline.")
				case dtLoc == nil:
					// The datetime block itself is invalid; its own error is already reported.
				default:
					if d, err := time.ParseInLocation(dateTimeLayout, b.DeadlineLocal, dtLoc); err != nil {
						iss.add(path+".deadline_local", "invalid_datetime", "Must be a valid date and time.")
					} else {
						deadline = &d
					}
				}
			}
			if b.Capacity != nil && (*b.Capacity < 1 || *b.Capacity > 10000) {
				iss.add(path+".capacity", "out_of_range", "must be between 1 and 10000")
				b.Capacity = nil
			}
			maxParty := defaultMaxPartySize
			if b.MaxPartySize != nil {
				if *b.MaxPartySize < 1 || *b.MaxPartySize > 20 {
					iss.add(path+".max_party_size", "out_of_range", "must be between 1 and 20")
				} else {
					maxParty = *b.MaxPartySize
				}
			}

			seenFieldKey := make(map[string]bool, len(b.Fields))
			fields := make([]RSVPFieldRef, 0, len(b.Fields))
			for j, f := range b.Fields {
				fp := fmt.Sprintf("%s.fields[%d]", path, j)
				if !occ.hasRSVPField(f.Key) {
					iss.add(fp+".key", "unknown_field", "Not one of this occasion's fields.")
					continue
				}
				if seenFieldKey[f.Key] {
					iss.add(fp+".key", "duplicate_key", "Field keys must be unique.")
					continue
				}
				seenFieldKey[f.Key] = true
				fields = append(fields, f)
			}

			questions, err := parseFieldDefs(b.Questions, path+".questions", hostQuestionKeyRe, 5)
			if err != nil {
				var ve *ValidationError
				if errors.As(err, &ve) {
					iss.list = append(iss.list, ve.Issues...)
				}
			}

			b.Fields = fields
			out[i] = b
			rsvpBlock = &RSVPBlock{
				Heading: b.Heading, Body: b.Body, Deadline: deadline, Capacity: b.Capacity,
				MaxPartySize: maxParty, Fields: fields, Questions: questions,
			}

		case "guest_photos":
			var b guestPhotosBlockJSON
			if err := strictUnmarshal(rawBlock, &b); err != nil {
				iss.add(path, "invalid_json", "invalid guest_photos block")
				continue
			}
			b.Kicker = checkField(iss, path+".kicker", b.Kicker, kickerMax, false, false)
			b.Heading = checkField(iss, path+".heading", b.Heading, headingMax, false, false)
			b.Body = checkField(iss, path+".body", b.Body, 500, false, false)
			out[i] = b
			photosBlock = &GuestPhotosBlock{Heading: b.Heading, Body: b.Body, Open: b.Open}

		case "people":
			var b peopleBlockJSON
			if err := strictUnmarshal(rawBlock, &b); err != nil {
				iss.add(path, "invalid_json", "invalid people block")
				continue
			}
			b.Kicker = checkField(iss, path+".kicker", b.Kicker, kickerMax, false, false)
			b.Heading = checkField(iss, path+".heading", b.Heading, headingMax, false, false)
			if len(b.People) < 1 || len(b.People) > 6 {
				iss.add(path+".people", "invalid_length", "must have between 1 and 6 people")
			}
			for j := range b.People {
				pp := fmt.Sprintf("%s.people[%d]", path, j)
				p := &b.People[j]
				p.Name = checkField(iss, pp+".name", p.Name, 80, true, false)
				p.Role = checkField(iss, pp+".role", p.Role, 60, false, false)
				if p.Photo != nil {
					p.Photo.Alt = checkField(iss, pp+".photo.alt", p.Photo.Alt, 200, false, false)
					addMedia(pp+".photo.media_id", p.Photo.MediaID)
				}
				p.FamilyLabel = checkField(iss, pp+".family_label", p.FamilyLabel, 40, false, false)
				p.FamilyNames = checkField(iss, pp+".family_names", p.FamilyNames, 160, false, false)
				p.Place = checkField(iss, pp+".place", p.Place, 120, false, false)
				p.Bio = checkField(iss, pp+".bio", p.Bio, 300, false, true)
			}
			if firstHeading == "" && b.Heading != "" {
				firstHeading = b.Heading
			}
			out[i] = b

		case "video":
			var b videoBlockJSON
			if err := strictUnmarshal(rawBlock, &b); err != nil {
				iss.add(path, "invalid_json", "invalid video block")
				continue
			}
			b.Kicker = checkField(iss, path+".kicker", b.Kicker, kickerMax, false, false)
			b.Heading = checkField(iss, path+".heading", b.Heading, headingMax, false, false)
			b.Caption = checkField(iss, path+".caption", b.Caption, 200, false, false)
			validateVideo(iss, path, &b)
			addMedia(path+".poster_media_id", b.PosterMediaID)
			if firstHeading == "" && b.Heading != "" {
				firstHeading = b.Heading
			}
			out[i] = b

		case "wishes":
			var b wishesBlockJSON
			if err := strictUnmarshal(rawBlock, &b); err != nil {
				iss.add(path, "invalid_json", "invalid wishes block")
				continue
			}
			b.Kicker = checkField(iss, path+".kicker", b.Kicker, kickerMax, false, false)
			b.Heading = checkField(iss, path+".heading", b.Heading, headingMax, false, false)
			if len(b.Items) < 1 || len(b.Items) > 30 {
				iss.add(path+".items", "invalid_length", "must have between 1 and 30 items")
			}
			for j := range b.Items {
				ip := fmt.Sprintf("%s.items[%d]", path, j)
				b.Items[j].Message = checkField(iss, ip+".message", b.Items[j].Message, 500, true, true)
				b.Items[j].Author = checkField(iss, ip+".author", b.Items[j].Author, 80, true, false)
			}
			if firstHeading == "" && b.Heading != "" {
				firstHeading = b.Heading
			}
			out[i] = b
		}
	}

	for typ, limit := range blockCardinality {
		if n := counts[typ]; n > limit {
			iss.add("content", "too_many_"+typ, fmt.Sprintf("only %d %s block(s) allowed", limit, typ))
		}
	}
	if len(mediaIDs) > maxMediaRefs {
		iss.add("content", "too_many_media", fmt.Sprintf("at most %d media references allowed", maxMediaRefs))
	}
	if err := iss.err(); err != nil {
		return Saved{}, err
	}

	canon, err := json.Marshal(out)
	if err != nil {
		return Saved{}, fmt.Errorf("marshal canonical content: %w", err)
	}
	if len(canon) > maxCanonicalBytes {
		return Saved{}, single("content", "too_large", "Content is too large.")
	}

	title := heroTitle
	if title == "" {
		title = firstHeading
	}
	if title == "" {
		title = "Untitled event"
	}

	return Saved{
		JSON:     canon,
		Title:    title,
		StartsAt: dtStart,
		EndsAt:   dtEnd,
		MediaIDs: mediaIDs,
		RSVP:     rsvpBlock,
		Photos:   photosBlock,
	}, nil
}

// ParseStored re-parses content this package has already validated and
// canonicalised (loaded from the database). It runs the same validation as
// ValidateContent: canonical output is a fixed point of validation, so this
// only fails if the stored row was corrupted or occ has changed incompatibly.
func ParseStored(stored []byte, occ Occasion) (Saved, error) {
	return ValidateContent(stored, occ)
}

// hasRSVPField reports whether key is one of the occasion's rsvp_fields.
func (o Occasion) hasRSVPField(key string) bool {
	for _, f := range o.RSVPFields {
		if f.Key == key {
			return true
		}
	}
	return false
}
