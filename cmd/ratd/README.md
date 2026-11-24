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
client certificate. It holds no terminal: persistence stays in ratd. A harness able to present
the certificate, and to accept the server without checking its host name (see package `mtls`),
can target ratd directly: the bridge adds nothing to MCP but what needs the machine of the
harness, the file transfers (`upload_file` and `download_file`, with `--transfers-dir`), which a
harness without rat goes without. How it relays is described in the README of `cmd/rat`.

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
supports, then to serve): about 0.25 ms and 400 KB each with the schemas cached (`SchemaCache`),
measured with 9 tools by `BenchmarkNewServer` (`go test -bench NewServer -benchmem`), against
milliseconds of tmux for a tool call and seconds for a model to answer. Rejected: a server kept
per session, a map for no noticeable gain.

Sessions are only kept apart by ratd: a command typed in a terminal reaches every session of its
tmux server (see the limit of the isolation model in `AGENTS.md`).

## Authentication

Mutual TLS, always, loopback included: ratd hands out a user shell, and a loopback address is
reachable by every user of the machine. There is no plain HTTP mode. Clients present a
certificate from the bundle of the tenant (package `mtls`: a closed bundle generated with
`rat-tool`, or issued by a sub CA of another PKI), and ratd presents the server certificate of
that bundle. The client name, which is its session, is logged with each call along with the
address it connects from, telling which client did what, and from where (see Logs).

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
of starting it (0), and it outlives `close_window` once detached from its terminal (`nohup`, see
below), until the service stops. Rejected: telling, with the status of a command, how many jobs
bash runs in the background (it counts them at each prompt), which would warn an agent that started
one: it would present the background as a way to run commands, which windows are for.

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
  scrollback size. Activity is relative ("12s ago"): models do not know the current time. A value
  holding a control character is quoted, as Go and JSON quote a string: a directory named with a
  new line keeps its window on a line, can not mimic another window, and stays exact for the agent
  to give back to a tool. The exit
  status is told once bash is back at its prompt after the last input, with when ("last command
  exited with status 1 (12s ago)"): the time tells a status from an earlier command, the absence
  of a status a command running (see Prompts in `tmux/README.md`). For a pipeline whose commands
  before the last one failed, they follow, by position ("last command exited with status 0,
  command 1 of its pipeline of 2 with 1 (statuses: 1 0) (12s ago)"), as `wait_window` tells them.
  The description tells it is the cheap way to check whether a command finished: an exit status
  shows it did, with its caveat (ssh shows as ssh even when idle, and what runs within it shows no
  status), and how pipelines exit, where the terminals record them. Where the terminals
  do not record statuses right (see Startup), neither the windows nor the description tell them:
  the description tells instead that bash in the foreground means the terminal waits for input,
  with its caveats (bash scripts, builtins and loops show as bash, see Prompts in
  `tmux/README.md`). Where the terminals record prompts, a window whose bash has not run rat's
  hook yet says so ("no prompt recorded yet: bash still starting, or its startup files replace
  PROMPT_COMMAND"): startup files changed since ratd checked them at its start, which the windows
  created afterwards show (see Hooked windows in `tmux/README.md`). When it creates the session
  (see Plumbing), a header says so: the agent learns its terminals did not exist, rather than
  finding a lone `main`.
- **`create_window`** `{name}`: makes sure the session exists, then the window, and returns once
  its bash shows its first prompt. Asking for `main` in a missing session is a success: creating
  the session made it. An existing window is not an error but a message saying it was not created,
  with what it runs and where: the agent must not believe it got a fresh terminal. The description
  tells a window is one per long running command, invites to list windows first (they persist,
  `main` exists) and to close those the agent is done with (see above).

  The wait keeps a text from running by itself: sent while bash starts, it is read as typed, each
  new line running the line before it, whether `enter` is true or not. Sent as soon as the window
  existed, all 240 texts of two lines ran a line on an idle machine (tmux 3.3a, 3.7c and 3.8, bash
  5.2 and 5.3), and none of 480 once the first prompt was recorded, half of them with every CPU
  busy. The prompt takes tens of milliseconds to come. Past 5 seconds, startup files holding bash
  up, the answer tells the window exists, and to wait for its prompt with `wait_window`. A bash
  exiting while it starts is told, its window closing with it. Where the terminals record no prompt
  (see Startup), there is nothing to wait for: the answer tells bash is starting, and the
  description to wait for its prompt with `read_window`. Rejected: a rule in the description telling
  agents to wait, which costs a call when followed, and runs a line when forgotten; the marker of
  the hook (`Window.Hooked`), set before readline asks for bracketed paste, after which 2 of 480
  texts ran a line, under load; `bracket_paste_flag`, which tells it exactly, but which tmux 3.3a
  does not have.
- **`close_window`** `{name}`: terminates what runs in the window. A command detached from it keeps
  running (see Closing a window in `tmux/README.md`), which the description tells: an agent closing
  the window of a server it started with `nohup` would take it for stopped, its port still taken.
  In the description rather than the result, as ratd follows no process and can not tell whether
  one is left. Nothing to close (missing window or session) is a message, not an error, and creates
  nothing. Closing the last window closes the session: the result says a fresh `main` comes next,
  at the cost of a tmux command checking it, sparing the agent the surprise.
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
  than an error. A text holding a control character, tab, new lines and carriage returns aside,
  is refused, and nothing is sent (see Control characters are refused in `tmux/README.md`): the
  error names the first one and its line, and points to `send_keys` for keys and `write_file` for
  content, the two things the agent may have meant. The `text` field tells the rule up front, the
  description leaves it out: agents rarely send control characters, and the error teaches the one
  who does.
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
  in the background, bash showing its prompt as soon as it started it. A window whose bash has
  not run rat's hook, once the wait is over, is told as such rather than as running: still
  starting, or startup files replacing `PROMPT_COMMAND`, changed since ratd checked them, and
  then what the window loses (`wait_window`, the exit statuses, bracketed paste) and the fix, add
  to `PROMPT_COMMAND` rather than replace it. Told whole: the agent may have changed them itself,
  an installer editing `.bashrc`, and can repair them. The exit status is told
  where the terminals record it right (see Startup). Where they record no prompt, the tool is
  offered all the same, its description and its answer telling why it can not wait, and what to
  use instead: the same tools everywhere, and an agent told why rather than left wondering where a
  tool went. A missing session is created, as by the other tools: waiting on `main` then waits for
  its first prompt. It returns at once when ratd stops (see Shutdown).

  A pipeline exits with the status of its last command, as bash tells it: `command 2>&1 | tee
  log`, which ratd recommends for long outputs, with the status of tee, 0 almost always. Told "exit
  status 0", an agent takes a build that failed for a success, and nothing on the screen says
  otherwise. Where the terminals record the statuses of pipelines (see Startup), the commands
  before the last one that failed come first, by position (tmux knows no command names), then the
  status of the pipeline, explained, and every status: `build: the command finished 3s ago:
  command 1 of its pipeline of 2 exited with status 1. The pipeline exits with the status of its
  last command, 0, as bash reports it (statuses, left to right: 1 0).` Failures first: a model
  reading "exit status 0" first anchors on it. No verdict ("failed"): a status other than 0 is
  not always a failure (`grep` matching nothing exits with 1). 141 is told as usually normal
  (SIGPIPE: `yes | head`, the reader stopped early), the one status misleading there. Nothing is
  added for a single command, nor for a pipeline whose last command alone failed, whose status
  tells it. The descriptions of `wait_window` and `list_windows` say it in a sentence, before the
  agent meets two statuses for one command. Rejected: the first status other than 0 as the status
  of the pipeline (what `pipefail` does), which tells `yes | head` failed, and differs from bash;
  `set -o pipefail` in the terminals, which changes what commands do, for the same reason; the
  pipeline told whenever it holds several commands, tokens saying nothing for every `ls | grep`.

  A command stopped (`C-z`, which an agent may press for `C-c`) is no more finished than one run
  in the background, but bash shows its prompt all the same, with the status 128 plus the signal
  stopping it (148 for SIGTSTP on Linux, 146 on macOS), and no pipeline. Told "finished, exit
  status 148", an agent takes its command for failed, and leaves it stopped. Where the terminals
  record statuses, these four (SIGTSTP, SIGSTOP, SIGTTIN, SIGTTOU) are told as a command stopped,
  a job of that bash, with how to resume it or end it: `build: the command was stopped 3s ago,
  status 148 (SIGTSTP, as C-z does): it waits as a job of this bash, fg resumes it, kill %% ends
  it.`, and `list_windows` the same in short. `%%` is the current job of bash, the one stopped last.
  A command exiting with such a status by itself reads as stopped, as in bash.

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
  result names them, a failure leaving them included, a call ending before its write aside (the
  time limit): a typo in a path creates directories the agent did not mean. The content is text (a JSON string), written as is. The description tells
  no way around for binary content: an agent that really needs one encodes it as text by itself,
  and pointing to a way (`base64 -d` in the terminal) would invite it into its context, a third
  larger and meaningless to the model. A path naming a directory by its form (a final slash, or `.`
  or `..` as its last part) is refused, rather than cleaned into the name of a file.
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
which the error says, the agent still holding the content to write again; and a write from a
terminal or another program at the same time can mix with it, two writes through ratd going one
at a time (see G7 in File transfers). A program reading the file during a write may see it
partial, as with `cp`: an agent for which it matters writes another file and renames it in the
terminal. Rejected:
writing a temporary file renamed over the target, atomic, but breaking hard links, turning a
symbolic link into a file, possibly changing the owner, and needing to write in the directory;
an `atomic` parameter, which small models would set "to be safe", a second way to write to test;
renaming for new files only, which have nothing to keep, a second way as well, for programs
watching new files only. File transfers make the other choice (see File transfers): a file
arriving for minutes must not be left half replaced.

Only a regular file is written: a FIFO would block, and a device is not a file to replace. The
file is checked before opening (opening some devices has effects), opened without blocking (a
FIFO without a reader then fails at once), and checked again once opened, as the file opened may
not be the one checked. Whether it is created or replaced is told by opening: exclusively first,
then the existing file, then creating again, for a symbolic link whose target is missing, which
the first open refuses and the second finds no file behind. The former size is that of the file
once no other write through ratd holds it (see G7 in File transfers), and a file is told created
only if found empty then: of two writes creating one file at once, the second to hold it is told
it replaced what the first wrote, if it wrote anything. Two calls at once through one symbolic
link whose target is missing, the first writing no byte, are both told created: neither knows
which created the target, and nothing was emptied. Rejected: resolving the link to create its
target exclusively, redoing what the system does of a link, for a word no agent acts on.

Errors of the file system are told as the system gives them ("no space left on device",
"read-only file system"), unlike tmux messages: an agent expects a low level operation to fail
this way, and can act upon it. A denied permission names the user ratd runs as.

Paths are absolute or start with `~/`. Rejected: paths relative to a window, whose directory is
that of its foreground process: under ssh, the local ssh client's, so the file would silently
land elsewhere. `list_windows` shows the directories, for the agent to build absolute paths.

The content of `write_file` is generated by the model, and stays in its context: a file copied
from the filesystem of the agent is in it twice, read then sent. ratd can not read the files of a
client, MCP giving a server no way to. The descriptions of `write_file` and `read_file` therefore
tell what they do with the context of the agent ("from your context", "into your context"), and
point a file the agent only moves to a copy by path: "a tool copying files by path if you have
one", which rat offers with its file transfers (see File transfers), and which a harness
connected directly goes without, ratd naming no tool it does not serve. Otherwise, the
description of `write_file` points to a subagent reading and writing the file, if the agent can
start one: the file then fills the context of the subagent, discarded once done, rather than the
agent's. It gives both steps to the subagent: content handed over in its prompt is generated by
the agent, costing as much as sending it, and a file the agent read first stays in its context.
Conditional, and naming no model, as ratd can not see the harness; for copies only, content the
agent writes itself having to go through that prompt; whatever the size, as copies add up (a file
sent again at each round of a loop of edits and tests), and a subagent copying a small file only
costs time and tokens of its own. The instructions carry the copy hint as well, in one sentence:
a harness disclosing tools progressively reads them first, and some drop them.

rat, offering its file transfers, puts sentences naming `upload_file` and `download_file` in
place of these generic hints. Both sides are declared in package `cmd/ratd/tools`, a test of ratd
checks its texts hold the generic ones, and a rat and a ratd of different versions, whose
sentences differ, leave the generic text, still true.

Rejected: large files only, judged against a context the agent does not measure (512 KiB may not
look large, yet fills a context of 128k tokens, at about 4 bytes a token); a file "from where you
run", the words of the instructions, which the description may come without, and which may read
as a directory; "send it", the verb of the terminal tools (`send_text`), which may point to pasting
the file in a terminal; the result of a write, which comes once the content is sent, and can not
tell a copy from content the agent wrote; a subagent pointed to from `read_file`, which agents
mostly call to read the content themselves; naming the tools of rat in ratd ("use upload_file if
you have it"), tools a harness connected directly never has; rat rewriting the texts of ratd
freely, which a sentence changed between versions would silently miss.

What `read_file` sends is checked first, as it can not be taken back from a model's context:

1. only a regular file is read: a FIFO would block forever, `/dev/zero` never ends. It is checked
   and opened as `write_file` does, without blocking;
2. the read is bounded whatever the size tells: a log grows while being read, and is read up to
   its size when opened, consistently with the size told; `/proc` files show 0, and are read to
   their end within the time of the call. A range of one is read from a copy of it, read whole,
   once, from its start, up to 64 MiB (`/proc/kallsyms` takes 10 to 20 MB, some never end): such a
   file is generated again at each read at an offset, its counters changing its length, and a
   range found by offsets came cut (an offset of `/proc/self/status` landed mid-line in the next
   read 354 times out of 2000, measured on Linux 6.6). A whole read takes one read, and is left
   as is;
3. the content must be text: no NUL byte (the heuristic of git and `grep -I`) and valid UTF-8,
   the line told. Otherwise the tool refuses, pointing to the terminal (`file`, `xxd`, `base64`).
   What is checked is what is sent; a file over the budget read without a range sends nothing,
   but its start is checked all the same (as git checks the start of a file), rather than telling
   a line count for a range to be refused next. Rejected: running `file`, not always installed,
   with an output to parse, and saying nothing of size;
4. the result fits the read budget. Without a range, a file over the budget returns only its
   size, line count and modification time, for the agent to pick a range: nothing big lands in
   its context by surprise. A file within the budget whose result is not, with its line numbers
   (about 7 bytes a line) or its header, is told so, and how to read it: told over the budget, its
   size under the budget would contradict it. With a range, a read returns the whole lines that
   fit and tells where to continue, as `read(2)` returns fewer bytes than asked. A single line over
   the budget (a minified file) can not be cut: the tool points to the terminal (`head -c`, `cut`).

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
takes about 5 KB, and at most 18.8 KiB, its 200×24 cells each holding the longest UTF-8
character (4 bytes), wide characters taking 2 cells for as much. ratd refuses a budget under 20
KiB (about 5k tokens): that screen, its line breaks and its longest header take 19349 bytes, and
about 1 KiB is left for a header to change (`TestMinReadBudget` fails if a header or the size of
the terminals outgrows it). Rejected: a larger margin, which would only let a screen full of
4-byte characters carry some combining accents as well, at the cost of tokens for the small
models a low budget is for. Characters piling up combining accents take more a cell (tmux holds
21 bytes a cell in 3.3a, 32 in its current source, up to 150 KiB a screen, beyond the default
budget): a screen of them measured 78 KB. A screen over the budget is refused, telling its size,
and the agent is told to close the window and start over in a new one: sent whole, it would
flood a context the agent could not recover from. The controller tells the size of the screen
alone (see `tmux/README.md`), which the joined content can not.

It is a setting of ratd, not a parameter of the tools: the admin knows which models and harnesses
connect, and long reads stay possible, one range at a time. The descriptions state the value.
Rejected: an agent parameter under a ceiling set on ratd (small models set it large "to be
safe", and it does nothing while the ceiling is the default), a constant (a rebuild for a
number), a floor of a screen of ASCII (8 KiB): box-drawing characters (full-screen programs) and
CJK take 3 bytes, a screen of them would be refused, its window unusable.

The budget bounds a single read, not what reads usually take: `read_window` returns the screen
alone unless asked for history (`scrollback_rows`, as many rows as the agent asks), about 1 to 2k
tokens, and `read_file` over the budget without a range returns what the file is, not its
content. For models whose context degrades early (local models), `--read-budget 20KiB` bounds
the largest read to about 5k tokens.

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
| bash exiting while a new window starts | yes | the window closed, its startup files may end it |
| invalid window name | yes | the naming rule |
| unknown key | yes | the key names, and `send_text` to type text |
| control character in a text | yes | the character and its line, `send_keys` for keys, `write_file` for content |
| tmux restarting | yes | every window was lost, retry in a few seconds to find a fresh `main` |
| tmux not answering in time | yes | logged, retrying may work |
| file refused (not regular, not text, missing, denied, relative path, naming a directory, a range too far into a huge file to reach in time) | yes | why, and the terminal when it can do better |
| file system failing (no space left, read-only…) | yes | the reason of the system, and whether the file may be partial |
| file system not answering in time, or a write waiting past its call for another into the file | yes | logged, the file may still be written: check it before writing again |
| anything else | yes | internal error, logged, retrying may work |
| nothing to close | no | nothing to close |
| window already existing | no | not created, what it runs and where |
| new window held up by its startup files | no | created, no prompt yet, `wait_window` to wait for it |
| file over the budget, without a range | no | size, line count, modification time, how to ask a range |

## File transfers

`write_file` and `read_file` move content through the context of a model: what it writes, what
it must read. A file an agent only moves (a binary, an archive, a build output) has no place
there: binary content does not fit a JSON string, and a large file fills the context for nothing.
MCP gives a server no way to reach the files of its client, so a copy by path takes a program on
each machine: ratd here, and rat on the machine of the harness, for its tools `upload_file` and
`download_file`. Their bytes travel over two endpoints of ratd next to `/mcp`, behind the same
client checks, never through MCP: `/mcp` stays MCP only. The contract is declared in package
`connect`, with both sides of it:

- **`POST /upload`**: the body is the file, written at the path `Rat-Path` names. `Rat-Mode`
  holds its permission bits, `Rat-Overwrite` whether an existing file may be replaced (false when
  absent, the safe side), `Rat-In-Place` whether a file replaced is written into rather than
  replaced by a new one (false when absent: see In place). The answer is 201 (created) or 204
  (replaced), with the permission bits of the file written in `Rat-Mode`, for rat to tell an agent
  that a replaced file kept a mode other than its source's: those ratd set, rather than those a
  look at the path finds once the file is placed, a file moved at once (a watcher of a spool
  directory) leaving the answer without them. A 201 names the directories created, as `write_file`
  does, and so does a failure leaving some, creating them included: the answer to an upload cut or
  canceled, nobody reads. A 204, which carries no body, tells in `Rat-Note` the setuid or setgid
  bit the file replaced showed and the file written lost (see L2), encoded as `Rat-Error` is, and
  in `Rat-In-Place` that the file was written into: asked in place, a file appearing at a path
  found empty is replaced by a new one, which rat would otherwise tell written in place.
- **`GET /download`**: the body of the answer is the file at the path `Rat-Path` names, with its
  permission bits in `Rat-Mode` and its size in `Rat-Size`. HEAD answers the headers alone.
- The answers of both endpoints carry `Rat-Version`, the version of ratd: a 404 without it comes
  from a ratd older than the transfers, whose mux serves `/mcp` alone, one with it from a missing
  file. A refusal is a status (400, 403, 404, 409, 412, 415, 507, or 500 for a failure of the
  system without a status of its own, an I/O error) with a sentence for the agent, which rat
  relays, as it relays the 503 of every request while ratd has no valid CRL.

HTTP as designed: the URL names the endpoint, headers describe the file, the body is the file.
Rejected: the path as a query parameter (`?path=`), a function call dressed as HTTP; the path in
the URL (`PUT /files/home/agent/app.tgz`), which routing rewrites: Go's mux redirects `..` and
`//` (307), so that `/files//~/x` and `/files/~/x` meet, and escaping the whole path into one
segment makes it a header in disguise. A header reaches ratd as sent, the path percent-encoded as
the path of a URL: parsers strip the spaces around a header value and refuse control characters,
both legal in a file name (measured with Go 1.27: a trailing space lost, a new line refused).
POST rather than PUT: the target of a request is its URL, and `/upload` writes what it receives
where a header says, which is what POST means; PUT would claim to replace `/upload` itself. The
headers of rat are prefixed `Rat-`: `Rat-Overwrite` rather than WebDAV's `Overwrite`, whose
absence means overwrite, the opposite of ours.

### What a transfer promises

A transfer copies one regular file, whole, by path. Its model is `rsync` for one file: a new file
written next to the target and renamed over it once complete, or, asked in place, the target
written into, as `rsync --inplace`, `cp` and `scp` write it; permissions as `rsync` and `scp`
without `-p`. It departs from them three times, each for an agent that does not watch its copy: an
existing file is replaced only when asked (`overwrite`, where they replace), a symbolic link at
the end of a path is refused (where they follow it), and a source changing while it is read fails
the copy (where GNU tar warns, and the others copy it torn). Where the list below is silent, a
transfer does what these tools do, or less, never more.

The list is what a review checks the code against, and nothing else. A finding is a bug when it
breaks a guarantee, with a reproduction on a local file system on Linux or macOS. Otherwise it is
a limit, a line below and no code, or nothing. A fix adds no promise: a new one is a line here
first, decided on its own. Without a list, every true statement about files is a promise to keep:
a file system, a kernel and a network have more cases than a review has rounds, each fix stating a
new one for the next review to test. A line marked *Not yet* is ahead of the code, and loses its
mark with the change making it true.

**Guarantees.**

- **G1. The bytes.** A transfer told done left at its destination the bytes its source held, for a
  source whose size, modification time and change time did not move while it was read (see The
  change check).
- **G2. Done is told from the fact.** "Uploaded" is told from ratd's 201 or 204 alone, "Downloaded"
  once the file is placed: never from what was sent or read.
- **G3. Whole or untouched.** Without `in_place`, the destination names its previous file, whole,
  or the new one, whole, whatever ends the transfer: a cancel, a connection lost, ratd or rat
  stopping, a full disk, a source that changed (see All or nothing).
- **G4. Nothing replaced unasked.** Without `overwrite`, an existing file is never replaced, one
  appearing during the copy included.
- **G5. Regular files, where asked.** A transfer reads and writes regular files only. It never
  follows a symbolic link at the end of a path, and never blocks on a FIFO. rat opens no file
  outside its transfers directory: of a symbolic link leading out of it, it tells that much, having
  followed it to know.
- **G6. Nine bits of mode.** A new file takes the permission bits (rwx) of its source minus the
  umask; a replaced file keeps its own. Nothing else of the metadata of a file is promised (see L1
  and L2).
- **G7. One write in place at a time.** Two writes in place into one file through ratd
  (`write_file`, an upload with `in_place`) never interleave: the second waits for the first, which
  keeps G1 for an upload in place following one canceled, whose handler may still write the bytes
  already sent.
- **G8. Nothing waits on a transfer.** A tool call waits 50 seconds at most
  (`tools.MaxWaitSeconds`), and stopping ratd or rat cuts their transfers at once. A transfer still
  running is found again by its paths, and its outcome kept until told.
- **G9. No byte to the model.** A transfer tells paths, sizes, modes and the errors of the system,
  never content.

**Outcomes.** Every answer about a transfer that ended conveys one of three states, decided from
three facts: what ratd answered (done, a refusal, its go-ahead and nothing after, or nothing at
all), whether it was asked in place, whether the whole file was sent.

| State | When | What the agent does |
|---|---|---|
| Done | ratd answered 201 or 204; a download was placed | nothing |
| Untouched | refused by the local side; ratd answering nothing at all, never reached or lost before its go-ahead, before which it does not touch the file; any end without `in_place`, the answer lost aside | fixes the cause, copies again |
| Unknown | `in_place`, ratd giving its go-ahead and answering nothing after, or the copy canceled, whenever the cancel came; the answer lost once the whole file was sent (without `in_place`: the old file or the new one, whole) | checks the file in a terminal, or copies again |

A refusal of ratd is relayed as it is: ratd, which knows whether it emptied a file written in
place, tells which state it left. rat, which does not know, never derives it from the bytes it
read or the size it saw: each such case is a proof to hold (an empty file, one emptied between two
looks, a body buffered), where an agent with a terminal checks in one command. The price: an
upload in place canceled before its first byte is told to check the file, rather than told it is
as it was.

**Limits.** Each is a line a finding is closed with.

- **L1. Metadata.** Replaced by a new file, a file loses its hard links, extended attributes, ACLs
  and file capabilities, and its owner and group become those of a new file of the user writing
  it. Written in place, it keeps them, its capabilities aside. As `mv` and `cp`.
- **L2. Setuid and setgid.** They never travel from a source, and a file written loses them, as
  with `cp` and `scp`: a new file put in place has none, and the system clears those of a file
  written in place, unless it keeps them: for root, and on macOS for a file only emptied, its
  new content empty. The answer tells the bits lost: those the file showed when a new one replaced it,
  those a file written in place showed before the bytes and no longer shows once written.
  Rejected: putting them back, with a chmod once written, and a chown before the rename
  for the owner the bit is for. ratd's chmod is that of the user of the terminals, where the agent
  sets the bit again itself (`chmod u+s`): it added no ability, and took a sync before the chmod
  for network file systems, a look after it for a chmod dropping setgid without an error, and a
  lock for two writes clearing and putting back in turn. Rejected as well: refusing such a file,
  which an agent then replaces in a terminal, with the same result.
- **L3. Power and kills.** No fsync. A ratd or a rat killed leaves `.NAME.rat-RANDOM` behind.
- **L4. No hard links.** On a file system without them (FAT, sshfs), G4 does not cover a file
  appearing during the copy: it is replaced.
- **L5. Network file systems.** A transfer tells what the system tells it. What the cache of a
  client shows, a reply lost, an error told at close: nothing specific is promised.
- **L6. The change check.** It misses a rewrite keeping the size within a tick of the clock, writes
  through a memory mapping and, on Windows, what the modification time does not show.
- **L7. Files without a true size** (`/proc`, `/sys`) are refused by ratd, as the source of a
  download. rat does not look for them in its transfers directory, not meant to hold any.
- **L8. Other writers.** For a file another program writes or swaps during a transfer, G5 holds,
  and nothing else. G7 covers the writes of one ratd only. On rat's side, which can not open a file
  without following (`os.Root`), a link put at the path between the look and the open is followed
  when it leads to the very file looked at.
- **L9. Early refusals.** An existing file without `overwrite`, a link, what is not a regular file
  and a path ratd can not read are refused before any byte is sent. Any other refusal comes when
  the system gives it.
- **L10. Wording.** The sentences are not the contract, the state they convey is. Progress, rate,
  time left and "ended N ago" are estimates.
- **L11. Windows** (rat only): built and vetted, not tested.
- **L12. rat restarting** cuts its transfers and forgets their outcomes: G3 holds, a file written
  in place is unknown.
- **L13. One file, two transfers.** rat knows a transfer by its destination as written, cleaned:
  `~/x` and the absolute path it stands for, or two paths through a link of ratd's machine, are
  two transfers of rat to one file, as are those of two rats. G3 and G7 hold for each: without
  `in_place` the last one placed stays, whole, and writes in place go one at a time. A write
  in place told done (an upload with `in_place`, `write_file`) may have written a file another
  transfer replaced by a new one meanwhile: its bytes are then in no file at the path.
- **L14. Directories.** The missing directories of an upload are created before the go-ahead and
  left by whatever ends it; an upload cut, canceled, told unreachable or whose answer was lost does
  not name them.
- **L15. File systems without permission bits** (FAT, exFAT): G6 does not hold, a new file showing
  what the file system gives, untold.
- **L16. A call canceled as it is answered.** rat forgets a transfer as it writes how it ended,
  never before. A harness canceling its call at that instant, its cancellation crossing the answer,
  may drop that answer: the outcome is then told to nobody (G8), the file being what the transfer
  left.

### How a transfer keeps them

**All or nothing.** Unless asked in place (see In place), an upload goes to a temporary file next
to its target (`.NAME.rat-RANDOM`, with the permission bits sent minus the umask), put in its place
once complete: an upload cut midway leaves the previous version whole. Its path, absolute, is 14
bytes longer than the target's for a name of 200 bytes or less: a target within 14 bytes of the
longest path the system takes (`PATH_MAX`: 1024 bytes on macOS, 4096 on Linux) is refused as a name
too long, though `write_file` writes it. Left as it is, for paths that long: working relative to
the directory, as rat does with `os.Root`, would take the whole placing of the file with it.
`write_file` makes the opposite choice (see Files), for content it holds whole; a file arriving for
minutes, half replaced, would be worse than none. Without overwrite, the temporary file is linked
under the name of the target, which fails if a file appeared meanwhile (412), where a rename would
replace it; with overwrite, it takes the permission bits of the file it replaces, then is renamed
over it. Permissions follow `scp`: a new file gets those of its source minus the umask, a replaced
file keeps its own nine bits, and setuid, setgid and sticky never travel nor are put back (see
L2). The price of the rename: hard links to a replaced file are broken, its extended attributes
lost, and its owner becomes ratd's user (rat runs as a user of its own on each side, as the manual
recommends). On a file system without hard links (FAT, some network ones, FUSE without link as
sshfs, whatever error its link gives: EPERM, EMLINK, ENOSYS, ENOTSUP), the file is renamed even
without overwrite, a file appearing in between then being replaced; rat does so too, on Windows
with the errors of Windows (FAT, exFAT: ERROR_INVALID_FUNCTION). No fsync: all or nothing covers a
transfer cut, not a power cut, after which a file may be lost as any other just written; an agent
wanting it on disk runs `sync`. A ratd killed during an upload leaves its temporary file behind. A
link or a rename failing is no proof: on NFS, one the server made fails all the same when its reply
is lost (link(2), BUGS), the temporary file then found in place, which ratd and rat both tell
written.

**In place.** With `Rat-In-Place`, an upload replacing a file writes into it instead, as `cp` does:
the file keeps the hard links, extended attributes and owner the rename loses, and its directory
needs no write permission (a file of ratd's user in a directory of root, one of another user under
the sticky bit of `/tmp`), where a new file can not be put in its place: the permission of the file
decides instead, a read-only file refused (403), which the rename replaces. Nor does the disk need
room for a second copy, which a new file takes until it is renamed over the old one: a large file
on a disk short of room can be replaced in place. The price: a body cut short leaves the file
partly written, the beginning of the new content, as `cp` leaves it, which an agent asks for
knowingly, harmless for a file nothing uses until it is uploaded whole. A missing file is created
all or nothing whatever the header says: in place would only lose there. The file is opened without
following a link at the end of the path nor blocking, and refused if it is not a regular file,
before the go-ahead: a refusal (a link put there meanwhile, a file ratd's user can not write)
leaves it as it was. It is emptied only once the body comes, its first bytes or its end: a
connection lost before, the go-ahead received or not, leaves it as it was too. Emptied before the
go-ahead, it would be left empty by a connection lost before the go-ahead reached rat, which then
can not tell ratd was reached at all.

Writes in place into a file go one at a time, `write_file`'s and those of uploads, by its device and
inode (`fileLocks`): a write can outlive the call that made it, the handler of an upload canceled
reading and writing the bytes already sent before it sees the cut, while the agent, told the file
may be partly written, writes it again at once. The second write waits for the first to end, an
upload before its go-ahead, `write_file` before emptying the file. Without the wait, the two would
interleave: the file torn, the late bytes of the first left after the content of the second. The
wait gives way to a stop of ratd, the file left as it was, and `write_file`'s ends with its call
too, nothing written: a write coming once the other ends, long after its call was answered, would
replace what that one left. Rejected: refusing the second write while
the first ends, which would make the agent call again for a wait of moments.

Without `Rat-In-Place`, an upload denied a new file in place of one ratd's user can write
(`access(2)`) is refused telling the agent to set `in_place`, the parameter of `upload_file`
sending the header, rather than told, as of any permission denied, that the file can not be
written, which it would believe. A file that user can not write either is told so, in place being
refused too. A directory refusing a new file is met before the go-ahead, the sticky bit only by the
rename, once the body came: ratd does not predict it, which would copy rules of the kernel that
vary (`CAP_FOWNER`, the ACLs of macOS).

What the headers alone refuse is refused before any byte is sent: rat waits for the go-ahead of
ratd (`Expect: 100-continue`), which Go's server gives when the handler first reads the body,
however long the checks take: `connect.HTTPClient` waits with no limit, rather than the second of
Go's default, past which the body would leave unasked, a check slower than that (a file system slow
to answer) then refusing once bytes had left, and a file written in place emptied by bytes whose
go-ahead never reached rat, which would tell ratd unreachable. Waiting costs nothing: ratd reads no
body before its go-ahead. The headers alone refuse an existing file without overwrite (412), a
symbolic link or what is not a regular file (409), a relative path or a header ratd can not read
(400), a path naming a directory by its form (a final slash, or a last part `.` or `..`, which
cleaning would turn into the name of a file: 400), a body declared other than bytes (415:
`curl -d @file`, for one, strips the new lines of the file), and a path the system fails to look at
or refuses, told as it tells it (a name too long, or not UTF-8 on APFS: 400). Measured: without the
go-ahead, a refusal came once 384 KiB of the file had left.

**Symbolic links.** A link at the end of the path is refused, naming its target, every link
resolved (for a link leading nowhere, its target as written): a rename would replace the link
itself, and following it, as `write_file` does, would surprise as much. The links among the
directories above are followed, as any program does (`/bin` under a merged `/usr`, `/tmp` on
macOS).

**Looked at before opened.** As `read_file` and `write_file` do, a download looks at its file
before opening it, with `Lstat`, and refuses unopened a link or what is not a regular file: opening
acts on what is not a regular file. It releases a command waiting to write into a FIFO, whose next
write then fails once the refusal closes it, and a device may act on it (a tape rewinds). The file
opened is checked again, as it may have been replaced in between: opened with `O_NOFOLLOW`, which
refuses a link as the last part of the path only, and without blocking. An upload looks at its
target with `Lstat` before its bytes come and once they came, and opens only a temporary file of
its own; in place, it looks before only, then opens its target as a download does, and checks it
again (see In place).

**The change check.** The side reading the file compares its size, modification time and change
time when opening it and once read (package `internal/stamp`), as GNU tar does: a file still being
written (a log, a build output) is not copied torn. A download sends the size the file had when
opened, so that a file growing forever still ends, then tells a change, or a read failing, in the
`Rat-Error` trailer of its chunked body, and rat drops what it received. Its sentence names the
path, which may hold what a header can not carry (an escape, a new line): it travels
percent-encoded there, and there only (`connect.EncodeError`). That size is not always
the one of the content: a file the system makes as it is read shows 0 (`/proc`, cgroups) or a
page (`/sys`), which a download would send empty, or end short of. Such a file is refused,
pointing to `read_file`, which reads it to its end, or to a copy made in a terminal: showing 0, by
a first read giving bytes before anything is sent; ending before its size, unchanged, in the
trailer. Rejected: reading it to its end, as `read_file` does, before its headers, a second way of
reading in ratd for files a terminal copies. A change in an upload is
rat's to find: it ends the body with an error instead of its last chunk, and ratd, seeing the body
cut, drops its temporary file, or leaves a file written in place partly written; a file read past
the size it had when opened is cut at once, rather than sent for as long as it grows. A cancel ends
it the same way, until the last chunk is sent: past it, ratd may be writing the file, and the
client waits for its answer rather than leave unknown whether it was written, unless it stops,
nobody reading that answer any more (`connect.Upload` holds the client side). No digest: TLS
authenticates every record, so a byte altered on the way cuts the connection, and the bytes go from
file to socket through the standard library alone. A digest computed on the stream at both ends
would check the same bytes again, and miss a file changing while read, both ends hashing the same
torn bytes; one computed by a first read of the file would catch it, at the price of reading it
twice, which the change check spares. A trailer only follows a chunked body in HTTP/1.1, which
carries no `Content-Length` (Go silently drops a trailer sent next to one): the size of a download
travels in `Rat-Size`.

**Bytes as they are.** `application/octet-stream` both ways, never a charset nor a conversion,
which FTP's ASCII mode taught. No compression: a content coding changes what the length and any
digest describe, and the one that would not, a transfer coding, Go's server refuses (501). gzip is
not the cost: it compresses Go sources at 200 to 350 MB/s on one core of a recent laptop, and
passes incompressible data through at 2.4 GB/s. An agent compresses on the remote side, where it
has a shell. Rejected as well: HTTP/3, which a single bulk stream gains
nothing from, and which Go 1.27 does not offer publicly.

Transfers have no size limit nor timeout: a full disk fails the transfer (507), its temporary file
removed, or a file written in place left partly written, and a transfer on a slow link may take
hours. Stopping ratd cuts them at once (see
Shutdown).

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
> with (close_window). A window named main exists when you start. write_file and read_file copy a
> whole file to or from the machine through your context, rather than the terminal: for text you
> write or need to read. To copy files without them entering your context, use a tool copying
> files by path if you have one. For the exact output of a long command, run it as command 2>&1 |
> tee /tmp/name.log: the screen still shows it, and read_file reads the file whole or a range at a
> time, rather than reading the window over and over.
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

1. **Load and check the bundle**: the server certificate names the tenant. When it names a CRL
   distribution point (a sub CA of another PKI), fetch the CRL, and refuse to start without a
   valid one, or with the certificate revoked: starting would let in the certificates the CRL
   revokes, and no terminal exists yet to lose (see Revocation in `mtls/README.md`). Otherwise,
   log that revocation is not checked: expected of a bundle, telling of a sub CA whose server
   template lacks the distribution point. Then warn of a deployment handing agents more than the
   user ratd runs as:
   - **Root**: every agent, and whoever holds a client key, is root. The likely mistake is a
     service missing its `User=`, which nothing else shows.
   - **A bundle that user can replace**, the key aside, which ratd owns: an agent could put one of
     its own in place, whose clients ratd would let in at its next start, a door left open after
     an incident was cleaned up. Writable means for the process (`access(2)`, groups and ACLs
     included): the certificates, and every directory above them up to the root, through which a
     file or a directory below can be renamed and another put in its place, for the path as given
     and for the target of its links. Not checked as root, which can write anything.

   Warnings rather than refusals: root is a choice for a machine given whole to the agents, and
   ratd started by hand, as one tries it, runs as a user owning its bundle. Rejected: refusing
   both, which needs a flag kept forever for root (`--allow-root`), and breaks a first try; telling sticky directories (`/tmp`) apart, which needs owners compared,
   while a bundle there is a mistake anyway.
2. **Start the tmux server**, which takes the tenant: a server already answering on its socket
   (`tmux.ErrServerSocketInUse`) means another ratd serves it on this machine, which the error
   names with its PID. Other failures: tmux missing, bash missing or too old.
3. **Check the terminals**, once (`tmux.Controller.CheckTerminals`, 10 seconds at most): what bash
   startup files defeat at the prompt of terminals, bracketed paste, and the prompts recorded with
   the status of commands, and of each command of a pipeline. Terminals keep working either way
   (single line input is fine), so an override, a command run before rat's (which loses the statuses
   of pipelines, ratd then telling none), or a check that can not run, is a warning: logged, and for
   bracketed paste told to agents, in the MCP instructions and the description of `send_text`. A
   check that can not run is taken as nothing holding. A startup file blocking also blocks every
   terminal: the log says so. Checked once rather than at each tmux restart: clients receive the
   instructions and the tools once, and must not be told something the server no longer believes. A
   startup file changed while ratd runs is caught at its next start, and shows meanwhile in the
   windows created since, which `list_windows` and `wait_window` tell as recording no prompt.
4. **Listen**: a second ratd for the same tenant fails at step 2, before opening an endpoint.

Started by systemd with `Type=notify`, ratd tells it it is ready (`READY=1` on `NOTIFY_SOCKET`)
once it serves: `systemctl start` returns on a ratd answering, or reports the failure of any step,
where a `Type=simple` service shows started, then failed a moment later. A few lines rather than a
library: one datagram on a socket. Not used: the watchdog (`WatchdogSec=`), which restarts or fails
a ratd that stopped pinging: ratd keeps its terminals up itself, and exits when it can not, with no
`Restart=` on purpose.

ratd takes `NOTIFY_SOCKET` out of its environment before starting tmux, as `sd_notify` does when
asked (`unset_environment`): the terminals inherit ratd's environment, and systemd's own tools
report how they exit to that socket (`EXIT_STATUS=`, `ERRNO=`, measured with systemd 255). Each
`systemctl` or `journalctl` an agent runs, or `systemd-detect-virt` in the MOTD of a login shell
(Ubuntu), would reach systemd, which drops what is not from ratd (`NotifyAccess=main`, the default
of `Type=notify`) with a warning in ratd's journal each time, burying ratd's own.

In the last tenth of the validity of the bundle (a year, for one of rat-tool; see Expiry in
`mtls/README.md`), ratd warns at startup and every 24 hours: sysadmins need reminding, an expired
bundle stops every client at once.

### Status

Started by systemd with `Type=notify`, ratd also tells it a status line (`STATUS=`), the one line
`systemctl status` shows whatever the logs did since: with one line per tool call, the last log
lines are calls, burying what was logged earlier. It tells first what degrades ratd while it
serves, then whether stopping ratd now cuts anyone:

```
serving tenant prod on [::]:7281 · bracketed paste overridden, bundle expires in 212d · last seen: alice now, bob 25m ago, carol 3h ago · 1 wait in flight · 2 transfers in flight
```

- **Degradations**: known issues of a ratd serving all the same, logged once when found, and shown
  for as long as they hold, in a fixed order, with short labels (the log tells the details):
  running as root or a bundle its user can write (see Startup), what bash startup files defeat in
  the terminals or the check not running, the bundle expiring soon (updated daily), the bundle
  files changed and refused (a new CA pending a restart, or another reason), the server key
  changed by a renewal (for a day), the CRL of the CA not renewed (with when the one held goes
  stale) or none valid, the deaths of the tmux server within the window of its restart budget
  (gone once older). Some exclude others. First in the line, should a terminal cut it.
- **Last seen**, per session, the time of its last request, whatever the method: the five most
  recent, then a count of the others. Last seen rather than active or idle: requests are
  stateless, no connection stays open between them.
- **Waits and transfers in flight**: the calls of `wait_window` waiting and the file transfers
  under way, what a stop cuts. The other calls take milliseconds, too short to be seen at a
  render: a count of them would read 0 nearly always.
- **Stopping**: the step of the stop replaces the line, `stopping: closing the door`, then
  `stopping: stopping the terminals`: a stop lasting shows where. No waits nor transfers there: a
  stop makes them end at once.

This is state ratd keeps, of its calls, which tmux knows nothing of: lost when ratd stops, which is
fine for a display. Rendered every 10 seconds, and sent only when it changed: times are coarse
(`now` under a minute, then minutes, hours and days), most renders send nothing. A degradation
found and a step of the stop are sent at once. Nothing is sent before ratd is ready. Not shown:
totals (calls, bytes), which grow forever and answer no question of an operator; the windows and
the commands they run, which are tmux's to tell, fresh: `list_windows`, or a human attaching. Any
local user can read the status of a system unit, where reading the journal takes a group: session
names are shown, never the addresses of clients.

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

Started by systemd with `Type=notify`, ratd tells it it is stopping (`STOPPING=1`) as soon as the
door starts closing, whatever stops it: a signal, or the terminals dying too often, which systemd
would otherwise only learn when ratd exits.

On SIGTERM or SIGINT, the door closes before the terminals: ratd stops accepting connections and
gives the calls in flight 10 seconds, then stops the tmux server (with the controller's own
escalation, SIGTERM then SIGKILL). The other order would answer calls arriving in between that
the terminals are restarting. A call waiting (`wait_window`, or `create_window` for a first prompt)
returns at once, the first telling ratd stops:
`http.Server` cancels no call when shutting down, it waits for them, and a wait would hold the
door open for the whole grace, then be cut. A file transfer is cut at once as well, the read and
write deadlines of its connection set to the moment ratd stops (`http.ResponseController`), as it
may last hours: an upload cut drops its temporary file, or leaves a file written in place partly
written. The kill switch does not depend on what agents wait for, nor on what they copy.

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
  `ReadTimeout` nor `WriteTimeout`: a 4 MiB request on a slow link must not be cut, nor a file
  transfer lasting hours. A file system not answering blocks a transfer until it answers; a stop
  does not wait for it beyond the grace.

## Serving

- **Endpoints**: `/mcp`, and the file transfers of rat, `/upload` and `/download` (see File
  transfers); anything else answers 404. The MCP specification imposes no path; `/mcp` is its
  example, and the convention of the SDK. ratd serves nothing else: `rat-tool` checks a running
  ratd.
- **Listen address**: all interfaces by default (`:7281`), which mutual TLS makes safe. Written
  with an empty host rather than `[::]`: Go then listens on IPv6 and IPv4 at once, where a
  literal `[::]` fails on machines with IPv6 disabled. 7281 reads "RAT!" on a phone keypad; IANA
  lists it for an obscure product (`itactionserver2`), and it is configurable.

The path and the default port are declared in package `connect`, the defaults ratd and its
clients share: both sides of the contract in one place.
- **TLS**: served through a TLS listener built from the checked bundle. `http.Server.TLSConfig`
  alone would be silently ignored by `ListenAndServe`, serving plain HTTP: a test must guard that
  a client without a certificate is refused. HTTP/1.1 only (no `h2` offered): MCP requests are
  short JSON exchanges, which HTTP/2 would not speed up, and a file transfer gets a connection of
  its own rather than sharing one with calls.
- **Rejected handshakes** are logged, at most once every 10 seconds with the count of the others:
  a public port gets scanned. The standard library would otherwise print them through the `log`
  package, outside ratd's logs.

## Logs

One line per tool call, at info level, or warning for a failure the agent did not cause (tmux
not answering, an internal error): the session (the client name), the address of the client, the
tool, the window, the outcome (`ok`, `message` for a result that is not an error, `error` with
its cause) and the duration, plus what tells the action without its content: the size of a text
and whether Enter was pressed, the number of keys, the rows of history asked and read (and the
size of a screen refused), the path and size of a file. Never the content: neither text, nor
keys (they can spell a password one key at a time), not even a key name refused, told by its
position (an agent mistaking `send_keys` for `send_text` may give a password as a key name), nor
files. One line per file transfer as well: the session, the address, the direction, the outcome,
the duration, the path, the bytes and the HTTP status, never the content. And the lifecycle: startup
(version, tenant, address, read budget, bundle expiry, the CRL fetched), the check of the
terminals, tmux exits and restarts, the crash budget, shutdown. A request refused (a client
misnamed, revoked or expired, or no valid CRL) is a warning, with the session, the address and
why; so is a fetch of the CRL failing, with when the CRL held goes stale, and each newer CRL is
logged. A renewal of the server certificate is logged, a warning when its key changed; so are
bundle files changed and refused, and restored (see Renewal in `mtls/README.md`).

The address tells apart the agents sharing a client certificate, and shows a certificate used from
an unexpected machine. It is the peer of the connection: behind a proxy, the proxy's, whose own
logs carry the trail on. A header naming the client (`X-Forwarded-For`) is never read: mutual TLS
ends at ratd, so no proxy can be trusted to set it. The address may be personal data (GDPR): it is
logged for security, as a web server logs its clients. ratd keeps nothing itself: how long the
logs are kept and who reads them is the operator's call (journald, log shipping). Not provided: an
option to leave the address out. An operator who must not keep it can filter it out of the logs,
one who needs it could not get it back once left out.

Not provided: the content of inputs, even as an option (`--log-input`). The trail it would give
is partial: only what is typed in terminals, not what `write_file` writes (a script then run by
one line), nor what scripts, detached commands and cron jobs run, which never go through ratd,
and a text cut to fit a log line loses the long heredocs an inquiry needs. It would also put the
secrets agents type (passwords at a `sudo` prompt, tokens) in logs readable by whoever reads the
journal, and wherever logs are shipped. The trail is elsewhere, whole: the logs of ratd tell who
called, from where, when, which tool on which window; the transcripts of harnesses hold every
call with its arguments and its result; the audit of the system (`auditd`) records every process
the user of ratd runs, with its arguments, whatever started it (see Security in [`MANUAL.md`](../../MANUAL.md#security)).

At debug level, one line per MCP request (the session, the address, the method, the duration),
whatever the protocol version: clients initialize, or discover the server from protocol 2026-07-28
on (the Go SDK does). The SDK logs every stateless request at info level (a session connecting,
then disconnecting): only its warnings and errors are kept, all of it at debug level.

Logs are text lines on stderr (`log/slog`). When stderr is the journal stream, which systemd
names in `JOURNAL_STREAM`, each line goes without its time, which the journal records itself,
and without its level, which a `<N>` prefix turns into the priority of the entry
(`SyslogLevelPrefix=`, on by default): `journalctl` shows the time once, highlights warnings and
errors, and filters them (`-p warning`). The error ratd exits with, printed outside the logs, is
an error there too. Detected by the device and inode of stderr rather than
`INVOCATION_ID`, which every service gets, logging to the journal or not, and which a command
started from a service inherits. Rejected: the native protocol of the journal, whose fields
(`SESSION=`, `TOOL=`) would take a second format, where `journalctl --grep` finds the same in the
text lines.

## Configuration

```
ratd --bundle DIR [--listen :7281] [--read-budget 64KiB] [--log-level info]
```

`--bundle` is the server directory of a bundle (see `rat-tool`); the tenant comes from its
certificate. `--read-budget` takes a size with its unit (`64KiB`, `1MiB`, `65536B`, parsed by
`github.com/hekmon/cunits`). Decimal units are accepted too, and so are bits (`Kb`): such a
mistake usually falls under the minimum, and the refusal tells the size read. No configuration file
nor environment variable: nothing ratd needs is secret on a command line.

## Open questions

- Output redaction (see `AGENTS.md`).
