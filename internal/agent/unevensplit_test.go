package agent

import (
	"strings"
	"testing"
)

// Cards of different sizes can only be pooled by an UNEVEN split: tensor
// parallel gives every rank the same share, so the smallest card bounds the
// rest. llama.cpp takes proportions on the command line; vLLM and SGLang take
// per-stage layer counts from the environment.
func TestUnevenSplitReachesEachEngine(t *testing.T) {
	if got := strings.Join(EngineFlags{"tensor_split": "12,8"}.llamaArgs(), " "); !strings.Contains(got, "--tensor-split 12,8") {
		t.Errorf("llama.cpp: %q", got)
	}
	sh, _ := EngineFlags{"pp_layer_partition": "8,12,12,8"}.vllmShellOverrides()
	if !strings.Contains(sh, "export VLLM_PP_LAYER_PARTITION=8,12,12,8;") {
		t.Errorf("vLLM: %q", sh)
	}
	sh, _ = EngineFlags{"pp_layer_partition": "15,15,15,16"}.sglangShellOverrides()
	if !strings.Contains(sh, "export SGLANG_PP_LAYER_PARTITION=15,15,15,16;") {
		t.Errorf("SGLang: %q", sh)
	}
}

// These values become a shell export and a CLI argument, so they are numbers
// and commas or they are nothing.
func TestASplitIsNumbersAndCommasOrNothing(t *testing.T) {
	for _, bad := range []string{"8;rm -rf /", "$(whoami)", "8,12 && echo", "`id`", "abc", "", "8|12", "--flag"} {
		if isSplitList(bad) {
			t.Errorf("accepted %q", bad)
		}
		if got := strings.Join(EngineFlags{"tensor_split": bad}.llamaArgs(), " "); strings.Contains(got, "tensor-split") {
			t.Errorf("llama.cpp took %q: %q", bad, got)
		}
		sh, _ := EngineFlags{"pp_layer_partition": bad}.vllmShellOverrides()
		if strings.Contains(sh, "PP_LAYER_PARTITION") {
			t.Errorf("vLLM took %q: %q", bad, sh)
		}
	}
	for _, ok := range []string{"12,8", "8,12,12,8", "0.6,0.4", "1"} {
		if !isSplitList(ok) {
			t.Errorf("refused %q", ok)
		}
	}
}
