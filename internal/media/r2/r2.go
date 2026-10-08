// Package r2 is a minimal S3-compatible object-storage client for Cloudflare
// R2: path-style requests signed with AWS Signature V4, standard library only.
//
// It deliberately imports nothing from the rest of the application so the
// media package can adapt its types and sentinel errors.
package r2

import (
	"bytes"
	"context"
	"crypto/md5" //nolint:gosec // Content-MD5 is an S3 protocol integrity header, not a security control.
	"crypto/sha256"
	"crypto/tls"
	"encoding/base64"
	"encoding/hex"
	"encoding/xml"
	"errors"
	"fmt"
	"io"
	"math/rand/v2"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"time"
)

const (
	region         = "auto"
	service        = "s3"
	maxAttempts    = 3
	attemptTimeout = 30 * time.Second
	// streamTimeout caps one attempt of a streamed GET including the body; it
	// matches the 60 s write deadline the HTTP layer gives a streamed image.
	streamTimeout = 60 * time.Second
	// maxBufferedPut bounds the in-memory copy Put makes of a body that is not
	// an io.ReaderAt (renditions are files, which are).
	maxBufferedPut  = 16 << 20
	maxErrorBody    = 4 << 10
	maxListBody     = 4 << 20
	maxDeleteBody   = 4 << 20
	maxKeysPerPage  = 1000
	maxKeyLen       = 201
	emptyPayloadSHA = "e3b0c44298fc1c149afbf4c8996fb92427ae41e4649b934ca495991b7852b855"
)

var (
	// ErrNotFound means the object does not exist.
	ErrNotFound = errors.New("r2: object not found")
	// ErrNotModified means the If-None-Match value matched the object's ETag.
	ErrNotModified = errors.New("r2: object not modified")
	// ErrStorageUnavailable means R2 could not be reached or kept failing
	// (network error, 429, 5xx) after all retries.
	ErrStorageUnavailable = errors.New("r2: storage unavailable")
)

// Object is an opened object. The caller must close Body.
type Object struct {
	Body io.ReadCloser
	Size int64
	ETag string
}

// ObjectInfo describes one listed object.
type ObjectInfo struct {
	Key          string
	Size         int64
	LastModified time.Time
}

// Client talks to one bucket. It is safe for concurrent use.
type Client struct {
	base        *url.URL
	bucket      string
	accessKeyID string
	secret      string
	hc          *http.Client
	now         func() time.Time
	backoff     [maxAttempts - 1]time.Duration
	timeout     time.Duration
	// streamTimeout is the whole-attempt cap for streamed responses (Get),
	// where timeout only covers waiting for the response headers.
	streamTimeout time.Duration
}

// New builds a Client for the endpoint (scheme and host only, e.g.
// https://<account>.r2.cloudflarestorage.com) and bucket.
func New(endpoint, bucket, accessKeyID, secret string) (*Client, error) {
	u, err := url.Parse(endpoint)
	if err != nil || (u.Scheme != "https" && u.Scheme != "http") || u.Host == "" ||
		u.User != nil || (u.Path != "" && u.Path != "/") || u.RawQuery != "" || u.Fragment != "" {
		return nil, errors.New("r2: invalid endpoint")
	}
	if bucket == "" || accessKeyID == "" || secret == "" {
		return nil, errors.New("r2: bucket and credentials are required")
	}
	if strings.ContainsAny(bucket, "/?#% ") {
		return nil, errors.New("r2: invalid bucket")
	}
	base := &url.URL{Scheme: u.Scheme, Host: u.Host}

	tr := &http.Transport{
		Proxy:                 nil, // never route credentials through an environment proxy
		TLSClientConfig:       &tls.Config{MinVersion: tls.VersionTLS12},
		ForceAttemptHTTP2:     true,
		MaxIdleConns:          8,
		MaxIdleConnsPerHost:   8,
		IdleConnTimeout:       90 * time.Second,
		TLSHandshakeTimeout:   10 * time.Second,
		ResponseHeaderTimeout: 15 * time.Second,
		DisableCompression:    true, // we serve bytes verbatim; Content-Length must match the stored size
	}
	return &Client{
		base:        base,
		bucket:      bucket,
		accessKeyID: accessKeyID,
		secret:      secret,
		hc: &http.Client{
			Transport:     tr,
			CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse },
		},
		now:           time.Now,
		backoff:       [maxAttempts - 1]time.Duration{250 * time.Millisecond, time.Second},
		timeout:       attemptTimeout,
		streamTimeout: streamTimeout,
	}, nil
}

// validKey reports whether key matches ^[a-z0-9][a-z0-9/._-]{0,200}$ and has
// neither ".." nor "//".
func validKey(key string) bool {
	if len(key) == 0 || len(key) > maxKeyLen {
		return false
	}
	for i := 0; i < len(key); i++ {
		c := key[i]
		switch {
		case c >= 'a' && c <= 'z', c >= '0' && c <= '9':
		case i > 0 && (c == '/' || c == '.' || c == '_' || c == '-'):
		default:
			return false
		}
	}
	return !strings.Contains(key, "..") && !strings.Contains(key, "//")
}

func errInvalidKey(op string) error { return fmt.Errorf("r2 %s: invalid key", op) }

// Put stores size bytes from body as image/jpeg, replacing any existing
// object. The body is read twice (hash, then upload) and again on retries.
// Every read goes through its own io.SectionReader over an io.ReaderAt (an
// *os.File, bytes.Reader, ...), so attempts never share a seek offset even if
// an abandoned attempt's transport goroutine is still reading. A body that is
// only an io.ReadSeeker is copied into memory first, up to maxBufferedPut.
func (c *Client) Put(ctx context.Context, key string, body io.ReadSeeker, size int64) error {
	if !validKey(key) {
		return errInvalidKey("put")
	}
	if size < 0 {
		return fmt.Errorf("r2 put %q: negative size", key)
	}
	ra, ok := body.(io.ReaderAt)
	if !ok {
		if size > maxBufferedPut {
			return fmt.Errorf("r2 put %q: body of %d bytes is not an io.ReaderAt and too large to buffer", key, size)
		}
		if _, err := body.Seek(0, io.SeekStart); err != nil {
			return fmt.Errorf("r2 put %q: seek: %w", key, err)
		}
		buf, err := io.ReadAll(io.LimitReader(body, size))
		if err != nil {
			return fmt.Errorf("r2 put %q: read body: %w", key, err)
		}
		ra = bytes.NewReader(buf)
	}
	h := sha256.New()
	n, err := io.Copy(h, io.NewSectionReader(ra, 0, size))
	if err != nil {
		return fmt.Errorf("r2 put %q: hash body: %w", key, err)
	}
	if n != size {
		return fmt.Errorf("r2 put %q: body is %d bytes, expected %d", key, n, size)
	}
	payloadHash := hex.EncodeToString(h.Sum(nil))

	resp, cancel, err := c.send(ctx, "put", key, payloadHash, false, func(actx context.Context) (*http.Request, error) {
		req, err := c.newRequest(actx, http.MethodPut, key, nil, io.NewSectionReader(ra, 0, size), size)
		if err != nil {
			return nil, err
		}
		req.Header.Set("Content-Type", "image/jpeg")
		return req, nil
	})
	if err != nil {
		return err
	}
	defer cancel()
	defer closeBody(resp)
	if resp.StatusCode != http.StatusOK {
		return statusError("put", key, resp)
	}
	return nil
}

// Get opens the object at key. A non-empty ifNoneMatch that matches the
// object's ETag yields ErrNotModified. The caller must close Object.Body.
func (c *Client) Get(ctx context.Context, key, ifNoneMatch string) (*Object, error) {
	if !validKey(key) {
		return nil, errInvalidKey("get")
	}
	resp, cancel, err := c.send(ctx, "get", key, emptyPayloadSHA, true, func(actx context.Context) (*http.Request, error) {
		req, err := c.newRequest(actx, http.MethodGet, key, nil, nil, 0)
		if err != nil {
			return nil, err
		}
		if ifNoneMatch != "" {
			req.Header.Set("If-None-Match", ifNoneMatch)
		}
		return req, nil
	})
	if err != nil {
		return nil, err
	}
	switch {
	case resp.StatusCode == http.StatusNotModified:
		closeBody(resp)
		cancel()
		return nil, ErrNotModified
	case resp.StatusCode != http.StatusOK:
		defer cancel()
		defer closeBody(resp)
		return nil, statusError("get", key, resp)
	case resp.ContentLength < 0:
		closeBody(resp)
		cancel()
		return nil, fmt.Errorf("r2 get %q: missing content length", key)
	}
	return &Object{
		Body: &cancelBody{ReadCloser: resp.Body, cancel: cancel},
		Size: resp.ContentLength,
		ETag: resp.Header.Get("ETag"),
	}, nil
}

type cancelBody struct {
	io.ReadCloser
	cancel context.CancelFunc
}

func (b *cancelBody) Close() error {
	err := b.ReadCloser.Close()
	b.cancel()
	return err
}

type deleteResult struct {
	XMLName xml.Name `xml:"DeleteResult"`
	Errors  []struct {
		Key  string `xml:"Key"`
		Code string `xml:"Code"`
	} `xml:"Error"`
}

// Delete removes keys, in requests of at most 1000. Missing keys are not an
// error; any other per-key failure is.
func (c *Client) Delete(ctx context.Context, keys []string) error {
	for _, k := range keys {
		if !validKey(k) {
			return errInvalidKey("delete")
		}
	}
	for len(keys) > 0 {
		n := min(len(keys), maxKeysPerPage)
		if err := c.deleteChunk(ctx, keys[:n]); err != nil {
			return err
		}
		keys = keys[n:]
	}
	return nil
}

func (c *Client) deleteChunk(ctx context.Context, keys []string) error {
	var b bytes.Buffer
	b.WriteString(`<Delete><Quiet>true</Quiet>`)
	for _, k := range keys {
		b.WriteString("<Object><Key>")
		b.WriteString(k) // validKey guarantees nothing needs XML escaping
		b.WriteString("</Key></Object>")
	}
	b.WriteString("</Delete>")
	payload := b.Bytes()
	sum := sha256.Sum256(payload)
	md := md5.Sum(payload) //nolint:gosec // see import
	md5b64 := base64.StdEncoding.EncodeToString(md[:])

	label := fmt.Sprintf("%d keys", len(keys))
	resp, cancel, err := c.send(ctx, "delete", label, hex.EncodeToString(sum[:]), false, func(actx context.Context) (*http.Request, error) {
		req, err := c.newRequest(actx, http.MethodPost, "", url.Values{"delete": {""}},
			bytes.NewReader(payload), int64(len(payload)))
		if err != nil {
			return nil, err
		}
		req.Header.Set("Content-Type", "application/xml")
		req.Header.Set("Content-MD5", md5b64)
		return req, nil
	})
	if err != nil {
		return err
	}
	defer cancel()
	defer closeBody(resp)
	if resp.StatusCode != http.StatusOK {
		return statusError("delete", label, resp)
	}
	data, err := io.ReadAll(io.LimitReader(resp.Body, maxDeleteBody+1))
	if err != nil {
		return fmt.Errorf("r2 delete %s: read response: %w", label, err)
	}
	if len(data) > maxDeleteBody {
		return fmt.Errorf("r2 delete %s: response too large", label)
	}
	var res deleteResult
	if err := xml.Unmarshal(data, &res); err != nil {
		return fmt.Errorf("r2 delete %s: parse response: %w", label, err)
	}
	var firstCode, firstKey string
	count := 0
	for _, e := range res.Errors {
		if e.Code == "NoSuchKey" {
			continue
		}
		if count == 0 {
			firstCode, firstKey = safeToken(e.Code), e.Key
		}
		count++
	}
	if count > 0 {
		if !validKey(firstKey) {
			firstKey = "?"
		}
		return fmt.Errorf("r2 delete: %d of %d keys failed (first %q: %s)", count, len(keys), firstKey, firstCode)
	}
	return nil
}

type listResult struct {
	XMLName               xml.Name `xml:"ListBucketResult"`
	IsTruncated           bool     `xml:"IsTruncated"`
	NextContinuationToken string   `xml:"NextContinuationToken"`
	Contents              []struct {
		Key          string    `xml:"Key"`
		Size         int64     `xml:"Size"`
		LastModified time.Time `xml:"LastModified"`
	} `xml:"Contents"`
}

// List calls fn with each page (at most 1000 objects, key order) of objects
// whose key starts with prefix. It stops at the first error from fn.
func (c *Client) List(ctx context.Context, prefix string, fn func([]ObjectInfo) error) error {
	if prefix != "" && !validKey(prefix) {
		return errInvalidKey("list")
	}
	token := ""
	for {
		q := url.Values{"list-type": {"2"}, "max-keys": {strconv.Itoa(maxKeysPerPage)}}
		if prefix != "" {
			q.Set("prefix", prefix)
		}
		if token != "" {
			q.Set("continuation-token", token)
		}
		res, err := c.listPage(ctx, prefix, q)
		if err != nil {
			return err
		}
		if len(res.Contents) > 0 {
			page := make([]ObjectInfo, len(res.Contents))
			for i, o := range res.Contents {
				page[i] = ObjectInfo{Key: o.Key, Size: o.Size, LastModified: o.LastModified}
			}
			if err := fn(page); err != nil {
				return err
			}
		}
		if !res.IsTruncated {
			return nil
		}
		if res.NextContinuationToken == "" || res.NextContinuationToken == token {
			return fmt.Errorf("r2 list %q: bad continuation token", prefix)
		}
		token = res.NextContinuationToken
	}
}

func (c *Client) listPage(ctx context.Context, prefix string, q url.Values) (*listResult, error) {
	resp, cancel, err := c.send(ctx, "list", prefix, emptyPayloadSHA, false, func(actx context.Context) (*http.Request, error) {
		return c.newRequest(actx, http.MethodGet, "", q, nil, 0)
	})
	if err != nil {
		return nil, err
	}
	defer cancel()
	defer closeBody(resp)
	if resp.StatusCode != http.StatusOK {
		return nil, statusError("list", prefix, resp)
	}
	data, err := io.ReadAll(io.LimitReader(resp.Body, maxListBody+1))
	if err != nil {
		return nil, fmt.Errorf("r2 list %q: read response: %w", prefix, err)
	}
	if len(data) > maxListBody {
		return nil, fmt.Errorf("r2 list %q: response too large", prefix)
	}
	var res listResult
	if err := xml.Unmarshal(data, &res); err != nil {
		return nil, fmt.Errorf("r2 list %q: parse response: %w", prefix, err)
	}
	return &res, nil
}

// newRequest builds an unsigned request for key (empty = the bucket itself).
func (c *Client) newRequest(ctx context.Context, method, key string, q url.Values, body io.Reader, size int64) (*http.Request, error) {
	u := *c.base
	u.Path = "/" + c.bucket
	if key != "" {
		u.Path += "/" + key
	}
	if len(q) > 0 {
		u.RawQuery = canonicalQuery(q)
	}
	var rb io.ReadCloser = http.NoBody
	if body != nil && size > 0 {
		rb = io.NopCloser(body)
	}
	req, err := http.NewRequestWithContext(ctx, method, u.String(), rb)
	if err != nil {
		return nil, err
	}
	req.ContentLength = size
	return req, nil
}

// send signs and sends a request built by mk, retrying network errors, 429
// and 5xx up to maxAttempts times with backoff. On success the caller owns
// resp and must call cancel after it is done with the body. key is only used
// in error text. stream marks a response whose body the caller streams: its
// per-attempt timeout then only covers waiting for the headers, and the body
// is bounded by streamTimeout instead.
func (c *Client) send(ctx context.Context, op, key, payloadHash string, stream bool, mk func(context.Context) (*http.Request, error)) (*http.Response, context.CancelFunc, error) {
	var last error
	for attempt := range maxAttempts {
		if attempt > 0 {
			if err := c.wait(ctx, c.backoff[attempt-1]); err != nil {
				return nil, nil, fmt.Errorf("r2 %s %q: %w", op, key, err)
			}
		}
		actx, cancel, headersDone := c.attemptCtx(ctx, stream)
		req, err := mk(actx)
		if err != nil {
			cancel()
			return nil, nil, fmt.Errorf("r2 %s %q: build request: %w", op, key, err)
		}
		sign(req, payloadHash, c.accessKeyID, c.secret, region, service, c.now())
		resp, err := c.hc.Do(req)
		headersDone()
		if err != nil {
			cancel()
			if cerr := ctx.Err(); cerr != nil {
				return nil, nil, fmt.Errorf("r2 %s %q: %w", op, key, cerr)
			}
			var ue *url.Error
			if errors.As(err, &ue) {
				err = ue.Err // drop the URL; keep only the cause
			}
			last = err
			continue
		}
		if resp.StatusCode == http.StatusTooManyRequests || resp.StatusCode >= 500 {
			last = fmt.Errorf("status %d", resp.StatusCode)
			closeBody(resp)
			cancel()
			continue
		}
		return resp, cancel, nil
	}
	return nil, nil, fmt.Errorf("r2 %s %q: %w: %v", op, key, ErrStorageUnavailable, last)
}

// attemptCtx returns the context for one attempt, its cancel func, and a func
// to call once the response headers have arrived. A non-streamed attempt is
// capped at c.timeout in total. A streamed one gets c.timeout to produce
// headers (the timer cancels it, so the failure is retried like any network
// error) and c.streamTimeout overall, so a slow client reading a large body
// is not cut off by the header deadline.
func (c *Client) attemptCtx(ctx context.Context, stream bool) (context.Context, context.CancelFunc, func()) {
	if !stream {
		actx, cancel := context.WithTimeout(ctx, c.timeout)
		return actx, cancel, func() {}
	}
	actx, cancel := context.WithTimeout(ctx, c.streamTimeout)
	t := time.AfterFunc(c.timeout, cancel)
	return actx, func() { t.Stop(); cancel() }, func() { t.Stop() }
}

func (c *Client) wait(ctx context.Context, d time.Duration) error {
	d += rand.N(d/2 + 1)
	if d <= 0 {
		return ctx.Err()
	}
	t := time.NewTimer(d)
	defer t.Stop()
	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-t.C:
		return nil
	}
}

// closeBody drains a little of the body so the connection can be reused,
// then closes it.
func closeBody(resp *http.Response) {
	_, _ = io.Copy(io.Discard, io.LimitReader(resp.Body, maxErrorBody))
	_ = resp.Body.Close()
}

type errorBody struct {
	Code string `xml:"Code"`
}

// statusError maps an unexpected response to an error. The text contains the
// operation, key, status and R2 error code only: never headers or body.
func statusError(op, key string, resp *http.Response) error {
	if op == "get" && resp.StatusCode == http.StatusNotFound {
		return fmt.Errorf("r2 %s %q: %w", op, key, ErrNotFound)
	}
	var eb errorBody
	data, _ := io.ReadAll(io.LimitReader(resp.Body, maxErrorBody))
	_ = xml.Unmarshal(data, &eb)
	code := safeToken(eb.Code)
	if op == "get" && code == "NoSuchKey" {
		return fmt.Errorf("r2 %s %q: %w", op, key, ErrNotFound)
	}
	return fmt.Errorf("r2 %s %q: unexpected status %d (code %s)", op, key, resp.StatusCode, code)
}

// safeToken keeps an R2 error code short and printable.
func safeToken(s string) string {
	if s == "" {
		return "none"
	}
	if len(s) > 64 {
		s = s[:64]
	}
	for i := 0; i < len(s); i++ {
		c := s[i]
		if !(c >= 'A' && c <= 'Z' || c >= 'a' && c <= 'z' || c >= '0' && c <= '9') {
			return "invalid"
		}
	}
	return s
}
