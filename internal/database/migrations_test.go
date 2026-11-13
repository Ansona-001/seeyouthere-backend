package database

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
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

// pickerSlugs returns the slugs the host picker listed before premium templates became selectable
// (published, non-premium, with a published version; templates.sort_order, name, id). It runs plain SQL
// instead of store.ListPublishedTemplates because the tests below run at schemas older than the
// price_paise column that query now selects.
func pickerSlugs(ctx context.Context, t *testing.T, pool *pgxpool.Pool) []string {
	t.Helper()
	rows, err := pool.Query(ctx, `SELECT t.slug FROM templates t
		WHERE t.status = 'published' AND NOT t.is_premium
		  AND EXISTS (SELECT 1 FROM template_versions tv WHERE tv.template_id = t.id AND tv.published_at IS NOT NULL)
		ORDER BY t.sort_order, t.name, t.id`)
	if err != nil {
		t.Fatalf("list picker templates: %v", err)
	}
	defer rows.Close()
	slugs := []string{}
	for rows.Next() {
		var slug string
		if err := rows.Scan(&slug); err != nil {
			t.Fatalf("scan picker template: %v", err)
		}
		slugs = append(slugs, slug)
	}
	if err := rows.Err(); err != nil {
		t.Fatalf("list picker templates: %v", err)
	}
	return slugs
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
		slugs := pickerSlugs(ctx, t, pool)
		if fmt.Sprint(slugs) != fmt.Sprint(wantOrder) {
			t.Errorf("picker order = %v, want %v", slugs, wantOrder)
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
		listed := pickerSlugs(ctx, t, pool)
		if len(listed) != 6 {
			t.Errorf("published templates = %d, want 6 (the draft themes must not be listed)", len(listed))
		}
		for _, slug := range listed {
			for _, w := range wantSeeds {
				if slug == w.slug {
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

	// Stop at 7: later migrations retag the seeded templates, which checkSeeded compares against 00007's tags.
	if _, err := provider.UpTo(ctx, 7); err != nil {
		t.Fatalf("up again: %v", err)
	}
	checkSeeded()
}

// TestMigration00025_Mimosa applies migrations up to 25 and checks the Mimosa seed: a draft wedding
// template with a published v1 whose stored manifest validates and resolves for every palette and
// font pair, and that it stays out of the host picker. It then runs 00025's Down with an event
// pinned to it (must fail by FK and change nothing), unpins it, runs Down again (both rows go),
// and applies Up again.
func TestMigration00025_Mimosa(t *testing.T) {
	ctx, pool, provider := scratchMigrator(t)
	if _, err := provider.UpTo(ctx, 24); err != nil {
		t.Fatalf("migrate to 24: %v", err)
	}
	var before int
	if err := pool.QueryRow(ctx, "SELECT count(*) FROM templates").Scan(&before); err != nil {
		t.Fatalf("count templates: %v", err)
	}
	if _, err := provider.UpTo(ctx, 25); err != nil {
		t.Fatalf("migrate to 25: %v", err)
	}

	const mimosaID = "01926a00-0000-7000-8000-000000000013"
	checkSeeded := func() {
		t.Helper()
		var (
			id, name, status, tags string
			sortOrder              int32
			premium                bool
		)
		err := pool.QueryRow(ctx, `SELECT id::text, name, status, tags::text, sort_order, is_premium
			FROM templates WHERE slug = 'mimosa'`).Scan(&id, &name, &status, &tags, &sortOrder, &premium)
		if err != nil {
			t.Fatalf("read template: %v", err)
		}
		if id != mimosaID || name != "Mimosa" || status != "draft" || sortOrder != 15 || premium || tags != `{"occasions": ["wedding"]}` {
			t.Errorf("template: id=%s name=%q status=%s sort_order=%d premium=%v tags=%s, want id=%s name=Mimosa draft sort_order=15 not premium tags={\"occasions\": [\"wedding\"]}",
				id, name, status, sortOrder, premium, tags, mimosaID)
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
			t.Fatalf("read version: %v", err)
		}
		if version != 1 || !published || assets != "" || size > 32768 {
			t.Errorf("version=%d published=%v assets_path=%q manifest size=%d, want v1 published, no assets, <= 32768", version, published, assets, size)
		}
		m, err := content.ValidateManifest(manifest)
		if err != nil {
			t.Fatalf("ValidateManifest: %v", err)
		}
		if m.Schema != 2 || len(m.Palettes) == 0 || len(m.Fonts) == 0 {
			t.Errorf("manifest schema=%d palettes=%d fonts=%d, want schema 2 with palettes and fonts", m.Schema, len(m.Palettes), len(m.Fonts))
		}
		for _, p := range m.Palettes {
			for _, f := range m.Fonts {
				overrides := fmt.Sprintf(`{"palette":%q,"font":%q}`, p.ID, f.ID)
				th := content.ResolveTheme(m, []byte(overrides), "")
				if th.Engine != 2 || th.Palette != p.Colors || th.Fonts.Heading != f.Heading || th.Fonts.Body != f.Body {
					t.Errorf("ResolveTheme(%s, %s) = engine %d %+v, want engine 2 with that palette and font pair", p.ID, f.ID, th.Engine, th)
				}
			}
		}

		var templates int
		if err := pool.QueryRow(ctx, "SELECT count(*) FROM templates").Scan(&templates); err != nil || templates != before+1 {
			t.Errorf("templates = %d (err %v), want %d", templates, err, before+1)
		}
		// A draft stays out of the public catalog.
		for _, slug := range pickerSlugs(ctx, t, pool) {
			if slug == "mimosa" {
				t.Errorf("draft template mimosa is listed in the public picker")
			}
		}
	}
	checkSeeded()

	// Pin an event to Mimosa v1: Down must fail by FK, roll back fully, and leave the rows.
	eventID := uuid.Must(uuid.NewV7())
	if _, err := pool.Exec(ctx, `INSERT INTO events (id, occasion_slug, title, content, template_id, template_version)
		VALUES ($1, 'wedding', 'pinned', '[]', $2, 1)`, eventID, mimosaID); err != nil {
		t.Fatalf("insert pinned event: %v", err)
	}
	_, err := provider.DownTo(ctx, 24)
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
	if _, err := provider.DownTo(ctx, 24); err != nil {
		t.Fatalf("down to 24: %v", err)
	}
	var left int
	if err := pool.QueryRow(ctx, "SELECT count(*) FROM templates WHERE id = $1 OR slug = 'mimosa'", mimosaID).Scan(&left); err != nil || left != 0 {
		t.Errorf("mimosa templates after Down = %d (err %v), want 0", left, err)
	}
	if err := pool.QueryRow(ctx, "SELECT count(*) FROM template_versions WHERE template_id = $1", mimosaID).Scan(&left); err != nil || left != 0 {
		t.Errorf("mimosa versions after Down = %d (err %v), want 0", left, err)
	}
	if err := pool.QueryRow(ctx, "SELECT count(*) FROM templates").Scan(&left); err != nil || left != before {
		t.Errorf("templates after Down = %d (err %v), want %d", left, err, before)
	}
	if err := pool.QueryRow(ctx, "SELECT tgenabled::text FROM pg_trigger WHERE tgname = 'template_versions_immutable'").Scan(&triggerEnabled); err != nil || triggerEnabled != "O" {
		t.Errorf("immutable trigger state after Down = %q (err %v), want enabled (O)", triggerEnabled, err)
	}

	if _, err := provider.UpTo(ctx, 25); err != nil {
		t.Fatalf("up again: %v", err)
	}
	checkSeeded()
}

// TestMigration00026_BirthstoneHeadings applies migrations up to 26 and checks that every template whose
// latest version used Great Vibes as a heading got exactly one new published version using Birthstone
// there (manifest valid, pair ids and accents unchanged, old versions untouched), that other templates
// got none, and that Down removes only those versions and Up can run again.
func TestMigration00026_BirthstoneHeadings(t *testing.T) {
	ctx, pool, provider := scratchMigrator(t)
	if _, err := provider.UpTo(ctx, 25); err != nil {
		t.Fatalf("migrate to 25: %v", err)
	}
	versionCount := func() int {
		t.Helper()
		var n int
		if err := pool.QueryRow(ctx, "SELECT count(*) FROM template_versions").Scan(&n); err != nil {
			t.Fatalf("count versions: %v", err)
		}
		return n
	}
	before := versionCount()
	if _, err := provider.UpTo(ctx, 26); err != nil {
		t.Fatalf("migrate to 26: %v", err)
	}

	check := func() {
		t.Helper()
		if got := versionCount(); got != before+2 {
			t.Errorf("versions = %d, want %d (garden and mimosa get one each)", got, before+2)
		}
		for slug, wantVersion := range map[string]int{"mimosa": 2, "garden": 3} {
			var (
				manifest, prev []byte
				version        int
				published      bool
			)
			err := pool.QueryRow(ctx, `SELECT v.version, v.manifest, v.published_at IS NOT NULL
				FROM template_versions v JOIN templates t ON t.id = v.template_id
				WHERE t.slug = $1 ORDER BY v.version DESC LIMIT 1`, slug).Scan(&version, &manifest, &published)
			if err != nil {
				t.Fatalf("%s: read latest version: %v", slug, err)
			}
			if version != wantVersion || !published {
				t.Errorf("%s: latest version = %d published=%v, want v%d published", slug, version, published, wantVersion)
			}
			if err := pool.QueryRow(ctx, `SELECT v.manifest FROM template_versions v JOIN templates t ON t.id = v.template_id
				WHERE t.slug = $1 AND v.version = $2`, slug, wantVersion-1).Scan(&prev); err != nil {
				t.Fatalf("%s: read previous version: %v", slug, err)
			}
			m, err := content.ValidateManifest(manifest)
			if err != nil {
				t.Fatalf("%s: ValidateManifest: %v", slug, err)
			}
			old, err := content.ValidateManifest(prev)
			if err != nil {
				t.Fatalf("%s: ValidateManifest(previous): %v", slug, err)
			}
			if len(m.Fonts) != len(old.Fonts) {
				t.Fatalf("%s: %d font pairs, previous had %d", slug, len(m.Fonts), len(old.Fonts))
			}
			for i, f := range m.Fonts {
				o := old.Fonts[i]
				wantHeading := o.Heading
				if o.Heading == "great_vibes" {
					wantHeading = "birthstone"
				}
				if f.ID != o.ID || f.Heading != wantHeading || f.Body != o.Body || f.Accent != o.Accent {
					t.Errorf("%s pair %d = %+v, want id/body/accent of %+v with heading %s", slug, i, f, o, wantHeading)
				}
				if strings.Contains(f.Name, "Great Vibes") && f.Heading == "birthstone" {
					t.Errorf("%s pair %q is still named %q", slug, f.ID, f.Name)
				}
			}
		}
		var left int
		if err := pool.QueryRow(ctx, `SELECT count(*) FROM template_versions v
			WHERE v.version = (SELECT max(version) FROM template_versions WHERE template_id = v.template_id)
			  AND EXISTS (SELECT 1 FROM jsonb_array_elements(v.manifest->'fonts') f WHERE f->>'heading' = 'great_vibes')`).Scan(&left); err != nil || left != 0 {
			t.Errorf("latest versions still using Great Vibes as a heading = %d (err %v), want 0", left, err)
		}
	}
	check()

	if _, err := provider.DownTo(ctx, 25); err != nil {
		t.Fatalf("down to 25: %v", err)
	}
	if got := versionCount(); got != before {
		t.Errorf("versions after Down = %d, want %d", got, before)
	}
	var triggerEnabled string
	if err := pool.QueryRow(ctx, "SELECT tgenabled::text FROM pg_trigger WHERE tgname = 'template_versions_immutable'").Scan(&triggerEnabled); err != nil || triggerEnabled != "O" {
		t.Errorf("immutable trigger state after Down = %q (err %v), want enabled (O)", triggerEnabled, err)
	}
	if _, err := provider.UpTo(ctx, 26); err != nil {
		t.Fatalf("up again: %v", err)
	}
	check()
}

// TestMigration00027_MimosaGoldLabels applies migrations up to 27 and checks that Mimosa got exactly one
// new published version (previous latest + 1) with the deep gold in the art2 slot and
// ornament.label_ink = art2, that the manifest validates and resolves the gold as the label ink, that
// no other template got a version, and that Down removes only that version and Up can run again.
func TestMigration00027_MimosaGoldLabels(t *testing.T) {
	const gold = "#85690A"
	ctx, pool, provider := scratchMigrator(t)
	if _, err := provider.UpTo(ctx, 26); err != nil {
		t.Fatalf("migrate to 26: %v", err)
	}
	versionCount := func() int {
		t.Helper()
		var n int
		if err := pool.QueryRow(ctx, "SELECT count(*) FROM template_versions").Scan(&n); err != nil {
			t.Fatalf("count versions: %v", err)
		}
		return n
	}
	mimosaLatest := func() (version int, manifest []byte, published bool) {
		t.Helper()
		err := pool.QueryRow(ctx, `SELECT v.version, v.manifest, v.published_at IS NOT NULL
			FROM template_versions v JOIN templates t ON t.id = v.template_id
			WHERE t.slug = 'mimosa' ORDER BY v.version DESC LIMIT 1`).Scan(&version, &manifest, &published)
		if err != nil {
			t.Fatalf("read mimosa latest version: %v", err)
		}
		return version, manifest, published
	}
	before := versionCount()
	prevVersion, prevManifest, _ := mimosaLatest()
	if _, err := provider.UpTo(ctx, 27); err != nil {
		t.Fatalf("migrate to 27: %v", err)
	}

	check := func() {
		t.Helper()
		if got := versionCount(); got != before+1 {
			t.Errorf("versions = %d, want %d (only mimosa gets one)", got, before+1)
		}
		version, manifest, published := mimosaLatest()
		if version != prevVersion+1 || !published {
			t.Errorf("mimosa latest = v%d published=%v, want v%d published", version, published, prevVersion+1)
		}
		m, err := content.ValidateManifest(manifest)
		if err != nil {
			t.Fatalf("ValidateManifest: %v", err)
		}
		old, err := content.ValidateManifest(prevManifest)
		if err != nil {
			t.Fatalf("ValidateManifest(previous): %v", err)
		}
		if old.Ornament.LabelInk != "" || old.Palettes[0].Art[1] == gold {
			t.Errorf("previous version was modified: label_ink=%q art2=%q", old.Ornament.LabelInk, old.Palettes[0].Art[1])
		}
		if len(m.Palettes) != len(old.Palettes) || m.Palettes[0].ID != old.Palettes[0].ID || m.Defaults != old.Defaults || len(m.Fonts) != len(old.Fonts) {
			t.Errorf("palette/font ids changed: %+v vs %+v", m.Defaults, old.Defaults)
		}
		if got := m.Palettes[0].Art[1]; got != gold {
			t.Errorf("art2 = %q, want %q", got, gold)
		}
		if m.Ornament.LabelInk != "art2" {
			t.Errorf("ornament.label_ink = %q, want art2", m.Ornament.LabelInk)
		}
		if th := content.ResolveTheme(m, nil, ""); th.Ornament == nil || th.Ornament.LabelInk != gold {
			t.Errorf("resolved label_ink = %+v, want %s", th.Ornament, gold)
		}
		var others int
		if err := pool.QueryRow(ctx, `SELECT count(*) FROM template_versions v JOIN templates t ON t.id = v.template_id
			WHERE t.slug <> 'mimosa' AND v.manifest->'ornament' ? 'label_ink' AND v.manifest->'ornament'->>'label_ink' <> ''`).Scan(&others); err != nil || others != 0 {
			t.Errorf("other templates with label_ink = %d (err %v), want 0", others, err)
		}
	}
	check()

	if _, err := provider.DownTo(ctx, 26); err != nil {
		t.Fatalf("down to 26: %v", err)
	}
	if got := versionCount(); got != before {
		t.Errorf("versions after Down = %d, want %d", got, before)
	}
	if v, _, _ := mimosaLatest(); v != prevVersion {
		t.Errorf("mimosa latest after Down = v%d, want v%d", v, prevVersion)
	}
	var triggerEnabled string
	if err := pool.QueryRow(ctx, "SELECT tgenabled::text FROM pg_trigger WHERE tgname = 'template_versions_immutable'").Scan(&triggerEnabled); err != nil || triggerEnabled != "O" {
		t.Errorf("immutable trigger state after Down = %q (err %v), want enabled (O)", triggerEnabled, err)
	}
	if _, err := provider.UpTo(ctx, 27); err != nil {
		t.Fatalf("up again: %v", err)
	}
	check()
}

// TestMigration00028_RetiredSlugOwner applies migrations up to 27, leaves a tombstone, then applies 00028:
// the old tombstone gets a NULL owner (held against everyone, its owner included), and the trigger now
// records the deleted event's owner, whom SlugTaken exempts. Down must restore 00011's trigger function
// exactly and drop the column and its index, with deletes still writing tombstones; Up then runs again.
func TestMigration00028_RetiredSlugOwner(t *testing.T) {
	ctx, pool, provider := scratchMigrator(t)
	if _, err := provider.UpTo(ctx, 27); err != nil {
		t.Fatalf("migrate to 27: %v", err)
	}
	functionDef := func() string {
		t.Helper()
		var def string
		if err := pool.QueryRow(ctx, "SELECT pg_get_functiondef('events_retire_slug'::regproc)").Scan(&def); err != nil {
			t.Fatalf("read events_retire_slug: %v", err)
		}
		return def
	}
	ownerColumns := func() int {
		t.Helper()
		var n int
		if err := pool.QueryRow(ctx, "SELECT count(*) FROM information_schema.columns WHERE table_name = 'retired_slugs' AND column_name = 'owner_id'").Scan(&n); err != nil {
			t.Fatalf("count owner_id columns: %v", err)
		}
		return n
	}
	ownerIndexes := func() int {
		t.Helper()
		var n int
		if err := pool.QueryRow(ctx, "SELECT count(*) FROM pg_indexes WHERE indexname = 'retired_slugs_owner_id_idx'").Scan(&n); err != nil {
			t.Fatalf("count owner_id indexes: %v", err)
		}
		return n
	}
	owner := uuid.Must(uuid.NewV7())
	if _, err := pool.Exec(ctx, "INSERT INTO users (id, email) VALUES ($1, $2)", owner, "mig28-"+owner.String()+"@example.invalid"); err != nil {
		t.Fatalf("insert user: %v", err)
	}
	// publishAndDelete hard-deletes a published event of owner's with slug, leaving a tombstone.
	publishAndDelete := func(slug string) {
		t.Helper()
		id := uuid.Must(uuid.NewV7())
		if _, err := pool.Exec(ctx, `
			INSERT INTO events (id, owner_id, occasion_slug, title, content, template_id, template_version, slug, status, published_at)
			SELECT $1, $2, 'birthday', 'mig28', '[]', t.id, tv.version, $3, 'published', now()
			FROM templates t JOIN template_versions tv ON tv.template_id = t.id LIMIT 1`, id, owner, slug); err != nil {
			t.Fatalf("insert published event %s: %v", slug, err)
		}
		if _, err := pool.Exec(ctx, "DELETE FROM events WHERE id = $1", id); err != nil {
			t.Fatalf("delete event %s: %v", slug, err)
		}
	}
	before := functionDef()
	publishAndDelete("mig28-legacy")

	if _, err := provider.UpTo(ctx, 28); err != nil {
		t.Fatalf("migrate to 28: %v", err)
	}
	q := store.New(pool)
	taken := func(slug string) bool {
		t.Helper()
		got, err := q.SlugTaken(ctx, store.SlugTakenParams{Slug: slug, UserID: &owner})
		if err != nil {
			t.Fatalf("SlugTaken(%s): %v", slug, err)
		}
		return got
	}
	checkUp := func() {
		t.Helper()
		var legacyOwner *uuid.UUID
		if err := pool.QueryRow(ctx, "SELECT owner_id FROM retired_slugs WHERE slug = 'mig28-legacy'").Scan(&legacyOwner); err != nil || legacyOwner != nil {
			t.Errorf("pre-migration tombstone owner = %v (err %v), want NULL", legacyOwner, err)
		}
		if !taken("mig28-legacy") {
			t.Error("pre-migration tombstone is not held against its former owner, want held (owner unknown)")
		}
		publishAndDelete("mig28-new")
		if taken("mig28-new") {
			t.Error("new tombstone is held against its owner, want the owner exempt")
		}
		if _, err := pool.Exec(ctx, "DELETE FROM retired_slugs WHERE slug = 'mig28-new'"); err != nil {
			t.Fatalf("clear new tombstone: %v", err)
		}
		if n := ownerIndexes(); n != 1 {
			t.Errorf("retired_slugs_owner_id_idx count = %d, want 1", n)
		}
	}
	checkUp()
	if functionDef() == before {
		t.Error("events_retire_slug unchanged by 00028")
	}

	if _, err := provider.DownTo(ctx, 27); err != nil {
		t.Fatalf("down to 27: %v", err)
	}
	if got := functionDef(); got != before {
		t.Errorf("events_retire_slug after Down = %q, want 00011's %q", got, before)
	}
	if n := ownerColumns(); n != 0 {
		t.Errorf("owner_id columns after Down = %d, want 0", n)
	}
	if n := ownerIndexes(); n != 0 {
		t.Errorf("owner_id indexes after Down = %d, want 0", n)
	}
	publishAndDelete("mig28-down")
	var n int
	if err := pool.QueryRow(ctx, "SELECT count(*) FROM retired_slugs WHERE slug = 'mig28-down' AND retired_at > now() - interval '1 minute'").Scan(&n); err != nil || n != 1 {
		t.Errorf("tombstones written after Down = %d (err %v), want 1", n, err)
	}

	if _, err := provider.UpTo(ctx, 28); err != nil {
		t.Fatalf("up again: %v", err)
	}
	checkUp()
}

// TestMigration00029_MimosaMusic applies migrations up to 29 and checks that Mimosa got exactly one new
// published version (previous latest + 1) that differs only by music = music_box, that the manifest
// validates and resolves the music, that no other template got a version, and that Down removes only
// that version and Up can run again.
func TestMigration00029_MimosaMusic(t *testing.T) {
	ctx, pool, provider := scratchMigrator(t)
	if _, err := provider.UpTo(ctx, 28); err != nil {
		t.Fatalf("migrate to 28: %v", err)
	}
	versionCount := func() int {
		t.Helper()
		var n int
		if err := pool.QueryRow(ctx, "SELECT count(*) FROM template_versions").Scan(&n); err != nil {
			t.Fatalf("count versions: %v", err)
		}
		return n
	}
	mimosaLatest := func() (version int, manifest []byte, published bool) {
		t.Helper()
		err := pool.QueryRow(ctx, `SELECT v.version, v.manifest, v.published_at IS NOT NULL
			FROM template_versions v JOIN templates t ON t.id = v.template_id
			WHERE t.slug = 'mimosa' ORDER BY v.version DESC LIMIT 1`).Scan(&version, &manifest, &published)
		if err != nil {
			t.Fatalf("read mimosa latest version: %v", err)
		}
		return version, manifest, published
	}
	before := versionCount()
	prevVersion, prevManifest, _ := mimosaLatest()
	if _, err := provider.UpTo(ctx, 29); err != nil {
		t.Fatalf("migrate to 29: %v", err)
	}

	check := func() {
		t.Helper()
		if got := versionCount(); got != before+1 {
			t.Errorf("versions = %d, want %d (only mimosa gets one)", got, before+1)
		}
		version, manifest, published := mimosaLatest()
		if version != prevVersion+1 || !published {
			t.Errorf("mimosa latest = v%d published=%v, want v%d published", version, published, prevVersion+1)
		}
		m, err := content.ValidateManifest(manifest)
		if err != nil {
			t.Fatalf("ValidateManifest: %v", err)
		}
		old, err := content.ValidateManifest(prevManifest)
		if err != nil {
			t.Fatalf("ValidateManifest(previous): %v", err)
		}
		if old.Music != "" {
			t.Errorf("previous version was modified: music=%q", old.Music)
		}
		if len(m.Palettes) != len(old.Palettes) || m.Palettes[0].ID != old.Palettes[0].ID || m.Defaults != old.Defaults || len(m.Fonts) != len(old.Fonts) {
			t.Errorf("palette/font ids changed: %+v vs %+v", m.Defaults, old.Defaults)
		}
		if m.Music != "music_box" {
			t.Errorf("music = %q, want music_box", m.Music)
		}
		if th := content.ResolveTheme(m, nil, ""); th.Music != "music_box" {
			t.Errorf("resolved music = %q, want music_box", th.Music)
		}
		var others int
		if err := pool.QueryRow(ctx, `SELECT count(*) FROM template_versions v JOIN templates t ON t.id = v.template_id
			WHERE t.slug <> 'mimosa' AND v.manifest ? 'music'`).Scan(&others); err != nil || others != 0 {
			t.Errorf("other templates with music = %d (err %v), want 0", others, err)
		}
	}
	check()

	if _, err := provider.DownTo(ctx, 28); err != nil {
		t.Fatalf("down to 28: %v", err)
	}
	if got := versionCount(); got != before {
		t.Errorf("versions after Down = %d, want %d", got, before)
	}
	if v, _, _ := mimosaLatest(); v != prevVersion {
		t.Errorf("mimosa latest after Down = v%d, want v%d", v, prevVersion)
	}
	var triggerEnabled string
	if err := pool.QueryRow(ctx, "SELECT tgenabled::text FROM pg_trigger WHERE tgname = 'template_versions_immutable'").Scan(&triggerEnabled); err != nil || triggerEnabled != "O" {
		t.Errorf("immutable trigger state after Down = %q (err %v), want enabled (O)", triggerEnabled, err)
	}
	if _, err := provider.UpTo(ctx, 29); err != nil {
		t.Fatalf("up again: %v", err)
	}
	check()
}

// TestMigration00032_EventAudio applies migrations up to 32 and checks the events.music_source default
// and CHECK, the event_audio size/duration CHECKs, the one-track-per-event UNIQUE, the CASCADE on event
// delete, the SET NULL on uploader delete, and that Down removes the table and column and Up can run again.
func TestMigration00032_EventAudio(t *testing.T) {
	ctx, pool, provider := scratchMigrator(t)
	if _, err := provider.UpTo(ctx, 31); err != nil {
		t.Fatalf("migrate to 31: %v", err)
	}
	owner := uuid.Must(uuid.NewV7())
	if _, err := pool.Exec(ctx, "INSERT INTO users (id, email) VALUES ($1, $2)", owner, "mig32-"+owner.String()+"@example.invalid"); err != nil {
		t.Fatalf("insert user: %v", err)
	}
	insertEvent := func() uuid.UUID {
		t.Helper()
		id := uuid.Must(uuid.NewV7())
		if _, err := pool.Exec(ctx, `
			INSERT INTO events (id, owner_id, occasion_slug, title, content, template_id, template_version)
			SELECT $1, $2, 'birthday', 'mig32', '[]', t.id, tv.version
			FROM templates t JOIN template_versions tv ON tv.template_id = t.id LIMIT 1`, id, owner); err != nil {
			t.Fatalf("insert event: %v", err)
		}
		return id
	}
	existing := insertEvent()
	if _, err := provider.UpTo(ctx, 32); err != nil {
		t.Fatalf("migrate to 32: %v", err)
	}

	source := func(id uuid.UUID) string {
		t.Helper()
		var s string
		if err := pool.QueryRow(ctx, "SELECT music_source FROM events WHERE id = $1", id).Scan(&s); err != nil {
			t.Fatalf("read music_source: %v", err)
		}
		return s
	}
	if got := source(existing); got != "template" {
		t.Errorf("music_source of a pre-existing event = %q, want template", got)
	}
	if got := source(insertEvent()); got != "template" {
		t.Errorf("music_source of a new event = %q, want template", got)
	}
	for _, v := range []string{"custom", "none", "template"} {
		if _, err := pool.Exec(ctx, "UPDATE events SET music_source = $1 WHERE id = $2", v, existing); err != nil {
			t.Errorf("music_source %q rejected: %v", v, err)
		}
	}
	wantCode := func(name string, err error, code string) {
		t.Helper()
		var pgErr *pgconn.PgError
		if !errors.As(err, &pgErr) || pgErr.Code != code {
			t.Errorf("%s: err = %v, want SQLSTATE %s", name, err, code)
		}
	}
	for _, v := range []string{"", "Custom", "mp3", "silent"} {
		_, err := pool.Exec(ctx, "UPDATE events SET music_source = $1 WHERE id = $2", v, existing)
		wantCode("music_source "+v, err, "23514")
	}
	if _, err := pool.Exec(ctx, "UPDATE events SET music_source = NULL WHERE id = $1", existing); err == nil {
		t.Error("NULL music_source accepted")
	}

	insertAudio := func(eventID uuid.UUID, uploader *uuid.UUID, size int64, dur int) error {
		_, err := pool.Exec(ctx, `INSERT INTO event_audio (id, event_id, uploaded_by, size_bytes, duration_ms)
			VALUES ($1, $2, $3, $4, $5)`, uuid.Must(uuid.NewV7()), eventID, uploader, size, dur)
		return err
	}
	probe := insertEvent()
	for _, c := range []struct {
		name string
		size int64
		dur  int
	}{
		{"size 0", 0, 30000}, {"size over 5 MiB", 5242881, 30000},
		{"duration under 1s", 1000, 999}, {"duration over 10m", 1000, 600001},
	} {
		wantCode(c.name, insertAudio(probe, &owner, c.size, c.dur), "23514")
	}
	wantCode("missing event", insertAudio(uuid.Must(uuid.NewV7()), &owner, 1000, 30000), "23503")
	for _, c := range []struct {
		size int64
		dur  int
	}{{1, 1000}, {5242880, 600000}} {
		if err := insertAudio(insertEvent(), &owner, c.size, c.dur); err != nil {
			t.Errorf("boundary size=%d dur=%d rejected: %v", c.size, c.dur, err)
		}
	}

	// One track per event.
	if err := insertAudio(probe, &owner, 1000, 30000); err != nil {
		t.Fatalf("insert audio: %v", err)
	}
	wantCode("second track", insertAudio(probe, &owner, 1000, 30000), "23505")

	// Uploader delete keeps the track (SET NULL).
	other := uuid.Must(uuid.NewV7())
	if _, err := pool.Exec(ctx, "INSERT INTO users (id, email) VALUES ($1, $2)", other, "mig32-"+other.String()+"@example.invalid"); err != nil {
		t.Fatalf("insert user: %v", err)
	}
	kept := insertEvent()
	if err := insertAudio(kept, &other, 1000, 30000); err != nil {
		t.Fatalf("insert audio: %v", err)
	}
	if _, err := pool.Exec(ctx, "DELETE FROM users WHERE id = $1", other); err != nil {
		t.Fatalf("delete uploader: %v", err)
	}
	var uploader *uuid.UUID
	if err := pool.QueryRow(ctx, "SELECT uploaded_by FROM event_audio WHERE event_id = $1", kept).Scan(&uploader); err != nil || uploader != nil {
		t.Errorf("uploaded_by after uploader delete = %v (err %v), want NULL row kept", uploader, err)
	}

	// Event delete cascades.
	if _, err := pool.Exec(ctx, "DELETE FROM events WHERE id = $1", probe); err != nil {
		t.Fatalf("delete event: %v", err)
	}
	var n int
	if err := pool.QueryRow(ctx, "SELECT count(*) FROM event_audio WHERE event_id = $1", probe).Scan(&n); err != nil || n != 0 {
		t.Errorf("audio rows after event delete = %d (err %v), want 0 (CASCADE)", n, err)
	}

	// Indexes exist: FK index on uploaded_by and the unique key on event_id.
	if err := pool.QueryRow(ctx, `SELECT count(*) FROM pg_indexes WHERE tablename = 'event_audio'
		AND indexname IN ('event_audio_uploaded_by_idx', 'event_audio_event_key')`).Scan(&n); err != nil || n != 2 {
		t.Errorf("event_audio indexes = %d (err %v), want 2", n, err)
	}

	// Down removes the table and the column; Up again restores the default.
	if _, err := provider.DownTo(ctx, 31); err != nil {
		t.Fatalf("down to 31: %v", err)
	}
	if err := pool.QueryRow(ctx, `SELECT
		(SELECT count(*) FROM information_schema.tables WHERE table_name = 'event_audio') +
		(SELECT count(*) FROM information_schema.columns WHERE table_name = 'events' AND column_name = 'music_source')`).Scan(&n); err != nil || n != 0 {
		t.Errorf("event_audio table/music_source column after Down = %d (err %v), want 0", n, err)
	}
	if _, err := provider.UpTo(ctx, 32); err != nil {
		t.Fatalf("up again: %v", err)
	}
	if got := source(existing); got != "template" {
		t.Errorf("music_source after Up again = %q, want template", got)
	}
}

// TestMigration00033_HarvestMusic applies migrations up to 33 and checks that Harvest got exactly one new
// published version (previous latest + 1) that differs only by music = music_box, that the manifest
// validates and resolves the music, that no other template got a version, and that Down removes only
// that version and Up can run again.
func TestMigration00033_HarvestMusic(t *testing.T) {
	ctx, pool, provider := scratchMigrator(t)
	if _, err := provider.UpTo(ctx, 32); err != nil {
		t.Fatalf("migrate to 32: %v", err)
	}
	versionCount := func() int {
		t.Helper()
		var n int
		if err := pool.QueryRow(ctx, "SELECT count(*) FROM template_versions").Scan(&n); err != nil {
			t.Fatalf("count versions: %v", err)
		}
		return n
	}
	harvestLatest := func() (version int, manifest []byte, published bool) {
		t.Helper()
		err := pool.QueryRow(ctx, `SELECT v.version, v.manifest, v.published_at IS NOT NULL
			FROM template_versions v JOIN templates t ON t.id = v.template_id
			WHERE t.slug = 'harvest' ORDER BY v.version DESC LIMIT 1`).Scan(&version, &manifest, &published)
		if err != nil {
			t.Fatalf("read harvest latest version: %v", err)
		}
		return version, manifest, published
	}
	before := versionCount()
	prevVersion, prevManifest, _ := harvestLatest()
	if _, err := provider.UpTo(ctx, 33); err != nil {
		t.Fatalf("migrate to 33: %v", err)
	}

	check := func() {
		t.Helper()
		if got := versionCount(); got != before+1 {
			t.Errorf("versions = %d, want %d (only harvest gets one)", got, before+1)
		}
		version, manifest, published := harvestLatest()
		if version != prevVersion+1 || !published {
			t.Errorf("harvest latest = v%d published=%v, want v%d published", version, published, prevVersion+1)
		}
		m, err := content.ValidateManifest(manifest)
		if err != nil {
			t.Fatalf("ValidateManifest: %v", err)
		}
		old, err := content.ValidateManifest(prevManifest)
		if err != nil {
			t.Fatalf("ValidateManifest(previous): %v", err)
		}
		if old.Music != "" {
			t.Errorf("previous version was modified: music=%q", old.Music)
		}
		if len(m.Palettes) != len(old.Palettes) || m.Palettes[0].ID != old.Palettes[0].ID || m.Defaults != old.Defaults || len(m.Fonts) != len(old.Fonts) {
			t.Errorf("palette/font ids changed: %+v vs %+v", m.Defaults, old.Defaults)
		}
		if m.Music != "music_box" {
			t.Errorf("music = %q, want music_box", m.Music)
		}
		if th := content.ResolveTheme(m, nil, ""); th.Music != "music_box" {
			t.Errorf("resolved music = %q, want music_box", th.Music)
		}
		var others int
		if err := pool.QueryRow(ctx, `SELECT count(*) FROM template_versions v JOIN templates t ON t.id = v.template_id
			WHERE t.slug NOT IN ('harvest', 'mimosa') AND v.manifest ? 'music'`).Scan(&others); err != nil || others != 0 {
			t.Errorf("other templates with music = %d (err %v), want 0", others, err)
		}
	}
	check()

	if _, err := provider.DownTo(ctx, 32); err != nil {
		t.Fatalf("down to 32: %v", err)
	}
	if got := versionCount(); got != before {
		t.Errorf("versions after Down = %d, want %d", got, before)
	}
	if v, _, _ := harvestLatest(); v != prevVersion {
		t.Errorf("harvest latest after Down = v%d, want v%d", v, prevVersion)
	}
	var triggerEnabled string
	if err := pool.QueryRow(ctx, "SELECT tgenabled::text FROM pg_trigger WHERE tgname = 'template_versions_immutable'").Scan(&triggerEnabled); err != nil || triggerEnabled != "O" {
		t.Errorf("immutable trigger state after Down = %q (err %v), want enabled (O)", triggerEnabled, err)
	}
	if _, err := provider.UpTo(ctx, 33); err != nil {
		t.Fatalf("up again: %v", err)
	}
	check()
}

// TestMigration00036_PremiumUnlock checks the price column and its range CHECK, the event_payments
// constraints (one grant per event+template, amount/note/method/currency CHECKs, FKs), that deleting
// an event keeps the payment row with event_id NULL (financial record outlives the event), that a
// template with payments cannot be deleted, and that Down removes everything and Up can run again.
func TestMigration00036_PremiumUnlock(t *testing.T) {
	ctx, pool, provider := scratchMigrator(t)
	if _, err := provider.UpTo(ctx, 36); err != nil {
		t.Fatalf("migrate to 36: %v", err)
	}
	wantCode := func(name string, err error, code string) {
		t.Helper()
		var pgErr *pgconn.PgError
		if !errors.As(err, &pgErr) || pgErr.Code != code {
			t.Errorf("%s: err = %v, want SQLSTATE %s", name, err, code)
		}
	}

	// Existing templates get the default price.
	var n int
	if err := pool.QueryRow(ctx, "SELECT count(*) FROM templates WHERE price_paise <> 59900").Scan(&n); err != nil || n != 0 {
		t.Errorf("templates without the default price = %d (err %v), want 0", n, err)
	}
	var tmpl uuid.UUID
	if err := pool.QueryRow(ctx, "SELECT id FROM templates ORDER BY id LIMIT 1").Scan(&tmpl); err != nil {
		t.Fatalf("pick template: %v", err)
	}
	for _, p := range []int{99, 1000001, 0, -1} {
		_, err := pool.Exec(ctx, "UPDATE templates SET price_paise = $1 WHERE id = $2", p, tmpl)
		wantCode(fmt.Sprintf("price %d", p), err, "23514")
	}
	for _, p := range []int{100, 1000000, 59900} {
		if _, err := pool.Exec(ctx, "UPDATE templates SET price_paise = $1 WHERE id = $2", p, tmpl); err != nil {
			t.Errorf("price %d rejected: %v", p, err)
		}
	}

	owner := uuid.Must(uuid.NewV7())
	if _, err := pool.Exec(ctx, "INSERT INTO users (id, email) VALUES ($1, $2)", owner, "mig36-"+owner.String()+"@example.invalid"); err != nil {
		t.Fatalf("insert user: %v", err)
	}
	insertEvent := func() uuid.UUID {
		t.Helper()
		id := uuid.Must(uuid.NewV7())
		if _, err := pool.Exec(ctx, `
			INSERT INTO events (id, owner_id, occasion_slug, title, content, template_id, template_version)
			SELECT $1, $2, 'birthday', 'mig36', '[]', t.id, tv.version
			FROM templates t JOIN template_versions tv ON tv.template_id = t.id WHERE t.id = $3 LIMIT 1`, id, owner, tmpl); err != nil {
			t.Fatalf("insert event: %v", err)
		}
		return id
	}
	insertPayment := func(eventID *uuid.UUID, by *uuid.UUID, amount int, method, currency, note string) error {
		_, err := pool.Exec(ctx, `INSERT INTO event_payments (id, event_id, template_id, granted_by, amount_paise, method, currency, note)
			VALUES ($1, $2, $3, $4, $5, $6, $7, $8)`, uuid.Must(uuid.NewV7()), eventID, tmpl, by, amount, method, currency, note)
		return err
	}

	ev := insertEvent()
	if err := insertPayment(&ev, &owner, 59900, "manual", "INR", "bank transfer"); err != nil {
		t.Fatalf("insert payment: %v", err)
	}
	// One grant per (event, template).
	wantCode("duplicate grant", insertPayment(&ev, &owner, 59900, "manual", "INR", ""), "23505")

	probe := insertEvent()
	for _, c := range []struct {
		name     string
		amount   int
		method   string
		currency string
		note     string
	}{
		{"amount 99", 99, "manual", "INR", ""}, {"amount over max", 1000001, "manual", "INR", ""},
		{"method stripe", 59900, "stripe", "INR", ""}, {"currency USD", 59900, "manual", "USD", ""},
		{"note 201 chars", 59900, "manual", "INR", strings.Repeat("x", 201)},
	} {
		wantCode(c.name, insertPayment(&probe, &owner, c.amount, c.method, c.currency, c.note), "23514")
	}
	if err := insertPayment(&probe, nil, 100, "manual", "INR", strings.Repeat("x", 200)); err != nil {
		t.Errorf("boundary payment (amount 100, note 200, no granter) rejected: %v", err)
	}

	// Defaults for method, currency and note.
	defaulted := insertEvent()
	var method, currency, note string
	if _, err := pool.Exec(ctx, `INSERT INTO event_payments (id, event_id, template_id, amount_paise)
		VALUES ($1, $2, $3, 100)`, uuid.Must(uuid.NewV7()), defaulted, tmpl); err != nil {
		t.Fatalf("insert payment with defaults: %v", err)
	}
	if err := pool.QueryRow(ctx, "SELECT method, currency, note FROM event_payments WHERE event_id = $1", defaulted).
		Scan(&method, &currency, &note); err != nil || method != "manual" || currency != "INR" || note != "" {
		t.Errorf("defaults = %q %q %q (err %v), want manual INR empty", method, currency, note, err)
	}

	// FKs: unknown event and unknown template are rejected.
	missing := uuid.Must(uuid.NewV7())
	wantCode("missing event", insertPayment(&missing, &owner, 59900, "manual", "INR", ""), "23503")
	_, err := pool.Exec(ctx, `INSERT INTO event_payments (id, event_id, template_id, amount_paise) VALUES ($1, $2, $3, 100)`,
		uuid.Must(uuid.NewV7()), ev, missing)
	wantCode("missing template", err, "23503")

	// Granter delete keeps the payment (SET NULL).
	admin := uuid.Must(uuid.NewV7())
	if _, err := pool.Exec(ctx, "INSERT INTO users (id, email) VALUES ($1, $2)", admin, "mig36-"+admin.String()+"@example.invalid"); err != nil {
		t.Fatalf("insert user: %v", err)
	}
	granted := insertEvent()
	if err := insertPayment(&granted, &admin, 59900, "manual", "INR", ""); err != nil {
		t.Fatalf("insert payment: %v", err)
	}
	if _, err := pool.Exec(ctx, "DELETE FROM users WHERE id = $1", admin); err != nil {
		t.Fatalf("delete granter: %v", err)
	}
	var by *uuid.UUID
	if err := pool.QueryRow(ctx, "SELECT granted_by FROM event_payments WHERE event_id = $1", granted).Scan(&by); err != nil || by != nil {
		t.Errorf("granted_by after granter delete = %v (err %v), want NULL row kept", by, err)
	}

	// Event delete keeps the payment with event_id NULL; orphans do not collide on the unique key.
	for _, e := range []uuid.UUID{ev, granted} {
		if _, err := pool.Exec(ctx, "DELETE FROM events WHERE id = $1", e); err != nil {
			t.Fatalf("delete event: %v", err)
		}
	}
	if err := pool.QueryRow(ctx, "SELECT count(*) FROM event_payments WHERE event_id IS NULL AND template_id = $1", tmpl).Scan(&n); err != nil || n != 2 {
		t.Errorf("orphaned payments after event delete = %d (err %v), want 2 (SET NULL)", n, err)
	}

	// A template with payments cannot be deleted (RESTRICT).
	_, err = pool.Exec(ctx, "DELETE FROM templates WHERE id = $1", tmpl)
	wantCode("delete template with payments", err, "23503")

	// Indexes exist.
	if err := pool.QueryRow(ctx, `SELECT count(*) FROM pg_indexes WHERE tablename = 'event_payments'
		AND indexname IN ('event_payments_event_template_key', 'event_payments_template_id_idx', 'event_payments_granted_by_idx')`).Scan(&n); err != nil || n != 3 {
		t.Errorf("event_payments indexes = %d (err %v), want 3", n, err)
	}

	// Down removes the table and the column; Up again restores them.
	if _, err := provider.DownTo(ctx, 35); err != nil {
		t.Fatalf("down to 35: %v", err)
	}
	if err := pool.QueryRow(ctx, `SELECT
		(SELECT count(*) FROM information_schema.tables WHERE table_name = 'event_payments') +
		(SELECT count(*) FROM information_schema.columns WHERE table_name = 'templates' AND column_name = 'price_paise')`).Scan(&n); err != nil || n != 0 {
		t.Errorf("event_payments table/price_paise column after Down = %d (err %v), want 0", n, err)
	}
	if _, err := provider.UpTo(ctx, 36); err != nil {
		t.Fatalf("up again: %v", err)
	}
	if err := pool.QueryRow(ctx, "SELECT count(*) FROM templates WHERE price_paise = 59900").Scan(&n); err != nil || n == 0 {
		t.Errorf("templates at the default price after Up again = %d (err %v), want > 0", n, err)
	}
}

// TestMigration00037_RegionalPrices checks the template_prices backfill and constraints, the
// event_payments region/currency/revoke columns, the partial unique index (a revoked row does not
// block a re-grant, two live rows collide), the grant/revoke/restore queries end to end, and that Down
// refuses to drop non-IN or revoked payments, otherwise restores the 00036 shape, and Up can run again.
func TestMigration00037_RegionalPrices(t *testing.T) {
	ctx, pool, provider := scratchMigrator(t)
	if _, err := provider.UpTo(ctx, 36); err != nil {
		t.Fatalf("migrate to 36: %v", err)
	}
	wantCode := func(name string, err error, code string) {
		t.Helper()
		var pgErr *pgconn.PgError
		if !errors.As(err, &pgErr) || pgErr.Code != code {
			t.Errorf("%s: err = %v, want SQLSTATE %s", name, err, code)
		}
	}

	// Two templates with a version: one premium, one free. A pre-existing INR payment must survive Up.
	rows, err := pool.Query(ctx, `SELECT t.id FROM templates t
		WHERE EXISTS (SELECT 1 FROM template_versions v WHERE v.template_id = t.id) ORDER BY t.id LIMIT 2`)
	if err != nil {
		t.Fatalf("pick templates: %v", err)
	}
	var ids []uuid.UUID
	for rows.Next() {
		var id uuid.UUID
		if err := rows.Scan(&id); err != nil {
			t.Fatal(err)
		}
		ids = append(ids, id)
	}
	rows.Close()
	if len(ids) != 2 {
		t.Fatalf("need 2 templates with a version, got %d", len(ids))
	}
	prem, free := ids[0], ids[1]
	if _, err := pool.Exec(ctx, "UPDATE templates SET is_premium = true WHERE id = $1", prem); err != nil {
		t.Fatalf("make premium: %v", err)
	}

	owner := uuid.Must(uuid.NewV7())
	if _, err := pool.Exec(ctx, "INSERT INTO users (id, email) VALUES ($1, $2)", owner, "mig37-"+owner.String()+"@example.invalid"); err != nil {
		t.Fatalf("insert user: %v", err)
	}
	insertEvent := func(tmpl uuid.UUID, status string) uuid.UUID {
		t.Helper()
		id := uuid.Must(uuid.NewV7())
		if _, err := pool.Exec(ctx, `
			INSERT INTO events (id, owner_id, slug, occasion_slug, title, content, template_id, template_version, status, published_at)
			SELECT $1, $2, $5, 'birthday', 'mig37', '[]', t.id, tv.version, $4::text,
			       CASE WHEN $4::text = 'draft' THEN NULL ELSE now() END
			FROM templates t JOIN template_versions tv ON tv.template_id = t.id WHERE t.id = $3 LIMIT 1`,
			id, owner, tmpl, status, "mig37-"+id.String()); err != nil {
			t.Fatalf("insert event: %v", err)
		}
		return id
	}
	legacy := insertEvent(prem, "published")
	if _, err := pool.Exec(ctx, `INSERT INTO event_payments (id, event_id, template_id, amount_paise)
		VALUES ($1, $2, $3, 59900)`, uuid.Must(uuid.NewV7()), legacy, prem); err != nil {
		t.Fatalf("insert legacy payment: %v", err)
	}

	if _, err := provider.UpTo(ctx, 37); err != nil {
		t.Fatalf("migrate to 37: %v", err)
	}

	// Backfill: the premium template gets AE 2900 AED and INT 900 USD, the free one gets nothing.
	var n int
	for _, c := range []struct {
		tmpl     uuid.UUID
		region   string
		currency string
		amount   int
		want     int
	}{
		{prem, "AE", "AED", 2900, 1}, {prem, "INT", "USD", 900, 1}, {free, "AE", "AED", 2900, 0}, {free, "INT", "USD", 900, 0},
	} {
		if err := pool.QueryRow(ctx, `SELECT count(*) FROM template_prices
			WHERE template_id = $1 AND region = $2 AND currency = $3 AND amount_minor = $4`,
			c.tmpl, c.region, c.currency, c.amount).Scan(&n); err != nil || n != c.want {
			t.Errorf("backfill %+v: rows = %d (err %v), want %d", c, n, err, c.want)
		}
	}
	// The legacy payment is an India/INR sale with the amount carried over.
	var region, currency string
	var amount int
	if err := pool.QueryRow(ctx, "SELECT region, currency, amount_minor FROM event_payments WHERE event_id = $1", legacy).
		Scan(&region, &currency, &amount); err != nil || region != "IN" || currency != "INR" || amount != 59900 {
		t.Errorf("legacy payment = %q %q %d (err %v), want IN INR 59900", region, currency, amount, err)
	}

	// template_prices constraints.
	insertPrice := func(tmpl uuid.UUID, region, currency string, amount int) error {
		_, err := pool.Exec(ctx, "INSERT INTO template_prices (template_id, region, currency, amount_minor) VALUES ($1, $2, $3, $4)",
			tmpl, region, currency, amount)
		return err
	}
	wantCode("duplicate price", insertPrice(prem, "AE", "AED", 3000), "23505")
	wantCode("region IN not allowed", insertPrice(free, "IN", "INR", 5000), "23514")
	wantCode("unknown region", insertPrice(free, "US", "USD", 5000), "23514")
	wantCode("AE with USD", insertPrice(free, "AE", "USD", 5000), "23514")
	wantCode("INT with AED", insertPrice(free, "INT", "AED", 5000), "23514")
	wantCode("amount 99", insertPrice(free, "AE", "AED", 99), "23514")
	wantCode("amount over max", insertPrice(free, "AE", "AED", 1000001), "23514")
	wantCode("unknown template", insertPrice(uuid.Must(uuid.NewV7()), "AE", "AED", 5000), "23503")
	for _, a := range []int{100, 1000000} {
		if err := insertPrice(free, "AE", "AED", a); err != nil {
			t.Errorf("amount %d rejected: %v", a, err)
		}
		if _, err := pool.Exec(ctx, "DELETE FROM template_prices WHERE template_id = $1", free); err != nil {
			t.Fatal(err)
		}
	}

	// event_payments region/currency checks.
	insertPayment := func(event uuid.UUID, region, currency string) error {
		_, err := pool.Exec(ctx, `INSERT INTO event_payments (id, event_id, template_id, region, currency, amount_minor)
			VALUES ($1, $2, $3, $4, $5, 2900)`, uuid.Must(uuid.NewV7()), event, prem, region, currency)
		return err
	}
	probe := insertEvent(prem, "draft")
	for _, c := range [][2]string{{"IN", "AED"}, {"IN", "USD"}, {"AE", "INR"}, {"AE", "USD"}, {"INT", "INR"}, {"INT", "AED"}, {"US", "USD"}, {"IN", "EUR"}} {
		wantCode(fmt.Sprintf("payment %s/%s", c[0], c[1]), insertPayment(probe, c[0], c[1]), "23514")
	}

	// Partial unique index: a live row blocks a second live row; a revoked row does not.
	if err := insertPayment(probe, "AE", "AED"); err != nil {
		t.Fatalf("live payment AE: %v", err)
	}
	wantCode("two live rows (same region)", insertPayment(probe, "AE", "AED"), "23505")
	wantCode("two live rows (other region)", insertPayment(probe, "INT", "USD"), "23505")
	if _, err := pool.Exec(ctx, "UPDATE event_payments SET revoked_at = now() WHERE event_id = $1", probe); err != nil {
		t.Fatalf("revoke: %v", err)
	}
	if err := insertPayment(probe, "INT", "USD"); err != nil {
		t.Errorf("re-grant after revoke rejected: %v", err)
	}
	if err := pool.QueryRow(ctx, "SELECT count(*) FROM event_payments WHERE event_id = $1", probe).Scan(&n); err != nil || n != 2 {
		t.Errorf("rows for probe = %d (err %v), want 2 (revoked + live)", n, err)
	}

	// revoked_by needs revoked_at; deleting the revoker keeps the revoked row.
	revoker := uuid.Must(uuid.NewV7())
	if _, err := pool.Exec(ctx, "INSERT INTO users (id, email) VALUES ($1, $2)", revoker, "mig37-"+revoker.String()+"@example.invalid"); err != nil {
		t.Fatalf("insert user: %v", err)
	}
	_, err = pool.Exec(ctx, "UPDATE event_payments SET revoked_by = $1 WHERE event_id = $2 AND revoked_at IS NULL", revoker, probe)
	wantCode("revoked_by without revoked_at", err, "23514")
	if _, err := pool.Exec(ctx, "UPDATE event_payments SET revoked_by = $1 WHERE event_id = $2 AND revoked_at IS NOT NULL", revoker, probe); err != nil {
		t.Fatalf("set revoked_by: %v", err)
	}
	if _, err := pool.Exec(ctx, "DELETE FROM users WHERE id = $1", revoker); err != nil {
		t.Fatalf("delete revoker: %v", err)
	}
	if err := pool.QueryRow(ctx, "SELECT count(*) FROM event_payments WHERE event_id = $1 AND revoked_at IS NOT NULL AND revoked_by IS NULL", probe).Scan(&n); err != nil || n != 1 {
		t.Errorf("revoked rows after revoker delete = %d (err %v), want 1 kept with NULL revoked_by", n, err)
	}

	// Deleting a template cascades its price rows.
	cascade := uuid.Must(uuid.NewV7())
	if _, err := pool.Exec(ctx, `INSERT INTO templates (id, slug, name, tags, is_premium) VALUES ($1, 'mig37-cascade', 'c', '{}', true)`, cascade); err != nil {
		t.Fatalf("insert template: %v", err)
	}
	if err := insertPrice(cascade, "AE", "AED", 2900); err != nil {
		t.Fatalf("insert price: %v", err)
	}
	if _, err := pool.Exec(ctx, "DELETE FROM templates WHERE id = $1", cascade); err != nil {
		t.Fatalf("delete template: %v", err)
	}
	if err := pool.QueryRow(ctx, "SELECT count(*) FROM template_prices WHERE template_id = $1", cascade).Scan(&n); err != nil || n != 0 {
		t.Errorf("prices after template delete = %d (err %v), want 0 (CASCADE)", n, err)
	}

	// GrantEventPayment takes amount and currency from the DB.
	q := store.New(pool)
	// livePrice is the price the admin would confirm: 0 when the template has none for the region.
	livePrice := func(tmpl uuid.UUID, region string) int32 {
		t.Helper()
		var amount *int32
		err := pool.QueryRow(ctx, `SELECT CASE WHEN $2::text = 'IN' THEN t.price_paise ELSE p.amount_minor END
			FROM templates t LEFT JOIN template_prices p ON p.template_id = t.id AND p.region = $2::text
			WHERE t.id = $1`, tmpl, region).Scan(&amount)
		if err != nil && !errors.Is(err, pgx.ErrNoRows) {
			t.Fatalf("live price: %v", err)
		}
		if amount == nil {
			return 0
		}
		return *amount
	}
	grantAt := func(event, tmpl uuid.UUID, region string, expected int32) (store.EventPayment, error) {
		return q.GrantEventPayment(ctx, store.GrantEventPaymentParams{
			ID: uuid.Must(uuid.NewV7()), EventID: event, TemplateID: tmpl, Region: region, Note: "q", ExpectedAmountMinor: expected})
	}
	grant := func(event, tmpl uuid.UUID, region string) (store.EventPayment, error) {
		return grantAt(event, tmpl, region, livePrice(tmpl, region))
	}
	g := insertEvent(prem, "draft")
	for _, c := range []struct {
		region   string
		currency string
		amount   int32
	}{{"IN", "INR", 59900}, {"AE", "AED", 2900}, {"INT", "USD", 900}} {
		p, err := grant(insertEvent(prem, "draft"), prem, c.region)
		if err != nil || p.Region != c.region || p.Currency != c.currency || p.AmountMinor != c.amount || p.RevokedAt != nil {
			t.Errorf("grant %s = %+v (err %v), want %s %d", c.region, p, err, c.currency, c.amount)
		}
	}
	for _, c := range []struct {
		name   string
		tmpl   uuid.UUID
		region string
	}{{"free template", free, "IN"}, {"unknown region", prem, "US"}, {"empty region", prem, ""}, {"unknown template", uuid.Must(uuid.NewV7()), "IN"}} {
		if _, err := grant(g, c.tmpl, c.region); !errors.Is(err, pgx.ErrNoRows) {
			t.Errorf("grant %s: err = %v, want ErrNoRows", c.name, err)
		}
	}
	// A confirmed amount that is not the live price records nothing.
	for _, bad := range []int32{0, 1, livePrice(prem, "IN") + 1, livePrice(prem, "IN") - 1} {
		if _, err := grantAt(g, prem, "IN", bad); !errors.Is(err, pgx.ErrNoRows) {
			t.Errorf("grant with stale amount %d: err = %v, want ErrNoRows", bad, err)
		}
	}
	if _, err := grantAt(g, prem, "INT", livePrice(prem, "IN")); !errors.Is(err, pgx.ErrNoRows) {
		t.Errorf("grant INT with the IN amount: err = %v, want ErrNoRows", err)
	}
	// LockEventForPayment: live owned event only.
	if tid, err := q.LockEventForPayment(ctx, g); err != nil || tid != prem {
		t.Errorf("LockEventForPayment = %v (err %v), want %v", tid, err, prem)
	}
	if _, err := q.LockEventForPayment(ctx, uuid.Must(uuid.NewV7())); !errors.Is(err, pgx.ErrNoRows) {
		t.Errorf("LockEventForPayment unknown event: err = %v, want ErrNoRows", err)
	}
	// Region not offered (row removed) -> no grant; offered again via upsert -> grant at the new price.
	if _, err := q.DeleteTemplatePrice(ctx, store.DeleteTemplatePriceParams{TemplateID: prem, Region: "AE"}); err != nil {
		t.Fatalf("delete price: %v", err)
	}
	if _, err := grant(g, prem, "AE"); !errors.Is(err, pgx.ErrNoRows) {
		t.Errorf("grant in a region not offered: err = %v, want ErrNoRows", err)
	}
	if tp, err := q.UpsertTemplatePrice(ctx, store.UpsertTemplatePriceParams{TemplateID: prem, Region: "AE", AmountMinor: 3500}); err != nil || tp.Currency != "AED" || tp.AmountMinor != 3500 {
		t.Errorf("upsert price = %+v (err %v), want AED 3500", tp, err)
	}
	if tp, err := q.UpsertTemplatePrice(ctx, store.UpsertTemplatePriceParams{TemplateID: prem, Region: "AE", AmountMinor: 3600}); err != nil || tp.AmountMinor != 3600 {
		t.Errorf("upsert existing price = %+v (err %v), want 3600", tp, err)
	}
	if _, err := q.UpsertTemplatePrice(ctx, store.UpsertTemplatePriceParams{TemplateID: prem, Region: "US", AmountMinor: 3500}); err == nil {
		t.Error("upsert with unknown region succeeded")
	}
	if prices, err := q.ListTemplatePrices(ctx, []uuid.UUID{prem, free}); err != nil || len(prices) != 2 || prices[0].Region != "AE" || prices[1].Region != "INT" {
		t.Errorf("ListTemplatePrices = %+v (err %v), want AE then INT for the premium template", prices, err)
	}
	p, err := grant(g, prem, "AE")
	if err != nil || p.AmountMinor != 3600 {
		t.Fatalf("grant after upsert = %+v (err %v), want 3600", p, err)
	}
	// A second live grant is DO NOTHING (no row).
	if _, err := grant(g, prem, "INT"); !errors.Is(err, pgx.ErrNoRows) {
		t.Errorf("second live grant: err = %v, want ErrNoRows", err)
	}
	if paid, err := q.EventTemplatePaid(ctx, store.EventTemplatePaidParams{EventID: g, TemplateID: prem}); err != nil || !paid {
		t.Errorf("paid after grant = %v (err %v), want true", paid, err)
	}
	// Revoke is soft and happens once; the gate closes; the history keeps it; a re-grant works.
	r, err := q.RevokeEventPayment(ctx, store.RevokeEventPaymentParams{EventID: g, TemplateID: prem, RevokedBy: owner})
	if err != nil || r.RevokedAt == nil || r.RevokedBy == nil || *r.RevokedBy != owner {
		t.Fatalf("revoke = %+v (err %v), want revoked_at and revoked_by set", r, err)
	}
	if _, err := q.RevokeEventPayment(ctx, store.RevokeEventPaymentParams{EventID: g, TemplateID: prem, RevokedBy: owner}); !errors.Is(err, pgx.ErrNoRows) {
		t.Errorf("second revoke: err = %v, want ErrNoRows", err)
	}
	if paid, err := q.EventTemplatePaid(ctx, store.EventTemplatePaidParams{EventID: g, TemplateID: prem}); err != nil || paid {
		t.Errorf("paid after revoke = %v (err %v), want false", paid, err)
	}
	if l, err := q.ListEventPayments(ctx, g); err != nil || len(l) != 0 {
		t.Errorf("ListEventPayments after revoke = %d (err %v), want 0", len(l), err)
	}
	if l, err := q.ListEventPaymentHistory(ctx, g); err != nil || len(l) != 1 || l[0].RevokedAt == nil {
		t.Errorf("ListEventPaymentHistory after revoke = %+v (err %v), want the revoked row", l, err)
	}
	if _, err := grant(g, prem, "INT"); err != nil {
		t.Errorf("re-grant after revoke: %v", err)
	}
	if l, err := q.ListEventPayments(ctx, g); err != nil || len(l) != 1 || l[0].Region != "INT" {
		t.Errorf("ListEventPayments after re-grant = %+v (err %v), want one INT row", l, err)
	}

	// The PublishEvent gate ignores a revoked grant.
	pub := insertEvent(prem, "draft")
	publish := func() error {
		var version int32
		if err := pool.QueryRow(ctx, "SELECT version FROM events WHERE id = $1", pub).Scan(&version); err != nil {
			t.Fatal(err)
		}
		_, err := q.PublishEvent(ctx, store.PublishEventParams{EventID: pub, UserID: owner, Version: version})
		return err
	}
	if err := publish(); !errors.Is(err, pgx.ErrNoRows) {
		t.Errorf("publish premium without grant: err = %v, want ErrNoRows", err)
	}
	if _, err := grant(pub, prem, "IN"); err != nil {
		t.Fatal(err)
	}
	if _, err := q.RevokeEventPayment(ctx, store.RevokeEventPaymentParams{EventID: pub, TemplateID: prem, RevokedBy: owner}); err != nil {
		t.Fatal(err)
	}
	if err := publish(); !errors.Is(err, pgx.ErrNoRows) {
		t.Errorf("publish premium with revoked grant: err = %v, want ErrNoRows", err)
	}
	if _, err := grant(pub, prem, "IN"); err != nil {
		t.Fatal(err)
	}
	if err := publish(); err != nil {
		t.Errorf("publish premium with live grant: %v", err)
	}

	// RestoreEvent: a taken-down event comes back 'hidden' on an unpaid premium template, 'published' when
	// paid or on a free template, and 'draft' when it was never published.
	restore := func(event uuid.UUID) string {
		t.Helper()
		row, err := q.RestoreEvent(ctx, event)
		if err != nil {
			t.Fatalf("restore: %v", err)
		}
		if row.OldStatus != "taken_down" {
			t.Errorf("restore old status = %q, want taken_down", row.OldStatus)
		}
		return row.NewStatus
	}
	takeDown := func(event uuid.UUID) {
		t.Helper()
		if _, err := pool.Exec(ctx, "UPDATE events SET status = 'taken_down' WHERE id = $1", event); err != nil {
			t.Fatal(err)
		}
	}
	// Published on a free template, switched to premium (as an admin can), then taken down.
	unpaid := insertEvent(free, "published")
	if _, err := pool.Exec(ctx, `UPDATE events e SET template_id = $2, template_version = tv.version
		FROM template_versions tv WHERE e.id = $1 AND tv.template_id = $2`, unpaid, prem); err != nil {
		t.Fatalf("switch to premium: %v", err)
	}
	takeDown(unpaid)
	if got := restore(unpaid); got != "hidden" {
		t.Errorf("restore unpaid premium = %q, want hidden", got)
	}
	paid := insertEvent(prem, "published")
	if _, err := grant(paid, prem, "IN"); err != nil {
		t.Fatal(err)
	}
	takeDown(paid)
	if got := restore(paid); got != "published" {
		t.Errorf("restore paid premium = %q, want published", got)
	}
	revokedPaid := insertEvent(prem, "published")
	if _, err := grant(revokedPaid, prem, "IN"); err != nil {
		t.Fatal(err)
	}
	if _, err := q.RevokeEventPayment(ctx, store.RevokeEventPaymentParams{EventID: revokedPaid, TemplateID: prem, RevokedBy: owner}); err != nil {
		t.Fatal(err)
	}
	takeDown(revokedPaid)
	if got := restore(revokedPaid); got != "hidden" {
		t.Errorf("restore premium with revoked payment = %q, want hidden", got)
	}
	freeEvent := insertEvent(free, "published")
	takeDown(freeEvent)
	if got := restore(freeEvent); got != "published" {
		t.Errorf("restore free template = %q, want published", got)
	}
	neverPublished := insertEvent(prem, "draft")
	takeDown(neverPublished)
	if got := restore(neverPublished); got != "draft" {
		t.Errorf("restore never-published = %q, want draft", got)
	}

	// Indexes after Up.
	if err := pool.QueryRow(ctx, `SELECT count(*) FROM pg_indexes WHERE tablename = 'event_payments'
		AND indexname IN ('event_payments_live_key', 'event_payments_event_template_idx', 'event_payments_template_id_idx',
		                  'event_payments_granted_by_idx', 'event_payments_revoked_by_idx')`).Scan(&n); err != nil || n != 5 {
		t.Errorf("event_payments indexes = %d (err %v), want 5", n, err)
	}
	if err := pool.QueryRow(ctx, `SELECT count(*) FROM pg_indexes WHERE indexname = 'event_payments_event_template_key'`).Scan(&n); err != nil || n != 0 {
		t.Errorf("old unique index present after Up = %d (err %v), want 0", n, err)
	}

	// Down refuses while a non-IN payment exists, and the failed Down leaves the schema at 37.
	if _, err := provider.DownTo(ctx, 36); err == nil {
		t.Fatal("down to 36 with non-IN payments succeeded, want an error")
	}
	if err := pool.QueryRow(ctx, "SELECT count(*) FROM information_schema.tables WHERE table_name = 'template_prices'").Scan(&n); err != nil || n != 1 {
		t.Errorf("template_prices after refused Down = %d (err %v), want 1 (rolled back)", n, err)
	}
	// Revoked rows also refuse Down (a rollback would destroy them), even once the non-IN rows are gone.
	if _, err := pool.Exec(ctx, "DELETE FROM event_payments WHERE region <> 'IN'"); err != nil {
		t.Fatalf("delete non-IN payments: %v", err)
	}
	var live, revoked int
	if err := pool.QueryRow(ctx, "SELECT count(*) FILTER (WHERE revoked_at IS NULL), count(*) FILTER (WHERE revoked_at IS NOT NULL) FROM event_payments").
		Scan(&live, &revoked); err != nil || revoked == 0 {
		t.Fatalf("payments before Down: live %d revoked %d (err %v), want some revoked", live, revoked, err)
	}
	if _, err := provider.DownTo(ctx, 36); err == nil {
		t.Fatal("down to 36 with revoked payments succeeded, want an error")
	}
	if err := pool.QueryRow(ctx, "SELECT count(*) FROM event_payments WHERE revoked_at IS NOT NULL").Scan(&n); err != nil || n != revoked {
		t.Fatalf("revoked rows after refused Down = %d (err %v), want %d kept", n, err, revoked)
	}
	// With only live India rows left, Down is clean.
	if _, err := pool.Exec(ctx, "DELETE FROM event_payments WHERE revoked_at IS NOT NULL"); err != nil {
		t.Fatalf("delete revoked payments: %v", err)
	}
	if err := pool.QueryRow(ctx, "SELECT count(*) FROM event_payments").Scan(&live); err != nil {
		t.Fatal(err)
	}
	if _, err := provider.DownTo(ctx, 36); err != nil {
		t.Fatalf("down to 36: %v", err)
	}
	// Back to the 00036 shape: amount_paise, INR-only, no region/revoke columns, old index.
	if err := pool.QueryRow(ctx, `SELECT
		(SELECT count(*) FROM information_schema.tables WHERE table_name = 'template_prices') +
		(SELECT count(*) FROM information_schema.columns WHERE table_name = 'event_payments'
		   AND column_name IN ('amount_minor', 'region', 'revoked_at', 'revoked_by')) +
		(SELECT count(*) FROM pg_indexes WHERE indexname IN ('event_payments_live_key', 'event_payments_event_template_idx', 'event_payments_revoked_by_idx'))`).
		Scan(&n); err != nil || n != 0 {
		t.Errorf("37 leftovers after Down = %d (err %v), want 0", n, err)
	}
	if err := pool.QueryRow(ctx, `SELECT count(*) FROM information_schema.columns WHERE table_name = 'event_payments' AND column_name = 'amount_paise'`).Scan(&n); err != nil || n != 1 {
		t.Errorf("amount_paise after Down = %d (err %v), want 1", n, err)
	}
	if err := pool.QueryRow(ctx, "SELECT count(*) FROM event_payments").Scan(&n); err != nil || n != live {
		t.Errorf("payments after Down = %d (err %v), want %d (all kept)", n, err, live)
	}
	_, err = pool.Exec(ctx, `INSERT INTO event_payments (id, event_id, template_id, amount_paise, currency) VALUES ($1, $2, $3, 100, 'AED')`,
		uuid.Must(uuid.NewV7()), probe, prem)
	wantCode("AED after Down", err, "23514")
	_, err = pool.Exec(ctx, `INSERT INTO event_payments (id, event_id, template_id, amount_paise) VALUES ($1, $2, $3, 99)`,
		uuid.Must(uuid.NewV7()), probe, prem)
	wantCode("amount 99 after Down", err, "23514")
	if err := pool.QueryRow(ctx, `SELECT count(*) FROM pg_indexes WHERE indexname = 'event_payments_event_template_key'`).Scan(&n); err != nil || n != 1 {
		t.Errorf("old unique index after Down = %d (err %v), want 1", n, err)
	}
	if _, err := provider.UpTo(ctx, 37); err != nil {
		t.Fatalf("up again: %v", err)
	}
	if err := pool.QueryRow(ctx, "SELECT count(*) FROM template_prices WHERE template_id = $1", prem).Scan(&n); err != nil || n != 2 {
		t.Errorf("premium prices after Up again = %d (err %v), want 2", n, err)
	}
}

// TestMigration00038_IllustratedTemplates applies migrations up to 38 and checks the four illustrated
// draft templates (Clothesline, Welcome Home, Lovebirds, Fizz): id, name, status, sort_order, free and
// tags, one published v1 each whose stored manifest validates and equals its testdata file, and that
// they stay out of the host picker. It then runs 00038's Down (all eight rows go, the immutable trigger
// is re-enabled) and applies Up again.
func TestMigration00038_IllustratedTemplates(t *testing.T) {
	ctx, pool, provider := scratchMigrator(t)
	if _, err := provider.UpTo(ctx, 37); err != nil {
		t.Fatalf("migrate to 37: %v", err)
	}
	var before int
	if err := pool.QueryRow(ctx, "SELECT count(*) FROM templates").Scan(&before); err != nil {
		t.Fatalf("count templates: %v", err)
	}
	if _, err := provider.UpTo(ctx, 38); err != nil {
		t.Fatalf("migrate to 38: %v", err)
	}

	cases := []struct {
		slug, id, name, tags string
		sortOrder            int32
	}{
		{"clothesline", "01926a00-0000-7000-8000-000000000017", "Clothesline", `{"occasions": ["baby-shower"]}`, 65},
		{"welcome-home", "01926a00-0000-7000-8000-000000000018", "Welcome Home", `{"occasions": ["housewarming"]}`, 85},
		{"lovebirds", "01926a00-0000-7000-8000-000000000019", "Lovebirds", `{"occasions": ["engagement"]}`, 25},
		{"fizz", "01926a00-0000-7000-8000-00000000001a", "Fizz", `{"occasions": ["party", "birthday"]}`, 105},
	}
	checkSeeded := func() {
		t.Helper()
		for _, c := range cases {
			var (
				id, name, status, tags string
				sortOrder              int32
				premium                bool
			)
			err := pool.QueryRow(ctx, `SELECT id::text, name, status, tags::text, sort_order, is_premium
				FROM templates WHERE slug = $1`, c.slug).Scan(&id, &name, &status, &tags, &sortOrder, &premium)
			if err != nil {
				t.Errorf("%s: read template: %v", c.slug, err)
				continue
			}
			if id != c.id || name != c.name || status != "draft" || sortOrder != c.sortOrder || premium || tags != c.tags {
				t.Errorf("%s: id=%s name=%q status=%s sort_order=%d premium=%v tags=%s, want id=%s name=%q draft sort_order=%d not premium tags=%s",
					c.slug, id, name, status, sortOrder, premium, tags, c.id, c.name, c.sortOrder, c.tags)
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
				t.Errorf("%s: read version: %v", c.slug, err)
				continue
			}
			if version != 1 || !published || assets != "" || size > 32768 {
				t.Errorf("%s: version=%d published=%v assets_path=%q manifest size=%d, want v1 published, no assets, <= 32768", c.slug, version, published, assets, size)
			}
			m, err := content.ValidateManifest(manifest)
			if err != nil {
				t.Errorf("%s: ValidateManifest: %v", c.slug, err)
				continue
			}
			if m.Schema != 2 || len(m.Palettes) == 0 || len(m.Fonts) == 0 {
				t.Errorf("%s: manifest schema=%d palettes=%d fonts=%d, want schema 2 with palettes and fonts", c.slug, m.Schema, len(m.Palettes), len(m.Fonts))
			}
			for _, p := range m.Palettes {
				for _, f := range m.Fonts {
					overrides := fmt.Sprintf(`{"palette":%q,"font":%q}`, p.ID, f.ID)
					th := content.ResolveTheme(m, []byte(overrides), "")
					if th.Engine != 2 || th.Palette != p.Colors || th.Fonts.Heading != f.Heading || th.Fonts.Body != f.Body {
						t.Errorf("%s: ResolveTheme(%s, %s) = engine %d %+v, want engine 2 with that palette and font pair", c.slug, p.ID, f.ID, th.Engine, th)
					}
				}
			}

			want, err := os.ReadFile("../content/testdata/themes_v2/" + c.slug + ".json")
			if err != nil {
				t.Errorf("%s: read testdata: %v", c.slug, err)
				continue
			}
			var gotJSON, wantJSON any
			if err := json.Unmarshal(manifest, &gotJSON); err != nil {
				t.Errorf("%s: decode stored manifest: %v", c.slug, err)
			} else if err := json.Unmarshal(want, &wantJSON); err != nil {
				t.Errorf("%s: decode testdata: %v", c.slug, err)
			} else if !reflect.DeepEqual(gotJSON, wantJSON) {
				t.Errorf("%s: stored manifest differs from testdata/themes_v2/%s.json", c.slug, c.slug)
			}
		}

		var templates int
		if err := pool.QueryRow(ctx, "SELECT count(*) FROM templates").Scan(&templates); err != nil || templates != before+len(cases) {
			t.Errorf("templates = %d (err %v), want %d", templates, err, before+len(cases))
		}
		// Drafts stay out of the public catalog.
		for _, slug := range pickerSlugs(ctx, t, pool) {
			for _, c := range cases {
				if slug == c.slug {
					t.Errorf("draft template %s is listed in the public picker", slug)
				}
			}
		}
	}
	checkSeeded()

	if _, err := provider.DownTo(ctx, 37); err != nil {
		t.Fatalf("down to 37: %v", err)
	}
	for _, c := range cases {
		var left int
		if err := pool.QueryRow(ctx, "SELECT count(*) FROM templates WHERE id = $1 OR slug = $2", c.id, c.slug).Scan(&left); err != nil || left != 0 {
			t.Errorf("%s templates after Down = %d (err %v), want 0", c.slug, left, err)
		}
		if err := pool.QueryRow(ctx, "SELECT count(*) FROM template_versions WHERE template_id = $1", c.id).Scan(&left); err != nil || left != 0 {
			t.Errorf("%s versions after Down = %d (err %v), want 0", c.slug, left, err)
		}
	}
	var left int
	if err := pool.QueryRow(ctx, "SELECT count(*) FROM templates").Scan(&left); err != nil || left != before {
		t.Errorf("templates after Down = %d (err %v), want %d", left, err, before)
	}
	var triggerEnabled string
	if err := pool.QueryRow(ctx, "SELECT tgenabled::text FROM pg_trigger WHERE tgname = 'template_versions_immutable'").Scan(&triggerEnabled); err != nil || triggerEnabled != "O" {
		t.Errorf("immutable trigger state after Down = %q (err %v), want enabled (O)", triggerEnabled, err)
	}

	if _, err := provider.UpTo(ctx, 38); err != nil {
		t.Fatalf("up again: %v", err)
	}
	checkSeeded()
}

// TestMigration00039_Glitterball applies migrations up to 39 and checks the Glitterball
// draft template: id, name, status, sort_order, free and
// tags, one published v1 each whose stored manifest validates and equals its testdata file, and that
// they stay out of the host picker. It then runs 00039's Down (both rows go, the immutable trigger
// is re-enabled) and applies Up again.
func TestMigration00039_Glitterball(t *testing.T) {
	ctx, pool, provider := scratchMigrator(t)
	if _, err := provider.UpTo(ctx, 38); err != nil {
		t.Fatalf("migrate to 38: %v", err)
	}
	var before int
	if err := pool.QueryRow(ctx, "SELECT count(*) FROM templates").Scan(&before); err != nil {
		t.Fatalf("count templates: %v", err)
	}
	if _, err := provider.UpTo(ctx, 39); err != nil {
		t.Fatalf("migrate to 39: %v", err)
	}

	cases := []struct {
		slug, id, name, tags string
		sortOrder            int32
	}{
		{"glitterball", "01926a00-0000-7000-8000-00000000001b", "Glitterball", `{"occasions": ["birthday", "party"]}`, 107},
	}
	checkSeeded := func() {
		t.Helper()
		for _, c := range cases {
			var (
				id, name, status, tags string
				sortOrder              int32
				premium                bool
			)
			err := pool.QueryRow(ctx, `SELECT id::text, name, status, tags::text, sort_order, is_premium
				FROM templates WHERE slug = $1`, c.slug).Scan(&id, &name, &status, &tags, &sortOrder, &premium)
			if err != nil {
				t.Errorf("%s: read template: %v", c.slug, err)
				continue
			}
			if id != c.id || name != c.name || status != "draft" || sortOrder != c.sortOrder || premium || tags != c.tags {
				t.Errorf("%s: id=%s name=%q status=%s sort_order=%d premium=%v tags=%s, want id=%s name=%q draft sort_order=%d not premium tags=%s",
					c.slug, id, name, status, sortOrder, premium, tags, c.id, c.name, c.sortOrder, c.tags)
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
				t.Errorf("%s: read version: %v", c.slug, err)
				continue
			}
			if version != 1 || !published || assets != "" || size > 32768 {
				t.Errorf("%s: version=%d published=%v assets_path=%q manifest size=%d, want v1 published, no assets, <= 32768", c.slug, version, published, assets, size)
			}
			m, err := content.ValidateManifest(manifest)
			if err != nil {
				t.Errorf("%s: ValidateManifest: %v", c.slug, err)
				continue
			}
			if m.Schema != 2 || len(m.Palettes) == 0 || len(m.Fonts) == 0 {
				t.Errorf("%s: manifest schema=%d palettes=%d fonts=%d, want schema 2 with palettes and fonts", c.slug, m.Schema, len(m.Palettes), len(m.Fonts))
			}
			for _, p := range m.Palettes {
				for _, f := range m.Fonts {
					overrides := fmt.Sprintf(`{"palette":%q,"font":%q}`, p.ID, f.ID)
					th := content.ResolveTheme(m, []byte(overrides), "")
					if th.Engine != 2 || th.Palette != p.Colors || th.Fonts.Heading != f.Heading || th.Fonts.Body != f.Body {
						t.Errorf("%s: ResolveTheme(%s, %s) = engine %d %+v, want engine 2 with that palette and font pair", c.slug, p.ID, f.ID, th.Engine, th)
					}
				}
			}

			want, err := os.ReadFile("../content/testdata/themes_v2/" + c.slug + ".json")
			if err != nil {
				t.Errorf("%s: read testdata: %v", c.slug, err)
				continue
			}
			var gotJSON, wantJSON any
			if err := json.Unmarshal(manifest, &gotJSON); err != nil {
				t.Errorf("%s: decode stored manifest: %v", c.slug, err)
			} else if err := json.Unmarshal(want, &wantJSON); err != nil {
				t.Errorf("%s: decode testdata: %v", c.slug, err)
			} else if !reflect.DeepEqual(gotJSON, wantJSON) {
				t.Errorf("%s: stored manifest differs from testdata/themes_v2/%s.json", c.slug, c.slug)
			}
		}

		var templates int
		if err := pool.QueryRow(ctx, "SELECT count(*) FROM templates").Scan(&templates); err != nil || templates != before+len(cases) {
			t.Errorf("templates = %d (err %v), want %d", templates, err, before+len(cases))
		}
		// Drafts stay out of the public catalog.
		for _, slug := range pickerSlugs(ctx, t, pool) {
			for _, c := range cases {
				if slug == c.slug {
					t.Errorf("draft template %s is listed in the public picker", slug)
				}
			}
		}
	}
	checkSeeded()

	if _, err := provider.DownTo(ctx, 38); err != nil {
		t.Fatalf("down to 38: %v", err)
	}
	for _, c := range cases {
		var left int
		if err := pool.QueryRow(ctx, "SELECT count(*) FROM templates WHERE id = $1 OR slug = $2", c.id, c.slug).Scan(&left); err != nil || left != 0 {
			t.Errorf("%s templates after Down = %d (err %v), want 0", c.slug, left, err)
		}
		if err := pool.QueryRow(ctx, "SELECT count(*) FROM template_versions WHERE template_id = $1", c.id).Scan(&left); err != nil || left != 0 {
			t.Errorf("%s versions after Down = %d (err %v), want 0", c.slug, left, err)
		}
	}
	var left int
	if err := pool.QueryRow(ctx, "SELECT count(*) FROM templates").Scan(&left); err != nil || left != before {
		t.Errorf("templates after Down = %d (err %v), want %d", left, err, before)
	}
	var triggerEnabled string
	if err := pool.QueryRow(ctx, "SELECT tgenabled::text FROM pg_trigger WHERE tgname = 'template_versions_immutable'").Scan(&triggerEnabled); err != nil || triggerEnabled != "O" {
		t.Errorf("immutable trigger state after Down = %q (err %v), want enabled (O)", triggerEnabled, err)
	}

	if _, err := provider.UpTo(ctx, 39); err != nil {
		t.Fatalf("up again: %v", err)
	}
	checkSeeded()
}
