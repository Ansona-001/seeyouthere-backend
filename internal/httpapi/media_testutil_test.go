package httpapi

import (
	"context"
	"io"
	"sync"
	"testing"

	"github.com/ansonarose/seeyouthere-backend/internal/config"
	"github.com/ansonarose/seeyouthere-backend/internal/media"
)

// Caps used by test fixtures: generous enough that only the tests that lower
// them hit a quota.
const (
	testUserQuotaBytes  = 200_000_000
	testTotalQuotaBytes = 9_000_000_000
)

// testConfig is the config shared by test servers that upload or serve media.
func testConfig() config.Config {
	return config.Config{
		SiteURL:              "https://seeyouthere.at",
		MediaUserQuotaBytes:  testUserQuotaBytes,
		MediaTotalQuotaBytes: testTotalQuotaBytes,
	}
}

// testBlobs wraps a real (disk) media.Blobs, counting calls and injecting
// failures, so tests can assert that a request made no storage call at all.
type testBlobs struct {
	inner media.Blobs

	mu                         sync.Mutex
	gets, puts, deletes, lists int
	getErr, putErr, deleteErr  error
	putErrAfter                int // fail Put once this many have succeeded (when putErr is set)
	putOK                      int
	deletedKeys                []string
}

func (b *testBlobs) Put(ctx context.Context, key string, body io.ReadSeeker, size int64) error {
	b.mu.Lock()
	b.puts++
	fail := b.putErr != nil && b.putOK >= b.putErrAfter
	if !fail {
		b.putOK++
	}
	err := b.putErr
	b.mu.Unlock()
	if fail {
		return err
	}
	return b.inner.Put(ctx, key, body, size)
}

func (b *testBlobs) Get(ctx context.Context, key, ifNoneMatch string) (*media.Object, error) {
	b.mu.Lock()
	b.gets++
	err := b.getErr
	b.mu.Unlock()
	if err != nil {
		return nil, err
	}
	return b.inner.Get(ctx, key, ifNoneMatch)
}

func (b *testBlobs) Delete(ctx context.Context, keys []string) error {
	b.mu.Lock()
	b.deletes++
	err := b.deleteErr
	if err == nil {
		b.deletedKeys = append(b.deletedKeys, keys...)
	}
	b.mu.Unlock()
	if err != nil {
		return err
	}
	return b.inner.Delete(ctx, keys)
}

func (b *testBlobs) List(ctx context.Context, prefix string, fn func([]media.ObjectInfo) error) error {
	b.mu.Lock()
	b.lists++
	b.mu.Unlock()
	return b.inner.List(ctx, prefix, fn)
}

// calls returns the total number of backend calls so far.
func (b *testBlobs) calls() int {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.gets + b.puts + b.deletes + b.lists
}

// newTestMediaStoreBlobs builds a media.Store on a temp dir whose objects live
// on disk under it, behind a counting/failure-injecting wrapper.
func newTestMediaStoreBlobs(t *testing.T) (*media.Store, *testBlobs) {
	t.Helper()
	root := t.TempDir()
	disk, err := media.NewDiskBlobs(root + "/objects")
	if err != nil {
		t.Fatalf("media.NewDiskBlobs: %v", err)
	}
	t.Cleanup(func() { _ = disk.Close() })
	blobs := &testBlobs{inner: disk}
	st, err := media.NewStore(root, blobs)
	if err != nil {
		t.Fatalf("media.NewStore: %v", err)
	}
	return st, blobs
}

// newTestMediaStore is newTestMediaStoreBlobs for callers that don't need the
// wrapper.
func newTestMediaStore(t *testing.T) *media.Store {
	t.Helper()
	st, _ := newTestMediaStoreBlobs(t)
	return st
}
