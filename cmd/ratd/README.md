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
`rat-tool`), and ratd presents the server certificate of that bundle. The client name, which is
its session, is logged with each call, telling which client did what.

Rejected: a bearer token. It is a secret stored next to the terminals, in a file or an
environment that agents, running as the same user, can read; with mTLS, nothing on the server
lets a client in (see package `mtls`).

The SDK DNS rebinding protection (on by default) is kept, although redundant: it stops a browser
tricked into calling a local server, and a browser can not present the client certificate.

## Tools

Eight tools, each serving a purpose; composing the controller primitives to serve it is what
ratd adds. Descriptions are short, as small models read them once and forget the details: what
an agent needs to interpret a result goes in the result itself, only when it applies. Results
are readable text, with a header line between brackets when there is something to say, rather
than JSON.

No tool waits for a command: sending returns once tmux holds the input (47 ms for a 4 MiB paste,
measured with tmux 3.7c), reading returns what is displayed at that moment. This is what makes
rat fit long running commands: an agent starts one, carries on, and comes back to check it.

### Terminals

- **`list_windows`**: for each window, its name, foreground command, working directory and last
  activity, plus when they apply: a full-screen program, the scrollback size, copy mode.
  Activity is relative ("12s ago"): models do not know the current time. The description tells
  it is the cheap way to check whether a command finished: bash in the foreground means the
  terminal waits for input, with its caveats (bash builtins and loops show as bash, ssh shows as
  ssh even when idle).
- **`create_window`** `{name}`: makes sure the session exists, then the window. Asking for
  `main` in a missing session is a success: creating the session made it. An existing window is
  not an error but a message saying it was not created, with what it runs and where: the agent
  must not believe it got a fresh terminal. The description invites to list windows first (they
  persist, `main` exists), and to wait for the prompt of a new window before sending text.
- **`close_window`** `{name}`: terminates what runs in the window. Nothing to close (missing
  window or session) is a message, not an error, and creates nothing.
- **`send_text`** `{window, text, enter}`: pastes the text as a human pastes, then presses Enter
  if asked. `enter` is required, with no default: the agent decides every time whether the text
  runs, and forgetting it fails validation instead of silently leaving a command unrun. Rejected
  defaults: false (small models forget it), true (runs text meant to wait, against "Enter only
  when asked"). The description tells what the text meets: at a bash prompt it waits on the
  command line, new lines included, until Enter; while a command runs, or before a new window
  shows its prompt, it is read as typed, each line running.
- **`send_keys`** `{window, keys}`: presses keys in order. The description lists the names the
  controller accepts, one per key (aliases are accepted, not shown): a printable character,
  `Space`, `Enter`, `Tab`, `BTab`, `BSpace`, `Escape`, `Up`, `Down`, `Left`, `Right`, `Home`,
  `End`, `PageUp`, `PageDown`, `Insert`, `Delete`, `F1` to `F12`, with the `C-`, `M-` and `S-`
  modifiers, combinable. An unknown name is refused with a pointer to `send_text`.
- **`read_window`** `{window, scrollback_rows}`: the screen (200×24), preceded by up to
  `scrollback_rows` rows of history (0 by default). The header tells the cursor position, how
  many rows it holds and why fewer than asked (a shorter history, a full-screen program, the
  read budget), a full-screen program, copy mode. Under a full-screen program, the history
  belongs to the terminal before the program started: it is never included.

Text and keys are two tools rather than one `send_input` taking either: JSON Schema can not say
"exactly one of", so a small model would learn it by failing, where two tools each have a schema
the SDK validates. One call stays one act either way: which keys to press usually depends on
what the text caused on the screen.

Rows, not lines: captures join the lines tmux wrapped (`-J`), so a result holds whole lines while
its history is counted in terminal rows. A long line wrapped over several rows counts for each.

Copy mode and the cursor position are not exposed by the controller yet. Copy mode matters: a
human inspecting a window can leave it in copy mode, and every key then goes to tmux, not to the
program. ratd shows it rather than leaving it, which would pull the view from under the human.

### Files

ratd reads and writes files itself, on its own machine and as its own user (see the principles
in `AGENTS.md`): tmux, its terminal backend, would only add a round trip, and replace precise
errors with its messages.

- **`write_file`** `{path, content}`: creates the file or replaces it, and says which (with the
  former size). A replaced file keeps its mode, a new one gets 0644 minus ratd's umask. Missing
  directories are not created. Only a regular file is written: a FIFO would block, and a device
  is not a file to replace.
- **`read_file`** `{path, start_line, max_lines}`: whole lines of a text file, from `start_line`
  (1 by default, negative counts from the end: -100 for the last 100 lines), at most `max_lines`
  (all by default). The header tells the lines returned out of how many, the size and the last
  modification ("modified 3s ago": a command may still be writing).

Paths are absolute or start with `~/`. Rejected: paths relative to a window, whose directory is
that of its foreground process: under ssh, the local ssh client's, so the file would silently
land elsewhere. `list_windows` shows the directories, for the agent to build absolute paths.

What `read_file` sends is checked first, as it can not be taken back from a model's context:

1. only a regular file is read: a FIFO would block forever, `/dev/zero` never ends;
2. the read is bounded whatever the size tells: `/proc` files show 0 and stream, a log grows
   while being read;
3. the content must be text: no NUL byte (the heuristic of git and `grep -I`) and valid UTF-8.
   Otherwise the tool refuses, pointing to the terminal (`file`, `xxd`, `base64`). Rejected:
   running `file`, not always installed, with an output to parse, and saying nothing of size;
4. the result fits the read budget. Without a range, a file over the budget returns only its
   size, line count and modification time, for the agent to pick a range: nothing big lands in
   its context by surprise. With a range, a read returns the whole lines that fit and tells where
   to continue, as `read(2)` returns fewer bytes than asked. A single line over the budget (a
   minified file) can not be cut: the tool points to the terminal (`head -c`, `cut`).

Lines rather than bytes: agents think in lines, and a byte offset cuts lines and characters. The
cost is that reaching line 30000 reads what comes before, having no index: fast enough for
ordinary files. Reading from the end reads backwards, instant on any size. The total line count
is only given when the file is small enough to be scanned within the call.

### Read budget

A result of `read_window` or `read_file` holds at most the read budget, 64 KiB by default (about
16k tokens), set by `--read-budget`. `read_window` keeps the rows closest to the screen that fit;
the screen alone always does (about 5 KB, 20 KB with multibyte characters): ratd refuses a budget
under 32 KiB. Harnesses also cut or divert large results, and a larger context still degrades as
it fills.

It is a setting of ratd, not a parameter of the tools: the admin knows which models and harnesses
connect, and long reads stay possible, one range at a time. The descriptions state the value.
Rejected: an agent parameter under a ceiling set on ratd (small models set it large "to be
safe", and it does nothing while the ceiling is the default), a constant (a rebuild for a
number).

### Annotations

| Tool | Read only | Destructive | Idempotent |
|---|---|---|---|
| `list_windows`, `read_window`, `read_file` | yes | | |
| `create_window` | | | yes (an existing window is reported, not recreated) |
| `close_window` | | yes | yes |
| `send_text`, `send_keys` | | yes | |
| `write_file` | | yes (it replaces) | yes |

### Plumbing

Hidden from agents: a tool needing a missing tmux session creates it (a concurrent creation is
not an error), so an agent closing all its windows finds a fresh `main`. What each tool does then
is part of its purpose:

- `list_windows` and `read_window` create it, and show the fresh `main`.
- `send_text` and `send_keys` create it, but send nothing: bash is still starting in the new
  `main`, and a pasted text would run line by line. This is the usual case after a tmux restart,
  when the agent retries its command. The error says so.
- `close_window` never creates it: there is nothing to close.
- The file tools do not touch sessions.

Nothing is retried automatically: a failure is reported, and the agent decides to retry.

### Errors

Phrased for agents, telling whether and how to retry. They never forward tmux messages, on which
agents can not act, nor session names. IsError marks a result whose action was not done; a
message tells an outcome the agent may act upon, such as nothing to close.

| Case | IsError | Result |
|---|---|---|
| sending to or reading a missing window | yes | the window does not exist, `list_windows` shows the others |
| sending to a session just created | yes | `main` was just created, nothing was sent: wait for its prompt |
| invalid window name | yes | the naming rule |
| unknown key | yes | the key names, and `send_text` to type text |
| tmux restarting | yes | every window was lost, retry in a few seconds to find a fresh `main` |
| tmux not answering in time | yes | logged, retrying may work |
| file refused (not regular, not text, missing, denied, relative path, a range too far into a huge file to reach in time) | yes | why, and the terminal when it can do better |
| anything else | yes | internal error, logged, retrying may work |
| nothing to close | no | nothing to close |
| window already existing | no | not created, what it runs and where |
| file over the budget, without a range | no | size, line count, modification time, how to ask a range |

## MCP instructions

Built at startup, sent to each client when it initializes:

> rat gives you persistent terminals on *host*: commands run there, not where you run. It is
> asynchronous by design: sending a command returns at once, without waiting for it to finish.
> Start long running commands (builds, tests, deployments), carry on with other work, and come
> back to check them: rat does not know when a command finishes, list_windows shows the
> foreground command (bash means the terminal waits for input), read_window shows the screen.
> Run several commands in parallel in several windows. Each terminal is a window running bash,
> found by name: windows persist across your restarts and context compactions, so call
> list_windows first. A window named main exists when you start. To move a whole file, or to read
> a long output (redirect it to a file), use write_file and read_file rather than the terminal.
> If the terminals restart after a failure, every window disappears and you find a fresh main.

The host name tells an agent that has a local shell too where commands run. The tenant and the
session are left out, agents having no use for them; the server info title carries the tenant
and the host (`rat prod on host`), for clients to display. When the bracketed paste check fails
or can not run (see below), a sentence is added: multi-line text may run line by line even at a
prompt, send one line at a time.

Tool names are fixed, with no tenant prefix: MCP has no namespaces, and harnesses prefix tool
names with the server name of their own configuration (`rat-prod`, `rat-staging`). A prefix in
ratd would repeat it, and make names vary with the tenant.

## Lifecycle

### Startup

In this order, a failure at steps 1, 2 or 4 making ratd exit while the admin is still there:

1. **Load and check the bundle**: the server certificate names the tenant.
2. **Start the tmux server**, which takes the tenant: a server already answering on its socket
   (`tmux.ErrServerSocketInUse`) means another ratd serves it on this machine, which the error
   names with its PID. Other failures: tmux missing, bash missing or too old.
3. **Check bracketed paste**, once (`tmux.Controller.CheckBracketedPaste`, 10 seconds at most):
   whether bash startup files defeat it. Terminals keep working either way (single line input is
   fine), so an override, or a check that can not run, is a warning: logged, and added to the MCP
   instructions. A startup file blocking also blocks every terminal: the log says so. Checked
   once rather than at each tmux restart: clients receive the instructions once, and must not be
   told something the server no longer believes. A startup file changed while ratd runs is caught
   at its next start.
4. **Listen**: a second ratd for the same tenant fails at step 2, before opening an endpoint.

When less than a year of validity remains on the bundle, ratd warns at startup and every 24 hours:
sysadmins need reminding, an expired bundle stops every client at once.

### tmux server

ratd keeps its tmux server running: that it needs a child process is its own concern, not the
one of the user nor of systemd. A supervisor waits for the server to exit
(`tmux.Controller.WaitServer`), logs why, and starts it again after a second. A server dying
loses its terminals and their commands (a crash, the OOM killer, `tmux kill-server` typed in a
terminal): tools meanwhile answer that the terminals are restarting, and agents learn of it by
their windows disappearing, which the instructions announce.

Five deaths within five minutes make ratd exit with an error, so that the service shows as
failed to whoever monitors it. A restart failing counts as a death: a stray server may hold the
socket (a command racing a crash can start one), which `ErrServerSocketInUse` names. Only the
times of recent deaths are kept, for this budget.

### Shutdown

On SIGTERM or SIGINT, the door closes before the terminals: ratd stops accepting connections and
gives the calls in flight 10 seconds, then stops the tmux server (with the controller's own
escalation, SIGTERM then SIGKILL). The other order would answer calls arriving in between that
the terminals are restarting.

### Timeouts

- **A tool call: 10 seconds.** No call waits for a command, and tmux answers in milliseconds even
  for the largest inputs (47 ms for a 4 MiB paste, 11 ms for a capture of 10000 rows): beyond,
  tmux is stuck, and the agent is told so.
- **HTTP**: `ReadHeaderTimeout` 10 seconds, which also bounds the TLS handshake (net/http bounds
  it by the smallest of its read and write timeouts), and `IdleTimeout` 2 minutes. No
  `ReadTimeout` nor `WriteTimeout`: a 4 MiB request on a slow link must not be cut.

## Serving

- **Endpoint**: `/mcp`, anything else answers 404. The MCP specification imposes no path; `/mcp`
  is its example, and the convention of the SDK. ratd serves nothing else: `rat-tool` checks a
  running ratd.
- **Listen address**: all interfaces by default (`:7281`), which mutual TLS makes safe. Written
  with an empty host rather than `[::]`: Go then listens on IPv6 and IPv4 at once, where a
  literal `[::]` fails on machines with IPv6 disabled. 7281 reads "RAT!" on a phone keypad; IANA
  lists it for an obscure product (`itactionserver2`), and it is configurable.
- **TLS**: served through a TLS listener built from the checked bundle. `http.Server.TLSConfig`
  alone would be silently ignored by `ListenAndServe`, serving plain HTTP: a test must guard that
  a client without a certificate is refused. HTTP/1.1 only (no `h2` offered): requests are short
  JSON exchanges.
- **Rejected handshakes** are logged, rate limited: a public port gets scanned. The standard
  library would otherwise print them through the `log` package, outside ratd's logs.

## Logs

One line per tool call: the session (the client name), the tool, the window, the outcome and the
duration, plus what tells the action without its content: the size of a text and whether Enter
was pressed, the number of keys, the rows read, the path and size of a file. Never the content:
neither text, nor keys (they can spell a password one key at a time), nor files. And the
lifecycle: startup (tenant, address, bundle expiry), the bracketed paste check, tmux exits and
restarts, the crash budget, shutdown.

## Configuration

```
ratd --bundle DIR [--listen :7281] [--read-budget 64KiB] [--log-level info]
```

`--bundle` is the server directory of a bundle (see `rat-tool`); the tenant comes from its
certificate. No configuration file nor environment variable: nothing ratd needs is secret on a
command line.

## Open questions

- Output redaction (see `AGENTS.md`).
