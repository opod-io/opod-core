package engines

import (
	"strings"
	"testing"
	"time"
)

func TestParsePromTextAndRate(t *testing.T) {
	m := ParsePromText(strings.NewReader(`# HELP vllm:gpu_cache_usage_perc x
# TYPE vllm:gpu_cache_usage_perc gauge
vllm:gpu_cache_usage_perc{model_name="m"} 0.42
vllm:num_requests_waiting{model_name="m"} 3
vllm:generation_tokens_total{model_name="m"} 1000
vllm:request_latency_seconds_bucket{le="0.5",model_name="m"} 7
llamacpp:kv_cache_usage_ratio 0.9
bad line here
`))
	if m["vllm:gpu_cache_usage_perc"] != 0.42 || m["vllm:num_requests_waiting"] != 3 || m["llamacpp:kv_cache_usage_ratio"] != 0.9 || m["vllm:request_latency_seconds_bucket"] != 7 {
		t.Fatalf("parsed %+v", m)
	}
	var r RateTracker
	t0 := time.Unix(1000, 0)
	if got := r.Rate(1000, t0); got != 0 {
		t.Fatalf("first sample rate %v", got)
	}
	if got := r.Rate(1100, t0.Add(10*time.Second)); got != 10 {
		t.Fatalf("rate %v, want 10", got)
	}
	if got := r.Rate(5, t0.Add(20*time.Second)); got != 0 {
		t.Fatalf("counter reset must read 0, got %v", got)
	}
	if v, ok := First(m, "vllm:kv_cache_usage_perc", "vllm:gpu_cache_usage_perc"); !ok || v != 0.42 {
		t.Fatalf("alias lookup %v %v", v, ok)
	}
}

// An info metric's facts are its labels (vLLM's cache_config_info); a
// malformed line elsewhere in the exposition does not stop them being read.
func TestPromLabelsReadsAnInfoMetric(t *testing.T) {
	text := "not a metric line {{{\n" +
		`vllm:cache_config_info{block_size="16",engine="0",num_gpu_blocks="2048"} 1.0` + "\n" +
		`vllm:cache_config_info{block_size="32",engine="1",num_gpu_blocks="None"} 1` + "\n" +
		`vllm:cache_config_info_other{x="y"} 1` + "\n"
	got := PromLabels(text, "vllm:cache_config_info")
	if len(got) != 2 || got[0]["num_gpu_blocks"] != "2048" || got[1]["block_size"] != "32" || got[1]["num_gpu_blocks"] != "None" {
		t.Fatalf("labels: %v", got)
	}
	if PromLabels(text, "vllm:absent") != nil {
		t.Fatal("an absent metric has no series")
	}
}
