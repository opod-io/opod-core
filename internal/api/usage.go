package api

import (
	"context"
	"time"

	"github.com/opod-io/opod-core/internal/router"

	"github.com/opod-io/opod-core/internal/auth"
	"github.com/opod-io/opod-core/internal/engines"
	"github.com/opod-io/opod-core/internal/metrics"
	usagelog "github.com/opod-io/opod-core/internal/usage" // api has a wire type named usage
)

// recordUsage hands the record of a finished request to h.Usage and updates
// metrics. It never fails the caller: the request already ended from the
// user's perspective, and the queue behind h.Usage never blocks.
//
// A request cut short (the client gone mid-stream) is recorded like any
// other: nothing here reads the request's context deadline.
//
// Metrics always fire (even when no API key is in context — e.g., dev mode
// with require_keys=false). The record then carries empty key/user
// identifiers.
func (h *Handler) recordUsage(ctx context.Context, operation, model string,
	u *engines.Usage, latency time.Duration, outcome string) {
	h.recordUsageTTFT(ctx, operation, model, u, latency, 0, outcome)
}

// The operations a usage record names (T32.47): what the request did. How it
// was spoken — OpenAI, Anthropic Messages, Vertex — is the protocol.
const (
	OpChat       = "chat"
	OpEmbeddings = "embeddings"
	OpRerank     = "rerank"
)

// recordUsageTTFT is recordUsage with the time to the first usable byte, which
// only the streaming path can know (R15.13). 0 = not streamed, and the control
// plane must not read it as "instant".
func (h *Handler) recordUsageTTFT(ctx context.Context, operation, model string,
	u *engines.Usage, latency, ttft time.Duration, outcome string) {
	pol := h.Policy()
	// A Messages or Vertex request is served by the OpenAI handlers; the
	// record says what the caller spoke (anthropic.go, vertex).
	protocol := protocolOf(ctx, "openai")

	var keyID, userID string
	if k := auth.KeyFrom(ctx); k != nil {
		keyID = k.ID
		userID = k.UserID
	}

	prompt, completion, cached := 0, 0, 0
	if u != nil {
		prompt, completion, cached = u.PromptTokens, u.CompletionTokens, u.CachedTokens
	}

	// Metrics first — always, regardless of auth state. Bound the label
	// cardinality: on non-ok outcomes the model string may be raw client
	// input that never resolved against the catalog, so label those
	// "unknown". The usage row below keeps the raw string — it's useful
	// for debugging there and the DB column isn't a Prometheus label.
	metricsModel := model
	if outcome != "ok" && !pol.modelInCatalog(model) {
		metricsModel = "unknown"
	}
	metrics.ObserveRequest(metricsModel, protocol, outcome, latency, prompt, completion)
	metrics.ObserveTTFT(metricsModel, ttft)
	if h.OnTTFT != nil && ttft > 0 && outcome == "ok" {
		h.OnTTFT(ctx, ttft)
	}

	if h.Usage == nil {
		return
	}
	id := RequestIDFrom(ctx)
	if id == "" {
		// A request that never passed the header middleware still needs an id
		// that is its own: a door's leader deduplicates on it (ADR-063).
		id = newRequestID()
	}
	h.Usage(usagelog.Record{
		TS:               time.Now(),
		RequestID:        id,
		APIKeyID:         keyID,
		UserID:           userID,
		Model:            model,
		Protocol:         protocol,
		PromptTokens:     prompt,
		CompletionTokens: completion,
		LatencyMS:        int(latency.Milliseconds()),
		Outcome:          outcome,
		NodeID:           router.NodeFrom(ctx), // "" when answered locally / never dispatched
		TTFTMS:           int(ttft.Milliseconds()),
		CachedTokens:     cached,
		Class:            router.ClassFrom(ctx),
		Operation:        operation,
	})
}
