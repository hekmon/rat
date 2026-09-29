# tmux controller: design

This document explains why the controller is built the way it is: the design decisions, the
alternatives they rejected, and the tmux pitfalls they avoid. How to use the package is
described by its godoc (`doc.go`).

Every pitfall described here was met on a real tmux, and most are guarded by a test: when
changing something below, run the tests against a real tmux, they are the executable part of
this document.

## Server

### rat owns a foreground server

tmux normally daemonizes its server: the process starting it exits, and nobody can wait for
the server. rat starts it with `-D` instead, which keeps it in the foreground as a child of rat.
rat can then wait for it, notice right away when it dies, and reap it.

A single goroutine calls `Wait()` on the server (it can only be called once); everything else
learns the server exited through a channel closed by that goroutine. This is how an early exit
is reported during startup, how `StopServer` waits for the exit, and how `StartServer` notices a
previous server died on its own and can start a new one.

The consequence is that stopping rat stops its terminals, and their running commands.
Rejected for now: adopting a server already running on the socket at startup, which would let
terminals survive a rat restart but loses the ability to wait for the server (it is not rat's
child anymore).

### One socket per tenant

The server listens on `-L rat-<tenant>` (`rat` for the default tenant). tmux puts sockets in a
per user directory (`/tmp/tmux-<uid>/`, mode 0700, refused if owned by someone else), so
tenants of different users never clash, and other users can not reach them. Tenant names are
validated: `/` or `..` would move the socket out of that directory, and the length is bounded
by the unix socket path limit (104 bytes on macOS, 108 on Linux).

### Readiness: our server, answering

The server socket appears a few milliseconds after the process starts. `StartServer` probes the
server (`display-message -p '#{pid}'`) every 50 ms, for up to a second, and only accepts an
answer carrying the PID of the process it started. Another server may be answering on the
socket: a leftover of a previous run, or another rat on the same tenant.

- If a server already runs on the socket, `tmux -D` does not start a second one: it connects to
  it as a client, fails (`open terminal failed: not a terminal`) and exits. The watcher reports
  that exit within milliseconds, with the tmux message: its stderr is kept (bounded) for that
  purpose, as tmux only writes there when failing at startup.
- A stale socket (its server died without cleaning it) is simply taken over by the new server.

Rejected: checking the socket is free before starting. It could be taken right after the
check, so the check after starting would be needed anyway.

### Stopping, gracefully then not

`StopServer` asks the server to exit (`kill-server`), then escalates:

1. no exit after 5 seconds: the server context is canceled, which sends SIGTERM (a custom
   `Cancel`: the default would be SIGKILL, giving tmux no chance to clean up);
2. no exit 2 seconds later: `WaitDelay` makes Go send SIGKILL.

`WaitDelay` does not bound how long `Wait()` blocks on a running process: its timer only starts
once the context is canceled (or the process exited with its pipes still open). This is why the
first step needs its own timer. The error tells what was needed (`ErrServerTerminated`,
`ErrServerKilled`), read from the exit status. Even SIGKILL can not guarantee the server is gone
when `StopServer` returns: a process in uninterruptible sleep only dies when it leaves it.

### Commands and the server lock

Commands on sessions and windows hold the server lock shared (they run concurrently), starting
and stopping hold it exclusively. Without it, a command could run while the server is being
stopped: `new-session` would then start a new server by itself, daemonized, which rat would not
watch nor stop. Only a server crashing while a command runs can still lead to that.

## Environment

The goal: the same terminals on every machine, whatever the user configuration and whatever the
environment rat runs in (a systemd service often has no locale, no terminal, `/` as working
directory).

### No tmux configuration

Every tmux invocation passes `-f /dev/null`, so neither the user configuration
(`~/.tmux.conf`, `~/.config/tmux/tmux.conf`) nor the system one (`/etc/tmux.conf`) is loaded:
they could change the prefix, the window indexes, add hooks or plugins. The flag only matters for
a command starting a server, but that is not only `StartServer`: `new-session` starts a server
when none is running, so every invocation carries it.

### Options

Without a configuration file, rat sets what it relies on once the server is ready, everything
else being tmux defaults:

- `default-shell`: bash (see below).
- `history-limit` 10000 (tmux default 2000): the scrollback a capture can include.
- `default-size` 200x24 (tmux default 80x24). Wide, because programs lay out their output for
  the terminal width and some truncate lines to it (`ps`, `docker`, tables): joining wrapped
  lines at capture can not recover what they cut. Short, because the height is what every
  capture returns, and agents capture repeatedly while waiting for a command. Not shorter than
  24 though, the classic height full-screen programs are designed for: below it some refuse to
  run or cut their menus, `top` shows few processes, and pagers kick in more often.
- `window-size manual`: with the default (`latest`), a human attaching to inspect the terminals
  resizes them to their own terminal, and they keep that size once the human detaches. It is set
  on each window, in the invocation creating it, not globally: tmux 3.3 to 3.6 crash (segfault,
  killing every terminal) when a session is created while the global value is `manual`. Fixed in
  tmux 3.7 (commit `7d41761e`, GitHub issue 4849), without a changelog entry. The global value
  stays at the default, and a test guards it.
- `allow-rename off`: already the default, but window names are how agents find their
  terminals: programs must not rename them (escape sequences).

### Terminals run bash, at home

Terminals run bash, checked at `StartServer` rather than failing on each new window: it is
always available, and the shell agents know best (zsh differs just enough to mislead them).
bash starts as a login shell: it reads `~/.bash_profile` or `~/.profile`, not `~/.bashrc` unless
the profile sources it.

Sessions and windows start in the home directory of the user running rat, as a terminal does:
with a dedicated user for agents, it is their own. tmux would otherwise start them in the
working directory of the client creating them, rat's (`/` for a systemd service). This holds for
windows too: a new window does not inherit the directory of its session, it needs `-c` as well.

### Terminal environment

Terminals inherit rat's environment, plus variables enforced at `StartServer`:

- `LC_ALL=C.UTF-8`. Without a UTF-8 locale, bash reads non ASCII input (é, ✓) as meta keys and
  mangles it: `echo 'ok ✓ café'` becomes `echo ' cafOk'`. The C locale gives English messages
  and stable formats (decimal point, dates), the easiest for models to read, and `LC_ALL`
  overrides any other locale variable. Rejected: fixing the locale only when rat's is not UTF-8
  (with `LC_CTYPE`), which kept terminals different from one machine to another.
- `PAGER`, `GIT_PAGER`, `MANPAGER`, `SYSTEMD_PAGER` set to `cat`: tools would otherwise open
  `less` when their output does not fit the screen, and agents would have to notice it and quit
  it. The output goes to the scrollback instead. The tool specific ones are needed as they take
  precedence over `PAGER` and users often set them (`GIT_PAGER` also overrides `core.pager`).
- `BASH_SILENCE_DEPRECATION_WARNING=1`: on macOS, bash prints a "the default shell is now zsh"
  notice at each start, the first thing agents would read in every new terminal.

`TMUX` is kept, on purpose. tmux sets it in every terminal (it can not be removed with
`set-environment`, only by the command starting the terminal), and a `tmux` command typed in a
terminal uses it to reach rat's server. Removing it would point such commands to the user's
default server instead: an agent typing `tmux kill-server` would kill the user's own sessions,
and a plain `tmux` would open a tmux nested in its terminal, which tmux refuses as long as
`TMUX` is set.

## Targets and names

### Exact targets, never IDs

Windows are always targeted as `=session:=window`. Without `=`, tmux also accepts a prefix or a
pattern: `sess` would reach the session `session`, and `win` the window `window`, silently
acting on another terminal than the one named.

tmux IDs (`$session`, `@window`, `%pane`) are never used: they are global to the server, so an
ID would reach a window of another session. Keeping them would also be a cache, stale as soon
as a window closes behind rat's back (an agent typing `exit`, a human closing it). Names are
asked to tmux every time instead.

### Names

Session and window names follow the tenant rule: letters, digits, `_` and `-`, up to 32
characters. In targets, `:` and `.` separate the session, window and pane parts, and a leading
`=` asks for an exact match: any of them in a name would change what it targets.

Window names are unique within a session, as they are how agents find their terminals. tmux
accepts duplicates, and an exact target then fails as if the window did not exist, so rat
checks the name is free before creating a window (under a lock, the check and the creation
are atomic within rat). This only protects against rat itself: a `tmux new-window` typed in a
terminal could still create a duplicate.

### display-message never fails

`display-message` does not fail on a missing target: for a missing session it prints empty
fields, and for a missing window it silently describes the current window of the session
instead. Windows are therefore described from `list-windows`, looking the name up. The only
`display-message` left is the readiness probe, which has no target.

### Missing targets: ask tmux, do not parse its messages

tmux reports a missing target with messages depending on its state: `can't find session: x`,
but `no current target` once no session is left. Rather than parsing them, a failed command
is explained by asking tmux which sessions and windows exist, to return `ErrSessionNotFound` or
`ErrWindowNotFound`. It costs extra commands on the error path only.

## Reading tmux output

### UTF-8, whatever rat's locale

When the environment of a tmux client has no UTF-8 locale (a service often has none), tmux
replaces non ASCII characters by `_` in what the client prints: a path `/tmp/café` is read as
`/tmp/caf_`, and captures lose every accent and symbol. Every invocation passes `-u` to always
output UTF-8.

This is distinct from the locale of the terminals (see Environment): `-u` fixes what tmux
prints, `LC_ALL=C.UTF-8` fixes what bash reads. Both are needed for a text to survive from
input to capture.

### Printable separators only

tmux also replaces control characters by `_` in its formatted output (`-F`, `-p`): a tab
separating fields comes back as `_`, and the fields can not be told apart anymore. Rejected
separators: tabs and other control characters (replaced), and non ASCII characters (replaced
too without `-u`, and not worth depending on).

The window description is therefore separated by spaces, which must not be ambiguous:

- neither the name (validated) nor the numbers (activity, full screen, scrollback) can contain
  a space;
- the foreground command is quoted by tmux, with the `q:` format modifier escaping spaces and
  special characters with a backslash, and unquoted by rat;
- the working directory comes last, so it needs no quoting: everything after the command is
  the path.

## Input

Agents type into a terminal, they do not run commands: the input may answer a prompt ("press y
to confirm") as well as start a command. rat does not know when a command finishes, only which
process is in the foreground.

### One input, one tmux invocation

An input is a text with its optional Enter, or a series of keys, and each is a single tmux
invocation. tmux runs an invocation whole, so no other input can interleave with it, and rat
needs no lock on windows. This comes from the tmux source (3.7c):

- the client sends all its arguments, `;` separators included, in a single message
  (`client.c`);
- the server turns that message into commands appended together to the queue of that client
  (`server-client.c`, `cmdq_get_command` in `cmd-queue.c`);
- the server is single threaded, and drains a client queue in one go, stopping only on a
  command that waits (`server_loop` in `server.c`, `cmdq_next` in `cmd-queue.c`);
- `send-keys` and `paste-buffer` never wait. `load-buffer -` does, while reading its stdin, but
  before anything is typed: other clients may run then, not in the middle of the text.

Two agents typing in the same window are like two persons sharing a keyboard: each input is one
hand on it, and the inputs come one after the other, never mixed. A text and keys are separate
inputs on purpose: which keys to press usually depends on what the text caused on the screen.

### Text is pasted, not typed with send-keys

The obvious way, `send-keys -l -- text`, fails agents twice:

- **Size.** The client sends its arguments in a single message of 16 KiB at most
  (`MAX_IMSGSIZE`), and fails with `command too long` beyond: a text of about 16,300 characters.
  Agents paste files, scripts and heredocs, as a human would, and must be able to.
- **Parsing.** Arguments go through tmux parsing: words such as `Enter` would be sent as keys
  (needing `-l`), a leading `-` read as a flag (needing `--`), and a trailing `;` read as the end
  of the command and dropped (see below).

Text is therefore loaded into a tmux buffer from stdin, then pasted, in the same invocation:

```
load-buffer -b rat-input-N - ; paste-buffer -d -r -b rat-input-N -t =session:=window ; send-keys -t =session:=window Enter
```

- stdin is not subject to the message limit (115 KB went through unchanged in a manual test,
  64 KiB in `TestSendLargeText`), and never goes through argument parsing.
- `paste-buffer` writes the buffer to the terminal as is. `-r` keeps new lines, which tmux
  would otherwise replace by carriage returns. Without `-p`, no bracketed paste markers are
  added, even when the program asks for them (`cmd-paste-buffer.c`). With them, what a text does
  would depend on the program: bash 5.1 and later ask for them, and insert pasted new lines into
  the command line rather than running it (readline documentation, not tested: the bash of the
  development machine, 3.2, has no bracketed paste). Without them, programs read the text as typed.
- Buffers are global to the server: each input uses its own name (a counter), so concurrent
  inputs do not paste each other's text. `-d` deletes the buffer once pasted; a failed paste
  (missing window) leaves it, holding the text of an agent, so rat deletes it.
- `load-buffer` creates no buffer from an empty input, and the paste would then fail: an empty
  text only presses Enter, if asked.

Nothing is added nor removed. Enter is only pressed when asked, as a separate key: a new line
in the text is typed as a new line character, which bash (like most programs) reads as Enter,
but rat does not decide it for the agent.

Rejected:

- `send-keys -l` for short texts and the buffer for long ones: two paths and two sets of
  pitfalls, for no gain.
- Splitting long texts over several invocations: each part becomes an input of its own, which
  others could interleave with, bringing back a lock on windows.
- Limiting the size of texts: an agent must be able to do what a human does, and a big paste is
  valid.

### Trailing semicolons

tmux reads an argument ending with `;` as the end of its command, and drops that `;`, even
when the argument is passed without a shell: `echo a;` would be typed as `echo a`, and the `;`
key would not be pressed at all. A `;` preceded by a backslash is kept instead (tmux removes the
backslash). Text escapes this by not being an argument, but key names are (`;` is a key), so they
go through `tmuxArg`, which escapes a trailing `;` that way. A `;` elsewhere in an argument is
not affected.

### Keys by name

Keys are tmux key names (`C-c`, `Escape`, `Up`, `F5`…), validated before being sent: tmux types
an unknown name as text instead of failing, so `Ctrl-C` would be typed rather than interrupt
the command. Names are case sensitive (stricter than tmux), and only cover what agents need:
printable ASCII characters and the usual named keys, with the `C-`, `M-` and `S-` modifiers.

## Capture

A capture (`capture-pane -p -J`) returns the screen as displayed, preceded on demand by rows of
scrollback (`-S -n`, rows above the screen).

- `-J` joins the rows tmux wrapped at the terminal width, so a long line comes back whole. It
  also keeps trailing spaces, which rat removes, with the empty lines at the end of the screen:
  an idle terminal returns its prompt, not a screen of blank lines.
- The snapshot tells how many scrollback rows it includes: the ones requested, fewer if the
  scrollback is shorter. They are rows: a line wrapped over several rows is joined, so the
  content may show fewer lines than that.
- Under a full-screen program (`less`, `vim`, `top`), the terminal shows its alternate screen,
  and the scrollback above belongs to the terminal before the program started: included, it
  would be mistaken for the program output. It is never included then, and the snapshot says a
  full-screen program is running. What such a program displays disappears when it quits.

The snapshot carries these properties rather than failing (asking for scrollback under a
full-screen program is not an error): an agent reads in the result itself what it got and why,
without a failed round trip, which small models handle worst.

The window is described just before being captured, which costs a second tmux command: a
program starting or quitting in between makes the properties slightly outdated, as any output
after the capture would.
