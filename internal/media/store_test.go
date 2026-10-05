package media

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"testing"

	"github.com/google/uuid"
)

func writeRendition(t *testing.T, dir string, w int) {
	t.Helper()
	if err := os.WriteFile(filepath.Join(dir, renditionName(w)), []byte("fake jpeg bytes"), 0o644); err != nil {
		t.Fatalf("write rendition: %v", err)
	}
}

func TestNewStore_CreatesLayout(t *testing.T) {
	root := t.TempDir()
	if _, err := NewStore(root); err != nil {
		t.Fatalf("NewStore: %v", err)
	}
	for _, dir := range []string{"tmp", "public", "pending", "quarantine"} {
		if info, err := os.Stat(filepath.Join(root, dir)); err != nil || !info.IsDir() {
			t.Errorf("expected directory %s to exist", dir)
		}
	}
}

func TestStore_CommitAndOpen(t *testing.T) {
	s := newTestStore(t)
	eventID, mediaID := uuid.Must(uuid.NewV7()), uuid.Must(uuid.NewV7())

	outDir := t.TempDir()
	for _, w := range Widths {
		writeRendition(t, outDir, w)
	}
	if err := s.Commit(outDir, AreaPublic, eventID, mediaID); err != nil {
		t.Fatalf("Commit: %v", err)
	}

	f, err := s.Open(eventID, mediaID, 480)
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	f.Close()

	if _, err := os.Stat(outDir); !os.IsNotExist(err) {
		t.Error("outDir should have been moved (renamed), not copied")
	}
}

func TestStore_Open_NotFound(t *testing.T) {
	s := newTestStore(t)
	if err := openAndClose(s, uuid.Must(uuid.NewV7()), uuid.Must(uuid.NewV7()), 480); !os.IsNotExist(err) {
		t.Errorf("err = %v, want os.IsNotExist", err)
	}
}

func TestStore_MoveMedia(t *testing.T) {
	s := newTestStore(t)
	eventID, mediaID := uuid.Must(uuid.NewV7()), uuid.Must(uuid.NewV7())
	outDir := t.TempDir()
	writeRendition(t, outDir, 480)
	if err := s.Commit(outDir, AreaPending, eventID, mediaID); err != nil {
		t.Fatalf("Commit: %v", err)
	}

	if err := s.MoveMedia(AreaPending, AreaPublic, eventID, mediaID); err != nil {
		t.Fatalf("MoveMedia: %v", err)
	}
	if err := openAndClose(s, eventID, mediaID, 480); err != nil {
		t.Fatalf("expected media to be openable from public after move: %v", err)
	}

	// Missing source is not an error.
	if err := s.MoveMedia(AreaPending, AreaQuarantine, eventID, mediaID); err != nil {
		t.Errorf("MoveMedia of an already-moved (now missing) source should not error: %v", err)
	}
}

func TestStore_MoveEvent_IdempotentAndMerges(t *testing.T) {
	s := newTestStore(t)
	eventID := uuid.Must(uuid.NewV7())
	media1, media2 := uuid.Must(uuid.NewV7()), uuid.Must(uuid.NewV7())

	for _, m := range []uuid.UUID{media1, media2} {
		outDir := t.TempDir()
		writeRendition(t, outDir, 480)
		if err := s.Commit(outDir, AreaPublic, eventID, m); err != nil {
			t.Fatalf("Commit: %v", err)
		}
	}

	if err := s.MoveEvent(AreaPublic, AreaQuarantine, eventID); err != nil {
		t.Fatalf("MoveEvent: %v", err)
	}
	for _, m := range []uuid.UUID{media1, media2} {
		if err := openAndClose(s, eventID, m, 480); err != nil {
			t.Errorf("media %s should be openable from quarantine: %v", m, err)
		}
	}

	// Idempotent: moving again (source now empty/missing) is a no-op, not an error.
	if err := s.MoveEvent(AreaPublic, AreaQuarantine, eventID); err != nil {
		t.Errorf("repeat MoveEvent should be a no-op: %v", err)
	}

	// Move back, then add a new media item to "public" and move again: the
	// existing quarantine dir must be merged into, not clobbered/failed.
	if err := s.MoveEvent(AreaQuarantine, AreaPublic, eventID); err != nil {
		t.Fatalf("MoveEvent back: %v", err)
	}
	media3 := uuid.Must(uuid.NewV7())
	outDir := t.TempDir()
	writeRendition(t, outDir, 480)
	if err := s.Commit(outDir, AreaQuarantine, eventID, media3); err != nil {
		t.Fatalf("Commit: %v", err)
	}
	if err := s.MoveEvent(AreaPublic, AreaQuarantine, eventID); err != nil {
		t.Fatalf("MoveEvent merge: %v", err)
	}
	for _, m := range []uuid.UUID{media1, media2, media3} {
		if err := openAndClose(s, eventID, m, 480); err != nil {
			t.Errorf("media %s should be openable after merge: %v", m, err)
		}
	}
}

func TestStore_DeleteMedia(t *testing.T) {
	s := newTestStore(t)
	eventID, mediaID := uuid.Must(uuid.NewV7()), uuid.Must(uuid.NewV7())
	outDir := t.TempDir()
	writeRendition(t, outDir, 480)
	if err := s.Commit(outDir, AreaPublic, eventID, mediaID); err != nil {
		t.Fatalf("Commit: %v", err)
	}

	if err := s.DeleteMedia(eventID, mediaID); err != nil {
		t.Fatalf("DeleteMedia: %v", err)
	}
	if err := openAndClose(s, eventID, mediaID, 480); !os.IsNotExist(err) {
		t.Errorf("media should be gone after delete, err = %v", err)
	}

	// Deleting again (already missing) is not an error.
	if err := s.DeleteMedia(eventID, mediaID); err != nil {
		t.Errorf("deleting already-missing media should not error: %v", err)
	}
}

func TestStore_CommitTemplateAsset(t *testing.T) {
	s := newTestStore(t)
	templateID := uuid.Must(uuid.NewV7())
	outDir := t.TempDir()
	writeRendition(t, outDir, 480)

	assetsPath, err := s.CommitTemplateAsset(outDir, templateID, 3)
	if err != nil {
		t.Fatalf("CommitTemplateAsset: %v", err)
	}
	want := "templates/" + templateID.String() + "/3"
	if assetsPath != want {
		t.Errorf("assetsPath = %q, want %q", assetsPath, want)
	}
	if _, err := os.Stat(filepath.Join(s.root, "public", assetsPath, "background", "480.jpg")); err != nil {
		t.Errorf("expected background file at the returned path: %v", err)
	}
}

func TestStore_CommitTemplateAsset_Reupload(t *testing.T) {
	s := newTestStore(t)
	templateID := uuid.Must(uuid.NewV7())

	firstDir := t.TempDir()
	writeRendition(t, firstDir, 480)
	if _, err := s.CommitTemplateAsset(firstDir, templateID, 3); err != nil {
		t.Fatalf("first CommitTemplateAsset: %v", err)
	}

	// Re-upload: outDir differs (a new file) from the previously committed
	// asset. On most platforms os.Rename onto a non-empty directory fails,
	// so CommitTemplateAsset must swap it in rather than plain-renaming.
	secondDir := t.TempDir()
	writeRendition(t, secondDir, 1080)
	assetsPath, err := s.CommitTemplateAsset(secondDir, templateID, 3)
	if err != nil {
		t.Fatalf("second CommitTemplateAsset (reupload): %v", err)
	}

	bgDir := filepath.Join(s.root, "public", assetsPath, "background")
	if _, err := os.Stat(filepath.Join(bgDir, "1080.jpg")); err != nil {
		t.Errorf("expected new rendition at the returned path: %v", err)
	}
	if _, err := os.Stat(filepath.Join(bgDir, "480.jpg")); !os.IsNotExist(err) {
		t.Errorf("expected old rendition to be gone after reupload, err = %v", err)
	}

	// No staging/displaced siblings should be left behind.
	entries, err := os.ReadDir(filepath.Dir(s.templateDir(templateID, 3)))
	if err != nil {
		t.Fatalf("ReadDir: %v", err)
	}
	if len(entries) != 1 || entries[0].Name() != "background" {
		names := make([]string, len(entries))
		for i, e := range entries {
			names[i] = e.Name()
		}
		t.Errorf("expected only the committed version dir, got %v", names)
	}
}

func TestStore_CommitTemplateAsset_FailurePartwayLeavesExistingAssetIntact(t *testing.T) {
	s := newTestStore(t)
	templateID := uuid.Must(uuid.NewV7())

	firstDir := t.TempDir()
	writeRendition(t, firstDir, 480)
	assetsPath, err := s.CommitTemplateAsset(firstDir, templateID, 3)
	if err != nil {
		t.Fatalf("first CommitTemplateAsset: %v", err)
	}

	// A non-existent outDir makes the initial stage rename fail before
	// anything about the existing asset is touched.
	if _, err := s.CommitTemplateAsset(filepath.Join(t.TempDir(), "missing"), templateID, 3); err == nil {
		t.Fatal("expected error for missing outDir")
	}

	bgDir := filepath.Join(s.root, "public", assetsPath, "background")
	if _, err := os.Stat(filepath.Join(bgDir, "480.jpg")); err != nil {
		t.Errorf("existing asset should be untouched after a failed reupload: %v", err)
	}
	entries, err := os.ReadDir(filepath.Dir(s.templateDir(templateID, 3)))
	if err != nil {
		t.Fatalf("ReadDir: %v", err)
	}
	if len(entries) != 1 || entries[0].Name() != "background" {
		names := make([]string, len(entries))
		for i, e := range entries {
			names[i] = e.Name()
		}
		t.Errorf("expected only the original version dir after failure, got %v", names)
	}
}

func TestStore_CheckFree(t *testing.T) {
	s := newTestStore(t)
	// On a real dev machine this should always have >2GiB free; this is a
	// smoke test that CheckFree doesn't error, not a disk-full simulation
	// (see diskfree_other.go's no-op on non-Linux).
	if err := s.CheckFree(); err != nil {
		t.Errorf("CheckFree: %v", err)
	}
}

func TestStore_DeleteEventFiles(t *testing.T) {
	s := newTestStore(t)
	eventA, eventB := uuid.Must(uuid.NewV7()), uuid.Must(uuid.NewV7())
	templateID := uuid.Must(uuid.NewV7())

	put := func(area Area, eventID uuid.UUID) string {
		t.Helper()
		dir := filepath.Join(s.root, string(area), eventID.String(), uuid.Must(uuid.NewV7()).String())
		if err := os.MkdirAll(dir, 0o755); err != nil {
			t.Fatalf("mkdir: %v", err)
		}
		writeRendition(t, dir, Widths[0])
		return filepath.Dir(dir)
	}
	areas := []Area{AreaPublic, AreaPending, AreaQuarantine}
	var aDirs, bDirs []string
	for _, a := range areas {
		aDirs = append(aDirs, put(a, eventA))
		bDirs = append(bDirs, put(a, eventB))
	}
	templateFile := filepath.Join(s.root, "public", "templates", templateID.String(), "1", "background")
	if err := os.MkdirAll(templateFile, 0o755); err != nil {
		t.Fatalf("mkdir template: %v", err)
	}

	exists := func(path string) bool {
		_, err := os.Stat(path)
		return err == nil
	}

	if err := s.DeleteEventFiles(context.Background(), eventA); err != nil {
		t.Fatalf("DeleteEventFiles: %v", err)
	}
	for _, d := range aDirs {
		if exists(d) {
			t.Errorf("%s still exists after delete", d)
		}
	}
	for _, d := range bDirs {
		if !exists(d) {
			t.Errorf("%s of another event was removed", d)
		}
	}
	if !exists(templateFile) {
		t.Error("public/templates was removed")
	}

	t.Run("second call and never-existing event", func(t *testing.T) {
		if err := s.DeleteEventFiles(context.Background(), eventA); err != nil {
			t.Errorf("second call: %v", err)
		}
		if err := s.DeleteEventFiles(context.Background(), uuid.Must(uuid.NewV7())); err != nil {
			t.Errorf("missing event: %v", err)
		}
	})

	t.Run("cancelled context", func(t *testing.T) {
		ctx, cancel := context.WithCancel(context.Background())
		cancel()
		err := s.DeleteEventFiles(ctx, eventB)
		if !errors.Is(err, context.Canceled) {
			t.Fatalf("err = %v, want context.Canceled", err)
		}
		for _, d := range bDirs {
			if !exists(d) {
				t.Errorf("%s removed despite cancelled context", d)
			}
		}
	})
}
