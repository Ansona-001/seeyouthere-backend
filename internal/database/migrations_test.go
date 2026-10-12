package database

import (
	"context"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgconn"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/jackc/pgx/v5/stdlib"
	"github.com/pressly/goose/v3"

	"github.com/ansonarose/seeyouthere-backend/internal/content"
	"github.com/ansonarose/seeyouthere-backend/internal/store"
)

// scratchMigrator creates a throwaway database, returns a pool on it and a goose
// provider over the embedded migrations, and drops the database when the test
// ends. It skips the test when DATABASE_URL is unset or the role cannot create
// databases.
func scratchMigrator(t *testing.T) (context.Context, *pgxpool.Pool, *goose.Provider) {
	t.Helper()
	url := os.Getenv("DATABASE_URL")
	if url == "" {
		t.Skip("DATABASE_URL not set")
	}
	ctx := context.Background()

	admin, err := pgxpool.New(ctx, url)
	if err != nil {
		t.Fatalf("open admin pool: %v", err)
	}
	t.Cleanup(admin.Close)

	name := "syt_migtest_" + uuid.Must(uuid.NewV7()).String()[24:]
	if _, err := admin.Exec(ctx, "CREATE DATABASE "+name); err != nil {
		t.Skipf("cannot create a scratch database: %v", err)
	}
	t.Cleanup(func() {
		if _, err := admin.Exec(context.Background(), "DROP DATABASE "+name+" WITH (FORCE)"); err != nil {
			t.Logf("drop scratch database %s: %v", name, err)
		}
	})

	poolCfg, err := pgxpool.ParseConfig(url)
	if err != nil {
		t.Fatalf("parse DATABASE_URL: %v", err)
	}
	poolCfg.ConnConfig.Database = name
	pool, err := pgxpool.NewWithConfig(ctx, poolCfg)
	if err != nil {
		t.Fatalf("open scratch pool: %v", err)
	}
	t.Cleanup(pool.Close)

	sub, err := fs.Sub(migrations, "migrations")
	if err != nil {
		t.Fatal(err)
	}
	db := stdlib.OpenDBFromPool(pool)
	t.Cleanup(func() { _ = db.Close() })
	provider, err := goose.NewProvider(goose.DialectPostgres, db, sub)
	if err != nil {
		t.Fatalf("goose provider: %v", err)
	}
	return ctx, pool, provider
}

// TestMigration00005_Backfill applies migrations 1-4 to a throwaway database,
// inserts legacy dated events (including one whose datetime block holds an
// impossible date that passes the shape regex), then applies 00005 and checks
// the backfill: ends_at from the block, retention_from = greatest(ends_at,
// now()) so legacy events get a full retention window, and no abort on the
// bad row. It also runs 00005's Down and Up again.
func TestMigration00005_Backfill(t *testing.T) {
	ctx, pool, provider := scratchMigrator(t)
	if _, err := provider.UpTo(ctx, 4); err != nil {
		t.Fatalf("migrate to 4: %v", err)
	}

	insert := func(title, start, end string, startsAt string) {
		t.Helper()
		content := fmt.Sprintf(`[{"type":"datetime","start_local":%q,"end_local":%q,"timezone":"UTC"}]`, start, end)
		if _, err := pool.Exec(ctx, `
			INSERT INTO events (id, occasion_slug, title, content, template_id, template_version, starts_at)
			SELECT gen_random_uuid(), 'birthday', $1, $2::jsonb, t.id, tv.version, NULLIF($3, '')::timestamptz
			FROM templates t JOIN template_versions tv ON tv.template_id = t.id LIMIT 1`,
			title, content, startsAt); err != nil {
			t.Fatalf("insert %s: %v", title, err)
		}
	}
	insert("old", "2020-01-01T10:00", "2020-01-01T13:00", "2020-01-01T10:00:00Z")
	insert("future", "2999-01-01T10:00", "2999-01-01T12:00", "2999-01-01T10:00:00Z")
	insert("impossible-date", "2026-02-31T10:00", "2026-02-31T12:00", "2026-02-27T10:00:00Z")
	insert("undated", "", "", "")
	for i := 0; i < 20; i++ {
		insert(fmt.Sprintf("bulk-%d", i), "2020-01-01T10:00", "2020-01-01T13:00", "2020-01-01T10:00:00Z")
	}

	if _, err := provider.UpTo(ctx, 5); err != nil {
		t.Fatalf("migrate to 5 (backfill must not abort on an impossible date): %v", err)
	}

	type row struct {
		starts, ends, retention *time.Time
	}
	read := func(title string) row {
		t.Helper()
		var r row
		if err := pool.QueryRow(ctx, "SELECT starts_at, ends_at, retention_from FROM events WHERE title = $1", title).
			Scan(&r.starts, &r.ends, &r.retention); err != nil {
			t.Fatalf("read %s: %v", title, err)
		}
		return r
	}
	// within reports whether a lies in [from, from+spread]: the backfill adds a
	// random spread of up to 14 days so legacy events do not all fall due at once.
	within := func(a *time.Time, from time.Time) bool {
		return a != nil && !a.Before(from.Add(-time.Minute)) && !a.After(from.Add(14*24*time.Hour+time.Minute))
	}

	if r := read("old"); r.ends == nil || !r.ends.Equal(time.Date(2020, 1, 1, 13, 0, 0, 0, time.UTC)) || !within(r.retention, time.Now()) {
		t.Errorf("old event: ends_at = %v, retention_from = %v, want 13:00 and now to now+14d", r.ends, r.retention)
	}
	if r := read("future"); r.ends == nil || !within(r.retention, *r.ends) {
		t.Errorf("future event: ends_at = %v, retention_from = %v, want ends_at to ends_at+14d", r.ends, r.retention)
	}
	if r := read("impossible-date"); r.ends == nil || r.starts == nil || !r.ends.Equal(*r.starts) || !within(r.retention, time.Now()) {
		t.Errorf("impossible-date event: starts_at = %v, ends_at = %v, retention_from = %v, want ends_at = starts_at and retention now to now+14d", r.starts, r.ends, r.retention)
	}
	if r := read("undated"); r.ends != nil || r.retention != nil {
		t.Errorf("undated event: ends_at = %v, retention_from = %v, want NULL", r.ends, r.retention)
	}
	var distinct int
	if err := pool.QueryRow(ctx, "SELECT count(DISTINCT retention_from) FROM events WHERE title LIKE 'bulk-%'").Scan(&distinct); err != nil || distinct < 2 {
		t.Errorf("distinct retention_from over 20 identical legacy events = %d (err %v), want a spread", distinct, err)
	}
	var fnExists bool
	if err := pool.QueryRow(ctx, "SELECT EXISTS (SELECT 1 FROM pg_proc WHERE proname = 'events_backfill_gap')").Scan(&fnExists); err != nil || fnExists {
		t.Errorf("temporary backfill function still exists (err = %v)", err)
	}

	if _, err := provider.DownTo(ctx, 4); err != nil {
		t.Fatalf("down to 4: %v", err)
	}
	if _, err := provider.Up(ctx); err != nil {
		t.Fatalf("up again: %v", err)
	}
}

// TestMigration00006_ThemeEngineV2 applies migrations 1-5 to a throwaway
// database, then 00006: the six seeded templates get their picker sort_order,
// ListPublishedTemplates returns them in that order (not alphabetically), every
// stored manifest version still validates and resolves for every palette and
// font pair, and the manifest size CHECK rejects an oversized manifest. It also
// runs 00006's Down (column and constraint gone) and Up again.
func TestMigration00006_ThemeEngineV2(t *testing.T) {
	ctx, pool, provider := scratchMigrator(t)
	if _, err := provider.UpTo(ctx, 5); err != nil {
		t.Fatalf("migrate to 5: %v", err)
	}
	var seeded int
	if err := pool.QueryRow(ctx, "SELECT count(*) FROM templates").Scan(&seeded); err != nil || seeded != 6 {
		t.Fatalf("seeded templates before 00006 = %d (err %v), want 6", seeded, err)
	}

	wantOrder := []string{"classic", "garden", "heirloom", "modern", "confetti", "minimal"}
	wantSort := map[string]int32{"classic": 1010, "garden": 1020, "heirloom": 1030, "modern": 1040, "confetti": 1050, "minimal": 1060}

	checkUp := func() {
		t.Helper()
		rows, err := pool.Query(ctx, "SELECT slug, sort_order FROM templates")
		if err != nil {
			t.Fatalf("read sort_order: %v", err)
		}
		got := map[string]int32{}
		for rows.Next() {
			var slug string
			var sortOrder int32
			if err := rows.Scan(&slug, &sortOrder); err != nil {
				t.Fatalf("scan sort_order: %v", err)
			}
			got[slug] = sortOrder
		}
		rows.Close()
		if err := rows.Err(); err != nil {
			t.Fatalf("read sort_order: %v", err)
		}
		if len(got) != len(wantSort) {
			t.Errorf("sort_order by slug = %v, want %v", got, wantSort)
		}
		for slug, want := range wantSort {
			if got[slug] != want {
				t.Errorf("sort_order[%s] = %d, want %d", slug, got[slug], want)
			}
		}

		q := store.New(pool)
		listed, err := q.ListPublishedTemplates(ctx, nil)
		if err != nil {
			t.Fatalf("list published templates: %v", err)
		}
		slugs := make([]string, len(listed))
		for i, r := range listed {
			slugs[i] = r.Slug
		}
		if fmt.Sprint(slugs) != fmt.Sprint(wantOrder) {
			t.Errorf("ListPublishedTemplates order = %v, want %v", slugs, wantOrder)
		}

		// Every stored version (drafts and old published ones too) must parse
		// and resolve for every palette x font combination.
		versions, err := q.ListTemplateVersionManifests(ctx)
		if err != nil {
			t.Fatalf("list template version manifests: %v", err)
		}
		if len(versions) != 10 {
			t.Errorf("stored template versions = %d, want 10", len(versions))
		}
		for _, v := range versions {
			label := fmt.Sprintf("template %s v%d", v.TemplateID, v.Version)
			m, err := content.ValidateManifest(v.Manifest)
			if err != nil {
				t.Errorf("ValidateManifest(%s): %v", label, err)
				continue
			}
			for _, p := range m.Palettes {
				for _, f := range m.Fonts {
					overrides := fmt.Sprintf(`{"palette":%q,"font":%q}`, p.ID, f.ID)
					th := content.ResolveTheme(m, []byte(overrides), "")
					if th.Palette != p.Colors || th.Fonts.Heading != f.Heading || th.Fonts.Body != f.Body ||
						th.Fonts.Accent == "" || th.AccentInk == "" {
						t.Errorf("ResolveTheme(%s, palette %s, font %s) = %+v, want that palette and font pair", label, p.ID, f.ID, th)
					}
				}
			}
		}
	}

	if _, err := provider.UpTo(ctx, 6); err != nil {
		t.Fatalf("migrate to 6: %v", err)
	}
	checkUp()

	// The size CHECK: a manifest just under 32 KiB is stored, one over is
	// rejected with check_violation on the named constraint. Both inside a
	// transaction that rolls back so the catalog stays as seeded.
	insertManifest := func(padding int) error {
		tx, err := pool.Begin(ctx)
		if err != nil {
			t.Fatalf("begin: %v", err)
		}
		defer func() { _ = tx.Rollback(ctx) }()
		manifest := `{"pad":"` + strings.Repeat("a", padding) + `"}`
		_, err = tx.Exec(ctx, `
			INSERT INTO template_versions (template_id, version, manifest)
			SELECT id, 99, $1::jsonb FROM templates WHERE slug = 'classic'`, manifest)
		return err
	}
	if err := insertManifest(32000); err != nil {
		t.Errorf("insert a manifest under the cap: %v", err)
	}
	err := insertManifest(33000)
	var pgErr *pgconn.PgError
	if !errors.As(err, &pgErr) || pgErr.Code != "23514" || pgErr.ConstraintName != "template_versions_manifest_size_check" {
		t.Errorf("insert an oversized manifest: err = %v, want check_violation on template_versions_manifest_size_check", err)
	}

	if _, err := provider.DownTo(ctx, 5); err != nil {
		t.Fatalf("down to 5: %v", err)
	}
	var columns, constraints int
	if err := pool.QueryRow(ctx, "SELECT count(*) FROM information_schema.columns WHERE table_name = 'templates' AND column_name = 'sort_order'").Scan(&columns); err != nil || columns != 0 {
		t.Errorf("sort_order columns after Down = %d (err %v), want 0", columns, err)
	}
	if err := pool.QueryRow(ctx, "SELECT count(*) FROM pg_constraint WHERE conname = 'template_versions_manifest_size_check'").Scan(&constraints); err != nil || constraints != 0 {
		t.Errorf("size constraints after Down = %d (err %v), want 0", constraints, err)
	}

	// Stop at 6: later migrations add templates, which checkUp's catalog counts exclude.
	if _, err := provider.UpTo(ctx, 6); err != nil {
		t.Fatalf("up again: %v", err)
	}
	checkUp()
}

// TestMigration00007_SeedV2Themes applies migrations 1-6 and then 00007: the
// 12 theme-engine-v2 templates exist as drafts with a published v1 whose
// manifest validates and resolves for every palette and font pair, they stay
// out of the host picker, and the original six are untouched. It then runs
// 00007's Down with an event pinned to one of them (must fail by FK and change
// nothing), unpins it, runs Down again (the 12 rows go), and applies Up again.
func TestMigration00007_SeedV2Themes(t *testing.T) {
	ctx, pool, provider := scratchMigrator(t)
	if _, err := provider.UpTo(ctx, 6); err != nil {
		t.Fatalf("migrate to 6: %v", err)
	}
	if _, err := provider.UpTo(ctx, 7); err != nil {
		t.Fatalf("migrate to 7: %v", err)
	}

	type seed struct {
		slug, name, occasion string
		extra                []string
	}
	wantSeeds := []seed{
		{"olive-grove", "Olive Grove", "wedding", nil},
		{"letterpress", "Letterpress", "wedding", nil},
		{"velvet-foil", "Velvet and Foil", "engagement", nil},
		{"sunlit", "Sunlit Watercolour", "engagement", nil},
		{"sprinkles", "Sprinkles", "birthday", []string{"kids-birthday"}},
		{"risograph", "Risograph", "birthday", []string{"milestone-birthday"}},
		{"cloud-nine", "Cloud Nine", "baby-shower", nil},
		{"gingham", "Gingham", "baby-shower", nil},
		{"open-door", "Open Door", "housewarming", nil},
		{"terrazzo", "Terrazzo", "housewarming", nil},
		{"gilt-noir", "Gilt Noir", "party", []string{"milestone-birthday"}},
		{"mirror-ball", "Mirror Ball", "party", nil},
	}

	checkSeeded := func() {
		t.Helper()
		for i, want := range wantSeeds {
			wantID := fmt.Sprintf("01926a00-0000-7000-8000-%012x", 0x07+i)
			var (
				id, name, status, tags string
				sortOrder              int32
				premium                bool
			)
			err := pool.QueryRow(ctx, `SELECT id::text, name, status, tags::text, sort_order, is_premium
				FROM templates WHERE slug = $1`, want.slug).Scan(&id, &name, &status, &tags, &sortOrder, &premium)
			if err != nil {
				t.Errorf("%s: read template: %v", want.slug, err)
				continue
			}
			wantTags := `{"occasions": ["` + want.occasion + `"`
			for _, e := range want.extra {
				wantTags += `, "` + e + `"`
			}
			wantTags += `]}`
			if id != wantID || name != want.name || status != "draft" || sortOrder != int32((i+1)*10) || premium || tags != wantTags {
				t.Errorf("%s: id=%s name=%q status=%s sort_order=%d premium=%v tags=%s, want id=%s name=%q draft sort_order=%d not premium tags=%s",
					want.slug, id, name, status, sortOrder, premium, tags, wantID, want.name, (i+1)*10, wantTags)
			}

			var (
				manifest  []byte
				version   int
				published bool
				assets    string
				size      int
			)
			err = pool.QueryRow(ctx, `SELECT version, manifest, published_at IS NOT NULL, assets_path, pg_column_size(manifest)
				FROM template_versions WHERE template_id = $1`, id).Scan(&version, &manifest, &published, &assets, &size)
			if err != nil { // also fails if there is more than one version
				t.Errorf("%s: read version: %v", want.slug, err)
				continue
			}
			if version != 1 || !published || assets != "" || size > 32768 {
				t.Errorf("%s: version=%d published=%v assets_path=%q manifest size=%d, want v1 published, no assets, <= 32768", want.slug, version, published, assets, size)
			}
			m, err := content.ValidateManifest(manifest)
			if err != nil {
				t.Errorf("%s: ValidateManifest: %v", want.slug, err)
				continue
			}
			if m.Schema != 2 {
				t.Errorf("%s: schema = %d, want 2", want.slug, m.Schema)
			}
			for _, p := range m.Palettes {
				for _, f := range m.Fonts {
					overrides := fmt.Sprintf(`{"palette":%q,"font":%q}`, p.ID, f.ID)
					th := content.ResolveTheme(m, []byte(overrides), "")
					if th.Engine != 2 || th.Palette != p.Colors || th.Fonts.Heading != f.Heading || th.Fonts.Body != f.Body {
						t.Errorf("%s: ResolveTheme(%s, %s) = engine %d %+v, want engine 2 with that palette and font pair", want.slug, p.ID, f.ID, th.Engine, th)
					}
				}
			}
		}

		var templates, versions int
		if err := pool.QueryRow(ctx, "SELECT count(*) FROM templates").Scan(&templates); err != nil || templates != 18 {
			t.Errorf("templates = %d (err %v), want 18", templates, err)
		}
		if err := pool.QueryRow(ctx, "SELECT count(*) FROM template_versions").Scan(&versions); err != nil || versions != 22 {
			t.Errorf("template versions = %d (err %v), want 22", versions, err)
		}

		// Drafts stay out of the public catalog: the picker still lists only the original six.
		listed, err := store.New(pool).ListPublishedTemplates(ctx, nil)
		if err != nil {
			t.Fatalf("list published templates: %v", err)
		}
		if len(listed) != 6 {
			t.Errorf("published templates = %d, want 6 (the draft themes must not be listed)", len(listed))
		}
		for _, r := range listed {
			for _, w := range wantSeeds {
				if r.Slug == w.slug {
					t.Errorf("draft template %s is listed in the public picker", w.slug)
				}
			}
		}
	}
	checkSeeded()

	// Pin an event to Olive Grove v1: Down must fail by FK, roll back fully, and leave the rows.
	oliveID := "01926a00-0000-7000-8000-000000000007"
	eventID := uuid.Must(uuid.NewV7())
	if _, err := pool.Exec(ctx, `INSERT INTO events (id, occasion_slug, title, content, template_id, template_version)
		VALUES ($1, 'wedding', 'pinned', '[]', $2, 1)`, eventID, oliveID); err != nil {
		t.Fatalf("insert pinned event: %v", err)
	}
	_, err := provider.DownTo(ctx, 6)
	var pgErr *pgconn.PgError
	if !errors.As(err, &pgErr) || pgErr.Code != "23503" {
		t.Fatalf("down with a pinned event: err = %v, want foreign_key_violation", err)
	}
	checkSeeded()
	var triggerEnabled string
	if err := pool.QueryRow(ctx, "SELECT tgenabled::text FROM pg_trigger WHERE tgname = 'template_versions_immutable'").Scan(&triggerEnabled); err != nil || triggerEnabled != "O" {
		t.Errorf("immutable trigger state after failed Down = %q (err %v), want enabled (O)", triggerEnabled, err)
	}

	if _, err := pool.Exec(ctx, "DELETE FROM events WHERE id = $1", eventID); err != nil {
		t.Fatalf("delete pinned event: %v", err)
	}
	if _, err := provider.DownTo(ctx, 6); err != nil {
		t.Fatalf("down to 6: %v", err)
	}
	var left int
	if err := pool.QueryRow(ctx, "SELECT count(*) FROM templates WHERE sort_order < 1000").Scan(&left); err != nil || left != 0 {
		t.Errorf("v2 templates after Down = %d (err %v), want 0", left, err)
	}
	if err := pool.QueryRow(ctx, "SELECT count(*) FROM template_versions").Scan(&left); err != nil || left != 10 {
		t.Errorf("template versions after Down = %d (err %v), want 10", left, err)
	}
	if err := pool.QueryRow(ctx, "SELECT tgenabled::text FROM pg_trigger WHERE tgname = 'template_versions_immutable'").Scan(&triggerEnabled); err != nil || triggerEnabled != "O" {
		t.Errorf("immutable trigger state after Down = %q (err %v), want enabled (O)", triggerEnabled, err)
	}

	if _, err := provider.Up(ctx); err != nil {
		t.Fatalf("up again: %v", err)
	}
	checkSeeded()
}
