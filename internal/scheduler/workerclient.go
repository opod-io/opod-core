package scheduler

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strings"
	"time"

	"github.com/opod-io/opod/internal/agent"
	"github.com/opod-io/opod/internal/auth"
	"github.com/opod-io/opod/internal/models"
	"github.com/opod-io/opod/internal/store"
)

func (o *Orchestrator) callWorkerStart(ctx context.Context, node store.Node, spec agent.ProcessSpec) (*agent.ProcessInfo, error) {
	body, _ := json.Marshal(spec)
	url := workerURL(node.Address) + "/v1/process/start"
	req, _ := http.NewRequestWithContext(ctx, http.MethodPost, url, bytes.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Authorization", "Bearer "+node.WorkerToken) // transition; HMAC below is the real auth
	auth.SignRequest(req, node.ID, node.WorkerToken)
	resp, err := o.HTTP.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	if resp.StatusCode >= 400 {
		b, _ := io.ReadAll(resp.Body)
		return nil, fmt.Errorf("%s: %s", resp.Status, string(b))
	}
	var info agent.ProcessInfo
	if err := json.NewDecoder(resp.Body).Decode(&info); err != nil {
		return nil, fmt.Errorf("decode info: %w", err)
	}
	// The worker spawns and answers 202 immediately; readiness happens on ITS
	// clock, not this HTTP call's. Poll until the process leaves "starting".
	if info.Status == "starting" {
		return o.awaitWorkerReady(ctx, node, spec)
	}
	return &info, nil
}

// awaitWorkerReady polls a worker process until it is running, failed, or the
// spec's ReadyTimeout expires.
//
// This exists because readiness cannot ride on the start request. o.HTTP has a
// 60s timeout — right for a wedged worker — but a shard coordinator legitimately
// takes up to ReadyTimeout (15 min) to load a model across its rpc backends. The
// old synchronous start blew the client deadline every time and reported a failed
// create for a healthy, still-loading process.
//
// A "failed" process is returned as an error carrying ProcessInfo.ExitErr, which
// already embeds the engine's own output (supervisor logTail) — so llama.cpp's
// actual complaint reaches `opod shard create` instead of a bare 502.
func (o *Orchestrator) awaitWorkerReady(ctx context.Context, node store.Node, spec agent.ProcessSpec) (*agent.ProcessInfo, error) {
	timeout := spec.ReadyTimeout
	if timeout <= 0 {
		timeout = 30 * time.Second
	}
	deadline := time.Now().Add(timeout)
	for {
		select {
		case <-ctx.Done():
			return nil, ctx.Err()
		case <-time.After(2 * time.Second):
		}
		info, err := o.callWorkerGet(ctx, node, spec.ID)
		if err != nil {
			// Transient worker blip: keep waiting until the deadline rather than
			// tearing down a process that may be loading fine.
			if time.Now().After(deadline) {
				return nil, fmt.Errorf("poll %s on %s: %w", spec.ID, node.ID, err)
			}
			continue
		}
		switch info.Status {
		case "running":
			return info, nil
		case "failed", "crashloop":
			return nil, fmt.Errorf("process %s on %s %s: %s", spec.ID, node.ID, info.Status, info.ExitErr)
		}
		if time.Now().After(deadline) {
			return nil, fmt.Errorf("process %s on %s still %q after %s (never became ready)",
				spec.ID, node.ID, info.Status, timeout)
		}
	}
}

func (o *Orchestrator) callWorkerGet(ctx context.Context, node store.Node, processID string) (*agent.ProcessInfo, error) {
	url := workerURL(node.Address) + "/v1/process/get?id=" + processID
	req, _ := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	req.Header.Set("Authorization", "Bearer "+node.WorkerToken)
	auth.SignRequest(req, node.ID, node.WorkerToken)
	resp, err := o.HTTP.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	if resp.StatusCode >= 400 {
		b, _ := io.ReadAll(resp.Body)
		return nil, fmt.Errorf("%s: %s", resp.Status, string(b))
	}
	var info agent.ProcessInfo
	if err := json.NewDecoder(resp.Body).Decode(&info); err != nil {
		return nil, fmt.Errorf("decode info: %w", err)
	}
	return &info, nil
}

// stopOrphanShardProcs stops shard processes for this model that run on
// registered workers but that no shard row in the store accounts for — the
// leftovers of a previous leader life. Best-effort: an unreachable worker is
// skipped (its orphan will collide later and surface as a create error).
func (o *Orchestrator) stopOrphanShardProcs(ctx context.Context, entry models.Entry) {
	prefix := "s-" + safeID(entry.ID) + "-"
	nodes, err := o.Store.Nodes().List(ctx)
	if err != nil {
		return
	}
	for _, nd := range nodes {
		if nd.ID == "local" || nd.Address == "" {
			continue
		}
		procs, err := o.callWorkerList(ctx, nd)
		if err != nil {
			continue
		}
		for _, p := range procs {
			if !strings.HasPrefix(p.ID, prefix) {
				continue
			}
			o.Log.Info("stopping orphan shard process from a previous leader",
				"node", nd.ID, "process", p.ID, "status", p.Status)
			if err := o.callWorkerStop(ctx, nd, p.ID); err != nil {
				o.Log.Warn("orphan shard process stop failed", "node", nd.ID, "process", p.ID, "err", err)
			}
		}
	}
}

// callWorkerList returns the worker's supervised process table.
func (o *Orchestrator) callWorkerList(ctx context.Context, node store.Node) ([]agent.ProcessInfo, error) {
	url := workerURL(node.Address) + "/v1/process/list"
	req, _ := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	req.Header.Set("Authorization", "Bearer "+node.WorkerToken)
	auth.SignRequest(req, node.ID, node.WorkerToken)
	resp, err := o.HTTP.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	if resp.StatusCode >= 400 {
		b, _ := io.ReadAll(resp.Body)
		return nil, fmt.Errorf("%s: %s", resp.Status, string(b))
	}
	var procs []agent.ProcessInfo
	if err := json.NewDecoder(resp.Body).Decode(&procs); err != nil {
		return nil, fmt.Errorf("decode process list: %w", err)
	}
	return procs, nil
}

func (o *Orchestrator) callWorkerStop(ctx context.Context, node store.Node, processID string) error {
	body, _ := json.Marshal(map[string]string{"id": processID})
	url := workerURL(node.Address) + "/v1/process/stop"
	req, _ := http.NewRequestWithContext(ctx, http.MethodPost, url, bytes.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Authorization", "Bearer "+node.WorkerToken)
	auth.SignRequest(req, node.ID, node.WorkerToken)
	resp, err := o.HTTP.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode >= 400 {
		b, _ := io.ReadAll(resp.Body)
		return fmt.Errorf("%s: %s", resp.Status, string(b))
	}
	return nil
}

// PlaceOnNodes pins a non-sharded model to the given ready workers by telling
// each worker to pull + load it into its own engine (/v1/model/load). Node IDs
// are validated and resolved (ready + addressable) the same way shard pinning
// is, via pickWorkersByID. Each worker reports the model on its next heartbeat,
// at which point the leader reconciles the placement — so this only drives the
// load; it writes no placement rows itself.
func (o *Orchestrator) PlaceOnNodes(ctx context.Context, entry models.Entry, nodeIDs []string, pin bool) error {
	workers, err := o.pickWorkersByID(ctx, nodeIDs)
	if err != nil {
		return err
	}
	for _, nd := range workers {
		if err := o.callWorkerLoad(ctx, nd, entry, pin); err != nil {
			return fmt.Errorf("node %s: %w", nd.ID, err)
		}
		o.Log.Info("model placed on worker", "model", entry.ID, "node", nd.ID)
	}
	return nil
}

func (o *Orchestrator) callWorkerLoad(ctx context.Context, node store.Node, entry models.Entry, pin bool) error {
	// Send source fields, not the whole Entry — the worker resolves the
	// engine-native name against its own engine (see agent.modelLoad).
	body, _ := json.Marshal(map[string]any{
		"id":          entry.ID,
		"ollama_name": entry.Source.OllamaName,
		"repo":        entry.Source.Repo,
		"path":        entry.Source.Path,
		"pin":         pin,
	})
	url := workerURL(node.Address) + "/v1/model/load"
	req, _ := http.NewRequestWithContext(ctx, http.MethodPost, url, bytes.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Authorization", "Bearer "+node.WorkerToken) // transition; HMAC below is the real auth
	auth.SignRequest(req, node.ID, node.WorkerToken)
	resp, err := o.HTTP.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode >= 400 {
		b, _ := io.ReadAll(resp.Body)
		return fmt.Errorf("%s: %s", resp.Status, string(b))
	}
	return nil
}

func workerURL(address string) string {
	if strings.HasPrefix(address, "http://") || strings.HasPrefix(address, "https://") {
		return strings.TrimRight(address, "/")
	}
	return "http://" + address
}

func safeID(s string) string {
	out := make([]byte, 0, len(s))
	for i := 0; i < len(s); i++ {
		c := s[i]
		if (c >= 'a' && c <= 'z') || (c >= '0' && c <= '9') || c == '-' {
			out = append(out, c)
		} else if c >= 'A' && c <= 'Z' {
			out = append(out, c+32)
		} else {
			out = append(out, '-')
		}
	}
	return string(out)
}
