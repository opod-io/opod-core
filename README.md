# Opod

> **Self-hosted AI for your team. One endpoint. Your hardware.**

[![License](https://img.shields.io/github/license/opod-io/opod-core?color=blue)](LICENSE)
[![Go](https://img.shields.io/github/go-mod/go-version/opod-io/opod-core)](go.mod)
[![Release](https://img.shields.io/github/v/release/opod-io/opod-core?sort=semver)](https://github.com/opod-io/opod-core/releases/latest)
[![CI](https://github.com/opod-io/opod-core/actions/workflows/ci.yml/badge.svg)](https://github.com/opod-io/opod-core/actions/workflows/ci.yml)
[![Auto-release](https://github.com/opod-io/opod-core/actions/workflows/auto-release.yml/badge.svg)](https://github.com/opod-io/opod-core/actions/workflows/auto-release.yml)

[**opod.io**](https://opod.io) · [GitHub](https://github.com/opod-io/opod-core) · Maintained by [Hadi Honarvar Nazari](https://www.linkedin.com/in/hadi-honarvar-nazari/) · Apache-2.0

>
> Engine-agnostic: bring **Ollama**, **vLLM**, **SGLang**, **MLX-LM**, or **llama.cpp**. Run open-weight models (Qwen, Llama, DeepSeek, …) on your own hardware and shard a giant model across several machines via llama.cpp-RPC. Core has no vendor egress: nothing leaves your network.
>
> Point Cursor, Aider, Continue, Codex CLI, or any OpenAI SDK at Opod. It just works.

## 🗺️ Where Opod sits

> **Scope (ADR-022, 2026-09-06).** Core is the CLI-only inference runtime: `opod up` / `opod join`, the OpenAI-compatible
> gateway, API keys + join tokens, engine adapters, model load, llama.cpp-RPC sharding, and a stable `/admin/v1` surface for an
> external manager. Everything product-shaped — console, invites, vendor egress and cloud key pools, routing chains, callbacks and
> sinks, dollar budgets, usage/audit query APIs, guardrail implementations, automatic update checks — lives in the control plane (`opodcp`).

```
           ┌──────────────────────────────────────────────────────────────┐
           │                       YOUR USE CASES                         │
           │             (the tools your team already uses)               │
           └──────────────────────────────────────────────────────────────┘
                  │           │          │             │            │
                  ▼           ▼          ▼             ▼            ▼
            ┌──────────┐ ┌──────────┐ ┌──────────┐ ┌──────────┐ ┌──────────┐
            │  Cursor  │ │  Codex   │ │  Aider   │ │  Custom  │ │   curl   │
            │          │ │   CLI    │ │          │ │ Python   │ │  scripts │
            │          │ │          │ │          │ │   SDK    │ │          │
            └────┬─────┘ └────┬─────┘ └────┬─────┘ └────┬─────┘ └────┬─────┘
                 │  OpenAI    │  OpenAI    │  OpenAI    │  OpenAI    │  HTTP
                 └────────────┴────────────┴────────────┴────────────┘
                                          │
                                          │   ONE URL · ONE API KEY
                                          ▼
      ╔══════════════════════════════════════════════════════════════════════╗
      ║                  ⬢ ⬢ ⬢   OPOD   ⬢ ⬢ ⬢                              ║
      ║                  (this is what we built)                             ║
      ║  ════════════════════════════════════════════════════════════════    ║
      ║  Gateway     OpenAI-compatible /v1/chat/completions + /v1/embeddings ║
      ║              per-key rpm/tpm/daily quotas · typed event stream       ║
      ║              CLI-only core · the console is the control plane        ║
      ║                                                                      ║
      ║  Router      Same model on N nodes  → load-balance                   ║
      ║              Different models per node → route by placement          ║
      ║              Model bigger than any node → split via llama.cpp-RPC    ║
      ║              Engine error or timeout  → retry catalog fallback chain ║
      ╚═════════════════════════════╤════════════════════════════════════════╝
                                    │
                    ┌───────────────┴───────────────┐
                    ▼                               ▼
             ┌─────────────┐                 ┌─────────────┐
             │  (any mix)  │                 │  (any mix)  │
             │  • Ollama   │                 │  • Ollama   │
             │  • vLLM     │                 │  • vLLM     │
             │  • MLX-LM   │                 │  • MLX-LM   │
             │  • llama.cpp│                 │  • llama.cpp│
             └──────┬──────┘                 └──────┬──────┘
                    ▼                               ▼
      ┌──────────────────────────────────────────────────────────────────────┐
      │                    UNDERLYING LLMs / WEIGHTS — YOUR HARDWARE         │
      │   • Mac Studio · Mac Mini · Linux + NVIDIA / AMD GPUs                │
      │   47 curated catalog models (Qwen 3.6, GLM, gpt-oss, Llama 4,        │
      │   Gemma 4, DeepSeek V4, Kimi K2.6, Nemotron 3 Ultra, vision +        │
      │   embedding models) + any HuggingFace or Ollama model.               │
      │   Nothing leaves your network: core has no vendor egress (ADR-022). │
      └──────────────────────────────────────────────────────────────────────┘
```

**One-sentence version:** Opod is the layer that lets your tools talk to open-weight LLMs on your own hardware through **one URL and one API key**, with the team controls (quotas, rate limits, per-user keys) a bare inference engine doesn't give you. It is a CLI and an HTTP API; there is no UI in core.

---

## 🚀 Try it in 60 seconds

Opod is engine-agnostic. The quickest path uses **Ollama** as the local engine — but vLLM, MLX-LM, and llama.cpp-RPC all work. See [Prerequisites — read first](#prerequisites--read-first) below for the alternatives.

### 🍎 macOS (Apple Silicon — M1/M2/M3/M4)

```bash
# 1. install Opod
curl -fsSL https://raw.githubusercontent.com/opod-io/opod-core/main/installer/install.sh | sh
export PATH="$HOME/.local/bin:$PATH"   # if the installer says so

# 2. install an engine (pick one) — Ollama is the simplest default
brew install --cask ollama && open -a Ollama
# alternatives: pip install mlx-lm  ·  or run llama.cpp's llama-server  ·  or run vLLM in Docker

# 3. start Opod with a tiny model (~1 GB, fast download)
OPOD_DEFAULT_MODEL=llama-3.2-1b opod up
```

### 🐧 Linux (x86_64 or arm64) — including Raspberry Pi, NAS, edge boxes

**Option A — `.deb` / `.rpm` package** (recommended for Debian / Ubuntu / Raspbian / QNAP / Asustor / Fedora / RHEL):

```bash
# Debian / Ubuntu / Raspbian (arm64 example — also amd64)
curl -LO https://github.com/opod-io/opod-core/releases/latest/download/opod_VERSION_linux_arm64.deb
sudo dpkg -i opod_VERSION_linux_arm64.deb
# Binary at /usr/bin/opod, catalog at /usr/share/opod/catalog
# Recommends llama.cpp for sharding — install via apt if you want it.

# Fedora / RHEL / CentOS
sudo rpm -i https://github.com/opod-io/opod-core/releases/latest/download/opod_VERSION_linux_amd64.rpm
```

(Replace `VERSION` with the latest from [Releases](https://github.com/opod-io/opod-core/releases). The package version stays current via your distro's normal upgrade path — `opod update` also works as an in-place binary swap for non-package installs.)

**Option B — install.sh** (works everywhere; drops the binary in `~/.local/bin/`; the catalog is embedded):

```bash
# 1. install Opod
curl -fsSL https://raw.githubusercontent.com/opod-io/opod-core/main/installer/install.sh | sh
echo 'export PATH="$HOME/.local/bin:$PATH"' >> ~/.bashrc && source ~/.bashrc

# 2. install an engine (pick one) — Ollama is the simplest default
curl -fsSL https://ollama.com/install.sh | sh && sudo systemctl enable --now ollama
# alternatives: vLLM in Docker for NVIDIA  ·  llama.cpp's llama-server  ·  MLX-LM (Apple Silicon only)

# 3. start Opod with a tiny model (~1 GB, fast download)
OPOD_DEFAULT_MODEL=llama-3.2-1b opod up
```

> 💡 Not sure which engine to install? Run `opod doctor` after step 1 — it inspects your hardware and tells you the single command to run.

### What you should see (both platforms)

Opod prints something like:

```
✔ default model: llama-3.2-1b
✔ engine: ollama at http://127.0.0.1:11434

  Opod is ready.

  API:        http://localhost:8080/v1
  Health:     http://localhost:8080/healthz

  Admin API key (shown once — store it now):
    sk-orc-<printed once>
```

**Every command supports `--help`** — `opod <cmd> --help` prints usage, flags, and examples.

**Copy that admin key.** In another terminal:

```bash
curl http://localhost:8080/v1/chat/completions \
  -H "Authorization: Bearer sk-orc-<your-key>" \
  -d '{"model":"auto","messages":[{"role":"user","content":"hi in 5 words"}]}'
```

You should see a JSON response with a 5-word reply. 🎉

**Or wire up an OpenAI-shape tool** (Codex CLI, Aider, Cursor, the OpenAI SDK): in any terminal, set:

```bash
export OPENAI_BASE_URL=http://localhost:8080/v1
export OPENAI_API_KEY=sk-orc-<your-key>
codex
```

…and the tool talks to your local model instead of paying for the API. `opod connect <tool>` prints the exact lines for each one.

**If something breaks**, run `opod doctor` — it tells you exactly what to fix. Common issues are in the [Troubleshooting installation](#troubleshooting-installation) section.

---

| | |
|---|---|
| **License** | Apache 2.0 |
| **Language** | Go — one static binary, CLI only |
| **Platforms** | macOS (Apple Silicon), Linux (x86_64, arm64) |

## What's shipped

See [CHANGELOG.md](CHANGELOG.md) for the full feature inventory, grouped by area (gateway, cluster, models, keys, observability, connect snippets, release + ops — and what moved to the control plane). For the per-release diff see [Releases](https://github.com/opod-io/opod-core/releases) — every `feat:` / `fix:` commit on `main` cuts a new tag automatically.

**For new users**: see [QUICKSTART.md](QUICKSTART.md) — 3-minute install + first chat completion.
**For full usage docs**: keep reading this file.
**For contributors**: see [ARCHITECTURE.md](ARCHITECTURE.md).
**For the roadmap**: see [ROADMAP.md](ROADMAP.md).

---

## Table of contents

- [Why Opod?](#why-opod)
- [60-second quick start](#60-second-quick-start)
- [Who is this for?](#who-is-this-for)
- [Architecture overview](#architecture-overview)
- [Features](#features)
- [Supported models](#supported-models)
- [Supported clients](#supported-clients)
- [Hardware recommendations](#hardware-recommendations)
- [Installation](#installation)
- [Configuration](#configuration)
- [Cluster operations](#cluster-operations)
- [Managing models](#managing-models)
- [Connecting clients](#connecting-clients)
- [API reference](#api-reference)
- [CLI reference](#cli-reference)
- [Troubleshooting](#troubleshooting)
- [FAQ](#faq)
- [License](#license)

---

## Why Opod?

AI coding tools are the new dev tax. Cursor, Claude Code, Copilot, custom agents — every team uses them, and the bill grows with usage. A single engineer running modern agentic tools heavily can burn $200–500/month in API tokens. For a team of 10 that's $30–60k a year, and rising. Every request also sends proprietary code to a third party.

There are excellent open-weight models now — Qwen3-Coder, Llama 3.3, DeepSeek-V3 — that match or exceed paid APIs for most coding work. But running them across a few machines, exposing them through one API, routing traffic intelligently, and making it all feel as easy as `pip install` is *not* solved.

**Opod is the orchestration layer.** It does for self-hosted LLMs what Kubernetes did for web services — minus the YAML. One binary. One install command. Auto-discovery. Auto-placement. Drop-in compatibility with every tool you already use.

### Design principles

1. **One binary, zero dependencies.** Static Go executable. No Python, no Docker (unless you want it), no virtualenv. Curl it down and run.
2. **Zero config to first response.** Smart defaults everywhere. Hardware auto-detected. Model auto-picked.
3. **The CLI tells you the next step.** `opod up` prints the key, a test `curl` and what to run next; `opod doctor` names the fix for what it finds; a mistyped command gets a "did you mean". Juniors should never stare at a blank prompt.
4. **Heterogeneous is invisible.** Apple Silicon, NVIDIA and AMD workers behind one gateway (Intel and Tenstorrent worker images ship; see [`images/README.md`](images/README.md) for what is proven on hardware) — the user picks models, not hardware.
5. **OpenAI-compatible, one protocol.** Every OpenAI-shape client connects with a base URL and a key; other wire shapes are a shim in front of the gateway, not core's job (ADR-022).
6. **Permissive open source.** Apache 2.0. No open-core gotchas.
7. **The CLI is the interface.** Every capability is an `opod` command, and behind it an `/admin/v1` route. There is no UI in core (ADR-022): what you can do at a terminal you can do in CI, scripts and SSH sessions, and an external manager drives the same routes.
8. **Adding or switching a model is one action.** No hand-written YAML, no manual GGUF downloads, no separate worker-side setup. `opod model add hf:owner/repo` does the rest — picks engine, picks quant, shards if needed, distributes weights, warms the model. The default model is auto-picked from hardware on first `opod up`; to change it later, set `router.default_model` in `~/.opod/config.yaml` and restart, or `OPOD_DEFAULT_MODEL=<id> opod up`.

---

## 60-second quick start

### On the first machine (becomes the leader)

```bash
curl -fsSL https://raw.githubusercontent.com/opod-io/opod-core/main/installer/install.sh | sh
opod up
```

You'll see:

```
▶ detected darwin/arm64 · 24 GB RAM · 8 cores
✔ default model: qwen-coder-7b
✔ engine: ollama at http://127.0.0.1:11434
▶ pulling qwen-coder-7b · downloading [████████████████████] 4.7/4.7 GB · 85 MB/s · ETA 0:00
✔ model ready: qwen-coder-7b

  Opod is ready.

  API:        http://localhost:8080/v1
  Health:     http://localhost:8080/healthz

  Admin API key (shown once — store it now):
    sk-orc-<printed once>
```

The key is also saved to `~/.opod/admin.key` on the leader.

### On any additional machine

```bash
# on the leader: mint a join token
opod token create --node

# on the new machine: install and join in one line
curl -fsSL https://raw.githubusercontent.com/opod-io/opod-core/main/installer/install.sh | sh -s -- join "http://<leader-host>:8080?token=<token>"
```

The worker registers with the leader over plain HTTP — the leader's port must be reachable from it (same LAN or your own VPN) — reports its hardware and engine, and heartbeats every 5 s. A worker serves whatever its engine has loaded; `opod model add <id> --node <node-id>` on the leader pulls and warms a model on a specific worker.

### Test it from your terminal

```bash
curl http://localhost:8080/v1/chat/completions \
  -H "Authorization: Bearer sk-orc-<your-key>" \
  -H "Content-Type: application/json" \
  -d '{
    "model": "auto",
    "messages": [{"role":"user","content":"write fizzbuzz in rust"}]
  }'
```

### Use it from an OpenAI-shape tool

```bash
export OPENAI_BASE_URL=http://localhost:8080/v1
export OPENAI_API_KEY=sk-orc-<your-key>
aider --model openai/qwen3-coder-30b
```

Aider is now talking to your local Qwen-Coder. Same UX, your hardware.

---

## Who is this for?

| You are… | Opod helps you… |
|---|---|
| A **10–50 person dev team** spending $30k+/yr on Claude/GPT APIs | Run the same workflows on hardware that pays for itself in <6 months |
| A **regulated org** (legal, health, defense) that can't send code to third parties | Keep 100% of inference on-prem — core has no vendor egress |
| An **AI/ML lab** with mixed-spec workstations and lab Macs | Pool all of it into one cluster behind one API |
| A **solo developer** who wants one endpoint covering their laptop, home server, and lab GPU | Use Cursor or Aider anywhere with the same key |
| A **classroom or research group** | Give every student a real LLM endpoint without per-seat costs |
| An **MSP or platform team** | Offer "internal Claude" as a service to product teams without lock-in |

### Non-goals

- **Training or fine-tuning** — Opod serves inference. Use Axolotl / Unsloth / torchtune for training, import the adapter.
- **Replacing frontier vendor models** — open models won't match the closed frontier for long agentic runs. Opod serves what runs on your hardware; it does not proxy to vendors (that left core with ADR-022).
- **A SaaS product** — Opod is the software you run. The OSS is always complete.

---

## Architecture overview

```
   CLIENTS  (Cursor · Aider · Continue · OpenAI SDKs · curl)
                       │
                       ▼  one endpoint, one key
   ┌──────────────────────────────────────────────────┐
   │  LEADER  (`opod up`)                             │
   │  gateway   OpenAI-compatible · auth · rate limits│
   │  router    placement · load · fallback chain     │
   │  admin     /admin/v1 · event + usage streams     │
   │  store     SQLite (nodes, models, keys, usage)   │
   └────────────────────┬─────────────────────────────┘
                        │  HTTP on your LAN / VPN
        ┌───────────────┼──────────────────┐
        ▼               ▼                  ▼
   ┌────────────┐ ┌────────────┐    ┌──────────────────┐
   │ Worker A   │ │ Worker B   │    │ Workers C + D    │
   │ Linux+GPU  │ │ Mac Mini   │    │ one model, split │
   │ vLLM       │ │ MLX-LM     │    │ llama.cpp RPC    │
   └────────────┘ └────────────┘    └──────────────────┘
     `opod join`: register once, heartbeat every 5 s (HMAC per node)
```

See [ARCHITECTURE.md](ARCHITECTURE.md) for the full design.

---

## Features

### Inference

- OpenAI-compatible API (`/v1/chat/completions`, `/v1/embeddings`, `/v1/models`) — the only protocol surface (ADR-022)
- SSE streaming with proper client-disconnect handling (no goroutine leaks; bounded drain on cancel)
- Tool / function calling (pass-through for capable models)
- Vision (image input) on multimodal models — `image_url` content blocks on `/v1/chat/completions` route through the Ollama engine path
- Structured output (JSON schema)
- `model=auto` smart routing
- **Response cache** — embeddings cached against a sha256 of the canonicalized request body (object keys sorted; ephemeral fields stripped). Two drivers: in-memory LRU (default) and SQLite-backed (persists across leader restart). Per-request opt-out via `Cache-Control: no-cache` / `no-store`; per-tenant scoping via `opod.cache.namespace` body field. `X-Opod-Cache: hit | miss` response header.
- Typed `engine_unreachable` errors with engine name, endpoint, and start-hint (e.g. `ollama serve`) when the upstream engine isn't responding
- Engine health watchdog on auto-spawned engines (force-restart after 3 consecutive failures, covers hung llama-server)
- LoRA adapters on vLLM workers: configured through `OPOD_ADAPTERS`, added or dropped at runtime through `/admin/v1/adapters`, served as `<model>:<adapter>`
- Chat completion caching with streaming replay + semantic cache (planned)

### Cluster

- Sleep tier: a worker's engine can drop its GPU working set and keep the process (vLLM sleep mode) — `POST /v1/model/sleep|resume` on the worker, proxied by the leader as `/admin/v1/nodes/{id}/sleep|resume`; a sleeping worker is resident but not routed to until resumed

- One-command join — `opod join <leader-url>?token=…` registers a worker; heartbeats every 5 s carry its loaded models
- Placement by residency — a worker serves what its engine has loaded (`opod model add <id> --node a,b` pulls and warms it there); the router prefers local, then the least-loaded worker
- **Memory lifecycle** — admission control against live engine residency (a machine is never overcommitted), `opod model load --swap` with LRU evict-and-drain, `--pin` to protect a model, desired placements restored on restart, `opod down` releases engine memory by default, `--exclusive` for one-model-per-machine
- Heterogeneous sharding via llama.cpp RPC for models larger than any single node — `opod shard create <model> <N>` orchestrates the coordinator + every rpc-server end-to-end
- Live model migration (planned)
- Cross-platform workers: Mac (MLX), Linux+NVIDIA (vLLM, SGLang, llama.cpp), Linux+AMD (llama.cpp ROCm proven on Radeon; vLLM ROCm image built, unproven), CPU (llama.cpp)
- HA leader (planned)

### Multi-tenancy

- Per-user API keys with revocation, scopes (admin / user / node), and **TTL expiry** (`--ttl 7d`, `--expires-at 2026-07-01`, `opod token renew/expire`)
- Daily token quotas per key with usage metering
- **Per-key RPM + TPM rate limits** — leaky-bucket admission control; HTTP 429 with `Retry-After` + `X-RateLimit-Limit/Remaining/Reset-*` headers (OpenAI shape). Reconciles upfront token estimate against actual completion tokens after the response.
- **Per-key model allowlist** — pin a key to specific model ids (or a family via a trailing-`*` glob such as `qwen3-*`); unauthorized models return 403 `model_not_allowed` and the refusal is audit-logged
- Standard `X-RateLimit-*` headers on every `/v1/*` response + always-on `X-Opod-Request-Id` correlation token (also embedded in audit rows for traceability)
- OIDC / SSO — **not in core** (explicitly out of scope; see [ROADMAP.md](ROADMAP.md#deliberately-out-of-scope)). Core authenticates API keys only; per-user keys, quotas and the event stream cover accountability, and SSO belongs to the control plane

```bash
opod token create alice --models qwen-coder-7b,qwen3-14b   # restrict at creation
opod token create bob   --models 'qwen3-*,llama-*'         # model families via glob
opod token create dave  --rpm 60 --tpm 100000 --ttl 30d    # rate-limited + expiring
opod token edit k_abc --add-model gpt-oss-20b              # extend
opod token edit k_abc --remove-model qwen3-14b             # tighten
opod token renew k_abc --ttl 30d                           # extend expiry
```

### Observability

- Prometheus metrics endpoint (`/metrics`) — per-model RPS, latency, tokens, errors
- `GET /loadz` for an external autoscaler: in-flight, rpm, waking 503s, plus the workers' own engine signals (KV-cache %, queue depth, tokens/s, prefix-cache hits) gathered from their heartbeats
- Per-call usage records (model, protocol, tokens, latency, outcome — `ok`, `error` and `cancelled` alike: a caller who hangs up mid-stream still leaves a row) on `/admin/v1/usage/stream` (cursor + replay) — the control plane pulls and exports them
- Admin actions are recorded and streamed to the control plane on `/admin/v1/events/stream`
- OpenTelemetry / OTLP traces. Set `observability.otlp_endpoint` (or `OPOD_OTLP_ENDPOINT`) to your collector — e.g. `http://localhost:4318` — and Opod emits a full span hierarchy per request: `http.request` → `router.Chat` (covers the whole stream) → `router.Chat.attempt` (one per fallback retry) → `<engine>.Chat` (engine call with prompt/completion token counts). All four engine drivers (ollama, vllm, mlx, llamacpp) export the same span shape. W3C `traceparent` propagation is always on so Opod participates correctly between two services that both export. Empty endpoint = no-op (zero overhead beyond the NoopTracerProvider).
- OTLP logs. Set `OPOD_OTLP_LOGS_ENDPOINT` and the leader's own `slog` records also go to your collector (and through it to Loki, Splunk, Datadog, Elastic…), beside stderr, never instead of it. `log_level` applies to both; a slow or absent collector drops the oldest queued records and never delays a request.

### Developer experience

- One-line install (`curl | sh`)
- One-line model add (`opod model add qwen3.6-27b`) with a real progress bar and `--dry-run` preview
- One-line client config (`opod connect <tool>` prints the snippet)
- Interactive picker for `opod model add|info|remove` and `opod connect` — no need to memorize IDs
- Shell completion for bash / zsh / fish (`opod completion <shell>`)
- Sensible defaults, no required flags
- `--json` on the read commands, for scripts

---

## Supported models

> **For the complete per-model walkthrough** (system requirements, performance per platform, install + use snippets for every client) see **[MODELS.md](MODELS.md)**.

Opod ships a curated catalog of **47 open-weight models**, embedded in the binary from [`opod-io/opod-sdk/catalog`](https://github.com/opod-io/opod-sdk) (`opod catalog ls`, `opod catalog export <dir>`), spanning everything from 1 B edge models to 1 T-parameter sharded frontier MoE. Any other model also works via `opod model add hf:<owner>/<repo>` (HuggingFace direct) or `opod model add ollama:<name>` (any Ollama-pullable tag). See the SDK's `catalog/README.md` for the YAML schema if you want to PR an entry; a file dropped in `~/.opod/catalog/` or `$OPOD_CATALOG_DIR` adds or overrides one locally. `opod fetch <repo> <file> --dir <models dir>` makes one GGUF present in a models directory — exclusive per file (a second caller waits for the first), written to a temp sibling and renamed only when the byte count matches the server's, skipped when already there — the same pull a worker runs before launching llama-server.

> 📋 **Picker table — what to install** — full table with size, RAM, chat/code/reasoning/vision/audio/context ratings and license per model: **[MODELS.md → Picker table](MODELS.md#-picker-table--what-to-install)**.

### Shipped catalog at a glance

| Tier | Models |
|---|---|
| **Edge (≤4 GB RAM)** | `llama-3.2-1b`, `llama-3.2-3b`, `nomic-embed-text` (embeddings), `moondream3` (vision) |
| **Consumer big (16-32 GB)** | `gpt-oss-20b` ⭐, `qwen3.6-27b` ⭐, `gemma4-26b`, `gemma4-31b` (vision), `qwen3-30b`, `qwen3-coder-30b`, `qwen3-vl-32b` (vision), `qwen-coder-32b` |
| **Single 80 GB GPU** | `llama-3.3-70b-sharded`, `gpt-oss-120b`, `llama-4-scout` (10M ctx, multimodal), `glm-4.5-air-sharded` (agentic MoE) |
| **Sharded frontier (≥128 GB combined)** | `step-3.7-flash-sharded` ⭐ (Apache-2.0), `deepseek-v4-flash-sharded`, `nemotron-3-ultra-sharded` (Mamba-MoE, 1M ctx), `glm-4.6-sharded` (agentic coder), `glm-5.1-sharded`, `glm-5.2-sharded` (1M ctx), `kimi-k2.6-sharded` |

⭐ = current top picks (June 2026). The table is a selection of the 47 catalog entries; [MODELS.md](MODELS.md) has the full picker table with sizes, RAM floors, and licenses.

Run `opod model search` to list everything live with sizes and capabilities, or `opod model info <id>` for one model's full spec. Add `--sort=released` for newest-first, `--since 2026-01-01` to filter by date, or `--json` for machine-readable output. `opod model ls`, `opod status`, `opod config show` and `opod cache ls|prune` also accept `--json`. Running any `opod model add|info|remove` or `opod connect` with no ID launches an interactive picker (type to filter; arrow keys to navigate). Output is colored when stdout is a TTY; set `NO_COLOR=1` (or `OPOD_NO_COLOR=1`) to disable.

Aggregates (summaries, breakdowns) are computed by the control plane from the streams; core keeps only the raw facts.

Engine reliability: when Opod auto-spawned the engine itself (`opod up` with `OPOD_ENGINE=llamacpp`), a health watchdog polls every 30 s and force-restarts the process after three consecutive failures — so a hung `llama-server` no longer requires manual intervention. For user-managed engines (Ollama, vLLM) Opod leaves the process alone but `/v1/chat/completions` now returns a typed `engine_unreachable` error with the engine name, endpoint, and the exact command to start it (`ollama serve`, `mlx_lm.server …`, etc.) when the engine isn't responding.

### Roadmap — model families not yet in catalog

These work today via `opod model add hf:owner/repo` but don't have curated YAML entries with hardware specs:

- **Larger general / agent models** — Qwen3-235B, MiniMax-M2.7, MiMo-V2 sharded variants — pending sharded YAML entries.

Shipped recently (don't fall in this list):
- **Vision (image input)** — `gemma4-12b`, `gemma4-26b`, `gemma4-31b`, `gemma4-e2b`, `gemma4-e4b`, `qwen3-vl-8b`, `qwen3-vl-32b`, `pixtral-12b`, `moondream3`, `mimo-vl-7b`, `llama-4-scout` all serve through `/v1/chat/completions` with `image_url` content blocks.
- **Embeddings (for RAG)** — `/v1/embeddings` is live; install `nomic-embed-text` and call it from any OpenAI-shape embedding client.
- **Audio (input)** — `mimo-audio`, `gemma4-e2b`, `gemma4-e4b` declare `audio` capability for future routing; today they serve as `chat` models.

---

## Supported clients

`opod connect <client>` prints a copy-pasteable config snippet for each tool.

| Client | Protocol | Config |
|---|---|---|
| **Cursor** | OpenAI | Settings → Models → Override OpenAI Base URL |
| **Continue.dev** | OpenAI | `~/.continue/config.json` → `apiBase` |
| **Aider** | OpenAI | `aider --openai-api-base http://opod:8080/v1` |
| **Zed** | OpenAI | `language_models.openai_compatible.api_url` |
| **Cline / Roo Code** (VS Code) | OpenAI | Provider settings panel (OpenAI-compatible provider) |
| **OpenAI Python SDK** | OpenAI | `OpenAI(base_url=…, api_key=…)` |
| **LangChain / LlamaIndex** | OpenAI | `openai_api_base` |
| **OpenCode** | OpenAI | per-provider `baseURL` override |
| **curl** | HTTP | Direct |

---

## Hardware recommendations

### Solo / dev (1 node)

| Hardware | Models that fit | Good for |
|---|---|---|
| MacBook M2/M3, 16 GB | 3–7B Q4 | Autocomplete, learning |
| MacBook M3/M4 Pro, 24–36 GB | 7–14B Q4 | Real coding work |
| Mac Mini M4 Pro, 64 GB | up to 32B Q4 | Solo agent-grade |
| Linux + RTX 4090 (24 GB) | up to 32B AWQ | Solo agent-grade, batched |

### Team of ~10 (recommended)

| Role | Box | Cost |
|---|---|---|
| Big chat/agent model | Linux + 2× RTX 5090 (64 GB total), Threadripper, 128 GB RAM | ~$11k |
| Code completion #1 | Mac Mini M4 Pro 64 GB | ~$2k |
| Code completion #2 | Mac Mini M4 Pro 64 GB | ~$2k |
| Leader (gateway + router, no GPU needed) | Mac Mini base / NUC | ~$1k |
| Network | 10 GbE switch + cables | ~$0.5k |
| **Total** | | **~$16k** |

Serves ~10 heavy users with headroom. Power draw ~300 W idle, ~900 W peak. Fits one 20 A circuit. Breaks even vs. typical Claude/GPT spend in ~5 months.

### Larger team / production

- 1× H100 80 GB or 2× A100 80 GB for the flagship model
- 2× Mac Mini for completion
- 1× dedicated leader box

Serves 25–50 users comfortably.

---

## Installation

### Prerequisites — read first

Opod is a **gateway** — it doesn't include an LLM engine. You need one of:
- **Ollama** (recommended for most users; works on Mac + Linux + NVIDIA + CPU)
- vLLM (for NVIDIA GPUs at scale — Linux only)
- MLX-LM (for fastest perf on Apple Silicon)

> ⚠️ **Apple Silicon heads-up:** the Homebrew `ollama` formula is currently missing the internal `llama-server` binary — model inference fails with `500: llama-server binary not found`. Use the **cask** (`brew install --cask ollama`) or the official installer instead. The Opod installer detects this and warns you.

### macOS (Apple Silicon)

```bash
# 1. install Ollama (use cask, NOT plain `brew install ollama`)
brew install --cask ollama
open -a Ollama                      # starts the daemon

# 2. install Opod
curl -fsSL https://raw.githubusercontent.com/opod-io/opod-core/main/installer/install.sh | sh

# 3. add the install dir to PATH if the installer says so, e.g.:
export PATH="$HOME/.local/bin:$PATH"

# 4. start Opod
opod up
```

### Linux (x86_64 or arm64)

```bash
# 1. install Ollama
curl -fsSL https://ollama.com/install.sh | sh
sudo systemctl enable --now ollama   # or just: ollama serve &

# 2. install Opod
curl -fsSL https://raw.githubusercontent.com/opod-io/opod-core/main/installer/install.sh | sh

# 3. add install dir to PATH if needed
echo 'export PATH="$HOME/.local/bin:$PATH"' >> ~/.bashrc
source ~/.bashrc

# 4. start Opod
opod up
```

### What the installer does

1. Detects your OS + architecture (must be macOS/arm64, Linux/x86_64, or Linux/arm64)
2. Checks for required shell tools (curl, tar)
3. Checks whether Ollama is installed and warns with the install command if not
4. Detects the broken-Homebrew-ollama case on macOS and tells you how to fix it
5. Fetches the **latest release** binary from GitHub Releases
6. Verifies SHA-256 against `checksums.txt`
7. Installs to `~/.local/bin/opod` (or `/usr/local/bin/opod` with sudo)
8. Creates `~/.opod/catalog/` for your own catalog overrides (the bundled catalog is embedded in the binary)
9. Prints next steps + tells you if PATH needs updating

### Installer flags (after `| sh -s --`)

```bash
--help                  show usage
--version <vX.Y.Z>      install a specific version
--install-dir <path>    install to a specific dir
--no-engine             skip the Ollama check
--dry-run               show what would happen, no writes
```

### Installer env vars (alternative to flags)

```bash
# pin a specific version (skips the GH API lookup — also avoids the 60/hr rate limit)
curl -fsSL https://raw.githubusercontent.com/opod-io/opod-core/main/installer/install.sh \
  | OPOD_VERSION=v1.14.0 sh

# install to a custom dir
curl -fsSL https://raw.githubusercontent.com/opod-io/opod-core/main/installer/install.sh \
  | OPOD_INSTALL_DIR=/opt/opod/bin sh

# skip the Ollama check (CI, custom engine setups)
curl -fsSL https://raw.githubusercontent.com/opod-io/opod-core/main/installer/install.sh \
  | OPOD_SKIP_ENGINE=1 sh
```

Install **and** join a cluster in one command:

```bash
curl -fsSL https://raw.githubusercontent.com/opod-io/opod-core/main/installer/install.sh | \
    sh -s -- join https://leader.local:8080?token=<TOKEN>
```

### Upgrade / uninstall

```bash
# upgrade in place (no need to re-run the installer)
opod update              # downloads latest release, verifies SHA-256, swaps binary
opod update --check      # just check, don't install

# uninstall — remove binary, catalog, and data dir
rm -f ~/.local/bin/opod       # (sudo-installed? then /usr/local/bin/opod)
rm -rf ~/.opod                 # catalog + data + config (destructive)
```

### Build from source

```bash
git clone https://github.com/opod-io/opod-core
cd opod-core
go build -o opod ./cmd/opod
./opod version
```

Requires Go 1.25+. See [ARCHITECTURE.md → Build from source](ARCHITECTURE.md#build-from-source) for cross-compile + release builds.

### System requirements

- **macOS** 13+ on Apple Silicon (M1 or newer). Intel Macs not tested.
- **Linux** x86_64 or arm64 (Ubuntu 22.04+, Debian 12+, Fedora 39+, RHEL 9+).
- **Linux + NVIDIA**: NVIDIA driver 535+ (for vLLM); CUDA installed via the standard NVIDIA repos.
- **RAM**: 8 GB minimum, 16+ GB recommended; whatever model you load needs to fit.
- **Disk**: 50 GB for the binary + configs + small model cache; 200+ GB if you'll cache 70B-class models.
- **Network**: outbound HTTPS to GitHub + HuggingFace for downloading.

### Troubleshooting installation

| Symptom | Cause | Fix |
|---|---|---|
| `curl: (22) … 404` from installer | No release yet for your platform | Check https://github.com/opod-io/opod-core/releases ; specify `--version` if needed |
| `command not found: opod` after install | Install dir not on PATH | `export PATH="$HOME/.local/bin:$PATH"` in your shell rc |
| `opod up` works, but chat returns 502 `llama-server binary not found` | Homebrew `ollama` formula on Apple Silicon | `brew uninstall ollama && brew install --cask ollama` |
| `opod up` says "engine not reachable" | Ollama daemon not running | `ollama serve &` (Linux: `sudo systemctl start ollama`) |
| `Port 8080 in use` | Another process is using the port | `OPOD_LISTEN=:8081 opod up` |
| `checksum MISMATCH` | Corrupt download or tampering | Re-run installer; if it persists, file a security report (see SECURITY.md) |
| GH API rate-limited during install | Anonymous GH API limit (60/hr) | Wait, or set `OPOD_VERSION` to a release tag (e.g. `OPOD_VERSION=v1.20.1`) to skip the lookup |

---

## Configuration

Opod follows a strict "no config required for defaults" rule. Every flag has a sensible default. The config file is YAML at `~/.opod/config.yaml`, or use env vars (`OPOD_LISTEN`, `OPOD_DATA_DIR`, …).

### Minimal config (auto-generated on first `opod up`)

```yaml
# ~/.opod/config.yaml
listen: ":8080"
data_dir: "~/.opod"
auth:
  require_keys: true   # set false for local-only dev mode
```

The initial admin key is auto-generated on first `opod up` and printed to stderr — copy it then. There is no `auth.initial_admin_key` field; the key lives in the SQLite store, not the YAML.

### Full reference

Every field below is parsed by `internal/config/config.go`. Anything not in this list is silently ignored.

```yaml
listen: ":8080"                       # HTTP listen address (used by leader and workers)
external_url: ""                      # public URL printed by `opod up` and embedded in `opod connect` snippets; empty → use listen addr
data_dir: "~/.opod"                  # root for state.db, models, logs
log_level: "info"                     # debug | info | warn | error
catalog_dir: ""                       # empty → built-in catalog/ directory
max_body_bytes: 0                     # request-body cap on /v1/* in bytes;
                                      # 0 → built-in 32 MiB ceiling

storage:
  type: "sqlite"                      # only sqlite ships today
  dsn: "~/.opod/state.db"
  models_dir: "~/.opod/models"

auth:
  require_keys: true                  # set false to disable API-key auth (dev only)

engine:
  preferred: "ollama"                 # ollama | vllm | mlx | llamacpp
  ollama_endpoint:   "http://127.0.0.1:11434"
  vllm_endpoint:     "http://127.0.0.1:8000"
  mlx_endpoint:      "http://127.0.0.1:8080"
  llamacpp_endpoint: "http://127.0.0.1:8089"   # llama-server (single-node or RPC coordinator) — port chosen to avoid Opod leader :8080 and worker :8081

router:
  default_model: ""                   # empty → auto-pick on first up
  sticky_sessions: true               # legacy boolean; superseded by the TTL below
  sticky_session_ttl_seconds: 0       # >0 → pin (user, model) to its last worker
                                      # for this many seconds (KV-cache reuse)
  placement_allowed_fails: 0          # consecutive engine errors before a worker
                                      # is parked in cooldown (circuit breaker);
  placement_cooldown_seconds: 0       # both must be >0 to enable
  hedge_replicas: 0                   # >1 → allow per-request hedging across the
                                      # N least-loaded workers (`opod.hedge: true`)
  latency_fallback_p95_seconds: 0     # 0 = disabled. When >0, the router
                                       # walks the catalog `fallback:` chain
                                       # for a faster candidate FIRST whenever
                                       # the primary's recent p95 latency
                                       # exceeds this many seconds. Bet #1.
  # vendor fallback (claude-*/gpt-* proxying) left core with ADR-022 — nothing leaves your network
observability:
  otlp_endpoint: ""                   # e.g. http://localhost:4318 — empty disables tracing (no-op overhead)
  response_cache:
    enabled: false                    # cache embeddings responses by request hash
    driver: "memory"                  # memory | sqlite
    max_entries: 0                    # memory driver only; 0 → 1000
    default_ttl_seconds: 0            # 0 → 24h

placement:                            # memory lifecycle for this node's local engine
  exclusive: false                    # true → one resident model per machine: every
                                      # load evicts all other non-pinned models first
  reserve_percent: 20                 # % of total RAM held back from the admission
  drain_timeout_seconds: 30           # max wait for in-flight requests before an
                                      # eviction unloads anyway
```

### Environment variables

| Var | Overrides |
|---|---|
| `OPOD_LISTEN` | `listen` |
| `OPOD_DATA_DIR` | `data_dir` |
| `OPOD_LOG_LEVEL` | `log_level` |
| `OPOD_EXTERNAL_URL` | `external_url` |
| `OPOD_ENGINE` | `engine.preferred` |
| `OPOD_OLLAMA_ENDPOINT` / `OPOD_VLLM_ENDPOINT` / `OPOD_MLX_ENDPOINT` / `OPOD_LLAMACPP_ENDPOINT` | corresponding `engine.*_endpoint` |
| `OPOD_VLLM_API_KEY` | bearer token sent to a vLLM server (no YAML equivalent). The old unprefixed `VLLM_API_KEY` still works as a deprecated fallback; the prefixed form wins when both are set |
| `OPOD_REQUIRE_KEYS` | `auth.require_keys` (truthy `1/true/yes`) |
| `OPOD_DEFAULT_MODEL` | `router.default_model` |
| `OPOD_CATALOG_DIR` | `catalog_dir` — a catalog directory merged over the bundled one: every directory that exists is merged and a later one wins on an id collision, in the order `./catalog` → `<exe-dir>/catalog` → `/usr/local/share/opod/catalog` → `/usr/share/opod/catalog` (.deb/.rpm) → `$OPOD_CATALOG_DIR` → `~/.opod/catalog` (user overrides, most authoritative) |
| `OPOD_OTLP_ENDPOINT` | `observability.otlp_endpoint` (OTLP/HTTP collector URL or bare `host:port`) |
| `OPOD_OTLP_LOGS_ENDPOINT` | the leader's own log records over OTLP/HTTP to a collector (URL or bare `host:port`; no YAML equivalent). stderr keeps working; the queue is bounded and never blocks. Empty = off |
| `OPOD_COORDINATOR_NODE` | which node hosts the `llama-server` coordinator for sharded models; `local` forces leader, otherwise a node id. Default: highest-RAM worker. |
| `OPOD_REJECT_BEARER` | set to `1` on a worker to refuse the bearer-fallback auth path and require HMAC for every `/v1/process/*` call. Use once every leader supports HMAC node auth (any current release). |
| `OPOD_LATENCY_P95_SECONDS` | `router.latency_fallback_p95_seconds` — when primary p95 exceeds this, prefer a faster fallback. 0 = disabled (default) |
| `OPOD_EXCLUSIVE` | `placement.exclusive` (truthy `1/true`) — one resident model per machine |
| `OPOD_PLACEMENT_DRAIN_TIMEOUT_SECONDS` | `placement.drain_timeout_seconds` — eviction drain bound (default 30) |
| `OPOD_UNLOAD_ON_EXIT` | `1` → Ctrl-C of `opod up` also unloads engine-resident models (the `opod down` path already does this by default) |
| `OPOD_SKIP_SOURCE_CHECK` | `1` → skip the pre-flight HEAD probe that `opod model add` runs against the upstream registry (use for air-gapped mirrors / custom registries) |

### Not yet configurable (roadmap)

These features are mentioned elsewhere in this README but have no YAML knob today. The list is here so you don't waste time guessing.

- **Mesh backend selection** — only the LAN backend ships today; there are no `mesh.*` config keys. The `tailscale` (tsnet) backend has an interface defined in `internal/mesh/` but no implementation. Tracked in [ROADMAP.md](ROADMAP.md).
- **OIDC / SSO** — out of scope for core (see [ROADMAP.md → Deliberately out of scope](ROADMAP.md#deliberately-out-of-scope)). `internal/auth/` ships API keys only.
- **Scheduler policy / replication** — `internal/scheduler/` ships sharding orchestration + GGUF distribution; placement is naive least-loaded with no policy tunables.
- **Separate metrics listener** — Prometheus is hardcoded to the main `/metrics` endpoint on the gateway port; there's no dedicated metrics listener. (OTLP tracing *is* configurable — `observability.otlp_endpoint` / `OPOD_OTLP_ENDPOINT` above.)
- **Per-node config** — `~/.opod/node.yaml` only remembers the worker's node id between runs; it holds no settings. A worker takes its engine endpoints from its own `config.yaml` or env vars.

### Per-node engine override

Workers run their own engine binary. To point a worker at a non-default endpoint, set env vars before `opod join`:

```bash
OPOD_ENGINE=vllm OPOD_VLLM_ENDPOINT=http://127.0.0.1:8000 opod join http://leader:8080?token=...
```

---

## Cluster operations

### Start the leader

```bash
opod up
```

Idempotent. Re-running it shows status if already running.

### Add a node

1. On the leader: `opod token create --node`
2. On the new machine: `curl -fsSL https://raw.githubusercontent.com/opod-io/opod-core/main/installer/install.sh | sh -s -- join <leader-url>?token=<token>`

The token is a node-scoped key (`sk-orc-<your-key>`): the shared secret between leader and worker, so mint it only on a network you trust and give it an expiry with `--ttl` if you like. The new node registers with the leader over HTTP, heartbeats every 5 s, and from then on signs its calls with a per-node HMAC. `opod join --gpu <index> --vram-budget <GB>` pins the worker to one GPU and a memory budget when several workers share a machine.

### Remove a node

```bash
opod node drain <node-id>   # stop routing new requests to it
opod node remove <node-id>  # forget it
```

**The node's weight cache.** Models are pulled once per node into `$OPOD_MODELS_DIR` and shared by every worker on it.
`opod cache` is how that space is reclaimed safely: it only ever considers files this binary fetched (each carries a
marker written at download time), it takes the same per-file lock a download takes, and it defaults to a dry run.

```bash
opod cache ls                                   # what is cached, what is ours, when it was last used
opod cache prune --keep qwen3-14b-q4.gguf       # what would go if nothing else referenced it
opod cache prune --keep … --min-age 24h --apply # actually delete, oldest unused first
```

A file nothing references still survives until it has gone unused for `--min-age`, so a rollback inside that window
finds its weights warm. A file without that marker is never deleted, whatever the disk pressure.

### End-to-end multi-node walkthrough

For a leader + one worker on the same LAN:

```bash
# === on the leader machine ===
brew install --cask ollama          # working Ollama (not the broken formula)
ollama serve &
opod up                            # bootstraps admin key, starts gateway on :8080
opod model add llama-3.2-3b        # pulls on the leader's Ollama
opod token create --node           # prints the worker join token

# === on the worker machine ===
brew install --cask ollama
ollama serve &
opod join http://<leader-host>:8080?token=<token>   # registers + starts worker HTTP server
opod model add qwen-coder-7b        # pulls on the worker's Ollama (reported back via heartbeat)

# === back on the leader ===
opod node ls                        # both nodes visible
# requests for "llama-3.2-3b" stay local
# requests for "qwen-coder-7b" get proxied to the worker automatically

# === from your laptop ===
curl http://<leader-host>:8080/v1/chat/completions \
  -H "Authorization: Bearer sk-orc-..." \
  -d '{"model":"qwen-coder-7b","messages":[{"role":"user","content":"hi"}]}'
# served by the worker, transparently
```

### Sharded models (split one brain across multiple machines)

For a model too large to fit on any single machine, Opod can split it across N workers using `llama.cpp`'s RPC backend. Opod orchestrates the whole thing — no SSHing into each box.

**Prereqs:**
- `brew install llama.cpp` on the leader (provides `llama-server` for the coordinator).
- `rpc-server` on PATH on every worker that will host a shard. (At time of writing this binary needs a source build of llama.cpp with `cmake --preset rpc`; the Homebrew bottle doesn't include it yet.)
- A catalog entry with `sharding.required: true` and `source.path` pointing at a local GGUF file the leader can read (see `catalog/llama-3.3-70b-sharded.yaml`).
- N workers already joined and `ready` (`opod node ls`).

**One command on the leader:**

```bash
opod model add llama-3.3-70b-sharded
# auto-detects sharding.required=true → delegates to `opod shard create`

# or explicitly:
opod shard create llama-3.3-70b-sharded 2
```

What Opod does:

1. Picks the 2 workers with the most free RAM
2. Sends `POST /v1/process/start` to each worker → launches `rpc-server -p 50052`
3. Waits for both rpc-servers to be TCP-reachable (readiness probe)
4. On the coordinator host, launches `llama-server -m <gguf> --rpc <worker1>:50052,<worker2>:50052 --port 9001`
5. Waits for the coordinator to be reachable
6. Persists shard rows + a `placements` row pointing the model at the coordinator
7. The Router routes any request for `llama-3.3-70b-sharded` to the coordinator, which fans out to the rpc-server shards internally

**Manage from the CLI:**

```bash
opod shard ls                              # show every shard + coordinator
opod shard remove llama-3.3-70b-sharded    # stops coordinator + every rpc-server, deletes rows
```

`opod shard create <model> --nodes a,b,c` pins the parts to named workers instead of letting Opod pick; the same routes are `GET|POST|DELETE /admin/v1/shards…` for a manager.

**Caveats:**
- Shard crash recovery is automatic for up to 5 restarts with exponential backoff (1s, 2s, 4s, 8s, 16s). After that the process enters `crashloop` state and the admin must intervene — typically by re-running `opod shard create`. Both `rpc-server` and the `llama-server` coordinator restart this way. See `internal/agent/supervisor.go`.
- The coordinator runs on the shard worker with the most RAM (the leader only when there are no workers). `OPOD_COORDINATOR_NODE=<node-id>` pins it; `local` forces the leader.
- Worker bin-packing is naive (descending free-RAM); doesn't factor GPU memory or current load.

### List nodes

```bash
opod node ls
# ID            HOSTNAME      HARDWARE          ENGINE   MODEL              STATE
# n_abc123      mac-mini-1    M4 Pro / 64 GB    mlx      qwen-coder-14b     ready
# n_def456      gpu-tower     2× RTX 5090       vllm     qwen3-30b          ready
# n_ghi789      lab-mac       M2 Pro / 32 GB    mlx      —                  idle
```

### Inspect a node

```bash
opod node show n_abc123
```

Prints the node record as JSON: hardware, engine, state, last heartbeat and the models its engine has loaded.

---

## Managing models

### Browse the catalog

```bash
opod model search coding
opod model search vision
```

### Add a model

```bash
opod model add qwen3-coder-30b                  # from catalog
opod model add hf:Qwen/Qwen3-72B-AWQ            # from HuggingFace (scheme prefix)
opod model add ollama:phi3:mini                 # any Ollama tag
opod model add file:/abs/path/my-finetune.gguf  # pre-downloaded GGUF
opod model add --from ./my-model.yaml           # from a user-supplied catalog YAML
```

**Four ways to install a model:**

1. **Curated catalog** — the 47 entries embedded from `opod-sdk/catalog` (`opod catalog ls`). Hardware-floor checks apply.
2. **Scheme prefix** (`hf:` / `ollama:` / `file:`) — one-liner for anything the engine supports; no hardware check.
3. **`--from <my.yaml>`** — install from a user-written catalog YAML. The file is copied into `~/.opod/catalog/` so it persists and shows up in `opod model search` / `info` next run.
4. **Drop-in dir** — write a YAML to `~/.opod/catalog/<id>.yaml` directly, then `opod model add <id>` treats it like a built-in entry. Same schema as `catalog/*.yaml` (`id`, `display_name`, `source.{type,repo,ollama_name,path}`, `hardware`, `capabilities`).

This:
1. Checks `catalog/<id>.yaml`'s `hardware.min_ram_gb` (and `min_vram_gb`) against the cluster — installs that overshoot the floor are refused with a clear error. Pass `--force` to override (e.g. when you know swap or a quantization knob will save you).
1. **Verifies the upstream exists** — a 5s HEAD against the Ollama registry / HuggingFace. A typo'd `hf:owner/repo` or renamed tag is refused immediately with the URL that 404'd, instead of "succeeding" and failing later at engine launch. Network trouble only warns (never blocks); `OPOD_SKIP_SOURCE_CHECK=1` for air-gapped mirrors. `--dry-run` shows the same check as a "Source check" row.
2. Records the model in the registry
3. Picks the best node(s) to host it (or shards across multiple)
4. Pulls the weights to those nodes (with resume support)
5. Launches the right inference engine
6. Flips the gateway routing to make the model available

### List active models

```bash
opod model ls
# ID                     STATUS     SOURCE                         INSTALLED
# qwen-coder-14b         ready      ollama:qwen2.5-coder:14b       2026-06-01T10:12:44Z
# qwen3-30b              ready      ollama:qwen3:30b               2026-06-02T08:03:10Z

opod model ps          # what is resident in engine memory right now, and how much is free
```

### Load, unload, and pin (memory lifecycle)

Installed ≠ resident: a model on disk loads into RAM on its first request. The
memory commands control what occupies RAM *right now*, with admission control
so a machine is never overcommitted:

```bash
opod model load qwen-coder-14b         # bring into RAM now; REFUSES if it doesn't fit
opod model load qwen-coder-14b --swap  # evict least-recently-used models to make room
opod model load nomic-embed-text --pin # pinned: never evicted, no idle TTL
opod model unload qwen-coder-14b       # drain in-flight requests, then release RAM
```

How it works:

- **Admission** checks the model's footprint (weights + ~20% overhead) against
  (total minus a 20% OS reserve, tunable via `placement.reserve_percent`).
- **`--swap`** evicts only as many models as needed, least-recently-used first
  (from the usage log), never pinned ones. Victims are **drained** — the router
  stops sending them requests and in-flight ones finish (up to
  `placement.drain_timeout_seconds`, default 30) — then unloaded and
  audit-logged (`model_evicted`). Evicted models stay installed and reload on
  demand.
- **Loaded/pinned models are remembered** (desired placements in SQLite) and
  restored in priority order on the next `opod up`.
- **`opod up --exclusive`** (or `placement.exclusive: true`, or
  `OPOD_EXCLUSIVE=1`) enforces one resident model per machine: every load
  evicts all other non-pinned models first. Good for single-GPU boxes.
- **`opod down` releases memory by default** — it's a deliberate teardown.
  Ctrl-C of `opod up` keeps models warm for fast dev restarts (Ollama's
  ~5-min idle TTL frees them anyway); opt into immediate release there with
  `--unload-on-exit`.

> Residency reporting requires Ollama today. Other engines degrade gracefully:
> processes are killed (memory freed) on shutdown by the process supervisor.

### Remove a model

```bash
opod model remove qwen-coder-14b
```

### LoRA adapters

LoRA adapters are loaded from the worker's `OPOD_ADAPTERS` environment and can be added or dropped at runtime through the leader's `/admin/v1/adapters`; there is no `opod model adapter` CLI verb yet.

---

## Connecting clients

### Fastest: `opod connect <client>`

```bash
opod connect cursor                               # OpenAI-shape: Cursor, Aider, Zed, OpenClaw, Codex CLI, …
opod connect open-webui                           # Open WebUI: a self-hosted chat front end (Docker)
opod connect open-notebook                        # OSS NotebookLM clone (sources → chat + podcast)
opod connect goose                                # Block's OSS terminal agent
opod connect plandex                              # terminal-native agentic planner (MIT)
opod connect openhands                            # autonomous coding agent (formerly OpenDevin)
opod connect codex-cli                            # OpenAI's official CLI
opod connect opencode                             # terminal coding agent w/ per-provider baseURL
opod connect --list                               # full client roster (15 today)

# Overrides
opod connect cursor --model qwen-coder-14b        # suggest a specific model
opod connect aider --base-url https://opod.lan   # override gateway URL
OPOD_TOKEN=sk-orc-<your-key> opod connect aider           # use a non-default token
opod connect aider --token sk-orc-<your-key>               # same, via flag
```

Anything that speaks the OpenAI API shape connects with one line. The full roster today: **cursor**, **aider**, **continue**, **zed**, **cline**, **openclaw**, **opencode**, **open-webui**, **open-notebook**, **goose**, **plandex**, **openhands**, **codex-cli**, **openai-sdk**, **curl**. Tools that only speak the Anthropic Messages shape (Claude Code, qwen-code, hermes) need a protocol shim in front of the gateway; core does not ship one (ADR-022).

Token comes from `--token`, then `$OPOD_TOKEN`, then `~/.opod/admin.key` (written when you ran `opod up`). Base URL comes from `--base-url`, then `external_url` in `~/.opod/config.yaml`, then `http://localhost:<listen>`.

### Reversing: `opod disconnect <client>`

```bash
opod disconnect aider              # prints the unset commands for the env vars connect set
opod disconnect cursor             # GUI steps to clear the override
opod disconnect --list             # same 15 clients
```

Prints the exact commands to roll back whatever `opod connect` set up — does NOT modify any shell, editor, or config file. You run the commands when you're ready. Once disconnected, the client talks straight to its vendor again (`api.openai.com`); nothing about your Opod host needs to change. Re-run `opod connect <client>` anytime to go back.

### For a teammate: `opod token create <name>`

```bash
opod token create hadi            # a user-scope API key, printed once
opod connect cursor --token <key> # the snippet for their tool, with that key
```

### No dashboard in core

Core is CLI-only since ADR-022: `/` answers 404, and nothing in this binary serves a page. A console — connect cards, a playground, keys, teams — is the control plane's (`opodcp`), a separate product that drives core through `/admin/v1`.

### Reference snippets (manual)

If you can't run `opod connect`, the snippets below are the same content you'd get from the CLI. Substitute your own base URL + token where shown.

### Cursor

Settings → Models → Add Model:
- Name: `opod`
- Provider: OpenAI Compatible
- Base URL: `http://<leader-host>:8080/v1`
- API Key: `sk-orc-<your-key>`

### Continue.dev

`~/.continue/config.json`:

```json
{
  "models": [
    {
      "title": "Opod - Qwen3-Coder",
      "provider": "openai",
      "model": "qwen3-coder-30b",
      "apiBase": "http://<leader-host>:8080/v1",
      "apiKey": "sk-orc-<your-key>"
    }
  ]
}
```

### Aider

```bash
aider --openai-api-base http://<leader-host>:8080/v1 \
      --openai-api-key sk-orc-<your-key> \
      --model openai/qwen3-coder-30b
```

### OpenAI Python SDK

```python
from openai import OpenAI

client = OpenAI(
    base_url="http://<leader-host>:8080/v1",
    api_key="sk-orc-<your-key>",
)

resp = client.chat.completions.create(
    model="auto",
    messages=[{"role": "user", "content": "write a haiku about caching"}],
)
print(resp.choices[0].message.content)
```

## API reference

### OpenAI surface

| Method | Path | Notes |
|---|---|---|
| `POST` | `/v1/chat/completions` | Streaming + non-streaming; accepts `image_url` content blocks (Ollama path). Returns typed `engine_unreachable` errors with engine name + start hint when the upstream engine is down. |
| `POST` | `/v1/embeddings` | Ollama embedding models (e.g. `nomic-embed-text`) |
| `GET` | `/v1/models` | Lists available models |

(Planned: `/v1/completions`.)

### Opod admin surface

| Method | Path | Notes |
|---|---|---|
| `GET` | `/healthz` `/readyz` | Liveness / readiness (`/readyz` modes: local engine · `router-only` · `sleeping` · `sleeping-workers` · `shard-coordinator`). No key |
| `GET` | `/loadz` | Load for an external autoscaler: in-flight, rpm, waking 503s, plus the workers' engine signals (`kv_used_pct`, `queue_depth`, `tokens_per_s`, `prefix_hit_pct`, `workers`, `reporting`). No key |
| `GET` | `/metrics` | Prometheus exposition. No key |
| `GET` | `/admin/v1/version` | Binary version + contract version |
| `GET` | `/admin/v1/capabilities` | The frozen route list, feature flags, and the engine drivers linked into this binary |
| `GET` | `/admin/v1/status` | The compact summary behind `opod status --json` |
| `GET` | `/admin/v1/config` | Effective config, secrets redacted (read-only: config is edited in the file) |
| `POST` | `/admin/v1/healthcheck` | One real completion through the gateway: did it answer, how fast, which engine |
| `GET` | `/admin/v1/nodes` | List nodes |
| `POST` | `/admin/v1/nodes/register` | (scope=admin or node) Worker registration |
| `POST` | `/admin/v1/nodes/heartbeat` | (scope=admin or node) Worker heartbeat with loaded models, the engine's load sample and whether it sleeps |
| `POST` | `/admin/v1/nodes/{id}/drain` | Mark node as draining |
| `POST` | `/admin/v1/nodes/{id}/sleep` `/resume` | Sleep tier: the worker's engine drops its GPU working set / wakes (vLLM sleep mode; engines without one answer 501 `unsupported`) |
| `DELETE` | `/admin/v1/nodes/{id}` | Forget a node |
| `GET` | `/admin/v1/models` | List installed models |
| `GET` | `/admin/v1/catalog` | List catalog entries |
| `POST` | `/admin/v1/models` | Install a model (auto-delegates to shard orchestration if `sharding.required`; `nodes` pulls and warms it on named workers) |
| `DELETE` | `/admin/v1/models/{id}` | Uninstall (auto-handles sharded teardown) |
| `POST` | `/admin/v1/models/{id}/load` | Bring a model into engine memory under admission control (`swap`, `pin`, `priority`); 409 `needs_swap` / `blocked_by_pinned`, 422 `impossible` |
| `POST` | `/admin/v1/models/{id}/unload` | Drain in-flight requests, drop the model from engine RAM (weights stay; cleared from desired placements so it stays unloaded across restarts). Engines that don't support it return `status:"noop"` |
| `GET` | `/admin/v1/memory` | Live engine residency + the desired set (what `opod model ps` prints) |
| `POST` | `/admin/v1/adapters` | Load a LoRA adapter on every worker holding the base model, no restart |
| `DELETE` | `/admin/v1/adapters/{name}` | Drop one |
| `GET` | `/admin/v1/tokens` | List API keys (no hash, no plaintext) |
| `POST` | `/admin/v1/tokens` | Create a key — returns plaintext ONCE |
| `PATCH` | `/admin/v1/tokens/{id}` | Edit a key's model allowlist / rate limits / expiry |
| `DELETE` | `/admin/v1/tokens/{id}` | Revoke a key |
| `GET` | `/admin/v1/shards` | List shards across all models |
| `GET` | `/admin/v1/shards/processes` | Process state of the parts the leader supervises (`running` · `starting` · `stopped` · `failed` · `crashloop`) |
| `POST` | `/admin/v1/shards/create` | Orchestrate a sharded model |
| `DELETE` | `/admin/v1/shards/{model_id}` | Tear down a sharded model |
| `GET` | `/admin/v1/usage/stream` | Per-call usage records, by cursor with replay — the raw facts; summaries are a consumer's job |
| `GET` | `/admin/v1/events/stream` | Lifecycle + admin-action events, by cursor with replay |
| `GET` | `/admin/v1/events` | Server-Sent Events. Push-on-change for `models` / `nodes` / `shards` topics. Sends a 25 s `keepalive` comment so proxies don't idle. Auth via Bearer or `?key=` query param. |
| `GET` | `/admin/v1/cache/stats` | Response-cache driver + counters |
| `DELETE` | `/admin/v1/cache` | Flush the response cache (`?namespace=…` or `?all=1`) |

All `/admin/v1` routes require an admin key (`opod token create --admin`), except the worker pair, which also accepts a node key. The subset an external manager may rely on across releases is frozen in `internal/controlplane/contract.go` and served by `/admin/v1/capabilities`; see [ARCHITECTURE.md → Stable admin surface](ARCHITECTURE.md#stable-admin-surface-v1).

### Model routing rules

`model` field in the request determines backend:

| Model name | Routes to |
|---|---|
| exact catalog ID (`qwen3-coder-30b`) | local cluster, that model |
| `auto` | local; gateway picks based on heuristics |
| `hf:…` | local, if the model is loaded |

---

## CLI reference

The CLI is the whole interface: every admin action is a command here, and each one calls an `/admin/v1` route you can also script. Most subcommands launch an interactive picker (type to filter, ↑↓/enter) when called with no argument or an unknown ID, so you rarely need to memorize an ID.

```
# --- lifecycle ---
opod up [--no-wizard] [--auto-pull=false] [--exclusive] [--unload-on-exit]
                                  Start the local node (first-run wizard picker
                                  installs a starter model unless --no-wizard is
                                  set; --exclusive = one resident model per machine)
opod down [--no-unload]          Stop the local node and release engine memory
                                  (--no-unload leaves models resident)
opod status [--json]             Show local + cluster status
opod join <url>?token=… [--gpu N] [--vram-budget GB]
                                  Join an existing cluster as a worker (optionally
                                  pinned to one GPU and a memory budget)
opod doctor                      Diagnose common problems
opod update [--check]            Check / install the latest Opod release
opod upgrade                     Alias for `update`
opod completion <bash|zsh|fish>  Print a shell completion script
opod version                     Print version

# --- nodes ---
opod node ls                     List nodes
opod node show <id>              Inspect a node
opod node drain <id>             Drain a node (no new requests routed to it)
opod node remove <id> [--yes]    Forget a node (prompts unless --yes)

# --- models (non-sharded) ---
opod model search [q] [--sort=released] [--since YYYY-MM-DD] [--json]
                                  Search catalog with optional date filters
opod model ls [--json]           List installed models
opod model add <id> [--force] [--dry-run] [--node a,b] [--from file.yaml]
                                  Install a model. --dry-run previews size/RAM/
                                  engine/ETA without pulling weights; --node pulls
                                  and warms it on the named workers.
opod model info <id> [--json]    Full details for one catalog model
opod model remove <id> [--yes]   Uninstall a model (prompts unless --yes)
opod catalog ls                  List the catalog embedded in the binary
opod catalog export <dir>        Write it out as files (overrides go beside them)

# --- memory lifecycle (which models occupy RAM right now) ---
opod model load <id> [--swap] [--pin] [--priority N]
                                  Bring a model into engine RAM with admission
                                  control: refuses when it doesn't fit; --swap
                                  evicts least-recently-used models (drained
                                  first, audit-logged); --pin exempts from
                                  eviction + the engine's idle TTL. Loaded/pinned
                                  models are restored on the next `opod up`.
opod model unload <id>           Drain, then drop from engine RAM (weights stay
                                  on disk; stays unloaded across restarts)
opod model ps                    Models resident in engine memory + what is free

# --- weights on a node ---
opod fetch <hf-repo> <file> [--dir D] [--revision R] [--sha256 S]
                                  Make one GGUF present: exclusive per file, atomic,
                                  digest-checked, skipped when already there
opod fetch --snapshot <hf-repo>[@rev] [--dir D]
                                  The same for a safetensors model: the file set
                                  vLLM / SGLang loads, under <dir>/<repo>@<rev>/
opod cache ls [--json]           What is cached, what is ours, when it was last used
opod cache prune [--keep a,b] [--min-age 24h] [--target-free GB] [--apply]
                                  Reclaim space; a dry run unless --apply, and never
                                  a file this binary did not fetch

# --- sharded models (one model split across N machines) ---
opod shard create <model> [N] [--nodes a,b,c] [--tp N] [--pp N]
                                  Orchestrate a sharded model across N workers
                                  (--nodes pins them; --tp/--pp for a vLLM gang)
opod shard ls                    List shards across all sharded models
opod shard remove <model> [--yes]  Tear down a sharded model (prompts unless --yes)

# --- API keys / tokens ---
opod token create [name]         Issue an API key (--admin, --node, --models a,b,
                                  --rpm N, --tpm N, --ttl 7d, --expires-at DATE)
opod token ls                    List API keys
opod token edit <id>             Change a key's model allowlist / rate limits
                                  (--add-model, --remove-model, --set-models,
                                  --clear-models, --rpm, --tpm)
opod token renew <id>            Extend expiry (--ttl DURATION | --expires-at DATE)
opod token expire <id> [--in D]  Expire a key now (or in DURATION, e.g. --in 1h)
opod token revoke <id>           Revoke a key

# --- connecting clients ---
opod connect <client>            Print the copy-paste config snippet for a client
                                  (--list shows the 15-client roster; --model,
                                  --base-url, --token overrides)
opod disconnect <client>         Print the rollback commands for a client (--list)

# --- config ---
opod config show [--json]        Show effective runtime config (secrets redacted)
opod config path                 Print config file path
opod config edit                 Print the editor command for the config file
```

Output is colored when stdout is a TTY. Set `NO_COLOR=1` (or `OPOD_NO_COLOR=1`) to disable. Top-level subcommand typos get a "did you mean ..." suggestion via Damerau-Levenshtein over the registered subcommand list.

---

## Troubleshooting

### `opod up` fails to start

```bash
opod doctor
```

Common issues:

- Port 8080 in use → set `listen: ":8081"` in config
- macOS firewall blocking the listen port → System Settings → Network → Firewall → allow incoming connections for `opod`
- Insufficient memory → pick a smaller model (`opod model add llama-3.2-3b`)

### A node won't join

- Token revoked or expired — mint a fresh one on the leader: `opod token create --node`
- Clock skew >5 minutes between leader and node (the HMAC replay window) — fix NTP
- Leader unreachable from the worker — joining happens over plain LAN HTTP today (no built-in tailnet/mesh; there are no `mesh.*` config keys). Make sure the worker can reach `http://<leader-host>:8080` directly (same LAN or VPN, no firewall in between)

### Slow inference

- Check the load the workers report: `curl http://<leader-host>:8080/loadz` (KV-cache use, queue depth, tokens/s). Saturated under load: add a worker with the same model, or upgrade.
- Sticky sessions disabled? Re-enable for better KV cache reuse.
- Model is CPU-falling-back? Check the leader's stderr where `opod up` is running — engine driver errors are logged there. Per-node log streaming is on the roadmap.

### A client shows "model not found"

- Make sure the model ID in your request matches an installed catalog ID (`GET /v1/models` lists them); core does not proxy vendor model ids.
- `opod model ls` to confirm what's loaded.

### Slow inference?

- Check engine reachability: `opod doctor`
- Add a node + install the model there: `opod node` / `opod model add` (router auto-load-balances)
- For sharded large models: `opod shard create`

---

## FAQ

**Can I run Claude or GPT on my hardware?**
No — those are closed-weight proprietary models, and core does not proxy to vendor APIs (that surface left with ADR-022). Opod serves open-weight models on your hardware; the control plane can front a vendor as a *remote endpoint* when you want one in the same fleet.

**Do I need a GPU?**
For real coding work, yes — either an NVIDIA GPU on Linux or an Apple Silicon Mac. CPU-only works via llama.cpp for tiny models (3B and under) and is useful for testing only.

**Can I mix Macs and NVIDIA boxes in one cluster?**
Yes. That's a core design goal. The scheduler treats them as distinct pools and assigns models that fit each.

**Does Opod work without internet?**
Yes, after initial model download. Nodes talk to the leader directly over your LAN — there's no external coordination service to reach. For air-gapped installs, point `opod model add` at a local mirror and set `OPOD_SKIP_SOURCE_CHECK=1` to skip the upstream-registry probe.

**How is this different from Ollama?**
Ollama is a great single-node inference engine. Opod is the *orchestration layer* across many machines. Opod uses Ollama as one of its supported engine backends.

**How is this different from vLLM?**
vLLM is a single-node inference server. Opod orchestrates vLLM (and others) across your fleet.

**How is this different from exo?**
exo is the closest project conceptually. Opod differs by: (1) an OpenAI-compatible gateway with per-key quotas, (2) explicit placement + llama.cpp-RPC sharding, (3) multi-tenant API keys, quotas and rate limits, (4) Prometheus metrics, OTLP traces and a typed event + usage stream, (5) Go single-binary install.

**Does Opod train models?**
No. Use Axolotl / Unsloth / torchtune for training. Bring back a LoRA adapter; Opod will serve it.

**Why Go and not Rust?**
Go ships a static binary as fast as Rust for this workload, with a faster development loop. We may rewrite hot paths in Rust if measurements justify it.

**Is there a hosted version?**
Not initially. The product is the software you run.

**Can I use my own Tailscale account?**
Opod has no built-in tailnet integration today — clustering is plain LAN HTTP, and there are no `mesh.*` config keys. Running Opod *over* a Tailscale network you manage yourself works fine (it's just IP connectivity between your machines); a built-in tsnet backend is a possible future addition (see [ROADMAP.md](ROADMAP.md)).

**Does Opod support AMD GPUs?**
Yes on Linux + ROCm through the llama.cpp worker image (proven on a Radeon RX 7900 XTX host). A vLLM ROCm worker image is built too, but has not had its run on hardware yet — see [`images/README.md`](images/README.md).

**Can I run this on Windows?**
Workers no (no MLX, no native vLLM). Leader/CLI yes via WSL2. Native Windows isn't a near-term priority.

---

## Also known as / search terms

Opod is a **self-hosted LLM gateway** and **inference router**. If you found this repo searching for an alternative to a hosted service or a frontend for a local engine, the answer is yes:

- **LiteLLM alternative** (Go binary instead of Python) — one OpenAI-compatible gateway in front of your own engines, plus multi-node routing (no vendor proxying).
- **Ollama frontend / multi-machine Ollama** — Opod orchestrates several Ollama (or vLLM / MLX-LM / llama.cpp) nodes behind one gateway with auth, quotas, and audit.
- **Private inference cluster / on-prem LLM gateway** — keep all inference on a trusted LAN or Tailscale; nothing leaves your network.
- **Self-hosted Cursor / Aider / Continue backend** — drop-in OpenAI-compatible URL for IDE coding tools.
- **AI gateway with per-user keys + quotas + audit** for teams of 10-50 spending $30k+/yr on Claude / GPT.
- **Sharded inference orchestrator** — split a model larger than any single machine across multiple workers via `llama.cpp-RPC`.

Related concepts: local LLM, on-prem AI, private GPT, GGUF, multi-tenant inference, model placement, fallback chain.

---

## License

Apache License 2.0 — see [LICENSE](LICENSE) and [NOTICE](NOTICE). Contributions are accepted under the
[Developer Certificate of Origin](https://developercertificate.org/) (`git commit -s`); there is no CLA.

You can use Opod commercially, modify it, fork it, embed it, redistribute it, and compete with it. The only requirements are (a) keep the license + notice, (b) state significant changes you made. No copyleft, no usage limit. The control plane (`opodcp`) is a separate proprietary product with a free tier of 16 GPUs per cell.

## Acknowledgments

Opod stands on the shoulders of:

- **vLLM** — for fast NVIDIA inference
- **MLX-LM** — for Apple Silicon inference
- **llama.cpp** — for the universal fallback
- **Ollama** — for proving the developer-experience bar
- **LiteLLM** — for showing what one gateway in front of many models should feel like
- **Hugging Face** — for the open-weight model ecosystem

---

**Project links**

- Website: https://opod.io
- GitHub: https://github.com/opod-io/opod-core
- Maintainer: [Hadi Honarvar Nazari](https://www.linkedin.com/in/hadi-honarvar-nazari/) — `hadi.work.ca@gmail.com`
- Security disclosures: see [SECURITY.md](SECURITY.md)
