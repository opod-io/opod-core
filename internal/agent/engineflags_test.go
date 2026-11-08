package agent

import (
	"strings"
	"testing"
)

func TestEngineFlags(t *testing.T) {
	f := ParseEngineFlags(`{"tp":2,"gpu_memory_utilization":"0.6","max_model_len":8192,"kv_cache_dtype":"fp8","extra":"--enable-prefix-caching; rm -rf /"}`)
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
	f = ParseEngineFlags(`{"ctx":"16384","ngl":99,"parallel":4,"kv_cache_type":"q8_0","tp":"nope"}`)
	got := strings.Join(f.llamaArgs(), " ")
	if got != "-c 16384 --n-gpu-layers 99 -np 4 -ctk q8_0 -ctv q8_0" {
		t.Fatalf("llama args: %q", got)
	}
	if sh, _ := f.vllmShellOverrides(); sh != "" {
		t.Fatalf("invalid tp must be ignored: %q", sh)
	}
	if len(ParseEngineFlags("")) != 0 {
		t.Fatal("unset → empty")
	}
}

// The KV layout is the manager's to say (G39 O-05): a shared worker runs one
// pool its slots share, a dedicated one a partition per slot. A build picks one
// when not told, and the default changed between builds, so the flag is
// rendered either way and anything else renders nothing.
func TestLlamaKVLayoutIsSaidEitherWay(t *testing.T) {
	for raw, want := range map[string]string{
		`{"kv_unified":"true"}`:  "--kv-unified",
		`{"kv_unified":true}`:    "--kv-unified",
		`{"kv_unified":"false"}`: "--no-kv-unified",
		`{"kv_unified":"maybe"}`: "",
		`{}`:                     "",
	} {
		if got := strings.Join(ParseEngineFlags(raw).llamaArgs(), " "); got != want {
			t.Errorf("%s: llama args %q, want %q", raw, got, want)
		}
	}
	// The gang coordinator is a llama-server too, and takes the same flags.
	if got := strings.Join(ParseEngineFlags(`{"kv_unified":"true"}`).LlamaServerArgs(), " "); got != "--kv-unified" {
		t.Errorf("a gang head takes the layout too: %q", got)
	}
}

// SGLang takes the KV cache's element type from a plan, as vLLM does (T23.10).
func TestSGLangTakesTheKVElementType(t *testing.T) {
	_, args := ParseEngineFlags(`{"kv_cache_dtype":"fp8_e5m2"}`).sglangShellOverrides()
	if args != "'--kv-cache-dtype' 'fp8_e5m2' '--enable-cache-report'" {
		t.Fatalf("sglang args: %q", args)
	}
	if _, args := ParseEngineFlags(`{"kv_cache_dtype":"fp8; rm -rf /"}`).sglangShellOverrides(); args != "'--enable-cache-report'" {
		t.Fatalf("a value that is not a plain token is dropped: %q", args)
	}
}

// T32.47: vLLM and SGLang put the cached prompt tokens in their answer only
// with their own switch, so the line always carries it — once, even when the
// plan's extra already names it.
func TestTheEngineLineAsksForTheCachedTokenCount(t *testing.T) {
	for _, c := range []struct {
		name, flags, want string
		line              func(EngineFlags) string
	}{
		{"vllm", `{}`, "'--enable-prompt-tokens-details'", func(f EngineFlags) string { _, a := f.vllmShellOverrides(); return a }},
		{"vllm extra", `{"extra":"--enable-prompt-tokens-details"}`, "'--enable-prompt-tokens-details'", func(f EngineFlags) string { _, a := f.vllmShellOverrides(); return a }},
		{"sglang", `{}`, "'--enable-cache-report'", func(f EngineFlags) string { _, a := f.sglangShellOverrides(); return a }},
		{"sglang extra", `{"extra":"--enable-cache-report"}`, "'--enable-cache-report'", func(f EngineFlags) string { _, a := f.sglangShellOverrides(); return a }},
	} {
		got := c.line(ParseEngineFlags(c.flags))
		if n := strings.Count(got, c.want); n != 1 {
			t.Errorf("%s: %q carries %s %d times, want once", c.name, got, c.want, n)
		}
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
		if got := AcceleratorPresent(tc.caps, tc.env); got != tc.want {
			t.Fatalf("OPOD_ACCELERATOR=%q gpus=%d: want %v, got %v", tc.env, len(tc.caps.GPUs), tc.want, got)
		}
	}
}

// The vLLM line always carries --max-num-seqs (ADR-091): the plan's value, else
// the stated default — unless the plan's raw extra args set it themselves —
// and LaunchSlots reports the number that is on the line.
func TestVLLMMaxNumSeqsIsAlwaysOnTheLine(t *testing.T) {
	cases := []struct {
		flags, wantArg string
		want           int
	}{
		{`{}`, "'--max-num-seqs' '256'", DefaultVLLMMaxNumSeqs},
		{`{"max_num_seqs":32}`, "'--max-num-seqs' '32'", 32},
		{`{"extra":"--max-num-seqs 48"}`, "'--max-num-seqs' '48'", 48},
		{`{"extra":"--max-num-seqs=12"}`, "'--max-num-seqs=12'", 12},
	}
	for _, c := range cases {
		f := ParseEngineFlags(c.flags)
		_, extra := f.vllmShellOverrides()
		if !strings.Contains(extra, c.wantArg) || strings.Count(extra, "--max-num-seqs") != 1 {
			t.Errorf("%s: want exactly one %s on the line, got %q", c.flags, c.wantArg, extra)
		}
		if got := f.LaunchSlots("vllm"); got != c.want {
			t.Errorf("%s: LaunchSlots(vllm) = %d, want %d", c.flags, got, c.want)
		}
	}
	if got := ParseEngineFlags(`{"parallel":6}`).LaunchSlots("llamacpp"); got != 6 {
		t.Errorf("llama.cpp -np 6: LaunchSlots = %d", got)
	}
	if got := ParseEngineFlags(`{}`).LaunchSlots("llamacpp"); got != 0 {
		t.Errorf("llama.cpp with no -np: only the engine can say, got %d", got)
	}
	if got := ParseEngineFlags(`{"max_running_requests":64}`).LaunchSlots("sglang"); got != 64 {
		t.Errorf("sglang: LaunchSlots = %d", got)
	}
	if got := ParseEngineFlags(`{}`).LaunchSlots("ollama"); got != 0 {
		t.Errorf("ollama has no slot count: got %d", got)
	}
}

// ADR-120: speculative decoding in each engine's own spelling; a draft method
// with no draft renders nothing, and the plan's extra wins.
func TestSpeculativeDecodingIsRenderedPerEngine(t *testing.T) {
	vllm := func(flags string) string { _, a := ParseEngineFlags(flags).vllmShellOverrides(); return a }
	sglang := func(flags string) string { _, a := ParseEngineFlags(flags).sglangShellOverrides(); return a }
	llama := func(flags string) string { return strings.Join(ParseEngineFlags(flags).llamaArgs(), " ") }
	for _, c := range []struct {
		name, got, want string
	}{
		{"vllm ngram", vllm(`{"spec_method":"ngram"}`), `'--speculative-config' '{"method":"ngram","num_speculative_tokens":4,"prompt_lookup_max":4,"prompt_lookup_min":2}'`},
		{"vllm draft", vllm(`{"spec_method":"draft","spec_tokens":"3","spec_draft":"/data/models/qwen-0.5b@main"}`), `'--speculative-config' '{"method":"draft_model","model":"/data/models/qwen-0.5b@main","num_speculative_tokens":3}'`},
		{"sglang ngram", sglang(`{"spec_method":"ngram","spec_tokens":"6"}`), `'--speculative-algorithm' 'NGRAM' '--speculative-num-draft-tokens' '6'`},
		{"sglang draft", sglang(`{"spec_method":"draft","spec_draft":"Qwen/Qwen2.5-0.5B-Instruct"}`), `'--speculative-algorithm' 'STANDALONE' '--speculative-draft-model-path' 'Qwen/Qwen2.5-0.5B-Instruct' '--speculative-num-steps' '4' '--speculative-eagle-topk' '1' '--speculative-num-draft-tokens' '5'`},
		{"llama ngram", llama(`{"spec_method":"ngram"}`), `--spec-type ngram-simple --spec-ngram-simple-size-m 4`},
		{"llama draft", llama(`{"spec_method":"draft","spec_draft":"/data/models/draft.gguf"}`), `--spec-type draft-simple --spec-draft-model /data/models/draft.gguf --spec-draft-n-max 4 --spec-draft-ngl 999`},
	} {
		if !strings.Contains(c.got, c.want) {
			t.Errorf("%s: %q does not carry %q", c.name, c.got, c.want)
		}
	}
	for name, got := range map[string]string{
		"draft with no draft": vllm(`{"spec_method":"draft"}`),
		"unknown method":      sglang(`{"spec_method":"eagle"}`),
		"unsafe draft":        llama(`{"spec_method":"draft","spec_draft":"x; rm -rf /"}`),
		"extra wins (vllm)":   vllm(`{"spec_method":"ngram","extra":"--speculative-config.method=ngram"}`),
		"extra wins (sglang)": sglang(`{"spec_method":"ngram","extra":"--speculative-algorithm EAGLE"}`),
		"extra wins (llama)":  llama(`{"spec_method":"ngram","extra":"--spec-type ngram-map-k"}`),
	} {
		if strings.Contains(got, "--speculative-config' '{") || strings.Contains(got, "'NGRAM'") || strings.Contains(got, "STANDALONE") || strings.Contains(got, "ngram-simple") || strings.Contains(got, "draft-simple") {
			t.Errorf("%s rendered speculation: %q", name, got)
		}
	}
	if got := vllm(`{"spec_method":"ngram","spec_tokens":"99"}`); !strings.Contains(got, `"num_speculative_tokens":4`) {
		t.Errorf("a token count out of 1–16 falls back to the default: %q", got)
	}
}
