package models

import "testing"

func TestSplitSortSuffix(t *testing.T) {
	cases := []struct {
		in, base, sort string
	}{
		{"qwen3.6-27b:floor", "qwen3.6-27b", SortPrice},
		{"qwen3.6-27b:nitro", "qwen3.6-27b", SortThroughput},
		// Ordinary Ollama tags with colons must pass through untouched.
		{"qwen3:8b", "qwen3:8b", ""},
		{"llama3.2:1b", "llama3.2:1b", ""},
		// Suffix only strips at end-of-string.
		{"x:floor:8b", "x:floor:8b", ""},
		// Tag + suffix composes.
		{"qwen3:8b:nitro", "qwen3:8b", SortThroughput},
		{"", "", ""},
		{"plain", "plain", ""},
	}
	for _, c := range cases {
		base, sort := SplitSortSuffix(c.in)
		if base != c.base || sort != c.sort {
			t.Errorf("SplitSortSuffix(%q) = (%q, %q), want (%q, %q)", c.in, base, sort, c.base, c.sort)
		}
	}
}

func TestValidSort(t *testing.T) {
	for _, ok := range []string{"", SortPrice, SortLatency, SortThroughput} {
		if !ValidSort(ok) {
			t.Errorf("ValidSort(%q) = false, want true", ok)
		}
	}
	for _, bad := range []string{"cheap", "fastest", "PRICE"} {
		if ValidSort(bad) {
			t.Errorf("ValidSort(%q) = true, want false", bad)
		}
	}
}
