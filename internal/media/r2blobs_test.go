package media

import (
	"bytes"
	"context"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
)

const testSecret = "super-secret-value"

func newR2TestBlobs(t *testing.T, h http.HandlerFunc) (Blobs, *atomic.Int32) {
	t.Helper()
	var calls atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls.Add(1)
		h(w, r)
	}))
	t.Cleanup(srv.Close)
	b, err := NewR2Blobs(srv.URL, "test-bucket", "AKID", testSecret)
	if err != nil {
		t.Fatalf("NewR2Blobs: %v", err)
	}
	return b, &calls
}

func TestNewR2Blobs_Invalid(t *testing.T) {
	cases := map[string][4]string{
		"bad endpoint": {"ftp://x", "bucket", "a", testSecret},
		"no bucket":    {"https://x.example", "", "a", testSecret},
		"no secret":    {"https://x.example", "bucket", "a", ""},
	}
	for name, c := range cases {
		t.Run(name, func(t *testing.T) {
			_, err := NewR2Blobs(c[0], c[1], c[2], c[3])
			if err == nil {
				t.Fatal("expected error")
			}
			if strings.Contains(err.Error(), testSecret) {
				t.Errorf("error leaks secret: %v", err)
			}
		})
	}
}

func TestR2Blobs_InvalidKeysNeverReachBackend(t *testing.T) {
	b, calls := newR2TestBlobs(t, func(w http.ResponseWriter, r *http.Request) {})
	ctx := context.Background()
	bad := []string{"", "../x", "A/b", "/abs", "a//b", "a b"}
	for _, k := range bad {
		if err := b.Put(ctx, k, bytes.NewReader([]byte("x")), 1); err == nil {
			t.Errorf("Put(%q) succeeded", k)
		}
		if _, err := b.Get(ctx, k, ""); err == nil {
			t.Errorf("Get(%q) succeeded", k)
		}
		if err := b.Delete(ctx, []string{"ok/key", k}); err == nil {
			t.Errorf("Delete(%q) succeeded", k)
		}
	}
	if err := b.List(ctx, "../x", func([]ObjectInfo) error { return nil }); err == nil {
		t.Error("List with bad prefix succeeded")
	}
	if n := calls.Load(); n != 0 {
		t.Errorf("backend called %d times for invalid keys", n)
	}
}

func TestR2Blobs_PutGet(t *testing.T) {
	var stored []byte
	b, _ := newR2TestBlobs(t, func(w http.ResponseWriter, r *http.Request) {
		switch r.Method {
		case http.MethodPut:
			stored, _ = io.ReadAll(r.Body)
			w.WriteHeader(http.StatusOK)
		case http.MethodGet:
			w.Header().Set("ETag", `"abc"`)
			_, _ = w.Write(stored)
		}
	})
	ctx := context.Background()
	if err := b.Put(ctx, "m/a/480.jpg", bytes.NewReader([]byte("jpegbytes")), 9); err != nil {
		t.Fatalf("Put: %v", err)
	}
	o, err := b.Get(ctx, "m/a/480.jpg", "")
	if err != nil {
		t.Fatalf("Get: %v", err)
	}
	defer o.Body.Close()
	got, _ := io.ReadAll(o.Body)
	if string(got) != "jpegbytes" || o.Size != 9 || o.ETag != `"abc"` {
		t.Errorf("got body=%q size=%d etag=%q", got, o.Size, o.ETag)
	}
}

func TestR2Blobs_GetErrors(t *testing.T) {
	cases := []struct {
		name   string
		status int
		want   error
	}{
		{"not found", http.StatusNotFound, ErrNotFound},
		{"not modified", http.StatusNotModified, ErrNotModified},
		{"unavailable", http.StatusServiceUnavailable, ErrStorageUnavailable},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			b, _ := newR2TestBlobs(t, func(w http.ResponseWriter, r *http.Request) {
				w.WriteHeader(tc.status)
			})
			_, err := b.Get(context.Background(), "m/a/480.jpg", `"abc"`)
			if !errors.Is(err, tc.want) {
				t.Fatalf("err = %v, want %v", err, tc.want)
			}
			if strings.Contains(err.Error(), testSecret) {
				t.Errorf("error leaks secret: %v", err)
			}
		})
	}
}

func TestR2Blobs_List(t *testing.T) {
	b, _ := newR2TestBlobs(t, func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Query().Get("prefix") != "m/" {
			t.Errorf("prefix = %q", r.URL.Query().Get("prefix"))
		}
		w.Header().Set("Content-Type", "application/xml")
		_, _ = w.Write([]byte(`<?xml version="1.0" encoding="UTF-8"?>
<ListBucketResult><IsTruncated>false</IsTruncated>
<Contents><Key>m/a.jpg</Key><LastModified>2026-01-02T03:04:05.000Z</LastModified><Size>12</Size></Contents>
<Contents><Key>m/b.jpg</Key><LastModified>2026-01-02T03:04:06.000Z</LastModified><Size>34</Size></Contents>
</ListBucketResult>`))
	})
	var got []ObjectInfo
	err := b.List(context.Background(), "m/", func(p []ObjectInfo) error {
		got = append(got, p...)
		return nil
	})
	if err != nil {
		t.Fatalf("List: %v", err)
	}
	if len(got) != 2 || got[0].Key != "m/a.jpg" || got[0].Size != 12 || got[1].Key != "m/b.jpg" || got[1].Size != 34 || got[0].LastModified.IsZero() {
		t.Errorf("got %+v", got)
	}

	sentinel := errors.New("stop")
	err = b.List(context.Background(), "m/", func([]ObjectInfo) error { return sentinel })
	if !errors.Is(err, sentinel) {
		t.Errorf("callback error not propagated: %v", err)
	}
}
