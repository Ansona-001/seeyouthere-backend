package jobs

import (
	"context"
	"errors"
	"log/slog"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/riverqueue/river"

	"github.com/ansonarose/seeyouthere-backend/internal/media"
	"github.com/ansonarose/seeyouthere-backend/internal/store"
)

// insertMedia adds a media row to the event and returns its id.
func (f retentionFixture) insertMedia(t *testing.T, eventID uuid.UUID, status string) uuid.UUID {
	t.Helper()
	id := uuid.Must(uuid.NewV7())
	f.exec(t, `INSERT INTO media (id, event_id, storage_key, content_type, size_bytes, width, height, uploaded_by, moderation_status)
		VALUES ($1, $2, $3, 'image/jpeg', 100, 10, 10, 'host', $4)`, id, eventID, "e/"+eventID.String()+"/"+id.String(), status)
	return id
}

func TestMediaVisibilityWorker(t *testing.T) {
	tests := []struct {
		name       string
		setup      string // SQL run against the event ($1), empty for none
		wantDelete bool   // whether the rejected media's files are deleted
	}{
		{"draft event deletes rejected media", "", true},
		{"published event deletes rejected media", "UPDATE events SET status = 'published', published_at = now(), slug = 'jobs-' || id::text WHERE id = $1", true},
		{"taken-down event does nothing", "UPDATE events SET status = 'taken_down', slug = 'jobs-' || id::text WHERE id = $1", false},
		{"soft-deleted event does nothing", "UPDATE events SET deleted_at = now() WHERE id = $1", false},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			f := newRetentionFixture(t)
			owner, _ := f.newUser(t)
			event := f.newEvent(t, retentionEvent{owner: &owner, endsAt: time.Now().Add(30 * day)})
			rejected := f.insertMedia(t, event, "rejected")
			f.insertMedia(t, event, "approved")
			f.insertMedia(t, event, "pending")
			if tt.setup != "" {
				f.exec(t, tt.setup, event)
			}

			w := &MediaVisibilityWorker{Queries: f.q, Media: f.deleter}
			if err := w.Work(context.Background(), &river.Job[MediaVisibilityArgs]{Args: MediaVisibilityArgs{EventID: event}}); err != nil {
				t.Fatalf("Work: %v", err)
			}
			calls := f.deleter.mediaCalls
			if !tt.wantDelete {
				if len(calls) != 0 {
					t.Errorf("DeleteMedia calls = %v, want none", calls)
				}
				return
			}
			if len(calls) != 1 || calls[0].eventID != event || len(calls[0].ids) != 1 || calls[0].ids[0] != rejected {
				t.Errorf("DeleteMedia calls = %v, want one call for only the rejected media %s", calls, rejected)
			}
		})
	}
}

func TestMediaVisibilityWorker_EdgeCases(t *testing.T) {
	f := newRetentionFixture(t)
	w := &MediaVisibilityWorker{Queries: f.q, Media: f.deleter}
	owner, _ := f.newUser(t)
	event := f.newEvent(t, retentionEvent{owner: &owner, endsAt: time.Now().Add(30 * day)})
	f.insertMedia(t, event, "approved")

	// A missing event and an event without rejected media are both a quiet no-op.
	for name, id := range map[string]uuid.UUID{"missing event": uuid.Must(uuid.NewV7()), "no rejected media": event} {
		if err := w.Work(context.Background(), &river.Job[MediaVisibilityArgs]{Args: MediaVisibilityArgs{EventID: id}}); err != nil {
			t.Errorf("%s: err = %v, want nil", name, err)
		}
	}
	if len(f.deleter.mediaCalls) != 0 {
		t.Errorf("DeleteMedia calls = %v, want none", f.deleter.mediaCalls)
	}

	// A failing delete is returned so River retries the job.
	f.insertMedia(t, event, "rejected")
	f.deleter.failFor = map[uuid.UUID]bool{event: true}
	if err := w.Work(context.Background(), &river.Job[MediaVisibilityArgs]{Args: MediaVisibilityArgs{EventID: event}}); err == nil {
		t.Error("err = nil, want the delete failure")
	}
}

func TestDeleteDetachedFiles(t *testing.T) {
	e1, e2 := uuid.Must(uuid.NewV7()), uuid.Must(uuid.NewV7())
	m := []uuid.UUID{uuid.Must(uuid.NewV7()), uuid.Must(uuid.NewV7()), uuid.Must(uuid.NewV7())}
	rows := []store.DeleteDetachedMediaRow{
		{EventID: e1, ID: m[0]}, {EventID: e2, ID: m[1]}, {EventID: e1, ID: m[2]},
	}

	t.Run("one call per event", func(t *testing.T) {
		d := &fakeDeleter{}
		deleteDetachedFiles(context.Background(), d, rows)
		if len(d.mediaCalls) != 2 {
			t.Fatalf("DeleteMedia calls = %d, want 2", len(d.mediaCalls))
		}
		got := map[uuid.UUID][]uuid.UUID{}
		for _, c := range d.mediaCalls {
			got[c.eventID] = c.ids
		}
		if len(got[e1]) != 2 || got[e1][0] != m[0] || got[e1][1] != m[2] || len(got[e2]) != 1 || got[e2][0] != m[1] {
			t.Errorf("grouped ids = %v", got)
		}
	})

	t.Run("failure for one event does not stop the rest", func(t *testing.T) {
		d := &fakeDeleter{failFor: map[uuid.UUID]bool{e1: true}}
		deleteDetachedFiles(context.Background(), d, rows)
		if len(d.mediaCalls) != 2 {
			t.Errorf("DeleteMedia calls = %d, want 2 despite the failure", len(d.mediaCalls))
		}
	})

	t.Run("no rows", func(t *testing.T) {
		d := &fakeDeleter{}
		deleteDetachedFiles(context.Background(), d, nil)
		if len(d.mediaCalls) != 0 {
			t.Errorf("DeleteMedia calls = %v, want none", d.mediaCalls)
		}
	})
}

// fakeReconciler asks the worker's live function about liveIDs, records the
// answer, and returns the canned stats.
type fakeReconciler struct {
	stats    media.ReconcileStats
	err      error
	liveIDs  []uuid.UUID
	liveSeen map[uuid.UUID]bool
}

func (r *fakeReconciler) Reconcile(ctx context.Context, live media.LiveMediaFunc) (media.ReconcileStats, error) {
	if r.err != nil {
		return media.ReconcileStats{}, r.err
	}
	var err error
	r.liveSeen, err = live(ctx, r.liveIDs)
	return r.stats, err
}

// logCapture collects slog records so tests can assert on level and message.
type logCapture struct {
	mu      sync.Mutex
	records []slog.Record
}

func (c *logCapture) Enabled(context.Context, slog.Level) bool { return true }
func (c *logCapture) Handle(_ context.Context, r slog.Record) error {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.records = append(c.records, r)
	return nil
}
func (c *logCapture) WithAttrs([]slog.Attr) slog.Handler { return c }
func (c *logCapture) WithGroup(string) slog.Handler      { return c }

func (c *logCapture) has(level slog.Level, msg string) bool {
	c.mu.Lock()
	defer c.mu.Unlock()
	for _, r := range c.records {
		if r.Level == level && strings.Contains(r.Message, msg) {
			return true
		}
	}
	return false
}

func TestMediaReconcileWorker(t *testing.T) {
	f := newRetentionFixture(t)
	owner, _ := f.newUser(t)
	event := f.newEvent(t, retentionEvent{owner: &owner, endsAt: time.Now().Add(30 * day)})
	liveMedia := f.insertMedia(t, event, "approved")
	rejected := f.insertMedia(t, event, "rejected")

	tests := []struct {
		name      string
		extra     int64 // bytes the bucket holds beyond the database total
		ceilingAt int64 // ceiling minus the bucket bytes
		err       error
		wantErr   bool
		wantError bool // ceiling error logged
		wantWarn  bool // drift warning logged
	}{
		{name: "healthy", ceilingAt: 1_000_000},
		{name: "at ceiling", extra: 1000, ceilingAt: 0, wantError: true, wantWarn: true},
		{name: "within 5 percent", extra: 0, ceilingAt: 1_000_000},
		{name: "reconcile fails", err: errors.New("boom"), wantErr: true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			var dbTotal int64
			if err := f.pool.QueryRow(context.Background(),
				"SELECT coalesce(sum(size_bytes), 0)::bigint FROM media WHERE moderation_status <> 'rejected'").Scan(&dbTotal); err != nil {
				t.Fatal(err)
			}
			// A dev database may hold other rows, so a 1000-byte excess is
			// only "over 5%" when the total is small; scale it.
			extra := tt.extra
			if extra > 0 {
				extra = dbTotal/20 + 1000
			}
			stats := media.ReconcileStats{Bytes: dbTotal + extra}

			logs := &logCapture{}
			old := slog.Default()
			slog.SetDefault(slog.New(logs))
			t.Cleanup(func() { slog.SetDefault(old) })

			rec := &fakeReconciler{stats: stats, err: tt.err, liveIDs: []uuid.UUID{liveMedia, rejected, uuid.Must(uuid.NewV7())}}
			w := &MediaReconcileWorker{Queries: f.q, Media: rec, TotalQuotaBytes: stats.Bytes + tt.ceilingAt}
			err := w.Work(context.Background(), &river.Job[MediaReconcileArgs]{})
			if (err != nil) != tt.wantErr {
				t.Fatalf("err = %v, wantErr %v", err, tt.wantErr)
			}
			if tt.wantErr {
				return
			}
			if len(rec.liveSeen) != 1 || !rec.liveSeen[liveMedia] {
				t.Errorf("live set = %v, want only the approved media", rec.liveSeen)
			}
			if got := logs.has(slog.LevelError, "media storage at ceiling"); got != tt.wantError {
				t.Errorf("ceiling error logged = %v, want %v", got, tt.wantError)
			}
			if got := logs.has(slog.LevelWarn, "exceeds database total"); got != tt.wantWarn {
				t.Errorf("drift warning logged = %v, want %v", got, tt.wantWarn)
			}
		})
	}
}
