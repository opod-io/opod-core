// Package scheduler is the leader-side orchestration logic for sharded
// models. CreateSharded picks workers, asks each one to start an rpc-server
// for its piece, then launches the coordinator llama-server locally and
// stitches everything together via a placement row that the router resolves.
//
// Current scope:
//   - manual shard-count override (`--shards=N`) or catalog default
//   - simple bin-pack on free RAM (highest free first)
//   - the coordinator runs on whichever node has the most RAM (leader or worker),
//     overridable with OPOD_COORDINATOR_NODE; with one shard it runs on that
//     shard's worker, which is the placement itself
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
	// WeightsHTTP carries the calls that move or load model weights (a worker's
	// /v1/model/load, a GGUF upload): they answer when the work is DONE, which
	// on a cold cache is minutes to hours. HTTP's 60 s is the budget of a small
	// control call and cut them mid-pull. The generous timeout is a backstop
	// only — the caller's context decides when to give up.
	WeightsHTTP *http.Client
	// ModelsDir is the local destination for HuggingFace GGUF downloads
	// when a sharded catalog entry sets source.type=huggingface. Empty
	// means HF auto-download is disabled — operators have to pre-place
	// the file at source.path the old-fashioned way.
	ModelsDir string
	// From the environment contract (config.Env), set by `opod up`:
	CoordinatorNode string // OPOD_COORDINATOR_NODE: pin the llama.cpp coordinator ("local" = the leader)
	HFToken         string // HF_TOKEN for the leader's own GGUF pulls
	HFEndpoint      string // HF_ENDPOINT ("" = the public Hub)
	// HeartbeatMaxAge is the liveness bound WorkerFor applies to a row
	// (router.heartbeat_max_age_seconds, set by `opod up`); 0 = DefaultHeartbeatMaxAge.
	HeartbeatMaxAge time.Duration

	moves moveGuard // one move at a time per model and per node (move.go)
}

// weightsHTTP is WeightsHTTP, or its default for an Orchestrator built as a
// literal (tests do): a missing client must not turn a load into a nil call.
func (o *Orchestrator) weightsHTTP() *http.Client {
	if o.WeightsHTTP != nil {
		return o.WeightsHTTP
	}
	return &http.Client{Timeout: 6 * time.Hour}
}

// New returns a configured orchestrator.
func New(st store.Store, sup *agent.Supervisor, log *slog.Logger, modelsDir string) *Orchestrator {
	if log == nil {
		log = slog.Default()
	}
	return &Orchestrator{
		Store:       st,
		Supervisor:  sup,
		Log:         log,
		HTTP:        &http.Client{Timeout: 60 * time.Second},
		WeightsHTTP: &http.Client{Timeout: 6 * time.Hour},
		ModelsDir:   modelsDir,
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
// TP × PP must equal the number of GPUs in the gang: ranks (parts) ×
// DevicesPerRank (GPUs each part holds — build item 5, "k parts per node and
// k GPUs per part"). The zero value means "decide for me": TP = the devices
// of one part (tensor parallel stays inside the machine), PP = <parts>.
//
// Parallelism is the tensor-parallel × pipeline-parallel split for a sharded model.
type Parallelism struct {
	TP int // tensor-parallel size
	PP int // pipeline-parallel size
	// DevicesPerRank is how many GPUs every part holds (0 = 1). TP must be a
	// multiple of it: the devices inside a part are always in one tensor group.
	DevicesPerRank int
}

// resolve fills in the defaults and checks the product against the GPU count.
func (p Parallelism) resolve(ranks int) (Parallelism, error) {
	k := max(1, p.DevicesPerRank)
	total := ranks * k
	tp, pp := p.TP, p.PP
	switch {
	case tp <= 0 && pp <= 0:
		tp, pp = k, ranks // the safe default: TP inside each part, PP across parts
	case tp > 0 && pp <= 0:
		if total%tp != 0 {
			return p, fmt.Errorf("tp=%d does not divide the gang's %d GPUs (%d parts × %d)", tp, total, ranks, k)
		}
		pp = total / tp
	case pp > 0 && tp <= 0:
		if total%pp != 0 {
			return p, fmt.Errorf("pp=%d does not divide the gang's %d GPUs (%d parts × %d)", pp, total, ranks, k)
		}
		tp = total / pp
	}
	if tp%k != 0 {
		return p, fmt.Errorf("tp=%d must be a multiple of the %d GPUs each part holds", tp, k)
	}
	if tp*pp != total {
		return p, fmt.Errorf("tp=%d × pp=%d = %d, but the gang has %d GPUs (%d parts × %d per part)",
			tp, pp, tp*pp, total, ranks, k)
	}
	return Parallelism{TP: tp, PP: pp, DevicesPerRank: k}, nil
}

// shardCountFor is the whole of how a create request becomes a part count: the
// named nodes, else the caller's count, else the catalog's default_shards. The
// leader never derives a count from the fleet — picking one from worker memory
// is the CLI's default alone (PickShards), sent here as an explicit shape. A
// caller that computed its own shape therefore always gets exactly that shape.
func shardCountFor(entry models.Entry, shardCount int, nodeIDs []string) (int, error) {
	if len(nodeIDs) > 0 {
		shardCount = len(nodeIDs)
	}
	if shardCount <= 0 {
		shardCount = entry.Sharding.DefaultShards
	}
	if shardCount < 1 {
		return 0, fmt.Errorf("shard count must be at least 1 (got %d)", shardCount)
	}
	return shardCount, nil
}

// gangID names which gang of the model to build.
//
//	""          the model's default gang, AND every other gang of the model is
//	            torn down first — what a bare `opod shard create <model>` has
//	            always meant: one gang, replacing whatever was there.
//	"g1", …     that gang alone. Other gangs keep serving, untouched.
//
// Several gangs of one model are independent copies of the same weights: each
// answers whole requests by itself, and the router picks among their
// coordinators. A part never spans gangs.
func (o *Orchestrator) CreateSharded(ctx context.Context, entry models.Entry, gangID string, shardCount int, nodeIDs []string, par Parallelism) error {
	if !entry.Sharding.Required {
		return fmt.Errorf("model %s is not configured for sharding", entry.ID)
	}
	gangID, replaceModel, err := resolveGang(gangID)
	if err != nil {
		return err
	}
	shardCount, err = shardCountFor(entry, shardCount, nodeIDs)
	if err != nil {
		return err
	}
	// Workers first: a create that cannot be placed refuses here, with the
	// numbers, before the gang it would have replaced is torn down and before
	// any process starts. Both backends build on this one list.
	var workers []store.Node
	if len(nodeIDs) > 0 {
		workers, err = o.pickWorkersByID(ctx, nodeIDs)
	} else {
		workers, err = o.pickWorkers(ctx, shardCount)
	}
	if err != nil {
		return fmt.Errorf("%w: %w", ErrUnplaceable, err)
	}
	// IDEMPOTENT REPLACE: tear down the prior shard before (re)creating. Without
	// this, re-running `shard create` (new --nodes, or after a worker was
	// recreated with a fresh overlay IP) left the OLD coordinator / rpc-server
	// rows in place — pointing at dead/old addresses — and the new create
	// collided or silently no-op'd, so the model 502'd on a stale coordinator.
	// A clean slate beats a stale one; best-effort teardown.
	//
	// What gets replaced depends on who asked. A bare create still replaces the
	// whole model, because that is what it has always meant and a caller who
	// names no gang is not thinking in gangs. A create that NAMES a gang
	// replaces only that gang — the whole point of gangs is that the others
	// keep serving through it.
	o.replacePrior(ctx, entry.ID, gangID, replaceModel)
	// Which placer chose the parts is worth a line: a control plane always
	// names the nodes (its ledger decided); the leader's own picker is the
	// standalone CLI's path, and two placers with two truths is what W8 warns of.
	if len(nodeIDs) > 0 {
		o.Log.Info("placer: caller-named nodes", "model", entry.ID, "nodes", nodeIDs)
	} else {
		o.Log.Info("placer: leader (ready workers by RAM)", "model", entry.ID, "shards", shardCount)
	}
	// Backend fork: vLLM multi-node uses a Ray cluster + pipeline/tensor
	// parallelism, NOT llama.cpp's rpc-server + coordinator. It skips all the
	// GGUF machinery below. Selected by the catalog's sharding.engine.
	if isVLLMRayBackend(entry.Sharding.Engine) {
		return o.createShardedVLLMRay(ctx, entry, gangID, workers, par)
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

	// The store-driven teardown above only sees shards THIS leader recorded.
	// After a leader restart (fresh state) the previous convergence's
	// rpc-servers / coordinator still run on the workers, and the create below
	// collides with `process "s-…" already exists`. Sweep every registered
	// worker for processes carrying this shard prefix and stop them.
	//
	// Scoped to the gang being built, NOT the model: a sweep by model prefix
	// would stop a sibling gang's parts, which are serving requests right now,
	// on every node the two gangs share.
	if replaceModel {
		o.stopOrphanShardProcs(ctx, entry)
	} else {
		o.stopOrphanGangProcs(ctx, entry, gangID)
	}

	// One allocator for this whole create: ports it hands out are remembered,
	// so two parts of this gang — or this gang and one already on the node —
	// never get the same port.
	ports := o.newPortAllocator(ctx)

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
			// A port free on THIS node: two gangs of one model may share a
			// worker, and rpcPortBase is a catalog scalar they both start from.
			port := ports.take(w.ID, rpcPortBase+i)
			shardID := gangShardID(entry.ID, gangID, fmt.Sprintf("rpc-%d", i))
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
				ID: shardID, ModelID: entry.ID, GangID: gangID, Role: "rpc",
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
		if ov := o.CoordinatorNode; ov != "" && ov != w.ID {
			o.Log.Warn("OPOD_COORDINATOR_NODE ignored for single-shard placement — coordinator runs on the selected node",
				"override", ov, "node", w.ID)
		}
	} else {
		coordHost = o.pickCoordinatorHost(ctx, workers)
	}

	// Avoid port collisions with shards already running on the same host —
	// two whole-model placements, or two gangs of one model, must not fight
	// over :9001. The allocator also knows the rpc ports taken just above.
	if free := ports.take(coordHost.nodeID, coordPort); free != coordPort {
		o.Log.Info("coordinator port in use on host — bumped", "node", coordHost.nodeID, "want", coordPort, "using", free)
		coordPort = free
	}

	// Launch the coordinator. Two branches: on the leader we use the local
	// supervisor; on a worker we POST /v1/process/start exactly like rpc-server.
	coordID := gangShardID(entry.ID, gangID, "coord")
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
		ID: coordID, ModelID: entry.ID, GangID: gangID, Role: "coordinator",
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
		case "rpc", "rank":
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

func mergeEnv(base, extra map[string]string) map[string]string {
	for k, v := range extra {
		base[k] = v
	}
	return base
}

// distEnv pins vLLM's rendezvous to the address the coordinator dialled.
// Without it a cross-node worker binds to the container's default interface
// (Docker's bridge, 172.17.0.x), which means nothing on another machine, and
// the mesh dies with "Gloo connectFullMesh failed ... Connection refused,
// remote=[172.17.0.2]". Ray is unaffected (it takes the address explicitly);
// only the engine init needs the hint. The socket interface for Gloo/NCCL is
// left to torch's autodetection on whatever network the nodes share.
func distEnv(host string) map[string]string {
	return map[string]string{"VLLM_HOST_IP": host}
}

// createShardedVLLMRay stands up a Ray cluster across the chosen workers so a
// single vLLM process can span them — tensor-parallel within each node,
// pipeline-parallel across nodes. It mirrors the llama.cpp coordinator pattern:
// eventually ONE endpoint (the Ray head's vLLM server) is registered as the
// placement and the router talks only to it; Ray owns the cross-node fan-out.
//
// The Ray cluster comes up first (`ray start --head` on workers[0],
// `ray start --address=<head>` on the rest), then `vllm serve
// --distributed-executor-backend ray` on the head is registered as the
// coordinator the router dials. Every rank is a shard row so the gang lists and
// tears down whole. Ports are pinned (not Ray's random range) so the fixed set
// has a chance of traversing the overlay. k GPUs per rank per build item 5.

// RemoveShardsOn removes every shard group with a part recorded on the node
// — the R10.1 answer to a worker whose process changed (boot id): nothing
// recorded on the previous process is running, so no process list is
// consulted. Returns the models whose groups went.
func (o *Orchestrator) RemoveShardsOn(ctx context.Context, nodeID string) (removed []store.GangKey, err error) {
	all, err := o.Store.Shards().List(ctx)
	if err != nil {
		return nil, err
	}
	// Per GANG. A part lost on this node takes ITS gang down — the others are
	// whole and serving, and tearing the model down would have stopped them
	// for a fault they did not have.
	gone := map[store.GangKey]bool{}
	for _, s := range all {
		if s.NodeID == nodeID {
			gone[store.GangKey{Model: s.ModelID, Gang: s.Gang()}] = true
		}
	}
	for key := range gone {
		if err := o.RemoveGang(ctx, key.Model, key.Gang); err != nil {
			o.Log.Warn("stale gang removal failed", "model", key.Model, "gang", key.Gang, "node", nodeID, "err", err)
			continue
		}
		removed = append(removed, key)
	}
	sortGangKeys(removed)
	return removed, nil
}

// ReconcileNode compares the shard rows recorded on one worker with the
// processes that worker reports running, and removes every shard group
// whose part is gone. The case: a worker registers AGAIN under the same node
// id — a recreated pod (LeaderWorkerSet group recreate, a StatefulSet
// restart) or a restarted container — and whatever the leader recorded as
// running there died with the old incarnation. A row that still read
// "ready" routed a coordinator address that no longer answered (found on
// the laptop cluster, 2026-09-14). Removing the group lets the manager (or
// an operator) create it again on the parts that exist now. Best effort: a
// worker that does not answer its process list is left alone until it does.
func (o *Orchestrator) ReconcileNode(ctx context.Context, node store.Node) (removed []store.GangKey, err error) {
	all, err := o.Store.Shards().List(ctx)
	if err != nil {
		return nil, err
	}
	var mine []store.Shard
	for _, s := range all {
		if s.NodeID == node.ID {
			mine = append(mine, s)
		}
	}
	if len(mine) == 0 {
		return nil, nil
	}
	procs, err := o.callWorkerList(ctx, node)
	if err != nil {
		return nil, fmt.Errorf("process list on %s: %w", node.ID, err)
	}
	running := map[string]bool{}
	for _, p := range procs {
		if p.Status == "running" || p.Status == "starting" {
			running[p.ID] = true
		}
	}
	// Per GANG, for the same reason as RemoveShardsOn: a process that died on
	// this worker belongs to one gang, and its siblings are still serving.
	gone := map[store.GangKey]bool{}
	for _, s := range mine {
		if !running[s.ProcessID] {
			gone[store.GangKey{Model: s.ModelID, Gang: s.Gang()}] = true
		}
	}
	for key := range gone {
		if err := o.RemoveGang(ctx, key.Model, key.Gang); err != nil {
			o.Log.Warn("stale gang removal failed", "model", key.Model, "gang", key.Gang, "node", node.ID, "err", err)
			continue
		}
		removed = append(removed, key)
	}
	sortGangKeys(removed)
	return removed, nil
}
