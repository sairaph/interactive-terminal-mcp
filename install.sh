#!/bin/sh
set -eu

owner="sairaph"
repo="interactive-terminal-mcp"
binary="interactive-terminal-mcp"

case "$(uname -s)" in
  Linux*) os="linux" ;;
  Darwin*) os="darwin" ;;
  *) printf 'Unsupported operating system: %s\n' "$(uname -s)" >&2; exit 1 ;;
esac

case "$(uname -m)" in
  x86_64|amd64) arch="amd64" ;;
  arm64|aarch64) arch="arm64" ;;
  *) printf 'Unsupported architecture: %s\n' "$(uname -m)" >&2; exit 1 ;;
esac

install_dir="${HOME}/.${repo}/bin"
target="${install_dir}/${binary}"
asset="${binary}-${os}-${arch}"
url="https://github.com/${owner}/${repo}/releases/latest/download/${asset}"

mkdir -p "$install_dir"
temporary="${target}.new"
trap 'rm -f "$temporary"' EXIT HUP INT TERM

printf 'Downloading %s...\n' "$asset"
if command -v curl >/dev/null 2>&1; then
  curl -fL --progress-bar "$url" -o "$temporary"
elif command -v wget >/dev/null 2>&1; then
  wget -O "$temporary" "$url"
else
  printf 'curl or wget is required.\n' >&2
  exit 1
fi
chmod 755 "$temporary"

# An in-place replacement would break a daemon that is currently running from
# this path, so any existing one is stopped before the binary changes.
if [ -x "$target" ]; then
  "$target" daemon --stop >/dev/null 2>&1 || true
fi
mv -f "$temporary" "$target"
trap - EXIT HUP INT TERM

case ":${PATH}:" in
  *":${install_dir}:"*) ;;
  *)
    profile="${HOME}/.profile"
    [ -f "${HOME}/.zshrc" ] && profile="${HOME}/.zshrc"
    [ -f "${HOME}/.bashrc" ] && profile="${HOME}/.bashrc"
    if ! grep -qF "$install_dir" "$profile" 2>/dev/null; then
      printf '\n# added by %s installer\nexport PATH="%s:$PATH"\n' "$repo" "$install_dir" >> "$profile"
      printf 'Added %s to your PATH in %s\n' "$install_dir" "$profile"
    fi
    ;;
esac

printf 'Installed %s\n' "$target"

# The installer is usually piped from curl, so stdin is the script itself.
# /dev/tty is the user's actual keyboard, which the setup flow needs.
if [ -e /dev/tty ]; then
  "$target" configure </dev/tty >/dev/tty 2>&1 || \
    printf 'Run `%s configure` later to choose your AI clients.\n' "$binary"
else
  printf 'Run `%s configure` to choose your AI clients.\n' "$binary"
fi

printf '\nOpen a new terminal, then run `%s` to browse your sessions.\n' "$binary"
