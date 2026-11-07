package leader

// A gang's coordinator row reads "ready" only once its API answers.
//
// SGLang, vLLM over Ray and vLLM expert gangs launch their head with no health
// gate on purpose (scheduler/sglang.go, vllmray.go, vllmexpert.go): the head
// binds its API only after every rank has joined and the weights are loaded,
// which is minutes on a model worth splitting, and rank 0 waits for ranks that
// are launched after it — so a create that waited would time out or deadlock.
// Those creates store the coordinator row "starting" instead, and this loop
// moves it to "ready" when the head answers the same request the router's
// driver sends (GET /v1/models).
//
// Before, the row was written "ready" at launch. Every reader that trusts the
// rows — the leader's /readyz, /admin/v1/shards and through it the control
// plane's serving state — then said a gang served while every request got 503:
// measured on the design-partner cell (2026-11-07), 45 s for a vLLM pipeline-2
// gang and about 2 min 20 s for an SGLang tensor-2 gang.
//
// The loop holds the invariant for every reader, rather than each reader
// probing the head itself. It is the store row that changes, so a leader that
// restarts while a head loads picks the probe up on its next tick.

import (
	"context"
	"time"

	"github.com/opod-io/opod-core/internal/store"
)

// gangReadyEvery is how often a starting coordinator is asked whether it
// answers: the worker heartbeat cadence, so the row turns ready within a few
// seconds of the head and the leader dials a loading head no more than that.
const gangReadyEvery = 5 * time.Second

// gangReadyProbe bounds one ask. A head that is loading refuses the connection
// at once; this only stops a wedged one holding the loop.
const gangReadyProbe = 2 * time.Second

// StartGangReadiness runs the loop for the leader's lifetime.
func (s *Server) StartGangReadiness(ctx context.Context) {
	go func() {
		t := time.NewTicker(gangReadyEvery)
		defer t.Stop()
		for {
			select {
			case <-ctx.Done():
				return
			case <-t.C:
				s.promoteAnsweringCoordinators(ctx)
			}
		}
	}()
}

// promoteAnsweringCoordinators asks every "starting" coordinator whether its
// API answers and stores "ready" for each one that does.
func (s *Server) promoteAnsweringCoordinators(ctx context.Context) {
	shards, err := s.store.Shards().List(ctx)
	if err != nil {
		return // the next tick reads again; a failed read proves nothing
	}
	for _, sh := range shards {
		if sh.Role != "coordinator" || sh.Status != "starting" || sh.Address == "" {
			continue
		}
		if !s.coordinatorAnswers(ctx, sh) {
			continue
		}
		moved, err := s.store.Shards().UpdateStatusIf(ctx, sh, "starting", "ready")
		if err != nil {
			s.log.Warn("a gang coordinator answers but its row could not be marked ready",
				"model", sh.ModelID, "gang", sh.Gang(), "coordinator", sh.Address, "err", err)
			continue
		}
		if moved {
			s.log.Info("gang coordinator answers: ready",
				"model", sh.ModelID, "gang", sh.Gang(), "coordinator", sh.Address,
				"loading_for", time.Since(sh.CreatedAt).Round(time.Second))
		}
	}
}

// coordinatorAnswers dials the coordinator through the driver the router and
// the load sampler use for it (gangEngine), so "answers" means what routing
// will meet.
func (s *Server) coordinatorAnswers(ctx context.Context, coord store.Shard) bool {
	eng, ok := s.gangEngine(coord)
	if !ok {
		return false
	}
	pctx, cancel := context.WithTimeout(ctx, gangReadyProbe)
	defer cancel()
	return eng.Health(pctx) == nil
}
