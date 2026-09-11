#!/usr/bin/env bash
# opod container entrypoint — one image, two roles. Env contract (set by the opod control
# plane's executor, or by hand):
#   OPOD_ROLE=leader|worker
#   leader:  OPOD_LISTEN, OPOD_JOIN_TOKEN, OPOD_REQUIRE_KEYS, OPOD_PULL_DEFAULT_MODEL, OPOD_ENGINE (all optional)
#   worker:  OPOD_LEADER_URL (required), OPOD_JOIN_TOKEN (required), OPOD_ENGINE=llamacpp|vllm|sglang,
#            OPOD_LOAD_MODEL=<catalog id> (+ OPOD_LOAD_REPO / OPOD_LOAD_FILE overrides), OPOD_MODELS_DIR,
#            OPOD_ENGINE_FLAGS (json), POD_IP / POD_NAME (Kubernetes downward API)
# Nothing here knows about Kubernetes or the control plane — it is plain opod.
set -euo pipefail
ROLE="${OPOD_ROLE:-leader}"
DATA="${OPOD_DATA_DIR:-/var/lib/opod}"
MODELS="${OPOD_MODELS_DIR:-/data/models}"
CATALOG="${OPOD_CATALOG_DIR:-/usr/local/share/opod/catalog}"
mkdir -p "$DATA" "$MODELS" "$MODELS/hf" "$MODELS/llamacpp"
export OPOD_DATA_DIR="$DATA" OPOD_CATALOG_DIR="$CATALOG" OPOD_NO_UPDATE_CHECK=1
export HF_HOME="${HF_HOME:-$MODELS/hf}" LLAMA_CACHE="${LLAMA_CACHE:-$MODELS/llamacpp}"

# models_dir has no env var in opod — write the config file.
# `opod join` has no --config flag and reads ~/.opod/config.yaml; `opod up` takes --config.
mkdir -p "$HOME/.opod"
for f in "$DATA/config.yaml" "$HOME/.opod/config.yaml"; do
cat > "$f" <<YAML
data_dir: $DATA
storage:
  models_dir: $MODELS
YAML
done

log() { printf '[entrypoint] %s\n' "$*" >&2; }

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
[ -n "${POD_IP:-}" ]   && export OPOD_ADVERTISE_ADDR="${OPOD_ADVERTISE_ADDR:-$POD_IP:8081}"
[ -n "${POD_NAME:-}" ] && export OPOD_NODE_ID="${OPOD_NODE_ID:-n_${POD_NAME}}"

# Wait for the leader (its Service may exist before the pod is ready).
for i in $(seq 1 120); do
  curl -sf -m 3 "$OPOD_LEADER_URL/healthz" >/dev/null 2>&1 && break
  [ $i -eq 120 ] && { log "leader $OPOD_LEADER_URL unreachable"; exit 1; }
  sleep 5
done
log "worker: joining $OPOD_LEADER_URL engine=$ENGINE advertise=${OPOD_ADVERTISE_ADDR:-auto}"
opod join "$OPOD_LEADER_URL?token=$OPOD_JOIN_TOKEN" &
JOIN_PID=$!

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
  PATHARG=""
  if [ -n "$FILE" ] && [ -f "$MODELS/$FILE" ]; then PATHARG="$MODELS/$FILE"; fi   # cached whole file wins
  if [ -n "$PATHARG" ]; then body=$(printf '{"id":"%s","path":"%s"}' "$OPOD_LOAD_MODEL" "$PATHARG")
  else body=$(printf '{"id":"%s","repo":"%s","file":"%s"}' "$OPOD_LOAD_MODEL" "$REPO" "$FILE"); fi
  log "load: $body"
  for i in $(seq 1 5); do
    out=$(curl -s -m 900 -X POST http://${POD_IP:-127.0.0.1}:8081/v1/model/load -H "Authorization: Bearer $OPOD_JOIN_TOKEN" -H 'Content-Type: application/json' -d "$body" || true)
    log "load response: $out"
    echo "$out" | grep -q '"status":"ready"' && break
    sleep 10
  done
fi
wait $JOIN_PID
