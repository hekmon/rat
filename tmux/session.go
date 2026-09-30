package tmux

import (
	"context"
	"errors"
	"fmt"
	"os"
	"slices"
	"strings"
	"time"
)

var (
	// ErrSessionNotFound is returned by commands on a session that does not exist.
	ErrSessionNotFound = errors.New("session not found")
	// ErrSessionExists is returned by NewSession when the session already exists.
	ErrSessionExists = errors.New("session already exists")
)

// ListSessions returns the names of the existing sessions, sorted by name.
func (c *Controller) ListSessions(ctx context.Context) ([]string, error) {
	out, err := c.run(ctx, "list-sessions", "-F", "#{session_name}")
	if err != nil {
		return nil, fmt.Errorf("failed to list sessions: %w", err)
	}
	return strings.Fields(out), nil
}

// FirstWindow is the name of the window a session is created with.
const FirstWindow = "main"

// NewSession creates session, with a first window named FirstWindow: a tmux session can not exist
// without a window. Agents name the windows they create themselves, and can use this one as is.
// Its terminals start in the home directory of the user running rat.
// The error wraps ErrSessionExists if the session already exists.
func (c *Controller) NewSession(ctx context.Context, session string) error {
	if err := checkNames(session); err != nil {
		return err
	}
	// Without -c, tmux would start in the directory rat runs from (/ for a systemd service).
	// A terminal starts at home, which is the agents' own when they run as a dedicated user.
	home, err := os.UserHomeDir()
	if err != nil {
		return fmt.Errorf("failed to create session %s: %w", session, err)
	}
	args := append([]string{"new-session", "-d", "-s", session, "-n", FirstWindow, "-c", home, ";"},
		fixedSize(target(session, FirstWindow))...)
	if _, err = c.run(ctx, args...); err != nil {
		if sessions, listErr := c.ListSessions(ctx); listErr == nil && slices.Contains(sessions, session) {
			return fmt.Errorf("%w: %s", ErrSessionExists, session)
		}
		return fmt.Errorf("failed to create session %s: %w", session, err)
	}
	return nil
}

// KillSession destroys session, terminating the processes running in its windows.
// The error wraps ErrSessionNotFound if the session does not exist.
func (c *Controller) KillSession(ctx context.Context, session string) error {
	if err := checkNames(session); err != nil {
		return err
	}
	if _, err := c.run(ctx, "kill-session", "-t", "="+session); err != nil {
		return c.sessionError(ctx, fmt.Errorf("failed to kill session %s: %w", session, err), session)
	}
	return nil
}

// serverExitWait is how long explaining a failed command waits for the server to be seen exiting
// (see sessionError).
const serverExitWait = 500 * time.Millisecond

// sessionError explains why a command on session failed. tmux reports a missing session with
// messages depending on the server state ("can't find session", but "no current target" once no
// session is left), so rather than parsing them, ask tmux what exists.
// It returns an error wrapping ErrSessionNotFound, ErrServerNotRunning, or err as is.
func (c *Controller) sessionError(ctx context.Context, err error, session string) error {
	if errors.Is(err, ErrServerNotRunning) {
		return err
	}
	sessions, listErr := c.ListSessions(ctx)
	switch {
	case listErr == nil && !slices.Contains(sessions, session):
		return fmt.Errorf("%w: %s", ErrSessionNotFound, session)
	case listErr == nil:
		return err
	}
	// Listing failed as well: the server may have died during the command, which then failed
	// saying little ("server exited unexpectedly"), and is reaped a moment later. Not once ctx
	// ended: a server stuck, not dying, would make every timeout longer.
	if ctx.Err() == nil && c.serverExited(serverExitWait) {
		return fmt.Errorf("%w: server exited during the command: %w", ErrServerNotRunning, err)
	}
	return err
}

// serverExited tells whether the current server has exited, waiting for it up to wait.
func (c *Controller) serverExited(wait time.Duration) bool {
	c.serverAction.RLock()
	p := c.server
	c.serverAction.RUnlock()
	if p == nil {
		return true
	}
	select {
	case <-p.done:
		return true
	case <-time.After(wait):
		return false
	}
}
