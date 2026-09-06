package api

import (
	"net/http"
	"testing"
)

// TestMergeBodyAndHeaders_BodyWins verifies the documented precedence:
// when both the body block and an X-Opod-* header carry a value, the
// body's value is used.
func TestMergeBodyAndHeaders_BodyWins(t *testing.T) {
	h := http.Header{
		"X-Opod-Fallbacks":        {"hdr-a,hdr-b"},
		"X-Opod-Num-Retries":      {"7"},
		"X-Opod-Retry-Backoff-Ms": {"888"},
	}
	body := &opodExtras{
		Fallbacks:      []string{"body-a", "body-b"},
		NumRetries:     3,
		RetryBackoffMS: 250,
	}
	got := mergeBodyAndHeaders(body, h)
	if len(got.Fallbacks) != 2 || got.Fallbacks[0] != "body-a" {
		t.Errorf("Fallbacks = %v, want body-a,body-b", got.Fallbacks)
	}
	if got.NumRetries != 3 {
		t.Errorf("NumRetries = %d, want 3", got.NumRetries)
	}
	if got.RetryBackoffMS != 250 {
		t.Errorf("RetryBackoffMS = %d, want 250", got.RetryBackoffMS)
	}
}

// TestMergeBodyAndHeaders_HeaderFillsZero — when the body omits a
// field, the corresponding header is consulted. Lets a header-only
// client (curl one-liner, edge proxy) drive overrides.
func TestMergeBodyAndHeaders_HeaderFillsZero(t *testing.T) {
	h := http.Header{
		"X-Opod-Fallbacks":        {"hdr-a, hdr-b , "},
		"X-Opod-Num-Retries":      {"2"},
		"X-Opod-Retry-Backoff-Ms": {"500"},
	}
	got := mergeBodyAndHeaders(nil, h)
	if len(got.Fallbacks) != 2 || got.Fallbacks[0] != "hdr-a" || got.Fallbacks[1] != "hdr-b" {
		t.Errorf("Fallbacks = %v", got.Fallbacks)
	}
	if got.NumRetries != 2 {
		t.Errorf("NumRetries = %d", got.NumRetries)
	}
	if got.RetryBackoffMS != 500 {
		t.Errorf("RetryBackoffMS = %d", got.RetryBackoffMS)
	}
}

// TestMergeBodyAndHeaders_BadHeader silently drops a malformed int —
// the request still goes through with the zero-value field.
func TestMergeBodyAndHeaders_BadHeader(t *testing.T) {
	h := http.Header{"X-Opod-Num-Retries": {"not a number"}}
	got := mergeBodyAndHeaders(nil, h)
	if got.NumRetries != 0 {
		t.Errorf("bad header should leave field zero, got %d", got.NumRetries)
	}
}
