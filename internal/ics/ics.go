// Package ics builds minimal RFC 5545 iCalendar (.ics) documents for a
// single VEVENT. It's a Go port of the frontend's own calendar builder
// (seeyouthere-frontend/src/lib/calendar.ts), used here to attach a
// calendar invite to RSVP confirmation emails; the frontend's "add to
// calendar" buttons on the public event page build the same document
// client-side as a fallback for guests who open the email on a device that
// won't open an attachment.
package ics

import (
	"crypto/rand"
	"encoding/hex"
	"strings"
	"time"
	"unicode/utf8"
)

// defaultDuration is used for DTEND when the caller has no end time,
// matching the frontend's own fallback (calendar.ts DEFAULT_DURATION_MS).
const defaultDuration = time.Hour

// maxLineOctets is RFC 5545 §3.1's line-folding limit.
const maxLineOctets = 75

// Build returns a complete .ics document (CRLF line endings) for one
// timed event. end may be the zero Time, meaning "unknown": DTEND then
// defaults to start + 1 hour. location is optional; an empty string omits
// the LOCATION property.
func Build(summary string, start, end time.Time, location string) []byte {
	return build(summary, start, end, location, time.Now(), newUID())
}

// build is Build with its non-deterministic inputs (DTSTAMP's clock, UID's
// randomness) passed in, so tests can assert exact output.
func build(summary string, start, end time.Time, location string, now time.Time, uid string) []byte {
	if end.IsZero() {
		end = start.Add(defaultDuration)
	}

	lines := []string{
		"BEGIN:VCALENDAR",
		"VERSION:2.0",
		"PRODID:-//See You There//EN",
		"CALSCALE:GREGORIAN",
		"METHOD:PUBLISH",
		"BEGIN:VEVENT",
		"UID:" + uid,
		"DTSTAMP:" + formatUTC(now),
		"DTSTART:" + formatUTC(start),
		"DTEND:" + formatUTC(end),
		"SUMMARY:" + escapeText(summary),
	}
	if location != "" {
		lines = append(lines, "LOCATION:"+escapeText(location))
	}
	lines = append(lines, "END:VEVENT", "END:VCALENDAR")

	var b strings.Builder
	for _, line := range lines {
		b.WriteString(foldLine(line))
		b.WriteString("\r\n")
	}
	return []byte(b.String())
}

// newUID generates a globally-unique-enough VEVENT UID. It doesn't need to
// be cryptographically secure, only practically unique per message.
func newUID() string {
	b := make([]byte, 16)
	_, _ = rand.Read(b)
	return hex.EncodeToString(b) + "@seeyouthere.at"
}

// formatUTC renders t as RFC 5545's UTC "floating" form, YYYYMMDDTHHMMSSZ.
func formatUTC(t time.Time) string {
	return t.UTC().Format("20060102T150405Z")
}

// escapeText applies RFC 5545 §3.3.11 TEXT-value escaping: backslash,
// semicolon and comma are backslash-escaped, newlines become the literal
// `\n`, and bare/CRLF line endings are normalised to `\n` first. Order
// matters — backslashes must be escaped before the characters that
// introduce new backslashes (`;`, `,`, the literal `\n`), or this would
// double-escape its own output.
func escapeText(s string) string {
	s = strings.ReplaceAll(s, "\r\n", "\n")
	s = strings.ReplaceAll(s, "\r", "\n")
	s = strings.ReplaceAll(s, `\`, `\\`)
	s = strings.ReplaceAll(s, ";", `\;`)
	s = strings.ReplaceAll(s, ",", `\,`)
	s = strings.ReplaceAll(s, "\n", `\n`)
	return s
}

// foldLine applies RFC 5545 §3.1 line folding: no physical line may exceed
// 75 octets (UTF-8 bytes), and continuation lines start with a single
// space (itself counted toward that line's 75-octet limit). It splits on
// code points so a multi-byte UTF-8 sequence is never cut in half.
func foldLine(line string) string {
	if len(line) <= maxLineOctets {
		return line
	}

	var out []string
	var current strings.Builder
	currentBytes := 0
	for _, r := range line {
		chBytes := utf8.RuneLen(r)
		if currentBytes+chBytes > maxLineOctets {
			out = append(out, current.String())
			current.Reset()
			current.WriteByte(' ')
			current.WriteRune(r)
			currentBytes = 1 + chBytes
		} else {
			current.WriteRune(r)
			currentBytes += chBytes
		}
	}
	out = append(out, current.String())
	return strings.Join(out, "\r\n")
}
