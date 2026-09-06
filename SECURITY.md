# Security Policy

## Supported versions

Opod auto-releases from `main` on every `feat:` / `fix:` commit (current stream: v1.x). Security fixes ship on `main` and land in the next auto-cut release. Older releases are not patched — upgrade with `opod update`.

| Version | Supported |
|---------|-----------|
| `main`  | ✅ |
| Latest tagged release | ✅ |
| Older  | ❌ |

## Reporting a vulnerability

**Do not file a public issue for security bugs.**

Email `hadi.work.ca@gmail.com` *or* (preferred) open a private GitHub Security Advisory at https://github.com/opod-io/opod/security/advisories/new

Include:
- A description of the issue and its impact
- Steps to reproduce
- Opod version, OS, and architecture
- Whether you intend to publish details and when

## Disclosure timeline

- **Day 0**: report received; acknowledgement within 48 hours
- **Day 7**: triage complete; severity assigned (CVSS 3.1)
- **Day 30**: fix in `main`, backports if applicable
- **Day 90**: public advisory and CVE if applicable

We coordinate disclosure timing with reporters when possible.

## Scope

In scope:

- The `opod` binary and all code in this repository
- Release artifacts we publish on GitHub Releases (binaries, `.deb` / `.rpm` packages, `checksums.txt`)
- Pre-built install scripts hosted at `raw.githubusercontent.com/opod-io/opod/main/installer/install.sh`

Out of scope:

- Upstream inference engines (vLLM, Ollama, MLX-LM, llama.cpp) — report to those projects
- Self-hosted deployments of Opod that have been modified
- Hypothetical issues without a reproduction

## Hall of fame

Credit will be given here for responsibly disclosed reports, with the reporter's permission.
