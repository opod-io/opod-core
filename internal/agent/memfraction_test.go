package agent

import (
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

// The rule is a shell line, so it is run: a stand-in nvidia-smi says how big the
// card is, and the fraction that comes out must keep the PROCESS inside the
// budget — the engine's pool plus the overhead the pool does not count.
func TestTheFractionKeepsTheProcessInsideItsBudget(t *testing.T) {
	if _, err := exec.LookPath("awk"); err != nil {
		t.Skip("no awk on this machine")
	}
	dir := t.TempDir()
	smi := func(totalMiB string) {
		if err := os.WriteFile(filepath.Join(dir, "nvidia-smi"), []byte("#!/bin/sh\necho "+totalMiB+"\n"), 0o755); err != nil {
			t.Fatal(err)
		}
	}
	run := func(budget string) string {
		cmd := exec.Command("sh", "-c", MemFractionShell("vllm")+`printf %s "$U"`)
		cmd.Env = []string{"PATH=" + dir + ":" + os.Getenv("PATH"), "OPOD_VRAM_BUDGET_GB=" + budget}
		out, err := cmd.Output()
		if err != nil {
			t.Fatal(err)
		}
		return strings.TrimSpace(string(out))
	}
	for _, c := range []struct{ total, budget, want string }{
		// No OPOD_ENGINE_OVERHEAD_MIB: vLLM's largest measured overhead, 1103 MiB.
		{"49140", "10", "0.18"}, // the measured case: 0.21 cost the card 11.08 GB
		{"8192", "6", "0.61"},   // (6144-1103)/8192 = 0.615, cut
		{"8192", "8", "0.86"},   // the whole card as a budget still leaves the overhead
		{"49140", "1", "0.05"},  // the floor: a budget no model fits is the engine's error to make
		{"49140", "0", "0.85"},  // no budget: the default, untouched
	} {
		smi(c.total)
		if got := run(c.budget); got != c.want {
			t.Errorf("a %s GB budget on a %s MiB card: fraction %s, want %s", c.budget, c.total, got, c.want)
		}
	}
	// No nvidia-smi answer: the default stands.
	_ = os.WriteFile(filepath.Join(dir, "nvidia-smi"), []byte("#!/bin/sh\nexit 1\n"), 0o755)
	if got := run("10"); got != "0.85" {
		t.Errorf("with no reading of the card the default stands: %s", got)
	}
}

// An SGLang command line carries the fraction only when a budget produced one.
// With no budget SGLang sizes its own pool from the card; the fixed 0.85 that
// stood in its place left an 8 GB card 0.6 GB for SGLang's workspace and a
// two-machine gang died on its first allocation (design-partner cell,
// 2026-10-16). Run as a shell line, because the flag is a shell expansion.
func TestSGLangGetsAFractionOnlyFromABudget(t *testing.T) {
	if _, err := exec.LookPath("awk"); err != nil {
		t.Skip("no awk on this machine")
	}
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "nvidia-smi"), []byte("#!/bin/sh\necho 8192\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	run := func(budget string) string {
		// printf '[%s]' shows each word the flag expands to, and nothing when it is empty.
		cmd := exec.Command("sh", "-c", MemFractionShell("sglang")+`printf '[%s]' x `+SGLangMemFractionFlag)
		cmd.Env = []string{"PATH=" + dir + ":" + os.Getenv("PATH"), "OPOD_VRAM_BUDGET_GB=" + budget}
		out, err := cmd.Output()
		if err != nil {
			t.Fatal(err)
		}
		return string(out)
	}
	if got := run("0"); got != "[x]" {
		t.Errorf("no budget: SGLang must size itself, got the words %s", got)
	}
	if got := run("6"); got != "[x][--mem-fraction-static][0.65]" {
		t.Errorf("a 6 GB budget on an 8 GB card: got the words %s", got)
	}
}

// The card's size comes from the manager first (OPOD_CARD_MIB, PLAN T23.3):
// a host with no nvidia-smi — AMD, Intel — used to start every budgeted engine
// at its default, the budget silently not applied. The manager's number also
// wins over nvidia-smi's, so both repos divide by the same card.
func TestTheCardSizeFromTheManagerWins(t *testing.T) {
	if _, err := exec.LookPath("awk"); err != nil {
		t.Skip("no awk on this machine")
	}
	empty := t.TempDir() // no nvidia-smi here
	lying := t.TempDir()
	if err := os.WriteFile(filepath.Join(lying, "nvidia-smi"), []byte("#!/bin/sh\necho 49140\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	run := func(engine, pathDir, budget, card string) string {
		cmd := exec.Command("sh", "-c", MemFractionShell(engine)+`printf %s "$U"`)
		cmd.Env = []string{"PATH=" + pathDir + ":/usr/bin:/bin", "OPOD_VRAM_BUDGET_GB=" + budget}
		if card != "" {
			cmd.Env = append(cmd.Env, "OPOD_CARD_MIB="+card)
		}
		out, err := cmd.Output()
		if err != nil {
			t.Fatal(err)
		}
		return strings.TrimSpace(string(out))
	}
	for _, c := range []struct{ engine, dir, budget, card, want, why string }{
		{"vllm", empty, "6", "8192", "0.61", "no nvidia-smi: the manager's card size applies the budget"},
		{"vllm", lying, "6", "8192", "0.61", "the manager's card size wins over nvidia-smi's"},
		{"vllm", lying, "6", "", "0.10", "unset: nvidia-smi as before ((6144-1103)/49140)"},
		{"vllm", empty, "6", "", "0.85", "neither: the default stands"},
		{"vllm", empty, "6", "8 GiB", "0.85", "a value that is not a number is not a card size"},
		{"vllm", empty, "0", "8192", "0.85", "no budget: the card size changes nothing"},
		{"sglang", empty, "6", "8192", "0.65", "SGLang divides by the same card"},
		{"sglang", empty, "0", "8192", "", "SGLang with no budget sizes itself"},
	} {
		if got := run(c.engine, c.dir, c.budget, c.card); got != c.want {
			t.Errorf("%s: %s budget %s card %q: fraction %q, want %q", c.why, c.engine, c.budget, c.card, got, c.want)
		}
	}
}

// Every engine whose fraction a budget is translated into has an overhead:
// vLLM's from the SDK's per-vendor table, SGLang's from its own row.
func TestEveryFractionEngineHasOneOverheadRow(t *testing.T) {
	for _, engine := range []string{"vllm", "sglang"} {
		if _, ok := memKnobs[engine]; !ok || overheadMiB(engine) <= 0 {
			t.Errorf("%s: no overhead (%d)", engine, overheadMiB(engine))
		}
	}
	if got := MemFractionShell("llamacpp"); !strings.HasPrefix(got, "U=; ") {
		t.Errorf("an engine with no fraction gets no default: %s", got)
	}
}

// The manager names the overhead for the card's vendor (OPOD_ENGINE_OVERHEAD_MIB,
// PLAN T23.3): vLLM measured 837 MiB beside its pool on NVIDIA and 1103 on AMD.
// Unset or not a number, the SDK's figure for an unmeasured vendor applies.
func TestTheManagersOverheadWins(t *testing.T) {
	if _, err := exec.LookPath("awk"); err != nil {
		t.Skip("no awk on this machine")
	}
	run := func(overhead string) string {
		cmd := exec.Command("sh", "-c", MemFractionShell("vllm")+`printf %s "$U"`)
		cmd.Env = []string{"PATH=/usr/bin:/bin", "OPOD_VRAM_BUDGET_GB=6", "OPOD_CARD_MIB=8192"}
		if overhead != "" {
			cmd.Env = append(cmd.Env, "OPOD_ENGINE_OVERHEAD_MIB="+overhead)
		}
		out, err := cmd.Output()
		if err != nil {
			t.Fatal(err)
		}
		return strings.TrimSpace(string(out))
	}
	for _, c := range []struct{ overhead, want, why string }{
		{"837", "0.64", "NVIDIA: (6144-837)/8192 = 0.648, cut"},
		{"1103", "0.61", "AMD: (6144-1103)/8192 = 0.615, cut"},
		{"", "0.61", "unset: the largest measured figure"},
		{"lots", "0.61", "not a number: the largest measured figure"},
	} {
		if got := run(c.overhead); got != c.want {
			t.Errorf("%s: overhead %q gives %s, want %s", c.why, c.overhead, got, c.want)
		}
	}
}
