# Agent Guidelines

## Scope discipline (CRITICAL)

- **Break every task into the smallest possible independent scope.** One agent = one concern.
- **Fan out**: launch multiple scoped agents in parallel when work is independent.
- **Sequential when dependent**: scout → plan → dev → review. Never dev before scout reports.
- **Never do by hand what an agent can do.** Manual edits are error-prone and waste tokens.
- **Keep agent prompts focused** — one file or one feature per agent. Large multi-file rewrites in a single agent prompt time out or produce incomplete work.

## Workflow

1. **Scout** (parallel, read-only): investigate the codebase, report findings. 3-5 agents fanned across different areas.
2. **Consolidate**: merge scout findings into a prioritized plan.
3. **Dev** (sequential, one concern per agent): implement fixes one scope at a time. Build + test after each.
4. **Review** (read-only): verify the dev agent's work. Report GREEN or findings.
5. **Fix** (dev): address review findings.
6. **Repeat** review → fix until GREEN.

## Anti-patterns to avoid

- Launching scout + dev simultaneously (dev needs scout findings first).
- One giant agent prompt covering multiple files/concerns.
- Manual hand-editing when agents are available.
- Asking the user questions when the task is clear — proceed autonomously.
- Implementing backwards compatibility the user didn't ask for — confirm first.

## Project-specific notes

- **The daemon is load-bearing.** Sessions must outlive any single MCP process,
  so `internal/daemon` owns every PTY and everything else is a client of it.
  Never move terminal state into the MCP server or the CLI.
- **Run `-race` on anything touching sessions.** The daemon multiplexes PTY
  readers, emulator writes, log writers, broadcasters, and socket handlers. Two
  real bugs (a lost-output drain and an emulator deadlock) were caught only
  under the race detector.
- **The emulator's reply pipe must always be drained.** Programs query the
  terminal on startup and block for the answer; an undrained emulator deadlocks
  every session. See `Session.answerQueries`.
- **Alternate-screen output never reaches the transcript.** That is correct
  terminal behaviour, not a bug. `it_tail` compensates by returning the live
  screen; don't "fix" it by writing alt-screen frames to the log.
- **Agent-facing strings are part of the contract.** Every error carries a
  concrete next tool call, and truncation always reports what was dropped plus a
  path to the whole file. Tests in `internal/mcpserver` pin this.
