# Engines: install and run each one with Opod

Opod does not run models itself. A **worker** (`opod join`) drives one **engine** on its machine, and the **leader** (`opod up`) puts every worker behind one URL. This page shows how to install each of the six engines and join it to a leader.

- [Pick an engine](#pick-an-engine)
- [How a worker runs its engine](#how-a-worker-runs-its-engine)
- [llama.cpp](#llamacpp) — any vendor, gangs across machines
- [vLLM](#vllm) — NVIDIA throughput, tensor parallel, Ray gangs
- [SGLang](#sglang) — NVIDIA, prefix-cache heavy workloads
- [TensorRT-LLM](#tensorrt-llm) — NVIDIA's own serving stack, one node
- [MLX-LM](#mlx-lm) — Apple Silicon
- [Ollama](#ollama) — the simplest single machine
- [Container images](#container-images)
- [Engine flags](#engine-flags)
- [If it fails](#if-it-fails)

Every example starts from a leader that is already running:

```bash
opod up                          # on any machine the workers can reach; it needs no GPU
opod token create --node         # prints the join token the workers use below as <token>
```

`<leader-host>` is that machine's address. Keep the join URL in quotes: `?` is a glob in zsh.

---

## Pick an engine

| Your hardware | Your goal | Engine |
|---|---|---|
| One NVIDIA card or a box of them | Throughput, many users, tool calls | **vLLM** |
| Two or more NVIDIA machines | One model across them | **vLLM** + Ray gang, or **llama.cpp** RPC gang |
| Mixed cards: NVIDIA + AMD + Intel, or big + small | One model across them | **llama.cpp** RPC gang |
| AMD Radeon or Intel Arc | One card | **llama.cpp** (or vLLM ROCm on AMD) |
| NVIDIA, long shared prompts (agents, RAG) | Prefix-cache reuse | **SGLang** |
| NVIDIA, NVIDIA's own stack | One node | **TensorRT-LLM** |
| A Mac | One machine, fastest on Apple Silicon | **MLX-LM**, or **llama.cpp** (also joins gangs) |
| Any laptop, first try | It works in a minute | **llama.cpp** or **Ollama** |

What has served on real hardware, per engine and vendor (from [`images/images.yaml`](../images/images.yaml); `opod image ls` prints it):

| Engine | NVIDIA | AMD | Intel | Tenstorrent | CPU | Gang across machines |
|---|---|---|---|---|---|---|
| llama.cpp | ✅ | ✅ one card | ✅ Arc A770 | — | ✅ | ✅ RPC parts, mixed vendors |
| vLLM | ✅ | ✅ one card | ❌ not yet | ✅ | — | ✅ Ray, pipeline parallel 2 on NVIDIA |
| SGLang | ✅ one card | ❌ Instinct build, not run | ❌ not yet | — | — | built, not proven |
| TensorRT-LLM | ✅ one card | — | — | — | — | ❌ one node only |
| MLX-LM | Apple Silicon only | | | | | ❌ |
| Ollama | ✅ | ✅ | — | — | ✅ | ❌ one machine per model |

The proof is for the container images on hardware. The plain `pip` and `brew` installs below use the same launch the images run, but not every line here was run exactly as written.

## How a worker runs its engine

You never start vLLM, SGLang, TensorRT-LLM or llama-server by hand. You tell the worker **which engine** and **which model**, and the worker:

1. starts the engine process with the right flags (`--served-model-name`, tensor parallel, memory fraction, tool-call parser),
2. waits until it answers,
3. reports the model to the leader on its next heartbeat. From then on the leader routes that model's requests to it.

```bash
OPOD_ENGINE=<engine> OPOD_LOAD_MODEL=<catalog id> opod join "http://<leader-host>:8080?token=<token>"
```

| Variable | Meaning |
|---|---|
| `OPOD_ENGINE` | `llamacpp`, `vllm`, `sglang`, `trtllm`, `mlx` or `ollama` |
| `OPOD_LOAD_MODEL` | A catalog id (`opod catalog ls`). The catalog names the repo and file to download |
| `OPOD_LOAD_REPO`, `OPOD_LOAD_FILE` | Use another Hugging Face repo (and GGUF file) under that id |
| `OPOD_ENGINE_FLAGS` | JSON of engine knobs — see [Engine flags](#engine-flags) |
| `HF_TOKEN` | For gated Hugging Face repos |
| `OPOD_ADVERTISE_ADDR` | `host:8081` the leader should dial back, when the default address is wrong (VPN, several NICs) |

Instead of `OPOD_LOAD_MODEL`, you can also join an idle worker and place a model from the leader: `opod model add <id> --node <node-id>`.

One worker serves one model at a time on vLLM, SGLang, TensorRT-LLM and llama.cpp. For a second model on the same machine, start a second worker (`opod join --gpu <index>` pins it to one card).

---

## llama.cpp

**For:** every vendor, CPU included, GGUF weights, and gangs: one model split across machines and vendors.

### Install

Mac (Metal is built in):

```bash
brew install llama.cpp
```

Linux, or a Mac that will host a gang part — build it with the RPC backend (the Homebrew bottle has no `rpc-server`):

```bash
git clone https://github.com/ggml-org/llama.cpp && cd llama.cpp
cmake -B build -DGGML_RPC=ON -DGGML_CUDA=ON     # NVIDIA · AMD: -DGGML_HIP=ON · Intel: -DGGML_SYCL=ON · CPU: no flag
cmake --build build --config Release -j
sudo cp build/bin/llama-server build/bin/rpc-server /usr/local/bin/
```

### Run

One machine, leader and engine in one process (Opod launches `llama-server` itself):

```bash
OPOD_ENGINE=llamacpp OPOD_DEFAULT_MODEL=llama-3.2-3b-gguf OPOD_PULL_DEFAULT_MODEL=1 opod up
```

As a worker of another leader:

```bash
OPOD_ENGINE=llamacpp OPOD_LOAD_MODEL=llama-3.2-3b-gguf opod join "http://<leader-host>:8080?token=<token>"
```

As a gang across machines: join each machine with no model, then on the leader:

```bash
opod shard create llama-3.2-3b-sharded 2      # the leader starts an rpc-server on each worker and a coordinator
```

### Check it worked

```bash
curl http://<leader-host>:8080/v1/chat/completions -H "Authorization: Bearer <admin key>" \
  -H "Content-Type: application/json" \
  -d '{"model":"llama-3.2-3b-gguf","messages":[{"role":"user","content":"hi"}]}'
```

Measured on an Apple M3 on 2026-10-31: the first `opod up` downloaded the 2 GB model and served in about 3 minutes; chat, a tool call and `/v1/messages` all answered.

GGUF models in the catalog: `llama-3.2-3b-gguf`, `qwen2.5-0.5b-gguf`, `nomic-embed-text-v1.5-gguf` (embeddings), `bge-reranker-v2-m3-gguf` (rerank), and the `*-sharded` entries for gangs.

---

## vLLM

**For:** NVIDIA throughput — continuous batching, tensor parallel across the cards of one box, Ray gangs across NVIDIA machines, LoRA adapters, sleep mode. Also AMD (ROCm) and Tenstorrent through the images. Linux only.

### Install

```bash
pip install vllm             # in a virtualenv; needs an NVIDIA driver
```

### Run

```bash
OPOD_ENGINE=vllm OPOD_LOAD_MODEL=llama-3.1-8b opod join "http://<leader-host>:8080?token=<token>"
```

The worker runs `vllm serve <repo> --served-model-name llama-3.1-8b …`. On a box with several cards it sets `--tensor-parallel-size` to the largest power of 2 that fits the GPU count (5 cards → 4). Pin it with `OPOD_ENGINE_FLAGS='{"tp":"2"}'`.

`llama-3.1-8b` is 16 GB of weights, so it needs a 24 GB card. Any Hugging Face repo vLLM can serve works under a catalog id with `OPOD_LOAD_REPO=<owner>/<repo>`. vLLM-ready catalog entries include `llama-3.1-8b`, `mimo-7b`, `pixtral-12b`, `moondream3`.

**A gang across two NVIDIA machines** uses a catalog entry whose `sharding.engine` is `vllm` (for example `mimo-7b-ray`): `opod shard create mimo-7b-ray --pp 2`. Pipeline parallel 2 over two 48 GB cards in two machines served on 2026-09-14.

---

## SGLang

**For:** NVIDIA workloads that repeat long prompts — agents, RAG, chat with long system prompts — where RadixAttention reuses the prefix cache. Linux only.

### Install

```bash
pip install uv && uv pip install sglang     # the worker runs `python3 -m sglang.launch_server`, so python3 must see it
```

### Run

```bash
OPOD_ENGINE=sglang OPOD_LOAD_MODEL=llama-3.1-8b opod join "http://<leader-host>:8080?token=<token>"
```

The worker starts SGLang on port 30000 with the model's tool-call parser from the catalog. Tensor parallel: `OPOD_ENGINE_FLAGS='{"tp":"2"}'`.

One known limit: SGLang v0.5.21 cannot run a **pipeline** (pp > 1) of a Llama-family model with tied embeddings; Opod refuses that gang by name. Use tensor parallel or vLLM.

---

## TensorRT-LLM

**For:** NVIDIA's own serving stack (`trtllm-serve`, PyTorch backend, safetensors, no engine build). One node only: its multi-node mode needs MPI, which Opod does not carry.

### Install

The simplest path is NVIDIA's container, with the host's `opod` binary mounted in. Opod does not publish a TensorRT-LLM image: NVIDIA's licence forbids distributing a modified copy, so you pull it from `nvcr.io` and accept NVIDIA's terms.

```bash
docker run -d --name opod-trtllm --gpus all --ipc host --network host \
  -v "$(command -v opod)":/usr/local/bin/opod:ro \
  -e OPOD_ENGINE=trtllm -e OPOD_LOAD_MODEL=llama-3.1-8b -e OPOD_JOIN_TOKEN=<token> \
  nvcr.io/nvidia/tensorrt-llm/release:1.2.1 \
  opod join "http://<leader-host>:8080"
```

Or bare metal, following [NVIDIA's install guide](https://nvidia.github.io/TensorRT-LLM/installation/linux.html) (`pip install tensorrt_llm` with CUDA and OpenMPI), then:

```bash
OPOD_ENGINE=trtllm OPOD_LOAD_MODEL=llama-3.1-8b opod join "http://<leader-host>:8080?token=<token>"
```

The worker runs `trtllm-serve` on port 8000. Served on one RTX 5090 on 2026-10-23 and 2026-10-25. No runtime LoRA, no sleep mode, no images yet.

---

## MLX-LM

**For:** the fastest single-machine inference on Apple Silicon. The worker does not launch MLX-LM: run its server yourself, on a port the leader does not use.

```bash
pip install mlx-lm
mlx_lm.server --model mlx-community/Llama-3.2-3B-Instruct-4bit --port 8090 &
OPOD_ENGINE=mlx OPOD_MLX_ENDPOINT=http://127.0.0.1:8090 opod join "http://<leader-host>:8080?token=<token>"
```

The model is served under the repo name you started it with. MLX-LM cannot be part of a gang.

---

## Ollama

**For:** the simplest single machine. Ollama serves each model whole on one machine: it cannot be part of a gang, and Opod refuses requests with `tools` on it (its tool protocol is a different shape). For agents, use vLLM, SGLang or llama.cpp.

```bash
brew install --cask ollama && open -a Ollama          # Mac: the cask, not the formula
curl -fsSL https://ollama.com/install.sh | sh          # Linux
OPOD_DEFAULT_MODEL=llama-3.2-1b OPOD_PULL_DEFAULT_MODEL=1 opod up
```

Ollama is Opod's default engine when `OPOD_ENGINE` is not set. As a worker: `OPOD_ENGINE=ollama opod join "…"`, then `opod model add <id> --node <node-id>` on the leader.

---

## Container images

Every engine except TensorRT-LLM and MLX-LM ships as a worker image with `opod` inside. The same images run under the Opod Control Plane on Kubernetes. On a plain Docker host:

```bash
docker run -d --name opod-worker --gpus all --ipc host --network host \
  -v /data/models:/data/models \
  -e OPOD_ROLE=worker -e OPOD_LEADER_URL=http://<leader-host>:8080 -e OPOD_JOIN_TOKEN=<token> \
  -e OPOD_ENGINE=vllm -e OPOD_LOAD_MODEL=llama-3.1-8b \
  ghcr.io/opod-io/opod-worker-vllm-nvidia:latest
```

| Image | Engine · vendor | Docker device flags |
|---|---|---|
| `opod-worker-vllm-nvidia` | vLLM · NVIDIA | `--gpus all --ipc host` |
| `opod-worker-vllm-amd` | vLLM · AMD ROCm | `--device /dev/kfd --device /dev/dri --group-add video --ipc host` |
| `opod-worker-sglang-nvidia` | SGLang · NVIDIA (driver 580+) | `--gpus all --ipc host` |
| `opod-worker-llamacpp-nvidia` | llama.cpp + RPC · NVIDIA | `--gpus all` |
| `opod-worker-llamacpp-amd` | llama.cpp + RPC · AMD ROCm | `--device /dev/kfd --device /dev/dri --group-add video` |
| `opod-worker-llamacpp-intel` | llama.cpp + RPC · Intel SYCL | `--device /dev/dri` |
| `opod-worker-llamacpp-cpu` | llama.cpp + RPC · CPU | none |

All pull from `ghcr.io/opod-io/` without a login. Set `OPOD_ENGINE` to match the image. `opod image recommend <model>` names the image for a model, and `opod image show <image>` its limits.

---

## Engine flags

`OPOD_ENGINE_FLAGS` is a JSON map. Only these keys are read; the rest of the command line stays the worker's choice.

| Engine | Keys |
|---|---|
| vLLM | `tp`, `gpu_memory_utilization` (0.05–0.95), `max_model_len`, `max_num_seqs`, `kv_cache_dtype`, `pp_layer_partition`, `extra` (raw args) |
| SGLang | `tp`, `mem_fraction_static`, `max_model_len`, `max_running_requests`, `kv_cache_dtype`, `pp_layer_partition`, `extra` |
| llama.cpp | `ctx`, `ngl`, `parallel`, `kv_cache_type`, `kv_unified`, `tensor_split`, `extra` |
| TensorRT-LLM | `tp`, `max_num_seqs`, `max_model_len`, `kv_cache_free_gpu_memory_fraction`, `kv_cache_dtype`, `extra` |

Example: `OPOD_ENGINE_FLAGS='{"tp":"2","max_model_len":"16384"}'`.

To share one card between two workers, give each a memory budget: `opod join --gpu 0 --vram-budget 12 "…"`. The worker turns it into the engine's own memory knob.

---

## If it fails

| Symptom | Cause | Fix |
|---|---|---|
| `opod node ls` shows the worker, but requests for its model answer 404 | The engine is still loading (vLLM takes minutes on a large model) | Wait for the next heartbeat; `opod node show <id>` prints the node's full record as JSON |
| `auto-spawn failed: … not reachable` on `opod up` with llama.cpp | `llama-server` is not on PATH, or the download outran the budget | `which llama-server`; then run `opod up` again |
| `400 … tools` on an Ollama model | Ollama's tool protocol is a different shape | Serve that model on vLLM, SGLang or llama.cpp |
| A gang will not form: `rpc-server not found` | llama.cpp built without `-DGGML_RPC=ON`, or the Homebrew bottle | Build from source as above |
| vLLM: `heads must be divisible by …` | Tensor parallel does not divide the model's heads | Set `OPOD_ENGINE_FLAGS='{"tp":"<power of 2>"}'` |
| The leader cannot reach the worker | The worker advertised an address the leader cannot dial | `OPOD_ADVERTISE_ADDR=<ip>:8081` on the worker |

`opod doctor` checks the local machine and names the fix for what it finds. More: the [README](../README.md), [QUICKSTART](../QUICKSTART.md), and [`images/README.md`](../images/README.md) for every image's limits.

Running this for a company — many teams, many GPUs, Kubernetes? See the **[Opod Control Plane](../README.md#-enterprise-opod-control-plane)** · [opod.io](https://opod.io) · [hadi@opod.io](mailto:hadi@opod.io).
