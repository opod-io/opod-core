<!--
Thanks for contributing to Opod! Please fill in the sections below.
See CONTRIBUTING.md for the full guide. Keep PRs focused: one change per PR.
-->

## Summary

<!-- One paragraph: what does this change and why? Link to the related issue. -->

Closes #

## Type of change

<!-- Tick all that apply. -->

- [ ] Bug fix (non-breaking, restores intended behavior)
- [ ] New feature (non-breaking, adds a capability)
- [ ] Breaking change (requires config / API / catalog migration)
- [ ] Docs only (README / ARCHITECTURE / MODELS / QUICKSTART / site)
- [ ] Catalog entry (new or updated YAML in `catalog/`)
- [ ] Refactor (no functional change)
- [ ] Tooling / CI

## Checklist

- [ ] One change per PR — no drive-by edits in unrelated files
- [ ] `make check` (or `go vet ./... && go test ./...`) passes locally
- [ ] Added or updated tests where behavior changed
- [ ] Updated docs in the same PR — `README.md`, `QUICKSTART.md`, `MODELS.md`, `ARCHITECTURE.md`, `internal/ui/index.html`, or CLI `--help` as applicable
- [ ] If a new CLI command: added it to `cmd/opod/main.go` help + `opod <cmd> --help` examples
- [ ] If a new catalog entry: verified the source actually exists (Ollama tag pulls, HF repo loads, or GGUF URL is real)
- [ ] PR title follows the convention from existing commits (e.g. `feat: ...`, `fix: ...`, `catalog: ...`, `docs: ...`)
- [ ] No secrets, API keys, or tokens committed

## How to test

<!--
Be explicit. Pretend the reviewer has never run Opod.

For a CLI change:
  opod <new-command> ...
  ↳ should print/do XYZ

For an API change:
  curl http://localhost:8080/v1/... -d '{...}'
  ↳ expected response: ...

For a catalog entry:
  OPOD_CATALOG_DIR=./catalog go run ./cmd/opod model info <id>
  ↳ should show the new entry with correct size and capabilities
-->

## Screenshots / output

<!-- For UI changes: before/after screenshots. For CLI: paste terminal output. -->

## Notes for reviewers

<!-- Anything subtle: edge cases you handled, deliberate trade-offs, follow-up work tracked elsewhere. -->
