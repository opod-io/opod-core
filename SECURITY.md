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

## Supply chain — verifying what you run

Container images published from this repo (`opod-leader`, `opod-worker-*`) are built by the release
workflow on a `v*` tag, signed with **cosign keyless** (Sigstore), and carry an **SPDX SBOM** attached as
an attestation. There is no signing key to distribute or leak: the signature is bound to the GitHub
workflow identity that produced the image, so verifying tells you *which build made this exact digest*.

```bash
cosign verify \
  --certificate-identity-regexp 'https://github.com/opod-io/' \
  --certificate-oidc-issuer https://token.actions.githubusercontent.com \
  ghcr.io/opod-io/opod-leader:<version>

# the worker images are mostly upstream bases (vLLM, llama.cpp, ROCm, SYCL);
# the SBOM is where their components and licences are enumerated
cosign download attestation ghcr.io/opod-io/opod-worker-llamacpp-nvidia:<version> \
  | jq -r .payload | base64 -d | jq .predicate
```

Two honest limits. Images built outside that workflow — a local `images/build.sh` run for development —
are **not signed**, and `build.sh` refuses to push from a dirty tree so such builds stay on the machine
that made them. And garbling (used by the commercial control plane, not by this repo) raises the cost of
reading a binary; it is not a security boundary. If verification fails, do not run the image.

## Hall of fame

Credit will be given here for responsibly disclosed reports, with the reporter's permission.
