package content

import (
	"fmt"
	"strings"
	"unicode/utf8"
)

// normalizeText trims surrounding whitespace and normalises line endings to
// "\n", the canonical form stored for every text field.
func normalizeText(s string) string {
	s = strings.ReplaceAll(s, "\r\n", "\n")
	s = strings.ReplaceAll(s, "\r", "\n")
	return strings.TrimSpace(s)
}

// validateText checks the rune length and character rules for an already
// normalised string. allowNewline permits "\n" for multi-line fields; every
// other ASCII control character, DEL, and the Unicode bidi-override code
// points (used in filename/text spoofing attacks) are always rejected.
func validateText(s string, maxRunes int, allowNewline bool) (code, message string, ok bool) {
	if n := utf8.RuneCountInString(s); n > maxRunes {
		return "too_long", fmt.Sprintf("must be %d characters or fewer", maxRunes), false
	}
	for _, r := range s {
		switch {
		case r == '\n':
			if !allowNewline {
				return "invalid_text", "must not contain line breaks", false
			}
		case isDisallowedControl(r), isBidiOverride(r):
			return "invalid_text", "contains characters that aren't allowed", false
		}
	}
	return "", "", true
}

func isDisallowedControl(r rune) bool {
	return (r < 0x20 && r != '\n') || r == 0x7f
}

// isBidiOverride reports whether r is a Unicode bidirectional-override or
// isolate control (U+202A-202E, U+2066-2069), used to visually disguise text
// (e.g. reversing a file extension or URL).
func isBidiOverride(r rune) bool {
	return (r >= 0x202A && r <= 0x202E) || (r >= 0x2066 && r <= 0x2069)
}

// checkField normalises and validates a single-line or multi-line text field
// and records an issue at path if it fails required or the text rules.
// Empty, non-required fields are left as "".
func checkField(iss *issues, path, raw string, maxRunes int, required, allowNewline bool) string {
	s := normalizeText(raw)
	if s == "" {
		if required {
			iss.add(path, "required", "This field is required.")
		}
		return ""
	}
	if code, msg, ok := validateText(s, maxRunes, allowNewline); !ok {
		iss.add(path, code, msg)
		return ""
	}
	return s
}
