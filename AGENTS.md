# RAT: Remote Agent Terminal

RAT is an MCP server giving agents persistent terminals on a remote machine. Agents can run
several long running commands in parallel without being blocked, and find them back after a
restart or a context compaction.

## Architecture

- **`tmux` package (controller)**: low level Go API over tmux. Thin and explicit: sessions are
  created and destroyed manually, no automatic behavior.
- **MCP server**: high level, agent facing. Keeps the tool set minimal (list windows,
  create/close window, send input, get content) and handles the plumbing automatically
  (e.g. a session is created on first use) to avoid tool and context bloat.

## Principles

- **tmux is the terminal emulator.** It handles control characters, cursor moves and live UIs
  redraws. rat never parses raw terminal output: it reads the rendered screen (`capture-pane`).
- **tmux is the single source of truth.** rat keeps no cache: no output buffer, no stored
  window or pane IDs. Every read asks tmux, even if a high level operation takes several tmux
  commands. This is what allows agents to rediscover their terminals (window names, running
  commands, paths, activity) after a restart.
- **Terminal input, not command execution.** Agents type into a terminal: sending Enter is
  optional (e.g. "press y to confirm"), and the input may answer a prompt rather than start a
  command. rat does not know when a command finishes, only which process is in the foreground.
- **KISS.** Prefer what tmux already provides over rebuilding it.

## Isolation model

| Level | Maps to | Set by | Purpose |
|---|---|---|---|
| Tenant | one tmux server (`-L rat-<tenant>`), one rat process, its own endpoint and auth | MCP server configuration | isolation between rat instances |
| Session | one tmux session | MCP client configuration | organization between agents/clients (not enforced) |
| Window | one terminal running `bash` | the agent, by name | a workspace |

The tenant name is optional: an empty one is the default tenant (socket `rat`). Agents never
choose their tenant nor their session.

**Limit:** all tenants of a Unix user run as that user. A command typed in a terminal can reach
other tenants (tmux sockets, signals, files), or even run tmux within tmux. Tenants protect
against mistakes, not against a malicious agent: real isolation requires a Unix user (or
container) per tenant.

## Invariants

- **rat owns its tmux server**: it starts it (`-D`, never daemonized), watches it and stops it.
  It refuses a socket already served by another server. Stopping rat stops its terminals.
- The server ignores the user tmux configuration: rat sets the options it relies on itself.
- Terminals run `bash`: always available, and the shell agents know best.
- Tenant, session and window names are validated (plain names only): they end up in socket
  paths and tmux targets.
- tmux targets are always exact (`=session:=window`), never tmux IDs (`%pane`, `@window`),
  which are global to the server and would cross sessions. Window names are unique per session.
- Literal input is sent with `send-keys -l --`, special keys (Enter, C-c, …) separately.

## Open questions

- **rat's own secrets** (e.g. the MCP endpoint auth token). Never as flags: command lines are
  visible to every user (`ps`, `top`). The likely way is a systemd unit reading an environment
  file readable by root only, which hides them from other users. Not from the agents: rat and
  its terminals run as the same user, so its environment is readable (`/proc/<pid>/environ`,
  which keeps the startup environment even after an unset) and terminals inherit it. The token
  only grants a shell as that user, which agents already have; still, rat should keep its
  secrets out of the terminals environment so that a plain `env` does not print them. Hiding
  them from agents for real requires running rat and its terminals as different users.
- **Output redaction** (MCP layer): configured words or patterns masked in captures before they
  are sent. Agents keep using the secrets on the machine (they have a shell to do things), but
  their values do not travel back to the model, its logs or transcripts. Best effort only: it
  matches the literal output, not an encoded or split value.

## Working on rat

rat is built with agents, and every agent session starts from scratch: it sees what the code
does, not why. The why (which alternatives were tried and dropped, which invariant a line
protects, which tmux behavior a detail works around) is what gets lost first. Once it is gone,
changes pile up as patches around the structure instead of through it, until nobody can tell
what is safe to change. The rules below keep the why alive: written where the next reader will
look, and enforced by tests where it can be checked.

### Where the why is written

- **Package documentation** (`doc.go`, plus a README in the package when it needs room) holds
  the design: the problem the package addresses and what it leaves out on purpose, its concepts,
  how it relates to the other packages, and its design decisions along with the alternatives
  that lost. A decision spanning several packages is written once, in the package that owns it,
  or in this file when none does.
- **Doc comments on declarations** hold the contract: what it does and returns, the state it
  leaves after success and after failure, what the caller can count on and what the caller must
  do. Not how it does it: the implementation may change, the contract should not. When the
  contract goes against Go habits or looks like a bug, give the reason in a sentence: godoc
  readers never see the body.
- **Comments inside bodies** hold the local reasons. A body reads as a series of steps; comment
  a step when its purpose, the invariant it keeps or the alternative it avoids is not obvious.
  Only the chosen path is visible in code, so name the rejected ones when they were real
  contenders. Never paraphrase the code below: it adds nothing and will drift.
- **Names** carry meaning too: a name stating its concept needs less comment.

Everywhere:

- Write what the next reader needs, not everything. An explanation nobody maintains ends up
  wrong, and a wrong explanation is worse than none.
- Update an explanation in the same commit as the code it explains. When they disagree, one of
  them left the design: find out which (ask if unsure) instead of aligning one on the other.
- Explanations stand on their own. Never justify something by pointing to a plan, a ticket or
  a conversation: those vanish, the code stays.

### Tests

Comments rot quietly, tests break loudly. When a decision can be checked, write the check.

- A bug fix comes with a regression test that fails without the fix.
- A test's doc comment states the behavior it protects.
- Failure paths are tested, not only the happy ones: most of rat is about what happens when
  tmux or the processes it runs misbehave.
- Tests drive a real tmux, which rat cannot work without: a test run lacking tmux fails instead
  of skipping, since a green run that tested nothing is a false signal.

### Code conventions

- Exported errors are sentinels named `ErrXxx`, wrapped with `%w`.
