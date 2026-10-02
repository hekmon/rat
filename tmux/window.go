package tmux

import (
	"context"
	"errors"
	"fmt"
	"os"
	"slices"
	"strconv"
	"strings"
	"time"
)

var (
	// ErrWindowNotFound is returned by commands on a window that does not exist in its session.
	ErrWindowNotFound = errors.New("window not found")
	// ErrWindowExists is returned by NewWindow when the session already has a window of that name.
	ErrWindowExists = errors.New("window already exists")
)

// Window describes a terminal, as tmux sees it when asked.
type Window struct {
	Name string
	// Command is the process in the foreground of the terminal: the shell (bash) when it waits
	// for input, the running program otherwise. It does not see bash builtins and loops,
	// background jobs, nor what runs within ssh or a nested shell (which show as bash, bash and
	// ssh). It tells nothing about the exit status of the last command: Prompt does.
	Command string
	// Path is the working directory of the foreground process.
	Path string
	// Activity is the last time the terminal output changed.
	Activity time.Time
	// FullScreen is true when a full-screen program (less, vim, top…) draws on the alternate
	// screen of the terminal: what it displays disappears when it quits.
	FullScreen bool
	// Scrollback is the number of terminal rows kept above the screen, which a capture can
	// include (bounded by the history limit).
	Scrollback int
	// Prompt is the prompt the bash of the terminal displayed since the last input (SendText,
	// SendKeys), with the exit status of the command before it: the command sent finished. It is
	// the zero Prompt while bash displayed none since: a command running, a text left on the command
	// line, keys typed at the prompt, bash starting. Only the bash of the terminal, and the ones
	// started in it, record their prompts (see StartServer): not a program displaying a prompt of
	// its own (ssh, an interpreter). Nor a prompt displayed while a line waits on the terminal: text
	// sent with Enter while a command runs, or while bash starts, runs right after it, and the prompt
	// recorded is the one after the text. With bash 4.4 and 5.0, the lines of a text run at once
	// each get a prompt, recorded while the next ones run.
	Prompt Prompt
}

// Prompt is a prompt the bash of a terminal displayed, as it recorded it.
type Prompt struct {
	// Time is when bash displayed it, to the second. The zero Time is no prompt.
	Time time.Time
	// Status is the exit status of the command bash ran before it, NoStatus for the first prompt of
	// a bash, which follows its startup files rather than a command.
	Status int
}

// NoStatus is the Status of a Prompt following no command.
const NoStatus = -1

// windowFormat is the tmux format describing a window, parsed by parseWindow. Fields are
// separated by spaces: tmux replaces control characters (such as tabs) by '_' in its output, so
// the separator has to be printable. The name (validated) and the numbers can not contain it,
// the prompt and the command are quoted by tmux (q: escapes spaces and special characters with a
// backslash), and the path comes last so it needs no quoting.
const windowFormat = "#{window_name} #{window_activity} #{alternate_on} #{history_size} #{q:" + promptOption +
	"} #{q:pane_current_command} #{pane_current_path}"

func parseWindow(line string) (w Window, err error) {
	fields := strings.SplitN(line, " ", 5)
	if len(fields) != 5 {
		return w, fmt.Errorf("unexpected window description %q", line)
	}
	prompt, rest, ok := cutQuoted(fields[4])
	if !ok {
		return w, fmt.Errorf("unexpected window description %q", line)
	}
	command, path, ok := cutQuoted(rest)
	if !ok {
		return w, fmt.Errorf("unexpected window description %q", line)
	}
	var numbers [3]int64
	for i, field := range fields[1:4] {
		if numbers[i], err = strconv.ParseInt(field, 10, 64); err != nil {
			return w, fmt.Errorf("unexpected window description %q: %w", line, err)
		}
	}
	return Window{
		Name:       fields[0],
		Command:    command,
		Path:       path,
		Activity:   time.Unix(numbers[0], 0),
		FullScreen: numbers[1] == 1,
		Scrollback: int(numbers[2]),
		Prompt:     parsePrompt(prompt),
	}, nil
}

// parsePrompt returns the prompt recorded as value (see promptCommand), or the zero Prompt for
// anything else: the mark of a prompt still being recorded, or a value rat did not write, which a
// command typed in a terminal can, and which must not fail the description of the window.
func parsePrompt(value string) Prompt {
	statusField, timeField, _ := strings.Cut(value, " ")
	seconds, err := strconv.ParseInt(timeField, 10, 64)
	if err != nil || seconds <= 0 {
		return Prompt{}
	}
	status := NoStatus
	if statusField != "-" {
		if status, err = strconv.Atoi(statusField); err != nil || status < 0 || status > 255 {
			return Prompt{}
		}
	}
	return Prompt{Time: time.Unix(seconds, 0), Status: status}
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
// The error wraps ErrSessionNotFound if the session does not exist, and ErrServerNotRunning if the
// server shut down during the command.
func (c *Controller) ListWindows(ctx context.Context, session string) ([]Window, error) {
	if err := checkNames(session); err != nil {
		return nil, err
	}
	out, err := c.run(ctx, "list-windows", "-t", "="+session, "-F", windowFormat)
	if err != nil {
		return nil, c.sessionError(ctx, fmt.Errorf("failed to list windows of %s: %w", session, err), session)
	}
	// A session has a window at least: nothing listed means the server shut down during the
	// command, its client exiting as if the command succeeded.
	if out == "" {
		return nil, fmt.Errorf("%w: server shut down while listing the windows of %s", ErrServerNotRunning, session)
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

// NewWindow creates window in session, a terminal running bash in the home directory of the user
// running rat, and leaves the window a human may be looking at selected.
// The error wraps ErrSessionNotFound if the session does not exist, or ErrWindowExists if a window
// already has that name in the session: names are how windows are found back.
func (c *Controller) NewWindow(ctx context.Context, session, window string) error {
	if err := checkNames(session, window); err != nil {
		return err
	}
	c.windowCreation.Lock()
	defer c.windowCreation.Unlock()
	windows, err := c.ListWindows(ctx, session)
	if err != nil {
		return err
	}
	if slices.ContainsFunc(windows, func(w Window) bool { return w.Name == window }) {
		return fmt.Errorf("%w: %s:%s", ErrWindowExists, session, window)
	}
	// Like the session, the window starts at home: without -c, tmux would not use the session
	// directory but the one of the client creating the window, which is rat's.
	home, err := os.UserHomeDir()
	if err != nil {
		return fmt.Errorf("failed to create window %s:%s: %w", session, window, err)
	}
	// "session:" targets the next free index of the session, -d keeps the selected window as is
	args := append([]string{"new-window", "-d", "-t", "=" + session + ":", "-n", window, "-c", home, ";"},
		fixedSize(target(session, window))...)
	if _, err = c.run(ctx, args...); err != nil {
		return c.sessionError(ctx, fmt.Errorf("failed to create window %s:%s: %w", session, window, err), session)
	}
	return nil
}

// KillWindow destroys window in session, terminating the processes running in it. Destroying
// the last window of a session destroys the session too.
// The error wraps ErrSessionNotFound or ErrWindowNotFound if they do not exist.
func (c *Controller) KillWindow(ctx context.Context, session, window string) error {
	if err := checkNames(session, window); err != nil {
		return err
	}
	if _, err := c.run(ctx, "kill-window", "-t", target(session, window)); err != nil {
		return c.windowError(ctx, fmt.Errorf("failed to kill window %s:%s: %w", session, window, err), session, window)
	}
	return nil
}

// windowError explains why a command on window of session failed, as sessionError does.
// It returns an error wrapping ErrSessionNotFound or ErrWindowNotFound, or err as is.
func (c *Controller) windowError(ctx context.Context, err error, session, window string) error {
	if err = c.sessionError(ctx, err, session); errors.Is(err, ErrSessionNotFound) || errors.Is(err, ErrServerNotRunning) {
		return err
	}
	windows, listErr := c.ListWindows(ctx, session)
	if listErr == nil && !slices.ContainsFunc(windows, func(w Window) bool { return w.Name == window }) {
		return fmt.Errorf("%w: %s:%s", ErrWindowNotFound, session, window)
	}
	return err
}
