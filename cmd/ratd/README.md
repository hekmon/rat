# ratd: design

This document explains why ratd is built the way it is: the design decisions and the
alternatives they rejected. What ratd is and how its parts fit is described by its godoc
(`doc.go`); the project invariants are in [`AGENTS.md`](../../AGENTS.md).

## Name

ratd follows the convention of daemons (`sshd`, `containerd`): it is a long running service,
and `rat` is its client, the stdio bridge (see Topology). It also keeps "server" for the tmux
server, which ratd owns.

## Topology

One ratd process per tenant (see the isolation model in `AGENTS.md`): a service owning the tmux
server of that tenant, and serving several MCP clients over the network. The tenant is named by
the server certificate of its bundle, not by a setting: a ratd can not serve a tenant with the
bundle of another.

Rejected alternatives, and why:

- **stdio, ratd run by each client on its own machine**: terminals live on the remote machine,
  which a local process can not reach.
- **stdio over ssh** (the client runs `ssh host ratd`), ratd owning tmux as it does: each
  connection would start its own ratd, and terminals would die with the connection, when
  persistence is the point of rat.
- **stdio over ssh with tmux as its own daemon**, ratd being a thin process per client
  (terminals then outlive every ratd process). Decisive: stopping rat would no longer stop
  everything: an admin stopping the service must be sure that no terminal and no way in is left,
  without hunting for tmux sockets (see `AGENTS.md`). The rest has answers, at a price: tmux
  supervised by nobody, or by a service of its own (systemd running `tmux -D`, rat's settings
  reapplied by each client process), which moves rat's work into configuration outside rat; the
  tmux daemon inheriting the environment of whichever ssh session started it (agent forwarding
  would give every terminal the user's ssh agent), unless that service starts it; and what one
  process guarantees today (a window name checked free, then created) turning into races between
  processes.

### The bridge

Clients authenticate with a certificate (see Authentication), which most harnesses can not
present when they configure an HTTP MCP server, while all of them can start a stdio one. `rat` is
that stdio server: started by the harness, it relays every message to ratd, presenting the
client certificate. It keeps no state and holds no terminal: persistence stays in ratd. A
harness able to present the certificate, and to accept the server without checking its host name
(see package `mtls`), can target ratd directly: the bridge adds no protocol of its own.

The bridge relays JSON-RPC messages as they are, rather than being an MCP client and server
repeating ratd's tools and instructions: it never needs to change when ratd does.

Hosted clients (web and cloud agents) are left out on purpose: they only reach MCP servers by
URL, and can present no certificate. Admitting them would take a credential stored by a third
party, in front of a user shell.

An ssh tunnel to a ratd listening on 127.0.0.1 is still possible, and needs mTLS all the same
(see Authentication).

## Transport

Streamable HTTP, the transport of the MCP specification for networked servers (HTTP+SSE, its
predecessor, is deprecated), with the official Go SDK (`github.com/modelcontextprotocol/go-sdk`).
Only the transport is needed, not its streaming:

- **Stateless mode**: each request carries everything it needs (its client certificate names
  its tmux session), and tmux is the only source of truth, so ratd keeps no state per
  connection. It drops server to client requests (sampling, elicitation, roots), which rat does
  not need, and follows the direction of the specification and of the SDK, whose stateful
  handler rejects the newer protocol versions.
  Rejected: stateful sessions (`Mcp-Session-Id`), state kept for nothing.
- **JSON responses**: a request gets a single JSON answer rather than an event stream (SSE),
  which only serves messages sent before the result (progress, logs) and requests to the client.
  Tool calls are short, so no long lived connection is left for a proxy to cut, and exchanges
  are plain to debug.

ratd stays a standard MCP server: a client written with any MCP SDK connects to it with a
single addition, outside the protocol: an HTTP client presenting the client certificate and
accepting the server without checking its host name (package `mtls`). The bridge is such a
client.

The SDK bounds request bodies to 4 MiB of JSON (`DefaultMaxRequestBodyBytes`), answering 413
beyond. It is kept: it protects ratd from exhausting its memory, and a tool call is written by a
model within its output limit, a small fraction of it. The limit stays usable: bash reads a 4
MiB paste at its prompt in about 11 seconds (1 MiB in under 2, measured with bash 5.3 and tmux
3.7c), a time growing faster than the size. The controller sets no limit on pasted text (see
`tmux/README.md`): the transport does, and the bridge's stdio transport allows more (16 MiB per
message).

Two different things are called a session: the tmux session holding the windows of a client,
and the MCP session of the protocol (`Mcp-Session-Id`, the SDK `ServerSession`), which stateless
mode does not use. Names in the code say which one they mean.

## Sessions

The tmux session of a request is the name in the client certificate presenting it: a session is
a client of the tenant, named when the bundle is generated. Agents never see it nor choose it
(no tool mentions sessions), and a client can only reach its own session. Agents meant to share
terminals share a client certificate; agents meant to be apart get one each.

The stateless handler asks for a server with each request (`getServer`, given the HTTP request):
ratd reads the client name from the TLS connection there, and returns a server whose tools are
bound to that session. Tools need nothing from the request, so they can be tested without HTTP.

Rejected:

- **A header naming the session**, set by the client configuration: any client could pick any
  session, the harness or bridge configuration grows, and a client connecting directly would
  need to send it.
- **A tool parameter**, which the agent fills in and could change.
- **A URL path**, which does not reach tool handlers.

Sessions are only kept apart by ratd: a command typed in a terminal reaches every session of its
tmux server (see the limit of the isolation model in `AGENTS.md`).

## Authentication

Mutual TLS, always, loopback included: ratd hands out a user shell, and a loopback address is
reachable by every user of the machine. There is no plain HTTP mode. Clients present a
certificate from the bundle of the tenant (package `mtls`: a closed bundle, generated with
`rat-mtls`), and ratd presents the server certificate of that bundle. The client name, which is
its session, is logged with each call, telling which client did what.

Rejected: a bearer token. It is a secret stored next to the terminals, in a file or an
environment that agents, running as the same user, can read; with mTLS, nothing on the server
lets a client in (see package `mtls`).

The SDK DNS rebinding protection (on by default) is kept, although redundant: it stops a browser
tricked into calling a local server, and a browser can not present the client certificate.

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

- Configuration: listen address (default port), bundle directory.
- The restart delay and the crash budget (for instance 5 deaths in 5 minutes).
- Output redaction (see `AGENTS.md`).
