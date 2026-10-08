package media

import (
	"context"
	"fmt"
	"log/slog"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"time"

	"github.com/google/uuid"
)

// orphanAge is how long an object with no live media row must sit before
// it's removed, so a request that's mid-flight (objects uploaded, row insert
// not yet committed) is never deleted out from under it.
const orphanAge = time.Hour

// LiveMediaFunc reports which of the given media ids still have a live
// (non-rejected) row. It takes at most one listing page of ids (<= 1000).
type LiveMediaFunc func(ctx context.Context, ids []uuid.UUID) (map[uuid.UUID]bool, error)

// ReconcileStats summarises one reconcile pass: every object listed under
// "e/" (Objects, Bytes, taken before any orphan was removed) and how many
// orphans were deleted.
type ReconcileStats struct {
	Objects        int
	Bytes          int64
	OrphansDeleted int
}

// Reconcile removes objects under "e/" whose media row is gone or rejected
// and that are older than orphanAge (a crash between Store.Commit and the
// row insert leaves exactly this kind of orphan), and removes stale tmp/
// entries. Keys it can't parse are logged and left alone. Template objects
// ("t/") are not reconciled. It takes its DB lookup as a function rather
// than importing internal/store, keeping this package free of a database
// dependency; the jobs package wires it to a real query. Memory is bounded
// by one listing page. The stats are returned even alongside an error, for
// whatever was processed before it.
func (s *Store) Reconcile(ctx context.Context, live LiveMediaFunc) (ReconcileStats, error) {
	now := time.Now()
	var st ReconcileStats
	var unparseable int

	err := s.blobs.List(ctx, "e/", func(page []ObjectInfo) error {
		ids := make([]uuid.UUID, 0, len(page))
		keyMedia := make([]uuid.UUID, len(page)) // uuid.Nil = unparseable
		seen := make(map[uuid.UUID]struct{}, len(page))
		for i, o := range page {
			st.Objects++
			st.Bytes += o.Size
			id, ok := mediaIDFromKey(o.Key)
			if !ok {
				unparseable++
				continue
			}
			keyMedia[i] = id
			if _, dup := seen[id]; !dup {
				seen[id] = struct{}{}
				ids = append(ids, id)
			}
		}
		if len(ids) == 0 {
			return nil
		}

		liveIDs, err := live(ctx, ids)
		if err != nil {
			return fmt.Errorf("check live media: %w", err)
		}
		var doomed []string
		for i, o := range page {
			id := keyMedia[i]
			if id == uuid.Nil || liveIDs[id] || now.Sub(o.LastModified) <= orphanAge {
				continue
			}
			doomed = append(doomed, o.Key)
		}
		if len(doomed) == 0 {
			return nil
		}
		if err := s.blobs.Delete(ctx, doomed); err != nil {
			return fmt.Errorf("delete orphans: %w", err)
		}
		st.OrphansDeleted += len(doomed)
		return nil
	})
	if err != nil {
		return st, fmt.Errorf("media: reconcile: %w", err)
	}

	if unparseable > 0 {
		slog.WarnContext(ctx, "media reconcile skipped unrecognised keys", "count", unparseable)
	}
	slog.InfoContext(ctx, "media reconcile", "objects", st.Objects, "bytes", st.Bytes, "orphans_deleted", st.OrphansDeleted)
	return st, s.reconcileTmp(now)
}

// mediaIDFromKey extracts the media id from "e/<event>/<media>/<w>.jpg". The
// key must be valid and both ids canonical (lowercase, hyphenated), so every
// object is attributed to exactly one id and nothing else is ever deleted.
func mediaIDFromKey(key string) (uuid.UUID, bool) {
	if !ValidKey(key) {
		return uuid.Nil, false
	}
	parts := strings.Split(key, "/")
	if len(parts) != 4 || parts[0] != "e" {
		return uuid.Nil, false
	}
	if _, ok := canonicalUUID(parts[1]); !ok {
		return uuid.Nil, false
	}
	name, ok := strings.CutSuffix(parts[3], ".jpg")
	if !ok {
		return uuid.Nil, false
	}
	if w, err := strconv.Atoi(name); err != nil || !validWidth(w) || strconv.Itoa(w) != name {
		return uuid.Nil, false
	}
	id, ok := canonicalUUID(parts[2])
	if !ok || id == uuid.Nil {
		return uuid.Nil, false
	}
	return id, true
}

// canonicalUUID parses only the lowercase hyphenated form.
func canonicalUUID(s string) (uuid.UUID, bool) {
	id, err := uuid.Parse(s)
	if err != nil || id.String() != s {
		return uuid.Nil, false
	}
	return id, true
}

func (s *Store) reconcileTmp(now time.Time) error {
	entries, err := os.ReadDir(s.tmpDir())
	if err != nil {
		if os.IsNotExist(err) {
			return nil
		}
		return fmt.Errorf("media: reconcile list tmp: %w", err)
	}
	for _, e := range entries {
		path := filepath.Join(s.tmpDir(), e.Name())
		if olderThan(path, orphanAge, now) {
			_ = os.RemoveAll(path)
		}
	}
	return nil
}

func olderThan(path string, age time.Duration, now time.Time) bool {
	info, err := os.Stat(path)
	if err != nil {
		return false
	}
	return now.Sub(info.ModTime()) > age
}
