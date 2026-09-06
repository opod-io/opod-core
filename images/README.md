# opod images

One engine on one accelerator per image, always FROM the official upstream image plus a thin
opod layer (`opod` binary + catalog + `entrypoint.sh`), pushed to `ghcr.io/opod-io/opod-*` and
digest-pinned by consumers. Never build on cluster nodes.

**Two lanes (since 2026-09-05):**
- **Local (default, zero Actions minutes):** `images/build.sh --push --latest [image…]` on the Mac.
  Cross-compiles `opod` natively for linux/amd64, feeds it to the Dockerfiles' `opod` stage via
  `--build-context opod=dist/linux-amd64`, and pushes only the thin top layers. Tag `dev-<sha>[-dirty]`
  (+`:latest`, which the CP pulls; a rollout re-pins the digest). Login once with `gh auth refresh -s
  write:packages && gh auth token | docker login ghcr.io -u "$(gh api user -q .login)" --password-stdin`.
- **CI release lane** (`.github/workflows/images.yml`, tags `v*` + manual dispatch; **disabled** while
  the quota recovers — `gh workflow enable images`): same Dockerfiles, `sha`/`latest`/tag tags, registry cache.

The llama.cpp CUDA **RPC pair** (`/opt/llama-rpc`, ~60 min of nvcc) is built **once per `LLAMA_RELEASE`**
as `ghcr.io/opod-io/llama-rpc-cuda:<rel>` (`--target rpc-export`; CI dispatch with `rpc=true`, or
`images/build.sh rpc-build` on an amd64 box) and substituted for the `rpc-cuda` stage in both lanes.
Bootstrap it from the last CI-built worker without compiling: `images/build.sh rpc-bootstrap`.

| Image | Dockerfile | BASE |
|---|---|---|
| `opod-leader` | `leader/` | debian:bookworm-slim |
| `opod-worker-llamacpp-nvidia` | `worker-llamacpp/` | ghcr.io/ggml-org/llama.cpp:full-cuda |
| `opod-worker-llamacpp-amd` | `worker-llamacpp/` | ghcr.io/ggml-org/llama.cpp:full-rocm |
| `opod-worker-vllm-nvidia` | `worker-vllm/` | vllm/vllm-openai:v0.27.1 |
| `opod-worker-vllm-amd` | `worker-vllm/` | rocm/vllm:latest (not built by default) |

Raw build (any lane): `docker buildx build --platform linux/amd64 -f images/worker-llamacpp/Dockerfile --build-arg BASE=… -t opod-worker-llamacpp-nvidia .`
Env contract: see `entrypoint.sh` header. Weights persist under `/data/models` (mount it).
