package tmux

import (
	"context"
	"errors"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"testing"

	"github.com/hekmon/rat/tmux/names"
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

// TestSessionsClientNames guards why session names are not names of tmux clients
// (names.CheckSession): an exact target ('=') naming no session reaches the session of a client of
// that name, whose windows a command then acts on. A client without a terminal (control mode, its
// input a pipe) is named client-<pid>, and attached to session alice, its name reaches alice: the
// controller refuses it, before tmux.
func TestSessionsClientNames(t *testing.T) {
	c := startTestServer(t, "sessionsclient")
	ctx := context.Background()
	if err := c.NewSession(ctx, "alice"); err != nil {
		t.Fatal(err)
	}
	client := c.cmd(ctx, []string{"-C", "attach-session", "-t", "=alice"})
	input, err := client.StdinPipe()
	if err != nil {
		t.Fatal(err)
	}
	if err = client.Start(); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		_ = input.Close()
		_ = client.Wait()
	})
	name := "client-" + strconv.Itoa(client.Process.Pid)
	eventually(t, "client attached", func() (bool, string) {
		out, err := c.run(ctx, "list-clients", "-F", "#{client_name}")
		return err == nil && strings.Contains(out, name+"\n"), out
	})
	// the pitfall: no session is named so, and tmux lists the windows of alice
	if out, err := c.run(ctx, "list-windows", "-t", "="+name, "-F", "#{session_name}"); err != nil || out != "alice\n" {
		t.Errorf("expected tmux to reach alice through its client %s, got %q, %v", name, out, err)
	}
	if _, err = c.ListWindows(ctx, name); !errors.Is(err, names.ErrInvalid) {
		t.Errorf("expected %s refused as a session name, got %v", name, err)
	}
}

// TestSessionsNames guards that session names are validated before reaching tmux, where ':' and
// '.' are target separators.
func TestSessionsNames(t *testing.T) {
	c := startTestServer(t, "sessionsnames")
	ctx := context.Background()
	for _, session := range []string{"a:b", "s.1", "", "=s"} {
		if err := c.NewSession(ctx, session); !errors.Is(err, names.ErrInvalid) {
			t.Errorf("%q: expected names.ErrInvalid, got %v", session, err)
		}
	}
}

// TestSessionsForeignNames guards that a session a command typed in a terminal named with a space,
// as tmux accepts (tmux new-session -s 'a b'), is listed under its name, and not taken for the
// sessions its words name: a missing session a is still told missing, rather than its command
// failing as is.
func TestSessionsForeignNames(t *testing.T) {
	c := startTestServer(t, "sessionsforeign")
	ctx := context.Background()
	if err := c.NewSession(ctx, "s"); err != nil {
		t.Fatal(err)
	}
	if _, err := c.run(ctx, "new-session", "-d", "-s", "a b"); err != nil {
		t.Fatal(err)
	}
	if sessions, err := c.ListSessions(ctx); err != nil || !slices.Equal(sessions, []string{"a b", "s"}) {
		t.Errorf("expected [a b s], got %q, %v", sessions, err)
	}
	if _, err := c.ListWindows(ctx, "a"); !errors.Is(err, ErrSessionNotFound) {
		t.Errorf("expected ErrSessionNotFound, got %v", err)
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
