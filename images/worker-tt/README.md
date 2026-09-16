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
- **1 GB hugepages are a gate for serving, not a performance knob.** The host provides them
  (Tenstorrent's `tenstorrent-hugepages.service` mounts `/dev/hugepages-1G`); the container must see that
  mount. Without it tt-metal only *opens* the chips — which is all the 2026-09-11 check did — and then dies
  in firmware init with `TT_ASSERT … local_chip.cpp:246: channel < get_num_host_channels()` right after
  `No hugepage mapping at device 0` (2026-09-15, on a host booted with `iommu=pt`). Tenstorrent's launcher
  bind-mounts it; a pod needs the same (`hostPath` or a `hugepages-1Gi` request with a `HugePages-1Gi`
  emptyDir).
- **Ethernet firmware below 1.7.0** makes tt-metal fall back to single-erisc mode. Fine for one chip;
  it is the thing to check first when multi-chip performance disappoints.

Verified on a 4× Blackhole p150b Quietbox, tt-kmd 2.11.0, 2026-09-11: tt-metal opened all four chips.

## What a plan must supply (the remaining V10 work)

The base server is configured by environment, not by `vllm serve <id>`, so the control plane has to
translate a plan's model into that contract rather than pass an id. Read out of
`run_vllm_api_server.py` and tt-inference-server's `run.py` / `workflows/` at the release tag:

| env | meaning |
|---|---|
| `TT_MODEL_SPEC_JSON_PATH` | the model spec as JSON: `workflows.model_spec.get_runtime_model_spec(args).to_json(…)` for `--model <name> --device <p150\|p150x4\|…>`. The image contains no spec; the launcher generates and mounts it |
| `CACHE_ROOT` | a writable volume; the vendor entrypoint re-groups it and the server keeps weight symlinks there |
| `TT_CACHE_PATH` | `$CACHE_ROOT/tt_metal_cache/cache_<model>/<P150\|P150x4\|…>` — the converted-weights cache, reused across restarts |
| `MODEL_WEIGHTS_PATH` | a directory of HF safetensors + tokenizer. The server names it by the spec's `hf_model_repo`, so any copy of the same weights works |
| `HF_HUB_OFFLINE=1` + spec `vllm_args.model` = the weights path, `served_model_name` = the repo id | without these vLLM fetches `config.json` from the Hub, which for a gated repo needs `HF_TOKEN` even when the weights are mounted |
| `/dev/hugepages-1G` | bind-mounted from the host (see above) |
| `VLLM_API_KEY`, `JWT_SECRET`, or spec `cli_args.no_auth` | the base's own auth; opod talks to it on loopback |

The server listens on the spec's `service_port` (8000 by default), which matches `OPOD_VLLM_ENDPOINT`.

**Models this build supports** (`tt-metal/models/tt_transformers/model_params/`): Llama-3.1-8B/70B,
Llama-3.2-1B/3B and the 11B/90B Vision pair, Mistral-7B-v0.3, Qwen2.5-7B, the Qwen2.5-VL family,
Qwen3-VL-32B, gemma-3-27b.

**The base image is chosen by the spec, not by recency.** Each spec names the tt-metal and vLLM commits it
was validated on for one (model, device) pair. In release 0.10.1 on Blackhole:

| Device | LLM specs | tt-metal / vLLM commits → image tag |
|---|---|---|
| P150 (one p150a/b) | Llama-3.1-8B(-Instruct), EXPERIMENTAL | `55fd115` / `aa4ae1e` → `0.10.0-55fd115-aa4ae1e` (the default here) |
| P150X4 (Quietbox) | Llama-3.1-8B COMPLETE; Llama-3.1/3.3-70B FUNCTIONAL | `55fd115` / `aa4ae1e` |
| P150X8, P300, P300X2 | Llama-3.1-8B, Llama-3.x-70B, Qwen3-32B | `555f240` / `22be241` → `0.10.1-555f240-22be241` |

Qwen2.5-7B is specified only for Wormhole (N300, N150X4), so it cannot be the Blackhole proof model.
