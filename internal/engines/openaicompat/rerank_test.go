package openaicompat

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"testing"

	"github.com/opod-io/opod/internal/engines"
)

// vLLM and llama.cpp answer an object with `results`; the engine is asked for
// scores only — no top_n (llama.cpp keeps zero results for 0) and no text.
func TestRerankReadsTheResultsObject(t *testing.T) {
	c := embedServer(t, func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/v1/rerank" || r.Method != http.MethodPost {
			t.Errorf("want POST /v1/rerank, got %s %s", r.Method, r.URL.Path)
		}
		var body map[string]json.RawMessage
		if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
			t.Fatal(err)
		}
		if _, sent := body["top_n"]; sent {
			t.Errorf("top_n reached the engine: %s", body["top_n"])
		}
		if string(body["return_documents"]) != "false" || string(body["query"]) != `"q"` {
			t.Errorf("body did not carry the request as a scorer: %v", body)
		}
		_, _ = w.Write([]byte(`{"model":"m","results":[
			{"index":1,"relevance_score":0.9,"document":{"text":"b"}},
			{"index":0,"relevance_score":0.1}],
			"usage":{"prompt_tokens":11,"total_tokens":11}}`))
	})
	got, err := Rerank(context.Background(), c, engines.RerankRequest{Model: "m", Query: "q", Documents: []string{"a", "b"}})
	if err != nil {
		t.Fatal(err)
	}
	if len(got.Results) != 2 || got.Results[0] != (engines.RerankResult{Index: 1, Score: 0.9}) {
		t.Fatalf("results: %+v", got.Results)
	}
	if got.Usage == nil || got.Usage.PromptTokens != 11 {
		t.Fatalf("usage not carried: %+v", got.Usage)
	}
}

// SGLang answers a bare array with `score`.
func TestRerankReadsSGLangsBareArray(t *testing.T) {
	c := embedServer(t, func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write([]byte(`[{"score":0.7,"index":0},{"score":0.2,"index":1,"meta_info":{}}]`))
	})
	got, err := Rerank(context.Background(), c, engines.RerankRequest{Model: "m", Query: "q", Documents: []string{"a", "b"}})
	if err != nil {
		t.Fatal(err)
	}
	if len(got.Results) != 2 || got.Results[0].Score != 0.7 || got.Results[1].Index != 1 {
		t.Fatalf("results: %+v", got.Results)
	}
}

// llama-server started without --reranking answers 501: that is a named
// refusal the gateway turns into rerank_not_supported, not a bad gateway.
func TestRerank501IsNotSupported(t *testing.T) {
	c := embedServer(t, func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusNotImplemented)
		_, _ = w.Write([]byte(`{"error":{"code":501,"message":"This server does not support reranking. Start it with` + " `--reranking`" + `","type":"not_supported_error"}}`))
	})
	_, err := Rerank(context.Background(), c, engines.RerankRequest{Model: "m", Query: "q", Documents: []string{"a"}})
	if !errors.Is(err, engines.ErrRerankNotSupported) {
		t.Fatalf("err = %v, want ErrRerankNotSupported", err)
	}
	var up *engines.UpstreamError
	if !errors.As(err, &up) || up.Status != http.StatusNotImplemented {
		t.Fatalf("the engine's own words were lost: %v", err)
	}
}

// An answer that does not match the request is the engine's fault, said so.
func TestRerankRejectsABadAnswer(t *testing.T) {
	for name, answer := range map[string]string{
		"out of range": `{"results":[{"index":5,"relevance_score":1}]}`,
		"twice":        `{"results":[{"index":0,"relevance_score":1},{"index":0,"relevance_score":2}]}`,
		"no score":     `{"results":[{"index":0}]}`,
	} {
		t.Run(name, func(t *testing.T) {
			c := embedServer(t, func(w http.ResponseWriter, r *http.Request) { _, _ = w.Write([]byte(answer)) })
			_, err := Rerank(context.Background(), c, engines.RerankRequest{Model: "m", Query: "q", Documents: []string{"a", "b"}})
			if !errors.Is(err, engines.ErrUpstream) {
				t.Fatalf("err = %v, want an upstream error", err)
			}
		})
	}
	if _, err := Rerank(context.Background(), Client{Driver: "x"}, engines.RerankRequest{Query: "q"}); err == nil {
		t.Fatal("no documents must be refused before any call")
	}
}
