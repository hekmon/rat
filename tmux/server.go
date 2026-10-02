package tmux

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"regexp"
	"strconv"
	"strings"
	"sync/atomic"
	"syscall"
	"time"
)

var (
	// ErrServerAlreadyStarted is returned by StartServer while its server is running.
	ErrServerAlreadyStarted = errors.New("server already started")
	// ErrServerSocketInUse is returned by StartServer when another tmux server answers on the
	// socket of the tenant: another rat serving the same tenant, or a server left behind.
	ErrServerSocketInUse = errors.New("socket already served by another tmux server")
	// ErrServerNotRunning is returned by commands and StopServer when the server has not been
	// started, has been stopped, or has exited on its own. StartServer can start a new one.
	ErrServerNotRunning = errors.New("server not running")
	// ErrServerTerminated is returned by StopServer when the server had to be sent SIGTERM.
	ErrServerTerminated = errors.New("server did not stop gracefully and has been terminated (SIGTERM)")
	// ErrServerKilled is returned by StopServer when the server had to be sent SIGKILL.
	ErrServerKilled = errors.New("server did not stop gracefully and has been killed (SIGKILL)")
	// ErrUnsupportedBash is returned by StartServer when the bash found in PATH is older than
	// bashMinVersion, before any server is started.
	ErrUnsupportedBash = errors.New("unsupported bash version")
)

// bashMinVersion is the oldest bash terminals can run: pasted text relies on bracketed paste,
// which bash has since 4.4 (readline 7.0).
var bashMinVersion = [2]int{4, 4}

// bashVersionFormat reads the major and minor version in the first line of bash --version, such
// as "GNU bash, version 5.2.15(1)-release (aarch64-unknown-linux-gnu)".
var bashVersionFormat = regexp.MustCompile(`version (\d+)\.(\d+)`)

// checkBashVersion returns an error wrapping ErrUnsupportedBash if the bash at path is older than
// bashMinVersion.
func checkBashVersion(ctx context.Context, path string) error {
	// --version rather than running a script printing BASH_VERSINFO: bash reads no startup file
	// then (a script would run BASH_ENV). LC_ALL=C: the message is not translated.
	cmd := exec.CommandContext(ctx, path, "--version")
	cmd.Env = append(os.Environ(), "LC_ALL=C")
	out, err := cmd.Output()
	if err != nil {
		return fmt.Errorf("failed to get the version of %s: %w", path, err)
	}
	firstLine, _, _ := strings.Cut(string(out), "\n")
	match := bashVersionFormat.FindStringSubmatch(firstLine)
	if match == nil {
		return fmt.Errorf("failed to read the version of %s in %q", path, firstLine)
	}
	major, _ := strconv.Atoi(match[1])
	minor, _ := strconv.Atoi(match[2])
	if major < bashMinVersion[0] || major == bashMinVersion[0] && minor < bashMinVersion[1] {
		return fmt.Errorf("%w: %s is bash %d.%d, terminals need %d.%d or later (macOS ships 3.2: install a recent bash, with Homebrew for instance, first in PATH)",
			ErrUnsupportedBash, path, major, minor, bashMinVersion[0], bashMinVersion[1])
	}
	return nil
}

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

// ScreenColumns and ScreenRows are the fixed size of the terminals (tmux default: 80x24).
// Wide, because programs lay out their output for the terminal width: some truncate lines to it
// (ps, docker, tables), and joining wrapped lines at capture can not recover what they cut.
// Short, because the height is what a full screen capture returns by default: more lines are
// available on demand from the scrollback. But not shorter than 24, the classic terminal height
// full-screen programs are designed for: below it some refuse to run or cut their menus (dialog,
// whiptail), top shows few processes, and pagers (less via git, journalctl…) kick in more often.
const (
	ScreenColumns = 200
	ScreenRows    = 24
)

// serverDefaultSize is the size of the terminals, as tmux options take it.
var serverDefaultSize = fmt.Sprintf("%dx%d", ScreenColumns, ScreenRows)

// serverStopGracePeriod is how long StopServer waits for the server to exit, kill-server included,
// before terminating it (SIGTERM).
const serverStopGracePeriod = 5 * time.Second

// serverKillDelay is how long the server has to exit after receiving SIGTERM
// (context cancellation) before being sent SIGKILL.
const serverKillDelay = 2 * time.Second

// serverProcess is one run of the tmux server, from StartServer until it exits. Waiters keep a
// reference to it: its exit reason outlives the controller moving on to another server.
type serverProcess struct {
	cmd    *exec.Cmd
	ctx    context.Context // canceled to terminate the server (SIGTERM, then SIGKILL)
	cancel func()
	done   chan struct{} // closed once the process has exited and been reaped
	// waitErr is the Wait() result, with the server stderr if any: only valid once done is closed
	waitErr error
	// stopRequested is set by StopServer before asking the server to exit: its exit is expected
	stopRequested atomic.Bool
	// promptCommand is the PROMPT_COMMAND of its terminals, as StartServer set it (see
	// promptCommand), for CheckTerminals: tmux 3.4 escapes $ in what show-environment prints
	promptCommand string
}

// exitedOnItsOwn returns the error reporting the server exited on its own, once done is closed.
func (p *serverProcess) exitedOnItsOwn() error {
	if p.waitErr != nil {
		return fmt.Errorf("%w: server exited on its own: %w", ErrServerNotRunning, p.waitErr)
	}
	return fmt.Errorf("%w: server exited on its own", ErrServerNotRunning)
}

// bracketedPasteCommand turns bracketed paste on in bash, run before each prompt through
// PROMPT_COMMAND (see promptCommand).
const bracketedPasteCommand = `bind "set enable-bracketed-paste on"`

// promptOption is the pane option where the bash of a terminal records its last prompt (see
// promptCommand), which Window reads: a user option, tmux has no use for it.
const promptOption = "@rat_prompt"

// promptMark starts the value promptCommand marks a prompt being recorded with, on promptOption: it
// reads as no prompt.
const promptMark = "recording"

// inputOption is the pane option where inputs record their time (see recordInput), which Window
// reads: a user option, tmux has no use for it.
const inputOption = "@rat_input"

// promptCommand returns the PROMPT_COMMAND of terminals, run by bash before each prompt, after its
// startup files, and reaching the server with the tmux at tmuxPath. In order, it:
//   - reads the exit status of the last command: anything run before would change it;
//   - turns bracketed paste on, which pasted text relies on, and which bash 4.4 and 5.0 do not
//     enable by default, and an inputrc can disable: enforced whatever startup files say, with no
//     file of rat's to maintain;
//   - marks the prompt as being recorded, on promptOption, with a mark of its own (promptMark, the
//     PID of the bash and a count), and waits for tmux to hold it: an input reaching the window
//     from then on clears it (see clearPrompt), and the mark of a newer prompt replaces it;
//   - stops there if a line waits on the terminal, which read -t 0 tells without reading it: typed
//     while a command ran or bash started, bash runs it right after this prompt, which does not end
//     the command sent last. A whole line: until readline takes the terminal, after
//     PROMPT_COMMAND, the terminal is in canonical mode, where a text without Enter is not
//     readable. It does not run either, waiting on the command line;
//   - records the prompt on promptOption if its mark is still there: the status, - for the first
//     prompt of a bash (which follows its startup files, not a command), and the time, in seconds.
//     The record lands a millisecond or more after the check (see below): an input the check
//     missed, or a newer prompt, may have replaced the mark by then. The record is dropped then,
//     rather than read as following that input, or telling the status of the command before the
//     newer prompt.
//
// Startup files assigning PROMPT_COMMAND defeat it: see CheckTerminals, which reads the status it
// leaves in __rat_status. Nothing it runs writes on the screen. The time comes from printf, without
// starting a process, or from date for a bash older than 4.2 started in a terminal (on macOS,
// typing bash runs the 3.2 of the system). tmux is the one running the server, at tmuxPath, which
// StartServer finds in rat's PATH: looked up in the PATH of the terminal, which the agent and its
// startup files change, it could be missing, or another tmux, and fail silently. The record runs in
// the background from a subshell: in the foreground, it would show as the command of the terminal
// while it runs. The mark is waited for from a command substitution, which runs in the process
// group of bash: bash still shows as the command. It is only run by a bash in a terminal, whose
// TMUX and TMUX_PANE tmux sets: an agent unsetting TMUX would reach another server, and the bash of
// CheckTerminals records nothing.
func promptCommand(tmuxPath string) string {
	tmux := shellQuote(tmuxPath)
	return `__rat_status=$?; [ -n "${__rat_prompted-}" ] || __rat_status=-; __rat_prompted=1; ` +
		bracketedPasteCommand + `; [ -z "${TMUX-}" ] || [ -z "${TMUX_PANE-}" ] || ` +
		`{ __rat_marks=$((${__rat_marks-0}+1)); __rat_mark="` + promptMark + ` $$.$__rat_marks"; ` +
		`: "$(` + tmux + ` set-option -p -t "$TMUX_PANE" ` + promptOption + ` "$__rat_mark" </dev/null 2>&1)"; ` +
		`read -t 0 || { printf -v __rat_time '%(%s)T' -1 2>/dev/null || __rat_time=$(date +%s); ` +
		`(` + tmux + ` if-shell -F -t "$TMUX_PANE" "#{==:#{` + promptOption + `},$__rat_mark}" ` +
		`"set-option -p -t $TMUX_PANE ` + promptOption + ` '$__rat_status $__rat_time'" ` +
		`</dev/null >/dev/null 2>&1 &); }; }`
}

// shellQuote returns s quoted for bash, between single quotes.
func shellQuote(s string) string {
	return "'" + strings.ReplaceAll(s, "'", `'\''`) + "'"
}

// terminalEnvironment is what StartServer adds to the environment terminals inherit, which is
// otherwise rat's own, besides PROMPT_COMMAND (see promptCommand).
var terminalEnvironment = [][2]string{
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

// windowKeys are the keys moving between the windows of a session (C-b n, p, l, 0 to 9), each with
// the window it targets, bound to switch-client rather than to the tmux defaults (next-window,
// previous-window, last-window, select-window): a human watching read-only (attach -r) only gets
// the keys bound to switch-client or detach-client, the others answering "Client is read-only".
// switch-client to a window of the current session selects it, as the defaults do for a client that
// is not read-only, wrapping around the same way. rat never reads the current window of a session,
// targeting windows by name: a human changing it changes nothing for agents.
var windowKeys = [][2]string{
	{"n", ":+"}, {"p", ":-"}, {"l", ":!"},
	{"0", ":=0"}, {"1", ":=1"}, {"2", ":=2"}, {"3", ":=3"}, {"4", ":=4"},
	{"5", ":=5"}, {"6", ":=6"}, {"7", ":=7"}, {"8", ":=8"}, {"9", ":=9"},
}

// fixedSize returns the tmux command giving the window target the size of terminals (ScreenColumns
// by ScreenRows) and keeping it whoever attaches, to chain in the invocation creating the window.
// With the default size policy (latest), a human attaching to inspect resizes the windows to their
// terminal, and they keep that size once the human detaches. The policy alone is not enough: a
// window created while a client is attached, to any session, is created at the size of that client
// (default-size only applies when none is), and the manual policy would keep it. resize-window sets
// both, the size and the manual policy, as an option of the window rather than the global one,
// which crashes tmux 3.3 to 3.6 (see StartServer).
func fixedSize(target string) []string {
	return []string{"resize-window", "-t", target, "-x", strconv.Itoa(ScreenColumns), "-y", strconv.Itoa(ScreenRows)}
}

// StartServer starts the tmux server and returns once it is ready and configured.
// ctx bounds the life of the server: once canceled, the server is terminated (SIGTERM, then
// SIGKILL), as a safeguard for a caller leaving it running. A caller stopping the server in
// order (StopServer) must pass a context that outlives that stop, such as one a signal does not
// cancel.
// The server does not load any tmux configuration and its terminals run bash, which must be
// installed (the returned error then wraps exec.ErrNotFound) in version bashMinVersion or later
// (the error then wraps ErrUnsupportedBash). It refuses a socket another server answers on,
// leaving that server untouched: the error then wraps ErrServerSocketInUse, with its PID.
func (c *Controller) StartServer(ctx context.Context) (err error) {
	defer c.serverAction.Unlock()
	c.serverAction.Lock()
	if c.server != nil {
		select {
		case <-c.server.done:
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
	if err = checkBashVersion(ctx, bashPath); err != nil {
		return err
	}
	// Start the server
	p := &serverProcess{}
	p.ctx, p.cancel = context.WithCancel(ctx)
	p.cmd = c.cmd(p.ctx, []string{"-D"})
	p.cmd.Cancel = func() error {
		// let tmux clean up (socket, clients) before resorting to SIGKILL
		return p.cmd.Process.Signal(syscall.SIGTERM)
	}
	// if the server has not exited serverKillDelay after Cancel has been called,
	// os/exec kills it (SIGKILL)
	p.cmd.WaitDelay = serverKillDelay
	// keep the server stderr to explain an early exit
	stderr := &limitedBuffer{limit: serverStderrMaxLen}
	p.cmd.Stderr = stderr
	c.server = p
	// Cleanup on error
	defer func() {
		if err != nil {
			p.cancel() // terminate the server if started
			if p.done != nil {
				<-p.done // wait for the watcher to reap the terminated process
			}
			c.resetServer()
		}
	}()
	if err = p.cmd.Start(); err != nil {
		return fmt.Errorf("failed to start server: %w", err)
	}
	// Watch the server: this goroutine is the only one calling Wait(), others use done
	done := make(chan struct{})
	p.done = done
	go func() {
		waitErr := p.cmd.Wait()
		if msg := stderr.String(); waitErr != nil && msg != "" {
			waitErr = fmt.Errorf("%w: %s", waitErr, msg)
		}
		p.waitErr = waitErr
		close(done)
	}()
	// Wait for it to be ready: the deadline also bounds a probe hanging
	readyCtx, readyCtxCancel := context.WithTimeout(ctx, serverStartTimeout)
	defer readyCtxCancel()
	ticker := time.NewTicker(serverReadyProbeInterval)
	defer ticker.Stop()
	serverPID := strconv.Itoa(p.cmd.Process.Pid)
	var probeErr error
readiness:
	for {
		// probe server, checking it is ours answering (and not a leftover one on the same socket)
		pid, err := c.socketServerPID(readyCtx)
		if err == nil {
			if pid == serverPID {
				break readiness
			}
			err = fmt.Errorf("%w: socket %s, tmux server pid %s (ours is %s)",
				ErrServerSocketInUse, c.socketName(), pid, serverPID)
		}
		if readyCtx.Err() == nil {
			// keep the last meaningful probe error, not the one of a probe interrupted by readyCtx
			probeErr = err
		}
		select {
		case <-ticker.C:
		case <-done:
			// A server already answering on the socket makes tmux -D connect to it as a client,
			// fail and exit ("not a terminal", which tells nothing): ask the socket instead. Ours
			// has exited, so a server answering is another one.
			inUseCtx, inUseCtxCancel := context.WithTimeout(ctx, serverStartTimeout)
			defer inUseCtxCancel()
			if pid, err := c.socketServerPID(inUseCtx); err == nil {
				return fmt.Errorf("server exited during startup: %w: socket %s, tmux server pid %s: %w",
					ErrServerSocketInUse, c.socketName(), pid, p.waitErr)
			}
			return fmt.Errorf("server exited during startup: %w", p.waitErr)
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
	// the tmux running the server, for the prompt hook of terminals
	tmuxPath, err := exec.LookPath("tmux")
	if err != nil {
		return fmt.Errorf("failed to configure server: %w", err)
	}
	p.promptCommand = promptCommand(tmuxPath)
	var args []string
	for _, option := range options {
		args = append(args, "set-option", "-g", option[0], option[1], ";")
	}
	for _, variable := range terminalEnvironment {
		args = append(args, "set-environment", "-g", variable[0], variable[1], ";")
	}
	for _, key := range windowKeys {
		args = append(args, "bind-key", "-T", "prefix", key[0], "switch-client", "-t", key[1], ";")
	}
	args = append(args, "set-environment", "-g", "PROMPT_COMMAND", p.promptCommand)
	if out, err := c.cmd(optsCtx, args).CombinedOutput(); err != nil {
		return fmt.Errorf("failed to configure server: %w: %s", err, strings.TrimSpace(string(out)))
	}
	return
}

// socketServerPID returns the PID of the tmux server answering on the socket of the tenant, if
// any answers: ours once started, or another one.
func (c *Controller) socketServerPID(ctx context.Context) (string, error) {
	out, err := c.cmd(ctx, []string{"display-message", "-p", "#{pid}"}).Output()
	if err != nil {
		return "", err
	}
	return strings.TrimSpace(string(out)), nil
}

// StopServer stops the current server if running, asking it to exit with kill-server, and
// terminating it if it has not exited within serverStopGracePeriod, stuck or not answering.
// A nil error means the server exited gracefully. Otherwise:
//   - ErrServerNotRunning: there was no server to stop, or it exited on its own, before or while
//     being stopped: a service manager stopping a service signals all its processes at once,
//     the server included
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
	p := c.server
	defer c.resetServer()
	// Has it exited on its own ?
	select {
	case <-p.done:
		return p.exitedOnItsOwn()
	default:
	}
	// from now on its exit is expected, whatever happens: WaitServer reports it as such
	p.stopRequested.Store(true)
	// Ask it to exit, and wait for it within the grace period, which also bounds kill-server: a
	// stuck server answers nothing, and ctx may have no deadline. kill-server failing does not
	// mean the server is stuck: it may be exiting already, on a signal of its own.
	graceCtx, cancelGrace := context.WithTimeout(ctx, serverStopGracePeriod)
	defer cancelGrace()
	killErr := c.cmd(graceCtx, []string{"kill-server"}).Run()
	select {
	case <-p.done:
	case <-graceCtx.Done():
		// terminate it through its context: SIGTERM, then SIGKILL after serverKillDelay
		p.cancel()
		<-p.done // wait for the watcher to reap the terminated process
		if killErr != nil {
			return fmt.Errorf("%w: kill-server failed: %w", p.forcedStopError(), killErr)
		}
		return fmt.Errorf("still running %s after kill-server command: %w", serverStopGracePeriod, p.forcedStopError())
	}
	if killErr != nil {
		// exited without having answered: on a signal of its own, not on our request
		return p.exitedOnItsOwn()
	}
	if p.waitErr != nil {
		// non-zero exit codes are reported as *exec.ExitError
		return fmt.Errorf("server exited with an error: %w", p.waitErr)
	}
	// clean stop
	return
}

// WaitServer waits for the current server to exit, and tells why. It returns nil when StopServer
// asked it to exit (StopServer reports how that went), and an error wrapping ErrServerNotRunning
// with the reason when it exited on its own: crash, killed, or kill-server typed in a terminal.
// It returns ErrServerNotRunning right away if no server is started, and ctx.Err() if ctx ends
// first. It does not restart the server: the caller decides, and StartServer replaces a server
// that exited on its own.
func (c *Controller) WaitServer(ctx context.Context) error {
	c.serverAction.RLock()
	p := c.server
	c.serverAction.RUnlock()
	if p == nil {
		return ErrServerNotRunning
	}
	select {
	case <-p.done:
	case <-ctx.Done():
		return ctx.Err()
	}
	if p.stopRequested.Load() {
		return nil
	}
	return p.exitedOnItsOwn()
}

// resetServer releases the server context resources and clears the server state.
// If the server is still running, canceling its context terminates it.
func (c *Controller) resetServer() {
	c.server.cancel()
	c.server = nil
}

// forcedStopError tells, once the server context has been canceled and done closed,
// if the server has exited on SIGTERM or had to be killed after serverKillDelay.
func (p *serverProcess) forcedStopError() error {
	if p.cmd.ProcessState == nil {
		// Wait failed before reaping the process: we can not tell how it stopped
		return fmt.Errorf("failed to wait for the server: %w", p.waitErr)
	}
	if ws, ok := p.cmd.ProcessState.Sys().(syscall.WaitStatus); ok && ws.Signaled() && ws.Signal() == syscall.SIGKILL {
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
