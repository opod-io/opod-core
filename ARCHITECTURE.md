# Opod Architecture

> Scope note (ADR-022, 2026-09-06): core is the CLI-only inference runtime. The dashboard, invites, vendor egress, routing chains, callbacks, budgets, usage/audit query APIs and guardrail implementations moved to the control plane; this document describes what remains. The stable manager surface is listed at the end.

Deep-dive design for contributors and maintainers. For user-facing docs, see [README.md](README.md). For what is next, see [ROADMAP.md](ROADMAP.md).

> **Doc-vs-code currency:** this document covers the shipped feature set — cross-node routing, sharding auto-orchestration, the CLI and `/admin/v1` as the only interfaces, HMAC mutual auth, GGUF distribution, OTLP traces, 15 connect clients, interactive picker, shell completion, `--json` on every read command, first-run wizard, real progress bar, colored output, engine health watchdog, typed `engine_unreachable` errors. The code on `main` is the source of truth — if you find a mismatch please file an issue or PR.

---

## Table of contents

- [Goals and non-goals](#goals-and-non-goals)
- [Big picture](#big-picture)
- [Process model](#process-model)
- [Control plane internals](#control-plane-internals)
- [Agent internals](#agent-internals)
- [Mesh networking](#mesh-networking)
- [Storage](#storage)
- [Protocol adapters](#protocol-adapters)
- [Router](#router)
- [Scheduler](#scheduler)
- [Engine drivers](#engine-drivers)
- [Model registry and puller](#model-registry-and-puller)
- [Authentication and authorization](#authentication-and-authorization)
- [Observability](#observability)
- [Security model](#security-model)
- [Why each technology was chosen](#why-each-technology-was-chosen)
- [Concurrency model](#concurrency-model)
- [Project layout](#project-layout)
- [Coding conventions](#coding-conventions)
- [Build from source](#build-from-source)
- [Getting started as a contributor](#getting-started-as-a-contributor)
- [How to extend Opod](#how-to-extend-opod)

---

## Goals and non-goals

### Goals

1. Run on a single laptop *and* a multi-node cluster with the same binary.
2. One-command install. Zero config to first response.
3. Drop-in compatibility with the OpenAI API (chat, embeddings, models) — the one protocol surface (ADR-022).
4. Mac + Linux + NVIDIA in one fleet, transparently.
5. Strong defaults; expert overrides via YAML.
6. Maintainable by junior engineers — small surface, no magic.

### Non-goals

1. Training or fine-tuning.
2. Beating frontier models. We surface them via fallback.
3. Replacing Kubernetes for general workloads.
4. Windows-native workers.

---

## Big picture

```
   CLIENTS  (Cursor · Claude Code · Aider · SDKs · curl)
                       │
                       ▼  one endpoint, one key
   ┌──────────────────────────────────────────────────┐
   │  GATEWAY (leader)                                │
   │  OpenAI-compatible · auth · quotas               │
   └────────────────────┬─────────────────────────────┘
                        │
   ┌────────────────────▼─────────────────────────────┐
   │  ROUTER  (internal/router)                       │
   │  model → placements → least-loaded node          │
   │  caches remote engine handles per node           │
   └────┬───────────────────────┬─────────────────────┘
        │ local                 │ remote (via worker HTTP)
        ▼                       ▼
   ┌─────────────┐   ┌─────────────────────┐   ┌──────────────────┐
   │ leader's    │   │ Worker A (Mac Mini) │   │ Worker B (NVIDIA)│
   │ local       │   │  agent.Server       │   │  agent.Server    │
   │ engine      │   │  → local Ollama     │   │  → local vLLM    │
   │ (Ollama)    │   │  (token-auth'd)     │   │  (token-auth'd)  │
   └─────────────┘   └─────────────────────┘   └──────────────────┘
                              ▲                         ▲
                              │  heartbeat every 5s     │
                              │  carries loaded_models  │
   ┌──────────────────────────┴─────────────────────────┴──────────┐
   │  CONTROL PLANE                                                │
   │  node registry · model placements · usage · audit · /admin/v1 │
   └───────────────────────────────────────────────────────────────┘
                              ▲
                              │ mesh: LAN today
                              │ (tsnet planned — not implemented)
```

Two distinct planes:

- **North-south** — clients → gateway → router → engine (local or remote). Data plane. Latency-sensitive. Per-request work; KV caches live in the chosen engine.
- **East-west** — control plane ↔ agents. Cluster management. Lower volume. Direct HTTP: agents POST register/heartbeat to the leader's admin API, and the leader calls each worker's HTTP server directly. There is no message broker.

A control-plane DB outage does **not** kill in-flight requests — the router keeps using its in-memory cache of node addresses + worker tokens. If a node disappears mid-stream, the next request will surface the routing error; the cache is rebuilt from the placements table once the DB is back.

---

## Process model

One binary, four modes determined by subcommand:

| Mode | What runs in-process |
|---|---|
| `opod up` | **Leader**: HTTP gateway · Router · Control plane (`/admin/v1`) · embedded SQLite · local engine adapter. No UI: `/` answers 404 (ADR-022) |
| `opod join "<url>?token=…"` | **Worker**: agent.Loop (heartbeat with loaded_models) · agent.Server (OpenAI-compat passthrough bound to the LAN/tailnet address) · local engine adapter |
| `opod <cmd>` (e.g. `node ls`, `model add`) | One-shot CLI; reads SQLite directly or calls the leader's admin API |
| `opod doctor` | Stand-alone diagnostics — port availability, Ollama reachability, catalog count, hardware summary |
| `opod update` / `opod upgrade` | Hits `api.github.com/repos/opod-io/opod-core/releases/latest`, downloads the matching platform tarball, verifies SHA-256 against `checksums.txt`, atomically replaces the running binary. Restarts are user-driven (`opod down && opod up`). |

The leader and worker share the same internal packages; the difference is which subsystems are wired up in `cmd/opod/main.go`.

### Process lifecycle

1. `main()` parses subcommand + flags
2. Loads config (`internal/config`)
3. Initializes telemetry (`internal/controlplane/tracing.go`, `internal/metrics`)
4. Initializes mesh (`internal/mesh`)
5. Initializes store (`internal/store`)
6. Wires up subsystems based on mode
7. Runs until SIGINT/SIGTERM, then graceful shutdown via context cancellation

### Graceful shutdown

- Stop accepting new HTTP connections
- Wait up to `drain_timeout_s` for in-flight requests
- Stop background goroutines (heartbeat loop on workers, supervised engine processes, cache reaper)
- Close mesh
- Flush metrics, traces, logs
- Close DB

---

## Cross-node routing (the v0.3 core)

The Router is what makes "leverage multiple machines" mean something. It implements `engines.Engine`, so handlers don't know whether a request is served locally or proxied — they just call `h.Engine.Chat(ctx, req)`.

```
   Handler.Chat(req)
        │
        ▼
   Router.pick(model)              ← internal/router/router.go
        │
        ├─ store.Placements.GetByNode("local", model) → has it? → return local engine
        │
        └─ store.Placements.GetByModel(model)
                │
                ▼
           filter: status == "ready"
                │
                ▼
           sort by router.inflight[nodeID] ASC
                │
                ▼
           pick first → store.Nodes.Get(id) → build/cached VLLM driver
                                              pointing at node.Address
                                              with node.WorkerToken
                ▼
           return remoteEngine
```

### Selection policy (v0.3)

1. **Local first.** If the leader's local engine has the model, use it. Lowest latency, no network hop.
2. **Least-loaded worker** otherwise. The router maintains an in-process `map[nodeID]int` of in-flight request counts and picks the lowest.
3. **Fall back to local** if no node has the model. Local will return a clear "model not found" the client can act on.

The router's wrapping of the engine channel decrements the in-flight counter when the upstream stream closes, so counts stay accurate without explicit acknowledgement from the caller.

### Which workers take new work — one rule

"Live" is decided in one place, from the node row, and never stored: `store.Node.TakesNewWork(maxAge, now)` (`internal/store/nodelive.go`; `WhyNoNewWork` is the same rule with the reason, `LiveState` the state to show). A node takes new work when it is not drained, its state is a serving one, and it heartbeated within `router.heartbeat_max_age_seconds`. Everything that decides or reports on workers asks it:

- the router — `takingRequests` filters a model's holders once, *before* roles, revision groups and load scores, adding the two things only a router knows (an address to dial, its cooldown penalty box). So a revision whose only worker is drained, lost or cooling down is a revision with no worker: its weight goes to the others instead of being rolled and then dropped on the local fallback. The hedged pick and the shard-gang check (`shardGroupRoutable`) use the same function;
- `/readyz`, the waking `503`, `/loadz` `workers`, `GET /v1/models`;
- the shard pickers (`scheduler.WorkerFor` = this rule + "has an address, is not the leader's own row") and `opod node ls`.

### Draining a node (`opod node drain` · `POST /admin/v1/nodes/{id}/drain|undrain`)

A drain is one column — the node row's `state = "draining"` — that every picker reads at pick time; nothing is cached, so it takes effect on the next request and `undrain` does too.

- **No new request.** `pick()` drops draining workers from the candidate list *before* roles, revision groups and load scores see them, so the load-aware scorer never ranks one and a revision whose only worker drains is a revision with no worker (its share goes to the rest). The hedged pick skips them the same way. Requests already streaming from the node are not touched: they finish on the engine they started on.
- **A gang is one unit.** A sharded model with *any* part on a draining node — coordinator or rpc backend — stops receiving requests (`shardGroupRoutable`, checked on every pick ahead of the cached coordinator engine, together with the heartbeat-age rule).
- **No new shard part.** The shard pickers take `ready` rows only, so a draining worker is never picked; naming one outright — `opod shard create --nodes …`, `opod model add <id> --node …` — is refused (`node … is not ready`, `pickWorkersByID`).
- **Nothing left = the waking 503.** Draining workers are not serving capacity: `/readyz` and the dispatch check (`hasServingCapacity`) skip them, so when every worker that could serve drains, a router-only leader answers the same `503` + `Retry-After` it answers while workers wake.
- **It survives.** Heartbeats write liveness with a targeted update (`NodeStore.Heartbeat`: `last_heartbeat`, boot id, `joining → ready`) and never the state, so a drain cannot be lost to the read-modify-write of a concurrent heartbeat; a worker that restarts and registers again under the same id stays drained. `undrain` is the only way back. The leader's own `local` row is refused — the router serves from the local engine before it looks at workers, so that state would be honoured by nothing.
- **What it does not do.** It moves no model and waits for nothing: there is no per-model drain on a worker yet, and no "wait for in-flight to reach zero, then remove" (see *Drain algorithm* under Scheduler — still planned). Feature key `node_drain`; events `node.drained` / `node.undrained`.

### Engine placement on the accelerator

llama.cpp keeps every layer on the CPU unless it is told otherwise. A worker that holds a GPU therefore
has to ask for the offload, or the card it reserved does nothing while the model serves slowly from the
CPU — which is easy to miss, because a small model answers fast enough either way. So an accelerated
worker passes `--n-gpu-layers` for every layer and lets the engine place what fits, and a plan that pins
`ngl` always wins, keeping a partial offload the operator's choice.

Whether the worker is accelerated is stated by the control plane in `OPOD_ACCELERATOR`, not guessed by
the worker: the AMD and Intel images carry no NVIDIA tooling, so a worker probing for itself would
conclude it has no GPU on exactly the vendors that need the offload most. A worker started by hand, with
no control plane to tell it, falls back to what it can detect.

### Worker HTTP server (`internal/agent/server.go`)

Each worker runs a thin OpenAI-compatible HTTP server bound to the address it reported at registration time. The server has three routes:

| Route | Behavior |
|---|---|
| `GET /healthz` | Calls `Engine.Health(ctx)`; returns 200 if the local engine is reachable. |
| `GET /v1/models` | Calls `Engine.List(ctx)` and emits the OpenAI `{"object":"list","data":[…]}` shape. |
| `POST /v1/chat/completions` | Decodes the OpenAI request, calls `Engine.Chat(ctx, req)`, re-emits as SSE (stream=true) or aggregated JSON (stream=false). |

Auth is HMAC-based: the leader and agent both sign requests with the per-node worker_token, set at registration. Signature header `X-Opod-Auth: v=1,id=<nodeID>,ts=<unix>,sig=<hex>` carries an HMAC-SHA256 of `v1\n<METHOD>\n<PATH>\n<ts>` keyed by the token. Receiver re-derives and constant-time compares; ts must be within ±5 minutes (replay window). The bearer fallback (`Authorization: Bearer <worker_token>`) is still accepted for one transition release; set `OPOD_REJECT_BEARER=1` on workers to refuse it. See `internal/auth/hmac.go`.

### Placements (`internal/store/sqlite.go → model_placements`)

```sql
CREATE TABLE model_placements (
    node_id    TEXT NOT NULL,    -- "local" for the leader, or a worker node id
    model_id   TEXT NOT NULL,    -- the engine-native model id (e.g. "llama3.2:1b")
    status     TEXT NOT NULL,    -- "ready" | "loading" | "draining" | "error"
    last_seen  INTEGER NOT NULL,
    PRIMARY KEY (node_id, model_id)
);
CREATE INDEX idx_placements_model ON model_placements(model_id);
```

Worker heartbeats carry `loaded_models`; the leader calls `PlacementStore.ReplaceForNode(nodeID, …)` to reconcile atomically every 5s. Local placements (`node_id="local"`) are populated by `cmd_model.go` on add and by `cmd_up.go` on startup (it lists the leader's local engine).

`GetByModel` returns only `status="ready"` rows, so flipping a placement to
`draining` instantly unroutes it — that's the hook the memory lifecycle uses
during evictions (drain → unload → back to `ready`, since the model stays
installed and demand-loadable).
A heartbeat does not undo the flip: `ReplaceForNode` keeps a `draining` mark
while the worker still reports the model, and the row goes — mark and all —
when the model leaves the report (`TestReplaceForNodeKeepsADrainingMark`,
`TestPlacementDrainSurvivesHeartbeats`). A leader start lifts marks left by a
previous process.

### Memory lifecycle (`internal/lifecycle`)

Placements say what a node *can serve* (installed); they say nothing about
what occupies RAM. The lifecycle manager closes that gap for the **local
engine**:

- **Residency ground truth** comes from the engine, not the DB: Ollama's
  `/api/ps` reports per-model RAM/VRAM bytes (`engines.ResidentLister`).
- **Admission**: `opod model load` / `POST /admin/v1/models/{id}/load`
  checks footprint (weights + ~20%) against `total RAM × (1 − reserve)` minus
  live resident bytes, and refuses rather than overcommit.
- **Evict-and-swap**: with `swap`, victims are chosen LRU (from the usage
  table) among non-pinned residents, drained via the `draining` placement
  status + the router's in-flight counts, unloaded (`keep_alive:0`), and
  audit-logged (`model_evicted`).
- **Desired placements** (`desired_placements` table: priority, pinned) are
  the declarative intent — `opod up` restores them in priority order in a
  background goroutine. Pinning maps to Ollama `keep_alive:-1`.
- **Release**: `opod down` unloads all resident models by default; Ctrl-C of
  `opod up` does not (dev-restart friendliness — the supervisor still kills
  any Opod-spawned engine processes either way).

Worker-side enforcement is deferred: workers load models via their own
`opod model add`, the leader has no remote-unload path, and heartbeats
rewrite worker placements every 5s (they never touch `node_id="local"`, which
is why local draining is safe).

### Sharding auto-orchestration (v0.4)

For models that don't fit on a single machine, `llama.cpp`'s `--rpc` mode lets the model be split **layer-wise** across multiple nodes (each host holds a contiguous slab of layers; this is model-parallel-by-layer, **not** tensor-parallel — the RPC backend has no tensor split and `CreateSharded` rejects `--tp>1`). **v0.4 automates the entire orchestration** — no SSHing into workers, no managing rpc-server processes by hand.

#### Components

| File | Role |
|---|---|
| `internal/agent/supervisor.go` | Process supervisor used on both leader and workers. Start/Stop/Logs with a TCP readiness probe. |
| `internal/agent/server.go` | Worker exposes `POST /v1/process/start`, `/stop`, `/list`, `/logs` — token-auth'd, calls into the supervisor. |
| `internal/scheduler/sharding.go` | Leader-side `Orchestrator.CreateSharded` / `RemoveSharded`. Picks workers, calls their process endpoints, launches the coordinator locally, persists shard rows. |
| `internal/scheduler/llamacpp.go` | Single-node `EnsureLlamaServer` — `cmd_up` calls this when `engine.preferred=llamacpp` and nothing is listening on `llamacpp_endpoint`. Same `ProcessSpec` shape as the sharding coordinator, just without `--rpc`. |
| `internal/engines/llamacpp/` | Driver that talks OpenAI-compat to a `llama-server` (single-node or RPC coordinator — driver doesn't care). Composes `engines/openaicompat` like vLLM/MLX. |
| `internal/router/router.go` | `shardCoordinator()` short-circuits the normal placement lookup when a sharded model is requested — points the request at the coordinator's address. |

#### Flow: `opod shard create llama-3.3-70b-sharded 2`

```
  CLI → POST /admin/v1/shards/create on the leader
            │
            ▼
   Orchestrator.CreateSharded(entry, 2):
       │
       ├─ pickWorkers(2) — rows scheduler.WorkerFor accepts, descending RAM;
       │     fewer than 2 → refused here (409), nothing torn down or started
       │
       ├─ for each worker i:
       │     spec = { id, command: "rpc-server", args: ["-p", port],
       │              healthPort: port }
       │     POST <worker>/v1/process/start
       │     (worker supervisor launches rpc-server,
       │      waits for TCP readiness on port, returns PID)
       │     persist Shard{role:"rpc", node_id:<worker>, address:<worker>:<port>}
       │
       ├─ leader.Supervisor.Start("llama-server",
       │     args: ["-m", <gguf>, "--rpc", "w1:port,w2:port", "--port", 9001])
       │   wait for TCP readiness on 9001
       │   persist Shard{role:"coordinator", node_id:"local", address:"127.0.0.1:9001"}
       │
       └─ Placement{node_id:"local", model_id:<id>, status:"ready"}

   Now the Router sees this placement; when a client requests the model,
   shardCoordinator() returns a llamacpp engine pointing at 127.0.0.1:9001.
```

#### Picking the part count: a CLI default, never the API's

`opod shard create <model>` with **no shape at all** — no count, no `--nodes`, no `--tp`/`--pp` — picks the shape itself (`scheduler.PickShards`, a pure function; `internal/scheduler/autoshard.go`). `opod model add <sharded-model>` hands off to the same command, so it picks too.

- **What it reads.** Rows the leader already has — nothing is probed (`scheduler.WorkerMemoryFacts`). A worker is a candidate when its row is `ready` and it heartbeated within `router.heartbeat_max_age_seconds`; the leader's own `local` row never is. Its capacity is the memory it registered — the sum of its cards' memory, else its RAM — under the lifecycle's reserve (`placement.reserve_percent`, `lifecycle.Budget`). What is already on it comes from its placement rows (the models its heartbeats report) and from shard rows (each part of a gang holds an equal share), sized by the lifecycle's estimate, weights × 1.2 (`lifecycle.Footprint`). A model whose size nobody recorded counts as 0, the same optimism admission applies. The gang being replaced is ignored, because a create tears it down first.
- **The rule.** Parts are taken as equal. A model that fits the freest worker is **1** part. Otherwise it is the **smallest N** whose N freest workers can each hold `need / N`. N never exceeds the live worker count, nor the model's layer count when the catalog records one (`architecture.layers` — a layer is the smallest thing either backend cuts). When nothing fits the command **refuses and names the numbers**: what the model needs, what each worker has free, and what the largest allowed split would ask of the worker that cannot hold it. Equal parts is exact for pipeline stages and conservative for llama.cpp's RPC split, which weights its cut by device memory.
- **What it sends.** The picked workers go out as an explicit `nodes` list, so the leader builds exactly what was printed (`picked 3 part(s): the model needs …`). A catalog entry with no size keeps its `default_shards`, and says so.
- **Who wins.** Any shape the operator names is sent untouched and the picker is not consulted (`TestShardShapeExplicitIsNeverChanged`). `POST /admin/v1/shards/create` **never picks**: a body's `nodes`, else its `shards`, else the catalog's `default_shards` is the whole of it (`shardCountFor`, `TestShardCountCallerWins`, `TestCreateShardedNeverPicksACount`). A caller of the API computed its own shape against its own view of the fleet; a leader that chose another would be a second placer with a second truth.
- **What it does not know.** A worker's `--vram-budget` is not reported to the leader, so a worker sharing its card is sized by the whole card — name the count there. A leader running with an in-memory store (managed mode) shows this command no workers; it refuses and asks for a count.

#### Failure handling

- **Which rows are workers is one rule** — `scheduler.WorkerFor` — asked by the CLI's shard-count picker (`WorkerMemoryFacts`), by the leader's own pick for a count without nodes (`pickWorkers`) and by the check of a named list (`pickWorkersByID`, which `model add --node` uses too): state `ready` (so never a draining node), an address, a heartbeat within `router.heartbeat_max_age_seconds` (60 s when that check is off), and never the leader's own `local` row — it reads ready and has an address, but that address is the gateway, which has no `/v1/process/start`. A create that cannot find its workers is refused **before** the gang it would replace is torn down and before any process starts (`scheduler.ErrUnplaceable` → `409`), and the message carries the numbers: `need 3 ready workers, have 1 (w1); not counted: local — the leader's own row, not a worker; w2 — state draining; w3 — last heartbeat 4m0s ago`.
- If any rpc-server fails to come up (readiness timeout, process exits), `Orchestrator.rollback()` stops every previously-launched process and returns the error to the caller (the CLI, or whoever called `/admin/v1/shards/create`).
- If a shard process crashes *after* CreateSharded returns, the supervisor auto-restarts it up to 5 times with exponential backoff (1s, 2s, 4s, 8s, 16s; capped at 30s for any longer chain). After 5 the process enters `crashloop` state and stays there — the admin must intervene. Both `rpc-server` (per-shard) and the `llama-server` coordinator are restart-enabled; the policy is set on the `agent.ProcessSpec` at launch time in `internal/scheduler/sharding.go`. Explicit `Stop()` suppresses any pending restart.

#### Out of scope for v0.4

- The coordinator (`llama-server`) is placed on the highest-RAM host in the shard set — by default the strongest worker, not the leader. Override with `OPOD_COORDINATOR_NODE=<node_id>` (or `local` to force leader). When the coordinator runs on a worker it's launched via the same `/v1/process/start` endpoint used for `rpc-server`, and the leader's router dials it at `<worker-address>:<coord_port>`. Single-machine sharding still pins the coordinator to the local supervisor.
- **Automatic GGUF download + distribution** fully closes M5-T12. For catalog entries with `source.type: huggingface` + `source.file:`, `CreateSharded` first downloads the GGUF from `huggingface.co/<repo>/resolve/main/<file>` into `storage.models_dir` on the leader (skipped if already present, partial→rename atomicity). Then for both `huggingface` and `file` types, fans the local file out to every shard host via `/v1/process/file` HEAD + `/v1/process/upload` POST (sha256-verified, skipped if the worker already has the file). No more manual `wget` to leader or `scp` to workers — `opod shard create <id>` is sufficient.
- Live shard migration / rebalancing. (Moving a whole, unsharded model between workers is designed — "Live model move" under Scheduler — and not built.)
- Dynamic shard count change.

#### Experimental: vLLM + Ray backend (true tensor / pipeline parallelism)

The default sharding path above is **layer-split** — llama.cpp's RPC backend cuts the
model by layers, not tensors. A separate, **experimental** backend instead brings up a
Ray cluster and launches `vllm serve … --distributed-executor-backend ray
--tensor-parallel-size <TP> --pipeline-parallel-size <PP>`, giving genuine
tensor- **and** pipeline-parallel sharding. It is opt-in per catalog entry
(`sharding.engine: vllm`) and currently **alpha** — exactly one catalog entry uses it
(`mimo-7b-ray`, labeled `TEST`). Cross-machine TP is latency-bound and forced to
`--enforce-eager`; a crash can leave stale Ray actors, so the coordinator launches
without auto-restart. Not recommended for production until it graduates out of `TEST`.
See `createShardedVLLMRay` / `isVLLMRayBackend` in `internal/scheduler/sharding.go`.
(Note: single-machine, multi-GPU vLLM tensor parallelism — the worker auto-picking
`--tensor-parallel-size` from the local GPU count — is separate and stable; it's
intra-node, not cross-machine sharding.)

---

## Control plane internals

```
                       ┌──────────────────────────────────┐
                       │           HTTP Server             │
                       │   (chi router — no UI, ADR-022)   │
                       └──────────┬───────────────────────┘
                                  │
       ┌────────────┬─────────────┼────────────┐
       ▼            ▼             ▼            ▼
   ┌────────┐  ┌─────────┐  ┌──────────┐  ┌─────────┐
   │  API   │  │  Admin  │  │   Auth   │  │ Metrics │
   │adapter │  │  API    │  │ (keys,   │  │         │
   │ OpenAI │  │         │  │  HMAC)   │  │         │
   └───┬────┘  └────┬────┘  └──────────┘  └─────────┘
       │            │
       ▼            ▼
   ┌──────────────────────┐
   │       Router         │  ── picks a node + protocol for a request
   └────────┬─────────────┘
            │
            ▼
   ┌──────────────────────┐      ┌───────────────────┐
   │  Scheduler           │◄────►│  Node registry    │
   │  (sharding, GGUF)    │      │  (capabilities)   │
   └────────┬─────────────┘      └───────────────────┘
            │
            ▼
   ┌──────────────────────┐      ┌───────────────────┐
   │  Model registry      │◄────►│  Model puller     │
   │  (catalog + state)   │      │  (HF, Ollama,     │
   └──────────────────────┘      │   local file)     │
                                 └───────────────────┘

   All state above lives in SQLite via the `store` package.
   Eventing between leader and agents is direct HTTP (heartbeats POST to
   the leader's admin API). `internal/events` is an in-process pub/sub
   leaves the leader process.
```

### Subsystem responsibilities

- **HTTP server** — request routing, TLS termination, middleware stack
- **API adapter** — translates OpenAI requests to the internal `InferenceRequest`; translates responses back
- **Admin API** — node management, model management, token issuance, usage queries
- **Auth** — API key validation (scope-gated routes), token issuance, HMAC verification for worker traffic
- **Router** — given a request, pick a target node + engine endpoint
- **Scheduler** — sharded-model orchestration, single-node `llama-server` bootstrap, GGUF download + distribution
- **Node registry** — current cluster state, heartbeat tracking
- **Model registry** — what models exist (catalog), where they live (placement), what state they're in
- **Model puller** — download GGUFs from HuggingFace, delegate `ollama:` sources to the engine's own pull, use `file:` sources as-is

### Implemented examples (the pattern in production)

As of 2026-06-05 the onboarding-and-sharing endpoints follow this pattern strictly — use them as references when writing new ones:

| CLI command | `internal/control/` function | Admin endpoint (in `internal/controlplane/`) |
|---|---|---|
| `opod connect <client>` | `control.ConnectSnippet()` + `control.Clients()` | `POST /admin/v1/connect/snippet`, `GET /admin/v1/connect/clients` (in `admin_connect.go`) |
| `opod disconnect <client>` | `control.DisconnectSnippet()` | (no HTTP endpoint — purely local string lookup; the reversal text is static per client) |

`internal/control/snippets/*.tmpl` are `go:embed`-ed templates — adding a new supported client is a one-file change. Existing CLI/admin pairs (model add, token create, node drain, etc.) still duplicate logic and will move into `internal/control/` as part of the rest of M4-T20.

---

## Agent internals

```
   ┌────────────────────────────────────┐
   │            Agent loop              │
   │   (one goroutine per concern)      │
   └────┬───────┬────────┬─────────┬────┘
        │       │        │         │
        ▼       ▼        ▼         ▼
   ┌────────┐┌────────┐┌────────┐┌──────────┐
   │Heart-  ││Capa-   ││Engine  ││Process   │
   │beat    ││bility  ││driver  ││supervisor│
   │loop    ││report  ││(health,││(rpc-srv, │
   │        ││        ││ loaded ││ llama-   │
   │        ││        ││ models)││ server)  │
   └────────┘└────────┘└────────┘└──────────┘
        │       │        │         │
        ▼       ▼        ▼         ▼
   ┌──────────────────────────────────────┐
   │   HTTP to leader (register/heartbeat)│
   │   + agent.Server (leader → worker)   │
   └──────────────────────────────────────┘
```

There is no message-bus subscription — everything is direct HTTP (`internal/agent/loop.go`). The agent POSTs to `/admin/v1/nodes/register` at startup (capabilities + address) and to `/admin/v1/nodes/heartbeat` every 5s, carrying the engine's `loaded_models`. On heartbeat failure it backs off exponentially (up to 1 minute), re-registers on 404, and exits on 401/403 (revoked token). Inbound work — proxied inference, process start/stop for sharding — arrives via the worker's own HTTP server (`agent.Server`).

### Capability detection

- macOS: `system_profiler SPHardwareDataType -json`, `sysctl hw.memsize`
- Linux + NVIDIA: `nvidia-smi --query-gpu=…`, `/proc/meminfo`, `/proc/cpuinfo`
- Linux + AMD/Intel: GPU count from `/dev/dri/renderD*` (generic render-node fallback — no vendor-specific probe yet; ROCm `rocm-smi` / oneAPI detection is planned)
- Generic: GOOS, GOARCH, hostname, kernel

Output: a `Capabilities{}` struct with RAM, GPUs (model, VRAM), CPU cores, OS, available engines.

---

## Mesh networking

Today Opod ships exactly one backend: **LAN** (`internal/mesh/mesh.go`). Each node reports a directly-routable `host:port` (the IP of its default outbound interface), and the leader talks to it over plain HTTP. This assumes a single trusted network — or Tailscale/WireGuard running at the host level, outside Opod.

The `Backend` interface (`Name()`, `Address(port)`, `Hostname()`, `Close()`) is the seam for future overlays.

### tsnet (planned — not implemented)

Embedding Tailscale's `tsnet` library so each Opod process is itself a tailnet node is on the roadmap. Why it's attractive:

- NAT traversal works without firewall config
- WireGuard noise protocol = mTLS-equivalent
- Discovery by name (`<node>.<tailnet>.ts.net`)
- Stable IPs across network changes
- Works across NATs, VPNs, Wi-Fi, LTE
- One Go import

The planned boot sequence: the leader creates/reuses a tailnet and generates an auth key; `opod join` passes the key to `tsnet` and dials `leader.<tailnet>.ts.net`; tsnet exposes a `net.Listener` and `Dial(ctx, addr)` that everything sits on top of. None of this exists yet — `go.mod` has no tailscale dependency.

### Alternative backends (planned — not implemented)

Other backends that could implement the same `internal/mesh` interface:

- `tailscale` — embedded tsnet (see above)
- `netbird` — for orgs already on NetBird
- `headscale` — self-hosted Tailscale control server (for air-gapped)

Only `lan` ships today; the backend is not yet configurable.

---

## Storage

### SQLite (default)

- File at `~/.opod/state.db`
- WAL mode for concurrent reads with one writer (set via DSN pragma)
- Driver: `modernc.org/sqlite` — pure Go, no CGO, so cross-compilation stays trivial
- Plain `database/sql` with hand-written queries; no ORM, no sqlx
- Schema is created in code: the DDL lives as a string in `internal/store/sqlite.go` and is applied at `OpenSQLite()` time, followed by idempotent inline column migrations (each checks `PRAGMA table_info` before `ALTER TABLE`) — there is no separate migrations directory

### Tables

```
nodes               (id, hostname, os, arch, ram_gb, address, worker_token, hardware_json, last_heartbeat, state, …)
models              (id, catalog_id, source, status, size_bytes, installed_at)
model_placements    (node_id, model_id, status, last_seen)
desired_placements  (node_id, model_id, priority, pinned, created_at)
shards              (id, model_id, role, node_id, address, process_id, status, …)
api_keys            (id, hash, name, scope, user_id, quota_daily_tokens, rpm_limit, tpm_limit, allowed_models, expires_at, revoked, …)
cache               (key, namespace, value, expires_at)
audit_log           (id, ts, actor, action, target, metadata_json)
```

### Postgres (planned — not implemented)

Same schema, swap the driver. `internal/store` already exposes a `Store` interface that a `postgres.go` backend would implement; only the SQLite backend exists today.

### Model files

Not in SQLite. GGUFs downloaded for sharding land in `storage.models_dir` as `<models_dir>/<source.file>`; Ollama-sourced models live wherever Ollama keeps them. The placements table records which nodes can serve which model.

Every Hub pull goes through `internal/fetch`, one step per file: an exclusive lock file, a temp sibling renamed into place once the byte count matches, the sha256 checked when one is known, and a `<file>.opod` marker so `opod cache prune` can reclaim it (and nothing it did not write). A pinned revision lives in `<models_dir>/<repo-slug>@<rev>/`, so two revisions of one repo coexist. `opod cache ls|prune` walk those directories under the same marker and lock rules: a pinned GGUF is an entry of its own (`<repo-slug>@<rev>/<file>`), while a snapshot directory (below) is ONE entry — listed with its total size, removed only under the lock of every file in it with the manifest unlinked first, and left whole if any file in it lacks our marker, is locked by a pull, was used inside `--min-age`, or is named by the keep list (by directory, or by the path of a file inside it). The `--json` shape only grew: `revision`, `snapshot`, `files`.

A GGUF is one file (`opod fetch <repo> <file>`). A safetensors model is a directory, so `opod fetch --snapshot <repo>[@rev]` (feature `fetch_snapshot`) asks the Hub what the repo holds at that revision — one listing call that also carries each file's size and, for LFS files, its sha256 — and makes present what an engine loads: top-level files only (subdirectories such as `original/` or `onnx/` are the model again in another runtime's format), one weight format (safetensors when there is any, PyTorch `.bin`/`.pt` otherwise, never both), `consolidated*.safetensors` dropped when `model*.safetensors` exists, plus configs, tokenizer files, chat templates and the repo's own `.py` modelling code. Each file takes the same step as a GGUF, verified against the digest the Hub declares; an unpinned snapshot goes to `…@main`, never to the top of the cache. `.opod-snapshot.json` (repo, revision, the commit it resolved to, every file with size and digest) is written last, and only a directory whose manifest checks out file by file counts: a vLLM or SGLang worker that finds one for its model at its pinned revision (`OPOD_MODEL_REVISION`) is launched on that directory instead of the repo name, so nothing is pulled inside the launch path. No snapshot, an incomplete one, or one missing a reclaimed shard → the engine pulls for itself as before. When it does and the worker pins a revision, the engine is launched with its own `--revision <rev>` (vLLM and SGLang spell it the same), so a pin is honoured with or without a snapshot; a revision that is not a plain sha, tag or branch refuses the launch instead of serving the branch head.

---

## Protocol adapters

### OpenAI adapter (`internal/api/openai.go`)

- Parses `/v1/chat/completions` request into `InferenceRequest`
- Streams tokens back as SSE `data: {...}\n\n`

### Other protocol shapes

None. Anthropic Messages, audio and rerank adapters left core on 2026-09-07 (ADR-022 step 4); a shim in front of the gateway is the place for them.

### Internal request shape

```go
type InferenceRequest struct {
    Model        string
    Messages     []Message
    System       string
    Tools        []Tool
    Stream       bool
    MaxTokens    int
    Temperature  *float32
    TopP         *float32
    Stop         []string
    UserID       string
    SessionID    string  // for sticky routing
    // ...
}
```

LiteLLM is used as a reference for edge cases in protocol translation but we don't ship it; we hand-write the adapters in Go for control and zero-dep deployment.

---

## Router

Given an authenticated `InferenceRequest`, the router decides:

2. Is `model` `auto`? Apply heuristics:
   - Short prompt with code shape → coder pool
   - Long agentic context with tools → flagship pool
   - Vision input → vision pool
   - Embedding request → embedding pool
3. Otherwise look up `model` in the registry → get list of nodes serving it.
4. Apply scoring per candidate node:
   - Free queue slots (higher = better)
   - Sticky-session match by `SessionID` (huge bonus for KV reuse)
   - Recent latency (lower = better)
   - Network distance (same site = better)
5. Pick winner; open HTTP/SSE connection to its local engine.
6. Stream response back through gateway, accumulating token counts.

### Sticky sessions

- `SessionID` derived from `userID + first message hash` or explicit header `X-Session-Id`
- Bound to a node for `session_ttl_s` (default 600s)
- Soft binding: if the node is overloaded, router will move the session and absorb the cache miss

### Catalog fallback chain (failure-based retry)

A catalog entry can declare an ordered list of fallback model IDs:

```yaml
id: qwen3.6-27b
# … other fields …
fallback:
  - qwen3-14b      # try next if 27B can't serve
  - gpt-oss-20b    # last resort
```

The router uses this list **only on failure** — not for load-balancing or capacity tuning. When the primary model can't serve a request, the router transparently retries the next ID in the chain until one succeeds or the chain is exhausted.

**What counts as a failure**:

- Engine connection refused / timeout
- HTTP 5xx from the engine
- Streaming connection drops before any tokens are delivered
- Model-not-loaded errors when no node has it

**What does not count**:

- HTTP 4xx from the engine (bad request → client's problem)
- A token-by-token stream that disconnects mid-response (the client already has partial output)
- Validation failures upstream of engine dispatch

**Transparency**:

- The response back to the client carries the **originally requested** `model:` string. The client never learns a fallback was used.
- Each substitution is recorded in `audit_log` with `actor=router`, `action=fallback`, `details={"from":"qwen3.6-27b","to":"qwen3-14b","reason":"503"}`.
- The leader's stderr also logs each fallback hit for live observability.

**Why failure-based, not policy-based**:

**Implementation**: `internal/router/router.go` resolves `[primary, ...fallback]` from the catalog, then walks the chain in order on each retriable error (`Chat()` and `Embed()` both do this). The chain length is whatever the catalog YAML declares — keep it short (≤ 3 usually) so a single bad request can't cascade through your whole catalog. See `internal/router/router_fallback_test.go` for the test coverage.

---

## Scheduler

Runs on the leader. What ships today in `internal/scheduler` is the **sharding orchestrator** (`sharding.go`, described above), the single-node `llama-server` bootstrap (`llamacpp.go`), and GGUF download + distribution (`hf_download.go`, `gguf_distribute.go`). Workers install their own models via `opod model add`; the leader does not push placements.

The general placement/drain/replication scheduler below is **planned — not implemented**. Goals:

1. Every requested model is loaded on at least 1 node it fits on.
2. Highly-used models get replicas to handle load.
3. Drains complete without dropping requests.

### Placement algorithm (planned — not implemented)

```
for each requested model M, sorted by priority (size desc, requests desc):
  candidates = nodes whose free RAM/VRAM >= M.size + headroom
  if candidates empty:
    if M can be sharded → try llama.cpp RPC across N nodes
    else → mark M as unschedulable, alert
  else:
    pick candidate with most free capacity (binpack=false)
    or least free capacity (binpack=true)
    issue assignment to the worker over HTTP
```

### Drain algorithm (planned — not implemented)

```
mark node as draining (no new sessions routed to it)
for each model M on the node:
  if M has another replica → done
  else → schedule M on another node, wait for ready
wait drain_timeout_s for in-flight requests
remove node from registry
```

### Replication (planned — not implemented)

- `auto` — start with 1 replica; scheduler adds replicas when sustained queue depth > threshold for >5 min
- `always` — every model gets ≥2 replicas if hardware allows
- `never` — exactly 1 replica per model

### Live model move (planned — not implemented)

`opod model move <id> --from <node> --to <node>`: take a model that one worker serves and have another worker serve it instead, with no request failing and none waiting for a cold load. This section is the design and the reason it is **not built yet**: the sequence is sound, but three of the mechanisms it stands on do not exist today, and building it on the ones that do would produce a command that reports success and quietly does something else. No feature key is registered — a key in `contractFeatures()` means the mechanism ships.

**What "without a cold start" can honestly mean.** Weights are never transferred between engines; the target loads them itself (from its cache, the Hub, or the leader's GGUF fan-out). What a move can promise is *overlap*: the source keeps serving until the target is proven ready, so no request ever meets a model that is loading.

```
1. admit     the target is a live worker that can hold the model (below); the source holds it
2. load      POST /v1/model/load on the target — the source keeps serving, untouched
3. ready     the target's placement row appears, written by its own heartbeat
4. flip      the router stops choosing the source for this model (new requests → target)
5. drain     in-flight requests on (source, model) run to completion (router.InflightByModel → 0)
6. unload    the source releases the model
```

Steps 2–3 are bounded by a timeout, and **a timeout leaves everything as it was**: the source was never touched, so it is still serving; the move reports the failure and the half-loaded target is the operator's to inspect. Nothing before step 4 is visible to a client. After step 4 the only irreversible act is step 6, and it happens only once step 5 has seen zero in-flight or its drain timeout has passed (the same `placement.drain_timeout_seconds` the local lifecycle uses, with the same warning).

**What it costs.** The model is resident twice for the overlap — once on each machine. The target must hold it *in addition to* what it already serves: admission uses the same facts as the shard-count picker (`scheduler.WorkerMemoryFacts`, the lifecycle's footprint estimate) and refuses, naming the numbers, when the target's free memory is below the model's footprint. The target also fetches the weights if it does not have them; that time is inside step 2's timeout, not in front of any request.

**What it is not.** No KV-cache transfer. Every conversation that was being served by the source loses its prefix cache: its next turn is recomputed from the prompt on the target (slower first token, same answer). The router's prefix-affinity and sticky pins for the source are dropped at step 4 for the same reason. It is not a way to move a sharded model (a gang is rebuilt with `opod shard create`), and it does not move a LoRA adapter on its own — adapters already load and unload live on the workers that hold the base (`/admin/v1/adapters`) and follow the base.

**Per engine**, because "load" and "unload" are different acts on each:

| Worker engine | Step 2 on the target | Step 3's ready signal | Step 6 on the source |
|---|---|---|---|
| vLLM, SGLang, llama.cpp | launches the engine process for this model. These workers serve **exactly one model**: `/v1/model/load` stops whatever is running first, so a target that already serves another model would drop it. The move must refuse such a target | honest: the heartbeat lists the model only once the server answers `/v1/models`, i.e. after the weights are in | stop the supervised engine process; the model leaves the heartbeat and the placement row goes with it |
| Ollama | pull + warm load, synchronous; other models stay resident | weaker: the heartbeat lists *installed* models, so the row appears once pulled. The load call's own success is the signal that it is warm | an engine unload frees the memory, but the model stays installed, so the heartbeat keeps listing it and the row stays. "Moved" needs either the weights deleted from the source or a standing exclusion of (source, model) |

**What is missing today** — each is a small, additive mechanism; together they are the work:

1. **A worker-side unload.** Workers expose `/v1/model/load`, `/sleep` and `/resume`; there is no `/v1/model/unload`. The leader's `POST /admin/v1/models/{id}/unload` acts on the leader's own engine only. Needed: `POST /v1/model/unload {id}` on the worker — engine unload where the driver supports it, a supervisor stop of the engine process where the worker launched one (`vllm-serve`, `sglang-serve`, `llama-server`) — refused while a shard part of that model runs there.
2. **A flip that survives a heartbeat — built (feature `placement_drain`).** A placement whose status is `draining` is invisible to `Placements().GetByModel`, which is how the local lifecycle drains before an eviction; on a worker the mark used to last one heartbeat, because every heartbeat rewrote the node's rows as `ready`. `ReplaceForNode` now carries the mark: inside its one transaction a row that was `draining` stays `draining` while the model is still in the worker's report, and goes with the row when the model leaves it. The heartbeat body is unchanged — a worker reports residency, never routability. The mark is set and lifted with `Placements().SetStatus` (no admin route sets it yet: the move is its first caller), and since it now lives in the store rather than in one heartbeat interval, a leader start lifts every mark a previous process left (`liftStaleDrains`) — a restart abandons the move and the source, never unloaded, is simply serving again.
3. **The worker's engine, known to the leader.** Registration carries hardware, not the engine. Without it the leader cannot tell a one-model worker from an Ollama one, so it can neither refuse the target that would lose its model (row 1 of the table) nor choose the right step 6. Until it is registered, the only safe rule is the blunt one: refuse any target that has a placement for another model.
4. **An answer for Ollama sources** (last cell of the table): delete the weights on the source, or keep the exclusion as durable state. Deleting is what "move" says; it is also the only one of the two that survives a leader restart without new state. It needs a worker-side delete, which does not exist either.

**Shape when built.** The sequence is leader-driven and lives beside `PlaceOnNodes` in `internal/scheduler`; the admin route is `POST /admin/v1/models/{id}/move {from, to}` with feature key `model_move` (additive; `TestLeaderContract` extended); the CLI is `opod model move <id> --from <node> --to <node>`, printing each step as it completes. The proofs it lands with: a router test in which requests run continuously through the move and none fails, and after the flip none is routed to the source; a test that a target which never becomes ready leaves the source serving and its row `ready`; a test that a heartbeat from the source during the drain does not make it routable again; a refusal test per admission rule (target too small, target serving another model, source not holding the model, either node not live, a sharded model).

---

## Engine drivers

`internal/engines` is the **contract package**: the `Engine` interface, request/stream types, error
classes (`ErrUnreachable` · `ErrUpstream` · `ErrUnloadNotSupported`), shared tracing helpers, and the
**registry**. Each driver is its **own package** under `internal/engines/<name>/` that calls
`engines.Register(engines.Descriptor{...})` from `init` — name, aliases, factory, native-name rule,
start hint. `internal/engines/all` blank-imports every driver and is what `cmd/opod` links; a slim
build imports only the drivers it ships (the `database/sql` driver model). Nothing outside those
packages names a concrete driver type: construct with `engines.New(name, endpoint)` /
`engines.NewWithAuth(...)`, resolve names with `engines.NativeName` / `engines.CatalogID`.
OpenAI-wire engines (vLLM, MLX-LM, llama-server, Tenstorrent) embed `engines/openaicompat.Client`
and add only what differs. The interface (from `internal/engines/types.go`):

```go
type Engine interface {
    Name() string
    Endpoint() string
    Health(ctx context.Context) error

    List(ctx context.Context) ([]string, error)
    Pull(ctx context.Context, modelID string, onProgress func(status string, completed, total int64)) error
    Delete(ctx context.Context, modelID string) error

    // Unload drops the model from memory without uninstalling weights.
    // Engines that can't return ErrUnloadNotSupported.
    Unload(ctx context.Context, modelID string) error

    Chat(ctx context.Context, req ChatRequest) (<-chan StreamEvent, error)
}
```

### Implemented drivers

- **Ollama** — easiest dev backend. Driver shells out to `ollama` CLI for pulls; talks to its HTTP API.
- **vLLM** — for NVIDIA. Driver runs the official Docker image or local install with the right `--model`, `--tensor-parallel-size`, `--max-model-len` flags.
- **MLX-LM** — for Apple Silicon. Driver runs `mlx_lm.server` in a managed subprocess.
- **llama.cpp** — universal fallback. Driver runs `llama-server` with the right `-m`, `-c`, `--rpc` flags.

### Adding a new engine

1. New package `internal/engines/<name>/` implementing `Engine` (embed `openaicompat.Client` if the wire is OpenAI).
2. `engines.Register(engines.Descriptor{Name, Aliases, New, NativeName, StartHint})` in its `init`.
3. One line in `internal/engines/all/all.go`.
4. `conformance_test.go` calling `enginetest.Run` with a fake upstream — the suite checks identity,
   Health/List/Pull, Chat streaming, Embed/Unload/Resident/Loader contracts, error classes, native names.
   Embeddings are implemented once, on the OpenAI-compatible client every wire-compatible driver embeds,
   so llama.cpp, vLLM and MLX gained them together rather than one at a time; the suite checks that a
   driver returns vectors in the caller's order, since a server may answer out of order and only the
   index says where a vector belongs.
5. Capability matching (when does the scheduler pick you?) — TARGET, lives with the vendor registry.

---

## Model registry and puller

### Catalog

YAML files in the SDK's `catalog/<id>.yaml` (`opod-io/opod-sdk/catalog`, embedded in the binary since R9.8 — `opod catalog ls|export`):

```yaml
id: qwen3-coder
display_name: Qwen3 Coder 30B-A3B
source:
  type: huggingface
  repo: Qwen/Qwen3-Coder-30B-A3B-Instruct-AWQ
size_bytes: 21474836480
quant: awq
context_window: 262144
capabilities: [chat, tools, code]
recommended_engines: [vllm, mlx]
hardware:
  min_vram_gb: 22
  min_ram_gb: 32
tags: [coding, agent]
```

Loaded into the model registry at startup. Users add via `opod model add qwen3-coder`.

**Where entries come from** (`models.LoadCatalog` → `resolveCatalogDirs`). The catalog embedded in the binary is the base; every override directory that exists is merged over it by id, a later source replacing an earlier one's entry whole: embedded → `/usr/local/share/opod/catalog` → `/usr/share/opod/catalog` → `$OPOD_CATALOG_DIR` → `~/.opod/catalog`. Nothing else is searched — `./catalog` and a `catalog/` beside the executable were candidates only while the bundled set was a directory on disk. `catalog_dir` in `config.yaml` is not part of the merge: a non-empty value reads that one directory alone, without the embedded catalog.

### Signed catalog files

A catalog entry decides which weights are pulled and how an engine is launched, so a directory an operator does not fully control is worth a signature. The scheme is [minisign](https://jedisct1.github.io/minisign/): `minisign -S -m my-model.yaml` writes `my-model.yaml.minisig` beside the file, and the reader (`internal/models/trust.go`, the `github.com/jedisct1/go-minisign` verifier) checks it against **one** public key, `OPOD_CATALOG_PUBKEY` — the base64 line itself, or a file holding it (`minisign.pub`). Both knobs are rows of the environment contract (`config.Env`, side `both`): a worker does not read the catalog to serve — the leader sends it a model's source fields — but the CLI on a worker machine does.

| Configured | A file with a signature | A file without one |
|---|---|---|
| no key | loaded — the signature is not read | loaded |
| `OPOD_CATALOG_PUBKEY` | must verify, **or the load is refused** | loaded |
| + `OPOD_CATALOG_REQUIRE_SIGNED=1` | must verify, or the load is refused | **refused** |

- **A signature that is present and wrong is always a refusal**, never "warn and load": it means the file changed after it was signed or another key signed it, and neither is something to serve from. `OPOD_CATALOG_REQUIRE_SIGNED=1` with no key is a configuration error, refused the same way.
- **Where it runs.** At every catalog load — `opod up` and every CLI command that reads the catalog go through one function (`loadCatalog` → `models.LoadCatalogTrusted`), for every directory in the merge: the share directories, `$OPOD_CATALOG_DIR`, `~/.opod/catalog`, and an explicit `catalog_dir`. A refusal fails the load and names the file, exactly as a YAML that does not parse does; the file is checked before it is decoded. And in `opod model add --from <file>`, against `<file>.minisig`, before anything is planned, saved or pulled; the signature is saved beside the copy in the user catalog (`<id>.yaml.minisig`) so the copy verifies the way the original did. `opod doctor` reports a refusal as what it is and says which policy is in force.
- **The embedded catalog is not subject to it.** It ships inside the binary; whoever trusts the binary trusts it. A directory file that is byte-for-byte the embedded entry of the same name — what `opod catalog export` writes, and what the worker image's entrypoint exports at start — *is* that entry and is not checked. Edit it, or keep an export from an older binary, and it is a directory file like any other.
- **What it is not.** Without `OPOD_CATALOG_REQUIRE_SIGNED=1` this is an integrity check on the files that carry a signature, not a boundary: whoever can write the directory can also delete the `.minisig`. The switch is the boundary. The scheme ids (`hf:`, `ollama:`, `file:`) involve no catalog file and are untouched by it. One key, no rotation set: to rotate, re-sign the files and change the variable together.

### Puller

Three source types (`internal/models/catalog.go`): `ollama`, `huggingface`, `file`. CLI shorthand: `hf:owner/repo[:file.gguf]`, `ollama:name[:tag]`, `file:/abs/path/x.gguf`. There is no `https://`, `s3://`, or `minio://` support.

- `huggingface` sources (GGUFs for sharding) are a single streaming GET from `huggingface.co/<repo>/resolve/main/<file>` into `storage.models_dir`, written to a `.partial` file and renamed on success; skipped if already present (`internal/scheduler/hf_download.go`). Resume of interrupted transfers is planned — not implemented; today an interrupted download starts over.
- `file` sources are used in place — Opod just verifies the path exists
- Shard fan-out copies the leader's GGUF to workers via `/v1/process/file` + `/v1/process/upload`, sha256-verified and skipped when the worker already has the file

---

## Authentication and authorization

### API keys

- Format: `sk-orc-` + 24 random bytes, base64url-encoded (`internal/auth/keys.go`)
- Stored as sha256 hashes — only the hex digest is persisted, never the plaintext
- Scopes: `user`, `admin`, `node` — exactly one scope per key
- Per-key controls: daily token quota, RPM/TPM rate limits, model allowlist, optional expiry (`--ttl` / `--expires-at`)
- Revocable at any time

### Key scopes

| Scope | Purpose | TTL |
|---|---|---|
| `user` | Inference keys for `/v1/...` | until revoked or expired |
| `admin` | Cluster admin operations (`/admin/v1/...`) | until revoked or expired |
| `node` | Worker join + register/heartbeat | until revoked or expired |

### Bootstrap and worker auth

- **Admin key bootstrap**: the first `opod up` generates an `admin`-scope key, prints it once to the operator, and saves the plaintext to `~/.opod/admin.key` (mode 0600) so subsequent local CLI calls work without copy-paste.
- **Worker auth**: leader ↔ worker requests are signed with HMAC-SHA256 over `v1\n<METHOD>\n<PATH>\n<ts>` keyed by the per-node worker token (`internal/auth/hmac.go`), with constant-time comparison and a ±5-minute replay window. Bearer fallback is accepted for one transition release.

### Authorization model

- Scope checks via middleware (`auth.RequireScope` / `RequireScopeAny` in `internal/auth/keys.go`) — no role system
- Per-key model allowlist (optional): requests for non-listed models get HTTP 403 `model_not_allowed`

---

## Observability

### Metrics

Declared in `internal/metrics/metrics.go`. Exposed at `/metrics` on the main listener (default `:8080`) — there is no separate metrics port. The endpoint is unauthenticated by design so Prometheus can scrape without a key.

Key series:

- `opod_requests_total{model,protocol,outcome}` — counter
- `opod_request_duration_seconds{model,protocol,outcome}` — histogram
- `opod_request_tokens_total{model,direction}` — counter
- `opod_model_loaded{model,node}` — gauge
- `opod_node_up{node,hostname}` — gauge

Router subsystem (added in the observability pass):

- `opod_router_picks_total{path,outcome}` — counter. `path` is one of `local|worker|shard|fallback-to-local`; `outcome` is `ok|error|store-error|no-workers|stale-heartbeat|all-workers-stale`.
- `opod_router_inflight{node}` — gauge. Mirrors the router's per-node in-flight request count.
- `opod_router_fallback_total{op,reason}` — counter. `op` is `chat|embed`; `reason` is `primary-error|latency-reorder|cap-exhausted`.
- `opod_router_attempt_duration_seconds{model,outcome}` — histogram.

### Traces

OpenTelemetry/OTLP-HTTP. Set `observability.otlp_endpoint` (or `OPOD_OTLP_ENDPOINT`) to a collector URL — empty disables tracing with zero overhead (NoopTracerProvider).

Span hierarchy today:

```
http.request                                         (otelhttp on the chi router)
└── router.Chat                                      (request entry; covers full stream)
    ├── router.Chat.attempt (i=0, primary model)
    │   └── ollama.Chat                              (engine driver; covers stream)
    ├── router.Chat.attempt (i=1, fallback model)   (only on retriable failure)
    │   └── ollama.Chat
    └── … further attempts as catalog `fallback:` chain dictates

http.request
└── router.Embed
    └── router.Embed.attempt (i=0)
        └── ollama.Embed                             (when /v1/embeddings)
```

Per-span attributes:
- `router.Chat` / `router.Embed`: `opod.model.requested`, `opod.fallback.chain_length`, `opod.fallback.used_at` (if fallback fired), `opod.model.served`, `opod.stream.events`
- `router.Chat.attempt` / `router.Embed.attempt`: `opod.attempt`, `opod.model.candidate`, `opod.is_fallback`, `opod.engine`, `opod.node_id`
- `ollama.Chat`: `opod.engine`, `opod.model`, `opod.engine.endpoint`, `opod.messages`, `http.status_code`, `opod.tokens.prompt`, `opod.tokens.completion`

vLLM / MLX / llamacpp drivers all carry the same `<driver>.Chat` span shape via the shared `engines.StartChatSpan()` helper in `internal/engines/tracing.go`. W3C `traceparent` propagation is always on (even when export is disabled), so Opod participates correctly when sandwiched between two services that both export upstream.

### Logs

`slog` to stderr in JSON. Levels: debug, info, warn, error. Request IDs propagated through context.

**OTLP export (feature `otlp_logs`).** With `OPOD_OTLP_LOGS_ENDPOINT` set (a URL or bare `host:port`, the same forms as the trace endpoint) the leader also sends its own records to a collector over OTLP/HTTP, on the upstream SDK (`otel/sdk/log` + `otlploghttp`, bridged from `slog` by `contrib/bridges/otelslog`) — `internal/controlplane/logexport.go`. It is a tee, not a redirect: stderr keeps every record it had. `log_level` governs both sides, so a collector never receives what the operator did not ask for. Records carry the same `service.name=opod` / `service.version` resource as the spans. The batch queue holds 2,048 records and emit never waits on the network: a slow or absent collector costs the oldest queued records, never a request, and a bad endpoint is a startup warning rather than a failure. Only the leader process's own records are exported — what an engine writes to its stdout belongs to whatever collects the host's or the pod's logs.

### Dashboards

- `cluster-overview.json` — total RPS, p50/p95/p99 latency, error rate, tokens/s (prompt vs completion), nodes up, loaded models inventory
- `per-model.json` — same questions filtered to one model (Grafana template variable picks the model)
- `per-node.json` — per-node fleet view: nodes up, models loaded per node, full inventory

All three bind to whichever Prometheus data source you pick at import time via the `${DS_PROMETHEUS}` variable. No edits required. Schema matches what `internal/metrics/metrics.go` actually emits — keep them in sync when you add a metric.

---

## Security model

### Network

- Mesh = plain HTTP on a trusted LAN today. Inter-node requests are authenticated (HMAC-signed) but not encrypted by Opod — run Tailscale/WireGuard at the host level, or keep the fleet on one trusted network. Embedded tsnet (WireGuard in-process) is planned — not implemented.
- The gateway serves plain HTTP; there is no built-in TLS termination. Put a reverse proxy (Caddy, nginx) in front if you expose it beyond the LAN.
- Workers must be reachable from the leader on their reported `host:port`.

### Auth

- Per-user API keys, revocable.
- Every caller — a person at the CLI, a client tool, an external manager — authenticates with an API key (no OIDC in core — see § Authentication and authorization).
- Admin keys are separate from user keys, never sent to workers.

### Data

- Request bodies not persisted by default — only metadata (user, model, tokens, latency).
- Opt-in full-payload logging for debugging.
- External-API fallback uses user-scoped provider keys.

### Threat model

| Threat | Mitigation |
|---|---|
| Compromised worker reads other workers' state | Workers have no admin scope; leader↔worker requests are HMAC-signed per node |
| Leaked user key | One-click revoke; quota caps blast radius |
| Mesh traffic sniffed on host network | Out of scope today (trusted-LAN assumption) — run WireGuard/Tailscale at the host level; embedded tsnet is planned |
| Compromised leader | Treat leader as trust root; rotate admin keys periodically |
| Jailbroken local model | Optional gateway-level moderation hook |
| Supply chain (downloaded weights) | SHA256 verification against catalog or HF |
| Supply chain (a catalog file that names the weights) | minisign signatures on directory catalog files, required with `OPOD_CATALOG_REQUIRE_SIGNED=1` ("Signed catalog files"); the embedded catalog ships in the binary |

### Reporting vulnerabilities

`hadi.work.ca@gmail.com`, or (preferred) a private GitHub Security Advisory — see `SECURITY.md`. 90-day disclosure.

---

## Why each technology was chosen

| Choice | Alternatives considered | Why we picked this |
|---|---|---|
| Go | Rust, Python | Single binary, fast enough, big ecosystem for networking |
| LAN mesh (`tsnet` planned) | libp2p, raw WireGuard, custom | LAN + plain HTTP needs zero deps and works today; tsnet would add NAT traversal + mTLS + discovery in one import when it lands |
| SQLite via `modernc.org/sqlite` (default) | Postgres, etcd, mattn/go-sqlite3 | Embedded, file-backed, no operator; pure Go (no CGO) keeps cross-compilation trivial |
| vLLM / MLX / llama.cpp | Build our own engine | Years of perf work; we'd never catch up |
| Hand-written adapters | LiteLLM as a library | LiteLLM is Python; we want one binary. We use it as a reference. |
| CLI + `/admin/v1`, no UI in the binary | An embedded dashboard (core had one until ADR-022) | One interface to keep true; a console is a product of its own and drives the same routes from outside |
| Chi router | gin, echo, stdlib | Minimal, idiomatic, well-typed |
| Apache 2.0 | MIT, AGPL | Permissive enough for enterprise adoption; patent grant included |

---

## Concurrency model

- The leader has a small fixed set of goroutines: HTTP server, cache reaper, desired-placement restore (startup), and per-process supervisor watchers (sharding). Workers add the heartbeat loop.
- Each in-flight request spawns one goroutine in the gateway and one streaming connection to the worker.
- Locks are scoped to single subsystems. There is no global lock.
- All shared state is in SQLite (durable) or in-memory maps protected by per-key locks (caches).

Rules of thumb:

- Pass `context.Context` first arg, always
- Never store contexts in structs
- Use channels at boundaries between subsystems; mutexes inside one subsystem
- Avoid `sync.Map` unless profiling shows contention on `map + Mutex`
- Don't spawn goroutines without bounded lifetime — every `go` must respect a context

---

## Project layout

```
opod/
├── README.md                  # user docs
├── QUICKSTART.md              # 3-min new user landing page
├── ARCHITECTURE.md            # this file
├── ROADMAP.md                 # what ships, what is next, what is out of scope
├── ROADMAP.md                 # scope decisions (incl. explicitly-killed features)
├── LICENSE                    # Apache 2.0
├── SECURITY.md
├── CODE_OF_CONDUCT.md
├── CONTRIBUTING.md            # short pointer to this doc
├── Makefile                   # dev shortcuts
├── go.mod, go.sum
│
├── .github/workflows/         # CI + Release workflows
├── .goreleaser.yaml           # release config
├── .golangci.yml              # lint config
│
├── cmd/opod/                 # single-binary entrypoint + every subcommand
│   ├── main.go                # dispatch + top-level help
│   ├── help.go                # helpSpec / showHelp / dieHelp / wantsHelp
│   ├── common.go              # adminCall + readLocalAdminKey + shared helpers
│   ├── cmd_{up,down,status,join,doctor,version}.go
│   ├── cmd_{node,model,shard,token,usage,audit,config}.go
│   └── …
│
├── internal/
│   ├── controlplane/          # leader HTTP server + admin API + middlewares
│   ├── agent/                 # capability detect + heartbeat loop + worker HTTP + process supervisor
│   ├── router/                # model → node dispatch, least-loaded, shard coordinator
│   ├── scheduler/             # sharding orchestrator + llama-server bootstrap + GGUF download/distribute
│   ├── mesh/                  # mesh.go — LAN backend (tsnet planned)
│   ├── engines/               # contract (types, errors, registry) + ollama/ vllm/ mlx/ llamacpp/ driver pkgs + openaicompat/ + all/ + enginetest/
│   ├── models/                # catalog parser (incl. ShardingSpec), auto-pick
│   ├── store/                 # SQLite backend (api_keys / models / nodes / placements / shards / usage / audit)
│   ├── auth/                  # API keys (sha256) + scope middleware + HMAC worker auth
│   ├── control/               # mutating ops shared by CLI + admin API
│   ├── cache/                 # response cache (memory + SQLite drivers)
│   ├── lifecycle/             # local-engine memory lifecycle (load/evict/pin)
│   ├── config/                # YAML + env loader
│   └── metrics/               # Prometheus declarations
│
├── (catalog lives in opod-io/opod-sdk/catalog — embedded, one copy for the leader and the control plane)
│   ├── llama-3.2-1b.yaml
│   ├── llama-3.2-3b.yaml
│   ├── llama-3.3-70b-sharded.yaml
│   ├── qwen-coder-{7b,14b,32b}.yaml
│   └── … (see opod-sdk/catalog/README.md)
│
└── installer/
    ├── install.sh             # the curl | sh script
    └── homebrew/opod.rb      # tap formula template (publishing disabled until tap repo exists)
```

### Naming conventions

- Packages: short, lowercase, no underscores (`controlplane`, not `control_plane`)
- Files: snake_case (`llamacpp_rpc.go`)
- Tests: same file with `_test.go` suffix
- Exported types: PascalCase, exported funcs: PascalCase
- Errors: `errFoo` for sentinel, `ErrFoo` if exported
- Context is always the first arg; never store contexts in structs

---

## Coding conventions

- **Error handling**: wrap with `fmt.Errorf("operation: %w", err)`. Never swallow.
- **Logging**: `slog` only. Levels: debug (verbose), info (user-relevant), warn (degraded), error (request failed).
- **Tests**: table-driven where it fits. No mocks for stdlib. Use real SQLite (in-memory or temp file) and `httptest.Server` for HTTP.
- **HTTP**: handlers are thin; logic lives in services. Handlers do parse → call → respond.
- **Concurrency**: prefer channels at boundaries; use mutexes for small protected state.
- **No `init()` functions** except for package-level registry registration.
- **No global mutable state** beyond metrics and the driver registries.
- **Generics**: only where a type-safe alternative is impossible.
- **File length**: aim under 600 lines; split at 800.

---

## Build from source

### Prerequisites

- Go 1.25+
- Optional: NVIDIA Container Toolkit (for vLLM workers)

### Build

```bash
git clone https://github.com/opod-io/opod-core
cd opod-core

# Build the binary — one static executable, nothing else to bundle
go build -o opod ./cmd/opod

# Smoke test
./opod version
./opod doctor
./opod up
```

### Cross-compile

```bash
GOOS=linux   GOARCH=amd64 go build -o dist/opod-linux-amd64   ./cmd/opod
GOOS=linux   GOARCH=arm64 go build -o dist/opod-linux-arm64   ./cmd/opod
GOOS=darwin  GOARCH=arm64 go build -o dist/opod-darwin-arm64  ./cmd/opod
GOOS=darwin  GOARCH=amd64 go build -o dist/opod-darwin-amd64  ./cmd/opod
```

### Release

Tag-driven via GoReleaser:

```bash
git tag v0.x.y
git push --tags        # CI builds binaries, publishes checksums + tarballs to GH Releases
```

---

## Getting started as a contributor

### Your first 30 minutes

```bash
git clone https://github.com/opod-io/opod-core
cd opod-core
make check             # lint + test + build (this is what CI runs)
./opod up             # boots a single-node leader against local Ollama
```

You only need Go 1.25+ and a working Ollama install (`brew install --cask ollama` on macOS, or `curl -fsSL https://ollama.com/install.sh | sh` on Linux). No Docker, no Python, no Node.

The first `opod up` will:

1. Bootstrap `~/.opod/state.db` (SQLite)
2. Print an admin API key to stderr — copy it; it's shown only once
3. Auto-pick a model based on hardware (`opod model search` for the list)
4. Start serving on `http://localhost:8080` (OpenAI API + `/admin/v1`)

From there: edit code → `make build` → restart `./opod up` → done.

### Make targets

The Makefile is intentionally tiny — every target maps to a single `go` invocation. Run any of these from the repo root:

| Target | What it runs |
|---|---|
| `make build` (default) | `go build -trimpath -o opod ./cmd/opod` |
| `make test` | `go test ./...` |
| `make lint` | `go vet ./...` (plus `.golangci.yml` rules when you run `golangci-lint` separately) |
| `make check` | lint + test + build, in that order. **This is what every PR must pass.** |
| `make run` | `make build && ./opod up` |
| `make tidy` | `go mod tidy` |
| `make clean` | remove the `opod` binary and `data/`, `.opod/` working dirs |

There is no `make dev` or `make test-e2e` — a hot-reload-free Go binary with no front end needs neither. End-to-end tests run inline as `go test ./...` (look for `_test.go` files that spin up an `httptest.Server`).

### Finding your way around

Start with these files in order. Each top-of-file comment explains what the package owns; no file exceeds ~600 lines.

1. `cmd/opod/main.go` — switch statement over subcommand verbs
2. `cmd/opod/cmd_*.go` — one file per CLI subcommand (no file over 400 lines); each parses flags, calls a package and prints: model install/search → `internal/models` (`Install`, `Search`, `PersistUserCatalogEntry`), boot steps → `internal/control/bootstrap.go`, self-update → `internal/update`
3. `internal/controlplane/server.go` — leader HTTP server (chi router); wires data-plane + admin routes
4. `internal/api/openai.go` — OpenAI protocol adapter (`/v1/chat/completions`, `/v1/models`, `/v1/embeddings`)
7. `internal/control/control.go` — every mutating operation in one place; both CLI and admin HTTP call into here (the load-bearing rule: the CLI and the admin API are two callers of one function)
8. `internal/router/router.go` — picks the backing engine per request (local → remote → fallback)
9. `internal/scheduler/sharding.go` — orchestrates sharded models (rpc-server + coordinator)
10. `internal/engines/types.go` — `Engine` interface; `registry.go` — `Register`/`New`/`NativeName`/`CatalogID`; `internal/engines/{ollama,vllm,mlx,llamacpp}/` are the drivers; `all/` links them
11. `internal/agent/loop.go` — worker register + heartbeat loop; `internal/agent/server.go` is the worker HTTP server
12. `internal/store/sqlite.go` — schema, migrations, query helpers

### Common contributor tasks

| Task | Touch these files |
|---|---|
| Add a new inference engine | `internal/engines/<name>/` (implement `Engine`, `engines.Register` in `init`), one line in `internal/engines/all`, `enginetest.Run` conformance test |
| Add a new model to the catalog | `opod-sdk/catalog/<id>.yaml` — see the SDK's catalog/README.md for the schema; bump the SDK and the go.mod require |
| Add a new CLI subcommand | `cmd/opod/cmd_<name>.go` + add a case in `cmd/opod/main.go` + add the mutating function in `internal/control/` first (CLI is the source of truth) |
| Add a new admin HTTP endpoint | `internal/controlplane/admin_<name>.go` — must delegate to `internal/control/` |
| Add a metric | declare in `internal/metrics/metrics.go`, increment at the relevant call site |
| Add a config field | extend the `Config` struct in `internal/config/config.go`, add a default in `Default()`, optionally read an env var in `applyEnv()`, document in [README.md → Full reference](README.md#full-reference) |

### Submitting a PR

1. Open a discussion or issue first if the change is non-trivial.
2. Branch from `main`: `feat/<short-name>` or `fix/<short-name>`.
3. One change per PR.
4. Update tests + docs in the same PR (no follow-up "I'll fix docs later" PRs).
5. `make check` must pass locally.
6. If the change adds or modifies a CLI surface, the README CLI reference must be updated.
7. If the change adds a config field, the README "Full reference" must include it.

### Communication

- **GitHub Discussions** — design questions, RFCs, "is this a bug?"
- **GitHub Issues** — confirmed bugs, concrete feature requests
- **Maintainer** — [Hadi Honarvar Nazari](https://www.linkedin.com/in/hadi-honarvar-nazari/) (`hadi.work.ca@gmail.com`)

---

## How to extend Opod

### Add a new inference engine

1. Read `internal/engines/mlx/mlx.go` (OpenAI wire, ~60 LOC on top of `openaicompat`) or `internal/engines/ollama/ollama.go` (own protocol) as the example.
2. Implement the `Engine` interface (`internal/engines/types.go`) in `internal/engines/<name>/`. The interface today: `Name()`, `Endpoint()`, `Health(ctx)`, `List(ctx)`, `Pull(ctx, …)`, `Delete(ctx, …)`, `Unload(ctx, …)`, `Chat(ctx, req)`. Optional capabilities (`Embed(ctx, req)`, and the `ResidentLister`/`Loader` interfaces) are implemented separately by drivers that support them. Wrap failures with `engines.Unreachable` / `engines.Upstream` so callers classify with `errors.Is`.
3. Register from `init` with `engines.Register(engines.Descriptor{...})` and add the package to `internal/engines/all`. `NativeName` in the descriptor is the one place that knows which catalog field your engine pulls by.
4. Add `conformance_test.go` calling `enginetest.Run` against a fake HTTP server — don't require a real GPU in CI. Every driver passes the same table.
5. Document any required system binaries in [README.md → Installation](README.md#installation) and add a "What ships" bullet in [README.md → What's shipped](README.md#whats-shipped).

### Add a new client protocol

1. Read `internal/api/openai.go` as the simplest example.
3. Wire the routes in `internal/controlplane/server.go` (look for `r.Post("/v1/chat/completions", …)` and follow the pattern).
4. Document in [README.md → Supported clients](README.md#supported-clients) and in [README.md → API reference](README.md#api-reference).

### Add a new mesh backend

E.g. swapping LAN for Tailscale tsnet:

1. Read `internal/mesh/mesh.go` — the `Backend` interface and the existing LAN implementation.
2. Create `internal/mesh/tailscale.go` (or similar) implementing the `Backend` interface.
3. Surface a `mesh.backend` field in `internal/config/config.go` and switch on it in the controlplane bootstrap.
4. Note the "Not yet configurable" disclaimer in the README will need updating.

### Add a new storage backend

1. Read `internal/store/sqlite.go` for the table layout and the `Store` interface.
2. Create `internal/store/<name>.go` implementing the same interface (e.g. `postgres.go` for HA).
3. Add a migration runner for the new backend (SQLite uses inline, idempotent column migrations applied at open time).
4. Add a `storage.type` switch where the store is opened (today `store.OpenSQLite` is called directly).

### Add a new model to the catalog

Add `catalog/<id>.yaml` in `opod-io/opod-sdk` (embedded into the binary; an operator can also drop a file in `~/.opod/catalog` or `OPOD_CATALOG_DIR` to add or override one at runtime). A dropped-in file can carry a minisign signature — see "Signed catalog files". See the SDK's catalog/README.md for the schema and required fields.

## Stable admin surface (v1)

`internal/controlplane/contract.go` freezes the routes an external manager may rely on
(probes incl. `/loadz`, the two gateway routes, the worker join/heartbeat pair, and the `/admin/v1`
manager routes: `version`, `capabilities`, `nodes`, `nodes/{id}/sleep|resume`, `models`,
`models/{id}/load`, `healthcheck`, `events/stream`, `usage/stream`, `shards` list/create/delete). The
list is additive-only: `GET /admin/v1/capabilities` serves it together with feature flags
(`events_stream usage_stream loadz shards plan_file auth_file router_only_ready vram_budget stream_boot
policy_file load_signals worker_sleep`), and `TestLeaderContract` walks the real router so a change that
drops one of these routes fails `go test`. Everything else under `/admin/v1` may change between releases.

The wire types of this surface — `Load`, the usage and lifecycle stream batches, the auth and policy
snapshot documents, `Route`/`Version`/`Capabilities` — live in the SDK module
`github.com/opod-io/opod-sdk/adminapi` (Apache-2.0, stdlib only). The leader marshals those exact structs,
so a manager imports the package instead of mirroring it; a field changed there is a field changed on the wire.

`capabilities` also lists the **engine drivers linked into the binary** (feature `engines`):
`engines: [{id, aliases, native}]` — the canonical name (`llamacpp`, `mlx`, `ollama`, `sglang`, `vllm`), the
other spellings the registry accepts for it, and the catalog source field the driver pulls and serves a
model by (`ollama_name`, `repo`, `path`, or `id`), reported by probing the driver's own `NativeName` rule
rather than restating it. A manager that offers engines by name checks its list against this one, so a
dropped or renamed driver is a failed comparison and not a worker that dies at launch. Engine ids and
aliases are additive-only like the routes; `TestLeaderContract` pins the published ones. The type is
core-local (`controlplane.EngineInfo`) until the next SDK tag carries it on `adminapi.Capabilities`.

Two mechanisms a manager drives through this surface (2026-09-07):

- **Load signals** (`load_signals`): a worker engine that implements `engines.LoadReporter` (vLLM and
  llama.cpp scrape their own `/metrics`) sends `{kv_used_pct, queue_depth, tokens_per_s, prefix_hit_pct}`
  with every heartbeat; `/loadz` aggregates the ALIVE workers of the plan model (max KV, Σ queue, Σ tok/s,
  mean prefix hits, `workers`, `reporting`); a sample older than 30 s stops reporting.
- **Sleep tier** (`worker_sleep`): `engines.Sleeper` (vLLM sleep mode) behind the worker's
  `/v1/model/sleep|resume`; the heartbeat says `sleeping`, the leader keeps those placements as
  `sleeping` (not routable), `/readyz` answers `sleeping-workers`, requests get `503 waking`; an engine with
  no sleep mode answers `501 unsupported` and the manager parks the pod instead.

## Managed mode

A leader run by an external manager sets one switch: `surfaces.managed` (`OPOD_MANAGED=1`). The store
becomes an in-memory, rebuildable cache unless `OPOD_STORAGE_DSN` names one (keys come from the auth
snapshot, nodes from heartbeats, usage and events are pulled by cursor), and the startup banner reads
`Mode: managed`. It never touches the request path or the stable admin contract; `TestSurfacesOff` walks
the whole contract on a managed leader.

There were three more — `ui`, `egress`, `callbacks` (`OPOD_UI`, `OPOD_EGRESS`, `OPOD_CALLBACKS`) — from
the time core still carried a dashboard, vendor egress and callback sinks. Those left with ADR-022, so
the switches gated nothing, and a standalone banner that read "egress on" described a binary with no
egress. They were removed, compatibly: the YAML decode is not strict, so a `config.yaml` that still
carries the three keys loads as before (every file an older binary saved has them), and no code reads or
warns about the variables, so a process started with `OPOD_UI=off` behaves exactly like one started
without it (`internal/config/surfaces_test.go`). A manager may stop rendering them whenever it likes.
The request surface itself is fixed — OpenAI chat, embeddings, models — since the Anthropic/audio/rerank
adapters and their `protocols` switch left on 2026-09-07 (ADR-022 step 4).
