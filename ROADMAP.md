# Roadmap

Last updated: 2026-09-18 · Apache-2.0, no usage limit · [Releases](https://github.com/opod-io/opod-core/releases) · milestone history: [docs/archive/TASKS-milestones-M0-M5.md](docs/archive/TASKS-milestones-M0-M5.md)

**What `opod` is.** A CLI-only inference runtime for open-weight models on machines you already have.
One leader and any number of workers: `opod up` starts the leader (OpenAI-compatible gateway, router,
auth, join tokens), `opod join <leader>` turns another machine into a worker. It serves models, splits
one model across machines with llama.cpp RPC, and gets out of the way. No daemon phones home, no
account, no usage ceiling.

**What `opod` is not, and will not become.** Not a dashboard, not a fleet manager, not a billing
system. Everything in that direction lives in a separate product and is not part of this repo — see
[§ Deliberately out of scope](#deliberately-out-of-scope). This roadmap only describes the runtime.

---

## Shipped

| | |
|---|---|
| **Serving** | OpenAI-compatible `/v1/chat/completions` (streaming and not), `/v1/models`, `/v1/embeddings`. Vision (image content blocks). |
| **Engines** | Ollama · llama.cpp (incl. the RPC pair for sharding) · vLLM · SGLang · MLX · any OpenAI-compatible backend. Typed engine errors, a health watchdog, and an `Adapt` hook that reads the engine's own refusal and corrects once (vLLM's context-length refit). |
| **Cluster** | `opod join` with HMAC-SHA256 mutual auth over a 5-minute replay window · heartbeats carrying loaded models · placement reconciliation · a router that picks per request (local-preferred, then least-loaded) and, since v0.6, prefers a faster fallback when the primary's rolling p95 exceeds a threshold · load-aware scoring (in-flight, queue depth, KV-cache use, prefix affinity). |
| **Sharding** | `opod shard create <model> [N]` launches `rpc-server` on the chosen workers and the coordinator on the rank you name, with automatic GGUF distribution (sha256-verified), crash recovery with backoff, and rollback on failure. |
| **Weights** | `opod fetch` — one exclusive, atomic, digest-checked pull per file, with a pinned Hub revision cached as `<repo>@<rev>` so two revisions coexist on a node. `opod fetch --snapshot <repo>[@rev]` does the same for a safetensors model — the file set vLLM or SGLang loads, each weight file checked against the Hub's digest — and a worker serves from a complete snapshot instead of pulling at launch. `opod cache ls|prune` deletes only files this binary fetched, never anything it did not. |
| **Auth & limits** | per-key scopes, rpm / tpm / daily-token quotas, expiry, an audit log, and a usage stream with cursors. Plan and auth are watched files, so limits change at runtime without a restart. |
| **Operations** | `opod doctor`, `opod node ls/show/drain/remove`, `opod model ls/ps/load/unload`, `--json` on every read command, shell completion, an interactive picker, a first-run wizard. Prometheus metrics, OTLP traces across all four drivers, the leader's own logs over OTLP beside stderr, reference Grafana dashboards. |
| **Packaging** | Homebrew, `.deb`, `.rpm`, `install.sh`, `linux/arm64` and `darwin/arm64` builds, and container images per engine × vendor, every upstream base pinned by digest (`images/build.sh refresh-bases` moves a pin, deliberately). |

---

## Next

| Item | What it adds | Size |
|---|---|---|
| **Auto-rebalancing sharding** | `N` is the operator's today; pick it from worker count, model size and free VRAM | M |
| **Mesh backends** | the interface is defined and the LAN backend ships; a `tsnet` (and later NetBird) backend lets workers join across networks | M |
| **Live model migration** | move a loaded model between workers without a cold start | M |
| **Probe port for `/loadz`** | serve the scaling signals on a plain-HTTP probe port (or document the CA) so a scraper need not skip certificate verification when the leader serves TLS — see `docs/SCALING-SIGNALS.md` | S |
| **Engine liveness in the heartbeat** | a worker whose engine is crash-looping still reports as a worker, so anything counting workers overstates capacity | S |
| **Uneven split flags** | a pipeline layer partition, and a llama.cpp tensor split, settable per worker from the plan — lets one model use cards of different sizes instead of being bounded by the smallest | M |

## Later

| Item | Why it waits |
|---|---|
| **Signed model catalogs** | supply-chain trust for catalog entries — `minisign` signatures verified by `opod model add`. Wanted, not urgent. |
| **Embeddable Go library** | `pkg/runtime`, `pkg/router`, `pkg/store` so a desktop app or IDE plugin can import the runtime instead of shelling out. A package-layout break, so it waits for an API-stability commitment. |
| **Image generation** | `/v1/images/generations` needs a job-aware scheduler: 5–30 s synchronous calls with a different VRAM profile would squeeze out chat. Only if there is demand. |

Every release is cut from conventional commits — see `.github/workflows/auto-release.yml`.

---

## Deliberately out of scope

Not "later" — **not this project**. Each was considered and declined for a reason.

| | Why not |
|---|---|
| **A web dashboard, SSO, RBAC, teams** | core is CLI-only. Per-key scopes, quotas and the audit log are the accountability story on a trusted network. |
| **Cost, billing, or dollar figures** | the usage stream records tokens, never money. What a token costs depends on hardware, power and contracts this binary cannot see. |
| **Vendor egress: Bedrock, Vertex, hosted-model key pools** | serving *your* weights on *your* machines is the whole point. Routing to someone else's API is a different product. |
| **Non-chat protocol surfaces** (Anthropic Messages, `/v1/rerank`, audio transcription and speech) | one protocol, done properly. These were removed in the 2026-09 contraction. |
| **Content policies, output filtering, guardrail implementations** | the interface and the event stream stay; the policies belong where the request originates. |
| **Kubernetes, Helm, operators, CRDs** | `opod` runs as a process. Anything that schedules processes across a fleet is the orchestrator's job, not the runtime's. |
| **Training and fine-tuning** | use `axolotl`, `unsloth`, or `torchtune`. |
| **A vector store** | an adapter for SQLite-VSS / pgvector may ship with signed catalogs; running one will not. |
| **Video generation, real-time voice agents** | minutes-per-inference render farms and full-duplex sub-300 ms loops are different operational models. They belong in separate projects that use `opod` for the LLM leg. |
| **Phoning home** | no automatic update check, no telemetry. `opod update` is something you type. |

The pre-pivot roadmap, written when this repo also carried the product surfaces above, is kept for history at
[`docs/archive/ROADMAP-pre-pivot-2026-06-12.md`](docs/archive/ROADMAP-pre-pivot-2026-06-12.md).
