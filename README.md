# Opod

> **Self-hosted AI for your team. One endpoint. Your hardware.**

[![License](https://img.shields.io/github/license/opod-io/opod-core?color=blue)](LICENSE)
[![Go](https://img.shields.io/github/go-mod/go-version/opod-io/opod-core)](go.mod)
[![CI](https://github.com/opod-io/opod-core/actions/workflows/ci.yml/badge.svg)](https://github.com/opod-io/opod-core/actions/workflows/ci.yml)
[![Auto-release](https://github.com/opod-io/opod-core/actions/workflows/auto-release.yml/badge.svg)](https://github.com/opod-io/opod-core/actions/workflows/auto-release.yml)
[![Contributor Covenant](https://img.shields.io/badge/code%20of%20conduct-Contributor%20Covenant%202.1-5e4b8b)](CODE_OF_CONDUCT.md)
[![Discussions](https://img.shields.io/github/discussions/opod-io/opod-core?label=discussions)](https://github.com/opod-io/opod-core/discussions)

[**opod.io**](https://opod.io) · [Discussions](https://github.com/opod-io/opod-core/discussions) · [Go SDK](https://github.com/opod-io/opod-sdk) · [Contributing](CONTRIBUTING.md) · [Support](SUPPORT.md) · Maintained by [Hadi Honarvar Nazari](https://www.linkedin.com/in/hadi-honarvar-nazari/) · Apache-2.0

>
> Engine-agnostic: bring **Ollama**, **vLLM**, **SGLang**, **MLX-LM**, or **llama.cpp**. Run open-weight models (Qwen, Llama, DeepSeek, …) on your own hardware and shard a giant model across several machines — llama.cpp RPC, vLLM + Ray, or SGLang's own launcher, whichever the catalog entry names. Core has no vendor egress: nothing leaves your network.
>
> Point Cursor, Aider, Continue, Codex CLI, or any OpenAI SDK at Opod. It just works.

## 🗺️ Where Opod sits

> **Scope (ADR-022, 2026-09-06).** Core is the CLI-only inference runtime: `opod up` / `opod join`, the OpenAI-compatible
> gateway, API keys + join tokens, engine adapters, model load, multi-machine sharding (llama.cpp RPC · vLLM + Ray · SGLang), and a stable `/admin/v1` surface for an
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
      ║              keys as identity · usage + typed event streams          ║
      ║              CLI-only core · the console is the control plane        ║
      ║                                                                      ║
      ║  Router      Same model on N nodes  → load-balance                   ║
      ║              Different models per node → route by placement          ║
      ║              Model bigger than any node → split it: llama.cpp RPC,   ║
      ║                                    vLLM + Ray, or SGLang's launcher  ║
      ║              Engine error or timeout  → retry catalog fallback chain ║
      ╚═════════════════════════════╤════════════════════════════════════════╝
                                    │
                    ┌───────────────┴───────────────┐
                    ▼                               ▼
             ┌─────────────┐                 ┌─────────────┐
             │  (any mix)  │                 │  (any mix)  │
             │  • Ollama   │                 │  • Ollama   │
             │  • vLLM     │                 │  • vLLM     │
             │  • SGLang   │                 │  • SGLang   │
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

**One-sentence version:** Opod is the layer that lets your tools talk to open-weight LLMs on your own hardware through **one URL and one API key**, with the team controls (per-user keys with scopes and expiry, a per-key usage stream) a bare inference engine doesn't give you. It is a CLI and an HTTP API; there is no UI in core.

---

## 🚀 Try it in 60 seconds

Opod is engine-agnostic. The quickest path uses **Ollama** as the local engine — but vLLM, SGLang, MLX-LM and llama.cpp all work. See [Prerequisites — read first](#prerequisites--read-first) below for the alternatives.

### 🍎 macOS (Apple Silicon — M1/M2/M3/M4)

```bash
# 1. install Opod
curl -fsSL https://raw.githubusercontent.com/opod-io/opod-core/main/installer/install.sh | sh
export PATH="$HOME/.local/bin:$PATH"   # if the installer says so

# 2. install an engine (pick one) — Ollama is the simplest default
brew install --cask ollama && open -a Ollama
# alternatives: pip install mlx-lm  ·  or run llama.cpp's llama-server  ·  or run vLLM in Docker

# 3. start Opod with a tiny model (~1 GB, fast download)
OPOD_DEFAULT_MODEL=llama-3.2-1b OPOD_PULL_DEFAULT_MODEL=1 opod up   # PULL=1: this machine serves the model too
```

### 🐧 Linux (x86_64 or arm64) — including Raspberry Pi, NAS, edge boxes

**Option A — `.deb` / `.rpm` package** (recommended for Debian / Ubuntu / Raspbian / QNAP / Asustor / Fedora / RHEL):

```bash
# Debian / Ubuntu / Raspbian (arm64 example — also amd64)
curl -LO https://github.com/opod-io/opod-core/releases/latest/download/opod_VERSION_linux_arm64.deb
sudo dpkg -i opod_VERSION_linux_arm64.deb
# Binary at /usr/bin/opod; the catalog is embedded in it (no files to install).
# Docs land in /usr/share/doc/opod/, the two-node smoke script in
# /usr/share/opod/scripts/. Recommends llama.cpp for sharding — apt-install it
# if you want it.

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
OPOD_DEFAULT_MODEL=llama-3.2-1b OPOD_PULL_DEFAULT_MODEL=1 opod up   # PULL=1: this machine serves the model too
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
**For what shipped when**: see [CHANGELOG.md](CHANGELOG.md) and the [releases](https://github.com/opod-io/opod-core/releases). **For what this project will never do**: see [Deliberately out of scope](#deliberately-out-of-scope).

---

## Table of contents

- [Why Opod?](#why-opod)
- [60-second quick start](#60-second-quick-start)
- [Who is this for?](#who-is-this-for)
- [Architecture overview](#architecture-overview)
- [Features](#features)
- [Deliberately out of scope](#deliberately-out-of-scope)
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
OPOD_PULL_DEFAULT_MODEL=1 opod up   # without it the leader is router-only and pulls no model of its own
```

You'll see:

```
▶ detected darwin/arm64 · 24 GB RAM · 8 cores
✔ auto-selected model: qwen-coder-7b (Qwen 2.5 Coder 7B Instruct)
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
   │  gateway   OpenAI-compatible · auth · usage rows │
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
   │ vLLM       │ │ MLX-LM     │    │ a gang: RPC /    │
   └────────────┘ └────────────┘    │ Ray / SGLang     │
                                    └──────────────────┘
     `opod join`: register once, heartbeat every 5 s (HMAC per node)
```

See [ARCHITECTURE.md](ARCHITECTURE.md) for the full design.

---

## Features

### Inference

- OpenAI-compatible API (`/v1/chat/completions`, `/v1/embeddings`, `/v1/models`) and `/v1/rerank` — the only protocol surface (ADR-022; rerank returned with ADR-084)
- SSE streaming with proper client-disconnect handling (no goroutine leaks; bounded drain on cancel)
- Vision (image input) on multimodal models — `image_url` content blocks on `/v1/chat/completions` route through the Ollama engine path (the only driver that forwards images today)
- **Tool / function calling and structured output work** (since 2026-10-05), as a loop rather than one exchange. `tools`, `tool_choice` and `response_format` are carried to the engine and the model's `tool_calls` come back — streamed as the engine sent them, or merged when you ask for one answer, with `arguments` left a string for you to parse and an `id` to echo back. Your next request carries the assistant turn's `tool_calls` and each result's `tool_call_id` through to the engine, which is what makes a second turn work at all. **Opod never runs a tool**: it carries the declaration down and the call back up, and your client runs the function, as with any OpenAI-compatible server. An engine that cannot do tools refuses by name rather than answering prose — Ollama does today, because its tool protocol is a different shape; serve that model through vLLM or SGLang. A catalog entry's `capabilities: [chat, tools]` describes the *model*, not this gateway
- `model=auto` resolves to the leader's default model (`router.default_model`) — one name a client can hardcode. There are no prompt heuristics and no vendor chain behind it (ADR-022)
- **Response cache** — embeddings cached against a sha256 of the canonicalized request body (object keys sorted; ephemeral fields stripped). Two drivers: in-memory LRU (default) and SQLite-backed (persists across leader restart). Per-request opt-out via `Cache-Control: no-cache` / `no-store`; per-tenant scoping via `opod.cache.namespace` body field. `X-Opod-Cache: hit | miss` response header.
- Typed `engine_unreachable` errors with engine name, endpoint, and start-hint (e.g. `ollama serve`) when the upstream engine isn't responding
- Engine health watchdog on auto-spawned engines (force-restart after 3 consecutive failures, covers hung llama-server)
- LoRA adapters on vLLM workers: configured through `OPOD_ADAPTERS`, added or dropped at runtime through `/admin/v1/adapters`, served as `<model>:<adapter>`
- Chat completion caching with streaming replay + semantic cache (planned)

### Cluster

- Sleep tier: a worker's engine can drop its GPU working set and keep the process (vLLM sleep mode) — `POST /v1/model/sleep|resume` on the worker, proxied by the leader as `/admin/v1/nodes/{id}/sleep|resume`; a sleeping worker is resident but not routed to until resumed — and a request for its model resumes it: the leader wakes the worker, holds the request through the wake (up to 3 s) and serves it (`request_wake`)

- Worker unload (`worker_unload`): `POST /v1/model/unload` on a worker is the counterpart of `/v1/model/load` — the engine's unload (Ollama), or a stop of the engine process the worker launched (vLLM, SGLang, llama.cpp); idempotent, `409` while a shard part or an adapter of the model is held there, `501` when the engine cannot. The leader calls it; there is no CLI verb on it yet

- One-command join — `opod join "<leader-url>?token=…"` registers a worker (hardware, and which engine it runs); heartbeats every 5 s carry the models it answers for and, from an engine that loads on request (Ollama), which of them are in memory
- Placement by residency — a worker serves what its engine has loaded (`opod model add <id> --node a,b` pulls and warms it there; on a worker whose engine serves one model per process — vLLM, SGLang, llama.cpp — that would stop what it serves, so it is refused naming both models unless you add `--force`); the router prefers local, then the least-loaded worker
- **Memory lifecycle** — admission control against live engine residency (a machine is never overcommitted), `opod model load --swap` with LRU evict-and-drain, `--pin` to protect a model, desired placements restored on restart, `opod down` releases engine memory by default, `--exclusive` for one-model-per-machine
- Heterogeneous sharding for a model larger than any single node — `opod shard create <model> [N]` orchestrates every part end to end. Three backends, picked by the catalog entry's `sharding.engine`: llama.cpp **RPC parts** (layer-split, the default), a **vLLM + Ray** cluster (tensor and pipeline parallel), and **SGLang's own distributed launcher** (rank 0 serves the group). A model may run **several gangs** under one leader (`--gang <id>`) and the router picks the least loaded ready one
- Live model migration — **built**: `opod model move <id> --from <node> --to <node>` hands a whole model to another worker by overlap, so no request fails and none waits for a cold load (no KV-cache transfer; a gang is rebuilt, not moved)
- **Front doors** (`opod up --role gateway --leader <url>`): extra copies of the gateway for one endpoint. A door serves `/v1` only — no `/admin/v1`, no join surface, no engine — mirrors the leader's worker list, pushes its usage rows to the leader (the single writer) and polls back the leader's door count and lag bound (`/admin/v1/spend`)
- Cross-platform workers: Mac (MLX), Linux+NVIDIA (vLLM, SGLang, llama.cpp), Linux+AMD (llama.cpp ROCm and vLLM ROCm both proven on a Radeon host; SGLang ROCm is an Instinct-only upstream build), Linux+Intel Arc (llama.cpp SYCL proven), Tenstorrent (vLLM on tt-metal, proven), CPU (llama.cpp)
- HA leader (planned)

- **Uneven layer splits, per worker** — `flags.tensor_split` (llama.cpp) and `flags.pp_layer_partition` (vLLM, SGLang) are declared per engine and validated, so a worker can weight its own split towards a bigger card instead of being bounded by the smallest. **On a llama.cpp gang the split is in force too**: a gang's flags reach its coordinator, and on a 48 GB and an 8 GB card `tensor_split` `12,8` left 11.0 GB on the one and 6.4 GB on the other, serving (measured 2026-09-29). **A vLLM or SGLang gang is not there**: `pp_layer_partition` is delivered and each rank loads its share, but the one vLLM gang it was tried on never finished starting, so do not plan a cross-machine split on those engines
- **A worker says goodbye on `SIGTERM`** — a process being stopped leaves the router's rotation at once, instead of costing a request per shutdown while its heartbeat ages out

### Multi-tenancy

- Per-user API keys with revocation, scopes (admin / user / node), and **TTL expiry** (`--ttl 7d`, `--expires-at 2026-07-01`, `opod token renew/expire`)
- **A key is an identity — who is calling, with which scope, until when — and nothing more.** Per-key daily quotas, RPM/TPM rate limits and model allowlists were part of core until 2026-09-28 and are not any more: per-caller policy belongs to the application layer in front of an endpoint (an API gateway, a billing service), which is where accounts, users and plans live. Every request is still recorded per key in the usage stream, which is what such a layer meters from. Older clients that send `rpm_limit`, `tpm_limit`, `quota_daily_tokens` or `allowed_models` on a key are accepted and the fields are ignored.
- An always-on `X-Opod-Request-Id` correlation token on every `/v1/*` response (also embedded in audit rows for traceability)
- OIDC / SSO — **not in core** (explicitly [out of scope](#deliberately-out-of-scope)). Core authenticates API keys only; per-user keys and the event stream cover accountability, and SSO belongs to the control plane

```bash
opod token create dave  --ttl 30d                           # expiring key
opod token renew k_abc --ttl 30d                           # extend expiry
```

### Observability

- Prometheus metrics endpoint (`/metrics`) — per-model RPS, latency, tokens, errors
- `GET /loadz` for an external autoscaler: in-flight, rpm, waking 503s, TTFT p50/p95, how many workers hold a card with a dead engine, plus the workers' own engine signals (KV-cache %, queue depth, tokens/s, prefix-cache hits) gathered from their heartbeats — a sharded endpoint's pressure comes from its gang coordinator. `OPOD_PROBE_LISTEN` puts the probes on a second, always-plain port so a scraper needs no certificate, and a request marked `X-Opod-Probe` is served like any other and counted in none of it. `GET /gatewayz` is the same idea for the front doors: how many there are and what the endpoint carries across them. See [docs/SCALING-SIGNALS.md](docs/SCALING-SIGNALS.md)
- Per-call usage records (model, protocol, tokens, latency, outcome — `ok`, `error` and `cancelled` alike: a caller who hangs up mid-stream still leaves a row) on `/admin/v1/usage/stream` (cursor + replay) — the control plane pulls and exports them
- Admin actions are recorded and streamed to the control plane on `/admin/v1/events/stream`
- OpenTelemetry / OTLP traces. Set `observability.otlp_endpoint` (or `OPOD_OTLP_ENDPOINT`) to your collector — e.g. `http://localhost:4318` — and Opod emits a full span hierarchy per request: `http.request` → `router.Chat` (covers the whole stream) → `router.Chat.attempt` (one per fallback retry) → `<engine>.Chat` (engine call with prompt/completion token counts). All five engine drivers (ollama, vllm, sglang, mlx, llamacpp) export the same span shape. W3C `traceparent` propagation is always on so Opod participates correctly between two services that both export. Empty endpoint = no-op (zero overhead beyond the NoopTracerProvider).
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

## Deliberately out of scope

Not "later" — **not this project**. Each was considered and declined for a reason. A pull request that adds one
of these will be declined for the same reason, so the table is here to save you the work.

| | Why not |
|---|---|
| **A web dashboard, SSO, RBAC, teams** | core is CLI-only. Per-key scopes and expiry, the usage stream and the audit log are the accountability story on a trusted network. |
| **Cost, billing, or dollar figures** | the usage stream records tokens, never money. What a token costs depends on hardware, power and contracts this binary cannot see. |
| **Vendor egress: Bedrock, Vertex, hosted-model key pools** | serving *your* weights on *your* machines is the whole point. Routing to someone else's API is a different product. |
| **Non-chat protocol surfaces** (Anthropic Messages, audio transcription and speech) | one protocol, done properly. These were removed in the 2026-09 contraction (ADR-022); `/v1/rerank` came back on 2026-10-06 (ADR-084) because a retrieval stack asks for it. |
| **Content policies, output filtering, guardrail implementations** | the interface and the event stream stay; the policies belong where the request originates. |
| **Kubernetes, Helm, operators, CRDs** | `opod` runs as a process. Anything that schedules processes across a fleet is an orchestrator's job, not the runtime's. |
| **Training and fine-tuning** | use `axolotl`, `unsloth`, or `torchtune`. |
| **A vector store** | an adapter for SQLite-VSS / pgvector may ship with signed catalogs; running one will not. |
| **Video generation, real-time voice agents** | minutes-per-inference render farms and full-duplex sub-300 ms loops are different operational models. They belong in separate projects that use `opod` for the LLM leg. |
| **Phoning home** | no automatic update check, no telemetry. `opod update` is something you type. |

Everything in the dashboard/fleet-management direction lives in a separate product and is not part of this repo.

---

## Supported models

> **For the complete per-model walkthrough** (system requirements, performance per platform, install + use snippets for every client) see **[MODELS.md](MODELS.md)**.

Opod ships a curated catalog of **47 open-weight models**, embedded in the binary from [`opod-io/opod-sdk/catalog`](https://github.com/opod-io/opod-sdk) (`opod catalog ls`, `opod catalog export <dir>`), spanning everything from 1 B edge models to 1 T-parameter sharded frontier MoE. Any other model also works via `opod model add hf:<owner>/<repo>` (HuggingFace direct) or `opod model add ollama:<name>` (any Ollama-pullable tag). See the SDK's `catalog/README.md` for the YAML schema if you want to PR an entry; a file dropped in `~/.opod/catalog/` or `$OPOD_CATALOG_DIR` adds or overrides one locally. `opod fetch <repo> <file> --dir <models dir>` makes one GGUF present in a models directory — exclusive per file (a second caller waits for the first), written to a temp sibling and renamed only when the byte count matches the server's, skipped when already there — the same pull a worker runs before launching llama-server.

**Signed catalog files.** A catalog entry decides which weights get pulled, so files you add can be signed with [minisign](https://jedisct1.github.io/minisign/): `minisign -S -m my-model.yaml` writes `my-model.yaml.minisig` beside it. Set `OPOD_CATALOG_PUBKEY` (the public key line, or the path of your `minisign.pub`) and every signed file in `~/.opod/catalog/`, `$OPOD_CATALOG_DIR` and the share directories must verify — at every catalog load, and in `opod model add --from my-model.yaml`, which also saves the signature beside its copy. A signature that is present and does not verify is always a refusal that names the file. Unsigned files keep loading unless you also set `OPOD_CATALOG_REQUIRE_SIGNED=1` — that switch is what makes it a boundary, since whoever can write the directory could otherwise just delete the signature. The embedded catalog ships inside the binary and is not subject to any of this, and neither is an unedited `opod catalog export` copy of it.

> 📋 **Picker table — what to install** — full table with size, RAM, chat/code/reasoning/vision/audio/context ratings and license per model: **[MODELS.md → Picker table](MODELS.md#-picker-table--what-to-install)**.

### Shipped catalog at a glance

| Tier | Models |
|---|---|
| **Edge (≤4 GB RAM)** | `llama-3.2-1b`, `llama-3.2-3b`, `nomic-embed-text` (embeddings), `moondream3` (vision) |
| **Consumer big (16-32 GB)** | `gpt-oss-20b` ⭐, `qwen3.6-27b` ⭐, `gemma4-26b`, `gemma4-31b` (vision), `qwen3-30b`, `qwen3-coder-30b`, `qwen3-vl-32b` (vision), `qwen-coder-32b` |
| **Single 80 GB GPU** | `llama-3.3-70b-sharded`, `gpt-oss-120b`, `llama-4-scout` (10M ctx, multimodal), `glm-4.5-air-sharded` (agentic MoE) |
| **Sharded frontier (≥128 GB combined)** | `step-3.7-flash-sharded` ⭐ (Apache-2.0), `deepseek-v4-flash-sharded`, `nemotron-3-ultra-sharded` (Mamba-MoE, 1M ctx), `glm-4.6-sharded` (agentic coder), `glm-5.1-sharded`, `glm-5.2-sharded` (1M ctx), `kimi-k2.6-sharded` |

⭐ = current top picks (reviewed September 2026). The table is a selection of the 47 catalog entries; [MODELS.md](MODELS.md) has the full picker table with sizes, RAM floors, and licenses.

Run `opod model search` to list everything live with sizes and capabilities, or `opod model info <id>` for one model's full spec. Add `--sort=released` for newest-first, `--since 2026-01-01` to filter by date, or `--json` for machine-readable output. `opod model ls`, `opod status`, `opod config show` and `opod cache ls|prune` also accept `--json`. Running any `opod model add|info|remove` or `opod connect` with no ID launches an interactive picker (type to filter; arrow keys to navigate). Output is colored when stdout is a TTY; set `NO_COLOR=1` (or `OPOD_NO_COLOR=1`) to disable.

Aggregates (summaries, breakdowns) are computed by the control plane from the streams; core keeps only the raw facts.

Engine reliability: when Opod auto-spawned the engine itself (`opod up` with `OPOD_ENGINE=llamacpp`), a health watchdog polls every 30 s and force-restarts the process after three consecutive failures — so a hung `llama-server` no longer requires manual intervention. For user-managed engines (Ollama, vLLM) Opod leaves the process alone but `/v1/chat/completions` now returns a typed `engine_unreachable` error with the engine name, endpoint, and the exact command to start it (`ollama serve`, `mlx_lm.server …`, etc.) when the engine isn't responding.

### Roadmap — model families not yet in catalog

These work today via `opod model add hf:owner/repo` but don't have curated YAML entries with hardware specs:

- **Larger general / agent models** — Qwen3-235B, MiniMax-M2.7, MiMo-V2 sharded variants — pending sharded YAML entries.

Shipped recently (don't fall in this list):
- **Vision (image input)** — `gemma4-12b`, `gemma4-26b`, `gemma4-31b`, `gemma4-e2b`, `gemma4-e4b`, `qwen3-vl-8b`, `qwen3-vl-32b`, `pixtral-12b`, `moondream3`, `mimo-vl-7b`, `llama-4-scout` all serve through `/v1/chat/completions` with `image_url` content blocks.
- **Embeddings (for RAG)** — `/v1/embeddings` is live; install `nomic-embed-text` and call it from any OpenAI-shape embedding client.
- **Audio (input)** — `mimo-audio`, `gemma4-12b`, `gemma4-e2b`, `gemma4-e4b` declare `audio` capability for future routing; today they serve as `chat` models.

---

## Supported clients

`opod connect <client>` prints a copy-pasteable config snippet for each tool.

The roster is 15 (`opod connect --list`), and it is one list in the code — `internal/control/connect.go`
plus one `snippets/*.tmpl` per client:

| Client (`opod connect <id>`) | Config |
|---|---|
| **Cursor** (`cursor`) | Settings → Models → Override OpenAI Base URL |
| **Aider** (`aider`) | `aider --openai-api-base http://opod:8080/v1` |
| **Continue.dev** (`continue`) | `~/.continue/config.json` → `apiBase` |
| **Zed** (`zed`) | `language_models.openai_compatible.api_url` |
| **Cline / Roo Code** (`cline`, VS Code) | Provider settings panel (OpenAI-compatible provider) |
| **OpenClaw** (`openclaw`) | OSS CLI coding agent — env vars |
| **OpenCode** (`opencode`) | per-provider `baseURL` override |
| **Open WebUI** (`open-webui`) | self-hosted chat UI (Docker) → OpenAI connection |
| **Open Notebook** (`open-notebook`) | OSS NotebookLM alternative → OpenAI provider |
| **goose** (`goose`) | Block's OSS terminal agent |
| **Plandex** (`plandex`) | terminal-native agentic planner |
| **OpenHands** (`openhands`) | autonomous coding agent (formerly OpenDevin) |
| **Codex CLI** (`codex-cli`) | `OPENAI_BASE_URL` / `OPENAI_API_KEY` |
| **OpenAI Python / JS SDK** (`openai-sdk`) | `OpenAI(base_url=…, api_key=…)` |
| **curl** (`curl`) | direct HTTP |

Anything else that speaks the OpenAI shape works without a snippet — point it at the base URL and the key
(LangChain and LlamaIndex, for example, take `openai_api_base`). A tool that only speaks the **Anthropic
Messages** shape (Claude Code among them) needs a protocol shim in front of the gateway; core does not ship
one (ADR-022).

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

Opod is a **gateway** — it doesn't include an LLM engine. You need one of the five it drives:
- **Ollama** (recommended for most users; works on Mac + Linux + NVIDIA + CPU)
- vLLM (for NVIDIA / ROCm / XPU GPUs at scale — Linux only)
- SGLang (NVIDIA, and AMD Instinct; RadixAttention prefix caching — Linux only)
- llama.cpp `llama-server` (GGUF anywhere, including CPU; the engine Opod can also launch for you, and the one that shards by layers across machines)
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
  | OPOD_VERSION=v0.1.0 sh

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
    sh -s -- join "https://leader.local:8080?token=<TOKEN>"
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
| GH API rate-limited during install | Anonymous GH API limit (60/hr) | Wait, or set `OPOD_VERSION` to a release tag (e.g. `OPOD_VERSION=v0.1.0`) to skip the lookup |

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

The initial admin key is auto-generated on first `opod up` and printed to stdout (and saved to `~/.opod/admin.key`) — copy it then. There is no `auth.initial_admin_key` field; the key lives in the SQLite store, not the YAML.

### Full reference

Every field below is parsed by `internal/config/config.go`. Anything not in this list is silently ignored.

```yaml
listen: ":8080"                       # HTTP listen address (used by leader and workers)
external_url: ""                      # public URL printed by `opod up` and embedded in `opod connect` snippets; empty → use listen addr
tls_cert: ""                          # PEM cert; with tls_key the ONE listener speaks TLS (gateway, /admin/v1,
tls_key:  ""                          # probes, the join path). Empty → plain HTTP. Workers trust it via OPOD_LEADER_CA
probe_listen: ""                      # e.g. ":8081" — a SECOND, always-plain listener serving only /healthz,
                                      # /readyz, /loadz, /metrics, so a scraper or autoscaler needs no CA
data_dir: "~/.opod"                  # root for state.db, models, logs
log_level: "info"                     # debug | info | warn | error
catalog_dir: ""                       # empty → the embedded catalog + override dirs (see OPOD_CATALOG_DIR); a path → ONLY that directory
max_body_bytes: 0                     # request-body cap on /v1/* in bytes;
                                      # 0 → built-in 32 MiB ceiling
trust_proxy_headers: false            # honour X-Forwarded-Host/-Proto when deriving the public base URL
block_private_targets: false          # refuse outbound hook/probe calls to loopback + RFC-1918 (SSRF defence)

storage:
  type: "sqlite"                      # only sqlite ships today
  dsn: "~/.opod/state.db"
  models_dir: "~/.opod/models"

auth:
  require_keys: true                  # set false to disable API-key auth (dev only)
  admin_token: ""                     # seeded as an admin-scope key on every `opod up` (idempotent) — how a
                                      # manager that provisioned this leader calls /admin/v1 without reading admin.key
  join_token: ""                      # seeded as a node-scope key on every `opod up`, so worker joins survive a
                                      # wiped leader DB. EMPTY BY DEFAULT — there is no built-in shared secret

engine:
  preferred: "ollama"                 # ollama | vllm | sglang | mlx | llamacpp (a driver's aliases work too)
  ollama_endpoint:   "http://127.0.0.1:11434"
  vllm_endpoint:     "http://127.0.0.1:8000"
  mlx_endpoint:      "http://127.0.0.1:8080"
  llamacpp_endpoint: "http://127.0.0.1:8089"   # llama-server (single-node or RPC coordinator) — port chosen to avoid Opod leader :8080 and worker :8081
  sglang_endpoint:   "http://127.0.0.1:30000"  # the port a worker launches sglang.launch_server on

router:
  default_model: ""                   # empty → auto-pick on first up; this is what model="auto" resolves to
  pull_default_model: false           # false = the leader is ROUTER-ONLY and pulls no model of its own
  heartbeat_max_age_seconds: 30       # a worker whose last heartbeat is older takes no new work (0 → 60 s bound)
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

surfaces:
  managed: false                      # true (OPOD_MANAGED=1) = run by an external manager: the store becomes an
                                      # in-memory cache unless OPOD_STORAGE_DSN names one, and the banner says so.
                                      # A file that still carries the retired `ui`/`egress`/`callbacks` keys loads fine
```

There is no `observability.guardrails:` and no `observability.callbacks:` key: guardrail rules arrive in the
**policy snapshot** a manager mounts (`OPOD_POLICY_FILE`, default `/etc/opod-auth/policy.json`) and the
callback sinks left with ADR-022. Beyond this file, a manager steers a leader or a worker through the
**process-environment contract** — `internal/config/env.go`, exported as a table for a manager as
`leader.EnvContract` (`internal/leader/contract.go`) and reproduced below. A test (`TestEnvSurfaceOutsideConfigIsTheAllowlist`) fails
when library code reads a variable that is not on it, so the list is the whole surface.

### Environment variables

**Overrides for a `config.yaml` field** (env wins over the file, the file wins over the default):

| Var | Overrides |
|---|---|
| `OPOD_LISTEN` | `listen` |
| `OPOD_TLS_CERT` / `OPOD_TLS_KEY` | `tls_cert` / `tls_key` — both set = the leader's listener speaks TLS |
| `OPOD_PROBE_LISTEN` | `probe_listen` — the second, always-plain probe listener (`/healthz`, `/readyz`, `/loadz`, `/metrics`) |
| `OPOD_DATA_DIR` | `data_dir` |
| `OPOD_STORAGE_DSN` | `storage.dsn` (also what gives a managed leader a durable store) |
| `OPOD_LOG_LEVEL` | `log_level` |
| `OPOD_EXTERNAL_URL` | `external_url` |
| `OPOD_MAX_BODY_BYTES` | `max_body_bytes` |
| `OPOD_TRUST_PROXY_HEADERS` | `trust_proxy_headers` |
| `OPOD_BLOCK_PRIVATE_TARGETS` | `block_private_targets` |
| `OPOD_ENGINE` | `engine.preferred` |
| `OPOD_OLLAMA_ENDPOINT` / `OPOD_VLLM_ENDPOINT` / `OPOD_SGLANG_ENDPOINT` / `OPOD_MLX_ENDPOINT` / `OPOD_LLAMACPP_ENDPOINT` | the corresponding `engine.*_endpoint` |
| `OPOD_VLLM_API_KEY` | bearer token sent to a vLLM server (no YAML equivalent). The old unprefixed `VLLM_API_KEY` still works as a deprecated fallback; the prefixed form wins when both are set |
| `OPOD_REQUIRE_KEYS` | `auth.require_keys` (`1/true` or `0/false`) |
| `OPOD_ADMIN_TOKEN` | `auth.admin_token` — seeded as an admin-scope key at every `opod up` |
| `OPOD_JOIN_TOKEN` | `auth.join_token` — seeded as a node-scope key, so joins survive a wiped leader DB |
| `OPOD_DEFAULT_MODEL` | `router.default_model` |
| `OPOD_PULL_DEFAULT_MODEL` | `router.pull_default_model` — `1` lets the leader pull that model into its own engine |
| `OPOD_HEARTBEAT_MAX_AGE_SECONDS` | `router.heartbeat_max_age_seconds` (default 30; `0` falls back to a 60 s bound) |
| `OPOD_STICKY_SESSION_TTL_SECONDS` | `router.sticky_session_ttl_seconds` |
| `OPOD_LATENCY_P95_SECONDS` | `router.latency_fallback_p95_seconds` — when primary p95 exceeds this, prefer a faster fallback. 0 = disabled (default) |
| `OPOD_PLACEMENT_ALLOWED_FAILS` / `OPOD_PLACEMENT_COOLDOWN_SECONDS` | `router.placement_allowed_fails` / `router.placement_cooldown_seconds` — the per-worker circuit breaker; both must be > 0 |
| `OPOD_OTLP_ENDPOINT` | `observability.otlp_endpoint` (OTLP/HTTP collector URL or bare `host:port`) |
| `OPOD_EXCLUSIVE` | `placement.exclusive` (truthy `1/true`) — one resident model per machine |
| `OPOD_PLACEMENT_RESERVE_PERCENT` | `placement.reserve_percent` (default 20) |
| `OPOD_PLACEMENT_DRAIN_TIMEOUT_SECONDS` | `placement.drain_timeout_seconds` — eviction drain bound (default 30) |
| `OPOD_MANAGED` | `surfaces.managed` — `1` = run by an external manager (in-memory store unless `OPOD_STORAGE_DSN`, banner reads `Mode: managed`) |

**The process-environment contract** (`internal/config/env.go`): variables that steer a leader, a front door
or a worker and have **no YAML equivalent** — this is the surface a control plane or a systemd unit renders.
`side` says which process reads it.

| Var | Side | What it does |
|---|---|---|
| `OPOD_PLAN_FILE` | leader | the mounted plan the leader serves within (default `/etc/opod/plan.json`; absent = standalone) |
| `OPOD_AUTH_FILE` | leader | the mounted auth snapshot — keys, `requireKeys`, the worker-certificate policy (default `/etc/opod-auth/auth.json`; `off` = no watcher) |
| `OPOD_POLICY_FILE` | leader | the mounted policy snapshot — routing weights, access log, guardrail webhook rules (default `/etc/opod-auth/policy.json`; `off` = no watcher) |
| `OPOD_COORDINATOR_NODE` | leader | pin the llama.cpp RPC coordinator to a node id (`local` = the leader itself). Default: the worker with the most **GPU** memory, falling back to host RAM for a worker with no card |
| `OPOD_OTLP_LOGS_ENDPOINT` | leader | the leader's own log records over OTLP/HTTP, beside stderr (URL or bare `host:port`). Bounded queue, never blocks. Empty = off |
| `OPOD_ROLE` | leader | `leader` (default) or `gateway` — a front door that serves `/v1` only (`opod up --role gateway`) |
| `OPOD_LEADER_URL` | leader | the leader a **gateway** mirrors and pushes to (`http://host:8080`); required with `OPOD_ROLE=gateway` (`--leader`) |
| `OPOD_GATEWAY_ID` | leader | this door's name in the leader's door count, which sets each key's 1/N rate share; unset = the hostname |
| `OPOD_ACCELERATOR` | worker | the vendor the manager placed this worker on (`nvidia\|amd\|intel\|tt\|none`); unset = what the worker detects |
| `OPOD_ENGINE_FLAGS` | worker | JSON map of engine flags from the plan (`tp`, `max_model_len`, `ctx`, `ngl`, …) |
| `OPOD_ADAPTERS` | worker | JSON list of LoRA adapters `[{name, source, rank}]`, served as `<model>:<name>` |
| `OPOD_REJECT_BEARER` | worker | `1` = this worker's API accepts HMAC only, never a bearer token |
| `OPOD_KV_EVENTS` | worker | `1` = vLLM publishes its prefix-cache events on localhost and the worker reports block hashes on its heartbeat, so the leader can route by what a worker holds |
| `OPOD_SLEEP_MODE` | worker | `1` = vLLM starts with sleep mode on (the sleep autoscale tier) |
| `OPOD_WORKER_ROLE` | worker | `prefill` \| `decode` for disaggregated serving; unset = a whole worker |
| `OPOD_PLAN_REVISION` | worker | the plan revision this worker process was started for; the leader splits traffic per revision |
| `OPOD_ADVERTISE_ADDR` | worker | the `host:port` the leader should dial (overlay / multi-NIC hosts) |
| `OPOD_NODE_ID` | worker | a stable node id across restarts (else `node.yaml`, else `POD_NAME`, else random) |
| `POD_NAME` | worker | the pod's name, injected by any Kubernetes manifest; a worker with no `OPOD_NODE_ID` and no `node.yaml` takes `n_<pod name>` as its id rather than a random one per restart |
| `OPOD_LEADER_CA` | worker | PEM certificate the worker trusts for a TLS leader — exactly that one |
| `OPOD_NODE_CERT` / `OPOD_NODE_KEY` | worker | the client certificate this worker presents to its leader; its SPIFFE SAN `spiffe://<domain>/opod/node/<id>` **is** the worker's identity (feature `mtls`). With it set, `opod join <leader-url>` needs no `?token=`: the worker mints the secret the leader signs its calls back with and hands it over in the register |
| `OPOD_VRAM_BUDGET_GB` | worker | the slice of the card this worker may use (shared placement); also `opod join --vram-budget` |
| `OPOD_GPU_INDEX` | worker | the device index the worker was pinned to (informational; also `opod join --gpu`) |
| `OPOD_LOAD_MODEL` | worker | the catalog id this worker loads itself once its engine answers, with `OPOD_LOAD_REPO` / `OPOD_LOAD_FILE` overriding the catalog's repo and file. No call to its own API, so `OPOD_REJECT_BEARER=1` is usable |
| `OPOD_CATALOG_DIR` | both | a catalog directory merged over the catalog embedded in the binary. Every source that exists is merged by id and a later one wins: embedded → `/usr/local/share/opod/catalog` → `/usr/share/opod/catalog` (.deb/.rpm) → `$OPOD_CATALOG_DIR` → `~/.opod/catalog` (most authoritative). Nothing else is searched — not `./catalog`, not a `catalog/` beside the binary. `catalog_dir` in `config.yaml` is a different switch: it reads that one directory **alone** |
| `OPOD_CATALOG_PUBKEY` | both | a [minisign](https://jedisct1.github.io/minisign/) public key — the base64 line, or a file holding it. A catalog file with a `<file>.minisig` beside it must verify against this key; one that does not is **always refused**. The embedded catalog is never checked. Unset → signatures are ignored |
| `OPOD_CATALOG_REQUIRE_SIGNED` | both | `1` → a directory catalog file with **no** signature is refused too (needs `OPOD_CATALOG_PUBKEY`) |
| `OPOD_SKIP_SOURCE_CHECK` | both | `1` → never HEAD-check a model's upstream (air-gapped mirrors) |
| `OPOD_MODEL_REVISION` | both | pin the model's Hub revision (commit sha, tag or branch); cached under `<repo>@<rev>` so two revisions coexist. Empty = `main`, which moves |
| `OPOD_MODEL_SHA256` | both | the expected sha256 of the model file; a mismatch removes the file and fails the load |
| `HF_TOKEN` | both | Hugging Face token for gated repositories |
| `HF_ENDPOINT` | both | Hugging Face Hub base URL (a mirror); default `https://huggingface.co` |

A few more are read outside that contract, by the CLI rather than by a serving process:
`OPOD_MODELS_DIR` (the default `--dir` for `opod fetch` and `opod cache`), `OPOD_TOKEN` and `OPOD_BASE_URL`
(which key and leader the CLI talks to), `OPOD_UNLOAD_ON_EXIT`, `OPOD_NO_COLOR` / `NO_COLOR`, and `EDITOR`
(for `opod config edit`).

### Not yet configurable (roadmap)

These features are mentioned elsewhere in this README but have no YAML knob today. The list is here so you don't waste time guessing.

- **Mesh backend selection** — only the LAN backend ships today; there are no `mesh.*` config keys. The `tailscale` (tsnet) backend has an interface defined in `internal/mesh/` but no implementation.
- **OIDC / SSO** — out of scope for core (see [Deliberately out of scope](#deliberately-out-of-scope)). `internal/auth/` ships API keys only.
- **Automatic replication / a placement policy engine** — `internal/scheduler/` ships gang orchestration, the part-count picker, GGUF distribution and `model move`; nothing decides on its own that a hot model needs a second replica, or moves one to balance a fleet. That is deliberate — an external manager owns capacity (ADR-022). What *is* tunable today: `placement.*` (admission, reserve, drain), `router.placement_allowed_fails` / `placement_cooldown_seconds` (the circuit breaker), `router.hedge_replicas`, and the load-aware routing weights a manager mounts in the policy snapshot (`routing.kvWeight`, `kvSaturationPct`, `prefixAffinity`).
- Replication and a **mesh backend** (above) are the two real gaps. `/metrics` is no longer one of them: it is served on the main listener *and*, when `probe_listen` / `OPOD_PROBE_LISTEN` is set, on a second always-plain port beside `/healthz`, `/readyz` and `/loadz`. What is still missing there is a way to serve *only* metrics, or to authenticate them.
- **Per-node config** — `~/.opod/node.yaml` records the last join (node id, leader URL, token, address), but only the node id is read back, by the next `opod join`; it holds no settings. A worker takes its engine endpoints from its own `config.yaml` or env vars.

### Per-node engine override

Workers run their own engine binary. To point a worker at a non-default endpoint, set env vars before `opod join`:

```bash
OPOD_ENGINE=vllm OPOD_VLLM_ENDPOINT=http://127.0.0.1:8000 opod join "http://leader:8080?token=..."
```

---

## Cluster operations

### Start the leader

```bash
opod up
```

Re-running it after a stop reuses the store and the admin key (re-displayed from `~/.opod/admin.key` while that file exists). There is no already-running check: a second `opod up` on the same port fails with `listen :8080: … address already in use` — `opod status` shows the one that is running.

### Add a node

1. On the leader: `opod token create --node`
2. On the new machine: `curl -fsSL https://raw.githubusercontent.com/opod-io/opod-core/main/installer/install.sh | sh -s -- join "<leader-url>?token=<token>"` (keep the quotes: `?` is a glob in zsh)

The token is a node-scoped key (`sk-orc-<your-key>`): the shared secret between leader and worker, so mint it only on a network you trust and give it an expiry with `--ttl` if you like. The new node registers with the leader over HTTP, heartbeats every 5 s, and from then on signs its calls with a per-node HMAC. `opod join --gpu <index> --vram-budget <GB>` pins the worker to one GPU and a memory budget when several workers share a machine.

### Remove a node

```bash
opod node drain <node-id>   # no new requests or shard parts go to it; what is in flight finishes
opod node undrain <node-id> # changed your mind: back in rotation
opod node remove <node-id>  # forget it
```

A drained node stays drained — across heartbeats and a worker restart — until you `undrain` it. `opod node ls` shows the state the leader acts on: `draining` for a drained node, `lost` for one whose heartbeats stopped (`router.heartbeat_max_age_seconds`), `engine-silent` for one that heartbeats but whose engine has not answered it for that long (it gets no new requests until the engine answers again; a single slow answer changes nothing), whatever the stored row says. `drain`, `undrain` and `remove` go through the running leader — so its router forgets a removed node's cached connection, cooldown and placements at once — and write the store directly only when no leader answers; each says which it did. A sharded model with a part on it stops serving, and when every worker that holds a model is drained, requests for that model get `503` with `Retry-After` and a message that says it is a drain — other models on the same leader keep serving.

### Move a model to another worker

```bash
opod model move llama-3.2-3b --from <node-id> --to <node-id>
```

Another worker takes over a whole (non-sharded) model, and no request fails or waits for a cold load, because the two **overlap**: the target loads the model itself while the source keeps serving; only when the target's own heartbeat shows it serving does the router stop choosing the source; requests already on the source finish (`placement.drain_timeout_seconds`); then the source unloads. A target that never serves (30 minutes by default — a cold target downloads the weights inside that) is unloaded again and **the source is left serving, untouched**. The command prints each step when the move ends; the same steps are events `model.move_started|loaded|flipped|drained|finished|aborted` while it runs.

It refuses up front what cannot work, with the reason: the source does not hold the model, or serves LoRA adapters of it (drop them first — a move does not carry adapters); the target takes no new work (drained, lost), cannot hold the model **beside** what it already has (the model is resident twice during the overlap — the message has the numbers), or runs a one-model-per-process engine (vLLM, SGLang, llama.cpp) that already serves another model, which a load would stop — both models are named; a sharded model (rebuild the gang with `opod shard create`).

What it is not: no KV cache moves, so a conversation the source was serving recomputes its prompt on the target at its next turn (slower first token, same answer). An Ollama source keeps the weights installed after the unload; its placement is marked `released` — not routed to, also after a leader restart — until the model is loaded there again or the weights are removed, and the command says so. A move needs the running leader from start to end (there is no store-only path, unlike `node drain`); a leader that restarts mid-move abandons it safely — the source serves again and the target's copy is simply a second replica.

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

Pinned revisions live one level down, in `<repo>@<rev>/`, and both commands reach them. A pinned GGUF is listed and
pruned like any other file (`org-model@v1/model-q4.gguf`). A safetensors snapshot (`opod fetch --snapshot`) is **one
entry** with the directory's total size: it is pruned whole — under every one of its files' locks — or not at all, and
one file in it that this binary did not write leaves the whole directory alone. `--keep` takes a file name (kept wherever
it is cached), a path under the models directory, or a snapshot directory name (`org-model@<rev>`).

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
opod join "http://<leader-host>:8080?token=<token>" # registers + starts worker HTTP server
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
- `llama-server` on PATH on whichever machine runs the coordinator — by default the shard worker with the most **GPU** memory, so install llama.cpp there (`brew install llama.cpp`, or your distro's build).
- `rpc-server` on PATH on every worker that will host a part. (At time of writing this needs a source build of llama.cpp with `-DGGML_RPC=ON`; the Homebrew bottle doesn't include it. The `opod-worker-llamacpp-*` container images carry the pair already.)
- A catalog entry with `sharding.required: true`. `source.type: file` + `source.path` is a local GGUF the leader can read (the bundled `llama-3.3-70b-sharded` entry, in [`opod-io/opod-sdk/catalog`](https://github.com/opod-io/opod-sdk), is that shape); a `huggingface` source with a `file:` is downloaded by the leader and fanned out for you.
- N workers already joined and `ready` (`opod node ls`).

**One command on the leader:**

```bash
opod model add llama-3.3-70b-sharded
# auto-detects sharding.required=true → delegates to `opod shard create`

# or explicitly:
opod shard create llama-3.3-70b-sharded 2
```

What Opod does:

1. Picks 2 workers (highest RAM among the rows that can take a part) and refuses up front, with the numbers, if it cannot find them
2. Sends `POST /v1/process/start` to each worker → launches `rpc-server -p 50052`
3. Waits for both rpc-servers to be TCP-reachable (readiness probe)
4. On the coordinator host — the shard worker with the most GPU memory, or the one named by `head` in `POST /admin/v1/shards/create` / `OPOD_COORDINATOR_NODE` — launches `llama-server -m <gguf> --rpc <worker1>:50052,<worker2>:50052 --port 9001 --metrics`
5. Waits for the coordinator to be reachable
6. Persists shard rows + a `placements` row pointing the model at the coordinator
7. The Router routes any request for `llama-3.3-70b-sharded` to that gang's coordinator, which fans out to the rpc-server parts internally

**Manage from the CLI:**

```bash
opod shard ls                              # show every part + coordinator, per gang
opod shard remove llama-3.3-70b-sharded    # stops coordinator + every rpc-server, deletes rows
```

**Several gangs of one model.** A *gang* is one complete serving copy — N parts plus the coordinator that
fronts them — and a model may have several. A second gang is throughput, not a bigger model, and the router
picks the least loaded gang whose coordinator is ready:

```bash
opod shard create llama-3.3-70b-sharded --nodes a,b            # the default gang
opod shard create llama-3.3-70b-sharded --gang g1 --nodes c,d  # a second copy; the first keeps serving
opod shard remove llama-3.3-70b-sharded --gang g1              # removes that one only
```

A create **without** `--gang` still replaces every gang of the model — what it has always meant.
`DELETE /admin/v1/shards/{model_id}/{gang_id}` is the API half.

**Which engine splits the model** is the catalog entry's `sharding.engine`: `llamacpp` (RPC parts, layer-split,
the default), `vllm` (a Ray cluster — real tensor *and* pipeline parallelism, `--tp` / `--pp`), or `sglang`
(SGLang's own distributed launcher, rank 0 serving the group). llama.cpp has no tensor split, so `--tp 2` on
an RPC gang is refused and says which backends have one.

`opod shard create <model> --nodes a,b,c` pins the parts to named workers instead of letting Opod pick; the same routes are `GET|POST|DELETE /admin/v1/shards…` for a manager.

`opod shard create <model>` with no count picks one: the smallest number of equal parts that fits the live workers' free memory — 1 when the model fits one worker, never more parts than workers or than the model's layers — and prints what it picked and why; when nothing fits it refuses and names the numbers. This is a default of the command only. A count, `--nodes` or `--tp`/`--pp` you give is sent untouched, and `POST /admin/v1/shards/create` never picks: a body without a count means the catalog's `default_shards`.

**Caveats:**
- Shard crash recovery is automatic for up to 5 restarts with exponential backoff (1s, 2s, 4s, 8s, 16s). After that the process enters `crashloop` state and the admin must intervene — typically by re-running `opod shard create`. Both `rpc-server` and the `llama-server` coordinator restart this way. See `internal/agent/supervisor.go`.
- The coordinator runs on the shard worker with the most **GPU memory** — it holds the KV cache, and picking by host RAM once put it on an 8 GB card behind a 31 GB host and llama.cpp died allocating its KV buffer. The leader runs it only when the shard set has no workers. `OPOD_COORDINATOR_NODE=<node-id>` pins it (`local` forces the leader), and `POST /admin/v1/shards/create` can name a `head`.
- Worker selection for the *parts* is naive (descending registered RAM) and doesn't factor current load; the **count** picker does read free memory (above).

### List nodes

```bash
opod node ls
# ID             HOSTNAME             OS/ARCH      ADDRESS                STATE      LAST HB
# local          mac-studio-1         darwin/arm64 127.0.0.1:8080         ready      2026-06-01T10:12:44Z
# n_abc123       mac-mini-1           darwin/arm64 192.0.2.50:8081        ready      2026-06-01T10:12:43Z
# n_def456       gpu-tower            linux/amd64  192.0.2.51:8081        draining   2026-06-01T10:12:42Z
# n_ghi789       lab-mac              darwin/arm64 192.0.2.52:8081        lost       2026-06-01T10:06:40Z
```

`STATE` is what the leader acts on, computed at read time from the row: `ready`, `joining`, `draining`,
`lost` (heartbeats stopped) or `engine-silent` (still heartbeating, but its engine has not answered it for
the heartbeat bound). `LAST HB` is the last heartbeat's time (RFC 3339). `node ls` has no `--json`; `opod node show <id>` prints one full record as JSON.

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
4. **Drop-in dir** — write a YAML to `~/.opod/catalog/<id>.yaml` directly, then `opod model add <id>` treats it like a built-in entry. Same schema as the SDK's `catalog/*.yaml` (`id`, `display_name`, `source.{type,repo,ollama_name,path}`, `hardware`, `capabilities`).

This:
1. Checks the entry's `hardware.min_ram_gb` (and `min_vram_gb`) against the cluster — an install that overshoots the floor is refused with a clear error. Pass `--force` to override (e.g. when you know swap or a quantization knob will save you).
2. **Verifies the upstream exists** — a 5s HEAD against the Ollama registry / HuggingFace. A typo'd `hf:owner/repo` or renamed tag is refused immediately with the URL that 404'd, instead of "succeeding" and failing later at engine launch. Network trouble only warns (never blocks); `OPOD_SKIP_SOURCE_CHECK=1` for air-gapped mirrors. `--dry-run` shows the same check as a "Source check" row.
3. Records the model in the registry
4. Pulls the weights — on this node's engine, or on each worker named by `--node a,b` (a worker whose engine serves one model per process is refused, naming both models, unless you add `--force`). An interrupted download **starts over**: every pull is atomic and digest-checked, but HTTP resume is not implemented
5. Launches or asks the engine for the model, and for an entry with `sharding.required: true` hands off to `opod shard create`
6. The gateway serves it as soon as a heartbeat reports it loaded

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

### Pick a container image for a model

Workers also ship as container images — one engine on one accelerator per image, listed with their limits in
[`images/README.md`](images/README.md). The same list is embedded in the binary as data
([`images/images.yaml`](images/images.yaml)), so a person, a script or an agent can ask which image serves a model
without a checkout or a registry call:

```bash
opod image ls                                        # every image: engine, vendor, platforms, gang, proven
opod image show vllm-nvidia                          # what the node must provide, limits, how to pin it
opod image recommend llama-3.1-8b --vendor nvidia    # the image for a catalog model on that accelerator
opod image recommend hf:Qwen/Qwen2.5-7B-Instruct --json
```

`recommend` is a lookup, not a measurement: it walks the model's `recommended_engines` in order, keeps the engines
whose images load the entry's weights (a GGUF file → llama.cpp; a safetensors Hugging Face repo → vLLM or SGLang; an
Ollama-library entry → no image), narrows a model that must be split across workers to images that can join a gang
(`rpc`, `ray`), and stars one pick per vendor — images proven on hardware first. Everything it passed over is listed
with the reason. Whether the model fits a card is `min_vram_gb` in the answer and your call. Every sub-verb takes
`--json`; image fields are `name`, `repository`, `role`, `engine`, `vendor`, `arch`, `dockerfile`, `base`, `weights`,
`gang`, `proven`, `hardware`, `families`, `requires`, `summary`, `notes` (plus `tag` on a release binary — there is no floating `latest`, pin a digest).

### Remove a model

```bash
opod model remove qwen-coder-14b
```

### LoRA adapters

LoRA adapters are loaded from the worker's `OPOD_ADAPTERS` environment and can be added or dropped at runtime through the leader's `/admin/v1/adapters`; there is no `opod model adapter` CLI verb yet. A runtime add works within what the engine was started for: vLLM fixes `--enable-lora` and `--max-lora-rank` at launch (the worker passes them for the adapters and the largest `rank` in `OPOD_ADAPTERS`), so a worker started with no adapter, or asked for a `rank` above its launch value, answers `409` naming both numbers instead of relaying the engine's error.

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
| `POST` | `/v1/chat/completions` | Streaming + non-streaming; accepts `image_url` content blocks (Ollama path). Returns typed `engine_unreachable` errors with engine name + start hint when the upstream engine is down. Every body field the gateway does not model (`seed`, `logprobs`, `top_k`, `user`, …) reaches a vLLM / SGLang / llama.cpp / MLX-LM engine verbatim and `logprobs` come back; `model`, `messages`, `stream` and `stream_options` stay opod's; `n` or `best_of` above 1 is refused `400`. |
| `POST` | `/v1/embeddings` | Embedding models (e.g. `nomic-embed-text`): Ollama's own API, or the OpenAI `/v1/embeddings` of a vLLM / SGLang / MLX-LM / llama.cpp engine |
| `POST` | `/v1/rerank` | `{model, query, documents, top_n, return_documents}` → `results` most relevant first, each `{index, relevance_score, document.text}`. Served by a reranker model on vLLM, SGLang, or llama.cpp started with `--reranking`; Ollama and MLX-LM have no rerank route and answer `501 rerank_not_supported` (ADR-084) |
| `GET` | `/v1/models` | Lists the models a request can be answered for now: installed on the leader, or held by a worker that takes new work. A model held only by drained or lost workers is not listed; one that is merely asleep (sleeping workers, or a plan that scales to zero) is, because a request for it is how it wakes (`503` + `Retry-After`) |

(Planned: `/v1/completions`.)

### Opod admin surface

| Method | Path | Notes |
|---|---|---|
| `GET` | `/healthz` `/readyz` | Liveness / readiness. `/readyz` answers 200 with a mode: local engine · `router-only` · `shard-coordinator` · `sleeping-workers` · `sleeping` (parked on purpose) · `waking` (workers registered, nothing serving yet). Failing the probe would turn the honest `503 waking` into a connection error, so it almost never does. No key |
| `GET` | `/loadz` | Load for an external autoscaler: in-flight, rpm, waking 503s, plus the workers' engine signals (`kv_used_pct`, `queue_depth`, `tokens_per_s`, `prefix_hit_pct`, `workers`, `reporting`). `workers` = workers that can take a new request for the plan's model now: a drained, lost or sleeping one is not counted, and neither is its pressure. No key |
| `GET` | `/metrics` | Prometheus exposition. No key |
| `GET` | `/gatewayz` | The front doors. On a **leader**: how many doors it has heard from, the endpoint's total and per-door in-flight / rpm, the door-liveness window and the spend lag bound. On a **door**: how stale its mirrored registry is and how big its backlog is. No key, no key ids, no prompts. Not part of the frozen contract |
| `GET` | `/admin/v1/version` | Binary version + contract version |
| `GET` | `/admin/v1/capabilities` | The frozen route list, feature flags, and the engine drivers linked into this binary |
| `GET` | `/admin/v1/status` | The compact summary behind `opod status --json` |
| `GET` | `/admin/v1/config` | Effective config, secrets redacted (read-only: config is edited in the file) |
| `POST` | `/admin/v1/healthcheck` | One real completion through the gateway: did it answer, how fast, which engine |
| `GET` | `/admin/v1/nodes` | List nodes |
| `POST` | `/admin/v1/nodes/register` | (scope=admin or node) Worker registration |
| `POST` | `/admin/v1/nodes/heartbeat` | (scope=admin or node) Worker heartbeat with loaded models, the engine's load sample and whether it sleeps |
| `POST` | `/admin/v1/nodes/{id}/drain` `/undrain` | Take a worker out of rotation (no new requests or shard parts; in-flight finishes; survives heartbeats and a re-register) / put it back. The leader's own `local` node is refused |
| `POST` | `/admin/v1/nodes/{id}/sleep` `/resume` | Sleep tier: the worker's engine drops its GPU working set / wakes (vLLM sleep mode; engines without one answer 501 `unsupported`) |
| `DELETE` | `/admin/v1/nodes/{id}` | Forget a node |
| `GET` | `/admin/v1/models` | List installed models |
| `GET` | `/admin/v1/catalog` | List catalog entries |
| `POST` | `/admin/v1/models` | Install a model (auto-delegates to shard orchestration if `sharding.required`; `nodes` pulls and warms it on named workers) |
| `DELETE` | `/admin/v1/models/{id}` | Uninstall (auto-handles sharded teardown) |
| `POST` | `/admin/v1/models/{id}/load` | Bring a model into engine memory under admission control (`swap`, `pin`, `priority`); 409 `needs_swap` / `blocked_by_pinned`, 422 `impossible` |
| `POST` | `/admin/v1/models/{id}/move` | Move a whole model between workers by overlap (feature `model_move`). Body `{from, to, ready_timeout_seconds?, drain_timeout_seconds?}`. `200 {model, from, to, steps[], note?}`; `404` unknown model or node; `409` a refusal made before anything was touched (the reason is the message); `502 {error, steps[]}` a move that aborted — the last step says what serves now. Synchronous; the steps are also events `model.move_*` |
| `POST` | `/admin/v1/models/{id}/unload` | Drain in-flight requests, drop the model from engine RAM (weights stay; cleared from desired placements so it stays unloaded across restarts). Engines that don't support it return `status:"noop"` |
| `GET` | `/admin/v1/memory` | Live engine residency + the desired set (what `opod model ps` prints) |
| `POST` | `/admin/v1/adapters` | Load a LoRA adapter on every worker holding the base model, no restart. Body `{name, source, rank?}`; answered per worker. vLLM sizes its LoRA slots at start, so a worker whose engine was started without adapters, or for a smaller rank than `rank`, refuses with both numbers — that adapter needs the worker restarted with it in `OPOD_ADAPTERS` |
| `DELETE` | `/admin/v1/adapters/{name}` | Drop one |
| `GET` | `/admin/v1/tokens` | List API keys (no hash, no plaintext) |
| `POST` | `/admin/v1/tokens` | Create a key — returns plaintext ONCE |
| `PATCH` | `/admin/v1/tokens/{id}` | Edit a key's expiry (`expires_at` / `ttl_seconds`) |
| `DELETE` | `/admin/v1/tokens/{id}` | Revoke a key |
| `POST` | `/admin/v1/usage/push` | A front door's usage rows (feature `gateway_spend`), deduplicated by a row id the door mints. Also the door's heartbeat, carrying its own load |
| `GET` | `/admin/v1/spend` | The snapshot each door polls: `{ts, doors, gateways, lag_bound_ms}` — how many doors the leader believes are serving, their names, and the published lag bound. Per-key ceilings and shares left with the per-key policy (2026-09-28) |
| `GET` | `/admin/v1/gateways` | The doors the leader has heard from — including one that stopped pushing |
| `GET` | `/admin/v1/shards` | List shards across all models, per gang |
| `GET` | `/admin/v1/shards/processes` | Process state of the parts the leader supervises (`running` · `starting` · `stopped` · `failed` · `crashloop`) |
| `POST` | `/admin/v1/shards/create` | Orchestrate a sharded model. `409` when the workers for the shape cannot be found (never the leader's own row, a draining or a silent node) — refused with the numbers, the gang being replaced left serving |
| `DELETE` | `/admin/v1/shards/{model_id}` | Tear down every gang of a sharded model |
| `DELETE` | `/admin/v1/shards/{model_id}/{gang_id}` | Tear down one gang (feature `shard_groups`); the model keeps its placement while any gang is left |
| `GET` | `/admin/v1/usage/stream` | Per-call usage records, by cursor with replay — the raw facts; summaries are a consumer's job |
| `GET` | `/admin/v1/events/stream` | Lifecycle + admin-action events, by cursor with replay |
| `GET` | `/admin/v1/events` | Server-Sent Events. Push-on-change for `models` / `nodes` / `shards` topics. Sends a 25 s `keepalive` comment so proxies don't idle. Auth via Bearer or `?key=` query param. |
| `GET` | `/admin/v1/cache/stats` | Response-cache driver + counters |
| `DELETE` | `/admin/v1/cache` | Flush the response cache (`?namespace=…` or `?all=1`) |

All `/admin/v1` routes require an admin key (`opod token create --admin`), except the worker pair, which also accepts a node key. The subset an external manager may rely on across releases is frozen in `internal/leader/contract.go` and served by `/admin/v1/capabilities`; see [ARCHITECTURE.md → Stable admin surface](ARCHITECTURE.md#stable-admin-surface-v1).

### Model routing rules

`model` field in the request determines backend:

| Model name | Routes to |
|---|---|
| exact catalog ID (`qwen3-coder-30b`) | that model: its gang if it is sharded, else the leader's own engine if it holds it, else the least loaded live worker that does |
| `auto` | the leader's **default model** (`router.default_model`, auto-picked on first `opod up`). Not a heuristic and not a vendor chain — those left with ADR-022 |
| an engine-native name (`qwen2.5-coder:14b`) | whichever node reports it loaded; no catalog entry is needed |
| a vendor id (`gpt-4o`, `claude-…`) | nothing. Core does not proxy to a vendor; the request fails at the engine, which has no such model |

---

## CLI reference

The CLI is the whole interface: every admin action is a command here, and each one calls an `/admin/v1` route you can also script. Most subcommands launch an interactive picker (type to filter, ↑↓/enter) when called with no argument or an unknown ID, so you rarely need to memorize an ID.

```
# --- lifecycle ---
opod up [--no-wizard] [--auto-pull=false] [--exclusive] [--unload-on-exit]
        [--config PATH] [--role gateway --leader http://leader:8080]
                                  Start the local node (first-run wizard picker
                                  installs a starter model unless --no-wizard is
                                  set — only with router.pull_default_model /
                                  OPOD_PULL_DEFAULT_MODEL=1, off by default;
                                  --exclusive = one resident model per machine).
                                  --role gateway starts a FRONT DOOR for another
                                  leader's endpoint: /v1 only, no /admin/v1, no join
                                  surface, no engine — its worker list mirrored from
                                  --leader and its usage pushed there
opod down [--no-unload]          Stop the local node and release engine memory
                                  (--no-unload leaves models resident)
opod status [--json]             Show local + cluster status
opod join "<url>?token=…" [--gpu N] [--vram-budget GB]
                                  Join an existing cluster as a worker (optionally
                                  pinned to one GPU and a memory budget)
opod doctor                      Diagnose common problems
opod update [--check] [--version vX.Y.Z] [--force]
                                  Check / install the latest (or a pinned) Opod release
opod upgrade                     Alias for `update`
opod completion <bash|zsh|fish>  Print a shell completion script
opod version                     Print version

# --- nodes ---
opod node ls                     List nodes
opod node show <id>              Inspect a node
opod node drain <id>             Drain a node (no new requests or shard parts; in-flight finishes)
opod node undrain <id>           Put a drained node back in rotation
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

# --- container images (embedded manifest; offline, read-only) ---
opod image ls [--engine E] [--vendor V] [--json]
                                  The published images: engine, vendor,
                                  platforms, gang (rpc | ray | -), proven
opod image show <image> [--json] One image: node requirements, limits, base
opod image recommend <model> [--vendor V] [--json]
                                  Which image serves a catalog model (or
                                  hf:/file: id), one pick per vendor + why not

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
opod model move <id> --from <node> --to <node>
                                  Another worker takes the model over by overlap:
                                  the source serves until the target does, in-flight
                                  requests finish, then the source unloads. Refused
                                  up front when it cannot work; needs the leader
opod model ps [--json]           Models resident in engine memory + what is free

# --- weights on a node ---
opod fetch <hf-repo> <file> [--dir D] [--revision R] [--sha256 S]
                                  Make one GGUF present: exclusive per file, atomic,
                                  digest-checked, skipped when already there
opod fetch --snapshot <hf-repo>[@rev] [--dir D]
                                  The same for a safetensors model: the file set
                                  vLLM / SGLang loads, under <dir>/<repo>@<rev>/
                                  Either mode, refused by the Hub, fails BY NAME:
                                  hub-token-missing (no HF_TOKEN, and the repo is
                                  gated or private), hub-token-refused (the token is
                                  not accepted), hub-access-denied (the token is good
                                  and its account may not read this repo)
opod cache ls [--dir D] [--json] What is cached, what is ours, when it was last used
opod cache prune [--dir D] [--keep a.gguf,repo@rev] [--min-age 24h] [--target-free GB]
                 [--sweep 24h] [--apply] [--json]
                                  Reclaim space; a dry run unless --apply, and never
                                  a file this binary did not fetch; --sweep also clears
                                  crashed pulls older than that (0 = off)

# --- sharded models (one model split across N machines) ---
opod shard create <model> [N] [--nodes a,b,c] [--tp N] [--pp N] [--gang ID]
                                  Orchestrate a sharded model across N workers
                                  (--nodes pins them; --tp/--pp for a vLLM or SGLang
                                  gang; --gang names a SECOND copy of the model that
                                  serves in parallel. Without --gang, a create
                                  replaces every gang of the model)
opod shard ls                    List every part + coordinator, per gang
opod shard remove <model> [--gang ID] [--yes]
                                  Tear down a sharded model, or just one gang
                                  (prompts unless --yes)

# --- API keys / tokens ---
opod token create [name]         Issue an API key (--admin, --node, --ttl 7d, --expires-at DATE)
opod token ls                    List API keys
opod token renew <id>            Extend expiry (--ttl DURATION | --expires-at DATE)
opod token expire <id> [--in D]  Expire a key now (or in DURATION, e.g. --in 1h)
opod token revoke <id>           Revoke a key

# --- connecting clients ---
opod connect <client>            Print the copy-paste config snippet for a client
                                  (--list shows the 15-client roster; --model,
                                  --base-url, --token overrides; --retries N adds
                                  X-Opod-Num-Retries)
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

- Check the load the workers report: `curl http://<leader-host>:8080/loadz` (KV-cache use, queue depth, tokens/s, TTFT p50/p95). Saturated under load: add a worker with the same model, or upgrade.
- Check engine reachability and the host's own limits: `opod doctor`.
- Sticky sessions are **off by default** — set `router.sticky_session_ttl_seconds` (or `OPOD_STICKY_SESSION_TTL_SECONDS`) so a user's turns come back to the worker that still holds their prefix.
- On a GPU worker, is the model actually on the card? An accelerated worker passes `--n-gpu-layers` for llama.cpp, but a plan that pins `ngl` wins — a partial offload serves slowly and looks fine.
- Model is CPU-falling-back? Check the leader's stderr where `opod up` is running — engine driver errors are logged there. Per-node log streaming is on the roadmap.
- A model too big for one box? Split it: `opod shard create <model>`.

### A client shows "model not found"

- Make sure the model ID in your request matches an installed catalog ID (`GET /v1/models` lists them); core does not proxy vendor model ids.
- `opod model ls` to confirm what's loaded.

---

## FAQ

**Can I run Claude or GPT on my hardware?**
No — those are closed-weight proprietary models, and core does not proxy to vendor APIs (that surface left with ADR-022). Opod serves open-weight models on your hardware; the control plane can front a vendor as a *remote endpoint* when you want one in the same fleet.

**Do I need a GPU?**
For real coding work, yes — either an NVIDIA GPU on Linux or an Apple Silicon Mac. CPU-only works via llama.cpp for tiny models (3B and under) and is useful for testing only.

**Can I mix Macs and NVIDIA boxes in one cluster?**
Yes. That's a core design goal. There are no "pools": a worker serves whatever its own engine has loaded, and the router picks per request among the workers that hold the model asked for — so a Mac running MLX and a Linux box running vLLM can back the same model id, or different ones.

**Does Opod work without internet?**
Yes, after initial model download. Nodes talk to the leader directly over your LAN — there's no external coordination service to reach. For air-gapped installs, point `opod model add` at a local mirror and set `OPOD_SKIP_SOURCE_CHECK=1` to skip the upstream-registry probe.

**How is this different from Ollama?**
Ollama is a great single-node inference engine. Opod is the *orchestration layer* across many machines. Opod uses Ollama as one of its supported engine backends.

**How is this different from vLLM?**
vLLM is a single-node inference server. Opod orchestrates vLLM (and others) across your fleet.

**How is this different from exo?**
exo is the closest project conceptually. Opod differs by: (1) an OpenAI-compatible gateway with per-user API keys, (2) explicit placement + sharding on three backends (llama.cpp RPC, vLLM + Ray, SGLang), (3) multi-tenant API keys with scopes, expiry and a per-key usage stream, (4) Prometheus metrics, OTLP traces and a typed event + usage stream, (5) Go single-binary install.

**Does Opod train models?**
No. Use Axolotl / Unsloth / torchtune for training. Bring back a LoRA adapter; Opod will serve it.

**Why Go and not Rust?**
Go ships a static binary as fast as Rust for this workload, with a faster development loop. We may rewrite hot paths in Rust if measurements justify it.

**Is there a hosted version?**
Not initially. The product is the software you run.

**Can I use my own Tailscale account?**
Opod has no built-in tailnet integration today — clustering is plain LAN HTTP, and there are no `mesh.*` config keys. Running Opod *over* a Tailscale network you manage yourself works fine (it's just IP connectivity between your machines); a built-in tsnet backend is a possible future addition.

**Does Opod support AMD GPUs?**
Yes on Linux + ROCm. Both the llama.cpp and the vLLM worker images have served on a Radeon RX 7900 XTX host
(single card; peer-to-peer between consumer Radeon cards faults, so tensor parallel across two of them is a
machine limit, not an image one). SGLang's ROCm build is Instinct-only upstream, so a Radeon card serves
through llama.cpp or vLLM instead. Per-image limits are in [`images/README.md`](images/README.md) and in
`opod image show <image>`.

**Can I run this on Windows?**
Workers no (no MLX, no native vLLM). Leader/CLI yes via WSL2. Native Windows isn't a near-term priority.

---

## Also known as / search terms

Opod is a **self-hosted LLM gateway** and **inference router**. If you found this repo searching for an alternative to a hosted service or a frontend for a local engine, the answer is yes:

- **LiteLLM alternative** (Go binary instead of Python) — one OpenAI-compatible gateway in front of your own engines, plus multi-node routing (no vendor proxying).
- **Ollama frontend / multi-machine Ollama** — Opod orchestrates several Ollama (or vLLM / MLX-LM / llama.cpp) nodes behind one gateway with auth and audit.
- **Private inference cluster / on-prem LLM gateway** — keep all inference on a trusted LAN or Tailscale; nothing leaves your network.
- **Self-hosted Cursor / Aider / Continue backend** — drop-in OpenAI-compatible URL for IDE coding tools.
- **AI gateway with per-user keys + usage records + audit** for teams of 10-50 spending $30k+/yr on Claude / GPT.
- **Sharded inference orchestrator** — split a model larger than any single machine across multiple workers via `llama.cpp` RPC, vLLM + Ray, or SGLang's distributed launcher.

Related concepts: local LLM, on-prem AI, private GPT, GGUF, multi-tenant inference, model placement, fallback chain.

---

## License

Apache License 2.0 — see [LICENSE](LICENSE) and [NOTICE](NOTICE). Contributions are accepted under the
[Developer Certificate of Origin](https://developercertificate.org/) (`git commit -s`); there is no CLA.

You can use Opod commercially, modify it, fork it, embed it, redistribute it, and compete with it. The only requirements are (a) keep the license + notice, (b) state significant changes you made. No copyleft, no usage limit. The control plane (`opodcp`) is a separate proprietary product with a free tier of 4 GPUs in one cell.

## Acknowledgments

Opod stands on the shoulders of:

- **vLLM** — for fast NVIDIA inference
- **SGLang** — for RadixAttention and a prefix cache that survives across requests
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
