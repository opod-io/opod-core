# Scaling signals — what a leader publishes, and how an external scaler should read it

`opod` does not scale anything. It runs a leader and its workers, and it publishes the numbers an
external scaler needs so that something else — a script, an orchestrator's autoscaler, an operator's own
tooling — can decide how many workers should exist. This document is the contract for that reader.

## `GET /loadz`

Unauthenticated, probe-grade, on the leader's API port — and, when `OPOD_PROBE_LISTEN` is set, on a second
always-plain port beside `/healthz`, `/readyz` and `/metrics` (see *The probe port* below). One JSON object,
aggregated over the workers that hold the leader's model. The typed shape is
`github.com/opod-io/opod-sdk/adminapi.Load`; read that rather than this example when you write a client:

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
  "kv_busy_workers": 1.6,
  "ttft_p50_ms": 180,
  "ttft_p95_ms": 640,
  "engines_unhealthy": 0,
  "engine_issue": "",
  "ts": 1758300001
}
```

| Field | Meaning | How a scaler should use it |
|---|---|---|
| `queue_depth` | requests accepted and waiting, summed over workers | **the leading signal** — anything above zero means demand already exceeds capacity |
| `kv_used_pct` | the busiest worker's KV-cache occupancy | **the ceiling** — grow before the cache fills, because a full cache means refused or preempted requests |
| `in_flight` | requests being served right now | the smooth, proportional signal for ordinary growth |
| `unavailable_1m` | `/v1` requests answered with a 5xx in the last minute — chiefly `503 waking`, refused because no worker was ready | **the wake signal** — how an endpoint scaled to zero asks to come back |
| `rpm_1m` | requests per minute | keeps one worker alive under a trickle |
| `workers` / `reporting` | workers known / whose sample is under 30 s old | `reporting < workers` means the numbers are partial; prefer no decision over a wrong one |
| `kv_busy_workers` | the mean KV-cache use across reporting workers × their count — "how many workers' worth of KV cache is in use"; always present, `0` when nothing reports | the **proportional** form of `kv_used_pct` for a scaler that computes replicas as `ceil(metric ÷ target)`; a maximum over workers can never ask for more than two |
| `tokens_per_s`, `prefix_hit_pct` | throughput and prefix-cache hits | reporting, not scaling |
| `prefix_index` | `{workers_reporting, blocks}`: how many workers report prefix-cache events and how many blocks the leader holds for them (feature `kv_block_events`); absent when none do | reporting, not scaling |
| `ttft_p50_ms`, `ttft_p95_ms` | time to the first usable token over the last minute, **streamed answers only** — nearest-rank over the last 60 s or the last 4096 streamed answers, whichever is fewer; probes (`X-Opod-Probe`) are not counted (feature `loadz_ttft`) | the number a user feels. Always present; `0` means *not measured in the last minute* — never "instant" — because an external scaler reads a missing key as a failed metric and stops scaling down. A good secondary target beside `queue_depth`. It is what the leader streamed: a front door (`--role gateway`) measures its own on its own `/loadz` |
| `engines_unhealthy` | how many workers are holding a card while their engine is **not** serving — crash-looping or stopped. A `stopped` one has already left `workers`; a crash-looping one is counted in both | **do not count a crash-looping worker as capacity.** Those workers heartbeat like any other; a reader that counts them as capacity scales out too late, or not at all. `engine_issue` carries the worst one's own last word |

CPU and memory are **not** useful signals for a GPU worker and the leader does not publish them.

### A sharded endpoint

A gang's parts are rpc-servers: they hold weights and multiply matrices, but run no engine and have no queue,
KV cache or notion of a request. So `kv_used_pct`, `queue_depth`, `tokens_per_s` and `prefix_hit_pct` for a
sharded model come from the gang's **coordinator** — the one process that has all three — scraped through the
driver the router dials it with and cached for the heartbeat cadence (feature `gang_load`). One sample speaks
for the whole gang: `reporting` counts it once. Before this, a sharded endpoint reported `kv_used_pct 0` and
`queue_depth 0` however loaded it was. `workers` still counts a gang through its parts, as the capacity they
are.

### A prover is not a customer

A reader that sends its own request to check the endpoint is alive would move the very numbers it is reading —
and, on an endpoint scaled to zero, wake what the scaler had just parked. Mark such a request
**`X-Opod-Probe: 1`** (feature `probe_header`): it is served like any other request and counted in none of
`in_flight`, `rpm_1m`, `unavailable_1m` or the idle clock.

### The probe port

`OPOD_PROBE_LISTEN=:8081` (or `probe_listen` in `config.yaml`) serves `/healthz`, `/readyz`, `/loadz` and
`/metrics` on a second listener that is **always plain HTTP**, whatever TLS the main one speaks — feature
`probe_port`. It exists because these endpoints are read by machines that are not clients of the endpoint: a
Kubernetes probe, a scrape, a scaler. When the main listener serves a certificate minted per endpoint, every
one of those readers has to be handed the CA — or, in practice, told to skip verification, which is how a
scaling decision ends up resting on a disabled check. Nothing authenticated is routed to that port and its
router is built from scratch, so a new `/admin/v1` route cannot appear there by accident.

### vLLM-named aliases on `/metrics` — a pool member for an inference gateway

Beside the `opod_*` names, `/metrics` carries three gauges under the names a vLLM engine uses (feature
`vllm_metric_aliases`), so a Gateway API inference pool whose endpoint picker reads vLLM's names — llm-d's
default mapping for the `vllm` engine type — can select leaders as pool members with no mapping written for
them. The leader is one pod to that picker; the leader still picks the worker inside.

| Alias | What it is | Aggregate |
|---|---|---|
| `vllm:num_requests_waiting` | requests the endpoint's engines report queued | **sum** over the reporting workers and gang coordinators |
| `vllm:num_requests_running` | requests the leader has in flight that its engines do not report queued | live in-flight − the queue above, floored at 0 |
| `vllm:kv_cache_usage_perc` | KV-cache use, a **fraction 0–1** as vLLM publishes it despite the name | **mean** over the reporting samples — not the maximum `/loadz` carries, because one full worker beside idle ones is an endpoint with room |

Each carries a `model_name` label set to the plan's model. The worker-derived part is recomputed at most once a
second (the samples change on a heartbeat); in-flight is read live. Not published: `vllm:lora_requests_info`
(the picker skips it when absent) and `vllm:cache_config_info` — an endpoint of several workers has no single
block size, and a made-up one would steer prefix scoring wrongly; its absence costs one extract error per poll
on the picker's side. A test fails the build if an alias disappears (ADR-083). The source of the names is cited
in `internal/leader/vllmalias.go`.

### Scaling the front doors — `GET /gatewayz`

An endpoint may have several **front doors** (`opod up --role gateway`), and a scaler for the doors must not
read one door's share of the traffic: keep-alive pins a client to one door, so that share is uneven. The
leader publishes the aggregate instead — also unauthenticated, and carrying no key, model or prompt data:

| Field | Meaning |
|---|---|
| `doors` / `doors_reporting` | doors the leader counts (its own front included, so never 0) / how many have pushed inside the liveness window |
| `doors_in_flight`, `doors_rpm_1m` | the endpoint's totals across the doors |
| `in_flight_per_door`, `rpm_1m_per_door` | the totals divided by the doors, rounded **up** — this is what to compare against a per-door target |
| `live_for_s` | how long a silent door still counts (deliberately longer than the 10 s rebalance, so one missed push does not make every other door overshoot) |
| `spend_lag_bound_s` | how far a per-key daily quota may lag across doors |
| `gateways` | the doors the leader has heard from, by name, with their age — which is how a door that stopped pushing is visible at all |

On a **door**, the same path answers that door's own view: how stale its mirrored worker registry is
(`registry_age_s`) and how big its push backlog is. A door is a copy of the front, not a second brain: it
never serves `/admin/v1`.

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

The **probe port** shipped and is documented above. The **uneven split flags** (`flags.tensor_split` for
llama.cpp, `flags.pp_layer_partition` for vLLM and SGLang) are declared per engine, validated, and in force for
a single worker. **Across machines they are shipped for one engine and not for the others.** A llama.cpp gang
carries its flags to the coordinator: on a 48 GB and an 8 GB card, `tensor_split` `12,8` left 11.0 GB on the one
and 6.4 GB on the other, and the gang served (measured 2026-09-29). A vLLM gang is delivered its
`pp_layer_partition` and each rank loads its share, but the one gang it was tried on never finished starting;
SGLang has not been tried. Read the flags as a cross-machine capability for llama.cpp only.

What a reader should still not expect from this endpoint: a decision. The leader publishes numbers and routes
across the workers it has; it never creates or removes one.
