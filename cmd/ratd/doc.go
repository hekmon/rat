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
// One ratd process serves one tenant, named by its server certificate: it owns the tmux server of
// that tenant, and serves several MCP clients over the network (Streamable HTTP, stateless, JSON
// responses). Clients authenticate with a certificate, over mutual TLS (package mtls); harnesses
// that can not present one start rat, a stdio MCP server relaying to ratd. Each client works in
// its own tmux session, named by its certificate: agents never see sessions, and a client only
// reaches its own.
//
// # Tools
//
// Six tools act on terminals: list_windows, create_window, close_window, send_text (pasted as a
// human pastes, then Enter if asked), send_keys (by their tmux names) and read_window (the
// screen, with optional scrollback above it). Two copy files to and from the machine, which ratd
// reads and writes itself: write_file and read_file. No tool waits for a command. What they send back is checked
// and bounded by the read budget. Missing tmux sessions are created on the fly, so an agent
// closing all its windows finds a fresh main.
//
// # tmux server
//
// Ratd takes its tenant by starting the tmux server before listening, keeps it running,
// restarting it when it dies on its own, and exits when that makes no sense: a first start
// failing, or a server dying repeatedly. Stopping ratd closes its endpoint, then stops every
// terminal.
package main
