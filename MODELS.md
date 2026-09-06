# Models — the complete walkthrough

Every model in Opod's catalog, with step-by-step install + use instructions for every common client (curl, Cursor / VS Code, Claude Code, OpenAI SDK, Anthropic SDK). Pick the row that matches your hardware, run the install, copy the snippet for your tool.

> **Tip:** the snippets below are written by hand for reference, but Opod can also generate them automatically with your URL + token already filled in:
>
> ```bash
> opod connect claude-code        # or: cursor, aider, continue, zed, cline, qwen-code, openai-sdk, anthropic-sdk, curl
> opod connect --list             # all 19 supported tools
> ```
>
> Or click **Connect** in the dashboard (`http://localhost:8080`).

> For the catalog summary table see [QUICKSTART](QUICKSTART.md#-use-a-different-model-qwen-llama-deepseek). For "why those specific models?" see [README → Supported models](README.md#supported-models).

---

## 🎯 Picker table — what to install

Scan the column that matches your hardware, then pick by use case. ⭐ = recommended starting point in each row. Catalog has **41 models** at last update (run `opod model search` for the live list, or `opod model search --sort=released` for newest-first).

| Model ID                       | Size   | Min RAM | Chat | Code | Reasoning | Vision | Audio | Long ctx | License     | Notes                                  |
| ------------------------------ | ------ | ------- | :--: | :--: | :-------: | :----: | :---: | :------: | ----------- | -------------------------------------- |
| **Edge — laptop / Raspberry-Pi-ish** |  |  |  |  |  |  |  |  |  |  |
| `llama-3.2-1b`                 | 1.3 GB | 2 GB    |  •   |      |           |        |       |          | Llama 3.2 \* | Smoke test only                        |
| `nomic-embed-text`             | 0.3 GB | 2 GB    |      |      |           |        |       |          | Apache-2.0  | Embeddings for RAG (768-dim, 8K ctx)   |
| `moondream3`                   | 1.2 GB | 4 GB    |  ●   |      |           |   ●    |       |          | BSL-1.1 \*  | Tiny vision-language, runs on Pi       |
| `llama-3.2-3b` ⭐               | 2.0 GB | 4 GB    |  ●   |      |           |        |       |          | Llama 3.2 \* | Edge default                           |
| **Small — 8-16 GB box**        |        |         |      |      |           |        |       |          |             |                                        |
| `mimo-7b`                      | 4.5 GB | 8 GB    |  ●   |      |    ●●     |        |       |          | Apache-2.0  | Xiaomi reasoning-focused 7B            |
| `qwen-coder-7b`                | 4.7 GB | 8 GB    |  ●   |  ●   |           |        |       |          | Apache-2.0  | FIM-capable, older                     |
| `mimo-vl-7b`                   | 4.8 GB | 8 GB    |  ●   |      |           |   ●    |       |          | Apache-2.0  | Small vision-language                  |
| `mimo-audio`                   | 4.8 GB | 8 GB    |  ●   |      |           |        |   ●   |          | Apache-2.0  | Speech + audio understanding           |
| `deepseek-r1-8b`               | 4.9 GB | 12 GB   |  ●   |      |    ●●     |        |       |          | MIT         | Distilled reasoning                    |
| `lfm2.5-8b-a1b` ⭐              | 5.0 GB | 8 GB    |  ●   |      |    ●●     |        |       |    ●●    | LFM Open    | Best on-device MoE (1B active)         |
| `qwen3-8b`                     | 5.2 GB | 12 GB   |  ●   |      |           |        |       |          | Apache-2.0  | General chat                           |
| `glm-4-9b`                     | 5.5 GB | 12 GB   |  ●   |      |           |        |       |    ●●    | GLM-4 \*    | Dense, 128K ctx, strong tool-calling   |
| `qwen3-vl-8b`                  | 5.5 GB | 10 GB   |  ●   |  ●   |           |   ●●   |       |          | Apache-2.0  | Vision + tools (charts, OCR, UI)       |
| `gemma4-e2b`                   | 7.2 GB | 8 GB    |  ●   |      |           |   ●    |   ●   |    ●●    | Gemma \*    | Mobile/edge multimodal (T/I/A)         |
| `mellum2-12b`                  | 7.0 GB | 12 GB   |  ●   |  ●●  |    ●●     |        |       |          | Apache-2.0  | JetBrains MoE coder (2.5B active)      |
| `mistral-nemo-12b`             | 7.1 GB | 12 GB   |  ●   |      |           |        |       |    ●●    | Apache-2.0  | 128K context                           |
| `gemma4-12b`                   | 7.6 GB | 12 GB   |  ●   |      |           |   ●●   |   ●   |    ●●    | Gemma \*    | Encoder-free any-to-any (T/I/A/V)      |
| `pixtral-12b`                  | 7.8 GB | 16 GB   |  ●   |      |           |   ●●   |       |          | Apache-2.0  | Mistral vision-language                |
| `qwen-coder-14b`               | 9.0 GB | 16 GB   |  ●   |  ●●  |           |        |       |          | Apache-2.0  | Dense code+agent                       |
| `qwen3-14b`                    | 9.0 GB | 16 GB   |  ●   |      |           |        |       |          | Apache-2.0  | More capable Qwen3 chat                |
| `phi-4-14b`                    | 9.1 GB | 12 GB   |  ●   |      |    ●●     |        |       |          | MIT         | Strong reasoning per byte              |
| `gemma4-e4b`                   | 9.6 GB | 12 GB   |  ●   |      |           |   ●    |   ●   |    ●●    | Gemma \*    | Mobile/edge multimodal, larger E-tier  |
| **Mid — 24-32 GB box**         |        |         |      |      |           |        |       |          |             |                                        |
| `gpt-oss-20b` ⭐                | 14 GB  | 16 GB   |  ●   |  ●   |    ●●     |        |       |    ●     | Apache-2.0  | OpenAI open-weight, adjustable thinking |
| `qwen3.6-27b` ⭐                | 17 GB  | 24 GB   |  ●●  |  ●●  |    ●      |        |       |    ●●    | Apache-2.0  | 77 % SWE-bench, top consumer pick      |
| `gemma4-26b`                   | 18 GB  | 24 GB   |  ●   |      |           |   ●    |       |    ●●    | Gemma \*    | MoE 4B-active, multimodal              |
| `qwen3-30b`                    | 19 GB  | 24 GB   |  ●●  |      |           |        |       |    ●●    | Apache-2.0  | MoE 3B-active, very fast               |
| `qwen3-coder-30b`              | 19 GB  | 24 GB   |  ●   |  ●●  |           |        |       |    ●●    | Apache-2.0  | MoE 3.3B-active coder                  |
| `qwen-coder-32b`               | 20 GB  | 32 GB   |  ●   |  ●●  |           |        |       |          | Apache-2.0  | Dense, older but proven                |
| `qwen3-vl-32b`                 | 20 GB  | 32 GB   |  ●   |  ●   |           |   ●●●  |       |          | Apache-2.0  | Frontier-tier vision-language          |
| `gemma4-31b`                   | 20 GB  | 32 GB   |  ●   |      |           |   ●●   |       |    ●●    | Gemma \*    | Multimodal, larger than 26B            |
| **Power user — single 80 GB GPU / sharded** |  |  |  |  |  |  |  |  |  |  |
| `llama-3.3-70b-sharded`        | 43 GB  | 48 GB   |  ●●  |      |           |        |       |    ●●    | Llama 3.3 \* | Needs ≥2 nodes                         |
| `gpt-oss-120b`                 | 65 GB  | 80 GB   |  ●●  |  ●   |    ●●●    |        |       |    ●     | Apache-2.0  | ≈ o4-mini reasoning, single H100       |
| `llama-4-scout`                | 67 GB  | 80 GB   |  ●●  |      |           |   ●●   |       |   ●●●    | Llama 4 \*  | 10M context, multimodal                |
| `glm-4.5-air-sharded`          | 70 GB  | 80 GB   |  ●●  |  ●●  |    ●●     |        |       |    ●●    | MIT         | 106B MoE/12B active, agentic           |
| **Frontier — multi-machine sharded** |        |         |      |      |           |        |       |          |             |                                        |
| `step-3.7-flash-sharded` ⭐     | 100 GB | 128 GB  |  ●●  |  ●●  |    ●●     |   ●●   |       |    ●●    | Apache-2.0  | 11B active VLM, fastest frontier MoE   |
| `deepseek-v4-flash-sharded` ⭐  | 150 GB | 160 GB  |  ●●  |  ●●  |    ●●●    |        |       |    ●●    | MIT         | 13B active = fast at frontier quality  |
| `glm-4.6-sharded`              | 200 GB | 224 GB  |  ●●  |  ●●● |    ●●     |        |       |   ●●●    | MIT         | 357B MoE/32B active, agentic coder     |
| `nemotron-3-ultra-sharded`     | 280 GB | 320 GB  |  ●●  |  ●●  |    ●●●    |        |       |   ●●●    | NVIDIA Open \* | Hybrid Mamba-MoE, 1M ctx, MMLU 89.1    |
| `glm-5.1-sharded`              | 400 GB | 416 GB  |  ●●  |  ●●● |    ●●     |        |       |    ●●    | MIT         | Best agentic coder                     |
| `glm-5.2-sharded`              | 460 GB | 480 GB  |  ●●  |  ●●● |    ●●     |        |       |   ●●●    | MIT         | Successor to 5.1, 1M context           |
| `kimi-k2.6-sharded`            | 500 GB | 512 GB  |  ●●  |  ●●● |    ●●     |        |       |    ●●    | Mod. MIT    | #1 open coding benchmarks              |

**Legend** — • basic / ● good / ●● strong / ●●● best-in-tier · ⭐ recommended starting point · \* restricted license — these 12 entries carry the `restricted-license` catalog tag (extra terms beyond Apache/MIT-style permissive; same amber badge the dashboard shows). `opod model search restricted-license` lists them; `opod model info <id>` links the license text.

> Sizes assume Q4_K_M (Ollama's default). Sharded entries assume Q4 baseline; unsloth's 2-bit dynamic GGUFs cut them roughly in half.

**Quick rules of thumb:**
- **I just want to try Opod** → `llama-3.2-3b` (2 GB, runs anywhere).
- **Best on-device edge MoE** → `lfm2.5-8b-a1b` — only 1 B active, MATH500 88.8, MLX-ready.
- **My laptop coding agent** → `mellum2-12b` (Apache-2.0, MoE, 2.5 B active) on 12 GB or `qwen3-coder-30b` on 24 GB.
- **Multimodal on a laptop (text/image/audio/video)** → `gemma4-12b` — encoder-free unified architecture.
- **Best consumer general model** → `qwen3.6-27b` (24 GB) or `gpt-oss-20b` (16 GB).
- **Reasoning-heavy work** → `gpt-oss-20b` (small) or `gpt-oss-120b` (big).
- **My team has 4 Mac Studios** → install the same model (e.g. `qwen3.6-27b`) on each — Opod load-balances automatically. No sharding needed for throughput.
- **A model bigger than any one machine** → sharded tier. Start with `step-3.7-flash-sharded` (Apache-2.0) or `deepseek-v4-flash-sharded` (MIT).

For per-model walkthroughs (with detailed install + client snippets), the curated set below covers the longest-standing entries. For the rest of the catalog (10+ newer additions including Gemma 4 family, MiMo, Qwen3-VL, Pixtral, Moondream), run `opod model info <id>` — same fields as the table above, plus engine compatibility, license, and release date.

---

## How to choose

Use the [picker table above](#-picker-table--what-to-install). It covers every model in the catalog with size, RAM, capability ratings, and license — pick the row that matches your hardware, then go by use case.

The per-model walkthroughs below cover a curated subset with full install + client snippets. For every other catalog entry, run `opod model info <id>` for the same metadata (size, RAM, capabilities, license, release date, engine compatibility), or open a [catalog issue](https://github.com/opod-io/opod/issues/new?template=catalog_request.yml) if you'd like a full walkthrough added.

### Installing models that aren't in the catalog

`opod model add` gives you three paths beyond the curated 41 entries:

**1. Scheme prefixes** — one-liner; no YAML, no hardware-floor check:

```bash
opod model add hf:Qwen/Qwen3-72B-AWQ           # vLLM / MLX / llama-server
opod model add hf:bartowski/Phi-3-mini-GGUF:Phi-3-mini-4k-instruct.Q4_K_M.gguf
opod model add ollama:phi3:mini                # ollama engine only
opod model add file:/abs/path/my-finetune.gguf # llama-server / MLX, pre-downloaded
```

The dashboard's **Models** tab has the same input under "Add custom model".

**2. `--from <my.yaml>`** — install from your own catalog YAML; copied into `~/.opod/catalog/` so it persists:

```bash
opod model add --from ./my-model.yaml
```

**3. Drop-in dir** — write a YAML to `~/.opod/catalog/<id>.yaml` directly, then install by id:

```bash
mkdir -p ~/.opod/catalog
cat > ~/.opod/catalog/my-llama.yaml <<'EOF'
id: my-llama
display_name: My fine-tuned llama
source:
  type: huggingface
  repo: my-org/my-llama-gguf
size_bytes: 8000000000
hardware:
  min_ram_gb: 8
capabilities: [chat]
recommended_engines: [llamacpp]
license: apache-2.0
EOF
opod model add my-llama
```

Paths 2 and 3 give you the same UX as a catalog entry — `opod model search my-llama`, `opod model info my-llama`, hardware-floor checks, license badges all work. Path 1 is the quick fallback when you don't want to write YAML.

---

## Table of contents (curated walkthroughs)

- [`llama-3.2-1b`](#llama-3-2-1b--smallest-smoke-test) — smallest, smoke test
- [`llama-3.2-3b`](#llama-3-2-3b--small-fast-chat) — small fast chat
- [`qwen-coder-7b`](#qwen-coder-7b--coding-baseline) — coding baseline
- [`qwen-coder-14b`](#qwen-coder-14b--general-purpose-coder) — general-purpose coder ⭐
- [`qwen-coder-32b`](#qwen-coder-32b--laptop-frontier-coder) — laptop frontier coder
- [`qwen3-8b`](#qwen3-8b--general-chat-balanced) — general chat, balanced
- [`qwen3-14b`](#qwen3-14b--general-chat-capable) — general chat, more capable
- [`deepseek-r1-8b`](#deepseek-r1-8b--reasoning-thinking) — reasoning
- [`llama-3.3-70b-sharded`](#llama-3-3-70b-sharded--frontier-multi-machine) — frontier, multi-machine
- [`nomic-embed-text`](#nomic-embed-text--embeddings-for-rag) — embeddings for RAG

---

## `llama-3.2-1b` — smallest, smoke test

**What it is:** Meta's Llama 3.2 1B Instruct. The tiniest model that's still coherent for short prompts.

**System requirements:** 2 GB RAM. Any laptop made in the last 5 years.

**Best for:** First-run smoke test. "Does Opod work end-to-end?" Verifying the CLI / UI / Claude Code wiring. Quick latency benchmarks.

**Not for:** Real coding work. Multi-turn agents. Anything where output quality matters.

**Performance:**
- Mac M3 (24 GB): ~80–120 tok/s on Ollama, sub-second TTFT
- Linux + RTX 4090: ~150+ tok/s
- CPU-only: still usable, ~10–20 tok/s

**Install:**
```bash
opod model add llama-3.2-1b
```

**Use it (pick one):**

```bash
# curl
curl http://localhost:8080/v1/chat/completions \
  -H "Authorization: Bearer sk-orc-..." \
  -d '{"model":"llama-3.2-1b","messages":[{"role":"user","content":"say hi"}]}'
```

```bash
# Cursor / VS Code Continue.dev / Aider — OpenAI-compatible base URL
# Base URL: http://localhost:8080/v1
# API key:  sk-orc-...
# Model:    llama-3.2-1b
```

```bash
# Claude Code
export ANTHROPIC_BASE_URL=http://localhost:8080
export ANTHROPIC_AUTH_TOKEN=sk-orc-...
export ANTHROPIC_MODEL=llama-3.2-1b
claude
```

```python
# Python OpenAI SDK
from openai import OpenAI
client = OpenAI(base_url="http://localhost:8080/v1", api_key="sk-orc-...")
resp = client.chat.completions.create(
    model="llama-3.2-1b",
    messages=[{"role": "user", "content": "say hi"}],
)
print(resp.choices[0].message.content)
```

**Switch up when:** you start typing real prompts. Pick `llama-3.2-3b` or `qwen3-8b` next.

---

## `llama-3.2-3b` — small fast chat

**What it is:** Meta's Llama 3.2 3B Instruct. Big enough to hold context, small enough to be snappy on any laptop.

**System requirements:** 4 GB RAM. Comfortable on a M2 MacBook Air.

**Best for:** Personal chat, simple summarization, short-prompt agents, classification. Quick autocomplete-style use cases.

**Not for:** Heavy coding. Long-running agents. Complex multi-tool reasoning.

**Performance:**
- Mac M3 (24 GB): ~50–80 tok/s
- Mac M4 Pro: ~80–120 tok/s
- Linux + RTX 5090: ~200+ tok/s

**Install:**
```bash
opod model add llama-3.2-3b
```

**Use it (pick one):**

```bash
# curl
curl http://localhost:8080/v1/chat/completions \
  -H "Authorization: Bearer sk-orc-..." \
  -d '{"model":"llama-3.2-3b","messages":[{"role":"user","content":"summarize: …"}]}'
```

```bash
# Claude Code
export ANTHROPIC_BASE_URL=http://localhost:8080
export ANTHROPIC_AUTH_TOKEN=sk-orc-...
export ANTHROPIC_MODEL=llama-3.2-3b
claude
```

**Switch up when:** you do real coding work — pick a `qwen-coder-*`. For better chat, jump to `qwen3-8b`.

---

## `qwen-coder-7b` — coding baseline

**What it is:** Alibaba's Qwen 2.5 Coder 7B Instruct. Code-tuned, supports fill-in-the-middle (FIM), good tool use.

**System requirements:** 8 GB RAM. Mac M1/M2/M3 with 16 GB sails. M2 Air 8 GB is borderline (close other apps).

**Best for:** Single-line code completion (Continue / Cursor autocomplete). Short coding chats. Quick refactor suggestions.

**Not for:** Multi-turn agentic loops with lots of tool calls — the 14B is meaningfully smarter for that.

**Performance:**
- Mac M3 24 GB: ~30–50 tok/s
- Mac M4 Pro 64 GB: ~60–80 tok/s
- Linux + RTX 5090: ~150+ tok/s

**Install:**
```bash
opod model add qwen-coder-7b
```

**Use it:**

```bash
# curl
curl :8080/v1/chat/completions \
  -H "Authorization: Bearer sk-orc-..." \
  -d '{"model":"qwen-coder-7b","messages":[{"role":"user","content":"write fizzbuzz in rust"}]}'
```

```bash
# Cursor
# Settings → Models → Override OpenAI Base URL: http://localhost:8080/v1
# Model id: qwen-coder-7b

# Continue.dev (~/.continue/config.json)
{
  "models": [{
    "title": "Opod — Qwen Coder 7B",
    "provider": "openai",
    "model": "qwen-coder-7b",
    "apiBase": "http://localhost:8080/v1",
    "apiKey": "sk-orc-..."
  }]
}
```

```bash
# Claude Code
export ANTHROPIC_BASE_URL=http://localhost:8080
export ANTHROPIC_AUTH_TOKEN=sk-orc-...
export ANTHROPIC_MODEL=qwen-coder-7b
claude
```

**Switch up when:** you start hitting "this completion is wrong" too often → try `qwen-coder-14b`.

---

## `qwen-coder-14b` — general-purpose coder ⭐

**What it is:** Alibaba's Qwen 2.5 Coder 14B Instruct. The sweet spot for most laptops — strong code + agent capability while still fitting in 16 GB.

**System requirements:** 16 GB RAM minimum, 24 GB recommended (leaves room for Chrome / your editor / Slack).

**Best for:** Real coding work via Cursor / Claude Code / Aider. Multi-turn refactors. Agentic loops with tool use. Code review. Test generation.

**Not for:** Frontier reasoning tasks where you'd reach for o1 / R1 — use `deepseek-r1-8b` for those. Models bigger than 16 GB if you're tight on RAM.

**Performance:**
- Mac M3 24 GB: ~20–35 tok/s
- Mac M4 Pro 64 GB: ~40–60 tok/s
- Linux + RTX 5090: ~100+ tok/s

**Install:**
```bash
opod model add qwen-coder-14b
```

**Use it (this is the model most people should default to):**

```bash
# curl
curl :8080/v1/chat/completions \
  -H "Authorization: Bearer sk-orc-..." \
  -d '{"model":"qwen-coder-14b","messages":[{"role":"user","content":"refactor this …"}]}'
```

```bash
# Claude Code — this is where it shines
export ANTHROPIC_BASE_URL=http://localhost:8080
export ANTHROPIC_AUTH_TOKEN=sk-orc-...
export ANTHROPIC_MODEL=qwen-coder-14b
claude
# Now claude can edit files, run bash, etc. — using your local model
```

```python
# Anthropic SDK (works because Opod speaks Anthropic format too)
from anthropic import Anthropic
client = Anthropic(base_url="http://localhost:8080", api_key="sk-orc-...")
resp = client.messages.create(
    model="qwen-coder-14b",
    max_tokens=2048,
    messages=[{"role": "user", "content": "explain CRDTs in 100 words"}],
)
print(resp.content[0].text)
```

**Switch up when:** You have a 64 GB+ machine and want the strongest single-box coder → `qwen-coder-32b`. For frontier-tier you need multi-machine sharding.

---

## `qwen-coder-32b` — laptop frontier coder

**What it is:** Alibaba's Qwen 2.5 Coder 32B Instruct. Approaches GPT-4 / Sonnet quality on coding tasks. The biggest coder that fits a 64 GB Mac without swapping.

**System requirements:** 32 GB RAM minimum (will swap on smaller). 64 GB Mac Mini / MacBook Pro recommended.

**Best for:** Heavy agentic coding (multi-hour Claude Code sessions). Complex refactors. Pull-request review. When the 14B isn't smart enough.

**Not for:** Tight-RAM machines. Latency-sensitive autocomplete (TTFT is noticeably higher).

**Performance:**
- Mac M3 24 GB: don't (will swap)
- Mac M4 Pro 64 GB: ~12–20 tok/s
- Mac Studio M3 Ultra 96 GB: ~25–40 tok/s
- Linux + 2× RTX 5090: ~80+ tok/s

**Install:**
```bash
opod model add qwen-coder-32b
```

**Use it:**

```bash
# Claude Code — best target for this size model
export ANTHROPIC_BASE_URL=http://localhost:8080
export ANTHROPIC_AUTH_TOKEN=sk-orc-...
export ANTHROPIC_MODEL=qwen-coder-32b
claude
```

**Switch up when:** You need bigger than 32 GB of model. That means sharding — see `llama-3.3-70b-sharded`.

---

## `qwen3-8b` — general chat, balanced

**What it is:** Alibaba's Qwen3 8B Instruct (general-purpose, not code-tuned).

**System requirements:** 12 GB RAM. Mid-tier laptop comfort.

**Best for:** Mixed workloads where you need general chat + occasional code (Qwen3 isn't as strong on code as Qwen Coder, but it's a better generalist).

**Not for:** Pure code work — pick `qwen-coder-7b` instead at this RAM tier.

**Install:**
```bash
opod model add qwen3-8b
```

**Use it:**

```bash
# curl
curl :8080/v1/chat/completions \
  -H "Authorization: Bearer sk-orc-..." \
  -d '{"model":"qwen3-8b","messages":[{"role":"user","content":"…"}]}'
```

```bash
# Claude Code
export ANTHROPIC_MODEL=qwen3-8b
```

---

## `qwen3-14b` — general chat, capable

**What it is:** Alibaba's Qwen3 14B Instruct. Strong general-purpose chat — close to GPT-4-class on many benchmarks.

**System requirements:** 16 GB RAM.

**Best for:** Customer-facing chatbots, document Q&A, agent loops where the agent needs broad world knowledge (not just code).

**Not for:** Pure-code workflows (pick `qwen-coder-14b` for those — Qwen Coder is specifically tool-use-tuned).

**Install:**
```bash
opod model add qwen3-14b
```

**Use it:** same patterns as above with `model: qwen3-14b`.

---

## `deepseek-r1-8b` — reasoning ("thinking")

**What it is:** DeepSeek-R1-Distill-Qwen-8B. A reasoning model — produces a hidden `<thinking>` block before its answer (where it works through the problem step by step).

**System requirements:** 12 GB RAM.

**Best for:** Math problems, multi-step reasoning, careful code review where correctness matters more than speed, debugging complex logic.

**Not for:** Latency-sensitive autocomplete (every response includes a long internal monologue). Pure chat.

**Performance note:** R1 models are slower per-token because they "think" before answering — total time-to-final-answer is what matters, not raw tok/s.

**Install:**
```bash
opod model add deepseek-r1-8b
```

**Use it:**

```bash
# curl — note that the response will include `<think>...</think>` blocks
curl :8080/v1/chat/completions \
  -H "Authorization: Bearer sk-orc-..." \
  -d '{"model":"deepseek-r1-8b","messages":[{"role":"user","content":"if a train leaves Chicago at 3pm…"}]}'
```

```bash
# Claude Code
export ANTHROPIC_MODEL=deepseek-r1-8b
```

**Tip:** When a model emits `<think>` tags, most UIs (Claude Code, Cursor) show them as part of the response. If that's distracting, strip them client-side or use a non-reasoning model for that task.

---

## `llama-3.3-70b-sharded` — frontier, multi-machine

**What it is:** Meta Llama 3.3 70B Instruct, split across multiple machines via llama.cpp RPC. The frontier of what's runnable on consumer-grade hardware.

**System requirements:** ~48+ GB of *combined* memory across at least 2 machines. E.g. 2× Mac Mini 32 GB, or 4× Mac Mini 16 GB.

**Best for:** When you need GPT-4 / Sonnet-tier quality and don't want to pay per token. Frontier coding agents. Complex reasoning chains. Long-context document analysis.

**Not for:** Single-machine setups. Latency-sensitive use cases (network hops between shards add overhead). Use Claude Sonnet via fallback if you want this quality without the hardware.

**Prereqs (one-time):**
- `brew install llama.cpp` on the leader (provides `llama-server`)
- `rpc-server` on PATH on every worker — currently needs a source build of llama.cpp (the Homebrew bottle doesn't include it)
- Catalog entry with `sharding.required: true` and a local GGUF path (`catalog/llama-3.3-70b-sharded.yaml` already configured)
- Place the model GGUF at `/var/lib/opod/models/llama-3.3-70b-q4_k_m.gguf` (or update the YAML's `source.path`)
- At least 2 workers joined and `ready` (`opod node ls`)

**Install (orchestrates the whole thing):**

```bash
# splits across 2 workers automatically
opod shard create llama-3.3-70b-sharded 2

# what Opod does:
# 1. picks the 2 workers with the most free RAM
# 2. POSTs /v1/process/start to each → launches `rpc-server -p 50052`
# 3. waits for both rpc-servers to be TCP-reachable
# 4. on the leader, launches `llama-server -m <gguf> --rpc w1:50052,w2:50052 --port 9001`
# 5. persists shard rows + a placements row pointing at the local coordinator
# 6. Router routes any request for `llama-3.3-70b-sharded` to the coordinator
```

**Use it:**

```bash
# curl
curl :8080/v1/chat/completions \
  -H "Authorization: Bearer sk-orc-..." \
  -d '{"model":"llama-3.3-70b-sharded","messages":[{"role":"user","content":"…"}]}'
```

```bash
# Claude Code
export ANTHROPIC_MODEL=llama-3.3-70b-sharded
claude
```

**Tear down (cleanly):**

```bash
opod shard remove llama-3.3-70b-sharded
# stops the coordinator on the leader + every rpc-server on every worker
# removes shard rows + placements
```

**Switch up when:** You need bigger than 70B (DeepSeek-V3 671B MoE, Qwen3-Coder-480B) — those need more nodes and more RAM. v0.5 will support auto-scaling shard counts.

---

## `nomic-embed-text` — embeddings for RAG

**Why pick it.** It's the default embedding model in Opod's catalog — Apache-2.0 licensed, 137 M parameters, 768-dimensional vectors, 8K-token context window. Beats OpenAI `text-embedding-ada-002` on MTEB benchmarks. If you're wiring Opod into a RAG pipeline (LangChain, LlamaIndex, Haystack, raw vector DB), this is the one to start with.

**Picker fit.** Any machine — it needs ~270 MB on disk and 2 GB RAM. Runs alongside your chat model with no extra hardware budget.

**Install.**

```bash
opod model add nomic-embed-text
```

**Use via API (OpenAI embeddings shape).**

```bash
KEY="sk-orc-..."

curl http://localhost:8080/v1/embeddings \
  -H "Authorization: Bearer $KEY" \
  -H "Content-Type: application/json" \
  -d '{"model":"nomic-embed-text","input":"the quick brown fox"}'
```

Response (truncated):

```json
{
  "object": "list",
  "data": [{"object":"embedding","index":0,"embedding":[0.0214, -0.0179, ...768 floats...]}],
  "model": "nomic-embed-text",
  "usage": {"prompt_tokens": 5, "total_tokens": 5}
}
```

**Use via SDK.**

```python
from openai import OpenAI
client = OpenAI(base_url="http://localhost:8080/v1", api_key="sk-orc-...")
resp = client.embeddings.create(model="nomic-embed-text", input=["hello", "world"])
print(len(resp.data[0].embedding))  # → 768
```

Drop-in for any code that calls OpenAI `text-embedding-3-small` / `text-embedding-ada-002` — same request and response shape.

**Use via LangChain / LlamaIndex.** Point the `OpenAIEmbeddings` wrapper at `http://localhost:8080/v1` with your `sk-orc-*` key and `model="nomic-embed-text"`. No other code changes.

**What it doesn't do.** This is an embedding-only model. Don't try `/v1/chat/completions` with it — it won't respond. Pair it with a chat model (Qwen, Llama, Gemma) for the generation half of your RAG stack.

---

## Use any other Ollama model (catalog pass-through)

Opod's catalog is curated for UX, but the router will pass through any model name the engine doesn't recognize. Steps:

```bash
# 1. pull via Ollama directly (Opod's catalog is bypassed)
ollama pull mistral-nemo:12b      # or any model from https://ollama.com/library

# 2. use the engine-native name in your API request
curl :8080/v1/chat/completions \
  -H "Authorization: Bearer sk-orc-..." \
  -d '{"model":"mistral-nemo:12b","messages":[…]}'
```

Or add a custom catalog entry (so it shows up in `opod model search` + UI): drop a YAML file in `catalog/` matching the schema of the existing entries:

```yaml
# catalog/my-model.yaml
id: my-model
display_name: My Custom Model
source:
  type: ollama
  ollama_name: my-model:7b
size_bytes: 4000000000
quant: q4_k_m
context_window: 32768
capabilities: [chat]
recommended_engines: [ollama]
hardware:
  min_ram_gb: 8
tags: [chat]
```

Restart `opod up` — your model now appears in `opod model search` and the web UI's catalog picker.

---

## Switching models on the fly

For Claude Code:

```bash
# different sessions can use different models:
ANTHROPIC_MODEL=llama-3.2-1b claude       # smoke test session
ANTHROPIC_MODEL=qwen-coder-14b claude     # real work session
ANTHROPIC_MODEL=claude-opus-4-7 claude    # falls back to real Anthropic (if fallback configured)
```

For curl / SDKs: change the `model` field in each request body. The same Opod instance serves all of them.

For Cursor / Continue: change the model id in the UI / config file and reload.

---

## Troubleshooting per-model

| Symptom | Likely model | Fix |
|---|---|---|
| Slow first response | Any (cold model load) | First chat after `opod up` loads the model into RAM — subsequent ones reuse it |
| `out of memory` mid-stream | Too big for RAM | Pick a smaller model OR close other apps OR add a worker and offload |
| Constant `<think>` tags in output | `deepseek-r1-*` | That's normal — it's a reasoning model. Use a non-reasoning model if you don't want them |
| `model not found` | Typo in catalog id | `opod model ls` to see what's actually installed; `opod model search` to find the right id |
| Sharded model 404 | Coordinator failed | `opod shard ls` to check status; `opod shard remove …` then `opod shard create …` to redo |

---

## Roadmap (planned models in upcoming versions)

These have docs and references in the repo but no catalog YAMLs yet. Coming with v0.5+:

- **Llama 3.3 70B** (non-sharded, for boxes with 48+ GB single-machine)
- **DeepSeek V3 / R1 671B MoE** (frontier, sharded across many nodes)
- **Qwen3-72B Instruct** + **Qwen3-Coder-30B-A3B**
- **GLM-4.6**, **Kimi K2**
- **Vision models** (Qwen2.5-VL, Llama 3.2 Vision)
- **Embeddings** (BGE-M3) + **Whisper** (transcription)

If you have a specific model you want in the curated catalog: open an issue at https://github.com/opod-io/opod/issues with the Ollama tag / HF repo + your hardware target, and we'll add a YAML.
