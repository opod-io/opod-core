package models

import "strings"

// Sort mode tokens shared by the suffix shortcut, the `opod.sort`
// body field, and the X-Opod-Sort header. There is no price sort: core
// prices nothing (ADR-003, ADR-022 — vendor egress and dollar budgets
// left with the control plane), so `:floor` is not a suffix either.
const (
	SortLatency    = "latency"
	SortThroughput = "throughput"
)

// SplitSortSuffix strips an OpenRouter-style routing shortcut from the
// end of a model id:
//
//	"qwen3.6-27b:nitro" → ("qwen3.6-27b", SortThroughput)
//	"qwen3:8b"          → ("qwen3:8b", "")   — ordinary tags untouched
//
// Only the exact suffix `:nitro` at end-of-string is recognized, so
// engine-native names with colons (Ollama tags, sharded ids) pass
// through unharmed.
func SplitSortSuffix(model string) (base, sort string) {
	if strings.HasSuffix(model, ":nitro") {
		return strings.TrimSuffix(model, ":nitro"), SortThroughput
	}
	return model, ""
}

// ValidSort reports whether s is a recognized sort mode (empty = unset
// is also fine).
func ValidSort(s string) bool {
	switch s {
	case "", SortLatency, SortThroughput:
		return true
	}
	return false
}
