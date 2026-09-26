# Reference Grafana dashboards

Importable JSON for the Prometheus metrics that `opod` exposes at `/metrics`.

| File | What it shows |
|---|---|
| `cluster-overview.json` | Total RPS, p50/p95/p99 latency across all models, error rate, tokens/s (prompt vs completion), nodes up, loaded-model inventory |
| `per-model.json` | Same questions filtered to one model (selectable from a Grafana template variable) — useful for capacity planning or debugging a hot model |
| `per-node.json` | Per-node fleet view — which nodes are up, how many models each is hosting, full loaded-model table |

All three render against these five series, declared in `internal/metrics/metrics.go`:

- `opod_requests_total{model,protocol,outcome}` — counter
- `opod_request_duration_seconds{model,protocol,outcome}` — histogram
- `opod_request_tokens_total{model,direction}` — counter (`direction` is `prompt`|`completion`)
- `opod_model_loaded{model,node}` — gauge (0/1)
- `opod_node_up{node,hostname}` — gauge (0/1)

Those five are a **deliberate subset**. `/metrics` exposes more, and they are worth a panel if you
build your own: `opod_time_to_first_token_seconds{model}` (streamed answers only — the number a user
feels), the router series (`opod_router_picks_total{path,outcome}`, `opod_router_inflight{node}`,
`opod_router_fallback_total{op,reason}`, `opod_router_attempt_duration_seconds{model,outcome}`,
`opod_router_cooldowns_active`, `opod_router_sticky_hits_total{outcome}`,
`opod_router_hedge_total{outcome}`), the response cache
(`opod_cache_hits_total{path}` / `opod_cache_misses_total{path}`) and
`opod_guardrail_action_total{name,action}`. Two names you will see in the registry are
**declared but never incremented** — `opod_callback_sent_total` and `opod_callback_queue_depth`:
the callback sinks left core with ADR-022 and the declarations are the last thing to remove. Do not
build a panel on them. `internal/metrics/metrics.go` is the list that decides.

For anything the metrics cannot answer about *capacity* — queue depth, KV-cache use, TTFT
percentiles, how many workers hold a card with a dead engine — read `GET /loadz` instead; it is the
autoscaling contract and it is documented in [docs/SCALING-SIGNALS.md](../docs/SCALING-SIGNALS.md).

## Importing

1. In Grafana, **Dashboards → New → Import**.
2. Upload the JSON file or paste its contents.
3. When prompted for a Prometheus data source, pick the one that scrapes your Opod leader's `/metrics`.

That's it — no edits needed. The dashboards use a `${DS_PROMETHEUS}` variable so they bind to whichever data source you pick.

## Scraping Opod

Minimal Prometheus scrape config (`prometheus.yml`):

```yaml
scrape_configs:
  - job_name: opod
    metrics_path: /metrics
    scrape_interval: 15s
    static_configs:
      - targets: ['localhost:8080']
```

**Scrape the probe port instead, when there is one.** Setting `probe_listen` (`OPOD_PROBE_LISTEN=:8081`)
puts `/healthz`, `/readyz`, `/loadz` and `/metrics` on a second listener that is **always plain HTTP**,
whatever TLS the main one speaks. That is the target you want: if the leader serves a certificate
(`tls_cert` / `tls_key`), scraping the main port means handing Prometheus that CA — or, in practice,
turning verification off. Nothing authenticated is routed to the probe port and its router is built
from scratch, so a new `/admin/v1` route cannot appear there by accident. Point the scrape config at
`localhost:8081` and leave `:8080` for clients.

**Note:** `/metrics` is registered before the auth middleware and is always unauthenticated by design — `auth.require_keys` only protects the `/v1/*` API surface, not metrics. There is no way to authenticate it or to serve metrics *only*. So if the port is reachable beyond localhost, protect it at the network level: bind to a private interface, firewall it to your Prometheus host, or front it with a reverse proxy that enforces auth. (An `authorization:` bearer block in the scrape config is only meaningful if such a proxy requires it — Opod itself ignores it on `/metrics`.)

## Compatibility

Tested against Grafana 10.x and 11.x with `schemaVersion: 39`. Older Grafana (≤ 9.x) may need a one-time export-and-reimport to upgrade the schema.

## Modifying

These are intentionally minimal — five series, three dashboards. Fork them if you want richer cuts (the router and TTFT series above, per-protocol breakdowns, alert rules, multi-cluster mixins). Upstream PRs welcome if you find a panel that's broadly useful.
