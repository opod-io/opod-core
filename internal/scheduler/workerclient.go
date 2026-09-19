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
	body, err := json.Marshal(spec)
	if err != nil {
		// A spec that cannot be encoded must fail here, named — not reach the
		// worker as an empty body (2026-09-14: a func-typed hook on ProcessSpec
		// did exactly that, and every gang part failed with "invalid body: EOF").
		return nil, fmt.Errorf("encode process spec %s: %w", spec.ID, err)
	}
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
	prefix := agent.ShardProcessPrefix(entry.ID)
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

// loadModelRequest is the body of the worker's /v1/model/load, field for
// field what agent.modelLoad reads. File names one GGUF inside Repo: without
// it a worker given a repository that holds several files has to choose by
// itself, and may not choose the one the catalog entry means. Key names and
// the omitted-when-empty File are the wire; the type becomes the SDK's
// nodeapi.LoadModelRequest when core adopts that package.
type loadModelRequest struct {
	ID         string `json:"id"`
	OllamaName string `json:"ollama_name"`
	Repo       string `json:"repo"`
	File       string `json:"file,omitempty"`
	Path       string `json:"path"`
	Pin        bool   `json:"pin"`
}

func (o *Orchestrator) callWorkerLoad(ctx context.Context, node store.Node, entry models.Entry, pin bool) error {
	// Send source fields, not the whole Entry — the worker resolves the
	// engine-native name against its own engine (see agent.modelLoad).
	body, _ := json.Marshal(loadModelRequest{
		ID:         entry.ID,
		OllamaName: entry.Source.OllamaName,
		Repo:       entry.Source.Repo,
		File:       entry.Source.File,
		Path:       entry.Source.Path,
		Pin:        pin,
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

// unloadModelRequest / unloadModelResponse are the worker's /v1/model/unload
// wire (agent.modelUnload): the load body's source fields without `file` and
// `pin`. They become the SDK's nodeapi.UnloadModelRequest / Response when
// core adopts that package.
type unloadModelRequest struct {
	ID         string `json:"id"`
	OllamaName string `json:"ollama_name"`
	Repo       string `json:"repo"`
	Path       string `json:"path"`
}

type unloadModelResponse struct {
	Status string `json:"status"`
	Model  string `json:"model,omitempty"`
	Engine string `json:"engine,omitempty"`
	Reason string `json:"reason,omitempty"`
}

// UnloadResult is a worker's answer to an unload it carried out or had no
// need to: Unloaded is false when the model was not resident there (the call
// is idempotent) and Reason says so.
type UnloadResult struct {
	Unloaded bool
	Model    string // the engine-native name
	Reason   string
}

// WorkerRefusal is a worker's non-2xx answer, kept typed so a caller can tell
// "cannot, by design" from "failed": 409 = a shard part or an adapter of the
// model is held there, 501 = the engine cannot unload and the worker did not
// start it, 502 = the engine failed.
type WorkerRefusal struct {
	Code    int
	Message string
}

func (e *WorkerRefusal) Error() string {
	return fmt.Sprintf("%d %s: %s", e.Code, http.StatusText(e.Code), e.Message)
}

// UnloadFromNode asks one worker (id or hostname) to stop holding a
// non-sharded model: POST /v1/model/unload, the counterpart of PlaceOnNodes.
// The worker need not take new work — unloading from a drained node is the
// point — but it must be a registered worker with an address. Like a load,
// this writes no placement row: the model leaves the worker's next heartbeat
// and the row, with any draining mark on it, goes then.
func (o *Orchestrator) UnloadFromNode(ctx context.Context, entry models.Entry, nodeID string) (UnloadResult, error) {
	all, err := o.Store.Nodes().List(ctx)
	if err != nil {
		return UnloadResult{}, err
	}
	for _, nd := range all {
		if nd.ID != nodeID && (nd.Hostname == "" || nd.Hostname != nodeID) {
			continue
		}
		if nd.ID == "local" || nd.Address == "" {
			return UnloadResult{}, fmt.Errorf("node %q is not a worker with an address", nodeID)
		}
		res, err := o.callWorkerUnload(ctx, nd, entry)
		if err != nil {
			return UnloadResult{}, fmt.Errorf("node %s: %w", nd.ID, err)
		}
		o.Log.Info("model unloaded from worker", "model", entry.ID, "node", nd.ID, "unloaded", res.Unloaded, "reason", res.Reason)
		return res, nil
	}
	return UnloadResult{}, fmt.Errorf("node %q not found", nodeID)
}

func (o *Orchestrator) callWorkerUnload(ctx context.Context, node store.Node, entry models.Entry) (UnloadResult, error) {
	body, _ := json.Marshal(unloadModelRequest{
		ID:         entry.ID,
		OllamaName: entry.Source.OllamaName,
		Repo:       entry.Source.Repo,
		Path:       entry.Source.Path,
	})
	url := workerURL(node.Address) + "/v1/model/unload"
	req, _ := http.NewRequestWithContext(ctx, http.MethodPost, url, bytes.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Authorization", "Bearer "+node.WorkerToken) // transition; HMAC below is the real auth
	auth.SignRequest(req, node.ID, node.WorkerToken)
	resp, err := o.HTTP.Do(req)
	if err != nil {
		return UnloadResult{}, err
	}
	defer resp.Body.Close()
	raw, _ := io.ReadAll(io.LimitReader(resp.Body, 64<<10))
	var ans unloadModelResponse
	decodeErr := json.Unmarshal(raw, &ans)
	if resp.StatusCode >= 400 {
		msg := strings.TrimSpace(string(raw))
		if decodeErr == nil && ans.Reason != "" { // the typed 501
			msg = fmt.Sprintf("engine %s: %s", ans.Engine, ans.Reason)
		}
		if resp.StatusCode == http.StatusNotFound {
			msg = "this worker has no /v1/model/unload — it runs a version that predates it"
		}
		return UnloadResult{}, &WorkerRefusal{Code: resp.StatusCode, Message: msg}
	}
	if decodeErr != nil {
		return UnloadResult{}, fmt.Errorf("decode unload answer: %w", decodeErr)
	}
	return UnloadResult{Unloaded: ans.Status == "unloaded", Model: ans.Model, Reason: ans.Reason}, nil
}

func workerURL(address string) string {
	if strings.HasPrefix(address, "http://") || strings.HasPrefix(address, "https://") {
		return strings.TrimRight(address, "/")
	}
	return "http://" + address
}

// safeID folds a model id into a process id; the rule is the worker's
// (agent.SafeProcessID), which recognises the ids the leader builds with it.
func safeID(s string) string { return agent.SafeProcessID(s) }
