package openaicompat

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/opod-io/opod/internal/engines"
)

func embedServer(t *testing.T, handler http.HandlerFunc) Client {
	t.Helper()
	srv := httptest.NewServer(handler)
	t.Cleanup(srv.Close)
	return NewClient("test", srv.URL, nil)
}

func TestEmbedReturnsVectorsInCallerOrder(t *testing.T) {
	// Servers are free to answer out of order; Index decides where a vector goes.
	c := embedServer(t, func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/v1/embeddings" || r.Method != http.MethodPost {
			t.Errorf("want POST /v1/embeddings, got %s %s", r.Method, r.URL.Path)
		}
		var body embedBody
		if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
			t.Fatal(err)
		}
		if len(body.Input) != 2 || body.Model != "m" {
			t.Errorf("body did not carry the request: %+v", body)
		}
		_, _ = w.Write([]byte(`{"data":[
			{"index":1,"embedding":[0.3,0.4]},
			{"index":0,"embedding":[0.1,0.2]}],
			"usage":{"prompt_tokens":7,"total_tokens":7}}`))
	})

	got, err := c.Embed(context.Background(), engines.EmbedRequest{Model: "m", Inputs: []string{"first", "second"}})
	if err != nil {
		t.Fatal(err)
	}
	if len(got.Vectors) != 2 || got.Vectors[0][0] != 0.1 || got.Vectors[1][0] != 0.3 {
		t.Fatalf("vectors must follow the caller's order: %v", got.Vectors)
	}
	if got.Usage == nil || got.Usage.PromptTokens != 7 {
		t.Fatalf("usage not carried: %+v", got.Usage)
	}
}

func TestEmbedNoInputsIsRefusedWithoutCallingTheEngine(t *testing.T) {
	// Every driver reports this the same way (see enginetest): embedding nothing
	// is a caller mistake, and the engine is not troubled with it.
	c := embedServer(t, func(http.ResponseWriter, *http.Request) {
		t.Error("an empty request must not reach the engine")
	})
	if _, err := c.Embed(context.Background(), engines.EmbedRequest{Model: "m"}); err == nil {
		t.Fatal("embedding no inputs must be an error")
	}
}

func TestEmbedRejectsAShortOrDuplicateAnswer(t *testing.T) {
	for name, payload := range map[string]string{
		"fewer than asked":  `{"data":[{"index":0,"embedding":[0.1]}]}`,
		"same index twice":  `{"data":[{"index":0,"embedding":[0.1]},{"index":0,"embedding":[0.2]}]}`,
		"index off the end": `{"data":[{"index":0,"embedding":[0.1]},{"index":9,"embedding":[0.2]}]}`,
	} {
		t.Run(name, func(t *testing.T) {
			c := embedServer(t, func(w http.ResponseWriter, _ *http.Request) { _, _ = w.Write([]byte(payload)) })
			if _, err := c.Embed(context.Background(), engines.EmbedRequest{Model: "m", Inputs: []string{"a", "b"}}); err == nil {
				t.Fatal("a mismatched answer must be an error, not a silent short result")
			}
		})
	}
}

func TestEmbedSurfacesAnUpstreamFailure(t *testing.T) {
	c := embedServer(t, func(w http.ResponseWriter, _ *http.Request) {
		http.Error(w, "model does not support embeddings", http.StatusBadRequest)
	})
	_, err := c.Embed(context.Background(), engines.EmbedRequest{Model: "m", Inputs: []string{"a"}})
	if err == nil {
		t.Fatal("a 400 from the engine must reach the caller")
	}
}
