package tmux

import (
	"context"
	"fmt"
	"strconv"
	"strings"
)

// Snapshot is the content of a window at the time of a capture, with what is needed to interpret it.
type Snapshot struct {
	// Content is the screen as displayed, preceded by the extra lines of scrollback. Lines wrapped
	// by the terminal are joined, trailing spaces and empty lines at the end are removed.
	Content string
	// ExtraLines is the number of scrollback rows included above the screen: the ones requested,
	// fewer if the scrollback is shorter, none for a full-screen program. They are terminal rows:
	// a line wrapped over several rows is joined, so the content may show fewer lines.
	ExtraLines int
	// FullScreen is true when a full-screen program (less, vim, top…) draws on the alternate
	// screen: the scrollback above belongs to the terminal before it started, so it is not
	// included, and what the program displays disappears when it quits.
	FullScreen bool
}

// Capture returns the content of window in session, as currently displayed, preceded by up to
// extraLines rows of scrollback.
// The error wraps ErrSessionNotFound or ErrWindowNotFound if they do not exist.
func (c *Controller) Capture(ctx context.Context, session, window string, extraLines int) (Snapshot, error) {
	// Describe the window first, to know what the capture will be made of. A program starting or
	// quitting in between would make the description outdated, as would any later output.
	w, err := c.Window(ctx, session, window)
	if err != nil {
		return Snapshot{}, err
	}
	snapshot := Snapshot{FullScreen: w.FullScreen}
	if !w.FullScreen {
		snapshot.ExtraLines = max(0, min(extraLines, w.Scrollback))
	}
	// -J joins wrapped lines, but also keeps trailing spaces: removed below
	args := []string{"capture-pane", "-p", "-J", "-t", target(session, window)}
	if snapshot.ExtraLines > 0 {
		// -S: first row to capture, negative numbers being the scrollback above the screen
		args = append(args, "-S", strconv.Itoa(-snapshot.ExtraLines))
	}
	out, err := c.run(ctx, args...)
	if err != nil {
		return Snapshot{}, c.windowError(ctx, fmt.Errorf("failed to capture %s:%s: %w", session, window, err), session, window)
	}
	lines := strings.Split(out, "\n")
	for i, line := range lines {
		lines[i] = strings.TrimRight(line, " ")
	}
	snapshot.Content = strings.TrimRight(strings.Join(lines, "\n"), "\n")
	return snapshot, nil
}
