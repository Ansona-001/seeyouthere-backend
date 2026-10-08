package media

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
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

func liveSet(ids ...uuid.UUID) LiveMediaFunc {
	return func(_ context.Context, query []uuid.UUID) (map[uuid.UUID]bool, error) {
		out := map[uuid.UUID]bool{}
		for _, q := range query {
			for _, id := range ids {
				if q == id {
					out[q] = true
				}
			}
		}
		return out, nil
	}
}

func TestReconcile_Objects(t *testing.T) {
	s, b := newTestStoreBlobs(t)
	event := uuid.Must(uuid.NewV7())
	live, rejected, oldOrphan, youngOrphan := uuid.Must(uuid.NewV7()), uuid.Must(uuid.NewV7()), uuid.Must(uuid.NewV7()), uuid.Must(uuid.NewV7())
	const badKey = "e/not-a-uuid/whatever.jpg"
	tmplKey := templateKey(uuid.Must(uuid.NewV7()), 1, 480)

	for _, w := range Widths {
		b.seed(mediaKey(event, live, w), 10, 5*time.Hour)
		b.seed(mediaKey(event, rejected, w), 10, 5*time.Hour) // not in live set
		b.seed(mediaKey(event, oldOrphan, w), 10, 2*time.Hour)
		b.seed(mediaKey(event, youngOrphan, w), 10, 5*time.Minute)
	}
	b.seed(badKey, 1, 5*time.Hour)
	b.seed(tmplKey, 1, 5*time.Hour)
	b.pageSize = 5 // several pages, one media can straddle pages

	stats, err := s.Reconcile(context.Background(), liveSet(live))
	if err != nil {
		t.Fatalf("Reconcile: %v", err)
	}
	// 4 media x len(Widths) objects of 10 bytes, plus the junk and template
	// keys (the template key is under t/, so only the junk key is listed).
	wantObjects := 4*len(Widths) + 1
	wantBytes := int64(4*len(Widths)*10 + 1)
	if stats.Objects != wantObjects || stats.Bytes != wantBytes || stats.OrphansDeleted != 2*len(Widths) {
		t.Errorf("stats = %+v, want objects %d, bytes %d, orphans %d", stats, wantObjects, wantBytes, 2*len(Widths))
	}

	for _, w := range Widths {
		if !b.has(mediaKey(event, live, w)) {
			t.Errorf("live media %d deleted", w)
		}
		if b.has(mediaKey(event, rejected, w)) {
			t.Errorf("rejected media %d kept", w)
		}
		if b.has(mediaKey(event, oldOrphan, w)) {
			t.Errorf("old orphan %d kept", w)
		}
		if !b.has(mediaKey(event, youngOrphan, w)) {
			t.Errorf("young orphan %d deleted", w)
		}
	}
	if !b.has(badKey) {
		t.Error("unparseable key must be kept")
	}
	if !b.has(tmplKey) {
		t.Error("template objects must not be reconciled")
	}
}

func TestReconcile_Errors(t *testing.T) {
	errBoom := errors.New("boom")
	seed := func(b *memBlobs) {
		b.seed(mediaKey(uuid.Must(uuid.NewV7()), uuid.Must(uuid.NewV7()), 480), 1, 5*time.Hour)
	}
	tests := []struct {
		name  string
		setup func(b *memBlobs)
		live  LiveMediaFunc
	}{
		{"list fails", func(b *memBlobs) { b.listErr = errBoom }, liveSet()},
		{"live lookup fails", seed, func(context.Context, []uuid.UUID) (map[uuid.UUID]bool, error) { return nil, errBoom }},
		{"delete fails", func(b *memBlobs) { seed(b); b.deleteErr = errBoom }, liveSet()},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			s, b := newTestStoreBlobs(t)
			tt.setup(b)
			if _, err := s.Reconcile(context.Background(), tt.live); !errors.Is(err, errBoom) {
				t.Errorf("err = %v, want boom", err)
			}
		})
	}
}

func TestReconcile_SkipsLookupWithoutParseableKeys(t *testing.T) {
	s, b := newTestStoreBlobs(t)
	b.seed("e/junk.jpg", 1, 5*time.Hour)
	called := false
	_, err := s.Reconcile(context.Background(), func(context.Context, []uuid.UUID) (map[uuid.UUID]bool, error) {
		called = true
		return nil, nil
	})
	if err != nil {
		t.Fatal(err)
	}
	if called {
		t.Error("live lookup should not run for a page with no parseable keys")
	}
}

func TestMediaIDFromKey(t *testing.T) {
	ev, md := uuid.Must(uuid.NewV7()), uuid.Must(uuid.NewV7())
	tests := []struct {
		key string
		ok  bool
	}{
		{mediaKey(ev, md, 480), true},
		{mediaKey(ev, md, 1920), true},
		{"e/" + ev.String() + "/" + md.String() + "/500.jpg", false},
		{"e/" + ev.String() + "/" + md.String() + "/480.png", false},
		{"e/" + ev.String() + "/" + md.String(), false},
		{"e/bad/" + md.String() + "/480.jpg", false},
		{"e/" + ev.String() + "/bad/480.jpg", false},
		{templateKey(ev, 1, 480), false},
		{strings.ToUpper(mediaKey(ev, md, 480)), false},
		{"e/" + strings.ToUpper(ev.String()) + "/" + md.String() + "/480.jpg", false},
		{"e/" + ev.String() + "/" + strings.ToUpper(md.String()) + "/480.jpg", false},
		{"e/" + ev.String() + "/" + strings.ReplaceAll(md.String(), "-", "") + "/480.jpg", false},
		{"e/" + ev.String() + "/" + uuid.Nil.String() + "/480.jpg", false},
		{"e/" + ev.String() + "/" + md.String() + "/0480.jpg", false},
		{"e/" + ev.String() + "//" + md.String() + "/480.jpg", false},
		{"e/" + ev.String() + "/" + md.String() + "/480.jpg\n", false},
		{"", false},
	}
	for _, tt := range tests {
		t.Run(tt.key, func(t *testing.T) {
			id, ok := mediaIDFromKey(tt.key)
			if ok != tt.ok || (ok && id != md) {
				t.Errorf("got (%v, %v), want ok=%v", id, ok, tt.ok)
			}
		})
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

	if _, err := s.Reconcile(context.Background(), liveSet()); err != nil {
		t.Fatalf("Reconcile: %v", err)
	}

	if _, err := os.Stat(stale); !os.IsNotExist(err) {
		t.Error("stale tmp entry should have been removed")
	}
	if _, err := os.Stat(fresh); err != nil {
		t.Error("fresh tmp entry should survive")
	}
}
