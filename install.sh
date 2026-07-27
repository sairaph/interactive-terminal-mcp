#!/bin/sh
# interactive-terminal-mcp installer (Linux / macOS). Run with:
#   curl -fsSL https://github.com/sairaph/interactive-terminal-mcp/raw/main/install.sh | sh
set -e

OWNER="sairaph"
REPO="interactive-terminal-mcp"
BIN="interactive-terminal-mcp"

# --- detect OS / arch ------------------------------------------------------
OS="$(uname -s)"
ARCH="$(uname -m)"
case "$OS" in
  Linux*)  os=linux ;;
  Darwin*) os=darwin ;;
  *) printf 'Unsupported OS: %s\n' "$OS" >&2; exit 1 ;;
esac
case "$ARCH" in
  x86_64|amd64)  arch=amd64 ;;
  arm64|aarch64) arch=arm64 ;;
  *) printf 'Unsupported architecture: %s\n' "$ARCH" >&2; exit 1 ;;
esac

ASSET="${BIN}-${os}-${arch}"
URL="https://github.com/${OWNER}/${REPO}/releases/latest/download/${ASSET}"

# --- install location ------------------------------------------------------
INSTALL_DIR="$HOME/.${REPO}/bin"
TARGET="$INSTALL_DIR/$BIN"
mkdir -p "$INSTALL_DIR"

printf '\n  %s installer\n\n  Downloading...\n\n' "$BIN"

# A running daemon holds the old binary open. Ask it to stop first so an
# upgrade never races a live process.
if [ -x "$TARGET" ]; then
  "$TARGET" daemon --stop >/dev/null 2>&1 || true
fi

# Download beside the target so a failure never leaves a half-written binary
# where the shell would find it.
TEMP="${TARGET}.new"
trap 'rm -f "$TEMP"' EXIT HUP INT TERM

# curl --progress-bar writes the bar to stderr so it shows under `sh` but
# never pollutes the captured stdout of `curl | sh`.
if command -v curl >/dev/null 2>&1; then
  if ! curl -fSL --progress-bar "$URL" -o "$TEMP"; then
    printf '\n  Download failed. Please check your connection and try again.\n  URL: %s\n' "$URL" >&2
    exit 1
  fi
elif command -v wget >/dev/null 2>&1; then
  if ! wget -q --show-progress -O "$TEMP" "$URL"; then
    printf '\n  Download failed. Please check your connection and try again.\n  URL: %s\n' "$URL" >&2
    exit 1
  fi
else
  printf '  Neither curl nor wget is available; cannot download.\n' >&2
  exit 1
fi

if [ ! -s "$TEMP" ]; then
  printf '  Download did not complete; nothing was installed.\n' >&2
  exit 1
fi
chmod +x "$TEMP"
mv -f "$TEMP" "$TARGET"
trap - EXIT HUP INT TERM

# --- add to PATH if missing (silent on success) ----------------------------
case ":$PATH:" in
  *":$INSTALL_DIR:"*) on_path=1 ;;
  *) on_path=0 ;;
esac

if [ "$on_path" -eq 0 ]; then
  line="export PATH=\"$INSTALL_DIR:\$PATH\""
  # The trailing `|| true` matters: if none of these files exist, the loop's
  # status is the last failed `[ -f ]`, and `set -e` would kill the script
  # here -- after installing the binary but before saying anything.
  for rc in "$HOME/.zshrc" "$HOME/.bashrc" "$HOME/.profile" "$HOME/.bash_profile"; do
    [ -f "$rc" ] || continue
    if ! grep -qF "$INSTALL_DIR" "$rc" 2>/dev/null; then
      printf '\n# added by %s installer\n%s\n' "$BIN" "$line" >> "$rc"
    fi
    on_path=2
    break
  done || true
fi

# If we have a controlling terminal, run the interactive setup right away.
# Under `curl | sh` stdin is the script pipe, so read from /dev/tty to reach
# the user's terminal directly.
#
# Testing for the device file is not enough: /dev/tty exists inside containers
# and CI runners but fails to open with ENXIO when there is no controlling
# terminal. Opening it is the only reliable check.
#
# The subshell is required. A redirection failure on a special built-in such
# as `:` makes a POSIX shell exit outright, so `{ : </dev/tty; }` would kill
# the installer with status 2 right after it had written the binary. Inside a
# subshell the exit is contained and simply reports false.
if ( : </dev/tty ) 2>/dev/null; then
  "$TARGET" configure </dev/tty || \
    printf '  (configure skipped or failed; run `%s configure` later)\n' "$BIN"
else
  printf '\n  Not running on a terminal. Finish setup with:\n    %s configure\n' "$BIN"
fi

if [ "$on_path" -eq 0 ]; then
  printf '\n  Add this to your shell profile:\n    export PATH="%s:$PATH"\n' "$INSTALL_DIR"
elif [ "$on_path" -eq 2 ]; then
  printf '\n  Open a new terminal so `%s` is on your PATH.\n' "$BIN"
fi
