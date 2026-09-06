# Catalog

Every YAML file in this directory is one entry in Opod's model catalog. Entries are loaded at startup and surfaced through `opod model search`, `opod model info <id>`, `opod model add <id>`, and the Models tab of the web UI.

Adding a model is usually a one-file PR: drop in `catalog/<id>.yaml` and open a pull request. No code change required.

## File layout

- One file per model. Filename is `<id>.yaml` (kebab-case, matches the `id:` field).
- Either `.yaml` or `.yml` is accepted; we standardize on `.yaml`.
- Nested directories are not scanned — entries must live at the top level of `catalog/`.

## Schema

The parser is `internal/models/catalog.go`. Anything not in this schema is silently ignored.

### Required fields

| Field | Type | Description |
|---|---|---|
| `id` | string | Unique catalog ID, used in CLI/API calls. Kebab-case. Must match the filename. |
| `display_name` | string | Human-readable name for UI listings. |
| `source` | object | Where to fetch weights from. See [Source](#source). |
| `size_bytes` | int | On-disk weight size in bytes. Used for the picker and for placement. |
| `quant` | string | Quantization label (`q4_k_m`, `q8_0`, `f16`, …). Display only. |
| `context_window` | int | Maximum tokens the model accepts (input + output). |
| `capabilities` | []string | One or more of: `chat`, `tools`, `vision`, `audio`, `embedding`, `rerank`. Routing uses these to match requests. (`audio` is a discovery tag today — declare it on audio-capable models, but the API path for audio input is still pending.) |
| `recommended_engines` | []string | Engines that can serve this model, in preference order. One or more of: `ollama`, `vllm`, `mlx`, `llamacpp`. |
| `hardware` | object | Minimum hardware. See [Hardware](#hardware). |
| `tags` | []string | Free-form search tags (`small`, `code`, `vision`, `apache-2.0`, …). |

### Optional fields

| Field | Type | Description |
|---|---|---|
| `sharding` | object | Set when the model must be split across multiple nodes. See [Sharding](#sharding). |
| `license` | string | Short identifier (SPDX where possible) of the model's release license. Examples: `apache-2.0`, `mit`, `llama-3-community`, `llama-4-community`, `gemma`, `lfm-open`, `nvidia-open`. **Required** — CI fails on missing licenses. Surfaced in `opod model info` so commercial users see it before install. |
| `license_url` | string | Canonical license text URL — usually the HuggingFace LICENSE file. Rendered alongside `license` in `opod model info`. |
| `released` | string | Model's public release date in `YYYY-MM-DD` form. Lets users sort the catalog by recency and gauge how stale an entry is. Approximate is fine — use the first of the month if a precise day isn't known. Rendered in `opod model info`. |
| `fallback` | []string | **Generic** ordered fallback list — tried when the router can't classify the primary's failure into a more specific category (engine down, model not loaded, 503, timeout). Tried in order; the first that succeeds wins. Transparent to the client — the response carries the requested model name. Operators see hits in the audit log + stderr. |
| `fallback_on_context_length` | []string | Typed fallback — replaces `fallback` when the primary rejects with a context-length-exceeded error. Typically points at long-context variants. Empty falls through to `fallback`. |
| `fallback_on_content_policy` | []string | Typed fallback — replaces `fallback` when the upstream (typically a vendor) refuses on content-policy grounds. Typically points at a permissively-aligned open-weight model. Empty falls through to `fallback`. |
| `price_prompt_usd_per_1k` | float | Optional per-1k-token USD rate for prompt tokens. 0 = no cost tracking (the right default for open-weight models on your own hardware). Operators set this to internally charge departments for compute. Surfaced in `opod model info`. |
| `price_completion_usd_per_1k` | float | Optional per-1k-token USD rate for completion tokens. Same semantics as the prompt rate. Both rates are snapshotted into each `usage` row at write time, so historical totals stay correct after a rate change. |

**Restricted-license tag convention.** Any entry whose license is not in the permissive set (`apache-2.0`, `mit`, `bsd-2-clause`, `bsd-3-clause`, `lfm-open`) must also carry the tag `restricted-license`. CI enforces this. Users can then `opod model search restricted-license` to find every model with extra terms, and the dashboard's Models tab renders an amber license badge instead of green.

### Source

```yaml
source:
  type: ollama | huggingface | file
  # type: ollama
  ollama_name: "llama3.2:1b"             # the tag passed to `ollama pull`

  # type: huggingface
  repo: "bartowski/Llama-3.3-70B-Instruct-GGUF"
  file: "Llama-3.3-70B-Instruct-Q4_K_M.gguf"   # specific file within the HF repo

  # type: file
  path: "/var/lib/opod/models/llama-3.3-70b-q4_k_m.gguf"   # local FS path
```

| Field | When |
|---|---|
| `type` | always |
| `ollama_name` | `type: ollama` |
| `repo` | `type: huggingface` |
| `file` | `type: huggingface` (optional — defaults to first GGUF in the repo) |
| `path` | `type: file` |

### Hardware

```yaml
hardware:
  min_ram_gb: 16      # required
  min_vram_gb: 12     # optional, GPU-only paths
```

`min_ram_gb` is the unified-memory floor on Apple Silicon (where RAM and VRAM are the same pool), and the system-RAM floor everywhere else. `min_vram_gb` is checked only when an NVIDIA / Metal path is selected. The picker uses these to refuse `opod model add` on under-spec hardware (override with `--force`).

For sharded models, `min_ram_gb` is the **combined** requirement across all shards.

### Sharding

For models too large for any single node — set this and the auto-orchestrator will split the model across workers via llama.cpp RPC.

```yaml
sharding:
  required: true
  default_shards: 2          # used when `opod shard create <id>` is called without N
  engine: llamacpp           # only "llamacpp" is supported in v0.4
  rpc_port_base: 50052       # each worker binds rpc-server to rpc_port_base + shard_index
  coordinator_port: 9001     # coordinator (llama-server --rpc <list>) binds locally
```

Prereqs for any sharded entry to actually serve traffic:

1. The GGUF must be readable on the leader at `source.path`. The leader then distributes it automatically: each shard host that doesn't already have the file (matched by sha256) receives it via a verified upload to the worker's `/v1/process/upload` endpoint — no manual copying.
2. `llama.cpp` is installed on every shard host (provides `llama-server` + `rpc-server`).
3. At least `default_shards` workers have joined.

Then either `opod model add <id>` (which will call shard create with `default_shards`) or `opod shard create <id> <N>` explicitly.

## Capabilities reference

| Capability | What it does | API surface |
|---|---|---|
| `chat` | Model accepts conversational messages | `POST /v1/chat/completions`, `POST /v1/messages` |
| `tools` | Model supports tool/function calling | Both chat endpoints; tool blocks in request/response |
| `vision` | Model accepts image content blocks | `POST /v1/chat/completions` with `image_url` (Ollama path) |
| `audio` | Model accepts audio content (discovery tag) | API path pending; declare on models with audio understanding |
| `embedding` | Model returns vector embeddings | `POST /v1/embeddings` |
| `rerank` | Cross-encoder reranking | `POST /v1/rerank` (Cohere-shape response; passes through to llama-server's native `/v1/rerank`, b3580+) |

The router uses capabilities to match a request to a model. A request for embeddings will never hit a `chat`-only model.

## Examples

### Smallest chat model

```yaml
id: llama-3.2-1b
display_name: Llama 3.2 1B Instruct
source:
  type: ollama
  ollama_name: llama3.2:1b
size_bytes: 1321000000
quant: q4_k_m
context_window: 131072
capabilities: [chat]
recommended_engines: [ollama, llamacpp]
hardware:
  min_ram_gb: 2
tags: [small, fast, smoke-test]
license: llama-3-community
license_url: https://www.llama.com/llama3_2/license/
```

### Embedding model

```yaml
id: nomic-embed-text
display_name: Nomic Embed Text v1.5 (768-dim, 8K ctx)
source:
  type: ollama
  ollama_name: nomic-embed-text
size_bytes: 274000000
quant: f16
context_window: 8192
capabilities: [embedding]
recommended_engines: [ollama, vllm, llamacpp]
hardware:
  min_ram_gb: 2
tags: [embedding, retrieval, nomic, apache-2.0]
```

### Sharded large model

```yaml
id: llama-3.3-70b-sharded
display_name: Llama 3.3 70B (sharded, manual GGUF)
source:
  type: file
  path: /var/lib/opod/models/llama-3.3-70b-q4_k_m.gguf
size_bytes: 42949672960
quant: q4_k_m
context_window: 131072
capabilities: [chat, tools]
recommended_engines: [llamacpp]
hardware:
  min_ram_gb: 48      # combined across shards
tags: [sharded, large]
sharding:
  required: true
  default_shards: 2
  engine: llamacpp
  rpc_port_base: 50052
  coordinator_port: 9001
```

### Model with fallback chain

```yaml
id: qwen3-coder-30b
display_name: Qwen3 Coder 30B
# … other fields …
fallback:
  - qwen-coder-14b         # try next if 30B is unavailable
  - qwen-coder-7b          # last resort
```

When a request for `qwen3-coder-30b` can't be served (engine down, 503, timeout), the router tries the fallback chain in order. The response is returned to the client with `model: "qwen3-coder-30b"` — the fallback is invisible. Operators see the substitution in the audit log.

### Model with typed fallback chains

`fallback_on_context_length` and `fallback_on_content_policy` let you target the alternative that's actually appropriate for the failure class. The router classifies the primary's error before walking the chain.

```yaml
id: claude-3-5-sonnet
display_name: Claude 3.5 Sonnet (proxied)
# … other fields …
fallback:                          # generic engine-down / 503 / timeout
  - claude-3-5-haiku
fallback_on_context_length:        # prompt too long for the model
  - claude-3-7-sonnet-200k         # bigger context window
  - qwen-2.5-coder-32b-1m
fallback_on_content_policy:        # vendor refused on alignment
  - qwen3-coder-30b                # permissively-aligned open weight
```

Behavior:

- `fallback_on_context_length` fires only when the classifier identifies a context-length error (`n_ctx exceeded`, OpenAI's "maximum context length", etc.).
- `fallback_on_content_policy` fires on refusal patterns (`content_policy_violation`, "unable to help with that", Bedrock guardrails).
- An empty typed list short-circuits to `fallback`.

Traces tag the choice as `opod.fallback.classifier = generic | context-length | content-policy`. The metric `opod_router_fallback_total{reason}` separates the buckets.

## Submitting a new catalog entry

1. Pick a canonical kebab-case ID. Match the filename.
2. Verify the model actually loads in at least one engine you list under `recommended_engines`.
3. Measure `size_bytes` from the downloaded weight files (or `ollama show <tag> --modelfile` for Ollama entries).
4. Set `hardware.min_ram_gb` conservatively — better to refuse install on borderline hardware than to crash the engine.
5. Open a PR. Two CI tests guard the catalog:
   - **Per-PR**: `cmd/opod/docs_drift_test.go` parses every YAML and asserts the filename matches the `id:` field.
   - **Daily**: `.github/workflows/catalog-live.yml` HEADs every entry's `source:` against Ollama / HuggingFace and fails if a tag is gone (renamed, deleted, repo private). Run it locally with `CATALOG_LIVE_CHECK=1 go test -run TestCatalogSourcesReachable ./cmd/opod/`.

For models that need GGUF prep or aren't on Ollama, include the prep steps as a top-of-file comment (see `llama-3.3-70b-sharded.yaml` for an example).

If you want a model added but don't want to author the YAML yourself, open a [catalog request issue](https://github.com/opod-io/opod/issues/new?template=catalog_request.yml).
