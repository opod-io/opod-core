package agent

import (
	"context"
	"io"
	"log/slog"
	"os"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"
	"testing"
	"time"
)

func TestVLLMFittedLenReadsTheHint(t *testing.T) {
	tail := []string{
		"INFO loading weights",
		`ValueError: To serve at least one request with the model's max seq len (131072), (16.0 GiB KV cache is needed, which is larger than the available KV cache memory (8.1 GiB). Based on the available memory, the estimated maximum model length is 66352. Try increasing gpu_memory_utilization`,
		"RuntimeError: Engine core initialization failed.",
	}
	n, ok := vllmFittedLen(tail)
	if !ok || n != 59648 { // 66352 × 0.9 = 59716 → 59648 on a 256 boundary
		t.Fatalf("got %d %v", n, ok)
	}
	if _, ok := vllmFittedLen([]string{"nothing useful"}); ok {
		t.Fatal("no hint must mean no change")
	}
	if vllmFitContext(EngineFlags{"max_model_len": "8192"}, "x") != nil {
		t.Fatal("a pinned max_model_len is the operator's choice: no hook")
	}
	args, ok := vllmFitContext(EngineFlags{}, "exec vllm serve m")(tail)
	if !ok || !strings.HasSuffix(args[1], " --max-model-len 59648") || args[0] != "-lc" {
		t.Fatalf("adapted args: %v %v", args, ok)
	}
}

// The supervisor consults Adapt before a restart and relaunches with the new
// arguments: a script that fails while printing vLLM's hint, then succeeds
// once it is started with the fitted length.
func TestSupervisorAdaptsArgsOnCrash(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("POSIX shell")
	}
	dir := t.TempDir()
	marker := filepath.Join(dir, "fitted")
	// vLLM prints the hint and then two tracebacks (~80 lines) before the
	// process exits: the hint must be found through that much noise.
	script := `case "$*" in *--max-model-len*) echo "$*" > ` + marker + `; sleep 2; exit 0;; esac
echo "Based on the available memory, the estimated maximum model length is 66352. Try increasing"
i=0; while [ $i -lt 120 ]; do echo "  File \"/usr/local/lib/python3.12/dist-packages/vllm/v1/engine/core.py\", line $i, in run_engine_core"; i=$((i+1)); done; exit 1`
	sup := NewSupervisor(slog.New(slog.NewTextHandler(io.Discard, nil)))
	defer sup.StopAll()
	if _, err := sup.Start(context.Background(), ProcessSpec{
		ID: "vllm-fit", Command: "/bin/sh", Args: []string{"-c", script + "\n", "sh"},
		Restart: true, MaxRestarts: 3, RestartBackoff: 10 * time.Millisecond,
		Adapt: func(tail []string) ([]string, bool) {
			if n, ok := vllmFittedLen(tail); ok {
				return []string{"-c", script + "\n", "sh", "--max-model-len", strconv.Itoa(n)}, true
			}
			return nil, false
		},
	}); err != nil {
		t.Fatalf("Start: %v", err)
	}
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		if b, err := os.ReadFile(marker); err == nil {
			if !strings.Contains(string(b), "--max-model-len 59648") {
				t.Fatalf("relaunched with %q", string(b))
			}
			return
		}
		time.Sleep(50 * time.Millisecond)
	}
	t.Fatal("the process was never relaunched with the fitted length")
}
