package mail

import (
	"bytes"
	"encoding/base64"
	"io"
	"mime"
	"mime/multipart"
	"net/mail"
	"strings"
	"testing"
)

func addr(t *testing.T, s string) *mail.Address {
	t.Helper()
	a, err := mail.ParseAddress(s)
	if err != nil {
		t.Fatalf("parse address %q: %v", s, err)
	}
	return a
}

func TestBuild_NoAttachmentsUnchanged(t *testing.T) {
	from := addr(t, "See You There <hello@seeyouthere.at>")
	to := addr(t, "Guest <guest@example.com>")
	m := Message{To: to.Address, Subject: "Hello", Text: "line one\nline two"}

	msg := build(from, to, m)
	s := string(msg)

	if strings.Contains(s, "multipart") {
		t.Errorf("no-attachment message should not be multipart:\n%s", s)
	}
	if !strings.Contains(s, "Content-Type: text/plain; charset=utf-8\r\n") {
		t.Errorf("missing single-part Content-Type:\n%s", s)
	}
	if !strings.Contains(s, "Content-Transfer-Encoding: 8bit\r\n\r\n") {
		t.Errorf("missing single-part Content-Transfer-Encoding:\n%s", s)
	}
	if !strings.HasSuffix(s, "line one\r\nline two") {
		t.Errorf("body not appended verbatim:\n%s", s)
	}

	// Rebuilding with the same inputs must stay byte-identical except for
	// the random Message-ID and Date header, which build() regenerates
	// every call; strip those before comparing the rest of the message.
	msg2 := build(from, to, m)
	strip := func(b []byte) string {
		lines := strings.Split(string(b), "\r\n")
		out := make([]string, 0, len(lines))
		for _, l := range lines {
			if strings.HasPrefix(l, "Message-ID:") || strings.HasPrefix(l, "Date:") {
				continue
			}
			out = append(out, l)
		}
		return strings.Join(out, "\r\n")
	}
	if strip(msg) != strip(msg2) {
		t.Errorf("single-part output not stable across calls:\n%s\n---\n%s", msg, msg2)
	}
}

func TestBuild_WithAttachmentRoundTrips(t *testing.T) {
	from := addr(t, "See You There <hello@seeyouthere.at>")
	to := addr(t, "Guest <guest@example.com>")
	icsContent := []byte("BEGIN:VCALENDAR\r\nEND:VCALENDAR\r\n")
	m := Message{
		To:      to.Address,
		Subject: "Your RSVP",
		Text:    "See attached invite.",
		Attachments: []Attachment{
			{Filename: "event.ics", ContentType: "text/calendar; charset=utf-8; method=PUBLISH", Content: icsContent},
		},
	}

	msg := build(from, to, m)

	headerEnd := bytes.Index(msg, []byte("\r\n\r\n"))
	if headerEnd < 0 {
		t.Fatal("no header/body separator found")
	}
	header, rest := msg[:headerEnd], msg[headerEnd+4:]

	var boundary string
	for _, line := range strings.Split(string(header), "\r\n") {
		if strings.HasPrefix(line, "Content-Type:") {
			_, params, err := mime.ParseMediaType(strings.TrimPrefix(line, "Content-Type: "))
			if err != nil {
				t.Fatalf("parse Content-Type: %v", err)
			}
			boundary = params["boundary"]
		}
	}
	if boundary == "" {
		t.Fatal("no multipart boundary in header")
	}

	mr := multipart.NewReader(bytes.NewReader(rest), boundary)

	part, err := mr.NextPart()
	if err != nil {
		t.Fatalf("read text part: %v", err)
	}
	textBody, err := io.ReadAll(part)
	if err != nil {
		t.Fatalf("read text body: %v", err)
	}
	if string(textBody) != "See attached invite." {
		t.Errorf("text part = %q", textBody)
	}

	part, err = mr.NextPart()
	if err != nil {
		t.Fatalf("read attachment part: %v", err)
	}
	if ct := part.Header.Get("Content-Type"); !strings.HasPrefix(ct, "text/calendar") {
		t.Errorf("attachment Content-Type = %q", ct)
	}
	if cd := part.Header.Get("Content-Disposition"); !strings.Contains(cd, `filename="event.ics"`) {
		t.Errorf("attachment Content-Disposition = %q", cd)
	}
	// mime/multipart hands back the raw part body; base64 decoding per
	// Content-Transfer-Encoding is this test's job, same as a real client's.
	attBody, err := io.ReadAll(base64.NewDecoder(base64.StdEncoding, part))
	if err != nil {
		t.Fatalf("decode attachment body: %v", err)
	}
	if !bytes.Equal(attBody, icsContent) {
		t.Errorf("attachment content = %q, want %q", attBody, icsContent)
	}

	if _, err := mr.NextPart(); err != io.EOF {
		t.Errorf("expected exactly two parts, got extra: err=%v", err)
	}
}

func TestSanitizeFilename(t *testing.T) {
	cases := map[string]string{
		"event.ics":            "event.ics",
		"evil\r\nBcc: x\n.ics": "evilBcc: x.ics",
		"../../etc/passwd.ics": "....etcpasswd.ics",
		`back\slash".ics`:      "backslash.ics",
	}
	for in, want := range cases {
		if got := sanitizeFilename(in); got != want {
			t.Errorf("sanitizeFilename(%q) = %q, want %q", in, got, want)
		}
	}
}
