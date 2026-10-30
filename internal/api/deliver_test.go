package api

// An answer that is not streamed is the client's the moment it is complete.
// The usage row is the gateway's bookkeeping and is written after it: these
// tests hold the write on a gate and require the whole body to have reached
// the client while the gate is still shut (PLAN T32.1).

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/opod-io/opod/internal/engines"
	"github.com/opod-io/opod/internal/store"
)

// gatedStore holds every usage write until its gate opens.
type gatedStore struct {
	store.Store
	gate chan struct{}
}

func (s gatedStore) Usage() store.UsageStore { return gatedUsage{s.Store.Usage(), s.gate} }

type gatedUsage struct {
	store.UsageStore
	gate chan struct{}
}

func (u gatedUsage) Record(ctx context.Context, rec store.Usage) error {
	<-u.gate
	return u.UsageStore.Record(ctx, rec)
}

// everythingEngine answers chat, embeddings and rerank in one go.
type everythingEngine struct{ finishingEngine }

func (e *everythingEngine) Embed(context.Context, engines.EmbedRequest) (engines.EmbedResponse, error) {
	return engines.EmbedResponse{Vectors: [][]float32{{0.5}}, Usage: &engines.Usage{PromptTokens: 2, TotalTokens: 2}}, nil
}

func (e *everythingEngine) Rerank(_ context.Context, req engines.RerankRequest) (engines.RerankResponse, error) {
	out := engines.RerankResponse{Usage: &engines.Usage{PromptTokens: 3, TotalTokens: 3}}
	for i := range req.Documents {
		out.Results = append(out.Results, engines.RerankResult{Index: i, Score: float64(i)})
	}
	return out, nil
}

func TestANonStreamedAnswerIsDeliveredBeforeItsUsageRow(t *testing.T) {
	cases := []struct {
		name, body, want string
		route            func(h *Handler) http.HandlerFunc
	}{
		{"chat", `{"model":"m","messages":[{"role":"user","content":"hi"}]}`, `"content":"Hello"`,
			func(h *Handler) http.HandlerFunc { return h.ChatCompletions }},
		{"embeddings", `{"model":"m","input":"hi"}`, `"embedding"`,
			func(h *Handler) http.HandlerFunc { return h.Embeddings }},
		{"rerank", `{"model":"m","query":"q","documents":["a","b"]}`, `"relevance_score"`,
			func(h *Handler) http.HandlerFunc { return h.Rerank }},
		// The Messages route wraps the chat handler in a writer that buffers
		// the whole answer to translate it; it too must hand it over first.
		{"anthropic", `{"model":"m","messages":[{"role":"user","content":"hi"}]}`, `"type":"message"`,
			func(h *Handler) http.HandlerFunc {
				return func(w http.ResponseWriter, r *http.Request) {
					aw := NewAnthropicWriter(w, "m")
					defer aw.Finish()
					h.ChatCompletions(aw, r)
				}
			}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			st := usageTestStore(t)
			gate := make(chan struct{})
			h := &Handler{Engine: &everythingEngine{finishingEngine{cancelEngine{deltas: []string{"Hel", "lo"}}}},
				Store: gatedStore{st, gate}, Default: "m"}
			srv := httptest.NewServer(tc.route(h))
			defer srv.Close()
			defer func() {
				select {
				case <-gate:
				default:
					close(gate)
				}
			}()

			got := make(chan string, 1)
			go func() {
				resp, err := http.Post(srv.URL, "application/json", strings.NewReader(tc.body))
				if err != nil {
					got <- "POST: " + err.Error()
					return
				}
				defer resp.Body.Close()
				// A JSON body is complete when it decodes; reading to EOF would
				// wait for the handler to return, which is the thing under test.
				var v json.RawMessage
				if err := json.NewDecoder(resp.Body).Decode(&v); err != nil {
					got <- "decode: " + err.Error()
					return
				}
				_, _ = io.Copy(io.Discard, io.LimitReader(resp.Body, 0))
				got <- string(v)
			}()

			select {
			case body := <-got:
				if !strings.Contains(body, tc.want) {
					t.Fatalf("body = %s, want it to contain %s", body, tc.want)
				}
			case <-time.After(3 * time.Second):
				t.Fatal("the client had no answer while its usage row was still being written")
			}
			close(gate)
			if row := waitForUsage(t, st); row.Outcome != "ok" {
				t.Fatalf("usage row outcome = %q, want ok", row.Outcome)
			}
		})
	}
}
