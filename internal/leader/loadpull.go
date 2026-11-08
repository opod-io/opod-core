package leader

// Every router process pulls its workers' engine load (feature "load_pull",
// ADR-109, PLAN T28.1).
//
// The picker ranks a worker on in-flight + queue depth + kvWeight × KV%, and
// only in-flight was live: the queue and KV came from the heartbeat, up to
// 5 s old and dropped only past 30 s, and a front door got no sample at all —
// it ranked on its own in-flight count. Now the leader and every door pull
// GET /v1/load from each worker they route to, every routing.loadRefreshMs
// (500 ms by default). A pulled sample older than three intervals leaves the
// rank: the worker is scored on what is in flight alone, never refused. A
// worker whose core has no such route (404) keeps the heartbeat's sample and
// its 30 s rule, so an older worker does not lose load-aware routing.

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strings"
	"sync"
	"time"

	"github.com/opod-io/opod-core/internal/auth"
	"github.com/opod-io/opod-core/internal/engines"
	"github.com/opod-io/opod-core/internal/store"
	"github.com/opod-io/opod-core/pkg/adminapi"
	"github.com/opod-io/opod-core/pkg/nodeapi"
)

const (
	loadRefreshDefault = 500 * time.Millisecond
	loadRefreshFloor   = 100 * time.Millisecond
	// loadPullAgeFactor: a pulled sample older than this many intervals is
	// not ranked on.
	loadPullAgeFactor = 3
	// loadPullRetryUnsupported: how long a worker that has no /v1/load (an
	// older core, or an engine that reports no load) is left on its heartbeat
	// sample before it is asked again — it may have been upgraded.
	loadPullRetryUnsupported = time.Minute
)

// loadPuller holds the pulled samples and what each worker can answer.
type loadPuller struct {
	mu          sync.Mutex
	samples     map[string]nodeLoadSample // node id → last pulled sample, stamped when the engine was scraped
	unsupported map[string]time.Time      // node id → when it answered 404/501
	inflight    map[string]bool           // node id → a pull is in flight (one per worker at a time)
}

func newLoadPuller() *loadPuller {
	return &loadPuller{samples: map[string]nodeLoadSample{}, unsupported: map[string]time.Time{}, inflight: map[string]bool{}}
}

// loadRefresh is the policy's pull interval, floored.
func (s *Server) loadRefresh() time.Duration {
	ms := s.loadRefreshMs.Load()
	if ms <= 0 {
		return loadRefreshDefault
	}
	if d := time.Duration(ms) * time.Millisecond; d > loadRefreshFloor {
		return d
	}
	return loadRefreshFloor
}

// StartLoadPuller runs the pull loop for the process's lifetime, in either
// role: a door routes too.
func (s *Server) StartLoadPuller(ctx context.Context) {
	go func() {
		for {
			d := s.loadRefresh()
			select {
			case <-ctx.Done():
				return
			case <-time.After(d):
			}
			nodes, err := s.store.Nodes().List(ctx)
			if err != nil {
				continue
			}
			now, maxAge := time.Now(), s.heartbeatMaxAge()
			for i := range nodes {
				n := nodes[i]
				if n.ID == "local" || n.Address == "" || !n.TakesNewWork(maxAge, now) {
					continue
				}
				if !s.pull.begin(n.ID, now) {
					continue
				}
				go s.pullLoad(ctx, &n, d)
			}
		}
	}()
}

// begin reserves a pull of node, and reports false when one is in flight or
// the node answered that it has no load route not long ago.
func (p *loadPuller) begin(node string, now time.Time) bool {
	p.mu.Lock()
	defer p.mu.Unlock()
	if p.inflight[node] {
		return false
	}
	if at, ok := p.unsupported[node]; ok && now.Sub(at) < loadPullRetryUnsupported {
		return false
	}
	p.inflight[node] = true
	return true
}

func (s *Server) pullLoad(ctx context.Context, n *store.Node, interval time.Duration) {
	ld, status, err := s.fetchLoad(ctx, n, interval)
	p := s.pull
	p.mu.Lock()
	defer p.mu.Unlock()
	delete(p.inflight, n.ID)
	switch {
	case err == nil:
		at := time.Now()
		if ld.SampledAtMs > 0 {
			at = time.UnixMilli(ld.SampledAtMs)
		}
		p.samples[n.ID] = nodeLoadSample{EngineLoad: ld, at: at}
		delete(p.unsupported, n.ID)
	case status == http.StatusNotFound || status == http.StatusNotImplemented:
		p.unsupported[n.ID] = time.Now()
		delete(p.samples, n.ID)
	}
	// Any other failure keeps the last sample, which ages out by the rule.
}

// fetchLoad is one GET /v1/load, signed like every leader→worker call.
func (s *Server) fetchLoad(ctx context.Context, n *store.Node, interval time.Duration) (engines.EngineLoad, int, error) {
	timeout := 2 * time.Second
	if interval > timeout {
		timeout = interval
	}
	ctx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()
	addr := n.Address
	if !strings.HasPrefix(addr, "http://") && !strings.HasPrefix(addr, "https://") {
		addr = "http://" + addr
	}
	url := fmt.Sprintf("%s%s?maxAgeMs=%d", strings.TrimRight(addr, "/"), nodeapi.PathLoad, interval.Milliseconds())
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return engines.EngineLoad{}, 0, err
	}
	req.Header.Set("Authorization", "Bearer "+n.WorkerToken)
	auth.SignRequest(req, n.ID, n.WorkerToken)
	resp, err := s.workerHTTP().Do(req)
	if err != nil {
		return engines.EngineLoad{}, 0, fmt.Errorf("pull load from %s: %w", n.ID, err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		_, _ = io.Copy(io.Discard, io.LimitReader(resp.Body, 4<<10))
		return engines.EngineLoad{}, resp.StatusCode, fmt.Errorf("pull load from %s: %s", n.ID, resp.Status)
	}
	var ld engines.EngineLoad
	if err := json.NewDecoder(io.LimitReader(resp.Body, 64<<10)).Decode(&ld); err != nil {
		return engines.EngineLoad{}, resp.StatusCode, fmt.Errorf("pull load from %s: decode: %w", n.ID, err)
	}
	return ld, resp.StatusCode, nil
}

// pulledLoad is the node's pulled sample for the picker. pulls=false means
// the node is not pulled (it has no route, or no pull has answered yet): the
// heartbeat's sample stands. pulls=true with ok=false means the pulled sample
// is too old to rank on.
func (s *Server) pulledLoad(node string, now time.Time) (ld nodeLoadSample, pulls, ok bool) {
	p := s.pull
	p.mu.Lock()
	defer p.mu.Unlock()
	smp, has := p.samples[node]
	if !has {
		return nodeLoadSample{}, false, false
	}
	return smp, true, now.Sub(smp.at) <= loadPullAgeFactor*s.loadRefresh()
}

// loadSampleAges is each worker's sample age for /gatewayz and /loadz: which
// sample the picker reads and how old it is.
func (s *Server) loadSampleAges(now time.Time) map[string]adminapi.LoadSample {
	out := map[string]adminapi.LoadSample{}
	p := s.pull
	p.mu.Lock()
	for node, smp := range p.samples {
		age := now.Sub(smp.at)
		out[node] = adminapi.LoadSample{Source: adminapi.LoadSourcePull, AgeMs: age.Milliseconds(),
			Ranked: age <= loadPullAgeFactor*s.loadRefresh()}
	}
	for node := range p.unsupported {
		if _, ok := out[node]; !ok {
			out[node] = adminapi.LoadSample{Source: adminapi.LoadSourceHeartbeat, Why: "the worker has no " + nodeapi.PathLoad}
		}
	}
	p.mu.Unlock()
	// A heartbeat source is ranked under that sample's own 30 s rule.
	for node, smp := range out {
		if smp.Source != adminapi.LoadSourceHeartbeat {
			continue
		}
		if v, ok := s.nodeLoad.Load(node); ok && now.Sub(v.(nodeLoadSample).at) <= loadSampleMaxAge {
			smp.Ranked = true
			out[node] = smp
		}
	}
	return out
}
