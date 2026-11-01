# Opod — Quick Start

The fastest path from zero to your first local chat completion. **3 minutes** on a fresh Mac or Linux machine.

> For full docs see [README.md](README.md). For design see [ARCHITECTURE.md](ARCHITECTURE.md).

---

## 🤔 First — how many machines?

| Your situation | Use | Engine |
|---|---|---|
| **Try it** on one laptop or one box | **1 machine** — everything below | llama.cpp (Mac, AMD, Intel, CPU) or vLLM (NVIDIA) |
| **One box with two or more NVIDIA cards** | 1 machine; the engine splits the model across the cards | vLLM or SGLang, tensor parallel |
| **A model bigger than any one card** — two 24 GB PCs as one 48 GB model, or NVIDIA + AMD + Intel together | 2+ machines as one **gang** (`opod shard create`) — see [Pool your GPUs](#-pool-your-gpus-one-model-across-machines) | llama.cpp RPC (any vendor) · vLLM + Ray (NVIDIA) |
| **More throughput** or **different models on different machines** | 2+ machines — leader + workers, each serving whole models | any |

Every setup installs the same way — only the *commands you run after installing* differ. This page starts on one machine with llama.cpp (Mac) or vLLM (Linux + NVIDIA): both serve tool calls, and the same llama.cpp build joins a gang later. Ollama also works as an engine, but it serves one model whole on one machine and Opod refuses tool calls on it — see [docs/ENGINES.md](docs/ENGINES.md#ollama).

### 🖼️ Single-machine setup (what you're building below)

```
              Your computer  (Mac or Linux)
   ┌─────────────────────────────────────────────────┐
   │                                                 │
   │   ┌───────────┐  ┌────────────┐  ┌───────────┐  │
   │   │  Cursor   │  │  Continue  │  │   curl    │  │
   │   │  Aider    │  │    Zed     │  │   SDKs    │  │
   │   └─────┬─────┘  └─────┬──────┘  └─────┬─────┘  │
   │         └──────────────┼───────────────┘        │
   │                        │                        │
   │                        ▼                        │
   │           ┌──────────────────────────┐          │
   │           │      OPOD  :8080         │          │
   │           │   OpenAI-compatible API  │          │
   │           │   auth · API keys        │          │
   │           │   usage + event streams  │          │
   │           └────────────┬─────────────┘          │
   │                        │ (localhost HTTP)       │
   │                        ▼                        │
   │           ┌──────────────────────────┐          │
   │           │   llama-server  :8089    │          │
   │           │   (the engine; Opod      │          │
   │           │    starts it for you)    │          │
   │           └──────────────────────────┘          │
   │                                                 │
   └─────────────────────────────────────────────────┘
```

---

## 🐣 Step 0 — what you need

- **A computer**: Mac (Apple Silicon) or Linux (x86_64 / arm64)
- **8 GB+ RAM** (more is better; the model has to fit in memory)
- **10 GB free disk** (for the model)
- **Internet** (one-time, to download the binary + the model)

---

## 🍎 Mac (Apple Silicon) — llama.cpp

```bash
# 1. install Opod
curl -fsSL https://raw.githubusercontent.com/opod-io/opod-core/main/installer/install.sh | sh

# 2. install the engine
brew install llama.cpp

# 3. start Opod; it launches llama-server and downloads a 2 GB model on the first run (a few minutes)
OPOD_ENGINE=llamacpp OPOD_DEFAULT_MODEL=llama-3.2-3b-gguf OPOD_PULL_DEFAULT_MODEL=1 opod up
```

> `OPOD_PULL_DEFAULT_MODEL=1` (or `router.pull_default_model: true` in config) is what makes this machine download and serve a model itself. It is off by default: a plain `opod up` is a router-only leader that serves models from the workers that join it.
>
> First-run behavior: with the pull on, on an empty install `opod up` shows a one-line prompt asking whether to pull the recommended starter for your hardware (press enter to accept, `o` to pick another, `n` to skip). Add `--no-wizard` for a quiet boot, or `--auto-pull=false` to skip the pull entirely.

## 🐧 Linux + NVIDIA — vLLM

The leader and a worker on the same box, in two terminals. The worker starts `vllm serve` itself and uses every GPU it finds.

```bash
# 1. install Opod and the engine
curl -fsSL https://raw.githubusercontent.com/opod-io/opod-core/main/installer/install.sh | sh
pip install vllm

# 2. terminal 1 — the leader (its "engine not reachable" warning is expected: the worker serves)
opod up

# 3. terminal 2 — a worker on this box's GPUs (a 24 GB card or more for this model)
opod token create --node
OPOD_ENGINE=vllm OPOD_LOAD_MODEL=llama-3.1-8b opod join "http://127.0.0.1:8080?token=<token>"
```

## 🐧 Linux with AMD, Intel or no GPU — llama.cpp

```bash
git clone https://github.com/ggml-org/llama.cpp && cd llama.cpp
cmake -B build -DGGML_RPC=ON -DGGML_HIP=ON      # AMD · Intel: -DGGML_SYCL=ON · NVIDIA: -DGGML_CUDA=ON · CPU: no flag
cmake --build build --config Release -j && sudo cp build/bin/llama-server build/bin/rpc-server /usr/local/bin/
```

Then step 3 of the Mac block. SGLang, TensorRT-LLM, MLX-LM, Ollama and the container images: [docs/ENGINES.md](docs/ENGINES.md).

---

## ✅ What you should see

After step 3 of the Mac block, Opod prints (measured on an M3, 2026-10-31):

```
▶ detected darwin/arm64 · 24 GB RAM · 8 cores
✔ default model: llama-3.2-3b-gguf
▶ auto-spawning llama-server for llama-3.2-3b-gguf on :8089 ...
✔ llama-server ready (auto-spawned)
✔ engine: llamacpp at http://127.0.0.1:8089
✔ model ready: llama-3.2-3b-gguf

  Opod is ready.

  API:        http://localhost:8080/v1
  Health:     http://localhost:8080/healthz

  Admin API key (shown once — store it now):
    sk-orc-<admin key — printed once, keep it>

  Next steps:
    →  Wire up Aider:           opod connect aider
    →  Wire up Cursor:          opod connect cursor
    →  See all clients:         opod connect --list
    →  Mint another admin key:  opod token create ops --admin

  Quick test from the shell:
    curl http://localhost:8080/v1/chat/completions \
      …

  Network behavior on this node:
    …

  Press Ctrl-C to stop.
```

**Copy that admin key now.** It is also saved to `~/.opod/admin.key` — that is where `opod connect` reads it from, and a later `opod up` re-prints it from there. The database keeps only a hash, so if that file is gone the key cannot be recovered: mint another with `opod token create ops --admin`.

---

## 💬 Test it (pick one)

### A) Fastest — `opod connect <tool>`

Prints copy-paste config for any of 15 supported tools, with your URL + token already substituted:

```bash
opod connect cursor          # IDE settings
opod connect aider           # CLI flags
opod connect codex-cli       # OPENAI_BASE_URL / OPENAI_API_KEY env vars
opod connect                 # no arg → interactive picker
opod connect --list          # see all 15
```

The roster: `cursor`, `aider`, `continue`, `zed`, `cline`, `openclaw`, `opencode`, `open-webui`, `open-notebook`, `goose`, `plandex`, `openhands`, `codex-cli`, `openai-sdk`, `curl`. A tool that speaks the Anthropic Messages shape — Claude Code is one — is not on the roster, but it needs no snippet: point it at the same base URL. The leader serves `POST /v1/messages` on every chat model and translates it to the chat path (ADR-092 §1).

### B) The console

Core has no web dashboard (ADR-022). The console — Connect cards, Playground, keys, teams — is the control plane (`opodcp`, `helm install opod`). From core alone, use the CLI (`opod connect`, `opod token`) or curl.

### C) curl from your terminal

```bash
KEY="sk-orc-<admin key — printed once, keep it>…"   # paste your key

curl http://localhost:8080/v1/chat/completions \
  -H "Authorization: Bearer $KEY" \
  -H "Content-Type: application/json" \
  -d '{"model":"auto","messages":[{"role":"user","content":"say hi in 5 words"}]}'
```

You'll see JSON like:

```json
{"choices":[{"message":{"role":"assistant","content":"Hello! How can I help?"}}]}
```

### D) Manual — any OpenAI-shape tool (if you can't run `opod connect`)

```bash
export OPENAI_BASE_URL=http://localhost:8080/v1
export OPENAI_API_KEY=sk-orc-<admin key — printed once, keep it>
codex --model llama-3.2-3b-gguf "say hi"      # OpenAI's Codex CLI; the OpenAI SDKs read the same two variables

# Aider takes flags instead:
aider --openai-api-base http://localhost:8080/v1 \
      --openai-api-key  sk-orc-<admin key — printed once, keep it> \
      --model openai/llama-3.2-3b-gguf
```

The tool now talks to your local Llama 3B instead of `api.openai.com`. Name a model you have installed (`opod model ls`), or `auto` for the default: a vendor model id such as `gpt-4o` is not something core can serve, and it does not proxy to a vendor.

### E) Vision (image input)

If you've installed a vision-capable model (e.g. `opod model add gemma4-12b`, `gemma4-26b`, `llama-4-scout`, or any `qwen3-vl-*`), you can send images on the same `/v1/chat/completions` endpoint:

```bash
curl http://localhost:8080/v1/chat/completions \
  -H "Authorization: Bearer $KEY" \
  -H "Content-Type: application/json" \
  -d '{
    "model": "gemma4-12b",
    "messages": [{
      "role": "user",
      "content": [
        {"type": "text", "text": "what is in this picture?"},
        {"type": "image_url", "image_url": {"url": "data:image/png;base64,iVBORw0KGgoAA..."}}
      ]
    }]
  }'
```

Images reach an Ollama or vLLM engine; the engine driver pulls the image bytes from data URLs or http(s) URLs. An engine not proven to take images (SGLang, llama.cpp, MLX-LM, TensorRT-LLM) is refused `400` naming `image_url`. Anthropic Messages clients send base64 image blocks to `/v1/messages`.

### F) Embeddings

```bash
opod model add nomic-embed-text       # one-time install of the default embedding model

curl http://localhost:8080/v1/embeddings \
  -H "Authorization: Bearer $KEY" \
  -H "Content-Type: application/json" \
  -d '{"model":"nomic-embed-text","input":"hello world"}'
```

You'll get back an OpenAI-shape `{"data":[{"embedding":[…768 floats…]}]}` response. Use it with any RAG library that talks OpenAI embeddings.

The second pass of a retrieval stack is `/v1/rerank`, served by a reranker model on vLLM, SGLang, or llama.cpp started with `--reranking` (Ollama has no rerank route and answers `501 rerank_not_supported`):

```bash
curl http://localhost:8080/v1/rerank \
  -H "Authorization: Bearer $KEY" \
  -H "Content-Type: application/json" \
  -d '{"model":"<your reranker>","query":"what is a gang?","documents":["one model split across GPUs","a group of people"],"top_n":1}'
```

The answer lists `results` most relevant first, each with its `index`, `relevance_score` and `document.text`.

---

## 👥 Share with your team

Once you've confirmed it works:

```bash
opod token create hadi
```

This creates a user-scope key for `hadi` and prints it **once**. Give it an expiry at creation if you like:

```bash
opod token create hadi --ttl 30d                           # an expiry (or --expires-at 2026-12-31)
opod token ls                                              # ids, names, scopes, expiry
opod token revoke <id>                                     # when they leave
```

A key is an identity — who is calling, with which scope, until when. Per-key model allowlists, per-minute ceilings and daily quotas are not core's: they belong to the application layer in front of the gateway. Then print the snippet for the tool your teammate uses, with their key and the address they will reach you on, and send them that:

```bash
opod connect cursor --token <their key> --base-url http://<your-host>:8080
```

---

## 🆘 If it doesn't work

Run the doctor first — it tells you what's wrong in plain English:

```bash
opod doctor
```

Most common failures:

| You see | Fix |
|---|---|
| `command not found: opod` | The install dir isn't on your PATH. Run: `export PATH="$HOME/.local/bin:$PATH"` (and add it to `~/.zshrc` or `~/.bashrc` to make it permanent) |
| `engine (ollama) at http://127.0.0.1:11434 is not reachable` | Start Ollama: `ollama serve &` (Linux: `sudo systemctl start ollama`) |
| `502 Bad Gateway` with `llama-server binary not found` | The Homebrew `ollama` formula on Apple Silicon is broken. Fix: `brew uninstall ollama && brew install --cask ollama` |
| `address already in use` on `:8080` (`opod doctor`: `listen port :8080 already in use`) | Another process is on it. Use a different port: `OPOD_LISTEN=:8090 opod up` (avoid `:8081` — that's the default worker port) |
| `no admin key on disk` (running CLI) | `opod up` isn't running on this host. Start it first, then re-run the CLI command |

More fixes in the [main README's troubleshooting table](README.md#troubleshooting-installation).

---

## 🌐 Add a second (or third…) machine

Same install command everywhere. The first machine becomes the **leader**, every other machine becomes a **worker**. A second machine does one of two jobs:

- **Serve more, or serve different models** — each worker serves whole models. This section, with any engine (vLLM is shown).
- **Pool its GPU with the others into one model** — a gang. Skip to [Pool your GPUs](#-pool-your-gpus-one-model-across-machines).

### 🖼️ Two-machine setup (what you're about to build)

```
                MACHINE A  (leader)                            MACHINE B  (worker)
   ┌──────────────────────────────────────┐       ┌──────────────────────────────────────┐
   │                                      │       │                                      │
   │  Your tools ──► Opod :8080          │       │      Opod agent :8081               │
   │                 ┌───────┐            │       │      (worker HTTP server,            │
   │                 │ Router│ ───────────┼───────┼────► proxies requests to its         │
   │                 └───────┘            │       │       engine, HMAC-signed)           │
   │                                      │       │                  │                   │
   │  Admin API :8080/admin/v1            │       │                  ▼                   │
   │  CLI: opod node ls / model ls       │       │      vLLM :8000 (the worker starts   │
   │                                      │       │      it; does the model serving)     │
   │  (router only: the leader needs      │       │                                      │
   │   no GPU and no engine)              │       │  ◄── heartbeat every 5s ──┐          │
   │                                      │       │      (carries loaded_models)         │
   │                                      │       │                           │          │
   └──────────────────────────────────────┘       └───────────────────────────┼──────────┘
                  ▲                                                           │
                  │                                                           │
                  └────────── plain HTTP over your LAN (or your own VPN) ─────┘
```

### 🖼️ Step-by-step (what happens when)

```
   LEADER (Machine A)                              WORKER (Machine B)
   ──────────────────                              ─────────────────

   1. install Opod              (the leader needs no engine)
   2. opod up
      ✔ admin key shown
      ✔ listening :8080

   3. opod token create --node
      ✔ sk-orc-NodeJoin-AbCd1234…
              ─────► copy this token to the worker machine

                                                  1. install Opod + vLLM
                                                  2. OPOD_ENGINE=vllm \
                                                     OPOD_LOAD_MODEL=llama-3.1-8b \
                                                     opod join "http://leader:8080?token=..."
                                                     ✔ registered with leader

                                ◄──── heartbeat every 5s ────

   4. opod node ls
      ID         HOSTNAME    STATE
      local      machine-a   ready
      n_abc123   machine-b   ready

                                                  3. the worker starts vllm serve
                                                     and reports llama-3.1-8b

   5. curl :8080/v1/chat/completions
      with model=llama-3.1-8b
            ────► router sees worker has this model ────►
                                                     (worker serves it)
            ◄──────────── response streamed back ─────
```

### Step 1 — on the leader

```bash
opod token create --node
# prints something like:
#   sk-orc-NodeJoin-ABcD1234…
```

Note the leader's reachable address. Joining is plain HTTP and Opod has no built-in mesh: the worker must reach `http://<leader>:8080`, and the leader must reach the worker back on `:8081`. On a LAN that is the leader's LAN IP (e.g. `192.0.2.42`). Over a VPN you run yourself, use the VPN address — and if the worker's default-route address is not the one the leader can dial, set `OPOD_ADVERTISE_ADDR=<ip>:8081` on the worker.

### Step 2 — on the new machine

Install Opod and the engine **the same way as above**, then instead of `opod up`, join with the engine and the model this machine serves:

```bash
OPOD_ENGINE=vllm OPOD_LOAD_MODEL=llama-3.1-8b opod join "http://192.0.2.42:8080?token=sk-orc-NodeJoin-ABcD1234…"
# llama.cpp instead: OPOD_ENGINE=llamacpp OPOD_LOAD_MODEL=llama-3.2-3b-gguf opod join "…"
```

(Substitute the leader's address and the token you copied. Keep the quotes: `?` is a glob character in zsh.)

### Step 3 — or place a model from the leader

A worker joined with no `OPOD_LOAD_MODEL` waits for one. Place it from the leader:

```bash
opod model add llama-3.1-8b --node n_abc123
```

### Step 4 — verify on the leader

```bash
opod node ls
# example output:
# ID         HOSTNAME      OS/ARCH       ADDRESS              STATE   LAST HB
# local      machine-a     darwin/arm64  127.0.0.1:8080       ready   2026-06-05T…
# n_abc123   machine-b     darwin/arm64  192.0.2.50:8081    ready   2026-06-05T…
```

Any request the gateway gets for `llama-3.1-8b` is now routed automatically to the worker. If you install the **same** model on two workers, the leader load-balances between them.

> ⚠️  Only do this on a network you trust (your LAN, or a VPN you run) — see [Security model](#-security-model-read-before-exposing-it) below.

---

## 🧩 Pool your GPUs: one model across machines

A model that fits on no single card runs as a **gang**: one model split into parts, one part per GPU, behind the leader's one URL. The cards may come from different vendors. Ollama cannot do this; llama.cpp can, on any vendor.

```
   LEADER (any machine, no GPU needed)
   opod up · opod token create --node
              │
     ┌────────┴─────────┐   `opod join` on each
     ▼                  ▼
   PC 1                PC 2
   NVIDIA 24 GB        AMD 24 GB
   rpc-server          rpc-server        ◄── the leader starts these for you
     └──── one 70B model at 4-bit (~43 GB), split by layers ────┘
```

### Step 1 — on every GPU machine: llama.cpp with RPC

Install llama.cpp so that `rpc-server` and `llama-server` are on PATH. The Homebrew bottle has no RPC backend, so build it from source:

```bash
git clone https://github.com/ggml-org/llama.cpp && cd llama.cpp
cmake -B build -DGGML_RPC=ON -DGGML_CUDA=ON      # AMD: -DGGML_HIP=ON · Intel: -DGGML_SYCL=ON · Mac: Metal is on by default
cmake --build build --config Release -j
sudo cp build/bin/rpc-server build/bin/llama-server /usr/local/bin/
```

Then join each machine to the leader as in [Step 2 above](#step-2--on-the-new-machine):

```bash
opod join "http://<leader-host>:8080?token=<token>"
```

### Step 2 — on the leader: split a model

```bash
opod node ls                                 # every GPU machine is ready
opod shard create llama-3.2-3b-sharded 2     # small smoke test: the leader downloads it and fans it out
opod shard ls                                # parts + the coordinator that serves the gang
```

What happens: the leader starts an `rpc-server` on each worker, starts `llama-server` (the coordinator) on the worker with the most GPU memory, and routes the model's requests to it. A big model works the same way: `opod model info llama-3.3-70b-sharded` names the GGUF file the leader must hold, and `opod shard create llama-3.3-70b-sharded` with no count picks the fewest parts that fit.

### Step 3 — use it

```bash
curl http://<leader-host>:8080/v1/chat/completions \
  -H "Authorization: Bearer $KEY" -H "Content-Type: application/json" \
  -d '{"model":"llama-3.2-3b-sharded","messages":[{"role":"user","content":"hi"}]}'
```

**Two NVIDIA cards in one box** need no gang: vLLM splits the model across both cards itself. Start `vllm serve <repo> --tensor-parallel-size 2 --port 8000`, then join with `OPOD_ENGINE=vllm OPOD_VLLM_ENDPOINT=http://127.0.0.1:8000 opod join "…"`.

**What a split costs:** every token crosses the network between the parts. A gang on plain ethernet is slower than one big card; it runs a model you could not run at all. Keep the machines on one fast LAN.

Every flag, vLLM + Ray gangs across NVIDIA machines, several gangs of one model, and what is proven per engine: [README → Pool your GPUs](README.md#-pool-your-gpus-one-model-across-machines) and [Sharded models](README.md#sharded-models-split-one-brain-across-multiple-machines).

---

## 🤖 Use a different model (Qwen, Llama, DeepSeek…)

The starter `llama-3.2-3b-gguf` is small — good for "does this work?" but underpowered for real work. Opod ships a curated **catalog** of better models.

### Browse what's in the catalog

```bash
opod model search           # list everything
opod model search coder     # filter
```

A summary of 41 of the 48 catalog entries — `opod model search` (or `opod catalog ls`) is the live list. ⭐ marks the current top picks. The seven not listed are the reranker `bge-reranker-v2-m3-gguf` (for `/v1/rerank`, not chat) and six variants and test fixtures rather than different models: `llama-3.1-8b`, `llama-3.2-3b-gguf`, `llama-3.2-3b-sharded`, `nomic-embed-text-v1.5-gguf`, `qwen2.5-0.5b-gguf` and `mimo-7b-ray` (the one entry that uses the vLLM + Ray gang backend, labelled TEST).

| Catalog id | What it's for | RAM | Engine name |
|---|---|---|---|
| `llama-3.2-1b` | smoke test | 2 GB | `ollama:llama3.2:1b` |
| `nomic-embed-text` | embeddings for RAG (768-dim, 8K ctx) | 2 GB | `ollama:nomic-embed-text` |
| `llama-3.2-3b` ⭐ | small fast chat | 4 GB | `ollama:llama3.2:3b` |
| `moondream3` | tiny vision-language (Raspberry Pi) | 4 GB | `hf: moondream/moondream3-preview` (vLLM/MLX) |
| `qwen-coder-7b` | code completion + chat | 8 GB | `ollama:qwen2.5-coder:7b` |
| `mimo-7b` | reasoning-focused 7B | 8 GB | `hf: XiaomiMiMo/MiMo-7B-RL` (vLLM/MLX) |
| `mimo-vl-7b` | small vision-language | 8 GB | `hf: XiaomiMiMo/MiMo-VL-7B-RL` (vLLM/MLX) |
| `mimo-audio` | speech + audio understanding | 8 GB | `hf: XiaomiMiMo/MiMo-Audio-7B-Instruct` (vLLM/MLX) |
| `deepseek-r1-8b` | reasoning ("thinking") | 12 GB | `ollama:deepseek-r1:8b` |
| `lfm2.5-8b-a1b` ⭐ | best on-device edge MoE | 8 GB | `hf: LiquidAI/LFM2.5-8B-A1B` (llama.cpp/MLX/vLLM) |
| `qwen3-8b` | general chat, balanced | 12 GB | `ollama:qwen3:8b` |
| `glm-4-9b` | dense chat, 128K ctx, tool-calling | 12 GB | `ollama:glm4:9b` |
| `qwen3-vl-8b` | vision + tools (charts, OCR, UI) | 10 GB | `ollama:qwen3-vl:8b` |
| `gemma4-e2b` | mobile/edge multimodal (text+image+audio) | 8 GB | `ollama:gemma4:e2b` |
| `gemma4-12b` | encoder-free multimodal | 12 GB | `ollama:gemma4:12b` |
| `gemma4-e4b` | mobile/edge multimodal | 12 GB | `ollama:gemma4:e4b` |
| `pixtral-12b` | Mistral vision-language | 16 GB | `hf: mistralai/Pixtral-12B-2409` (vLLM/MLX) |
| `mellum2-12b` | JetBrains MoE coder (2.5B active) | 12 GB | `hf: JetBrains/Mellum-2-12B-A2.5B-Thinking` (llama.cpp/MLX/vLLM) |
| `mistral-nemo-12b` | 128K context chat | 12 GB | `ollama:mistral-nemo:12b` |
| `qwen-coder-14b` | better code + agent | 16 GB | `ollama:qwen2.5-coder:14b` |
| `qwen3-14b` | general chat, more capable | 16 GB | `ollama:qwen3:14b` |
| `phi-4-14b` | strong reasoning per byte | 12 GB | `ollama:phi4:14b` |
| `gpt-oss-20b` ⭐ | OpenAI open-weight reasoning | 16 GB | `ollama:gpt-oss:20b` |
| `qwen3.6-27b` ⭐ | top consumer pick (77% SWE-bench) | 24 GB | `ollama:qwen3.6:27b` |
| `gemma4-26b` | MoE 4B-active, multimodal | 24 GB | `ollama:gemma4:26b` |
| `qwen3-30b` | MoE 3B-active, fast | 24 GB | `ollama:qwen3:30b` |
| `qwen3-coder-30b` | MoE coder, 3.3B active | 24 GB | `ollama:qwen3-coder:30b` |
| `qwen3-vl-32b` | frontier-tier vision-language | 32 GB | `ollama:qwen3-vl:32b` |
| `qwen-coder-32b` | strong code agent (laptop max) | 32 GB | `ollama:qwen2.5-coder:32b` |
| `gemma4-31b` | multimodal vision-language | 32 GB | `ollama:gemma4:31b` |
| `llama-3.3-70b-sharded` | sharded across machines | 48+ GB total | sharded llama.cpp |
| `gpt-oss-120b` | OpenAI open-weight, single H100 | 80 GB | `ollama:gpt-oss:120b` |
| `llama-4-scout` | 10M context, multimodal | 80 GB | `ollama:llama4:scout` |
| `glm-4.5-air-sharded` | 106B MoE/12B active, agentic | 80 GB total | sharded llama.cpp |
| `step-3.7-flash-sharded` ⭐ | fastest frontier MoE VLM | 128 GB total | sharded llama.cpp |
| `deepseek-v4-flash-sharded` ⭐ | 13B active, frontier reasoning | 160 GB total | sharded llama.cpp |
| `glm-4.6-sharded` | 357B MoE/32B active, agentic coder | 224 GB total | sharded llama.cpp |
| `nemotron-3-ultra-sharded` | Mamba-MoE, 1M ctx, MMLU 89.1 | 320 GB total | sharded llama.cpp |
| `glm-5.1-sharded` | best agentic coder | 416 GB total | sharded llama.cpp |
| `glm-5.2-sharded` | successor to 5.1, 1M context | 480 GB total | sharded llama.cpp |
| `kimi-k2.6-sharded` | #1 open coding benchmarks | 512 GB total | sharded llama.cpp |

### Install + use a specific model

```bash
# 1. install the model
opod model add qwen-coder-14b
# Opod asks Ollama to pull qwen2.5-coder:14b and registers it.

# 2. use it by its catalog id in API requests
curl http://localhost:8080/v1/chat/completions \
  -H "Authorization: Bearer sk-orc-..." \
  -d '{"model":"qwen-coder-14b","messages":[{"role":"user","content":"explain monads"}]}'

# 3. or from a tool — the snippet with that model already filled in
opod connect aider --model qwen-coder-14b
```

> 📖 **Full step-by-step per-model guide:** [MODELS.md](MODELS.md) — for *every* model in the catalog: system requirements, performance expectations on Mac/Linux, install + use snippets, when to switch up.

### Switching models without running out of RAM

Installed models sit on disk; they only occupy RAM once used. When you switch
between big models on one machine, let Opod manage the memory:

```bash
opod model ps                          # what's in RAM right now + free budget
opod model load qwen3.6-27b --swap     # release the least-recently-used model, then load
opod model load nomic-embed-text --pin # keep the embedding model resident forever
opod model unload qwen-coder-14b       # free its RAM (weights stay on disk)
```

`load` refuses rather than overcommit your machine — `--swap` is the explicit
"yes, evict the old one first" (in-flight requests finish before anything is
unloaded). Pinned and loaded models come back automatically on the next
`opod up`. And `opod down` releases all engine memory by default — pass
`--no-unload` if you want models kept warm.

### Use ANY model Ollama supports (not just the catalog)

The catalog is curated for UX, but Opod will pass through any Ollama model name as-is. Steps:

```bash
# pull directly via Ollama (Opod's catalog is bypassed):
ollama pull mistral-nemo:12b

# then in your API request, use the engine-native name:
curl :8080/v1/chat/completions -H "Authorization: Bearer sk-orc-..." \
  -d '{"model":"mistral-nemo:12b","messages":[…]}'
```

This works because Opod's router falls through to the engine when no catalog entry matches the requested model id.

### Use a different engine entirely (vLLM, SGLang, MLX-LM, llama.cpp)

Edit `~/.opod/config.yaml`:

```yaml
engine:
  preferred: vllm                       # was: ollama
  vllm_endpoint: http://gpu-host:8000   # where your vLLM is running
```

Or via env: `OPOD_ENGINE=vllm OPOD_VLLM_ENDPOINT=http://gpu:8000 opod up`. The five engine names are
`ollama`, `vllm`, `sglang`, `mlx` and `llamacpp` (each driver's aliases work too — `sgl`, `tt`, `mlx-lm`,
`llama-cpp`), and each has its own endpoint setting (`engine.sglang_endpoint` defaults to
`http://127.0.0.1:30000`). The router doesn't care which engine serves the request — it just routes by model
name.

**Want bare-metal speed on weak hardware?** Use `llama.cpp` directly — lower RAM and cold-start latency than Ollama, and Opod auto-launches `llama-server` for you so it's still one command:

```bash
# 1. install llama.cpp (provides llama-server)
brew install llama.cpp     # macOS · apt: see https://github.com/ggml-org/llama.cpp
# (rpc-server is not in the Homebrew bottle — only needed for sharded
#  models; build from source with `cmake -DGGML_RPC=ON` if you need it)

# 2. that's it — Opod spawns llama-server itself
OPOD_ENGINE=llamacpp OPOD_PULL_DEFAULT_MODEL=1 OPOD_DEFAULT_MODEL=llama-3.2-3b-gguf opod up   # a catalog entry with a GGUF repo; an ollama-only entry has none
```

With `OPOD_PULL_DEFAULT_MODEL=1`, Opod looks up the catalog entry for the default model, reads its `source.repo` (a GGUF HuggingFace repo), and runs `llama-server -hf <repo> --port 8089` via its internal supervisor — the same one that already manages `rpc-server` for sharded models. When Opod stops, the spawned `llama-server` stops too.

If you'd rather manage `llama-server` yourself (e.g. to pass custom flags like `-ngl 999`), start it before `opod up` and Opod will detect it and skip the auto-spawn:

```bash
llama-server -m ~/models/qwen2.5-coder-7b-q4_k_m.gguf --port 8089 --n-gpu-layers 999
OPOD_ENGINE=llamacpp opod up
```

Opod's default `llamacpp_endpoint` is `http://127.0.0.1:8089` — chosen to avoid both `:8080` (Opod leader) and `:8081` (worker agent). The same `llamacpp` engine name also covers RPC sharding — `opod shard create` launches a `llama-server --rpc …` coordinator that this driver talks to.

---

## 🔌 Switch a tool back to its vendor

`opod connect` only ever printed settings for you to paste; nothing on the Opod host needs to change. The fastest way back:

```bash
opod disconnect codex-cli      # or: aider, cursor, continue, … — `opod disconnect --list`
```

It prints the exact `unset` commands (or the GUI steps, for an editor) for that client and modifies nothing itself. For the env-var tools it comes down to:

```bash
unset OPENAI_BASE_URL          # Aider's variable is OPENAI_API_BASE
export OPENAI_API_KEY=sk-…     # your own vendor key again
```

Or just **open a fresh terminal** that never had the Opod variables exported. If you had added the `export` lines to `~/.zshrc` or `~/.bashrc`, remove them there and `source` the file.

Core has no hybrid mode: it serves the models on your hardware and never forwards a request to a cloud vendor (ADR-022). To use both, keep two configurations in the tool — one pointing at Opod, one at the vendor.

---

## ⬆️ Updating to a new version

When a new release is published on GitHub, update in one command:

```bash
opod update
```

This checks the latest release, downloads the right binary for your platform, verifies the SHA-256 against the published `checksums.txt`, and replaces the binary in place. After it finishes:

```bash
opod down
opod up
```

Other options:

```bash
opod update --check              # see if there's a new version, don't install
opod update --version v0.1.0     # pin a specific version (see github.com/opod-io/opod-core/releases)
opod update --force              # reinstall even if already on the latest
opod upgrade                     # alias of `update`
```

If your binary lives in `/usr/local/bin/` (installed with sudo), `opod update` stages the new binary next to it and prints the exact `sudo mv` command to finish.

### 🔕 No automatic update check

`opod up` never contacts GitHub. A release is looked up only when you run `opod update` (or `opod update --check`) yourself — there is nothing to switch off in an offline or private environment.

---

## 🎯 Next steps

- **See the full CLI + API reference, configuration, troubleshooting**: [README.md](README.md)
- **Understand the architecture**: [ARCHITECTURE.md](ARCHITECTURE.md)
- **Per-command help**: `opod <cmd> --help` for any command
- **Add more workers**: see [Add a second machine](#-add-a-second-or-third-machine) above

---

## 🔒 Security model (read before exposing it)

Opod assumes a **trusted network** — your LAN, or a VPN you run yourself ([Tailscale](https://tailscale.com/), WireGuard…). It ships no mesh of its own. Specifically:

- **User API keys** (admin / user scope) are **sha256-hashed** in the database. The plaintext shown at creation time is the only way to use the key.
- **Worker tokens** (the shared secret between leader and worker) are stored on the `nodes.worker_token` column. Control-plane traffic uses **HMAC-SHA256 signatures** so the token itself isn't transmitted on the wire after the initial join — the agent and leader both sign with the per-node token. The SQLite file still holds the secret, so a stolen DB still lets an attacker impersonate a worker; encrypt the DB at rest if you can't trust the host. Set `OPOD_REJECT_BEARER=1` on workers to refuse the bearer-fallback path entirely (HMAC-only).
- **Worker HTTP servers** bind to the machine's LAN address (the one on its default route), or to `OPOD_ADVERTISE_ADDR` when you set it — and to all interfaces only if no address can be determined. Every worker route except `/healthz` is authenticated. Network reachability is still the first line of defense.
- There is **no web UI** in core and `/` answers 404; every caller — CLI, tool, manager — authenticates with an API key.

If you're not on a trusted LAN, run the cluster **over a VPN or zero-trust overlay you manage**. Give the leader a certificate with `OPOD_TLS_CERT` / `OPOD_TLS_KEY` and its workers `OPOD_LEADER_CA` and the *north* side is encrypted (`OPOD_PROBE_LISTEN` then keeps `/healthz`, `/readyz`, `/loadz` and `/metrics` on a plain port so a scraper needs no CA); leader → worker stays plain HTTP, HMAC-signed. A worker can also be identified by a **client certificate** instead of the join token it holds (`OPOD_NODE_CERT` / `OPOD_NODE_KEY`, node id read from its SPIFFE SAN), which a manager turns on per endpoint in the auth snapshot — HMAC stops in-flight token theft but doesn't replace network-layer encryption. The bearer-fallback path is supported for upgrade transitions; set `OPOD_REJECT_BEARER=1` once every leader and worker is on a recent build. A worker also loads the model it was started for (`OPOD_LOAD_MODEL`) in-process rather than over its own API, so nothing on a worker needs the bearer path to start serving.

### 🌐 Network behavior — every call Opod can make

`opod up` prints the part of this that is configured on this node — tracing, log export and "Telemetry: none" — as the "Network behavior on this node" banner; the table below is the complete set. **Telemetry is off — Opod never reports installs, usage, errors, or any data to opod.io or any analytics endpoint.** The only calls below the gateway makes are operator-configured.

| Direction | When | Disable |
|---|---|---|
| → engine endpoint (`ollama` / `vllm` / `sglang` / `mlx` / `llamacpp`) | Every inference request. The engine is operator-selected (`engine.preferred`) and normally on this host or your LAN. | Don't pick that engine. |
| ↔ leader / workers | A worker you joined heartbeats to its leader every 5 s and receives model loads; HMAC-signed per node. Only between machines you joined. | `opod node remove`, or don't `opod join`. |
| → Hugging Face Hub (or `HF_ENDPOINT`) / the Ollama registry | When weights are pulled: `opod model add`, `opod fetch`, the first-run starter model you accepted, a worker loading a model it does not hold yet. | Pre-place the weights in the models directory; point `HF_ENDPOINT` at your mirror. |
| → `api.github.com/repos/opod-io/opod-core/releases/latest` (and the release download from `github.com/opod-io/opod-core/releases`) | Only when you run `opod update` / `opod update --check`. Anonymous; no Opod-specific identifier sent. **Never at `opod up`.** | Don't run `opod update`. |
| → OTLP collector (traces) | If `OPOD_OTLP_ENDPOINT` is set. Spans go **only** to that endpoint — your own collector. | Unset `OPOD_OTLP_ENDPOINT`. |
| → OTLP collector (logs) | If `OPOD_OTLP_LOGS_ENDPOINT` is set. This process's log records, beside stderr, to your own collector. | Unset `OPOD_OTLP_LOGS_ENDPOINT`. |

That is the whole list. Opod does not forward requests to any cloud model vendor, holds no vendor API keys, and has no usage callbacks; with no OTLP endpoint configured, `opod up` makes **zero** outbound calls beyond the engines and nodes you chose.

---

## 📖 Every command has --help

```bash
opod --help                  # top-level
opod up --help               # any subcommand
opod shard create --help     # any sub-subcommand
opod model --help            # see the available actions
```

---

**Stuck?** Open an issue: <https://github.com/opod-io/opod-core/issues>
