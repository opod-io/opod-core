# Two-node verification — 10-minute manual checklist

Opod's cross-node routing has automated coverage via `internal/leader/two_node_e2e_test.go` (an in-process register / heartbeat / placement-reconciliation simulation). This document is for the **real-hardware** verification: two physical machines, one network, end-to-end inference.

Walking the checklist on your own hardware confirms the path holds for your setup; nothing in this repo claims it for your machines.

## tl;dr — run the smoke script

If you've already got a leader + worker running and just want to confirm the wire path works, skip to:

```bash
LEADER=http://leader.lan:8080 \
WORKER=http://worker.lan:8081 \
ADMIN_KEY=sk-orc-... \
MODEL=llama-3.2-3b \
  ./scripts/two-node-smoke.sh
```

Six checks in under 30 seconds: leader healthz, admin auth, node count ≥ 2, worker healthz (direct), model registered, real chat round-trip. Exit codes 1–5 each map to a specific failure mode (asymmetric firewall, missing model, etc.) so CI / on-call playbooks can branch on them.

The full walkthrough below is for first-time setup or when the smoke script fails and you need to bisect.

## Pre-flight

- Two machines on the same LAN. macOS / Linux either way. Apple Silicon + Linux+NVIDIA is the most interesting mix.
- Ollama installed and running on **both** machines (`ollama serve` reachable at `http://127.0.0.1:11434` on each).
- Opod built from main (`go build ./cmd/opod`) or the latest signed release on both machines.
- Decide which is the **leader**. Pick the box you find it convenient to run the CLI on — it doesn't need to be the strongest, and it doesn't need a GPU: a leader with no local engine is router-only and `/readyz` says so.

## The walkthrough

### Step 1 — boot the leader

On the leader:

```bash
./opod up
```

You should see:

- a version line: `opod <version> (darwin/arm64, go1.25)` — whatever you installed
- Auto-detected engine + default model line
- **Admin key printed once** — copy it. You'll need it.
- "✔ Listening on :8080"

In a second terminal on the leader:

```bash
./opod model add llama-3.2-3b      # small smoke-test model
./opod status                       # should show 1 node, 1 model loaded
```

### Step 2 — mint a worker token

On the leader:

```bash
./opod token create --node
```

The output should look like:

```
✔ created node-join (id=k_…, scope=node)

  Key (shown once — store it now):
    sk-orc-XXXXXXXX
```

It prints the key only; the join command is yours to assemble from the leader's address — one the
worker can reach, not `localhost` — and that key.

### Step 3 — join the worker

On the **second** machine, join with the leader's address and the key from step 2. Keep the quotes — an
unquoted `?` is a glob in zsh:

```bash
./opod join "http://192.0.2.42:8080?token=sk-orc-XXXXXXXX"
```

You should see:

- "✔ Registered with leader at http://192.0.2.42:8080"
- "✔ Worker HTTP server listening on :8081"
- Periodic "heartbeat OK" lines

### Step 4 — verify from the leader

Back on the leader:

```bash
./opod node ls
# Expect (the columns the CLI actually prints):
# ID             HOSTNAME             OS/ARCH      ADDRESS                STATE      LAST HB
# local          leader.local         darwin/arm64 127.0.0.1:8080         ready      2s ago
# n_def456       worker.local         linux/amd64  192.0.2.50:8081        ready      3s ago
```

```bash
./opod model add llama-3.2-3b     # if already installed on leader, will be a no-op
# On the worker side this triggers an Ollama pull. Wait until it appears in `opod node show n_def456`.
```

Wait ~30s for the worker's heartbeat to carry the new loaded model. Then:

```bash
./opod node show n_def456
# Should list llama-3.2-3b under "loaded_models"
```

### Step 5 — exercise cross-node routing

Send a chat request to the leader, asking for a model only the worker has loaded. The router should proxy to the worker transparently.

Easiest path:

1. On the worker only, install a second model that the leader doesn't have:
   ```bash
   ollama pull qwen2.5:0.5b
   ```
2. Wait ~10s for the heartbeat.
3. From any machine (use the admin key from step 1):
   ```bash
   curl http://<leader-ip>:8080/v1/chat/completions \
     -H "Authorization: Bearer <admin-key>" \
     -H "Content-Type: application/json" \
     -d '{"model":"qwen2.5:0.5b","messages":[{"role":"user","content":"say hi in 5 words"}]}'
   ```

You should get a normal OpenAI-shape response. If you `tail -f` the worker's stderr while the request is in flight, you should see the leader's **signed** `POST /v1/chat/completions` arriving at the worker's own HTTP server (`X-Opod-Auth`, not a bearer token). `/v1/process/*` is a different thing — that is how shard parts are launched, and no ordinary request uses it.

### Step 6 — kill the worker, watch graceful degradation

On the worker, `Ctrl-C` the `opod join` process. Then on the leader:

- The worker sends a **goodbye** on its way out (a final heartbeat declaring its engine `stopped`), so it should leave rotation within a second or two rather than at the heartbeat bound. If the goodbye is lost — the leader was restarting, the process was `kill -9`'d — wait for `router.heartbeat_max_age_seconds` instead (**30 s** by default; 60 s if you set it to 0).
- `./opod node ls` should show the worker's state as **`lost`** — not `down`, which is not a state this binary has. A worker that keeps heartbeating while its engine stays silent reads `engine-silent` instead.
- A request for `qwen2.5:0.5b` should now answer `503` with `Retry-After` and a message that says which it is (stopped heartbeating), rather than hanging or blaming the leader's own engine. Other models on the same leader keep serving.

If the catalog entry has a `fallback:` chain declared, the router will retry the chain transparently. Verify by tailing the leader's stderr — you should see a `fallback` log line.

### Step 7 — bring the worker back

On the worker, re-run the `opod join` command. After one heartbeat:

- `./opod node ls` shows state `ready` again
- The model becomes available again
- The pending request from step 6 (if you retry it) succeeds

## What to do if something breaks

| Symptom | Likely cause | Fix |
|---|---|---|
| Join command says "connection refused" | Leader not reachable on that address (bound elsewhere, or a firewall) | Check `OPOD_LISTEN` (`:8080` binds every interface) and that the worker can reach `http://<lan-ip>:8080/healthz`. `OPOD_EXTERNAL_URL` only changes the URL the leader *prints* and embeds in snippets |
| Join succeeds but no heartbeats | Worker can reach leader, leader can't reach worker (asymmetric firewall) | Worker's `Address:port` must be reachable from the leader. Check macOS Firewall / Linux iptables |
| Worker registers but loaded_models is empty | Ollama isn't running on the worker | `pgrep -f "ollama serve"` then `curl http://127.0.0.1:11434/api/tags` |
| Cross-node request hangs, or answers 503 `worker_unreachable` | The worker's HTTP server isn't reachable from the leader | `curl http://<worker-ip>:8081/healthz` from the leader. A connection is given 3 s, then the leader re-picks another worker for the same model; with none left the answer is `503` + `Retry-After` |
| Heartbeat returns 401 | Token revoked or wrong | Re-run `opod token create --node` on the leader, restart `opod join` on the worker |
| Clock skew warning | NTP not synced | `sudo sntp -sS time.apple.com` (macOS) / `sudo systemctl restart systemd-timesyncd` (Linux) |

## Reporting back

Once you've completed steps 1–7 successfully on real hardware:

1. Open an issue (or PR against this doc) noting the run, listing the two machines you used (OS + arch + RAM) so we capture the actual verified matrix.

If something *did* break, open an issue with the relevant transcript and the `opod doctor` output from both machines.
