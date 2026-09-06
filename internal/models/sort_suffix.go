package models

import "strings"

// Sort mode tokens shared by the suffix shortcuts, the `opod.sort`
// body field, and the X-Opod-Sort header.
const (
	SortPrice      = "price"
	SortLatency    = "latency"
	SortThroughput = "throughput"
)

// SplitSortSuffix strips an OpenRouter-style routing shortcut from the
// end of a model id:
//
//	"qwen3.6-27b:floor" → ("qwen3.6-27b", SortPrice)
//	"qwen3.6-27b:nitro" → ("qwen3.6-27b", SortThroughput)
//	"qwen3:8b"          → ("qwen3:8b", "")   — ordinary tags untouched
//
// Only the exact suffixes `:floor` and `:nitro` at end-of-string are
// recognized, so engine-native names with colons (Ollama tags, sharded
// ids) pass through unharmed.
func SplitSortSuffix(model string) (base, sort string) {
	switch {
	case strings.HasSuffix(model, ":floor"):
		return strings.TrimSuffix(model, ":floor"), SortPrice
	case strings.HasSuffix(model, ":nitro"):
		return strings.TrimSuffix(model, ":nitro"), SortThroughput
	default:
		return model, ""
	}
}

// ValidSort reports whether s is a recognized sort mode (empty = unset
// is also fine).
func ValidSort(s string) bool {
	switch s {
	case "", SortPrice, SortLatency, SortThroughput:
		return true
	}
	return false
}
