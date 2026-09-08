#!/usr/bin/env bash
# images/build.sh — the LOCAL image lane (zero GitHub Actions minutes).
#
# Cross-compiles `opod` for linux/amd64 on this machine, then builds every image
# with buildx on top of the official upstream base and pushes ONLY the thin opod
# layers to ghcr.io (base layers are already there). Nothing compiles under
# emulation: the Go build is native, the llama.cpp CUDA RPC pair comes from the
# prebuilt ghcr.io/opod-io/llama-rpc-cuda:<LLAMA_RELEASE> image.
#
#   images/build.sh [--push] [--latest] [--dry-run] [--tag T] [image ...]
#       images: leader llamacpp-nvidia llamacpp-amd llamacpp-cpu llamacpp-intel vllm-nvidia   (default: all)
#       ARCH=arm64 builds linux/arm64 instead (a local kind on Apple silicon; leader + llamacpp-cpu only —
#       the GPU bases are amd64). Tag gets a "-arm64" suffix so it never shadows the amd64 image.
#       tag:    dev-<short sha>[-dirty] unless --tag; --latest ALSO tags :latest,
#               which is what the CP pulls by default (ADR-017: a rollout re-pins the digest).
#   images/build.sh prune
#       keep the Mac lean: drop local opod-*:dev-* tags (they live in ghcr once pushed) and trim the
#       buildx cache to BUILD_KEEP (default 60GB: enough to hold every upstream base so nothing re-pulls).
#   images/build.sh rpc-bootstrap
#       one-time: publish llama-rpc-cuda:<LLAMA_RELEASE> by lifting /opt/llama-rpc out of
#       the last CI-built worker image (no compile). Re-run only after bumping LLAMA_RELEASE,
#       and then it must be a real build:  images/build.sh rpc-build   (~60 min, needs amd64+nvcc → CI dispatch)
#
# One-time login (token stays in the Docker credential store, never in the repo):
#   gh auth refresh -s write:packages && gh auth token | docker login ghcr.io -u "$(gh api user -q .login)" --password-stdin
set -euo pipefail
cd "$(dirname "$0")/.."

REG=${REG:-ghcr.io/opod-io}
ARCH=${ARCH:-amd64}
PLATFORM=linux/$ARCH
LLAMA_RELEASE=$(sed -nE 's/^ARG LLAMA_RELEASE=(.*)$/\1/p' images/worker-llamacpp/Dockerfile.rpc-cuda)
RPC_IMAGE=${RPC_IMAGE:-$REG/llama-rpc-cuda:$LLAMA_RELEASE}
DIST=dist/linux-$ARCH

log() { printf '\033[1;34m▶ %s\033[0m\n' "$*" >&2; }
die() { printf '\033[1;31m✘ %s\033[0m\n' "$*" >&2; exit 1; }

# name  dockerfile  base
spec() {
  case "$1" in
    leader)          echo "opod-leader images/leader/Dockerfile -" ;;
    llamacpp-nvidia) echo "opod-worker-llamacpp-nvidia images/worker-llamacpp/Dockerfile.rpc-cuda ghcr.io/ggml-org/llama.cpp:full-cuda" ;;
    llamacpp-amd)    echo "opod-worker-llamacpp-amd images/worker-llamacpp/Dockerfile ghcr.io/ggml-org/llama.cpp:full-rocm" ;;
    llamacpp-cpu)    echo "opod-worker-llamacpp-cpu images/worker-llamacpp/Dockerfile ghcr.io/ggml-org/llama.cpp:full" ;;
    llamacpp-intel)  echo "opod-worker-llamacpp-intel images/worker-llamacpp/Dockerfile ghcr.io/ggml-org/llama.cpp:full-intel" ;;
    vllm-nvidia)     echo "opod-worker-vllm-nvidia images/worker-vllm/Dockerfile vllm/vllm-openai:v0.27.1" ;;
    *) die "unknown image '$1' (leader|llamacpp-nvidia|llamacpp-amd|llamacpp-cpu|llamacpp-intel|vllm-nvidia)" ;;
  esac
}

push=0 latest=0 dry=0 tag="" images=()
while [ $# -gt 0 ]; do
  case "$1" in
    --push) push=1 ;;
    --latest) latest=1 ;;
    --dry-run) dry=1 ;;
    --tag) tag=$2; shift ;;
    -h|--help) sed -n '2,22p' "$0"; exit 0 ;;
    rpc-bootstrap|rpc-build|prune) mode=$1 ;;
    -*) die "unknown flag $1" ;;
    *) images+=("$1") ;;
  esac; shift
done
[ ${#images[@]} -eq 0 ] && images=(leader llamacpp-nvidia llamacpp-amd llamacpp-cpu llamacpp-intel vllm-nvidia)

run() { if [ $dry = 1 ]; then printf '  %q' "$@"; echo; else "$@"; fi; }

[ $dry = 1 ] || docker info >/dev/null 2>&1 || die "Docker daemon not running (open -a Docker)"

case "${mode:-}" in
  prune)
    log "removing local dev tags"
    docker images --format '{{.Repository}}:{{.Tag}}' | grep -E "^$REG/opod-[a-z-]+:dev-" | xargs -r docker rmi >/dev/null || true
    log "trimming build cache to ${BUILD_KEEP:-60GB}"
    run docker builder prune -f --keep-storage "${BUILD_KEEP:-60GB}" | tail -1
    docker system df
    exit 0 ;;
  rpc-bootstrap)
    src=${RPC_SOURCE:-$REG/opod-worker-llamacpp-nvidia:latest}
    log "publishing $RPC_IMAGE from $src (/opt/llama-rpc lifted, no compile)"
    run docker buildx build --platform $PLATFORM -t "$RPC_IMAGE" --push - <<DF
FROM scratch
COPY --from=$src /opt/llama-rpc /opt/llama-rpc
DF
    exit 0 ;;
  rpc-build)
    log "REAL rpc build for $LLAMA_RELEASE → $RPC_IMAGE (nvcc; ~60 min on amd64, do NOT run emulated on a Mac)"
    run docker buildx build --platform $PLATFORM -f images/worker-llamacpp/Dockerfile.rpc-cuda --target rpc-export \
      --build-arg LLAMA_RELEASE="$LLAMA_RELEASE" -t "$RPC_IMAGE" --push .
    exit 0 ;;
esac

sha=$(git rev-parse --short HEAD)
dirty=""; git diff --quiet HEAD -- 2>/dev/null || dirty="-dirty"
[ -n "$tag" ] || tag="dev-$sha$dirty"
[ "$ARCH" = amd64 ] || tag="$tag-$ARCH"
version="$sha$dirty"

log "cross-compiling opod ($version) → $DIST/opod"
run env GOOS=linux GOARCH=$ARCH CGO_ENABLED=0 go build -trimpath -ldflags "-s -w -X main.version=$version" -o "$DIST/opod" ./cmd/opod

for img in "${images[@]}"; do
  read -r name file base <<<"$(spec "$img")"
  args=(docker buildx build --platform $PLATFORM -f "$file"
        --build-context "opod=$DIST"
        --build-arg "VERSION=$version"
        -t "$REG/$name:$tag")
  [ "$base" != "-" ] && args+=(--build-arg "BASE=$base")
  [ "$img" = llamacpp-nvidia ] && args+=(--build-context "rpc-cuda=docker-image://$RPC_IMAGE")
  [ $latest = 1 ] && [ "$ARCH" = amd64 ] && args+=(-t "$REG/$name:latest")
  [ $push = 1 ] && args+=(--push)
  log "$name:$tag$( [ $latest = 1 ] && echo ' (+latest)' )$( [ $push = 1 ] && echo ' → push' )"
  run "${args[@]}" .
done

[ $push = 1 ] && log "done. Re-pin on the cell: console → Rollouts → 'Roll out plan…' (or POST /api/v1/endpoints/{id}/rollouts)."
exit 0
