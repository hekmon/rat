# RAT: Remote Agent Terminal

RAT is an MCP server giving agents persistent terminals on a remote machine. Agents can run
several long running commands in parallel without being blocked, and find them back after a
restart or a context compaction.

## Architecture

- **`tmux` package (controller)**: low level Go API over tmux, explicit (sessions are created
  and destroyed manually, no automatic behavior) but opinionated for agent usage: it exposes
  what agents need, not all of tmux, and enforces the invariants below.
- **MCP server**: high level, agent facing. Keeps the tool set minimal (list windows,
  create/close window, send input, get content) and handles the plumbing automatically
  (e.g. a session is created on first use) to avoid tool and context bloat.

A tmux session can not exist without a window: the controller creates sessions with a first
window named `main`, and a session disappears with its last window. The MCP server composes the
controller primitives to hide this from agents: whenever a tool meets a missing session, it
creates it (a concurrent creation is not an error) and carries on. An agent closing all its
windows simply finds a fresh `main` on its next call.

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
- **Terminals are the same on every machine**, whatever the user configuration and rat's own
  environment: bash, starting at home, a neutral UTF-8 locale, no pager, a fixed size.
- **Terminals are found by name**: tenant, session and window names are plain names, validated
  before reaching tmux, and targeted exactly. A window name is unique in its session; tmux IDs
  are never used nor stored.
- **Agent input reaches the terminal as is**: text is pasted literally, as a human pastes, never
  interpreted by tmux, and Enter is only pressed when asked.

How the tmux controller keeps them, and the tmux pitfalls behind each of them, are described in
[`tmux/README.md`](tmux/README.md).

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

### How we work

A guideline rather than a rule: rat is built with a human in the loop, closer to pair
programming than to reviewing results at checkpoints. It is slower, and it is what catches what
tests written in advance do not: most pitfalls described in `tmux/README.md` were found this way.

- Work in small steps, bottom up, each one building and passing its tests on its own: one
  commit, read by the other side before the next step starts.
- Surface what a step reveals (a tool pitfall, a doubtful choice) as soon as it is found, with
  its evidence, rather than fixing it silently among other changes: the finding is what the
  reader needs to challenge, and challenging it often leads to a better design than the fix.
- Read adversarially, whoever wrote: question every decision and every comment. A question from
  the reader means the code or its explanation misses something: answer it in the code (a
  comment, a name, a test), not only in the conversation.
- Longer autonomous iterations, reviewed at checkpoints, fit mechanical changes. The design, and
  anything touching the invariants, goes step by step.

### Code conventions

- Exported errors are sentinels named `ErrXxx`, wrapped with `%w`.
