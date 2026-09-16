package scheduler

// LoRA adapters at runtime (ROADMAP R15.15, feature "adapters_runtime").
//
// An adapter lives in the engine's LoRA slots on the worker that already holds
// the base model, so adding one is not a placement decision and must not roll a
// pod: the whole point of ADR-044's hold mode is that a plan change which needs
// no new process does not restart the ones that are serving.
//
// The fan-out is here rather than in the control plane because a manager does
// not know a worker's address, its HMAC identity, or which workers currently
// hold the base — the leader does, and that boundary is what §11 is about.
//
// Partial success is reported, never hidden. If three of four workers take the
// adapter, the caller is told which one refused and why: the model id answers
// on three workers and 404s on the fourth, and an operator who is not told that
// spends the outage looking at the engine.

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"sort"

	"github.com/opod-io/opod/internal/auth"
	"github.com/opod-io/opod/internal/store"
)

// AdapterResult is one worker's answer.
type AdapterResult struct {
	Node  string `json:"node"`
	OK    bool   `json:"ok"`
	Error string `json:"error,omitempty"`
}

// LoadAdapter tells every ready worker holding base to load the adapter into
// its engine and serve it as "<base>:<name>". Workers that do not hold the base
// are skipped, not failed: they have nothing to attach it to.
func (o *Orchestrator) LoadAdapter(ctx context.Context, base, name, source string) ([]AdapterResult, error) {
	return o.adapterFanOut(ctx, base, "/v1/adapters/load",
		map[string]any{"base": base, "name": name, "source": source})
}

// UnloadAdapter is the reverse. A worker that does not hold the adapter is not
// an error — the desired state is "not loaded", and it already is.
func (o *Orchestrator) UnloadAdapter(ctx context.Context, base, name string) ([]AdapterResult, error) {
	return o.adapterFanOut(ctx, base, "/v1/adapters/unload",
		map[string]any{"base": base, "name": name})
}

func (o *Orchestrator) adapterFanOut(ctx context.Context, base, path string, body map[string]any) ([]AdapterResult, error) {
	holders, err := o.workersHolding(ctx, base)
	if err != nil {
		return nil, err
	}
	if len(holders) == 0 {
		return nil, fmt.Errorf("no ready worker holds %s: an adapter attaches to a resident base model, so there is nothing to load it into", base)
	}
	raw, _ := json.Marshal(body)
	out := make([]AdapterResult, 0, len(holders))
	for _, nd := range holders {
		res := AdapterResult{Node: nd.ID, OK: true}
		if err := o.postWorker(ctx, nd, path, raw); err != nil {
			res.OK, res.Error = false, err.Error()
		}
		out = append(out, res)
	}
	return out, nil
}

// workersHolding is every ready, addressable worker whose engine reports the
// base model resident.
//
// A node that holds the base but is no longer ready is SKIPPED, not an error:
// unlike a placement, this is a fan-out over whoever is serving right now, and
// refusing the whole call because one worker is draining would mean the adapter
// could never be added while any node is unhealthy. The worker picks it up from
// the plan when it comes back.
func (o *Orchestrator) workersHolding(ctx context.Context, base string) ([]store.Node, error) {
	places, err := o.Store.Placements().GetByModel(ctx, base)
	if err != nil {
		return nil, err
	}
	if len(places) == 0 {
		return nil, nil
	}
	holds := map[string]bool{}
	for _, p := range places {
		holds[p.NodeID] = true
	}
	all, err := o.Store.Nodes().List(ctx)
	if err != nil {
		return nil, err
	}
	out := []store.Node{}
	for _, nd := range all {
		if holds[nd.ID] && nd.State == "ready" && nd.Address != "" {
			out = append(out, nd)
		}
	}
	sort.Slice(out, func(i, j int) bool { return out[i].ID < out[j].ID })
	return out, nil
}

func (o *Orchestrator) postWorker(ctx context.Context, node store.Node, path string, raw []byte) error {
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, workerURL(node.Address)+path, bytes.NewReader(raw))
	if err != nil {
		return err
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Authorization", "Bearer "+node.WorkerToken)
	auth.SignRequest(req, node.ID, node.WorkerToken)
	resp, err := o.HTTP.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode >= 400 {
		b, _ := io.ReadAll(io.LimitReader(resp.Body, 4096))
		return fmt.Errorf("%s: %s", resp.Status, string(b))
	}
	return nil
}
