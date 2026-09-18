# opod images

One engine on one accelerator per image, always FROM the official upstream image plus a thin
opod layer (`opod` binary + catalog + `entrypoint.sh`), pushed to `ghcr.io/opod-io/opod-*` and
digest-pinned by consumers. Never build on cluster nodes.

**Two lanes (since 2026-09-05):**
- **Local (default, zero Actions minutes):** `images/build.sh --push [--multi] [image…]` on the Mac.
  Cross-compiles `opod` natively, feeds it to the Dockerfiles' `opod` stage via
  `--build-context opod=dist/linux-<arch>`, and pushes only the thin top layers. Tag `dev-<sha>`.
  Login once with `gh auth refresh -s write:packages && gh auth token | docker login ghcr.io -u
  "$(gh api user -q .login)" --password-stdin`.
- **CI release lane** (`.github/workflows/images.yml`, tags `v*` + manual dispatch): same Dockerfiles,
  `sha` + `X.Y.Z` + `X.Y` + tag, registry cache. It builds every image the chart can reference —
  including the AMD and Intel vLLM workers and the Tenstorrent one, whose bases are 7–30 GB, so those
  jobs free the runner's disk first. A first push of a package the workflow has not created itself is
  refused (`permission_denied`) until that package grants `opod-core` write under *Manage Actions access*:
  linking a package to a repo is not access, and no API sets it.

**No floating `latest`, in either lane** (ARCHITECTURE §7): a moving tag is the rolling upstream tag we
forbid everywhere else, and consumers pin a digest anyway (the chart's `engineImages`, re-pinned per
rollout by ADR-017). **A dirty tree cannot be pushed** — `ALLOW_DIRTY=1` if you must, and the tag then
says `-dirty`.

**Every upstream base is pinned by digest** (`<ref>:<tag>@sha256:…`) — in each Dockerfile's `FROM`/`ARG BASE`,
in `build.sh` and in `images.yml`, the same digest in all three (`cmd/opod/images_drift_test.go` holds both rules).
The tag stays in the reference for the reader; the digest is what is pulled, and for a multi-arch base it is the
index digest. A tag like `full-cuda` moves with every upstream release, so an unpinned build is not reproducible
from its commit. Moving a pin is deliberate: `images/build.sh refresh-bases [--dry-run]` re-resolves every pinned
tag (read-only, `crane` or `docker buildx`, no daemon) and rewrites the three places together — review the diff,
rebuild, commit the pins on their own. An override (`VLLM_AMD_BASE=…`, `--build-arg BASE=…`) should name a digest too.

**Multi-arch.** Only the leader and the CPU worker have a base that exists for arm64 (a kind cluster on
Apple silicon pulls it); the GPU bases are amd64-only. `--multi` builds `<tag>-amd64` and `<tag>-arm64`,
composes the index under `<tag>` from a name computed once, and **reads it back** to prove both
platforms are there — a mistyped target once published two-arch indexes to ghcr packages called
`opod-leaderatest` / `opod-worker-llamacpp-cpuatest` while the real tags stayed single-arch for two days.

The llama.cpp **RPC pairs** (`/opt/llama-rpc`: `rpc-server` + the `llama-server --rpc` coordinator) that need a
compiler are built **once per `LLAMA_RELEASE`** and substituted for the Dockerfile's `rpc-<vendor>` stage in both
lanes: `ghcr.io/opod-io/llama-rpc-cuda:<rel>` (~60 min of nvcc), `llama-rpc-rocm:<rel>` (HIP, Radeon and Instinct
targets in one binary), `llama-rpc-sycl:<rel>` (oneAPI icpx) and `llama-rpc-cpu:<rel>` (plain C++, one half per
architecture). Each is `--target rpc-export` of its Dockerfile, built by CI dispatch or
`images/build.sh rpc-build cuda|rocm|sycl|cpu`; `build.sh` refuses to build a worker whose pair it cannot read
rather than compile it under emulation. Bootstrap one from an already published worker without compiling:
`RPC_SOURCE=<image> images/build.sh rpc-bootstrap [vendor]`.

**The `io.opod.llamacpp.rpc=true` label.** Upstream's llama.cpp images are built without the RPC backend, so the
image name does not say whether a worker can be a part of a gang — and a part on an image without `rpc-server`
fails at process launch. Every worker image whose final stage really carries the pair (`/opt/llama-rpc` plus the
`rpc-server` entry point) states it as an OCI label, set in the Dockerfile next to the `COPY` that makes it true:
today `opod-worker-llamacpp-nvidia`, `-amd`, `-intel` and `-cpu`. No other image has it, and a llama.cpp image built
some other way (upstream's own, a fork without the pair) has no label and should be treated as single-node only.
Read it from the registry without pulling: `crane config <image> | jq '.config.Labels'`, or
`docker buildx imagetools inspect <image> --format '{{json .Image.Config.Labels}}'`. A drift test holds label ↔ pair.

| Image | Dockerfile | BASE |
|---|---|---|
| `opod-leader` | `leader/` | debian:bookworm-slim |
| `opod-worker-llamacpp-nvidia` | `worker-llamacpp/` | ghcr.io/ggml-org/llama.cpp:full-cuda |
| `opod-worker-llamacpp-amd` | `worker-llamacpp/` (`Dockerfile.rpc-rocm`) | ghcr.io/ggml-org/llama.cpp:full-rocm **plus a source-built RPC pair** in `/opt/llama-rpc` from `llama-rpc-rocm:<rel>` (the base is built without `GGML_RPC`). Compiled with upstream's full target list — Instinct (gfx908, gfx90a, gfx942) and Radeon (gfx1030, gfx1100–1102, gfx1150–1151, gfx1200–1201) in one binary — because upstream's ROCm release tarball carries RDNA targets only and compiled GPU code does not fall back. The tarball-lifted pair was proven 2026-09-15 on a 3× RX 7900 XTX (gfx1100) host; the source-built one still owes its run on hardware. |
| `opod-worker-llamacpp-cpu` | `worker-llamacpp/` (`Dockerfile.rpc-cpu`) | ghcr.io/ggml-org/llama.cpp:full (CPU; dev clusters, kind CI, a GPU-less node the plan names — the control plane's vendor "none") **plus a source-built RPC pair** in `/opt/llama-rpc` since 2026-09-14 (the upstream image has no rpc-server and no `--rpc`), so a CPU worker can be a part of a gang — the laptop gang drills (A12, D10) need no GPU. A few minutes of C++ on the build platform, never emulated: the local lane builds arm64 natively on Apple silicon, CI builds amd64 |
| `opod-worker-llamacpp-intel` | `worker-llamacpp/` (`Dockerfile.rpc-sycl`) | ghcr.io/ggml-org/llama.cpp:full-intel (SYCL/oneAPI; Arc A770 / Pro B60 with the i915/xe driver) **plus a source-built RPC pair** from `llama-rpc-sycl:<rel>`: upstream's own Intel recipe (same oneAPI image and level-zero) with `GGML_RPC=ON`, because neither the base nor the sycl release tarball carries an RPC backend. Unproven on hardware until the pair is published and an Intel node runs it |
| `opod-worker-vllm-nvidia` | `worker-vllm/` | vllm/vllm-openai:v0.27.1 |
| `opod-worker-vllm-amd` | `worker-vllm/` | rocm/vllm — **one build per GPU family, not one image**: `…_rdna_…` for Radeon / Radeon Pro (gfx11xx, the default) and `…_cdna_…` for Instinct MI2xx/MI3xx (gfx9xx). Neither runs the other's kernels. Override with `VLLM_AMD_BASE=`. ~25 GB base; not built by default. |
| `opod-worker-sglang-nvidia` | `worker-sglang/` | lmsysorg/sglang CUDA (`v0.5.2-cu126`). The agent runs `sglang.launch_server` on model load, as for any engine core drives. One node per endpoint: core has no multi-node path for SGLang, so the planner refuses a gang |
| `opod-worker-sglang-amd` | `worker-sglang/` | lmsysorg/sglang ROCm — **Instinct only**: the tags are `…-mi30x` (MI200/MI300) and `…-mi35x` (MI350), with no RDNA build at all, so a Radeon card serves through llama.cpp or vLLM instead. Override with `SGLANG_AMD_BASE=` |
| `opod-worker-vllm-intel` | `worker-vllm/` | intel/vllm (XPU: Arc / Flex / Max) — vLLM per silicon is published by different people, and upstream `vllm/vllm-openai` is CUDA-only. Single-card serving until an Intel node proves more: we have run no Ray gang on the XPU backend. Override with `VLLM_INTEL_BASE=` |
| `opod-worker-vllm-tt` | `worker-tt/` | Tenstorrent's `tt-inference-server` vLLM build — **the tag must match the tt-metal and vLLM commits of the model spec being served**, see `worker-tt/README.md`. Serving proven on a Blackhole p150b 2026-09-15; needs `/dev/hugepages-1G` and a mounted model spec, which the control plane's Tenstorrent render supplies. Override with `VLLM_TT_BASE=` |

**ghcr visibility.** Worker and leader packages must be **public** — nodes pull them anonymously and no
cluster of ours holds a registry token (the design-partner cell has none by design). A package first
published from this Mac starts **private**; the OCI `image.source` label links it to `opod-io/opod-core`,
but linking does not change visibility, and no REST API can. It is a one-click flip in the package's
settings page. As of 2026-09-10 `opod-worker-llamacpp-amd` and `opod-worker-llamacpp-intel` are still
private — an endpoint on those vendors will `ImagePullBackOff` until they are flipped.

Raw build (any lane): `docker buildx build --platform linux/amd64 -f images/worker-llamacpp/Dockerfile --build-arg BASE=… -t opod-worker-llamacpp-nvidia .`
Env contract: see `entrypoint.sh` header. Weights persist under `/data/models` (mount it).
