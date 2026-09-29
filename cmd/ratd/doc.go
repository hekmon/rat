// Ratd is the MCP server of rat: a service giving agents persistent terminals on the machine it
// runs on, built on the tmux controller (package tmux). The design decisions, and the
// alternatives they rejected, are described in the README of this command.
//
// The controller is the low level: explicit, with no automatic behavior. Ratd is the high level,
// agent facing: it composes the controller primitives so that agents see as few concepts and
// tools as possible, and handles the plumbing for them.
//
// # Service
//
// One ratd process serves one tenant: it owns the tmux server of that tenant, and serves several
// MCP clients over the network (Streamable HTTP, stateless, JSON responses). Clients
// authenticate with a bearer token. Each client works in its own tmux session, named by its
// configuration and sent with every request in an HTTP header: agents never see sessions.
//
// # Tools
//
// Five tools: list windows, create window, close window, send input (text pasted then Enter if
// asked, or keys), get content (the screen, with optional scrollback above it). Missing tmux
// sessions are created on the fly, so an agent closing all its windows finds a fresh main.
//
// # tmux server
//
// Ratd keeps its tmux server running, restarting it when it dies on its own, and exits when
// that makes no sense: a first start failing, or a server dying repeatedly. Stopping ratd stops
// every terminal.
package main
