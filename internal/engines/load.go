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

	"github.com/opod-io/opod-sdk/nodeapi"
	"github.com/prometheus/common/expfmt"
	"github.com/prometheus/common/model"
)

// EngineLoad is one sample of an engine's pressure: the heartbeat's wire type
// itself (opod-sdk nodeapi.EngineLoad), so a field a driver fills is a field
// the leader reads, with no second copy to drift. KVUsedPct is the pressure
// signal; the pointer fields are the engine's own report of the KV pool it
// built and what it did with it (PLAN T23.2), nil when the engine does not say.
type EngineLoad = nodeapi.EngineLoad

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

// KVLayoutSetter is implemented by a driver whose engine's report of its KV
// pool depends on a launch choice the engine does not report back: llama.cpp's
// /props gives one slot's context, which is the whole pool when the slots
// share it (--kv-unified) and a slot's share of it when they do not. The worker
// that launched the engine says which; until it does the driver states no
// pool for a server with several slots.
type KVLayoutSetter interface {
	SetKVUnified(unified bool)
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

// PromLabels returns the labels of every series of one metric in Prometheus
// text exposition, one map per series. It is how an INFO metric is read — a
// gauge fixed at 1 whose facts are its labels, such as vLLM's
// cache_config_info — which ParsePromText cannot, since it keeps only values.
// Only that metric's lines go to the parser, so a malformed line elsewhere in
// an engine's exposition (ParsePromText skips those) costs nothing here.
func PromLabels(text, name string) []map[string]string {
	var b strings.Builder
	for _, line := range strings.Split(text, "\n") {
		if t := strings.TrimSpace(line); strings.HasPrefix(t, name+"{") || strings.HasPrefix(t, name+" ") {
			b.WriteString(t + "\n")
		}
	}
	if b.Len() == 0 {
		return nil
	}
	parser := expfmt.NewTextParser(model.LegacyValidation)
	fams, err := parser.TextToMetricFamilies(strings.NewReader(b.String()))
	if err != nil || fams[name] == nil {
		return nil
	}
	var out []map[string]string
	for _, m := range fams[name].GetMetric() {
		labels := make(map[string]string, len(m.GetLabel()))
		for _, l := range m.GetLabel() {
			labels[l.GetName()] = l.GetValue()
		}
		out = append(out, labels)
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
