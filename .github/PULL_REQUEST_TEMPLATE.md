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
- [ ] Breaking change (requires config / API / schema migration)
<!-- Catalog entries are NOT in this repo: model YAMLs live in opod-io/opod-sdk and are
     embedded into the binary. A catalog change is a PR there, not here. -->
- [ ] Docs only (README / ARCHITECTURE / MODELS / QUICKSTART / site)
- [ ] Refactor (no functional change)
- [ ] Tooling / CI

## Checklist

- [ ] One change per PR — no drive-by edits in unrelated files
- [ ] `make check` (or `go vet ./... && go test ./...`) passes locally
- [ ] Added or updated tests where behavior changed
- [ ] Updated docs in the same PR — `README.md`, `QUICKSTART.md`, `MODELS.md`, `ARCHITECTURE.md`, `CHANGELOG.md`, or CLI `--help` as applicable
- [ ] If a new CLI command: added it to `cmd/opod/main.go` help + `opod <cmd> --help` examples
- [ ] If a new container image: the row exists in all four of `images/build.sh`, `.github/workflows/images.yml`, `images/images.yaml` and `images/README.md` (`make integration` checks this)
- [ ] PR title follows the convention from existing commits (e.g. `feat: ...`, `fix: ...`, `docs: ...`, `images: ...`)
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

For a local catalog override:
  OPOD_CATALOG_DIR=/tmp/mycatalog go run ./cmd/opod model info <id>
  ↳ should show the entry with correct size and capabilities
-->

## Output

<!-- Paste the terminal output — core is CLI-only, there is no UI to screenshot. -->

## Notes for reviewers

<!-- Anything subtle: edge cases you handled, deliberate trade-offs, follow-up work tracked elsewhere. -->
