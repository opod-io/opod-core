# Getting help with Opod

| You want to… | Go to |
|---|---|
| Ask "how do I…", get setup help, discuss a design | [GitHub Discussions](https://github.com/opod-io/opod-core/discussions) |
| Report something broken | [Bug report](https://github.com/opod-io/opod-core/issues/new?template=bug_report.yml) — include `opod version` and `opod doctor` output |
| Propose a capability or a CLI change | [Feature request](https://github.com/opod-io/opod-core/issues/new?template=feature_request.yml) |
| Add or fix a model in the catalog | The catalog lives in [`opod-io/opod-sdk`](https://github.com/opod-io/opod-sdk): [catalog request](https://github.com/opod-io/opod-sdk/issues/new?template=catalog_request.yml) or a PR with `catalog/<id>.yaml` |
| Report a security problem | **Privately**, per [SECURITY.md](SECURITY.md) — never in a public issue |
| Run Opod across many GPUs with a console, rollouts and a VRAM ledger | The control plane, a separate product: [opod.io](https://opod.io) |

## Read first

- [README](README.md) — install, configuration reference, CLI reference
- [QUICKSTART](QUICKSTART.md) — a two-machine cluster in a few minutes
- [MODELS](MODELS.md) — the shipped catalog and the picker table
- [ARCHITECTURE](ARCHITECTURE.md) — how the leader, workers, router and engines fit together
- [CHANGELOG](CHANGELOG.md) — what shipped, newest first

## What to include in a question

- `opod version` and your OS / architecture
- The engine you run (Ollama, vLLM, SGLang, llama.cpp, MLX-LM) and its version
- The command you ran and the full output, with any key or token redacted (`sk-orc-…`)
- For a cluster question: how many nodes, which one is the leader, and `opod node ls`

Core is a CLI-only runtime maintained in the open. There is no paid support tier for it; response times are best-effort.
