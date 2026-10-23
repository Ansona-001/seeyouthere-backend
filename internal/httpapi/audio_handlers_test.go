package httpapi

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/ansonarose/seeyouthere-backend/internal/media"
	"github.com/ansonarose/seeyouthere-backend/internal/store"
)

// synthMP3 returns a clean MPEG-1 Layer III stream of n 128 kbps / 44.1 kHz
// frames (417 bytes, 26.1 ms each): 40 frames is just over the 1 s minimum.
func synthMP3(n int) []byte {
	frame := make([]byte, 417)
	copy(frame, []byte{0xFF, 0xFB, 0x90, 0x00})
	return bytes.Repeat(frame, n)
}

// synthLongMP3 is a valid 32 kbps stream a little over MaxAudioDuration but
// well under MaxAudioBytes.
func synthLongMP3() []byte {
	frame := make([]byte, 104)
	copy(frame, []byte{0xFF, 0xFB, 0x10, 0x00})
	return bytes.Repeat(frame, 23_000)
}

// audioRequest is uploadRequest with the audio/mpeg content type.
func audioRequest(userID, eventID uuid.UUID, body []byte) *http.Request {
	req := uploadRequest(userID, eventID, body, nextTestIP())
	req.Header.Set("Content-Type", "audio/mpeg")
	return req
}

type audioBody struct {
	Audio struct {
		ID         uuid.UUID `json:"id"`
		Src        string    `json:"src"`
		SizeBytes  int64     `json:"size_bytes"`
		DurationMs int32     `json:"duration_ms"`
		CreatedAt  time.Time `json:"created_at"`
	} `json:"audio"`
}

// uploadAudio uploads body through the real handler and expects 201.
func uploadAudio(t *testing.T, f eventTestFixture, userID uuid.UUID, body []byte) audioBody {
	t.Helper()
	rec := httptest.NewRecorder()
	f.s.handleUploadEventAudio(rec, audioRequest(userID, f.eventID, body))
	if rec.Code != http.StatusCreated {
		t.Fatalf("upload status = %d, body = %s", rec.Code, rec.Body.String())
	}
	var out audioBody
	if err := json.Unmarshal(rec.Body.Bytes(), &out); err != nil {
		t.Fatalf("parse upload response: %v", err)
	}
	return out
}

// audioObjectExists reports whether the audio object is in the object store.
func audioObjectExists(t *testing.T, f eventTestFixture, eventID, audioID uuid.UUID) bool {
	t.Helper()
	obj, err := f.s.media.OpenAudio(context.Background(), eventID, audioID, "")
	if errors.Is(err, media.ErrNotFound) {
		return false
	}
	if err != nil {
		t.Fatalf("open audio: %v", err)
	}
	_ = obj.Body.Close()
	return true
}

func countAudioRows(t *testing.T, pool *pgxpool.Pool, eventID uuid.UUID) int {
	t.Helper()
	var n int
	if err := pool.QueryRow(context.Background(), "SELECT count(*) FROM event_audio WHERE event_id = $1", eventID).Scan(&n); err != nil {
		t.Fatalf("count audio rows: %v", err)
	}
	return n
}

// pinTemplate pins the event to the latest published version of a seeded
// template (whatever the template's catalogue status), or skips the test when
// this database has none.
func pinTemplate(t *testing.T, pool *pgxpool.Pool, eventID uuid.UUID, slug string) {
	t.Helper()
	tag, err := pool.Exec(context.Background(), `
		UPDATE events SET template_id = v.template_id, template_version = v.version
		FROM (SELECT tv.template_id, tv.version FROM template_versions tv
		      JOIN templates t ON t.id = tv.template_id
		      WHERE t.slug = $2 AND tv.published_at IS NOT NULL
		      ORDER BY tv.version DESC LIMIT 1) v
		WHERE events.id = $1`, eventID, slug)
	if err != nil {
		t.Fatalf("pin %s: %v", slug, err)
	}
	if tag.RowsAffected() == 0 {
		t.Skipf("template %s has no published version in this database", slug)
	}
}

func eventMusicState(t *testing.T, pool *pgxpool.Pool, eventID uuid.UUID) (source string, version int32) {
	t.Helper()
	if err := pool.QueryRow(context.Background(), "SELECT music_source, version FROM events WHERE id = $1", eventID).Scan(&source, &version); err != nil {
		t.Fatalf("read music state: %v", err)
	}
	return source, version
}

func TestUploadEventAudio_Matrix(t *testing.T) {
	pool := dbTestPool(t)
	rdb := dbTestRedis(t)
	f := newMediaTestFixture(t, pool, rdb)
	good := synthMP3(40)
	ctx := context.Background()

	cases := []struct {
		name       string
		userID     uuid.UUID
		ctype      string
		body       []byte
		wantStatus int
		wantCode   string
	}{
		{"owner", f.ownerID, "audio/mpeg", good, http.StatusCreated, ""},
		{"editor", f.editorID, "audio/mpeg", good, http.StatusCreated, ""},
		{"content type with parameter", f.ownerID, "audio/mpeg; foo=bar", good, http.StatusCreated, ""},
		{"viewer", f.viewerID, "audio/mpeg", good, http.StatusForbidden, "forbidden"},
		{"stranger", f.strangerID, "audio/mpeg", good, http.StatusNotFound, "not_found"},
		{"anonymous", uuid.Nil, "audio/mpeg", good, http.StatusUnauthorized, "unauthorized"},
		{"wrong content type", f.ownerID, "audio/ogg", good, http.StatusUnsupportedMediaType, "unsupported_audio"},
		{"image content type", f.ownerID, "image/jpeg", good, http.StatusUnsupportedMediaType, "unsupported_audio"},
		{"no content type", f.ownerID, "", good, http.StatusUnsupportedMediaType, "unsupported_audio"},
		{"not an mp3", f.ownerID, "audio/mpeg", []byte("RIFF....WAVEfmt this is not an mp3 at all"), http.StatusUnprocessableEntity, "invalid_audio"},
		{"empty body", f.ownerID, "audio/mpeg", []byte{}, http.StatusUnprocessableEntity, "invalid_audio"},
		{"trailing junk", f.ownerID, "audio/mpeg", append(synthMP3(40), 1, 2, 3), http.StatusUnprocessableEntity, "invalid_audio"},
		{"too short", f.ownerID, "audio/mpeg", synthMP3(5), http.StatusUnprocessableEntity, "invalid_audio"},
		{"over the size cap", f.ownerID, "audio/mpeg", synthMP3(media.MaxAudioBytes/417 + 100), http.StatusRequestEntityTooLarge, "body_too_large"},
		{"too long", f.ownerID, "audio/mpeg", synthLongMP3(), http.StatusUnprocessableEntity, "audio_too_long"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			// More cases than the hourly limit allows one user; the limit has its own test.
			if err := rdb.Del(ctx, "rl:audio:upload:user:"+c.userID.String()).Err(); err != nil {
				t.Fatalf("reset rate limit: %v", err)
			}
			req := audioRequest(c.userID, f.eventID, c.body)
			if c.ctype == "" {
				req.Header.Del("Content-Type")
			} else {
				req.Header.Set("Content-Type", c.ctype)
			}
			rec := httptest.NewRecorder()
			f.s.handleUploadEventAudio(rec, req)
			if rec.Code != c.wantStatus {
				t.Fatalf("status = %d, want %d; body = %s", rec.Code, c.wantStatus, rec.Body.String())
			}
			if c.wantCode != "" && !strings.Contains(rec.Body.String(), `"`+c.wantCode+`"`) {
				t.Errorf("body = %s, want code %s", rec.Body.String(), c.wantCode)
			}
		})
	}

	t.Run("invalid audio message is the user-facing one", func(t *testing.T) {
		rdb.Del(ctx, "rl:audio:upload:user:"+f.ownerID.String())
		rec := httptest.NewRecorder()
		f.s.handleUploadEventAudio(rec, audioRequest(f.ownerID, f.eventID, []byte("nope")))
		if !strings.Contains(rec.Body.String(), "That doesn't look like a standard MP3 file. Please export it as a standard MP3 and try again.") {
			t.Errorf("body = %s", rec.Body.String())
		}
	})

	t.Run("unknown event id", func(t *testing.T) {
		rdb.Del(ctx, "rl:audio:upload:user:"+f.ownerID.String())
		rec := httptest.NewRecorder()
		f.s.handleUploadEventAudio(rec, audioRequest(f.ownerID, uuid.Must(uuid.NewV7()), good))
		if rec.Code != http.StatusNotFound {
			t.Fatalf("status = %d, want 404", rec.Code)
		}
	})
}

func TestUploadEventAudio_TakenDown(t *testing.T) {
	pool := dbTestPool(t)
	f := newMediaTestFixture(t, pool, dbTestRedis(t))
	if _, err := pool.Exec(context.Background(),
		"UPDATE events SET status = 'taken_down', slug = 'tk-' || replace(id::text, '-', '') WHERE id = $1", f.eventID); err != nil {
		t.Fatalf("take down: %v", err)
	}
	rec := httptest.NewRecorder()
	f.s.handleUploadEventAudio(rec, audioRequest(f.ownerID, f.eventID, synthMP3(40)))
	if rec.Code != http.StatusConflict || !strings.Contains(rec.Body.String(), `"event_taken_down"`) {
		t.Fatalf("status = %d, body = %s, want 409 event_taken_down", rec.Code, rec.Body.String())
	}
	if n := countAudioRows(t, pool, f.eventID); n != 0 {
		t.Errorf("audio rows = %d, want 0", n)
	}
}

func TestUploadEventAudio_StoresAndReplaces(t *testing.T) {
	pool := dbTestPool(t)
	f := newMediaTestFixture(t, pool, dbTestRedis(t))

	first := uploadAudio(t, f, f.ownerID, synthMP3(40))
	if want := fmt.Sprintf("/media/%s/%s/audio.mp3", f.eventID, first.Audio.ID); first.Audio.Src != want {
		t.Errorf("src = %q, want %q", first.Audio.Src, want)
	}
	if first.Audio.SizeBytes != 40*417 || first.Audio.DurationMs != 1044 || first.Audio.CreatedAt.IsZero() {
		t.Errorf("audio = %+v, want 16680 bytes / 1044 ms", first.Audio)
	}
	if n := countAudioRows(t, pool, f.eventID); n != 1 {
		t.Fatalf("audio rows = %d, want 1", n)
	}
	if !audioObjectExists(t, f, f.eventID, first.Audio.ID) {
		t.Fatal("uploaded object missing")
	}
	source, version := eventMusicState(t, pool, f.eventID)
	if source != "template" || version != 1 {
		t.Errorf("music_source/version = %s/%d, want template/1 (upload must not change them)", source, version)
	}

	second := uploadAudio(t, f, f.editorID, synthMP3(80))
	if second.Audio.ID == first.Audio.ID {
		t.Fatal("replacement reused the audio id")
	}
	if n := countAudioRows(t, pool, f.eventID); n != 1 {
		t.Fatalf("audio rows after replace = %d, want 1", n)
	}
	if audioObjectExists(t, f, f.eventID, first.Audio.ID) {
		t.Error("replaced object still exists")
	}
	if !audioObjectExists(t, f, f.eventID, second.Audio.ID) {
		t.Error("new object missing")
	}
}

func TestUploadEventAudio_QuotaChargedOnGrowthOnly(t *testing.T) {
	pool := dbTestPool(t)
	f := newMediaTestFixture(t, pool, dbTestRedis(t))
	f.s.cfg.MediaUserQuotaBytes = 20_000 // fits one 16680-byte track, not two

	first := uploadAudio(t, f, f.ownerID, synthMP3(40))
	// Same size replacement: net growth is zero, so it fits although
	// old + new would exceed the cap.
	second := uploadAudio(t, f, f.ownerID, synthMP3(40))

	rec := httptest.NewRecorder()
	f.s.handleUploadEventAudio(rec, audioRequest(f.ownerID, f.eventID, synthMP3(60)))
	if rec.Code != http.StatusConflict || !strings.Contains(rec.Body.String(), `"storage_quota"`) {
		t.Fatalf("growth over the cap: status = %d, body = %s, want 409 storage_quota", rec.Code, rec.Body.String())
	}
	// The failed replacement left the previous track and no new object.
	if n := countAudioRows(t, pool, f.eventID); n != 1 || !audioObjectExists(t, f, f.eventID, second.Audio.ID) {
		t.Errorf("after a refused replacement: rows = %d, previous object kept = %v", n, audioObjectExists(t, f, f.eventID, second.Audio.ID))
	}
	if audioObjectExists(t, f, f.eventID, first.Audio.ID) {
		t.Error("first object should have been replaced")
	}
	// A smaller track always fits.
	uploadAudio(t, f, f.ownerID, synthMP3(39))
}

func TestUploadEventAudio_RateLimited(t *testing.T) {
	pool := dbTestPool(t)
	f := newMediaTestFixture(t, pool, dbTestRedis(t))
	for i := 0; i < audioUploadsPerHour; i++ {
		rec := httptest.NewRecorder()
		f.s.handleUploadEventAudio(rec, audioRequest(f.ownerID, f.eventID, []byte("junk")))
		if rec.Code != http.StatusUnprocessableEntity {
			t.Fatalf("upload %d: status = %d, want 422", i, rec.Code)
		}
	}
	rec := httptest.NewRecorder()
	f.s.handleUploadEventAudio(rec, audioRequest(f.ownerID, f.eventID, synthMP3(40)))
	if rec.Code != http.StatusTooManyRequests {
		t.Fatalf("status = %d, want 429; body = %s", rec.Code, rec.Body.String())
	}
	// Another user is not affected.
	rec = httptest.NewRecorder()
	f.s.handleUploadEventAudio(rec, audioRequest(f.editorID, f.eventID, synthMP3(40)))
	if rec.Code != http.StatusCreated {
		t.Fatalf("other user: status = %d, want 201", rec.Code)
	}
}

func TestUploadEventAudio_ConcurrentLeavesOneRow(t *testing.T) {
	pool := dbTestPool(t)
	f := newMediaTestFixture(t, pool, dbTestRedis(t))

	const n = 4
	var wg sync.WaitGroup
	results := make([]audioBody, n)
	codes := make([]int, n)
	for i := 0; i < n; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			rec := httptest.NewRecorder()
			f.s.handleUploadEventAudio(rec, audioRequest(f.ownerID, f.eventID, synthMP3(40+i)))
			codes[i] = rec.Code
			_ = json.Unmarshal(rec.Body.Bytes(), &results[i])
		}()
	}
	wg.Wait()

	if got := countAudioRows(t, pool, f.eventID); got != 1 {
		t.Fatalf("audio rows = %d, want exactly 1", got)
	}
	var liveID uuid.UUID
	if err := pool.QueryRow(context.Background(), "SELECT id FROM event_audio WHERE event_id = $1", f.eventID).Scan(&liveID); err != nil {
		t.Fatalf("read live audio: %v", err)
	}
	for i := 0; i < n; i++ {
		if codes[i] != http.StatusCreated {
			t.Errorf("upload %d: status = %d, want 201 (uploads are serialised, last one wins)", i, codes[i])
			continue
		}
		if exists := audioObjectExists(t, f, f.eventID, results[i].Audio.ID); exists != (results[i].Audio.ID == liveID) {
			t.Errorf("upload %d: object exists = %v, live = %v", i, exists, results[i].Audio.ID == liveID)
		}
	}
}

func TestUploadEventAudio_StorageFailureLeavesNoRow(t *testing.T) {
	pool := dbTestPool(t)
	f, blobs := newMediaTestFixtureBlobs(t, pool, dbTestRedis(t))
	blobs.putErr = errors.New("boom")
	rec := httptest.NewRecorder()
	f.s.handleUploadEventAudio(rec, audioRequest(f.ownerID, f.eventID, synthMP3(40)))
	if rec.Code != http.StatusServiceUnavailable {
		t.Fatalf("status = %d, want 503; body = %s", rec.Code, rec.Body.String())
	}
	if n := countAudioRows(t, pool, f.eventID); n != 0 {
		t.Errorf("audio rows = %d, want 0", n)
	}
}

func TestDeleteEventAudio(t *testing.T) {
	pool := dbTestPool(t)
	f := newMediaTestFixture(t, pool, dbTestRedis(t))
	ctx := context.Background()

	del := func(userID uuid.UUID) *httptest.ResponseRecorder {
		req := requestAs(http.MethodDelete, "/", userID)
		req = withRouteID(req, f.eventID)
		rec := httptest.NewRecorder()
		f.s.handleDeleteEventAudio(rec, req)
		return rec
	}

	t.Run("404 when the event has no audio", func(t *testing.T) {
		if rec := del(f.ownerID); rec.Code != http.StatusNotFound {
			t.Fatalf("status = %d, want 404", rec.Code)
		}
	})

	up := uploadAudio(t, f, f.ownerID, synthMP3(40))
	if _, err := pool.Exec(ctx, "UPDATE events SET music_source = 'custom' WHERE id = $1", f.eventID); err != nil {
		t.Fatalf("set custom: %v", err)
	}
	_, versionBefore := eventMusicState(t, pool, f.eventID)

	for _, c := range []struct {
		name   string
		userID uuid.UUID
		want   int
	}{
		{"anonymous", uuid.Nil, http.StatusUnauthorized},
		{"viewer", f.viewerID, http.StatusForbidden},
		{"stranger", f.strangerID, http.StatusNotFound},
	} {
		t.Run(c.name, func(t *testing.T) {
			if rec := del(c.userID); rec.Code != c.want {
				t.Fatalf("status = %d, want %d; body = %s", rec.Code, c.want, rec.Body.String())
			}
			if n := countAudioRows(t, pool, f.eventID); n != 1 {
				t.Errorf("audio rows = %d, want 1 (refused delete must keep it)", n)
			}
		})
	}

	t.Run("editor deletes: 204, custom falls back to none, file gone, version unchanged", func(t *testing.T) {
		rec := del(f.editorID)
		if rec.Code != http.StatusNoContent || rec.Body.Len() != 0 {
			t.Fatalf("status = %d, body = %s, want 204 empty", rec.Code, rec.Body.String())
		}
		if n := countAudioRows(t, pool, f.eventID); n != 0 {
			t.Errorf("audio rows = %d, want 0", n)
		}
		if audioObjectExists(t, f, f.eventID, up.Audio.ID) {
			t.Error("object still exists")
		}
		source, version := eventMusicState(t, pool, f.eventID)
		if source != "none" || version != versionBefore {
			t.Errorf("music_source/version = %s/%d, want none/%d", source, version, versionBefore)
		}
		if rec := del(f.ownerID); rec.Code != http.StatusNotFound {
			t.Errorf("second delete: status = %d, want 404", rec.Code)
		}
	})

	t.Run("other music sources are left alone", func(t *testing.T) {
		uploadAudio(t, f, f.ownerID, synthMP3(40))
		if _, err := pool.Exec(ctx, "UPDATE events SET music_source = 'template' WHERE id = $1", f.eventID); err != nil {
			t.Fatalf("set template: %v", err)
		}
		if rec := del(f.ownerID); rec.Code != http.StatusNoContent {
			t.Fatalf("status = %d, want 204", rec.Code)
		}
		if source, _ := eventMusicState(t, pool, f.eventID); source != "template" {
			t.Errorf("music_source = %s, want template", source)
		}
	})
}

// --- serving ---

func TestServeAudio_Matrix(t *testing.T) {
	pool := dbTestPool(t)
	f, blobs := newMediaTestFixtureBlobs(t, pool, dbTestRedis(t))
	f.s.audioSem = make(chan struct{}, audioStreams)
	ctx := context.Background()
	h := f.s.Routes()

	mp3 := synthMP3(40)
	up := uploadAudio(t, f, f.ownerID, mp3)
	imageID, _ := uploadHostMedia(t, f, f.ownerID, f.eventID)
	otherEvent := createTestEvent(t, ctx, f.s.q, "birthday", "classic", &f.ownerID)
	t.Cleanup(func() { _, _ = pool.Exec(context.Background(), "DELETE FROM events WHERE id = $1", otherEvent.ID) })

	audioPath := fmt.Sprintf("/media/%s/%s/audio.mp3", f.eventID, up.Audio.ID)

	t.Run("serves the track with the full header set", func(t *testing.T) {
		rec := getMedia(h, audioPath, nextTestIP(), nil)
		if rec.Code != http.StatusOK {
			t.Fatalf("status = %d, body = %s", rec.Code, rec.Body.String())
		}
		want := map[string]string{
			"Content-Type":                 "audio/mpeg",
			"Cache-Control":                "public, max-age=3600",
			"X-Content-Type-Options":       "nosniff",
			"Content-Security-Policy":      "default-src 'none'",
			"Cross-Origin-Resource-Policy": "same-site",
			"Content-Length":               fmt.Sprint(len(mp3)),
		}
		for k, v := range want {
			if got := rec.Header().Get(k); got != v {
				t.Errorf("%s = %q, want %q", k, got, v)
			}
		}
		if rec.Header().Get("ETag") == "" {
			t.Error("missing ETag")
		}
		if !bytes.Equal(rec.Body.Bytes(), mp3) {
			t.Error("body differs from the stored track")
		}
	})

	t.Run("matching If-None-Match gives 304 without a body", func(t *testing.T) {
		etag := getMedia(h, audioPath, nextTestIP(), nil).Header().Get("ETag")
		rec := getMedia(h, audioPath, nextTestIP(), map[string]string{"If-None-Match": etag})
		if rec.Code != http.StatusNotModified || rec.Body.Len() != 0 {
			t.Fatalf("status = %d, body = %d bytes, want 304 and empty", rec.Code, rec.Body.Len())
		}
		if rec.Header().Get("ETag") != etag || rec.Header().Get("Content-Type") != "audio/mpeg" ||
			rec.Header().Get("X-Content-Type-Options") != "nosniff" {
			t.Errorf("304 headers = %v", rec.Header())
		}
	})

	notFound := []struct {
		name string
		path string
	}{
		{"audio id under another event", fmt.Sprintf("/media/%s/%s/audio.mp3", otherEvent.ID, up.Audio.ID)},
		{"unknown audio id", fmt.Sprintf("/media/%s/%s/audio.mp3", f.eventID, uuid.Must(uuid.NewV7()))},
		{"image id on the audio route", fmt.Sprintf("/media/%s/%s/audio.mp3", f.eventID, imageID)},
		{"audio id on the image route", fmt.Sprintf("/media/%s/%s/480.jpg", f.eventID, up.Audio.ID)},
		{"uppercase uuids", fmt.Sprintf("/media/%s/%s/audio.mp3", strings.ToUpper(f.eventID.String()), strings.ToUpper(up.Audio.ID.String()))},
		{"uppercase event id", fmt.Sprintf("/media/%s/%s/audio.mp3", strings.ToUpper(f.eventID.String()), up.Audio.ID)},
		{"uppercase extension", fmt.Sprintf("/media/%s/%s/audio.MP3", f.eventID, up.Audio.ID)},
		{"other file name", fmt.Sprintf("/media/%s/%s/audio.ogg", f.eventID, up.Audio.ID)},
		{"not a uuid", fmt.Sprintf("/media/%s/x/audio.mp3", f.eventID)},
	}
	for _, c := range notFound {
		t.Run(c.name, func(t *testing.T) {
			before := blobs.calls()
			rec := getMedia(h, c.path, nextTestIP(), nil)
			if rec.Code != http.StatusNotFound {
				t.Fatalf("status = %d, want 404; body = %s", rec.Code, rec.Body.String())
			}
			if blobs.calls() != before {
				t.Error("storage backend was called for a non-servable file")
			}
		})
	}

	for _, c := range []struct{ name, setup, undo string }{
		{"event taken down", "UPDATE events SET status = 'taken_down', slug = 'tk-' || replace(id::text, '-', '') WHERE id = $1",
			"UPDATE events SET status = 'draft', slug = NULL WHERE id = $1"},
		{"event soft-deleted", "UPDATE events SET deleted_at = now() WHERE id = $1", "UPDATE events SET deleted_at = NULL WHERE id = $1"},
	} {
		t.Run(c.name, func(t *testing.T) {
			if _, err := pool.Exec(ctx, c.setup, f.eventID); err != nil {
				t.Fatalf("setup: %v", err)
			}
			t.Cleanup(func() { _, _ = pool.Exec(ctx, c.undo, f.eventID) })
			before := blobs.calls()
			rec := getMedia(h, audioPath, nextTestIP(), nil)
			if rec.Code != http.StatusNotFound || blobs.calls() != before {
				t.Fatalf("status = %d (storage calls %d -> %d), want 404 with no storage call", rec.Code, before, blobs.calls())
			}
		})
	}

	t.Run("deleted track is 404", func(t *testing.T) {
		if _, err := pool.Exec(ctx, "DELETE FROM event_audio WHERE id = $1", up.Audio.ID); err != nil {
			t.Fatalf("delete row: %v", err)
		}
		if rec := getMedia(h, audioPath, nextTestIP(), nil); rec.Code != http.StatusNotFound {
			t.Fatalf("status = %d, want 404", rec.Code)
		}
	})
}

func TestServeAudio_BusyWhenAllSlotsTaken(t *testing.T) {
	pool := dbTestPool(t)
	f := newMediaTestFixture(t, pool, dbTestRedis(t))
	f.s.audioSem = make(chan struct{}, 1)
	up := uploadAudio(t, f, f.ownerID, synthMP3(40))
	f.s.audioSem <- struct{}{} // the only slot is taken

	req := httptest.NewRequest(http.MethodGet, fmt.Sprintf("/media/%s/%s/audio.mp3", f.eventID, up.Audio.ID), nil)
	req.RemoteAddr = nextTestIP() + ":4000"
	rec := httptest.NewRecorder()
	f.s.Routes().ServeHTTP(rec, req)
	if rec.Code != http.StatusServiceUnavailable || rec.Header().Get("Retry-After") == "" || len(f.s.mediaSem) != 0 {
		t.Fatalf("status = %d, mediaSem in use = %d; want 503 busy without taking an image stream slot", rec.Code, len(f.s.mediaSem))
	}
}

func TestServeAudio_PerIPLimit(t *testing.T) {
	pool := dbTestPool(t)
	rdb := dbTestRedis(t)
	f := newMediaTestFixture(t, pool, rdb)
	f.s.audioSem = make(chan struct{}, audioStreams)
	up := uploadAudio(t, f, f.ownerID, synthMP3(40))
	path := fmt.Sprintf("/media/%s/%s/audio.mp3", f.eventID, up.Audio.ID)
	h := f.s.Routes()

	ip, other := nextTestIP(), nextTestIP()
	t.Cleanup(func() {
		for _, k := range []string{"rl:media:audio:ip:" + ip, "rl:media:audio:ip:" + other} {
			rdb.Del(context.Background(), k)
		}
	})
	for i := 1; i <= mediaAudioPerIPLimit; i++ {
		if rec := getMedia(h, path, ip, nil); rec.Code != http.StatusOK {
			t.Fatalf("request %d: status = %d, want 200", i, rec.Code)
		}
	}
	rec := getMedia(h, path, ip, nil)
	if rec.Code != http.StatusTooManyRequests || rec.Header().Get("Retry-After") == "" {
		t.Fatalf("over the limit: status = %d, Retry-After = %q; want 429 with Retry-After", rec.Code, rec.Header().Get("Retry-After"))
	}
	if rec := getMedia(h, path, other, nil); rec.Code != http.StatusOK {
		t.Fatalf("other IP: status = %d, want 200", rec.Code)
	}
}

func TestUploadAudio_BusyWhenAllSlotsTaken(t *testing.T) {
	pool := dbTestPool(t)
	f := newMediaTestFixture(t, pool, dbTestRedis(t))
	for range cap(f.s.audioUploadSem) {
		f.s.audioUploadSem <- struct{}{}
	}
	rec := httptest.NewRecorder()
	f.s.handleUploadEventAudio(rec, audioRequest(f.ownerID, f.eventID, synthMP3(40)))
	if rec.Code != http.StatusServiceUnavailable || !strings.Contains(rec.Body.String(), `"busy"`) {
		t.Fatalf("status = %d, body = %s; want 503 busy", rec.Code, rec.Body.String())
	}

	<-f.s.audioUploadSem // one slot frees up: the upload goes through and releases it
	rec = httptest.NewRecorder()
	f.s.handleUploadEventAudio(rec, audioRequest(f.ownerID, f.eventID, synthMP3(40)))
	if rec.Code != http.StatusCreated || len(f.s.audioUploadSem) != cap(f.s.audioUploadSem)-1 {
		t.Fatalf("status = %d, slots held = %d; want 201 and the held slot only", rec.Code, len(f.s.audioUploadSem))
	}
}

// --- PATCH music ---

type eventMusicBody struct {
	Event struct {
		Version int32 `json:"version"`
		Music   struct {
			Source            string          `json:"source"`
			TemplateAvailable bool            `json:"template_available"`
			Audio             json.RawMessage `json:"audio"`
		} `json:"music"`
	} `json:"event"`
}

func patchEvent(f eventTestFixture, userID uuid.UUID, body string) *httptest.ResponseRecorder {
	req := requestAs(http.MethodPatch, "/", userID)
	req = withRouteID(req, f.eventID)
	req.Header.Set("Content-Type", "application/json")
	req.Body = newJSONBody(body)
	rec := httptest.NewRecorder()
	f.s.handlePatchEvent(rec, req)
	return rec
}

func TestPatchEvent_Music(t *testing.T) {
	pool := dbTestPool(t)
	f := newMediaTestFixture(t, pool, dbTestRedis(t))

	version := func() int32 { _, v := eventMusicState(t, pool, f.eventID); return v }
	body := func(source string) string {
		return fmt.Sprintf(`{"version":%d,"music":{"source":%q}}`, version(), source)
	}

	t.Run("custom without audio is no_audio", func(t *testing.T) {
		rec := patchEvent(f, f.ownerID, body("custom"))
		if rec.Code != http.StatusBadRequest || !containsAll(rec.Body.String(), `"validation_failed"`, `"music.source"`, `"no_audio"`) {
			t.Fatalf("status = %d, body = %s", rec.Code, rec.Body.String())
		}
		if source, _ := eventMusicState(t, pool, f.eventID); source != "template" {
			t.Errorf("music_source = %s, want template", source)
		}
	})

	t.Run("none", func(t *testing.T) {
		v := version()
		rec := patchEvent(f, f.ownerID, body("none"))
		if rec.Code != http.StatusOK {
			t.Fatalf("status = %d, body = %s", rec.Code, rec.Body.String())
		}
		var out eventMusicBody
		if err := json.Unmarshal(rec.Body.Bytes(), &out); err != nil {
			t.Fatal(err)
		}
		if out.Event.Music.Source != "none" || out.Event.Version != v+1 || string(out.Event.Music.Audio) != "null" {
			t.Errorf("music = %+v version = %d", out.Event.Music, out.Event.Version)
		}
	})

	up := uploadAudio(t, f, f.ownerID, synthMP3(40))

	t.Run("custom with audio, by an editor", func(t *testing.T) {
		rec := patchEvent(f, f.editorID, body("custom"))
		if rec.Code != http.StatusOK {
			t.Fatalf("status = %d, body = %s", rec.Code, rec.Body.String())
		}
		var out struct {
			Event struct {
				Music struct {
					Source string `json:"source"`
					Audio  struct {
						ID  uuid.UUID `json:"id"`
						Src string    `json:"src"`
					} `json:"audio"`
				} `json:"music"`
			} `json:"event"`
		}
		if err := json.Unmarshal(rec.Body.Bytes(), &out); err != nil {
			t.Fatal(err)
		}
		if out.Event.Music.Source != "custom" || out.Event.Music.Audio.ID != up.Audio.ID || out.Event.Music.Audio.Src != up.Audio.Src {
			t.Errorf("music = %+v, want custom with the uploaded track", out.Event.Music)
		}
	})

	t.Run("template", func(t *testing.T) {
		rec := patchEvent(f, f.ownerID, body("template"))
		if rec.Code != http.StatusOK {
			t.Fatalf("status = %d, body = %s", rec.Code, rec.Body.String())
		}
		var out eventMusicBody
		_ = json.Unmarshal(rec.Body.Bytes(), &out)
		// The track stays attached while another source is chosen.
		if out.Event.Music.Source != "template" || string(out.Event.Music.Audio) == "null" {
			t.Errorf("music = %+v", out.Event.Music)
		}
	})

	for _, c := range []struct {
		name, body string
	}{
		{"unknown source", `"music":{"source":"loud"}`},
		{"empty source", `"music":{"source":""}`},
		{"unknown music field", `"music":{"source":"none","volume":3}`},
		{"music not an object", `"music":"none"`},
	} {
		t.Run(c.name, func(t *testing.T) {
			rec := patchEvent(f, f.ownerID, fmt.Sprintf(`{"version":%d,%s}`, version(), c.body))
			if rec.Code != http.StatusBadRequest {
				t.Fatalf("status = %d, want 400; body = %s", rec.Code, rec.Body.String())
			}
		})
	}
	t.Run("bad source names the field", func(t *testing.T) {
		rec := patchEvent(f, f.ownerID, body("loud"))
		if !containsAll(rec.Body.String(), `"validation_failed"`, `"music.source"`) {
			t.Errorf("body = %s", rec.Body.String())
		}
	})

	t.Run("viewer", func(t *testing.T) {
		if rec := patchEvent(f, f.viewerID, body("none")); rec.Code != http.StatusForbidden {
			t.Fatalf("status = %d, want 403", rec.Code)
		}
	})

	t.Run("stale version", func(t *testing.T) {
		rec := patchEvent(f, f.ownerID, `{"version":1,"music":{"source":"none"}}`)
		if rec.Code != http.StatusConflict {
			t.Fatalf("status = %d, want 409; body = %s", rec.Code, rec.Body.String())
		}
	})
}

func TestPatchEvent_MusicAnonymousDraft(t *testing.T) {
	pool := dbTestPool(t)
	f := newMediaTestFixture(t, pool, dbTestRedis(t))
	ctx := context.Background()
	draft := createTestEvent(t, ctx, f.s.q, "birthday", "classic", nil)
	t.Cleanup(func() { _, _ = pool.Exec(context.Background(), "DELETE FROM events WHERE id = $1", draft.ID) })
	draftToken, err := randomToken()
	if err != nil {
		t.Fatal(err)
	}
	sum := sha256.Sum256([]byte(draftToken))
	if err := f.s.q.CreateAnonDraft(ctx, store.CreateAnonDraftParams{
		ID: uuid.Must(uuid.NewV7()), CookieHash: sum[:], EventID: draft.ID, ExpiresAt: time.Now().Add(anonDraftTTL),
	}); err != nil {
		t.Fatalf("create anon draft: %v", err)
	}
	patch := func(body string) *httptest.ResponseRecorder {
		req := requestWithIP(http.MethodPatch, "/", nextTestIP(), nil)
		req = withRouteID(req, draft.ID)
		req.AddCookie(&http.Cookie{Name: draftCookieName, Value: draftToken})
		req.Header.Set("Content-Type", "application/json")
		req.Body = newJSONBody(body)
		rec := httptest.NewRecorder()
		f.s.handlePatchEvent(rec, req)
		return rec
	}

	if rec := patch(`{"version":1,"music":{"source":"custom"}}`); rec.Code != http.StatusBadRequest || !strings.Contains(rec.Body.String(), `"no_audio"`) {
		t.Fatalf("custom: status = %d, body = %s, want 400 no_audio", rec.Code, rec.Body.String())
	}
	if rec := patch(`{"version":1,"music":{"source":"none"}}`); rec.Code != http.StatusOK {
		t.Fatalf("none: status = %d, body = %s", rec.Code, rec.Body.String())
	}
	if source, _ := eventMusicState(t, pool, draft.ID); source != "none" {
		t.Errorf("music_source = %s, want none", source)
	}
	if rec := patch(`{"version":2,"music":{"source":"template"}}`); rec.Code != http.StatusOK {
		t.Fatalf("template: status = %d, body = %s", rec.Code, rec.Body.String())
	}
}

// --- event response music block ---

func TestGetEvent_MusicBlock(t *testing.T) {
	pool := dbTestPool(t)
	f := newMediaTestFixture(t, pool, dbTestRedis(t))
	ctx := context.Background()

	get := func(eventID uuid.UUID) eventMusicBody {
		req := withRouteID(requestAs(http.MethodGet, "/", f.ownerID), eventID)
		rec := httptest.NewRecorder()
		f.s.handleGetEvent(rec, req)
		if rec.Code != http.StatusOK {
			t.Fatalf("status = %d, body = %s", rec.Code, rec.Body.String())
		}
		var out eventMusicBody
		if err := json.Unmarshal(rec.Body.Bytes(), &out); err != nil {
			t.Fatal(err)
		}
		return out
	}

	cases := []struct {
		template string
		want     bool
	}{{"classic", false}, {"mimosa", true}, {"harvest", true}}
	for _, c := range cases {
		t.Run(c.template, func(t *testing.T) {
			var id uuid.UUID
			if c.template == "classic" {
				id = f.eventID
			} else {
				ev := createTestEvent(t, ctx, f.s.q, "birthday", "classic", &f.ownerID)
				t.Cleanup(func() { _, _ = pool.Exec(context.Background(), "DELETE FROM events WHERE id = $1", ev.ID) })
				pinTemplate(t, pool, ev.ID, c.template)
				id = ev.ID
			}
			out := get(id)
			if out.Event.Music.Source != "template" || out.Event.Music.TemplateAvailable != c.want || string(out.Event.Music.Audio) != "null" {
				t.Errorf("music = %+v, want template / available=%v / audio null", out.Event.Music, c.want)
			}
			// template_available ignores the chosen source.
			if _, err := pool.Exec(ctx, "UPDATE events SET music_source = 'none' WHERE id = $1", id); err != nil {
				t.Fatal(err)
			}
			if out := get(id); out.Event.Music.Source != "none" || out.Event.Music.TemplateAvailable != c.want {
				t.Errorf("after none: music = %+v, want available=%v", out.Event.Music, c.want)
			}
		})
	}

	t.Run("audio object", func(t *testing.T) {
		up := uploadAudio(t, f, f.ownerID, synthMP3(40))
		req := withRouteID(requestAs(http.MethodGet, "/", f.viewerID), f.eventID)
		rec := httptest.NewRecorder()
		f.s.handleGetEvent(rec, req)
		var out struct {
			Event struct {
				Music struct {
					Audio struct {
						ID         uuid.UUID `json:"id"`
						Src        string    `json:"src"`
						SizeBytes  int64     `json:"size_bytes"`
						DurationMs int32     `json:"duration_ms"`
						CreatedAt  time.Time `json:"created_at"`
					} `json:"audio"`
				} `json:"music"`
			} `json:"event"`
		}
		if err := json.Unmarshal(rec.Body.Bytes(), &out); err != nil {
			t.Fatal(err)
		}
		if a := out.Event.Music.Audio; a.ID != up.Audio.ID || a.Src != up.Audio.Src || a.SizeBytes != 40*417 || a.DurationMs != 1044 || a.CreatedAt.IsZero() {
			t.Errorf("audio = %+v, want the uploaded track", a)
		}
	})
}

// --- public payload ---

func TestPublicEvent_Music(t *testing.T) {
	pool := dbTestPool(t)
	rdb := dbTestRedis(t)
	ctx := context.Background()

	type publicBody struct {
		Event struct {
			MusicSrc *string `json:"music_src"`
			Theme    struct {
				Music string `json:"music"`
			} `json:"theme"`
		} `json:"event"`
	}
	// mimosa carries the music_box knob (migration 00029).
	setup := func(t *testing.T, opts publicEventOpts) *publicEventFixture {
		f := newPublicEventFixture(t, pool, rdb, opts)
		pinTemplate(t, pool, f.eventID, "mimosa")
		return f
	}
	get := func(f *publicEventFixture) *httptest.ResponseRecorder {
		req := withSlugParam(requestWithIP(http.MethodGet, "/", nextTestIP(), nil), f.slug)
		rec := httptest.NewRecorder()
		f.s.handleGetPublicEvent(rec, req)
		return rec
	}
	upload := func(t *testing.T, f *publicEventFixture) audioBody {
		rec := httptest.NewRecorder()
		req := uploadRequest(f.ownerID, f.eventID, synthMP3(40), nextTestIP())
		req.Header.Set("Content-Type", "audio/mpeg")
		f.s.handleUploadEventAudio(rec, req)
		if rec.Code != http.StatusCreated {
			t.Fatalf("upload status = %d, body = %s", rec.Code, rec.Body.String())
		}
		var out audioBody
		_ = json.Unmarshal(rec.Body.Bytes(), &out)
		return out
	}

	t.Run("sources", func(t *testing.T) {
		f := setup(t, publicEventOpts{})
		up := upload(t, f)
		cases := []struct {
			source    string
			wantTheme string
			wantSrc   bool
		}{
			{"template", "music_box", false},
			{"none", "", false},
			{"custom", "", true},
		}
		for _, c := range cases {
			if _, err := pool.Exec(ctx, "UPDATE events SET music_source = $2 WHERE id = $1", f.eventID, c.source); err != nil {
				t.Fatal(err)
			}
			rec := get(f)
			if rec.Code != http.StatusOK {
				t.Fatalf("%s: status = %d, body = %s", c.source, rec.Code, rec.Body.String())
			}
			var out publicBody
			if err := json.Unmarshal(rec.Body.Bytes(), &out); err != nil {
				t.Fatal(err)
			}
			if out.Event.Theme.Music != c.wantTheme {
				t.Errorf("%s: theme.music = %q, want %q", c.source, out.Event.Theme.Music, c.wantTheme)
			}
			if c.wantSrc {
				if want := fmt.Sprintf("/media/%s/%s/audio.mp3", f.eventID, up.Audio.ID); out.Event.MusicSrc == nil || *out.Event.MusicSrc != want {
					t.Errorf("%s: music_src = %v, want %s", c.source, out.Event.MusicSrc, want)
				}
			} else if out.Event.MusicSrc != nil || !strings.Contains(rec.Body.String(), `"music_src":null`) {
				t.Errorf("%s: music_src = %v, want null", c.source, out.Event.MusicSrc)
			}
		}
	})

	t.Run("custom without a track has no music_src", func(t *testing.T) {
		f := setup(t, publicEventOpts{})
		if _, err := pool.Exec(ctx, "UPDATE events SET music_source = 'custom' WHERE id = $1", f.eventID); err != nil {
			t.Fatal(err)
		}
		var out publicBody
		_ = json.Unmarshal(get(f).Body.Bytes(), &out)
		if out.Event.MusicSrc != nil || out.Event.Theme.Music != "" {
			t.Errorf("event = %+v, want no music at all", out.Event)
		}
	})

	t.Run("a locked event leaks nothing", func(t *testing.T) {
		f := setup(t, publicEventOpts{password: "correct horse"})
		up := upload(t, f)
		if _, err := pool.Exec(ctx, "UPDATE events SET music_source = 'custom' WHERE id = $1", f.eventID); err != nil {
			t.Fatal(err)
		}
		rec := get(f)
		if rec.Code != http.StatusUnauthorized {
			t.Fatalf("status = %d, want 401; body = %s", rec.Code, rec.Body.String())
		}
		if s := rec.Body.String(); strings.Contains(s, "music") || strings.Contains(s, up.Audio.ID.String()) {
			t.Errorf("locked response leaks music: %s", s)
		}
	})
}
