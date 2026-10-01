package main

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"time"

	"github.com/hekmon/rat/tmux"
	"github.com/hekmon/rat/tmux/names"

	"github.com/modelcontextprotocol/go-sdk/mcp"
)

// toolTimeout bounds a tool call. No call waits for a command but wait_window, which waits on top
// of it (see timedTool), and tmux answers in milliseconds even for the largest inputs (47 ms for a
// 4 MiB paste, 11 ms for a capture of 10000 rows): beyond, tmux is stuck, and the agent is told so.
// A variable for tests.
var toolTimeout = 10 * time.Second

// nameRule tells agents what a window name may be (package names).
const nameRule = "only letters, digits, '_' and '-', up to 32 characters"

// outcome is how a tool call ended.
type outcome int

const (
	// done: the action was done
	done outcome = iota
	// message: the action was not done, which the agent may act upon (nothing to close); not an
	// error
	message
	// failed: the action was not done (IsError)
	failed
)

func (o outcome) String() string {
	switch o {
	case done:
		return "ok"
	case message:
		return "message"
	default:
		return "error"
	}
}

// result is what a tool call tells the agent, and logs.
type result struct {
	outcome outcome
	// text is what the agent reads
	text string
	// err is why the call failed, for the logs only: agents never read tmux messages
	err error
	// level is the level of the log line: warnings for what the agent did not cause
	level slog.Level
	// attrs tell the action without its content, for the log line
	attrs []any
}

// tool returns the handler of a tool of session: it runs do within toolTimeout, logs the call and
// turns its result into a tool result. window tells the window the input names, for the log line.
func tool[In any](d *daemon, session, name string, window func(In) string,
	do func(ctx context.Context, in In) result) mcp.ToolHandlerFor[In, any] {
	return timedTool(d, session, name, window, func(In) time.Duration { return 0 }, do)
}

// timedTool is tool, for a tool whose input asks to wait: do runs within toolTimeout plus what
// wait returns.
func timedTool[In any](d *daemon, session, name string, window func(In) string, wait func(In) time.Duration,
	do func(ctx context.Context, in In) result) mcp.ToolHandlerFor[In, any] {
	return func(ctx context.Context, _ *mcp.CallToolRequest, in In) (*mcp.CallToolResult, any, error) {
		start := time.Now()
		ctx, cancel := context.WithTimeout(ctx, wait(in)+toolTimeout)
		defer cancel()
		r := do(ctx, in)
		attrs := []any{"session", session, "tool", name}
		if w := window(in); w != "" {
			attrs = append(attrs, "window", w)
		}
		attrs = append(attrs, "outcome", r.outcome.String(), "duration", time.Since(start))
		attrs = append(attrs, r.attrs...)
		if r.err != nil {
			attrs = append(attrs, "error", r.err)
		}
		d.logger.Log(context.Background(), r.level, "tool", attrs...)
		return &mcp.CallToolResult{
			Content: []mcp.Content{&mcp.TextContent{Text: r.text}},
			IsError: r.outcome == failed,
		}, nil, nil
	}
}

// failure returns the result of a call on window that failed with err, phrased for the agent:
// whether and how to retry, never a tmux message nor a session name.
func failure(ctx context.Context, err error, window string) result {
	r := result{outcome: failed, err: err}
	switch {
	case errors.Is(err, names.ErrInvalid):
		r.text = fmt.Sprintf("Invalid window name %q: %s.", window, nameRule)
	case errors.Is(err, tmux.ErrWindowNotFound):
		r.text = fmt.Sprintf("No window %s: list_windows shows your windows.", window)
	case errors.Is(err, tmux.ErrServerNotRunning):
		r.text = "The terminals are restarting after a failure, and every window was lost. Retry in a few " +
			"seconds to find a fresh main."
	case errors.Is(ctx.Err(), context.DeadlineExceeded):
		r.text, r.level = "The terminals did not answer in time: retrying may work.", slog.LevelWarn
	default:
		r.text, r.level = "Internal error, logged: retrying may work.", slog.LevelWarn
	}
	return r
}

// createSession creates the tmux session of a client, with its window main. It tells whether it
// created it: a concurrent creation is not an error, the session existing all the same.
func (d *daemon) createSession(ctx context.Context, session string) (created bool, err error) {
	err = d.controller.NewSession(ctx, session)
	if errors.Is(err, tmux.ErrSessionExists) {
		return false, nil
	}
	return err == nil, err
}

// ptr returns a pointer to v, for the optional fields of the tool annotations.
func ptr[T any](v T) *T {
	return &v
}
