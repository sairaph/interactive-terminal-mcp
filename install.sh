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

# The progress display is rendered here rather than left to curl, so that the
# Linux and Windows installers show a person the same thing: a bar, a
# percentage, how much of how large, the rate, and how long is left. curl's own
# bar has none of that detail, and the two installers looking different is the
# kind of thing that makes an install feel improvised.
#
# It is drawn on stderr so that `curl ... | sh` cannot mistake it for output.
# temp_size reports how much has landed so far.
#
# The existence check is not defensive padding: curl has not created the file
# during the first poll, and a shell reports a failed input redirection itself,
# before any 2>/dev/null on the command can apply. Without this the download
# printed a screen of "cannot open" between the bar's own updates.
temp_size() {
  if [ -f "$TEMP" ]; then
    wc -c < "$TEMP" 2>/dev/null || echo 0
  else
    echo 0
  fi
}

render_progress() {
  # $1 bytes so far, $2 bytes expected, $3 tenths of a second elapsed
  progress_have=$1
  progress_total=$2
  progress_tenths=$3

  progress_pct=0
  if [ "$progress_total" -gt 0 ]; then
    progress_pct=$((progress_have * 100 / progress_total))
  fi
  if [ "$progress_pct" -gt 100 ]; then
    progress_pct=100
  fi

  progress_filled=$((progress_pct / 5))
  progress_bar=""
  progress_i=0
  while [ "$progress_i" -lt 20 ]; do
    if [ "$progress_i" -lt "$progress_filled" ]; then
      progress_bar="${progress_bar}#"
    else
      progress_bar="${progress_bar} "
    fi
    progress_i=$((progress_i + 1))
  done

  # Tenths throughout: a POSIX shell has no floating point, and one decimal
  # is all the display shows.
  progress_have10=$((progress_have * 10 / 1048576))
  progress_total10=$((progress_total * 10 / 1048576))
  progress_rate=0
  progress_eta=0
  if [ "$progress_tenths" -gt 0 ] && [ "$progress_have" -gt 0 ]; then
    progress_bps=$((progress_have * 10 / progress_tenths))
    progress_rate=$((progress_bps * 10 / 1048576))
    if [ "$progress_bps" -gt 0 ] && [ "$progress_total" -gt "$progress_have" ]; then
      progress_eta=$(((progress_total - progress_have) / progress_bps))
    fi
  fi

  # Padded so a shorter line always overwrites the one before it.
  printf '\r  [%s] %3d%%  %d.%d/%d.%d MB  %d.%d MB/s  ETA %02ds        ' \
    "$progress_bar" "$progress_pct" \
    $((progress_have10 / 10)) $((progress_have10 % 10)) \
    $((progress_total10 / 10)) $((progress_total10 % 10)) \
    $((progress_rate / 10)) $((progress_rate % 10)) \
    "$progress_eta" >&2
}

download_with_progress() {
  # The size has to be known before a proportion can be shown, so it is asked
  # for separately. Redirects are followed, and the last Content-Length seen
  # is the one that describes the file itself.
  progress_expected=$(curl -fsSLI "$URL" 2>/dev/null \
    | tr -d '\r' | tr '[:upper:]' '[:lower:]' \
    | awk '/^content-length:/ { value = $2 } END { print value + 0 }')
  [ -n "$progress_expected" ] || progress_expected=0
  if [ "$progress_expected" -le 0 ]; then
    return 1
  fi

  # Sub-second polling where the shell's sleep allows it. Where it does not,
  # a one-second tick still renders a correct bar, just less smoothly.
  if sleep 0.2 2>/dev/null; then
    progress_step="0.2"
    progress_step_tenths=2
  else
    progress_step="1"
    progress_step_tenths=10
  fi

  curl -fsSL "$URL" -o "$TEMP" &
  progress_pid=$!

  progress_elapsed=0
  while kill -0 "$progress_pid" 2>/dev/null; do
    sleep "$progress_step"
    progress_elapsed=$((progress_elapsed + progress_step_tenths))
    progress_size=$(temp_size)
    render_progress "$progress_size" "$progress_expected" "$progress_elapsed"
  done

  set +e
  wait "$progress_pid"
  progress_status=$?
  set -e
  [ "$progress_status" -eq 0 ] || return "$progress_status"

  progress_size=$(temp_size)
  render_progress "$progress_size" "$progress_expected" "$progress_elapsed"
  printf '\n' >&2
  return 0
}

if command -v curl >/dev/null 2>&1; then
  if ! download_with_progress; then
    # Either the size was not offered, or the download itself failed. A
    # retry with curl's own bar keeps the install working rather than
    # failing over a missing header.
    if ! curl -fSL --progress-bar "$URL" -o "$TEMP"; then
      printf '\n  Download failed. Please check your connection and try again.\n  URL: %s\n' "$URL" >&2
      exit 1
    fi
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

# Stop the daemon again, now that the new binary is the one on disk.
#
# The stop before the download releases the old executable, but it cannot be
# the last word: the download takes seconds, and any tool call an AI client
# makes in that window starts a daemon from the binary that is still there --
# the old one. It would then keep serving old code long after this script
# reported success, which reads as an upgrade that silently did nothing.
#
# Stopping here closes that window. Whatever is running is asked to stop, and
# the next tool call starts the version just installed.
stopped=$("$TARGET" daemon --stop 2>/dev/null) || stopped=""
case "$stopped" in
  *"No session daemon"*|"") ;;
  *)
    printf '\n  %s\n' "$stopped"
    # The daemon carries session behaviour and restarts on the next tool call,
    # so that part is already current. Tool descriptions come from the MCP
    # server process the AI client spawned, which is still the old binary and
    # will be until the client restarts it.
    printf '  Restart your AI client so it picks up the new tool descriptions.\n'
    ;;
esac

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
