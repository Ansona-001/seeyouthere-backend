package r2

import (
	"bytes"
	"context"
	"errors"
	"io"
	"os"
	"testing"
	"time"

	"github.com/google/uuid"
)

// TestIntegration runs against a real bucket. It is skipped unless
// R2_TEST_ENDPOINT, R2_TEST_BUCKET, R2_TEST_ACCESS_KEY_ID and
// R2_TEST_SECRET_ACCESS_KEY are all set. Objects live under test/<uuid>/ and
// are removed afterwards.
func TestIntegration(t *testing.T) {
	endpoint, bucket := os.Getenv("R2_TEST_ENDPOINT"), os.Getenv("R2_TEST_BUCKET")
	ak, secret := os.Getenv("R2_TEST_ACCESS_KEY_ID"), os.Getenv("R2_TEST_SECRET_ACCESS_KEY")
	if endpoint == "" || bucket == "" || ak == "" || secret == "" {
		t.Skip("R2_TEST_* not set")
	}
	c, err := New(endpoint, bucket, ak, secret)
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
	defer cancel()

	prefix := "test/" + uuid.NewString() + "/"
	keys := []string{prefix + "a.jpg", prefix + "b.jpg"}
	t.Cleanup(func() {
		cctx, ccancel := context.WithTimeout(context.Background(), 30*time.Second)
		defer ccancel()
		if err := c.Delete(cctx, keys); err != nil {
			t.Errorf("cleanup: %v", err)
		}
	})

	data := []byte("integration test payload")
	for _, k := range keys {
		if err := c.Put(ctx, k, bytes.NewReader(data), int64(len(data))); err != nil {
			t.Fatalf("put %s: %v", k, err)
		}
	}

	obj, err := c.Get(ctx, keys[0], "")
	if err != nil {
		t.Fatal(err)
	}
	got, err := io.ReadAll(obj.Body)
	_ = obj.Body.Close()
	if err != nil || !bytes.Equal(got, data) || obj.Size != int64(len(data)) || obj.ETag == "" {
		t.Fatalf("get: %q size=%d etag=%q err=%v", got, obj.Size, obj.ETag, err)
	}
	if _, err := c.Get(ctx, keys[0], obj.ETag); !errors.Is(err, ErrNotModified) {
		t.Fatalf("want ErrNotModified, got %v", err)
	}

	var listed []string
	if err := c.List(ctx, prefix, func(p []ObjectInfo) error {
		for _, o := range p {
			listed = append(listed, o.Key)
		}
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	if len(listed) != 2 || listed[0] != keys[0] || listed[1] != keys[1] {
		t.Fatalf("list: %v", listed)
	}

	if err := c.Delete(ctx, append(keys, prefix+"missing.jpg")); err != nil {
		t.Fatalf("delete: %v", err)
	}
	if _, err := c.Get(ctx, keys[0], ""); !errors.Is(err, ErrNotFound) {
		t.Fatalf("want ErrNotFound, got %v", err)
	}
}
