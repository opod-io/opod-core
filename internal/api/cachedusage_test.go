package api

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/opod-io/opod/internal/engines"
)

// cachedEngine answers from its prefix cache and says how much.
type cachedEngine struct{ cancelEngine }

func (e *cachedEngine) Chat(context.Context, engines.ChatRequest) (<-chan engines.StreamEvent, error) {
	out := make(chan engines.StreamEvent, 2)
	out <- engines.StreamEvent{Delta: "ok"}
	out <- engines.StreamEvent{Done: true, Reason: "stop", Usage: &engines.Usage{PromptTokens: 1838, CompletionTokens: 1, TotalTokens: 1839, CachedTokens: 1824}}
	close(out)
	return out, nil
}

// The engine's prefix-cache count reaches the CLIENT, in OpenAI's place
// (usage.prompt_tokens_details.cached_tokens), streamed or not. It reached the
// usage record (T32.47) while every answer's usage carried the three counts
// only (design-partner cell, 2026-11-06: the record read 1 824 cached tokens,
// the answer none).
func TestTheAnswerCarriesTheEnginesCachedTokens(t *testing.T) {
	for _, stream := range []bool{false, true} {
		h := &Handler{Engine: &cachedEngine{}, Store: usageTestStore(t), Usage: newUsageSink().add, Default: "m"}
		body := `{"model":"m","messages":[{"role":"user","content":"hi"}]` + map[bool]string{true: `,"stream":true,"stream_options":{"include_usage":true}}`, false: `}`}[stream]
		rec := httptest.NewRecorder()
		h.ChatCompletions(rec, httptest.NewRequest(http.MethodPost, "/v1/chat/completions", strings.NewReader(body)))
		if rec.Code != http.StatusOK {
			t.Fatalf("stream=%v: status %d: %s", stream, rec.Code, rec.Body)
		}
		raw := rec.Body.String()
		if stream {
			// The usage rides the final chunk before [DONE].
			lines := strings.Split(strings.TrimSpace(raw), "\n\n")
			raw = strings.TrimPrefix(lines[len(lines)-2], "data: ")
		}
		var got struct {
			Usage struct {
				PromptTokensDetails *struct {
					CachedTokens int `json:"cached_tokens"`
				} `json:"prompt_tokens_details"`
			} `json:"usage"`
		}
		if err := json.Unmarshal([]byte(raw), &got); err != nil {
			t.Fatalf("stream=%v: %v in %s", stream, err, raw)
		}
		if got.Usage.PromptTokensDetails == nil || got.Usage.PromptTokensDetails.CachedTokens != 1824 {
			t.Fatalf("stream=%v: the answer's usage has no cached tokens: %s", stream, raw)
		}
	}
	// An engine that reports none: the field is absent, not a 0.
	if b, _ := json.Marshal(openAIUsage(&engines.Usage{PromptTokens: 3})); strings.Contains(string(b), "prompt_tokens_details") {
		t.Fatalf("no count, no field: %s", b)
	}
}
