# Contributing to Opod

Thanks for your interest in contributing.

If you're here to fix a typo, you can skip the rest of this file — open a PR. **Catalog entries are not in this repo:** the bundled catalog is embedded from [`opod-io/opod-sdk`](https://github.com/opod-io/opod-sdk), so a new or corrected model YAML is a PR there.

For everything else, the goal of this doc is to get you to a green `make check` (or `make integration` for a stricter pre-push smoke) and a running cluster in under 30 minutes.

## Prerequisites

- **Go 1.25+** — `go version` to check.
- **A running Ollama** — `brew install --cask ollama` on macOS, `curl -fsSL https://ollama.com/install.sh | sh` on Linux. The dev loop assumes Ollama at `http://127.0.0.1:11434`.
- That's it. No Docker, no Python, no Node — core is CLI-only and ships no web assets (ADR-022; `/` answers 404). The console is the control plane's, a separate product.

## First run

```bash
git clone https://github.com/opod-io/opod-core
cd opod-core
make check             # lint + test + build (this is what CI runs)
./opod up             # boots a single-node leader against local Ollama
```

`opod up` will:

1. Bootstrap `~/.opod/state.db` (SQLite, pure Go — no CGO).
2. Print an admin API key **once**, and save the plaintext to `~/.opod/admin.key` (mode 0600) so later CLI runs on this host authenticate without a copy-paste.
3. Auto-pick a model based on hardware (run `opod model search` to see options).
4. Start serving on `http://localhost:8080` — the OpenAI-shape gateway (`/v1/chat/completions`, `/v1/embeddings`, `/v1/models`) plus `/admin/v1`. No other protocol shape and no UI (ADR-022).

From there: edit code → `make build` → restart `./opod up`. There is no watch mode; the binary boots in under a second.

## Make targets

The Makefile is intentionally tiny. Every target maps to a single `go` invocation:

| Target | What it runs |
|---|---|
| `make build` (default) | `go build -trimpath -o opod ./cmd/opod` |
| `make fmt-check` | `gofmt -s -l .` — fails if any file would change |
| `make integration` | `fmt-check` + lint + test + build, then a binary smoke (`version`, `help`, `connect --list`, `model search`) and the drift tests. Pre-push sanity in ~10 s warm. |
| `make test` | `go test ./...` |
| `make lint` | `go vet ./...`, then `golangci-lint run ./...` with this repo's `.golangci.yml` when that binary is on PATH or in `GOPATH/bin` (CI always runs it) |
| `make check` | lint + test + build, in order. **This is what every PR must pass.** |
| `make run` | `make build && ./opod up` |
| `make tidy` | `go mod tidy` |
| `make clean` | remove the binary and `data/`, `.opod/` working dirs |

## Where to put new code

Quick map. Deeper explanation in [ARCHITECTURE.md](ARCHITECTURE.md).

| You want to … | Edit … |
|---|---|
| Add a new CLI subcommand | `cmd/opod/cmd_<name>.go` + a case in `cmd/opod/main.go` + a function in `internal/control/` first (the CLI and the admin HTTP endpoint must be two callers of that one function) |
| Add a new admin HTTP endpoint | `internal/controlplane/admin_<name>.go` — decode request, authenticate, delegate to `internal/control/` |
| Add a new inference engine | a package: `internal/engines/<name>/` implementing `Engine` from `types.go`, registered from its own `init` with `engines.Register(engines.Descriptor{…})` and blank-imported by `internal/engines/all`. Add a `conformance_test.go` calling `enginetest.Run`. Full steps in [ARCHITECTURE.md → Add a new inference engine](ARCHITECTURE.md#add-a-new-inference-engine) |
| Add a route to the OpenAI surface (e.g. `/v1/completions`) | `internal/api/`, wired in `internal/controlplane/server.go`. A *second* wire shape (Anthropic Messages, audio, rerank) is out of scope for core — it belongs in a shim in front of the gateway (ADR-022) |
| Add a model to the catalog | not here — `catalog/<id>.yaml` in [`opod-io/opod-sdk`](https://github.com/opod-io/opod-sdk) (schema in that repo's `catalog/README.md`). To add or override one locally instead, drop a file in `~/.opod/catalog/` or `$OPOD_CATALOG_DIR` |
| Add a config field | extend `Config` in `internal/config/config.go`, add a default in `Default()`, optionally an env override in `applyEnv()`, document in [README.md → Full reference](README.md#full-reference). A variable a *manager* may set goes in `internal/config/env.go` instead — `TestEnvSurfaceOutsideConfigIsTheAllowlist` fails when library code reads one that is not on that list |
| Add a container image | a row in **four** places, held together by `cmd/opod/images_drift_test.go`: `images/build.sh`, `.github/workflows/images.yml`, `images/images.yaml` and the table in `images/README.md` |
| Add a metric | declare in `internal/metrics/metrics.go`, increment at the call site |

## PR checklist

1. Open a discussion or issue first if the change is non-trivial.
2. Branch from `main`: `feat/<short-name>` or `fix/<short-name>`.
3. One change per PR.
4. Add or update tests and docs in the **same** PR — no "I'll fix docs later" follow-ups.
5. `make check` passes locally.
5b. Every commit is signed off (`git commit -s`, the DCO — see below).
6. If the change adds or modifies a CLI verb, README's CLI reference is updated.
7. If the change adds a config field, README's "Full reference" includes it.
8. PR title follows the commit convention (`feat:`, `fix:`, `docs:`, `catalog:` …) — that is what cuts the next release tag.

## Licence and the Developer Certificate of Origin

Opod core is Apache-2.0 (see [LICENSE](LICENSE)). There is no contributor licence agreement.
Every commit must carry a [Developer Certificate of Origin](https://developercertificate.org/) sign-off —
`git commit -s` adds the `Signed-off-by:` trailer — which states that you wrote the change or have the
right to submit it under the project's licence. Unsigned commits are not merged.

## Reporting bugs

File an issue with:

- Opod version (`opod version`)
- OS and architecture
- Output of `opod doctor`
- Minimal reproduction

## Asking questions

Use **GitHub Discussions** for design questions, RFCs, and "is this a bug?". Use **GitHub Issues** only for confirmed bugs and concrete feature requests.

## Further reading

- [ARCHITECTURE.md](ARCHITECTURE.md) — design rationale, subsystem boundaries, and the rule that the CLI and the admin API are two callers of one function in `internal/control/`
- [ROADMAP.md](ROADMAP.md) — what ships, what is next, and what is deliberately out of scope
- [CHANGELOG.md](CHANGELOG.md) — the feature inventory by area, newest notes on top
- [docs/archive/TASKS-milestones-M0-M5.md](docs/archive/TASKS-milestones-M0-M5.md) — the M0–M5 milestone history, kept for context
- [`opod-io/opod-sdk`](https://github.com/opod-io/opod-sdk) — the catalog YAML schema (`catalog/README.md` there) and the `/admin/v1` wire types
- [CODE_OF_CONDUCT.md](CODE_OF_CONDUCT.md)
- [SECURITY.md](SECURITY.md) — vulnerability disclosure process
