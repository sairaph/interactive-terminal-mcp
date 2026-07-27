# Interactive Terminal MCP

[![release](https://img.shields.io/github/v/release/sairaph/interactive-terminal-mcp?include_prereleases&label=release)](https://github.com/sairaph/interactive-terminal-mcp/releases)
[![CI](https://github.com/sairaph/interactive-terminal-mcp/actions/workflows/ci.yml/badge.svg)](https://github.com/sairaph/interactive-terminal-mcp/actions/workflows/ci.yml)
[![license](https://img.shields.io/github/license/sairaph/interactive-terminal-mcp)](#license)
[![platform](https://img.shields.io/badge/platform-macOS%20%7C%20Linux%20%7C%20Windows-blue)](#what-it-does)

Give any AI agent a **real terminal** - persistent shell sessions that keep
running between tool calls, handle `vim` and `htop` correctly, and that you can
attach to and watch live - through **8 MCP tools**, with a universal one-command
installer, built-in client detection, and a full TUI for you.

macOS:

```bash
curl -fsSL https://github.com/sairaph/interactive-terminal-mcp/raw/main/install.sh | sh
```

Linux:

```bash
curl -fsSL https://github.com/sairaph/interactive-terminal-mcp/raw/main/install.sh | sh
```

Windows (PowerShell):

```powershell
irm https://github.com/sairaph/interactive-terminal-mcp/raw/main/install.ps1 | iex
```

The installer downloads the right binary for your OS/arch, puts
`interactive-terminal-mcp` on your `PATH`, then opens an interactive configurer:
it detects your AI clients, lets you toggle which ones to wire up with the arrow
keys, and asks how long to keep session logs. Run
`interactive-terminal-mcp configure` anytime to change what's connected.

> **Windows** - the ConPTY and named-pipe paths are compiled and type-checked in
> CI on every build, but the first release has not yet been exercised on a real
> Windows machine. Reports welcome on the
> [issue tracker](https://github.com/sairaph/interactive-terminal-mcp/issues).

## What it does

- **Sessions that outlive the call** - an agent starts a build, does something
  else, and comes back to it. Restarting your AI client doesn't kill anything.
- **Real terminal emulation** - full VT with an alternate screen, scrollback,
  and DEC modes, so `vim`, `less`, `htop`, and `tmux` behave the way they do in
  your own terminal rather than a mangled approximation.
- **Keystrokes, not just commands** - `"CTRL+C"`, `"ESC; :wq; ENTER"`,
  `"DOWN*5"`. Arrow keys are encoded for whatever the running program expects,
  which is why they work *inside* `vim` and not only at a prompt.
- **You can watch** - run `interactive-terminal-mcp` and attach to any session
  the agent is using, live, and type into it yourself. The agent's terminal
  access is visible instead of happening off-screen.
- **Detects 13 AI clients** - Claude Desktop, Claude Code, Cursor, Codex,
  Gemini CLI, Windsurf, Zed, Cline, Roo Code, Amazon Q, Continue, OpenCode, and
  VS Code - and registers itself with the ones you pick.
- **Single static binary** - one CGO-free Go binary, no Python, no Node, no
  runtime deps. Linux, macOS, Windows on x64 and arm64.
- **YAML frontmatter + Markdown output** - every tool result is structured and
  human-readable, so the agent (and you) can scan it at a glance.
- **Nothing truncated silently** - when a response hits its token budget, the
  reply says how many lines were dropped and gives an absolute path to the
  complete log.

## Install

**macOS / Linux** - downloads the matching binary to
`~/.interactive-terminal-mcp/bin` and adds it to your `PATH`:

```bash
curl -fsSL https://github.com/sairaph/interactive-terminal-mcp/raw/main/install.sh | sh
```

**Windows (PowerShell)** - downloads to
`%LOCALAPPDATA%\interactive-terminal-mcp` and adds it to your user `PATH`:

```powershell
irm https://github.com/sairaph/interactive-terminal-mcp/raw/main/install.ps1 | iex
```

Each installer picks the right asset for your OS/arch
(`interactive-terminal-mcp-<os>-<arch>[.exe]`) from the latest
[GitHub Release](https://github.com/sairaph/interactive-terminal-mcp/releases).
**Open a new terminal** afterward so `interactive-terminal-mcp` is found.

There is nothing to log into. The server talks to your own machine, so there is
no account, no token, and no third-party service anywhere in the path.

### Register with your AI clients

```
interactive-terminal-mcp configure
```

This scans your machine for MCP-capable clients (Claude Desktop, Claude Code,
Cursor, Codex, Gemini CLI, Windsurf, Zed, Cline, Roo Code, Amazon Q, Continue,
OpenCode, VS Code) and lets you pick which to wire up with an interactive
checklist. Clients already registered are pre-checked; press `v` to reveal the
ones that aren't installed. Each client's config is written **safely and
idempotently** - your other servers are preserved, and re-running won't
duplicate anything.

An environment that could not be inspected (a permission error, an unsupported
path) is shown with its reason and is never written to, so "could not inspect"
is never quietly treated as "not installed".

Flags:

| Flag | What it does |
| --- | --- |
| `--client <id>` | Register with these clients only; repeatable or comma-separated |
| `--all` | Register with every supported client, detected or not |
| `--yes` | Skip the interactive flow, use detected clients |

Remove it everywhere later with `interactive-terminal-mcp uninstall`.

### Session logs

Setup asks one question, and it is the only one:

```
Session logs — when should logs from closed sessions be deleted?

 > ● When the session is closed   (recommended)
     After 1 hour
     After 4 hours
     After 1 day
     After 1 week
     After 1 month
     Never
```

Logs are what let `it_tail` and `it_head` reach past the visible screen. Running
sessions are never swept, whatever the setting. Change it later with
`interactive-terminal-mcp configure`, or press `c` in the interactive app.

### Manual configuration

Prefer to edit a client config by hand? Point it at the binary plus the `mcp`
subcommand. Use an absolute path - a client launches the server with its own
working directory and `PATH`:

```json
{
  "mcpServers": {
    "interactive-terminal-mcp": {
      "command": "/absolute/path/to/interactive-terminal-mcp",
      "args": ["mcp"]
    }
  }
}
```

Settings live in `~/.interactive-terminal-mcp/config.toml` and are shared by
every client, so there is nothing to duplicate per client.

## Tools

All **8 tools** operate on persistent terminal sessions. A session is addressed
by its id (`t-k3f9qa`) or the name you gave it (`build`); omit it and the tool
uses the active session.

### Sessions

| Tool | Description |
| --- | --- |
| `it_active` | Report which session is active, or switch to another, and return its screen |
| `it_list` | List all sessions, running and recently ended, newest activity first |
| `it_new` | Create a session, make it active, return its first screen |
| `it_kill` | End a session (**requires** an explicit session; never inferred) |

### Using a session

| Tool | Description |
| --- | --- |
| `it_read` | Return a session's current screen, optionally resizing first |
| `it_send` | Type text and/or keystrokes, then return the screen |
| `it_tail` | Recent log lines plus the live screen |
| `it_head` | Earliest log lines |

A typical agent flow:

```js
it_active({})                                   // is a session already open?
it_new({"name": "dev"})                         // no - create one
it_send({"text": "npm run dev", "wait": 10})    // start the server
it_read({"session": "dev"})                     // check on it later
it_tail({"session": "dev", "lines": 50})        // what scrolled past?
it_kill({"session": "dev"})                     // done
```

### Keystrokes

`it_send`'s `keys` argument takes semicolon-separated chords:

| Form | Example |
| --- | --- |
| Named keys | `ENTER` `TAB` `ESC` `HOME` `END` `PAGE_UP` `UP` `F1`-`F20` |
| Modifiers | `CTRL+C`, `ALT+X`, `SHIFT+TAB`, and combinations |
| Repeats | `DOWN*5` |
| Literal text | `"hello world"`, or an unquoted run like `:wq` |

```js
it_send({"keys": "i; \"hello from vim\"; ESC"})
it_send({"keys": "ESC; :wq; ENTER"})
it_send({"keys": "CTRL+B; PAGE_UP"})
```

Arrows and Home/End are encoded according to the modes the running program has
actually set, so they behave correctly inside `vim` and `less` rather than only
at a shell prompt. A typo like `PAGEUPP` is reported with the list of valid
names, and nothing is sent.

### Waiting is a ceiling, not a sleep

`wait` is the maximum time a call will spend waiting for output to settle. It
returns as soon as output goes quiet, so `wait: 30` costs milliseconds for
`echo hi` and is still correct for a slow build. When the budget runs out with
output still arriving, the result says `settled: false` and tells the agent to
look again - it never quietly returns a half-drawn screen.

### Full-screen programs

Output from a program using the alternate screen never scrolls, so it is
correctly absent from the log. That is why `it_tail` returns the live screen
alongside the log by default, and labels the two:

````markdown
~~~text
  CC   src/parser.o
make: *** [Makefile:42: all] Error 1
~~~

Showing the last 138 of 500 requested lines (1842 in the log). 362 older lines
were omitted to fit the response budget. The complete log is at
`~/.interactive-terminal-mcp/sessions/t-k3f9qa/transcript.log`.
````

## The interactive app

Run `interactive-terminal-mcp` in a terminal with no arguments:

```
╭─ interactive-terminal-mcp ──────────────────────────────────────────────╮
│                                                                          │
│  > + New session                                                         │
│                                                                          │
│    ● build       t-k3f9qa   running   120x30    3s ago      1842 lines   │
│    ● dev-server  t-w81mza   running   120x30    1m ago      412 lines    │
│    ○ (unnamed)   t-p2m8wd   exit 0    120x30    26m ago     96 lines     │
│                                                                          │
╰──────────────────────────────────────────────────────────────────────────╯
  ↑↓ navigate · enter open · n new · backspace delete · r rename
  a set active · c configure · q quit
```

Press enter on a session to open it in a framed terminal. It behaves like a
normal terminal, with a few additions:

- **A composer** under the frame for typing commands: `shift+enter` for a
  newline, `↑` for history, `ctrl+v` to paste, `tab` for shell completion. It
  grows as you type and starts scrolling at 80% of the window height.
- **Automatic raw mode** - the moment a program takes the alternate screen,
  every keystroke goes straight to it, so `vim` and `htop` just work. `ctrl+g`
  toggles manually for programs that read keys without using the alternate
  screen.
- **Scrollback** with `shift+pgup` / `shift+pgdn` or the mouse wheel.
- `ctrl+q` returns to the list. Quitting the app leaves every session running.

Deleting asks first, and tells you what you would lose:

```
  Delete session "build" (t-k3f9qa)?

  It is still running: make -j8
  Its logs will be deleted immediately (retention: when the session is closed).

  > Cancel
    Delete
```

Press `c` from the list for the same settings screen the installer shows:
harness registration, terminal size, response budgets, log retention,
scrollback, session limits. `d` restores the recommended defaults after showing
exactly which values would change.

## CLI

Every operation is also a one-shot command:

| Command | What it does |
| --- | --- |
| `ls` | List sessions |
| `new [name] [--cmd C]` | Create a session (`--cwd`, `--cols`, `--rows`, `--wait`) |
| `attach <session>` | Open a session in this terminal |
| `read [session]` | Print a session's screen (`--wait`) |
| `send [session] --text T` | Type into a session (`--no-enter`, `--wait`) |
| `send [session] --keys K` | Send keystrokes |
| `tail [session] [-n N]` | Recent output |
| `head [session] [-n N]` | Earliest output |
| `rename <session> <name>` | Rename a session |
| `kill <session>` | End a session (`--signal TERM\|INT\|HUP\|KILL`) |
| `status` | Daemon status |
| `doctor` | Diagnose the installation |
| `config` | Show settings and their paths |

`kill` requires a session for the same reason `it_kill` does: ending the wrong
terminal cannot be undone.

## How it works

Sessions have to outlive the process that created them, so the PTYs are owned by
a small per-user **daemon**. Every MCP server, CLI command, and the app itself
are thin clients of it over a local socket.

```
┌──────────────┐   ┌──────────────┐   ┌──────────────┐
│ Claude Code  │   │ Cursor       │   │ you (TUI)    │
└──────┬───────┘   └──────┬───────┘   └──────┬───────┘
       └──────────────────┼──────────────────┘
                   ┌──────▼───────┐
                   │    daemon    │  owns PTYs, emulators, logs
                   └──────┬───────┘
              ┌───────────┼───────────┐
          ┌───▼───┐   ┌───▼───┐   ┌───▼───┐
          │ pty 1 │   │ pty 2 │   │ pty 3 │
          └───────┘   └───────┘   └───────┘
```

It starts automatically on first use and exits after 60 seconds with no sessions
and no clients. You never start or manage it, but
`interactive-terminal-mcp status` and `doctor` will tell you about it.

The socket is mode `0600` in a `0700` directory, or a per-user named pipe with an
owner-only DACL on Windows. No TCP port is ever opened.

### Trust model

This is a local tool that runs with your authority. `it_send` can run anything
you can run: there is no allowlist, no sandbox, and no command filtering. That
is the feature - it is your console, reachable by your agent. Control what the
agent may do through your AI client's own permission system.

## Documentation

- [`docs/tool-contract.md`](docs/tool-contract.md) - the complete agent-facing
  contract: every argument, every frontmatter field, every error code
- [`docs/app-design.md`](docs/app-design.md) - the interactive application
- [`docs/implementation-plan.md`](docs/implementation-plan.md) - architecture,
  the reasoning behind it, and what changed while building it

## Build from source

Requires [Go 1.26+](https://go.dev/dl/):

```bash
git clone https://github.com/sairaph/interactive-terminal-mcp.git
cd interactive-terminal-mcp
go test ./...
go build -o interactive-terminal-mcp .
./interactive-terminal-mcp configure
```

Cross-compile all six targets with `CGO_ENABLED=0`:

```bash
for t in linux-amd64 linux-arm64 darwin-amd64 darwin-arm64 windows-amd64 windows-arm64; do
  CGO_ENABLED=0 GOOS="${t%-*}" GOARCH="${t#*-}" go build -ldflags="-s -w" -o "interactive-terminal-mcp-$t" .
done
```

Releases are produced automatically: bump the version in
[`VERSION`](VERSION) and merge to `main`, and the `autorelease` workflow tags
`v<version>` and publishes binaries for all six targets. The race detector runs
on every CI job, not only on release.

## License

MIT. Copyright © 2026 [Łael Al-Halawani](mailto:laelhalawani@gmail.com).
