#!/usr/bin/env bash
# images/build.sh — the LOCAL image lane (zero GitHub Actions minutes).
#
# Cross-compiles `opod` for linux/amd64 on this machine, then builds every image
# with buildx on top of the official upstream base and pushes ONLY the thin opod
# layers to ghcr.io (base layers are already there). Nothing compiles under
# emulation: the Go build is native, the llama.cpp CUDA RPC pair comes from the
# prebuilt ghcr.io/opod-io/llama-rpc-cuda:<LLAMA_RELEASE> image.
#
#   images/build.sh [--push] [--multi] [--dry-run] [--tag T] [image ...]
#       images: leader llamacpp-nvidia llamacpp-amd llamacpp-cpu llamacpp-intel vllm-nvidia
#               vllm-amd sglang-nvidia sglang-amd   (default: everything but vllm-amd and the
#               sglang pair, whose bases are ~25 GB and whose hardware proof is still owed)
#       ARCH=arm64 builds linux/arm64 instead (a local kind on Apple silicon; leader + llamacpp-cpu only —
#       the GPU bases are amd64). Tag gets a "-arm64" suffix so it never shadows the amd64 image.
#       --multi builds BOTH arches and publishes the two-arch index under the plain tag
#               (leader + llamacpp-cpu only; --push required, since an index lives in the registry).
#       tag:    dev-<short sha> unless --tag. There is no floating tag: a moving `latest`
#               is the rolling upstream tag ARCHITECTURE §7 forbids, and the CP re-pins a
#               digest per rollout anyway (ADR-017).
#       A dirty working tree cannot be pushed (ALLOW_DIRTY=1 to override): a published image
#               whose tag names a commit that does not contain it is unreproducible.
#   images/build.sh prune
#       keep the Mac lean: drop local opod-*:dev-* tags (they live in ghcr once pushed) and trim the
#       buildx cache to BUILD_KEEP (default 60GB: enough to hold every upstream base so nothing re-pulls).
#   images/build.sh rpc-bootstrap
#       one-time: publish llama-rpc-cuda:<LLAMA_RELEASE> by lifting /opt/llama-rpc out of
#       a published worker image named by RPC_SOURCE (no compile). Re-run only after bumping LLAMA_RELEASE,
#       and then it must be a real build:  images/build.sh rpc-build   (~60 min, needs amd64+nvcc → CI dispatch)
#
# One-time login (token stays in the Docker credential store, never in the repo):
#   gh auth refresh -s write:packages && gh auth token | docker login ghcr.io -u "$(gh api user -q .login)" --password-stdin
set -euo pipefail
cd "$(dirname "$0")/.."

REG=${REG:-ghcr.io/opod-io}
SOURCE_URL=${SOURCE_URL:-https://github.com/opod-io/opod-core}
# ROCm vLLM ships a build per GPU family, not one image: RDNA (Radeon / Radeon
# Pro, gfx11xx) and CDNA (Instinct MI2xx/MI3xx, gfx9xx) have different kernels
# and neither runs the other's. The default is RDNA because that is the AMD
# hardware we have; a CDNA fleet builds the same target with
#   VLLM_AMD_BASE=rocm/vllm:rocm7.14.1_cdna_ubuntu24.04_py3.14_pytorch_2.11_vllm_0.23.0 \
#   images/build.sh --push --tag <t> vllm-amd
# and points chart engineImages.vllmAmd at it. ~25 GB base pull either way.
VLLM_AMD_BASE=${VLLM_AMD_BASE:-rocm/vllm:rocm7.14.1_rdna_ubuntu24.04_py3.14_pytorch_2.11_vllm_0.23.0}
ARCH=${ARCH:-amd64}
PLATFORM=linux/$ARCH
LLAMA_RELEASE=$(sed -nE 's/^ARG LLAMA_RELEASE=(.*)$/\1/p' images/worker-llamacpp/Dockerfile.rpc-cuda)
RPC_IMAGE=${RPC_IMAGE:-$REG/llama-rpc-cuda:$LLAMA_RELEASE}

log() { printf '\033[1;34m▶ %s\033[0m\n' "$*" >&2; }
die() { printf '\033[1;31m✘ %s\033[0m\n' "$*" >&2; exit 1; }

# name  dockerfile  base  arches
# arches is the platforms the BASE exists for: the GPU bases are amd64-only, so
# only the leader and the CPU worker can carry a two-arch index (kind on Apple
# silicon pulls the arm64 half).
spec() {
  case "$1" in
    leader)          echo "opod-leader images/leader/Dockerfile - amd64,arm64" ;;
    llamacpp-nvidia) echo "opod-worker-llamacpp-nvidia images/worker-llamacpp/Dockerfile.rpc-cuda ghcr.io/ggml-org/llama.cpp:full-cuda amd64" ;;
    llamacpp-amd)    echo "opod-worker-llamacpp-amd images/worker-llamacpp/Dockerfile ghcr.io/ggml-org/llama.cpp:full-rocm amd64" ;;
    llamacpp-cpu)    echo "opod-worker-llamacpp-cpu images/worker-llamacpp/Dockerfile ghcr.io/ggml-org/llama.cpp:full amd64,arm64" ;;
    llamacpp-intel)  echo "opod-worker-llamacpp-intel images/worker-llamacpp/Dockerfile ghcr.io/ggml-org/llama.cpp:full-intel amd64" ;;
    vllm-nvidia)     echo "opod-worker-vllm-nvidia images/worker-vllm/Dockerfile vllm/vllm-openai:v0.27.1 amd64" ;;
    vllm-amd)        echo "opod-worker-vllm-amd images/worker-vllm/Dockerfile $VLLM_AMD_BASE amd64" ;;
    sglang-nvidia)   echo "opod-worker-sglang-nvidia images/worker-sglang/Dockerfile lmsysorg/sglang:v0.5.2-cu126 amd64" ;;
    sglang-amd)      echo "opod-worker-sglang-amd images/worker-sglang/Dockerfile lmsysorg/sglang:v0.5.2-rocm630 amd64" ;;
    *) die "unknown image '$1' (leader|llamacpp-nvidia|llamacpp-amd|llamacpp-cpu|llamacpp-intel|vllm-nvidia|vllm-amd|sglang-nvidia|sglang-amd)" ;;
  esac
}

push=0 multi=0 dry=0 tag="" images=()
while [ $# -gt 0 ]; do
  case "$1" in
    --push) push=1 ;;
    --multi) multi=1 ;;
    --dry-run) dry=1 ;;
    --tag) tag=$2; shift ;;
    -h|--help) sed -n '2,32p' "$0"; exit 0 ;;
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
    src=${RPC_SOURCE:?set RPC_SOURCE to the published worker image to lift /opt/llama-rpc from — there is no floating :latest to guess}
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
# S1: a pushed image must be reproducible from the commit its tag names. A dirty
# tree can still be built and loaded locally; it just cannot reach the registry.
if [ $push = 1 ] && [ -n "$dirty" ] && [ "${ALLOW_DIRTY:-0}" != 1 ]; then
  die "working tree is dirty — commit first, or ALLOW_DIRTY=1 to push an unreproducible image
     $(git status --porcelain | head -5)"
fi
[ -n "$tag" ] || tag="dev-$sha$dirty"
version="$sha$dirty"

# build <arch> <tag> — one image for one platform.
build_one() {
  local arch=$1 t=$2 dist=dist/linux-$1
  local args=(docker buildx build --platform "linux/$arch" -f "$file"
        --build-context "opod=$dist"
        --build-arg "VERSION=$version"
        # The standard OCI provenance labels. ghcr uses image.source to link a
        # package to its repository, which is also what makes a package
        # published from a public repo inherit public pull.
        --label "org.opencontainers.image.source=$SOURCE_URL"
        --label "org.opencontainers.image.revision=$sha"
        --label "org.opencontainers.image.licenses=Apache-2.0"
        -t "$REG/$name:$t")
  [ "$base" != "-" ] && args+=(--build-arg "BASE=$base")
  [ "$img" = llamacpp-nvidia ] && args+=(--build-context "rpc-cuda=docker-image://$RPC_IMAGE")
  [ $push = 1 ] && args+=(--push)
  log "$name:$t (linux/$arch)$( [ $push = 1 ] && echo ' → push' )"
  run "${args[@]}" .
}

compile() {
  local arch=$1
  log "cross-compiling opod ($version) → dist/linux-$arch/opod"
  run env GOOS=linux GOARCH="$arch" CGO_ENABLED=0 go build -trimpath -ldflags "-s -w -X main.version=$version" -o "dist/linux-$arch/opod" ./cmd/opod
}

for img in "${images[@]}"; do
  read -r name file base arches <<<"$(spec "$img")"
  if [ $multi = 1 ]; then
    # The index is composed from per-arch tags and then VERIFIED, because the one
    # thing that can silently go wrong here is publishing under a name nobody
    # reads: a mistyped target once created ghcr packages called
    # "opod-leaderatest" while the real :latest stayed single-arch for two days.
    [ "$arches" = amd64,arm64 ] || die "$img has an amd64-only base — --multi cannot make an index for it"
    [ $push = 1 ] || die "--multi publishes an index in the registry; add --push"
    for a in amd64 arm64; do compile "$a"; build_one "$a" "$tag-$a"; done
    log "index $REG/$name:$tag ← $tag-amd64 + $tag-arm64"
    run docker buildx imagetools create -t "$REG/$name:$tag" "$REG/$name:$tag-amd64" "$REG/$name:$tag-arm64"
    if [ $dry = 0 ]; then
      got=$(docker buildx imagetools inspect "$REG/$name:$tag" | awk '/^ *Platform: */{print $2}' | grep -c '^linux/' || true)
      [ "$got" = 2 ] || die "$REG/$name:$tag is not a two-arch index (found $got linux platforms)"
      log "verified $REG/$name:$tag carries linux/amd64 + linux/arm64"
    fi
  else
    compile "$ARCH"
    build_one "$ARCH" "$tag$( [ "$ARCH" = amd64 ] || echo "-$ARCH" )"
  fi
done

[ $push = 1 ] && log "done. Re-pin on the cell: console → Rollouts → 'Roll out plan…' (or POST /api/v1/endpoints/{id}/rollouts)."
exit 0
