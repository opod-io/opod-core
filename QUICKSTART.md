# Opod — Quick Start

The fastest path from zero to your first local chat completion. **3 minutes** on a fresh Mac or Linux machine.

> For full docs see [README.md](README.md). For design see [ARCHITECTURE.md](ARCHITECTURE.md).

---

## 🤔 First — how many machines?

| Your situation | Use |
|---|---|
| **One person**, or a small team sharing one beefy box | **1 machine** — everything below works |
| **More throughput** (lots of concurrent users) | 2+ machines — leader + workers |
| **A model bigger than any single machine** (e.g. Llama 70B on Mac Minis) | 2+ machines + sharding (`opod shard create`) |
| **Heterogeneous fleet** (e.g. Mac for coder model, NVIDIA for chat) | 2+ machines, models pinned per node |

**One machine is enough for most teams.** Multi-machine is for scale-out, not a requirement. Both setups install the same way — only the *commands you run after installing* differ.

### 🖼️ Single-machine setup (what you're building below)

```
              Your computer  (Mac or Linux)
   ┌─────────────────────────────────────────────────┐
   │                                                 │
   │   ┌───────────┐  ┌────────────┐  ┌───────────┐  │
   │   │  Cursor   │  │ Claude Code│  │   curl    │  │
   │   │  Aider    │  │            │  │   SDKs    │  │
   │   └─────┬─────┘  └─────┬──────┘  └─────┬─────┘  │
   │         └──────────────┼───────────────┘        │
   │                        │                        │
   │                        ▼                        │
   │           ┌──────────────────────────┐          │
   │           │      OPOD  :8080        │          │
   │           │   OpenAI + Anthropic     │          │
   │           │   APIs · auth · quotas   │          │
   │           │   audit log · admin UI   │          │
   │           └────────────┬─────────────┘          │
   │                        │ (local pipe)           │
   │                        ▼                        │
   │           ┌──────────────────────────┐          │
   │           │   Ollama  :11434         │          │
   │           │   (the actual LLM)       │          │
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

## 🍎 macOS (Apple Silicon)

```bash
# 1. install Ollama (use the cask — the plain `brew install ollama` is broken)
brew install --cask ollama
open -a Ollama

# 2. install Opod
curl -fsSL https://raw.githubusercontent.com/opod-io/opod/main/installer/install.sh | sh

# 3. start Opod with a small model (auto-downloads on first run)
OPOD_DEFAULT_MODEL=llama-3.2-1b opod up
```

> First-run behavior: on an empty install, `opod up` shows a one-line prompt asking whether to pull the recommended starter for your hardware (press enter to accept, `o` to pick another, `n` to skip). Add `--no-wizard` for a quiet boot, or `--auto-pull=false` to skip the pull entirely.

## 🐧 Linux (x86_64 or arm64)

```bash
# 1. install Ollama
curl -fsSL https://ollama.com/install.sh | sh
sudo systemctl enable --now ollama

# 2. install Opod
curl -fsSL https://raw.githubusercontent.com/opod-io/opod/main/installer/install.sh | sh

# 3. start Opod with a small model
OPOD_DEFAULT_MODEL=llama-3.2-1b opod up
```

---

## ✅ What you should see

After step 3, Opod prints:

```
▶ detected darwin/arm64 · 24 GB RAM · 8 cores
✔ default model: llama-3.2-1b
✔ engine: ollama at http://127.0.0.1:11434
▶ pulling llama3.2:1b ... 100%
✔ model ready: llama-3.2-1b

  Opod is ready.

  Dashboard:  http://localhost:8080
  API:        http://localhost:8080/v1
  Health:     http://localhost:8080/healthz

  Admin API key (shown once — store it now):
    sk-orc-xK9pQANw-nmzUbVdvL3S-aJKKvPeNa-eedqt

  Next steps:
    →  Test in the browser:  http://localhost:8080
    →  Wire up Claude Code:  opod connect claude-code
    →  Wire up Cursor:       opod connect cursor
    →  See all clients:      opod connect --list

  Press Ctrl-C to stop.
```

**Copy that admin key now.** You won't see it again. (It's also saved to `~/.opod/admin.key` for subsequent CLI commands like `opod connect` and `opod token`.)

---

## 💬 Test it (pick one)

### A) Fastest — `opod connect <tool>`

Prints copy-paste config for any of 19 supported tools, with your URL + token already substituted:

```bash
opod connect claude-code     # Anthropic-API tools
opod connect cursor          # IDE settings
opod connect aider           # CLI flags
opod connect                 # no arg → interactive picker
opod connect --list          # see all 19
```

### B) The console

Core has no web dashboard (ADR-022). The console — Connect cards, Playground, keys, teams — is the control plane (`opodcp`, `helm install opod`). From core alone, use the CLI (`opod connect`, `opod token`) or curl.

### C) curl from your terminal

```bash
KEY="sk-orc-xK9p…"   # paste your key

curl http://localhost:8080/v1/chat/completions \
  -H "Authorization: Bearer $KEY" \
  -H "Content-Type: application/json" \
  -d '{"model":"auto","messages":[{"role":"user","content":"say hi in 5 words"}]}'
```

You'll see JSON like:

```json
{"choices":[{"message":{"role":"assistant","content":"Hello! How can I help?"}}]}
```

### D) Manual — Claude Code (if you can't run `opod connect`)

```bash
export ANTHROPIC_BASE_URL=http://localhost:8080
export ANTHROPIC_AUTH_TOKEN=sk-orc-xK9p…
export ANTHROPIC_MODEL=llama-3.2-1b      # tell Claude Code which local model to use
claude
```

Claude Code now talks to your local Llama 1B instead of `api.anthropic.com`.

> **Why `ANTHROPIC_MODEL`?** Without it Claude Code defaults to a `claude-*` model name. With no `ANTHROPIC_API_KEY` set, Opod won't proxy to real Anthropic, so the request would 404 against your local engine. Setting `ANTHROPIC_MODEL` to a local catalog id makes Claude Code request your local model.

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

Anthropic-shape (`/v1/messages` with `image` content blocks) works too. Vision routes through the Ollama path today; the engine driver pulls the image bytes from data URLs or http(s) URLs.

### F) Embeddings

```bash
opod model add nomic-embed-text       # one-time install of the default embedding model

curl http://localhost:8080/v1/embeddings \
  -H "Authorization: Bearer $KEY" \
  -H "Content-Type: application/json" \
  -d '{"model":"nomic-embed-text","input":"hello world"}'
```

You'll get back an OpenAI-shape `{"data":[{"embedding":[…768 floats…]}]}` response. Use it with any RAG library that talks OpenAI embeddings.

---

## 👥 Share with your team

Once you've confirmed it works:

```bash
opod token create hadi
```

This creates a user-scope token for `hadi` (capped at 100k tokens/day) and prints a paste-into-Slack markdown card with config snippets for every supported client (Claude Code, Cursor, Aider, Continue, Zed, Cline, Qwen-Code, **Hermes Agent**, **OpenClaw**, **OpenCode**, **Open WebUI**, **Open Notebook**, **Goose**, **Plandex**, **OpenHands**, **Codex CLI**, OpenAI SDK, Anthropic SDK, curl). Your teammate copies the snippet for the tool they use → they're talking to your hardware.


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
| `Port 8080 in use` | Another process is on it. Use a different port: `OPOD_LISTEN=:8090 opod up` (avoid `:8081` — that's the default worker port) |
| `no admin key on disk` (running CLI) | `opod up` isn't running on this host. Start it first, then re-run the CLI command |

More fixes in the [main README's troubleshooting table](README.md#troubleshooting-installation).

---

## 🌐 Add a second (or third…) machine

Same install command everywhere. The first machine becomes the **leader**, every other machine becomes a **worker**.

### 🖼️ Two-machine setup (what you're about to build)

```
                MACHINE A  (leader)                            MACHINE B  (worker)
   ┌──────────────────────────────────────┐       ┌──────────────────────────────────────┐
   │                                      │       │                                      │
   │  Your tools ──► Opod :8080          │       │      Opod agent :8081               │
   │                 ┌───────┐            │       │      (worker HTTP server,            │
   │                 │ Router│ ───────────┼───────┼────► proxies requests to local       │
   │                 └───────┘            │       │       Ollama, token-auth'd)          │
   │                                      │       │                  │                   │
   │  Admin UI  :8080/                    │       │                  ▼                   │
   │  CLI: opod node ls / model ls       │       │      Ollama :11434                   │
   │                                      │       │      (does the model serving)        │
   │  Local Ollama :11434                 │       │                                      │
   │  (serves models the leader hosts)    │       │  ◄── heartbeat every 5s ──┐          │
   │                                      │       │      (carries loaded_models)         │
   │                                      │       │                           │          │
   └──────────────────────────────────────┘       └───────────────────────────┼──────────┘
                  ▲                                                           │
                  │                                                           │
                  └───────────── LAN / tailnet (e.g. WiFi, Tailscale) ────────┘
```

### 🖼️ Step-by-step (what happens when)

```
   LEADER (Machine A)                              WORKER (Machine B)
   ──────────────────                              ─────────────────

   1. install Ollama
   2. install Opod              (steps 1+2 same on every machine)
   3. opod up
      ✔ admin key shown
      ✔ listening :8080

   4. opod token create --node
      ✔ sk-orc-NodeJoin-AbCd1234…
              ─────► copy this token to the worker machine

                                                  1. install Ollama
                                                  2. install Opod
                                                  3. opod join \
                                                       http://leader:8080?token=...
                                                     ✔ registered with leader

                                ◄──── heartbeat every 5s ────

   5. opod node ls
      ID         HOSTNAME    STATE
      local      machine-a   ready
      n_abc123   machine-b   ready

                                                  6. opod model add qwen-coder-7b
                                                     (pulls on the worker's Ollama)

   7. curl :8080/v1/chat/completions
      with model=qwen-coder-7b
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

Note the leader's reachable address. On a LAN it's its LAN IP (e.g. `192.0.2.42`); on Tailscale, the tailnet hostname.

### Step 2 — on the new machine

Install Opod + Ollama **the same way as above**, then instead of `opod up`:

```bash
opod join http://192.0.2.42:8080?token=sk-orc-NodeJoin-ABcD1234…
```

(Substitute the leader's address and the token you copied.)

### Step 3 — install a model on the worker

```bash
opod model add qwen-coder-7b
```

### Step 4 — verify on the leader

```bash
opod node ls
# example output:
# ID         HOSTNAME      OS/ARCH       ADDRESS              STATE   LAST HB
# local      machine-a     darwin/arm64  127.0.0.1:8080       ready   2026-06-05T…
# n_abc123   machine-b     darwin/arm64  192.0.2.50:8081    ready   2026-06-05T…
```

Any request the gateway gets for `qwen-coder-7b` is now routed automatically to the worker. If you install the **same** model on two workers, the leader load-balances between them.

> ⚠️  Only do this on a trusted LAN or Tailscale — see [Security model](#-security-model-read-before-exposing-it) below.

**Need to split one big model across multiple machines?** That's *sharding* — `opod shard create <model> <N>`. See the [sharded models section in the README](README.md).

---

## 🤖 Use a different model (Qwen, Llama, DeepSeek…)

The default `llama-3.2-1b` is tiny — good for "does this work?" but underpowered for real work. Opod ships a curated **catalog** of better models.

### Browse what's in the catalog

```bash
opod model search           # list everything
opod model search coder     # filter
```

A summary table of the 41 catalog entries — see `opod model search` for the live list. ⭐ marks the current top picks.

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
| `lfm2.5-8b-a1b` ⭐ | best on-device edge MoE | 8 GB | `ollama:lfm2.5:8b-a1b` |
| `qwen3-8b` | general chat, balanced | 12 GB | `ollama:qwen3:8b` |
| `glm-4-9b` | dense chat, 128K ctx, tool-calling | 12 GB | `ollama:glm4:9b` |
| `qwen3-vl-8b` | vision + tools (charts, OCR, UI) | 10 GB | `ollama:qwen3-vl:8b` |
| `gemma4-e2b` | mobile/edge multimodal (text+image+audio) | 8 GB | `ollama:gemma4:e2b` |
| `gemma4-12b` | encoder-free multimodal | 12 GB | `ollama:gemma4:12b` |
| `gemma4-e4b` | mobile/edge multimodal | 12 GB | `ollama:gemma4:e4b` |
| `pixtral-12b` | Mistral vision-language | 16 GB | `hf: mistralai/Pixtral-12B-2409` (vLLM/MLX) |
| `mellum2-12b` | JetBrains MoE coder (2.5B active) | 12 GB | `ollama:mellum2:12b` |
| `mistral-nemo-12b` | 128K context chat | 12 GB | `ollama:mistral-nemo:12b` |
| `qwen-coder-14b` | better code + agent | 16 GB | `ollama:qwen2.5-coder:14b` |
| `qwen3-14b` | general chat, more capable | 16 GB | `ollama:qwen3:14b` |
| `phi-4-14b` | strong reasoning per byte | 12 GB | `ollama:phi-4:14b` |
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

# 3. or in Claude Code
export ANTHROPIC_MODEL=qwen-coder-14b
claude
```

> 📖 **Full step-by-step per-model guide:** [MODELS.md](MODELS.md) — for *every* model in the catalog: system requirements, performance expectations on Mac/Linux, install + use snippets for curl / Cursor / Claude Code / SDKs, when to switch up.

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

### Use a different engine entirely (vLLM, MLX-LM, llama.cpp)

Edit `~/.opod/config.yaml`:

```yaml
engine:
  preferred: vllm                       # was: ollama
  vllm_endpoint: http://gpu-host:8000   # where your vLLM is running
```

Or via env: `OPOD_ENGINE=vllm OPOD_VLLM_ENDPOINT=http://gpu:8000 opod up`. The router doesn't care which engine serves the request — it just routes by model name.

**Want bare-metal speed on weak hardware?** Use `llama.cpp` directly — lower RAM and cold-start latency than Ollama, and Opod auto-launches `llama-server` for you so it's still one command:

```bash
# 1. install llama.cpp (provides llama-server)
brew install llama.cpp     # macOS · apt: see https://github.com/ggml-org/llama.cpp
# (rpc-server is not in the Homebrew bottle — only needed for sharded
#  models; build from source with `cmake -DGGML_RPC=ON` if you need it)

# 2. that's it — Opod spawns llama-server itself
OPOD_ENGINE=llamacpp OPOD_DEFAULT_MODEL=llama-3.2-1b opod up
```

Opod looks up the catalog entry for the default model, reads its `source.repo` (a GGUF HuggingFace repo), and runs `llama-server -hf <repo> --port 8089` via its internal supervisor — the same one that already manages `rpc-server` for sharded models. When Opod stops, the spawned `llama-server` stops too.

If you'd rather manage `llama-server` yourself (e.g. to pass custom flags like `-ngl 999`), start it before `opod up` and Opod will detect it and skip the auto-spawn:

```bash
llama-server -m ~/models/qwen2.5-coder-7b-q4_k_m.gguf --port 8089 --n-gpu-layers 999
OPOD_ENGINE=llamacpp opod up
```

Opod's default `llamacpp_endpoint` is `http://127.0.0.1:8089` — chosen to avoid both `:8080` (Opod leader) and `:8081` (worker agent). The same `llamacpp` engine name also covers RPC sharding — `opod shard create` launches a `llama-server --rpc …` coordinator that this driver talks to.

---

## 🔌 Switch Claude Code back to real Anthropic

You set three env vars to route Claude Code through Opod. The fastest way back:

```bash
opod disconnect claude-code
```

This prints the exact `unset` + `export` commands you need (works for every client `opod connect` supports — see `opod disconnect --list`). Paste what it prints, and you're back on `api.anthropic.com`.

Manually, it's just unsetting the three env vars:

```bash
unset ANTHROPIC_BASE_URL
unset ANTHROPIC_AUTH_TOKEN
unset ANTHROPIC_MODEL
```

Then in the same terminal, set your real Anthropic key:

```bash
export ANTHROPIC_API_KEY=sk-ant-…
claude
```

Or just **open a fresh terminal** that never had the Opod vars exported — Claude Code defaults to `api.anthropic.com` when `ANTHROPIC_BASE_URL` isn't set.

### Make the switch permanent

If you'd added those `export` lines to `~/.zshrc` or `~/.bashrc`, remove them:

```bash
# before:
export ANTHROPIC_BASE_URL=http://localhost:8080
export ANTHROPIC_AUTH_TOKEN=sk-orc-...
export ANTHROPIC_MODEL=llama-3.2-1b

# after (remove all three, or just comment them out)
```

Then `source ~/.zshrc` (or open a new terminal).

### Hybrid: keep Opod as your default, fall back to real Claude when needed

This is the best-of-both pattern. Leave the three vars set, but configure Opod with your real Anthropic key:

```bash
# add to your shell rc:
export ANTHROPIC_API_KEY=sk-ant-…    # real Anthropic key (for Opod to proxy)
# (Opod vars stay as before)
```

Restart `opod up`. Now:
- `claude --model llama-3.2-1b` → served by your local Ollama (free, private)
- `claude --model claude-opus-4-7` → transparently proxied to real Anthropic by Opod, logged in the Usage tab, billed to *your* Anthropic account

Same `claude` command, same key paste, you pick per-prompt.

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
opod update --version v1.11.0    # pin a specific version (see github.com/opod-io/opod/releases)
opod update --force              # reinstall even if already on the latest
opod upgrade                     # alias of `update`
```

If your binary lives in `/usr/local/bin/` (installed with sudo), `opod update` stages the new binary next to it and prints the exact `sudo mv` command to finish.

### 🔔 Update notice on `opod up`

`opod up` checks GitHub for a newer release on startup and prints a one-liner if one exists. The check is cached for 24 hours at `~/.opod/update-check.json` so it only hits GitHub once a day, with a hard 1-second budget so it never slows startup.

To disable (offline environments, privacy):

```bash
export OPOD_NO_UPDATE_CHECK=1
opod up
```

---

## 🎯 Next steps

- **See the full UI tour, CLI reference, troubleshooting**: [README.md](README.md)
- **Understand the architecture**: [ARCHITECTURE.md](ARCHITECTURE.md)
- **Per-command help**: `opod <cmd> --help` for any command
- **Add more workers**: see [Add a second machine](#-add-a-second-or-third-machine) above

---

## 🔒 Security model (read before exposing it)

Opod assumes a **trusted network** (LAN or [Tailscale](https://tailscale.com/)). Specifically:

- **User API keys** (admin / user scope) are **sha256-hashed** in the database. The plaintext shown at creation time is the only way to use the key.
- **Worker tokens** (the shared secret between leader and worker) are stored on the `nodes.worker_token` column. Control-plane traffic uses **HMAC-SHA256 signatures** so the token itself isn't transmitted on the wire after the initial join — the agent and leader both sign with the per-node token. The SQLite file still holds the secret, so a stolen DB still lets an attacker impersonate a worker; encrypt the DB at rest if you can't trust the host. Set `OPOD_REJECT_BEARER=1` on workers to refuse the bearer-fallback path entirely (HMAC-only).
- **Worker HTTP servers** bind only to the mesh address (LAN / tailnet IP), never to `0.0.0.0`. Network reachability is the first line of defense.
- The **embedded web UI** authenticates by pasted admin key (stored in browser `localStorage`).

If you're not on a trusted LAN, still run the cluster **behind Tailscale** or a similar zero-trust overlay — HMAC stops in-flight token theft but doesn't replace network-layer encryption. The bearer-fallback path is supported for upgrade transitions; set `OPOD_REJECT_BEARER=1` once every leader and worker is on a recent build.

### 🌐 Network behavior — every call Opod can make

Opod prints this same list at startup as the "Network behavior on this node" banner. **Telemetry is off — Opod never reports installs, usage, errors, or any data to opod.io or any analytics endpoint.** The only calls below the gateway makes are operator-configured.

| Direction | When | Disable |
|---|---|---|
| → `github.com/opod-io/opod/releases/latest` | At `opod up`, max 1× per 24h. Anonymous; no Opod-specific identifier sent. Cached at `~/.opod/update-check.json`. | `OPOD_NO_UPDATE_CHECK=1` |
| → engine endpoint (`ollama` / `vllm` / `mlx` / `llamacpp`) | Every inference request. Engine is operator-selected via `engine.preferred`. | Don't pick that engine. |
| → `api.anthropic.com` | On `claude-*` requests if `ANTHROPIC_API_KEY` is set. | Unset the key. |
| → `api.openai.com` | On `gpt-*`/`o-*` requests if `OPENAI_API_KEY` is set. | Unset the key. |
| → `bedrock-runtime.<region>.amazonaws.com` | On `anthropic.*` requests if `OPOD_BEDROCK_REGION` is set. Uses AWS credentials chain. | Unset `OPOD_BEDROCK_REGION`. |
| → `<location>-aiplatform.googleapis.com` | On `gemini-*` requests if `OPOD_VERTEX_PROJECT` is set. Uses ADC. | Unset `OPOD_VERTEX_PROJECT`. |
| → `openrouter.ai/api/v1` | On `openrouter/<model>` requests if `OPENROUTER_API_KEY` is set. | Unset the key (or set `router.fallback.openrouter_url` to redirect). |
| → `api.groq.com/openai/v1` | On `groq/<model>` requests if `GROQ_API_KEY` is set. | Unset the key. |
| → `api.together.xyz/v1` | On `together/<model>` requests if `TOGETHER_API_KEY` is set. | Unset the key. |
| → `api.fireworks.ai/inference/v1` | On `fireworks/<model>` requests if `FIREWORKS_API_KEY` is set. | Unset the key. |
| → `api.cohere.com/compatibility/v1` | On `cohere/<model>` requests if `COHERE_API_KEY` is set. | Unset the key. |
| → `api.mistral.ai/v1` | On `mistral/<model>` requests if `MISTRAL_API_KEY` is set. | Unset the key. |
| → `api.perplexity.ai` | On `perplexity/<model>` requests if `PERPLEXITY_API_KEY` is set. | Unset the key. |
| → `OPOD_WHISPER_ENDPOINT` | On `POST /v1/audio/transcriptions` if set. | Unset; endpoint returns 501 with setup hint. |
| → `OPOD_PIPER_ENDPOINT` | On `POST /v1/audio/speech` if set. | Unset; endpoint returns 501. |
| → OTLP collector | If `OPOD_OTLP_ENDPOINT` is set. Spans go **only** to that endpoint — your own collector, not upstream. | Unset `OPOD_OTLP_ENDPOINT`. |
| → webhook URL(s) | Every usage / audit event if `observability.callbacks: [- kind: webhook]` is configured. HMAC-SHA256 signature in `X-Opod-Signature`. | Remove the entry from `config.yaml`. |
| → `cloud.langfuse.com` (or `host`) | Every usage event if `observability.callbacks: [- kind: langfuse]` is configured. | Remove the entry. |
| → S3 bucket (or `endpoint`) | Batched usage / audit events as NDJSON objects if `observability.callbacks: [- kind: s3]` is configured. Uploads on the worker goroutine, never the request path. | Remove the entry. |
| → guardrail webhook URL(s) | Every `/v1/chat/completions` if `observability.guardrails:` is configured. **Synchronous on the request path**; gateway waits for the response. | Remove the entry. |
| → HuggingFace / Ollama registry | At `opod model add` when pulling weights for a catalog entry. Operator-invoked. | Don't run `opod model add`. |

Set `OPOD_NO_UPDATE_CHECK=1` if you want **zero** outbound calls from `opod up` itself (assuming no API keys / OTLP / Bedrock / Vertex are configured). The gateway only talks to engines you've chosen and vendors you've keyed.

---

## 📖 Every command has --help

```bash
opod --help                  # top-level
opod up --help               # any subcommand
opod shard create --help     # any sub-subcommand
opod model --help            # see the available actions
```

---

**Stuck?** Open an issue: <https://github.com/opod-io/opod/issues>
