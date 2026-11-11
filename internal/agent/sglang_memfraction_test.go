package agent

import (
	"context"
	"io"
	"log/slog"
	"runtime"
	"strings"
	"testing"
	"time"
)

// T29.19 (Hadi 2026-11-10: the hand setting, explained): SGLang's refusal names
// its own flag; the explanation names the engine flag an operator sets, at the
// next hundredth above SGLang's minimum.
func TestSGLangMemFractionExplain(t *testing.T) {
	tail := []string{
		"[2026-11-09] Scheduler hit an exception",
		"RuntimeError: Not enough memory. Please try to increase --mem-fraction-static. Current value: --mem-fraction-static=0.504. Raise --mem-fraction-static above 0.532 (minimum viable = 1 - available/pre = 0.5311). If using speculative decoding, the draft model also needs memory.",
		"Traceback (most recent call last):",
	}
	got := sglangMemFractionExplain(tail)
	for _, want := range []string{"mem_fraction_static to 0.54", "more than 0.532", "draft"} {
		if !strings.Contains(got, want) {
			t.Errorf("explanation %q does not say %q", got, want)
		}
	}
	if got := sglangMemFractionExplain([]string{"Raise --mem-fraction-static above 0.5"}); !strings.Contains(got, "to 0.51 ") {
		t.Errorf("an exact hundredth still goes strictly above: %q", got)
	}
	for _, none := range [][]string{{"nothing useful"}, {"Raise --mem-fraction-static above 0.995"}, {"Raise --mem-fraction-static above 1.2"}} {
		if got := sglangMemFractionExplain(none); got != "" {
			t.Errorf("%q: no advice expected, got %q", none, got)
		}
	}
}

// The supervisor puts Explain's sentence first in the exit error, before the
// output tail, and finds the line behind a long traceback.
func TestSupervisorCarriesTheExplanationOnTheExitError(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("POSIX shell")
	}
	script := `echo "Raise --mem-fraction-static above 0.532 (minimum viable = 0.5311)"
i=0; while [ $i -lt 120 ]; do echo "  File \"scheduler.py\", line $i"; i=$((i+1)); done; exit 1`
	sup := NewSupervisor(slog.New(slog.NewTextHandler(io.Discard, nil)))
	defer sup.StopAll()
	if _, err := sup.Start(context.Background(), ProcessSpec{
		ID: "sglang-explain", Command: "/bin/sh", Args: []string{"-c", script + "\n", "sh"},
		Restart: true, MaxRestarts: 1, RestartBackoff: 10 * time.Millisecond, Explain: sglangMemFractionExplain,
	}); err != nil {
		t.Fatalf("Start: %v", err)
	}
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		if p, ok := sup.Get("sglang-explain"); ok && p.ExitErr != "" {
			i := strings.Index(p.ExitErr, "set the engine flag mem_fraction_static to 0.54")
			j := strings.Index(p.ExitErr, "--- process output")
			if i < 0 || j < 0 || i > j {
				t.Fatalf("the explanation leads the exit error: %q", p.ExitErr)
			}
			return
		}
		time.Sleep(50 * time.Millisecond)
	}
	t.Fatal("the process never reported its exit error")
}
