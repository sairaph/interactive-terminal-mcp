# Agent Tool Contract

Status: implemented and tested against the first release.

Project, repository, binary, and MCP server name: `interactive-terminal-mcp`.
Remote: `github.com/sairaph/interactive-terminal-mcp`.

`interactive-terminal-mcp` gives an agent a real terminal: persistent PTY
sessions that survive between tool calls and between client restarts, run
interactive and full-screen TUI programs correctly, and can be attached to by
the human user at the same time. It is the agent-facing equivalent of `tmux`.

## Entry Point Model

Same terminal-sensitive entrypoint as `favro-mcp` and `apis-mcp`:

| Invocation | Behavior |
|---|---|
| `interactive-terminal-mcp` on a TTY | Full-screen interactive application |
| `interactive-terminal-mcp` without a TTY | stdio MCP server |
| `interactive-terminal-mcp mcp` | stdio MCP server, always |
| `interactive-terminal-mcp configure` | Installer / settings flow |
| `interactive-terminal-mcp daemon` | Session daemon in the foreground |

The first release supports MCP over stdio only. No HTTP, SSE, or other network
transport is exposed.

The server uses the official Go MCP SDK at
`github.com/modelcontextprotocol/go-sdk`. Tool handlers decode into typed Go
input structures; JSON Schemas declare validation, defaults, and
configuration-dependent limits explicitly.

## Runtime Model

Terminal sessions outlive any single MCP process, so the PTYs are owned by a
per-user background **daemon**. Every MCP process, every CLI one-shot, and the
interactive application are thin clients of that daemon over a local socket.

```
┌──────────────┐   ┌──────────────┐   ┌──────────────┐
│ Claude Code  │   │ Cursor       │   │ human TUI    │
│  (mcp stdio) │   │  (mcp stdio) │   │  (attach)    │
└──────┬───────┘   └──────┬───────┘   └──────┬───────┘
       │ JSON lines over unix socket / named pipe
       └──────────────────┼──────────────────┘
                   ┌──────▼───────┐
                   │    daemon    │  owns PTYs, emulators, logs
                   └──────┬───────┘
              ┌───────────┼───────────┐
          ┌───▼───┐   ┌───▼───┐   ┌───▼───┐
          │ pty 1 │   │ pty 2 │   │ pty 3 │
          └───────┘   └───────┘   └───────┘
```

Consequences the agent can rely on:

- A session created by one tool call is visible to the next tool call, to a
  different AI client, and to the human application.
- Restarting the AI client does not kill running commands.
- The human can attach to a session the agent created and watch it live, or
  type into it. The agent and the human share one terminal, exactly like two
  `tmux` clients attached to one window.

The daemon starts automatically the first time any client needs it and exits
once it has had zero sessions and zero connected clients for 60 seconds.

## Local Trust Model

`interactive-terminal-mcp` is a local tool acting with the authority of the
user who runs it. `it_send` can run any command the user can run. There is no
allowlist, no sandbox, no command filtering, and no confirmation step; that is
the feature, not an oversight. The user is responsible for what the agent is
allowed to do, through the harness's own permission system.

The daemon listens on a Unix domain socket at
`~/.interactive-terminal-mcp/daemon.sock` with mode `0600` inside a `0700`
directory, or on a per-user Windows named pipe with an owner-only DACL. It
never opens a TCP port.

A deep home directory can push that path past the kernel's 104-byte `sun_path`
limit, which fails at connect time with an undiagnosable "invalid argument". The
daemon detects this and falls back to `$XDG_RUNTIME_DIR` or the temporary
directory, using a name derived from the application root so it stays stable and
per-user. `interactive-terminal-mcp status` prints the socket actually in use. Sessions inherit the daemon's environment plus any
`env` supplied at creation.

## Session Model

### Identity

Each session has a generated **id** and an optional user-supplied **name**.

- id: `t-` followed by six lowercase base32 characters, e.g. `t-k3f9qa`.
  Short enough for an agent to repeat, unique across live and retained
  sessions.
- name: matches `^[a-z0-9][a-z0-9._-]{0,63}$`, must be unique among live
  sessions, and may not start with `t-`.

Any argument named `session` accepts either form. Every tool that can act on a
session accepts `session` as an optional argument and falls back to the active
session, except `it_kill`, which always requires it.

### The active session

The daemon tracks one active session per user. It is set by `it_new` and can be
changed with `it_active`. If the active session exits, it stays active (so the
agent can still read its final screen and logs) until another session is
created or selected. If the active session is killed, the most recently active
live session becomes active, or none.

### Lifecycle

A session is `running` while its child process is alive and `exited`
afterwards, carrying `exit_code`. An exited session keeps its final screen and
its logs until retention removes them, so an agent that missed the end of a
build can still read it.

### Size

Sessions are created at a typical virtual size, `160x48` by default
(configurable). `it_new` accepts `cols` and `rows`; `it_read` accepts them too
and resizes before snapshotting, which is how an agent gives a pager or TUI
more room. The human application resizes the session to the real terminal size
while attached and restores the previous size on detach.

## MCP Surface

Eight direct, typed tools. Every name is short and prefixed `it_`.

| Tool | Purpose |
|---|---|
| `it_active` | Report or change the active session |
| `it_list` | List sessions, token-budget paginated |
| `it_new` | Create a session, make it active, return its screen |
| `it_read` | Snapshot the visible screen of a session |
| `it_send` | Send text and/or key chords, wait, return the screen |
| `it_kill` | Terminate a session by explicit id or name |
| `it_tail` | Last N lines of a session log |
| `it_head` | First N lines of a session log |

Every result is a text content block containing YAML frontmatter followed by a
Markdown body, rendered by the shared `render.Document` used in `apis-mcp`.
Failures set MCP `isError` and use error frontmatter plus an actionable
explanation. No tool duplicates its result through `structuredContent`.

Each tool has its own input schema. There is no action discriminator, no nested
operation envelope, and no loose `args` object.

### Screen snapshots

`it_new`, `it_read`, `it_active`, and `it_send` all return a **snapshot**: the
exact contents of the visible screen at the moment the tool returned, as plain
text inside a tilde fence.

Rendering rules:

- Cells are read out of the terminal emulator and written as plain text.
  Colors and other styling are dropped by default.
- Trailing whitespace is stripped from each line.
- Trailing blank lines at the bottom of the screen are dropped and reported as
  `blank_lines_trimmed`. Interior blank lines are always preserved, because in
  a full-screen TUI they carry layout.
- Wide characters occupy their real display width; the continuation cell is not
  emitted twice.
- The fence grows past three tildes if the screen content contains tildes, so
  program output can never close it.

A snapshot is at most one screen, so it needs no token budget. The global 1 MiB
rendered-output ceiling still applies.

Snapshot frontmatter is shared by all four tools:

```yaml
session: t-k3f9qa
name: build
active: true
running: true
pid: 48213
size: [160, 48]
cursor: [12, 41]
alt_screen: false
title: make -j8
settled: true
busy: false
waited_ms: 820
last_activity_at: 2026-07-27T09:31:44Z
blank_lines_trimmed: 17
```

| Field | Meaning |
|---|---|
| `cursor` | One-based `[row, column]` of the cursor on the visible screen |
| `alt_screen` | True while a full-screen program owns the alternate buffer |
| `title` | Last title set by the program through OSC 0/2, when any |
| `settled` | Output stopped changing before the wait budget expired. Absent when the call did not wait long enough to establish anything |
| `busy` | A command still holds the terminal. Absent where that cannot be established |
| `waited_ms` | Milliseconds actually spent waiting |
| `logs_retained` | Whether this session's log survives the session ending |
| `exit_code` | Present only once the session has exited |

### Waiting

`it_new` and `it_send` accept `wait`, a number of seconds from `0` to `300`.

`wait` is a ceiling, not a sleep. The tool returns as soon as the session has
produced no new output for the settle interval (250 ms by default), or when
`wait` seconds have elapsed, whichever comes first. `wait: 0` returns after a
50 ms grace period without waiting for quiet.

`settled: false` in the frontmatter means the wait budget expired while output
was still arriving. The body then tells the agent to call `it_read` again with
a longer wait rather than assuming the command finished. When the call did not
wait long enough to observe anything either way, the field is absent rather
than false: nothing was looked at, which is not the same as having looked and
seen output still coming.

This makes `it_send({"text": "ls", "wait": 5})` cheap for a fast command and
correct for a slow one, without the agent having to guess a duration.

### Knowing whether a command has finished

`settled` answers a question about output, not about completion. A command that
prints nothing is quiet from the moment it starts, so quiet is the weakest of
the three signals a reply carries.

`busy` is stronger and comes from the terminal rather than from timing: it
reports whether the foreground of the terminal has been handed to something
other than the shell. `busy: true` is proof a command is running. `busy: false`
means no separate command holds the terminal, which is an idle prompt nearly
always and occasionally the shell working inside itself, as a `while` loop
does. Where neither can be established the field is absent and the reply claims
nothing.

`wait_for` is exact. The wait ends the moment the given text appears, and what
counts as an appearance is defined rather than guessed:

- Text already on the screen when a call that types nothing begins (`it_read`)
  is a match, because that call is asking whether the text is there.
- Text already on the screen before `it_send` types its input is not a match.
  It was not produced by this call.
- The echo of the input `it_send` types is not a match either, which is what
  makes `it_send({"text": "make && echo BUILT", "wait_for": "BUILT"})` work.
  The occurrences the typed line itself contains are discounted, so the wait
  ends on the output rather than on the command line.

Nothing is excluded on the basis of when it arrived, so a command that finishes
in a millisecond is matched exactly as a slow one is.

### `it_active`

| Argument | Required | Meaning |
|---|---:|---|
| `session` | no | Session to make active. Omit to report the current one. |

Without arguments it reports the active session and returns its snapshot. With
`session`, it switches the active session and returns that session's snapshot.

When nothing is active:

```yaml
active: null
live_sessions: 0
```

```markdown
No session is currently active. Create one with `it_new({})`.
```

When sessions exist but none is active, the body lists how to select one with
`it_active({"session":"<id>"})`.

### `it_list`

| Argument | Required | Meaning |
|---|---:|---|
| `page` | no | One-based result page; defaults to `1`. |

Sessions are ordered live-first, then by most recent activity. Results are
packed into pages by token budget; the caller does not control page size.

```yaml
page: 1
total: 3
total_pages: 1
active: t-k3f9qa
sessions:
  - id: t-k3f9qa
    name: build
    active: true
    running: true
    pid: 48213
    command: /bin/bash
    cwd: /home/lael/project
    size: [160, 48]
    alt_screen: false
    title: make -j8
    created_at: 2026-07-27T09:12:03Z
    last_activity_at: 2026-07-27T09:31:44Z
    transcript_lines: 1842
    log_path: /home/lael/.interactive-terminal-mcp/sessions/t-k3f9qa/transcript.log
  - id: t-p2m8wd
    name: null
    active: false
    running: false
    exit_code: 0
    exited_at: 2026-07-27T09:05:10Z
    command: /bin/bash
    cwd: /home/lael
    size: [160, 48]
    created_at: 2026-07-27T08:58:02Z
    last_activity_at: 2026-07-27T09:05:10Z
    transcript_lines: 96
    log_path: /home/lael/.interactive-terminal-mcp/sessions/t-p2m8wd/transcript.log
```

The body explains how to read a listed session, and includes an exact
`it_list({"page": 2})` continuation when another page exists.

With no sessions at all:

```markdown
No terminal sessions exist. Create one with `it_new({})`.
```

### `it_new`

| Argument | Required | Meaning |
|---|---:|---|
| `name` | no | Stable name for the session. Generated id is used when omitted. |
| `command` | no | String run through the login shell, or an argv array executed directly. Defaults to the user's shell. |
| `cwd` | no | Working directory. Defaults to the daemon's working directory. |
| `env` | no | Extra environment variables merged over the inherited environment. |
| `cols` | no | Terminal width; defaults to the configured `120`. |
| `rows` | no | Terminal height; defaults to the configured `30`. |
| `wait` | no | Seconds to wait for the first output to settle; defaults to `2`. |

`command` overloads its type the way `apis_call` overloads `headers`:

- A string is passed to the user's login shell as a single command line, so
  `"npm run dev"` and `"cd /tmp && ls"` both work.
- An array is executed directly with no shell, so arguments containing spaces
  or quotes need no escaping.

The session is created, made active, and its first screen is returned. The
default `wait: 2` is long enough for a shell prompt to be drawn.

Creating a session with a name that is already taken by a live session is an
error naming the existing session; it does not silently attach or replace.

Frontmatter is the shared snapshot block plus `created_at`, `command`, `cwd`,
and `log_path`.

Body when the session started cleanly:

````markdown
~~~text
lael@host:~/project$
~~~

Session `t-k3f9qa` is active. Send input with `it_send({"text":"..."})`.
````

### `it_read`

| Argument | Required | Meaning |
|---|---:|---|
| `session` | no | Session to read. Defaults to the active session. |
| `cols` | no | Resize to this width before snapshotting. |
| `rows` | no | Resize to this height before snapshotting. |
| `wait` | no | Seconds to wait for output to settle; defaults to `0`. |

Returns the visible screen. This is the tool an agent uses to check on a
running command, to see the current state of a full-screen program, and to
confirm what a previous `it_send` produced.

Supplying `cols` or `rows` resizes the session first and delivers `SIGWINCH` to
the child, then waits for the settle interval so a redrawing TUI is captured
after the redraw rather than during it.

If the session has exited, the final screen is returned with `running: false`
and `exit_code`, and the body points at `it_tail` for what scrolled away.

### `it_send`

| Argument | Required | Meaning |
|---|---:|---|
| `session` | no | Session to write to. Defaults to the active session. |
| `text` | conditional | Literal text typed into the terminal. |
| `keys` | conditional | Key chords, see the key language below. |
| `enter` | no | Append a carriage return after `text`. Defaults to `true`. |
| `wait` | no | Seconds to wait for output to settle; defaults to `5`. |

At least one of `text` and `keys` is required. When both are present, `text` is
sent first, then `keys`.

`text` is typed verbatim, exactly as if the user had typed it. `enter` defaults
to `true` and appends a carriage return, so
`it_send({"text": "echo \"This is a test\""})` runs the command. `enter` is
ignored when `text` already ends in a newline or carriage return. Set
`enter: false` to fill in a prompt field without submitting it.

If the program has enabled bracketed paste and `text` contains a newline other
than a trailing one, the text is wrapped in paste markers so editors receive it
as a paste instead of as a sequence of commands.

Writing to an exited session is an error that reports the exit code and points
at `it_new`.

#### The key language

`keys` is a semicolon-separated list of chords. Whitespace is insignificant, a
trailing semicolon is optional, and key names are case-insensitive.

```text
keys: "CTRL+B; PAGE_UP;"
keys: "ESC; :wq; ENTER"
keys: "DOWN*5; ENTER"
keys: 'i; "hello world"; ESC'
```

| Element | Form | Notes |
|---|---|---|
| Modifier | `CTRL+`, `ALT+`, `SHIFT+` | `META` is an alias for `ALT`. Combinable. |
| Named key | `ENTER` `TAB` `ESC` `SPACE` `BACKSPACE` `DELETE` `INSERT` `HOME` `END` `PAGE_UP` `PAGE_DOWN` `UP` `DOWN` `LEFT` `RIGHT` `F1`–`F20` | `RETURN`, `ESCAPE`, `PGUP`, `PGDN` are accepted aliases. |
| Literal character | `A` `c` `9` `/` | Any single printable character. |
| Literal string | `"some text"` | Double-quoted run typed verbatim, for mixing text into a key sequence. |
| Bare run | `:wq` `--force` | A multi-character run is typed verbatim when it could not be a key name. A run of only letters, digits, and underscores must be a key name or be quoted, so a typo like `PAGEUPP` is reported instead of silently typed. |
| Repeat | `<chord>*<n>` | `1`–`1000`, e.g. `DOWN*5`. |

Encoding follows the terminal's current modes rather than a fixed table. When
the running program has enabled application cursor keys (DECCKM), arrows,
`HOME`, and `END` are sent as SS3 sequences; otherwise as CSI sequences. This
is what makes arrow keys work correctly inside `vim`, `less`, and `htop`
instead of only at a shell prompt.

An unparseable chord is a validation error that names the offending token and
lists the accepted forms. Nothing is sent when parsing fails, so a typo in the
fifth chord never leaves the terminal in a half-typed state.

### `it_kill`

| Argument | Required | Meaning |
|---|---:|---|
| `session` | **yes** | Session id or name. Never inferred from the active session. |
| `signal` | no | `TERM` (default), `INT`, `HUP`, or `KILL`. |

Requiring `session` is deliberate: killing the wrong terminal is destructive
and an agent should have to name its target.

`TERM`, `HUP`, and `KILL` signal the child's process group. `INT` writes the
interrupt character to the PTY instead, on every platform, which is what
actually reaches a foreground program under a shell. `TERM`/`HUP`/`KILL`
terminate the process tree; on Windows they end it through the job object.

Writing the character rather than raising a signal is what makes `INT` work
through a nested terminal. When the session is running `ssh`, `wsl`, or `tmux`,
the command being interrupted lives on a pty on the far side of that client,
and the byte is passed along to it: the client is holding this terminal in raw
mode precisely so that control characters are data to it and signals only at
the other end. Anything that signals locally instead hits the client and takes
the whole nested session down with it. `it_send({"keys": "CTRL+C"})` writes the
same byte and is interchangeable.

After `TERM`, the daemon waits up to 5 seconds for the process to exit and then
escalates to `KILL`, reporting which one ended it.

```yaml
killed: t-k3f9qa
name: build
signal: TERM
escalated: false
exit_code: 143
logs_retained: false
log_path: null
```

`logs_retained` reflects the configured retention policy. Under the default
`on_close` policy the session directory is deleted immediately and `log_path`
is null; under any other policy the path stays valid until retention expires.

The same policy decides how long a session stays listed at all. Under
`on_close` an ended session leaves `it_list` as soon as it ends, because there
is nothing left to point at; `it_list` carries the policy as `retention` and
says so in its body, since a session quietly disappearing otherwise looks like
data loss. Under a retention window ended sessions stay listed until it
expires.

### `it_tail` and `it_head`

| Argument | Required | Meaning |
|---|---:|---|
| `session` | no | Session to read. Defaults to the active session. |
| `lines` | no | Lines requested; defaults to `100`, maximum `5000`. |
| `screen` | no | `it_tail` only. Append the live screen after the log lines. Defaults to `true`, and should stay on: the log holds only what has scrolled off, so with it off the newest output is missing entirely. |

`it_tail` returns the last `lines` lines of the session transcript, `it_head`
the first `lines` lines. Both read the durable on-disk transcript, so they
reach further back than the in-memory scrollback and still work after the
daemon has restarted.

The transcript contains lines as they scrolled off the top of the screen, plus
the visible screen at the moment the session exited. Output written while a
full-screen program owned the alternate screen is **not** in the transcript,
because alternate-screen content does not scroll into history in any terminal.
That is why `it_tail` appends the live screen by default: for a session sitting
in `vim` or `htop`, the log is the history from before the program started and
the screen is the current state. The body labels the two parts.

Both tools apply the configured read token budget. When the requested lines do
not fit, the response is truncated **from the far end**, so `it_tail` always
keeps the newest lines and `it_head` always keeps the oldest.

```yaml
session: t-k3f9qa
name: build
lines_requested: 500
lines_returned: 138
lines_omitted: 362
total_lines: 1842
truncated: true
truncated_by: token_budget
screen_included: true
log_path: /home/lael/.interactive-terminal-mcp/sessions/t-k3f9qa/transcript.log
```

`truncated_by` is `token_budget` when the response budget cropped the result,
or `log_start` / `log_end` when the log simply has fewer lines than requested.
The latter is not an error and sets `truncated: false`.

Body shape:

````markdown
~~~text
  CC   src/parser.o
  CC   src/render.o
make: *** [Makefile:42: all] Error 1
~~~

Showing the last 138 of 500 requested lines (1842 in the log). 362 older lines
were omitted to fit the response budget. The complete log is at
`/home/lael/.interactive-terminal-mcp/sessions/t-k3f9qa/transcript.log`.

Live screen:

~~~text
lael@host:~/project$
~~~
````

Reporting the count and handing back an absolute path to the complete file is
the same escape hatch `apis_call` uses for large responses: the agent is never
silently given a partial answer, and it can always read the rest with ordinary
file tools.

## Pagination And Response Budgets

Numeric pagination is exposed as `page`. Callers never control page size. The
application packs complete records until the configured approximate token
budget is reached, so different pages may contain different numbers of records.

| Output class | Default approximate budget |
|---|---:|
| `it_list` | 2,000 tokens |
| `it_tail`, `it_head` | 4,000 tokens |
| Screen snapshots | not budgeted; bounded by the terminal size |

Token estimates use OpenAI's `o200k_base` encoding internally. That is an
implementation detail and is never named in user-facing text. The budget covers
only retrieved information; frontmatter, fences, and guidance sit outside it.

An independent 1 MiB ceiling on rendered output protects the transport.

## Session Log Retention

The installer asks one question about logs. The choice is stored as
`log_retention` and applies to every session directory.

| Value | Behavior |
|---|---|
| `on_close` | **Default.** The session directory is deleted as soon as the session exits or is killed. |
| `1h` `4h` `1d` `1w` `1mo` | The directory is deleted that long after the session exited. |
| `never` | Logs are kept until the user deletes them. |

Live sessions are never swept regardless of the setting. Cleanup runs when the
daemon starts and every ten minutes while it is running. Failures to delete are
recorded in diagnostics and never fail an unrelated tool call.

The setting can be changed later from `interactive-terminal-mcp configure`
without reinstalling.

## Error Contract

Failures set `isError` and render error frontmatter plus a Markdown
explanation, matching `apis-mcp`:

```yaml
error:
  code: session_not_found
  message: no session matches "buidl"
  hint: Call it_list() to see existing sessions, or it_new() to create one.
  fields:
    session: buidl
```

```markdown
## Error

no session matches "buidl"

Call it_list() to see existing sessions, or it_new() to create one.
```

| Code | Raised when |
|---|---|
| `invalid_input` | Schema-valid but semantically wrong arguments, including unparseable `keys` |
| `no_active_session` | A tool defaulted to the active session and none is set |
| `session_not_found` | `session` matches no live or retained session |
| `session_exited` | Input was sent to a session whose process has ended |
| `name_conflict` | `it_new` was given a name already held by a live session |
| `daemon_unavailable` | The daemon could not be started or reached |
| `cancelled` | The MCP request context was cancelled |
| `internal_error` | Anything else |

Every hint names a concrete next tool call.

## Confirmed Naming Rules

- Tools are `it_active`, `it_list`, `it_new`, `it_read`, `it_send`, `it_kill`,
  `it_tail`, `it_head`.
- A session is addressed by `session`, accepting an id or a name.
- Literal typing is `text`; key chords are `keys`.
- Pagination is `page`, with no agent-facing `limit`.
- Log length is `lines`, which is a request rather than a guarantee.
- Durations are seconds and named `wait`.
