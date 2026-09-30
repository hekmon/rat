package tmux

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os/exec"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/hekmon/rat/tmux/names"
)

// Controller owns the tmux server of a tenant, and runs the commands on its sessions and windows.
// Create it with New, which validates the tenant. It is safe for concurrent use.
//
// It holds the server process, never the state of its terminals: sessions and windows are asked
// to tmux on every call. It runs one server at a time, and a server that exited on its own can
// be replaced by calling StartServer again.
type Controller struct {
	// config
	tenant string
	// runtime: the current server, nil when none is started
	server *serverProcess
	// serverAction is held exclusively to start or stop the server, and shared to run commands on it
	serverAction sync.RWMutex
	// windowCreation makes checking a window name is free and creating it atomic: tmux itself
	// accepts duplicate names
	windowCreation sync.Mutex
	// inputBuffers numbers the tmux buffers SendText pastes from: buffers are global to the
	// server, so concurrent inputs need distinct names
	inputBuffers atomic.Uint64
}

// New returns a controller for tenant, which can be empty (default socket) or a plain name (see
// package names): the error then wraps names.ErrInvalid. It does not start the server: see
// StartServer.
func New(tenant string) (c *Controller, err error) {
	if tenant != "" {
		if err = names.Check(tenant); err != nil {
			return nil, fmt.Errorf("tenant: %w", err)
		}
	}
	c = &Controller{
		tenant: tenant,
	}
	return
}

// checkNames returns an error wrapping names.ErrInvalid if one of the session or window names of
// a target is invalid, before anything reaches tmux.
func checkNames(targetNames ...string) error {
	for _, name := range targetNames {
		if err := names.Check(name); err != nil {
			return err
		}
	}
	return nil
}

// socketName returns the name of the tmux socket of the tenant (-L).
func (c *Controller) socketName() string {
	if c.tenant == "" {
		return "rat"
	}
	return "rat-" + c.tenant
}

// commandWaitDelay is how long waiting for a tmux client killed by its context waits for its
// output pipes to close (see cmd).
const commandWaitDelay = time.Second

// cmd returns the tmux client command running args on the server of the tenant. It returns once
// ctx ends, even on a server that does not answer.
func (c *Controller) cmd(ctx context.Context, args []string) (cmd *exec.Cmd) {
	// -f /dev/null: never load the user (nor system) configuration, rat must behave the same
	// everywhere. It only matters when the command starts a server, but is harmless otherwise
	// and ensures a server started by any command is configuration free.
	// -u: output UTF-8 whatever rat's locale. Without a UTF-8 locale (common for services), tmux
	// replaces non ASCII characters by '_' in what it prints: captures, paths.
	cmd = exec.CommandContext(ctx, "tmux", append([]string{"-L", c.socketName(), "-f", "/dev/null", "-u"}, args...)...)
	// The client hands its stdin, stdout and stderr to the server, over the socket: a server not
	// reading it (stopped, stuck) keeps them open after the client is killed, and waiting for the
	// output would wait for the server. WaitDelay gives up on the pipes once ctx has ended.
	cmd.WaitDelay = commandWaitDelay
	return cmd
}

// tmuxArg protects an argument ending with ';': tmux reads it as the end of the command (the ';'
// is dropped), even in arguments passed without a shell, unless the ';' is preceded by a
// backslash, which tmux then removes. Arguments coming from agents (key names) must go through it.
func tmuxArg(arg string) string {
	if strings.HasSuffix(arg, ";") {
		return arg[:len(arg)-1] + `\;`
	}
	return arg
}

// run executes a tmux command on the server and returns its standard output. It fails with
// ErrServerNotRunning if the server is not started or has exited, and a tmux failure includes
// the tmux message.
func (c *Controller) run(ctx context.Context, args ...string) (string, error) {
	return c.runWithStdin(ctx, nil, args...)
}

// serverContext returns the context of the running server, canceled once it is stopped, or nil
// if no server was started. Commands that must run whatever their caller context (cleanups) use
// it: they are only pointless once the server, and what they clean up with it, is gone.
func (c *Controller) serverContext() context.Context {
	c.serverAction.RLock()
	defer c.serverAction.RUnlock()
	if c.server == nil {
		return nil
	}
	return c.server.ctx
}

// runWithStdin is run, feeding stdin to the tmux client (nil for none).
func (c *Controller) runWithStdin(ctx context.Context, stdin io.Reader, args ...string) (string, error) {
	// Holding the lock keeps the server from being stopped while the command runs: some commands
	// (new-session) start a server when none is running, as a daemon rat would not watch.
	// Only a server crashing in the meantime can still lead to that.
	c.serverAction.RLock()
	defer c.serverAction.RUnlock()
	if c.server == nil {
		return "", ErrServerNotRunning
	}
	select {
	case <-c.server.done:
		return "", c.server.exitedOnItsOwn()
	default:
	}
	cmd := c.cmd(ctx, args)
	cmd.Stdin = stdin
	out, err := cmd.Output()
	if err != nil {
		var exitErr *exec.ExitError
		if errors.As(err, &exitErr) && len(exitErr.Stderr) > 0 {
			return "", fmt.Errorf("%w: %s", err, strings.TrimSpace(string(exitErr.Stderr)))
		}
		return "", err
	}
	return string(out), nil
}
