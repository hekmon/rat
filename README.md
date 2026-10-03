# RAT: Remote Agent Terminal

Shell access for AI agents, over MCP, as you get yours with ssh and tmux: persistent terminals on a
remote machine, which agents drive as a human does, and which a human operator can cut at once.

- **What it does:** [What it is](#what-it-is) · [Not a tmux MCP server](#not-a-tmux-mcp-server) ·
  [Knowing when a command is done](#knowing-when-a-command-is-done) ·
  [What RAT takes on](#what-rat-takes-on) · [Tools](#tools)
- **Trying it:** [Components](#components) ·
  [Users, tenants and sessions](#users-tenants-and-sessions) · [Requirements](#requirements) ·
  [Getting started](#getting-started)
- **Deploying it:** [Deployment](#deployment) · [Security](#security) ·
  [Your shell configuration](#your-shell-configuration)

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
- **Built to be remote, and to be reached by no one else.** One service per tenant, reached over
  HTTPS with mutual TLS only, on every address, loopback included: without a certificate of its
  bundle, no request even reaches it, and there is no password to guess nor to steal. One command
  stops the service, its terminals and what runs in them.

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
- **The same terminal everywhere.** bash, starting at home, a neutral UTF-8 locale (English
  messages, stable formats), no tmux configuration, and no pager: `git log` prints its output
  rather than opening `less` for the agent to quit.
- **Checked at start.** ratd checks that the bash startup files of its user keep what RAT relies
  on (the prompt hook, exit statuses, bracketed paste), and adapts its tool descriptions to what
  holds: where a `.bashrc` replaces `PROMPT_COMMAND`, `wait_window` says why it can not wait,
  rather than waiting for a prompt that never comes (see Your shell configuration).
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
  no ssh access (see Deployment; the alternatives are weighed in
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
install them in the PATH: ratd on the server, rat where the harness runs, rat-tool anywhere (see
Requirements).

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

Copy `bundle-prod/server` to the server, and `bundle-prod/clients/alice` and
`bundle-prod/clients/bob` to the machines of their harnesses, each directory whole, and to that
side only: a client key is what lets its holder in, and nothing else does.

Agents can do whatever the user running ratd can: give it a Unix user of its own, neither root
nor yours, and run it with the server directory (as a service: see Deployment):

```sh
ratd --bundle /etc/rat/prod/server
```

Declare rat in each harness, as a stdio MCP server, with its client directory, here for alice:

```json
{
  "mcpServers": {
    "rat-prod": {
      "command": "rat",
      "args": ["--server", "host:7281", "--bundle", "/home/you/.config/rat/prod/alice"]
    }
  }
}
```

Check it works, with the same flags as rat, before the harness does:

```sh
rat-tool check --server host:7281 --bundle /home/you/.config/rat/prod/alice
```

A harness able to present a client certificate, and to accept the server without checking its
host name, can target `https://host:7281/mcp` directly, without rat.

## Deployment

ratd is meant to run as a service, under a Unix user of its own (see Security). Linux with systemd
is the recommended target, the only one where the kill switch is complete: stopping the service
ends every process in its control group, commands detached from their terminals included. ratd
runs on macOS too, but launchd leaves detached commands running.

### Linux, with systemd

```sh
useradd --create-home --shell /usr/sbin/nologin rat
echo 'DenyUsers rat' > /etc/ssh/sshd_config.d/rat.conf && systemctl reload sshd
install -d -m 755 /etc/rat/prod && cp -r bundle-prod/server /etc/rat/prod/
chmod 755 /etc/rat/prod/server && chown rat /etc/rat/prod/server/server.key
```

```ini
# /etc/systemd/system/ratd-prod.service
[Unit]
Description=RAT: persistent terminals for agents, tenant prod
# the address of --listen must be up when ratd binds it, or ratd exits: a VPN address needs
# the unit of the VPN before this one as well (After=wg-quick@wg0.service)
Wants=network-online.target
After=network-online.target

[Service]
# a Unix user for RAT alone: what agents do is bounded by it
User=rat
# ratd tells systemd when it serves: systemctl start waits for it, and reports a failed startup
Type=notify
ExecStart=/usr/local/bin/ratd --bundle /etc/rat/prod/server --listen :7281
# the default, and what the kill switch relies on: stopping the service kills every process
# left in its control group, detached commands (nohup, setsid, daemons) included
KillMode=control-group
# above the worst stop of ratd: 10 seconds for the calls in flight, then 7 for its terminals
TimeoutStopSec=30
# what the service may take of the machine, every process counted, terminals included: values to
# fit the machine and the work of the agents (see Resources below)
#TasksMax=2048
#MemoryMax=8G
#CPUQuota=400%

[Install]
WantedBy=multi-user.target
```

```sh
systemctl enable --now ratd-prod
journalctl -u ratd-prod -f
```

- **The only door is ratd's.** `useradd` gives the account no password (nothing matches the locked
  field it leaves: sshd refuses an empty password and a guess alike, password authentication on
  or not) and, with `--shell`, no login shell, which the terminals do not need: they run the bash
  ratd finds in its PATH, whatever the shell of the account. What closes ssh is `DenyUsers`: an
  agent can add a key to `~/.ssh/authorized_keys`, which a login shell turns into a shell, and a
  `nologin` one still into a tunnel (`ssh -N -L` needs no shell). `DenyUsers` refuses the account
  before it authenticates, key, password and tunnel alike (measured with OpenSSH 9.2 on Debian 12;
  leaving it out of an `AllowUsers` list does the same). Check with `sshd -T | grep -i denyusers`.
  Reach the account with `sudo -u rat`, as the attach command does (see Security): it runs the
  command itself, where `su -` and `sudo -i` run the login shell, and are refused.
- **The bundle belongs to root, but for its key.** ratd only reads it, and the agents run as its
  user: a bundle that user could write, an agent could replace with one of its own, whose clients
  ratd would let in at its next start (measured on Debian 12): ratd warns at startup of a bundle
  its user can write, naming the paths. The key stays the user's, ratd having to read it and
  refusing it readable by others: an agent can spoil it, and ratd then refuses to start, or copy
  it, which stopping the service does not revoke (see Security).
- **The kill switch.** `systemctl stop ratd-prod` stops ratd, its endpoint first, the terminals
  and the commands they run, and whatever else is left in the service: commands detached from
  their terminals included (measured with systemd 252: a `nohup` command started by an agent is
  gone). Keep `KillMode=control-group`: started by hand, ratd can not stop what detached from its
  terminals.
- **No `Restart=`, on purpose.** ratd restarts its terminals when they die, and only exits when
  it can not keep them up: the service then shows as failed, for whoever monitors it.
- **Resources are bounded by the service, not by ratd.** Whoever calls ratd has a shell: a loop
  of `tmux new-window`, a fork bomb or a build eating the memory gets around any count ratd could
  keep of windows or calls. The control group of the service counts every process it holds,
  terminals and detached commands included. `TasksMax=` caps its processes and threads (the
  default of systemd, 15% of the limit of the system, usually allows thousands), `MemoryMax=`
  keeps the out-of-memory killer within the service, which kills its largest process (a build,
  rather than another service of the machine), `CPUQuota=` keeps the machine responsive. Their
  values depend on the machine and the work of the agents: the unit holds them commented. At a
  limit, starting a process fails: windows and commands do not start, tool calls fail, and the
  stop still ends everything, systemd killing what is left in the group. A file operation stuck
  on a hung file system holds its content in ratd until it returns, the agent being told to check
  the file before writing again: `MemoryMax=` also bounds what retries could pile up. Rejected:
  limits in ratd (windows per session, calls at once), which a single command typed in a
  terminal gets around.
- **One unit per tenant**, each with its bundle and port. Tenants whose agents must not reach each
  other need a Unix user each (see Users, tenants and sessions).
- **Logs** go to the journal: every tool call (the client, the tool, the outcome, never the
  content of what agents type or read), the start and stop, the bundle expiry warnings. Each
  level is a priority of the journal: `journalctl -u ratd-prod -p warning` shows what needs an
  admin.

### macOS, with launchd

Create a standard user named `rat` (System Settings, Users & Groups; not an administrator), and
install the bundle as on Linux. Unlike `useradd`, this gives the user a password and a login
shell: if Remote Login is on, leave `rat` out of the users it allows (General, Sharing, Remote
Login), and take its login shell away, which the terminals do not need:
`sudo dscl . -create /Users/rat UserShell /usr/bin/false`. Then, as a daemon of the system
running as that user, in `/Library/LaunchDaemons/com.github.hekmon.rat.prod.plist`:

```xml
<?xml version="1.0" encoding="UTF-8"?>
<!DOCTYPE plist PUBLIC "-//Apple//DTD PLIST 1.0//EN" "http://www.apple.com/DTDs/PropertyList-1.0.dtd">
<plist version="1.0">
<dict>
  <key>Label</key>
  <string>com.github.hekmon.rat.prod</string>
  <key>UserName</key>
  <string>rat</string>
  <key>ProgramArguments</key>
  <array>
    <string>/usr/local/bin/ratd</string>
    <string>--bundle</string>
    <string>/etc/rat/prod/server</string>
    <string>--listen</string>
    <string>:7281</string>
  </array>
  <!-- launchd starts jobs with /usr/bin:/bin:/usr/sbin:/sbin, whose bash (3.2) ratd refuses:
       the one of Homebrew must come first. HOME is where terminals start. -->
  <key>EnvironmentVariables</key>
  <dict>
    <key>PATH</key>
    <string>/opt/homebrew/bin:/usr/local/bin:/usr/bin:/bin:/usr/sbin:/sbin</string>
    <key>HOME</key>
    <string>/Users/rat</string>
  </dict>
  <key>RunAtLoad</key>
  <true/>
  <key>StandardErrorPath</key>
  <string>/Library/Logs/rat/ratd-prod.log</string>
</dict>
</plist>
```

```sh
sudo install -d -o root -g wheel -m 755 /Library/Logs/rat
sudo touch /Library/Logs/rat/ratd-prod.log
sudo chown rat /Library/Logs/rat/ratd-prod.log
sudo chflags sappnd /Library/Logs/rat/ratd-prod.log
sudo chown root:wheel /Library/LaunchDaemons/com.github.hekmon.rat.prod.plist
sudo launchctl bootstrap system /Library/LaunchDaemons/com.github.hekmon.rat.prod.plist
tail -f /Library/Logs/rat/ratd-prod.log
```

- **The kill switch is partial.** `sudo launchctl bootout system/com.github.hekmon.rat.prod`
  stops ratd, its endpoint first, the terminals and the commands they run in the foreground. But
  launchd only ends the process group of the service, not what it started: commands detached from
  their terminals (`nohup`, `setsid`, daemons) outlive it (measured: a `nohup` command started by
  an agent is left running). macOS has nothing like the control groups of systemd. With a user
  for rat alone, end them all after the service: `sudo pkill -KILL -u rat`.
- **The log is out of the agents' reach**, as the journal is on Linux. launchd opens it as the
  user of the job, `rat`, appending to it: the file has to be that user's, or the job does not
  start. In a directory of root, with the system append-only flag (`sappnd`), which only root
  clears, agents can add lines to it, but neither rewrite, truncate, rename nor remove it. To
  rotate it, stop the service (ratd keeps writing to the file it opened), clear the flag (`sudo
  chflags nosappnd`), move the file, and create it again as above. Taken from the source of launchd
  (`launchd-842`, the last one published: the user is taken before the file is opened, with
  `O_APPEND`) and from measures of others on recent macOS, not measured with RAT.
- **No `KeepAlive`, on purpose**, as there is no `Restart=` on Linux.
- **One plist per tenant**, each with its label, bundle and port.

## Security

ratd hands out a shell: who gets in is decided by mutual TLS, and what they can do once in by the
Unix user it runs as. Stopping ratd leaves no terminal and no way in; run as a service with
systemd, it also stops what agents detached from their terminals (on macOS, see Deployment).

### Mutual TLS

TLS is what HTTPS uses: the connection is encrypted, and the client checks the certificate of the
server, to know it talks to the right one. With mutual TLS, the server checks a certificate of the
client as well: ratd only talks to clients presenting a certificate of the bundle of its tenant.
Any other connection is cut during the handshake, before a single request reaches ratd: there is
no login page, no password to guess, nothing to try.

It protects as SSH keys do, and in the same way: each side holds a private key that never leaves
it, and proves it holds it at each connection. A client key is to ratd what an SSH private key is
to a server; the bundle stands for `authorized_keys`, but closed:

- **Nothing on the server lets a client in.** ratd holds its own key and the certificate of the
  bundle's authority, which is public. The key of that authority existed only while generating
  the bundle, and was never written: no one can issue a certificate afterwards, not even someone
  controlling the server. Nor does the certificate of ratd stand for a client: each certificate
  carries one extended key usage, server or client authentication, which each side requires of
  its peer (`openssl x509 -in server.crt -noout -text` shows it). A password or an API key would
  instead travel with every request and, kept in clear, sit next to the terminals, readable by
  the agents running there.
- **Modern and fixed:** TLS 1.3 only, with keys that change at each connection (what is captured
  today can not be decrypted with a key stolen tomorrow), ECDSA P-256 certificates.
- **On every address, loopback included**: other users of the machine reach loopback too.
- **Revoking is generating a new bundle**, and deploying it: a bundle is closed, no certificate
  can be added to it nor removed from it. Certificates are valid 10 years, and every side warns
  once less than one remains. This holds for the server key too, which the agents can read (see
  Deploying safely).

Unlike HTTPS on the web, the client does not check the name of the host it reaches: certificates
carry no address, as both sides know each other beforehand, and presenting the certificate of the
bundle proves being its ratd, wherever it is reached (a tunnel, another address). Most harnesses
can not accept this, nor present a certificate: RAT does it for them. And unlike SSH, no session
is tied to a connection: the terminals keep running whether agents are connected or not, until
the service stops.

### Deploying safely

RAT is not a sandbox: terminals run as the user running ratd, and an agent can do whatever that
user can. Tenants and sessions keep agents apart from mistakes, not from a malicious agent. RAT
enforces what it can (mutual TLS, bounded reads, terminals free of your configuration), and
leaves the rest to how you deploy it:

- **Avoid root.** ratd as root hands root to every agent, and to whoever holds a client key: one
  mistaken command, or an instruction an agent read in a web page or a file, and the machine is
  lost. Unless the machine is meant to be the agent's (a machine you can lose, given to it whole),
  run ratd as an unprivileged user (see Deployment). ratd warns at startup when run as root: a
  service missing its `User=` runs silently as root otherwise.
- **A Unix user for RAT, and for nothing else.** What agents do is bounded by what this user can
  do: give it none of your own files, no password, and no login but ratd (no login shell, and
  sshd refusing it: see Deployment).
- **No sudo, or sudo for named commands only.** When agents must run privileged commands, grant
  the user these commands only, never every command (`ALL`), in a file of `/etc/sudoers.d` edited
  with `visudo -f`, which refuses a file with an error:

  ```
  rat ALL=(root) NOPASSWD: /usr/bin/systemctl restart myapp, /usr/bin/journalctl --no-pager -u myapp
  ```

  - Full paths, and the arguments in full. A wildcard (`*`) matches any arguments, spaces and
    options included: `systemctl restart *` restarts any service, and does more.
  - No command that can start another one or write any file: editors (`vim`, `nano`), `find`,
    `tar`, `cp`, `tee`, shells and interpreters, scripts the user can change. Each is root for
    whoever runs it.
  - No pager: `journalctl` or `systemctl status` open `less` under sudo (which resets the
    environment, `PAGER` included), whose `!` runs a shell as root. Grant them with
    `--no-pager`.
  - `NOPASSWD` is needed, agents typing no password, and acceptable because the list is closed.
    Check what it grants with `sudo -l -U rat`.
- **No secrets in its reach.** Agents read what the user reads: keep credentials, tokens and keys
  out of its home and environment. Keys it needs to reach other machines should be of their own,
  restricted on the other side (`authorized_keys` options, a restricted command).
- **Client keys are credentials.** A client key grants a shell as that user: give each harness its
  own (logs then tell who did what), never copy one elsewhere, and generate a new bundle to revoke
  one (bundles are closed: no certificate can be added nor removed). A leaked client key is an
  incident: whoever used it had that shell, and could copy the server key (see below) or leave
  something behind. The logs tell whether it was used, each call naming its client and the
  address it came from. Then revoking the client alone would not be enough: generate a new
  bundle, deploy it to ratd and every client, and look for what the user may have kept.
- **The server key is readable by the agents.** ratd reads it as its user, the one the terminals
  run as: an agent can copy it off the machine. It lets no one into ratd: its certificate is only
  valid for a server, and ratd only accepts certificates valid for a client (see Mutual TLS). So
  its holder can not be a man in the middle, relaying the calls of clients to ratd: only pose as
  ratd to them, wherever they reach for it, on their way to the machine or on its port once ratd
  is stopped. Their agents then read screens made up to instruct them, and what they send is
  collected. Stopping the service does not revoke the key: after an incident, or any doubt about
  what ran as that user, generate a new bundle and deploy it, to ratd and every client.
- **Choose who can reach the port.** `--listen :7281`, the default, answers on every address of
  the machine, the internet included when the machine is on it, and mutual TLS is made for
  that: whoever holds no client certificate is refused during the handshake. What stays exposed
  is the handshake itself, to a flaw yet unknown in Go's TLS or in ratd. A firewall letting only
  the machines of the agents in removes most of that exposure at no cost, and a private or VPN
  address (`--listen 10.8.0.2:7281`) all of it.
- **Watch, read-only.** Attach to the terminals of a tenant as its user, in read-only mode:
  `sudo -u rat tmux -L rat-prod attach -r -t alice` (the socket is named after the tenant, here
  `prod`, and the session after the client, here `alice`: the terminals of that client). Move
  between its windows with `C-b n`, `C-b p`, `C-b l` and `C-b 0` to `C-b 9`; read-only refuses
  the window list (`C-b w`), and scrolling back (`C-b [`) before tmux 3.7. The terminals are the
  agents': what you type would reach them, and a window you resize stays so.
- **Keep a trail.** The logs of ratd tell who called, from where, when, which tool on which
  window, never what was typed or read (see Logs in `cmd/ratd/README.md`): the transcripts of the
  harnesses hold that. The audit of the system records every process the user runs, with its
  arguments, whatever started it (scripts and detached commands included), out of the reach of
  agents. With `auditd`, in `/etc/audit/rules.d/rat.rules`:

  ```
  -a always,exit -F arch=b64 -S execve -F uid=rat -k rat
  -a always,exit -F arch=b32 -S execve -F uid=rat -k rat
  ```

  Then `ausearch -k rat -i` lists them. A command run through `sudo` runs as root: sudo logs it
  itself.
- **What outlives the service.** Stopping the service stops the terminals and whatever agents
  started, detached or not (with systemd; on macOS, end what is left with `pkill -u rat`, see
  Deployment). What an agent set up to outlive it, as the user (crontab, user services,
  `~/.ssh/authorized_keys`, files), a dedicated user lets you find all at once, and cut by
  locking the account: `usermod --expiredate 1 rat`, which expires the account itself, not only
  its password (`passwd -l`, which stops neither cron jobs nor ssh keys): cron then runs none of
  its jobs, and sshd refuses it even without `DenyUsers` (measured with Debian 12).

## Your shell configuration

Terminals run bash as a login shell, which reads the bash startup files of the user running RAT
(`~/.bash_profile`, `~/.profile`…). With a user for RAT alone (see Deployment), these startup
files are RAT's: leave them as the account was created. What makes a shell pleasant to a human has
no use for agents, and gets in their way: a prompt framework such as starship hides how commands
exited, and an alias such as `rm='rm -i'` makes a command ask a question the agent did not expect.
Running RAT as your own user brings your configuration to the terminals of the agents.

RAT enforces what it relies on, but a startup file can still defeat it: one assigning
`PROMPT_COMMAND` (rather than adding to it) removes what makes a pasted text wait for Enter, and
unless bash does it by itself (5.1 and later, without an inputrc turning it off), each line of a
pasted text then runs as soon as it is pasted. It also removes what tells RAT when a command
finished, and how. A command run before RAT's does less: one changing the exit status hides the
status (starship does, `history -a` added in front does), and any, even one keeping it (direnv),
hides the status of each command of a pipeline: `command | tee log` then tells the status of tee
only. RAT checks what the bash prompt ends up with when it starts, warns, and only tells agents
what holds, but does not refuse to run. A startup file changed while RAT runs is only checked at
its next start: meanwhile, the windows created since tell agents they record no prompt, why, and
how to fix it.
