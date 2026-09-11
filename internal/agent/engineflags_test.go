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

func TestLlamaModelSource(t *testing.T) {
	if got := strings.Join(llamaModelSource("id", "org/repo-GGUF", "q4.gguf", ""), " "); got != "-hf org/repo-GGUF --hf-file q4.gguf" {
		t.Fatalf("repo+file: %q", got)
	}
	if got := strings.Join(llamaModelSource("id", "org/repo", "", "/data/models/q4.gguf"), " "); got != "-m /data/models/q4.gguf" {
		t.Fatalf("cached path wins: %q", got)
	}
	if got := strings.Join(llamaModelSource("id", "org/repo", "", ""), " "); got != "-hf org/repo" {
		t.Fatalf("repo only: %q", got)
	}
}

func TestLlamaOffloadArgs(t *testing.T) {
	// A worker holding a card must ask llama.cpp for the offload; without the
	// argument every layer stays on the CPU and the reserved card does nothing.
	if got := (EngineFlags{}).llamaOffloadArgs(true); len(got) != 2 || got[0] != "--n-gpu-layers" || got[1] != "999" {
		t.Fatalf("an accelerated worker offloads every layer, got %v", got)
	}
	// A CPU worker says nothing at all.
	if got := (EngineFlags{}).llamaOffloadArgs(false); got != nil {
		t.Fatalf("a CPU worker must not ask for an offload, got %v", got)
	}
	// A plan that pins the layer count wins, on any hardware.
	for _, accelerated := range []bool{true, false} {
		if got := (EngineFlags{"ngl": "20"}).llamaOffloadArgs(accelerated); got != nil {
			t.Fatalf("a pinned ngl must not be overridden (accelerated=%v), got %v", accelerated, got)
		}
	}
	// An empty value is not a pin.
	if got := (EngineFlags{"ngl": " "}).llamaOffloadArgs(true); len(got) != 2 {
		t.Fatalf("a blank ngl is not a pin, got %v", got)
	}
}

func TestAcceleratorPresent(t *testing.T) {
	withGPU := Capabilities{GPUs: []GPU{{Name: "card", VRAMGB: 24}}}
	for _, tc := range []struct {
		env  string
		caps Capabilities
		want bool
	}{
		{"nvidia", Capabilities{}, true}, // the control plane's word beats a probe that cannot see AMD or Intel
		{"amd", Capabilities{}, true},
		{"none", withGPU, false}, // a CPU worker, whatever the host has
		{"", withGPU, true},      // standalone: fall back to detection
		{"", Capabilities{}, false},
		{"  NVIDIA  ", Capabilities{}, true},
	} {
		t.Setenv("OPOD_ACCELERATOR", tc.env)
		if got := AcceleratorPresent(tc.caps); got != tc.want {
			t.Fatalf("OPOD_ACCELERATOR=%q gpus=%d: want %v, got %v", tc.env, len(tc.caps.GPUs), tc.want, got)
		}
	}
}
