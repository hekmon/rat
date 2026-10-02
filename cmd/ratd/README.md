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
(see package `mtls`), can target ratd directly: the bridge adds no protocol of its own. How it
relays is described in the README of `cmd/rat`.

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
accepting the server without checking its host name (package `mtls`). `rat-tool check` is such
a client, written with the Go SDK as a harness author would.

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
The SDK passes tool handlers no more of the request than its headers (`RequestExtra`), which is
why the session is bound when the server is built.

Rejected:

- **A header naming the session**, set by the client configuration: any client could pick any
  session, the harness or bridge configuration grows, and a client connecting directly would
  need to send it.
- **A tool parameter**, which the agent fills in and could change.
- **A URL path**, which does not reach tool handlers.

A server is built for each request, and the SDK asks for it twice (for the protocol versions it
supports, then to serve): about 0.1 ms and 335 KB each with the schemas cached
(`SchemaCache`), measured with 8 tools, against milliseconds of tmux for a tool call and seconds
for a model to answer. Rejected: a server kept per session, a map for no noticeable gain.

Sessions are only kept apart by ratd: a command typed in a terminal reaches every session of its
tmux server (see the limit of the isolation model in `AGENTS.md`).

## Authentication

Mutual TLS, always, loopback included: ratd hands out a user shell, and a loopback address is
reachable by every user of the machine. There is no plain HTTP mode. Clients present a
certificate from the bundle of the tenant (package `mtls`: a closed bundle, generated with
`rat-tool`), and ratd presents the server certificate of that bundle. The client name, which is
its session, is logged with each call along with the address it connects from, telling which client
did what, and from where (see Logs).

Rejected: a bearer token. It is a secret stored next to the terminals, in a file or an
environment that agents, running as the same user, can read; with mTLS, nothing on the server
lets a client in (see package `mtls`).

The SDK DNS rebinding protection (on by default) is turned off. It refuses a request reaching a
loopback address with another host name than `localhost` or a loopback address: a client
through an ssh tunnel under an alias, or on the machine itself under its name (Debian maps it to
127.0.1.1). It protects nothing here: a browser tricked into calling ratd can not present a
client certificate, and the TLS handshake refuses it before any request exists. Users get the
constraints of mTLS: they keep the freedom of their host names.

## Tools

Nine tools, each serving a purpose; composing the controller primitives to serve it is what
ratd adds. Descriptions are short, as small models read them once and forget the details: what
an agent needs to interpret a result goes in the result itself, only when it applies. Results
are readable text, with a header line between brackets when there is something to say, rather
than JSON.

Sending never waits for a command: it returns once tmux holds the input (47 ms for a 4 MiB paste,
measured with tmux 3.7c), and reading returns what is displayed at that moment. This is what makes
rat fit long running commands: an agent starts one, carries on, and comes back to check it. Waiting
is a tool of its own, `wait_window`, which the agent calls when it has nothing else to do: a
server can not wake an agent up, and polling the screen would cost a screen of context each time.

Windows are how commands run in parallel: each long running command in the foreground of a
window of its own, which rat follows (see the principles in `AGENTS.md`). A single `list_windows`
tells which commands still run, `wait_window` waits for one, and its screen shows its progress.
Agents run long commands in the background all the same (`&`, `nohup`), a habit of shell tools
blocking until a command ends, then write scripts checking the jobs and their logs, which windows
spare them. rat does not follow a background job: it looks finished at once, with the exit status
of starting it (0), and it outlives `close_window` when it ignores SIGHUP (`nohup`), until the
service stops. Rejected: telling, with the status of a command, how many jobs bash runs in the
background (it counts them at each prompt), which would warn an agent that started one: it would
present the background as a way to run commands, which windows are for.

Windows persist until closed, which lets agents find them back, and few agents close them once
done: they pile up, `list_windows` telling each of them at every call, each keeping a bash and up
to 10000 rows of history.

The instructions point to a window of its own for each long running command, with what the agent
gains (a single `list_windows` telling which commands still run, no script), and invite to close
the windows an agent is done with. The descriptions of the tools where the agent acts repeat what
applies to them: `send_text`, where it types the `&`, the window and what it gains;
`create_window`, a window per long running command, closed once done. The specification leaves the
instructions to clients, which may not pass them on to the model, and a harness disclosing tools
progressively may show those a search matched without them, while a tool always comes with its
description. The description of `wait_window` tells that a background job is seen as finished at
once.

### Terminals

- **`list_windows`**: for each window, its name, foreground command, working directory and last
  activity, plus when they apply: how its last command exited, a full-screen program, the
  scrollback size. Activity is relative ("12s ago"): models do not know the current time. The exit
  status is told once bash is back at its prompt after the last input, with when ("last command
  exited with status 1 (12s ago)"): the time tells a status from an earlier command, the absence
  of a status a command running (see Prompts in `tmux/README.md`). The description tells it is
  the cheap way to check whether a command finished: an exit status shows it did, with its caveat
  (ssh shows as ssh even when idle, and what runs within it shows no status). Where the terminals
  do not record statuses right (see Startup), neither the windows nor the description tell them:
  the description tells instead that bash in the foreground means the terminal waits for input,
  with its caveats (bash scripts, builtins and loops show as bash, see Prompts in
  `tmux/README.md`). When it creates the session (see Plumbing), a header says so: the agent
  learns its terminals did not exist, rather than finding a lone `main`.
- **`create_window`** `{name}`: makes sure the session exists, then the window. Asking for
  `main` in a missing session is a success: creating the session made it. An existing window is
  not an error but a message saying it was not created, with what it runs and where: the agent
  must not believe it got a fresh terminal. The description tells a window is one per long running
  command, invites to list windows first (they persist, `main` exists) and to close those the
  agent is done with (see above), and to wait for the prompt of a new window before sending text:
  with `wait_window`, or `read_window` where it is not offered, as the result of an input sent to
  a session just created does.
- **`close_window`** `{name}`: terminates what runs in the window. Nothing to close (missing
  window or session) is a message, not an error, and creates nothing. Closing the last window
  closes the session: the result says a fresh `main` comes next, at the cost of a tmux command
  checking it, sparing the agent the surprise.
- **`send_text`** `{window, text, enter}`: pastes the text as a human pastes, then presses Enter
  if asked. `enter` is required, with no default: the agent decides every time whether the text
  runs, and forgetting it fails validation instead of silently leaving a command unrun. Rejected
  defaults: false (small models forget it), true (runs text meant to wait, against "Enter only
  when asked"). The description tells it returns at once, then what the text meets, the case to
  aim for first: at a bash prompt it waits on the command line, new lines included, and nothing
  runs until Enter. Otherwise (a command running, a new window before its prompt), it is read as
  typed, a new line as Enter: the running program gets it first (a `read` takes the first line),
  then bash runs the rest line by line once back at its prompt. Then the rule this calls for:
  before pasting several lines, wait for the prompt (`wait_window`, `read_window` where it is not
  offered). Rejected: the exception told as "each line runs", right before "returns at once":
  inexact (a program reading its input takes lines, a last line without a new line waits on the
  command line), and a model summarizing the description merged the two, telling its user that
  pasted text runs line by line. Where the terminals do not keep bracketed paste (see Startup), the
  description tells instead what the instructions do: multi-line text may run line by line even at
  a prompt, send one line at a time. It points to a window of its own for each long running command,
  rather than the background, telling how the tools then follow it (see above): `wait_window`
  where the terminals record prompts, `list_windows` and `read_window`.
  An empty text only presses Enter; an empty text without Enter sends nothing, a message rather
  than an error. A text holding a control character, tab and new lines aside, is refused, and
  nothing is sent (see Control characters are refused in `tmux/README.md`): the error names the
  first one and its line, and points to `send_keys` for keys and `write_file` for content, the two
  things the agent may have meant. The `text` field tells the rule up front, the description
  leaves it out: agents rarely send control characters, and the error teaches the one who does.
- **`send_keys`** `{window, keys}`: presses keys in order. The description lists the names the
  controller accepts, one per key (aliases are accepted, not shown): a printable character,
  `Space`, `Enter`, `Tab`, `BTab`, `BSpace`, `Escape`, `Up`, `Down`, `Left`, `Right`, `Home`,
  `End`, `PageUp`, `PageDown`, `Insert`, `Delete`, `F1` to `F12`, with the `C-`, `M-` and `S-`
  modifiers, combinable. An unknown name is refused, naming it (`tmux.CheckKey`), with a pointer
  to `send_text`, and nothing is sent; so is an empty list of keys. The result repeats the keys
  pressed, which the logs never hold.
- **`read_window`** `{window, scrollback_rows}`: the screen (200×24), preceded by up to
  `scrollback_rows` rows of history (0 by default). A header tells the rows of history it holds
  when some were asked, a full-screen program, with its cursor position unless the program hides
  it, and a cut to the read budget. Fewer rows than asked, with no other header, mean the history
  holds no more: models infer it, the header does not spell it out. Under a full-screen program,
  the history belongs to the terminal before the program started: it is never included, and the
  header says so when history was asked. `scrollback_rows` is unsigned, so that the schema
  refuses a negative number.

- **`wait_window`** `{window, max_seconds}`: waits until the command sent last to the window
  finished, then tells how it exited (`build: the command finished 3s ago, exit status 1.`), or,
  after `max_seconds` (20 by default, from 1 to 50, bounded by the schema), what still runs, and
  since when (`build: still running, 3m since your last input, make in the foreground.`). Finished
  means the window recorded a prompt since the last input, with bash in the foreground. A text
  sent while a command ran is waited for: bash records no prompt while a line waits to run. bash
  4.4 and 5.0 record a prompt after each line of a text run at once, while the next ones run: the
  foreground command tells, unless bash runs them (see Prompts in `tmux/README.md`). It asks tmux
  every quarter of a second rather than being told: ratd keeps no state, and faster would only
  load tmux, the refresh of terminal UIs (about 100 ms) being for human eyes. Answers are one
  line, never the screen: waiting 50 seconds at a time for a twenty minute build costs 24 lines,
  where reading the screen each time would cost 24 screens. The time told is the one since the
  last input, which tmux records with each input, as is the time without output: both from the
  clock of the machine, rather than the time this call waited. An agent adding up its waits
  miscounts them: one waited on a window three or four times at once (seen in ratd's logs), each
  wait telling 50 seconds when all of them took 50, and found the time without output running
  slow. Without an input recorded (none since the window was created), the time told is the one
  waited. The description tells how to wait longer, which those waits at once were for: call it
  again once it returns, several calls at once on a window waiting together, no longer than one.
  Two hints, worded calmly, as an agent told its command looks stuck interrupts it: when nothing
  was displayed for ten seconds, that it is normal for some commands, and that `read_window` shows
  whether it waits for input; when bash is in the foreground with no prompt since the last input,
  that a bash script or builtin may still be running, or a text wait on the command line (sent
  without Enter), `read_window` telling which: a script shows as bash. Not seen as finished, which
  the description tells: a program waiting for input, and what runs within ssh or an interpreter,
  which show no prompt of rat's bash. Seen as finished at once, which it tells too: a command run
  in the background, bash showing its prompt as soon as it started it. The exit status is told
  where the terminals record it right (see Startup). Where they record no prompt, the tool is
  offered all the same, its description and its answer telling why it can not wait, and what to
  use instead: the same tools everywhere, and an agent told why rather than left wondering where a
  tool went. A missing session is created, as by the other tools: waiting on `main` then waits for
  its first prompt. It returns at once when ratd stops (see Shutdown).

  50 seconds at most: a harness cuts a tool call at a limit of its own, which ratd can not see,
  nor push back without a stream to send progress on. That limit is 60 seconds by default for
  several harnesses (Cline, Zed, OpenCode, Continue, the Cursor CLI, and Claude Code reaching ratd
  over HTTP, until the first byte of the answer), configurable in most, and longer for others
  (Codex 300 s, Gemini CLI 10 min, Claude Code through rat), checked in their code in 2026-10. A
  wait of 60 seconds would race the first ones, its answer coming after the cut, the agent then
  getting an error from its harness rather than an answer: 50 leaves room for the network and for
  ratd to answer. Rejected: a wait without parameter, which an agent can not shorten to check on a
  command it expects to ask something; a longer maximum, which more harnesses would cut.

The inputs of the tools (names, types, JSON schemas) are declared in package `cmd/ratd/tools`,
which rat imports: from protocol 2026-07-28 on, an argument whose schema carries an
`x-mcp-header` annotation travels in an HTTP header too (SEP-2243), which rat computes from the
same schemas. A rat and a ratd of the same version agree on them, and rat never learns the tools
from ratd. ratd builds the schemas once, at startup, and serves the same ones to every request.

Text and keys are two tools rather than one `send_input` taking either: JSON Schema can not say
"exactly one of", so a small model would learn it by failing, where two tools each have a schema
the SDK validates. One call stays one act either way: which keys to press usually depends on
what the text caused on the screen.

Rows, not lines: on the normal screen, captures join the lines tmux wrapped (`-J`), so a result
holds whole lines while its history is counted in terminal rows. A long line wrapped over several
rows counts for each.

The cursor is only told under a full-screen program, whose screen is captured as displayed,
without joining rows: a full-screen program lays out its own screen to the width, and its cursor
tells where input goes (a field, a position in a file). Row 1 of the content is then row 1 of
the screen, and the cursor exact. On the normal screen, joined rows and trimmed empty ones would
make screen coordinates point at the wrong line, for a cursor that tells little there: it sits
at the end of the prompt, the last line. Rejected: mapping the cursor to joined lines by
capturing twice, heavier, and wrong when output arrives in between. A program hiding its cursor
(`htop`) gets no position, only a header saying so: a human sees no cursor either.

Agents never see tmux modes. A human peeking at the terminals can leave a window in one (copy
mode, to scroll back), where input would not reach the program as sent: the controller leaves
any mode before an input, and a capture shows the program whatever the mode (see
`tmux/README.md`, which tells why modes are not shown to agents).

### Files

ratd reads and writes files itself, on its own machine and as its own user (see the principles
in `AGENTS.md`): tmux, its terminal backend, would only add a round trip, and replace precise
errors with its messages.

- **`write_file`** `{path, content}`: creates the file or replaces it, and says which (with the
  former size). A new file gets 0644 minus ratd's umask. Missing directories are created, as
  `mkdir -p` does (0755 minus ratd's umask), sparing a round trip through the terminal, and the
  result names them: a typo in a path creates directories the agent did not mean. The content is
  text (a JSON string), written as is: binary content goes through the terminal (`base64 -d`).
- **`read_file`** `{path, start_line, max_lines, line_numbers}`: whole lines of a text file, from
  `start_line` (1 by default, negative counts from the end: -100 for the last 100 lines), at most
  `max_lines` (all by default). The header tells the lines returned out of how many, the size and
  the last modification ("modified 3s ago": a command may still be writing). The content is
  exact, a final new line included, so that it can go back through `write_file`; `line_numbers`
  prefixes each line with its number and a tab, as `cat -n` does, for an agent to refer to lines.
  Not by default: about 7 bytes a line, and a content to strip before writing it back.

A file is written in place, as `cp` does: truncated, then written. It keeps its mode, owner,
hard links and extended attributes, and a symbolic link is followed, writing its target, as a
human editing it would. The cost: a write failing midway (no space left) leaves the file partial,
which the error says, the agent still holding the content to write again; and two writes of the
same file at once can mix. Rejected: writing a temporary file renamed over the target, atomic,
but breaking hard links, turning a symbolic link into a file, possibly changing the owner, and
needing to write in the directory.

Only a regular file is written: a FIFO would block, and a device is not a file to replace. The
file is checked before opening (opening some devices has effects), opened without blocking (a
FIFO without a reader then fails at once), and checked again once opened, as the file opened may
not be the one checked. Whether it is created or replaced is told by opening: exclusively first,
then the existing file.

Errors of the file system are told as the system gives them ("no space left on device",
"read-only file system"), unlike tmux messages: an agent expects a low level operation to fail
this way, and can act upon it. A denied permission names the user ratd runs as.

Paths are absolute or start with `~/`. Rejected: paths relative to a window, whose directory is
that of its foreground process: under ssh, the local ssh client's, so the file would silently
land elsewhere. `list_windows` shows the directories, for the agent to build absolute paths.

What `read_file` sends is checked first, as it can not be taken back from a model's context:

1. only a regular file is read: a FIFO would block forever, `/dev/zero` never ends. It is checked
   and opened as `write_file` does, without blocking;
2. the read is bounded whatever the size tells: a log grows while being read, and is read up to
   its size when opened, consistently with the size told; `/proc` files show 0, and are read to
   their end within the time of the call;
3. the content must be text: no NUL byte (the heuristic of git and `grep -I`) and valid UTF-8,
   the line told. Otherwise the tool refuses, pointing to the terminal (`file`, `xxd`, `base64`).
   What is checked is what is sent; a file over the budget read without a range sends nothing,
   but its start is checked all the same (as git checks the start of a file), rather than telling
   a line count for a range to be refused next. Rejected: running `file`, not always installed,
   with an output to parse, and saying nothing of size;
4. the result fits the read budget. Without a range, a file over the budget returns only its
   size, line count and modification time, for the agent to pick a range: nothing big lands in
   its context by surprise. With a range, a read returns the whole lines that fit and tells where
   to continue, as `read(2)` returns fewer bytes than asked. A single line over the budget (a
   minified file) can not be cut: the tool points to the terminal (`head -c`, `cut`).

Lines rather than bytes: agents think in lines, and a byte offset cuts lines and characters. The
cost is that reaching line 30000 reads what comes before, having no index: fast enough for
ordinary files. Reading from the end reads backwards, instant on any size.

Scanning (reaching a line, counting lines) takes half of the time of the call at most. A bound in
time rather than in size, as the speed depends on where the file is: 1 GiB is scanned in 80 ms
from the page cache (checks for text included), around 7 s from a hard disk. A line out of reach
in time is refused, pointing to a negative `start_line` or the terminal (`sed -n`, `tail -n`),
and a total not counted in time is not told: the file is "too large to count its lines", and a
range from the end is then numbered from the end (`lines -100 to -41 counted from the end`). The
same file may be counted on a warm cache and not on a cold one.

Every range is read forward and cut at the end, telling the line to continue with: from the end
too, as the continuation keeps the numbering of the header. `start_line` 0 is no start, the
schema not telling 0 from a missing number: a range is a `start_line` or a `max_lines` given, so
that `start_line` 1 reads a large file from its start, one cut at a time.

Not provided: a tool telling the metadata of files (type, size, times). `ls -l` or `stat` in a
terminal tells it, at the cost of a screen for a line, and `read_file` tells the size, line count
and modification of a file over the budget. Worth a tool if agents turn out to spend many calls
on it; the logs of ratd can not tell, as they never hold what is typed in a terminal: the
transcripts of harnesses can.

### Read budget

A result of `read_window` or `read_file` holds at most the read budget, 64 KiB by default (about
16k tokens), set by `--read-budget`, header included. Harnesses also cut or divert large
results, and a larger context still degrades as it fills.

`read_window` cuts the history, keeping its end: the last lines whole, then the end of the line
before them, cut at a character boundary. Rather than whole lines only: a long output with no new
line is a single line once joined, spanning the history and the screen (a line of 100000
characters), and dropping it would take the screen away. Once cut, lines could be counted, not
rows: the header tells no number.

The screen itself is never cut: a cut screen would be read as the whole screen. A screen usually
takes about 5 KB, 20 KB with multibyte characters, and ratd refuses a budget under 32 KiB. But
characters piling up combining accents take more: a screen of them measured 78 KB. A screen over
the budget is refused, telling its size, and the agent is told to close the window and start
over in a new one: sent whole, it would flood a context the agent could not recover from. The
controller tells the size of the screen alone (see `tmux/README.md`), which the joined content
can not.

It is a setting of ratd, not a parameter of the tools: the admin knows which models and harnesses
connect, and long reads stay possible, one range at a time. The descriptions state the value.
Rejected: an agent parameter under a ceiling set on ratd (small models set it large "to be
safe", and it does nothing while the ceiling is the default), a constant (a rebuild for a
number).

### Annotations

| Tool | Read only | Destructive | Idempotent | Open world |
|---|---|---|---|---|
| `list_windows`, `read_window`, `wait_window`, `read_file` | yes | | | |
| `create_window` | | | yes (an existing window is reported, not recreated) | |
| `close_window` | | yes | yes | |
| `send_text`, `send_keys` | | yes | | yes |
| `write_file` | | yes (it replaces) | yes | |

Every hint is set explicitly: the specification reads a missing destructive or open world hint
as true. Only the inputs reach beyond rat: what runs in a terminal can reach anything, where the
other tools act on rat's terminals and on files of its machine.

### Plumbing

Hidden from agents: a tool needing a missing tmux session creates it (a concurrent creation is
not an error), so an agent closing all its windows finds a fresh `main`. What each tool does then
is part of its purpose:

- `list_windows` and `read_window` create it, and show the fresh `main` with a header saying so.
  `read_window` naming another window fails: it does not exist, the error says how to create it.
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
| reading another window than `main` in a session just created | yes | `main` was just created, `create_window` for the other |
| screen over the read budget | yes | its size, close the window and start over in a new one |
| invalid window name | yes | the naming rule |
| unknown key | yes | the key names, and `send_text` to type text |
| control character in a text | yes | the character and its line, `send_keys` for keys, `write_file` for content |
| tmux restarting | yes | every window was lost, retry in a few seconds to find a fresh `main` |
| tmux not answering in time | yes | logged, retrying may work |
| file refused (not regular, not text, missing, denied, relative path, a range too far into a huge file to reach in time) | yes | why, and the terminal when it can do better |
| file system failing (no space left, read-only…) | yes | the reason of the system, and whether the file may be partial |
| file system not answering in time | yes | logged, the file may still be written: check it before writing again |
| anything else | yes | internal error, logged, retrying may work |
| nothing to close | no | nothing to close |
| window already existing | no | not created, what it runs and where |
| file over the budget, without a range | no | size, line count, modification time, how to ask a range |

## MCP instructions

Built at startup, sent to each client when it initializes, or discovers the server from protocol
2026-07-28 on:

> rat gives you persistent terminals on *host*: commands run there, not where you run. It is
> asynchronous by design: sending a command returns at once, without waiting for it to finish.
> Start long running commands (builds, tests, deployments), carry on with other work, and come
> back to check them: wait_window waits until a command finished, 50 seconds at most per call, and
> tells how it exited; list_windows shows the foreground command of each window, and how its last
> command exited once bash is back at its prompt; read_window shows the screen. Run several
> commands in parallel in several windows, each long running command in the foreground of a
> window of its own (create_window): a single list_windows then tells which still run, and reading
> a window shows its progress, with no need for a script to check jobs or their logs. A command
> run in the background (&, nohup) looks finished at once: rat follows the foreground command of a
> window. Each terminal is a window running bash, found by name: windows persist across your
> restarts and context compactions, so call list_windows first, and close the windows you are done
> with (close_window). A window named main exists when you start. To copy a whole file to or from
> the machine, use write_file and read_file rather than the terminal. For the exact output of a
> long command, run it as command 2>&1 | tee /tmp/name.log: the screen still shows it, and
> read_file reads the file whole or a range at a time, rather than reading the window over and
> over.
> If the terminals restart after a failure, every window disappears and you find a fresh main.

How to check a command depends on what holds in the terminals (see Startup): where they do not
record statuses right, the instructions tell of no status; where they record no prompt, that rat
can not tell when a command finishes there, wait_window telling why. Both tell that bash in the
foreground of list_windows means the terminal waits for input, unless it runs a bash script or
builtin. The host name tells an agent that has a local shell too where commands run. The tenant
and the session are left out, agents having no use for them; the server info title carries the
tenant and the host (`rat prod on host`), for clients to display. When the check of the
terminals finds bracketed paste defeated, or can not run (see below), a sentence is added:
multi-line text may run line by line even at a prompt, send one line at a time. The description
of `send_text` tells it too, in place of what text meets at a prompt.

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
3. **Check the terminals**, once (`tmux.Controller.CheckTerminals`, 10 seconds at most): what bash
   startup files defeat at the prompt of terminals, bracketed paste, and the prompts recorded with
   the status of commands. Terminals keep working either way (single line input is fine), so an
   override, or a check that can not run, is a warning: logged, and for bracketed paste told to
   agents, in the MCP instructions and the description of `send_text`. A check that can not run is
   taken as nothing holding. A startup file blocking also blocks every terminal: the log says so.
   Checked once rather than at each tmux restart: clients receive the instructions and the tools
   once, and must not be told something the server no longer believes. A startup file changed while ratd runs is caught at its next start.
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
the terminals are restarting. A call waiting (`wait_window`) returns at once, telling ratd stops:
`http.Server` cancels no call when shutting down, it waits for them, and a wait would hold the
door open for the whole grace, then be cut. The kill switch does not depend on what agents wait
for.

The controller terminates its server when the context it was started with ends: ratd starts it
with a context the signal does not cancel, which would otherwise stop the terminals with the
door rather than after it. The supervisor is stopped before the server, so that it does not
restart what ratd stops.

Stopped as a service, ratd is not the only one signaled: systemd sends SIGTERM to every process
of the service at once, tmux included (see the kill switch in `AGENTS.md`). The terminals then
stop with the door, and `StopServer` reports a server that exited on its own, which ratd does not
warn about.

### Timeouts

- **A tool call: 10 seconds**, plus its wait for `wait_window`. No other call waits for a command,
  and tmux answers in milliseconds even for the largest inputs (47 ms for a 4 MiB paste, 11 ms for a capture of 10000 rows): beyond,
  tmux is stuck, and the agent is told so. File operations can not be interrupted: on a file
  system not answering (a stale NFS mount, which blocks a human as well), the call returns at the
  end of its time, logged as a warning, while the operation stays blocked until the file system
  answers, one per call.
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

The path and the default port are declared in package `connect`, the defaults ratd and its
clients share: both sides of the contract in one place.
- **TLS**: served through a TLS listener built from the checked bundle. `http.Server.TLSConfig`
  alone would be silently ignored by `ListenAndServe`, serving plain HTTP: a test must guard that
  a client without a certificate is refused. HTTP/1.1 only (no `h2` offered): requests are short
  JSON exchanges.
- **Rejected handshakes** are logged, at most once every 10 seconds with the count of the others:
  a public port gets scanned. The standard library would otherwise print them through the `log`
  package, outside ratd's logs.

## Logs

One line per tool call, at info level, or warning for a failure the agent did not cause (tmux not
answering, an internal error): the session (the client name), the address of the client, the tool,
the window, the outcome (`ok`, `message` for a result that is not an error, `error` with its
cause) and the duration, plus what tells the action without its content: the size of a text and
whether Enter was pressed, the number of keys, the rows of history asked and read (and the size of
a screen refused), the path and size of a file. Never the content: neither text, nor keys (they
can spell a password one key at a time), nor files. And the lifecycle: startup (version, tenant,
address, read budget, bundle expiry), the check of the terminals, tmux exits and restarts, the
crash budget, shutdown.

The address tells apart the agents sharing a client certificate, and shows a certificate used from
an unexpected machine. It is the peer of the connection: behind a proxy, the proxy's, whose own
logs carry the trail on. A header naming the client (`X-Forwarded-For`) is never read: mutual TLS
ends at ratd, so no proxy can be trusted to set it. The address may be personal data (GDPR): it is
logged for security, as a web server logs its clients. ratd keeps nothing itself: how long the
logs are kept and who reads them is the operator's call (journald, log shipping). Not provided: an
option to leave the address out. An operator who must not keep it can filter it out of the logs,
one who needs it could not get it back once left out.

At debug level, one line per MCP request (the session, the address, the method, the duration),
whatever the protocol version: clients initialize, or discover the server from protocol 2026-07-28
on (the Go SDK does). The SDK logs every stateless request at info level (a session connecting,
then disconnecting): only its warnings and errors are kept, all of it at debug level.

## Configuration

```
ratd --bundle DIR [--listen :7281] [--read-budget 64KiB] [--log-level info]
```

`--bundle` is the server directory of a bundle (see `rat-tool`); the tenant comes from its
certificate. `--read-budget` takes a size with its unit (`64KiB`, `1MiB`, `65536B`, parsed by
`github.com/hekmon/cunits`). Decimal units are accepted too, and so are bits (`Kb`): such a
mistake usually falls under 32 KiB, and the refusal tells the size read. No configuration file
nor environment variable: nothing ratd needs is secret on a command line.

## Open questions

- Output redaction (see `AGENTS.md`).
