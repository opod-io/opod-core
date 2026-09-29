package scheduler

import (
	"strings"
	"testing"

	"github.com/opod-io/opod/internal/agent"
)

// A gang part is started with the budget its PLAN gave it, not with a constant.
//
// This is the twin of TestSGLangGangHonoursThePartsVRAMBudget, and it exists
// because the fix that landed there on 2026-09-22 did not reach this start
// path: vllmray.go carried its own copy of the command with a literal
// `--gpu-memory-utilization 0.85` for four more days. Every unit test of the
// WORKER's vLLM launch passed the whole time, because vllmShellOverrides is
// correct — the gang path has its own command, and nothing asserted over it.
//
// On a dedicated whole-card part 0.85 and the budget coincide and nothing shows.
// On a PACKED card the part takes memory the ledger promised to another
// endpoint, and the ledger is then wrong rather than merely optimistic.
func TestVLLMRayGangHonoursThePartsVRAMBudget(t *testing.T) {
	got := vllmGangCommand(vllmGang{
		Model: "org/model", ServedAs: "m", Host: "10.0.0.1", Port: 9100, TP: 1, PP: 2,
	})
	if strings.Contains(got, "--gpu-memory-utilization 0.85") {
		t.Errorf("the fraction must come from OPOD_VRAM_BUDGET_GB, not a constant:\n%s", got)
	}
	if !strings.Contains(got, "OPOD_VRAM_BUDGET_GB") {
		t.Errorf("the budget line must run before the exec:\n%s", got)
	}
	if !strings.Contains(got, `--gpu-memory-utilization "$U"`) {
		t.Errorf("the flag must read the variable that line writes:\n%s", got)
	}
	// And it is the SAME rule the worker's own launches and the SGLang gang use —
	// one rule, three readers; a second copy is how the constant got here in the
	// first place.
	if !strings.Contains(got, agent.MemFractionShell("0.85")) {
		t.Errorf("the gang must use agent.MemFractionShell, not its own arithmetic:\n%s", got)
	}
	// The budget line writes $U, so it can only come BEFORE the exec that reads it.
	if strings.Index(got, "OPOD_VRAM_BUDGET_GB") > strings.Index(got, "exec vllm serve") {
		t.Errorf("the budget line must precede the exec, or $U is unset when the flag reads it:\n%s", got)
	}
}

// The rest of the coordinator's command line is what the router and the engine
// both depend on, and none of it had a test either.
func TestVLLMRayGangCommandShape(t *testing.T) {
	got := vllmGangCommand(vllmGang{
		Model: "org/model", ServedAs: "catalog-id", Host: "10.0.0.1", Port: 9100, TP: 2, PP: 3,
	})
	for _, want := range []string{
		// Served under the catalog id AND the repo name: a coordinator that served
		// the id alone answered 404 "model does not exist" (cell, 2026-09-14).
		"vllm serve 'org/model'", "--served-model-name 'catalog-id'",
		"--distributed-executor-backend ray",
		"--tensor-parallel-size 2", "--pipeline-parallel-size 3",
		"--host 10.0.0.1", "--port 9100",
	} {
		if !strings.Contains(got, want) {
			t.Errorf("missing %q in:\n%s", want, got)
		}
	}
}

// Eager mode is the cross-machine-TP escape hatch (132 s per CUDA-graph capture
// over the overlay), and it must be absent when it was not asked for — a gang
// that silently ran eager would be slower per token for no stated reason.
func TestVLLMRayGangEagerOnlyWhenAsked(t *testing.T) {
	base := vllmGang{Model: "m", ServedAs: "m", Host: "h", Port: 9100, TP: 1, PP: 1}
	if got := vllmGangCommand(base); strings.Contains(got, "--enforce-eager") {
		t.Errorf("eager must not appear unless asked:\n%s", got)
	}
	base.Eager = true
	if got := vllmGangCommand(base); !strings.Contains(got, "--enforce-eager") {
		t.Errorf("eager was asked for and is missing:\n%s", got)
	}
}

// A model name is placed inside single quotes, so a quote in it would end the
// argument and hand the rest to the shell. sglangGangCommand has always escaped;
// this path had not.
func TestVLLMRayGangQuotesTheModelName(t *testing.T) {
	got := vllmGangCommand(vllmGang{
		Model: "org/mo'del", ServedAs: "m", Host: "h", Port: 9100, TP: 1, PP: 1,
	})
	if strings.Contains(got, "'org/mo'del'") {
		t.Errorf("an unescaped quote ends the argument and hands the rest to the shell:\n%s", got)
	}
	if !strings.Contains(got, `'org/mo'\''del'`) {
		t.Errorf("the model name must be shell-escaped:\n%s", got)
	}
}
