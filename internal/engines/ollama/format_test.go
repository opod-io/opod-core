package ollama

import (
	"encoding/json"
	"testing"
)

// Ollama's field is `format`, not `response_format`, and its schema sits one
// level higher than OpenAI's. The translation is the whole reason this driver
// does not share the OpenAI serialiser here (T10.14).
func TestOllamaFormat(t *testing.T) {
	cases := []struct {
		name string
		in   string
		want string // JSON of the translated value, "" = nothing sent
	}{
		{"nothing asked", ``, ""},
		{"plain text", `{"type":"text"}`, ""},
		{"unknown type is left to the engine", `{"type":"something_new"}`, ""},
		{"json object", `{"type":"json_object"}`, `"json"`},
		{"json schema", `{"type":"json_schema","json_schema":{"name":"r","schema":{"type":"object"}}}`, `{"type":"object"}`},
		{"json schema with no schema falls back to json", `{"type":"json_schema"}`, `"json"`},
		{"broken json is ignored, never guessed", `{"type":`, ""},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := ollamaFormat(json.RawMessage(tc.in))
			if tc.want == "" {
				if got != nil {
					t.Fatalf("format = %v, want nothing", got)
				}
				return
			}
			raw, err := json.Marshal(got)
			if err != nil {
				t.Fatalf("marshal: %v", err)
			}
			if string(raw) != tc.want {
				t.Fatalf("format = %s, want %s", raw, tc.want)
			}
		})
	}
}
