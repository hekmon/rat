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

`WaitServer` exposes that exit to the caller, with its reason, so that it can restart the server
(the controller never does it by itself). Each run of the server is a value of its own
(`serverProcess`): a waiter keeps it, and still reads why it exited once the controller has moved
on to another server. An exit asked by `StopServer` is told apart from a server exiting on its
own: a crash, the OOM killer, or `tmux kill-server` typed in a terminal, which reaches rat's
server since `TMUX` is kept (see Terminal environment).

A foreground server also survives its last session: tmux turns `exit-empty` off for it
(`server.c`). Otherwise an agent closing its last window would stop every terminal of the tenant.

The consequence is that stopping rat stops its terminals, and their running commands: what an
admin stopping the service counts on (see AGENTS.md). Rejected: adopting a server already
running on the socket at startup, which would let terminals survive a rat restart, but loses
that guarantee, and the ability to wait for the server (it is not rat's child anymore).

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
  that exit within milliseconds. The message tells nothing of the cause, so rather than parsing
  it, `StartServer` asks the socket: a server answering there, ours being gone, is another one,
  reported as `ErrServerSocketInUse` with its PID. This is what keeps a tenant to a single rat
  on a machine (for a user). The tmux stderr is still kept (bounded) and included in the error,
  for the other startup failures: tmux only writes there when failing at startup.
- A stale socket (its server died without cleaning it) is simply taken over by the new server.

Rejected: checking the socket is free before starting. It could be taken right after the
check, so the check after starting would be needed anyway.

### Stopping, gracefully then not

`StopServer` asks the server to exit (`kill-server`), then escalates:

1. no exit after 5 seconds: the server context is canceled, which sends SIGTERM (a custom
   `Cancel`: the default would be SIGKILL, giving tmux no chance to clean up);
2. no exit 2 seconds later: `WaitDelay` makes Go send SIGKILL.

The 5 seconds include `kill-server` itself: a stuck server does not answer it, and the caller may
give no deadline (ratd does not, when it stops). `kill-server` failing does not mean the server is
stuck either: it may be exiting already, on a signal of its own, as when systemd stops a service
by signaling all its processes at once. The server is then waited for all the same, and its exit
reported as its own (`ErrServerNotRunning`) rather than as terminated by `StopServer`.

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

### A stuck server

A command on a server that does not answer (stopped, deadlocked, dying) must still return once
its context ends, for the caller to tell its own caller. Killing the tmux client is not enough: a
client hands its stdin, stdout and stderr to the server (file descriptors passed over the
socket), and a server not reading its socket leaves them in the socket queue, where the kernel
keeps them open. Waiting for the output of the killed client then waits for the server: with a
server stopped for 3 seconds and a client killed after half a second, its output reached its end
after 4 seconds (tmux 3.7c). Every command therefore has a `WaitDelay`: once its context has
ended and the client is killed, waiting gives up on the pipes after a second, and closes them.

### A server shutting down during a command

A server shutting down cleanly (SIGTERM, `kill-server`) tells its clients to exit normally: a
client whose command was still waiting exits with status 0, no output and no error, although its
command never ran (tmux 3.3a and 3.7c). The controller can not tell it from a success by the exit
status. It can by the output, for the commands that always print something: `list-windows`
lists a window at least (a session has one), a capture prints the state of the pane. No output
then means the server is gone, reported as `ErrServerNotRunning`. Silent commands (a paste, keys,
a new window) have nothing to tell: one sent while the server shuts down is reported as done, and
the caller learns of the shutdown with its next command.

A server killed, or dying on its own, makes its clients fail instead ("server exited
unexpectedly"). A failed command is explained by asking tmux what exists (see Targets and
names): when that fails as well, the server is given half a second to be seen exiting, and the
failure is reported as `ErrServerNotRunning` rather than with the message. Not once the context
of the command has ended: a server stuck rather than dying would make every timeout longer.

Rejected: ending each silent invocation with a command printing a marker, whose absence would
tell the invocation did not complete. Should the shutdown lose the output of an invocation that
did complete, it would be reported as failed, and retried: a command pasted twice runs twice,
worse than a paste reported done in terminals about to vanish.

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

bash 4.4 or later is required, and `StartServer` refuses an older one: pasted text relies on
bracketed paste (see Input), which bash has since 4.4. macOS ships bash 3.2 (the last version
under GPLv2) as `/bin/bash`: a recent one must be installed (Homebrew) and come first in PATH.
The version is read from `bash --version` rather than from a script printing `BASH_VERSINFO`,
which would run the file named by `BASH_ENV`.

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
- `PROMPT_COMMAND`, which records each prompt (see Prompts) and runs `bind "set
  enable-bracketed-paste on"`: pasted text relies on bracketed paste (see Input), which bash 4.4
  and 5.0 do not enable by default, and an inputrc can disable. bash runs `PROMPT_COMMAND` before
  each prompt, after its startup files, so the setting holds whatever they say. It is set apart
  from the other variables, as it names the tmux running the server. Rejected: an inputrc of
  rat's (`INPUTRC`), a file to maintain; typing the `bind` command in each new terminal, which
  shows on the screen and in the history, and holds only until something changes the setting.

  Limit: a startup file assigning `PROMPT_COMMAND` (rather than adding to it, as most do)
  replaces it: the inputrc or bash default applies, and no prompt is recorded. A command a
  startup file adds in front of rat's may change the exit status rat's gets. rat does not refuse
  to run then (single line inputs still work), but `CheckTerminals` detects both, for the caller
  to warn: it runs bash once as terminals start it (login, interactive, their environment, `TERM`
  and `TMUX` included, as startup files often run tmux when `TMUX` is empty), and types commands
  into it:

  1. `unset HISTFILE`: an interactive bash saves the commands it read in the history of the user
     when it exits.
  2. `(exit 7)`: a status the commands of a `PROMPT_COMMAND` are unlikely to leave, unlike 0 or 1
     (`history -a` fails with 1 once `HISTFILE` is unset).
  3. A line answering with what bracketed paste is at the prompt, as readline reports it (`bind
     -v`), the status rat's command got (`__rat_status`, which it leaves in the shell; empty if it
     did not run), and what `PROMPT_COMMAND` became, for the logs.

  Typed on its input rather than given with `-c`, the commands make bash show its prompt before
  each, running `PROMPT_COMMAND` as in a terminal: what is checked is what the prompt ends up
  with, whoever turned it on. Rejected: reading what `PROMPT_COMMAND` became, the check until
  then, which took starship for an override (measured with starship 1.26): it replaces
  `PROMPT_COMMAND` with a function of its own, which runs the previous value (and changes the
  status before). Measured with bash 4.4, 5.0 and 5.3: `PROMPT_COMMAND` runs, and `bind -v`
  answers, without a terminal. `TMUX_PANE` is left out of its environment: rat's command would
  record the prompts of the check in the pane it names, on rat's server, which `TMUX` names (a
  rat started in a terminal of another tmux inherits a `TMUX_PANE`, naming a pane of rat's server
  as well, measured). It runs bash directly: tmux 3.3 does not return the output of `run-shell`
  to the client asking for it. It is a snapshot: a startup file changed afterwards goes unnoticed
  until the next check.

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

Session and window names follow the tenant rule, held by package `tmux/names`: letters,
digits, `_` and `-`, up to 32 characters. In targets, `:` and `.` separate the session, window
and pane parts, and a leading `=` asks for an exact match: any of them in a name would change
what it targets.

Window names are unique within a session, as they are how agents find their terminals. tmux
accepts duplicates, and an exact target then fails as if the window did not exist, so rat
checks the name is free before creating a window (under a lock, the check and the creation
are atomic within rat). This only protects against rat itself: a `tmux new-window` typed in a
terminal could still create a duplicate.

### display-message never fails

`display-message` does not fail on a missing target: for a missing session it prints empty
fields, and for a missing window it silently describes the current window of the session
instead. Windows are therefore described from `list-windows`, looking the name up. Two
`display-message` are left: the readiness probe, which has no target, and the state read with a
capture, in an invocation that fails with the capture on a missing target (see Capture).

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
- the last prompt (see Prompts) and the foreground command are quoted by tmux, with the `q:`
  format modifier escaping spaces and special characters with a backslash, and unquoted by rat;
- the working directory comes last, so it needs no quoting: everything after the command is
  the path.

## Input

Agents act on a terminal, they do not run commands: they paste text and press keys, and the
input may answer a prompt ("press y to confirm") as well as start a command. rat does not know
when a command finishes in general: it knows which process is in the foreground, and when the
bash of the terminal shows its prompt again (see Prompts).

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
- `copy-mode -q`, `send-keys` and `paste-buffer` never wait. `load-buffer -` does, while
  reading its stdin, but before anything is typed: other clients may run then, not in the middle
  of the text.

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
load-buffer -b rat-input-N - ; copy-mode -q -t =session:=window ; paste-buffer -t =session:=window -b rat-input-N -r -d -p ; send-keys -t =session:=window Enter
```

- stdin is not subject to the message limit (115 KB went through unchanged in a manual test,
  64 KiB in `TestSendLargeText`), and never goes through argument parsing.
- `paste-buffer` writes the buffer to the terminal as is. `-r` keeps new lines, which tmux
  would otherwise replace by carriage returns. `-p` marks the paste, see below.
- Buffers are global to the server: each input uses its own name (a counter), so concurrent
  inputs do not paste each other's text. `-d` deletes the buffer once pasted; a failed paste
  (missing window) leaves it, holding the text of an agent, so rat deletes it.
- `load-buffer` creates no buffer from an empty input, and the paste would then fail: an empty
  text only presses Enter, if asked.
- `copy-mode -q` leaves any tmux mode, right before the paste (see Input leaves tmux modes).

Nothing is added nor removed, and Enter is only pressed when asked, as a separate key.

### A paste is all or nothing

A command ended by its context kills its tmux client. Killed while `load-buffer` still reads the
text, the invocation fails whole: nothing is pasted, and no buffer is left (measured with a
client killed after 1 MB of a stream, tmux 3.3a and 3.7c; the same stream not killed pastes all
of it). Killed once the text is read, the server holds every command of the invocation, and
`paste-buffer` writes the buffer at once. A caller canceling an input (a timeout, an agent
abandoning its call) therefore never pastes part of a text. `TestSendTextAllOrNothing` guards it.

This holds because the text is held in memory, whole: a text streamed from a source ending early
would be pasted as far as it went, `load-buffer` taking the end of its input for the end of the
text.

### Pasted as a human pastes

A terminal program receives bytes, and can not tell a paste from typing: pasting `rm -rf x`
followed by a new line into a shell would run it. Programs can ask the terminal to mark pastes
(bracketed paste): the terminal, here tmux, then wraps each paste between `ESC[200~` and
`ESC[201~`. `-p` adds these markers when the program asks for them, and only then
(`cmd-paste-buffer.c`).

bash asks for them (readline's `enable-bracketed-paste`) while it waits at its prompt: it
inserts a pasted text into its command line, new lines included, and runs nothing until Enter,
which then runs every line. The rule for agents is the one of a human paste: the text is pasted,
Enter runs it. Without `-p`, each new line would run the line before it, and a text ending with a
new line would run by itself.

As for a human, what the text does depends on the program when it arrives, which rat does not
know:

- A program that does not ask (`cat`, a `read` prompt, most scripts) reads the text as typed, a
  new line as Enter.
- bash only asks while waiting at its prompt: readline turns the mode on before displaying the
  prompt, and off before running a command. A text pasted while bash starts (a window just
  created) or runs a command waits in the terminal, marked for nobody, and bash reads it later as
  typed.
- bash before 5.1 does not ask by default (the setting exists since 4.4, off until 5.1), and an
  inputrc can turn it off: rat enforces it with `PROMPT_COMMAND` (see Terminal environment).
  bash 3.2, shipped by macOS, can not ask: rat refuses it.

Verified with bash 3.2 to 5.3 (tmux 3.5a and 3.7c), for the default setting, an inputrc turning
it off, and with and without `-p`. `TestSendTextPasted` guards the behavior at a bash prompt.

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

### Input leaves tmux modes

A human peeking at the terminals (attached to rat's tmux server) can leave a window in a tmux
mode: copy mode to scroll back (`C-b [`), clock mode (`C-b t`), the choose-tree menus (`C-b s`,
`C-b w`)… A mode is displayed over the program, and stays on once the human detaches. Input then
does not reach the program as sent (tmux 3.3a and 3.7c):

- **Text is pasted unmarked.** `paste-buffer` still writes to the program, but checks whether it
  asked for bracketed paste on the screen of the mode rather than on its own (`wp->screen` in
  `cmd-paste-buffer.c`): at a bash prompt, a multi-line text runs line by line.
- **Keys go to the mode.** Copy mode binds them to its own commands: `q` leaves it, and a key
  prompting for more (`f`, `t`, `g`) fails the whole invocation (`no current client`). The other
  modes only take keys from an attached client (`window_pane_key` in `window.c`): the keys of a
  command are dropped, and the command reports success.

Input therefore leaves them first, with `copy-mode -q`, which leaves every mode of the window
(`window_pane_reset_mode_all`), and does nothing on a window in none. It runs in the invocation
of the input, right before it: after `load-buffer`, which waits, so that no mode can be entered
in between. A human scrolling back sees the view return to the program as the agent types: the
terminals are the agents', humans peek.

Reads leave modes alone: a capture reads the screen of the program, not the view of the mode.

Rejected: reporting the mode (in the window description, then in `list_windows` and
`read_window`), or refusing input in a mode, for the caller to leave it: the caller is an agent
in the end, who would get tmux plumbing to handle, and could not leave every mode anyway, its
keys being dropped.

## Prompts

tmux knows which process is in the foreground of a terminal, not when a command finished nor how:
`bash` in the foreground also means a builtin or a loop running, or bash not having started the
command just sent yet. bash knows: it shows its prompt again once the command finished, and runs
`PROMPT_COMMAND` right before. rat's `PROMPT_COMMAND` (see Terminal environment) records each
prompt on the pane option `@rat_prompt`: the exit status of the command, and the time. The window
description reads it (`Window.Prompt`).

### The hook

- **`$?` is read first**: anything run before it changes it. A startup file putting a command of
  its own in front of rat's can change it too: `history -a` does, every status then reading 0,
  as does starship, which runs rat's command from a function of its own; direnv and bash-preexec
  keep it (measured with starship 1.26, direnv and bash-preexec on Debian 12). `CheckTerminals`
  tells whether the status reaches rat's command (see Terminal environment).
- **The first prompt of a bash records no status** (`-`): it follows the startup files, not a
  command, and their last status (1 on macOS with an empty `~/.bash_profile`) is not the agent's.
  So does the first prompt of a bash started in the terminal: the variable telling a first
  prompt is not exported.
- **tmux is reached by the client of the terminal**, the tmux running the server, by its absolute
  path: `StartServer` finds it in rat's PATH, as the server was found (`/usr/bin/tmux` on Debian,
  `/opt/homebrew/bin/tmux` with Homebrew). Rejected: `tmux` looked up at each prompt in the PATH
  of the terminal, which the agent and its startup files change: tmux missing there, or another
  one found first, would fail silently, the terminal recording no prompt anymore. The client uses
  `TMUX` and `TMUX_PANE`, which tmux sets in every terminal, and is not run without them: an agent
  unsetting `TMUX` would reach another server, and a bash run elsewhere (the startup check)
  records nothing.
- **In the background, from a subshell**: in the foreground, the tmux client would show as the
  command of the terminal while it runs (`tmux` instead of `bash`, a few milliseconds per
  prompt). A subshell rather than a job: bash would announce a job, and its end.
- **Nothing on the screen**: every output is silenced, and the time comes from `printf '%(%s)T'`,
  without starting a process, or `date` for a bash older than 4.2 started in the terminal (typing
  `bash` on macOS runs the 3.2 of the system, first in the PATH of a login shell). The cost is one
  tmux client per prompt: about 1 ms on Linux, 4 ms on macOS.

Measured with tmux 3.3a, 3.5a and 3.7c, and bash 4.4 to 5.3: a command records its status, C-c
at the prompt or on a command 130, an empty Enter keeps the previous status, each line typed while
a command runs gets a prompt of its own, a bash started in the terminal records its prompts, and
its exit status once it exits. A multi-line text pasted then run gets a single prompt with bash
5.1 and later, and one per line with 4.4 and 5.0, which show a prompt between them.

### Inputs clear it

Each input clears the option, in its own invocation, right before the text or the keys: a prompt
recorded afterwards follows the input. The option then tells, by itself, whether bash showed its
prompt since the last input, the command sent having finished, with no state in rat, not even a
marker of the input. Without it, the prompt of the previous command would read as the end of the
one just sent. While it is cleared: a command running, a text left on the command line (no
Enter), keys typed at the prompt, bash starting. Clearing an option that is not set succeeds
(measured with tmux 3.3a to 3.7c): it can not fail the input.

Limits, where the prompt recorded is not the one of the command sent:

- A text sent while a command runs is read when it ends (see Pasted as a human pastes): the
  prompt of that command is recorded after the input, then bash runs the text. An input sent
  within milliseconds of a command ending meets the same race. In both cases the foreground
  command tells: the text runs then, unless it is a bash builtin.
- bash 4.4 and 5.0 show a prompt between the lines of a multi-line text run at once: the prompt
  after its first line is recorded while the next ones run.
- Input typed by a human attached to the terminal does not go through rat: it clears nothing.

### Reading it

The option is read with the window description, quoted (`#{q:@rat_prompt}`). A command typed in
a terminal can write it, `TMUX` being kept: a value rat did not write reads as no prompt, never
as an error failing the description.

Rejected:

- The pane title, set by an escape sequence without starting a process: any program sets it
  (ssh sessions, vim), and would be taken for a prompt.
- Waiting on a tmux channel (`wait-for`) signaled by the hook, rather than reading an option:
  the channel would be named after the pane ID, which rat never uses, and an option keeps the
  status for whoever reads it later.

## Capture

A capture returns the screen as displayed, preceded on demand by rows of scrollback, with what is
needed to interpret it. How the screen is captured depends on what the terminal shows.

### The normal screen

`capture-pane -p -J`, with `-S -n` for `n` rows of scrollback above the screen.

- `-J` joins the rows tmux wrapped at the terminal width, so a long line comes back whole. It
  also keeps trailing spaces, which rat removes, with the empty lines at the end of the screen:
  an idle terminal returns its prompt, not a screen of blank lines.
- The snapshot tells how many scrollback rows it includes: the ones requested, fewer if the
  scrollback is shorter. They are rows: a line wrapped over several rows is joined, so the
  content may show fewer lines than that.
- A request beyond the scrollback starts at its first row, but tmux silently captures no
  scrollback at all for a number out of its range (a C `int`, `cmd-capture-pane.c`): rat bounds
  the request by the history limit it sets, beyond which no scrollback is kept.
- The cursor is not told: its position is in screen rows, which joined rows and removed empty
  lines make point at the wrong line, and it sits at the end of the prompt anyway.
- The size of the screen alone is told, for the caller to bound what it sends (a screen over a
  budget can then be refused rather than cut). The content can not tell it once it includes
  scrollback: a line wrapped from the scrollback onto the screen is joined into one line (a line
  of 100000 characters then comes whole, the screen included). The screen is therefore captured
  a second time, alone and unjoined, before the content: tmux prints exactly one line per row,
  empty rows included (3.3a and 3.7c), so the output splits at the height of the pane. It costs
  a screen more of output, about 5 KB. Rejected: capturing the screen alone only when needed, in
  a second invocation, which would measure another frame than the content.

### A full-screen program

Under a full-screen program (`less`, `vim`, `top`), the terminal shows its alternate screen.

- The scrollback above belongs to the terminal before the program started: included, it would
  be mistaken for the program output. It is never included then, and the snapshot says a
  full-screen program is running. What such a program displays disappears when it quits.
- The screen is captured as displayed, without `-J`: a full-screen program lays out its own
  screen, and may let the terminal wrap a row it fills (`less` does): joined, that row would
  shift every row below it. Line N of the content is row N of the screen. Empty lines at the end
  are still removed, and tmux removes trailing spaces itself without `-J`.
- The cursor is told, counted from 1 (tmux counts from 0): it tells where the input of the
  program goes (a field, a position in a file). It may be below the last line of the content,
  empty lines at the end being removed. Columns count terminal cells, a wide character taking
  two. A program may hide its cursor (`htop` does, `less` and `vim` do not), which tmux tells
  (`cursor_flag`): no position is told then, as a human sees no cursor, and the one tmux keeps
  is a leftover.

The snapshot carries these properties rather than failing (asking for scrollback under a
full-screen program is not an error): an agent reads in the result itself what it got and why,
without a failed round trip, which small models handle worst.

### State and screen, in one invocation

The state of the terminal (alternate screen, scrollback size, cursor) must describe the very
screen captured: read apart, a program starting or quitting in between would get its screen
captured the wrong way, and a cursor could come from another frame than the content, for a
program redrawing. Both are read in a single invocation, which tmux runs whole (see Input):

```
display-message -p -t T '#{alternate_on} #{history_size} #{cursor_flag} #{cursor_x} #{cursor_y} #{pane_height}' ;
if-shell -F -t T '#{alternate_on}' 'capture-pane -p -t T' 'capture-pane -p -t T ; capture-pane -p -J -S -n -t T'
```

- `if-shell -F` evaluates its condition in tmux, without running a shell, and queues the commands
  chosen right after itself (`cmd-if-shell.c`): it never waits, and the queue is drained in one
  go.
- The program output can not change the screen in between: tmux reads it in its event loop
  (`window_pane_read_callback` in `window.c`), and drains client queues between two iterations
  of that loop (`proc_loop` in `proc.c`, `server_loop` in `server.c`).
- The commands of `if-shell` are strings parsed by tmux: only the target, made of validated
  names, and a number go in them.
- For a missing window, `display-message` describes another one (see Targets and names), but the
  capture fails, and so does the invocation: its output is discarded.

Rejected: describing the window, then capturing it, in two invocations: simpler, but the
properties and the capture were a few milliseconds apart.
