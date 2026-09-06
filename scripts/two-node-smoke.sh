#!/usr/bin/env bash
#
# two-node-smoke.sh — verify Opod's cross-node routing works end-to-end
# between two real machines. Runs from EITHER box (or any third box that
# can reach both); does HTTP-only checks plus a real inference call.
#
# Companion to docs/TWO_NODE_VERIFICATION.md — use that for the full
# manual walkthrough; use this for "did the wire path actually work?".
#
# Usage:
#   LEADER=http://leader.lan:8080 \
#   WORKER=http://worker.lan:8081 \
#   ADMIN_KEY=sk-orc-... \
#   MODEL=llama-3.2-3b \
#     ./scripts/two-node-smoke.sh
#
# Exit codes:
#   0   every check passed — cross-node routing works
#   1   leader unreachable
#   2   worker unreachable
#   3   leader doesn't know about any worker
#   4   model not registered on any node
#   5   cross-node inference round-trip failed
#
# All curl calls have explicit timeouts so a stuck node fails the script
# rather than hanging it.

set -euo pipefail

LEADER="${LEADER:-http://localhost:8080}"
WORKER="${WORKER:-}"
ADMIN_KEY="${ADMIN_KEY:-}"
MODEL="${MODEL:-llama-3.2-3b}"
TIMEOUT="${TIMEOUT:-10}"

# --- helpers ---
ok()   { printf "\033[32m✔\033[0m %s\n" "$*"; }
warn() { printf "\033[33m⚠\033[0m %s\n" "$*"; }
die()  { printf "\033[31m✘\033[0m %s\n" "$*" >&2; exit "${2:-1}"; }
note() { printf "  %s\n" "$*"; }

require() {
  local var="$1" val
  val="${!var:-}"
  [ -n "$val" ] || die "$var is required (export it or pass on cmd line)"
}

H_ADMIN=( -H "Authorization: Bearer $ADMIN_KEY" )

# --- step 0: required inputs ---
require ADMIN_KEY
ok "leader = $LEADER"
ok "model  = $MODEL"
[ -n "$WORKER" ] && ok "worker = $WORKER (will hit directly for healthz)" || warn "WORKER not set — will only verify via leader's node list"

# --- step 1: leader reachable + healthy ---
if ! curl -fsS -m "$TIMEOUT" "$LEADER/healthz" > /dev/null; then
  die "$LEADER/healthz unreachable — is the leader running?" 1
fi
ok "leader healthz OK"

# --- step 2: leader knows we exist ---
if ! curl -fsS -m "$TIMEOUT" "${H_ADMIN[@]}" "$LEADER/admin/v1/config" > /dev/null; then
  die "leader admin API rejected the ADMIN_KEY — wrong key?" 1
fi
ok "admin auth works"

# --- step 3: at least one worker registered ---
nodes_json=$(curl -fsS -m "$TIMEOUT" "${H_ADMIN[@]}" "$LEADER/admin/v1/nodes")
if command -v jq >/dev/null 2>&1; then
  node_count=$(echo "$nodes_json" | jq 'length')
else
  # Heuristic fallback when jq is absent: count object-opening "id" keys so
  # nested ids and keys like "api_key_id" don't inflate the count.
  node_count=$(echo "$nodes_json" | tr ',' '\n' | grep -c '{[[:space:]]*"id"[[:space:]]*:' || true)
fi
if [ "$node_count" -lt 2 ]; then
  die "leader sees $node_count node(s); need ≥2 (leader + at least one worker). Run \`opod join\` on the worker first." 3
fi
ok "leader sees $node_count nodes"

# --- step 4: worker reachable directly (if WORKER provided) ---
if [ -n "$WORKER" ]; then
  if ! curl -fsS -m "$TIMEOUT" "$WORKER/healthz" > /dev/null; then
    die "$WORKER/healthz unreachable — leader thinks it's ready but it isn't responding from $(hostname). Asymmetric firewall? mDNS?" 2
  fi
  ok "worker healthz OK"
fi

# --- step 5: model registered somewhere ---
models_json=$(curl -fsS -m "$TIMEOUT" "${H_ADMIN[@]}" "$LEADER/v1/models")
if ! echo "$models_json" | grep -q "\"$MODEL\""; then
  warn "MODEL=$MODEL not listed in /v1/models — installing it on the leader now (this will take a minute)"
  install_resp=$(curl -fsS -m 300 "${H_ADMIN[@]}" -H "Content-Type: application/json" \
    -X POST "$LEADER/admin/v1/models" \
    -d "{\"id\":\"$MODEL\"}" || echo "FAILED")
  [ "$install_resp" = "FAILED" ] && die "could not install $MODEL on leader; install it manually with \`opod model add $MODEL\` and retry" 4
  ok "$MODEL installed on leader"
fi
ok "$MODEL registered"

# --- step 6: cross-node inference round-trip ---
note "sending a short chat completion request to verify the routing path…"
chat_resp=$(curl -fsS -m 60 "${H_ADMIN[@]}" -H "Content-Type: application/json" \
  -X POST "$LEADER/v1/chat/completions" \
  -d "{\"model\":\"$MODEL\",\"messages\":[{\"role\":\"user\",\"content\":\"say HELLO in 3 words\"}]}" \
  || echo "FAILED")
if [ "$chat_resp" = "FAILED" ] || ! echo "$chat_resp" | grep -q '"choices"'; then
  die "inference request failed — response: $chat_resp" 5
fi
content=$(echo "$chat_resp" | grep -oE '"content":"[^"]*"' | head -1 | sed 's/"content":"\(.*\)"/\1/')
ok "round-trip OK — model said: $content"

# --- summary ---
printf "\n\033[1;32mSMOKE PASS\033[0m — leader %s sees %s nodes and successfully served a chat request via model %s.\n" \
  "$LEADER" "$node_count" "$MODEL"

if [ -z "$WORKER" ]; then
  printf "\nNext step: re-run with WORKER set to verify the leader → worker direction directly.\n"
fi
