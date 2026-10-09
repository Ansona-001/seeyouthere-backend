package httpapi

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/ansonarose/seeyouthere-backend/internal/store"
)

// v2ImageManifestJSON is the Olive Grove launch manifest (schema 2) plus an
// image layer, so the template needs the version's background asset.
func v2ImageManifestJSON(t *testing.T) string {
	t.Helper()
	raw, err := os.ReadFile(filepath.Join("..", "content", "testdata", "themes_v2", "olive-grove.json"))
	if err != nil {
		t.Fatalf("read seed manifest: %v", err)
	}
	var m map[string]any
	if err := json.Unmarshal(raw, &m); err != nil {
		t.Fatalf("decode seed manifest: %v", err)
	}
	layers, _ := m["layers"].([]any)
	m["layers"] = append(layers, map[string]any{"kind": "image", "opacity": 0.3})
	out, err := json.Marshal(m)
	if err != nil {
		t.Fatalf("encode manifest: %v", err)
	}
	return string(out)
}

func TestAdminTemplateV2_ImageLayerNeedsAssetToPublish(t *testing.T) {
	pool := dbTestPool(t)
	rdb := dbTestRedis(t)
	f := newAdminHandlerFixture(t, pool, rdb)
	ctx := context.Background()
	templateID := newV2Template(t, f, pool)
	roles := []string{"super_admin"}

	publish := func() *httptest.ResponseRecorder {
		req := adminRequestAs(http.MethodPost, "/", f.superAdminID, f.superAdminSessID, roles)
		req = withRouteParam(withRouteID(req, templateID), "v", "1")
		rec := httptest.NewRecorder()
		f.s.handleAdminPublishTemplateVersion(rec, req)
		return rec
	}

	rec := publish()
	if rec.Code != http.StatusConflict || !containsAll(rec.Body.String(), `"not_ready"`) {
		t.Fatalf("publish without an asset: status = %d, body = %s, want 409 not_ready", rec.Code, rec.Body.String())
	}
	var published *time.Time
	if err := pool.QueryRow(ctx, `SELECT published_at FROM template_versions WHERE template_id = $1 AND version = 1`, templateID).Scan(&published); err != nil {
		t.Fatal(err)
	}
	if published != nil {
		t.Fatal("version was published despite the missing asset")
	}

	setTemplateAsset(t, f, templateID)
	rec = publish()
	if rec.Code != http.StatusOK {
		t.Fatalf("publish with the asset: status = %d, body = %s", rec.Code, rec.Body.String())
	}
	// Publishing again is a conflict, not a second publish.
	rec = publish()
	if rec.Code != http.StatusConflict || !containsAll(rec.Body.String(), `"version_published"`) {
		t.Fatalf("second publish: status = %d, body = %s, want 409 version_published", rec.Code, rec.Body.String())
	}
}

func TestAdminTemplateV2_PublishUnknownVersionIs404(t *testing.T) {
	pool := dbTestPool(t)
	rdb := dbTestRedis(t)
	f := newAdminHandlerFixture(t, pool, rdb)
	templateID := newV2Template(t, f, pool)

	req := adminRequestAs(http.MethodPost, "/", f.superAdminID, f.superAdminSessID, []string{"super_admin"})
	req = withRouteParam(withRouteID(req, templateID), "v", "99")
	rec := httptest.NewRecorder()
	f.s.handleAdminPublishTemplateVersion(rec, req)
	if rec.Code != http.StatusNotFound {
		t.Fatalf("status = %d, body = %s, want 404", rec.Code, rec.Body.String())
	}
}

func TestAdminTemplateV2_PreviewImageLayer(t *testing.T) {
	pool := dbTestPool(t)
	rdb := dbTestRedis(t)
	f := newAdminHandlerFixture(t, pool, rdb)
	templateID := newV2Template(t, f, pool)

	preview := func() []imageLayerView {
		req := adminRequestAs(http.MethodGet, "/", f.superAdminID, f.superAdminSessID, []string{"super_admin"})
		req = withRouteParam(withRouteID(req, templateID), "v", "1")
		rec := httptest.NewRecorder()
		f.s.handleAdminPreviewTemplateVersion(rec, req)
		if rec.Code != http.StatusOK {
			t.Fatalf("preview: status = %d, body = %s", rec.Code, rec.Body.String())
		}
		var resp struct {
			Previews []struct {
				Theme struct {
					Engine int              `json:"engine"`
					Layers []imageLayerView `json:"layers"`
				} `json:"theme"`
			} `json:"previews"`
		}
		if err := json.Unmarshal(rec.Body.Bytes(), &resp); err != nil {
			t.Fatalf("decode: %v", err)
		}
		if len(resp.Previews) == 0 || resp.Previews[0].Theme.Engine != 2 {
			t.Fatalf("previews = %+v, want engine-2 themes", resp.Previews)
		}
		var images []imageLayerView
		for _, p := range resp.Previews {
			for _, l := range p.Theme.Layers {
				if l.Kind == "image" {
					images = append(images, l)
				}
			}
		}
		return images
	}

	if got := preview(); len(got) != 0 {
		t.Fatalf("image layers without an asset = %+v, want none (dropped)", got)
	}
	assetsPath := setTemplateAsset(t, f, templateID)
	got := preview()
	if len(got) == 0 {
		t.Fatal("no image layer in the preview once the asset is set")
	}
	for _, l := range got {
		if l.Src != "/media/"+assetsPath+"/background" || l.Opacity == nil || *l.Opacity != 0.3 {
			t.Errorf("image layer = %+v, want src /media/%s/background opacity 0.3", l, assetsPath)
		}
	}
}

func TestPublicEventV2_ReturnsEngine2AndImageSrc(t *testing.T) {
	pool := dbTestPool(t)
	rdb := dbTestRedis(t)
	f := newAdminHandlerFixture(t, pool, rdb)
	templateID := newV2Template(t, f, pool)
	assetsPath := setTemplateAsset(t, f, templateID)

	req := adminRequestAs(http.MethodPost, "/", f.superAdminID, f.superAdminSessID, []string{"super_admin"})
	req = withRouteParam(withRouteID(req, templateID), "v", "1")
	rec := httptest.NewRecorder()
	f.s.handleAdminPublishTemplateVersion(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("publish: status = %d, body = %s", rec.Code, rec.Body.String())
	}

	pf := newPublicEventFixture(t, pool, rdb, publicEventOpts{})
	// Pin the public fixture's event to the v2 template. Registered after the
	// fixture's own cleanup, so it runs before the event is deleted.
	if _, err := pool.Exec(context.Background(),
		`UPDATE events SET template_id = $1, template_version = 1 WHERE id = $2`, templateID, pf.eventID); err != nil {
		t.Fatalf("pin event to template: %v", err)
	}

	preq := withSlugParam(requestWithIP(http.MethodGet, "/", nextTestIP(), nil), pf.slug)
	prec := httptest.NewRecorder()
	pf.s.handleGetPublicEvent(prec, preq)
	if prec.Code != http.StatusOK {
		t.Fatalf("public event: status = %d, body = %s", prec.Code, prec.Body.String())
	}
	var resp struct {
		Event struct {
			Theme struct {
				Engine   int              `json:"engine"`
				Layers   []imageLayerView `json:"layers"`
				Ornament *struct {
					HeroInk string `json:"hero_ink"`
				} `json:"ornament"`
				Card *struct {
					Style string `json:"style"`
				} `json:"card"`
			} `json:"theme"`
		} `json:"event"`
	}
	if err := json.Unmarshal(prec.Body.Bytes(), &resp); err != nil {
		t.Fatalf("decode: %v", err)
	}
	th := resp.Event.Theme
	if th.Engine != 2 || th.Ornament == nil || th.Card == nil || th.Ornament.HeroInk == "" {
		t.Fatalf("theme = %+v, want an engine-2 theme with ornament and card", th)
	}
	var src string
	for _, l := range th.Layers {
		if l.Kind == "image" {
			src = l.Src
		} else if l.Src != "" {
			t.Errorf("%s layer carries src %q", l.Kind, l.Src)
		}
	}
	if want := "/media/" + assetsPath + "/background"; src != want {
		t.Errorf("image layer src = %q, want %q", src, want)
	}
}

// A concurrent PUT that lands between publish's validation and its UPDATE
// must not slip an unvalidated manifest into a published (immutable)
// version: the publish transaction locks the row first, so the edit either
// completes before publish reads it (and is validated) or waits and then
// fails with version_published.
func TestAdminTemplateV2_PublishIsSafeAgainstConcurrentEdit(t *testing.T) {
	pool := dbTestPool(t)
	rdb := dbTestRedis(t)
	f := newAdminHandlerFixture(t, pool, rdb)
	ctx := context.Background()
	templateID := newV2Template(t, f, pool)
	setTemplateAsset(t, f, templateID)

	// An in-flight edit: invalid manifest written, transaction still open.
	tx, err := pool.Begin(ctx)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = tx.Rollback(ctx) })
	if _, err := tx.Exec(ctx, `UPDATE template_versions SET manifest = '{"schema":2}'::jsonb WHERE template_id = $1 AND version = 1`, templateID); err != nil {
		t.Fatal(err)
	}

	done := make(chan *httptest.ResponseRecorder, 1)
	go func() {
		req := adminRequestAs(http.MethodPost, "/", f.superAdminID, f.superAdminSessID, []string{"super_admin"})
		req = withRouteParam(withRouteID(req, templateID), "v", "1")
		rec := httptest.NewRecorder()
		f.s.handleAdminPublishTemplateVersion(rec, req)
		done <- rec
	}()

	select {
	case rec := <-done:
		t.Fatalf("publish finished while the row was locked by an open edit: status = %d, body = %s", rec.Code, rec.Body.String())
	case <-time.After(300 * time.Millisecond):
	}
	if err := tx.Commit(ctx); err != nil {
		t.Fatal(err)
	}

	select {
	case rec := <-done:
		if rec.Code != http.StatusBadRequest || !containsAll(rec.Body.String(), "validation_failed") {
			t.Fatalf("publish after the edit: status = %d, body = %s, want 400 validation_failed", rec.Code, rec.Body.String())
		}
	case <-time.After(10 * time.Second):
		t.Fatal("publish never finished")
	}
	var published *time.Time
	if err := pool.QueryRow(ctx, `SELECT published_at FROM template_versions WHERE template_id = $1 AND version = 1`, templateID).Scan(&published); err != nil {
		t.Fatal(err)
	}
	if published != nil {
		t.Fatal("an invalid manifest was published")
	}
}

type imageLayerView struct {
	Kind    string   `json:"kind"`
	Src     string   `json:"src"`
	Opacity *float64 `json:"opacity"`
}

// newV2Template creates a template with one draft version holding the
// image-layer manifest, through the real admin handler.
func newV2Template(t *testing.T, f adminHandlerFixture, pool *pgxpool.Pool) uuid.UUID {
	t.Helper()
	ctx := context.Background()
	templateID := uuid.Must(uuid.NewV7())
	if _, err := f.s.q.CreateTemplate(ctx, store.CreateTemplateParams{
		ID: templateID, Slug: "test-v2-" + templateID.String(), Name: "Test v2", Tags: []byte("{}"),
	}); err != nil {
		t.Fatalf("create template: %v", err)
	}
	t.Cleanup(func() {
		// Drafts are deleted so a deliberately broken manifest can't linger
		// in the dev DB. A published version can't be deleted
		// (template_versions_immutable), so its template is made premium
		// instead, which removes it from every public catalog query.
		bg := context.Background()
		_, _ = pool.Exec(bg, "DELETE FROM template_versions WHERE template_id = $1 AND published_at IS NULL", templateID)
		if _, err := pool.Exec(bg, "DELETE FROM templates WHERE id = $1", templateID); err != nil {
			_, _ = pool.Exec(bg, "UPDATE templates SET is_premium = true WHERE id = $1", templateID)
		}
	})
	req := jsonRequest(http.MethodPost, "/", f.superAdminID, f.superAdminSessID, []string{"super_admin"},
		fmt.Sprintf(`{"manifest":%s}`, v2ImageManifestJSON(t)))
	req = withRouteID(req, templateID)
	rec := httptest.NewRecorder()
	f.s.handleCreateAdminTemplateVersion(rec, req)
	if rec.Code != http.StatusCreated {
		t.Fatalf("create version: status = %d, body = %s", rec.Code, rec.Body.String())
	}
	return templateID
}

// setTemplateAsset records a committed background asset for version 1 and
// returns its assets_path.
func setTemplateAsset(t *testing.T, f adminHandlerFixture, templateID uuid.UUID) string {
	t.Helper()
	assetsPath := "templates/" + templateID.String() + "/1"
	n, err := f.s.q.SetTemplateVersionAssets(context.Background(), store.SetTemplateVersionAssetsParams{
		AssetsPath: assetsPath, TemplateID: templateID, Version: 1,
	})
	if err != nil || n != 1 {
		t.Fatalf("set assets: n=%d err=%v", n, err)
	}
	return assetsPath
}
