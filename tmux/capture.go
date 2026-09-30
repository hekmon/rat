package tmux

import (
	"context"
	"fmt"
	"strconv"
	"strings"
)

// Snapshot is the content of a window at the time of a capture, with what is needed to interpret it.
type Snapshot struct {
	// Content is the screen as displayed, preceded by the extra lines of scrollback. Trailing
	// spaces and empty lines at the end are removed. On the normal screen, lines wrapped by the
	// terminal are joined. Under a full-screen program, rows are kept as displayed: line N of the
	// content is row N of the screen.
	Content string
	// ExtraLines is the number of scrollback rows included above the screen: the ones requested,
	// fewer if the scrollback is shorter, none for a full-screen program. They are terminal rows:
	// a line wrapped over several rows is joined, so the content may show fewer lines.
	ExtraLines int
	// FullScreen is true when a full-screen program (less, vim, top…) draws on the alternate
	// screen: the scrollback above belongs to the terminal before it started, so it is not
	// included, and what the program displays disappears when it quits.
	FullScreen bool
	// Cursor is where the cursor of a full-screen program is, telling where its input goes (a
	// field, a position in a file). It may be below the last line of the content, empty lines at
	// the end being removed. It is the zero Position on the normal screen, where joined rows and
	// removed empty lines would make it point at the wrong line (it sits at the end of the prompt
	// anyway), and when the program hides its cursor, as a human then sees none.
	Cursor Position
}

// Position is a position on the screen, counted from 1: row 1 is the first line of a full-screen
// capture. Columns count terminal cells, a wide character (CJK, emoji) taking two. The zero
// Position is no position.
type Position struct {
	Row, Column int
}

// captureStateFormat is the tmux format describing a pane in the invocation capturing it (see
// Capture): alternate screen, scrollback rows, cursor visibility, and cursor position counted
// from 0. Numbers only, separated by spaces.
const captureStateFormat = "#{alternate_on} #{history_size} #{cursor_flag} #{cursor_x} #{cursor_y}"

// Capture returns the content of window in session, as currently displayed, preceded by up to
// extraLines rows of scrollback.
// The error wraps ErrSessionNotFound or ErrWindowNotFound if they do not exist.
func (c *Controller) Capture(ctx context.Context, session, window string, extraLines int) (Snapshot, error) {
	if err := checkNames(session, window); err != nil {
		return Snapshot{}, err
	}
	tg := target(session, window)
	// tmux starts a capture beyond the scrollback at its first row, but silently captures no
	// scrollback at all for a number out of its range: bounded by the history limit, beyond which
	// no scrollback is kept.
	extraLines = max(0, min(extraLines, serverHistoryLimit))
	// -J joins wrapped lines, but also keeps trailing spaces: removed below
	normalScreen := "capture-pane -p -J -t " + tg
	if extraLines > 0 {
		// -S: first row to capture, negative numbers being the scrollback above the screen
		normalScreen += " -S " + strconv.Itoa(-extraLines)
	}
	// A single invocation, which tmux runs whole: the state describes the very screen captured,
	// and the capture is made for that state. Read apart, a cursor could come from another frame
	// than the content. if-shell -F evaluates its condition in tmux, without a shell, and runs the
	// command chosen right after. Its commands are parsed by tmux: only the target (validated
	// names) and a number go in them. For a missing window, display-message describes another one,
	// but the capture fails, and so does the invocation.
	out, err := c.run(ctx, "display-message", "-p", "-t", tg, captureStateFormat, ";",
		"if-shell", "-F", "-t", tg, "#{alternate_on}", "capture-pane -p -t "+tg, normalScreen)
	if err != nil {
		return Snapshot{}, c.windowError(ctx, fmt.Errorf("failed to capture %s:%s: %w", session, window, err), session, window)
	}
	state, content, _ := strings.Cut(out, "\n")
	snapshot, err := parseCaptureState(state, extraLines)
	if err != nil {
		return Snapshot{}, fmt.Errorf("failed to capture %s:%s: %w", session, window, err)
	}
	lines := strings.Split(content, "\n")
	for i, line := range lines {
		lines[i] = strings.TrimRight(line, " ")
	}
	snapshot.Content = strings.TrimRight(strings.Join(lines, "\n"), "\n")
	return snapshot, nil
}

// parseCaptureState returns the snapshot properties told by state (captureStateFormat), for a
// capture asking for extraLines rows of scrollback.
func parseCaptureState(state string, extraLines int) (Snapshot, error) {
	fields := strings.Split(state, " ")
	var numbers [5]int
	if len(fields) != len(numbers) {
		return Snapshot{}, fmt.Errorf("unexpected pane state %q", state)
	}
	for i, field := range fields {
		var err error
		if numbers[i], err = strconv.Atoi(field); err != nil {
			return Snapshot{}, fmt.Errorf("unexpected pane state %q: %w", state, err)
		}
	}
	alternateOn, scrollback, cursorVisible, cursorX, cursorY := numbers[0] == 1, numbers[1], numbers[2] == 1, numbers[3], numbers[4]
	if !alternateOn {
		return Snapshot{ExtraLines: min(extraLines, scrollback)}, nil
	}
	snapshot := Snapshot{FullScreen: true}
	if cursorVisible {
		snapshot.Cursor = Position{Row: cursorY + 1, Column: cursorX + 1}
	}
	return snapshot, nil
}
