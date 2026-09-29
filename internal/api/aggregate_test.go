package api

import (
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/opod-io/opod/internal/engines"
)

// deltaStream is a finished stream of n deltas, each `piece`, then a Done event.
func deltaStream(n int, piece string) <-chan engines.StreamEvent {
	ch := make(chan engines.StreamEvent, n+1)
	for i := 0; i < n; i++ {
		ch <- engines.StreamEvent{Delta: piece}
	}
	ch <- engines.StreamEvent{Done: true, Reason: "stop", Usage: &engines.Usage{PromptTokens: 1, CompletionTokens: n, TotalTokens: n + 1}}
	close(ch)
	return ch
}

// The non-streamed answer is every delta in order, nothing dropped and nothing
// doubled — the property the strings.Builder rewrite (PLAN T15.1) must keep.
func TestAggregateResponseIsEveryDeltaInOrder(t *testing.T) {
	h := &Handler{Store: usageTestStore(t), Default: "m"}
	const n = 300
	stream := make(chan engines.StreamEvent, n+1)
	var want strings.Builder
	for i := 0; i < n; i++ {
		piece := fmt.Sprintf("tok%d ", i)
		want.WriteString(piece)
		stream <- engines.StreamEvent{Delta: piece}
	}
	stream <- engines.StreamEvent{Done: true, Reason: "length", Usage: &engines.Usage{PromptTokens: 2, CompletionTokens: n, TotalTokens: n + 2}}
	close(stream)

	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodPost, "/v1/chat/completions", nil)
	h.aggregateResponse(rec, req, stream, "chatcmpl-1", 1, "m", time.Now())
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d: %s", rec.Code, rec.Body.String())
	}
	var resp struct {
		Choices []struct {
			Message      struct{ Content string } `json:"message"`
			FinishReason string                   `json:"finish_reason"`
		} `json:"choices"`
		Usage struct {
			TotalTokens int `json:"total_tokens"`
		} `json:"usage"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &resp); err != nil {
		t.Fatalf("body is not JSON: %v\n%s", err, rec.Body.String())
	}
	if len(resp.Choices) != 1 || resp.Choices[0].Message.Content != want.String() {
		t.Fatalf("content is not the deltas in order")
	}
	if resp.Choices[0].FinishReason != "length" || resp.Usage.TotalTokens != n+2 {
		t.Fatalf("finish/usage lost: %+v", resp)
	}
}

// BenchmarkAggregateResponse — B/op is the figure: with `text +=` the
// assembly allocated the sum of every partial answer (quadratic, ~10 MB at
// 2000 tokens of 5 bytes); a Builder allocates about the answer once.
func BenchmarkAggregateResponse(b *testing.B) {
	h := &Handler{Store: usageTestStore(b), Default: "m"}
	req := httptest.NewRequest(http.MethodPost, "/v1/chat/completions", nil)
	b.ReportAllocs()
	for i := 0; i < b.N; i++ {
		b.StopTimer()
		stream := deltaStream(2000, "token")
		b.StartTimer()
		h.aggregateResponse(httptest.NewRecorder(), req, stream, "id", 1, "m", time.Now())
	}
}
