# RAT: Remote Agent Terminal

Shell access for AI agents, over MCP: persistent terminals on a remote machine, which agents
drive as a human does, and which a human operator can cut at once.

## What it is

You give an agent a machine to work on. ratd runs there, and the agent reaches it through its
harness: it gets terminals on that machine, which it opens, types in and reads, as you would in
tmux over ssh, and it copies files to and from it. The terminals keep running between its calls:
it starts a build, works on something else meanwhile, and comes back to read the result. And when
you want it to stop, you stop the service: every terminal, and everything the agent started, goes
with it.

- **Persistent terminals.** Named windows running bash, which survive the restarts and context
  compactions of an agent: it finds its terminals back, with what runs in them.
- **Asynchronous by design.** Sending a command returns at once: agents start long running
  commands (builds, tests, deployments) in parallel windows, carry on, and come back to check
  them.
- **A keyboard, not a command runner.** Agents paste text as a human pastes (at a prompt,
  multi-line text waits for Enter) and press keys by name: they answer prompts and drive
  full-screen programs, as a human would.
- **Files, without a terminal.** Agents write and read whole files directly: exact content,
  nothing on a screen, and nothing unchecked nor oversized reaching their context.
- **Built to be remote.** One service per tenant, reached over HTTP with mutual TLS on every
  address, loopback included: it hands out a shell.

## Not a tmux MCP server

rat uses tmux as its terminal emulator: tmux renders what programs display, and rat reads the
rendered screen. That is all agents get from tmux. They see no tmux session, pane, option nor
command, but a few tools, each serving a purpose, which rat composes out of tmux (and out of its
own file access) rather than forwarding tmux and leaving agents to assemble it: fewer tools,
fewer options, less context spent. The tmux server is rat's own, started with no user
configuration, for the same terminals on every machine, and stopped with rat.

## Tools

ratd adds eight tools to your harness, and instructions telling agents how to use them: the
terminals are on another machine, and sending a command does not wait for it.

**Terminals**

| Tool | Parameters | What it does |
|---|---|---|
| `list_windows` | none | Lists the windows: each with its foreground command, working directory and last activity. |
| `create_window` | `name` | Opens a window running bash, in the home directory of the user running ratd. |
| `close_window` | `name` | Closes a window, ending what runs in it. |
| `send_text` | `window`, `text`, `enter` | Pastes text as a human pastes, then presses Enter if `enter` is true (required: the agent decides each time whether the text runs). |
| `send_keys` | `window`, `keys` | Presses keys by name, in order: `C-c`, `Escape`, `Up`, `Tab`… |
| `read_window` | `window`, `scrollback_rows` (optional, 0) | Returns what the window displays: its screen (200 columns, 24 rows), with rows of history above it on demand. |

**Files**, copied to and from the machine of ratd, as its user:

| Tool | Parameters | What it does |
|---|---|---|
| `write_file` | `path`, `content` | Creates or replaces a text file with exactly the content given, creating missing directories. |
| `read_file` | `path`, `start_line` (optional, 1), `max_lines` (optional, all), `line_numbers` (optional, false) | Returns whole lines of a text file, from the end with a negative `start_line`. |

Paths are absolute or start with `~/`. What `read_window` and `read_file` return is bounded by
the read budget of ratd (64 KiB by default, `--read-budget`): a longer output is read a range at
a time. `list_windows`, `read_window` and `read_file` only read; the others act on the terminals
or the files, and only `send_text` and `send_keys` reach beyond rat, through what runs in the
terminals.

## Components

- **ratd**: the MCP server, and the entry point. A standard MCP server (Streamable HTTP): a
  harness written with any MCP SDK connects to it, presenting a client certificate.
- **rat**: an adapter for the harnesses that can not present a client certificate (most of them
  today): a stdio MCP server, started by the harness, relaying to ratd.
- **rat-tool**: creates and inspects certificate bundles, and checks a running ratd.

A Go program can reach ratd directly with package `connect`, which builds the transport of the Go
MCP SDK from a client directory (package `mtls` loads it, or parses credentials from elsewhere).

## Users, tenants and sessions

rat keeps agents apart in layers, from the widest to the narrowest:

| Layer | What it is | Set by | Keeps apart |
|---|---|---|---|
| Unix user | the account ratd runs as | you, when deploying | the rest of the machine: agents can do what the user can, nothing more |
| Tenant | one ratd: its port, its bundle, its terminals | the bundle, generated for it | groups of agents, such as production and staging, or two teams |
| Session | the terminals of a client of the tenant | the client certificate | the harnesses of a tenant: a client only reaches its own session |
| Window | a terminal running bash | the agent, by name | the commands of an agent, run in parallel |

Harnesses meant to share terminals share a client certificate; harnesses meant to be apart get
one each. Neither agents nor harnesses choose their tenant or their session: the certificates
carry them.

Tenants and sessions keep agents apart from mistakes, not from each other: a command typed in a
terminal can do whatever its Unix user can, reaching the terminals and files of the other tenants
and sessions of that user. Several tenants may share a Unix user; agents that must not be able to
reach each other need a Unix user each, or better, a container or a machine each.

## Getting started

Download the three binaries from the [releases](https://github.com/hekmon/rat/releases), and
install them in the PATH: ratd on the server, rat where the harness runs, rat-tool anywhere (see
Requirements).

Generate the bundle of a tenant, with a client per harness:

```sh
rat-tool bundle generate --tenant prod --client alice --output bundle-prod
```

Copy `bundle-prod/server` to the server, and `bundle-prod/clients/alice` to the machine of the
harness, each directory whole, and to that side only: the key of a client grants a shell. Run
ratd with the server directory (as a service: see Deployment):

```sh
ratd --bundle /etc/rat/prod/server
```

Declare rat in the harness, as a stdio MCP server, with the client directory:

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

ratd is meant to run as a service, under a Unix user of its own (see Security).

### Linux, with systemd

```sh
useradd --create-home --shell /bin/bash rat
install -d -m 700 -o rat /etc/rat/prod
cp -r bundle-prod/server /etc/rat/prod/ && chown -R rat /etc/rat/prod
```

```ini
# /etc/systemd/system/ratd-prod.service
[Unit]
Description=rat: persistent terminals for agents, tenant prod
# ratd only listens: binding every interface needs no configured address
# (binding a given one would need network-online.target)
After=network.target

[Service]
# a Unix user for rat alone: what agents do is bounded by it
User=rat
ExecStart=/usr/local/bin/ratd --bundle /etc/rat/prod/server --listen :7281
# the default, and what the kill switch relies on: stopping the service kills every process
# left in its control group, detached commands (nohup, setsid, daemons) included
KillMode=control-group
# above the worst stop of ratd: 10 seconds for the calls in flight, then 7 for its terminals
TimeoutStopSec=30

[Install]
WantedBy=multi-user.target
```

```sh
systemctl enable --now ratd-prod
journalctl -u ratd-prod -f
```

- **The kill switch.** `systemctl stop ratd-prod` stops ratd, its endpoint first, the terminals
  and the commands they run, and whatever else is left in the service: commands detached from
  their terminals included (measured with systemd 252: a `nohup` command started by an agent is
  gone). Keep `KillMode=control-group`: started by hand, ratd can not stop what detached from its
  terminals.
- **No `Restart=`, on purpose.** ratd restarts its terminals when they die, and only exits when
  it can not keep them up: the service then shows as failed, for whoever monitors it.
- **One unit per tenant**, each with its bundle and port. The tenants of a user keep their agents
  apart from mistakes, not from each other (see Users, tenants and sessions).
- **Logs** go to the journal: every tool call (the client, the tool, the outcome, never the
  content of what agents type or read), the start and stop, the bundle expiry warnings.

### macOS, with launchd

Create a standard user named `rat` (System Settings, Users & Groups; not an administrator), and
install the bundle as on Linux. Then, as a daemon of the system running as that user, in
`/Library/LaunchDaemons/com.github.hekmon.rat.prod.plist`:

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
  <string>/Users/rat/Library/Logs/ratd-prod.log</string>
</dict>
</plist>
```

```sh
sudo chown root:wheel /Library/LaunchDaemons/com.github.hekmon.rat.prod.plist
sudo launchctl bootstrap system /Library/LaunchDaemons/com.github.hekmon.rat.prod.plist
tail -f /Users/rat/Library/Logs/ratd-prod.log
```

- **The kill switch is partial.** `sudo launchctl bootout system/com.github.hekmon.rat.prod`
  stops ratd, its endpoint first, the terminals and the commands they run in the foreground. But
  launchd only ends the process group of the service, not what it started: commands detached from
  their terminals (`nohup`, `setsid`, daemons) outlive it (measured: a `nohup` command started by
  an agent is left running). macOS has nothing like the control groups of systemd. With a user
  for rat alone, end them all after the service: `sudo pkill -KILL -u rat`.
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
  controlling the server. A password or a token would instead sit next to the terminals, readable
  by the agents running there.
- **Modern and fixed:** TLS 1.3 only, with keys that change at each connection (what is captured
  today can not be decrypted with a key stolen tomorrow), ECDSA P-256 certificates.
- **On every address, loopback included**: other users of the machine reach loopback too.
- **Revoking is generating a new bundle**, and deploying it: a bundle is closed, no certificate
  can be added to it nor removed from it. Certificates are valid 10 years, and every side warns
  once less than one remains.

Unlike HTTPS on the web, the client does not check the name of the host it reaches: certificates
carry no address, as both sides know each other beforehand, and presenting the certificate of the
bundle proves being its ratd, wherever it is reached (a tunnel, another address). Most harnesses
can not accept this, nor present a certificate: rat does it for them. And unlike SSH, no session
is tied to a connection: the terminals keep running whether agents are connected or not, until
the service stops.

### Deploying safely

rat is not a sandbox: terminals run as the user running ratd, and an agent can do whatever that
user can. Tenants and sessions keep agents apart from mistakes, not from a malicious agent. rat
enforces what it can (mutual TLS, bounded reads, terminals free of your configuration), and
leaves the rest to how you deploy it:

- **Avoid root.** ratd as root hands root to every agent, and to whoever holds a client key: one
  mistaken command, or an instruction an agent read in a web page or a file, and the machine is
  lost. Unless the machine is meant to be the agent's (a machine you can lose, given to it whole),
  run ratd as an unprivileged user (see Deployment).
- **A Unix user for rat, and for nothing else.** What agents do is bounded by what this user can
  do: give it no password, no login but what ratd runs, and none of your own files. Its tenants
  keep their agents apart from mistakes only (see Users, tenants and sessions).
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
  one (bundles are closed: no certificate can be added nor removed).
- **Restrict who reaches the port.** Mutual TLS refuses whoever has no client certificate, but a
  firewall letting only the machines of the agents in costs nothing.
- **Watch, read-only.** Attach to the terminals of a tenant as its user, in read-only mode:
  `sudo -u rat tmux -L rat-prod attach -r -t alice` (the socket is named after the tenant, the
  session after the client).
  The terminals are the agents': what you type would reach them, and a window you resize stays so.
- **What outlives the service.** Stopping the service stops the terminals and whatever agents
  started, detached or not (with systemd; on macOS, end what is left with `pkill -u rat`, see
  Deployment). What an agent set up to outlive it, as the user (crontab, user services,
  `~/.ssh/authorized_keys`, files), a dedicated user lets you find all at once, and cut by
  locking the account.

## Your shell configuration

Terminals run bash as a login shell, which reads the bash startup files of the user running rat
(`~/.bash_profile`, `~/.profile`…). rat enforces what it relies on, but a startup file can still
defeat it: one assigning `PROMPT_COMMAND` (rather than adding to it) turns off what makes a
pasted text wait for Enter, and each of its lines then runs as soon as it is pasted. rat checks
this when it starts and warns, but does not refuse to run. A startup file changed while rat runs
is only checked at its next start.

## Requirements

- **Go 1.27 or later**, only to build from source.
- **tmux 3.3 or later.** 3.3a (Debian 12) is the oldest version rat is tested with: older ones
  behave differently in ways rat relies on, and are not supported.
- **bash 4.4 or later**, which terminals run. macOS ships bash 3.2: install a recent one
  (`brew install bash`) and make sure it comes first in the PATH of rat.
