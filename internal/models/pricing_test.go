package models

import (
	"math"
	"testing"
)

func TestPriceFor_VendorPrefix(t *testing.T) {
	cases := []struct {
		id     string
		wantPP float64 // USD per 1k prompt tokens
		wantPC float64 // USD per 1k completion tokens
	}{
		{"claude-opus-4-7", 0.015, 0.075},
		{"claude-opus-4-7-20251101", 0.015, 0.075}, // suffix tags inherit
		{"claude-sonnet-4-6", 0.003, 0.015},
		{"claude-3-5-sonnet-latest", 0.003, 0.015},
		{"claude-3-haiku-20240307", 0.00025, 0.00125},
		{"claude-totally-new", 0.003, 0.015}, // generic claude- fallback
		{"gpt-4o-mini", 0.00015, 0.0006},
		{"gpt-4o", 0.0025, 0.010},
		{"gpt-5-something", 0.0025, 0.010}, // generic gpt- fallback
		{"o3", 0.015, 0.060},
		{"qwen3-14b", 0, 0}, // not in vendor table
		{"unknown", 0, 0},
	}
	for _, c := range cases {
		t.Run(c.id, func(t *testing.T) {
			pp, pc := PriceFor(c.id, nil)
			if !approxEq(pp, c.wantPP) || !approxEq(pc, c.wantPC) {
				t.Errorf("PriceFor(%q) = (%.6f, %.6f), want (%.6f, %.6f)", c.id, pp, pc, c.wantPP, c.wantPC)
			}
		})
	}
}

// TestPriceFor_CatalogEntryWinsOverVendor — if a catalog row sets a
// non-zero price (operator wants to internally bill for a self-hosted
// model), that overrides the vendor table lookup.
func TestPriceFor_CatalogEntryOverridesVendor(t *testing.T) {
	cat := []Entry{
		{ID: "gpt-4o", PricePromptUSDPer1K: 1.234, PriceCompletionUSDPer1K: 5.678},
	}
	pp, pc := PriceFor("gpt-4o", cat)
	if !approxEq(pp, 1.234) || !approxEq(pc, 5.678) {
		t.Errorf("catalog override failed: (%.6f, %.6f)", pp, pc)
	}
}

// TestCostOf_Math — sanity-check the per-call cost formula against a
// hand-computed example.
func TestCostOf_Math(t *testing.T) {
	// claude-opus-4 → $15 input / $75 output per 1M
	// 2000 prompt + 500 completion → 2k*15/1M + 500*75/1M = 0.030 + 0.0375 = 0.0675
	got := CostOf("claude-opus-4", nil, 2000, 500)
	want := 0.0675
	if !approxEq(got, want) {
		t.Errorf("CostOf claude-opus-4 = %.6f, want %.6f", got, want)
	}
	// Free model
	if c := CostOf("qwen3-14b", nil, 9999, 9999); c != 0 {
		t.Errorf("free model should cost 0, got %.6f", c)
	}
	// Empty token counts
	if c := CostOf("claude-opus-4", nil, 0, 0); c != 0 {
		t.Errorf("zero tokens should cost 0, got %.6f", c)
	}
}

func approxEq(a, b float64) bool {
	if a == 0 && b == 0 {
		return true
	}
	return math.Abs(a-b) < 1e-9
}
