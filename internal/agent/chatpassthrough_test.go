package agent

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/opod-io/opod/internal/engines"
)

type chatRecorder struct {
	engines.Engine
	got engines.ChatRequest
}

func (c *chatRecorder) Name() string { return "recorder" }
func (c *chatRecorder) Chat(_ context.Context, req engines.ChatRequest) (<-chan engines.StreamEvent, error) {
	c.got = req
	ch := make(chan engines.StreamEvent, 2)
	ch <- engines.StreamEvent{Delta: "ok"}
	ch <- engines.StreamEvent{Done: true, Reason: "stop"}
	close(ch)
	return ch, nil
}

// The leader re-serialises a chat through the shared OpenAI body builder
// before it reaches a worker, so any field the builder sends and the worker's
// own request struct does not name is dropped HERE — one hop short of the
// engine and invisibly. That is the whole failure mode T10.14 exists to fix,
// and a worker is the normal path under the control plane, so the leader-local
// test alone would have proved nothing.
func TestWorkerCarriesResponseFormatToTheEngine(t *testing.T) {
	eng := &chatRecorder{}
	s := &Server{Engine: eng}

	body := `{"model":"m","messages":[{"role":"user","content":"hi"}],` +
		`"response_format":{"type":"json_schema","json_schema":{"name":"r","schema":{"type":"object"}}}}`
	w := httptest.NewRecorder()
	s.chatCompletions(w, httptest.NewRequest(http.MethodPost, "/v1/chat/completions", strings.NewReader(body)))

	if w.Code != http.StatusOK {
		t.Fatalf("status = %d: %s", w.Code, w.Body.String())
	}
	if len(eng.got.ResponseFormat) == 0 {
		t.Fatal("response_format never reached the engine: the worker dropped it")
	}
	var rf struct {
		Type       string `json:"type"`
		JSONSchema *struct {
			Name string `json:"name"`
		} `json:"json_schema"`
	}
	if err := json.Unmarshal(eng.got.ResponseFormat, &rf); err != nil {
		t.Fatalf("response_format is not the caller's JSON: %v (%q)", err, eng.got.ResponseFormat)
	}
	if rf.Type != "json_schema" || rf.JSONSchema == nil || rf.JSONSchema.Name != "r" {
		t.Fatalf("response_format changed in flight: %q", eng.got.ResponseFormat)
	}

	// A caller who asked for nothing must not have an empty object invented
	// for them on the way down.
	eng.got = engines.ChatRequest{}
	w = httptest.NewRecorder()
	s.chatCompletions(w, httptest.NewRequest(http.MethodPost, "/v1/chat/completions",
		strings.NewReader(`{"model":"m","messages":[{"role":"user","content":"hi"}]}`)))
	if len(eng.got.ResponseFormat) != 0 {
		t.Fatalf("response_format was invented: %q", eng.got.ResponseFormat)
	}
}

type toolEngine struct {
	engines.Engine
	got    engines.ChatRequest
	refuse bool
}

func (e *toolEngine) Name() string { return "tooler" }
func (e *toolEngine) Chat(_ context.Context, req engines.ChatRequest) (<-chan engines.StreamEvent, error) {
	e.got = req
	if e.refuse {
		return nil, engines.Unsupported("tooler", "tools", "not translated yet")
	}
	ch := make(chan engines.StreamEvent, 3)
	ch <- engines.StreamEvent{ToolCalls: json.RawMessage(
		`[{"index":0,"id":"call_1","type":"function","function":{"name":"f","arguments":"{\"a\""}}]`)}
	ch <- engines.StreamEvent{ToolCalls: json.RawMessage(
		`[{"index":0,"function":{"arguments":":1}"}}]`)}
	ch <- engines.StreamEvent{Done: true, Reason: "tool_calls"}
	close(ch)
	return ch, nil
}

// The worker is the normal path under the control plane, so a tool call has
// to survive its hop in both directions: the declaration down, and the
// fragments back up merged into one call (T10.8).
func TestWorkerCarriesToolsAndAggregatesTheCall(t *testing.T) {
	eng := &toolEngine{}
	s := &Server{Engine: eng}

	body := `{"model":"m","messages":[{"role":"user","content":"hi"}],` +
		`"tools":[{"type":"function","function":{"name":"f"}}],"tool_choice":"auto"}`
	w := httptest.NewRecorder()
	s.chatCompletions(w, httptest.NewRequest(http.MethodPost, "/v1/chat/completions", strings.NewReader(body)))

	if w.Code != http.StatusOK {
		t.Fatalf("status = %d: %s", w.Code, w.Body.String())
	}
	if len(eng.got.Tools) == 0 || len(eng.got.ToolChoice) == 0 {
		t.Fatalf("the worker dropped the declaration: tools=%q choice=%q", eng.got.Tools, eng.got.ToolChoice)
	}
	var resp struct {
		Choices []struct {
			Message struct {
				ToolCalls []struct {
					ID       string `json:"id"`
					Function struct {
						Name      string `json:"name"`
						Arguments string `json:"arguments"`
					} `json:"function"`
				} `json:"tool_calls"`
			} `json:"message"`
			FinishReason string `json:"finish_reason"`
		} `json:"choices"`
	}
	if err := json.Unmarshal(w.Body.Bytes(), &resp); err != nil {
		t.Fatalf("answer is not JSON: %v (%s)", err, w.Body.String())
	}
	calls := resp.Choices[0].Message.ToolCalls
	if len(calls) != 1 {
		t.Fatalf("got %d tool calls, want 1: %s", len(calls), w.Body.String())
	}
	if calls[0].ID != "call_1" || calls[0].Function.Name != "f" || calls[0].Function.Arguments != `{"a":1}` {
		t.Fatalf("the fragments did not merge: %+v", calls[0])
	}
	if resp.Choices[0].FinishReason != "tool_calls" {
		t.Fatalf("finish_reason = %q", resp.Choices[0].FinishReason)
	}
}

// An engine that cannot do tools says so as a 400 the leader relays, not a
// 502 that reads like our fault (T10.8).
func TestWorkerAnswers400WhenTheEngineCannotDoTools(t *testing.T) {
	s := &Server{Engine: &toolEngine{refuse: true}}
	w := httptest.NewRecorder()
	s.chatCompletions(w, httptest.NewRequest(http.MethodPost, "/v1/chat/completions",
		strings.NewReader(`{"model":"m","messages":[{"role":"user","content":"hi"}],"tools":[{"type":"function"}]}`)))

	if w.Code != http.StatusBadRequest {
		t.Fatalf("status = %d, want 400: %s", w.Code, w.Body.String())
	}
	var env struct {
		Error struct {
			Type    string `json:"type"`
			Message string `json:"message"`
		} `json:"error"`
	}
	if err := json.Unmarshal(w.Body.Bytes(), &env); err != nil {
		t.Fatalf("not OpenAI-shaped, so the leader cannot relay it: %v", err)
	}
	if env.Error.Type != "unsupported_request" || !strings.Contains(env.Error.Message, "`tools`") {
		t.Fatalf("the caller cannot tell which field: %+v", env.Error)
	}
}

// The SECOND request of a tool loop is the one that used to break: the client
// appends the assistant message it just received and a tool result beside it.
// Drop `tool_calls` or `tool_call_id` and the engine rejects a correct request
// as if it were the caller's mistake (review finding, 2026-10-05).
func TestWorkerCarriesTheSecondTurnOfAToolLoop(t *testing.T) {
	eng := &chatRecorder{}
	s := &Server{Engine: eng}

	body := `{"model":"m","messages":[
	  {"role":"user","content":"weather?"},
	  {"role":"assistant","content":"","tool_calls":[{"id":"call_1","type":"function","function":{"name":"f","arguments":"{}"}}]},
	  {"role":"tool","tool_call_id":"call_1","content":"sunny"}
	]}`
	w := httptest.NewRecorder()
	s.chatCompletions(w, httptest.NewRequest(http.MethodPost, "/v1/chat/completions", strings.NewReader(body)))

	if w.Code != http.StatusOK {
		t.Fatalf("status = %d: %s", w.Code, w.Body.String())
	}
	if len(eng.got.Messages) != 3 {
		t.Fatalf("got %d messages, want 3", len(eng.got.Messages))
	}
	if !json.Valid(eng.got.Messages[1].ToolCalls) {
		t.Fatalf("the assistant turn's calls were dropped: %q", eng.got.Messages[1].ToolCalls)
	}
	if eng.got.Messages[2].ToolCallID != "call_1" {
		t.Fatalf("tool_call_id was dropped; the engine would refuse this: %+v", eng.got.Messages[2])
	}
}

// The leader sends the caller's extra fields merged into the body it built
// (T17.1), and the worker's own struct does not name them: this hop is where
// they would vanish. They must reach the worker's engine as extras.
func TestWorkerCarriesTheCallersExtraFieldsToTheEngine(t *testing.T) {
	eng := &chatRecorder{}
	s := &Server{Engine: eng}

	body := `{"model":"m","messages":[{"role":"user","content":"hi"}],"stream":true,` +
		`"stream_options":{"include_usage":true},"seed":7,"logprobs":true,"a_field_from_2027":{"x":1}}`
	w := httptest.NewRecorder()
	s.chatCompletions(w, httptest.NewRequest(http.MethodPost, "/v1/chat/completions", strings.NewReader(body)))
	if w.Code != http.StatusOK {
		t.Fatalf("status = %d: %s", w.Code, w.Body.String())
	}
	for k, want := range map[string]string{"seed": `7`, "logprobs": `true`, "a_field_from_2027": `{"x":1}`} {
		if string(eng.got.Extra[k]) != want {
			t.Errorf("extra %s = %s, want %s", k, eng.got.Extra[k], want)
		}
	}
	for _, k := range []string{"model", "messages", "stream", "stream_options"} {
		if _, ok := eng.got.Extra[k]; ok {
			t.Errorf("reserved key %q rode as an extra", k)
		}
	}
}
