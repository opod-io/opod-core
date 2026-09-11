package openaicompat

// Embeddings over the OpenAI wire. vLLM, llama-server and every other
// OpenAI-compatible server expose POST /v1/embeddings with the same shape, so
// one implementation here gives every driver that embeds Client its embeddings
// support — the drivers add nothing.

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"

	"github.com/opod-io/opod/internal/engines"
)

// embedBody is the request the OpenAI embeddings route takes. Input is an
// array even for a single string: servers accept both, and the array form
// keeps one code path and preserves the caller's order.
type embedBody struct {
	Model string   `json:"model"`
	Input []string `json:"input"`
}

// embedResponse is the subset of the reply we use. Servers return the vectors
// out of order often enough that Index is authoritative, not the array position.
type embedResponse struct {
	Data []struct {
		Index     int       `json:"index"`
		Embedding []float32 `json:"embedding"`
	} `json:"data"`
	Usage *struct {
		PromptTokens int `json:"prompt_tokens"`
		TotalTokens  int `json:"total_tokens"`
	} `json:"usage"`
}

// Embed returns one vector per input, in the caller's order.
func (c Client) Embed(ctx context.Context, req engines.EmbedRequest) (engines.EmbedResponse, error) {
	// Asking to embed nothing is a caller mistake, and every driver reports it
	// the same way (see enginetest). Answering with an empty array instead would
	// hide the mistake one layer further from where it was made.
	if len(req.Inputs) == 0 {
		return engines.EmbedResponse{}, fmt.Errorf("%s: embed needs at least one input", c.Driver)
	}

	raw, err := json.Marshal(embedBody{Model: req.Model, Input: req.Inputs})
	if err != nil {
		return engines.EmbedResponse{}, err
	}
	httpReq, err := http.NewRequestWithContext(ctx, http.MethodPost, c.BaseURL+"/v1/embeddings", bytes.NewReader(raw))
	if err != nil {
		return engines.EmbedResponse{}, err
	}
	httpReq.Header.Set("Content-Type", "application/json")
	c.applyAuth(httpReq)

	resp, err := c.HTTP.Do(httpReq)
	if err != nil {
		return engines.EmbedResponse{}, engines.Unreachable(c.Driver, c.BaseURL, err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		b, _ := io.ReadAll(io.LimitReader(resp.Body, 4<<10))
		return engines.EmbedResponse{}, engines.Upstream(c.Driver, "POST /v1/embeddings", resp.StatusCode, b)
	}

	var out embedResponse
	if err := json.NewDecoder(resp.Body).Decode(&out); err != nil {
		return engines.EmbedResponse{}, engines.Upstream(c.Driver, "POST /v1/embeddings", resp.StatusCode, []byte("decode: "+err.Error()))
	}
	if len(out.Data) != len(req.Inputs) {
		return engines.EmbedResponse{}, engines.Upstream(c.Driver, "POST /v1/embeddings", resp.StatusCode,
			[]byte(fmt.Sprintf("asked for %d embeddings, got %d", len(req.Inputs), len(out.Data))))
	}

	vectors := make([][]float32, len(out.Data))
	for _, d := range out.Data {
		if d.Index < 0 || d.Index >= len(vectors) {
			return engines.EmbedResponse{}, engines.Upstream(c.Driver, "POST /v1/embeddings", resp.StatusCode,
				[]byte(fmt.Sprintf("embedding index %d is outside the %d inputs", d.Index, len(vectors))))
		}
		if vectors[d.Index] != nil {
			return engines.EmbedResponse{}, engines.Upstream(c.Driver, "POST /v1/embeddings", resp.StatusCode,
				[]byte(fmt.Sprintf("embedding index %d returned twice", d.Index)))
		}
		vectors[d.Index] = d.Embedding
	}

	res := engines.EmbedResponse{Vectors: vectors}
	if out.Usage != nil {
		res.Usage = &engines.Usage{PromptTokens: out.Usage.PromptTokens, TotalTokens: out.Usage.TotalTokens}
	}
	return res, nil
}
