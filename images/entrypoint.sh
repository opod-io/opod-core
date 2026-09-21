#!/usr/bin/env bash
# opod container entrypoint — one image, two roles. Env contract (set by the opod control
# plane's executor, or by hand):
#   OPOD_ROLE=leader|gateway|worker
#   leader:  OPOD_LISTEN, OPOD_JOIN_TOKEN, OPOD_REQUIRE_KEYS, OPOD_PULL_DEFAULT_MODEL, OPOD_ENGINE (all optional)
#   gateway: OPOD_LEADER_URL (required), OPOD_ADMIN_TOKEN (required — the registry and spend reads are
#            admin-keyed), OPOD_GATEWAY_ID (defaults to the hostname), OPOD_LISTEN (optional)
#   worker:  OPOD_LEADER_URL (required), OPOD_JOIN_TOKEN (required), OPOD_ENGINE=llamacpp|vllm|sglang,
#            OPOD_LOAD_MODEL=<catalog id> (+ OPOD_LOAD_REPO / OPOD_LOAD_FILE overrides), OPOD_MODELS_DIR,
#            OPOD_ENGINE_FLAGS (json), POD_IP / POD_NAME (Kubernetes downward API)
# Nothing here knows about Kubernetes or the control plane — it is plain opod.
set -euo pipefail

# A base image whose own ENTRYPOINT prepared the environment before exec'ing its
# command (Intel's XPU images: `source /opt/intel/oneapi/setvars.sh --force`)
# lost that step when this script replaced it, and the engine then dies on its
# first import ("libccl.so.1: cannot open shared object file"). Such an image
# records the script and its arguments at build time (images/worker-vllm); it is
# sourced here, once, for every process this entrypoint starts. Vendor scripts
# are not written for `set -eu`, so both are off while it runs.
base_env() {
  local rec="${OPOD_BASE_ENV_FILE:-/usr/local/share/opod/base-env}" script args
  [ -r "$rec" ] || return 0
  read -r script args < "$rec" || true
  [ -r "$script" ] || { echo "entrypoint: $rec names '$script', which this image does not have" >&2; return 1; }
  set +eu
  # shellcheck disable=SC1090,SC2086
  . "$script" $args >/dev/null
  set -eu
}
base_env

ROLE="${OPOD_ROLE:-leader}"
DATA="${OPOD_DATA_DIR:-/var/lib/opod}"
# A GATEWAY holds no weights: it routes to the leader's workers and loads
# nothing (T11.1). Nothing mounts a models volume for it, and the image root is
# read-only, so the usual /data/models is a crash loop — it was every door of
# the first cell run (2026-09-21): first here (`mkdir: cannot create directory
# '/data': Permission denied`), then one layer deeper, because core creates its
# CONFIGURED models directory whatever the role. So a door's (unused) models
# directory lives under its writable data dir. The path is never read: what a
# door needs is the registry it mirrors and the auth snapshot it is given.
if [ "$ROLE" = gateway ]; then
  MODELS="${OPOD_MODELS_DIR:-$DATA/models}"
else
  MODELS="${OPOD_MODELS_DIR:-/data/models}"
fi
CATALOG="${OPOD_CATALOG_DIR:-/usr/local/share/opod/catalog}"
mkdir -p "$DATA" "$MODELS" "$MODELS/hf" "$MODELS/llamacpp"
# The catalog is embedded in the binary (opod-sdk/catalog, R9.8); the file copy
# this script reads repo/file from is exported once, so no image carries one.
[ -d "$CATALOG" ] || opod catalog export "$CATALOG" >/dev/null 2>&1 || true
export OPOD_DATA_DIR="$DATA" OPOD_CATALOG_DIR="$CATALOG" OPOD_NO_UPDATE_CHECK=1
export HF_HOME="${HF_HOME:-$MODELS/hf}" LLAMA_CACHE="${LLAMA_CACHE:-$MODELS/llamacpp}"

# models_dir has no env var in opod — write the config file.
# `opod join` has no --config flag and reads ~/.opod/config.yaml; `opod up` takes --config.
# A pod running non-root (uid 65532, restricted PSS) has no home: "~" is "/" and unwritable,
# so the data dir stands in for it (the control plane also sets HOME for the leader).
if [ -z "${HOME:-}" ] || [ "$HOME" = "/" ] || [ ! -w "$HOME" ]; then export HOME="$DATA"; fi
mkdir -p "$HOME/.opod"
for f in "$DATA/config.yaml" "$HOME/.opod/config.yaml"; do
cat > "$f" <<YAML
data_dir: $DATA
storage:
  models_dir: $MODELS
YAML
done

log() { printf '[entrypoint] %s\n' "$*" >&2; }

# A DOOR is `opod up` with the role set: it serves /v1, mirrors the leader's
# registry, pushes its usage there and enforces from the spend snapshot
# (T11.1, ADR-063). Same command as a leader — core reads OPOD_ROLE and
# OPOD_LEADER_URL — but it is refused early and loudly without the leader it
# belongs to, so the variable is required here rather than discovered inside.
if [ "$ROLE" = gateway ]; then
  : "${OPOD_LEADER_URL:?OPOD_LEADER_URL is required for a gateway (the leader whose registry it mirrors)}"
  export OPOD_LISTEN="${OPOD_LISTEN:-:8080}" OPOD_PULL_DEFAULT_MODEL=false
  log "gateway: listen=$OPOD_LISTEN leader=$OPOD_LEADER_URL id=${OPOD_GATEWAY_ID:-$(hostname)}"
  exec opod up --config "$DATA/config.yaml" --no-wizard
fi

if [ "$ROLE" = leader ]; then
  export OPOD_LISTEN="${OPOD_LISTEN:-:8080}" OPOD_PULL_DEFAULT_MODEL="${OPOD_PULL_DEFAULT_MODEL:-false}"
  log "leader: listen=$OPOD_LISTEN engine=${OPOD_ENGINE:-none} model=${OPOD_ENDPOINT_MODEL:-*}"
  exec opod up --config "$DATA/config.yaml" --no-wizard
fi

# ---- worker ----
: "${OPOD_LEADER_URL:?OPOD_LEADER_URL is required for a worker}"
: "${OPOD_JOIN_TOKEN:?OPOD_JOIN_TOKEN is required for a worker}"
ENGINE="${OPOD_ENGINE:-llamacpp}"
case "$ENGINE" in
  llamacpp) export OPOD_ENGINE=llamacpp OPOD_LLAMACPP_ENDPOINT="${OPOD_LLAMACPP_ENDPOINT:-http://127.0.0.1:8089}";;
  vllm)     export OPOD_ENGINE=vllm     OPOD_VLLM_ENDPOINT="${OPOD_VLLM_ENDPOINT:-http://127.0.0.1:8000}";;
  sglang)   export OPOD_ENGINE=sglang   OPOD_SGLANG_ENDPOINT="${OPOD_SGLANG_ENDPOINT:-http://127.0.0.1:30000}";;
  *) log "unknown engine $ENGINE"; exit 2;;
esac
# Some vendor images own their engine's launcher: Tenstorrent's vLLM build is
# configured by env and a model spec and starts through its own entrypoint, so
# `vllm serve <model>` — what the agent runs on model load — does not apply.
# OPOD_ENGINE_START lets such an image say how to start its engine; opod then
# just connects to it, which is exactly what the vLLM driver documents ("assumes
# the user runs vLLM; it does not start/stop the process"). Left unset,
# behaviour is unchanged and the agent launches the engine on model load.
if [ -n "${OPOD_ENGINE_START:-}" ]; then
  log "engine: starting vendor launcher: $OPOD_ENGINE_START"
  sh -c "$OPOD_ENGINE_START" >&2 &
  ENGINE_PID=$!
fi

[ -n "${POD_IP:-}" ]   && export OPOD_ADVERTISE_ADDR="${OPOD_ADVERTISE_ADDR:-$POD_IP:8081}"
[ -n "${POD_NAME:-}" ] && export OPOD_NODE_ID="${OPOD_NODE_ID:-n_${POD_NAME}}"

# Wait for the leader (its Service may exist before the pod is ready).
for i in $(seq 1 120); do
  # A TLS leader (R9.5) is trusted through the certificate the control plane mounted (OPOD_LEADER_CA).
  curl -sf -m 3 ${OPOD_LEADER_CA:+--cacert "$OPOD_LEADER_CA"} "$OPOD_LEADER_URL/healthz" >/dev/null 2>&1 && break
  [ $i -eq 120 ] && { log "leader $OPOD_LEADER_URL unreachable"; exit 1; }
  sleep 5
done
log "worker: joining $OPOD_LEADER_URL engine=$ENGINE advertise=${OPOD_ADVERTISE_ADDR:-auto}"
opod join "$OPOD_LEADER_URL?token=$OPOD_JOIN_TOKEN" &
JOIN_PID=$!

# load_body <id> <repo> <file> <models dir> — the body of the worker's own /v1/model/load.
# An UNPINNED model takes the whole file already at the top of the node cache, by path: no second
# download. A PINNED one (OPOD_MODEL_REVISION and/or OPOD_MODEL_SHA256) never does: a file that merely
# has the same NAME is not the version that was asked for, and loading it by path skipped the
# revision directory AND the digest check — a node that had ever served the model unpinned then
# served those bytes under every pinned version. The agent fetches a pinned file into
# <models>/<repo>@<revision>/ itself, verifies it, and reuses it when it is already there.
load_body() {
  if [ -z "${OPOD_MODEL_REVISION:-}" ] && [ -z "${OPOD_MODEL_SHA256:-}" ] && [ -n "$3" ] && [ -f "$4/$3" ]; then
    printf '{"id":"%s","path":"%s"}' "$1" "$4/$3"
  else
    printf '{"id":"%s","repo":"%s","file":"%s"}' "$1" "$2" "$3"
  fi
}

# Warm-load the endpoint's model through the worker's own agent API (same path the leader uses).
if [ -n "${OPOD_LOAD_MODEL:-}" ]; then
  for i in $(seq 1 60); do
    code=$(curl -s -o /dev/null -w '%{http_code}' -m 3 http://${POD_IP:-127.0.0.1}:8081/healthz || true)
    [ "$code" != 000 ] && break; sleep 2
  done
  REPO="${OPOD_LOAD_REPO:-}"; FILE="${OPOD_LOAD_FILE:-}"
  if [ -z "$REPO" ] && [ -f "$CATALOG/$OPOD_LOAD_MODEL.yaml" ]; then
    REPO=$(awk '/^ *repo:/{print $2; exit}' "$CATALOG/$OPOD_LOAD_MODEL.yaml")
    FILE=$(awk '/^ *file:/{print $2; exit}' "$CATALOG/$OPOD_LOAD_MODEL.yaml")
  fi
  body=$(load_body "$OPOD_LOAD_MODEL" "$REPO" "$FILE" "$MODELS")
  log "load: $body"
  for i in $(seq 1 5); do
    out=$(curl -s -m 900 -X POST http://${POD_IP:-127.0.0.1}:8081/v1/model/load -H "Authorization: Bearer $OPOD_JOIN_TOKEN" -H 'Content-Type: application/json' -d "$body" || true)
    log "load response: $out"
    echo "$out" | grep -q '"status":"ready"' && break
    sleep 10
  done
fi
# The worker is the join process; a vendor engine started above dies with the
# container. Reporting the engine's exit is what turns "pod running, nothing
# served" into a restart the supervisor can act on.
if [ -n "${ENGINE_PID:-}" ]; then
  wait -n "$JOIN_PID" "$ENGINE_PID"
  log "worker: join or engine exited — container will restart"
  exit 1
fi
wait $JOIN_PID
