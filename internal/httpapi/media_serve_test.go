package httpapi

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/redis/go-redis/v9"

	"github.com/ansonarose/seeyouthere-backend/internal/media"
	"github.com/ansonarose/seeyouthere-backend/internal/ratelimit"
	"github.com/ansonarose/seeyouthere-backend/internal/store"
)

// uploadHostMedia uploads the test JPEG as userID through the real handler and
// returns the new media id and its size_bytes.
func uploadHostMedia(t *testing.T, f eventTestFixture, userID, eventID uuid.UUID) (uuid.UUID, int64) {
	t.Helper()
	rec := httptest.NewRecorder()
	f.s.handleUploadEventMedia(rec, uploadRequest(userID, eventID, testJPEG(t), nextTestIP()))
	if rec.Code != http.StatusCreated {
		t.Fatalf("upload status = %d, body = %s", rec.Code, rec.Body.String())
	}
	var out struct {
		Media struct {
			ID        uuid.UUID `json:"id"`
			SizeBytes int64     `json:"size_bytes"`
		} `json:"media"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &out); err != nil {
		t.Fatalf("parse upload response: %v", err)
	}
	return out.Media.ID, out.Media.SizeBytes
}

// getMedia drives the full router, so the test also covers the route group
// (no secureHeaders, no session) and the logging middleware.
func getMedia(h http.Handler, path, ip string, hdr map[string]string) *httptest.ResponseRecorder {
	req := httptest.NewRequest(http.MethodGet, path, nil)
	req.RemoteAddr = ip + ":4000"
	for k, v := range hdr {
		req.Header.Set(k, v)
	}
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	return rec
}

// exhaustBudget raises a rate-limit counter to just below its cap (so the next
// hit is the last allowed one when slack is 1) and restores it afterwards.
func exhaustBudget(t *testing.T, rdb *redis.Client, key string, limit, slack int64) {
	t.Helper()
	ctx := context.Background()
	rk := "rl:" + key
	cur, err := rdb.Get(ctx, rk).Int64()
	if err != nil && !errors.Is(err, redis.Nil) {
		t.Fatalf("read %s: %v", rk, err)
	}
	need := limit - slack - cur
	if need <= 0 {
		return
	}
	if err := rdb.IncrBy(ctx, rk, need).Err(); err != nil {
		t.Fatalf("raise %s: %v", rk, err)
	}
	t.Cleanup(func() { rdb.DecrBy(context.Background(), rk, need) })
}

func TestServeMedia_Matrix(t *testing.T) {
	pool := dbTestPool(t)
	rdb := dbTestRedis(t)
	f, blobs := newMediaTestFixtureBlobs(t, pool, rdb)
	ctx := context.Background()
	h := f.s.Routes()

	mediaID, _ := uploadHostMedia(t, f, f.ownerID, f.eventID)
	otherEvent := createTestEvent(t, ctx, f.s.q, "birthday", "classic", &f.ownerID)
	t.Cleanup(func() { _, _ = pool.Exec(context.Background(), "DELETE FROM events WHERE id = $1", otherEvent.ID) })

	path := func(w string) string { return fmt.Sprintf("/media/%s/%s/%s", f.eventID, mediaID, w) }

	t.Run("approved media serves every rendition with the full header set", func(t *testing.T) {
		for _, w := range []string{"480.jpg", "1080.jpg", "1920.jpg"} {
			rec := getMedia(h, path(w), nextTestIP(), nil)
			if rec.Code != http.StatusOK {
				t.Fatalf("%s: status = %d, body = %s", w, rec.Code, rec.Body.String())
			}
			want := map[string]string{
				"Content-Type":                 "image/jpeg",
				"Cache-Control":                "public, max-age=3600",
				"X-Content-Type-Options":       "nosniff",
				"Content-Security-Policy":      "default-src 'none'",
				"Cross-Origin-Resource-Policy": "same-site",
			}
			for k, v := range want {
				if got := rec.Header().Get(k); got != v {
					t.Errorf("%s: %s = %q, want %q", w, k, got, v)
				}
			}
			if rec.Header().Get("ETag") == "" {
				t.Errorf("%s: missing ETag", w)
			}
			if cl := rec.Header().Get("Content-Length"); cl != fmt.Sprint(rec.Body.Len()) {
				t.Errorf("%s: Content-Length = %q, body = %d bytes", w, cl, rec.Body.Len())
			}
			if b := rec.Body.Bytes(); len(b) < 3 || b[0] != 0xFF || b[1] != 0xD8 {
				t.Errorf("%s: body is not a JPEG", w)
			}
		}
	})

	t.Run("matching If-None-Match gives 304 without a body", func(t *testing.T) {
		first := getMedia(h, path("480.jpg"), nextTestIP(), nil)
		etag := first.Header().Get("ETag")
		rec := getMedia(h, path("480.jpg"), nextTestIP(), map[string]string{"If-None-Match": etag})
		if rec.Code != http.StatusNotModified || rec.Body.Len() != 0 {
			t.Fatalf("status = %d, body = %d bytes, want 304 and empty", rec.Code, rec.Body.Len())
		}
		if rec.Header().Get("ETag") != etag || rec.Header().Get("Cache-Control") != "public, max-age=3600" {
			t.Errorf("304 headers = %v", rec.Header())
		}
	})

	t.Run("a malformed If-None-Match is not forwarded", func(t *testing.T) {
		rec := getMedia(h, path("480.jpg"), nextTestIP(), map[string]string{"If-None-Match": `"a", "b"`})
		if rec.Code != http.StatusOK {
			t.Fatalf("status = %d, want 200 (list validators are dropped)", rec.Code)
		}
	})

	// Every reason a file is not servable must produce the same 404 and must
	// not reach the storage backend.
	notServable := []struct {
		name  string
		path  string
		setup string // SQL run before the request, undone by reset
	}{
		{"unsupported width", path("720.jpg"), ""},
		{"non-jpg extension", path("480.png"), ""},
		{"uppercase uuid", fmt.Sprintf("/media/%s/%s/480.jpg", strings.ToUpper(f.eventID.String()), mediaID), ""},
		{"uuid without hyphens", fmt.Sprintf("/media/%s/%s/480.jpg", strings.ReplaceAll(f.eventID.String(), "-", ""), mediaID), ""},
		{"not a uuid", "/media/nope/nope/480.jpg", ""},
		{"wrong event", fmt.Sprintf("/media/%s/%s/480.jpg", otherEvent.ID, mediaID), ""},
		{"unknown media", fmt.Sprintf("/media/%s/%s/480.jpg", f.eventID, uuid.Must(uuid.NewV7())), ""},
		{"pending", path("480.jpg"), "UPDATE media SET moderation_status = 'pending' WHERE id = $1"},
		{"rejected", path("480.jpg"), "UPDATE media SET moderation_status = 'rejected' WHERE id = $1"},
		{"event taken down", path("480.jpg"), "UPDATE events SET status = 'taken_down', slug = 'tk-' || replace(id::text, '-', '') WHERE id = (SELECT event_id FROM media WHERE id = $1)"},
		{"event soft-deleted", path("480.jpg"), "UPDATE events SET deleted_at = now() WHERE id = (SELECT event_id FROM media WHERE id = $1)"},
	}
	var wantBody string
	for _, c := range notServable {
		t.Run(c.name, func(t *testing.T) {
			if c.setup != "" {
				if _, err := pool.Exec(ctx, c.setup, mediaID); err != nil {
					t.Fatalf("setup: %v", err)
				}
				t.Cleanup(func() {
					_, _ = pool.Exec(ctx, "UPDATE media SET moderation_status = 'approved' WHERE id = $1", mediaID)
					_, _ = pool.Exec(ctx, "UPDATE events SET status = 'draft', slug = NULL, deleted_at = NULL WHERE id = $1", f.eventID)
				})
			}
			before := blobs.calls()
			rec := getMedia(h, c.path, nextTestIP(), nil)
			if rec.Code != http.StatusNotFound {
				t.Fatalf("status = %d, want 404; body = %s", rec.Code, rec.Body.String())
			}
			if blobs.calls() != before {
				t.Errorf("storage backend was called for a non-servable file")
			}
			if cc := rec.Header().Get("Cache-Control"); cc != "no-store" {
				t.Errorf("Cache-Control = %q, want no-store", cc)
			}
			if wantBody == "" {
				wantBody = rec.Body.String()
			} else if rec.Body.String() != wantBody {
				t.Errorf("body = %s, differs from other 404s (%s)", rec.Body.String(), wantBody)
			}
		})
	}

	t.Run("storage backend down is 503 with Retry-After", func(t *testing.T) {
		blobs.getErr = fmt.Errorf("get: %w", media.ErrStorageUnavailable)
		t.Cleanup(func() { blobs.getErr = nil })
		rec := getMedia(h, path("480.jpg"), nextTestIP(), nil)
		if rec.Code != http.StatusServiceUnavailable || !strings.Contains(rec.Body.String(), `"storage_unavailable"`) {
			t.Fatalf("status = %d, body = %s, want 503 storage_unavailable", rec.Code, rec.Body.String())
		}
		if rec.Header().Get("Retry-After") != "30" || rec.Header().Get("Cache-Control") != "no-store" {
			t.Errorf("headers = %v", rec.Header())
		}
	})

	t.Run("missing object is 404", func(t *testing.T) {
		otherID, _ := uploadHostMedia(t, f, f.ownerID, f.eventID)
		if err := f.s.media.DeleteMedia(ctx, f.eventID, otherID); err != nil {
			t.Fatalf("delete objects: %v", err)
		}
		rec := getMedia(h, fmt.Sprintf("/media/%s/%s/480.jpg", f.eventID, otherID), nextTestIP(), nil)
		if rec.Code != http.StatusNotFound {
			t.Fatalf("status = %d, want 404; body = %s", rec.Code, rec.Body.String())
		}
	})

	t.Run("per-IP view limit is 429 before any lookup", func(t *testing.T) {
		ip := nextTestIP()
		key := "media:view:ip:" + ip
		if err := rdb.Set(ctx, "rl:"+key, mediaViewPerIPLimit, time.Hour).Err(); err != nil {
			t.Fatalf("seed limiter: %v", err)
		}
		t.Cleanup(func() { rdb.Del(context.Background(), "rl:"+key) })
		before := blobs.calls()
		rec := getMedia(h, path("480.jpg"), ip, nil)
		if rec.Code != http.StatusTooManyRequests || blobs.calls() != before {
			t.Fatalf("status = %d, calls changed = %v, want 429 and no backend call", rec.Code, blobs.calls() != before)
		}
	})

	t.Run("exhausted daily read budget is 503", func(t *testing.T) {
		exhaustBudget(t, rdb, "r2:reads:day", mediaReadsPerDay, 0)
		before := blobs.calls()
		rec := getMedia(h, path("480.jpg"), nextTestIP(), nil)
		if rec.Code != http.StatusServiceUnavailable || blobs.calls() != before {
			t.Fatalf("status = %d, want 503 without a backend call; body = %s", rec.Code, rec.Body.String())
		}
	})

	t.Run("a full stream pool is 503 busy after the wait and spends no read budget", func(t *testing.T) {
		if testing.Short() {
			t.Skip("waits for the 5s stream slot timeout")
		}
		saved := f.s.mediaSem
		f.s.mediaSem = make(chan struct{}, 1)
		f.s.mediaSem <- struct{}{}
		t.Cleanup(func() { f.s.mediaSem = saved })
		before := blobs.calls()
		budgetBefore, _ := rdb.Get(ctx, "rl:r2:reads:day").Int64()
		rec := getMedia(h, path("480.jpg"), nextTestIP(), nil)
		if rec.Code != http.StatusServiceUnavailable || blobs.calls() != before {
			t.Fatalf("status = %d, want 503 without a backend call; body = %s", rec.Code, rec.Body.String())
		}
		if budgetAfter, _ := rdb.Get(ctx, "rl:r2:reads:day").Int64(); budgetAfter != budgetBefore {
			t.Errorf("read budget moved from %d to %d on a busy refusal", budgetBefore, budgetAfter)
		}
	})

	t.Run("the public read budget does not switch off previews", func(t *testing.T) {
		exhaustBudget(t, rdb, "r2:reads:day", mediaReadsPerDay, 0)
		if rec := getMedia(h, path("480.jpg"), nextTestIP(), nil); rec.Code != http.StatusServiceUnavailable {
			t.Fatalf("public status = %d, want 503 with the public budget exhausted", rec.Code)
		}
		req := requestAs(http.MethodGet, "/?w=480", f.ownerID)
		req = withRouteParam(withRouteParam(req, "id", f.eventID.String()), "mediaID", mediaID.String())
		rec := httptest.NewRecorder()
		f.s.handleGetEventMediaFile(rec, req)
		if rec.Code != http.StatusOK {
			t.Fatalf("preview status = %d, want 200; body = %s", rec.Code, rec.Body.String())
		}
	})

	t.Run("an exhausted preview budget refuses previews only", func(t *testing.T) {
		exhaustBudget(t, rdb, previewReads.key, mediaPreviewReadsPerDay, 0)
		req := requestAs(http.MethodGet, "/?w=480", f.ownerID)
		req = withRouteParam(withRouteParam(req, "id", f.eventID.String()), "mediaID", mediaID.String())
		rec := httptest.NewRecorder()
		f.s.handleGetEventMediaFile(rec, req)
		if rec.Code != http.StatusServiceUnavailable {
			t.Fatalf("preview status = %d, want 503", rec.Code)
		}
		if rec := getMedia(h, path("480.jpg"), nextTestIP(), nil); rec.Code != http.StatusOK {
			t.Fatalf("public status = %d, want 200", rec.Code)
		}
	})

	t.Run("a rate limiter outage fails open", func(t *testing.T) {
		saved := f.s.limiter
		dead := redis.NewClient(&redis.Options{Addr: "127.0.0.1:1", DialTimeout: 50 * time.Millisecond, MaxRetries: -1})
		t.Cleanup(func() { _ = dead.Close(); f.s.limiter = saved; f.s.limiterWarnAt.Store(0) })
		f.s.limiter = ratelimit.New(dead)
		f.s.limiterWarnAt.Store(0)
		for i := 0; i < 2; i++ {
			rec := getMedia(h, path("480.jpg"), nextTestIP(), nil)
			if rec.Code != http.StatusOK {
				t.Fatalf("request %d: status = %d, want 200 while the limiter is down; body = %s", i, rec.Code, rec.Body.String())
			}
		}
		if f.s.limiterWarnAt.Load() == 0 {
			t.Error("limiter outage was never logged")
		}
	})

	t.Run("HEAD and other methods are not served", func(t *testing.T) {
		req := httptest.NewRequest(http.MethodPost, path("480.jpg"), nil)
		rec := httptest.NewRecorder()
		h.ServeHTTP(rec, req)
		if rec.Code == http.StatusOK {
			t.Fatalf("POST served media")
		}
	})
}

func TestServeTemplateMedia(t *testing.T) {
	pool := dbTestPool(t)
	rdb := dbTestRedis(t)
	f, blobs := newMediaTestFixtureBlobs(t, pool, rdb)
	ctx := context.Background()
	h := f.s.Routes()
	q := f.s.q

	templateID := uuid.Must(uuid.NewV7())
	if _, err := q.CreateTemplate(ctx, store.CreateTemplateParams{
		ID: templateID, Slug: "test-tpl-serve-" + templateID.String(), Name: "Serve", Tags: []byte("{}"),
	}); err != nil {
		t.Fatalf("create template: %v", err)
	}
	t.Cleanup(func() { _, _ = pool.Exec(context.Background(), "DELETE FROM templates WHERE id = $1", templateID) })
	if _, err := q.CreateTemplateVersion(ctx, store.CreateTemplateVersionParams{TemplateID: templateID, Manifest: []byte("{}")}); err != nil {
		t.Fatalf("create version: %v", err)
	}

	tp := func(v, w string) string {
		return fmt.Sprintf("/media/templates/%s/%s/background/%s", templateID, v, w)
	}

	t.Run("no asset uploaded yet is 404 without a backend call", func(t *testing.T) {
		rec := getMedia(h, tp("1", "480.jpg"), nextTestIP(), nil)
		if rec.Code != http.StatusNotFound || blobs.calls() != 0 {
			t.Fatalf("status = %d, calls = %d, want 404 and 0", rec.Code, blobs.calls())
		}
	})

	outDir, _ := processTestUpload(t, f.s, testJPEG(t))
	assetsPath, err := f.s.media.CommitTemplateAsset(ctx, outDir, templateID, 1)
	if err != nil {
		t.Fatalf("commit template asset: %v", err)
	}
	if _, err := q.SetTemplateVersionAssets(ctx, store.SetTemplateVersionAssetsParams{AssetsPath: assetsPath, TemplateID: templateID, Version: 1}); err != nil {
		t.Fatalf("set assets: %v", err)
	}

	t.Run("uploaded asset serves", func(t *testing.T) {
		rec := getMedia(h, tp("1", "1080.jpg"), nextTestIP(), nil)
		if rec.Code != http.StatusOK || rec.Header().Get("Content-Type") != "image/jpeg" ||
			rec.Header().Get("Cache-Control") != "public, max-age=3600" {
			t.Fatalf("status = %d, headers = %v", rec.Code, rec.Header())
		}
	})

	for _, c := range []struct{ name, path string }{
		{"unknown version", tp("2", "480.jpg")},
		{"leading zero version", tp("01", "480.jpg")},
		{"zero version", tp("0", "480.jpg")},
		{"negative version", tp("-1", "480.jpg")},
		{"overflowing version", tp("99999999999", "480.jpg")},
		{"bad width", tp("1", "700.jpg")},
		{"uppercase template id", "/media/templates/" + strings.ToUpper(templateID.String()) + "/1/background/480.jpg"},
	} {
		t.Run(c.name, func(t *testing.T) {
			before := blobs.calls()
			rec := getMedia(h, c.path, nextTestIP(), nil)
			if rec.Code != http.StatusNotFound || blobs.calls() != before {
				t.Fatalf("status = %d, want 404 without a backend call", rec.Code)
			}
		})
	}
}

func TestPreviewMedia_UsesPrivateCaching(t *testing.T) {
	pool := dbTestPool(t)
	rdb := dbTestRedis(t)
	f := newMediaTestFixture(t, pool, rdb)
	mediaID, _ := uploadHostMedia(t, f, f.ownerID, f.eventID)

	req := requestAs(http.MethodGet, "/?w=480", f.ownerID)
	req = withRouteParam(withRouteParam(req, "id", f.eventID.String()), "mediaID", mediaID.String())
	rec := httptest.NewRecorder()
	f.s.handleGetEventMediaFile(rec, req)
	if rec.Code != http.StatusOK || rec.Header().Get("Cache-Control") != "private, max-age=300" ||
		rec.Header().Get("Content-Type") != "image/jpeg" || rec.Header().Get("ETag") == "" {
		t.Fatalf("status = %d, headers = %v", rec.Code, rec.Header())
	}
}

func TestForwardableETag(t *testing.T) {
	tests := []struct {
		name string
		in   string
		want string
	}{
		{"empty", "", ""},
		{"strong", `"abc123"`, `"abc123"`},
		{"weak", `W/"abc123"`, `W/"abc123"`},
		{"r2 style md5", `"d41d8cd98f00b204e9800998ecf8427e"`, `"d41d8cd98f00b204e9800998ecf8427e"`},
		{"star", `*`, ""},
		{"unquoted", `abc123`, ""},
		{"only opening quote", `"abc`, ""},
		{"empty tag", `""`, ""},
		{"comma list", `"a", "b"`, ""},
		{"comma list without space", `"a","b"`, ""},
		{"comma inside quotes is still a list-looking value", `"a,b"`, `"a,b"`},
		{"leading space", ` "abc"`, ""},
		{"trailing space", `"abc" `, ""},
		{"double space", `"a  b"`, ""},
		{"inner space", `"a b"`, ""},
		{"quote inside", `"a"b"`, ""},
		{"lowercase weak marker", `w/"abc"`, ""},
		{"control character", "\"a\x01b\"", ""},
		{"non-ascii", "\"a\u00e9\"", ""},
		{"newline", "\"a\nb\"", ""},
		{"126 char tag", `"` + strings.Repeat("a", 126) + `"`, `"` + strings.Repeat("a", 126) + `"`},
		{"127 char tag", `"` + strings.Repeat("a", 127) + `"`, ""},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := forwardableETag(tt.in); got != tt.want {
				t.Errorf("forwardableETag(%q) = %q, want %q", tt.in, got, tt.want)
			}
		})
	}
}

func TestWarnLimiterDownIsThrottled(t *testing.T) {
	var s Server
	s.warnLimiterDown(context.Background(), errors.New("down"))
	first := s.limiterWarnAt.Load()
	if first == 0 {
		t.Fatal("first outage was not logged")
	}
	s.warnLimiterDown(context.Background(), errors.New("down"))
	if got := s.limiterWarnAt.Load(); got != first {
		t.Errorf("second outage within the window was logged again")
	}
	s.limiterWarnAt.Store(time.Now().Add(-2 * limiterWarnEvery).UnixNano())
	stale := s.limiterWarnAt.Load()
	s.warnLimiterDown(context.Background(), errors.New("down"))
	if s.limiterWarnAt.Load() == stale {
		t.Error("outage after the window was not logged")
	}
}
