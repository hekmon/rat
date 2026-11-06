# RAT: Remote Agent Terminal

Shell access for AI agents, over MCP, as you get yours with ssh and tmux: persistent terminals on a
remote machine, which agents drive as a human does, and which a human operator can cut at once.

- **What it does:** [What it is](#what-it-is) · [The name](#the-name) ·
  [Not a tmux MCP server](#not-a-tmux-mcp-server) ·
  [Knowing when a command is done](#knowing-when-a-command-is-done) ·
  [What RAT takes on](#what-rat-takes-on) · [Tools](#tools)
- **Trying it:** [Components](#components) ·
  [Users, tenants and sessions](#users-tenants-and-sessions) · [Requirements](#requirements) ·
  [Getting started](#getting-started)
- **Deploying it:** [Security, in short](#security-in-short), then in the [manual](MANUAL.md):
  [Deployment](MANUAL.md#deployment) · [Security](MANUAL.md#security) ·
  [Your shell configuration](MANUAL.md#your-shell-configuration)

## What it is

You give an agent a machine to work on. ratd runs there, and the agent reaches it through its
harness: it gets terminals on that machine, which it opens, types in and reads, and it copies
files to and from it. The terminals keep running between its calls: it starts a build, works on
something else meanwhile, and comes back to read the result. And when you want it to stop, you
stop the service: every terminal goes with it, and with systemd, everything the agent started.

- **Persistent terminals.** Named windows running bash, which survive the restarts and context
  compactions of an agent: it finds its terminals back, with what runs in them.
- **Asynchronous by design.** Sending a command returns at once: agents start long running
  commands (builds, tests, deployments) in parallel windows, carry on, and come back to check
  them, or wait for them, 50 seconds at most at a time, learning how they exited.
- **A keyboard, not a command runner.** Agents paste text as a human pastes (at a prompt,
  multi-line text waits for Enter) and press keys by name: they answer prompts and drive
  full-screen programs, as a human would.
- **Files, without a terminal.** Agents write and read whole files directly: exact content,
  nothing on a screen, and nothing unchecked nor oversized reaching their context.
- **Built to be remote, and to be reached by no one else.** A service reached over HTTPS with
  mutual TLS only, on every address, loopback included. Clients get in with a key, as with ssh:
  without one, no request even reaches it, and there is no password to guess nor to steal. One
  command stops the service, its terminals and what runs in them.

Mutual TLS, a service, a Unix user of its own: none of it depends on how many people use RAT. A
machine opened to the network takes the same care for one person as for a team, and more with an
agent at the keyboard: whoever finds the port tries it, and the agent may run a mistaken command,
or an instruction it read in a web page or a file.

## The name

Yes, it is a RAT: a remote shell on your machine, driven by something you do not fully control.
But this one is yours: you install it, it listens where you tell it, admits only those you issued
a key to, runs in plain sight, and lets go of every terminal when you stop it. The other kind does
not ask.

## Not a tmux MCP server

RAT uses tmux as its terminal emulator: tmux renders what programs display, and RAT reads the
rendered screen. That is all agents get from tmux. They see no tmux session, pane, option nor
command, and need to know nothing about tmux to use RAT.

A tmux MCP server passes tmux through, and agents have to put it together themselves. To run a
command and read its output, an agent has to know that a window lives in a session, which must
exist first; that a target is written `session:window.pane`; that `send-keys` reads `Enter` or
`C-c` in its text as key names unless given `-l`; that multi-line text sent this way runs line
by line, as it is typed; and that `capture-pane` returns the visible screen only, wrapped lines
split, unless given `-S` and `-J`. Each of these is a command, a flag or a pitfall to learn, and
each mistake costs a call and some context. Knowing them all is not enough: much of what RAT
works around is not in the manual of tmux, but was met on real servers, some of it varying from
one version to the next (see [`tmux/README.md`](tmux/README.md)).

RAT takes these on, and agents get a terminal in its simplest form: a keyboard and a screen.
The result is nine tools with few parameters, explained in a few lines of instructions. Less
context goes to describing the tools, and the agent needs fewer tries to get a command right:
there is no tmux syntax to get wrong, and nothing to check after each call. The tmux server
belongs to RAT: it starts and stops with RAT.

## Knowing when a command is done

Typing a command in a remote terminal is easy. Knowing when it finished, and how, is the hard
part. With tmux passed through, an agent sleeps for a guessed time, reads the screen, guesses from
its last lines whether the prompt is back, and appends `; echo $?` to its commands to learn how
they exited, spending a call and some context on each guess. RAT asks bash, which knows: each
terminal records every prompt bash shows, with the exit status of the command before it, and
sending input clears that record, so that a prompt tells about the command sent last, never about
an older one.

**`list_windows` tells every terminal at a glance**, one line each: what runs in its foreground,
where, when it last displayed something, and once bash is back at its prompt, how the last
command exited:

```
main: bash in /home/agent, active 2m ago, last command exited with status 0 (2m ago), 35 rows of history
build: make in /home/agent/src/app, active 1s ago, 1840 rows of history
tests: bash in /home/agent/src/app, active 40s ago, last command exited with status 0, command 1 of its pipeline of 2 with 1 (statuses: 1 0) (40s ago), 412 rows of history
edit: vim in /home/agent/src/app, active 5m ago, full-screen program
```

One call tells which commands still run, which succeeded and which failed, with no screen to
read. Times are relative, as models do not know the current time, and paths absolute, ready for
`read_file`. A pipeline hides no failure: `go test ./... | tee /tmp/test.log` exits with the status
of `tee`, 0, and RAT tells that `go test` failed.

**`wait_window` returns as soon as the command finished**: as soon as bash shows its prompt
again, a quarter of a second at most after it does, rather than after a guessed sleep. It tells
how the command exited, or what still runs once its time is up:

```
build: the command finished 0s ago, exit status 2.
```

```
build: still running, 3m since your last input, make in the foreground. No output for 45s, which is normal for some commands: read_window shows whether it waits for input.
```

It waits 50 seconds at most per call, below the timeouts of harnesses: an agent waits longer by
calling it again, or does something else meanwhile. Calls made in parallel on the same window
all return together as soon as its command finishes, or each at the end of its own
`max_seconds`. The time running is counted since the agent's last input, by the clock of the
machine, rather than added up by an agent that miscounts its waits. A quiet command is told
calmly, as normal for some commands: an agent told that its command looks stuck interrupts it.

Where bash can not tell, RAT does not guess, and says so: a command run within ssh or an
interpreter shows no prompt of that bash, and one run in the background (`&`, `nohup`) is
finished for bash at once. So agents are told to run each long command in the foreground of a
window of its own: a single `list_windows` then follows them all, with no script to check jobs or
their logs.

## What RAT takes on

Beyond knowing when commands finish, RAT takes on the details an agent would otherwise handle
itself, in every session, or miss:

- **Found by name, created on first use.** Windows are named by the agent, and their session is
  created when first needed. An agent whose terminals were lost (ratd or its tmux server
  restarted) is told so, and finds a fresh `main`. Creating a window that exists tells what runs
  in it, rather than letting the agent believe it got a fresh terminal.
- **Pasted, as a human pastes.** At a prompt, a multi-line script waits on the command line, as a
  whole, until Enter: it never runs line by line as typed. Pressing Enter is a parameter the agent
  must set each time, so a text meant to answer a prompt is never run by mistake.
- **Refused rather than altered.** A text holding a control character is refused, naming it and
  the tool to use instead (`send_keys`, `write_file`), and nothing is sent. An unknown key name
  is refused, rather than typed as text.
- **A screen that reads well.** `read_window` returns the screen with wrapped lines joined, at the
  same size every time (200 columns, 24 rows), with rows of history on request. A history too
  long for the read budget is cut from the top, keeping the screen whole, and tells how to read a
  long output in full.
- **Full-screen programs too.** tmux renders what they draw, so agents drive them as a human
  does: an editor (`vim`, `nano`), a monitor (`htop`), an interactive installer or configuration
  menu (`menuconfig`), a debugger. `send_keys` presses `Escape`, arrows, `C-x` or `F10`, and
  `read_window` returns the screen as drawn, telling that a full-screen program holds it, and
  where its cursor is: the agent knows where its next keys land. `list_windows` tells which
  windows run one.
- **Files as `cp` copies them.** `write_file` writes the exact content, creating missing
  directories, and a replaced file keeps its mode and owner. `read_file` reads a range of lines,
  from the end with a negative start, and keeps out what would flood the context: a binary file,
  a device or a FIFO is refused, a file too large is told with its size and line count, and a
  range cut to the read budget tells where to continue.
- **Every answer tells what happened, and what to do next.** Errors included: an agent never
  gets a bare error code nor a tmux message to decipher, but a sentence saying what was done or
  not, why, and how to go on, naming the tool or the parameter to use. An unknown key lists the
  key names; a denied permission names the user ratd runs as; a missing window tells to create it
  (`create_window`); input sent while the terminals were restarting tells that nothing was sent,
  and to wait for the prompt before sending again. Successes say as much: `Pasted in build, Enter
  not pressed.` tells exactly what the terminal received.
- **The same terminal everywhere.** bash (whatever the login shell of the account), starting at
  home, a neutral UTF-8 locale (English messages, stable formats), no tmux configuration, and no
  pager: `git log` prints its output rather than opening `less` for the agent to quit.
- **Checked at start.** ratd checks that the bash startup files of its user keep what RAT relies
  on (the prompt hook, exit statuses, bracketed paste), and adapts its tool descriptions to what
  holds: where a `.bashrc` replaces `PROMPT_COMMAND`, `wait_window` says why it can not wait,
  rather than waiting for a prompt that never comes (see [Your shell
  configuration](MANUAL.md#your-shell-configuration)).
- **A human watching does not get in the way.** Attaching to the tmux server does not resize the
  windows, and a copy mode left on (to scroll back) is left before the agent's next input.

## Tools

ratd adds nine tools to your harness, and instructions telling agents how to use them: the
terminals are on another machine, and sending a command does not wait for it.

**Terminals**

| Tool | Parameters | What it does |
|---|---|---|
| `list_windows` | none | Lists the windows: each with its foreground command, working directory and last activity, and how its last command exited once bash is back at its prompt. |
| `create_window` | `name` | Opens a window running bash, in the home directory of the user running ratd. |
| `close_window` | `name` | Closes a window, ending what runs in it. |
| `send_text` | `window`, `text`, `enter` | Pastes text as a human pastes, then presses Enter if `enter` is true (required: the agent decides each time whether the text runs). |
| `send_keys` | `window`, `keys` | Presses keys by name, in order: `C-c`, `Escape`, `Up`, `Tab`… |
| `read_window` | `window`, `scrollback_rows` (optional, 0) | Returns what the window displays: its screen (200 columns, 24 rows), with rows of history above it on demand. |
| `wait_window` | `window`, `max_seconds` (optional, 20, 50 at most) | Waits until the command sent last finished, returning as soon as bash is back at its prompt, then tells how it exited, or what still runs and since when once `max_seconds` passed. |

**Files**, copied to and from the machine of ratd, as its user:

| Tool | Parameters | What it does |
|---|---|---|
| `write_file` | `path`, `content` | Creates or replaces a text file with exactly the content given, creating missing directories. |
| `read_file` | `path`, `start_line` (optional, 1), `max_lines` (optional, all), `line_numbers` (optional, false) | Returns whole lines of a text file, from the end with a negative `start_line`. |

Paths are absolute or start with `~/`. What `read_window` and `read_file` return is bounded by
the read budget of ratd (64 KiB by default, `--read-budget`): a longer output is read a range at
a time. `list_windows`, `read_window`, `wait_window` and `read_file` only read; the others act on
the terminals or the files, and only `send_text` and `send_keys` reach beyond RAT, through what
runs in the terminals.

## Components

- **ratd**: the MCP server, and the entry point. A standard MCP server (Streamable HTTP): a
  harness written with any MCP SDK connects to it, presenting a client certificate. It runs as a
  service rather than as a stdio server started over ssh, as remote MCP servers often are: the
  terminals outlive the connections and stop with the service, and the account running them needs
  no ssh access (see [Deployment](MANUAL.md#deployment); the alternatives are weighed in
  [`cmd/ratd/README.md`](cmd/ratd/README.md)).
- **rat**: an adapter for the harnesses that can not present a client certificate (most of them
  today): a stdio MCP server, started by the harness, relaying to ratd over HTTPS and presenting
  the client certificate in its place.
- **rat-tool**: creates and inspects certificate bundles, and checks a running ratd.

A Go program can reach ratd directly with package `connect`, which builds the transport of the Go
MCP SDK from a client directory (package `mtls` loads it, or parses credentials from elsewhere).

## Users, tenants and sessions

RAT keeps agents apart in layers, from the widest to the narrowest:

| Layer | What it is | Set by | Keeps apart |
|---|---|---|---|
| Unix user | the account ratd runs as | you, when deploying | the rest of the machine: agents can do what the user can, nothing more |
| Tenant | one ratd: its port, its bundle, its terminals | the bundle, generated for it | groups of agents, such as production and staging, or two teams |
| Session | the terminals of a client of the tenant | the client certificate | the calls of the harnesses of a tenant: each reaches its own session only, while commands typed in a terminal reach them all |
| Window | a terminal running bash | the agent, by name | the commands of an agent, run in parallel |

Harnesses meant to share terminals share a client certificate; harnesses meant to be apart get
one each. Neither agents nor harnesses choose their tenant or their session: the certificates
carry them.

Tenants and sessions keep agents apart from mistakes, not from each other: a command typed in a
terminal can do whatever its Unix user can, reaching the terminals and files of the other tenants
and sessions of that user. Several tenants may share a Unix user; agents that must not be able to
reach each other need a Unix user each, or better, a container or a machine each.

## Requirements

- **tmux 3.3 or later.** 3.3a (Debian 12) is the oldest version RAT is tested with: older ones
  behave differently in ways RAT relies on, and are not supported.
- **bash 4.4 or later**, which terminals run. macOS ships bash 3.2: install a recent one
  (`brew install bash`) and make sure it comes first in the PATH of ratd.
- **Go 1.27 or later**, only to build from source.

## Getting started

Download the three binaries from the [releases](https://github.com/hekmon/rat/releases), and
install them: ratd on the server, rat where the harness runs, rat-tool anywhere (see
Requirements). The examples install them in `/usr/local/bin`, and give their full path wherever a
program starts them, as the systemd unit below does: which binary runs does not depend on the
PATH of that program, which a harness launched from a desktop does not inherit from your shell.

Each release comes with a file of SHA-256 sums: download it next to the archives, in a directory
holding no other release, and check them before installing (`--ignore-missing` skips the archives
of the other platforms):

```sh
shasum -a 256 -c --ignore-missing rat_*_SHA256SUMS   # sha256sum on Linux, same flags
```

On Windows, with PowerShell, which has no such command:

```powershell
Get-Content rat_*_SHA256SUMS | ForEach-Object {
    $hash, $file = $_ -split '\s+', 2
    if (Test-Path $file) {
        $ok = (Get-FileHash -Algorithm SHA256 $file).Hash -eq $hash
        "${file}: $(if ($ok) { 'OK' } else { 'FAILED' })"
    }
}
```

Generate the bundle of a tenant, with a client per harness:

```sh
rat-tool bundle generate --tenant prod --client alice --client bob --output bundle-prod
```

With a PKI of your own, a sub CA of it can issue them instead (see
[A sub CA of your PKI](MANUAL.md#a-sub-ca-of-your-pki)).

Copy `bundle-prod/server` to the server, and `bundle-prod/clients/alice` and
`bundle-prod/clients/bob` to the machines of their harnesses, each directory whole, and to that
side only: a client key is what lets its holder in, and nothing else does.

Agents can do whatever the user running ratd can: give it a Unix user of its own, neither root nor
yours, and run it with the server directory (as a service: see
[Deployment](MANUAL.md#deployment)):

```sh
ratd --bundle /etc/rat/prod/server
```

Declare rat in each harness, as a stdio MCP server, with its client directory, here for alice:

```json
{
  "mcpServers": {
    "rat-prod": {
      "command": "/usr/local/bin/rat",
      "args": ["--server", "host:7281", "--bundle", "/home/you/.config/rat/prod/alice"]
    }
  }
}
```

A harness configured in TOML or YAML takes the same command and arguments, in its own format. On
Windows, give the full paths of `rat.exe` and of the client directory, backslashes doubled in JSON.

Check it works, with the same flags as rat, before the harness does:

```sh
rat-tool check --server host:7281 --bundle /home/you/.config/rat/prod/alice
```

A harness able to present a client certificate, and to accept the server without checking its
host name, can target `https://host:7281/mcp` directly, without rat.

## Security, in short

ratd hands out a shell. Who gets in is decided by mutual TLS: a client presents a certificate
issued by the CA of its tenant, a bundle generated by rat-tool, standing alone, or a sub CA of your
PKI, revoking certificates with its CRL and renewing them without a restart. There is no password
nor token to guess or steal, and a client reaches its own session only. What gets in can do
whatever the Unix user running ratd can:

- **Not a sandbox.** Run ratd under a Unix user of its own, neither root nor yours: agents do what
  that user can, nothing more. Tenants and sessions keep agents apart from mistakes, not from a
  malicious agent.
- **A kill switch.** Stopping the service stops every terminal and, with systemd, every process
  the agents started, detached ones included.

Running ratd as a service, and deploying it safely, are in the [manual](MANUAL.md):
[Deployment](MANUAL.md#deployment), [Security](MANUAL.md#security), and
[Your shell configuration](MANUAL.md#your-shell-configuration), what the terminals rely on in the
bash startup files of the user of ratd.
