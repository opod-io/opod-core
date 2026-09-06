package models

import "strings"

// VendorPrice carries the per-1k-token cost for a vendor-proxied model.
// All values are in USD; fields are zero for models that should not be
// cost-attributed (typically operator's own open-weight models).
type VendorPrice struct {
	PromptUSDPer1K     float64
	CompletionUSDPer1K float64
}

// vendorPriceTable maps model-id prefixes / exact ids to current public
// rates. Updated by hand as vendors change pricing; checked in source
// so PRs to update rates are obvious in git blame.
//
// Lookup order in PriceFor:
//
//  1. Exact id match.
//  2. Longest-prefix match against known vendor families.
//
// Adding a new model: just append a row. Removing a model: delete the
// row — PriceFor falls through to 0/0, which the cost-tracking code
// treats as "no cost recorded" (safe default).
//
// Sources (last refreshed 2026-06-10):
//   - Anthropic:  https://www.anthropic.com/api#pricing
//   - OpenAI:     https://openai.com/api/pricing/
//
// Each row stores USD per 1k tokens. Helper `perMillion(input, output)`
// makes the table read like the vendor's published rates (which are
// usually quoted as $/M tokens).
var vendorPriceTable = []struct {
	IDPrefix string
	Price    VendorPrice
}{
	// ── Anthropic ────────────────────────────────────────────────
	{"claude-opus-4-7", perMillion(15.00, 75.00)},
	{"claude-opus-4-6", perMillion(15.00, 75.00)},
	{"claude-opus-4-5", perMillion(15.00, 75.00)},
	{"claude-opus-4", perMillion(15.00, 75.00)},
	{"claude-sonnet-4-6", perMillion(3.00, 15.00)},
	{"claude-sonnet-4-5", perMillion(3.00, 15.00)},
	{"claude-sonnet-4", perMillion(3.00, 15.00)},
	{"claude-haiku-4-5", perMillion(1.00, 5.00)},
	{"claude-haiku-4", perMillion(0.80, 4.00)},
	{"claude-3-5-sonnet", perMillion(3.00, 15.00)},
	{"claude-3-7-sonnet", perMillion(3.00, 15.00)},
	{"claude-3-5-haiku", perMillion(0.80, 4.00)},
	{"claude-3-opus", perMillion(15.00, 75.00)},
	{"claude-3-sonnet", perMillion(3.00, 15.00)},
	{"claude-3-haiku", perMillion(0.25, 1.25)},
	// Generic fallback for any other claude-*.
	{"claude-", perMillion(3.00, 15.00)},

	// ── OpenAI ──────────────────────────────────────────────────
	{"gpt-4o-mini", perMillion(0.15, 0.60)},
	{"gpt-4o", perMillion(2.50, 10.00)},
	{"gpt-4-turbo", perMillion(10.00, 30.00)},
	{"gpt-4.1-mini", perMillion(0.40, 1.60)},
	{"gpt-4.1", perMillion(2.00, 8.00)},
	{"gpt-4", perMillion(30.00, 60.00)},
	{"o4-mini", perMillion(1.10, 4.40)},
	{"o4", perMillion(15.00, 60.00)},
	{"o3-mini", perMillion(1.10, 4.40)},
	{"o3", perMillion(15.00, 60.00)},
	{"o1-mini", perMillion(3.00, 12.00)},
	{"o1", perMillion(15.00, 60.00)},
	// Generic fallbacks.
	{"gpt-", perMillion(2.50, 10.00)},

	// ── Cohere (via cohere/<model> prefix) ──────────────────────
	{"cohere/command-r-plus", perMillion(2.50, 10.00)},
	{"cohere/command-r", perMillion(0.15, 0.60)},
	{"cohere/command-a", perMillion(2.50, 10.00)},
	{"cohere/command-", perMillion(2.50, 10.00)}, // family fallback

	// ── Mistral La Plateforme (via mistral/<model> prefix) ─────
	{"mistral/mistral-large", perMillion(2.00, 6.00)},
	{"mistral/mistral-medium", perMillion(0.40, 2.00)},
	{"mistral/mistral-small", perMillion(0.20, 0.60)},
	{"mistral/ministral-8b", perMillion(0.10, 0.10)},
	{"mistral/codestral", perMillion(0.20, 0.60)},
	{"mistral/", perMillion(0.40, 2.00)}, // family fallback

	// ── Perplexity (via perplexity/<model> prefix) ──────────────
	// Per-query pricing for online models is the bigger cost; per-token
	// rates below are for the LLM portion only — operators tracking
	// per-call cost may want to add the search surcharge separately.
	{"perplexity/sonar-pro", perMillion(3.00, 15.00)},
	{"perplexity/sonar-reasoning", perMillion(1.00, 5.00)},
	{"perplexity/sonar", perMillion(1.00, 1.00)},
	{"perplexity/", perMillion(1.00, 5.00)}, // family fallback
}

// perMillion converts the (input, output) USD-per-million-tokens
// pairing that vendors typically publish into the per-1k-token unit
// VendorPrice stores. So a table row reads as the vendor's price
// sheet, not a hand-converted decimal.
func perMillion(inputUSDPerM, outputUSDPerM float64) VendorPrice {
	return VendorPrice{
		PromptUSDPer1K:     inputUSDPerM / 1000,
		CompletionUSDPer1K: outputUSDPerM / 1000,
	}
}

// PriceFor returns the pricing rate (USD per 1k tokens, prompt and
// completion) for the given model id. The lookup consults the catalog
// entry's `price_*` fields first, then falls back to the vendor table
// for proxied models. Returns (0, 0) for any model with no explicit
// pricing — the cost-tracking code treats that as "no cost recorded",
// which is the right answer for an operator's open-weight install.
//
// `cat` may be nil — vendor models still resolve via the prefix table.
func PriceFor(modelID string, cat []Entry) (promptPer1K, completionPer1K float64) {
	if e := FindByID(cat, modelID); e != nil {
		if e.PricePromptUSDPer1K > 0 || e.PriceCompletionUSDPer1K > 0 {
			return e.PricePromptUSDPer1K, e.PriceCompletionUSDPer1K
		}
	}
	// Vendor lookup: longest-prefix wins. The table is hand-sorted
	// roughly longest-to-shortest so a linear scan finds the most
	// specific match first; we still iterate fully for safety.
	var best VendorPrice
	var bestLen int
	for _, row := range vendorPriceTable {
		if strings.HasPrefix(modelID, row.IDPrefix) && len(row.IDPrefix) > bestLen {
			best = row.Price
			bestLen = len(row.IDPrefix)
		}
	}
	return best.PromptUSDPer1K, best.CompletionUSDPer1K
}

// CostOf computes the dollar cost for a single request given its actual
// prompt and completion token counts. Used by the usage-recording path
// at the moment of write so historical totals stay accurate even if
// prices change later.
func CostOf(modelID string, cat []Entry, promptTokens, completionTokens int) float64 {
	pp, pc := PriceFor(modelID, cat)
	if pp == 0 && pc == 0 {
		return 0
	}
	return float64(promptTokens)*pp/1000 + float64(completionTokens)*pc/1000
}
