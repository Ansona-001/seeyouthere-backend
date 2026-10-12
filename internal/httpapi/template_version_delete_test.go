package httpapi

import (
	"bytes"
	"context"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/ansonarose/seeyouthere-backend/internal/media"
	"github.com/ansonarose/seeyouthere-backend/internal/store"
)

// seedTemplateVersions creates a template with draft versions 1..n, each with
// the minimal manifest. withBackground lists versions that also get a
// committed background (assets_path plus stored renditions).
func seedTemplateVersions(t *testing.T, a adminAPI, blobs *testBlobs, n int, withBackground ...int32) uuid.UUID {
	t.Helper()
	ctx := context.Background()
	templateID := uuid.Must(uuid.NewV7())
	if _, err := a.f.s.q.CreateTemplate(ctx, store.CreateTemplateParams{
		ID: templateID, Slug: "test-vdel-" + templateID.String(), Name: "Test version delete", Tags: []byte("{}"),
	}); err != nil {
		t.Fatalf("create template: %v", err)
	}
	t.Cleanup(func() {
		// A published version can't be deleted (template_versions_immutable),
		// so its template is made premium instead, which hides it from every
		// public catalog query.
		bg := context.Background()
		_, _ = a.pool.Exec(bg, "DELETE FROM template_versions WHERE template_id = $1 AND published_at IS NULL", templateID)
		if _, err := a.pool.Exec(bg, "DELETE FROM templates WHERE id = $1", templateID); err != nil {
			_, _ = a.pool.Exec(bg, "UPDATE templates SET is_premium = true WHERE id = $1", templateID)
		}
	})
	for v := 1; v <= n; v++ {
		if _, err := a.pool.Exec(ctx, `INSERT INTO template_versions (template_id, version, manifest) VALUES ($1, $2, $3::jsonb)`,
			templateID, v, testManifestJSON(t)); err != nil {
			t.Fatalf("insert version %d: %v", v, err)
		}
	}
	for _, v := range withBackground {
		for _, w := range media.Widths {
			key := fmt.Sprintf("t/%s/%d/%d.jpg", templateID, v, w)
			if err := blobs.inner.Put(ctx, key, bytes.NewReader([]byte("jpg")), 3); err != nil {
				t.Fatalf("put %s: %v", key, err)
			}
		}
		rows, err := a.f.s.q.SetTemplateVersionAssets(ctx, store.SetTemplateVersionAssetsParams{
			AssetsPath: fmt.Sprintf("templates/%s/%d", templateID, v), TemplateID: templateID, Version: v,
		})
		if err != nil || rows != 1 {
			t.Fatalf("set assets v%d: rows=%d err=%v", v, rows, err)
		}
	}
	return templateID
}

func publishVersionSQL(t *testing.T, a adminAPI, templateID uuid.UUID, version int) {
	t.Helper()
	if _, err := a.pool.Exec(context.Background(),
		`UPDATE template_versions SET published_at = now() WHERE template_id = $1 AND version = $2`, templateID, version); err != nil {
		t.Fatalf("publish v%d: %v", version, err)
	}
}

func versionPath(templateID uuid.UUID, version any) string {
	return fmt.Sprintf("/v1/admin/templates/%s/versions/%v", templateID, version)
}

func templateVersions(t *testing.T, a adminAPI, templateID uuid.UUID) []int {
	t.Helper()
	rows, err := a.pool.Query(context.Background(), `SELECT version FROM template_versions WHERE template_id = $1 ORDER BY version`, templateID)
	if err != nil {
		t.Fatalf("list versions: %v", err)
	}
	defer rows.Close()
	var out []int
	for rows.Next() {
		var v int
		if err := rows.Scan(&v); err != nil {
			t.Fatal(err)
		}
		out = append(out, v)
	}
	if err := rows.Err(); err != nil {
		t.Fatal(err)
	}
	return out
}

func templateObjectCount(t *testing.T, blobs *testBlobs, templateID uuid.UUID, version int) int {
	t.Helper()
	n := 0
	err := blobs.inner.List(context.Background(), fmt.Sprintf("t/%s/%d/", templateID, version), func(page []media.ObjectInfo) error {
		n += len(page)
		return nil
	})
	if err != nil {
		t.Fatalf("list objects: %v", err)
	}
	return n
}

func templateVersionAudit(t *testing.T, a adminAPI, templateID uuid.UUID, version int) []store.AuditLog {
	t.Helper()
	rows, err := a.f.s.q.ListAuditForTarget(context.Background(), store.ListAuditForTargetParams{
		TargetType: "template_version", TargetID: fmt.Sprintf("%s/%d", templateID, version),
		CursorCreatedAt: time.Date(9999, 12, 31, 0, 0, 0, 0, time.UTC), CursorID: uuid.Max, Lim: 20,
	})
	if err != nil {
		t.Fatalf("list audit: %v", err)
	}
	return rows
}

func TestAdminDeleteTemplateVersion_Success(t *testing.T) {
	tests := []struct {
		name           string
		withBackground bool
	}{
		{"draft without a background", false},
		{"draft with a background", true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			a, blobs := newAdminAPIWithBlobs(t)
			var bg []int32
			if tt.withBackground {
				bg = []int32{2}
			}
			id := seedTemplateVersions(t, a, blobs, 2, bg...)
			// Another template's files must survive.
			other := seedTemplateVersions(t, a, blobs, 1, 1)

			rec := a.do(http.MethodDelete, versionPath(id, 2), a.superTok, "")
			if rec.Code != http.StatusNoContent || rec.Body.Len() != 0 {
				t.Fatalf("status = %d, body = %q, want empty 204", rec.Code, rec.Body.String())
			}
			if got := templateVersions(t, a, id); len(got) != 1 || got[0] != 1 {
				t.Errorf("versions = %v, want [1]", got)
			}
			if n := templateObjectCount(t, blobs, id, 2); n != 0 {
				t.Errorf("%d background objects left", n)
			}
			if n := templateObjectCount(t, blobs, other, 1); n != len(media.Widths) {
				t.Errorf("other template objects = %d, want %d", n, len(media.Widths))
			}
			if tt.withBackground && blobs.deletes == 0 {
				t.Error("storage delete not called for a version with a background")
			}
			if !tt.withBackground && blobs.deletes != 0 {
				t.Error("storage delete called for a version without a background")
			}

			rows := templateVersionAudit(t, a, id, 2)
			if len(rows) != 1 || rows[0].Action != "template_version.delete" {
				t.Fatalf("audit rows = %+v", rows)
			}
			wantBG := fmt.Sprintf(`"had_background": %t`, tt.withBackground)
			if !strings.Contains(string(rows[0].Before), wantBG) || !strings.Contains(string(rows[0].Before), id.String()) {
				t.Errorf("audit before = %s, want template id and %s", rows[0].Before, wantBG)
			}
			if strings.Contains(string(rows[0].Before), "@") {
				t.Errorf("audit row leaks PII: %s", rows[0].Before)
			}

			// Gone for good: a second delete is a 404.
			wantStatusCode(t, a.do(http.MethodDelete, versionPath(id, 2), a.superTok, ""), http.StatusNotFound, "not_found")
		})
	}
}

func TestAdminDeleteTemplateVersion_Rejections(t *testing.T) {
	a, blobs := newAdminAPIWithBlobs(t)
	pubID := seedTemplateVersions(t, a, blobs, 2, 1, 2)
	publishVersionSQL(t, a, pubID, 1)
	lastID := seedTemplateVersions(t, a, blobs, 1, 1)
	// Both published and last: published is reported first.
	bothID := seedTemplateVersions(t, a, blobs, 1)
	publishVersionSQL(t, a, bothID, 1)
	missing := uuid.Must(uuid.NewV7())

	tests := []struct {
		name   string
		token  string
		path   string
		status int
		code   string
	}{
		{"unauthenticated", "", versionPath(pubID, 2), http.StatusUnauthorized, ""},
		{"moderator is forbidden", a.moderatorT, versionPath(pubID, 2), http.StatusForbidden, "forbidden"},
		{"malformed template id", a.superTok, "/v1/admin/templates/nope/versions/2", http.StatusNotFound, "not_found"},
		{"malformed version", a.superTok, "/v1/admin/templates/" + pubID.String() + "/versions/x", http.StatusNotFound, "not_found"},
		{"unknown template", a.superTok, versionPath(missing, 1), http.StatusNotFound, "not_found"},
		{"unknown version", a.superTok, versionPath(pubID, 9), http.StatusNotFound, "not_found"},
		{"version zero", a.superTok, versionPath(pubID, 0), http.StatusNotFound, "not_found"},
		{"published version", a.superTok, versionPath(pubID, 1), http.StatusConflict, "version_published"},
		{"only version", a.superTok, versionPath(lastID, 1), http.StatusConflict, "last_version"},
		{"only version, published", a.superTok, versionPath(bothID, 1), http.StatusConflict, "version_published"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			wantStatusCode(t, a.do(http.MethodDelete, tt.path, tt.token, ""), tt.status, tt.code)
		})
	}
	if got := templateVersions(t, a, pubID); len(got) != 2 {
		t.Errorf("versions of pubID = %v, want both", got)
	}
	if got := templateVersions(t, a, lastID); len(got) != 1 {
		t.Errorf("versions of lastID = %v, want 1", got)
	}
	if n := templateObjectCount(t, blobs, pubID, 1) + templateObjectCount(t, blobs, pubID, 2) + templateObjectCount(t, blobs, lastID, 1); n != 3*len(media.Widths) {
		t.Errorf("objects = %d, rejected deletes must keep every file", n)
	}
	if blobs.deletes != 0 {
		t.Errorf("storage delete called %d times by rejected requests", blobs.deletes)
	}
	for _, c := range []struct {
		id uuid.UUID
		v  int
	}{{pubID, 1}, {pubID, 2}, {lastID, 1}, {bothID, 1}} {
		if rows := templateVersionAudit(t, a, c.id, c.v); len(rows) != 0 {
			t.Errorf("rejected delete of %s/%d wrote audit rows", c.id, c.v)
		}
	}
}

func TestAdminDeleteTemplateVersion_StorageFailureKeepsRowAndRetrySucceeds(t *testing.T) {
	a, blobs := newAdminAPIWithBlobs(t)
	id := seedTemplateVersions(t, a, blobs, 2, 2)

	blobs.deleteErr = fmt.Errorf("delete: %w", media.ErrStorageUnavailable)
	rec := a.do(http.MethodDelete, versionPath(id, 2), a.superTok, "")
	blobs.deleteErr = nil
	wantStatusCode(t, rec, http.StatusServiceUnavailable, "storage_unavailable")
	if got := templateVersions(t, a, id); len(got) != 2 {
		t.Errorf("versions = %v, want both kept", got)
	}
	if n := templateObjectCount(t, blobs, id, 2); n != len(media.Widths) {
		t.Errorf("objects = %d, want all kept", n)
	}
	if rows := templateVersionAudit(t, a, id, 2); len(rows) != 0 {
		t.Errorf("failed delete wrote %d audit rows", len(rows))
	}

	wantStatusCode(t, a.do(http.MethodDelete, versionPath(id, 2), a.superTok, ""), http.StatusNoContent, "")
	if got := templateVersions(t, a, id); len(got) != 1 {
		t.Errorf("versions after retry = %v", got)
	}
	if n := templateObjectCount(t, blobs, id, 2); n != 0 {
		t.Errorf("%d objects left after retry", n)
	}
}

// A publish that holds the version row lock wins: the delete waits, then
// answers version_published and touches neither row nor files.
func TestAdminDeleteTemplateVersion_WaitsForPublishLock(t *testing.T) {
	a, blobs := newAdminAPIWithBlobs(t)
	ctx := context.Background()
	id := seedTemplateVersions(t, a, blobs, 2, 2)

	tx, err := a.pool.Begin(ctx)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = tx.Rollback(ctx) })
	if _, err := tx.Exec(ctx, `SELECT 1 FROM template_versions WHERE template_id = $1 AND version = 2 FOR UPDATE`, id); err != nil {
		t.Fatal(err)
	}

	done := make(chan *httptest.ResponseRecorder, 1)
	go func() { done <- a.do(http.MethodDelete, versionPath(id, 2), a.superTok, "") }()
	select {
	case rec := <-done:
		t.Fatalf("delete finished while the row was locked: status = %d, body = %s", rec.Code, rec.Body.String())
	case <-time.After(300 * time.Millisecond):
	}
	if _, err := tx.Exec(ctx, `UPDATE template_versions SET published_at = now() WHERE template_id = $1 AND version = 2`, id); err != nil {
		t.Fatal(err)
	}
	if err := tx.Commit(ctx); err != nil {
		t.Fatal(err)
	}

	select {
	case rec := <-done:
		wantStatusCode(t, rec, http.StatusConflict, "version_published")
	case <-time.After(10 * time.Second):
		t.Fatal("delete never finished")
	}
	if got := templateVersions(t, a, id); len(got) != 2 {
		t.Errorf("versions = %v, want both", got)
	}
	if n := templateObjectCount(t, blobs, id, 2); n != len(media.Widths) {
		t.Errorf("objects = %d, want all kept", n)
	}
}

// Real concurrent publish and delete of the same draft: exactly one wins, and
// the state matches the winner.
func TestAdminDeleteTemplateVersion_ConcurrentPublishExactlyOneWins(t *testing.T) {
	a, blobs := newAdminAPIWithBlobs(t)
	for i := 0; i < 8; i++ {
		id := seedTemplateVersions(t, a, blobs, 2, 2)
		var wg sync.WaitGroup
		var del, pub *httptest.ResponseRecorder
		wg.Add(2)
		go func() { defer wg.Done(); del = a.do(http.MethodDelete, versionPath(id, 2), a.superTok, "") }()
		go func() {
			defer wg.Done()
			pub = a.do(http.MethodPost, versionPath(id, 2)+"/publish", a.superTok, "")
		}()
		wg.Wait()

		versions := templateVersions(t, a, id)
		switch {
		case del.Code == http.StatusNoContent && pub.Code == http.StatusNotFound:
			if len(versions) != 1 || templateObjectCount(t, blobs, id, 2) != 0 {
				t.Fatalf("round %d: delete won but versions=%v", i, versions)
			}
		case pub.Code == http.StatusOK && del.Code == http.StatusConflict && strings.Contains(del.Body.String(), "version_published"):
			if len(versions) != 2 || templateObjectCount(t, blobs, id, 2) != len(media.Widths) {
				t.Fatalf("round %d: publish won but versions=%v", i, versions)
			}
		default:
			t.Fatalf("round %d: delete = %d %s, publish = %d %s, want exactly one winner",
				i, del.Code, del.Body.String(), pub.Code, pub.Body.String())
		}
	}
}

// Two concurrent deletes of the only two versions: one must lose with
// last_version rather than leaving the template with none.
func TestAdminDeleteTemplateVersion_ConcurrentDeletesKeepOneVersion(t *testing.T) {
	a, blobs := newAdminAPIWithBlobs(t)
	for i := 0; i < 8; i++ {
		id := seedTemplateVersions(t, a, blobs, 2)
		var wg sync.WaitGroup
		recs := make([]*httptest.ResponseRecorder, 2)
		for j := range recs {
			wg.Add(1)
			go func() { defer wg.Done(); recs[j] = a.do(http.MethodDelete, versionPath(id, j+1), a.superTok, "") }()
		}
		wg.Wait()
		ok, last := 0, 0
		for _, rec := range recs {
			switch {
			case rec.Code == http.StatusNoContent:
				ok++
			case rec.Code == http.StatusConflict && strings.Contains(rec.Body.String(), "last_version"):
				last++
			}
		}
		if ok != 1 || last != 1 || len(templateVersions(t, a, id)) != 1 {
			t.Fatalf("round %d: %d deleted, %d last_version, versions=%v", i, ok, last, templateVersions(t, a, id))
		}
	}
}
