package main

import (
	"io"
	"os"
	"strings"
	"testing"

	"github.com/opod-io/opod/internal/config"
	"github.com/opod-io/opod/internal/store"
)

// captureOut runs f with os.Stdout sent to a pipe and returns what it wrote.
func captureOut(t *testing.T, f func()) string {
	t.Helper()
	r, w, err := os.Pipe()
	if err != nil {
		t.Fatal(err)
	}
	old := os.Stdout
	os.Stdout = w
	defer func() { os.Stdout = old }()
	f()
	_ = w.Close()
	b, _ := io.ReadAll(r)
	return string(b)
}

// `opod config show` with no leader says "secrets redacted" and must be:
// neither the admin nor the join token appears, in either output form.
func TestConfigShowLocalRedactsTheTokens(t *testing.T) {
	cfg := &config.Config{}
	cfg.Auth.AdminToken = "sk-orc-placeholder-admin-0123456789"
	cfg.Auth.JoinToken = "sk-orc-placeholder-join-9876543210"
	cfg.Engine.VLLMAPIKey = "vllm-placeholder-key-abcdef"
	for _, asJSON := range []bool{false, true} {
		out := captureOut(t, func() { printConfigLocal(cfg, asJSON) })
		for _, secret := range []string{cfg.Auth.AdminToken, cfg.Auth.JoinToken, cfg.Engine.VLLMAPIKey} {
			if strings.Contains(out, secret) {
				t.Errorf("json=%v: the output carries a secret %q:\n%s", asJSON, secret[:10], out)
			}
		}
		if !strings.Contains(out, "vllm_api_key") {
			t.Errorf("json=%v: the view lost its fields:\n%s", asJSON, out)
		}
	}
}

// `opod node show` prints the row without the worker's credential (T10.18):
// the token is redacted to its prefix and last four, never printed whole.
func TestNodeShowRedactsTheWorkerToken(t *testing.T) {
	n := store.Node{ID: "n1", WorkerToken: "sk-orc-placeholder-worker-0123456789"}
	out := nodeShowJSON(n, []string{"m"})
	if strings.Contains(out, n.WorkerToken) || !strings.Contains(out, "sk-orc…6789") || !strings.Contains(out, `"loadedModels"`) {
		t.Fatalf("node show:\n%s", out)
	}
}
