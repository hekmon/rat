package tmux

import (
	"context"
	"errors"
	"path/filepath"
	"slices"
	"strings"
	"testing"
)

// startTestServer returns a controller whose server is started.
func startTestServer(t *testing.T, tenant string) *Controller {
	t.Helper()
	c := newTestController(t, tenant)
	if err := c.StartServer(context.Background()); err != nil {
		t.Fatal(err)
	}
	return c
}

// TestSessions guards the sessions lifecycle and its errors, including a missing session once
// no session is left, which tmux reports differently.
func TestSessions(t *testing.T) {
	c := startTestServer(t, "sessions")
	ctx := context.Background()
	sessions, err := c.ListSessions(ctx)
	if err != nil || len(sessions) != 0 {
		t.Fatalf("expected no session, got %v, %v", sessions, err)
	}
	if err = c.NewSession(ctx, "s1"); err != nil {
		t.Fatal(err)
	}
	if err = c.NewSession(ctx, "s2"); err != nil {
		t.Fatal(err)
	}
	if err = c.NewSession(ctx, "s1"); !errors.Is(err, ErrSessionExists) {
		t.Errorf("expected ErrSessionExists, got %v", err)
	}
	if sessions, err = c.ListSessions(ctx); err != nil || !slices.Equal(sessions, []string{"s1", "s2"}) {
		t.Errorf("expected [s1 s2], got %v, %v", sessions, err)
	}
	for _, session := range []string{"s1", "s2"} {
		if err = c.KillSession(ctx, session); err != nil {
			t.Fatal(err)
		}
	}
	if err = c.KillSession(ctx, "s1"); !errors.Is(err, ErrSessionNotFound) {
		t.Errorf("expected ErrSessionNotFound, got %v", err)
	}
}

// TestSessionsNames guards that session names are validated before reaching tmux, where ':' and
// '.' are target separators.
func TestSessionsNames(t *testing.T) {
	c := startTestServer(t, "sessionsnames")
	ctx := context.Background()
	for _, session := range []string{"a:b", "s.1", "", "=s"} {
		if err := c.NewSession(ctx, session); !errors.Is(err, ErrInvalidName) {
			t.Errorf("%q: expected ErrInvalidName, got %v", session, err)
		}
	}
}

// TestCommandsWithoutServer guards that commands fail with ErrServerNotRunning when the server
// is not running, and never start a server by themselves (as a daemon rat would not watch).
func TestCommandsWithoutServer(t *testing.T) {
	c := newTestController(t, "noserver")
	ctx := context.Background()
	if err := c.NewSession(ctx, "s"); !errors.Is(err, ErrServerNotRunning) {
		t.Errorf("expected ErrServerNotRunning, got %v", err)
	}
	if _, err := c.ListSessions(ctx); !errors.Is(err, ErrServerNotRunning) {
		t.Errorf("expected ErrServerNotRunning, got %v", err)
	}
	if err := c.cmd(ctx, []string{"display-message", "-p", "ok"}).Run(); err == nil {
		t.Error("a server is running on the socket")
	}
}

// TestSessionStartsAtHome guards that terminals start in the home directory, not in the one rat
// runs from.
func TestSessionStartsAtHome(t *testing.T) {
	home, err := filepath.EvalSymlinks(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	t.Setenv("HOME", home)
	c := startTestServer(t, "home")
	ctx := context.Background()
	if err = c.NewSession(ctx, "s"); err != nil {
		t.Fatal(err)
	}
	out, err := c.run(ctx, "display-message", "-p", "-t", "=s:="+FirstWindow, "#{pane_current_path}")
	if err != nil {
		t.Fatal(err)
	}
	if path := strings.TrimSpace(out); path != home {
		t.Errorf("terminal started in %s, expected %s", path, home)
	}
}
