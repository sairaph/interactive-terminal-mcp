# Interactive Application Design

The full-screen application opened by a bare `interactive-terminal-mcp` on a
TTY. It is a terminal wrapper running inside a terminal: the human sees exactly
the sessions the agent sees, in exactly the state the agent left them. That
shared view is the point — it makes the agent's terminal access transparent
rather than invisible.

Built with `bubbletea` in the alternate screen, one `tea.Program` for the whole
application, following `favro-mcp/internal/app`.

## Screens

```
        ┌──────────┐  enter / n         ┌──────────┐
        │   Home   │ ─────────────────> │ Session  │
        │  (list)  │ <───────────────── │  (term)  │
        └────┬─────┘      ctrl+q        └──────────┘
             │ c
             v
        ┌──────────┐
        │Configure │
        └──────────┘
```

## Home

The session list. `+ New session` is always the first row, so creating a
session is `enter` on a freshly opened application with nothing selected yet.

```
╭─ interactive-terminal-mcp ──────────────────────────────────────────────╮
│                                                                          │
│  > + New session                                                         │
│                                                                          │
│    ● build       t-k3f9qa   running   160x48    3s ago      1842 lines   │
│    ● dev-server  t-w81mza   running   160x48    1m ago      412 lines    │
│    ○ (unnamed)   t-p2m8wd   exit 0    160x48    26m ago     96 lines     │
│                                                                          │
╰──────────────────────────────────────────────────────────────────────────╯
  ↑↓ navigate · enter open · n new · backspace delete · r rename
  c configure · q quit
```

- `●` green for running, `○` grey for exited.
- Rows refresh on a 1 s ticker while Home is visible; the daemon is polled, so
  a session the agent creates appears without the human doing anything.
- `↑`/`↓` and `k`/`j` navigate. Wraps at both ends.
- `enter` opens the highlighted session, or creates one on the `+ New session`
  row.
- `n` creates and opens a new session from anywhere in the list.
- `backspace` (and `delete`) opens a confirmation before killing:

```
  Delete session "build" (t-k3f9qa)?

  It is still running: make -j8
  Its logs will be deleted immediately (retention: when the session is closed).

  > Cancel
    Delete
```

  `Cancel` is preselected. `esc` cancels. The running command and the retention
  consequence are both stated, because those are the two facts that make the
  choice reversible or not.
- `r` renames in place with an inline text input, validated against the same
  name rules the MCP tools use.
- `c` opens Configure.
- `q` quits the application. It does **not** stop the daemon or kill sessions;
  the agent's terminals keep running. `Q` offers to stop the daemon too, after
  a confirmation that lists what would die.

## Session

A framed terminal. The frame is a rounded single-line border in a dim accent
colour, with the product name inset into the top edge. Inside the frame is the
session's screen, rendered from the same emulator the agent snapshots.

```
╭─ interactive-terminal-mcp ── build · t-k3f9qa · running ─────────────────╮
│ lael@host:~/project$ make -j8                                            │
│   CC   src/parser.o                                                      │
│   CC   src/render.o                                                      │
│ make: *** [Makefile:42: all] Error 1                                     │
│ lael@host:~/project$ █                                                   │
│                                                                          │
╰──────────────────────────────────────────────────────────────────────────╯
 > echo "this is a test"                                                    
   ctrl+q back · ctrl+g raw · shift+enter newline · ctrl+v paste · ↑ history
```

The frame costs two columns and two rows; the session is sized to the interior,
so what the human sees is exactly one screen of the session, not a scaled or
cropped one.

### Two input modes

A wrapper needs both a comfortable composer and honest key passthrough, so the
application has both and switches automatically.

**Composer mode** (default, for shells). Typing goes into the input field under
the frame. `enter` submits the whole buffer to the PTY. This is what makes
multi-line input, paste, and history possible at all — a raw passthrough has
nowhere to hold an unsubmitted line.

**Raw mode** (for full-screen programs). Every keystroke goes straight to the
PTY, unbuffered, exactly as a real terminal would deliver it. `vim`, `htop`,
`less`, and `tmux` work normally.

The application enters raw mode automatically the moment the session switches
to the alternate screen, and returns to the composer when it switches back.
That covers essentially every full-screen program without the human thinking
about modes. `ctrl+g` toggles manually for the programs that read keys directly
without using the alternate screen — `ssh`, `python`, a `read -n1` prompt. The
current mode is always named in the footer, and the frame's top-right corner
shows `RAW` while raw mode is on.

In raw mode, `ctrl+q` still returns to Home. Nothing else is intercepted.

### The composer

- Starts one line tall and grows as the text wraps or newlines are added.
- Grows until it reaches 80% of the application height. Past that it stops
  growing and scrolls internally: `↑`/`↓` move the cursor by line and scroll
  when the cursor reaches an edge, `page up`/`page down` move by a viewport,
  `ctrl+home`/`ctrl+end` jump to the start and end of the buffer. A
  `12/48` line indicator appears in the composer's right margin once it
  scrolls, so a long paste is visibly long.
- `shift+enter` inserts a newline. Terminals that cannot report it fall back to
  `ctrl+enter`, and `alt+enter` works everywhere as a last resort; all three
  are accepted always, and the footer names whichever the current terminal
  actually reports.
- `enter` submits. A multi-line buffer is submitted as a bracketed paste when
  the program has bracketed paste enabled, so an editor receives it as one
  paste rather than as a sequence of commands — the same rule `it_send` uses.
- `ctrl+v` pastes from the system clipboard; a bracketed paste arriving from the
  outer terminal is captured natively, so `cmd+v` and middle-click work too.
  Pasted text is inserted literally, never executed, even when it contains
  newlines: the human presses `enter` deliberately.
- `ctrl+c` with a selection copies; with an empty composer it sends an
  interrupt to the session, which is what a terminal does. With text in the
  composer it clears the composer, matching a shell.
- `↑`/`↓` on an empty single-line composer walk the per-session input history
  (persisted in the session directory). With text present they move the cursor
  instead.
- `tab` sends a literal tab to the PTY rather than being swallowed, so shell
  completion works: the composer submits the current buffer without a newline,
  lets the shell complete it, and reloads the result. Completion is the one
  place the composer defers to the PTY.
- `esc` clears the composer; `ctrl+q` leaves the session.

### Scrollback

`shift+page up` / `shift+page down` scroll the framed view back through the
session's scrollback without touching the running program. A
`─ scrollback 240/1842 ─` marker replaces the frame's title while scrolled
back, and any new output or keystroke jumps back to the live view. `shift+home`
goes to the oldest retained line.

### Resize

While a session is open, the session is resized to the frame interior and
`SIGWINCH` is delivered. On leaving, the previous size is restored, so a size
the agent chose deliberately survives a human glance. A local terminal resize
while attached re-resizes live.

### Copy out

`ctrl+shift+c` copies the visible screen to the clipboard as plain text. `y`
in scrollback mode copies the scrolled-back view. Where no clipboard is
reachable — a bare Linux console, a stripped container — the application says
so in the footer instead of silently doing nothing.

## Configure

Reached with `c` from Home, and the same screen the installer shows, so there
is exactly one place these settings live. Changes are validated together and
saved atomically; invalid values keep the editor open with an inline message
and save nothing.

```
╭─ interactive-terminal-mcp ── configure ──────────────────────────────────╮
│                                                                          │
│  AI harnesses                                                            │
│    [x] Claude Code          configured                                   │
│    [x] Cursor               detected                                     │
│    [ ] Codex CLI            detected                                     │
│    [ ] Zed                  not detected                                 │
│        Amazon Q             could not inspect: permission denied         │
│                                                                          │
│  Settings                          Recommended defaults                  │
│    Terminal size                   160x48                                │
│    Default wait                    5s                                    │
│    List output                     ~2000 tokens                          │
│    Log output                      ~4000 tokens                          │
│    Session log retention           When the session is closed            │
│    Scrollback                      10000 lines                           │
│    Maximum sessions                50                                    │
│    Daemon idle shutdown            60s                                   │
│                                                                          │
╰──────────────────────────────────────────────────────────────────────────╯
  tab panels · ↑↓ select · space toggle · enter edit · s save
  d restore defaults · esc back

  Limits are approximate and apply only to retrieved information.
```

- Harness rows use `detect-harness` states: `configured`, `detected`,
  `not detected`, and `could not inspect` with the reason. Only the first two
  are selectable; the fourth is never silently treated as absent.
- `Recommended defaults` appears only when every setting equals its default.
- `d` restores defaults after a confirmation listing just the values that would
  change, `current -> default`.
- Saving prints each harness's reload hint, because a registered harness needs
  a restart before it sees the server.
- Settings that only take effect for new sessions (terminal size) or on daemon
  restart (idle shutdown) are labelled as such at the moment they are edited.

## Behaviour Under Stress

| Situation | Behaviour |
|---|---|
| Daemon not running | Home shows `Starting session daemon…`, autostarts it, then loads. Failure shows the reason and a `doctor` hint rather than an empty list. |
| Daemon dies while open | A banner replaces the footer; the application retries with backoff and restores the view on reconnect. Composer input is preserved. |
| Session exits while open | The frame title becomes `exited 1`, the composer is replaced by `Session ended (exit 1). ctrl+q back · enter new session here`. |
| Session killed by the agent while open | Same, with `killed by it_kill`. |
| A flood of output | The view renders at most 30 fps from the latest state; it never queues frames. A slow human view can never slow down an agent's tool call. |
| Terminal too small | Below 60x20 the frame is dropped and a `terminal too small (60x20 needed)` message is shown, restoring itself on resize. |
| No TTY | The application refuses to start and points at `interactive-terminal-mcp mcp`. |

## Why A Composer Instead Of Pure Passthrough

Pure passthrough is simpler and is what `tmux attach` does. It cannot offer
multi-line editing, paste-before-submit, or input history, because those need a
buffer that the PTY does not have. The composer provides them for the 90% case
of typing shell commands, and raw mode — entered automatically — provides exact
passthrough for the 10% where a program owns the keyboard. The human gets both
without choosing.
