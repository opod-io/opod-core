package api

// A worker whose own engine fails after the worker has answered 200 ends its
// stream with an in-band error chunk (`data: {"error":{...}}`, agent.writeSSE)
// and then closes cleanly. The leader's stream reader skipped that chunk and
// synthesized a normal end from the close, so the caller got 200 with an
// empty (or cut-short) answer and the usage row said "ok" (PLAN T8.18). A
// silent wrong answer is worse than an error: the caller must see one, and the
// row must say "error".

import (
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/opod-io/opod/internal/engines"
	_ "github.com/opod-io/opod/internal/engines/all"
)

const roleChunk = `data: {"choices":[{"index":0,"delta":{"role":"assistant"},"finish_reason":null}]}` + "\n\n"

// workerAnswer serves the bytes a worker writes for one streamed chat: the
// role chunk, the given chunks, and a clean end of body.
func workerAnswer(t *testing.T, chunks ...string) string {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		_, _ = io.WriteString(w, roleChunk)
		for _, c := range chunks {
			_, _ = io.WriteString(w, c)
		}
	}))
	t.Cleanup(srv.Close)
	return srv.URL
}

func chatThrough(t *testing.T, workerURL string, stream bool) (*httptest.ResponseRecorder, string) {
	t.Helper()
	st := usageTestStore(t)
	h := &Handler{Engine: engines.MustNew("llamacpp", workerURL, ""), Store: st, Default: "m"}
	body := `{"model":"m","stream":false,"messages":[{"role":"user","content":"hi"}]}`
	if stream {
		body = strings.Replace(body, `"stream":false`, `"stream":true`, 1)
	}
	rec := httptest.NewRecorder()
	h.ChatCompletions(rec, httptest.NewRequest(http.MethodPost, "/v1/chat/completions", strings.NewReader(body)))
	return rec, waitForUsage(t, st).Outcome
}

const engineBroke = `data: {"error":{"message":"stream: unexpected EOF","type":"stream_broken"}}` + "\n\n"

func TestAWorkersInBandErrorAfterTheFirstTokenReachesTheCaller(t *testing.T) {
	url := workerAnswer(t, `data: {"choices":[{"delta":{"content":"half an ans"}}]}`+"\n\n", engineBroke)

	t.Run("stream", func(t *testing.T) {
		rec, outcome := chatThrough(t, url, true)
		out := rec.Body.String()
		if !strings.Contains(out, `"half an ans"`) {
			t.Fatalf("what was sent before the error still reaches the caller: %s", out)
		}
		if !strings.Contains(out, `data: {"error":{"message":`) || !strings.Contains(out, "unexpected EOF") {
			t.Fatalf("the stream must end with an error event carrying the engine's break, got: %s", out)
		}
		if strings.Contains(out, `"finish_reason":"stop"`) {
			t.Fatalf("an engine that broke must not look like a finished answer: %s", out)
		}
		if outcome != "error" {
			t.Fatalf("usage outcome = %q, want error", outcome)
		}
	})
	t.Run("non-stream", func(t *testing.T) {
		rec, outcome := chatThrough(t, url, false)
		if rec.Code != http.StatusBadGateway || !strings.Contains(rec.Body.String(), "unexpected EOF") {
			t.Fatalf("want 502 carrying the engine's break, got %d %s", rec.Code, rec.Body.String())
		}
		if outcome != "error" {
			t.Fatalf("usage outcome = %q, want error", outcome)
		}
	})
}

// A worker older than the chunk's type sends only a message: still an error,
// never a clean end — it is shown, not replayed.
func TestAnUntypedInBandErrorIsStillAnError(t *testing.T) {
	url := workerAnswer(t, `data: {"error":{"message":"engine: out of memory"}}`+"\n\n")
	rec, outcome := chatThrough(t, url, false)
	if rec.Code != http.StatusBadGateway || !strings.Contains(rec.Body.String(), "out of memory") {
		t.Fatalf("want 502 carrying the engine's message, got %d %s", rec.Code, rec.Body.String())
	}
	if strings.Contains(rec.Body.String(), "llama-server") {
		t.Fatalf("an engine that answered is not the leader's engine to start: %s", rec.Body.String())
	}
	if outcome != "error" {
		t.Fatalf("usage outcome = %q, want error", outcome)
	}
}

// A worker that sends no error chunk is read as before: its answer, and "ok".
func TestAWorkerWithoutAnInBandErrorAnswersAsBefore(t *testing.T) {
	url := workerAnswer(t, `data: {"choices":[{"delta":{"content":"hello"}}]}`+"\n\n",
		`data: {"choices":[{"delta":{},"finish_reason":"stop"}]}`+"\n\n", "data: [DONE]\n\n")
	for _, stream := range []bool{true, false} {
		rec, outcome := chatThrough(t, url, stream)
		if rec.Code != http.StatusOK || !strings.Contains(rec.Body.String(), "hello") || strings.Contains(rec.Body.String(), `"error"`) {
			t.Fatalf("stream=%v: want 200 with the answer, got %d %s", stream, rec.Code, rec.Body.String())
		}
		if outcome != "ok" {
			t.Fatalf("stream=%v: usage outcome = %q, want ok", stream, outcome)
		}
	}
}
