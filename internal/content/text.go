package content

import (
	"fmt"
	"strings"
	"unicode"
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

// Wish length bounds, shared by the block validator and the guest wish handler.
const (
	WishMessageMax = 500
	WishAuthorMax  = 80
)

// CleanText normalises raw (trim, canonical line endings) and applies the text
// rules for a field of at most maxRunes runes. ok is false for blank text, too
// long text, disallowed control characters, or a line break when
// allowNewline is false. It is stricter than validateText because it guards
// guest input: it also rejects C1 controls, line/paragraph separators, format
// characters other than ZWNJ/ZWJ (needed by several scripts and emoji
// sequences), runs of more than maxCombiningRun combining marks (Zalgo), and
// text with no visible letter, number, punctuation or symbol (zero-width or
// filler-only). The returned string is the stored form.
func CleanText(raw string, maxRunes int, allowNewline bool) (string, bool) {
	s := normalizeText(raw)
	if s == "" {
		return "", false
	}
	if _, _, ok := validateText(s, maxRunes, allowNewline); !ok {
		return "", false
	}
	if !isCleanGuestText(s) {
		return "", false
	}
	return s, true
}

// maxCombiningRun is the longest run of consecutive nonspacing marks accepted
// in guest text; real scripts need a handful, stacked "Zalgo" marks need more.
const maxCombiningRun = 4

// isCleanGuestText applies the extra guest-text rules on top of validateText.
func isCleanGuestText(s string) bool {
	visible := false
	run := 0
	for _, r := range s {
		if unicode.Is(unicode.Mn, r) {
			if run++; run > maxCombiningRun {
				return false
			}
		} else {
			run = 0
		}
		switch {
		case unicode.Is(unicode.Cc, r) && r != '\n',
			unicode.Is(unicode.Zl, r), unicode.Is(unicode.Zp, r):
			return false
		case unicode.Is(unicode.Cf, r):
			if r != 0x200C && r != 0x200D {
				return false
			}
		case unicode.IsLetter(r), unicode.IsNumber(r), unicode.IsPunct(r), unicode.IsSymbol(r):
			visible = true
		}
	}
	return visible
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

// badgeMaxRunes bounds the hero badge (a numeral or monogram such as "50" or "A&T").
const badgeMaxRunes = 4

// checkBadge validates the hero badge. It is deliberately stricter than
// checkField and is not trimmed: only Unicode letters and digits plus
// "&", "·" (U+00B7), "+" and "-" are allowed, so whitespace, control and
// format characters, bidi overrides, markup and emoji are all rejected.
// An empty badge is valid.
func checkBadge(iss *issues, path, s string) string {
	if s == "" {
		return ""
	}
	if utf8.RuneCountInString(s) > badgeMaxRunes {
		iss.add(path, "too_long", fmt.Sprintf("must be %d characters or fewer", badgeMaxRunes))
		return ""
	}
	for _, r := range s {
		if !unicode.IsLetter(r) && !unicode.IsDigit(r) && r != '&' && r != '·' && r != '+' && r != '-' {
			iss.add(path, "invalid_text", "may only contain letters, digits and & · + -")
			return ""
		}
	}
	return s
}
