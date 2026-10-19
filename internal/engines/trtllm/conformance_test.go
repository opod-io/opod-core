package trtllm_test

import (
	"context"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/opod-io/opod/internal/engines"
	"github.com/opod-io/opod/internal/engines/enginetest"
	"github.com/opod-io/opod/internal/engines/trtllm"
)

// The suite every driver passes. trtllm-serve is on the OpenAI wire, so this
// checks the driver did not quietly change any of it (T19.15).
func TestConformance(t *testing.T) {
	usage := engines.Usage{PromptTokens: 7, CompletionTokens: 3, TotalTokens: 10}
	enginetest.Run(t, enginetest.Fixture{
		Name:    "trtllm",
		Backend: enginetest.OpenAIBackend([]string{"Qwen/Qwen2.5-0.5B-Instruct", "qwen2.5-0.5b"}, []string{"Hel", "lo"}, usage),
		Expect: enginetest.Expect{
			Models: []string{"Qwen/Qwen2.5-0.5B-Instruct", "qwen2.5-0.5b"},
			Chat:   "Hello",
			Usage:  &usage,
			Reason: "stop",
			// The shared client carries Embed; trtllm-serve has no embeddings
			// route, so the control plane offers this engine for chat only.
			Embeds: true,
		},
		NativeNames: []enginetest.NativeNameCase{
			{Source: engines.Source{ID: "qwen2.5-0.5b", Repo: "Qwen/Qwen2.5-0.5B-Instruct"}, Want: "Qwen/Qwen2.5-0.5B-Instruct"},
		},
	})
}

func TestAlias(t *testing.T) {
	if got := engines.Canonical("tensorrt-llm"); got != "trtllm" {
		t.Errorf("Canonical(tensorrt-llm) = %q, want trtllm", got)
	}
}

// Load reads KV utilisation and the queue from /prometheus/metrics.
func TestLoadReadsPrometheusMetrics(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/prometheus/metrics" {
			http.NotFound(w, r)
			return
		}
		_, _ = w.Write([]byte("# TYPE trtllm_kv_cache_utilization gauge\ntrtllm_kv_cache_utilization 0.25\ntrtllm_num_requests_waiting 3\n"))
	}))
	defer srv.Close()
	ld, err := trtllm.New(srv.URL, "").Load(context.Background())
	if err != nil || ld.KVUsedPct != 25 || ld.QueueDepth != 3 {
		t.Fatalf("load %+v err %v", ld, err)
	}
}
