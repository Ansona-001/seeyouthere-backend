package database

import (
	"context"
	"fmt"
	"io/fs"
	"os"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/jackc/pgx/v5/stdlib"
	"github.com/pressly/goose/v3"
)

// TestMigration00005_Backfill applies migrations 1-4 to a throwaway database,
// inserts legacy dated events (including one whose datetime block holds an
// impossible date that passes the shape regex), then applies 00005 and checks
// the backfill: ends_at from the block, retention_from = greatest(ends_at,
// now()) so legacy events get a full retention window, and no abort on the
// bad row. It also runs 00005's Down and Up again.
func TestMigration00005_Backfill(t *testing.T) {
	url := os.Getenv("DATABASE_URL")
	if url == "" {
		t.Skip("DATABASE_URL not set")
	}
	ctx := context.Background()

	admin, err := pgxpool.New(ctx, url)
	if err != nil {
		t.Fatalf("open admin pool: %v", err)
	}
	defer admin.Close()

	name := "syt_migtest_" + uuid.Must(uuid.NewV7()).String()[24:]
	if _, err := admin.Exec(ctx, "CREATE DATABASE "+name); err != nil {
		t.Skipf("cannot create a scratch database: %v", err)
	}
	defer func() {
		if _, err := admin.Exec(context.Background(), "DROP DATABASE "+name+" WITH (FORCE)"); err != nil {
			t.Logf("drop scratch database %s: %v", name, err)
		}
	}()

	poolCfg, err := pgxpool.ParseConfig(url)
	if err != nil {
		t.Fatalf("parse DATABASE_URL: %v", err)
	}
	poolCfg.ConnConfig.Database = name
	pool, err := pgxpool.NewWithConfig(ctx, poolCfg)
	if err != nil {
		t.Fatalf("open scratch pool: %v", err)
	}
	defer pool.Close()

	sub, err := fs.Sub(migrations, "migrations")
	if err != nil {
		t.Fatal(err)
	}
	db := stdlib.OpenDBFromPool(pool)
	defer db.Close()
	provider, err := goose.NewProvider(goose.DialectPostgres, db, sub)
	if err != nil {
		t.Fatalf("goose provider: %v", err)
	}
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
