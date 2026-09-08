# Opod Architecture

> Scope note (ADR-022, 2026-09-06): core is the CLI-only inference runtime. The dashboard, invites, vendor egress, routing chains, callbacks, budgets, usage/audit query APIs and guardrail implementations moved to the control plane; this document describes what remains. The stable manager surface is listed at the end.

Deep-dive design for contributors and maintainers. For user-facing docs, see [README.md](README.md). For the active implementation plan, see [TASKS.md](TASKS.md).

> **Doc-vs-code currency:** this document covers the shipped feature set — cross-node routing, sharding auto-orchestration, CLI/UI parity, HMAC mutual auth, GGUF distribution, OTLP traces, 19 connect clients, interactive picker, shell completion, `--json` on every read command, `--summary` aggregates for usage/audit, first-run wizard, real progress bar, colored output, engine health watchdog, typed `engine_unreachable` errors. The code on `main` is the source of truth — if you find a mismatch please file an issue or PR.

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
   │  node registry · model placements · usage · audit · web UI    │
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
| `opod up` | **Leader**: HTTP gateway · Router · Control plane · Web UI · embedded SQLite · local engine adapter |
| `opod join <url>?token=…` | **Worker**: agent.Loop (heartbeat with loaded_models) · agent.Server (OpenAI-compat passthrough bound to the LAN/tailnet address) · local engine adapter |
| `opod <cmd>` (e.g. `node ls`, `model add`) | One-shot CLI; reads SQLite directly or calls the leader's admin API |
| `opod doctor` | Stand-alone diagnostics — port availability, Ollama reachability, catalog count, hardware summary |
| `opod update` / `opod upgrade` | Hits `api.github.com/repos/opod-io/opod/releases/latest`, downloads the matching platform tarball, verifies SHA-256 against `checksums.txt`, atomically replaces the running binary. Restarts are user-driven (`opod down && opod up`). |

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
       ├─ pickWorkers(2) — ready nodes, descending RAM
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

#### Failure handling

- If any rpc-server fails to come up (readiness timeout, process exits), `Orchestrator.rollback()` stops every previously-launched process and returns the error to the CLI/UI.
- If a shard process crashes *after* CreateSharded returns, the supervisor auto-restarts it up to 5 times with exponential backoff (1s, 2s, 4s, 8s, 16s; capped at 30s for any longer chain). After 5 the process enters `crashloop` state and stays there — the admin must intervene. Both `rpc-server` (per-shard) and the `llama-server` coordinator are restart-enabled; the policy is set on the `agent.ProcessSpec` at launch time in `internal/scheduler/sharding.go`. Explicit `Stop()` suppresses any pending restart.

#### Out of scope for v0.4

- The coordinator (`llama-server`) is placed on the highest-RAM host in the shard set — by default the strongest worker, not the leader. Override with `OPOD_COORDINATOR_NODE=<node_id>` (or `local` to force leader). When the coordinator runs on a worker it's launched via the same `/v1/process/start` endpoint used for `rpc-server`, and the leader's router dials it at `<worker-address>:<coord_port>`. Single-machine sharding still pins the coordinator to the local supervisor.
- **Automatic GGUF download + distribution** fully closes M5-T12. For catalog entries with `source.type: huggingface` + `source.file:`, `CreateSharded` first downloads the GGUF from `huggingface.co/<repo>/resolve/main/<file>` into `storage.models_dir` on the leader (skipped if already present, partial→rename atomicity). Then for both `huggingface` and `file` types, fans the local file out to every shard host via `/v1/process/file` HEAD + `/v1/process/upload` POST (sha256-verified, skipped if the worker already has the file). No more manual `wget` to leader or `scp` to workers — `opod shard create <id>` is sufficient.
- Live shard migration / rebalancing.
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
                       │   (chi router, embedded UI)       │
                       └──────────┬───────────────────────┘
                                  │
       ┌────────────┬─────────────┼────────────┬──────────────┐
       ▼            ▼             ▼            ▼              ▼
   ┌────────┐  ┌─────────┐  ┌──────────┐  ┌─────────┐  ┌──────────┐
   │  API   │  │  Admin  │  │   Auth   │  │ Metrics │  │  Web UI  │
   │adapters│  │  API    │  │ (keys,   │  │         │  │ (embed)  │
   │ OAI/   │  │         │  │  HMAC)   │  │         │  │          │
   │ Anthr  │  │         │  │          │  │         │  │          │
   └───┬────┘  └────┬────┘  └──────────┘  └─────────┘  └──────────┘
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
5. Capability matching (when does the scheduler pick you?) — TARGET, lives with the vendor registry.

---

## Model registry and puller

### Catalog

YAML files in `catalog/<id>.yaml`:

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

`slog` to stdout in JSON. Levels: debug, info, warn, error. Request IDs propagated through context.

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
- The web UI authenticates with an API key (no OIDC — see § Authentication and authorization).
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
| Single embedded HTML page (`go:embed`) | Next.js SPA, separate web server | Embedded UI = one binary, no Node toolchain in the build |
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
├── TASKS.md                   # implementation plan
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
│   ├── metrics/               # Prometheus declarations
│   └── ui/                    # embed.go + index.html (single embedded page)
│
├── catalog/                   # YAML model catalog entries
│   ├── llama-3.2-1b.yaml
│   ├── llama-3.2-3b.yaml
│   ├── llama-3.3-70b-sharded.yaml
│   ├── qwen-coder-{7b,14b,32b}.yaml
│   └── … (41 entries — see catalog/README.md)
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
- **No global mutable state** beyond metrics and the embedded UI fs.
- **Generics**: only where a type-safe alternative is impossible.
- **File length**: aim under 600 lines; split at 800.

### UI conventions

- Vanilla JavaScript, inline at the bottom of the file
- Tailwind via CDN for styles
- Data fetching via `fetch` against the admin API; live updates via `EventSource` on `/admin/v1/events` (SSE), with polling as the fallback
- New UI capability = edit `index.html` directly; the backing logic must already exist in `internal/control/` (CLI-first rule)

---

## Build from source

### Prerequisites

- Go 1.25+
- Optional: NVIDIA Container Toolkit (for vLLM workers)

### Build

```bash
git clone https://github.com/opod-io/opod
cd opod

# Build the binary — the UI is a single embedded HTML file
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
git push --tags        # CI builds binaries (UI is embedded), publishes checksums + tarballs to GH Releases
```

---

## Getting started as a contributor

### Your first 30 minutes

```bash
git clone https://github.com/opod-io/opod
cd opod
make check             # lint + test + build (this is what CI runs)
./opod up             # boots a single-node leader against local Ollama
```

You only need Go 1.25+ and a working Ollama install (`brew install --cask ollama` on macOS, or `curl -fsSL https://ollama.com/install.sh | sh` on Linux). No Docker, no Python, no Node — the web UI is a single embedded HTML file compiled into the binary.

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

There is no `make dev`, `make ui`, or `make test-e2e` — those are not needed for a hot-reload-free Go binary with an embedded UI. End-to-end tests run inline as `go test ./...` (look for `_test.go` files that spin up an `httptest.Server`).

### Finding your way around

Start with these files in order. Each top-of-file comment explains what the package owns; no file exceeds ~600 lines.

1. `cmd/opod/main.go` — switch statement over subcommand verbs
2. `cmd/opod/cmd_*.go` — one file per CLI subcommand (no file over 400 lines); each parses flags, calls a package and prints: model install/search → `internal/models` (`Install`, `Search`, `PersistUserCatalogEntry`), boot steps → `internal/control/bootstrap.go`, self-update → `internal/update`
3. `internal/controlplane/server.go` — leader HTTP server (chi router); wires data-plane + admin routes
4. `internal/api/openai.go` — OpenAI protocol adapter (`/v1/chat/completions`, `/v1/models`, `/v1/embeddings`)
7. `internal/control/control.go` — every mutating operation in one place; both CLI and admin HTTP call into here (the load-bearing rule from § CLI / Admin API / Web UI contract above)
8. `internal/router/router.go` — picks the backing engine per request (local → remote → fallback)
9. `internal/scheduler/sharding.go` — orchestrates sharded models (rpc-server + coordinator)
10. `internal/engines/types.go` — `Engine` interface; `registry.go` — `Register`/`New`/`NativeName`/`CatalogID`; `internal/engines/{ollama,vllm,mlx,llamacpp}/` are the drivers; `all/` links them
11. `internal/agent/loop.go` — worker register + heartbeat loop; `internal/agent/server.go` is the worker HTTP server
12. `internal/store/sqlite.go` — schema, migrations, query helpers

### Common contributor tasks

| Task | Touch these files |
|---|---|
| Add a new inference engine | `internal/engines/<name>/` (implement `Engine`, `engines.Register` in `init`), one line in `internal/engines/all`, `enginetest.Run` conformance test |
| Add a new model to the catalog | `catalog/<id>.yaml` — see [catalog/README.md](catalog/README.md) for the schema |
| Add a new CLI subcommand | `cmd/opod/cmd_<name>.go` + add a case in `cmd/opod/main.go` + add the mutating function in `internal/control/` first (CLI is the source of truth) |
| Add a new admin HTTP endpoint | `internal/controlplane/admin_<name>.go` — must delegate to `internal/control/` |
| Add a UI page or tab | edit `internal/ui/index.html` directly; the JS is inline at the bottom |
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

Add `catalog/<id>.yaml`. The catalog is loaded at startup; no code change needed. See [catalog/README.md](catalog/README.md) for the schema and required fields.

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

Two mechanisms a manager drives through this surface (2026-09-07):

- **Load signals** (`load_signals`): a worker engine that implements `engines.LoadReporter` (vLLM and
  llama.cpp scrape their own `/metrics`) sends `{kv_used_pct, queue_depth, tokens_per_s, prefix_hit_pct}`
  with every heartbeat; `/loadz` aggregates the ALIVE workers of the plan model (max KV, Σ queue, Σ tok/s,
  mean prefix hits, `workers`, `reporting`); a sample older than 30 s stops reporting.
- **Sleep tier** (`worker_sleep`): `engines.Sleeper` (vLLM sleep mode) behind the worker's
  `/v1/model/sleep|resume`; the heartbeat says `sleeping`, the leader keeps those placements as
  `sleeping` (not routable), `/readyz` answers `sleeping-workers`, requests get `503 waking`; an engine with
  no sleep mode answers `501 unsupported` and the manager parks the pod instead.

## Surface switches (managed mode)

A leader run by an external manager is essentials-only. `surfaces:` in the config (env overrides
in brackets) switches product surfaces off without touching the request path or the stable admin
contract: `ui` (`OPOD_UI`, accepted for compatibility — core has had no dashboard since ADR-022, `/`
is always 404), `egress` (`OPOD_EGRESS=off`: never forward to a remote vendor whatever keys are in the
environment), `callbacks` (`OPOD_CALLBACKS=off`: no sinks, no `/admin/v1/callbacks`) and
`managed` (`OPOD_MANAGED=1`: update check off, in-memory store under a mounted plan, banner says so).
The request surface itself is fixed — OpenAI chat, embeddings, models — since the Anthropic/audio/rerank
adapters and their `protocols` switch left on 2026-09-07 (ADR-022 step 4). Defaults keep everything on
for a standalone `opod up`. `TestSurfacesOff` proves the stable admin surface is intact with every switch off.
