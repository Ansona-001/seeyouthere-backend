package content

import "testing"

func TestNormalizeSlug(t *testing.T) {
	cases := []struct {
		in   string
		want string
		ok   bool
	}{
		{"  My-Wedding  ", "my-wedding", true},
		{"AB", "", false},          // 2 chars, below the 3-char minimum
		{"abc", "abc", true},       // 3 chars, at the minimum
		{"has--double", "", false}, // no double hyphens
		{"-leading", "", false},    // must start alphanumeric
		{"trailing-", "", false},   // must end alphanumeric
		{"has_underscore", "", false},
		{"has space", "", false},
		{"", "", false},
	}
	for _, c := range cases {
		got, ok := NormalizeSlug(c.in)
		if got != c.want || ok != c.ok {
			t.Errorf("NormalizeSlug(%q) = %q, %v; want %q, %v", c.in, got, ok, c.want, c.ok)
		}
	}
}

func TestNormalizeSlug_LengthBounds(t *testing.T) {
	fifty := "a" + repeat("b", 48) + "a" // 50 chars
	if _, ok := NormalizeSlug(fifty); !ok {
		t.Errorf("50-char slug should be valid")
	}
	fiftyOne := "a" + repeat("b", 49) + "a" // 51 chars
	if _, ok := NormalizeSlug(fiftyOne); ok {
		t.Errorf("51-char slug should be invalid")
	}
}

func repeat(s string, n int) string {
	out := make([]byte, 0, len(s)*n)
	for i := 0; i < n; i++ {
		out = append(out, s...)
	}
	return string(out)
}
