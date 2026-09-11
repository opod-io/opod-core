package sglang

import (
	"context"
	"fmt"
	"net/http"
	"net/http/httptest"
	"testing"
)

// The autoscaler acts on these four numbers, so a metric name SGLang renamed is
// not a cosmetic problem: it silently pins a signal at zero and the loop stops
// growing on pressure it cannot see. Both spellings are therefore tested.
func TestLoadScrapesSGLangMetrics(t *testing.T) {
	for _, tc := range []struct {
		name string
		body string
		// gen_throughput is already a rate; the counter needs two samples.
		wantRateOnFirstSample bool
	}{
		{
			name: "current names",
			body: "sglang:token_usage{model_name=\"m\"} 0.42\nsglang:num_queue_reqs{model_name=\"m\"} 3\n" +
				"sglang:gen_throughput{model_name=\"m\"} 137.5\nsglang:cache_hit_rate{model_name=\"m\"} 0.61\n",
			wantRateOnFirstSample: true,
		},
		{
			name: "older names, hit rate as a percentage",
			body: "sglang:kv_cache_usage_perc{model_name=\"m\"} 0.42\nsglang:num_requests_waiting{model_name=\"m\"} 3\n" +
				"sglang:generation_tokens_total{model_name=\"m\"} 1000\nsglang:cache_hit_rate{model_name=\"m\"} 61\n",
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			calls := 0
			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				if r.URL.Path != "/metrics" {
					http.NotFound(w, r)
					return
				}
				calls++
				body := tc.body
				if !tc.wantRateOnFirstSample && calls > 1 {
					body = fmt.Sprintf("sglang:kv_cache_usage_perc 0.42\nsglang:num_requests_waiting 3\nsglang:generation_tokens_total %d\nsglang:cache_hit_rate 61\n", 1000+500*calls)
				}
				_, _ = w.Write([]byte(body))
			}))
			defer srv.Close()

			d := New(srv.URL, "")
			ld, err := d.Load(context.Background())
			if err != nil {
				t.Fatalf("load: %v", err)
			}
			if ld.KVUsedPct != 42 || ld.QueueDepth != 3 || ld.SampledAt == 0 {
				t.Fatalf("signals: %+v", ld)
			}
			if ld.PrefixHitPct != 61 {
				t.Fatalf("RadixAttention's hit rate is the reason to run SGLang; got %v", ld.PrefixHitPct)
			}
			if tc.wantRateOnFirstSample {
				if ld.TokensPerSec != 137.5 {
					t.Fatalf("gen_throughput is already a rate: %v", ld.TokensPerSec)
				}
				return
			}
			if ld.TokensPerSec != 0 {
				t.Fatalf("a counter cannot yield a rate on the first sample: %v", ld.TokensPerSec)
			}
			if ld, _ = d.Load(context.Background()); ld.TokensPerSec <= 0 {
				t.Fatalf("second sample must carry a generation rate: %+v", ld)
			}
		})
	}
}

// An engine with no sleep mode must say so through the interface, not through a
// comment: the control plane checks the capability before it plans a sleep tier.
func TestNoSleepTier(t *testing.T) {
	var e any = New("http://x", "")
	if _, ok := e.(interface{ Sleep(context.Context) error }); ok {
		t.Error("SGLang has no sleep mode — claiming one would have the control plane park an endpoint that never wakes")
	}
}
