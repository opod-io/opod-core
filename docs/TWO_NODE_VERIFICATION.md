# Two-node verification — 10-minute manual checklist

Opod's cross-node routing has automated coverage via `internal/controlplane/two_node_e2e_test.go` (an in-process register / heartbeat / placement-reconciliation simulation). This document is for the **real-hardware** verification: two physical machines, one network, end-to-end inference.

The README's Status row already reflects this path (single-node verified end-to-end; real two-machine verification via this walkthrough + the smoke script). Walking the checklist on your own hardware confirms it holds for your setup.

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
- Decide which is the **leader**. Pick the box that's more convenient for you to dashboard at — it doesn't need to be the strongest.

## The walkthrough

### Step 1 — boot the leader

On the leader:

```bash
./opod up
```

You should see:

- "opod 1.11.0 (darwin/arm64, go1.25)" (version line — actual version depends on what you installed)
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
# → prints: sk-orc-... and a one-line `opod join` invocation
```

The output should look like:

```
worker token: sk-orc-XXXXXXXX
join command:  opod join http://<leader-ip>:8080?token=sk-orc-XXXXXXXX
```

If `<leader-ip>` is `localhost` or `127.0.0.1`, override with `OPOD_EXTERNAL_URL`:

```bash
OPOD_EXTERNAL_URL=http://192.168.1.42:8080 ./opod up
```

### Step 3 — join the worker

On the **second** machine, paste the join command from step 2:

```bash
./opod join http://192.168.1.42:8080?token=sk-orc-XXXXXXXX
```

You should see:

- "✔ Registered with leader at http://192.168.1.42:8080"
- "✔ Worker HTTP server listening on :8081"
- Periodic "heartbeat OK" lines

### Step 4 — verify from the leader

Back on the leader:

```bash
./opod node ls
# Expect:
# ID         HOSTNAME             OS       ARCH    RAM   STATE
# n_abc123   leader.local         darwin   arm64   24    ready
# n_def456   worker.local         linux    amd64   32    ready
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

You should get a normal OpenAI-shape response. If you `tail -f` the worker's stderr while the request is in flight, you should see the request hitting the worker's HTTP server (`/v1/process/...` calls from the leader's router).

### Step 6 — kill the worker, watch graceful degradation

On the worker, `Ctrl-C` the `opod join` process. Then on the leader:

- Wait ~15s (3 missed heartbeats).
- `./opod node ls` should show the worker's state change to `down`.
- A request for `qwen2.5:0.5b` should now return a clear error ("no node has this model loaded") rather than hanging.

If the catalog entry has a `fallback:` chain declared, the router will retry the chain transparently. Verify by tailing the leader's stderr — you should see a `fallback` log line.

### Step 7 — bring the worker back

On the worker, re-run the `opod join` command. After one heartbeat:

- `./opod node ls` shows state `ready` again
- The model becomes available again
- The pending request from step 6 (if you retry it) succeeds

## What to do if something breaks

| Symptom | Likely cause | Fix |
|---|---|---|
| Join command says "connection refused" | Leader bound to localhost only | Restart leader with `OPOD_EXTERNAL_URL=http://<lan-ip>:8080 ./opod up` |
| Join succeeds but no heartbeats | Worker can reach leader, leader can't reach worker (asymmetric firewall) | Worker's `Address:port` must be reachable from the leader. Check macOS Firewall / Linux iptables |
| Worker registers but loaded_models is empty | Ollama isn't running on the worker | `pgrep -f "ollama serve"` then `curl http://127.0.0.1:11434/api/tags` |
| Cross-node request hangs forever | Worker's `/v1/process/*` endpoint isn't reachable | `curl http://<worker-ip>:8081/healthz` from the leader |
| Heartbeat returns 401 | Token revoked or wrong | Re-run `opod token create --node` on the leader, restart `opod join` on the worker |
| Clock skew warning | NTP not synced | `sudo sntp -sS time.apple.com` (macOS) / `sudo systemctl restart systemd-timesyncd` (Linux) |

## Reporting back

Once you've completed steps 1–7 successfully on real hardware:

1. Open an issue (or PR against this doc) noting the run, listing the two machines you used (OS + arch + RAM) so we capture the actual verified matrix.

If something *did* break, open an issue with the relevant transcript and the `opod doctor` output from both machines.
