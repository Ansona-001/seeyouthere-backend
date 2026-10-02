package media

import (
	"context"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/google/uuid"
)

func touchOld(t *testing.T, path string, age time.Duration) {
	t.Helper()
	old := time.Now().Add(-age)
	if err := os.Chtimes(path, old, old); err != nil {
		t.Fatalf("chtimes: %v", err)
	}
}

func TestReconcile_RemovesOrphanEventDir(t *testing.T) {
	s := newTestStore(t)
	liveEvent, orphanEvent := uuid.Must(uuid.NewV7()), uuid.Must(uuid.NewV7())
	mediaID := uuid.Must(uuid.NewV7())

	for _, ev := range []uuid.UUID{liveEvent, orphanEvent} {
		outDir := t.TempDir()
		writeRendition(t, outDir, 480)
		if err := s.Commit(outDir, AreaPublic, ev, mediaID); err != nil {
			t.Fatalf("Commit: %v", err)
		}
	}
	orphanDir := filepath.Join(s.root, "public", orphanEvent.String())
	touchOld(t, orphanDir, 2*time.Hour)

	eventExists := func(_ context.Context, ids []uuid.UUID) (map[uuid.UUID]bool, error) {
		out := map[uuid.UUID]bool{}
		for _, id := range ids {
			out[id] = id == liveEvent
		}
		return out, nil
	}
	eventMedia := func(_ context.Context, _ uuid.UUID) (map[uuid.UUID]bool, error) {
		return map[uuid.UUID]bool{mediaID: true}, nil
	}

	if err := s.Reconcile(context.Background(), eventExists, eventMedia); err != nil {
		t.Fatalf("Reconcile: %v", err)
	}

	if _, err := os.Stat(orphanDir); !os.IsNotExist(err) {
		t.Error("orphan event directory should have been removed")
	}
	if err := openAndClose(s, liveEvent, mediaID, 480); err != nil {
		t.Errorf("live event's media should survive reconcile: %v", err)
	}
}

func TestReconcile_KeepsRecentOrphan(t *testing.T) {
	s := newTestStore(t)
	orphanEvent := uuid.Must(uuid.NewV7())
	mediaID := uuid.Must(uuid.NewV7())
	outDir := t.TempDir()
	writeRendition(t, outDir, 480)
	if err := s.Commit(outDir, AreaPublic, orphanEvent, mediaID); err != nil {
		t.Fatalf("Commit: %v", err)
	}
	// Not touched: mtime is "now", well within orphanAge.

	eventExists := func(_ context.Context, ids []uuid.UUID) (map[uuid.UUID]bool, error) {
		return map[uuid.UUID]bool{}, nil // nothing exists
	}
	eventMedia := func(_ context.Context, _ uuid.UUID) (map[uuid.UUID]bool, error) {
		return map[uuid.UUID]bool{}, nil
	}

	if err := s.Reconcile(context.Background(), eventExists, eventMedia); err != nil {
		t.Fatalf("Reconcile: %v", err)
	}
	if err := openAndClose(s, orphanEvent, mediaID, 480); err != nil {
		t.Error("a recent orphan (crash between commit and insert) must survive one reconcile pass")
	}
}

func TestReconcile_RemovesOrphanMediaWithinLiveEvent(t *testing.T) {
	s := newTestStore(t)
	eventID := uuid.Must(uuid.NewV7())
	knownMedia, orphanMedia := uuid.Must(uuid.NewV7()), uuid.Must(uuid.NewV7())

	for _, m := range []uuid.UUID{knownMedia, orphanMedia} {
		outDir := t.TempDir()
		writeRendition(t, outDir, 480)
		if err := s.Commit(outDir, AreaPublic, eventID, m); err != nil {
			t.Fatalf("Commit: %v", err)
		}
	}
	orphanDir := filepath.Join(s.root, "public", eventID.String(), orphanMedia.String())
	touchOld(t, orphanDir, 2*time.Hour)

	eventExists := func(_ context.Context, ids []uuid.UUID) (map[uuid.UUID]bool, error) {
		out := map[uuid.UUID]bool{}
		for _, id := range ids {
			out[id] = id == eventID
		}
		return out, nil
	}
	eventMedia := func(_ context.Context, _ uuid.UUID) (map[uuid.UUID]bool, error) {
		return map[uuid.UUID]bool{knownMedia: true}, nil
	}

	if err := s.Reconcile(context.Background(), eventExists, eventMedia); err != nil {
		t.Fatalf("Reconcile: %v", err)
	}
	if _, err := os.Stat(orphanDir); !os.IsNotExist(err) {
		t.Error("orphan media directory should have been removed")
	}
	if err := openAndClose(s, eventID, knownMedia, 480); err != nil {
		t.Errorf("known media should survive: %v", err)
	}
}

func TestReconcile_RemovesStaleTmpEntries(t *testing.T) {
	s := newTestStore(t)
	stale := filepath.Join(s.tmpDir(), "stale-upload")
	if err := os.WriteFile(stale, []byte("x"), 0o644); err != nil {
		t.Fatalf("write: %v", err)
	}
	touchOld(t, stale, 2*time.Hour)

	fresh := filepath.Join(s.tmpDir(), "fresh-upload")
	if err := os.WriteFile(fresh, []byte("x"), 0o644); err != nil {
		t.Fatalf("write: %v", err)
	}

	noop := func(_ context.Context, ids []uuid.UUID) (map[uuid.UUID]bool, error) { return nil, nil }
	noopMedia := func(_ context.Context, _ uuid.UUID) (map[uuid.UUID]bool, error) { return nil, nil }
	if err := s.Reconcile(context.Background(), noop, noopMedia); err != nil {
		t.Fatalf("Reconcile: %v", err)
	}

	if _, err := os.Stat(stale); !os.IsNotExist(err) {
		t.Error("stale tmp entry should have been removed")
	}
	if _, err := os.Stat(fresh); err != nil {
		t.Error("fresh tmp entry should survive")
	}
}
