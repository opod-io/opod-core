# opod worker — vLLM on the OFFICIAL vllm image.
#   nvidia: BASE=vllm/vllm-openai:v0.27.1    amd: BASE=rocm/vllm:latest
ARG BASE=vllm/vllm-openai:v0.27.1@sha256:0a51ea5b4ae2dc5d81890e5173f54203d2a3ae0cfffe51b8fd2afd4391bfd967
# Stage `opod` = the binary. CI compiles it here; the LOCAL lane (images/build.sh)
# overrides this stage with a prebuilt cross-compiled binary via
# `--build-context opod=dist/linux-amd64` so nothing compiles under emulation.
FROM --platform=$BUILDPLATFORM golang:1.26@sha256:3c3e25a4da13fd0478eed2df1eb35a0e667094a7124d3993a6a1d30f71c17e79 AS opod
WORKDIR /src
COPY go.mod go.sum ./
RUN go mod download
COPY . .
ARG VERSION=dev
# Cross-compile from the BUILD platform: a two-arch index must never compile Go
# under emulation (the arm64 half of the leader image is what a kind cluster on
# Apple silicon pulls).
ARG TARGETOS=linux
ARG TARGETARCH=amd64
RUN CGO_ENABLED=0 GOOS=${TARGETOS} GOARCH=${TARGETARCH} go build -trimpath -ldflags "-s -w -X main.version=${VERSION}" -o /opod ./cmd/opod

FROM ${BASE}
RUN (apt-get update && apt-get install -y --no-install-recommends ca-certificates curl bash && rm -rf /var/lib/apt/lists/*) || true
# Ray is what vLLM runs a MULTI-NODE gang over (--distributed-executor-backend
# ray: pipeline stages on different machines, ADR-040 R4). The upstream image
# stopped carrying it (v0.27.1: "No module named 'ray'", design-partner cell
# 2026-09-14), so the worker adds the pinned wheel; a single-node worker never
# imports it. The AMD base keeps its own.
ARG RAY_VERSION=2.58.0
RUN python3 -c "import ray" 2>/dev/null || pip install --no-cache-dir "ray[default]==${RAY_VERSION}"
COPY --from=opod /opod /usr/local/bin/opod
COPY images/entrypoint.sh /usr/local/bin/entrypoint.sh
# Intel's XPU base sets up oneAPI in its own ENTRYPOINT (`source setvars.sh
# --force && exec "$@"`), which the worker's entrypoint replaces: without it
# torch cannot find libccl and vLLM never starts. Record the script for
# entrypoint.sh (base_env) when this base has one; every other base records
# nothing and starts exactly as before.
RUN if [ -r /opt/intel/oneapi/setvars.sh ]; then mkdir -p /usr/local/share/opod && echo "/opt/intel/oneapi/setvars.sh --force" > /usr/local/share/opod/base-env; fi
ENV OPOD_ROLE=worker OPOD_ENGINE=vllm OPOD_DATA_DIR=/var/lib/opod OPOD_MODELS_DIR=/data/models OPOD_NO_UPDATE_CHECK=1 VLLM_WORKER_MULTIPROC_METHOD=spawn
EXPOSE 8081
ENTRYPOINT ["/usr/local/bin/entrypoint.sh"]
