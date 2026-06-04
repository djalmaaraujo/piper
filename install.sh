#!/usr/bin/env bash
#
# Piper installer — downloads the piper CLI to your PATH.
#
#   curl -fsSL https://raw.githubusercontent.com/djalmaaraujo/piper/main/install.sh | bash
#
# Env overrides:
#   PIPER_INSTALL_DIR   target directory (default: /usr/local/bin, falls back to ~/.local/bin)
#   PIPER_REF           git ref/branch/tag to install from (default: main)

set -euo pipefail

REPO="djalmaaraujo/piper"
REF="${PIPER_REF:-main}"
RAW="https://raw.githubusercontent.com/${REPO}/${REF}/stream.js"

# --- preflight ---------------------------------------------------------------
if ! command -v node >/dev/null 2>&1; then
  echo "Error: Node.js is required but not found. Install Node 16+ first: https://nodejs.org" >&2
  exit 1
fi

# --- pick an install dir we can actually write to ----------------------------
choose_dir() {
  if [ -n "${PIPER_INSTALL_DIR:-}" ]; then
    echo "$PIPER_INSTALL_DIR"; return
  fi
  if [ -w "/usr/local/bin" ] || { [ ! -e "/usr/local/bin/piper" ] && [ -w "/usr/local" ]; }; then
    echo "/usr/local/bin"; return
  fi
  echo "$HOME/.local/bin"
}

INSTALL_DIR="$(choose_dir)"
TARGET="${INSTALL_DIR}/piper"

mkdir -p "$INSTALL_DIR"

echo "Downloading piper from ${REPO}@${REF}..."
if command -v curl >/dev/null 2>&1; then
  curl -fsSL "$RAW" -o "$TARGET"
elif command -v wget >/dev/null 2>&1; then
  wget -qO "$TARGET" "$RAW"
else
  echo "Error: need curl or wget to download." >&2
  exit 1
fi

chmod +x "$TARGET"

echo "✓ Installed piper to ${TARGET}"

# --- PATH hint ---------------------------------------------------------------
case ":${PATH}:" in
  *":${INSTALL_DIR}:"*) ;;
  *)
    echo ""
    echo "⚠  ${INSTALL_DIR} is not on your PATH. Add this to your shell profile:"
    echo "    export PATH=\"${INSTALL_DIR}:\$PATH\""
    ;;
esac

echo ""
echo "Try it:  piper echo hello"
