package tmux

import (
	"context"
	"errors"
	"path/filepath"
	"testing"
	"time"
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

// TestParseWindow guards the parsing of the window format, whose command is quoted by tmux and
// whose path comes last unquoted.
func TestParseWindow(t *testing.T) {
	w, err := parseWindow(`w 1700000000 my\ prog\|x /tmp/a dir|b`)
	if err != nil {
		t.Fatal(err)
	}
	expected := Window{Name: "w", Command: "my prog|x", Path: "/tmp/a dir|b", Activity: time.Unix(1700000000, 0)}
	if w != expected {
		t.Errorf("got %+v, expected %+v", w, expected)
	}
	for _, line := range []string{"", "w", "w 1700000000", "w notanumber bash /tmp"} {
		if _, err := parseWindow(line); err == nil {
			t.Errorf("%q: expected an error", line)
		}
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
	windows, err := c.ListWindows(ctx, "s")
	if err != nil {
		t.Fatal(err)
	}
	if len(windows) != 1 {
		t.Fatalf("expected one window, got %+v", windows)
	}
	if w := windows[0]; w.Name != FirstWindow || w.Command != "bash" || w.Path != home || time.Since(w.Activity) > time.Minute {
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
