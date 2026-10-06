package openaicompat

// Rerank over HTTP (ADR-084). Not a method on Client on purpose: MLX-LM embeds
// Client and has no rerank route, and a promoted method would make every
// driver claim engines.RerankEngine. A driver whose engine serves rerank adds
// a one-line Rerank that calls this with its route.
//
// Routes, read from each engine's source (2026-10-06):
//   - vLLM serves /rerank, /v1/rerank and /v2/rerank for a pooling model
//     (vllm/entrypoints/pooling/scoring/api_router.py); /v1/rerank logs a
//     one-time note that /rerank is the canonical path, and serves.
//   - llama.cpp's server serves /rerank, /reranking, /v1/rerank and
//     /v1/reranking, and answers 501 not_supported_error unless it was started
//     with --reranking (tools/server/server-context.cpp).
//   - SGLang serves /v1/rerank only, and answers a bare JSON array
//     (python/sglang/srt/entrypoints/http_server.py, serving_rerank.py).
//
// /v1/rerank is the one path all three serve, and an opod worker serves it too.

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"

	"github.com/opod-io/opod/internal/engines"
)

// RerankPath is the route every rerank-capable engine and an opod worker serve.
const RerankPath = "/v1/rerank"

// rerankBody is what the engine is sent. No top_n: the gateway truncates, and
// the engines disagree on it (llama.cpp keeps zero results for 0, SGLang
// refuses below 1). return_documents is false because the gateway attaches the
// text from its own request; vLLM returns it regardless and it is ignored.
type rerankBody struct {
	Model           string   `json:"model"`
	Query           string   `json:"query"`
	Documents       []string `json:"documents"`
	ReturnDocuments bool     `json:"return_documents"`
}

// rerankItem is one scored document in either shape: Jina/Cohere's
// relevance_score (vLLM, llama.cpp) or SGLang's score.
type rerankItem struct {
	Index          int      `json:"index"`
	RelevanceScore *float64 `json:"relevance_score"`
	Score          *float64 `json:"score"`
}

// Rerank posts req to the engine's rerank route and returns one score per
// document. A 501 from the engine — llama.cpp started without --reranking, or
// a worker whose engine has no rerank route — is engines.ErrRerankNotSupported
// with the engine's own words attached.
func Rerank(ctx context.Context, c Client, req engines.RerankRequest) (engines.RerankResponse, error) {
	if len(req.Documents) == 0 {
		return engines.RerankResponse{}, fmt.Errorf("%s: rerank needs at least one document", c.Driver)
	}
	raw, err := json.Marshal(rerankBody{Model: req.Model, Query: req.Query, Documents: req.Documents})
	if err != nil {
		return engines.RerankResponse{}, err
	}
	httpReq, err := http.NewRequestWithContext(ctx, http.MethodPost, c.BaseURL+RerankPath, bytes.NewReader(raw))
	if err != nil {
		return engines.RerankResponse{}, err
	}
	httpReq.Header.Set("Content-Type", "application/json")
	c.applyAuth(httpReq)

	resp, err := c.HTTP.Do(httpReq)
	if err != nil {
		return engines.RerankResponse{}, engines.Unreachable(c.Driver, c.BaseURL, err)
	}
	defer resp.Body.Close()
	op := "POST " + RerankPath
	if resp.StatusCode != http.StatusOK {
		b, _ := io.ReadAll(io.LimitReader(resp.Body, 4<<10))
		up := engines.Upstream(c.Driver, op, resp.StatusCode, b)
		if resp.StatusCode == http.StatusNotImplemented {
			return engines.RerankResponse{}, fmt.Errorf("%w: %w", engines.ErrRerankNotSupported, up)
		}
		return engines.RerankResponse{}, up
	}
	body, err := io.ReadAll(io.LimitReader(resp.Body, 16<<20))
	if err != nil {
		return engines.RerankResponse{}, engines.Upstream(c.Driver, op, resp.StatusCode, []byte("read: "+err.Error()))
	}
	items, usage, err := decodeRerank(body)
	if err != nil {
		return engines.RerankResponse{}, engines.Upstream(c.Driver, op, resp.StatusCode, []byte("decode: "+err.Error()))
	}
	out := engines.RerankResponse{Results: make([]engines.RerankResult, 0, len(items)), Usage: usage}
	seen := make([]bool, len(req.Documents))
	for _, it := range items {
		if it.Index < 0 || it.Index >= len(seen) {
			return engines.RerankResponse{}, engines.Upstream(c.Driver, op, resp.StatusCode,
				[]byte(fmt.Sprintf("result index %d is outside the %d documents", it.Index, len(seen))))
		}
		if seen[it.Index] {
			return engines.RerankResponse{}, engines.Upstream(c.Driver, op, resp.StatusCode,
				[]byte(fmt.Sprintf("result index %d returned twice", it.Index)))
		}
		seen[it.Index] = true
		score := it.RelevanceScore
		if score == nil {
			score = it.Score
		}
		if score == nil {
			return engines.RerankResponse{}, engines.Upstream(c.Driver, op, resp.StatusCode,
				[]byte(fmt.Sprintf("result index %d carries no score", it.Index)))
		}
		out.Results = append(out.Results, engines.RerankResult{Index: it.Index, Score: *score})
	}
	return out, nil
}

// decodeRerank reads either answer shape: an object with `results` (and
// `usage`), or SGLang's bare array.
func decodeRerank(body []byte) ([]rerankItem, *engines.Usage, error) {
	body = bytes.TrimSpace(body)
	if len(body) == 0 {
		return nil, nil, errors.New("empty body")
	}
	if body[0] == '[' {
		var items []rerankItem
		err := json.Unmarshal(body, &items)
		return items, nil, err
	}
	var obj struct {
		Results []rerankItem `json:"results"`
		Usage   *struct {
			PromptTokens int `json:"prompt_tokens"`
			TotalTokens  int `json:"total_tokens"`
		} `json:"usage"`
	}
	if err := json.Unmarshal(body, &obj); err != nil {
		return nil, nil, err
	}
	var u *engines.Usage
	if obj.Usage != nil {
		u = &engines.Usage{PromptTokens: obj.Usage.PromptTokens, TotalTokens: obj.Usage.TotalTokens}
	}
	return obj.Results, u, nil
}
