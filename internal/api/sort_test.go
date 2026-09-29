package api

import (
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/opod-io/opod/internal/router"
)

func TestMergeBodyAndHeaders_Sort(t *testing.T) {
	// Body wins over header.
	got := mergeBodyAndHeaders(&opodExtras{Sort: "latency"}, http.Header{"X-Opod-Sort": {"price"}})
	if got.Sort != "latency" {
		t.Errorf("body sort should win, got %q", got.Sort)
	}
	// Header fills when body empty.
	got = mergeBodyAndHeaders(nil, http.Header{"X-Opod-Sort": {"price"}})
	if got.Sort != "price" {
		t.Errorf("header sort = %q, want price", got.Sort)
	}
	// Garbage clamps to empty (ignored, not a 400 — forward compat).
	if c := (router.Overrides{Sort: "cheapest"}).Clamp(); c.Sort != "" {
		t.Errorf("unknown sort survived Clamp: %q", c.Sort)
	}
}

// TestOverridesContext_SuffixPrecedence: explicit opod.sort beats the
// :nitro suffix hint; the suffix applies when nothing explicit is
// present.
func TestOverridesContext_SuffixPrecedence(t *testing.T) {
	req := httptest.NewRequest(http.MethodPost, "/v1/chat/completions", nil)

	ctx := overridesContext(req, &opodExtras{Sort: "latency"}, nil, "m", "throughput")
	if got := router.FromContext(ctx).Sort; got != "latency" {
		t.Errorf("explicit sort should beat suffix, got %q", got)
	}

	ctx = overridesContext(req, nil, nil, "m", "throughput")
	if got := router.FromContext(ctx).Sort; got != "throughput" {
		t.Errorf("suffix hint should apply when nothing explicit, got %q", got)
	}

	// No sort anywhere → overrides not set at all (zero-allocation path).
	ctx = overridesContext(req, nil, nil, "m", "")
	if router.FromContext(ctx).IsSet() {
		t.Errorf("no overrides expected")
	}
}
