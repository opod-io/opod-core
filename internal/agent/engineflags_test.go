package agent

import (
	"strings"
	"testing"
)

func TestEngineFlags(t *testing.T) {
	t.Setenv("OPOD_ENGINE_FLAGS", `{"tp":2,"gpu_memory_utilization":"0.6","max_model_len":8192,"kv_cache_dtype":"fp8","extra":"--enable-prefix-caching; rm -rf /"}`)
	f := engineFlagsFromEnv()
	sh, extra := f.vllmShellOverrides()
	if sh != "TP=2; U=0.60;" {
		t.Fatalf("vllm overrides: %q", sh)
	}
	if !strings.Contains(extra, "'--max-model-len' '8192' '--kv-cache-dtype' 'fp8'") {
		t.Fatalf("vllm extra: %q", extra)
	}
	// every token is single-quoted and shell metacharacters never survive:
	// "--enable-prefix-caching;" is dropped, what remains are inert argv words
	if strings.Contains(extra, ";") || strings.Contains(extra, "$") || strings.Contains(extra, "`") {
		t.Fatalf("shell metacharacters must never pass through: %q", extra)
	}
	for _, tok := range strings.Fields(extra) {
		if !strings.HasPrefix(tok, "'") || !strings.HasSuffix(tok, "'") {
			t.Fatalf("every extra token must be quoted: %q in %q", tok, extra)
		}
	}
	t.Setenv("OPOD_ENGINE_FLAGS", `{"ctx":"16384","ngl":99,"parallel":4,"kv_cache_type":"q8_0","tp":"nope"}`)
	f = engineFlagsFromEnv()
	got := strings.Join(f.llamaArgs(), " ")
	if got != "-c 16384 --n-gpu-layers 99 -np 4 -ctk q8_0 -ctv q8_0" {
		t.Fatalf("llama args: %q", got)
	}
	if sh, _ := f.vllmShellOverrides(); sh != "" {
		t.Fatalf("invalid tp must be ignored: %q", sh)
	}
	t.Setenv("OPOD_ENGINE_FLAGS", "")
	if len(engineFlagsFromEnv()) != 0 {
		t.Fatal("unset → empty")
	}
}
