package agent

// GET /v1/load (feature "load_pull", ADR-109): the engine's load, for the
// processes that rank this worker. The leader and every front door pull it a
// few times a second; the heartbeat's sample is up to 5 s old, and a door got
// none at all. The engine is scraped at most once per maxAgeMs whoever asks,
// so N routers cost the engine one scrape, not N.

import (
	"context"
	"encoding/json"
	"net/http"
	"strconv"
	"sync"
	"time"

	"github.com/opod-io/opod-core/internal/engines"
)

// loadFloor is the youngest sample a caller may ask for: the policy's floor.
const loadFloor = 100 * time.Millisecond

// loadCache is the worker's last engine sample and when it was taken.
type loadCache struct {
	mu     sync.Mutex
	sample engines.EngineLoad
	at     time.Time
}

func (s *Server) load(w http.ResponseWriter, r *http.Request) {
	lr, ok := s.Engine.(engines.LoadReporter)
	if !ok || s.Engine == nil {
		http.Error(w, "engine "+engineName(s.Engine)+" reports no load", http.StatusNotImplemented)
		return
	}
	maxAge := loadFloor
	if ms, err := strconv.Atoi(r.URL.Query().Get("maxAgeMs")); err == nil && time.Duration(ms)*time.Millisecond > maxAge {
		maxAge = time.Duration(ms) * time.Millisecond
	}
	c := &s.loadCache
	c.mu.Lock() // one scrape at a time: a second caller waits for it and reads its result
	defer c.mu.Unlock()
	if c.at.IsZero() || time.Since(c.at) >= maxAge {
		ctx, cancel := context.WithTimeout(r.Context(), 2*time.Second)
		ld, err := lr.Load(ctx)
		cancel()
		if err != nil {
			http.Error(w, "engine load: "+err.Error(), http.StatusBadGateway)
			return
		}
		now := time.Now()
		ld.SampledAt, ld.SampledAtMs = now.Unix(), now.UnixMilli()
		c.sample, c.at = ld, now
	}
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(c.sample)
}

func engineName(e engines.Engine) string {
	if e == nil {
		return "none"
	}
	return e.Name()
}
