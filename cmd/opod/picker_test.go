package main

import (
	"reflect"
	"testing"
)

func TestFilterPickerItems_EmptyQueryReturnsAll(t *testing.T) {
	items := []pickerItem{{ID: "a"}, {ID: "b"}, {ID: "c"}}
	got := filterPickerItems(items, "")
	if !reflect.DeepEqual(got, items) {
		t.Errorf("empty query should return all; got %d items", len(got))
	}
}

func TestFilterPickerItems_SubstringCaseInsensitive(t *testing.T) {
	items := []pickerItem{
		{ID: "qwen3-coder-14b", Label: "Qwen3 Coder 14B"},
		{ID: "llama-3.2-3b", Label: "Llama 3.2 3B"},
		{ID: "claude-3-5-haiku", Label: "Claude 3.5 Haiku"},
	}
	got := filterPickerItems(items, "QWEN")
	if len(got) != 1 || got[0].ID != "qwen3-coder-14b" {
		t.Errorf("QWEN should match qwen3-coder-14b only; got %v", got)
	}
	got = filterPickerItems(items, "haiku")
	if len(got) != 1 || got[0].ID != "claude-3-5-haiku" {
		t.Errorf("haiku should match claude-3-5-haiku; got %v", got)
	}
}

func TestFilterPickerItems_MatchesAcrossIDLabelMeta(t *testing.T) {
	items := []pickerItem{
		{ID: "a-1", Label: "Alpha One", Meta: "fast"},
		{ID: "a-2", Label: "Alpha Two", Meta: "slow"},
	}
	got := filterPickerItems(items, "fast")
	if len(got) != 1 || got[0].ID != "a-1" {
		t.Errorf("Meta filter broken: %v", got)
	}
}

func TestFilterPickerItems_SubsequenceFallback(t *testing.T) {
	items := []pickerItem{
		{ID: "mistral-nemo-12b", Label: "Mistral Nemo 12B"},
		{ID: "qwen3-14b", Label: "Qwen3 14B"},
	}
	// "mstr" has no substring match anywhere, but matches mistral as
	// a subsequence.
	got := filterPickerItems(items, "mstr")
	if len(got) != 1 || got[0].ID != "mistral-nemo-12b" {
		t.Errorf("subseq fallback broken: %v", got)
	}
}

func TestFilterPickerItems_NoMatchReturnsEmpty(t *testing.T) {
	items := []pickerItem{{ID: "qwen3-14b"}, {ID: "llama-3"}}
	got := filterPickerItems(items, "zzzzz")
	if len(got) != 0 {
		t.Errorf("expected empty result, got %v", got)
	}
}

func TestSubseqContainsFold(t *testing.T) {
	cases := []struct {
		s, q string
		want bool
	}{
		{"mistral", "mst", true},      // simple subseq
		{"mistral", "mistral", true},  // full equality
		{"Mistral", "mst", true},      // case-insensitive
		{"qwen3-coder", "qcdr", true}, // dashes ignored as part of fold
		{"mistral", "tsm", false},     // out-of-order — must remain false
		{"", "abc", false},            // empty haystack
		{"abc", "", true},             // empty needle matches anything
	}
	for _, c := range cases {
		if got := subseqContainsFold(c.s, c.q); got != c.want {
			t.Errorf("subseqContainsFold(%q, %q) = %v, want %v", c.s, c.q, got, c.want)
		}
	}
}

func TestExtractJSONFlag(t *testing.T) {
	cases := []struct {
		name   string
		in     []string
		want   []string
		wantOn bool
	}{
		{"absent", []string{"foo"}, []string{"foo"}, false},
		{"trailing", []string{"foo", "--json"}, []string{"foo"}, true},
		{"leading", []string{"--json", "foo"}, []string{"foo"}, true},
		{"middle", []string{"foo", "--json", "bar"}, []string{"foo", "bar"}, true},
		{"empty", []string{}, []string{}, false},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			rest, on := extractJSONFlag(c.in)
			if !reflect.DeepEqual(rest, c.want) {
				t.Errorf("rest = %v, want %v", rest, c.want)
			}
			if on != c.wantOn {
				t.Errorf("on = %v, want %v", on, c.wantOn)
			}
		})
	}
}

func TestExtractYesFlag(t *testing.T) {
	cases := []struct {
		name   string
		in     []string
		want   []string
		wantOn bool
	}{
		{"absent", []string{"id"}, []string{"id"}, false},
		{"long form trailing", []string{"id", "--yes"}, []string{"id"}, true},
		{"short form anywhere", []string{"-y", "id"}, []string{"id"}, true},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			rest, on := extractYesFlag(c.in)
			if !reflect.DeepEqual(rest, c.want) {
				t.Errorf("rest = %v, want %v", rest, c.want)
			}
			if on != c.wantOn {
				t.Errorf("on = %v, want %v", on, c.wantOn)
			}
		})
	}
}
