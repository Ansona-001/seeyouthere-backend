package httpapi

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/google/uuid"

	"github.com/ansonarose/seeyouthere-backend/internal/store"
)

// TestHandleListTemplates_SkipsInvalidManifest proves one published template
// whose stored manifest no longer validates is skipped (and logged) rather
// than failing the whole picker with a 500.
func TestHandleListTemplates_SkipsInvalidManifest(t *testing.T) {
	pool := dbTestPool(t)
	ctx := context.Background()
	tx, err := pool.Begin(ctx)
	if err != nil {
		t.Fatalf("begin: %v", err)
	}
	t.Cleanup(func() { _ = tx.Rollback(ctx) })

	// A unique occasion tag keeps the listing to this test's rows.
	tag := "zz-" + uuid.NewString()
	tags := `{"occasions":["` + tag + `"]}`
	insert := func(slug, manifest string, sortOrder int) {
		t.Helper()
		id := uuid.Must(uuid.NewV7())
		if _, err := tx.Exec(ctx,
			`INSERT INTO templates (id, slug, name, tags, status, sort_order) VALUES ($1, $2, $3, $4::jsonb, 'published', $5)`,
			id, slug+"-"+id.String(), slug, tags, sortOrder); err != nil {
			t.Fatalf("insert template %s: %v", slug, err)
		}
		if _, err := tx.Exec(ctx,
			`INSERT INTO template_versions (template_id, version, manifest, published_at) VALUES ($1, 1, $2::jsonb, now())`,
			id, manifest); err != nil {
			t.Fatalf("insert version %s: %v", slug, err)
		}
	}
	insert("good-one", testManifestJSON(t), 1)
	insert("broken", `{"schema":2,"layout":"centered"}`, 2)
	insert("good-two", testManifestJSON(t), 3)

	s := &Server{q: store.New(tx)}
	rec := httptest.NewRecorder()
	s.handleListTemplates(rec, httptest.NewRequest(http.MethodGet, "/v1/templates?occasion="+tag, nil))
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, body = %s", rec.Code, rec.Body.String())
	}
	var resp struct {
		Templates []struct {
			Name string `json:"name"`
		} `json:"templates"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &resp); err != nil {
		t.Fatalf("decode: %v", err)
	}
	var got []string
	for _, tpl := range resp.Templates {
		got = append(got, tpl.Name)
	}
	if len(got) != 2 || got[0] != "good-one" || got[1] != "good-two" {
		t.Errorf("templates = %v, want [good-one good-two]", got)
	}
}
