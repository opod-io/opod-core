package api

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

// A request that asks for something we cannot do is refused BY NAME, and one
// that asks for nothing unusual is untouched (T10.13).
func TestRefuseUnsupportedChatFields(t *testing.T) {
	cases := []struct {
		name    string
		body    string
		refused bool
		names   string // the field the message must name
	}{
		{"plain chat", `{"model":"m","messages":[{"role":"user","content":"hi"}]}`, false, ""},
		{"tools", `{"model":"m","tools":[{"type":"function","function":{"name":"f"}}]}`, true, "`tools`"},
		{"tools with tool_choice none", `{"model":"m","tools":[{"type":"function"}],"tool_choice":"none"}`, false, ""},
		{"tools with tool_choice auto", `{"model":"m","tools":[{"type":"function"}],"tool_choice":"auto"}`, true, "`tools`"},
		{"empty tools array", `{"model":"m","tools":[]}`, false, ""},
		{"deprecated functions are ignored, not refused", `{"model":"m","functions":[{"name":"f"}]}`, false, ""},
		{"response_format json_object", `{"model":"m","response_format":{"type":"json_object"}}`, true, "json_object"},
		{"response_format json_schema", `{"model":"m","response_format":{"type":"json_schema"}}`, true, "json_schema"},
		{"response_format text", `{"model":"m","response_format":{"type":"text"}}`, false, ""},
		{"invalid json is left to the handler", `{"model":`, false, ""},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			w := httptest.NewRecorder()
			got := refuseUnsupportedChatFields(w, []byte(tc.body))
			if got != tc.refused {
				t.Fatalf("refused = %v, want %v", got, tc.refused)
			}
			if !tc.refused {
				if w.Code != http.StatusOK || w.Body.Len() != 0 {
					t.Fatalf("nothing should have been written, got %d %q", w.Code, w.Body.String())
				}
				return
			}
			if w.Code != http.StatusBadRequest {
				t.Fatalf("status = %d, want 400", w.Code)
			}
			var env struct {
				Error struct {
					Type    string `json:"type"`
					Message string `json:"message"`
				} `json:"error"`
			}
			if err := json.Unmarshal(w.Body.Bytes(), &env); err != nil {
				t.Fatalf("error body is not JSON: %v (%s)", err, w.Body.String())
			}
			if env.Error.Type != "invalid_request" {
				t.Fatalf("error type = %q, want invalid_request", env.Error.Type)
			}
			if !strings.Contains(env.Error.Message, tc.names) {
				t.Fatalf("message does not name %s: %q", tc.names, env.Error.Message)
			}
			// The refusal has to say what to do instead, or it is only a no.
			if !strings.Contains(env.Error.Message, "Remove") {
				t.Fatalf("message gives no way forward: %q", env.Error.Message)
			}
		})
	}
}
