package r2

import (
	"bytes"
	"context"
	"crypto/md5" //nolint:gosec // verifying the Content-MD5 header
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

const testSecret = "s3cr3t-do-not-leak-0123456789"

func TestSignAWSVector(t *testing.T) {
	req, err := http.NewRequest(http.MethodGet, "https://examplebucket.s3.amazonaws.com/test.txt", nil)
	if err != nil {
		t.Fatal(err)
	}
	req.Header.Set("Range", "bytes=0-9")
	ts := time.Date(2013, 5, 24, 0, 0, 0, 0, time.UTC)
	sign(req, emptyPayloadSHA, "AKIAIOSFODNN7EXAMPLE", "wJalrXUtnFEMI/K7MDENG/bPxRfiCYEXAMPLEKEY", "us-east-1", "s3", ts)

	want := "AWS4-HMAC-SHA256 Credential=AKIAIOSFODNN7EXAMPLE/20130524/us-east-1/s3/aws4_request," +
		" SignedHeaders=host;range;x-amz-content-sha256;x-amz-date," +
		" Signature=f0e8bdb87c964420e857bd35b5d6ed310bd44f0170aba48dd91039c6036bdb41"
	if got := req.Header.Get("Authorization"); got != want {
		t.Fatalf("Authorization\n got %s\nwant %s", got, want)
	}
}

func TestUriEncode(t *testing.T) {
	tests := []struct {
		in    string
		slash bool
		want  string
	}{
		{"a/b c", false, "a/b%20c"},
		{"a/b", true, "a%2Fb"},
		{"x+y=z~", true, "x%2By%3Dz~"},
		{"", true, ""},
	}
	for _, tt := range tests {
		if got := uriEncode(tt.in, tt.slash); got != tt.want {
			t.Errorf("uriEncode(%q,%v)=%q want %q", tt.in, tt.slash, got, tt.want)
		}
	}
}

func TestValidKey(t *testing.T) {
	tests := []struct {
		key string
		ok  bool
	}{
		{"e/0190/0191/480.jpg", true},
		{"t/a/1/1920.jpg", true},
		{"a", true},
		{strings.Repeat("a", 201), true},
		{strings.Repeat("a", 202), false},
		{"", false},
		{"/abs", false},
		{"-a", false},
		{".a", false},
		{"A/b", false},
		{"a/../b", false},
		{"a..b", false},
		{"a//b", false},
		{"a b", false},
		{"a?b", false},
		{"a%2e", false},
		{"é", false},
		{"a\x00", false},
	}
	for _, tt := range tests {
		if got := validKey(tt.key); got != tt.ok {
			t.Errorf("validKey(%q)=%v want %v", tt.key, got, tt.ok)
		}
	}
}

func newTestClient(t *testing.T, h http.HandlerFunc) (*Client, *httptest.Server) {
	t.Helper()
	srv := httptest.NewServer(h)
	t.Cleanup(srv.Close)
	c, err := New(srv.URL, "bkt", "AKIDTEST", testSecret)
	if err != nil {
		t.Fatal(err)
	}
	c.backoff = [maxAttempts - 1]time.Duration{}
	return c, srv
}

func TestNewValidation(t *testing.T) {
	tests := []struct {
		name, endpoint, bucket, ak, secret string
	}{
		{"bad scheme", "ftp://x.example", "b", "a", "s"},
		{"no host", "https://", "b", "a", "s"},
		{"path", "https://x.example/foo", "b", "a", "s"},
		{"query", "https://x.example?a=b", "b", "a", "s"},
		{"userinfo", "https://u:p@x.example", "b", "a", "s"},
		{"no bucket", "https://x.example", "", "a", "s"},
		{"bucket slash", "https://x.example", "a/b", "a", "s"},
		{"no secret", "https://x.example", "b", "a", ""},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if _, err := New(tt.endpoint, tt.bucket, tt.ak, tt.secret); err == nil {
				t.Fatal("expected error")
			}
		})
	}
	if _, err := New("https://x.example/", "b", "a", "s"); err != nil {
		t.Fatalf("trailing slash should be fine: %v", err)
	}
}

func TestPut(t *testing.T) {
	data := []byte("hello jpeg bytes")
	sum := sha256.Sum256(data)
	var calls atomic.Int32
	c, _ := newTestClient(t, func(w http.ResponseWriter, r *http.Request) {
		calls.Add(1)
		got, _ := io.ReadAll(r.Body)
		switch {
		case r.Method != http.MethodPut || r.URL.Path != "/bkt/e/a/b/480.jpg":
			t.Errorf("unexpected %s %s", r.Method, r.URL.Path)
		case !bytes.Equal(got, data):
			t.Errorf("body mismatch")
		case r.Header.Get("X-Amz-Content-Sha256") != hex.EncodeToString(sum[:]):
			t.Errorf("payload hash header wrong")
		case r.Header.Get("Content-Type") != "image/jpeg":
			t.Errorf("content type %q", r.Header.Get("Content-Type"))
		case r.ContentLength != int64(len(data)):
			t.Errorf("content length %d", r.ContentLength)
		case !strings.Contains(r.Header.Get("Authorization"), "SignedHeaders=content-type;host;x-amz-content-sha256;x-amz-date"):
			t.Errorf("authorization %q", r.Header.Get("Authorization"))
		case !strings.Contains(r.Header.Get("Authorization"), "/auto/s3/aws4_request"):
			t.Errorf("scope wrong: %q", r.Header.Get("Authorization"))
		}
	})
	if err := c.Put(context.Background(), "e/a/b/480.jpg", bytes.NewReader(data), int64(len(data))); err != nil {
		t.Fatal(err)
	}
	if calls.Load() != 1 {
		t.Fatalf("calls=%d", calls.Load())
	}
}

func TestPutErrors(t *testing.T) {
	ok := func(http.ResponseWriter, *http.Request) {}
	c, _ := newTestClient(t, ok)
	tests := []struct {
		name string
		key  string
		body io.ReadSeeker
		size int64
	}{
		{"bad key", "../x", strings.NewReader("abc"), 3},
		{"short body", "a/b", strings.NewReader("abc"), 10},
		{"negative", "a/b", strings.NewReader("abc"), -1},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if err := c.Put(context.Background(), tt.key, tt.body, tt.size); err == nil {
				t.Fatal("expected error")
			}
		})
	}
}

func TestPutRetriesRewindBody(t *testing.T) {
	data := []byte("retry me")
	var calls atomic.Int32
	c, _ := newTestClient(t, func(w http.ResponseWriter, r *http.Request) {
		got, _ := io.ReadAll(r.Body)
		if !bytes.Equal(got, data) {
			t.Errorf("attempt body %q", got)
		}
		if calls.Add(1) == 1 {
			w.WriteHeader(http.StatusInternalServerError)
		}
	})
	if err := c.Put(context.Background(), "a/b", bytes.NewReader(data), int64(len(data))); err != nil {
		t.Fatal(err)
	}
	if calls.Load() != 2 {
		t.Fatalf("calls=%d", calls.Load())
	}
}

func TestGet(t *testing.T) {
	c, _ := newTestClient(t, func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/bkt/a/ok":
			if r.Header.Get("If-None-Match") == `"etag1"` {
				w.Header().Set("ETag", `"etag1"`)
				w.WriteHeader(http.StatusNotModified)
				return
			}
			w.Header().Set("ETag", `"etag1"`)
			_, _ = w.Write([]byte("payload"))
		case "/bkt/a/missing":
			w.WriteHeader(http.StatusNotFound)
			_, _ = io.WriteString(w, `<Error><Code>NoSuchKey</Code></Error>`)
		case "/bkt/a/denied":
			w.WriteHeader(http.StatusForbidden)
			_, _ = io.WriteString(w, `<Error><Code>AccessDenied</Code><Message>x</Message></Error>`)
		default:
			w.WriteHeader(http.StatusBadRequest)
		}
	})
	ctx := context.Background()

	obj, err := c.Get(ctx, "a/ok", "")
	if err != nil {
		t.Fatal(err)
	}
	b, _ := io.ReadAll(obj.Body)
	_ = obj.Body.Close()
	if string(b) != "payload" || obj.Size != 7 || obj.ETag != `"etag1"` {
		t.Fatalf("got %q size=%d etag=%q", b, obj.Size, obj.ETag)
	}

	if _, err := c.Get(ctx, "a/ok", `"etag1"`); !errors.Is(err, ErrNotModified) {
		t.Fatalf("want ErrNotModified, got %v", err)
	}
	if _, err := c.Get(ctx, "a/missing", ""); !errors.Is(err, ErrNotFound) {
		t.Fatalf("want ErrNotFound, got %v", err)
	}
	_, err = c.Get(ctx, "a/denied", "")
	if err == nil || errors.Is(err, ErrNotFound) || errors.Is(err, ErrStorageUnavailable) {
		t.Fatalf("want plain error, got %v", err)
	}
	for _, want := range []string{"403", "AccessDenied", "a/denied", "get"} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("error %q lacks %q", err, want)
		}
	}
	if _, err := c.Get(ctx, "A/BAD", ""); err == nil {
		t.Fatal("invalid key accepted")
	}
}

func TestRetries(t *testing.T) {
	t.Run("500 then success", func(t *testing.T) {
		var calls atomic.Int32
		c, _ := newTestClient(t, func(w http.ResponseWriter, r *http.Request) {
			if calls.Add(1) == 1 {
				w.WriteHeader(http.StatusInternalServerError)
				return
			}
			_, _ = w.Write([]byte("x"))
		})
		obj, err := c.Get(context.Background(), "a/b", "")
		if err != nil {
			t.Fatal(err)
		}
		_ = obj.Body.Close()
		if calls.Load() != 2 {
			t.Fatalf("calls=%d", calls.Load())
		}
	})
	t.Run("429 retried", func(t *testing.T) {
		var calls atomic.Int32
		c, _ := newTestClient(t, func(w http.ResponseWriter, r *http.Request) {
			if calls.Add(1) < 3 {
				w.WriteHeader(http.StatusTooManyRequests)
				return
			}
			_, _ = w.Write([]byte("x"))
		})
		obj, err := c.Get(context.Background(), "a/b", "")
		if err != nil {
			t.Fatal(err)
		}
		_ = obj.Body.Close()
		if calls.Load() != 3 {
			t.Fatalf("calls=%d", calls.Load())
		}
	})
	t.Run("503 x3 unavailable", func(t *testing.T) {
		var calls atomic.Int32
		c, _ := newTestClient(t, func(w http.ResponseWriter, r *http.Request) {
			calls.Add(1)
			w.WriteHeader(http.StatusServiceUnavailable)
		})
		_, err := c.Get(context.Background(), "a/b", "")
		if !errors.Is(err, ErrStorageUnavailable) {
			t.Fatalf("got %v", err)
		}
		if calls.Load() != maxAttempts {
			t.Fatalf("calls=%d", calls.Load())
		}
	})
	t.Run("4xx not retried", func(t *testing.T) {
		var calls atomic.Int32
		c, _ := newTestClient(t, func(w http.ResponseWriter, r *http.Request) {
			calls.Add(1)
			w.WriteHeader(http.StatusBadRequest)
		})
		if _, err := c.Get(context.Background(), "a/b", ""); err == nil || errors.Is(err, ErrStorageUnavailable) {
			t.Fatalf("got %v", err)
		}
		if calls.Load() != 1 {
			t.Fatalf("calls=%d", calls.Load())
		}
	})
	t.Run("network error unavailable", func(t *testing.T) {
		c, srv := newTestClient(t, func(http.ResponseWriter, *http.Request) {})
		srv.Close()
		_, err := c.Get(context.Background(), "a/b", "")
		if !errors.Is(err, ErrStorageUnavailable) {
			t.Fatalf("got %v", err)
		}
		if strings.Contains(err.Error(), testSecret) || strings.Contains(err.Error(), srv.URL) {
			t.Fatalf("error leaks details: %v", err)
		}
	})
	t.Run("cancelled ctx not retried", func(t *testing.T) {
		var calls atomic.Int32
		c, _ := newTestClient(t, func(w http.ResponseWriter, r *http.Request) {
			calls.Add(1)
			w.WriteHeader(http.StatusInternalServerError)
		})
		ctx, cancel := context.WithCancel(context.Background())
		cancel()
		_, err := c.Get(ctx, "a/b", "")
		if !errors.Is(err, context.Canceled) || errors.Is(err, ErrStorageUnavailable) {
			t.Fatalf("got %v", err)
		}
		if calls.Load() != 0 {
			t.Fatalf("calls=%d", calls.Load())
		}
	})
	t.Run("attempt timeout retried", func(t *testing.T) {
		var calls atomic.Int32
		c, _ := newTestClient(t, func(w http.ResponseWriter, r *http.Request) {
			if calls.Add(1) == 1 {
				<-r.Context().Done()
				return
			}
			_, _ = w.Write([]byte("x"))
		})
		c.timeout = 100 * time.Millisecond
		obj, err := c.Get(context.Background(), "a/b", "")
		if err != nil {
			t.Fatal(err)
		}
		_ = obj.Body.Close()
	})
}

func TestRedirectNotFollowed(t *testing.T) {
	var hits atomic.Int32
	target := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		hits.Add(1)
	}))
	defer target.Close()
	c, _ := newTestClient(t, func(w http.ResponseWriter, r *http.Request) {
		http.Redirect(w, r, target.URL+"/evil", http.StatusFound)
	})
	_, err := c.Get(context.Background(), "a/b", "")
	if err == nil || !strings.Contains(err.Error(), "302") {
		t.Fatalf("want 302 error, got %v", err)
	}
	if hits.Load() != 0 {
		t.Fatal("redirect was followed")
	}
}

func TestOversizedErrorBodyTruncated(t *testing.T) {
	c, _ := newTestClient(t, func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusBadRequest)
		_, _ = io.WriteString(w, strings.Repeat("A", 5<<20))
	})
	_, err := c.Get(context.Background(), "a/b", "")
	if err == nil {
		t.Fatal("expected error")
	}
	if len(err.Error()) > 300 || strings.Contains(err.Error(), "AAAA") {
		t.Fatalf("error not bounded: %d bytes", len(err.Error()))
	}
}

func TestErrorsNeverContainSecretOrHeaders(t *testing.T) {
	statuses := []int{http.StatusBadRequest, http.StatusUnauthorized, http.StatusForbidden, http.StatusTeapot, http.StatusNotFound, http.StatusBadGateway}
	for _, st := range statuses {
		t.Run(fmt.Sprint(st), func(t *testing.T) {
			c, _ := newTestClient(t, func(w http.ResponseWriter, r *http.Request) {
				// A hostile or buggy server echoing the credentials back.
				w.Header().Set("X-Echo", r.Header.Get("Authorization"))
				w.WriteHeader(st)
				_, _ = io.WriteString(w, "<Error><Code>"+testSecret+"</Code><Message>"+r.Header.Get("Authorization")+"</Message></Error>")
			})
			ctx := context.Background()
			errs := []error{c.Put(ctx, "a/b", strings.NewReader("x"), 1), c.Delete(ctx, []string{"a/b"}),
				c.List(ctx, "a/", func([]ObjectInfo) error { return nil })}
			_, gerr := c.Get(ctx, "a/b", "")
			errs = append(errs, gerr)
			for _, err := range errs {
				if err == nil {
					t.Fatal("expected error")
				}
				for _, bad := range []string{testSecret, "AWS4-HMAC", "Signature=", "AKIDTEST"} {
					if strings.Contains(err.Error(), bad) {
						t.Fatalf("error %q contains %q", err, bad)
					}
				}
			}
		})
	}
}

func TestDelete(t *testing.T) {
	t.Run("headers and body", func(t *testing.T) {
		var gotBody []byte
		c, _ := newTestClient(t, func(w http.ResponseWriter, r *http.Request) {
			gotBody, _ = io.ReadAll(r.Body)
			md := md5.Sum(gotBody) //nolint:gosec // test
			sum := sha256.Sum256(gotBody)
			switch {
			case r.Method != http.MethodPost || r.URL.Path != "/bkt" || r.URL.RawQuery != "delete=":
				t.Errorf("unexpected %s %s?%s", r.Method, r.URL.Path, r.URL.RawQuery)
			case r.Header.Get("Content-MD5") != base64.StdEncoding.EncodeToString(md[:]):
				t.Errorf("bad Content-MD5 %q", r.Header.Get("Content-MD5"))
			case r.Header.Get("X-Amz-Content-Sha256") != hex.EncodeToString(sum[:]):
				t.Errorf("bad payload hash")
			}
			_, _ = io.WriteString(w, `<DeleteResult xmlns="http://s3.amazonaws.com/doc/2006-03-01/"></DeleteResult>`)
		})
		if err := c.Delete(context.Background(), []string{"a/1", "a/2"}); err != nil {
			t.Fatal(err)
		}
		want := `<Delete><Quiet>true</Quiet><Object><Key>a/1</Key></Object><Object><Key>a/2</Key></Object></Delete>`
		if string(gotBody) != want {
			t.Fatalf("body %s", gotBody)
		}
	})
	t.Run("per-key error in 200", func(t *testing.T) {
		var md5Seen atomic.Bool
		c, _ := newTestClient(t, func(w http.ResponseWriter, r *http.Request) {
			md5Seen.Store(r.Header.Get("Content-MD5") != "")
			_, _ = io.WriteString(w, `<DeleteResult><Error><Key>a/2</Key><Code>InternalError</Code><Message>boom</Message></Error></DeleteResult>`)
		})
		err := c.Delete(context.Background(), []string{"a/1", "a/2"})
		if err == nil || !strings.Contains(err.Error(), "InternalError") {
			t.Fatalf("got %v", err)
		}
		if !md5Seen.Load() {
			t.Fatal("Content-MD5 missing")
		}
	})
	t.Run("NoSuchKey ignored", func(t *testing.T) {
		c, _ := newTestClient(t, func(w http.ResponseWriter, r *http.Request) {
			_, _ = io.WriteString(w, `<DeleteResult><Error><Key>a/1</Key><Code>NoSuchKey</Code></Error></DeleteResult>`)
		})
		if err := c.Delete(context.Background(), []string{"a/1"}); err != nil {
			t.Fatal(err)
		}
	})
	t.Run("chunks of 1000", func(t *testing.T) {
		var sizes []int
		c, _ := newTestClient(t, func(w http.ResponseWriter, r *http.Request) {
			b, _ := io.ReadAll(r.Body)
			sizes = append(sizes, strings.Count(string(b), "<Object>"))
			_, _ = io.WriteString(w, `<DeleteResult></DeleteResult>`)
		})
		keys := make([]string, 2500)
		for i := range keys {
			keys[i] = fmt.Sprintf("a/%d", i)
		}
		if err := c.Delete(context.Background(), keys); err != nil {
			t.Fatal(err)
		}
		if fmt.Sprint(sizes) != "[1000 1000 500]" {
			t.Fatalf("sizes %v", sizes)
		}
	})
	t.Run("empty and invalid", func(t *testing.T) {
		var calls atomic.Int32
		c, _ := newTestClient(t, func(w http.ResponseWriter, r *http.Request) { calls.Add(1) })
		if err := c.Delete(context.Background(), nil); err != nil {
			t.Fatal(err)
		}
		if err := c.Delete(context.Background(), []string{"a/1", "../x"}); err == nil {
			t.Fatal("expected error for invalid key")
		}
		if calls.Load() != 0 {
			t.Fatal("request sent")
		}
	})
	t.Run("malformed response", func(t *testing.T) {
		c, _ := newTestClient(t, func(w http.ResponseWriter, r *http.Request) { _, _ = io.WriteString(w, "<DeleteResult") })
		if err := c.Delete(context.Background(), []string{"a/1"}); err == nil {
			t.Fatal("expected error")
		}
	})
}

func TestList(t *testing.T) {
	page := func(w http.ResponseWriter, keys []string, next string) {
		var b strings.Builder
		b.WriteString(`<ListBucketResult xmlns="http://s3.amazonaws.com/doc/2006-03-01/">`)
		fmt.Fprintf(&b, "<IsTruncated>%v</IsTruncated>", next != "")
		if next != "" {
			fmt.Fprintf(&b, "<NextContinuationToken>%s</NextContinuationToken>", next)
		}
		for _, k := range keys {
			fmt.Fprintf(&b, "<Contents><Key>%s</Key><LastModified>2024-05-01T10:20:30.000Z</LastModified><Size>42</Size></Contents>", k)
		}
		b.WriteString("</ListBucketResult>")
		_, _ = io.WriteString(w, b.String())
	}
	t.Run("pagination", func(t *testing.T) {
		tokens := []string{}
		c, _ := newTestClient(t, func(w http.ResponseWriter, r *http.Request) {
			q := r.URL.Query()
			if q.Get("list-type") != "2" || q.Get("prefix") != "e/x/" || q.Get("max-keys") != "1000" || r.URL.Path != "/bkt" {
				t.Errorf("bad query %s %s", r.URL.Path, r.URL.RawQuery)
			}
			tok := q.Get("continuation-token")
			tokens = append(tokens, tok)
			switch tok {
			case "":
				page(w, []string{"e/x/1", "e/x/2"}, "tok+/=1")
			case "tok+/=1":
				page(w, []string{"e/x/3"}, "tok2")
			default:
				page(w, nil, "")
			}
		})
		var got []ObjectInfo
		pages := 0
		err := c.List(context.Background(), "e/x/", func(p []ObjectInfo) error {
			pages++
			got = append(got, p...)
			return nil
		})
		if err != nil {
			t.Fatal(err)
		}
		if pages != 2 || len(got) != 3 || got[2].Key != "e/x/3" || got[0].Size != 42 ||
			!got[0].LastModified.Equal(time.Date(2024, 5, 1, 10, 20, 30, 0, time.UTC)) {
			t.Fatalf("pages=%d got=%+v", pages, got)
		}
		if fmt.Sprint(tokens) != "[ tok+/=1 tok2]" {
			t.Fatalf("tokens %v", tokens)
		}
	})
	t.Run("fn error stops", func(t *testing.T) {
		var calls atomic.Int32
		c, _ := newTestClient(t, func(w http.ResponseWriter, r *http.Request) {
			calls.Add(1)
			page(w, []string{"a/1"}, "more")
		})
		stop := errors.New("stop")
		if err := c.List(context.Background(), "a/", func([]ObjectInfo) error { return stop }); !errors.Is(err, stop) {
			t.Fatalf("got %v", err)
		}
		if calls.Load() != 1 {
			t.Fatalf("calls=%d", calls.Load())
		}
	})
	t.Run("repeated token", func(t *testing.T) {
		c, _ := newTestClient(t, func(w http.ResponseWriter, r *http.Request) { page(w, []string{"a/1"}, "same") })
		err := c.List(context.Background(), "a/", func([]ObjectInfo) error { return nil })
		if err == nil || !strings.Contains(err.Error(), "continuation") {
			t.Fatalf("got %v", err)
		}
	})
	t.Run("truncated without token", func(t *testing.T) {
		c, _ := newTestClient(t, func(w http.ResponseWriter, r *http.Request) {
			_, _ = io.WriteString(w, `<ListBucketResult><IsTruncated>true</IsTruncated></ListBucketResult>`)
		})
		if err := c.List(context.Background(), "a/", func([]ObjectInfo) error { return nil }); err == nil {
			t.Fatal("expected error")
		}
	})
	t.Run("response too large", func(t *testing.T) {
		c, _ := newTestClient(t, func(w http.ResponseWriter, r *http.Request) {
			_, _ = io.WriteString(w, "<ListBucketResult>"+strings.Repeat(" ", maxListBody+10)+"</ListBucketResult>")
		})
		if err := c.List(context.Background(), "a/", func([]ObjectInfo) error { return nil }); err == nil {
			t.Fatal("expected error")
		}
	})
	t.Run("invalid prefix and empty prefix", func(t *testing.T) {
		c, _ := newTestClient(t, func(w http.ResponseWriter, r *http.Request) {
			if r.URL.Query().Has("prefix") {
				t.Errorf("empty prefix should be omitted")
			}
			page(w, nil, "")
		})
		if err := c.List(context.Background(), "../", func([]ObjectInfo) error { return nil }); err == nil {
			t.Fatal("expected error")
		}
		if err := c.List(context.Background(), "", func([]ObjectInfo) error { return nil }); err != nil {
			t.Fatal(err)
		}
	})
	t.Run("500 retried then ok", func(t *testing.T) {
		var calls atomic.Int32
		c, _ := newTestClient(t, func(w http.ResponseWriter, r *http.Request) {
			if calls.Add(1) == 1 {
				w.WriteHeader(http.StatusInternalServerError)
				return
			}
			page(w, []string{"a/1"}, "")
		})
		if err := c.List(context.Background(), "a/", func([]ObjectInfo) error { return nil }); err != nil {
			t.Fatal(err)
		}
	})
}

func TestPutBodyKinds(t *testing.T) {
	data := []byte("some jpeg bytes")
	size := int64(len(data))
	// seekOnly hides io.ReaderAt, forcing the buffered fallback.
	type seekOnly struct{ io.ReadSeeker }

	tests := []struct {
		name    string
		body    func() io.ReadSeeker
		size    int64
		wantErr bool
	}{
		{"ReaderAt at offset 0", func() io.ReadSeeker { return bytes.NewReader(data) }, size, false},
		{"ReaderAt with stale offset", func() io.ReadSeeker {
			r := bytes.NewReader(data)
			_, _ = r.Seek(0, io.SeekEnd)
			return r
		}, size, false},
		{"seek-only body is buffered", func() io.ReadSeeker { return seekOnly{bytes.NewReader(data)} }, size, false},
		{"seek-only body too large to buffer", func() io.ReadSeeker { return seekOnly{bytes.NewReader(data)} }, maxBufferedPut + 1, true},
		{"seek-only body shorter than size", func() io.ReadSeeker { return seekOnly{bytes.NewReader(data)} }, size + 1, true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			var got []byte
			c, _ := newTestClient(t, func(w http.ResponseWriter, r *http.Request) { got, _ = io.ReadAll(r.Body) })
			err := c.Put(context.Background(), "a/b", tt.body(), tt.size)
			if (err != nil) != tt.wantErr {
				t.Fatalf("err = %v, wantErr %v", err, tt.wantErr)
			}
			if !tt.wantErr && !bytes.Equal(got, data) {
				t.Errorf("uploaded %q, want %q", got, data)
			}
		})
	}
}

func TestGetStreamTimeouts(t *testing.T) {
	slowBody := func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Length", "10")
		_, _ = w.Write([]byte("01234"))
		w.(http.Flusher).Flush()
		select {
		case <-time.After(300 * time.Millisecond):
		case <-r.Context().Done():
			return
		}
		_, _ = w.Write([]byte("56789"))
	}

	t.Run("header timeout does not cut off a slow body", func(t *testing.T) {
		c, _ := newTestClient(t, slowBody)
		c.timeout = 100 * time.Millisecond
		c.streamTimeout = 5 * time.Second
		obj, err := c.Get(context.Background(), "a/b", "")
		if err != nil {
			t.Fatal(err)
		}
		defer obj.Body.Close()
		b, err := io.ReadAll(obj.Body)
		if err != nil || string(b) != "0123456789" {
			t.Fatalf("read %q, %v", b, err)
		}
	})
	t.Run("stream timeout bounds the body", func(t *testing.T) {
		c, _ := newTestClient(t, slowBody)
		c.timeout = 100 * time.Millisecond
		c.streamTimeout = 150 * time.Millisecond
		obj, err := c.Get(context.Background(), "a/b", "")
		if err != nil {
			t.Fatal(err)
		}
		defer obj.Body.Close()
		if _, err := io.ReadAll(obj.Body); !errors.Is(err, context.DeadlineExceeded) {
			t.Fatalf("err = %v, want context.DeadlineExceeded", err)
		}
	})
	t.Run("slow headers are retried", func(t *testing.T) {
		var calls atomic.Int32
		c, _ := newTestClient(t, func(w http.ResponseWriter, r *http.Request) {
			if calls.Add(1) == 1 {
				<-r.Context().Done()
				return
			}
			_, _ = w.Write([]byte("x"))
		})
		c.timeout = 100 * time.Millisecond
		obj, err := c.Get(context.Background(), "a/b", "")
		if err != nil {
			t.Fatal(err)
		}
		_ = obj.Body.Close()
		if calls.Load() != 2 {
			t.Errorf("calls = %d, want 2", calls.Load())
		}
	})
}

func TestWrongXMLRootIsAnError(t *testing.T) {
	const errRoot = `<Error><Code>InternalError</Code><Message>nope</Message></Error>`
	t.Run("delete", func(t *testing.T) {
		c, _ := newTestClient(t, func(w http.ResponseWriter, r *http.Request) { _, _ = io.WriteString(w, errRoot) })
		if err := c.Delete(context.Background(), []string{"a/1"}); err == nil {
			t.Fatal("200 with an <Error> root was treated as success")
		}
	})
	t.Run("list", func(t *testing.T) {
		c, _ := newTestClient(t, func(w http.ResponseWriter, r *http.Request) { _, _ = io.WriteString(w, errRoot) })
		err := c.List(context.Background(), "a/", func([]ObjectInfo) error {
			t.Error("callback invoked for a non-ListBucketResult response")
			return nil
		})
		if err == nil {
			t.Fatal("200 with an <Error> root was treated as success")
		}
	})
}
