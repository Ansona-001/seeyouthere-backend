package r2

import (
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"net/http"
	"net/url"
	"sort"
	"strings"
	"time"
)

const (
	amzDate   = "20060102T150405Z"
	shortDate = "20060102"
	algorithm = "AWS4-HMAC-SHA256"
)

// sign adds x-amz-date, x-amz-content-sha256 and the Authorization header to
// req using AWS Signature Version 4. Every header already on req, plus Host,
// is signed. payloadHash is the lower-case hex SHA-256 of the request body.
func sign(req *http.Request, payloadHash, accessKeyID, secret, region, service string, t time.Time) {
	t = t.UTC()
	req.Header.Set("X-Amz-Date", t.Format(amzDate))
	req.Header.Set("X-Amz-Content-Sha256", payloadHash)

	host := req.Host
	if host == "" {
		host = req.URL.Host
	}
	names := make([]string, 0, len(req.Header)+1)
	values := map[string]string{"host": host}
	names = append(names, "host")
	for k, v := range req.Header {
		lk := strings.ToLower(k)
		if lk == "host" {
			continue
		}
		names = append(names, lk)
		values[lk] = canonicalValue(strings.Join(v, ","))
	}
	sort.Strings(names)

	var hdrs strings.Builder
	for _, n := range names {
		hdrs.WriteString(n)
		hdrs.WriteByte(':')
		hdrs.WriteString(values[n])
		hdrs.WriteByte('\n')
	}
	signedHeaders := strings.Join(names, ";")

	canonical := strings.Join([]string{
		req.Method,
		uriEncode(req.URL.Path, false),
		canonicalQuery(req.URL.Query()),
		hdrs.String(),
		signedHeaders,
		payloadHash,
	}, "\n")

	scope := t.Format(shortDate) + "/" + region + "/" + service + "/aws4_request"
	canonicalHash := sha256.Sum256([]byte(canonical))
	stringToSign := algorithm + "\n" + t.Format(amzDate) + "\n" + scope + "\n" + hex.EncodeToString(canonicalHash[:])

	key := hmacSHA256([]byte("AWS4"+secret), t.Format(shortDate))
	key = hmacSHA256(key, region)
	key = hmacSHA256(key, service)
	key = hmacSHA256(key, "aws4_request")
	signature := hex.EncodeToString(hmacSHA256(key, stringToSign))

	req.Header.Set("Authorization", algorithm+" Credential="+accessKeyID+"/"+scope+
		", SignedHeaders="+signedHeaders+", Signature="+signature)
}

func hmacSHA256(key []byte, data string) []byte {
	m := hmac.New(sha256.New, key)
	m.Write([]byte(data))
	return m.Sum(nil)
}

// canonicalValue trims a header value and collapses runs of spaces.
func canonicalValue(v string) string {
	return strings.Join(strings.Fields(v), " ")
}

// canonicalQuery renders query parameters sorted by encoded name, as SigV4
// requires. The same encoding is used to build the request's RawQuery.
func canonicalQuery(q url.Values) string {
	pairs := make([]string, 0, len(q))
	for k, vs := range q {
		ek := uriEncode(k, true)
		for _, v := range vs {
			pairs = append(pairs, ek+"="+uriEncode(v, true))
		}
	}
	sort.Strings(pairs)
	return strings.Join(pairs, "&")
}

// uriEncode percent-encodes s per the SigV4 rules: unreserved characters stay,
// everything else is %XX upper-case; "/" is kept unless encodeSlash.
func uriEncode(s string, encodeSlash bool) string {
	const hexUpper = "0123456789ABCDEF"
	var b strings.Builder
	b.Grow(len(s))
	for i := 0; i < len(s); i++ {
		c := s[i]
		switch {
		case c >= 'A' && c <= 'Z', c >= 'a' && c <= 'z', c >= '0' && c <= '9',
			c == '-', c == '_', c == '.', c == '~':
			b.WriteByte(c)
		case c == '/' && !encodeSlash:
			b.WriteByte(c)
		default:
			b.WriteByte('%')
			b.WriteByte(hexUpper[c>>4])
			b.WriteByte(hexUpper[c&15])
		}
	}
	return b.String()
}
