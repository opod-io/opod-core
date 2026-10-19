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

// A Messages request with every translated field becomes the chat request an
// OpenAI-shaped client would have sent (T19.3).
func TestAnthropicRequestTranslates(t *testing.T) {
	chat, model, stream, aerr := AnthropicToChat([]byte(`{
		"model":"m","max_tokens":64,"stream":true,"temperature":0.2,"top_p":0.9,"top_k":40,
		"stop_sequences":["END"],"metadata":{"user_id":"u-7"},
		"system":[{"type":"text","text":"Be brief."},{"type":"text","text":"Use tools."}],
		"tools":[{"name":"get_weather","description":"Weather now","input_schema":{"type":"object","properties":{"city":{"type":"string"}}}}],
		"tool_choice":{"type":"any","disable_parallel_tool_use":true},
		"messages":[
			{"role":"user","content":[{"type":"text","text":"What is this, and the weather?"},{"type":"image","source":{"type":"base64","media_type":"image/png","data":"iVBORw0"}}]},
			{"role":"assistant","content":[{"type":"text","text":"Checking."},{"type":"tool_use","id":"toolu_1","name":"get_weather","input":{"city":"Paris"}}]},
			{"role":"user","content":[{"type":"tool_result","tool_use_id":"toolu_1","content":[{"type":"text","text":"18C"}]},{"type":"text","text":"Thanks"}]}
		]}`), false)
	if aerr != nil || model != "m" || !stream {
		t.Fatalf("translate: %v model=%q stream=%v", aerr, model, stream)
	}
	var got struct {
		Messages []struct {
			Role       string          `json:"role"`
			Content    json.RawMessage `json:"content"`
			ToolCalls  json.RawMessage `json:"tool_calls"`
			ToolCallID string          `json:"tool_call_id"`
		} `json:"messages"`
		MaxTokens         int             `json:"max_tokens"`
		Stream            bool            `json:"stream"`
		Stop              []string        `json:"stop"`
		TopK              int             `json:"top_k"`
		User              string          `json:"user"`
		Tools             json.RawMessage `json:"tools"`
		ToolChoice        json.RawMessage `json:"tool_choice"`
		ParallelToolCalls *bool           `json:"parallel_tool_calls"`
	}
	if err := json.Unmarshal(chat, &got); err != nil {
		t.Fatal(err)
	}
	roles := []string{}
	for _, m := range got.Messages {
		roles = append(roles, m.Role)
	}
	if strings.Join(roles, ",") != "system,user,assistant,tool,user" {
		t.Fatalf("roles %v in %s", roles, chat)
	}
	if string(got.Messages[0].Content) != `"Be brief.\n\nUse tools."` {
		t.Errorf("system = %s", got.Messages[0].Content)
	}
	if !strings.Contains(string(got.Messages[1].Content), `"url":"data:image/png;base64,iVBORw0"`) {
		t.Errorf("image part = %s", got.Messages[1].Content)
	}
	if !strings.Contains(string(got.Messages[2].ToolCalls), `"id":"toolu_1"`) || !strings.Contains(string(got.Messages[2].ToolCalls), `"arguments":"{\"city\":\"Paris\"}"`) {
		t.Errorf("tool call = %s", got.Messages[2].ToolCalls)
	}
	if got.Messages[3].ToolCallID != "toolu_1" || string(got.Messages[3].Content) != `"18C"` {
		t.Errorf("tool result = %+v", got.Messages[3])
	}
	if got.MaxTokens != 64 || !got.Stream || got.Stop[0] != "END" || got.TopK != 40 || got.User != "u-7" {
		t.Errorf("scalars = %+v", got)
	}
	if !strings.Contains(string(got.Tools), `"parameters":{"type":"object"`) || string(got.ToolChoice) != `"required"` || got.ParallelToolCalls == nil || *got.ParallelToolCalls {
		t.Errorf("tools %s choice %s parallel %v", got.Tools, got.ToolChoice, got.ParallelToolCalls)
	}

	// count_tokens: no max_tokens needed, a one-token unstreamed generation.
	chat, _, stream, aerr = AnthropicToChat([]byte(`{"model":"m","stream":true,"messages":[{"role":"user","content":"hi"}]}`), true)
	if aerr != nil || stream || !strings.Contains(string(chat), `"max_tokens":1`) || strings.Contains(string(chat), `"stream"`) {
		t.Fatalf("count: %v stream=%v %s", aerr, stream, chat)
	}
}

// What this endpoint cannot serve is refused in Anthropic's envelope, naming
// the field — never accepted and ignored.
func TestAnthropicRequestRefusals(t *testing.T) {
	msgs := `"messages":[{"role":"user","content":"hi"}]`
	for name, body := range map[string]string{
		"no max_tokens":    `{"model":"m",` + msgs + `}`,
		"thinking":         `{"model":"m","max_tokens":5,"thinking":{"type":"enabled","budget_tokens":1024},` + msgs + `}`,
		"server tool":      `{"model":"m","max_tokens":5,"tools":[{"type":"web_search_20250305","name":"web_search"}],` + msgs + `}`,
		"image by url":     `{"model":"m","max_tokens":5,"messages":[{"role":"user","content":[{"type":"image","source":{"type":"url","url":"https://x/y.png"}}]}]}`,
		"document":         `{"model":"m","max_tokens":5,"messages":[{"role":"user","content":[{"type":"document","source":{"type":"text","data":"x"}}]}]}`,
		"mcp_servers":      `{"model":"m","max_tokens":5,"mcp_servers":[],` + msgs + `}`,
		"unknown field":    `{"model":"m","max_tokens":5,"frobnicate":1,` + msgs + `}`,
		"no messages":      `{"model":"m","max_tokens":5,"messages":[]}`,
		"bad role":         `{"model":"m","max_tokens":5,"messages":[{"role":"system","content":"hi"}]}`,
		"tool_use in user": `{"model":"m","max_tokens":5,"messages":[{"role":"user","content":[{"type":"tool_use","id":"t","name":"n","input":{}}]}]}`,
	} {
		_, _, _, aerr := AnthropicToChat([]byte(body), false)
		if aerr == nil || aerr.Status != http.StatusBadRequest || aerr.Type != "invalid_request_error" {
			t.Errorf("%s: %+v, want a 400 invalid_request_error", name, aerr)
		}
	}
	if _, _, _, aerr := AnthropicToChat([]byte(`{"model":"m","max_tokens":5,"thinking":{"type":"disabled"},`+msgs+`}`), false); aerr != nil {
		t.Errorf("thinking disabled is accepted: %v", aerr)
	}
}

// toolEngine answers a sentence and one tool call in fragments, as vLLM
// streams them, then its usage.
type toolEngine struct{ cancelEngine }

func (e *toolEngine) Chat(_ context.Context, _ engines.ChatRequest) (<-chan engines.StreamEvent, error) {
	out := make(chan engines.StreamEvent, 6)
	out <- engines.StreamEvent{Delta: "Let me "}
	out <- engines.StreamEvent{Delta: "check."}
	out <- engines.StreamEvent{ToolCalls: json.RawMessage(`[{"index":0,"id":"call_9","type":"function","function":{"name":"get_weather","arguments":""}}]`)}
	out <- engines.StreamEvent{ToolCalls: json.RawMessage(`[{"index":0,"function":{"arguments":"{\"city\":"}}]`)}
	out <- engines.StreamEvent{ToolCalls: json.RawMessage(`[{"index":0,"function":{"arguments":"\"Paris\"}"}}]`)}
	out <- engines.StreamEvent{Done: true, Reason: "tool_calls", Usage: &engines.Usage{PromptTokens: 12, CompletionTokens: 5, TotalTokens: 17}}
	close(out)
	return out, nil
}

// serveMessages runs a Messages body through the chat handler the way the
// leader does: translated in, the answer rewritten on the way out.
func serveMessages(t *testing.T, h *Handler, body string) *httptest.ResponseRecorder {
	t.Helper()
	chat, model, _, aerr := AnthropicToChat([]byte(body), false)
	if aerr != nil {
		t.Fatal(aerr)
	}
	rec := httptest.NewRecorder()
	r := httptest.NewRequest(http.MethodPost, "/v1/messages", strings.NewReader(body))
	r = WithRequestBody(r.WithContext(WithProtocol(r.Context(), ProtocolAnthropic)), chat)
	aw := NewAnthropicWriter(rec, model)
	h.ChatCompletions(aw, r)
	aw.Finish()
	return rec
}

func TestAnthropicAnswerAggregated(t *testing.T) {
	st := usageTestStore(t)
	h := &Handler{Engine: &toolEngine{}, Store: st, Default: "m"}
	rec := serveMessages(t, h, `{"model":"m","max_tokens":64,"messages":[{"role":"user","content":"Weather in Paris?"}]}`)
	if rec.Code != http.StatusOK {
		t.Fatalf("status %d: %s", rec.Code, rec.Body)
	}
	var msg struct {
		ID         string `json:"id"`
		Type       string `json:"type"`
		Role       string `json:"role"`
		StopReason string `json:"stop_reason"`
		Content    []struct {
			Type  string          `json:"type"`
			Text  string          `json:"text"`
			ID    string          `json:"id"`
			Name  string          `json:"name"`
			Input json.RawMessage `json:"input"`
		} `json:"content"`
		Usage struct {
			In  int `json:"input_tokens"`
			Out int `json:"output_tokens"`
		} `json:"usage"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &msg); err != nil {
		t.Fatalf("%v: %s", err, rec.Body)
	}
	if !strings.HasPrefix(msg.ID, "msg_") || msg.Type != "message" || msg.Role != "assistant" || msg.StopReason != "tool_use" {
		t.Errorf("envelope %+v", msg)
	}
	if len(msg.Content) != 2 || msg.Content[0].Text != "Let me check." || msg.Content[1].Type != "tool_use" ||
		msg.Content[1].ID != "call_9" || msg.Content[1].Name != "get_weather" || string(msg.Content[1].Input) != `{"city":"Paris"}` {
		t.Errorf("content %+v", msg.Content)
	}
	if msg.Usage.In != 12 || msg.Usage.Out != 5 {
		t.Errorf("usage %+v", msg.Usage)
	}
	if u := waitForUsage(t, st); u.Protocol != ProtocolAnthropic {
		t.Errorf("usage row protocol = %q, want anthropic", u.Protocol)
	}
}

func TestAnthropicAnswerStreamed(t *testing.T) {
	h := &Handler{Engine: &toolEngine{}, Store: usageTestStore(t), Default: "m"}
	rec := serveMessages(t, h, `{"model":"m","max_tokens":64,"stream":true,"messages":[{"role":"user","content":"Weather in Paris?"}]}`)
	if rec.Code != http.StatusOK || !strings.HasPrefix(rec.Header().Get("Content-Type"), "text/event-stream") {
		t.Fatalf("status %d type %q", rec.Code, rec.Header().Get("Content-Type"))
	}
	var types []string
	var deltas, args strings.Builder
	var stop string
	var outTokens int
	for _, ev := range strings.Split(strings.TrimSpace(rec.Body.String()), "\n\n") {
		lines := strings.SplitN(ev, "\n", 2)
		name := strings.TrimPrefix(lines[0], "event: ")
		var data struct {
			Type  string `json:"type"`
			Delta struct {
				Type        string `json:"type"`
				Text        string `json:"text"`
				PartialJSON string `json:"partial_json"`
				StopReason  string `json:"stop_reason"`
			} `json:"delta"`
			Usage struct {
				Out int `json:"output_tokens"`
			} `json:"usage"`
		}
		if err := json.Unmarshal([]byte(strings.TrimPrefix(lines[1], "data: ")), &data); err != nil || data.Type != name {
			t.Fatalf("event %q: %v", ev, err)
		}
		types = append(types, name)
		deltas.WriteString(data.Delta.Text)
		args.WriteString(data.Delta.PartialJSON)
		if name == "message_delta" {
			stop, outTokens = data.Delta.StopReason, data.Usage.Out
		}
	}
	want := "message_start ping content_block_start content_block_delta content_block_delta content_block_stop " +
		"content_block_start content_block_delta content_block_delta content_block_stop message_delta message_stop"
	if strings.Join(types, " ") != want {
		t.Errorf("events\n got %s\nwant %s", strings.Join(types, " "), want)
	}
	if deltas.String() != "Let me check." || args.String() != `{"city":"Paris"}` || stop != "tool_use" || outTokens != 5 {
		t.Errorf("text %q args %q stop %q out %d", deltas.String(), args.String(), stop, outTokens)
	}
	if strings.Contains(rec.Body.String(), "[DONE]") || strings.Contains(rec.Body.String(), "chat.completion") {
		t.Errorf("a chat chunk leaked into the Anthropic stream: %s", rec.Body)
	}
}

// An error the chat path writes reaches the caller in Anthropic's envelope,
// with Anthropic's type for its status.
func TestAnthropicErrorEnvelope(t *testing.T) {
	for status, typ := range map[int]string{404: "not_found_error", 503: "overloaded_error", 429: "rate_limit_error", 400: "invalid_request_error", 502: "api_error"} {
		rec := httptest.NewRecorder()
		aw := NewAnthropicWriter(rec, "m")
		writeJSONError(aw, status, "whatever", "the reason")
		aw.Finish()
		var e struct {
			Type  string `json:"type"`
			Error struct {
				Type    string `json:"type"`
				Message string `json:"message"`
			} `json:"error"`
		}
		if err := json.Unmarshal(rec.Body.Bytes(), &e); err != nil || rec.Code != status || e.Type != "error" || e.Error.Type != typ || e.Error.Message != "the reason" {
			t.Errorf("%d: code %d body %s", status, rec.Code, rec.Body)
		}
	}
}
