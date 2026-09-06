package engines

import "context"

// EmbedRequest is the engine-agnostic embedding input.
type EmbedRequest struct {
	Model  string
	Inputs []string // one or more pieces of text — each produces one vector
}

// EmbedResponse is the engine-agnostic embedding output.
type EmbedResponse struct {
	Vectors [][]float32 // one per input, in the same order
	Usage   *Usage      // PromptTokens populated; CompletionTokens is zero for embeddings
}

// EmbedEngine is implemented by engines that produce embeddings.
//
// Sub-interface (not part of Engine) so engines can opt in incrementally —
// callers do a type assertion: `if ee, ok := eng.(EmbedEngine); ok { … }`.
// Engines that don't implement it surface as a 501 at the API boundary.
type EmbedEngine interface {
	Embed(ctx context.Context, req EmbedRequest) (EmbedResponse, error)
}
