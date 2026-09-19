# Changelog

What core ships today, by area — the honest inventory after ADR-022 (2026-09-05/07) contracted core to
the CLI-only inference runtime. For the per-release diff see
[Releases](https://github.com/opod-io/opod-core/releases). For what moved to the control plane and why, see
the last section. For what is next, [ROADMAP.md](ROADMAP.md).

## 2026-09-19 — router log

- **"router skipping stale worker" is logged once per stale episode, not once per request.** A worker whose process is
  replaced keeps its node row, and every pick that walked past the row logged a WARN — one line per request per dead row
  for as long as the leader ran. It is now logged when the worker becomes stale, and again only if it heartbeats and goes
  stale a second time. The `router_pick` metric still counts every skip.

## 2026-09-18 — `opod image`

- **The images as data.** `images/images.yaml` lists every image the repository builds — engine, vendor, platforms,
  weight format, gang scheme, proven on hardware or not, node requirements, limits — embedded in the binary and held
  to `images/build.sh` by a drift test. `opod image ls | show <image> | recommend <model>` print it, all with `--json`;
  `recommend` names the image for a catalog model (or an `hf:` / `file:` id) per accelerator vendor and says why every
  other engine was passed over. Offline and read-only: no config, no store, no registry call.

## 2026-09-07 — manager signals, sleep tier, services, SDK

- **Load signals (`load_signals`).** vLLM and llama.cpp workers scrape their own `/metrics` (KV-cache %,
  requests waiting, generated tokens/s, prefix-cache hit rate); the sample rides the heartbeat and
  `GET /loadz` aggregates the model's live workers (`kv_used_pct`, `queue_depth`, `tokens_per_s`,
  `prefix_hit_pct`, `workers`, `reporting`). The worker passes `--metrics` to `llama-server`.
- **Sleep tier (`worker_sleep`).** `POST /v1/model/sleep|resume` on a worker drives vLLM's sleep mode
  (`--enable-sleep-mode`, set when the manager asks for it); llama.cpp answers `501 unsupported` — never a
  fake sleep. The heartbeat carries `sleeping`, the leader keeps those placements as `sleeping` (not
  routable), `/readyz` answers `sleeping-workers`, and `POST /admin/v1/nodes/{id}/sleep|resume` proxies to
  the worker with its own token.
- **Services.** Node register/heartbeat/drain/remove, model add/delete/unload and shard create/remove are
  plain methods with typed errors (`nodeservice.go`, `modelservice.go`); handlers only decode → call → encode.
- **One emit.** `record()` writes a lifecycle event to the durable log and the in-process bus at once.
- **SDK.** The wire types moved to `github.com/opod-io/opod-sdk/adminapi` (Apache-2.0); the leader
  marshals those exact structs, so a manager imports them instead of mirroring.
- **Images.** `opod-worker-llamacpp-cpu` (CPU build; dev clusters, kind CI) and `opod-worker-llamacpp-intel`
  (SYCL). `images/build.sh` takes `ARCH=arm64` for a local kind on Apple silicon.
- **Licence.** Apache-2.0 (was PolyForm Shield); DCO sign-off, no CLA (ADR-019).

## Gateway

- OpenAI-compatible `/v1/chat/completions`, `/v1/embeddings`, `/v1/models`; SSE streaming with
  client-disconnect handling (bounded drain, no goroutine leaks)
- Engine drivers: **Ollama**, **vLLM**, **MLX-LM**, **llama.cpp** (single node and RPC); `llama-server` and
  `vllm serve` are launched by the worker for a placement; engine endpoints and keys per engine via env
- Hardware auto-detection (mac + linux + NVIDIA) and a default model auto-pick
- Per-key API keys with rpm / tpm / daily-token quotas, model allowlists, expiry; OpenAI-style
  `X-RateLimit-*` headers; keys as hashes in a watched `auth.json` when a manager mounts one
- Per-request fallback: an engine 5xx or timeout retries the catalog's fallback chain within the endpoint
- Placement cooldown (circuit breaker) per worker; heartbeat-age liveness drives `/readyz`, node and
  shard listings and routing
- Response cache (in-process, opt-in) and the pre/post guardrail **hook interface** (implementations live
  in the manager; a webhook rule in the mounted `policy.json` is applied by the leader)
- Errors follow the OpenAI shape with a `type` that follows the status (`unavailable`, `server_error`, …)

## Catalog and models

- 47 curated entries with `released:` dates and licence metadata enforced by CI; `opod model search|info|ls|ps`
- Non-catalog installs: `opod model add hf:owner/repo`, `ollama:tag`, `file:/path.gguf`, `--from my.yaml`;
  user entries persist in `~/.opod/catalog/`
- Signed catalog files: a minisign `<file>.minisig` beside a directory catalog file is verified against
  `OPOD_CATALOG_PUBKEY` at every catalog load and in `opod model add --from`; a signature that does not
  verify is always a refusal, `OPOD_CATALOG_REQUIRE_SIGNED=1` refuses unsigned files too, and the embedded
  catalog is exempt
- Pre-flight source probe on add (a certain 404 is refused; an unverifiable source proceeds with a warning)
- Memory lifecycle: installed vs resident, admission, evict-and-swap, release (`/admin/v1/memory`)
- Per-worker VRAM budgets (`opod join --gpu <i> --vram-budget <GB>`) → vLLM `--gpu-memory-utilization`
- Worker flags from a plan (`OPOD_ENGINE_FLAGS`): vLLM tp / gpu_memory_utilization / max_model_len /
  max_num_seqs / kv_cache_dtype; llama.cpp ctx / ngl / parallel / kv_cache_type; inert `extra`

## Cluster

- `opod up` (leader) / `opod join "<leader>?token=…"` (worker); register + 5 s heartbeats with loaded
  models, the engine load sample and the sleep state; HMAC-signed leader → worker calls
- Router: same model on N workers → load-balance; different models → route by placement; a model bigger
  than any node → llama.cpp-RPC sharding (`opod shard create <model> [N] [--nodes …]`), the coordinator on a
  worker, orphan-process sweep on create, `/readyz` counts a ready gang
- `opod shard create <model>` with no count picks the smallest number of equal parts that fits the live
  workers' free memory (never more than the workers or the model's layers) and prints why; a shape the
  caller names is sent untouched and `POST /admin/v1/shards/create` never picks
- `opod node remove` goes through the running leader (its router drops the node's cached connection, cooldown
  and placements) and falls back to the store — placements included — only when none answers; drain, undrain
  and remove say which path they took; `opod node ls` shows the live state (`lost`, `draining`)
- The "can anything serve this?" check is per model: a model whose every holder is drained, lost or asleep
  answers `503` + `Retry-After` — with the cause in the message — while other models on the leader keep
  serving; it used to pass a leader-wide check and answer 502/404 from the local engine
- `GET /v1/models` lists what can be answered for now: a model held only by drained or lost workers (or a
  gang with a part on one) leaves the list; a model that is merely asleep — sleeping workers, a plan that
  scales to zero — is listed, since a request for it is how it wakes
- `/loadz` `workers` (and `reporting`) count only workers that can take a new request for the plan's model:
  a drained, lost, sleeping or still-loading worker is neither capacity nor pressure
- One rule for "this worker takes new work" (`store.Node.TakesNewWork`: not drained, a serving state,
  heartbeating) behind the router, `/readyz`, the waking 503, `/loadz`, `/v1/models`, the shard pickers and
  `opod node ls`; the router applies it before revision groups, so a revision whose only worker is lost or
  cooling down gives its share to the others instead of dropping it on the local fallback
- A heartbeat whose engine did not answer (`loaded_models: null`) is "no report", not "nothing loaded": it
  advances the node's liveness and changes no placement row — it used to delete them all, the leader's
  `draining` / `released` marks included, so one slow engine tick put a draining placement or the source of a
  model move back in rotation. `[]` still clears the rows. A worker whose engine stays silent past the
  heartbeat bound leaves rotation through the one live rule (`nodes.engine_silent_since`; state
  `engine-silent` in `opod node ls`, a 503 that says so, events `node.engine_silent` / `node.engine_reporting`)
  with its rows and marks untouched, and the first real report brings it back (feature `heartbeat_no_report`)
- `opod model move <id> --from <node> --to <node>` (`POST /admin/v1/models/{id}/move`, feature `model_move`):
  another worker takes over a whole model by overlap — load on the target while the source serves, flip the
  router only when the target's heartbeat shows it serving, let in-flight requests on the source finish,
  unload the source; a target that never serves is unloaded again and the source is left serving; refused up
  front when it cannot work (target too small beside what it holds, a one-model engine serving something
  else, a node that takes no new work, a source that does not hold it or serves its adapters, a sharded
  model); every step is an event `model.move_*`; no KV cache moves; an Ollama source's placement is marked
  `released` (not routed to, across leader restarts) because its weights stay installed
- A worker registers which engine it runs (`hardware_json.Engine`, feature `worker_engine`), and each engine
  driver says whether a server of it serves one model per process (vLLM, SGLang, llama.cpp) or several
  (Ollama, MLX) — so the leader can refuse a load that would silently stop another model, naming both
  (`scheduler.LoadWouldReplace`; a worker that did not say gets the blunt rule: refused if it serves anything)
- **Worker images: a pinned model version is never satisfied by a file of the same name.** The entrypoint loaded "the
  whole file at the top of the node cache" by path whenever one existed, which skipped the revision directory AND the
  digest check: a node that had once served the model unpinned served those bytes under every `OPOD_MODEL_REVISION` /
  `OPOD_MODEL_SHA256`. A pinned load now always goes through the agent's fetch (`<models>/<repo>@<rev>/`, verified,
  reused when present); the by-path shortcut stays for unpinned models
- Requires `opod-sdk v0.2.1`: the usage stream carries `ttft_ms` (time to the first usable token of a streamed answer;
  omitted when not measured), and the revision weights of a canary split are read from the shared, typed policy
  snapshot instead of re-parsing the raw document
- **A worker whose engine is not running reports "nothing loaded" (`[]`), not "no report" (`null`).** Since the
  heartbeat learned to say "no report", a refused connection was one too — and the leader takes a worker with no report
  for 30 s out of rotation (`engine-silent`). That hit every worker whose engine is started by a load (vLLM, SGLang,
  llama.cpp) once it had idled for 30 s, and every part of a llama.cpp RPC gang for ever: a part runs an rpc-server,
  never an engine, so the gang was never routable and its leader never ready. A slow engine, or one that answers with
  an error, is still "no report"
- When the worker the router picked cannot be reached and no other can take the request, the gateway answers `503
  worker_unreachable` + `Retry-After` — no capacity right now — instead of `502 engine_unreachable` naming the LEADER's
  own engine with a hint to start `llama-server`. A parked or just-removed worker still looks alive for a heartbeat or
  two; its first caller was told to fix an engine that was never there (chat and embeddings)
- A connection to an engine or a worker must be established within 3 s (`openaicompat.ConnectTimeout`); responses still
  stream with no overall deadline. The client had inherited Go's 30 s connect timeout, and a removed pod's address does
  not refuse, it hangs — so the request picked for a just-removed worker stalled 20–30 s before the next worker was
  asked, and an outer deadline sometimes answered it 502 first (one per pod swap, measured)
- **A worker that has just gone away costs no request while another serves the model.** Its placement row stays
  fresh until its heartbeat ages out; a request picked for it could not connect and was answered 502, because the
  walk went on to the next fallback MODEL, never to the next WORKER. An unreachable worker received nothing, so the
  same model is picked again with that node set aside for this request (chat and embeddings); an engine's own
  answer — a refusal included — is never replayed. The re-pick stays in the revision group the request was assigned
  to, so a traffic split is not re-rolled by a retry. Seen as one failed chat per worker move under a manager's rollout
- `opod model add <id> --node …` no longer replaces what a worker serves in silence: every named worker is judged
  first (the rule `model move` already used), a load that would stop another model is refused with 409 naming both
  — before any worker is touched — and `--force` (`"force": true`) is how an operator says the replacement is meant
- A leader-driven model load (`opod model add --node`, `opod model move`) is no longer cut at 60 s: the worker
  answers when the pull and the load are done, so the call rides the weights client (as the GGUF upload does),
  bounded by the caller's context — a cold pull used to fail at the leader while the worker carried on
- **`engine.preferred: sglang` starts.** The SGLang driver was registered, but the function that builds the
  configured engine for `opod up` / `opod join` chose the endpoint in a hand-written list without it, so a leader
  or worker set to SGLang exited with `unknown engine "sglang" (valid: … sglang …)`. The endpoint is now chosen by
  the driver registry's canonical name (aliases are spelled once, by the driver), SGLang has its own setting
  (`engine.sglang_endpoint`, `OPOD_SGLANG_ENDPOINT`, default `http://127.0.0.1:30000`), and a test fails when a
  linked driver has no endpoint
- An Ollama worker's heartbeat says which of its installed models are in memory (`resident_models`, feature
  of the same name) beside `loaded_models`, which stays "what this worker answers for" — the router needs
  that; the leader marks the rest `cold` (routable, holding no memory) and the shard-count picker's memory
  facts stop counting installed-but-idle Ollama models as memory in use
- A worker can be asked to stop holding a model: `POST /v1/model/unload` (feature `worker_unload`, HMAC-signed
  like `/v1/model/load`, the same source fields) — the engine's own unload where it has one (Ollama), a stop
  of the engine process where the worker launched it (`vllm serve`, SGLang, `llama-server`); idempotent
  (`noop` when not resident), `409` for a shard part or a held adapter of the model, `501` when the engine
  cannot and the worker did not start it; the model leaves the next heartbeat and its placement row goes
- A placement the leader marks `draining` stays out of rotation across the worker's heartbeats (feature
  `placement_drain`) — it used to be rewritten to `ready` within 5 s; it ends when it is set back, when the
  model leaves the worker, or at a leader start
- A LoRA adapter's `rank` travels on the live path too (`POST /admin/v1/adapters {name, source, rank?}` →
  the worker's `/v1/adapters/load`); a worker whose vLLM was started without LoRA slots, or for a smaller
  rank, refuses with `409` and both numbers instead of relaying the engine's error
- `opod model add <id> --node <n>` sends the catalog entry's `source.file` to the worker (`/v1/model/load`
  `file`, omitted when empty), so a repository holding several GGUF files loads the one the entry names
- One rule for "which rows can take a shard part" (`scheduler.WorkerFor`) on the CLI and the API path alike:
  never the leader's own `local` row, a draining node or a silent one; a create that cannot find its workers
  answers `409` with the numbers before the gang it would replace is touched
- `opod node drain|undrain <id>` (`POST /admin/v1/nodes/{id}/drain|undrain`, feature `node_drain`): the
  router, the hedged pick and the shard pickers give a draining worker nothing new, a gang with a part on
  it leaves rotation, in-flight finishes, `/readyz` and the waking 503 do not count it; the state survives
  heartbeats and a re-register
- Router-only leaders (no local engine) are the default in a cluster; `/readyz` modes: local engine ·
  `router-only` · `sleeping` · `sleeping-workers` · `shard-coordinator`
- Plan as a watched file (`/etc/opod/plan.json`): one model identity per leader, revision on `/loadz`,
  floor-0 sleep = `503 waking` + `Retry-After` (itself the wake signal)
- Managed mode (`OPOD_MANAGED=1`): in-memory store, no admin-key file, bounded usage/event rings,
  `boot` stamp on every stream batch; the admin group always requires a key

## Manager surface

- Frozen, additive-only `/admin/v1` contract (`contract.go`, `GET /admin/v1/version|capabilities`,
  `TestLeaderContract`): probes, the two gateway routes, worker register/heartbeat, nodes (list, drain,
  delete, sleep, resume), models (list, add, delete, load), healthcheck, `events/stream` and
  `usage/stream` (cursor + replay + `boot`), shards (list, create, delete)
- Every state-changing `/admin/v1` call is an `admin.call` lifecycle event (actor, status) — audit rides
  the stream; guardrail verdicts too
- Managed mode: `OPOD_MANAGED` (`TestSurfacesOff` walks the contract on a managed leader). The
  `OPOD_UI` · `OPOD_EGRESS` · `OPOD_CALLBACKS` switches are retired — the surfaces they switched left
  with ADR-022 — and are still accepted and ignored, in the environment and in `config.yaml`
- Prometheus `/metrics`; OTLP traces (`observability.otlp_endpoint`)

## CLI

- `opod up|down|join|status`, `opod node ls|show|drain|remove`, `opod model add|load|unload|rm|ls|ps|search|info`,
  `opod shard create|ls|remove`, `opod token create|ls|revoke|edit`, `opod connect|disconnect <client>`
  (copy-paste snippets for OpenAI-shape tools), `opod update`, `opod config show` (secrets redacted)
- Interactive pickers, `--json` everywhere, `NO_COLOR`

## Release and ops

- One-line installer (`curl | sh`) for macOS (Apple silicon) and Linux (x86_64, arm64); launchd / systemd
  units; `opod update` with checksum verification; static single binary
- Images from the official upstream engine images plus a thin opod layer, pushed to `ghcr.io/opod-io/*`,
  digest-pinned by consumers, never built on cluster nodes; the llama.cpp CUDA RPC pair prebuilt once per
  release (`images/README.md`)
- Reference Grafana dashboards in `dashboards/`

## Security and hardening

- SSRF protection for outbound hook clients (`block_private_targets`); forwarded headers gated behind
  `trust_proxy_headers`; `opod update` tar extraction hardened; `SQLITE_BUSY` avoided with `busy_timeout`;
  dev mode never locks out cluster admin; the managed admin group always requires a key

## Removed with ADR-022 (2026-09-05 → 2026-09-07) — now the control plane's

The embedded dashboard and its Connect/Invite tabs, the localhost key bootstrap, the automatic update check,
vendor egress (12 hosted providers, key pools, Bedrock/Vertex signing, the `model="auto"` routing chain),
the Anthropic Messages and audio/rerank adapters, callback sinks (webhooks, Langfuse, S3), dollar budgets
and `$` cost fields, the usage/audit query APIs (`opod usage|audit`), and the guardrail implementations.
Each has one home now: the control plane (`opodcp`) — console, Routing, Integrations, Usage, Audit,
Guardrails pages — driven from core's typed event and usage streams. Core keeps the mechanisms and the
hook interfaces. Rollback of any core release is a re-pin of the previous leader image digest.
