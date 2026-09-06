// Package scheduler is the leader-side orchestration logic for sharded
// models. CreateSharded picks workers, asks each one to start an rpc-server
// for its piece, then launches the coordinator llama-server locally and
// stitches everything together via a placement row that the router resolves.
//
// Current scope:
//   - manual shard-count override (`--shards=N`) or catalog default
//   - simple bin-pack on free RAM (highest free first)
//   - coordinator always runs on the leader
//   - no automatic restart on shard crash (admin re-runs the create)
//   - no replacement-node logic if a worker disappears mid-stream
//
// All of those are clean follow-ups; the orchestrator + supervisor + store
// types are designed to support them without changing the wire shape.
package scheduler

import (
	"context"
	"fmt"
	"log/slog"
	"net"
	"net/http"
	"os"
	"strconv"
	"strings"
	"time"

	"github.com/opod-io/opod/internal/agent"
	"github.com/opod-io/opod/internal/models"
	"github.com/opod-io/opod/internal/store"
)

// Orchestrator owns the shard-lifecycle operations on the leader.
type Orchestrator struct {
	Store      store.Store
	Supervisor *agent.Supervisor // leader's own supervisor (coordinator runs here)
	Log        *slog.Logger
	HTTP       *http.Client
	// ModelsDir is the local destination for HuggingFace GGUF downloads
	// when a sharded catalog entry sets source.type=huggingface. Empty
	// means HF auto-download is disabled — operators have to pre-place
	// the file at source.path the old-fashioned way.
	ModelsDir string
}

// New returns a configured orchestrator.
func New(st store.Store, sup *agent.Supervisor, log *slog.Logger, modelsDir string) *Orchestrator {
	if log == nil {
		log = slog.Default()
	}
	return &Orchestrator{
		Store:      st,
		Supervisor: sup,
		Log:        log,
		HTTP:       &http.Client{Timeout: 60 * time.Second},
		ModelsDir:  modelsDir,
	}
}

// CreateSharded launches all the processes needed to serve one sharded model.
// Steps:
//  1. Validate the catalog entry's ShardingSpec.
//  2. Pick N worker nodes (descending free-RAM bin-pack).
//  3. POST /v1/process/start to each worker to launch `rpc-server -p <port>`.
//  4. Launch `llama-server -m <model> --rpc <list> --port <coord>` locally.
//  5. Persist N+1 Shard rows and a single Placement row pointing at "local".
//
// On any failure, all previously-launched processes are stopped via Rollback
// before returning.
// CreateSharded splits entry across workers. When nodeIDs is non-empty those
// exact ready workers are used (in order); otherwise the highest-RAM ready
// workers are auto-selected. shardCount is ignored when nodeIDs is given.
//
// A shardCount of 1 is allowed and means "no split": no rpc-servers are
// launched — a single whole llama-server runs on the one chosen host. This is
// the leader-driven way to place an entire GGUF model on a specific machine
// when it fits there, using the same command surface as real sharding.

// Parallelism is how a vLLM shard is split across the ranks it is given.
//
// TP (tensor-parallel) cuts every layer's matrices across ranks and all-reduces
// TWICE PER LAYER — the network sits in the inner loop, so it wants an intra-box
// link. PP (pipeline-parallel) cuts the layers into stages and crosses the network
// ONCE per token. On a fleet of single-GPU boxes the default is therefore TP=1 ×
// PP=<nodes>; TP>1 across machines is offered so its cost can be MEASURED rather
// than assumed, not because it is a good idea on a relayed overlay.
//
// TP × PP must equal the number of ranks (one GPU per node in this fleet).
// The zero value means "decide for me": TP=1, PP=<nodes>.
//
// Parallelism is the tensor-parallel × pipeline-parallel split for a sharded model.
type Parallelism struct {
	TP int // tensor-parallel size
	PP int // pipeline-parallel size
}

// resolve fills in the defaults and checks the product against the rank count.
func (p Parallelism) resolve(ranks int) (Parallelism, error) {
	tp, pp := p.TP, p.PP
	switch {
	case tp <= 0 && pp <= 0:
		tp, pp = 1, ranks // the safe default: cross the network once per token
	case tp > 0 && pp <= 0:
		if ranks%tp != 0 {
			return p, fmt.Errorf("tp=%d does not divide %d ranks", tp, ranks)
		}
		pp = ranks / tp
	case pp > 0 && tp <= 0:
		if ranks%pp != 0 {
			return p, fmt.Errorf("pp=%d does not divide %d ranks", pp, ranks)
		}
		tp = ranks / pp
	}
	if tp*pp != ranks {
		return p, fmt.Errorf("tp=%d × pp=%d = %d, but there are %d ranks (one GPU per node)",
			tp, pp, tp*pp, ranks)
	}
	return Parallelism{TP: tp, PP: pp}, nil
}

func (o *Orchestrator) CreateSharded(ctx context.Context, entry models.Entry, shardCount int, nodeIDs []string, par Parallelism) error {
	if !entry.Sharding.Required {
		return fmt.Errorf("model %s is not configured for sharding", entry.ID)
	}
	if len(nodeIDs) > 0 {
		shardCount = len(nodeIDs)
	}
	if shardCount <= 0 {
		shardCount = entry.Sharding.DefaultShards
	}
	if shardCount < 1 {
		return fmt.Errorf("shard count must be at least 1 (got %d)", shardCount)
	}
	// IDEMPOTENT REPLACE: tear down any prior shard for this model before
	// (re)creating. Without this, re-running `shard create` (new --nodes, or after
	// a worker was recreated with a fresh overlay IP) left the OLD coordinator /
	// rpc-server rows in place — pointing at dead/old addresses — and the new
	// create collided or silently no-op'd, so the model 502'd on a stale
	// coordinator. A clean slate beats a stale one; best-effort teardown.
	if existing, _ := o.Store.Shards().GetByModel(ctx, entry.ID); len(existing) > 0 {
		o.Log.Info("replacing existing shard for model", "model", entry.ID, "prior_rows", len(existing))
		if err := o.RemoveSharded(ctx, entry.ID); err != nil {
			o.Log.Warn("prior shard teardown had errors (continuing with create)", "model", entry.ID, "err", err)
		}
	}
	// Backend fork: vLLM multi-node uses a Ray cluster + pipeline/tensor
	// parallelism, NOT llama.cpp's rpc-server + coordinator. It skips all the
	// GGUF machinery below. Selected by the catalog's sharding.engine.
	if isVLLMRayBackend(entry.Sharding.Engine) {
		return o.createShardedVLLMRay(ctx, entry, shardCount, nodeIDs, par)
	}
	// llama.cpp's RPC backend has NO tensor split — it only cuts layers. Silently
	// ignoring --tp here would hand back a working-but-not-what-you-asked-for shard,
	// which is exactly the class of quiet lie we are trying to eliminate.
	if par.TP > 1 {
		return fmt.Errorf("tp=%d: llama.cpp RPC sharding is layer-wise only and has no tensor split "+
			"(tensor-parallel needs the vLLM backend — set sharding.engine=vllm in the catalog)", par.TP)
	}

	// source.type=huggingface entries resolve their path via auto-download
	// below (ensureLocalGGUF); everything else must point at a GGUF already
	// on disk.
	if entry.Source.Type != "huggingface" && entry.Source.Path == "" {
		return fmt.Errorf("sharded model %s requires source.path (local GGUF path)", entry.ID)
	}

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

	// The store-driven teardown above only sees shards THIS leader recorded.
	// After a leader restart (fresh state) the previous convergence's
	// rpc-servers / coordinator still run on the workers, and the create below
	// collides with `process "s-…" already exists`. Sweep every registered
	// worker for processes carrying this model's shard prefix and stop them.
	o.stopOrphanShardProcs(ctx, entry)

	rpcPortBase := entry.Sharding.RPCPortBase
	if rpcPortBase == 0 {
		rpcPortBase = 50052
	}
	coordPort := entry.Sharding.CoordinatorPort
	if coordPort == 0 {
		coordPort = 9001
	}

	created := make([]store.Shard, 0, shardCount+1)
	rpcEndpoints := make([]string, 0, shardCount)

	// Resolve where the GGUF actually lives on the leader. For source.type=file
	// this is just source.path; for source.type=huggingface we download it
	// from HF into storage.models_dir first (closes M5-T12 fully — no more
	// "wget the GGUF before shard create").
	//
	// workerModelPaths records where the GGUF landed on each worker (keyed
	// by node ID): if the coordinator ends up on a remote worker, its `-m`
	// must use that worker's path, not the leader-local one.
	var workerModelPaths map[string]string
	if entry.Source.Type == "file" || entry.Source.Type == "huggingface" {
		localPath, err := o.ensureLocalGGUF(ctx, entry)
		if err != nil {
			return fmt.Errorf("resolve GGUF on leader: %w", err)
		}
		// Then fan it out to every shard host (sha256-skip if already present).
		workerModelPaths, err = o.ensureGGUFOnAllWorkers(ctx, workers, localPath)
		if err != nil {
			return fmt.Errorf("distribute GGUF: %w", err)
		}
		// Patch the path the coordinator will use so it points at the local
		// resolved file rather than whatever source.path said.
		entry.Source.Path = localPath
	}

	// Launch one rpc-server per worker — only when there is an actual split.
	// With a single shard the coordinator (below) serves the whole model by
	// itself on the one chosen host, so no rpc-servers at all.
	if shardCount > 1 {
		for i, w := range workers {
			port := rpcPortBase + i
			shardID := fmt.Sprintf("s-%s-rpc-%d", safeID(entry.ID), i)
			wHost, _, sErr := net.SplitHostPort(w.Address)
			if sErr != nil {
				wHost = w.Address
			}
			// rpc-server speaks the unauthenticated llama.cpp RPC protocol, so
			// bind it to the worker's advertised (mesh) address only — not every
			// interface. Fall back to 0.0.0.0 only when the address is unknown.
			rpcBind := wHost
			if rpcBind == "" {
				rpcBind = "0.0.0.0"
				o.Log.Warn("worker address unknown — rpc-server binding 0.0.0.0 (unauthenticated RPC exposed on all interfaces)",
					"node", w.ID, "port", port)
			}
			// Worker probes the same address rpc-server binds (loopback when we
			// fell back to 0.0.0.0); the leader dials via the worker's address below.
			healthHost := rpcBind
			if rpcBind == "0.0.0.0" {
				healthHost = "127.0.0.1"
			}
			spec := agent.ProcessSpec{
				ID:         shardID,
				Command:    "rpc-server",
				Args:       []string{"-p", strconv.Itoa(port), "-H", rpcBind},
				HealthPort: port,
				HealthHost: healthHost,
				// If the rpc-server dies mid-stream the model goes unavailable —
				// auto-restart up to 5 times (1s, 2s, 4s, 8s, 16s backoffs) so
				// an admin doesn't have to re-run `opod shard create` for a
				// transient OOM or crash. After 5 the process enters "crashloop"
				// and the operator needs to intervene.
				Restart:        true,
				MaxRestarts:    5,
				RestartBackoff: time.Second,
			}
			o.Log.Info("starting rpc shard", "model", entry.ID, "node", w.ID, "port", port)
			if _, err := o.callWorkerStart(ctx, w, spec); err != nil {
				o.rollback(ctx, created)
				return fmt.Errorf("launch rpc on %s: %w", w.ID, err)
			}
			endpoint := fmt.Sprintf("%s:%d", wHost, port)
			rpcEndpoints = append(rpcEndpoints, endpoint)
			rec := store.Shard{
				ID: shardID, ModelID: entry.ID, Role: "rpc",
				NodeID: w.ID, Address: endpoint, ProcessID: shardID,
				Status:    "ready",
				CreatedAt: time.Now(), LastSeen: time.Now(),
			}
			if err := o.Store.Shards().Create(ctx, rec); err != nil {
				o.rollback(ctx, append(created, rec))
				return fmt.Errorf("persist shard %s: %w", shardID, err)
			}
			created = append(created, rec)
		}
	}

	// Pick the coordinator host. Default: whichever node (leader or worker)
	// has the most RAM, so the strongest box owns the cross-node aggregation
	// instead of always pinning to the leader. Override via env
	// OPOD_COORDINATOR_NODE=<node_id> (use "local" for the leader).
	//
	// Single shard: the coordinator IS the placement — it must run on the one
	// selected worker, so the OPOD_COORDINATOR_NODE override is ignored here
	// (honoring a leftover override would silently serve the model on a
	// different machine than the one the user pinned with --nodes).
	var coordHost coordinatorChoice
	if shardCount == 1 {
		w := workers[0]
		coordHost = coordinatorChoice{nodeID: w.ID, node: &w}
		if ov := os.Getenv("OPOD_COORDINATOR_NODE"); ov != "" && ov != w.ID {
			o.Log.Warn("OPOD_COORDINATOR_NODE ignored for single-shard placement — coordinator runs on the selected node",
				"override", ov, "node", w.ID)
		}
	} else {
		coordHost = o.pickCoordinatorHost(ctx, workers)
	}

	// Avoid port collisions with shards already running on the same host —
	// two whole-model placements on one worker must not fight over :9001.
	if free := o.pickCoordPort(ctx, coordHost.nodeID, coordPort); free != coordPort {
		o.Log.Info("coordinator port in use on host — bumped", "node", coordHost.nodeID, "want", coordPort, "using", free)
		coordPort = free
	}

	// Launch the coordinator. Two branches: on the leader we use the local
	// supervisor; on a worker we POST /v1/process/start exactly like rpc-server.
	coordID := fmt.Sprintf("s-%s-coord", safeID(entry.ID))
	// llama-server exposes an UNAUTHENTICATED OpenAI-compatible API, so a
	// worker-hosted coordinator binds the worker's advertised (mesh) address
	// only — never every interface — mirroring the rpc-server binding above.
	// Fall back to 0.0.0.0 only when the address is unknown. The worker's
	// health probe targets the same address it binds.
	coordHostBind := "127.0.0.1"
	coordHealthHost := "127.0.0.1"
	if !coordHost.local {
		host, _, sErr := net.SplitHostPort(coordHost.node.Address)
		if sErr != nil {
			host = coordHost.node.Address
		}
		coordHostBind = host
		coordHealthHost = host
		if coordHostBind == "" {
			coordHostBind = "0.0.0.0"
			coordHealthHost = "127.0.0.1"
			o.Log.Warn("coordinator node address unknown — llama-server binding 0.0.0.0 (unauthenticated API exposed on all interfaces)",
				"node", coordHost.nodeID, "port", coordPort)
		}
	}
	// The `-m` path must be valid on the machine that runs the coordinator:
	// leader-local path when it runs here, the worker's own path (reported
	// during GGUF distribution) when it runs remotely.
	coordModelPath := entry.Source.Path
	if !coordHost.local {
		if p, ok := workerModelPaths[coordHost.nodeID]; ok && p != "" {
			coordModelPath = p
		} else {
			o.Log.Warn("no worker-side GGUF path known for coordinator host — falling back to leader-local path",
				"node", coordHost.nodeID, "path", coordModelPath)
		}
	}
	// `--rpc` only when there are actual rpc shards; a 1-shard model is a
	// plain whole-model llama-server.
	coordArgs := []string{
		"-m", coordModelPath,
		"--port", strconv.Itoa(coordPort),
		"--host", coordHostBind,
	}
	if len(rpcEndpoints) > 0 {
		// Every rpc backend must be dialable BEFORE we launch the coordinator.
		// llama-server does not treat an unreachable backend as fatal: it logs
		// "Failed to connect to <addr>", loads the model anyway, starts serving —
		// and then aborts ("signal: aborted (core dumped)") once it actually needs
		// the missing shard. The supervisor restarts it, it aborts again, and the
		// only symptom upstream is an endless 502 from the gateway. An rpc-server
		// whose port merely accepted a TCP connection at start_rpc time can be gone
		// by now, so re-check here, and fail the create with the exact address.
		if bad := unreachable(ctx, rpcEndpoints); len(bad) > 0 {
			o.rollback(ctx, created)
			return fmt.Errorf("rpc backend(s) unreachable from the leader: %s — "+
				"refusing to start a coordinator that would abort on them "+
				"(check the rpc-server on those nodes)", strings.Join(bad, ", "))
		}
		coordArgs = append(coordArgs, "--rpc", strings.Join(rpcEndpoints, ","))
	}
	coordSpec := agent.ProcessSpec{
		ID:      coordID,
		Command: "llama-server",
		// Coordinator also benefits from restart-on-crash — without it,
		// llama-server dying takes the model offline even if every rpc-server
		// is fine.
		Restart:        true,
		MaxRestarts:    5,
		RestartBackoff: time.Second,
		Args:           coordArgs,
		HealthPort:     coordPort,
		HealthHost:     coordHealthHost, // probe the address llama-server binds
		// llama-server binds its socket BEFORE loading the model, so a TCP probe
		// passes instantly and the shard gets stamped "ready" while the model is
		// still minutes from serving — or wedged on an rpc-server that never
		// answers. Then the only symptom is a 502 from the gateway, long after
		// `shard create` reported success. Require a real 200 from /health.
		HealthPath: "/health",
		// llama-server only listens once the model is loaded, and loading it
		// across rpc-servers can take minutes on a slower accelerator. The
		// default 30s probe expired mid-load, so the supervisor marked it failed
		// and Restart crash-looped it forever — the model never got the chance to
		// finish loading. Give it a window sized to a real load.
		ReadyTimeout: 15 * time.Minute,
	}
	o.Log.Info("starting coordinator",
		"model", entry.ID, "port", coordPort, "shards", len(rpcEndpoints),
		"host", coordHost.nodeID, "local", coordHost.local)

	if coordHost.local {
		if _, err := o.Supervisor.Start(ctx, coordSpec); err != nil {
			o.rollback(ctx, created)
			return fmt.Errorf("launch coordinator (local): %w", err)
		}
	} else {
		if _, err := o.callWorkerStart(ctx, *coordHost.node, coordSpec); err != nil {
			o.rollback(ctx, created)
			return fmt.Errorf("launch coordinator on %s: %w", coordHost.nodeID, err)
		}
	}

	// Address the *leader's router* will use to dial the coordinator.
	// Local: loopback. Remote: the worker's mesh address + the coord port.
	coordAddr := fmt.Sprintf("127.0.0.1:%d", coordPort)
	coordNodeID := "local"
	if !coordHost.local {
		coordNodeID = coordHost.nodeID
		host, _, sErr := net.SplitHostPort(coordHost.node.Address)
		if sErr != nil {
			host = coordHost.node.Address
		}
		coordAddr = fmt.Sprintf("%s:%d", host, coordPort)
	}

	coordRec := store.Shard{
		ID: coordID, ModelID: entry.ID, Role: "coordinator",
		NodeID: coordNodeID, Address: coordAddr,
		ProcessID: coordID, Status: "ready",
		CreatedAt: time.Now(), LastSeen: time.Now(),
	}
	if err := o.Store.Shards().Create(ctx, coordRec); err != nil {
		// coordinator running but not persisted; try to stop + return error
		if coordHost.local {
			_ = o.Supervisor.Stop(coordID)
		} else {
			_ = o.callWorkerStop(ctx, *coordHost.node, coordID)
		}
		o.rollback(ctx, created)
		return fmt.Errorf("persist coordinator: %w", err)
	}
	// (The `created` slice isn't read after this point — we're past the
	// rollback window. Successful return follows.)

	// Register a placement so the Router knows local hosts this model.
	if err := o.Store.Placements().Upsert(ctx, store.Placement{
		NodeID: "local", ModelID: entry.ID, Status: "ready", LastSeen: time.Now(),
	}); err != nil {
		o.Log.Warn("placement upsert failed for sharded model", "model", entry.ID, "err", err)
	}

	// Persist the model row too so /v1/models reflects the new model.
	_ = o.Store.Models().Upsert(ctx, store.Model{
		ID: entry.ID, CatalogID: entry.ID,
		Source: "llamacpp:" + entry.Source.Path,
		Status: "ready", SizeBytes: entry.SizeBytes,
		InstalledAt: time.Now(),
	})

	o.Log.Info("sharded model ready", "model", entry.ID, "shards", shardCount, "coordinator", coordRec.Address)
	return nil
}

// RemoveSharded tears down every shard for the given model: stops the
// coordinator locally, asks each worker to stop its rpc-server, deletes
// all shard + placement + model rows.
func (o *Orchestrator) RemoveSharded(ctx context.Context, modelID string) error {
	shards, err := o.Store.Shards().GetByModel(ctx, modelID)
	if err != nil {
		return err
	}
	for _, s := range shards {
		switch s.Role {
		case "coordinator":
			if s.NodeID == "" || s.NodeID == "local" {
				if err := o.Supervisor.Stop(s.ProcessID); err != nil {
					o.Log.Warn("coordinator stop failed", "id", s.ID, "err", err)
				}
			} else {
				node, err := o.Store.Nodes().Get(ctx, s.NodeID)
				if err != nil || node == nil {
					o.Log.Warn("coordinator's node not found", "id", s.ID, "node", s.NodeID)
					continue
				}
				if err := o.callWorkerStop(ctx, *node, s.ProcessID); err != nil {
					o.Log.Warn("remote coordinator stop failed", "id", s.ID, "err", err)
				}
			}
		case "rpc":
			node, err := o.Store.Nodes().Get(ctx, s.NodeID)
			if err != nil || node == nil {
				o.Log.Warn("rpc shard's node not found", "id", s.ID, "node", s.NodeID)
				continue
			}
			if err := o.callWorkerStop(ctx, *node, s.ProcessID); err != nil {
				o.Log.Warn("rpc shard stop failed", "id", s.ID, "err", err)
			}
		}
	}
	if err := o.Store.Shards().DeleteByModel(ctx, modelID); err != nil {
		return err
	}
	if err := o.Store.Placements().Delete(ctx, "local", modelID); err != nil {
		o.Log.Warn("placement delete failed", "err", err)
	}
	_ = o.Store.Models().Delete(ctx, modelID)
	return nil
}

// isVLLMRayBackend reports whether a sharding spec targets the vLLM multi-node
// (Ray) backend rather than the default llama.cpp RPC backend.
func isVLLMRayBackend(engine string) bool {
	e := strings.ToLower(strings.TrimSpace(engine))
	return e == "vllm" || e == "vllm-ray" || e == "vllm_ray"
}

// hostOf returns the host part of a host:port address (or the whole string if
// it carries no port).
// unreachable returns the rpc endpoints that do not accept a TCP connection.
func unreachable(ctx context.Context, endpoints []string) []string {
	var bad []string
	for _, ep := range endpoints {
		d := net.Dialer{Timeout: 5 * time.Second}
		conn, err := d.DialContext(ctx, "tcp", ep)
		if err != nil {
			bad = append(bad, ep+" ("+err.Error()+")")
			continue
		}
		_ = conn.Close()
	}
	return bad
}

func hostOf(addr string) string {
	if h, _, err := net.SplitHostPort(addr); err == nil {
		return h
	}
	return addr
}

// distEnv pins torch's collective backends (Gloo for the CPU control plane, NCCL
// for GPU transfers) to the interface that carries `host`.
//
// Without this, vLLM's cross-node workers bind their rendezvous sockets to the
// container's DEFAULT interface — which inside Docker is the bridge (eth0,
// 172.17.0.x). That address is meaningless on any other machine, so the remote
// worker advertises 172.17.0.2 and the mesh dies with
// "Gloo connectFullMesh failed ... Connection refused, remote=[172.17.0.2]".
// Ray itself is unaffected (it uses the address we pass explicitly), which is why
// the cluster forms and only the vLLM engine init fails.
//
// On an overlay-joined node the routable address is the tailnet IP (CGNAT
// 100.64.0.0/10) on tailscale0, so pin to that; otherwise leave the vars unset
// and let torch autodetect on a flat LAN.
func mergeEnv(base, extra map[string]string) map[string]string {
	for k, v := range extra {
		base[k] = v
	}
	return base
}

func distEnv(host string) map[string]string {
	env := map[string]string{"VLLM_HOST_IP": host}
	if ip := net.ParseIP(host); ip != nil {
		if ip4 := ip.To4(); ip4 != nil && ip4[0] == 100 && ip4[1] >= 64 && ip4[1] <= 127 {
			env["GLOO_SOCKET_IFNAME"] = "tailscale0"
			env["NCCL_SOCKET_IFNAME"] = "tailscale0"
		}
	}
	return env
}

// createShardedVLLMRay stands up a Ray cluster across the chosen workers so a
// single vLLM process can span them — tensor-parallel within each node,
// pipeline-parallel across nodes. It mirrors the llama.cpp coordinator pattern:
// eventually ONE endpoint (the Ray head's vLLM server) is registered as the
// placement and the router talks only to it; Ray owns the cross-node fan-out.
//
// PHASE 1 (this implementation): bring up the Ray cluster ONLY and leave it
// running — `ray start --head` on workers[0], `ray start --address=<head>` on
// the rest. Launching `vllm serve --distributed-executor-backend ray` on the
// head and registering the placement is Phase 2. Ports are pinned (not Ray's
// random range) so the fixed set has a chance of traversing the overlay.
