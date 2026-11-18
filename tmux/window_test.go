package tmux

import (
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"reflect"
	"slices"
	"strconv"
	"strings"
	"syscall"
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

// TestParseWindow guards the parsing of the window format, whose name, prompt, input, command and
// path are quoted by tmux, a new line being a character like another in them, the hook marker, and
// the parsing of prompts, with the statuses of a pipeline, and inputs: the mark of a prompt being
// recorded reads as no prompt, and a value rat did not write, which a command typed in a terminal
// can, as no prompt or no input, rather than failing.
func TestParseWindow(t *testing.T) {
	// a description, ended as tmux ends it
	parse := func(description string) (Window, error) {
		w, after, err := parseWindow(description + " \n")
		if err == nil && after != "" {
			err = fmt.Errorf("unexpected %q after the description", after)
		}
		return w, err
	}
	w, err := parse(`w 1700000000 1 42 1 0\ 1790000000 1790000100 my\ prog\|x /tmp/a\ dir\|b`)
	if err != nil {
		t.Fatal(err)
	}
	expected := Window{Name: "w", Command: "my prog|x", Path: "/tmp/a dir|b", Activity: time.Unix(1700000000, 0),
		FullScreen: true, Scrollback: 42, Prompt: Prompt{Time: time.Unix(1790000000, 0), Status: 0},
		Input: time.Unix(1790000100, 0), Hooked: true}
	if !reflect.DeepEqual(w, expected) {
		t.Errorf("got %+v, expected %+v", w, expected)
	}
	// a name a command typed in a terminal gave, outside the naming rule
	for line, name := range map[string]string{`my\ build 1700000000 0 0 0   bash /tmp`: "my build",
		`it\'s\  1700000000 0 0 0   bash /tmp`: "it's ", ` 1700000000 0 0 0   bash /tmp`: ""} {
		if w, err := parse(line); err != nil || w.Name != name || w.Command != "bash" || w.Path != "/tmp" {
			t.Errorf("%q: expected the window %q, got %+v, %v", line, name, w, err)
		}
	}
	// a new line does not end a description: the space and the new line after its last value do
	windows, err := parseWindows("a\nb 1700000000 0 0 0   bash\nx /tmp/c\nd \ne 1700000000 0 0 0   bash /tmp \n")
	if err != nil || len(windows) != 2 || windows[0].Name != "a\nb" || windows[0].Command != "bash\nx" ||
		windows[0].Path != "/tmp/c\nd" || windows[1].Name != "e" || windows[1].Path != "/tmp" {
		t.Errorf("unexpected windows %+v, %v", windows, err)
	}
	for _, out := range []string{"w 1700000000 0 0 0   bash /tmp", "w 1700000000 0 0 0   bash /tmp \n" + "next",
		"w 1700000000 0 0 0   bash /tmp x\n", "w 1700000000 0 0 0   bash /tmp w 1700000000 0 0 0   bash /tmp \n"} {
		if _, err := parseWindows(out); err == nil {
			t.Errorf("%q: expected an error", out)
		}
	}
	for _, line := range []string{"", "w", "w 1700000000 0 0 0", "w 1700000000 0 0 0 1\\ 1790000000 bash",
		"w 1700000000 0 0 0  1790000100 bash", "w notanumber 0 0 0   bash /tmp", "w 1700000000 0 x 0   bash /tmp",
		"w 1700000000 0 0 x   bash /tmp"} {
		if _, err := parse(line); err == nil {
			t.Errorf("%q: expected an error", line)
		}
	}
	for value, expected := range map[string]time.Time{
		`1790000100`:    time.Unix(1790000100, 0),
		``:              {},
		`garbage`:       {},
		`0`:             {},
		`-5`:            {},
		`1790000100\ 2`: {},
	} {
		w, err := parse("w 1700000000 0 0 0  " + value + " bash /tmp")
		if err != nil || !w.Input.Equal(expected) {
			t.Errorf("input %q: expected %v, got %v, %v", value, expected, w.Input, err)
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
		// a pipeline, ending with the status
		`0\ 1790000000\ 1\ 0`:    {Time: time.Unix(1790000000, 0), Status: 0, Pipeline: []int{1, 0}},
		`0\ 1790000000\ 3\ 1\ 0`: {Time: time.Unix(1790000000, 0), Status: 0, Pipeline: []int{3, 1, 0}},
		`1\ 1790000000\ 0\ 0`:    {},
		`-\ 1790000000\ 1\ 0`:    {},
		`0\ 1790000000\ x\ 0`:    {},
		`0\ 1790000000\ 256\ 0`:  {},
		// a prompt being recorded
		promptMark + `\ 123.4`: {},
	} {
		w, err := parse("w 1700000000 0 0 0 " + value + "  bash /tmp")
		if err != nil || !reflect.DeepEqual(w.Prompt, expected) {
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
	if w, err := c.Window(ctx, "s", FirstWindow); err != nil || !reflect.DeepEqual(w.Prompt, Prompt{}) {
		t.Errorf("expected no prompt from a value rat did not write, got %+v, %v", w.Prompt, err)
	}
}

// TestPromptPipeline guards the statuses of a pipeline the prompts record, left to right, its last
// one being the status bash tells: none for a single command, nor for a pipeline negated (!), whose
// status is not its last command's, and the same whatever IFS startup files set.
func TestPromptPipeline(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	c := startTestServer(t, "pipeline")
	ctx := context.Background()
	if err := c.NewSession(ctx, "s"); err != nil {
		t.Fatal(err)
	}
	eventually(t, "first prompt", promptIs(c, "s", FirstWindow, NoStatus))
	for _, step := range []struct {
		command  string
		status   int
		pipeline []int
	}{
		{"false | true", 0, []int{1, 0}},
		{"true | (exit 3)", 3, []int{0, 3}},
		{"(exit 2) | (exit 3) | true", 0, []int{2, 3, 0}},
		{"! (exit 4) | true", 1, nil},
		{"(exit 5)", 5, nil},
		{"IFS=,; (exit 6) | true", 0, []int{6, 0}},
	} {
		typeCommand(t, c, "s", FirstWindow, step.command)
		eventually(t, "prompt after "+step.command, func() (bool, string) {
			w, err := c.Window(ctx, "s", FirstWindow)
			if err != nil {
				return false, err.Error()
			}
			return !w.Prompt.Time.IsZero() && w.Prompt.Status == step.status &&
				slices.Equal(w.Prompt.Pipeline, step.pipeline), fmt.Sprintf("%+v", w.Prompt)
		})
	}
}

// TestHooked guards that a window tells whether rat's hook ran in it: once its bash showed a
// prompt, unless its startup files replace PROMPT_COMMAND, here in a window created after they
// changed, while the windows created before stay hooked.
func TestHooked(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	c := startTestServer(t, "hooked")
	ctx := context.Background()
	if err := c.NewSession(ctx, "s"); err != nil {
		t.Fatal(err)
	}
	eventually(t, "first prompt", promptIs(c, "s", FirstWindow, NoStatus))
	if w, err := c.Window(ctx, "s", FirstWindow); err != nil || !w.Hooked {
		t.Fatalf("expected a hooked window, got %+v, %v", w, err)
	}
	if err := os.WriteFile(filepath.Join(home, ".bash_profile"), []byte(`PROMPT_COMMAND="history -a"`+"\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := c.NewWindow(ctx, "s", "w"); err != nil {
		t.Fatal(err)
	}
	// a prompt displayed after the output of a command: bash started, and showed its prompts
	typeCommand(t, c, "s", "w", "echo ready")
	eventually(t, "prompt after the command", screenContains(c, "s", "w", "\nready\n"))
	if w, err := c.Window(ctx, "s", "w"); err != nil || w.Hooked || !w.Prompt.Time.IsZero() {
		t.Errorf("expected a window neither hooked nor recording prompts, got %+v, %v", w, err)
	}
	if w, err := c.Window(ctx, "s", FirstWindow); err != nil || !w.Hooked {
		t.Errorf("expected the window created before to stay hooked, got %+v, %v", w, err)
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
	if w, err := c.Window(ctx, "s", FirstWindow); err != nil || !reflect.DeepEqual(w, windows[0]) {
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

// TestListWindowsForeignNames guards that a window a command typed in a terminal named outside the
// naming rule, as tmux accepts (tmux new-window -n 'my build'), does not fail the description of
// the windows of its session: it is listed under the name tmux holds, the other windows still
// described. No command reaches it until renamed, its name being refused before reaching tmux.
func TestListWindowsForeignNames(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	c := startTestServer(t, "windowsforeign")
	ctx := context.Background()
	if err := c.NewSession(ctx, "s"); err != nil {
		t.Fatal(err)
	}
	foreign := []string{"my build", `a\b`, "it's", "end ", ""}
	for _, name := range foreign {
		if _, err := c.run(ctx, "new-window", "-d", "-t", "=s:", "-n", name); err != nil {
			t.Fatalf("%q: %v", name, err)
		}
	}
	windows, err := c.ListWindows(ctx, "s")
	if err != nil {
		t.Fatal(err)
	}
	var listed []string
	for _, w := range windows {
		listed = append(listed, w.Name)
	}
	// The names tmux holds, unquoted, one per line: tmux 3.7c and 3.8 double the backslash of a new
	// window, which 3.3a to 3.5a keep as given.
	out, err := c.run(ctx, "list-windows", "-t", "=s", "-F", "#{window_name}")
	if err != nil {
		t.Fatal(err)
	}
	expected := strings.Split(strings.TrimSuffix(out, "\n"), "\n")
	if len(expected) != len(foreign)+1 {
		t.Fatalf("expected %d windows, tmux holds %q", len(foreign)+1, expected)
	}
	if !slices.Equal(listed, expected) {
		t.Errorf("expected the windows %q, got %q", expected, listed)
	}
	if _, err = c.Window(ctx, "s", FirstWindow); err != nil {
		t.Errorf("expected %s described, got %v", FirstWindow, err)
	}
	if err = c.KillWindow(ctx, "s", "my build"); !errors.Is(err, names.ErrInvalid) {
		t.Errorf("expected names.ErrInvalid, got %v", err)
	}
}

// TestListWindowsNewLines guards that a new line in what describes a window, which tmux prints as
// is, fails the description of no window of its session: in a working directory, kept exact, an
// option a command typed in a terminal wrote, a foreground command, and a window name, which tmux
// 3.7c and 3.8 refuse. New windows can still be created next to them.
func TestListWindowsNewLines(t *testing.T) {
	home, err := filepath.EvalSymlinks(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	t.Setenv("HOME", home)
	c := startTestServer(t, "windowsnewlines")
	ctx := context.Background()
	if err = c.NewSession(ctx, "s"); err != nil {
		t.Fatal(err)
	}
	if err = c.NewWindow(ctx, "s", "odd"); err != nil {
		t.Fatal(err)
	}
	eventually(t, "first prompt", promptIs(c, "s", "odd", NoStatus))
	if err = c.SendText(ctx, "s", "odd", `d=$'new line\nhere'; mkdir "$d"; cd "$d"`, true); err != nil {
		t.Fatal(err)
	}
	directory := filepath.Join(home, "new line\nhere")
	eventually(t, "directory entered", func() (bool, string) {
		w, err := c.Window(ctx, "s", "odd")
		if err != nil {
			return false, err.Error()
		}
		return w.Path == directory, w.Path
	})
	if w, err := c.Window(ctx, "s", FirstWindow); err != nil || w.Path != home {
		t.Errorf("expected %s described, got %+v, %v", FirstWindow, w, err)
	}
	if err = c.NewWindow(ctx, "s", "other"); err != nil {
		t.Fatalf("expected a new window, got %v", err)
	}
	if _, err = c.run(ctx, "set-option", "-p", "-t", target("s", FirstWindow), promptOption, "not\nrat's"); err != nil {
		t.Fatal(err)
	}
	if w, err := c.Window(ctx, "s", FirstWindow); err != nil || !reflect.DeepEqual(w.Prompt, Prompt{}) {
		t.Errorf("expected no prompt from a value rat did not write, got %+v, %v", w, err)
	}
	// named so on Linux, where tmux reads its arguments
	if err = c.SendText(ctx, "s", "other", `(exec -a $'new\ncommand' sleep 30)`, true); err != nil {
		t.Fatal(err)
	}
	eventually(t, "command in the foreground", func() (bool, string) {
		w, err := c.Window(ctx, "s", "other")
		if err != nil {
			return false, err.Error()
		}
		return w.Command != "bash", w.Command
	})
	const name = "new\nname"
	if _, err = c.run(ctx, "new-window", "-d", "-t", "=s:", "-n", name); err == nil {
		windows, err := c.ListWindows(ctx, "s")
		if err != nil || !slices.ContainsFunc(windows, func(w Window) bool { return w.Name == name }) {
			t.Errorf("expected a window named %q, got %+v, %v", name, windows, err)
		}
	}
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
		if w, err := c.Window(ctx, "s", FirstWindow); err != nil || w.Command != "bash" || !reflect.DeepEqual(w.Prompt, Prompt{}) {
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

// processRunning tells whether the process pid runs. A zombie does not: orphaned by its terminal
// closing, it waits for the first process of the system to reap it, which in a container may never
// come.
func processRunning(pid int) bool {
	out, err := exec.Command("ps", "-o", "stat=", "-p", strconv.Itoa(pid)).Output()
	state := strings.TrimSpace(string(out))
	return err == nil && state != "" && !strings.HasPrefix(state, "Z")
}

// TestKillWindowSurvivors guards what closing a window terminates: the command in its foreground
// and the jobs of its bash. What the hangup of its terminal does not end keeps running, as the
// description of close_window tells agents: a command ignoring it (nohup), in a session of its own
// (setsid, which macOS does not have), no longer a job of bash (disown), or left in the background
// by a script.
func TestKillWindowSurvivors(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	// tells its PID, then sleeps under it: for a bounded time, should the test be killed before its
	// cleanup
	marker := filepath.Join(home, "marker")
	if err := os.WriteFile(marker, []byte("#!/bin/sh\necho $$ > \"$1\"\nexec sleep 60\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	c := startTestServer(t, "killsurvivors")
	ctx := context.Background()
	if err := c.NewSession(ctx, "s"); err != nil {
		t.Fatal(err)
	}
	if err := c.NewWindow(ctx, "s", "w"); err != nil {
		t.Fatal(err)
	}
	eventually(t, "first prompt", promptIs(c, "s", "w", NoStatus))
	type process struct {
		name, command string
		survives      bool
	}
	processes := []process{
		{"job", "%s &", false},
		{"nohup", "nohup %s >/dev/null 2>&1 &", true},
		{"disown", "%s & disown", true},
		{"script", `sh -c "%s &"`, true},
	}
	if _, err := exec.LookPath("setsid"); err == nil {
		processes = append(processes, process{"setsid", "setsid %s >/dev/null 2>&1 &", true})
	}
	// last: it holds the terminal
	processes = append(processes, process{"foreground", "%s", false})
	pids := make(map[string]int, len(processes))
	for _, p := range processes {
		pidFile := filepath.Join(home, p.name)
		if err := c.SendText(ctx, "s", "w", fmt.Sprintf(p.command, shellQuote(marker)+" "+shellQuote(pidFile)), true); err != nil {
			t.Fatal(err)
		}
		eventually(t, p.name+" started", func() (bool, string) {
			data, err := os.ReadFile(pidFile)
			if err != nil {
				return false, err.Error()
			}
			pid, err := strconv.Atoi(strings.TrimSpace(string(data)))
			if err != nil {
				return false, err.Error()
			}
			pids[p.name] = pid
			return true, ""
		})
		t.Cleanup(func() { _ = syscall.Kill(pids[p.name], syscall.SIGKILL) })
	}
	if err := c.KillWindow(ctx, "s", "w"); err != nil {
		t.Fatal(err)
	}
	for _, p := range processes {
		if !p.survives {
			eventually(t, p.name+" terminated", func() (bool, string) {
				return !processRunning(pids[p.name]), "still running"
			})
		}
	}
	// the hangup went through: a moment for a signal on its way to the others to end them
	time.Sleep(200 * time.Millisecond)
	for _, p := range processes {
		if p.survives && !processRunning(pids[p.name]) {
			t.Errorf("%s: expected it to keep running once its window is closed", p.name)
		}
	}
}
