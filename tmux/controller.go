package tmux

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os/exec"
	"regexp"
	"strings"
	"sync"
	"sync/atomic"
)

var (
	// ErrInvalidTenant is returned by New for a tenant name that is not a plain name.
	ErrInvalidTenant = errors.New("invalid tenant name")
	// ErrInvalidName is returned for a session or window name that is not a plain name, before
	// anything reaches tmux (see CheckName).
	ErrInvalidName = errors.New("invalid name")
)

// nameMaxLen bounds tenant, session and window names. For tenants, it keeps the socket path
// (/tmp/tmux-<uid>/rat-<tenant>) well under the unix socket path limit (104 bytes on macOS,
// 108 on Linux).
const nameMaxLen = 32

// nameFormat only allows plain names. A tenant ends up in the tmux socket name, where "/" or ".."
// would move the socket out of the tmux private directory. Session and window names end up in
// tmux targets, where ':' and '.' separate the session, window and pane parts.
var nameFormat = regexp.MustCompile(`^[a-zA-Z0-9_-]+$`)

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

// New returns a controller for tenant, which can be empty (default socket) or
// only contain letters, digits, '_' and '-' (up to nameMaxLen characters).
// It does not start the server: see StartServer.
func New(tenant string) (c *Controller, err error) {
	if tenant != "" && !validName(tenant) {
		return nil, fmt.Errorf("%w %q: only letters, digits, '_' and '-' are allowed (up to %d characters)",
			ErrInvalidTenant, tenant, nameMaxLen)
	}
	c = &Controller{
		tenant: tenant,
	}
	return
}

func validName(name string) bool {
	return len(name) <= nameMaxLen && nameFormat.MatchString(name)
}

// CheckName returns an error wrapping ErrInvalidName if name can not be a session or window
// name: only letters, digits, '_' and '-' are allowed, up to 32 characters. It is the rule every
// method applies, for callers to reject a name without asking tmux.
func CheckName(name string) error {
	if !validName(name) {
		return fmt.Errorf("%w %q: only letters, digits, '_' and '-' are allowed (up to %d characters)",
			ErrInvalidName, name, nameMaxLen)
	}
	return nil
}

// checkNames returns an error wrapping ErrInvalidName if one of the session or window names is invalid.
func checkNames(names ...string) error {
	for _, name := range names {
		if err := CheckName(name); err != nil {
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

func (c *Controller) cmd(ctx context.Context, args []string) (cmd *exec.Cmd) {
	// -f /dev/null: never load the user (nor system) configuration, rat must behave the same
	// everywhere. It only matters when the command starts a server, but is harmless otherwise
	// and ensures a server started by any command is configuration free.
	// -u: output UTF-8 whatever rat's locale. Without a UTF-8 locale (common for services), tmux
	// replaces non ASCII characters by '_' in what it prints: captures, paths.
	return exec.CommandContext(ctx, "tmux", append([]string{"-L", c.socketName(), "-f", "/dev/null", "-u"}, args...)...)
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
