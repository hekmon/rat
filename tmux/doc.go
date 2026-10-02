// Package tmux controls a dedicated tmux server giving agents persistent terminals.
//
// It is the low level layer of rat: explicit, with no automatic behavior, but opinionated for
// agent usage. It exposes what agents need rather than all of tmux, and enforces its invariants
// (exact targets, validated names, a server free of any user configuration…). The MCP server
// composes these primitives into the few tools agents see. The design decisions, and the tmux
// pitfalls they avoid, are described in the README of this package.
//
// # Server
//
// A [Controller] owns one tmux server, on a socket named after its tenant (rat-<tenant>, or rat
// for the default tenant). [Controller.StartServer] starts it in the foreground, as a child of
// rat, watches it and returns once it is ready. [Controller.StopServer] asks it to exit, then
// terminates it if needed. Stopping the server stops every terminal it runs.
// [Controller.WaitServer] tells when and why the server exited on its own, for the caller to
// restart it: the controller never does it by itself.
//
// # Sessions and windows
//
// A session groups the windows of an agent (or of an MCP client). A tmux session can not be
// empty: [Controller.NewSession] creates it with a first window named [FirstWindow], and closing
// its last window closes the session.
//
// A window is a terminal running bash, starting in the home directory of the user running rat.
// Windows are identified by their name, unique within their session: it is how agents find
// them back. Tenant, session and window names are plain names: package tmux/names holds the
// rule, for callers to check a name without tmux. [Controller.ListWindows] and
// [Controller.Window] describe them as tmux currently sees them: foreground command, working
// directory, last activity, full-screen program, size of the scrollback, the last prompt of their
// bash, with the exit status of the command before it ([Prompt]), which bash records in tmux
// itself, the time of the last input, which inputs record there too, and whether their bash ran
// the hook recording prompts ([Window].Hooked), which startup files changed since
// [Controller.CheckTerminals] can prevent. Nothing is cached: tmux is the single source of truth.
//
// # Input and capture
//
// [Controller.SendText] pastes text as is, as a human pastes, then presses Enter only if asked:
// agents may be answering a prompt rather than running a command. A text holding a control
// character but tab and new lines is refused, which [CheckText] tells without tmux: until tmux 3.6,
// one (ESC[201~) ends the paste early, and from 3.7, tmux alters them. Pasting relies on bash
// bracketed paste, and the prompts windows record on the prompt hook of bash, both of which
// startup files can defeat: [Controller.CheckTerminals] tells.
// [Controller.SendKeys] presses keys by their tmux names (C-c, Escape, Up…), which [CheckKey]
// validates without tmux. Both first leave any tmux mode (copy mode…) a human peeking at the
// terminals left the window in, where input would not reach the program as sent.
// [Controller.Capture] returns a [Snapshot] of what a window displays, with optional scrollback
// above it, and what is needed to interpret and bound it: whether a full-screen program runs,
// captured as displayed then, where its cursor is, and the size of the screen alone.
//
// # Errors
//
// Failures are reported with sentinel errors ([ErrSessionNotFound], [ErrWindowExists],
// [ErrServerNotRunning]…) for the caller to decide what to do: the controller never works
// around them. For instance, the MCP server meeting ErrSessionNotFound creates the session and
// carries on, treating ErrSessionExists as a concurrent creation rather than an error.
package tmux
