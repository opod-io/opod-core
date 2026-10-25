package trtllm_test

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/opod-io/opod/internal/engines"
	"github.com/opod-io/opod/internal/engines/trtllm"
)

// PLAN T27.8: trtllm-serve 1.2.1 reads an OpenAI json_schema response_format
// as if the {"name","schema"} wrapper were the schema, so the answer was any
// JSON. The driver sends the engine's own {"type":"json","schema":…}; every
// other format reaches it exactly as the caller wrote it.
func TestAJSONSchemaReachesTheEngineAsItsOwnJSONType(t *testing.T) {
	schema := `{"type":"object","properties":{"capital":{"type":"string"}},"required":["capital"]}`
	for name, c := range map[string]struct{ in, want string }{
		"json_schema": {`{"type":"json_schema","json_schema":{"name":"capital","strict":true,"schema":` + schema + `}}`, `{"schema":` + schema + `,"type":"json"}`},
		"json_object": {`{"type":"json_object"}`, `{"type":"json_object"}`},
		"no schema":   {`{"type":"json_schema","json_schema":{"name":"x"}}`, `{"type":"json_schema","json_schema":{"name":"x"}}`},
	} {
		var got json.RawMessage
		srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			b, _ := io.ReadAll(r.Body)
			var body struct {
				ResponseFormat json.RawMessage `json:"response_format"`
			}
			_ = json.Unmarshal(b, &body)
			got = body.ResponseFormat
			w.Header().Set("Content-Type", "text/event-stream")
			_, _ = io.WriteString(w, "data: {\"choices\":[{\"delta\":{\"content\":\"{}\"},\"finish_reason\":\"stop\"}]}\n\ndata: [DONE]\n\n")
		}))
		stream, err := trtllm.New(srv.URL, "").Chat(context.Background(), engines.ChatRequest{Model: "m", Stream: true,
			Messages: []engines.Message{{Role: "user", Content: "hi"}}, ResponseFormat: json.RawMessage(c.in)})
		if err != nil {
			t.Fatal(name, err)
		}
		for range stream {
		}
		srv.Close()
		var g, w any
		_ = json.Unmarshal(got, &g)
		_ = json.Unmarshal([]byte(c.want), &w)
		gb, _ := json.Marshal(g)
		wb, _ := json.Marshal(w)
		if string(gb) != string(wb) {
			t.Errorf("%s: the engine got %s, want %s", name, got, c.want)
		}
	}
}
