package media

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"github.com/google/uuid"
)

func writeRenditions(t *testing.T, dir string) {
	t.Helper()
	for _, w := range Widths {
		if err := os.WriteFile(filepath.Join(dir, renditionName(w)), []byte(fmt.Sprintf("jpeg-%d", w)), 0o644); err != nil {
			t.Fatalf("write rendition: %v", err)
		}
	}
}

func newIDs() (uuid.UUID, uuid.UUID) { return uuid.Must(uuid.NewV7()), uuid.Must(uuid.NewV7()) }

func TestNewStore_CreatesTmp(t *testing.T) {
	root := t.TempDir()
	if _, err := NewStore(root, newMemBlobs()); err != nil {
		t.Fatalf("NewStore: %v", err)
	}
	if info, err := os.Stat(filepath.Join(root, "tmp")); err != nil || !info.IsDir() {
		t.Error("expected tmp directory to exist")
	}
}

func TestNewStore_RootNotWritable(t *testing.T) {
	file := filepath.Join(t.TempDir(), "file")
	if err := os.WriteFile(file, nil, 0o644); err != nil {
		t.Fatal(err)
	}
	if _, err := NewStore(file, newMemBlobs()); err == nil {
		t.Error("expected error when root is a file")
	}
}

func TestStore_Commit(t *testing.T) {
	s, b := newTestStoreBlobs(t)
	eventID, mediaID := newIDs()
	outDir := t.TempDir()
	writeRenditions(t, outDir)

	if err := s.Commit(context.Background(), outDir, eventID, mediaID); err != nil {
		t.Fatalf("Commit: %v", err)
	}
	for _, w := range Widths {
		if !b.has(fmt.Sprintf("e/%s/%s/%d.jpg", eventID, mediaID, w)) {
			t.Errorf("rendition %d not stored", w)
		}
	}
	if b.count() != 3 {
		t.Errorf("objects = %d, want 3", b.count())
	}
	if _, err := os.Stat(outDir); !os.IsNotExist(err) {
		t.Error("outDir should be removed after Commit")
	}
}

func TestStore_Commit_Failures(t *testing.T) {
	errBoom := errors.New("boom")
	tests := []struct {
		name    string
		failOn  int // width whose Put fails; 0 = missing rendition file instead
		cancel  bool
		wantErr error
	}{
		{name: "second rendition put fails", failOn: 1080, wantErr: errBoom},
		{name: "first rendition put fails", failOn: 480, wantErr: errBoom},
		{name: "cancelled context", cancel: true, wantErr: context.Canceled},
		{name: "missing rendition file", failOn: 0, wantErr: os.ErrNotExist},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			s, b := newTestStoreBlobs(t)
			eventID, mediaID := newIDs()
			outDir := t.TempDir()
			writeRenditions(t, outDir)
			ctx := context.Background()
			switch {
			case tt.cancel:
				c, cancel := context.WithCancel(ctx)
				cancel()
				ctx = c
				b.putErr = func(string) error { return c.Err() }
			case tt.failOn == 0:
				if err := os.Remove(filepath.Join(outDir, renditionName(Widths[2]))); err != nil {
					t.Fatal(err)
				}
			default:
				b.putErr = func(key string) error {
					if strings.HasSuffix(key, fmt.Sprintf("/%d.jpg", tt.failOn)) {
						return errBoom
					}
					return nil
				}
			}

			err := s.Commit(ctx, outDir, eventID, mediaID)
			if !errors.Is(err, tt.wantErr) {
				t.Fatalf("err = %v, want %v", err, tt.wantErr)
			}
			if b.count() != 0 {
				t.Errorf("%d objects left after failed Commit", b.count())
			}
			if _, err := os.Stat(outDir); !os.IsNotExist(err) {
				t.Error("outDir should be removed even when Commit fails")
			}
			if tt.cancel && b.deletes == 0 {
				t.Error("cleanup delete should run despite cancelled context")
			}
		})
	}
}

func TestStore_Commit_CleanupFailureReturnsPutError(t *testing.T) {
	s, b := newTestStoreBlobs(t)
	errPut := errors.New("put failed")
	b.putErr = func(key string) error {
		if strings.HasSuffix(key, "/1920.jpg") {
			return errPut
		}
		return nil
	}
	b.deleteErr = errors.New("delete failed")
	outDir := t.TempDir()
	writeRenditions(t, outDir)
	eventID, mediaID := newIDs()
	if err := s.Commit(context.Background(), outDir, eventID, mediaID); !errors.Is(err, errPut) {
		t.Errorf("err = %v, want the put error", err)
	}
}

func TestStore_Open(t *testing.T) {
	s, b := newTestStoreBlobs(t)
	eventID, mediaID := newIDs()
	outDir := t.TempDir()
	writeRenditions(t, outDir)
	if err := s.Commit(context.Background(), outDir, eventID, mediaID); err != nil {
		t.Fatal(err)
	}

	obj, err := s.Open(context.Background(), eventID, mediaID, 480, "")
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	body, _ := io.ReadAll(obj.Body)
	obj.Body.Close()
	if string(body) != "jpeg-480" || obj.Size != int64(len(body)) || obj.ETag == "" {
		t.Errorf("got body %q size %d etag %q", body, obj.Size, obj.ETag)
	}

	tests := []struct {
		name    string
		event   uuid.UUID
		media   uuid.UUID
		width   int
		inm     string
		wantErr error
		wantGet bool
	}{
		{"unknown media", eventID, uuid.Must(uuid.NewV7()), 480, "", ErrNotFound, true},
		{"wrong event", uuid.Must(uuid.NewV7()), mediaID, 480, "", ErrNotFound, true},
		{"invalid width", eventID, mediaID, 500, "", ErrNotFound, false},
		{"zero width", eventID, mediaID, 0, "", ErrNotFound, false},
		{"etag match", eventID, mediaID, 480, obj.ETag, ErrNotModified, true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			before := b.gets
			_, err := s.Open(context.Background(), tt.event, tt.media, tt.width, tt.inm)
			if !errors.Is(err, tt.wantErr) {
				t.Errorf("err = %v, want %v", err, tt.wantErr)
			}
			if got := b.gets - before; (got == 1) != tt.wantGet {
				t.Errorf("backend gets = %d, wantGet = %v", got, tt.wantGet)
			}
		})
	}

	t.Run("backend error passes through", func(t *testing.T) {
		b.getErr = ErrStorageUnavailable
		if _, err := s.Open(context.Background(), eventID, mediaID, 480, ""); !errors.Is(err, ErrStorageUnavailable) {
			t.Errorf("err = %v", err)
		}
	})
}

func TestStore_DeleteMedia(t *testing.T) {
	s, b := newTestStoreBlobs(t)
	eventID, mediaID := newIDs()
	otherMedia := uuid.Must(uuid.NewV7())
	for _, m := range []uuid.UUID{mediaID, otherMedia} {
		outDir := t.TempDir()
		writeRenditions(t, outDir)
		if err := s.Commit(context.Background(), outDir, eventID, m); err != nil {
			t.Fatal(err)
		}
	}

	if err := s.DeleteMedia(context.Background(), eventID, mediaID); err != nil {
		t.Fatalf("DeleteMedia: %v", err)
	}
	if b.count() != 3 || !b.has(mediaKey(eventID, otherMedia, 480)) {
		t.Errorf("only the named media should be removed, %d objects left", b.count())
	}
	if err := s.DeleteMedia(context.Background(), eventID, mediaID); err != nil {
		t.Errorf("deleting already-missing media: %v", err)
	}

	t.Run("no ids makes no backend call", func(t *testing.T) {
		before := b.deletes
		if err := s.DeleteMedia(context.Background(), eventID); err != nil {
			t.Fatal(err)
		}
		if b.deletes != before {
			t.Error("backend Delete called for empty id list")
		}
	})

	t.Run("many ids go in one backend call", func(t *testing.T) {
		ids := make([]uuid.UUID, 400)
		for i := range ids {
			ids[i] = uuid.Must(uuid.NewV7())
		}
		before := len(b.deleteBatches)
		if err := s.DeleteMedia(context.Background(), eventID, ids...); err != nil {
			t.Fatal(err)
		}
		if len(b.deleteBatches) != before+1 || len(b.deleteBatches[before]) != 1200 {
			t.Errorf("want one batch of 1200 keys, got %d batches", len(b.deleteBatches)-before)
		}
	})

	t.Run("backend failure is returned", func(t *testing.T) {
		b.deleteErr = ErrStorageUnavailable
		if err := s.DeleteMedia(context.Background(), eventID, otherMedia); !errors.Is(err, ErrStorageUnavailable) {
			t.Errorf("err = %v", err)
		}
		if !b.has(mediaKey(eventID, otherMedia, 480)) {
			t.Error("object removed despite failing backend")
		}
	})
}

func TestStore_CommitTemplateAsset(t *testing.T) {
	s, b := newTestStoreBlobs(t)
	templateID := uuid.Must(uuid.NewV7())
	outDir := t.TempDir()
	writeRenditions(t, outDir)

	assetsPath, err := s.CommitTemplateAsset(context.Background(), outDir, templateID, 3)
	if err != nil {
		t.Fatalf("CommitTemplateAsset: %v", err)
	}
	if want := "templates/" + templateID.String() + "/3"; assetsPath != want {
		t.Errorf("assetsPath = %q, want %q", assetsPath, want)
	}
	for _, w := range Widths {
		if !b.has(fmt.Sprintf("t/%s/3/%d.jpg", templateID, w)) {
			t.Errorf("rendition %d not stored", w)
		}
	}
	if _, err := os.Stat(outDir); !os.IsNotExist(err) {
		t.Error("outDir should be removed")
	}

	// Re-upload overwrites in place.
	outDir = t.TempDir()
	for _, w := range Widths {
		if err := os.WriteFile(filepath.Join(outDir, renditionName(w)), []byte("v2"), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	if _, err := s.CommitTemplateAsset(context.Background(), outDir, templateID, 3); err != nil {
		t.Fatalf("reupload: %v", err)
	}
	obj, err := s.OpenTemplateAsset(context.Background(), templateID, 3, 1080, "")
	if err != nil {
		t.Fatalf("OpenTemplateAsset: %v", err)
	}
	body, _ := io.ReadAll(obj.Body)
	obj.Body.Close()
	if string(body) != "v2" {
		t.Errorf("body = %q, want overwritten content", body)
	}

	t.Run("put failure", func(t *testing.T) {
		b.putErr = func(string) error { return ErrStorageUnavailable }
		out := t.TempDir()
		writeRenditions(t, out)
		if _, err := s.CommitTemplateAsset(context.Background(), out, templateID, 4); !errors.Is(err, ErrStorageUnavailable) {
			t.Errorf("err = %v", err)
		}
		if _, err := os.Stat(out); !os.IsNotExist(err) {
			t.Error("outDir should be removed on failure")
		}
	})
}

func TestStore_OpenTemplateAsset_Errors(t *testing.T) {
	s, _ := newTestStoreBlobs(t)
	id := uuid.Must(uuid.NewV7())
	tests := []struct {
		name  string
		width int
	}{
		{"missing object", 480},
		{"invalid width", 481},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if _, err := s.OpenTemplateAsset(context.Background(), id, 1, tt.width, ""); !errors.Is(err, ErrNotFound) {
				t.Errorf("err = %v, want ErrNotFound", err)
			}
		})
	}
}

func TestStore_CheckFree(t *testing.T) {
	s := newTestStore(t)
	// Smoke test that CheckFree doesn't error (see diskfree_other.go's
	// no-op on non-Linux), not a disk-full simulation.
	if err := s.CheckFree(); err != nil {
		t.Errorf("CheckFree: %v", err)
	}
}

func TestStore_DeleteEventFiles(t *testing.T) {
	s, b := newTestStoreBlobs(t)
	b.pageSize = 2 // force several pages
	eventA, eventB := newIDs()
	templateID := uuid.Must(uuid.NewV7())

	for i := 0; i < 3; i++ {
		for _, ev := range []uuid.UUID{eventA, eventB} {
			outDir := t.TempDir()
			writeRenditions(t, outDir)
			if err := s.Commit(context.Background(), outDir, ev, uuid.Must(uuid.NewV7())); err != nil {
				t.Fatal(err)
			}
		}
	}
	b.seed(templateKey(templateID, 1, 480), 5, 0)
	b.seed("e/"+eventA.String()+"/stray-orphan.jpg", 5, 0) // sweeps orphans too

	if err := s.DeleteEventFiles(context.Background(), eventA); err != nil {
		t.Fatalf("DeleteEventFiles: %v", err)
	}
	for k := range b.objs {
		if strings.HasPrefix(k, "e/"+eventA.String()+"/") {
			t.Errorf("%s of the deleted event remains", k)
		}
	}
	if b.count() != 9+1 {
		t.Errorf("objects left = %d, want 10 (event B + template)", b.count())
	}
	if len(b.deleteBatches) < 5 {
		t.Errorf("expected page-by-page deletes, got %d batches", len(b.deleteBatches))
	}

	t.Run("second call and never-existing event", func(t *testing.T) {
		if err := s.DeleteEventFiles(context.Background(), eventA); err != nil {
			t.Errorf("second call: %v", err)
		}
		if err := s.DeleteEventFiles(context.Background(), uuid.Must(uuid.NewV7())); err != nil {
			t.Errorf("missing event: %v", err)
		}
	})

	t.Run("list failure", func(t *testing.T) {
		b.listErr = ErrStorageUnavailable
		defer func() { b.listErr = nil }()
		if err := s.DeleteEventFiles(context.Background(), eventB); !errors.Is(err, ErrStorageUnavailable) {
			t.Errorf("err = %v", err)
		}
	})

	t.Run("delete failure keeps objects", func(t *testing.T) {
		b.deleteErr = ErrStorageUnavailable
		defer func() { b.deleteErr = nil }()
		before := b.count()
		if err := s.DeleteEventFiles(context.Background(), eventB); !errors.Is(err, ErrStorageUnavailable) {
			t.Errorf("err = %v", err)
		}
		if b.count() != before {
			t.Error("objects removed despite failing delete")
		}
	})

	t.Run("invalid keys are skipped, not deleted", func(t *testing.T) {
		bad := "e/" + eventB.String() + "/UPPER.JPG"
		b.seed(bad, 5, 0)
		b.deleteBatches = nil
		if err := s.DeleteEventFiles(context.Background(), eventB); err != nil {
			t.Fatalf("DeleteEventFiles: %v", err)
		}
		for _, batch := range b.deleteBatches {
			if slices.Contains(batch, bad) {
				t.Errorf("invalid key passed to Delete: %v", batch)
			}
		}
		if !b.has(bad) {
			t.Error("invalid key should be left in place")
		}
		for k := range b.objs {
			if strings.HasPrefix(k, "e/"+eventB.String()+"/") && k != bad {
				t.Errorf("%s of the deleted event remains", k)
			}
		}
		delete(b.objs, bad)
	})
}
