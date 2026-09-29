package tmux

import (
	"context"
	"errors"
	"os/exec"
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
	if _, err := exec.LookPath("tmux"); err != nil {
		t.Skip("tmux not installed")
	}
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
