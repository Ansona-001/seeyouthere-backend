package media

import (
	"context"
	"errors"
	"fmt"
	"io"

	"github.com/ansonarose/seeyouthere-backend/internal/media/r2"
)

// r2Blobs adapts *r2.Client (which deliberately knows nothing about this
// package) to Blobs: it validates keys up front and maps r2's sentinel errors
// onto this package's, so callers only ever test media.Err*.
type r2Blobs struct{ c *r2.Client }

// NewR2Blobs returns Blobs backed by a Cloudflare R2 bucket.
func NewR2Blobs(endpoint, bucket, accessKeyID, secret string) (Blobs, error) {
	c, err := r2.New(endpoint, bucket, accessKeyID, secret)
	if err != nil {
		return nil, fmt.Errorf("media: r2 client: %w", err)
	}
	return &r2Blobs{c: c}, nil
}

// mapR2Err wraps err so errors.Is matches the media sentinel as well as the
// original r2 one.
func mapR2Err(err error) error {
	switch {
	case err == nil:
		return nil
	case errors.Is(err, r2.ErrNotFound):
		return fmt.Errorf("%w: %w", ErrNotFound, err)
	case errors.Is(err, r2.ErrNotModified):
		return fmt.Errorf("%w: %w", ErrNotModified, err)
	case errors.Is(err, r2.ErrStorageUnavailable):
		return fmt.Errorf("%w: %w", ErrStorageUnavailable, err)
	}
	return err
}

func (b *r2Blobs) Put(ctx context.Context, key string, body io.ReadSeeker, size int64) error {
	if !ValidKey(key) {
		return errInvalidKey(key)
	}
	return mapR2Err(b.c.Put(ctx, key, body, size))
}

func (b *r2Blobs) Get(ctx context.Context, key, ifNoneMatch string) (*Object, error) {
	if !ValidKey(key) {
		return nil, errInvalidKey(key)
	}
	o, err := b.c.Get(ctx, key, ifNoneMatch)
	if err != nil {
		return nil, mapR2Err(err)
	}
	return &Object{Body: o.Body, Size: o.Size, ETag: o.ETag}, nil
}

func (b *r2Blobs) Delete(ctx context.Context, keys []string) error {
	for _, k := range keys {
		if !ValidKey(k) {
			return errInvalidKey(k)
		}
	}
	return mapR2Err(b.c.Delete(ctx, keys))
}

func (b *r2Blobs) List(ctx context.Context, prefix string, fn func([]ObjectInfo) error) error {
	if !ValidPrefix(prefix) {
		return errInvalidKey(prefix)
	}
	err := b.c.List(ctx, prefix, func(page []r2.ObjectInfo) error {
		out := make([]ObjectInfo, len(page))
		for i, o := range page {
			out[i] = ObjectInfo{Key: o.Key, Size: o.Size, LastModified: o.LastModified}
		}
		return fn(out)
	})
	return mapR2Err(err)
}
