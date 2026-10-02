package main

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"math"
	"unicode/utf8"

	"github.com/hekmon/rat/cmd/ratd/tools"
	"github.com/hekmon/rat/tmux"

	"github.com/modelcontextprotocol/go-sdk/mcp"
)

// freshMainHeader tells an agent its session was missing, and was just created with main: it
// learns its terminals did not exist, rather than finding a lone main.
const freshMainHeader = "[your terminals did not exist: main was just created, bash is starting]"

// addScreenTools adds the tool reading the windows of session. It reads rat's terminals only: no
// open world.
func (d *daemon) addScreenTools(server *mcp.Server, session string, logger *slog.Logger) {
	mcp.AddTool(server, &mcp.Tool{
		Name:        tools.ReadWindow,
		InputSchema: d.inputSchemas[tools.ReadWindow],
		Description: fmt.Sprintf("Read what a window displays now: its screen (%d columns, %d rows), preceded by up to "+
			"scrollback_rows rows of history (0 by default). The result holds at most %s: for a long output, run the "+
			"command with | tee /tmp/name.log and use read_file.", tmux.ScreenColumns, tmux.ScreenRows,
			sizeText(d.readBudget)),
		Annotations: &mcp.ToolAnnotations{ReadOnlyHint: true, OpenWorldHint: ptr(false)},
	}, tool(logger, tools.ReadWindow, func(in tools.ReadWindowInput) string { return in.Window },
		func(ctx context.Context, in tools.ReadWindowInput) result { return d.readWindow(ctx, session, in) }))
}

// readWindow returns what window displays in session, preceded by the rows of history asked, with
// a header when there is something to say: the rows of history included, a full-screen program
// and its cursor, a cut. A missing session is created, the agent then reading its fresh main.
// The result fits the read budget: the history is cut to the end that fits, and a screen over the
// budget alone is refused rather than cut. The log line tells the rows read, never the content.
func (d *daemon) readWindow(ctx context.Context, session string, in tools.ReadWindowInput) result {
	rows := int(min(in.ScrollbackRows, math.MaxInt))
	attrs := []any{"scrollback_rows", rows}
	snapshot, err := d.controller.Capture(ctx, session, in.Window, rows)
	header := ""
	if errors.Is(err, tmux.ErrSessionNotFound) {
		if _, err = d.createSession(ctx, session); err != nil {
			return failure(ctx, err, in.Window)
		}
		if in.Window != tmux.FirstWindow {
			return result{outcome: failed, err: tmux.ErrSessionNotFound, attrs: attrs, text: fmt.Sprintf(
				"No window %s: your terminals did not exist, main was just created. Create %s with create_window.",
				in.Window, in.Window)}
		}
		// the fresh main has no history, and the header tells it all
		header = freshMainHeader
		snapshot, err = d.controller.Capture(ctx, session, in.Window, rows)
	}
	if err != nil {
		r := failure(ctx, err, in.Window)
		r.attrs = attrs
		return r
	}
	switch {
	case header != "":
	case snapshot.FullScreen:
		header = "[full-screen program: "
		if rows > 0 {
			header += "no history, it belongs to the terminal before the program started; "
		}
		if snapshot.Cursor == (tmux.Position{}) {
			header += "cursor hidden]"
		} else {
			header += fmt.Sprintf("cursor at row %d, column %d]", snapshot.Cursor.Row, snapshot.Cursor.Column)
		}
	case rows > 0:
		header = fmt.Sprintf("[history: %d rows above the screen]", snapshot.ExtraLines)
	}
	attrs = append(attrs, "history_rows", snapshot.ExtraLines)
	text, cut := withHeader(header, snapshot.Content), false
	if len(text) > d.readBudget {
		// The history is cut, keeping the end that fits: the screen, whole, and the rows closest
		// to it. Once cut, lines could be counted, not rows: the header tells no number.
		header = fmt.Sprintf("[history cut to fit the read budget (%s): tee long output to a file (command "+
			"2>&1 | tee /tmp/name.log) and use read_file]", sizeText(d.readBudget))
		room := d.readBudget - len(header) - len("\n")
		if snapshot.ExtraLines == 0 || snapshot.ScreenBytes > room {
			// A screen cut would be read as the whole screen, and one sent whole would flood the
			// context of the agent: it is refused, the agent can start over in a new window.
			attrs = append(attrs, "screen_bytes", snapshot.ScreenBytes)
			return result{outcome: failed, attrs: attrs, text: fmt.Sprintf("The screen of %s holds %s, more than the "+
				"read budget (%s): it was not sent. Close the window (close_window) and start over in a new one.",
				in.Window, sizeText(snapshot.ScreenBytes), sizeText(d.readBudget))}
		}
		text, cut = withHeader(header, tail(snapshot.Content, room)), true
	}
	return result{text: text, attrs: append(attrs, "bytes", len(text), "cut", cut)}
}

// withHeader returns content preceded by header on a line of its own, if any.
func withHeader(header, content string) string {
	if header == "" {
		return content
	}
	if content == "" {
		return header
	}
	return header + "\n" + content
}

// tail returns the end of content that fits in size bytes: its last lines whole, then the end of
// the line before them that does not fit whole, cut at a character boundary. Rather than whole
// lines only: a line joined from rows of history and of the screen (a long output with no new
// line) would take the screen away with it.
func tail(content string, size int) string {
	if len(content) <= size {
		return content
	}
	start := len(content) - size
	for start < len(content) && !utf8.RuneStart(content[start]) {
		start++
	}
	return content[start:]
}
