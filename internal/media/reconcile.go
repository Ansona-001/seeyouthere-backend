package media

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"time"

	"github.com/google/uuid"
)

// batchSize bounds how many directory entries reconcile inspects per DB
// round trip, matching the plan's "batches of 500 names".
const batchSize = 500

// orphanAge is how long a directory with no matching DB row must sit before
// it's removed, so a request that's mid-flight (files committed, row insert
// not yet committed) is never deleted out from under it.
const orphanAge = time.Hour

// EventExistsFunc reports which of the given (candidate) event ids still
// have a live row, backed by ExistingEventIDs.
type EventExistsFunc func(ctx context.Context, ids []uuid.UUID) (map[uuid.UUID]bool, error)

// EventMediaFunc reports which media ids exist for eventID, backed by
// MediaIDsForEvent.
type EventMediaFunc func(ctx context.Context, eventID uuid.UUID) (map[uuid.UUID]bool, error)

// Reconcile walks public/, pending/ and quarantine/ removing directories
// that don't correspond to a database row and are older than orphanAge (a
// crash between Store.Commit and the row insert leaves exactly this kind of
// orphan; see decision 9 in the media pipeline notes), and removes stale
// tmp/ entries. It takes its DB lookups as functions rather than importing
// internal/store directly, keeping this package free of a database
// dependency; the jobs package wires these to real queries.
func (s *Store) Reconcile(ctx context.Context, eventExists EventExistsFunc, eventMedia EventMediaFunc) error {
	now := time.Now()
	for _, area := range []Area{AreaPublic, AreaPending, AreaQuarantine} {
		if err := s.reconcileArea(ctx, area, eventExists, eventMedia, now); err != nil {
			return err
		}
	}
	return s.reconcileTmp(now)
}

func (s *Store) reconcileArea(ctx context.Context, area Area, eventExists EventExistsFunc, eventMedia EventMediaFunc, now time.Time) error {
	areaRoot := filepath.Join(s.root, string(area))
	entries, err := os.ReadDir(areaRoot)
	if err != nil {
		if os.IsNotExist(err) {
			return nil
		}
		return fmt.Errorf("media: reconcile list %s: %w", area, err)
	}

	for batchStart := 0; batchStart < len(entries); batchStart += batchSize {
		batch := entries[batchStart:min(batchStart+batchSize, len(entries))]

		var ids []uuid.UUID
		byID := make(map[uuid.UUID]os.DirEntry, len(batch))
		for _, e := range batch {
			if !e.IsDir() || e.Name() == "templates" {
				continue
			}
			id, err := uuid.Parse(e.Name())
			if err != nil {
				continue // not an event directory; leave it alone
			}
			ids = append(ids, id)
			byID[id] = e
		}
		if len(ids) == 0 {
			continue
		}

		exists, err := eventExists(ctx, ids)
		if err != nil {
			return fmt.Errorf("media: reconcile check events: %w", err)
		}

		for _, id := range ids {
			eventDir := filepath.Join(areaRoot, id.String())
			if !exists[id] {
				if olderThan(eventDir, orphanAge, now) {
					_ = os.RemoveAll(eventDir)
				}
				continue
			}
			if err := s.reconcileEventMedia(ctx, eventDir, id, eventMedia, now); err != nil {
				return err
			}
		}
	}
	return nil
}

func (s *Store) reconcileEventMedia(ctx context.Context, eventDir string, eventID uuid.UUID, eventMedia EventMediaFunc, now time.Time) error {
	mediaEntries, err := os.ReadDir(eventDir)
	if err != nil {
		if os.IsNotExist(err) {
			return nil
		}
		return fmt.Errorf("media: reconcile list event dir: %w", err)
	}
	if len(mediaEntries) == 0 {
		return nil
	}

	known, err := eventMedia(ctx, eventID)
	if err != nil {
		return fmt.Errorf("media: reconcile check media: %w", err)
	}
	for _, e := range mediaEntries {
		if !e.IsDir() {
			continue
		}
		id, err := uuid.Parse(e.Name())
		if err != nil {
			continue
		}
		if known[id] {
			continue
		}
		dir := filepath.Join(eventDir, e.Name())
		if olderThan(dir, orphanAge, now) {
			_ = os.RemoveAll(dir)
		}
	}
	return nil
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
