# Implementation Plan

Companion to [`tool-contract.md`](tool-contract.md), which defines the
agent-facing surface, and [`app-design.md`](app-design.md), which covers the
interactive application. This document records the architecture, the
dependencies, and the reasoning behind both.

**Status: built and tested.** Everything below is implemented. Section 12
records what changed against the original plan and why.

## 1. Dependency Decisions

Every choice below was resolved and compiled against a live PTY before this
plan was written.

| Concern | Choice | Why |
|---|---|---|
| PTY | `github.com/aymanbagabas/go-pty` v0.2.3 | One API over Unix PTYs and Windows ConPTY. `pty.New()`, `Command`, `Resize`, `io.ReadWriter`. CGO-free. |
| Terminal emulator | `github.com/charmbracelet/x/vt` | Full VT: alternate screen, scrollback, DEC modes, SGR, OSC titles, mouse, DCS. `SafeEmulator` is concurrency-safe. `Render()`, `CellAt`, `IsAltScreen()`, `Scrollback()`. |
| MCP | `github.com/modelcontextprotocol/go-sdk` v1.6.1 | Same version as `apis-mcp` and `favro-mcp`. |
| Installer | `github.com/sairaph/detect-harness` v0.1.0 | Detects 13 harnesses, plans, applies safely. Replaces the hand-rolled `internal/install` in both reference projects. |
| Tokenizer | `github.com/tiktoken-go/tokenizer` v0.8.1 | `o200k_base`, same as `apis-mcp`. |
| TUI | `bubbletea` v1.3.10 + `lipgloss` v1.1.0 + `bubbles` | Same as both reference projects. |
| Config | `github.com/pelletier/go-toml/v2` | Same as both reference projects. |
| YAML | `gopkg.in/yaml.v3` | Frontmatter rendering. |
| File locking | `github.com/gofrs/flock` | Daemon singleton lock, same as `apis-mcp`. |

Verified: `CGO_ENABLED=0 go build` succeeds for `linux/amd64`, `linux/arm64`,
`darwin/amd64`, `darwin/arm64`, `windows/amd64`, `windows/arm64` with the full
stack linked. A live `go-pty` + `x/vt` round-trip renders styled output,
reports cursor position, alternate-screen state, and scrollback length
correctly.

Two notes carried into the implementation:

- `x/vt` is published only as a pseudo-version
  (`v0.0.0-20260726004341-482a56510f1b`). Pin it exactly, and keep it behind
  the `internal/vterm` interface described in §3 so it can be swapped for
  `hinshun/vt10x` if Charm churns the API.
- `Emulator.Render()` emits SGR escape sequences. Plain-text snapshots walk
  cells through `CellAt` instead; `Render()` is reserved for the future
  `ansi: true` mode and for the human application's attach replay.

## 2. Repository Layout

```
interactive-terminal-mcp/
├── .env                        # gitignored; GITHUB_PAT for sairaph/interactive-terminal-mcp
├── .github/workflows/
│   ├── ci.yml                  # vet, test, race, 6-target cross-build
│   ├── release.yml             # reusable; tag or version input -> 6 binaries + release
│   └── autorelease.yml         # VERSION bump on main -> tag -> release
├── .gitignore
├── AGENTS.md
├── LICENSE                     # MIT
├── README.md
├── VERSION
├── install.sh
├── install.ps1
├── docs/
│   ├── tool-contract.md
│   └── implementation-plan.md
├── main.go
└── internal/
    ├── app/            # human full-screen application (bubbletea)
    ├── budget/         # o200k_base counting, truncation, pagination
    ├── cli/            # argument parsing, one-shot commands, human rendering
    ├── config/         # ~/.interactive-terminal-mcp/config.toml
    ├── daemon/         # session-owning server: listener, registry, lifecycle
    ├── fsx/            # atomic replace, unix + windows
    ├── ipc/            # wire protocol, client, autostart, framing
    ├── install/        # detect-harness driver + installer TUI
    ├── keys/           # key-chord language parser and mode-aware encoder
    ├── mcpserver/      # 8 MCP tools, schemas, frontmatter, bodies
    ├── render/         # Document (frontmatter + body), Fence
    ├── session/        # PTY + emulator + logs + subscribers (one per session)
    └── vterm/          # emulator interface, x/vt adapter, plain-text extraction
```

`internal/budget`, `internal/render`, and `internal/fsx` are lifted from
`apis-mcp` essentially unchanged. `internal/config` follows its shape.

## 3. Package Design

### `internal/vterm` — emulator abstraction

Narrow interface so the emulator stays swappable and so plain-text extraction
lives in one place.

```go
type Terminal interface {
    Write(p []byte) (int, error)
    Resize(cols, rows int)
    Size() (cols, rows int)
    Snapshot() Snapshot          // plain text + metadata, taken atomically
    ScrollbackLines() int
    TakeEvictedLines() []string  // lines that scrolled off since last call
    IsAltScreen() bool
    Title() string
    CursorPosition() (row, col int)
}

type Snapshot struct {
    Lines             []string
    Cursor            [2]int
    AltScreen         bool
    Title             string
    BlankLinesTrimmed int
}
```

The `x/vt` adapter walks `CellAt(x, y)` across the bounds, concatenating
`Cell.Content` and skipping continuation cells of wide graphemes, then strips
trailing whitespace per line and counts trailing blank lines without deleting
interior ones.

`TakeEvictedLines` is the transcript source. `x/vt` maintains its own
scrollback ring; the adapter tracks `ScrollbackLen()` between calls and reads
the newly pushed lines out of `Scrollback().Line(i)`, converting them to text.
Scrollback size is set to `max(2 * transcript_flush_lines, 10000)` so a burst
of output cannot evict lines before they are captured.

### `internal/session` — one live terminal

Owns the PTY, one emulator, the log writers, and the fan-out to subscribers.

```go
type Session struct {
    ID, Name string
    Command  []string
    Cwd      string
    Created  time.Time

    pty   pty.Pty
    cmd   *pty.Cmd
    term  vterm.Terminal
    logs  *logWriter
    subs  *broadcaster
    // …
}
```

One reader goroutine per session is the single writer to the emulator:

```
pty.Read(buf)
  ├─> raw.log            (append, exact bytes)
  ├─> emulator.Write     (single-writer; parser state stays consistent)
  ├─> broadcaster.Push   (copy to each attached human client)
  └─> activity.Signal    (wakes anything waiting for settle)
```

Because that goroutine is the only writer, a snapshot taken under the
emulator's read lock is always at a byte boundary. It can still land mid-frame,
which is why the settle mechanism exists rather than a fixed sleep.

**Settle.** `WaitSettled(ctx, budget, quiet)` blocks until no bytes have
arrived for `quiet` (default 250 ms), or `budget` elapses, or the child exits.
It reports which. A 30 ms coalesce always runs before rendering so a redraw in
flight is not captured half-finished.

**Broadcaster.** Attached human clients each get a bounded channel. A client
that cannot keep up has its buffer collapsed to "resync": it is sent a clear
plus a full `Render()` of the current screen instead of a backlog. The agent's
snapshot path never goes through the broadcaster, so a stalled human attach
cannot slow down tool calls.

**Logs.** Two files per session under
`~/.interactive-terminal-mcp/sessions/<id>/`:

| File | Content | Cap |
|---|---|---|
| `raw.log` | Exact PTY bytes. Feeds human attach replay and debugging. | 32 MiB, rotates once to `raw.log.1` |
| `transcript.log` | UTF-8 text, one line per line evicted from the top of the screen, plus the final screen on exit. Feeds `it_tail` / `it_head`. | 64 MiB / 200k lines, rotates once |
| `meta.json` | id, name, command, cwd, size, timestamps, exit code. Lets a restarted daemon list retained sessions. | — |

Both are buffered and flushed on a 200 ms timer, on settle, and on exit.
`it_head` seeks from the start; `it_tail` reads backwards in 64 KiB chunks so a
large transcript is not loaded to answer a 100-line request.

Alternate-screen output never enters `transcript.log`, because it never
scrolls. That is correct terminal behavior and is documented in the contract.

**Exit.** On child exit the final screen is appended to the transcript, both
files are flushed and closed, `meta.json` records `exit_code` and `exited_at`,
the session flips to `exited`, and retention is scheduled.

### `internal/keys` — key language

Pure, table-driven, no I/O. `Parse(string) ([]Chord, error)` then
`Encode(chords, ModeState) []byte`.

`ModeState` carries DECCKM (application cursor keys), keypad mode, and
bracketed-paste state, read from the emulator at send time. Arrows, `HOME`, and
`END` encode as `SS3` under DECCKM and as `CSI` otherwise; that single detail
is the difference between arrow keys working inside `vim` and `less` and only
working at a shell prompt.

`CTRL+<letter>` maps to the control code; `CTRL+` on non-letters follows xterm
conventions (`CTRL+SPACE` → `NUL`, `CTRL+[` → `ESC`, `CTRL+\` → `FS`).
`ALT+<x>` prefixes `ESC`. `SHIFT+` on named keys uses the xterm modifier
parameter form (`CSI 1;2 A`).

Parsing is total before anything is written: an invalid chord anywhere aborts
the whole send.

### `internal/ipc` — client/daemon wire

Newline-delimited JSON, one request object and one response object per line,
over a Unix domain socket or a Windows named pipe. Deliberately not MCP: it
carries streaming attach traffic that MCP has no shape for.

```json
{"v":1,"id":7,"op":"session.new","args":{"name":"build","cols":120,"rows":30}}
{"v":1,"id":7,"ok":true,"result":{ ... }}
```

Operations: `ping`, `session.list`, `session.new`, `session.read`,
`session.send`, `session.kill`, `session.log`,
`session.resize`, `attach.open`, `attach.input`, `attach.close`,
`daemon.status`, `daemon.stop`.

`attach.open` upgrades that one connection to a streaming channel: the daemon
pushes framed output chunks until the client closes it. Everything else is
strict request/response.

**Autostart.** `ipc.Dial` tries the socket; on `ENOENT` or `ECONNREFUSED` it
removes a stale socket, re-execs the current binary as
`interactive-terminal-mcp daemon --detach`, and retries with backoff for up to
5 seconds. Detaching reuses the `detach_unix.go` / `detach_windows.go` pattern
already in `apis-mcp/internal/cmd/ingest`.

**Singleton.** The daemon takes an exclusive `flock` on
`~/.interactive-terminal-mcp/daemon.lock` before binding. Losing the race means
another daemon won; the loser exits quietly and the client connects to the
winner.

### `internal/daemon` — the server

Holds the session registry, the retention sweeper,
and the listener. Single-threaded registry guarded by one mutex; per-session
work happens in that session's own goroutines.

Shutdown: `daemon.stop`, `SIGTERM`, or the idle rule (zero sessions and zero
clients for 60 s). Shutdown flushes every log, writes `meta.json`, and closes
the socket. It does **not** kill running sessions on `SIGTERM` from a client
disconnect — only an explicit `daemon.stop --kill` does, and the human
application confirms first.

Startup recovery: session directories on disk whose processes are gone are
loaded as `exited` records so `it_list`, `it_tail`, and `it_head` still work
after a daemon restart. Their PTYs are not resurrected.

### `internal/mcpserver` — the eight tools

Mirrors `apis-mcp/internal/mcpserver` exactly in shape:

- `service.go` — `New`, `registerTools`, per-tool handlers, JSON Schemas built
  with the same `objectSchema` / `stringProperty` / `pageProperty` helpers, and
  the `renderSDKToolErrors` receiving middleware that converts SDK-level
  argument errors into the project's error document.
- `render.go` — frontmatter structs, body builders, `toolCall`, `continuation`,
  `errorDetails`.
- `snapshot.go` — the shared snapshot frontmatter and fenced-screen body used
  by four tools.
- `logs.go` — `it_tail` / `it_head` far-end truncation and the
  omitted-lines-plus-path body.

Handlers are thin: validate, call `ipc.Client`, render. No business logic.

### `internal/app` — the human application

Single `tea.Program` in the alternate screen, following
`favro-mcp/internal/app`. Two modes.

**Browser** — a session list with live status, refreshed on a ticker:

```
INTERACTIVE TERMINAL

  NAME       ID         STATE     SIZE     LAST ACTIVITY   LOG
> build      t-k3f9qa   running   160x48   3s ago          1842 lines
  (unnamed)  t-p2m8wd   exited 0  160x48   26m ago         96 lines

  enter attach   n new   k kill   r rename
  c configure clients   s settings   q quit
```

**Attach** — the reason this project has a TUI at all. On `enter`:

1. Leave bubbletea's alternate screen and put the local terminal in raw mode.
2. Resize the session to the local terminal size, remembering the old size.
3. Open `attach.open`, replay the tail of `raw.log` so the screen is correct
   immediately, then stream.
4. Forward local stdin to `attach.input` byte for byte, watching for the detach
   prefix.
5. On detach: restore the session's previous size, restore cooked mode, and
   re-enter the browser.

Detach prefix is `Ctrl-\` followed by `d`; `Ctrl-\ ?` shows help and
`Ctrl-\ Ctrl-\` sends a literal `Ctrl-\`. `Ctrl-b` is deliberately avoided so
attaching to a session that is itself running `tmux` stays usable.

Local `SIGWINCH` while attached resizes the session, so the human's terminal
size wins for as long as they are watching.

### `internal/install` — installer and settings

Drives `detect-harness` rather than reimplementing client configuration:

```go
installer, _ := detectharness.New(detectharness.StdioServer{
    Name:    "interactive-terminal-mcp",
    Command: absoluteExecutablePath,
    Args:    []string{"mcp"},
})
detections := installer.Detect(ctx)
plan := installer.Plan(ctx, selected, detectharness.Present, detectharness.PlanOptions{})
results := installer.Apply(ctx, plan)
```

`Detection.State` distinguishes `present` / `absent` / `unavailable`, so the UI
can show "could not inspect" separately from "not installed" — something the
hand-rolled installers in both reference projects cannot do.
`Detection.ReloadHint` is printed per harness in the result screen.
`ConflictError` stays the default so a foreign entry with the same name is
reported, not overwritten.

## 4. Configuration

`~/.interactive-terminal-mcp/config.toml`, loaded once at startup by both the
daemon and each MCP process, written atomically through `fsx.Replace`.

```toml
version = 1

list_token_budget = 2000      # it_list
read_token_budget = 4000      # it_tail, it_head

default_cols = 160
default_rows = 48
default_wait_seconds = 5      # it_send
settle_quiet_ms = 250
maximum_wait_seconds = 300

log_retention = "on_close"    # on_close | 1h | 4h | 1d | 1w | 1mo | never
scrollback_lines = 10000
raw_log_max_bytes = 33554432
transcript_max_lines = 200000

daemon_idle_shutdown_seconds = 60
maximum_sessions = 50
```

`log_retention` is the one setting the installer asks about directly. The rest
appear in a settings summary the user can skip, following the `apis-mcp`
pattern.

## 5. Installation Flow

### One-line installers

`install.sh` and `install.ps1` are the `apis-mcp` scripts with the owner, repo,
binary name, and install directory changed. They resolve OS/arch, download
`interactive-terminal-mcp-<os>-<arch>[.exe]` from the latest release, install
to `~/.interactive-terminal-mcp/bin` or `%LOCALAPPDATA%\interactive-terminal-mcp`,
add it to `PATH` through the user's shell profile or user environment, and then
run `configure` against `/dev/tty`.

```bash
curl -fsSL https://github.com/sairaph/interactive-terminal-mcp/raw/main/install.sh | sh
```

```powershell
irm https://github.com/sairaph/interactive-terminal-mcp/raw/main/install.ps1 | iex
```

### Configure flow

One `tea.Program`, screens in order.

**1 — Harnesses.** Detected pre-checked, `v` reveals undetected,
`space` toggles, `enter` confirms. Unavailable rows show the reason.

**2 — Session logs.** The question the project was asked to include, with the
required default and options:

```
interactive-terminal-mcp setup
Session logs — when should logs from closed sessions be deleted?

 > ● When the session is closed   (recommended)
     After 1 hour
     After 4 hours
     After 1 day
     After 1 week
     After 1 month
     Never

   Logs let it_tail and it_head reach past the visible screen.

   up/down move · enter select · q cancel
```

**3 — Settings summary.** `apis-mcp` style; `Recommended defaults` shown only
when every value is default, `Continue` preselected, `Change settings` opens
the editor, and `r Restore defaults` appears whenever anything differs and
confirms with a change list before applying.

```
MCP tool configuration
Recommended defaults

Terminal size               160x48
Default wait                5s
List output                 ~2000 tokens
Log output                  ~4000 tokens
Session log retention       When the session is closed
Scrollback                  10000 lines

> Continue
  Change settings

Limits are approximate and apply only to retrieved information.
```

**4 — Apply and report.** Per-harness outcome plus its reload hint, then a
pointer to `interactive-terminal-mcp` for the interactive application.

`interactive-terminal-mcp configure` reopens this later; `s` from the
application launches it, releasing the terminal and resuming afterwards, the
way `favro-mcp` does.

## 6. CLI Surface

```
interactive-terminal-mcp                    interactive application (TTY) / mcp server (no TTY)
interactive-terminal-mcp mcp                stdio MCP server
interactive-terminal-mcp configure          harness + settings flow
interactive-terminal-mcp install            configure, non-interactive with --client
interactive-terminal-mcp uninstall          remove registrations

interactive-terminal-mcp ls                 list sessions
interactive-terminal-mcp new [name] [--cmd …] [--cols N] [--rows N] [--cwd DIR]
interactive-terminal-mcp attach <session>   attach directly, skipping the browser
interactive-terminal-mcp send <session> …   --text / --keys / --wait
interactive-terminal-mcp read <session>
interactive-terminal-mcp tail <session> [-n N]
interactive-terminal-mcp head <session> [-n N]
interactive-terminal-mcp kill <session> [--signal TERM]

interactive-terminal-mcp daemon [--detach]  run the daemon
interactive-terminal-mcp daemon --stop      stop it
interactive-terminal-mcp doctor             daemon reachability, socket perms, PTY probe, paths
interactive-terminal-mcp config [path]
interactive-terminal-mcp version | help
```

`main.go` copies the `apis-mcp` entrypoint: no arguments plus no TTY runs the
MCP server, no arguments plus a TTY opens the application, `mcp` always runs
the server, and installer commands run without opening the daemon.

## 7. CI, Release, Distribution

- **`ci.yml`** — on push to `main` and on PRs: `go vet ./...`, `go test ./...`,
  `go test -race ./...`, `CGO_ENABLED=0 go build -trimpath .`, then a
  `{linux,darwin,windows} × {amd64,arm64}` cross-build matrix. Copied from
  `apis-mcp` minus the ingest-specific steps.
- **`release.yml`** — reusable, triggered by `v*` tags or called with a
  `version` input. Runs tests, builds all six targets with
  `-ldflags="-s -w -X main.version=$version"`, publishes one GitHub Release
  with generated notes.
- **`autorelease.yml`** — from `favro-mcp`. A push to `main` whose `VERSION`
  file names a version with no matching tag creates the tag and calls
  `release.yml`. Shipping a release is bumping `VERSION` and merging.
- **`.gitignore`** — `.env`, the built binary, `dist/`, `coverage.out`,
  `*.test`.
- **`.env`** — gitignored, holds `GITHUB_PAT` for
  `sairaph/interactive-terminal-mcp` plus the authenticated origin URL, matching
  the `favro-mcp` convention. To be populated when the PAT is provided.

Race testing matters more here than in the reference projects: the daemon
multiplexes PTY readers, emulator writes, log writers, broadcasters, and IPC
handlers. `-race` runs on every CI job, not only on release.

## 8. Testing Strategy

| Layer | Approach |
|---|---|
| `keys` | Table-driven: every named key and modifier combination, both DECCKM states, repeats, quoted literals, and every rejection case. Pure functions, no fixtures. |
| `vterm` | Feed recorded escape-sequence fixtures and assert the extracted plain text, cursor, alt-screen flag, and evicted lines. Includes wide characters, combining marks, and a resize mid-stream. |
| `session` | Real PTYs running `sh -c`. Assert: prompt appears, `echo` round-trips, settle returns early on quiet, settle reports `settled:false` on a chatty loop, exit code is captured, transcript excludes alt-screen output, `it_tail`-style backward reads match a forward read. |
| Full-screen TUI | End-to-end golden tests driving `vi` (POSIX-guaranteed) through `it_send` keys and asserting screen contents. Skipped when `vi` is absent. |
| `ipc` / `daemon` | In-process socket pair. Concurrent clients, autostart race, singleton lock race, stale-socket recovery, retention sweep, restart recovery of exited sessions. |
| `budget` | Ported from `apis-mcp` unchanged. |
| `mcpserver` | In-memory MCP transport against a fake IPC client. Golden-file assertions on rendered documents, exactly as `apis-mcp/internal/mcpserver/render_sample_test.go` does. |
| `install` | `detect-harness` against a temporary `HOME` with fixture configs for each format; assert plan, apply, idempotency, and conflict reporting. |
| Cross-platform | Windows-specific ConPTY tests behind a build tag; CI compiles the Windows test binary on Linux (`go test -c GOOS=windows`) so Windows code cannot silently rot. |

## 9. Delivery Order

Each milestone ends with a working, tested, committed binary.

| # | Milestone | Contents |
|---|---|---|
| 1 | **Skeleton** | Repo, module `github.com/sairaph/interactive-terminal-mcp`, LICENSE, VERSION, `.gitignore`, `.env`, both workflows, `main.go` entrypoint switch, ported `budget` / `render` / `fsx` / `config`. CI green. |
| 2 | **Terminal core** | `vterm` adapter and `session`: PTY spawn, emulator, snapshot extraction, settle, transcript and raw logs, exit capture, retention. Full unit + PTY tests. No MCP yet. |
| 3 | **Keys** | `keys` parser and mode-aware encoder with the full table-driven suite. |
| 4 | **Daemon + IPC** | Socket/named pipe, wire protocol, registry, autostart, singleton lock, retention sweeper, restart recovery, `daemon` and `doctor` commands. |
| 5 | **MCP surface** | All eight tools, schemas, frontmatter, bodies, truncation, pagination, error contract. Golden-file tests. This is the first genuinely useful release — tag `v0.1.0`. |
| 6 | **CLI one-shots** | `ls`, `new`, `send`, `read`, `tail`, `head`, `kill`, `attach`, human rendering. |
| 7 | **Installer** | `detect-harness` integration, the configure TUI including the log-retention question, settings editor, restore defaults, `install.sh`, `install.ps1`. Tag `v0.2.0`. |
| 8 | **Human application** | Session browser, attach/detach, resize handling, rename, kill, launch configure. Tag `v0.3.0`. |
| 9 | **Polish** | README with badges and the one-line installers, `AGENTS.md`, `doctor` coverage, Windows verification pass on a real machine, `v1.0.0`. |

## 10. Risks And Judgment Calls

**The daemon is the load-bearing decision.** Without it, sessions die with the
MCP process, a client restart loses a running build, and the human application
cannot attach. It is what `tmux` does and what "runs in the background"
requires. The cost is process lifecycle, IPC, and autostart complexity —
roughly milestone 4 in full.

**`x/vt` is untagged.** Pinned to an exact pseudo-version and isolated behind
`internal/vterm`. The fallback, `hinshun/vt10x`, is stable and dependency-free
but older and weaker on truecolor and modern DEC modes; swapping means
rewriting one adapter file.

**Alternate-screen output is absent from the transcript.** Correct terminal
behavior, but surprising to an agent that runs `htop` and then calls `it_tail`.
Mitigated by `it_tail` appending the live screen by default and by the body
labeling both parts.

**Windows ConPTY differs.** No process groups, no real signals, and ConPTY
resize semantics vary by Windows build. Handled by mapping `INT` to a `0x03`
write and `TERM`/`KILL` to process-tree termination, and by compiling and
testing the Windows binary in CI even though the runners are Linux. A real
Windows pass is scheduled in milestone 9 rather than assumed.

**Snapshots can catch a mid-redraw frame.** The settle mechanism plus the 30 ms
coalesce make this rare rather than impossible. `settled: false` tells the
agent when the screen may be in flux.

### Deliberate v1 exclusions

- `wait_for`, a regex matched against the screen to end a wait early. High
  value, and the settle mechanism already has the right shape for it — the
  strongest v2 candidate.
- `ansi: true` on snapshots, returning styled output for TUIs that encode state
  purely in color.
- Session persistence across a machine reboot.
- Splits, panes, and windows. One session is one terminal.
- Remote or shared-over-network sessions.

## 11. Choices Worth Your Confirmation

Three places where a defensible alternative exists and the decision changes the
agent-facing surface:

1. **Argument names.** The brief used `string=` and `keyboard=`; the contract
   uses `text` and `keys`, which are shorter and read better in a tool call.
   Easy to change before implementation, painful after.
2. **`enter` defaults to `true`.** `it_send({"text":"echo hi"})` runs the
   command, which matches the example in the brief, at the cost of a small
   piece of implicit behavior. `enter: false` opts out.
3. **Resizing lives on `it_read`.** Rather than a ninth tool, `it_read` accepts
   `cols`/`rows` and resizes before snapshotting. It keeps the surface at eight
   tools and reads naturally, but it does put a mutation on a tool whose name
   suggests it only observes.


## 12. What Changed During Implementation

Four things were decided differently once the code met a real terminal.

### The emulator's reply pipe has to be drained

A terminal is not write-only. Programs ask it questions during startup —
device attributes, cursor position, colour support — and block until they are
answered. The emulator produces those answers from inside its write path and
hands them to an unbuffered pipe.

Nothing drained that pipe in the first implementation, so the first `vim`
session deadlocked the entire daemon: the emulator blocked mid-write while
holding its lock, and every snapshot from every session blocked behind it. The
symptom was `it_list` hanging while `status` still worked.

`internal/vterm` now owns a goroutine that is the only caller of the emulator's
`Read`, and `Session.answerQueries` forwards those replies to the PTY. Both the
deadlock and the handshake are fixed by the same change, and
`TestTerminalQueriesAreAnswered` pins it.

### Closing the emulator needed a different mechanism

The library's `Emulator.Close` sets an internal flag without synchronisation,
which races with a reader blocked in `Read`. That path is not used. `Close`
instead raises its own flag and pushes a device-attributes query through the
emulator; the reply wakes the reader, which sees the flag and exits. Because
the reader exists for the whole life of the `Charm`, the query always has a
consumer and `Close` never blocks.

### The child exiting does not end the PTY stream

`go-pty` keeps the slave descriptor open in the parent, so reads from the
master block forever after the child exits rather than draining and reporting
EOF. The original plan waited a fixed 500 ms for output to settle, which
silently lost the tail of any command that produced more than that.
`Session.reap` now releases the slave and waits for the reader to finish, which
is deterministic. This only showed up under `-race`, where the timing changed
enough to drop half the output.

### Unix socket paths have a length limit

`sun_path` is 104-108 bytes including the NUL. A deep home directory pushes the
socket past it and `connect` fails with a bare `invalid argument` that no
message explains. `config.unixSocketPath` now falls back to `$XDG_RUNTIME_DIR`
or the temporary directory with a name hashed from the application root, so the
fallback is stable and per-user.

### Smaller adjustments

- **The key language accepts bare unambiguous runs.** `ESC; :wq; ENTER` was in
  the contract but did not parse. A multi-character run is now typed verbatim
  when it could not be a key name; a run of only letters, digits, and
  underscores must resolve to a key name or be quoted, so `PAGEUPP` is still
  reported as a typo rather than typed into the terminal.
- **`call()` does not HTML-escape.** `encoding/json` renders `<id>` as
  `\u003cid\u003e`, which is noise in a string a model has to follow.
- **Schema validation messages are cleaned.** The SDK rejects an out-of-range
  argument before the handler runs, with a message like
  `validating "arguments": validating root: validating /properties/wait: ...`.
  The framing is stripped and the argument name preserved.
- **`it_send` does not advertise an empty log.** A session that lived entirely
  inside a full-screen program has no transcript, so pointing at `it_tail`
  would cost a round trip to learn nothing.

## 13. Test Coverage

| Package | What it covers |
|---|---|
| `keys` | Every named key and modifier, both DECCKM states, repeats, quoted and bare literals, and every rejection path |
| `vterm` | Plain-text extraction, wide characters, interior vs trailing blanks, mode and title tracking, eviction accounting, concurrent write/snapshot/query, close releasing a blocked reader |
| `session` | Real PTYs: input round trip, settle-early and settle-expired, exit capture, transcript vs alternate screen, resize reaching the child, kill and escalation, terminal queries, a full `vim` edit-and-save |
| `daemon` | Two-process lifecycle over a real socket, every tool requiring an explicit session, kill requiring an explicit target, TERM escalation, INT leaving the session usable, name conflicts, typed errors, singleton locking |
| `ipc` | Request/response matching under concurrency, typed errors across the wire, stale vs live socket handling, socket permissions, deadlines |
| `mcpserver` | Golden-file assertions on every rendered document, plus an end-to-end run of all eight tools against a live daemon, including driving `less` with keys |
| `budget` | Truncation direction, oversized records, pagination completeness |
| `config` | Round-trip, validation, atomic restore, socket-path length |
| `install` | `detect-harness` against fixture homes: registration, idempotency, removal, conflict reporting, uninstallable environments |
| `cli` | Argument parsing and the usage contract |

The race detector runs on every CI job rather than only on release; two of the
four bugs above were invisible without it.
