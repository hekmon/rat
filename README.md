# RAT: Remote Agent Terminal

Shell access for AI agents, over MCP: persistent terminals on a remote machine, which agents
drive as a human does, and which a human operator can cut at once.

> **Status:** under construction. The terminal controller is done; the MCP server, the bridge and
> the utility are specified, not implemented yet.

## What it is

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

## Components

- **ratd**: the MCP server, and the entry point. A standard MCP server (Streamable HTTP): a
  harness written with any MCP SDK connects to it, presenting a client certificate.
- **rat**: an adapter for the harnesses that can not present a client certificate (most of them
  today): a stdio MCP server, started by the harness, relaying to ratd.
- **rat-tool**: creates and inspects certificate bundles, and checks a running ratd.

## Security

Each tenant has its own certificate bundle: a server certificate naming the tenant, and a client
certificate per client, naming its session. Nothing on the server can issue a certificate.
Stopping ratd leaves no terminal and no way in; run as a service, it also stops what agents
detached from their terminals.

rat is not a sandbox: terminals run as the user running ratd, and an agent can do whatever that
user can. Tenants and sessions keep agents apart from mistakes, not from a malicious agent: run
rat under a dedicated Unix user, or in a container.

## Requirements

- **tmux 3.3 or later.** 3.3a (Debian 12) is the oldest version rat is tested with: older ones
  behave differently in ways rat relies on, and are not supported.
- **bash 4.4 or later**, which terminals run. macOS ships bash 3.2: install a recent one
  (`brew install bash`) and make sure it comes first in the PATH of rat.

## Your shell configuration

Terminals run bash as a login shell, which reads the bash startup files of the user running rat
(`~/.bash_profile`, `~/.profile`…). rat enforces what it relies on, but a startup file can still
defeat it: one assigning `PROMPT_COMMAND` (rather than adding to it) turns off what makes a
pasted text wait for Enter, and each of its lines then runs as soon as it is pasted. rat checks
this when it starts and warns, but does not refuse to run. A startup file changed while rat runs
is only checked at its next start.
