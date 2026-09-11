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
  `sha` + `X.Y.Z` + `X.Y` + tag, registry cache.

**No floating `latest`, in either lane** (ARCHITECTURE §7): a moving tag is the rolling upstream tag we
forbid everywhere else, and consumers pin a digest anyway (the chart's `engineImages`, re-pinned per
rollout by ADR-017). **A dirty tree cannot be pushed** — `ALLOW_DIRTY=1` if you must, and the tag then
says `-dirty`.

**Multi-arch.** Only the leader and the CPU worker have a base that exists for arm64 (a kind cluster on
Apple silicon pulls it); the GPU bases are amd64-only. `--multi` builds `<tag>-amd64` and `<tag>-arm64`,
composes the index under `<tag>` from a name computed once, and **reads it back** to prove both
platforms are there — a mistyped target once published two-arch indexes to ghcr packages called
`opod-leaderatest` / `opod-worker-llamacpp-cpuatest` while the real tags stayed single-arch for two days.

The llama.cpp CUDA **RPC pair** (`/opt/llama-rpc`, ~60 min of nvcc) is built **once per `LLAMA_RELEASE`**
as `ghcr.io/opod-io/llama-rpc-cuda:<rel>` (`--target rpc-export`; CI dispatch with `rpc=true`, or
`images/build.sh rpc-build` on an amd64 box) and substituted for the `rpc-cuda` stage in both lanes.
Bootstrap it from the last CI-built worker without compiling: `images/build.sh rpc-bootstrap`.

| Image | Dockerfile | BASE |
|---|---|---|
| `opod-leader` | `leader/` | debian:bookworm-slim |
| `opod-worker-llamacpp-nvidia` | `worker-llamacpp/` | ghcr.io/ggml-org/llama.cpp:full-cuda |
| `opod-worker-llamacpp-amd` | `worker-llamacpp/` | ghcr.io/ggml-org/llama.cpp:full-rocm |
| `opod-worker-llamacpp-cpu` | `worker-llamacpp/` | ghcr.io/ggml-org/llama.cpp:full (CPU; dev clusters, kind CI, a GPU-less node the plan names — the control plane's vendor "none") |
| `opod-worker-llamacpp-intel` | `worker-llamacpp/` | ghcr.io/ggml-org/llama.cpp:full-intel (SYCL/oneAPI; Arc A770 / Pro B60 with the i915/xe driver — unproven until an Intel node has a driver) |
| `opod-worker-vllm-nvidia` | `worker-vllm/` | vllm/vllm-openai:v0.27.1 |
| `opod-worker-vllm-amd` | `worker-vllm/` | rocm/vllm — **one build per GPU family, not one image**: `…_rdna_…` for Radeon / Radeon Pro (gfx11xx, the default) and `…_cdna_…` for Instinct MI2xx/MI3xx (gfx9xx). Neither runs the other's kernels. Override with `VLLM_AMD_BASE=`. ~25 GB base; not built by default. |

**ghcr visibility.** Worker and leader packages must be **public** — nodes pull them anonymously and no
cluster of ours holds a registry token (the design-partner cell has none by design). A package first
published from this Mac starts **private**; the OCI `image.source` label links it to `opod-io/opod-core`,
but linking does not change visibility, and no REST API can. It is a one-click flip in the package's
settings page. As of 2026-09-10 `opod-worker-llamacpp-amd` and `opod-worker-llamacpp-intel` are still
private — an endpoint on those vendors will `ImagePullBackOff` until they are flipped.

Raw build (any lane): `docker buildx build --platform linux/amd64 -f images/worker-llamacpp/Dockerfile --build-arg BASE=… -t opod-worker-llamacpp-nvidia .`
Env contract: see `entrypoint.sh` header. Weights persist under `/data/models` (mount it).
