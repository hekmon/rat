package tmux

import (
	"context"
	"errors"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
	"testing"
	"time"
)

func TestServerLifecycle(t *testing.T) {
	c := newTestController(t, "lifecycle")
	if err := c.StartServer(context.Background()); err != nil {
		t.Fatal(err)
	}
	if err := c.StartServer(context.Background()); !errors.Is(err, ErrServerAlreadyStarted) {
		t.Fatalf("expected ErrServerAlreadyStarted, got %v", err)
	}
	if err := c.StopServer(context.Background()); err != nil {
		t.Fatal(err)
	}
	if err := c.StopServer(context.Background()); !errors.Is(err, ErrServerNotRunning) {
		t.Fatalf("expected ErrServerNotRunning, got %v", err)
	}
	// restart on the same controller
	if err := c.StartServer(context.Background()); err != nil {
		t.Fatal(err)
	}
	if err := c.StopServer(context.Background()); err != nil {
		t.Fatal(err)
	}
}

func TestServerSocketInUse(t *testing.T) {
	c := newTestController(t, "inuse")
	// another server already listening on our socket
	foreign := c.cmd(context.Background(), []string{"-D"})
	if err := foreign.Start(); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		_ = c.cmd(context.Background(), []string{"kill-server"}).Run()
		_ = foreign.Wait()
	})
	time.Sleep(200 * time.Millisecond)
	start := time.Now()
	err := c.StartServer(context.Background())
	if err == nil {
		t.Fatal("expected an error")
	}
	if elapsed := time.Since(start); elapsed > serverStartTimeout/2 {
		t.Errorf("early exit detected after %s, expected the watcher to report it right away", elapsed)
	}
	if !strings.Contains(err.Error(), "not a terminal") {
		t.Errorf("expected tmux stderr in the error, got: %v", err)
	}
	if c.server != nil {
		t.Error("server state not reset")
	}
	// the other server must be left untouched
	if err := c.cmd(context.Background(), []string{"display-message", "-p", "ok"}).Run(); err != nil {
		t.Errorf("foreign server affected: %v", err)
	}
}

func TestServerStartCanceled(t *testing.T) {
	c := newTestController(t, "canceled")
	ctx, cancel := context.WithCancel(context.Background())
	time.AfterFunc(5*time.Millisecond, cancel)
	if err := c.StartServer(ctx); err == nil {
		t.Fatal("expected an error")
	}
	if c.server != nil {
		t.Error("server state not reset")
	}
}

func TestServerExitedOnItsOwn(t *testing.T) {
	c := newTestController(t, "exited")
	if err := c.StartServer(context.Background()); err != nil {
		t.Fatal(err)
	}
	// StartServer notices the dead server and starts a new one
	_ = c.server.Process.Signal(syscall.SIGKILL)
	<-c.serverDone
	if err := c.StartServer(context.Background()); err != nil {
		t.Fatal(err)
	}
	// StopServer reports it was not running anymore
	_ = c.server.Process.Signal(syscall.SIGKILL)
	<-c.serverDone
	if err := c.StopServer(context.Background()); !errors.Is(err, ErrServerNotRunning) {
		t.Fatalf("expected ErrServerNotRunning, got %v", err)
	}
}

func TestServerStopTerminated(t *testing.T) {
	c := newTestController(t, "terminated")
	if err := c.StartServer(context.Background()); err != nil {
		t.Fatal(err)
	}
	c.serverCtxCancel() // what StopServer does when kill-server fails or the grace period is over
	<-c.serverDone
	if err := c.forcedStopError(); !errors.Is(err, ErrServerTerminated) {
		t.Fatalf("expected ErrServerTerminated, got %v", err)
	}
}

func TestServerStopKilled(t *testing.T) {
	c := newTestController(t, "killed")
	if err := c.StartServer(context.Background()); err != nil {
		t.Fatal(err)
	}
	// a frozen server can not handle kill-server nor SIGTERM: only SIGKILL gets rid of it
	_ = c.server.Process.Signal(syscall.SIGSTOP)
	ctx, cancel := context.WithTimeout(context.Background(), 500*time.Millisecond)
	defer cancel()
	if err := c.StopServer(ctx); !errors.Is(err, ErrServerKilled) {
		t.Fatalf("expected ErrServerKilled, got %v", err)
	}
}

func TestServerStartFailureStderr(t *testing.T) {
	requireTmux(t)
	// bypass New validation to make tmux fail creating its socket
	c := &Controller{tenant: "test/no/such/dir"}
	err := c.StartServer(context.Background())
	if err == nil {
		t.Fatal("expected an error")
	}
	if !strings.Contains(err.Error(), "No such file or directory") {
		t.Errorf("expected tmux stderr in the error, got: %v", err)
	}
}

func TestLimitedBuffer(t *testing.T) {
	lb := &limitedBuffer{limit: 5}
	for _, p := range []string{"abc", "defgh", "ij"} {
		if n, err := lb.Write([]byte(p)); n != len(p) || err != nil {
			t.Fatalf("Write(%q) = %d, %v", p, n, err)
		}
	}
	if got := lb.String(); got != "abcde" {
		t.Errorf("got %q, expected %q", got, "abcde")
	}
}

// TestServerIgnoresUserConfig guards that the server never loads the user tmux configuration,
// which could change its behavior (prefix, base-index, hooks, plugins...).
func TestServerIgnoresUserConfig(t *testing.T) {
	c := newTestController(t, "userconfig")
	home := t.TempDir()
	config := "set -g @rat-test loaded\nset -g history-limit 42\n"
	if err := os.WriteFile(filepath.Join(home, ".tmux.conf"), []byte(config), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(filepath.Join(home, ".config", "tmux"), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(home, ".config", "tmux", "tmux.conf"), []byte(config), 0o600); err != nil {
		t.Fatal(err)
	}
	t.Setenv("HOME", home)
	t.Setenv("XDG_CONFIG_HOME", filepath.Join(home, ".config"))
	if err := c.StartServer(context.Background()); err != nil {
		t.Fatal(err)
	}
	if out, err := c.cmd(context.Background(), []string{"show-options", "-gv", "@rat-test"}).CombinedOutput(); err == nil {
		t.Errorf("user configuration loaded: @rat-test=%s", strings.TrimSpace(string(out)))
	}
}

// TestServerOptions guards the options and terminal environment rat relies on, set on top of
// the tmux defaults.
func TestServerOptions(t *testing.T) {
	c := newTestController(t, "options")
	if err := c.StartServer(context.Background()); err != nil {
		t.Fatal(err)
	}
	bashPath, err := exec.LookPath("bash")
	if err != nil {
		t.Fatal(err)
	}
	for option, expected := range map[string]string{
		"default-shell": bashPath,
		"history-limit": strconv.Itoa(serverHistoryLimit),
		"default-size":  serverDefaultSize,
		// tmux default: a global manual crashes tmux 3.3 to 3.6, windows get it one by one
		"window-size":  "latest",
		"allow-rename": "off",
	} {
		out, err := c.cmd(context.Background(), []string{"show-options", "-gv", option}).Output()
		if err != nil {
			t.Fatalf("%s: %v", option, err)
		}
		if got := strings.TrimSpace(string(out)); got != expected {
			t.Errorf("%s = %q, expected %q", option, got, expected)
		}
	}
	for variable, expected := range map[string]string{
		"BASH_SILENCE_DEPRECATION_WARNING": "1",
		"LC_ALL":                           "C.UTF-8",
		"PAGER":                            "cat",
		"GIT_PAGER":                        "cat",
		"MANPAGER":                         "cat",
		"SYSTEMD_PAGER":                    "cat",
		"PROMPT_COMMAND":                   `bind "set enable-bracketed-paste on"`,
	} {
		out, err := c.cmd(context.Background(), []string{"show-environment", "-g", variable}).Output()
		if err != nil {
			t.Fatalf("%s: %v", variable, err)
		}
		if got := strings.TrimSpace(string(out)); got != variable+"="+expected {
			t.Errorf("got %q, expected %s=%s", got, variable, expected)
		}
	}
}

// TestServerRequiresBash guards that a missing bash is reported by StartServer,
// before any server is started, instead of failing on each new terminal.
func TestServerRequiresBash(t *testing.T) {
	c := newTestController(t, "nobash")
	t.Setenv("PATH", t.TempDir())
	err := c.StartServer(context.Background())
	if !errors.Is(err, exec.ErrNotFound) || !strings.Contains(err.Error(), "bash") {
		t.Fatalf("expected a missing bash error, got %v", err)
	}
	if c.server != nil {
		t.Error("server started without bash")
	}
}

// TestServerRequiresRecentBash guards that a bash older than bashMinVersion (such as the 3.2 of
// macOS) is reported by StartServer, before any server is started: pasted text would run line by
// line in its terminals.
func TestServerRequiresRecentBash(t *testing.T) {
	c := newTestController(t, "oldbash")
	t.Setenv("PATH", fakeBash(t, "3.2.57(1)-release (arm64-apple-darwin25)"))
	err := c.StartServer(context.Background())
	if !errors.Is(err, ErrUnsupportedBash) || !strings.Contains(err.Error(), "3.2") {
		t.Fatalf("expected ErrUnsupportedBash, got %v", err)
	}
	if c.server != nil {
		t.Error("server started with an unsupported bash")
	}
	for version, supported := range map[string]bool{
		"4.3.48(1)-release": false, "4.4.23(1)-release": true, "5.0.18(1)-release": true,
		"5.3.20(1)-release": true, "10.0.0(1)-release": true,
	} {
		dir := fakeBash(t, version)
		if err := checkBashVersion(context.Background(), filepath.Join(dir, "bash")); (err == nil) != supported {
			t.Errorf("bash %s: supported %v, got %v", version, supported, err)
		}
	}
}

// fakeBash returns a directory holding a bash that only prints version as bash --version does.
func fakeBash(t *testing.T, version string) string {
	t.Helper()
	dir := t.TempDir()
	script := "#!/bin/sh\necho 'GNU bash, version " + version + "'\necho 'Copyright (C) 2007 Free Software Foundation, Inc.'\n"
	if err := os.WriteFile(filepath.Join(dir, "bash"), []byte(script), 0o700); err != nil {
		t.Fatal(err)
	}
	return dir
}

// TestServerWindowSizeFixed guards that a client attaching, such as a human inspecting the
// terminals, does not resize them: neither while attached, nor once detached. This holds for the
// first window of a session as for the ones created after it.
func TestServerWindowSizeFixed(t *testing.T) {
	c := startTestServer(t, "windowsize")
	ctx := context.Background()
	if err := c.NewSession(ctx, "s"); err != nil {
		t.Fatal(err)
	}
	if err := c.NewWindow(ctx, "s", "w"); err != nil {
		t.Fatal(err)
	}
	checkSize := func(when string) {
		t.Helper()
		out, err := c.run(ctx, "list-windows", "-t", "=s", "-F", "#{window_name} #{window_width}x#{window_height}")
		if err != nil {
			t.Fatal(err)
		}
		expected := FirstWindow + " " + serverDefaultSize + "\nw " + serverDefaultSize
		if got := strings.TrimSpace(out); got != expected {
			t.Errorf("%s: sizes are %q, expected %q", when, got, expected)
		}
	}
	checkSize("before attach")
	// a control mode client acting as a human terminal of 100x30
	client := c.cmd(context.Background(), []string{"-C", "attach-session", "-t", "=s"})
	stdin, err := client.StdinPipe()
	if err != nil {
		t.Fatal(err)
	}
	if err = client.Start(); err != nil {
		t.Fatal(err)
	}
	if _, err = io.WriteString(stdin, "refresh-client -C 100x30\n"); err != nil {
		t.Fatal(err)
	}
	time.Sleep(300 * time.Millisecond)
	checkSize("client attached")
	if _, err = io.WriteString(stdin, "detach-client\n"); err != nil {
		t.Fatal(err)
	}
	_ = stdin.Close()
	_ = client.Wait()
	checkSize("client detached")
}
