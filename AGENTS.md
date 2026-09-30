# RAT: Remote Agent Terminal

RAT gives agents shell access over MCP: persistent terminals on a remote machine. Agents can run
several long running commands in parallel without being blocked, and find them back after a
restart or a context compaction. It uses tmux as its terminal emulator, but it is not a tmux MCP
server: agents see a few tools, each serving a purpose, never tmux itself.

In effect, rat hands agents persistent remote shells: a human operator must be able to cut them
at once, and to know what they leave behind.

## Architecture

- **`tmux` package (controller)**: low level Go API over tmux, explicit (sessions are created
  and destroyed manually, no automatic behavior) but opinionated for agent usage: it exposes
  what agents need, not all of tmux, and enforces the invariants below. Its naming rule is a
  package of its own (`tmux/names`), shared with mtls.
- **`mtls` package**: the TLS contract between ratd and its clients: closed bundles (a CA, a
  server named after its tenant and clients named after their session, generated together) and
  the TLS configuration of each side.
- **`ratd` (MCP server)**: high level, agent facing, served over HTTP with mutual TLS. Each of
  its tools serves a purpose (list, create and close windows, send text or keys, read a window,
  read and write files), and composing the controller primitives to serve it is ratd's value:
  the plumbing (e.g. a session created on first use) stays hidden, to avoid tool and context
  bloat. It reads and writes files itself: tmux is its terminal backend, not its file backend.
- **`rat` (bridge)**: a stdio MCP server relaying to ratd, for harnesses that can not present a
  client certificate (most of them). It relays messages as they are, presenting the
  certificate.
- **`rat-tool`**: the utility. It creates and inspects bundles, and checks a running ratd end to
  end. It holds every secondary feature, so that ratd and rat keep a single mode each.

A tmux session can not exist without a window: the controller creates sessions with a first
window named `main`, and a session disappears with its last window. ratd hides this from agents:
whenever a tool needs a missing session, it creates it (a concurrent creation is not an error).
An agent closing all its windows simply finds a fresh `main` on its next call.

## Principles

- **tmux is the terminal emulator.** It handles control characters, cursor moves and live UIs
  redraws. rat never parses raw terminal output: it reads the rendered screen (`capture-pane`).
- **tmux is the single source of truth.** rat keeps no cache: no output buffer, no stored
  window or pane IDs. Every read asks tmux, even if a high level operation takes several tmux
  commands. This is what allows agents to rediscover their terminals (window names, running
  commands, paths, activity) after a restart.
- **Terminal input, not command execution.** Agents act on a terminal as a human does: they
  paste text and press keys. Pressing Enter is optional (e.g. "press y to confirm"), and the
  input may answer a prompt rather than start a command. rat does not know when a command
  finishes, only which process is in the foreground.
- **Humans peek, they do not take over.** The terminals are the agents'. A human may attach to
  rat's tmux server to watch them work, and what a peek leaves behind must not change what agents
  get: a window keeps its size, and a mode left on (copy mode, to scroll back) is left before the
  next input. Disturbing the terminals beyond a peek is on the human: rat does not guard against
  it.
- **Files, as a human copies them.** Agents also move whole files to and from the machine,
  without a terminal: exact content, nothing on a screen, whatever the terminals are doing.
  Files are read and written on ratd's machine, as its user, never inside an ssh session or a
  container running in a terminal.
- **What reaches a model is bounded.** Window captures and file reads are checked (text, a
  regular file) and bounded (the read budget) before being sent: what reaches a model's context
  can not be taken back.
- **ratd is the entry point, and a standard MCP server.** A harness written with any MCP SDK must
  be able to use it, with a client certificate as the only addition. rat adapts ratd for the
  harnesses that can not present one: it is never a requirement, and nothing is added to it
  that ratd would lack.
- **KISS.** Prefer what tmux already provides over rebuilding it.

## Isolation model

| Level | Maps to | Set by | Purpose |
|---|---|---|---|
| Tenant | one tmux server (`-L rat-<tenant>`), one ratd process, its own endpoint and mTLS bundle | the server certificate of the bundle | isolation between rat instances |
| Session | one tmux session | a client certificate of the bundle | telling apart the clients of a tenant |
| Window | one terminal running `bash` | the agent, by name | a workspace |

Both names are chosen when the bundle is generated, and carried by its certificates: ratd reads
its tenant from its server certificate, and the session of each request from the client
certificate presenting it. Agents never choose their tenant nor their session, and a client can
only reach its own session. Agents meant to share terminals share a client certificate. The
controller accepts an empty tenant (socket `rat`), ratd does not: a tenant is always named.

**Limit:** all tenants of a Unix user run as that user. A command typed in a terminal can reach
other tenants (tmux sockets, signals, files), or even run tmux within tmux. Within a tenant, ratd
keeps each client to its session, but a command typed in a terminal reaches every session of its
tmux server (`TMUX` is kept, see `tmux/README.md`). Tenants and sessions protect against
mistakes, not against a malicious agent: real isolation requires a Unix user (or container) per
tenant.

## Invariants

- **rat owns its tmux server**: it starts it (`-D`, never daemonized), watches it and stops it.
  It refuses a socket already served by another server, which keeps a tenant to a single ratd on
  a machine (for a Unix user).
- **Stopping rat leaves nothing behind**, in three levels:
  - **Stopping ratd closes the door**: first its endpoint, so that no call gets in anymore, then
    its tmux server, every terminal and the processes attached to them. An admin stopping the
    service is sure that no terminal and no way in is left, without hunting for tmux sockets.
  - **The service manager stops what escapes**: commands detached from their terminal (`nohup`,
    `setsid`, daemons) survive tmux. systemd stops them with the service, killing every process
    left in its control group (`KillMode=control-group`, the default, which must be kept).
    Running ratd as a service is what makes the kill switch complete: started by hand, detached
    commands outlive it.
  - **A dedicated Unix user bounds what was set up on purpose**: what an agent makes persistent
    outside the service (crontab, user services, ssh keys, files) is beyond it. A Unix user for
    rat alone lets the admin find and stop all of it at once, and lock the account.
- **A tmux server dying on its own is restarted** (a crash, the OOM killer, `tmux kill-server`
  typed in a terminal), by ratd, the controller only reporting the exit. Its terminals and their
  commands are lost, as when ratd stops. Repeated deaths make ratd exit, for its supervisor to
  notice.
- **Terminals are the same on every machine**, whatever the user configuration and rat's own
  environment: bash, starting at home, a neutral UTF-8 locale, no pager, bracketed paste, a
  fixed size.
- **Terminals are found by name**: tenant, session and window names are plain names, validated
  before reaching tmux, and targeted exactly. A window name is unique in its session; tmux IDs
  are never used nor stored.
- **Agent input reaches the terminal as is**: text is pasted literally, as a human pastes, never
  interpreted by tmux (not even by a mode a human left the terminal in), and Enter is only
  pressed when asked.

How the tmux controller keeps them, and the tmux pitfalls behind each of them, are described in
[`tmux/README.md`](tmux/README.md).

## Open questions

- **Output redaction** (MCP layer): configured words or patterns masked in window captures and
  file reads before they are sent. Agents keep using the secrets on the machine (they have a
  shell to do things), but their values do not travel back to the model, its logs or
  transcripts. Best effort only: it matches the literal output, not an encoded or split value.

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
- The tmux tests also run with the oldest tmux supported, in a Docker container
  (`TestMinimumTmux`): Docker is required as well, and `-short` skips it, as an explicit choice
  to test less.

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
- The three binaries share their command line conventions: `urfave/cli` v3 (long and short
  flags, the same help), rather than the standard `flag` package, whose flags and help differ.
- Logs go through `log/slog`, text handler, on stderr: readable in `journalctl` and in a
  terminal.
