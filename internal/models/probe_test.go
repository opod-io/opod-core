package models

import (
	"context"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"testing"
	"time"
)

// pointAt redirects both registry bases at the test server for the
// duration of the test.
func pointAt(t *testing.T, base string) {
	t.Helper()
	origOllama, origHF := ollamaRegistryBase, huggingFaceBase
	ollamaRegistryBase, huggingFaceBase = base, base
	t.Cleanup(func() { ollamaRegistryBase, huggingFaceBase = origOllama, origHF })
}

func TestProbeSourceVerdicts(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/v2/library/real/manifests/7b":
			w.WriteHeader(http.StatusOK)
		case "/real/repo/resolve/main/README.md":
			w.WriteHeader(http.StatusOK)
		case "/gated/repo/resolve/main/README.md":
			w.WriteHeader(http.StatusUnauthorized) // gated = exists
		case "/flaky/repo/resolve/main/README.md":
			w.WriteHeader(http.StatusBadGateway) // 5xx = indeterminate
		default:
			w.WriteHeader(http.StatusNotFound)
		}
	}))
	defer srv.Close()
	pointAt(t, srv.URL)
	client := &http.Client{Timeout: 2 * time.Second}
	ctx := context.Background()

	cases := []struct {
		name  string
		entry Entry
		want  ProbeVerdict
	}{
		{"ollama exists", Entry{Source: SourceSpec{Type: "ollama", OllamaName: "real:7b"}}, ProbeOK},
		{"ollama 404", Entry{Source: SourceSpec{Type: "ollama", OllamaName: "fake:7b"}}, ProbeNotFound},
		{"ollama empty name", Entry{Source: SourceSpec{Type: "ollama"}}, ProbeNotFound},
		{"hf exists", Entry{Source: SourceSpec{Type: "huggingface", Repo: "real/repo"}}, ProbeOK},
		{"hf gated counts as exists", Entry{Source: SourceSpec{Type: "huggingface", Repo: "gated/repo"}}, ProbeOK},
		{"hf 404", Entry{Source: SourceSpec{Type: "huggingface", Repo: "fake/repo"}}, ProbeNotFound},
		{"hf 5xx indeterminate", Entry{Source: SourceSpec{Type: "huggingface", Repo: "flaky/repo"}}, ProbeIndeterminate},
		{"unknown type skipped", Entry{Source: SourceSpec{Type: "bedrock"}}, ProbeSkipped},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			got, reason := ProbeSource(ctx, client, &c.entry)
			if got != c.want {
				t.Errorf("verdict = %v (reason %q), want %v", got, reason, c.want)
			}
			if got != ProbeOK && reason == "" {
				t.Errorf("non-OK verdict must carry a reason")
			}
		})
	}
}

func TestProbeSourceFileType(t *testing.T) {
	ctx := context.Background()
	existing := filepath.Join(t.TempDir(), "model.gguf")
	if err := os.WriteFile(existing, []byte("gguf"), 0o644); err != nil {
		t.Fatal(err)
	}
	if v, _ := ProbeSource(ctx, nil, &Entry{Source: SourceSpec{Type: "file", Path: existing}}); v != ProbeOK {
		t.Errorf("existing file: verdict %v, want ProbeOK", v)
	}
	if v, _ := ProbeSource(ctx, nil, &Entry{Source: SourceSpec{Type: "file", Path: "/no/such/file.gguf"}}); v != ProbeNotFound {
		t.Errorf("missing file: verdict %v, want ProbeNotFound", v)
	}
	if v, _ := ProbeSource(ctx, nil, &Entry{Source: SourceSpec{Type: "file"}}); v != ProbeNotFound {
		t.Errorf("empty path: verdict %v, want ProbeNotFound", v)
	}
}

func TestProbeSourceSkipEnv(t *testing.T) {
	t.Setenv("OPOD_SKIP_SOURCE_CHECK", "1")
	v, _ := ProbeSource(context.Background(), nil, &Entry{Source: SourceSpec{Type: "huggingface", Repo: "anything/at-all"}})
	if v != ProbeSkipped {
		t.Errorf("OPOD_SKIP_SOURCE_CHECK=1: verdict %v, want ProbeSkipped (no network)", v)
	}
}

func TestProbeSourceNetworkErrorIsIndeterminate(t *testing.T) {
	// Registry that refuses connections — must degrade to a warning,
	// never block an install.
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {}))
	srv.Close() // closed immediately → connection refused
	pointAt(t, srv.URL)
	client := &http.Client{Timeout: 2 * time.Second}
	v, _ := ProbeSource(context.Background(), client, &Entry{Source: SourceSpec{Type: "ollama", OllamaName: "real:7b"}})
	if v != ProbeIndeterminate {
		t.Errorf("connection failure: verdict %v, want ProbeIndeterminate", v)
	}
}
