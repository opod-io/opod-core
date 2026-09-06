package main

import (
	"testing"
	"unicode/utf8"
)

func TestTruncStr_RuneSafe(t *testing.T) {
	cases := []struct {
		s    string
		n    int
		want string
	}{
		{"hello", 10, "hello"}, // shorter than n
		{"hello", 5, "hello"},  // exactly n
		{"hello", 3, "he…"},    // truncated
		{"héllo", 3, "hé…"},    // multibyte rune must not be split
		{"日本語テスト", 3, "日本…"},   // CJK
		{"abc", 1, "…"},        // n==1 edge
		{"abc", 0, ""},         // n==0 edge
	}
	for _, c := range cases {
		got := truncStr(c.s, c.n)
		if got != c.want {
			t.Errorf("truncStr(%q,%d) = %q; want %q", c.s, c.n, got, c.want)
		}
		if !utf8.ValidString(got) {
			t.Errorf("truncStr(%q,%d) = %q is not valid UTF-8", c.s, c.n, got)
		}
	}
}
