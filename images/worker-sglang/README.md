# opod-worker-sglang

SGLang, wrapped the same way as every other worker: the official image brings the
engine, we add `opod` and the shared entrypoint. The worker runs `opod join`, and
the agent starts `python3 -m sglang.launch_server` when the leader asks it to
hold a model (`internal/agent/server.go`, `launchSGLang`).

Why an operator would choose it: RadixAttention, a prefix cache that survives
across requests. That is also why the driver reads `sglang:cache_hit_rate` —
a fleet where the hit rate collapses has lost the reason it is running SGLang.

```bash
# NVIDIA
images/build.sh --push sglang-nvidia
# AMD (ROCm). The default base is digest-pinned in build.sh (SGLANG_AMD_BASE);
# override it for another one — `BASE=` is NOT the variable for this image:
SGLANG_AMD_BASE=lmsysorg/sglang:v0.5.19-rocm700-mi35x images/build.sh --push sglang-amd
```

**AMD is Instinct only.** Upstream publishes `…-mi30x` (MI200/MI300) and `…-mi35x` (MI350)
and no RDNA build at all, so a Radeon card serves through `opod-worker-llamacpp-amd` or
`opod-worker-vllm-amd` instead — not through this one.

**A multi-node gang works** (feature `shard_sglang`): a catalog entry with
`sharding.engine: sglang` makes the leader launch the same `sglang.launch_server` on every
part with `--dist-init-addr <rank0>`, `--nnodes`, `--node-rank`, `--tp-size` over the whole
gang and `--pp-size` for stages, and **rank 0 serves the group's API** — it is the
coordinator row. That is a tensor group crossing a node by construction, which ADR-068
allows and logs loudly (on plain ethernet, every all-reduce of every token is on the wire)
rather than refusing. `images.yaml` cannot say so in its `gang` column yet: the field takes
only `rpc` and `ray`, so these rows read blank in `opod image ls`.

Not supported: the sleep tier. SGLang has no sleep mode, so an endpoint that
asks for one is refused at plan time rather than parked and never woken.
