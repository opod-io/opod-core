# Scaling signals — what a leader publishes, and how an external scaler should read it

`opod` does not scale anything. It runs a leader and its workers, and it publishes the numbers an
external scaler needs so that something else — a script, an orchestrator's autoscaler, an operator's own
tooling — can decide how many workers should exist. This document is the contract for that reader.

## `GET /loadz`

Unauthenticated, probe-grade, on the leader's API port. One JSON object, aggregated over the workers that
hold the leader's model:

```json
{
  "plan_revision": 7,
  "plan_model": "qwen2.5-7b-instruct",
  "in_flight": 9,
  "queue_depth": 3,
  "kv_used_pct": 71,
  "tokens_per_s": 412.5,
  "prefix_hit_pct": 38,
  "rpm_1m": 240,
  "unavailable_1m": 0,
  "last_request_unix": 1758300000,
  "workers": 3,
  "reporting": 3,
  "ts": 1758300001
}
```

| Field | Meaning | How a scaler should use it |
|---|---|---|
| `queue_depth` | requests accepted and waiting, summed over workers | **the leading signal** — anything above zero means demand already exceeds capacity |
| `kv_used_pct` | the busiest worker's KV-cache occupancy | **the ceiling** — grow before the cache fills, because a full cache means refused or preempted requests |
| `in_flight` | requests being served right now | the smooth, proportional signal for ordinary growth |
| `unavailable_1m` | responses the leader refused in the last minute because no worker was ready (`503 waking`) | **the wake signal** — how an endpoint scaled to zero asks to come back |
| `rpm_1m` | requests per minute | keeps one worker alive under a trickle |
| `workers` / `reporting` | workers known / whose sample is under 30 s old | `reporting < workers` means the numbers are partial; prefer no decision over a wrong one |
| `tokens_per_s`, `prefix_hit_pct` | throughput and prefix-cache hits | reporting, not scaling |

CPU and memory are **not** useful signals for a GPU worker and the leader does not publish them.

### Rules for a reader

1. **Poll, do not stream.** Ten seconds is a sensible interval; the numbers are one-minute aggregates or
   instantaneous gauges, never cumulative counters you must difference.
2. **An unreadable `/loadz` is not a reason to act.** Make no decision rather than a wrong one: a leader
   restarting for a few seconds must not cause a scale-in.
3. **Never scale in while `queue_depth > 0`**, and give scale-in a stabilisation window — a GPU worker is
   expensive to lose over a dip in traffic.
4. **Treat `unavailable_1m > 0` as activation**, separate from the value you scale on. It is the only
   signal that exists when the worker count is zero.

## What the leader does *not* decide

The leader routes requests across the workers it has, using these same numbers. It never creates or
removes a worker, knows nothing about the platform underneath it, and keeps serving from its mounted plan
and auth files whatever happens to whatever is scaling it.

## Owed (not shipped)

| | What | Why |
|---|---|---|
| **Probe port** | `/loadz` on a plain-HTTP probe port, or a documented CA, so a scraper does not have to skip verification when the leader serves TLS with a per-endpoint self-signed certificate | a scaling decision should not depend on disabled certificate checks |
| **Engine liveness in the heartbeat** | a worker whose engine is crash-looping still reports as a worker | a reader counting workers overstates capacity and scales out too late |
| **Uneven split flags from the plan** | a pipeline layer partition, and a llama.cpp tensor split, settable per worker | lets one model use cards of different sizes instead of being limited by the smallest |
