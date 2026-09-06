package api

import (
	"io"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"testing"

	"github.com/opod-io/opod/internal/store"
)

func TestVendor_RegistryProviders(t *testing.T) {
	cases := map[string]string{
		"deepseek/deepseek-chat":     "deepseek",
		"cerebras/llama-3.3-70b":     "cerebras",
		"nvidia/meta/llama-3.1-405b": "nvidia",
		"huggingface/Qwen/Qwen3-8B":  "huggingface",
		"zai/glm-4.6":                "zai",
		"opencode-zen/some-model":    "opencode-zen",

		// Slash-namespaced gemini is the OpenAI-compat registry provider;
		// the BARE gemini- id still routes to Vertex (unchanged).
		"gemini/gemini-2.0-flash": "gemini",
		"gemini-1.5-pro":          "vertex",

		// Unknown prefix and bare local ids are not vendors.
		"madeup/foo": "",
		"qwen3-14b":  "",
	}
	for model, want := range cases {
		if got := Vendor(model); got != want {
			t.Errorf("Vendor(%q) = %q, want %q", model, got, want)
		}
	}
}

func TestServeGeneric_StripsPrefixAndAuths(t *testing.T) {
	st, err := store.OpenSQLite(filepath.Join(t.TempDir(), "x.db"))
	if err != nil {
		t.Fatalf("open store: %v", err)
	}
	var gotModel, gotAuth string
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		gotModel = peekVendorModel(body)
		gotAuth = r.Header.Get("Authorization")
		w.WriteHeader(http.StatusOK)
		_, _ = io.WriteString(w, `{"id":"ok"}`)
	}))
	defer upstream.Close()

	pool := NewKeyPool()
	pool.Set("deepseek", []string{"dsk-1"})
	e := &EgressHandler{
		Store:        st,
		Keys:         pool,
		ProviderURLs: map[string]string{"deepseek": upstream.URL},
	}

	r := httptest.NewRequest(http.MethodPost, "/v1/chat/completions",
		strings.NewReader(`{"model":"deepseek/deepseek-chat","messages":[]}`))
	w := httptest.NewRecorder()
	e.ServeGeneric(w, r, "deepseek")

	if w.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200", w.Code)
	}
	if gotModel != "deepseek-chat" {
		t.Errorf("upstream model = %q, want deepseek-chat (prefix should be stripped)", gotModel)
	}
	if gotAuth != "Bearer dsk-1" {
		t.Errorf("upstream auth = %q, want Bearer dsk-1", gotAuth)
	}
}

func TestServeGeneric_MissingURLIsActionable(t *testing.T) {
	st, _ := store.OpenSQLite(filepath.Join(t.TempDir(), "x.db"))
	// "kilo" has no default URL and none configured → must refuse with a
	// clear setup hint rather than fire a blind request.
	e := &EgressHandler{Store: st}
	r := httptest.NewRequest(http.MethodPost, "/v1/chat/completions",
		strings.NewReader(`{"model":"kilo/foo"}`))
	w := httptest.NewRecorder()
	e.ServeGeneric(w, r, "kilo")

	if w.Code != http.StatusServiceUnavailable {
		t.Fatalf("status = %d, want 503 for unconfigured base URL", w.Code)
	}
	if !strings.Contains(w.Body.String(), "KILO_BASE_URL") {
		t.Errorf("error should name the env var to set; got %q", w.Body.String())
	}
}
