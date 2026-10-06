package api

import (
	"encoding/json"
	"errors"
	"net/http"
	"sort"
	"time"

	"github.com/opod-io/opod/internal/engines"
	"github.com/opod-io/opod/internal/models"
	"github.com/opod-io/opod/internal/router"
)

// ---- /v1/rerank ----
//
// Rerank returned to core with ADR-084 (amending ADR-022). The shape is the
// one vLLM, llama.cpp and the Jina/Cohere clients share:
//
//	{
//	  "model": "bge-reranker-v2-m3",
//	  "query": "what is a gang?",
//	  "documents": ["one model split across GPUs", "a group of people"],
//	  "top_n": 1,                 // optional; 0 or absent = every document
//	  "return_documents": true    // optional; default true, as vLLM and SGLang
//	}
//
// Response, most relevant first:
//
//	{
//	  "id": "rerank-…", "model": "bge-reranker-v2-m3",
//	  "results": [{"index": 0, "relevance_score": 0.97, "document": {"text": "one model split across GPUs"}}],
//	  "usage": {"prompt_tokens": 21, "total_tokens": 21}
//	}
//
// The engine is a scorer only (engines.RerankRequest): ordering, top_n and the
// document text are applied here, so every engine answers the caller alike.
// Never streamed, never cached.

type rerankRequest struct {
	Model           string   `json:"model"`
	Query           string   `json:"query"`
	Documents       []string `json:"documents"`
	TopN            int      `json:"top_n,omitempty"`
	ReturnDocuments *bool    `json:"return_documents,omitempty"`
}

type rerankDocument struct {
	Text string `json:"text"`
}

type rerankResult struct {
	Index          int             `json:"index"`
	RelevanceScore float64         `json:"relevance_score"`
	Document       *rerankDocument `json:"document,omitempty"`
}

type rerankResponse struct {
	ID      string         `json:"id"`
	Model   string         `json:"model"`
	Results []rerankResult `json:"results"`
	Usage   usage          `json:"usage"`
}

// Rerank handles POST /v1/rerank.
func (h *Handler) Rerank(w http.ResponseWriter, r *http.Request) {
	defer r.Body.Close()
	body, r, err := requestBody(r)
	if err != nil {
		BodyReadError(w, err)
		return
	}
	// The query and the documents are content, so pre-call guardrails see
	// them, exactly as they see an embedding's input.
	body, ok := h.applyPreCallGuardrails(r.Context(), w, body)
	if !ok {
		return
	}
	var req rerankRequest
	if err := json.Unmarshal(body, &req); err != nil {
		writeJSONError(w, http.StatusBadRequest, "invalid_request", "Invalid JSON body: "+err.Error())
		return
	}
	switch {
	case req.Query == "":
		writeJSONError(w, http.StatusBadRequest, "invalid_request", "query is required")
		return
	case len(req.Documents) == 0:
		writeJSONError(w, http.StatusBadRequest, "invalid_request", "documents must be a non-empty array of strings")
		return
	case req.TopN < 0:
		writeJSONError(w, http.StatusBadRequest, "invalid_request", "top_n must be zero (every document) or more")
		return
	}

	re, ok := h.Engine.(engines.RerankEngine)
	if !ok {
		writeJSONError(w, http.StatusNotImplemented, "rerank_not_supported",
			"the configured engine does not serve rerank")
		return
	}
	requested, sortHint := models.SplitSortSuffix(req.Model)
	if requested == "" {
		writeJSONError(w, http.StatusBadRequest, "invalid_request", "model is required")
		return
	}
	// Validate the model, route by its CATALOG id (Embeddings explains why).
	if _, err := h.ResolveModel(requested); err != nil {
		writeJSONError(w, http.StatusNotFound, "model_not_found", err.Error())
		return
	}

	r = r.WithContext(router.WithTrace(r.Context())) // which worker served it → usage row
	ctx := overridesContext(r, nil, h.Store, requested, sortHint)
	start := time.Now()
	res, err := re.Rerank(ctx, engines.RerankRequest{Model: requested, Query: req.Query, Documents: req.Documents})
	if err != nil {
		h.recordUsage(r.Context(), "openai", requested, nil, time.Since(start), "error")
		writeRerankError(w, h.Engine, router.NodeFrom(ctx), err)
		return
	}

	// Most relevant first; ties keep the caller's order, so the answer is
	// deterministic for a given set of scores.
	sort.SliceStable(res.Results, func(i, j int) bool {
		if res.Results[i].Score != res.Results[j].Score {
			return res.Results[i].Score > res.Results[j].Score
		}
		return res.Results[i].Index < res.Results[j].Index
	})
	if req.TopN > 0 && req.TopN < len(res.Results) {
		res.Results = res.Results[:req.TopN]
	}
	withText := req.ReturnDocuments == nil || *req.ReturnDocuments
	out := rerankResponse{
		ID:      "rerank-" + randID(),
		Model:   requested,
		Results: make([]rerankResult, 0, len(res.Results)),
	}
	for _, x := range res.Results {
		item := rerankResult{Index: x.Index, RelevanceScore: x.Score}
		if withText {
			item.Document = &rerankDocument{Text: req.Documents[x.Index]}
		}
		out.Results = append(out.Results, item)
	}
	var u *engines.Usage
	if res.Usage != nil {
		out.Usage = usage{PromptTokens: res.Usage.PromptTokens, TotalTokens: res.Usage.TotalTokens}
		u = &engines.Usage{PromptTokens: res.Usage.PromptTokens, TotalTokens: res.Usage.TotalTokens}
	}
	h.recordUsage(r.Context(), "openai", requested, u, time.Since(start), "ok")
	writeJSON(w, http.StatusOK, out)
}

// writeRerankError answers a failed rerank. An engine without a rerank route
// is a named 501, never the 404 its server would give; the rest is the same
// ladder the chat and embeddings routes use.
func writeRerankError(w http.ResponseWriter, eng engines.Engine, node string, err error) {
	if errors.Is(err, engines.ErrRerankNotSupported) {
		writeJSONError(w, http.StatusNotImplemented, "rerank_not_supported", err.Error())
		return
	}
	if msg, gone := workerGone(node, err); gone {
		w.Header().Set("Retry-After", "10")
		writeJSONError(w, http.StatusServiceUnavailable, "worker_unreachable", msg)
		return
	}
	if msg, gone := gangGone(node, err); gone {
		w.Header().Set("Retry-After", "10")
		writeJSONError(w, http.StatusServiceUnavailable, "gang_unreachable", msg)
		return
	}
	if status, code, msg, ok := upstreamPassthrough(err); ok {
		if status == http.StatusServiceUnavailable || status == http.StatusTooManyRequests {
			w.Header().Set("Retry-After", "10")
		}
		writeJSONError(w, status, code, msg)
		return
	}
	code, msg := classifyEngineError(eng, err)
	writeJSONError(w, http.StatusBadGateway, code, msg)
}
