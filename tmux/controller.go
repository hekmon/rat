package tmux

import (
	"context"
	"errors"
	"fmt"
	"os/exec"
	"regexp"
	"sync"
)

var (
	ErrInvalidTenant = errors.New("invalid tenant name")
)

// tenantMaxLen keeps the socket path (/tmp/tmux-<uid>/rat-<tenant>) well under
// the unix socket path limit (104 bytes on macOS, 108 on Linux).
const tenantMaxLen = 32

// tenantFormat only allows plain names: the tenant ends up in the tmux socket name,
// anything like "/" or ".." would move the socket out of the tmux private directory.
var tenantFormat = regexp.MustCompile(`^[a-zA-Z0-9_-]+$`)

type Controller struct {
	// config
	tenant string
	// runtime
	server          *exec.Cmd
	serverCtx       context.Context
	serverCtxCancel func()
	serverDone      chan struct{} // closed once the server process has exited and been reaped
	serverWaitErr   error         // server Wait() result, only valid once serverDone is closed
	serverAction    sync.Mutex
}

// New returns a controller for tenant, which can be empty (default socket) or
// only contain letters, digits, '_' and '-' (up to tenantMaxLen characters).
func New(ctx context.Context, tenant string) (c *Controller, err error) {
	if tenant != "" && (len(tenant) > tenantMaxLen || !tenantFormat.MatchString(tenant)) {
		return nil, fmt.Errorf("%w %q: only letters, digits, '_' and '-' are allowed (up to %d characters)",
			ErrInvalidTenant, tenant, tenantMaxLen)
	}
	c = &Controller{
		tenant: tenant,
	}
	return
}

func (c *Controller) cmd(ctx context.Context, args []string) (cmd *exec.Cmd) {
	socketName := "rat"
	if c.tenant != "" {
		socketName += "-" + c.tenant
	}
	return exec.CommandContext(ctx, "tmux", append([]string{"-L", socketName}, args...)...)
}
