# Reference Grafana dashboards

Importable JSON for the Prometheus metrics that `opod` exposes at `/metrics`.

| File | What it shows |
|---|---|
| `cluster-overview.json` | Total RPS, p50/p95/p99 latency across all models, error rate, tokens/s (prompt vs completion), nodes up, loaded-model inventory |
| `per-model.json` | Same questions filtered to one model (selectable from a Grafana template variable) — useful for capacity planning or debugging a hot model |
| `per-node.json` | Per-node fleet view — which nodes are up, how many models each is hosting, full loaded-model table |

All three render against the metrics declared in `internal/metrics/metrics.go`:

- `opod_requests_total{model,protocol,outcome}` — counter
- `opod_request_duration_seconds{model,protocol,outcome}` — histogram
- `opod_request_tokens_total{model,direction}` — counter (`direction` is `prompt`|`completion`)
- `opod_model_loaded{model,node}` — gauge (0/1)
- `opod_node_up{node,hostname}` — gauge (0/1)

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

**Note:** `/metrics` is registered before the auth middleware and is always unauthenticated by design — `auth.require_keys` only protects the `/v1/*` API surface, not metrics. If your Opod leader is reachable beyond localhost, protect the endpoint at the network level: bind the leader to a private interface, firewall the port to your Prometheus host, or front it with a reverse proxy that enforces auth. (A `authorization:` bearer block in the scrape config is only meaningful if such a proxy requires it — Opod itself ignores it on `/metrics`.)

## Compatibility

Tested against Grafana 10.x and 11.x with `schemaVersion: 39`. Older Grafana (≤ 9.x) may need a one-time export-and-reimport to upgrade the schema.

## Modifying

These are intentionally minimal — five metrics, three dashboards. Fork them if you want richer cuts (per-protocol breakdowns, alert rules, multi-cluster mixins). Upstream PRs welcome if you find a panel that's broadly useful.
