package content

import "testing"

func TestNormalizeText(t *testing.T) {
	cases := map[string]string{
		"  hi  ":       "hi",
		"a\r\nb":       "a\nb",
		"a\rb":         "a\nb",
		"\t leading\n": "leading",
	}
	for in, want := range cases {
		if got := normalizeText(in); got != want {
			t.Errorf("normalizeText(%q) = %q, want %q", in, got, want)
		}
	}
}

func TestValidateText(t *testing.T) {
	cases := []struct {
		name         string
		in           string
		max          int
		allowNewline bool
		ok           bool
	}{
		{"within limit", "hello", 5, false, true},
		{"exactly at limit", "hello", 5, false, true},
		{"over limit by one", "hellox", 5, false, false},
		{"newline rejected by default", "a\nb", 10, false, false},
		{"newline allowed", "a\nb", 10, true, true},
		{"control char rejected", "a\x01b", 10, true, false},
		{"del char rejected", "a\x7fb", 10, true, false},
		{"bidi override rejected", "a\u202Eb", 10, true, false},
		{"bidi isolate rejected", "a\u2066b", 10, true, false},
		{"multi-byte rune counted once", "日本語", 3, false, true},
		{"multi-byte over limit", "日本語x", 3, false, false},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			_, _, ok := validateText(c.in, c.max, c.allowNewline)
			if ok != c.ok {
				t.Errorf("validateText(%q, %d, %v) ok = %v, want %v", c.in, c.max, c.allowNewline, ok, c.ok)
			}
		})
	}
}
