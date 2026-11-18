package main

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"os"
	"strconv"
	"strings"
	"time"
	"unicode"

	"github.com/hekmon/rat/cmd/ratd/tools"
	"github.com/hekmon/rat/tmux"

	"github.com/modelcontextprotocol/go-sdk/mcp"
)

// firstPromptWait bounds how long create_window waits for the first prompt of a window: beyond, the
// bash startup files hold it up, which the agent is told. Well within the time of a tool call. A
// variable for tests.
var firstPromptWait = 5 * time.Second

// firstPromptPoll is how often create_window asks tmux whether bash showed its first prompt, which
// takes tens of milliseconds.
const firstPromptPoll = 25 * time.Millisecond

// addWindowTools adds the tools listing, creating and closing the windows of session. They act on
// rat's terminals only: no open world.
func (d *daemon) addWindowTools(server *mcp.Server, session string, logger *slog.Logger) {
	mcp.AddTool(server, &mcp.Tool{
		Name:        tools.ListWindows,
		InputSchema: d.inputSchemas[tools.ListWindows],
		Description: d.listWindowsDescription(),
		Annotations: &mcp.ToolAnnotations{ReadOnlyHint: true, OpenWorldHint: ptr(false)},
	}, tool(logger, tools.ListWindows, func(tools.NoInput) string { return "" },
		func(ctx context.Context, _ tools.NoInput) result { return d.listWindows(ctx, session) }))
	mcp.AddTool(server, &mcp.Tool{
		Name:        tools.CreateWindow,
		InputSchema: d.inputSchemas[tools.CreateWindow],
		Description: d.createWindowDescription(),
		Annotations: &mcp.ToolAnnotations{DestructiveHint: ptr(false), IdempotentHint: true, OpenWorldHint: ptr(false)},
	}, tool(logger, tools.CreateWindow, func(in tools.NameInput) string { return in.Name },
		func(ctx context.Context, in tools.NameInput) result { return d.createWindow(ctx, session, in.Name) }))
	mcp.AddTool(server, &mcp.Tool{
		Name:        tools.CloseWindow,
		InputSchema: d.inputSchemas[tools.CloseWindow],
		Description: "Close a window, terminating what runs in it. Keeps running until you kill it: a command " +
			"detached from the window (nohup, setsid, disown), or left in the background by a script.",
		Annotations: &mcp.ToolAnnotations{DestructiveHint: ptr(true), IdempotentHint: true, OpenWorldHint: ptr(false)},
	}, tool(logger, tools.CloseWindow, func(in tools.NameInput) string { return in.Name },
		func(ctx context.Context, in tools.NameInput) result { return d.closeWindow(ctx, session, in.Name) }))
}

// createWindowDescription returns the description of create_window: a new window is ready for input
// where the terminals record prompts (see createWindow), and is to be waited for where they do not.
func (d *daemon) createWindowDescription() string {
	description := "Create a terminal: a window running bash, in your home directory, one per long running " +
		"command. Windows persist across your restarts: call list_windows first, a window named main already " +
		"exists, and close the windows you are done with (close_window)."
	if d.terminals.Prompts {
		return description + " Returns once bash shows its prompt, ready for input."
	}
	return description + " Wait for the prompt of a new window (" + d.promptTool() + ") before sending text to it."
}

// pipelineHint returns what the descriptions of the tools telling how commands exited add for
// pipelines, where the terminals record their statuses (see pipelineSentence).
func (d *daemon) pipelineHint() string {
	if !d.terminals.Pipelines {
		return ""
	}
	return " " + pipelineSentence
}

// listWindowsDescription returns the description of list_windows, telling the exit status of commands
// only where the terminals record it, and of the commands of a pipeline where they record those.
func (d *daemon) listWindowsDescription() string {
	if d.terminals.Statuses {
		return "List your terminals: each window with its foreground command, working directory, last activity, " +
			"and how its last command exited once bash is back at its prompt. The cheap way to check whether a " +
			"command finished: an exit status shows it did (ssh shows as ssh even when idle, and what runs within " +
			"it shows no exit status)." + d.pipelineHint()
	}
	return "List your terminals: each window with its foreground command, working directory and last " +
		"activity. The cheap way to check whether a command finished: bash in the foreground means the " +
		"terminal waits for input (bash scripts, builtins and loops show as bash too, and ssh shows as ssh even " +
		"when idle)."
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
		lines[i] = describeWindow(w, now, d.terminals)
	}
	return result{text: header + strings.Join(lines, "\n"), attrs: []any{"windows", len(windows)}}
}

// describeWindow tells what window runs, where, and when it was last active, relatively to now:
// models do not know the current time. How the last command exited is told where the terminals
// record it right (see checkTerminals), once bash is back at its prompt after it, with the commands
// of its pipeline that failed before the last one, whose status the pipeline has (see
// finishedText). Where they
// record prompts, a window whose bash has not run rat's hook yet is told (see notHookedText): bash
// still starting, or startup files changed since ratd started, which its check did not see. The
// history is only told when a capture can include it, not under a full-screen program. Paths are
// absolute, for the file tools. A window takes one line, whatever its directory is named (see
// oneLine).
func describeWindow(w tmux.Window, now time.Time, terminals tmux.TerminalsCheck) string {
	description := fmt.Sprintf("%s: %s in %s, active %s ago", oneLine(w.Name), oneLine(w.Command), oneLine(w.Path),
		sinceText(now.Sub(w.Activity)))
	if terminals.Prompts && !w.Hooked {
		description += ", no prompt recorded yet: bash still starting, or its startup files replace PROMPT_COMMAND"
	}
	if terminals.Statuses && !w.Prompt.Time.IsZero() && w.Prompt.Status != tmux.NoStatus {
		if stop := stopSignals[w.Prompt.Status]; stop != "" {
			description += fmt.Sprintf(", last command stopped with status %d (%s: fg resumes it, kill %%%% ends it)",
				w.Prompt.Status, stop)
		} else {
			description += fmt.Sprintf(", last command exited with status %d", w.Prompt.Status)
			if failure, failed := failedPipeline(w.Prompt); terminals.Pipelines && failed {
				description += fmt.Sprintf(", %s with %s%s (statuses: %s)", failure.commands, failure.bare, failure.note,
					failure.all)
			}
		}
		description += fmt.Sprintf(" (%s ago)", sinceText(now.Sub(w.Prompt.Time)))
	}
	switch {
	case w.FullScreen:
		description += ", full-screen program"
	case w.Scrollback > 0:
		description += fmt.Sprintf(", %d rows of history", w.Scrollback)
	}
	return description
}

// oneLine returns value as a result tells it on a line: as is, or quoted, as Go and JSON quote a
// string, when it holds a control character. A directory unpacked from an archive may be named with
// a new line, which would otherwise split the description of a window, and could mimic the
// description of another one. Quoted, the value stays exact for the agent to give back to a tool.
func oneLine(value string) string {
	if strings.ContainsFunc(value, unicode.IsControl) {
		return strconv.Quote(value)
	}
	return value
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
// A new window is returned ready for input, once its bash shows its first prompt (see
// waitFirstPrompt), where the terminals record prompts.
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
			name, oneLine(w.Command), oneLine(w.Path))}
	case err != nil:
		return failure(ctx, err, name)
	}
	// where the controller starts terminals
	home, err := os.UserHomeDir()
	if err != nil {
		return failure(ctx, err, name)
	}
	if !d.terminals.Prompts {
		return result{text: fmt.Sprintf("Window %s created: bash is starting in %s.", name, home)}
	}
	ready, err := d.waitFirstPrompt(ctx, session, name)
	switch {
	case errors.Is(err, tmux.ErrWindowNotFound), errors.Is(err, tmux.ErrSessionNotFound):
		return result{outcome: failed, err: err, text: fmt.Sprintf(
			"Window %s closed while bash was starting: its startup files may end it.", name)}
	case err != nil:
		return failure(ctx, err, name)
	case !ready:
		return result{attrs: []any{"ready", false}, text: fmt.Sprintf("Window %s created in %s, but bash has shown no "+
			"prompt yet: wait for it (%s) before sending text.", name, home, tools.WaitWindow)}
	}
	return result{attrs: []any{"ready", true}, text: fmt.Sprintf("Window %s created: bash is ready in %s.", name, home)}
}

// waitFirstPrompt waits for the bash of window name of session, just created, to show its first
// prompt, and tells whether it did: a text pasted before that is read as typed, each new line running
// the line before it, whether the agent pressed Enter or not. It gives up, telling it did not, after
// firstPromptWait, once an input reached the window, which drops the prompt being recorded, and as
// soon as ratd stops: what agents wait for never delays the stop. The error is the one of asking
// tmux about the window, which is gone when bash exited while starting.
func (d *daemon) waitFirstPrompt(ctx context.Context, session, name string) (ready bool, err error) {
	deadline := time.NewTimer(firstPromptWait)
	defer deadline.Stop()
	ticker := time.NewTicker(firstPromptPoll)
	defer ticker.Stop()
	for {
		w, err := d.controller.Window(ctx, session, name)
		switch {
		case err != nil:
			return false, err
		case !w.Prompt.Time.IsZero():
			// the prompt recorded rather than w.Hooked, which the hook sets before readline asks for
			// bracketed paste (see create_window in the README)
			return true, nil
		case !w.Input.IsZero():
			return false, nil
		}
		select {
		case <-d.stopping:
			return false, nil
		case <-ctx.Done():
			return false, nil
		case <-deadline.C:
			return false, nil
		case <-ticker.C:
		}
	}
}

// closeWindow closes window name of session, terminating what runs in it, but for what was detached
// from it (see tmux.Controller.KillWindow). Nothing to close is not an error, and creates nothing.
// Closing the last window closes the session: the agent is told it finds a fresh main on its next
// call.
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
