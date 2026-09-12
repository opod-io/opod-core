# opod-worker-tt

vLLM on Tenstorrent, on Tenstorrent's own `tt-inference-server` tt-metal image.

**The engine starts itself.** tt-metal's vLLM is `run_vllm_api_server.py` — env- and
model-spec-configured, started by Tenstorrent's entrypoint, model loaded at startup. `vllm serve
<model>`, which the agent runs on model load for the NVIDIA/AMD images, does not apply. So this image
sets `OPOD_ENGINE_START` and opod connects to the OpenAI API rather than launching anything, which is
the contract core's vLLM driver already documents. Core carries no Tenstorrent-specific code: the
driver's `tt-openai`/`tenstorrent`/`tt` aliases exist because the wire protocol is identical.

Consequences worth knowing:

- **The model is fixed at pod start**, by the base image's own env. `POST /v1/model/load` will not swap
  it, so an endpoint changes model by rolling the pod — which is what a rollout does anyway.
- **The device arrives from the device plugin.** `tenstorrent.com/tt: 1` is enough; nothing mounts
  `/dev/tenstorrent` by hand (proven with a claim probe, 2026-09-11).
- **1 GB hugepages are a performance knob, not a gate.** Without them tt-metal warns
  (`no huge page mount found for hugepage_size: 1073741824`) and runs.
- **Ethernet firmware below 1.7.0** makes tt-metal fall back to single-erisc mode. Fine for one chip;
  it is the thing to check first when multi-chip performance disappoints.

Verified on a 4× Blackhole p150b Quietbox, tt-kmd 2.11.0, 2026-09-11: tt-metal opened all four chips.

## What a plan must supply (the remaining V10 work)

The base server is configured by environment, not by `vllm serve <id>`, so the control plane has to
translate a plan's model into that contract rather than pass an id. Read out of
`run_vllm_api_server.py` in the 0.10.1 image:

| env | meaning |
|---|---|
| `MODEL_SPEC` / `TT_MODEL_SPEC_JSON_PATH` | which model, as tt-metal's own spec |
| `MODEL_WEIGHTS_PATH`, `TT_CACHE_PATH`, `CACHE_ROOT` | weights and the compiled-kernel cache |
| `HF_TOKEN` | only for gated repos |
| `SERVICE_PORT` | pin to 8000 so it matches `OPOD_VLLM_ENDPOINT` |
| `VLLM_API_KEY`, `JWT_SECRET` | the base's own auth; opod talks to it on loopback |

**Models this build supports** (`tt-metal/models/tt_transformers/model_params/`): Llama-3.1-8B/70B,
Llama-3.2-1B/3B and the 11B/90B Vision pair, Mistral-7B-v0.3, Qwen2.5-7B, the Qwen2.5-VL family,
Qwen3-VL-32B, gemma-3-27b.

For the first serving proof prefer **Qwen2.5-7B-Instruct** over Llama-3.2-1B: it is the smallest
ungated repo on the list, so it needs no `HF_TOKEN` and the run does not stall on a licence click.
