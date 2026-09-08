# Changelog

What core ships today, by area — the honest inventory after ADR-022 (2026-09-05/07) contracted core to
the CLI-only inference runtime. For the per-release diff see
[Releases](https://github.com/opod-io/opod/releases). For what moved to the control plane and why, see
the last section. For what is next, [ROADMAP.md](ROADMAP.md) and [TASKS.md](TASKS.md).

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
- Pre-flight source probe on add (a certain 404 is refused; an unverifiable source proceeds with a warning)
- Memory lifecycle: installed vs resident, admission, evict-and-swap, release (`/admin/v1/memory`)
- Per-worker VRAM budgets (`opod join --gpu <i> --vram-budget <GB>`) → vLLM `--gpu-memory-utilization`
- Worker flags from a plan (`OPOD_ENGINE_FLAGS`): vLLM tp / gpu_memory_utilization / max_model_len /
  max_num_seqs / kv_cache_dtype; llama.cpp ctx / ngl / parallel / kv_cache_type; inert `extra`

## Cluster

- `opod up` (leader) / `opod join <leader>?token=` (worker); register + 5 s heartbeats with loaded
  models, the engine load sample and the sleep state; HMAC-signed leader → worker calls
- Router: same model on N workers → load-balance; different models → route by placement; a model bigger
  than any node → llama.cpp-RPC sharding (`opod shard create <model> [N] [--nodes …]`), the coordinator on a
  worker, orphan-process sweep on create, `/readyz` counts a ready gang
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
- Surface switches for a managed leader: `OPOD_UI` · `OPOD_EGRESS` · `OPOD_CALLBACKS` · `OPOD_MANAGED`
  (`TestSurfacesOff` proves the contract survives every switch)
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
