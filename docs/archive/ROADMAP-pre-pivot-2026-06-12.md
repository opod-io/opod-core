# Roadmap — multimodal + accessibility

Last updated: 2026-06-12 · Auto-released on every `feat:` / `fix:` commit; see [Releases](https://github.com/opod-io/opod/releases) for the current version · See [TASKS.md](TASKS.md) for the per-task tracker.

This file is the strategic plan. It groups everything into three buckets — **modalities that fit Opod's architecture**, **modalities that stretch it**, and the **eight accessibility bets** that turn open-source AI from "I can run a model" into "my team uses this in production."

Video and real-time voice agents are intentionally out of scope. They belong in sibling projects (`Reel` and `Murmur`) that depend on Opod for the LLM piece — see [§ Out of scope](#out-of-scope).

---

## Buckets

### A. Fits naturally — extend the gateway in place (v0.4)

These reuse the existing Engine interface, router, and store. They add endpoints or capability flags; the operational model stays the same.

| Item | Endpoint | Engines that support it | Compat notes | Status |
| --- | --- | --- | --- | --- |
| **Vision (image input)** | `POST /v1/chat/completions` with `image_url` content blocks | Ollama (`images: []`), vLLM, MLX-LM | `engines.Message.Content` → needs `Images []string`. OpenAI content-array parsing in `internal/api/openai.go`. Anthropic `image` blocks in `internal/api/anthropic.go`. Catalog already has `vision` capability. | **Shipped in v0.4** (Ollama path) |
| **Embeddings** | `POST /v1/embeddings` | Ollama (`/api/embeddings`), vLLM (`/v1/embeddings`), MLX-LM | New `Engine.Embed(ctx, model, input) []float32` method. Catalog entries get `embedding` capability + `embedding_dim`. Router picks by capability. | **Shipped in v0.4** (Ollama path) |
| **Rerank** | `POST /v1/rerank` (Cohere shape) | BGE / Jina / mxbai cross-encoders via llama-server `/v1/rerank` (b3580+) | Cohere-format handler passes through to llama-server's native `/v1/rerank`; the llama.cpp single-node driver is the transport. | **Shipped** |

### B. Stretches the gateway — works but requires new code paths (v0.5–v0.6)

These add endpoints that don't fit the chat-streaming pattern. Worth doing but each is a noticeable code-shape change.

| Item | Endpoint | Engines | Stretch | Verdict |
| --- | --- | --- | --- | --- |
| **ASR (speech → text)** | `POST /v1/audio/transcriptions` | faster-whisper, NVIDIA NeMo (Nemotron 3.5 ASR), vLLM-whisper | Synchronous request, non-streaming response. Audio bytes in, text out. | **Shipped** — proxies to a Whisper-compatible endpoint (`engine.whisper_endpoint` / `OPOD_WHISPER_ENDPOINT`); HTTP 501 with setup hint when unconfigured |
| **TTS (text → speech)** | `POST /v1/audio/speech` | Piper, Coqui XTTS, Bark | Output is binary audio (mp3/opus/pcm). Different result shape than chat. | **Shipped** — proxies to a Piper-compatible endpoint (`engine.piper_endpoint` / `OPOD_PIPER_ENDPOINT`); HTTP 501 with setup hint when unconfigured |
| **Image generation** | `POST /v1/images/generations` | Stable Diffusion via diffusers, ComfyUI, Flux | 5–30 s synchronous. Different VRAM profile (squeezes out chat). Router needs to know about "GPU-locked" jobs vs token-streamed jobs. | v0.6 — only if there's demand |

### C. Out of scope — separate apps that depend on Opod

| Workload | Why separate | Sibling project (proposed) |
| --- | --- | --- |
| Video generation (HunyuanVideo, Wan2.1, LTX, Mochi) | Minutes per inference, multi-GB output, needs real job queue + webhook callbacks. Operational model is render farm, not API gateway. | **`Reel`** |
| Real-time voice agents (full-duplex, < 300 ms loop) | Bidirectional streaming, VAD, interruption handling. Tight ASR + LLM + TTS loop in one socket. | **`Murmur`** — uses Opod as the LLM backend |

---

## Orchestration bets (the actual gateway value)

Scope filter: Opod is an **orchestration / router / gateway** for open-weight LLMs running on a trusted network. RBAC, SSO, billing-per-user analytics, and content policies are explicitly **out of scope** — they're enterprise-SaaS feature creep and other projects do them better. We assume:

- The network is trusted (LAN, Tailscale, internal VPN)
- Existing per-user API keys + daily token quotas + full audit log are sufficient for accountability
- Operators want better routing decisions, not more user management

With that filter, here's what genuinely moves the needle:

| # | Bet | Why it's gateway value | Where it lives | Effort | Target |
| --- | --- | --- | --- | --- | --- |
| 1 | **Latency-aware fallback** | Router silently prefers a faster fallback when the primary's recent p95 latency exceeds a threshold. Same code path as failure-fallback, new trigger condition. | ✅ Shipped v0.6. Per-model rolling window (default 50 samples). When p95 > `router.latency_fallback_p95_seconds` (0 = disabled), the catalog fallback chain is walked for the fastest candidate, which gets tried FIRST. Original primary stays in the chain so a temporarily-slow primary isn't permanently demoted. | **M** | v0.6 |
| 2 | **Hardware abstraction** | Treat M3 Studio + RTX 4090 + Snapdragon X laptop as one compute pool. Scheduler routes by VRAM/load/network. Pure orchestration. | Replace router's `pick()` with a planner over `nodes.capabilities` (already in store) | **M** | v0.8 |
| 3 | **Edge runtime (NAS / Pi)** | Gateway runs on smaller hardware = more deployments. Already cross-compiled to `linux/arm64`. | ✅ **.deb + .rpm shipped v0.6** via GoReleaser `nfpms` (binary at `/usr/bin/opod`, catalog at `/usr/share/opod/catalog`, recommends `llama.cpp` for sharding). Synology `.spk` (DSM SDK toolchain) is the remaining piece for v0.7. | **S** | v0.6 (.deb/.rpm) · v0.7 (.spk) |
| 4 | **Signed model catalogs** | Supply-chain trust for catalog entries. "apt for AI." | `minisign` signatures alongside catalog YAML; `opod model add` verifies before install | **S** | v0.8 |
| 5 | **Embeddable Go library** | Let desktop apps / IDE plugins import `opod/runtime` directly. Biggest distribution channel for OSS AI in 2026 isn't a CLI, it's *embedded in tools developers already use*. | Move CLI glue out of `internal/`; expose `pkg/runtime`, `pkg/router`, `pkg/store` | **L** | v1.0 |

### Explicitly killed (or sibling-projected) scope

| Item | Why not Opod |
| --- | --- |
| RBAC roles / OIDC / SSO | Enterprise auth is feature creep for a gateway on a trusted network. Per-user keys + quotas + audit already cover the accountability story. |
| Cost / billing tracking | Not Opod's job. LLM API charges go to the operator's account; how they slice cost by team / project / user is a manager-side reporting concern, not a gateway concern. The `audit_log` + `usage` tables expose enough data for anyone to roll their own. |
| Billing-per-user analytics dashboards | Same reasoning — manager-side concern. |
| Unified billing across local + vendor calls | Same family as cost / billing tracking — out. Operators reconcile vendor invoices themselves; the `usage` table records what each call cost in tokens, not dollars. |
| Policy-based routing by user / request shape | "By user" routing is tenant-isolation = enterprise creep, same family as RBAC. "By request shape" is already handled — the router picks engines by capability (vision vs embedding vs chat) and falls back via the catalog `fallback:` chain. Anything beyond that is a content-policy concern (see below). |
| Content policies / output filtering | Different operational concern; happens at the client (Claude Code, Cursor) layer, not the gateway. |
| Privacy-by-default RAG | RAG is a separate workload (vector store, retrieval, ranking pipelines). If needed, build it as a sibling project that depends on Opod's embeddings + chat endpoints. |
| Video / real-time voice | Already out of scope — see [§ Out of scope](#out-of-scope). |

---

## Gateway plumbing improvements

Not strategic bets — small, scoped extensions of subsystems that already exist. Listed here so they don't get lost between "modalities" and "bets."

| # | Item | What it adds | Where it lives | Effort | Target |
| --- | --- | --- | --- | --- | --- |
| P1 | **Bedrock + Vertex egress adapters** | Two more vendor routes alongside the existing Anthropic + OpenAI fallback. Lets orgs with AWS / GCP spend keep using their existing billing path. | ✅ **Bedrock shipped** v0.6 (SigV4 via aws-sdk-go-v2; `anthropic.*` model family, non-streaming). ADC auth probe wired for Vertex. **Remaining for v0.7**: Bedrock streaming + non-Anthropic body shapes (amazon.*, meta.*, mistral.*); Vertex body translation (OpenAI/Anthropic → generateContent Contents). | **S** | v0.6 (Bedrock) / v0.7 (Vertex) |
| P2 | **OpenTelemetry / OTLP traces** | End-to-end span coverage: HTTP handler → router → engine driver. Pairs with the existing Prometheus metrics so latency anomalies in Grafana have a corresponding trace to drill into. | ✅ **Shipped end-to-end** in v0.6 across all four drivers. HTTP-layer spans (`otelhttp` on chi, OTLP/HTTP exporter, W3C propagation) + router child spans (`router.Chat`, `router.Embed`, per-attempt span for fallback) + per-driver engine spans (`ollama.Chat`, `vllm.Chat`, `mlx.Chat`, `llamacpp.Chat`) with prompt/completion token counts. Default no-op when `OPOD_OTLP_ENDPOINT` unset (zero overhead). | **S** | v0.6 |
| P3 | **Reference Grafana dashboards** | Importable JSON for cluster overview, per-model, per-user / per-key — covers the same Prometheus metrics already exposed. | ✅ **Shipped** — `dashboards/` with `cluster-overview.json`, `per-model.json`, `per-node.json`; documented in README. No code change. | **XS** | Shipped |

---

## Compatibility review (per item)

Already-shipped subsystems that each item touches. Bold = breaking change, italic = additive only.

| Item | engines.Engine | internal/api/* | internal/store/* | internal/router | catalog YAML schema |
| --- | --- | --- | --- | --- | --- |
| Vision | *add Images []string* | *parse content array* | none | none | none (already has `vision`) |
| Embeddings | *new method `Embed()`* | *new `/v1/embeddings`* | *new `embedding_calls` table* | *route by capability* | *add `embedding_dim`* |
| Rerank | *new method `Rerank()`* | *new `/v1/rerank`* | piggyback embeddings table | route by capability | *add `rerank` capability* |
| ASR | *new sibling interface `ASREngine`* | *new `/v1/audio/transcriptions`* | *audio_calls table* | extend pick() | *add `asr` capability* |
| TTS | *new `TTSEngine`* | *new `/v1/audio/speech`* | reuse audio_calls | extend pick() | *add `tts` capability* |
| Image gen | *new `ImageEngine`* | *new `/v1/images/generations`* | *image_calls + storage* | needs job-aware scheduler | *add `image_gen` capability* |
| (1) Latency fallback | none | none | *add `route_telemetry` table* | extend pick() | *add fallback chain to catalog* |
| (2) HW abstraction | none | none | extend `nodes.capabilities` JSON | **rewrite `pick()`** | none |
| (3) Edge runtime | none | none | none | none | none |
| (4) Signed catalogs | none | none | none | none | *add `signature` field* |
| (5) Go library | **reorganize package layout** | none | none | none | none |

The only **breaking changes** are (2) router rewrite and (5) package layout — both planned for after v0.7 so users have time to adopt.

---

## Sequence

```
Shipped       → Vision (Ollama path) · Embeddings · catalog fallback chain · HMAC mutual auth
                 · GGUF distribution · coordinator-on-worker · OTLP traces end-to-end (all 4 engines)
                 · Grafana dashboards · Bedrock SigV4 signing · Vertex ADC probe · 19-client
                 `opod connect` roster · Latency-aware fallback (bet 1) · .deb + .rpm Edge runtime (bet 3)
                 · Interactive picker · Shell completion · --json on every read command
                 · --summary aggregates for usage/audit · First-run wizard · Real progress bar
                 · Colored output · Did-you-mean for typos · Engine health watchdog · Typed engine errors
                 · Rerank (`/v1/rerank`) · ASR + TTS (`/v1/audio/transcriptions` + `/v1/audio/speech`
                 via Whisper / Piper endpoint proxying)

Next          → Vertex body translation (generateContent) · Bedrock streaming + non-Anthropic families
                 · Anthropic extended thinking + computer use · Synology .spk

Later         → Hardware abstraction (bet 2) · Signed catalogs (bet 4) · Image generation
                 · LoRA hot-loading · Embeddable Go library (bet 5) · API stability commitment
```

Every release is auto-cut from conventional commits — see `.github/workflows/auto-release.yml`.

---

## Out of scope

- **Video generation.** Sibling repo `Reel`. Job-queue model, not gateway.
- **Real-time voice agents.** Sibling repo `Murmur`. Uses Opod as the LLM backend.
- **Training / fine-tuning.** Out of project scope. Use `axolotl`, `unsloth`, or `torchtune`.
- **Vector store as a service.** Opod will ship an *adapter* for SQLite-VSS / pgvector in (4) but won't run its own vector store.
