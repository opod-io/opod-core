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
		cmd := exec.Command("sh", "-c", MemFractionShell("0.85")+`printf %s "$U"`)
		cmd.Env = []string{"PATH=" + dir + ":" + os.Getenv("PATH"), "OPOD_VRAM_BUDGET_GB=" + budget}
		out, err := cmd.Output()
		if err != nil {
			t.Fatal(err)
		}
		return strings.TrimSpace(string(out))
	}
	for _, c := range []struct{ total, budget, want string }{
		{"49140", "10", "0.19"}, // the measured case: 0.21 cost the card 11.08 GB
		{"8192", "6", "0.65"},   // (6144-768)/8192 = 0.656, cut
		{"8192", "8", "0.90"},   // the whole card as a budget still leaves the overhead
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
