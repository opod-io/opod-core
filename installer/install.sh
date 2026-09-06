#!/usr/bin/env sh
# Opod installer — production install for macOS and Linux.
#
# Usage:
#   curl -fsSL https://raw.githubusercontent.com/opod-io/opod/main/installer/install.sh | sh
#
# Flags (after `| sh -s --`):
#   --help                  show this message
#   --version <vX.Y.Z>      install a specific version (default: latest release)
#   --install-dir <path>    install to a specific directory (default: try
#                           ~/.local/bin, ~/bin, /usr/local/bin in that order)
#   --no-engine             skip the Ollama detection / warning
#   --dry-run               show what would happen without writing anything
#
# Env overrides (alternative to flags):
#   OPOD_VERSION, OPOD_INSTALL_DIR, OPOD_SKIP_ENGINE=1
#
# This installer also accepts a `join <leader-url>?token=…` positional after
# `--`, in which case it runs `opod join` after installing.
set -eu

REPO="opod-io/opod"
VERSION="${OPOD_VERSION:-latest}"
INSTALL_DIR="${OPOD_INSTALL_DIR:-}"
SKIP_ENGINE="${OPOD_SKIP_ENGINE:-0}"
DRY_RUN=0

# ---- pretty output ----

if [ -t 1 ] && [ "${NO_COLOR:-}" = "" ]; then
    C_BLUE="\033[1;34m"; C_GREEN="\033[1;32m"; C_YELLOW="\033[1;33m"
    C_RED="\033[1;31m"; C_BOLD="\033[1m"; C_RESET="\033[0m"
else
    C_BLUE=""; C_GREEN=""; C_YELLOW=""; C_RED=""; C_BOLD=""; C_RESET=""
fi

note() { printf "%b▶%b %s\n" "$C_BLUE" "$C_RESET" "$*"; }
ok()   { printf "%b✔%b %s\n" "$C_GREEN" "$C_RESET" "$*"; }
warn() { printf "%b⚠%b %s\n" "$C_YELLOW" "$C_RESET" "$*" >&2; }
err()  { printf "%b✖%b %s\n" "$C_RED" "$C_RESET" "$*" >&2; }

usage() {
    cat <<EOF
Opod installer

Usage:
  curl -fsSL https://raw.githubusercontent.com/${REPO}/main/installer/install.sh | sh
  curl -fsSL https://raw.githubusercontent.com/${REPO}/main/installer/install.sh | sh -s -- [flags]

Flags:
  --help                Show this message
  --version <vX.Y.Z>    Install a specific version (default: latest release)
  --install-dir <path>  Install to a specific dir
  --no-engine           Skip the Ollama check
  --dry-run             Show what would happen, no writes

Examples:
  # standard install + check Ollama:
  curl -fsSL https://raw.githubusercontent.com/${REPO}/main/installer/install.sh | sh

  # install + join an existing cluster:
  curl -fsSL https://raw.githubusercontent.com/${REPO}/main/installer/install.sh | \\
    sh -s -- join https://leader.local:8080?token=...

  # install to a specific path, skip engine check:
  curl -fsSL https://... | sh -s -- --install-dir ~/.local/bin --no-engine
EOF
}

# ---- arg parsing ----

JOIN_URL=""
while [ $# -gt 0 ]; do
    case "$1" in
        --help|-h) usage; exit 0 ;;
        --install-dir)
            if [ $# -lt 2 ] || [ -z "$2" ]; then
                err "--install-dir requires a value (e.g. --install-dir ~/.local/bin)"
                usage >&2
                exit 1
            fi
            INSTALL_DIR="$2"; shift 2 ;;
        --version)
            if [ $# -lt 2 ] || [ -z "$2" ]; then
                err "--version requires a value (e.g. --version v0.1.0)"
                usage >&2
                exit 1
            fi
            VERSION="$2"; shift 2 ;;
        --no-engine) SKIP_ENGINE=1; shift ;;
        --dry-run) DRY_RUN=1; shift ;;
        join)
            if [ $# -lt 2 ] || [ -z "$2" ]; then
                err "join requires a leader URL (e.g. join http://leader:8080?token=...)"
                usage >&2
                exit 1
            fi
            JOIN_URL="$2"; shift 2
            break
            ;;
        *)
            err "unknown argument: $1"
            usage >&2
            exit 1
            ;;
    esac
done

# ---- platform detection ----

detect_platform() {
    OS="$(uname -s | tr '[:upper:]' '[:lower:]')"
    ARCH="$(uname -m)"
    case "$ARCH" in
        x86_64|amd64) ARCH="amd64" ;;
        arm64|aarch64) ARCH="arm64" ;;
        *)
            err "unsupported architecture: $ARCH"
            err "  supported: amd64 (x86_64), arm64 (aarch64)"
            exit 1
            ;;
    esac
    case "$OS" in
        darwin|linux) : ;;
        *)
            err "unsupported OS: $OS"
            err "  supported: darwin (macOS), linux"
            err "  for Windows: use WSL2 then re-run this installer"
            exit 1
            ;;
    esac
    PLATFORM="${OS}-${ARCH}"
    ok "platform: ${PLATFORM}"
}

# ---- prereq tools ----

check_tools() {
    missing=""
    for cmd in curl tar uname; do
        if ! command -v "$cmd" >/dev/null 2>&1; then
            missing="$missing $cmd"
        fi
    done
    if [ -n "$missing" ]; then
        err "missing required tools:$missing"
        case "$OS" in
            darwin) err "  install with: xcode-select --install" ;;
            linux)  err "  install with: apt install curl tar  (or your distro's equivalent)" ;;
        esac
        exit 1
    fi
    ok "shell tools present (curl, tar, uname)"
}

# ---- inference engine check ----

check_engine() {
    if [ "$SKIP_ENGINE" = "1" ]; then
        note "skipping engine check (--no-engine)"
        return
    fi

    if ! command -v ollama >/dev/null 2>&1; then
        warn "ollama is not installed — opod needs an inference engine to serve models"
        case "$OS" in
            darwin)
                warn "  recommended: brew install --cask ollama"
                warn "  alternative: curl -fsSL https://ollama.com/install.sh | sh"
                ;;
            linux)
                warn "  install:     curl -fsSL https://ollama.com/install.sh | sh"
                ;;
        esac
        warn "  or pass --no-engine to skip this check"
        return
    fi
    ok "ollama: $(command -v ollama)"

    # Detect the macOS Homebrew formula bug where llama-server is missing.
    if [ "$OS" = "darwin" ] && command -v brew >/dev/null 2>&1; then
        cellar="$(brew --cellar ollama 2>/dev/null || true)"
        if [ -n "$cellar" ] && [ -d "$cellar" ]; then
            if ! find "$cellar" -name "llama-server" -print -quit 2>/dev/null | grep -q .; then
                warn "the Homebrew 'ollama' formula on Apple Silicon is missing the"
                warn "internal 'llama-server' binary — model inference will fail with"
                warn "  500: error starting llama-server: llama-server binary not found"
                warn "  fix:  brew uninstall ollama && brew install --cask ollama"
            fi
        fi
    fi
}

# ---- version resolution ----

resolve_version() {
    if [ "$VERSION" = "latest" ]; then
        VERSION="$(curl -fsSL "https://api.github.com/repos/${REPO}/releases/latest" \
            | grep '"tag_name":' | head -1 | cut -d'"' -f4 || true)"
        if [ -z "$VERSION" ]; then
            err "could not resolve latest version from GitHub API"
            err "  (possible rate-limit; try setting OPOD_VERSION=v0.1.0)"
            err "  releases: https://github.com/${REPO}/releases"
            exit 1
        fi
    fi
    ok "version: ${VERSION}"
}

# ---- install dir ----

resolve_install_dir() {
    if [ -n "$INSTALL_DIR" ]; then
        ok "install dir: $INSTALL_DIR (override)"
        if [ "$DRY_RUN" = "1" ]; then
            note "[dry-run] would create $INSTALL_DIR if missing"
        else
            mkdir -p "$INSTALL_DIR" 2>/dev/null || true
        fi
        return
    fi
    # Try user-writable dirs first, falling back to /usr/local/bin (sudo).
    for d in "$HOME/.local/bin" "$HOME/bin"; do
        if [ -d "$d" ] && [ -w "$d" ]; then
            INSTALL_DIR="$d"
            ok "install dir: $INSTALL_DIR"
            return
        fi
    done
    # Create ~/.local/bin if it doesn't exist (user is creator, no sudo needed)
    if [ -w "$HOME" ]; then
        INSTALL_DIR="$HOME/.local/bin"
        if [ "$DRY_RUN" = "1" ]; then
            note "[dry-run] would create install dir $INSTALL_DIR"
        else
            mkdir -p "$INSTALL_DIR"
            ok "install dir: $INSTALL_DIR (created)"
        fi
        return
    fi
    INSTALL_DIR="/usr/local/bin"
    warn "no writable user-dir found; will sudo into $INSTALL_DIR"
}

# ---- download + verify ----

download_binary() {
    URL="https://github.com/${REPO}/releases/download/${VERSION}/opod-${PLATFORM}.tar.gz"
    SUM_URL="https://github.com/${REPO}/releases/download/${VERSION}/checksums.txt"

    if [ "$DRY_RUN" = "1" ]; then
        note "[dry-run] would download ${URL}"
        note "[dry-run] would verify sha256 against ${SUM_URL}"
        return
    fi

    # Note: deliberately NOT named TMPDIR — that would clobber the standard
    # TMPDIR env var that mktemp & friends consult.
    tmp_dir="$(mktemp -d 2>/dev/null || mktemp -d -t opod-install)"
    trap 'rm -rf "$tmp_dir"' EXIT

    note "downloading ${URL}"
    if ! curl -fsSL "$URL" -o "$tmp_dir/opod.tar.gz"; then
        err "download failed"
        err "  check that ${VERSION} was published for ${PLATFORM}:"
        err "  https://github.com/${REPO}/releases/tag/${VERSION}"
        exit 1
    fi

    # Verify checksum (best-effort — older releases might not ship checksums.txt)
    if curl -fsSL "$SUM_URL" -o "$tmp_dir/checksums.txt" 2>/dev/null; then
        # Prefer sha256sum (Linux coreutils), fall back to shasum (macOS).
        if command -v sha256sum >/dev/null 2>&1; then
            sha_cmd="sha256sum"
        elif command -v shasum >/dev/null 2>&1; then
            sha_cmd="shasum -a 256"
        else
            sha_cmd=""
        fi
        if [ -z "$sha_cmd" ]; then
            warn "neither sha256sum nor shasum is installed — CHECKSUM VERIFICATION SKIPPED"
            warn "  the downloaded binary was NOT verified against checksums.txt"
        else
            expected="$(grep "opod-${PLATFORM}.tar.gz" "$tmp_dir/checksums.txt" | awk '{print $1}')"
            if [ -z "$expected" ]; then
                # checksums.txt exists but has no line for this artifact:
                # fail closed, matching the Go updater's behavior.
                err "checksums.txt for ${VERSION} has no entry for opod-${PLATFORM}.tar.gz"
                err "  refusing to install an unverifiable binary"
                exit 1
            fi
            actual="$($sha_cmd "$tmp_dir/opod.tar.gz" | awk '{print $1}')"
            if [ "$expected" = "$actual" ]; then
                ok "checksum verified (sha256)"
            else
                err "checksum MISMATCH — refusing to install possibly-tampered binary"
                err "  expected: $expected"
                err "  actual:   $actual"
                exit 1
            fi
        fi
    else
        warn "checksums.txt not available for ${VERSION} — skipping verification"
    fi

    if ! tar -xzf "$tmp_dir/opod.tar.gz" -C "$tmp_dir"; then
        err "tar extract failed"
        exit 1
    fi
    if [ ! -x "$tmp_dir/opod" ]; then
        err "extracted archive does not contain a 'opod' binary"
        ls -la "$tmp_dir" >&2
        exit 1
    fi
}

# ---- install ----

install_binary() {
    target="$INSTALL_DIR/opod"
    if [ "$DRY_RUN" = "1" ]; then
        note "[dry-run] would install opod ${VERSION} to $target"
        return
    fi
    if [ -w "$INSTALL_DIR" ]; then
        mv "$tmp_dir/opod" "$target"
    else
        note "installing to $target (sudo required)"
        sudo mv "$tmp_dir/opod" "$target"
    fi
    chmod +x "$target" 2>/dev/null || sudo chmod +x "$target"
    ok "installed opod ${VERSION} to ${target}"

    # Catalog: copy the bundled YAML files to ~/.opod/catalog so
    # `opod up` and `opod model` work out of the box. resolveCatalogDir
    # picks this path up automatically.
    if [ -d "$tmp_dir/catalog" ]; then
        catalog_dst="$HOME/.opod/catalog"
        mkdir -p "$catalog_dst"
        cp "$tmp_dir"/catalog/*.yaml "$catalog_dst/" 2>/dev/null || true
        ok "catalog installed at $catalog_dst"
    fi

    # PATH check
    case ":$PATH:" in
        *":$INSTALL_DIR:"*) ;;
        *)
            warn "${INSTALL_DIR} is not on your PATH"
            warn "  add to your shell rc file:"
            warn "    export PATH=\"$INSTALL_DIR:\$PATH\""
            warn "  then either restart the shell or run:"
            warn "    source ~/.zshrc  (or ~/.bashrc)"
            ;;
    esac
}

# ---- post-install guidance ----

print_next_steps() {
    # printf '%b' interprets the \033 sequences in $C_BOLD/$C_RESET. Using
    # `cat <<EOF` would print them as literal "\033[1m" text on POSIX sh.
    printf '%b' "
  ${C_BOLD}Next steps${C_RESET}

    1. Make sure Ollama is running:
         ollama serve &
    2. (optional) preflight diagnostics:
         opod doctor
    3. Start Opod:
         opod up
    4. Sign into the web UI:
         open http://localhost:8080
       (use the admin key shown by 'opod up')

  ${C_BOLD}Upgrading later${C_RESET}

    From now on, you can upgrade in place — no need to re-run this installer:
         opod update            # downloads latest release, verifies SHA-256, replaces binary
         opod update --check    # just check, don't install

  ${C_BOLD}Joining an existing cluster?${C_RESET}
    Have the leader's admin run:
         opod token create --node
    then on this machine:
         opod join http://<leader-host>:8080?token=<TOKEN>

  Docs: https://github.com/${REPO}
"
}

# ---- main ----

note "opod installer"
detect_platform
check_tools
check_engine
resolve_version
resolve_install_dir
download_binary
install_binary

if [ -n "$JOIN_URL" ]; then
    if [ "$DRY_RUN" = "1" ]; then
        note "[dry-run] would join cluster: $JOIN_URL"
    else
        note "joining cluster: $JOIN_URL"
        "$INSTALL_DIR/opod" join "$JOIN_URL"
    fi
else
    print_next_steps
fi
