package tmux

import (
	"context"
	"errors"
	"fmt"
	"strconv"
	"strings"
	"time"
)

var (
	ErrWindowNotFound = errors.New("window not found")
)

// Window describes a terminal, as tmux sees it when asked.
type Window struct {
	Name string
	// Command is the process in the foreground of the terminal: the shell (bash) when it waits
	// for input, the running program otherwise. It does not see bash builtins and loops,
	// background jobs, nor what runs within ssh or a nested shell (which show as bash, bash and
	// ssh). It tells nothing about the exit status of the last command.
	Command string
	// Path is the working directory of the foreground process.
	Path string
	// Activity is the last time the terminal output changed.
	Activity time.Time
}

// windowFormat is the tmux format describing a window, parsed by parseWindow. Fields are
// separated by spaces: tmux replaces control characters (such as tabs) by '_' in its output, so
// the separator has to be printable. The name (validated) and the activity (a number) can not
// contain it, the command is quoted by tmux (q: escapes spaces and special characters with a
// backslash), and the path comes last so it needs no quoting.
const windowFormat = "#{window_name} #{window_activity} #{q:pane_current_command} #{pane_current_path}"

func parseWindow(line string) (w Window, err error) {
	name, rest, _ := strings.Cut(line, " ")
	activity, rest, _ := strings.Cut(rest, " ")
	command, path, ok := cutQuoted(rest)
	if !ok {
		return w, fmt.Errorf("unexpected window description %q", line)
	}
	seconds, err := strconv.ParseInt(activity, 10, 64)
	if err != nil {
		return w, fmt.Errorf("unexpected window activity in %q: %w", line, err)
	}
	return Window{Name: name, Command: command, Path: path, Activity: time.Unix(seconds, 0)}, nil
}

// cutQuoted reads a field quoted by the tmux q: format modifier (spaces and special characters
// escaped with a backslash) at the start of s: it returns the field unquoted, and what follows
// the space ending it. ok is false if no unescaped space ends it.
func cutQuoted(s string) (token, rest string, ok bool) {
	var b strings.Builder
	for i := 0; i < len(s); i++ {
		switch s[i] {
		case '\\':
			if i+1 < len(s) {
				i++
				b.WriteByte(s[i])
			}
		case ' ':
			return b.String(), s[i+1:], true
		default:
			b.WriteByte(s[i])
		}
	}
	return b.String(), "", false
}

// target is the tmux target of window in session. Both parts are exact matches ('='): tmux
// would otherwise also accept a prefix or a pattern, and pick another window than the one named.
// Windows are never targeted by their tmux ID (@window, %pane): IDs are global to the server
// and would reach windows of other sessions.
func target(session, window string) string {
	return "=" + session + ":=" + window
}

// ListWindows returns the windows of session, in tmux index order. It is not the creation order:
// a new window takes the first free index, such as one left by a closed window.
// The error wraps ErrSessionNotFound if the session does not exist.
func (c *Controller) ListWindows(ctx context.Context, session string) ([]Window, error) {
	if err := checkNames(session); err != nil {
		return nil, err
	}
	out, err := c.run(ctx, "list-windows", "-t", "="+session, "-F", windowFormat)
	if err != nil {
		return nil, c.sessionError(ctx, fmt.Errorf("failed to list windows of %s: %w", session, err), session)
	}
	var windows []Window
	for line := range strings.Lines(out) {
		w, err := parseWindow(strings.TrimSuffix(line, "\n"))
		if err != nil {
			return nil, err
		}
		windows = append(windows, w)
	}
	return windows, nil
}

// Window returns the description of window in session.
// The error wraps ErrSessionNotFound or ErrWindowNotFound if they do not exist.
func (c *Controller) Window(ctx context.Context, session, window string) (Window, error) {
	if err := checkNames(session, window); err != nil {
		return Window{}, err
	}
	// Not display-message: it never fails on a missing target, and silently describes the current
	// window of the session instead of a missing one.
	windows, err := c.ListWindows(ctx, session)
	if err != nil {
		return Window{}, err
	}
	for _, w := range windows {
		if w.Name == window {
			return w, nil
		}
	}
	return Window{}, fmt.Errorf("%w: %s:%s", ErrWindowNotFound, session, window)
}
