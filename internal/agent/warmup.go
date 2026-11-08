package agent

// A worker reports its engine's models as servable only after one generation
// has returned since the engine came up (PLAN T32.60).
//
// The model list (GET /v1/models) answers as soon as an engine's server is up,
// and some engines are not ready to generate then. Measured on the
// design-partner cell (2026-11-07): TensorRT-LLM logged "Application startup
// complete" and listed its model, the leader routed the next request to it,
// and that request answered after 71.9 s — the first generation after start;
// every request after it answered at once. Until then the worker's report, and
// with it the leader's placement row and the pod's readiness, said the worker
// served. The rule is the one gang heads follow (leader/gangready.go): ready
// means a request is answered.
//
// One tiny generation (max_tokens 1) runs in the background the first time
// the engine lists a model; until it returns the heartbeat says nothing is
// loaded and /readyz says why. Any answer from the engine counts, a 4xx too —
// an embeddings-only or rerank-only model refuses a chat, and that refusal
// already went through the engine's request path. Only no answer at all (the
// engine unreachable, the deadline) is tried again. A sleeping engine is not
// warmed: the request would wake it.

import (
	"context"
	"errors"
	"log/slog"
	"sync"
	"time"

	"github.com/opod-io/opod-core/internal/engines"
)

// warmupBudget bounds one warm-up generation. A first generation that loads
// or compiles can take minutes on a big model; past this the next heartbeat
// tries again.
const warmupBudget = 15 * time.Minute

// Warmup is the worker's record of whether its engine has answered a
// generation since it came up. The zero value is not usable; use NewWarmup.
type Warmup struct {
	engine engines.Engine
	log    *slog.Logger

	mu       sync.Mutex
	warm     bool
	inflight bool
	lastErr  string
	done     chan struct{} // receives once each time a warm-up succeeds
}

// NewWarmup returns the tracker for eng. A nil engine is always warm.
func NewWarmup(eng engines.Engine, log *slog.Logger) *Warmup {
	if log == nil {
		log = slog.Default()
	}
	return &Warmup{engine: eng, log: log, done: make(chan struct{}, 1)}
}

// Observe is called with the models the engine listed. It reports whether the
// worker may report them, and starts the warm-up the first time it may not.
func (w *Warmup) Observe(models []string) bool {
	if w == nil || w.engine == nil || len(models) == 0 {
		return true
	}
	w.mu.Lock()
	defer w.mu.Unlock()
	if w.warm {
		return true
	}
	if !w.inflight {
		w.inflight = true
		go w.run(models[0])
	}
	return false
}

// Reset forgets the warm-up: the engine stopped answering, so the next one to
// come up is a new process that has generated nothing yet.
func (w *Warmup) Reset() {
	if w == nil {
		return
	}
	w.mu.Lock()
	w.warm = false
	w.mu.Unlock()
}

// Ready reports whether the engine has answered a generation, and if not why,
// for /readyz.
func (w *Warmup) Ready() (bool, string) {
	if w == nil || w.engine == nil {
		return true, ""
	}
	w.mu.Lock()
	defer w.mu.Unlock()
	switch {
	case w.warm:
		return true, ""
	case w.lastErr != "":
		return false, "engine warming up (last try: " + w.lastErr + ")"
	default:
		return false, "engine warming up: its first generation has not returned"
	}
}

// Done fires each time a warm-up succeeds, so the loop can heartbeat at once
// instead of waiting out its interval.
func (w *Warmup) Done() <-chan struct{} {
	if w == nil {
		return nil
	}
	return w.done
}

func (w *Warmup) run(model string) {
	ctx, cancel := context.WithTimeout(context.Background(), warmupBudget)
	defer cancel()
	start := time.Now()
	err := generateOnce(ctx, w.engine, model)
	var up *engines.UpstreamError
	answered := err == nil || errors.As(err, &up)

	w.mu.Lock()
	w.inflight = false
	if answered {
		w.warm, w.lastErr = true, ""
	} else {
		w.lastErr = err.Error()
	}
	w.mu.Unlock()

	if !answered {
		w.log.Warn("engine warm-up got no answer; the next heartbeat tries again", "model", model, "after", time.Since(start).Round(time.Second), "err", err)
		return
	}
	w.log.Info("engine answered its warm-up generation; reporting its models", "model", model, "took", time.Since(start).Round(time.Millisecond))
	select {
	case w.done <- struct{}{}:
	default:
	}
}

// generateOnce asks the engine for one token and waits for the stream to end.
func generateOnce(ctx context.Context, eng engines.Engine, model string) error {
	one := 1
	ch, err := eng.Chat(ctx, engines.ChatRequest{
		Model:     model,
		Messages:  []engines.Message{{Role: "user", Content: "Hi"}},
		MaxTokens: &one,
	})
	if err != nil {
		return err
	}
	for ev := range ch {
		if ev.Err != nil {
			return ev.Err
		}
		if ev.Done {
			return nil
		}
	}
	return ctx.Err()
}
