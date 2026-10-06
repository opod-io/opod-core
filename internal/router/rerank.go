package router

import (
	"context"
	"fmt"

	"github.com/opod-io/opod/internal/engines"
)

// Rerank routes one rerank request (ADR-084) through the same dispatch as
// embeddings: the catalog chain, the retries, the next worker on an unreachable
// one. An engine that cannot rerank is refused by name with
// engines.ErrRerankNotSupported, never sent a request it would 404.
func (r *Router) Rerank(ctx context.Context, req engines.RerankRequest) (engines.RerankResponse, error) {
	var res engines.RerankResponse
	err := r.unary(ctx, rerankOp, req.Model, unaryCall{
		supported: func(eng engines.Engine) bool { _, ok := eng.(engines.RerankEngine); return ok },
		do: func(ctx context.Context, eng engines.Engine, model string) error {
			attempt := req
			attempt.Model = model
			out, err := eng.(engines.RerankEngine).Rerank(ctx, attempt)
			if err == nil {
				res = out
			}
			return err
		},
	})
	return res, err
}

var rerankOp = unaryOp{
	span:   "Rerank",
	metric: "rerank",
	unsupported: func(engine string) error {
		return fmt.Errorf("engine %s: %w", engine, engines.ErrRerankNotSupported)
	},
}
