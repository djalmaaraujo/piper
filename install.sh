#!/usr/bin/env bash
#
# Piper installer — downloads a prebuilt piper binary into your PATH.
# No Node, no Go, no compiler required.
#
#   curl -fsSL https://raw.githubusercontent.com/djalmaaraujo/piper/main/install.sh | bash
#
# Env overrides:
#   PIPER_INSTALL_DIR   target dir (default: /usr/local/bin, falls back to ~/.local/bin)
#   PIPER_VERSION       release tag to install (default: latest)

set -euo pipefail

REPO="djalmaaraujo/piper"
VERSION="${PIPER_VERSION:-latest}"

# --- detect platform ---------------------------------------------------------
os="$(uname -s)"
arch="$(uname -m)"

case "$os" in
  Darwin) goos="darwin" ;;
  Linux)  goos="linux" ;;
  *) echo "Error: unsupported OS '$os'. On Windows, download the .zip from the Releases page." >&2; exit 1 ;;
esac

case "$arch" in
  x86_64|amd64) goarch="amd64" ;;
  arm64|aarch64) goarch="arm64" ;;
  *) echo "Error: unsupported architecture '$arch'." >&2; exit 1 ;;
esac

ASSET="piper_${goos}_${goarch}.tar.gz"
if [ "$VERSION" = "latest" ]; then
  URL="https://github.com/${REPO}/releases/latest/download/${ASSET}"
else
  URL="https://github.com/${REPO}/releases/download/${VERSION}/${ASSET}"
fi

# --- choose a writable install dir -------------------------------------------
choose_dir() {
  if [ -n "${PIPER_INSTALL_DIR:-}" ]; then echo "$PIPER_INSTALL_DIR"; return; fi
  if [ -w "/usr/local/bin" ]; then echo "/usr/local/bin"; return; fi
  echo "$HOME/.local/bin"
}
INSTALL_DIR="$(choose_dir)"
mkdir -p "$INSTALL_DIR"

# --- download + extract ------------------------------------------------------
tmp="$(mktemp -d)"
trap 'rm -rf "$tmp"' EXIT

echo "Downloading ${ASSET} (${VERSION})..."
if command -v curl >/dev/null 2>&1; then
  curl -fsSL "$URL" -o "$tmp/piper.tar.gz"
elif command -v wget >/dev/null 2>&1; then
  wget -qO "$tmp/piper.tar.gz" "$URL"
else
  echo "Error: need curl or wget." >&2; exit 1
fi

tar -xzf "$tmp/piper.tar.gz" -C "$tmp"
install -m 0755 "$tmp/piper" "$INSTALL_DIR/piper"

echo "✓ Installed piper to ${INSTALL_DIR}/piper"

# --- PATH hint ---------------------------------------------------------------
case ":${PATH}:" in
  *":${INSTALL_DIR}:"*) ;;
  *)
    echo ""
    echo "⚠  ${INSTALL_DIR} is not on your PATH. Add to your shell profile:"
    echo "    export PATH=\"${INSTALL_DIR}:\$PATH\""
    ;;
esac

echo ""
echo "Try it:  piper echo hello"
