package api

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/opod-io/opod/internal/engines"
	"github.com/opod-io/opod/internal/engines/openaicompat"
)

// recordingEngine keeps the request it was handed and answers one token with
// logprobs, the way an OpenAI-shaped server does when asked for them.
type recordingEngine struct {
	cancelEngine
	got   *engines.ChatRequest
	calls int
}

func (e *recordingEngine) Chat(_ context.Context, req engines.ChatRequest) (<-chan engines.StreamEvent, error) {
	e.got = &req
	e.calls++
	out := make(chan engines.StreamEvent, 3)
	out <- engines.StreamEvent{Delta: "Hi", Logprobs: json.RawMessage(`{"content":[{"token":"Hi","logprob":-0.5}]}`)}
	out <- engines.StreamEvent{Delta: "!", Logprobs: json.RawMessage(`{"content":[{"token":"!","logprob":-0.25}]}`)}
	out <- engines.StreamEvent{Done: true, Reason: "stop"}
	close(out)
	return out, nil
}

func postChat(t *testing.T, h *Handler, body string) *httptest.ResponseRecorder {
	t.Helper()
	rec := httptest.NewRecorder()
	h.ChatCompletions(rec, httptest.NewRequest(http.MethodPost, "/v1/chat/completions", strings.NewReader(body)))
	return rec
}

// A request carrying seed, logprobs and a field no OpenAI revision has yet
// reaches the engine body intact, and our own routing bag does not (T17.1).
// The body checked is the one BuildChatBody makes — what a worker, or a
// vLLM/SGLang/llama.cpp server, receives.
func TestTheCallersFieldsReachTheEngineBody(t *testing.T) {
	eng := &recordingEngine{}
	h := &Handler{Engine: eng, Store: usageTestStore(t), Default: "m"}

	rec := postChat(t, h, `{"model":"m","messages":[{"role":"user","content":"hi"}],
		"seed":7,"logprobs":true,"top_logprobs":2,"user":"u-1","a_field_from_2027":{"x":[1]},
		"opod":{"num_retries":1}}`)
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d: %s", rec.Code, rec.Body.String())
	}
	raw, _ := json.Marshal(openaicompat.BuildChatBody(*eng.got))
	var body map[string]json.RawMessage
	if err := json.Unmarshal(raw, &body); err != nil {
		t.Fatal(err)
	}
	for k, want := range map[string]string{
		"seed": `7`, "logprobs": `true`, "top_logprobs": `2`, "user": `"u-1"`, "a_field_from_2027": `{"x":[1]}`,
	} {
		if string(body[k]) != want {
			t.Errorf("engine body %s = %s, want %s (body %s)", k, body[k], want, raw)
		}
	}
	if _, leaked := body["opod"]; leaked {
		t.Errorf("the opod routing bag reached the engine: %s", raw)
	}
	// The response half: the logprobs the engine produced come back merged.
	var resp struct {
		Choices []struct {
			Logprobs struct {
				Content []struct {
					Token string `json:"token"`
				} `json:"content"`
			} `json:"logprobs"`
		} `json:"choices"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &resp); err != nil {
		t.Fatal(err)
	}
	if lp := resp.Choices[0].Logprobs.Content; len(lp) != 2 || lp[0].Token != "Hi" || lp[1].Token != "!" {
		t.Fatalf("logprobs were not carried back: %s", rec.Body.String())
	}
}

// A streamed answer carries each chunk's logprobs beside its content.
func TestStreamedLogprobsRideTheirChunk(t *testing.T) {
	h := &Handler{Engine: &recordingEngine{}, Store: usageTestStore(t), Default: "m"}
	rec := postChat(t, h, `{"model":"m","stream":true,"logprobs":true,"messages":[{"role":"user","content":"hi"}]}`)
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d: %s", rec.Code, rec.Body.String())
	}
	if !strings.Contains(rec.Body.String(), `"delta":{"content":"Hi"},"logprobs":{"content":[{"token":"Hi","logprob":-0.5}]}`) {
		t.Fatalf("the chunk lost its logprobs:\n%s", rec.Body.String())
	}
}

// n > 1 and best_of > 1 are refused, never forwarded: the stream reader keeps
// choices[0] only, so they would be generated and billed several times and
// answered once (T17.1).
func TestMoreThanOneChoiceIsRefused(t *testing.T) {
	for _, tc := range []struct{ name, extra string }{
		{"n", `"n":2`},
		{"best_of", `"best_of":3`},
	} {
		t.Run(tc.name, func(t *testing.T) {
			eng := &recordingEngine{}
			h := &Handler{Engine: eng, Store: usageTestStore(t), Default: "m"}
			rec := postChat(t, h, `{"model":"m","messages":[{"role":"user","content":"hi"}],`+tc.extra+`}`)
			if rec.Code != http.StatusBadRequest {
				t.Fatalf("status = %d, want 400: %s", rec.Code, rec.Body.String())
			}
			var env struct {
				Error struct{ Type, Message string } `json:"error"`
			}
			_ = json.Unmarshal(rec.Body.Bytes(), &env)
			if env.Error.Type != "invalid_request" || !strings.Contains(env.Error.Message, "`"+tc.name+"`") {
				t.Fatalf("the refusal does not say why: %+v", env.Error)
			}
			if eng.calls != 0 {
				t.Fatal("the engine was called for a refused request")
			}
		})
	}
	// n: 1 asks for nothing extra and is served.
	h := &Handler{Engine: &recordingEngine{}, Store: usageTestStore(t), Default: "m"}
	if rec := postChat(t, h, `{"model":"m","n":1,"messages":[{"role":"user","content":"hi"}]}`); rec.Code != http.StatusOK {
		t.Fatalf("n=1 refused: %d %s", rec.Code, rec.Body.String())
	}
}

// A reserved key in the caller's body is ours: `stream_options` carries
// include_usage, and a caller who could switch it off would break usage
// capture without a word.
func TestTheCallerCannotSetAReservedKey(t *testing.T) {
	eng := &recordingEngine{}
	h := &Handler{Engine: eng, Store: usageTestStore(t), Default: "m"}
	rec := postChat(t, h, `{"model":"m","stream_options":{"include_usage":false},"messages":[{"role":"user","content":"hi"}]}`)
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d: %s", rec.Code, rec.Body.String())
	}
	if _, ok := eng.got.Extra["stream_options"]; ok {
		t.Fatal("stream_options rode as an extra")
	}
	raw, _ := json.Marshal(openaicompat.BuildChatBody(*eng.got))
	if !strings.Contains(string(raw), `"stream_options":{"include_usage":true}`) {
		t.Fatalf("usage capture was switched off by the caller: %s", raw)
	}
}
