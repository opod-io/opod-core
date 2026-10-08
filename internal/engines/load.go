package engines

// Engine load signals (core build item 14): what an engine knows about its
// own pressure that a request counter cannot see. A worker that implements
// LoadReporter has its sample ride the heartbeat; the leader aggregates the
// live workers of a model on /loadz, and an external autoscaler (the control
// plane, or KEDA reading the same JSON) scales on KV pressure and queue
// depth, not on in-flight requests alone.

import (
	"bufio"
	"context"
	"io"
	"strconv"
	"strings"
	"sync"
	"time"
)

// EngineLoad is one sample of an engine's pressure.
type EngineLoad struct {
	KVUsedPct    float64 `json:"kv_used_pct"`    // KV cache in use, 0–100 (the pressure signal)
	QueueDepth   int64   `json:"queue_depth"`    // requests waiting for a slot (every slot busy)
	TokensPerSec float64 `json:"tokens_per_s"`   // generated tokens per second since the previous sample
	PrefixHitPct float64 `json:"prefix_hit_pct"` // prefix-cache hit rate, 0–100 (0 when the engine does not report it)
	SampledAt    int64   `json:"sampled_at"`     // unix seconds
}

// LoadReporter is implemented by engines that can report EngineLoad.
type LoadReporter interface {
	Load(ctx context.Context) (EngineLoad, error)
}

// SlotReporter is implemented by engines that can say how many requests they
// serve at once (feature "slot_admission", ADR-091): the number the leader
// governs dispatch by. It is a property of the running process, not a sample
// — an engine fixes it at start — so a driver may read it once and keep it.
// ok = false when the engine does not say; the worker then falls back to the
// number it launched the engine with, or reports none.
type SlotReporter interface {
	Slots(ctx context.Context) (n int, ok bool)
}

// ParsePromText reads Prometheus text exposition into name → value. Labels
// are stripped; several series of one name (one per model label) are
// summed, which is right for a worker that serves one model. Histograms and
// summaries contribute their _sum/_count/_bucket series under those names.
func ParsePromText(r io.Reader) map[string]float64 {
	out := map[string]float64{}
	sc := bufio.NewScanner(r)
	sc.Buffer(make([]byte, 64*1024), 1024*1024)
	for sc.Scan() {
		line := strings.TrimSpace(sc.Text())
		if line == "" || strings.HasPrefix(line, "#") {
			continue
		}
		name := line
		rest := ""
		if i := strings.IndexAny(line, "{ "); i > 0 {
			name = line[:i]
			rest = line[i:]
			if strings.HasPrefix(rest, "{") {
				if j := strings.Index(rest, "}"); j >= 0 {
					rest = rest[j+1:]
				}
			}
		}
		f := strings.Fields(rest)
		if len(f) == 0 {
			continue
		}
		v, err := strconv.ParseFloat(f[0], 64)
		if err != nil {
			continue
		}
		out[name] += v
	}
	return out
}

// RateTracker turns a monotonic counter into a per-second rate between two
// samples (the first sample yields 0).
type RateTracker struct {
	mu    sync.Mutex
	last  float64
	at    time.Time
	valid bool
}

// Rate records value at now and returns the rate since the previous sample.
func (t *RateTracker) Rate(value float64, now time.Time) float64 {
	t.mu.Lock()
	defer t.mu.Unlock()
	defer func() { t.last, t.at, t.valid = value, now, true }()
	if !t.valid || !now.After(t.at) || value < t.last {
		return 0 // first sample, same instant, or a counter reset
	}
	return (value - t.last) / now.Sub(t.at).Seconds()
}

// First returns the first metric present among names (aliases across engine versions).
func First(m map[string]float64, names ...string) (float64, bool) {
	for _, n := range names {
		if v, ok := m[n]; ok {
			return v, true
		}
	}
	return 0, false
}

// Tokenizer is implemented by engines that tokenize a prompt for a caller,
// chat template applied — what the engine itself would feed the model. The
// leader has no tokenizer and asks for one through the worker to score a
// request against the prefix-cache blocks a worker holds (feature
// "kv_block_events"). limit bounds the answer; ≤ 0 = everything.
type Tokenizer interface {
	Tokenize(ctx context.Context, model string, messages []Message, prompt string, limit int) ([]int, error)
}

// Sleeper is implemented by engines that can drop their GPU working set
// while keeping the process (core build item 13, the §6 "sleep" tier):
// vLLM's sleep mode (weights offloaded / freed, wake in under a second).
// llama.cpp has no such mode — its weights stay mmapped; the worker answers
// "unsupported" and the control plane parks the pod instead.
type Sleeper interface {
	Sleep(ctx context.Context) error
	Resume(ctx context.Context) error
	// Sleeping reports the engine's own state (what the heartbeat carries).
	Sleeping(ctx context.Context) (bool, error)
}
