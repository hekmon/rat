package tmux

import (
	"context"
	"errors"
	"fmt"
	"os/exec"
	"regexp"
	"strings"
	"sync"
)

var (
	ErrInvalidTenant = errors.New("invalid tenant name")
	ErrInvalidName   = errors.New("invalid name")
)

// nameMaxLen bounds tenant, session and window names. For tenants, it keeps the socket path
// (/tmp/tmux-<uid>/rat-<tenant>) well under the unix socket path limit (104 bytes on macOS,
// 108 on Linux).
const nameMaxLen = 32

// nameFormat only allows plain names. A tenant ends up in the tmux socket name, where "/" or ".."
// would move the socket out of the tmux private directory. Session and window names end up in
// tmux targets, where ':' and '.' separate the session, window and pane parts.
var nameFormat = regexp.MustCompile(`^[a-zA-Z0-9_-]+$`)

type Controller struct {
	// config
	tenant string
	// runtime
	server          *exec.Cmd
	serverCtx       context.Context
	serverCtxCancel func()
	serverDone      chan struct{} // closed once the server process has exited and been reaped
	serverWaitErr   error         // server Wait() result, only valid once serverDone is closed
	// serverAction is held exclusively to start or stop the server, and shared to run commands on it
	serverAction sync.RWMutex
	// windowCreation makes checking a window name is free and creating it atomic: tmux itself
	// accepts duplicate names
	windowCreation sync.Mutex
}

// New returns a controller for tenant, which can be empty (default socket) or
// only contain letters, digits, '_' and '-' (up to nameMaxLen characters).
func New(ctx context.Context, tenant string) (c *Controller, err error) {
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

// checkNames returns an error wrapping ErrInvalidName if one of the session or window names is invalid.
func checkNames(names ...string) error {
	for _, name := range names {
		if !validName(name) {
			return fmt.Errorf("%w %q: only letters, digits, '_' and '-' are allowed (up to %d characters)",
				ErrInvalidName, name, nameMaxLen)
		}
	}
	return nil
}

func (c *Controller) cmd(ctx context.Context, args []string) (cmd *exec.Cmd) {
	socketName := "rat"
	if c.tenant != "" {
		socketName += "-" + c.tenant
	}
	// -f /dev/null: never load the user (nor system) configuration, rat must behave the same
	// everywhere. It only matters when the command starts a server, but is harmless otherwise
	// and ensures a server started by any command is configuration free.
	// -u: output UTF-8 whatever rat's locale. Without a UTF-8 locale (common for services), tmux
	// replaces non ASCII characters by '_' in what it prints: captures, paths.
	return exec.CommandContext(ctx, "tmux", append([]string{"-L", socketName, "-f", "/dev/null", "-u"}, args...)...)
}

// tmuxArg protects an argument ending with ';': tmux reads it as the end of the command (the ';'
// is dropped), even in arguments passed without a shell, unless the ';' is preceded by a
// backslash, which tmux then removes. Arguments coming from agents (text, keys) must go through it.
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
	// Holding the lock keeps the server from being stopped while the command runs: some commands
	// (new-session) start a server when none is running, as a daemon rat would not watch.
	// Only a server crashing in the meantime can still lead to that.
	c.serverAction.RLock()
	defer c.serverAction.RUnlock()
	if c.server == nil {
		return "", ErrServerNotRunning
	}
	select {
	case <-c.serverDone:
		return "", fmt.Errorf("%w: server exited on its own", ErrServerNotRunning)
	default:
	}
	out, err := c.cmd(ctx, args).Output()
	if err != nil {
		var exitErr *exec.ExitError
		if errors.As(err, &exitErr) && len(exitErr.Stderr) > 0 {
			return "", fmt.Errorf("%w: %s", err, strings.TrimSpace(string(exitErr.Stderr)))
		}
		return "", err
	}
	return string(out), nil
}
