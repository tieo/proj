package shellout

import "testing"

func TestQuote(t *testing.T) {
	cases := map[string]string{
		"webapp":      `'webapp'`,
		"my app":      `'my app'`,
		"":            `''`,
		"a;rm -rf ~":  `'a;rm -rf ~'`,
		"$(whoami)":   `'$(whoami)'`,
		"a'b":         `'a'\''b'`,
		"it's":        `'it'\''s'`,
		"`id`":        "'`id`'",
		"a && b || c": `'a && b || c'`,
	}
	for in, want := range cases {
		if got := Quote(in); got != want {
			t.Errorf("Quote(%q) = %q, want %q", in, got, want)
		}
	}
}
