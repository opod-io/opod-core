package engines

import (
	"context"
	"errors"
)

// Rerank (ADR-084, amending ADR-022): score a list of documents against one
// query with a reranker — a cross-encoder or a decoder-only reranker — and
// answer the scores. One non-streaming request, the retrieval stack's second
// pass after embeddings.

// RerankRequest is the engine-agnostic rerank input. The engine is used as a
// SCORER only: ordering, `top_n` and returning the documents' text are the
// gateway's, so every engine answers the caller the same way whatever its own
// response shape (vLLM and llama.cpp: an object with `results`; SGLang: a bare
// array) and whatever it does with `top_n` (llama.cpp truncates to zero on 0;
// SGLang refuses anything below 1).
type RerankRequest struct {
	Model     string
	Query     string
	Documents []string
}

// RerankResult is one document's relevance score, by its index in the request.
type RerankResult struct {
	Index int
	Score float64
}

// RerankResponse is one score per document, in no particular order.
type RerankResponse struct {
	Results []RerankResult
	Usage   *Usage // PromptTokens populated when the engine reports it
}

// RerankEngine is implemented by engines that serve reranking. A sub-interface,
// like EmbedEngine: only the drivers whose engine has a rerank route implement
// it, so an engine without one is refused by name (ErrRerankNotSupported)
// rather than sent a request it would answer 404.
type RerankEngine interface {
	Rerank(ctx context.Context, req RerankRequest) (RerankResponse, error)
}

// ErrRerankNotSupported says the engine serving this model has no rerank route
// (Ollama, MLX-LM), or a worker's engine said so. The gateway answers 501
// `rerank_not_supported`.
var ErrRerankNotSupported = errors.New("this engine does not serve rerank")
