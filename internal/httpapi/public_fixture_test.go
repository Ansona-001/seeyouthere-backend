package httpapi

import (
	"bytes"
	"context"
	crand "crypto/rand"
	"encoding/binary"
	"fmt"
	"image"
	"image/color"
	"image/jpeg"
	"net/http"
	"net/http/httptest"
	"net/netip"
	"strings"
	"sync/atomic"
	"testing"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/redis/go-redis/v9"

	"github.com/ansonarose/seeyouthere-backend/internal/config"
	"github.com/ansonarose/seeyouthere-backend/internal/content"
	"github.com/ansonarose/seeyouthere-backend/internal/media"
	"github.com/ansonarose/seeyouthere-backend/internal/passhash"
	"github.com/ansonarose/seeyouthere-backend/internal/ratelimit"
	"github.com/ansonarose/seeyouthere-backend/internal/store"
	"github.com/ansonarose/seeyouthere-backend/internal/token"
)

// testIPSalt is randomised once per test binary run. Rate-limited handlers
// key their Valkey counters by IP with hour-long windows, so hardcoded
// literal test IPs would make a test rerun within the same hour inherit a
// previous run's counts and fail on limits that have nothing to do with its
// own requests; nextTestIP avoids that by drawing from a fresh range each run.
var testIPSalt = func() uint32 {
	var b [4]byte
	if _, err := crand.Read(b[:]); err != nil {
		panic(err)
	}
	return binary.BigEndian.Uint32(b[:])
}()

var testIPCounter atomic.Uint32

// nextTestIP returns a syntactically valid, unique-per-call IPv4 address for
// use as a test request's client IP.
func nextTestIP() string {
	v := testIPSalt + testIPCounter.Add(1)
	return fmt.Sprintf("198.%d.%d.%d", byte(v>>16), byte(v>>8), byte(v))
}

// testAuthSecret is a fixed, deterministic 32-byte secret for deriving
// token.Keys in tests, matching the pattern internal/token's own tests use.
func testAuthSecret() []byte {
	secret := make([]byte, 32)
	for i := range secret {
		secret[i] = byte(i + 1)
	}
	return secret
}

// testJPEG returns a small, valid baseline JPEG (encoded fresh, no external
// fixture file) suitable for exercising the upload pipeline end to end.
func testJPEG(t *testing.T) []byte {
	t.Helper()
	img := image.NewRGBA(image.Rect(0, 0, 64, 48))
	for y := 0; y < 48; y++ {
		for x := 0; x < 64; x++ {
			img.Set(x, y, color.RGBA{R: uint8(x * 4), G: uint8(y * 5), B: 128, A: 255})
		}
	}
	var buf bytes.Buffer
	if err := jpeg.Encode(&buf, img, &jpeg.Options{Quality: 80}); err != nil {
		t.Fatalf("encode test jpeg: %v", err)
	}
	return buf.Bytes()
}

// requestWithIP builds a request carrying ip in the request context the way
// the clientIP middleware would, so rate-limit keys (which fold in the
// caller's IP) don't collide across subtests sharing one Valkey instance.
func requestWithIP(method, path, ip string, body []byte) *http.Request {
	var req *http.Request
	if body != nil {
		req = httptest.NewRequest(method, path, bytes.NewReader(body))
	} else {
		req = httptest.NewRequest(method, path, nil)
	}
	return req.WithContext(context.WithValue(req.Context(), ctxClientIP, netip.MustParseAddr(ip)))
}

// publicEventOpts configures newPublicEventFixture. Zero value is a public,
// open-RSVP, published event with guest photos closed.
type publicEventOpts struct {
	visibility  string // default "public"
	rsvpMode    string // default "open"
	password    string // non-empty sets visibility=password and hashes it (overrides visibility)
	photosOpen  bool
	unpublished bool // true leaves the event a draft (no slug gate to test, but useful for the 404 case)
}

// publicEventFixture is a Server plus one event built and (by default)
// published for the public-page / report / media handler tests, which all
// need the same "real event with a gate configuration" scaffolding.
type publicEventFixture struct {
	s       *Server
	ownerID uuid.UUID
	eventID uuid.UUID
	slug    string
}

// testEventContentJSON is a minimal but complete content document: a hero
// (supplies the title), a datetime (supplies starts_at, required to publish),
// an rsvp block using one of "birthday"'s occasion fields, and a
// guest_photos block whose open flag the caller controls.
func testEventContentJSON(photosOpen bool) string {
	return fmt.Sprintf(`[
		{"id":"hero1","type":"hero","title":"Sam's Party","subtitle":""},
		{"id":"dt1","type":"datetime","heading":"","start_local":"2027-06-01T18:00","end_local":"","timezone":"America/New_York","all_day":false},
		{"id":"rsvp1","type":"rsvp","heading":"RSVP","body":"","deadline_local":"","capacity":null,"max_party_size":4,"fields":[{"key":"dietary","required":false}],"questions":[]},
		{"id":"photos1","type":"guest_photos","heading":"Photos","body":"","open":%t}
	]`, photosOpen)
}

func newPublicEventFixture(t *testing.T, pool *pgxpool.Pool, rdb *redis.Client, opts publicEventOpts) *publicEventFixture {
	t.Helper()
	ctx := context.Background()
	q := store.New(pool)

	ownerID := uuid.Must(uuid.NewV7())
	if _, err := q.UpsertUserByEmail(ctx, store.UpsertUserByEmailParams{
		ID: ownerID, Email: "pub-owner-" + ownerID.String() + "@example.invalid",
	}); err != nil {
		t.Fatalf("create owner: %v", err)
	}

	event := createTestEvent(t, ctx, q, "birthday", "classic", &ownerID)

	occRow, err := q.GetOccasion(ctx, "birthday")
	if err != nil {
		t.Fatalf("get occasion: %v", err)
	}
	occ, err := content.ParseOccasion(occRow)
	if err != nil {
		t.Fatalf("parse occasion: %v", err)
	}
	saved, err := content.ValidateContent([]byte(testEventContentJSON(opts.photosOpen)), occ)
	if err != nil {
		t.Fatalf("validate content: %v", err)
	}
	if _, err := q.UpdateEventContent(ctx, store.UpdateEventContentParams{
		Content: saved.JSON, Title: saved.Title, StartsAt: saved.StartsAt,
		EventID: event.ID, Version: 1, UserID: ownerID,
	}); err != nil {
		t.Fatalf("update content: %v", err)
	}

	hasher := passhash.New(1)
	visibility := opts.visibility
	if visibility == "" {
		visibility = "public"
	}
	rsvpMode := opts.rsvpMode
	if rsvpMode == "" {
		rsvpMode = "open"
	}
	var passwordHash *string
	if opts.password != "" {
		visibility = "password"
		hash, err := hasher.Hash(ctx, opts.password)
		if err != nil {
			t.Fatalf("hash password: %v", err)
		}
		passwordHash = &hash
	}
	// The full id (dashes stripped) rather than a prefix: a UUIDv7's first
	// 12 hex characters are only its 48-bit millisecond timestamp, so two
	// fixtures built in the same millisecond under concurrent test load
	// would otherwise collide on events_slug_key.
	slug := "pub-fixture-" + strings.ReplaceAll(event.ID.String(), "-", "")
	if _, err := q.UpdateEventSettings(ctx, store.UpdateEventSettingsParams{
		Slug: &slug, Visibility: &visibility, PasswordHash: passwordHash, RsvpMode: &rsvpMode,
		EventID: event.ID, UserID: ownerID, Version: 2,
	}); err != nil {
		t.Fatalf("update settings: %v", err)
	}

	if !opts.unpublished {
		if _, err := q.PublishEvent(ctx, store.PublishEventParams{EventID: event.ID, UserID: ownerID, Version: 3}); err != nil {
			t.Fatalf("publish event: %v", err)
		}
	}

	t.Cleanup(func() {
		cleanupCtx := context.Background()
		if _, err := pool.Exec(cleanupCtx, "DELETE FROM events WHERE id = $1", event.ID); err != nil {
			t.Logf("cleanup event %s: %v", event.ID, err)
		}
		if _, err := pool.Exec(cleanupCtx, "DELETE FROM users WHERE id = $1", ownerID); err != nil {
			t.Logf("cleanup user %s: %v", ownerID, err)
		}
	})

	mediaStore, err := media.NewStore(t.TempDir())
	if err != nil {
		t.Fatalf("media.NewStore: %v", err)
	}
	tokens, err := token.NewKeys(testAuthSecret())
	if err != nil {
		t.Fatalf("token.NewKeys: %v", err)
	}

	s := &Server{
		pool: pool, q: q, rdb: rdb, limiter: ratelimit.New(rdb),
		cfg:    config.Config{SiteURL: "https://seeuthere.at"},
		tokens: tokens, hasher: hasher, media: mediaStore, images: media.NewProcessor(1),
	}
	return &publicEventFixture{s: s, ownerID: ownerID, eventID: event.ID, slug: slug}
}

// testGuest creates a guest of f's event and returns its id and a valid
// invite token for it.
func (f *publicEventFixture) testGuest(t *testing.T, name string, householdSize int32) (guestID uuid.UUID, inviteToken string) {
	t.Helper()
	ctx := context.Background()
	row, err := f.s.q.CreateGuest(ctx, store.CreateGuestParams{
		ID: uuid.Must(uuid.NewV7()), EventID: f.eventID, Name: name, HouseholdSize: householdSize, UserID: f.ownerID,
	})
	if err != nil {
		t.Fatalf("create guest: %v", err)
	}
	return row.ID, f.s.tokens.GuestToken(row.ID, row.TokenVersion)
}

// withSlugParam attaches the chi "slug" route param the way the router
// would for /v1/public/events/{slug}/*.
func withSlugParam(r *http.Request, slug string) *http.Request {
	return withRouteParam(r, "slug", slug)
}
