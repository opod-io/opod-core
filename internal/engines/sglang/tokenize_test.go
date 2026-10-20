package sglang

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/opod-io/opod/internal/engines"
)

// T19.21: the leader's prefix index tokenizes through the worker's engine, and
// SGLang answers POST /tokenize with messages and the generation prompt, the
// same shape as vLLM; the leading ids are kept up to the limit.
func TestTokenizeAsksSGLangWithTheChatTemplate(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var in map[string]any
		_ = json.NewDecoder(r.Body).Decode(&in)
		if r.URL.Path != "/tokenize" || in["add_generation_prompt"] != true || in["messages"] == nil {
			http.Error(w, "unexpected", http.StatusBadRequest)
			return
		}
		_, _ = w.Write([]byte(`{"tokens":[1,2,3,4,5],"count":5,"max_model_len":4096}`))
	}))
	defer srv.Close()
	ids, err := New(srv.URL, "").Tokenize(context.Background(), "m", []engines.Message{{Role: "user", Content: "hi"}}, "", 3)
	if err != nil || len(ids) != 3 || ids[2] != 3 {
		t.Fatalf("ids %v err %v", ids, err)
	}
}
