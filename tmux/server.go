package tmux

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"os/exec"
	"strconv"
	"strings"
	"syscall"
	"time"
)

var (
	// ErrServerAlreadyStarted is returned by StartServer while its server is running.
	ErrServerAlreadyStarted = errors.New("server already started")
	// ErrServerNotRunning is returned by commands and StopServer when the server has not been
	// started, has been stopped, or has exited on its own. StartServer can start a new one.
	ErrServerNotRunning = errors.New("server not running")
	// ErrServerTerminated is returned by StopServer when the server had to be sent SIGTERM.
	ErrServerTerminated = errors.New("server did not stop gracefully and has been terminated (SIGTERM)")
	// ErrServerKilled is returned by StopServer when the server had to be sent SIGKILL.
	ErrServerKilled = errors.New("server did not stop gracefully and has been killed (SIGKILL)")
)

// serverStartTimeout is how long StartServer waits for the server to answer probes.
const serverStartTimeout = time.Second

// serverReadyProbeInterval is the delay between two readiness probes.
const serverReadyProbeInterval = 50 * time.Millisecond

// serverStderrMaxLen bounds how much of the server stderr is kept: tmux only writes
// there when failing at startup (before becoming a server), a line is enough.
const serverStderrMaxLen = 1024

// serverHistoryLimit is the scrollback kept per pane, bounding how many extra lines above the
// visible screen can be captured (tmux default: 2000).
const serverHistoryLimit = 10000

// serverDefaultSize is the fixed size of the terminals (tmux default: 80x24).
// Wide, because programs lay out their output for the terminal width: some truncate lines to it
// (ps, docker, tables), and joining wrapped lines at capture can not recover what they cut.
// Short, because the height is what a full screen capture returns by default: more lines are
// available on demand from the scrollback. But not shorter than 24, the classic terminal height
// full-screen programs are designed for: below it some refuse to run or cut their menus (dialog,
// whiptail), top shows few processes, and pagers (less via git, journalctl…) kick in more often.
const serverDefaultSize = "200x24"

// serverStopGracePeriod is how long StopServer waits for the server to exit
// after a successful kill-server before terminating it (SIGTERM).
const serverStopGracePeriod = 5 * time.Second

// serverKillDelay is how long the server has to exit after receiving SIGTERM
// (context cancellation) before being sent SIGKILL.
const serverKillDelay = 2 * time.Second

// fixedSize returns the tmux command keeping the size of the window target (serverDefaultSize)
// whoever attaches, to chain in the invocation creating the window. With the default size policy
// (latest), a human attaching to inspect resizes the windows to their terminal, and they keep
// that size once the human detaches. It is a window option set on each window rather than a
// global one, which crashes tmux 3.3 to 3.6 (see StartServer).
func fixedSize(target string) []string {
	return []string{"set-option", "-w", "-t", target, "window-size", "manual"}
}

// StartServer starts the tmux server and returns once it is ready and configured.
// ctx should be the application context, as an exit safe guard (kill).
// The server does not load any tmux configuration and its terminals run bash, which must be
// installed (the returned error then wraps exec.ErrNotFound).
func (c *Controller) StartServer(ctx context.Context) (err error) {
	defer c.serverAction.Unlock()
	c.serverAction.Lock()
	if c.server != nil {
		select {
		case <-c.serverDone:
			// previous server exited on its own, we can start a new one
			c.resetServer()
		default:
			return ErrServerAlreadyStarted
		}
	}
	// Terminals run bash: check it now rather than failing on each new window
	bashPath, err := exec.LookPath("bash")
	if err != nil {
		return fmt.Errorf("bash is required to run terminals: %w", err)
	}
	// Cleanup on error
	defer func() {
		if err != nil {
			c.serverCtxCancel() // terminate the server if started
			if c.serverDone != nil {
				<-c.serverDone // wait for the watcher to reap the terminated process
			}
			c.resetServer()
		}
	}()
	// Start the server
	c.serverCtx, c.serverCtxCancel = context.WithCancel(ctx)
	server := c.cmd(c.serverCtx, []string{"-D"})
	server.Cancel = func() error {
		// let tmux clean up (socket, clients) before resorting to SIGKILL
		return server.Process.Signal(syscall.SIGTERM)
	}
	// if the server has not exited serverKillDelay after Cancel has been called,
	// os/exec kills it (SIGKILL)
	server.WaitDelay = serverKillDelay
	// keep the server stderr to explain an early exit
	stderr := &limitedBuffer{limit: serverStderrMaxLen}
	server.Stderr = stderr
	c.server = server
	if err = c.server.Start(); err != nil {
		return fmt.Errorf("failed to start server: %w", err)
	}
	// Watch the server: this goroutine is the only one calling Wait(), others use serverDone
	done := make(chan struct{})
	c.serverDone = done
	go func() {
		waitErr := server.Wait()
		if msg := stderr.String(); waitErr != nil && msg != "" {
			waitErr = fmt.Errorf("%w: %s", waitErr, msg)
		}
		c.serverWaitErr = waitErr
		close(done)
	}()
	// Wait for it to be ready: the deadline also bounds a probe hanging
	readyCtx, readyCtxCancel := context.WithTimeout(ctx, serverStartTimeout)
	defer readyCtxCancel()
	ticker := time.NewTicker(serverReadyProbeInterval)
	defer ticker.Stop()
	serverPID := strconv.Itoa(server.Process.Pid)
	var probeErr error
readiness:
	for {
		// probe server, checking it is ours answering (and not a leftover one on the same socket)
		out, err := c.cmd(readyCtx, []string{"display-message", "-p", "#{pid}"}).Output()
		if err == nil {
			pid := strings.TrimSpace(string(out))
			if pid == serverPID {
				break readiness
			}
			err = fmt.Errorf("socket answered by another server (pid %s, ours is %s)", pid, serverPID)
		}
		if readyCtx.Err() == nil {
			// keep the last meaningful probe error, not the one of a probe interrupted by readyCtx
			probeErr = err
		}
		select {
		case <-ticker.C:
		case <-done:
			return fmt.Errorf("server exited during startup: %w", c.serverWaitErr)
		case <-readyCtx.Done():
			if probeErr == nil {
				return fmt.Errorf("server not ready: %w", readyCtx.Err())
			}
			return fmt.Errorf("server not ready: %w (last probe error: %w)", readyCtx.Err(), probeErr)
		}
	}
	// Configure it: without a configuration file, these are the only differences with tmux defaults
	optsCtx, optsCtxCancel := context.WithTimeout(ctx, serverStartTimeout)
	defer optsCtxCancel()
	options := [][2]string{
		{"default-shell", bashPath},
		{"history-limit", strconv.Itoa(serverHistoryLimit)},
		{"default-size", serverDefaultSize},
		// Not global: set on each window as it is created instead (see fixedSize). tmux 3.3 to 3.6
		// crash (segfault, killing every terminal) when a session is created while the global
		// window-size is manual: fixed in 3.7 (commit 7d41761e, GitHub issue 4849).
		// {"window-size", "manual"},
		// already tmux default, but window names are how agents find their terminals back:
		// programs must not be able to rename them (escape sequences)
		{"allow-rename", "off"},
	}
	// Terminals inherit the server environment (rat's own) plus these variables
	environment := [][2]string{
		// macOS bash prints a "default shell is now zsh" notice at each start, which would be the
		// first thing agents read in every new terminal
		{"BASH_SILENCE_DEPRECATION_WARNING", "1"},
		// the same neutral UTF-8 locale everywhere, whatever rat's own (a service often has none).
		// UTF-8: without it, bash reads non ASCII input (é, ✓) as meta keys and mangles it, and
		// models read and write UTF-8. C: English messages and stable formats (decimal point,
		// dates), the easiest for models to read. LC_ALL overrides any other locale variable.
		{"LC_ALL", "C.UTF-8"},
		// no pager: when an output does not fit the screen, tools (man, git, systemctl, psql…) would
		// open less, and agents would have to notice it and quit it. Instead the output goes to the
		// terminal, where the scrollback keeps it for extra lines captures.
		{"PAGER", "cat"},
		// tool specific pagers take precedence over PAGER and users often set them (MANPAGER in a
		// shell profile, inherited by terminals). GIT_PAGER also overrides core.pager in gitconfig.
		{"GIT_PAGER", "cat"},
		{"MANPAGER", "cat"},
		{"SYSTEMD_PAGER", "cat"},
	}
	var args []string
	for _, option := range options {
		args = append(args, "set-option", "-g", option[0], option[1], ";")
	}
	for _, variable := range environment {
		args = append(args, "set-environment", "-g", variable[0], variable[1], ";")
	}
	args = args[:len(args)-1] // no trailing command separator
	if out, err := c.cmd(optsCtx, args).CombinedOutput(); err != nil {
		return fmt.Errorf("failed to configure server: %w: %s", err, strings.TrimSpace(string(out)))
	}
	return
}

// StopServer stops the current server if running, asking it to exit with kill-server.
// A nil error means the server exited gracefully. Otherwise:
//   - ErrServerNotRunning: there was no server to stop, or it had already exited on its own
//   - ErrServerTerminated: the server did not exit gracefully and has been sent SIGTERM, which it exited on
//   - ErrServerKilled: the server ignored SIGTERM for serverKillDelay and has been sent SIGKILL
//   - any other error: the server exited with a non-zero code, or waiting for it failed
//
// SIGKILL is issued as a last resort, but the server being gone once this returns is not
// guaranteed: a process in uninterruptible sleep (D state) for example only dies when it leaves it.
func (c *Controller) StopServer(ctx context.Context) (err error) {
	defer c.serverAction.Unlock()
	c.serverAction.Lock()
	if c.server == nil {
		return ErrServerNotRunning
	}
	defer c.resetServer()
	// Has it exited on its own ?
	select {
	case <-c.serverDone:
		if c.serverWaitErr != nil {
			return fmt.Errorf("%w: server exited on its own: %w", ErrServerNotRunning, c.serverWaitErr)
		}
		return fmt.Errorf("%w: server exited on its own", ErrServerNotRunning)
	default:
	}
	// Try to close it properly first
	stopCmd := c.cmd(ctx, []string{"kill-server"})
	if err = stopCmd.Run(); err != nil {
		// failed to execute command, let's terminate the server thru its context
		c.serverCtxCancel()
		<-c.serverDone // wait for the watcher to reap the terminated process
		return fmt.Errorf("%w: kill-server failed: %w", c.forcedStopError(), err)
	}
	// Let's wait for the server to exit after received the exit command,
	// terminating it if it does not within the grace period
	graceTimer := time.NewTimer(serverStopGracePeriod)
	defer graceTimer.Stop()
	select {
	case <-c.serverDone:
	case <-graceTimer.C:
		c.serverCtxCancel()
		<-c.serverDone // wait for the watcher to reap the terminated process
		return fmt.Errorf("still running %s after kill-server command: %w", serverStopGracePeriod, c.forcedStopError())
	}
	if c.serverWaitErr != nil {
		// non-zero exit codes are reported as *exec.ExitError
		return fmt.Errorf("server exited with an error: %w", c.serverWaitErr)
	}
	// clean stop
	return
}

// resetServer releases the server context resources and clears the server state.
// If the server is still running, canceling its context terminates it.
func (c *Controller) resetServer() {
	c.serverCtxCancel()
	c.server, c.serverCtx, c.serverCtxCancel = nil, nil, nil
	c.serverDone, c.serverWaitErr = nil, nil
}

// forcedStopError tells, once the server context has been canceled and serverDone closed,
// if the server has exited on SIGTERM or had to be killed after serverKillDelay.
func (c *Controller) forcedStopError() error {
	if c.server.ProcessState == nil {
		// Wait failed before reaping the process: we can not tell how it stopped
		return fmt.Errorf("failed to wait for the server: %w", c.serverWaitErr)
	}
	if ws, ok := c.server.ProcessState.Sys().(syscall.WaitStatus); ok && ws.Signaled() && ws.Signal() == syscall.SIGKILL {
		return ErrServerKilled
	}
	return ErrServerTerminated
}

// limitedBuffer keeps the first limit bytes written to it and discards the rest.
// It must only be read once the command writing to it has been waited for.
type limitedBuffer struct {
	buf   bytes.Buffer
	limit int
}

func (lb *limitedBuffer) Write(p []byte) (n int, err error) {
	if room := lb.limit - lb.buf.Len(); room > 0 {
		lb.buf.Write(p[:min(len(p), room)])
	}
	// report everything as written, otherwise os/exec would stop copying and error out
	return len(p), nil
}

func (lb *limitedBuffer) String() string {
	return strings.TrimSpace(lb.buf.String())
}
