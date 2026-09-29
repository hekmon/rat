# ratd: design

This document explains why ratd is built the way it is: the design decisions and the
alternatives they rejected. What ratd is and how its parts fit is described by its godoc
(`doc.go`); the project invariants are in [`AGENTS.md`](../../AGENTS.md).

## Name

ratd follows the convention of daemons (`sshd`, `containerd`): it is a long running service,
and `rat` is left for a client (such as the ssh proxy below). It also keeps "server" for the
tmux server, which ratd owns.

## Topology

One ratd process per tenant (see the isolation model in `AGENTS.md`): a service owning the tmux
server of that tenant, and serving several MCP clients over the network.

Rejected alternatives, and why:

- **stdio, ratd run by each client on its own machine**: terminals live on the remote machine,
  which a local process can not reach.
- **stdio over ssh** (the client runs `ssh host ratd`), ratd owning tmux as it does: each
  connection would start its own ratd, and terminals would die with the connection, when
  persistence is the point of rat.
- **stdio over ssh with tmux as its own daemon**, ratd being a thin process per client
  (terminals then outlive every ratd process). Decisive: hosted clients (web and cloud agents)
  can not spawn a process, they only reach MCP servers by URL. And stopping rat would no longer
  stop everything: an admin stopping the service must be sure that no terminal and no way in is
  left, without hunting for tmux sockets (see `AGENTS.md`). The rest has answers, at a price:
  tmux supervised by nobody, or by a service of its own (systemd running `tmux -D`, rat's
  settings reapplied by each client process), which moves rat's work into configuration outside
  rat; the tmux daemon inheriting the environment of whichever ssh session started it (agent
  forwarding would give every terminal the user's ssh agent), unless that service starts it;
  and what one process guarantees today (a window name checked free, then created) turning into
  races between processes.

What ssh offers is not lost: ratd listening on 127.0.0.1, reached through an ssh tunnel, gets
its authentication and encryption, with no token on the network and no TLS to configure. A
stdio proxy run over ssh, relaying to the service, could be added later without changing this
design; the opposite choice would have shut out hosted clients.

## Transport

Streamable HTTP, the transport of the MCP specification for networked servers (HTTP+SSE, its
predecessor, is deprecated), with the official Go SDK (`github.com/modelcontextprotocol/go-sdk`).
Only the transport is needed, not its streaming:

- **Stateless mode**: each request carries everything it needs (token, tmux session), and tmux
  is the only source of truth, so ratd keeps no state per connection. It drops server to client
  requests (sampling, elicitation, roots), which rat does not need, and follows the direction of
  the specification and of the SDK, whose stateful handler rejects the newer protocol versions.
  Rejected: stateful sessions (`Mcp-Session-Id`), state kept for nothing.
- **JSON responses**: a request gets a single JSON answer rather than an event stream (SSE),
  which only serves messages sent before the result (progress, logs) and requests to the client.
  Tool calls are short, so no long lived connection is left for a proxy to cut, and exchanges
  are plain to debug.

Two different things are called a session: the tmux session holding the windows of a client,
and the MCP session of the protocol (`Mcp-Session-Id`, the SDK `ServerSession`), which stateless
mode does not use. Names in the code say which one they mean.

## Sessions

The tmux session of a client comes from its MCP client configuration, sent with every request in
an HTTP header. Agents never see it nor choose it: no tool mentions sessions. An HTTP middleware
validates the header with `tmux.CheckName`, without asking tmux, and answers 400 when it is
missing or invalid: a misconfigured client fails when it connects, in front of the human
configuring it, rather than later in front of an agent. Rejected: a tool parameter, which the
agent fills in and could change, and a URL path, which does not reach tool handlers.

## Authentication

A bearer token, compared in constant time. Never passed as a flag: command lines are visible to
every user (see the open questions in `AGENTS.md`). TLS is optional but first class: rat is
remote by name, and running it on 127.0.0.1 only for persistence is valid too, where the SDK
DNS rebinding protection (on by default) matters.

## Tools

Five tools, with short descriptions: small models read them once and forget the details. What
an agent needs to interpret a result goes in the result itself, only when it applies.

- **list windows**: name, foreground command, working directory and last activity of each
  window. It is the cheap way to check whether a command finished: bash in the foreground means
  the terminal waits for input, with caveats worth a hint (bash builtins and loops show as bash,
  ssh shows as ssh even when idle). Activity is relative ("12s ago"): models do not know the
  current time.
- **create window**: its description invites to list windows first, a session starting with a
  window named `main`.
- **close window**.
- **send input**: one call is one act, like a hand on a keyboard. Either text, pasted as a human
  pastes, then Enter if asked (the text is pasted, Enter runs it), or keys by their tmux names,
  not both: which keys to press usually depends on what the text caused on the screen. A text
  pasted while a command runs, or into a window created a moment ago (bash still starting), is
  read as typed, each line running: agents need to know it.
- **get content**: the screen, preceded by optional extra lines of scrollback (none by default:
  asking for many is the agent's choice). The result tells how many extra lines it holds and
  whether a full-screen program runs (`tmux.Snapshot`), as readable text rather than JSON
  escaped lines.

### Plumbing

Hidden from agents: any tool meeting a missing tmux session creates it (a concurrent creation is
not an error) and carries on, so an agent closing all its windows finds a fresh `main`. Close
window checks the session first and does nothing when there is nothing to close, rather than
creating a session to close a window in it: composing the primitives smartly is the job of this
layer. Nothing is retried automatically: a failure is reported, and the agent decides to retry.

### Errors

Phrased for agents from the controller sentinel errors, telling whether and how to retry. They
never forward tmux messages, on which agents can not act, nor session names.

## tmux server lifecycle

ratd keeps its tmux server running: that it needs a child process is its own concern, not the
one of the user nor of systemd. A supervisor starts the server, waits for it to exit
(`tmux.Controller.WaitServer`), logs why, and starts it again after a delay. A server dying
loses its terminals and their commands (a crash, the OOM killer, `tmux kill-server` typed in a
terminal): tools meanwhile answer that terminals are restarting, and the MCP instructions tell
agents that a restart makes every window disappear, which they infer by themselves. No restart
history is kept.

ratd exits instead when retrying makes no sense:

- a first start failing (tmux missing, bash too old, socket owned by another rat) is a
  configuration problem, reported while the admin is still there;
- a server dying repeatedly in a short time makes it exit with an error, so that the service
  shows as failed to whoever monitors it.

At each start, `tmux.Controller.CheckBracketedPaste` tells whether bash startup files defeat
bracketed paste. Terminals keep working (single line inputs are fine), so it is a warning:
logged for the operator, and added to the MCP instructions built at startup for agents.

## Open questions

- How the auth token reaches ratd (see `AGENTS.md`: environment, systemd).
- Configuration: tenant, listen address, TLS files, their format.
- The name of the tmux session header.
- The restart delay and the crash budget (for instance 5 deaths in 5 minutes).
- Output redaction (see `AGENTS.md`).
