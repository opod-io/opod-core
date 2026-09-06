package scheduler

import (
	"context"
	"fmt"
	"strconv"
	"time"

	"github.com/opod-io/opod/internal/agent"
	"github.com/opod-io/opod/internal/models"
	"github.com/opod-io/opod/internal/store"
)

func (o *Orchestrator) createShardedVLLMRay(ctx context.Context, entry models.Entry, shardCount int, nodeIDs []string, par Parallelism) error {
	var workers []store.Node
	var err error
	if len(nodeIDs) > 0 {
		workers, err = o.pickWorkersByID(ctx, nodeIDs)
	} else {
		workers, err = o.pickWorkers(ctx, shardCount)
	}
	if err != nil {
		return fmt.Errorf("pick workers: %w", err)
	}
	if len(workers) < 1 {
		return fmt.Errorf("vllm-ray sharding needs at least one worker")
	}

	const gcsPort = 6379
	head := workers[0]
	headHost := hostOf(head.Address)
	if headHost == "" {
		return fmt.Errorf("ray head node %s has no advertised address", head.ID)
	}

	// 1. Ray HEAD on workers[0]. `--block` keeps the launching process alive and
	//    tied to the cluster so the supervisor can monitor/restart it (a bare
	//    `ray start` daemonizes and exits, which the supervisor would read as a
	//    crash). HealthPort lets the worker confirm the GCS port is listening.
	headSpec := agent.ProcessSpec{
		ID:      "ray-head-" + safeID(entry.ID),
		Command: "ray",
		Args: []string{
			"start", "--head",
			// Declare the GPU explicitly. Ray's autodetect registered ZERO GPUs on a
			// joining worker (observed on one worker: the node joined, `ray status`
			// showed it Active, and the cluster still reported 1 GPU total), so vLLM
			// waited forever for a placement group of [{GPU:1},{GPU:1}] that could
			// never be satisfied. Every node on the Ray path in this fleet is
			// single-GPU; stating it beats discovering it.
			"--num-gpus", "1",
			"--node-ip-address", headHost,
			"--port", strconv.Itoa(gcsPort),
			"--ray-client-server-port", "10001",
			"--dashboard-host", "0.0.0.0",
			"--min-worker-port", "10002", "--max-worker-port", "10102",
			"--block",
		},
		Env:            distEnv(headHost),
		HealthPort:     gcsPort,
		HealthHost:     headHost,
		Restart:        true,
		MaxRestarts:    3,
		RestartBackoff: 2 * time.Second,
	}
	if _, err := o.callWorkerStart(ctx, head, headSpec); err != nil {
		return fmt.Errorf("start ray head on %s: %w", head.ID, err)
	}
	o.Log.Info("ray head started", "model", entry.ID, "node", head.ID, "gcs", headHost+":"+strconv.Itoa(gcsPort))

	// 2. Ray WORKERS join the head's GCS.
	for _, w := range workers[1:] {
		wHost := hostOf(w.Address)
		if wHost == "" {
			o.Log.Warn("ray worker has no advertised address — skipping", "node", w.ID)
			continue
		}
		wSpec := agent.ProcessSpec{
			ID:      "ray-worker-" + safeID(entry.ID) + "-" + safeID(w.ID),
			Command: "ray",
			Args: []string{
				"start",
				"--num-gpus", "1", // see the head spec — autodetect cannot be trusted here
				"--address", headHost + ":" + strconv.Itoa(gcsPort),
				"--node-ip-address", wHost,
				"--min-worker-port", "10002", "--max-worker-port", "10102",
				"--block",
			},
			// The Ray daemon's env is inherited by the RayWorkerProc it spawns —
			// that child is the one that opens the Gloo mesh socket, so the
			// interface pin has to be here, not just on the head's vLLM process.
			Env:            distEnv(wHost),
			Restart:        true,
			MaxRestarts:    3,
			RestartBackoff: 2 * time.Second,
		}
		if _, err := o.callWorkerStart(ctx, w, wSpec); err != nil {
			return fmt.Errorf("join ray worker %s to head: %w", w.ID, err)
		}
		o.Log.Info("ray worker joined", "model", entry.ID, "node", w.ID, "head", headHost)
	}

	o.Log.Info("ray cluster up", "model", entry.ID, "nodes", len(workers), "head", head.ID)

	// PHASE 2: launch `vllm serve` on the HEAD, bound to the Ray cluster. vLLM
	// auto-detects Ray and distributes the model — tensor-parallel within each node
	// (× --tensor-parallel-size) and pipeline-parallel across nodes (× the number of
	// Ray nodes). Serve under the CATALOG id (--served-model-name) so the router
	// needs no name translation, and bind the head's advertised address so the
	// leader can dial it. No HealthPort: vLLM's cross-node load is slow (minutes),
	// so we register optimistically and let the first requests warm it up rather
	// than blocking the launch on readiness.
	const vllmPort = 9100
	// One GPU per node in this fleet, so ranks == nodes. TP×PP must cover them all.
	split, err := par.resolve(len(workers))
	if err != nil {
		return fmt.Errorf("parallelism: %w", err)
	}
	if split.TP > 1 {
		o.Log.Warn("tensor-parallel ACROSS MACHINES — all-reduce twice per layer over the network; "+
			"expect it to be latency-bound",
			"model", entry.ID, "tp", split.TP, "pp", split.PP, "nodes", len(workers))
	}
	gpusPerNode := split.TP
	pp := split.PP
	model := entry.Source.Repo
	if model == "" {
		model = entry.Source.Path
	}
	if model == "" {
		model = entry.ID
	}
	// Cross-machine TP cannot afford CUDA-graph capture: every one of the ~51
	// captures replays the full forward pass, and with TP the all-reduces inside it
	// traverse the overlay — measured at 132 SECONDS per capture (≈1h46m before the
	// first token). Eager mode skips capture entirely: slower per token, but the
	// server actually comes up. PP is unaffected (its captures are node-local).
	eager := ""
	if split.TP > 1 && len(workers) > 1 {
		eager = " --enforce-eager"
		o.Log.Info("cross-machine TP: skipping CUDA-graph capture (--enforce-eager)",
			"model", entry.ID, "tp", split.TP)
	}
	vllmCmd := fmt.Sprintf(
		"exec vllm serve '%s' --served-model-name '%s' --distributed-executor-backend ray "+
			"--tensor-parallel-size %d --pipeline-parallel-size %d --host %s --port %d "+
			"--trust-remote-code --gpu-memory-utilization 0.85"+eager,
		model, entry.ID, gpusPerNode, pp, headHost, vllmPort)
	vllmSpec := agent.ProcessSpec{
		ID:      "vllm-ray-" + safeID(entry.ID),
		Command: "/bin/sh",
		Args:    []string{"-lc", vllmCmd},
		Env: mergeEnv(distEnv(headHost), map[string]string{
			"VLLM_WORKER_MULTIPROC_METHOD": "spawn",
			// Loading + distributing the model across nodes over the overlay is slow;
			// give the engine much longer than vLLM's 600s default before it aborts.
			"VLLM_ENGINE_READY_TIMEOUT_S": "2400",
		}),
		// No auto-restart: a crashed vLLM leaves stale Ray actors that poison the
		// next attempt (ActorHandleNotFound across sessions). One clean try; the
		// operator recreates the Ray cluster to retry.
		Restart: false,
	}
	if _, err := o.callWorkerStart(ctx, head, vllmSpec); err != nil {
		return fmt.Errorf("launch vllm serve on ray head %s: %w", head.ID, err)
	}

	// Register the head's vLLM endpoint as the model's COORDINATOR so the router's
	// shardCoordinator() dials it (mirrors the llama.cpp coordinator; the endpoint
	// is OpenAI-compatible either way).
	coordRec := store.Shard{
		ID: "s-" + safeID(entry.ID) + "-vllm-ray-coord", ModelID: entry.ID, Role: "coordinator",
		NodeID: head.ID, Address: fmt.Sprintf("%s:%d", headHost, vllmPort),
		ProcessID: vllmSpec.ID, Status: "ready",
		CreatedAt: time.Now(), LastSeen: time.Now(),
	}
	if err := o.Store.Shards().Create(ctx, coordRec); err != nil {
		_ = o.callWorkerStop(ctx, head, vllmSpec.ID)
		return fmt.Errorf("persist vllm-ray coordinator: %w", err)
	}
	o.Log.Info("vllm-ray coordinator up (PHASE 2)", "model", entry.ID,
		"endpoint", coordRec.Address, "tp", gpusPerNode, "pp", pp,
		"note", "vLLM is loading across the Ray cluster — first requests warm it up")
	return nil
}

// pickWorkers selects N nodes ordered by descending RAM. Future revisions
// will incorporate GPU memory, current load, and same-site preference.
// pickWorkersByID selects the named ready workers, preserving the caller's order.
// Errors if any named node is unknown or not ready, so a typo fails fast here
// rather than silently sharding across the wrong machines.
