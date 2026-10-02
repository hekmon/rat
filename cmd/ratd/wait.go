package main

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"time"

	"github.com/hekmon/rat/cmd/ratd/tools"
	"github.com/hekmon/rat/tmux"

	"github.com/modelcontextprotocol/go-sdk/mcp"
)

// waitPollInterval is how often wait_window asks tmux about the window it waits on: tmux answers
// in milliseconds, and an agent learning a quarter of a second late that its command finished
// loses nothing. Faster would only load tmux: terminal UIs refresh about every 100 ms for human
// eyes, an agent reads no animation. A variable for tests.
var waitPollInterval = 250 * time.Millisecond

// quietHint is how long a window must have displayed nothing for wait_window to tell, when the
// command still runs, that it may be waiting for input. A variable for tests.
var quietHint = 10 * time.Second

// noPromptsText tells why wait_window can not wait, where the terminals record no prompt (see
// checkTerminals), and what to use instead.
const noPromptsText = "rat can not tell when a command finishes on this machine: bash startup files replace " +
	"the PROMPT_COMMAND it relies on. Check windows with list_windows and read_window instead."

// addWaitTool adds the tool waiting for the command sent last to a window of session to finish. It
// reads rat's terminals only: no open world. Where the terminals record no prompt, it is offered
// all the same, telling why it can not wait: an agent is better told than left wondering where a
// tool went.
func (d *daemon) addWaitTool(server *mcp.Server, session string) {
	description := fmt.Sprintf("Wait until the command sent last to a window finished: bash is back at its "+
		"prompt. Returns as soon as it is, or after max_seconds (%d by default, %d at most), in one line: how the "+
		"command exited, or what still runs and since when. Call it again to keep waiting, read_window to see the "+
		"screen. Not seen as finished: a program waiting for input (y/n, a password), and what runs within ssh or "+
		"an interpreter. Seen as finished at once: a command run in the background (&, nohup).",
		tools.DefaultWaitSeconds, tools.MaxWaitSeconds)
	if !d.terminals.Prompts {
		description = "Wait until the command sent last to a window finished. Unavailable: " + noPromptsText
	}
	mcp.AddTool(server, &mcp.Tool{
		Name:        tools.WaitWindow,
		InputSchema: d.inputSchemas[tools.WaitWindow],
		Description: description,
		Annotations: &mcp.ToolAnnotations{ReadOnlyHint: true, OpenWorldHint: ptr(false)},
	}, timedTool(d, session, tools.WaitWindow, func(in tools.WaitWindowInput) string { return in.Window },
		func(in tools.WaitWindowInput) time.Duration { return waitOf(in) },
		func(ctx context.Context, in tools.WaitWindowInput) result { return d.waitWindow(ctx, session, in) }))
}

// promptTool names the tool an agent waits for the prompt of a window with: wait_window where it
// is offered, read_window otherwise.
func (d *daemon) promptTool() string {
	if d.terminals.Prompts {
		return tools.WaitWindow
	}
	return tools.ReadWindow
}

// waitOf returns how long a call of wait_window with in waits at most.
func waitOf(in tools.WaitWindowInput) time.Duration {
	if in.MaxSeconds == 0 {
		return tools.DefaultWaitSeconds * time.Second
	}
	return time.Duration(min(in.MaxSeconds, tools.MaxWaitSeconds)) * time.Second
}

// waitWindow waits until bash is back at its prompt in window of session after the last input (the
// command sent finished), for the wait of in at most, and tells it in one line: how the command
// exited, or what still runs. It asks tmux every waitPollInterval rather than being told: ratd
// keeps no state, and the prompt is recorded in tmux (see tmux.Window.Prompt). A missing session
// is created, waiting on main then waiting for its first prompt. It returns at once, failing, when
// ratd stops: the door closing does not wait for it. Where the terminals record no prompt, it
// fails at once, telling why.
func (d *daemon) waitWindow(ctx context.Context, session string, in tools.WaitWindowInput) result {
	wait := waitOf(in)
	attrs := []any{"max_seconds", int(wait / time.Second)}
	if !d.terminals.Prompts {
		return result{outcome: failed, attrs: attrs, text: noPromptsText}
	}
	start := time.Now()
	deadline := time.NewTimer(wait)
	defer deadline.Stop()
	ticker := time.NewTicker(waitPollInterval)
	defer ticker.Stop()
	created := false
	for {
		w, err := d.controller.Window(ctx, session, in.Window)
		if errors.Is(err, tmux.ErrSessionNotFound) && !created {
			if _, err = d.createSession(ctx, session); err != nil {
				return withAttrs(failure(ctx, err, in.Window), attrs)
			}
			if in.Window != tmux.FirstWindow {
				return result{outcome: failed, err: tmux.ErrSessionNotFound, attrs: attrs, text: fmt.Sprintf(
					"No window %s: your terminals did not exist, main was just created. Create %s with create_window.",
					in.Window, in.Window)}
			}
			created = true
			continue
		}
		if err != nil {
			return withAttrs(failure(ctx, err, in.Window), attrs)
		}
		// bash in the foreground as well: bash 4.4 and 5.0 record a prompt after each line of a text
		// run at once, while the next ones run (see tmux.Window.Prompt)
		if !w.Prompt.Time.IsZero() && w.Command == "bash" {
			return result{text: d.finishedText(in.Window, w, time.Now()), attrs: append(attrs, "finished", true)}
		}
		select {
		case <-d.stopping:
			return result{outcome: failed, level: slog.LevelInfo, attrs: attrs,
				text: "ratd is stopping: the terminals stop with it."}
		case <-ctx.Done():
			if errors.Is(ctx.Err(), context.Canceled) {
				return result{outcome: failed, err: ctx.Err(), level: slog.LevelInfo, attrs: attrs, text: "Canceled."}
			}
			return withAttrs(failure(ctx, ctx.Err(), in.Window), attrs)
		case <-deadline.C:
			return result{text: runningText(in.Window, w, time.Since(start), time.Now()), attrs: append(attrs, "finished", false)}
		case <-ticker.C:
		}
	}
}

// withAttrs returns r with attrs, for its log line.
func withAttrs(r result, attrs []any) result {
	r.attrs = attrs
	return r
}

// finishedText tells that window w, its bash back at its prompt, finished the command sent last,
// relatively to now: how it exited, when the terminals record it right (see checkTerminals), but
// for the first prompt of a bash, which follows no command.
func (d *daemon) finishedText(window string, w tmux.Window, now time.Time) string {
	switch {
	case w.Prompt.Status == tmux.NoStatus:
		return fmt.Sprintf("%s: bash is at its prompt, waiting for input.", window)
	case d.terminals.Statuses:
		return fmt.Sprintf("%s: the command finished %s ago, exit status %d.", window, sinceText(now.Sub(w.Prompt.Time)),
			w.Prompt.Status)
	default:
		return fmt.Sprintf("%s: the command finished %s ago.", window, sinceText(now.Sub(w.Prompt.Time)))
	}
}

// runningText tells that the command sent last to window w is still running, relatively to now:
// since when, what runs in the foreground, and when nothing was displayed for quietHint, that it
// is normal, or that it may wait for input, which the screen shows. Calm on purpose: an agent told
// its command looks stuck interrupts it. bash in the foreground, with no prompt since the input,
// is a bash script or builtin running as well as a text left on the command line (see
// tmux.Window.Command): both are told, the screen telling which.
// The time is the one since the last input, as the time without output, both from the clock of
// the machine, rather than the time waited by this call: an agent adding up its waits miscounts
// them, waiting on a window several times at once, and then takes the time without output for
// running slow. Without an input recorded (none sent since the window was created), the time is
// the one waited.
func runningText(window string, w tmux.Window, waited time.Duration, now time.Time) string {
	running, finished := "still running after "+sinceText(waited), "nothing finished after "+sinceText(waited)
	noPromptSince := "since your last input"
	if !w.Input.IsZero() {
		since := sinceText(now.Sub(w.Input))
		running = fmt.Sprintf("still running, %s since your last input", since)
		finished, noPromptSince = fmt.Sprintf("nothing finished since your last input, %s ago", since), "since"
	}
	text := fmt.Sprintf("%s: %s, %s in the foreground.", window, running, w.Command)
	if w.Command == "bash" {
		text = fmt.Sprintf("%s: %s: bash has shown no prompt %s. A bash script or builtin (read) may still be "+
			"running, or a text may wait on the command line (sent without Enter): read_window shows which.", window,
			finished, noPromptSince)
	}
	if quiet := now.Sub(w.Activity); quiet >= quietHint {
		text += fmt.Sprintf(" No output for %s, which is normal for some commands: read_window shows whether it "+
			"waits for input.", sinceText(quiet))
	}
	return text
}
