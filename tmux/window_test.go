package tmux

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/hekmon/rat/tmux/names"
)

// eventually fails the test if condition does not become true within 3 seconds: terminals
// react asynchronously to input.
func eventually(t *testing.T, what string, condition func() (bool, string)) {
	t.Helper()
	var state string
	for deadline := time.Now().Add(3 * time.Second); time.Now().Before(deadline); time.Sleep(50 * time.Millisecond) {
		var ok bool
		if ok, state = condition(); ok {
			return
		}
	}
	t.Fatalf("%s: never happened, last state: %s", what, state)
}

// foregroundIs returns a condition true once command is in the foreground of window.
func foregroundIs(c *Controller, session, window, command string) func() (bool, string) {
	return func() (bool, string) {
		w, err := c.Window(context.Background(), session, window)
		if err != nil {
			return false, err.Error()
		}
		return w.Command == command, w.Command
	}
}

// TestParseWindow guards the parsing of the window format, whose prompt and command are quoted by
// tmux and whose path comes last unquoted, and the parsing of prompts: the mark of a prompt being
// recorded reads as no prompt, and so does a value rat did not write, which a command typed in a
// terminal can, rather than failing.
func TestParseWindow(t *testing.T) {
	w, err := parseWindow(`w 1700000000 1 42 0\ 1790000000 my\ prog\|x /tmp/a dir|b`)
	if err != nil {
		t.Fatal(err)
	}
	expected := Window{Name: "w", Command: "my prog|x", Path: "/tmp/a dir|b", Activity: time.Unix(1700000000, 0),
		FullScreen: true, Scrollback: 42, Prompt: Prompt{Time: time.Unix(1790000000, 0), Status: 0}}
	if w != expected {
		t.Errorf("got %+v, expected %+v", w, expected)
	}
	for _, line := range []string{"", "w", "w 1700000000 0 0", "w 1700000000 0 0 1\\ 1790000000 bash",
		"w notanumber 0 0  bash /tmp", "w 1700000000 0 x  bash /tmp"} {
		if _, err := parseWindow(line); err == nil {
			t.Errorf("%q: expected an error", line)
		}
	}
	for value, expected := range map[string]Prompt{
		`-\ 1790000000`:    {Time: time.Unix(1790000000, 0), Status: NoStatus},
		`130\ 1790000000`:  {Time: time.Unix(1790000000, 0), Status: 130},
		``:                 {},
		`garbage`:          {},
		`1\ x`:             {},
		`256\ 1790000000`:  {},
		`-1\ 1790000000`:   {},
		`1\ 1790000000\ 2`: {},
		// a prompt being recorded
		promptMark + `\ 123.4`: {},
	} {
		w, err := parseWindow("w 1700000000 0 0 " + value + " bash /tmp")
		if err != nil || w.Prompt != expected {
			t.Errorf("prompt %q: expected %+v, got %+v, %v", value, expected, w.Prompt, err)
		}
	}
}

// promptIs returns a condition true once window records a prompt following a command that exited
// with status (NoStatus for the first prompt of a bash).
func promptIs(c *Controller, session, window string, status int) func() (bool, string) {
	return func() (bool, string) {
		w, err := c.Window(context.Background(), session, window)
		if err != nil {
			return false, err.Error()
		}
		return !w.Prompt.Time.IsZero() && w.Prompt.Status == status, fmt.Sprintf("%+v", w.Prompt)
	}
}

// TestPrompt guards the prompts terminals record: the first one with no status, as it follows
// startup files, then the exit status of each command, again no status for the first prompt of a
// bash started in the terminal, its own statuses, and its exit status once it exits. A terminal
// whose PATH lost tmux records its prompts all the same: the hook runs the tmux of the server by
// its path. A value rat did not write, which a command typed in a terminal can, does not fail the
// description.
func TestPrompt(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	c := startTestServer(t, "prompt")
	ctx := context.Background()
	if err := c.NewSession(ctx, "s"); err != nil {
		t.Fatal(err)
	}
	eventually(t, "first prompt", promptIs(c, "s", FirstWindow, NoStatus))
	for _, step := range []struct {
		command string
		status  int
	}{
		{"false", 1}, {"true", 0}, {"bash", NoStatus}, {"(exit 3)", 3}, {"exit 4", 4},
		// tmux out of the PATH of the terminal: the hook does not look it up there
		{"export PATH=/nonexistent; (exit 5)", 5},
	} {
		typeCommand(t, c, "s", FirstWindow, step.command)
		eventually(t, "prompt after "+step.command, promptIs(c, "s", FirstWindow, step.status))
	}
	if w, err := c.Window(ctx, "s", FirstWindow); err != nil || time.Since(w.Prompt.Time) > time.Minute {
		t.Errorf("expected a recent prompt, got %+v, %v", w.Prompt, err)
	}
	if _, err := c.run(ctx, "set-option", "-p", "-t", target("s", FirstWindow), promptOption, "not rat's"); err != nil {
		t.Fatal(err)
	}
	if w, err := c.Window(ctx, "s", FirstWindow); err != nil || w.Prompt != (Prompt{}) {
		t.Errorf("expected no prompt from a value rat did not write, got %+v, %v", w.Prompt, err)
	}
}

// TestListWindows guards the description of windows: a new session lists its first window, a
// shell waiting at home, the foreground command is followed, and missing targets are errors
// rather than the description of another window.
func TestListWindows(t *testing.T) {
	home, err := filepath.EvalSymlinks(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	t.Setenv("HOME", home)
	c := startTestServer(t, "listwindows")
	ctx := context.Background()
	if _, err = c.ListWindows(ctx, "s"); !errors.Is(err, ErrSessionNotFound) {
		t.Errorf("expected ErrSessionNotFound, got %v", err)
	}
	for _, session := range []string{"s", "session"} {
		if err = c.NewSession(ctx, session); err != nil {
			t.Fatal(err)
		}
	}
	// bash started: its description no longer changes by itself
	eventually(t, "first prompt", promptIs(c, "s", FirstWindow, NoStatus))
	windows, err := c.ListWindows(ctx, "s")
	if err != nil {
		t.Fatal(err)
	}
	if len(windows) != 1 {
		t.Fatalf("expected one window, got %+v", windows)
	}
	if w := windows[0]; w.Name != FirstWindow || w.Command != "bash" || w.Path != home || time.Since(w.Activity) > time.Minute ||
		w.FullScreen {
		t.Errorf("unexpected description of a new window: %+v", w)
	}
	if w, err := c.Window(ctx, "s", FirstWindow); err != nil || w != windows[0] {
		t.Errorf("expected %+v, got %+v, %v", windows[0], w, err)
	}
	// tmux display-message would describe another window of the session instead
	if w, err := c.Window(ctx, "s", "nope"); !errors.Is(err, ErrWindowNotFound) {
		t.Errorf("expected ErrWindowNotFound, got %+v, %v", w, err)
	}
	if _, err = c.Window(ctx, "nope", FirstWindow); !errors.Is(err, ErrSessionNotFound) {
		t.Errorf("expected ErrSessionNotFound, got %v", err)
	}
	// "sess" is only a prefix of "session"
	if _, err = c.ListWindows(ctx, "sess"); !errors.Is(err, ErrSessionNotFound) {
		t.Errorf("session prefix: expected ErrSessionNotFound, got %v", err)
	}
	// the foreground command is followed
	if _, err = c.run(ctx, "send-keys", "-t", "=s:="+FirstWindow, "sleep 30", "Enter"); err != nil {
		t.Fatal(err)
	}
	eventually(t, "sleep in the foreground", foregroundIs(c, "s", FirstWindow, "sleep"))
}

// TestWindowsDigitNames guards that a window name made of digits only is refused before reaching
// tmux, by every command on a window: tmux reads the window part of a target as an index first,
// even after '=', and =s:=1 reaches the window at index 1 whatever its name, here a, rather than
// the window named 1. Digits with other characters name a window as any name does.
func TestWindowsDigitNames(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	c := startTestServer(t, "windowsdigits")
	ctx := context.Background()
	if err := c.NewSession(ctx, "s"); err != nil {
		t.Fatal(err)
	}
	if err := c.NewWindow(ctx, "s", "a"); err != nil {
		t.Fatal(err)
	}
	// the pitfall, with tmux alone: a takes index 1, 1 index 2
	if _, err := c.run(ctx, "new-window", "-d", "-t", "=s:", "-n", "1"); err != nil {
		t.Fatal(err)
	}
	if name, err := c.run(ctx, "display-message", "-p", "-t", "=s:=1", "#{window_name}"); err != nil || name != "a\n" {
		t.Fatalf("expected tmux to read =s:=1 as the window at index 1, a, got %q, %v", name, err)
	}
	_, captureErr := c.Capture(ctx, "s", "1", 0)
	_, windowErr := c.Window(ctx, "s", "1")
	for command, err := range map[string]error{
		"NewWindow":  c.NewWindow(ctx, "s", "2"),
		"SendText":   c.SendText(ctx, "s", "1", "echo sent", true),
		"SendKeys":   c.SendKeys(ctx, "s", "1", "C-c"),
		"Capture":    captureErr,
		"Window":     windowErr,
		"KillWindow": c.KillWindow(ctx, "s", "1"),
	} {
		if !errors.Is(err, names.ErrInvalid) {
			t.Errorf("%s: expected names.ErrInvalid, got %v", command, err)
		}
	}
	windows, err := c.ListWindows(ctx, "s")
	if err != nil || len(windows) != 3 || windows[1].Name != "a" {
		t.Errorf("expected main, a and 1 untouched, got %+v, %v", windows, err)
	}
	for _, name := range []string{"1a", "-1"} {
		if err := c.NewWindow(ctx, "s", name); err != nil {
			t.Fatal(name, err)
		}
		if got, err := c.run(ctx, "display-message", "-p", "-t", target("s", name), "#{window_name}"); err != nil ||
			got != name+"\n" {
			t.Errorf("expected %s to reach the window named so, got %q, %v", target("s", name), got, err)
		}
	}
}

// TestForegroundBash guards that bash shows in the foreground for what bash runs, not only at its
// prompt: tmux names the process leading the foreground after its first argument, bash for a bash
// script, bash -c, and a subshell or a group in a pipeline, while they run a command. The texts of
// ratd tell agents so: bash in the foreground may be a script running.
func TestForegroundBash(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	script := filepath.Join(home, "script")
	// echo st''arted prints what its command line, on the screen, does not show
	if err := os.WriteFile(script, []byte("#!/usr/bin/env bash\necho st''arted0\nsleep 30\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	c := startTestServer(t, "foregroundbash")
	ctx := context.Background()
	if err := c.NewSession(ctx, "s"); err != nil {
		t.Fatal(err)
	}
	eventually(t, "first prompt", promptIs(c, "s", FirstWindow, NoStatus))
	for i, command := range []string{
		script,
		"bash -c 'echo st''arted1; sleep 30; true'",
		"(echo st''arted2; sleep 30; true)",
		"{ echo st''arted3; sleep 30; } | cat",
	} {
		if err := c.SendText(ctx, "s", FirstWindow, command, true); err != nil {
			t.Fatal(err)
		}
		eventually(t, command+" started", screenContains(c, "s", FirstWindow, fmt.Sprintf("started%d", i)))
		if w, err := c.Window(ctx, "s", FirstWindow); err != nil || w.Command != "bash" || w.Prompt != (Prompt{}) {
			t.Errorf("%s: expected bash in the foreground, running, got %+v, %v", command, w, err)
		}
		if err := c.SendKeys(ctx, "s", FirstWindow, "C-c"); err != nil {
			t.Fatal(err)
		}
		eventually(t, command+" interrupted", promptIs(c, "s", FirstWindow, 130))
	}
}

// TestWindows guards the windows lifecycle and its errors: unique names, exact targets, windows
// starting at home, and the session disappearing with its last window.
func TestWindows(t *testing.T) {
	home, err := filepath.EvalSymlinks(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	t.Setenv("HOME", home)
	c := startTestServer(t, "windows")
	ctx := context.Background()
	if err = c.NewWindow(ctx, "s", "w"); !errors.Is(err, ErrSessionNotFound) {
		t.Errorf("expected ErrSessionNotFound, got %v", err)
	}
	if err = c.NewSession(ctx, "s"); err != nil {
		t.Fatal(err)
	}
	// the session starts at home, and so do its new windows
	if _, err = c.run(ctx, "send-keys", "-t", "=s:="+FirstWindow, "cd /", "Enter"); err != nil {
		t.Fatal(err)
	}
	if err = c.NewWindow(ctx, "s", "window"); err != nil {
		t.Fatal(err)
	}
	if err = c.NewWindow(ctx, "s", "window"); !errors.Is(err, ErrWindowExists) {
		t.Errorf("expected ErrWindowExists, got %v", err)
	}
	if w, err := c.Window(ctx, "s", "window"); err != nil || w.Path != home {
		t.Errorf("expected a new window at %s, got %+v, %v", home, w, err)
	}
	// exact targets: "win" is only a prefix of "window"
	if err = c.KillWindow(ctx, "s", "win"); !errors.Is(err, ErrWindowNotFound) {
		t.Errorf("window prefix: expected ErrWindowNotFound, got %v", err)
	}
	if err = c.NewSession(ctx, "session"); err != nil {
		t.Fatal(err)
	}
	if err = c.NewWindow(ctx, "sess", "w"); !errors.Is(err, ErrSessionNotFound) {
		t.Errorf("session prefix: expected ErrSessionNotFound, got %v", err)
	}
	windows, err := c.ListWindows(ctx, "s")
	if err != nil {
		t.Fatal(err)
	}
	if len(windows) != 2 || windows[0].Name != FirstWindow || windows[1].Name != "window" {
		t.Fatalf("expected windows %s and window, got %+v", FirstWindow, windows)
	}
	// the session disappears with its last window
	for _, window := range []string{FirstWindow, "window"} {
		if err = c.KillWindow(ctx, "s", window); err != nil {
			t.Fatal(err)
		}
	}
	if _, err = c.ListWindows(ctx, "s"); !errors.Is(err, ErrSessionNotFound) {
		t.Errorf("expected ErrSessionNotFound, got %v", err)
	}
	if err = c.KillWindow(ctx, "s", FirstWindow); !errors.Is(err, ErrSessionNotFound) {
		t.Errorf("expected ErrSessionNotFound, got %v", err)
	}
}
