package ics

import (
	"strings"
	"testing"
	"time"
)

var (
	now   = time.Date(2026, 1, 15, 10, 0, 0, 0, time.UTC)
	start = time.Date(2026, 6, 20, 16, 30, 0, 0, time.UTC)
	end   = time.Date(2026, 6, 20, 20, 0, 0, 0, time.UTC)
)

func TestBuild_WellFormed(t *testing.T) {
	doc := build("Priya & Arjun's Wedding", start, end, "The Grand Hall, Mumbai", now, "test-uid@seeuthere.at")
	text := string(doc)
	lines := strings.Split(text, "\r\n")

	if lines[0] != "BEGIN:VCALENDAR" {
		t.Errorf("lines[0] = %q", lines[0])
	}
	if lines[1] != "VERSION:2.0" {
		t.Errorf("lines[1] = %q", lines[1])
	}
	for _, want := range []string{
		"PRODID:-//See You There//EN",
		"CALSCALE:GREGORIAN",
		"METHOD:PUBLISH",
		"BEGIN:VEVENT",
		"UID:test-uid@seeuthere.at",
		"DTSTAMP:20260115T100000Z",
		"DTSTART:20260620T163000Z",
		"DTEND:20260620T200000Z",
		"SUMMARY:Priya & Arjun's Wedding",
		"LOCATION:The Grand Hall\\, Mumbai",
		"END:VEVENT",
	} {
		if !contains(lines, want) {
			t.Errorf("missing line %q in:\n%s", want, text)
		}
	}
	if lines[len(lines)-2] != "END:VCALENDAR" {
		t.Errorf("second-to-last line = %q, want END:VCALENDAR", lines[len(lines)-2])
	}
	if !strings.HasSuffix(text, "\r\n") {
		t.Error("document does not end with CRLF")
	}
}

func TestBuild_DefaultsEndToStartPlusOneHour(t *testing.T) {
	doc := build("Party", start, time.Time{}, "", now, "uid")
	text := string(doc)
	if !strings.Contains(text, "DTSTART:20260620T163000Z") {
		t.Errorf("missing DTSTART:\n%s", text)
	}
	if !strings.Contains(text, "DTEND:20260620T173000Z") {
		t.Errorf("missing defaulted DTEND:\n%s", text)
	}
}

func TestBuild_OmitsLocationWhenEmpty(t *testing.T) {
	doc := build("Party", start, end, "", now, "uid")
	if strings.Contains(string(doc), "LOCATION:") {
		t.Errorf("expected no LOCATION line:\n%s", doc)
	}
}

func TestBuild_EscapesTextProperties(t *testing.T) {
	doc := build("Reception; drinks, dancing \\ dinner\nSee you there!", start, end, "", now, "uid")
	want := `SUMMARY:Reception\; drinks\, dancing \\ dinner\nSee you there!`
	if !strings.Contains(string(doc), want) {
		t.Errorf("want %q in:\n%s", want, doc)
	}
}

func TestBuild_EscapesCRLFAndBareCR(t *testing.T) {
	doc := build("line1\r\nline2\rline3", start, end, "", now, "uid")
	want := `SUMMARY:line1\nline2\nline3`
	if !strings.Contains(string(doc), want) {
		t.Errorf("want %q in:\n%s", want, doc)
	}
}

func TestBuild_FoldsLongLinesAt75Octets(t *testing.T) {
	longLocation := strings.Repeat("A", 120)
	doc := build("Party", start, end, longLocation, now, "uid")
	physicalLines := strings.Split(string(doc), "\r\n")
	for _, line := range physicalLines {
		if len(line) > 75 { // ASCII here, so len() == octet count
			t.Errorf("line exceeds 75 octets (%d): %q", len(line), line)
		}
	}
	foundContinuation := false
	for _, line := range physicalLines {
		if strings.HasPrefix(line, " ") && strings.Contains(line, "AAA") {
			foundContinuation = true
		}
	}
	if !foundContinuation {
		t.Error("expected a folded continuation line starting with a space")
	}
}

func TestBuild_FoldingNeverSplitsMultiByteRune(t *testing.T) {
	doc := build("Party", start, end, strings.Repeat("café", 30), now, "uid")
	for _, line := range strings.Split(string(doc), "\r\n") {
		stripped := strings.TrimPrefix(line, " ")
		if !isValidUTF8(stripped) {
			t.Errorf("line is not valid UTF-8 (split rune): %q", line)
		}
	}
}

func isValidUTF8(s string) bool {
	for _, r := range s {
		if r == '�' {
			return false
		}
	}
	return true
}

func contains(lines []string, want string) bool {
	for _, l := range lines {
		if l == want {
			return true
		}
	}
	return false
}
