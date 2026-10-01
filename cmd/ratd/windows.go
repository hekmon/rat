package main

import (
	"context"
	"errors"
	"fmt"
	"os"
	"strings"
	"time"

	"github.com/hekmon/rat/cmd/ratd/tools"
	"github.com/hekmon/rat/tmux"

	"github.com/modelcontextprotocol/go-sdk/mcp"
)

// addWindowTools adds the tools listing, creating and closing the windows of session. They act on
// rat's terminals only: no open world.
func (d *daemon) addWindowTools(server *mcp.Server, session string) {
	mcp.AddTool(server, &mcp.Tool{
		Name:        tools.ListWindows,
		InputSchema: d.inputSchemas[tools.ListWindows],
		Description: d.listWindowsDescription(),
		Annotations: &mcp.ToolAnnotations{ReadOnlyHint: true, OpenWorldHint: ptr(false)},
	}, tool(d, session, tools.ListWindows, func(tools.NoInput) string { return "" },
		func(ctx context.Context, _ tools.NoInput) result { return d.listWindows(ctx, session) }))
	mcp.AddTool(server, &mcp.Tool{
		Name:        tools.CreateWindow,
		InputSchema: d.inputSchemas[tools.CreateWindow],
		Description: "Create a terminal: a window running bash, in your home directory. Windows persist across " +
			"your restarts: call list_windows first, a window named main already exists. Wait for the prompt of a " +
			"new window (read_window) before sending text to it.",
		Annotations: &mcp.ToolAnnotations{DestructiveHint: ptr(false), IdempotentHint: true, OpenWorldHint: ptr(false)},
	}, tool(d, session, tools.CreateWindow, func(in tools.NameInput) string { return in.Name },
		func(ctx context.Context, in tools.NameInput) result { return d.createWindow(ctx, session, in.Name) }))
	mcp.AddTool(server, &mcp.Tool{
		Name:        tools.CloseWindow,
		InputSchema: d.inputSchemas[tools.CloseWindow],
		Description: "Close a window, terminating what runs in it.",
		Annotations: &mcp.ToolAnnotations{DestructiveHint: ptr(true), IdempotentHint: true, OpenWorldHint: ptr(false)},
	}, tool(d, session, tools.CloseWindow, func(in tools.NameInput) string { return in.Name },
		func(ctx context.Context, in tools.NameInput) result { return d.closeWindow(ctx, session, in.Name) }))
}

// listWindowsDescription returns the description of list_windows, telling the exit status of commands
// only where the terminals record it.
func (d *daemon) listWindowsDescription() string {
	if d.terminals.Statuses {
		return "List your terminals: each window with its foreground command, working directory, last activity, " +
			"and how its last command exited once bash is back at its prompt. The cheap way to check whether a " +
			"command finished: an exit status shows it did (ssh shows as ssh even when idle, and what runs within " +
			"it shows no exit status)."
	}
	return "List your terminals: each window with its foreground command, working directory and last " +
		"activity. The cheap way to check whether a command finished: bash in the foreground means the " +
		"terminal waits for input (bash builtins and loops show as bash too, and ssh shows as ssh even when idle)."
}

// listWindows lists the windows of session, one per line. A missing session is created, the
// agent then finding its fresh main, with a header saying so.
func (d *daemon) listWindows(ctx context.Context, session string) result {
	windows, err := d.controller.ListWindows(ctx, session)
	header := ""
	if errors.Is(err, tmux.ErrSessionNotFound) {
		var created bool
		if created, err = d.createSession(ctx, session); err == nil {
			windows, err = d.controller.ListWindows(ctx, session)
		}
		if created {
			header = freshMainHeader + "\n"
		}
	}
	if err != nil {
		return failure(ctx, err, "")
	}
	now := time.Now()
	lines := make([]string, len(windows))
	for i, w := range windows {
		lines[i] = describeWindow(w, now, d.terminals.Statuses)
	}
	return result{text: header + strings.Join(lines, "\n"), attrs: []any{"windows", len(windows)}}
}

// describeWindow tells what window runs, where, and when it was last active, relatively to now:
// models do not know the current time. How the last command exited is told when statuses (the
// terminals record them right, see checkTerminals), once bash is back at its prompt after it. The
// history is only told when a capture can include it, not under a full-screen program. Paths are
// absolute, for the file tools.
func describeWindow(w tmux.Window, now time.Time, statuses bool) string {
	description := fmt.Sprintf("%s: %s in %s, active %s ago", w.Name, w.Command, w.Path, sinceText(now.Sub(w.Activity)))
	if statuses && !w.Prompt.Time.IsZero() && w.Prompt.Status != tmux.NoStatus {
		description += fmt.Sprintf(", last command exited with status %d (%s ago)", w.Prompt.Status,
			sinceText(now.Sub(w.Prompt.Time)))
	}
	switch {
	case w.FullScreen:
		description += ", full-screen program"
	case w.Scrollback > 0:
		description += fmt.Sprintf(", %d rows of history", w.Scrollback)
	}
	return description
}

// sinceText tells a duration in its largest unit: 12s, 5m, 3h, then days beyond two days.
func sinceText(d time.Duration) string {
	switch {
	case d < time.Minute:
		return fmt.Sprintf("%ds", max(0, int(d.Seconds())))
	case d < time.Hour:
		return fmt.Sprintf("%dm", int(d.Minutes()))
	case d < 48*time.Hour:
		return fmt.Sprintf("%dh", int(d.Hours()))
	default:
		return fmt.Sprintf("%dd", int(d.Hours()/24))
	}
}

// createWindow creates window name in session, creating the session first if needed: asking for
// main in a missing session is then a success, creating the session made it. An existing window is
// not recreated: the agent is told what it runs and where, not to believe it got a fresh terminal.
func (d *daemon) createWindow(ctx context.Context, session, name string) result {
	err := d.controller.NewWindow(ctx, session, name)
	if errors.Is(err, tmux.ErrSessionNotFound) {
		var created bool
		switch created, err = d.createSession(ctx, session); {
		case err != nil:
		case created && name == tmux.FirstWindow:
			// the session came with it
		default:
			err = d.controller.NewWindow(ctx, session, name)
		}
	}
	switch {
	case errors.Is(err, tmux.ErrWindowExists):
		w, err := d.controller.Window(ctx, session, name)
		if err != nil {
			return failure(ctx, err, name)
		}
		return result{outcome: message, text: fmt.Sprintf("Window %s already exists, not created: it runs %s in %s.",
			name, w.Command, w.Path)}
	case err != nil:
		return failure(ctx, err, name)
	}
	// where the controller starts terminals
	home, err := os.UserHomeDir()
	if err != nil {
		return failure(ctx, err, name)
	}
	return result{text: fmt.Sprintf("Window %s created: bash is starting in %s.", name, home)}
}

// closeWindow closes window name of session, terminating what runs in it. Nothing to close is not
// an error, and creates nothing. Closing the last window closes the session: the agent is told it
// finds a fresh main on its next call.
func (d *daemon) closeWindow(ctx context.Context, session, name string) result {
	err := d.controller.KillWindow(ctx, session, name)
	switch {
	case errors.Is(err, tmux.ErrSessionNotFound), errors.Is(err, tmux.ErrWindowNotFound):
		return result{outcome: message, text: fmt.Sprintf("No window %s: nothing to close.", name)}
	case err != nil:
		return failure(ctx, err, name)
	}
	text := fmt.Sprintf("Window %s closed.", name)
	if _, err = d.controller.ListWindows(ctx, session); errors.Is(err, tmux.ErrSessionNotFound) {
		text += " It was your last window: a fresh main will be there on your next call."
	}
	return result{text: text}
}
