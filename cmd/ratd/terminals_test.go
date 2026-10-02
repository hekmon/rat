package main

import (
	"context"
	"errors"
	"io"
	"log/slog"
	"net"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/hekmon/rat/connect"
	"github.com/hekmon/rat/mtls"
	"github.com/hekmon/rat/tmux"

	"github.com/modelcontextprotocol/go-sdk/mcp"
)

// requireTmux fails the test if tmux is not installed: ratd can not work without it, and a test
// run that tested nothing must not pass.
func requireTmux(t *testing.T) {
	t.Helper()
	if _, err := exec.LookPath("tmux"); err != nil {
		t.Fatal("tmux is required to run the tests:", err)
	}
}

// eventually fails the test if condition does not become true within 5 seconds.
func eventually(t *testing.T, what string, condition func() bool) {
	t.Helper()
	for deadline := time.Now().Add(5 * time.Second); time.Now().Before(deadline); time.Sleep(50 * time.Millisecond) {
		if condition() {
			return
		}
	}
	t.Fatalf("%s: never happened", what)
}

// runRatd runs ratd with the server directory of bundle, on a loopback port, until stop is
// called, which returns the error of run. It returns the address and the logs of ratd.
func runRatd(t *testing.T, bundle string) (addr string, logs *logBuffer, stop func() error) {
	t.Helper()
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	logs = &logBuffer{}
	ctx, cancel := context.WithCancel(context.Background())
	ran := make(chan error, 1)
	go func() {
		ran <- run(ctx, slog.New(slog.NewTextHandler(logs, nil)), mtls.ServerDir(bundle), defaultReadBudget,
			func() (net.Listener, error) { return listener, nil })
	}()
	stopped := false
	var runErr error
	stop = func() error {
		if !stopped {
			stopped = true
			cancel()
			runErr = <-ran
		}
		return runErr
	}
	t.Cleanup(func() { _ = stop() })
	return listener.Addr().String(), logs, stop
}

// TestRun guards the lifecycle of ratd: it serves once the tmux server of its tenant runs, a
// second ratd for the same tenant fails before listening, and stopping closes the door before
// stopping the terminals, leaving no tmux server behind.
func TestRun(t *testing.T) {
	requireTmux(t)
	const tenant = "test-ratd-run"
	bundle := newBundle(t, tenant)
	addr, logs, stop := runRatd(t, bundle)
	if resp, err := post(t, httpClient(t, bundle, "alice"), addr, addr, connect.Path); err != nil || resp.StatusCode != http.StatusOK {
		t.Fatalf("expected ratd to serve, got %v, %v\n%s", resp, err, logs)
	}

	listened := false
	err := run(context.Background(), slog.New(slog.DiscardHandler), mtls.ServerDir(bundle), defaultReadBudget, func() (net.Listener, error) {
		listened = true
		return nil, errors.New("not expected to listen")
	})
	if !errors.Is(err, tmux.ErrServerSocketInUse) || listened {
		t.Errorf("second ratd: expected tmux.ErrServerSocketInUse before listening, got %v (listened: %v)", err, listened)
	}

	if err = stop(); err != nil {
		t.Fatal(err)
	}
	out := logs.String()
	door, terminals := strings.Index(out, `msg="door closed"`), strings.Index(out, `msg="terminals stopped"`)
	if door < 0 || terminals < door {
		t.Errorf("expected the door to close before the terminals stop:\n%s", out)
	}
	// no server left on the socket of the tenant: a new one starts there
	c, err := tmux.New(tenant)
	if err != nil {
		t.Fatal(err)
	}
	if err = c.StartServer(context.Background()); err != nil {
		t.Errorf("expected no tmux server left, got %v", err)
	}
	_ = c.StopServer(context.Background())
}

// TestRunCallInFlight guards the order of the shutdown: the door closes before the terminals
// stop, so that a call in flight when ratd stops still reaches them, rather than being told they
// are restarting. The call is held in flight inside tmux, its server stopped by SIGSTOP until the
// door is closed. Were the terminals stopped with the door, the SIGTERM sent to the server would
// end it as soon as it resumes, failing the call.
func TestRunCallInFlight(t *testing.T) {
	requireTmux(t)
	const tenant = "test-ratd-inflight"
	bundle := newBundle(t, tenant)
	addr, logs, stop := runRatd(t, bundle)
	// serving: the tmux server runs
	if resp, err := post(t, httpClient(t, bundle, "alice"), addr, addr, connect.Path); err != nil || resp.StatusCode != http.StatusOK {
		t.Fatalf("expected ratd to serve, got %v, %v\n%s", resp, err, logs)
	}
	out, err := exec.Command("tmux", "-L", "rat-"+tenant, "display-message", "-p", "#{pid}").Output()
	if err != nil {
		t.Fatal(err)
	}
	pid := strings.TrimSpace(string(out))
	if err = exec.Command("kill", "-STOP", pid).Run(); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = exec.Command("kill", "-CONT", pid).Run() })

	type response struct {
		status int
		body   string
		err    error
	}
	responses := make(chan response, 1)
	go func() {
		call := `{"jsonrpc":"2.0","id":1,"method":"tools/call","params":{"name":"list_windows","arguments":{}}}`
		req, err := http.NewRequest(http.MethodPost, "https://"+addr+connect.Path, strings.NewReader(call))
		if err != nil {
			responses <- response{err: err}
			return
		}
		req.Header.Set("Content-Type", "application/json")
		req.Header.Set("Accept", "application/json, text/event-stream")
		req.Header.Set("Mcp-Protocol-Version", "2025-06-18")
		resp, err := httpClient(t, bundle, "alice").Do(req)
		if err != nil {
			responses <- response{err: err}
			return
		}
		defer resp.Body.Close()
		data, err := io.ReadAll(resp.Body)
		responses <- response{status: resp.StatusCode, body: string(data), err: err}
	}()
	eventually(t, "call blocked in tmux", func() bool {
		return exec.Command("pgrep", "-f", "rat-"+tenant+" .*list-windows").Run() == nil
	})
	stopped := make(chan error, 1)
	go func() { stopped <- stop() }()
	eventually(t, "door closed to new connections", func() bool {
		conn, err := net.Dial("tcp", addr)
		if err == nil {
			_ = conn.Close()
		}
		return err != nil
	})
	if err = exec.Command("kill", "-CONT", pid).Run(); err != nil {
		t.Fatal(err)
	}
	resp := <-responses
	if resp.err != nil || resp.status != http.StatusOK || !strings.Contains(resp.body, "main: bash in ") {
		t.Errorf("expected the call in flight to reach the terminals, got %d, %v: %s\n%s", resp.status, resp.err, resp.body, logs)
	}
	if err = <-stopped; err != nil {
		t.Error(err)
	}
}

// TestRunGivesUp guards that ratd exits with an error, closing the door, when its tmux server dies
// too often: whoever monitors the service sees it failed.
func TestRunGivesUp(t *testing.T) {
	requireTmux(t)
	policy := defaultRestartPolicy
	defaultRestartPolicy = restartPolicy{delay: 10 * time.Millisecond, deaths: 2, window: time.Minute}
	t.Cleanup(func() { defaultRestartPolicy = policy })
	const tenant = "test-ratd-gives-up"
	bundle := newBundle(t, tenant)
	addr, logs, stop := runRatd(t, bundle)
	if resp, err := post(t, httpClient(t, bundle, "alice"), addr, addr, connect.Path); err != nil || resp.StatusCode != http.StatusOK {
		t.Fatalf("expected ratd to serve, got %v, %v\n%s", resp, err, logs)
	}
	// killed from outside ratd, on the socket of the tenant (rat-<tenant>, see the tmux README)
	kill := func() { _ = exec.Command("tmux", "-L", "rat-"+tenant, "kill-server").Run() }
	kill()
	eventually(t, "server restarted", func() bool { return strings.Contains(logs.String(), `msg="tmux server restarted"`) })
	kill()
	// ratd stops by itself: stop only collects what run returned
	eventually(t, "door closed", func() bool { return strings.Contains(logs.String(), `msg="door closed"`) })
	if err := stop(); err == nil || !strings.Contains(err.Error(), "died 2 times") {
		t.Errorf("expected ratd to exit with an error after 2 deaths, got %v:\n%s", err, logs)
	}
}

// TestRunTerminalsWarnings guards that the admin is warned of what bash startup files defeat in
// the terminals, and agents told to send one line at a time when bracketed paste is defeated, or
// when the check could not run (startup files blocking it block every terminal as well): in the
// instructions, and in the description of send_text, which then no longer tells that text pasted at
// a prompt waits for Enter. An inputrc turns bracketed paste off, so that only rat's command turns
// it on, whatever the default of the bash running the tests (on from 5.1).
func TestRunTerminalsWarnings(t *testing.T) {
	requireTmux(t)
	inputrc := filepath.Join(t.TempDir(), "inputrc")
	if err := os.WriteFile(inputrc, []byte("set enable-bracketed-paste off\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	t.Setenv("INPUTRC", inputrc)
	timeout := terminalsCheckTimeout
	terminalsCheckTimeout = time.Second
	t.Cleanup(func() { terminalsCheckTimeout = timeout })
	for _, tc := range []struct {
		name, profile string
		logs          []string
		pasteWarning  bool
	}{
		{"override", "PROMPT_COMMAND=true\n", []string{"bash startup files override bracketed paste",
			"bash startup files replace PROMPT_COMMAND"}, true},
		{"status changed", "PROMPT_COMMAND=\"history -a;$PROMPT_COMMAND\"\n",
			[]string{"bash startup files run a command changing the exit status"}, false},
		{"pipeline lost", "__keep() { local s=$?; true; return $s; }; PROMPT_COMMAND=\"__keep;$PROMPT_COMMAND\"\n",
			[]string{"ratd does not tell the status of each command of a pipeline"}, false},
		{"blocking", "sleep 3\n", []string{"bash startup files did not finish"}, true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			home := t.TempDir()
			if err := os.WriteFile(filepath.Join(home, ".bash_profile"), []byte(tc.profile), 0o600); err != nil {
				t.Fatal(err)
			}
			t.Setenv("HOME", home)
			bundle := newBundle(t, "test-ratd-terminals")
			addr, logs, _ := runRatd(t, bundle)
			client := mcp.NewClient(&mcp.Implementation{Name: "test", Version: "1"}, nil)
			session, err := client.Connect(context.Background(), &mcp.StreamableClientTransport{
				Endpoint:   "https://" + addr + connect.Path,
				HTTPClient: httpClient(t, bundle, "alice"),
			}, nil)
			if err != nil {
				t.Fatalf("%v\n%s", err, logs)
			}
			defer session.Close()
			if warned := strings.HasSuffix(session.InitializeResult().Instructions, " "+pasteWarning); warned != tc.pasteWarning {
				t.Errorf("expected the instructions to warn about pasted text: %v, got %q", tc.pasteWarning,
					session.InitializeResult().Instructions)
			}
			list, err := session.ListTools(context.Background(), nil)
			if err != nil {
				t.Fatal(err)
			}
			for _, tool := range list.Tools {
				if tool.Name != "send_text" {
					continue
				}
				warned := strings.Contains(tool.Description, pasteWarning)
				waits := strings.Contains(tool.Description, "nothing runs until Enter")
				if warned != tc.pasteWarning || waits == tc.pasteWarning {
					t.Errorf("expected send_text to warn about pasted text: %v, got %q", tc.pasteWarning, tool.Description)
				}
			}
			for _, log := range tc.logs {
				if !strings.Contains(logs.String(), log) {
					t.Errorf("expected the warning %q to be logged:\n%s", log, logs)
				}
			}
		})
	}
}

// startSupervised starts a tmux server for tenant, supervised with policy until the test ends,
// and returns its controller, the logs of the supervisor, and the channel receiving its result.
func startSupervised(t *testing.T, tenant string, policy restartPolicy) (*tmux.Controller, *logBuffer, <-chan error) {
	t.Helper()
	requireTmux(t)
	c, err := tmux.New(tenant)
	if err != nil {
		t.Fatal(err)
	}
	if err = c.StartServer(context.Background()); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = c.StopServer(context.Background()) })
	logs := &logBuffer{}
	ctx, cancel := context.WithCancel(context.Background())
	supervised := make(chan error, 1)
	go func() {
		supervised <- supervise(ctx, slog.New(slog.NewTextHandler(logs, nil)), c, context.Background(), policy)
	}()
	t.Cleanup(cancel)
	return c, logs, supervised
}

// killServer kills the tmux server of c as an agent could: tmux kill-server typed in a terminal
// reaches rat's server, TMUX being kept. It returns once the server exited.
func killServer(t *testing.T, c *tmux.Controller) {
	t.Helper()
	ctx := context.Background()
	if err := c.NewSession(ctx, "s"); err != nil {
		t.Fatal(err)
	}
	if err := c.SendText(ctx, "s", tmux.FirstWindow, "tmux kill-server", true); err != nil {
		t.Fatal(err)
	}
	waitCtx, cancel := context.WithTimeout(ctx, 5*time.Second)
	defer cancel()
	if err := c.WaitServer(waitCtx); !errors.Is(err, tmux.ErrServerNotRunning) {
		t.Fatalf("expected the server to exit on its own, got %v", err)
	}
}

// TestSupervise guards that a tmux server exiting on its own is restarted, and that dying too
// often makes the supervisor give up, for ratd to exit.
func TestSupervise(t *testing.T) {
	c, logs, supervised := startSupervised(t, "test-ratd-supervise", restartPolicy{delay: 10 * time.Millisecond, deaths: 3, window: time.Minute})
	for restarts := 1; restarts <= 2; restarts++ {
		killServer(t, c)
		eventually(t, "server restarted", func() bool {
			_, err := c.ListSessions(context.Background())
			return err == nil && strings.Count(logs.String(), `msg="tmux server restarted"`) == restarts
		})
	}
	killServer(t, c)
	select {
	case err := <-supervised:
		if err == nil || !strings.Contains(err.Error(), "died 3 times") {
			t.Errorf("expected the supervisor to give up after 3 deaths, got %v", err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("the supervisor did not give up")
	}
}

// TestSuperviseRestartFailing guards that a restart failing counts as a death: here a stray tmux
// server holds the socket, which the error names.
func TestSuperviseRestartFailing(t *testing.T) {
	const tenant = "test-ratd-stray"
	c, logs, supervised := startSupervised(t, tenant, restartPolicy{delay: time.Second, deaths: 2, window: time.Minute})
	killServer(t, c)
	// within the delay before the restart
	stray, err := tmux.New(tenant)
	if err != nil {
		t.Fatal(err)
	}
	if err = stray.StartServer(context.Background()); err != nil {
		t.Fatal(err)
	}
	defer func() { _ = stray.StopServer(context.Background()) }()
	select {
	case err := <-supervised:
		if err == nil || !strings.Contains(logs.String(), "socket already served by another tmux server") {
			t.Errorf("expected a failed restart, counted as a death, got %v:\n%s", err, logs)
		}
	case <-time.After(5 * time.Second):
		t.Fatalf("the supervisor did not give up:\n%s", logs)
	}
}
