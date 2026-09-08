# Contributing to Opod

Thanks for your interest in contributing.

If you're here to fix a typo or add a model to the catalog, you can skip the rest of this file — open a PR.

For everything else, the goal of this doc is to get you to a green `make check` (or `make integration` for a stricter pre-push smoke) and a running cluster in under 30 minutes.

## Prerequisites

- **Go 1.25+** — `go version` to check.
- **A running Ollama** — `brew install --cask ollama` on macOS, `curl -fsSL https://ollama.com/install.sh | sh` on Linux. The dev loop assumes Ollama at `http://127.0.0.1:11434`.
- That's it. No Docker, no Python, no Node — the web UI is a single embedded HTML file compiled into the binary.

## First run

```bash
git clone https://github.com/opod-io/opod
cd opod
make check             # lint + test + build (this is what CI runs)
./opod up             # boots a single-node leader against local Ollama
```

`opod up` will:

1. Bootstrap `~/.opod/state.db` (SQLite, pure Go — no CGO).
2. Print an admin API key to **stderr** — copy it; it's shown only once.
3. Auto-pick a model based on hardware (run `opod model search` to see options).
4. Start serving on `http://localhost:8080` — OpenAI + Anthropic API + admin UI.

From there: edit code → `make build` → restart `./opod up`. There is no watch mode; the binary boots in under a second.

## Make targets

The Makefile is intentionally tiny. Every target maps to a single `go` invocation:

| Target | What it runs |
|---|---|
| `make build` (default) | `go build -trimpath -o opod ./cmd/opod` |
| `make fmt-check` | `gofmt -s -l .` — fails if any file would change |
| `make integration` | `check` + `fmt-check` + binary smoke (`version`, `help`, `connect --list`) + drift tests. Pre-push sanity in ~10s. |
| `make test` | `go test ./...` |
| `make lint` | `go vet ./...` |
| `make check` | lint + test + build, in order. **This is what every PR must pass.** |
| `make run` | `make build && ./opod up` |
| `make tidy` | `go mod tidy` |
| `make clean` | remove the binary and `data/`, `.opod/` working dirs |

## Where to put new code

Quick map. Deeper explanation in [ARCHITECTURE.md](ARCHITECTURE.md).

| You want to … | Edit … |
|---|---|
| Add a new CLI subcommand | `cmd/opod/cmd_<name>.go` + a case in `cmd/opod/main.go` + a function in `internal/control/` first (CLI is the source of truth — the admin HTTP endpoint and UI must call the same function) |
| Add a new admin HTTP endpoint | `internal/controlplane/admin_<name>.go` — decode request, authenticate, delegate to `internal/control/` |
| Add a new inference engine | `internal/engines/<name>.go` (implement `Engine` from `types.go`), register in `internal/engines/registry.go` |
| Add a new API protocol (e.g. Cohere) | `internal/api/<name>.go`, wire route in `internal/controlplane/server.go` |
| Add a model to the catalog | `catalog/<id>.yaml` — see [catalog/README.md](catalog/README.md) for the schema |
| Add a config field | extend `Config` in `internal/config/config.go`, add a default in `Default()`, optionally an env override in `applyEnv()`, document in [README.md → Full reference](README.md#full-reference) |
| Add a UI page or tab | edit `internal/ui/index.html` directly (single-file HTML + Tailwind + inline JS at the bottom) |
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
8. PR title references the task ID if applicable (e.g. `M1-T07: add API key bootstrap`).

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

- [ARCHITECTURE.md](ARCHITECTURE.md) — design rationale, subsystem boundaries, the CLI / Admin API / Web UI contract
- [TASKS.md](TASKS.md) — open and shipped work items, milestone tracker
- [ROADMAP.md](ROADMAP.md) — strategic plan through v1.0
- [catalog/README.md](catalog/README.md) — catalog YAML schema
- [CODE_OF_CONDUCT.md](CODE_OF_CONDUCT.md)
- [SECURITY.md](SECURITY.md) — vulnerability disclosure process
