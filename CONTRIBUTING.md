# Contributing to Opod

Thanks for your interest in contributing.

If you're here to fix a typo, you can skip the rest of this file — open a PR. **Catalog entries:** the bundled catalog is `pkg/catalog/<id>.yaml` in this repo (schema in `pkg/catalog/README.md`), embedded into the binary.

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
4. Start serving on `http://localhost:8080` — the OpenAI-shape gateway (`/v1/chat/completions`, `/v1/embeddings`, `/v1/models`, `/v1/rerank`) plus `/admin/v1`. No other protocol shape and no UI (ADR-022, amended by ADR-084 for rerank).

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
| `make perfgate` | the leader-cost gate, ~2 min: a real `opod up` + `opod join` in front of a stub engine, measured against the stub called directly. A PR fails when the leader adds more than its margin to time-to-first-token p95. CI runs it on every PR (`leader-cost` workflow) |
| `make run` | `make build && ./opod up` |
| `make tidy` | `go mod tidy` |
| `make clean` | remove the binary and `data/`, `.opod/` working dirs |

## Where to put new code

Quick map. Deeper explanation in [ARCHITECTURE.md](ARCHITECTURE.md).

| You want to … | Edit … |
|---|---|
| Add a new CLI subcommand | `cmd/opod/cmd_<name>.go` + a case in `cmd/opod/main.go` + a function in `internal/control/` first (the CLI and the admin HTTP endpoint must be two callers of that one function) |
| Add a new admin HTTP endpoint | `internal/leader/admin_<name>.go` — decode request, authenticate, delegate to `internal/control/` |
| Add a new inference engine | a package: `internal/engines/<name>/` implementing `Engine` from `types.go`, registered from its own `init` with `engines.Register(engines.Descriptor{…})` and blank-imported by `internal/engines/all`. Add a `conformance_test.go` calling `enginetest.Run`. Full steps in [ARCHITECTURE.md → Add a new inference engine](ARCHITECTURE.md#add-a-new-inference-engine) |
| Add a route to the OpenAI surface (e.g. `/v1/completions`) | `internal/api/`, wired in `internal/leader/server.go`. A *second* wire shape (Anthropic Messages, audio) is out of scope for core — it belongs in a shim in front of the gateway (ADR-022). Rerank came back only by an amendment (ADR-084), which is the way any new surface arrives |
| Add a model to the catalog | `pkg/catalog/<id>.yaml` (schema in `pkg/catalog/README.md`). To add or override one locally instead, drop a file in `~/.opod/catalog/` or `$OPOD_CATALOG_DIR` |
| Add a config field | extend `Config` in `internal/config/config.go`, add a default in `Default()`, optionally an env override in `applyEnv()`, document in [README.md → Full reference](README.md#full-reference). A variable a *manager* may set goes in `internal/config/env.go` instead — `TestEnvSurfaceOutsideConfigIsTheAllowlist` fails when library code reads one that is not on that list |
| Add a container image | a row in **four** places, held together by `cmd/opod/images_drift_test.go`: `images/build.sh`, `.github/workflows/images.yml`, `images/images.yaml` and the table in `images/README.md` |
| Add a metric | declare in `internal/metrics/metrics.go`, increment at the call site |

## CLI output

What `opod` prints is part of its interface: a person reads it to learn what happened and what to do next, and a
script reads its exit code and its JSON. Every command follows these rules.

1. **Success names what changed, with its ids, and the next command, filled in.** `token created: tok_3f… (admin)`,
   then `next: opod token ls`. A query with no result says so — `no nodes registered yet — issue a join token:
   opod token create --node` — never an empty table.
2. **A repeat says it changed nothing.** `already drained — nothing changed`, exit 0.
3. **An error goes to stderr with a non-zero exit, in three parts:** *what* failed (the verb and the object), *why*
   (the leader's message and code, or the wrapped cause — never a raw JSON body, a bare HTTP status or a bare Go
   error), and *what to do* — a command or a flag to type, already filled in, and where its value comes from
   (`--base-url`, `OPOD_BASE_URL`, `~/.opod/admin.key`). Where a docs page explains more, its URL.
4. **A long operation** prints a start line and progress at least every 10 s (the step, or the bytes). A timeout says
   what it waited for, for how long, and whether the work goes on in the leader. Ctrl-C says what state it left and how
   to clean up.
5. **A destructive operation** asks first or needs `--yes`, refuses without a terminal by naming `--yes`, and prints
   what it removed.
6. **Machine output:** every list and show verb has `--json`; JSON on stdout carries no human line; colour obeys
   `NO_COLOR` and a non-terminal; human warnings go to stderr.
7. **Help:** every flag has a sentence, its default and an example, and no internal ids (task numbers, ADR numbers,
   section signs).

**Every message is pinned by a transcript.** `cmd/opod/transcript_test.go` runs the real `main()` against a stub
leader and compares the exit code, stdout and stderr with `cmd/opod/testdata/cli/<case>.golden`. Add a case for each
message you add or change — success, repeat and each error class — then run
`go test ./cmd/opod -run TestCLITranscripts -update` and review the diff of the golden files like any other code.

## PR checklist

1. Open a discussion or issue first if the change is non-trivial.
2. Branch from `main`: `feat/<short-name>` or `fix/<short-name>`.
3. One change per PR.
4. Add or update tests and docs in the **same** PR — no "I'll fix docs later" follow-ups.
5. `make check` passes locally.
5b. Every commit is signed off (`git commit -s`, the DCO — see below).
6. If the change adds or modifies a CLI verb, README's CLI reference is updated, its output follows [CLI output](#cli-output), and a transcript case pins it.
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

Use **GitHub Discussions** for design questions, RFCs, and "is this a bug?". Use **GitHub Issues** only for confirmed bugs and concrete feature requests. [SUPPORT.md](SUPPORT.md) is the one-table map of where each kind of question goes.

## Further reading

- [ARCHITECTURE.md](ARCHITECTURE.md) — design rationale, subsystem boundaries, and the rule that the CLI and the admin API are two callers of one function in `internal/control/`
- [README.md → Deliberately out of scope](README.md#deliberately-out-of-scope) — what this project has decided not to become, and why
- [CHANGELOG.md](CHANGELOG.md) — the feature inventory by area, newest notes on top
- `pkg/` — the `/admin/v1` and worker wire types and the catalog (`pkg/catalog/README.md` for its schema). They are what the leader and the worker put on the wire, not a supported client API.
- [CODE_OF_CONDUCT.md](CODE_OF_CONDUCT.md)
- [SECURITY.md](SECURITY.md) — vulnerability disclosure process
