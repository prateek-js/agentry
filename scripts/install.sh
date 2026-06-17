#!/bin/sh
# agentry CLI installer.
#
#   curl -fsSL https://agentry.run/install.sh | sh
#
# Installs the `agentry` CLI for your OS/arch to a per-user location
# (no sudo by default), verifies its SHA-256 checksum, and tells you how
# to put it on PATH.
#
# Environment overrides:
#   AGENTRY_PREFIX   install dir       (default: $HOME/.local/bin)
#   AGENTRY_VERSION  version to fetch  (default: latest)
#   AGENTRY_BASE_URL download base     (default: https://agentry.run/install)
set -eu

AGENTRY_VERSION="${AGENTRY_VERSION:-latest}"
AGENTRY_BASE_URL="${AGENTRY_BASE_URL:-https://agentry.run/install}"
AGENTRY_PREFIX="${AGENTRY_PREFIX:-$HOME/.local/bin}"

say() { printf 'agentry: %s\n' "$1"; }
die() { printf 'agentry: error: %s\n' "$1" >&2; exit 1; }

command -v curl >/dev/null 2>&1 || die "curl is required to install"

# ── Detect platform ────────────────────────────────────────────────
os="$(uname -s)"
arch="$(uname -m)"
case "$os" in
  Darwin) os=darwin ;;
  Linux)  os=linux ;;
  *) die "unsupported OS '$os'. On Windows, download agentry-windows-amd64.exe from $AGENTRY_BASE_URL and add it to PATH." ;;
esac
case "$arch" in
  x86_64|amd64)  arch=amd64 ;;
  arm64|aarch64) arch=arm64 ;;
  *) die "unsupported architecture '$arch'" ;;
esac

# ── Resolve version ────────────────────────────────────────────────
if [ "$AGENTRY_VERSION" = latest ]; then
  AGENTRY_VERSION="$(curl -fsSL "$AGENTRY_BASE_URL/latest.txt" 2>/dev/null | tr -d '[:space:]')" \
    || die "could not reach $AGENTRY_BASE_URL/latest.txt"
  [ -n "$AGENTRY_VERSION" ] || die "latest.txt was empty"
fi

bin="agentry-${os}-${arch}"
url="$AGENTRY_BASE_URL/$AGENTRY_VERSION/$bin"

say "installing $AGENTRY_VERSION for $os/$arch"

# ── Download ───────────────────────────────────────────────────────
tmp="$(mktemp -d)"
trap 'rm -rf "$tmp"' EXIT INT TERM
curl -fSL --proto '=https' --tlsv1.2 -o "$tmp/agentry" "$url" 2>/dev/null \
  || die "download failed: $url"

# ── Verify checksum (skipped only if SHA256SUMS is unavailable) ─────
if curl -fsSL -o "$tmp/SHA256SUMS" "$AGENTRY_BASE_URL/$AGENTRY_VERSION/SHA256SUMS" 2>/dev/null; then
  want="$(grep " ${bin}\$" "$tmp/SHA256SUMS" 2>/dev/null | awk '{print $1}')"
  if [ -n "$want" ]; then
    if command -v shasum >/dev/null 2>&1; then
      got="$(shasum -a 256 "$tmp/agentry" | awk '{print $1}')"
    elif command -v sha256sum >/dev/null 2>&1; then
      got="$(sha256sum "$tmp/agentry" | awk '{print $1}')"
    else
      got=""
    fi
    if [ -n "$got" ] && [ "$got" != "$want" ]; then
      die "checksum mismatch for $bin (expected $want, got $got)"
    fi
    [ -n "$got" ] && say "checksum verified"
  fi
fi
chmod +x "$tmp/agentry"

# ── Install ────────────────────────────────────────────────────────
mkdir -p "$AGENTRY_PREFIX" 2>/dev/null || true
target="$AGENTRY_PREFIX/agentry"
if [ -w "$AGENTRY_PREFIX" ]; then
  mv "$tmp/agentry" "$target"
elif command -v sudo >/dev/null 2>&1; then
  say "$AGENTRY_PREFIX needs elevated permissions; using sudo"
  sudo mkdir -p "$AGENTRY_PREFIX" && sudo mv "$tmp/agentry" "$target"
else
  die "$AGENTRY_PREFIX is not writable. Re-run with AGENTRY_PREFIX pointing at a writable dir."
fi
say "installed → $target"

# macOS: strip the quarantine flag if a prior browser/zip download set
# it, so Gatekeeper doesn't block the freshly-installed binary.
if [ "$os" = darwin ] && command -v xattr >/dev/null 2>&1; then
  xattr -d com.apple.quarantine "$target" >/dev/null 2>&1 || true
fi

# ── PATH guidance ──────────────────────────────────────────────────
case ":${PATH}:" in
  *":$AGENTRY_PREFIX:"*)
    printf '\n'; say "on your PATH already — run: agentry help"
    ;;
  *)
    shell_name="$(basename "${SHELL:-sh}")"
    case "$shell_name" in
      zsh)  rc="$HOME/.zshrc" ;;
      bash) if [ -f "$HOME/.bash_profile" ]; then rc="$HOME/.bash_profile"; else rc="$HOME/.bashrc"; fi ;;
      fish) rc="$HOME/.config/fish/config.fish" ;;
      *)    rc="$HOME/.profile" ;;
    esac
    printf '\n'
    say "$AGENTRY_PREFIX is not on your PATH. Add it, then reload your shell:"
    if [ "$shell_name" = fish ]; then
      printf '\n    fish_add_path %s\n' "$AGENTRY_PREFIX"
    else
      printf '\n    echo '\''export PATH="%s:$PATH"'\'' >> %s\n    source %s\n' "$AGENTRY_PREFIX" "$rc" "$rc"
    fi
    printf '\n  (or run it now with the full path: %s help)\n' "$target"
    ;;
esac

cat <<'EOF'

Next steps:
  1. Open https://app.agentry.run and add a device — it shows an
     `agentry init …` command containing your token.
  2. Run that command to connect this machine.
  3. Point your AI editor at agentry over MCP:
       { "command": "agentry", "args": ["stdio"] }

Docs: https://docs.agentry.run
EOF
