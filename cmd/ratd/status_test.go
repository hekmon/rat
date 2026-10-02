package main

import (
	"context"
	"log/slog"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/hekmon/rat/mtls"
	"github.com/hekmon/rat/tmux"

	"github.com/modelcontextprotocol/go-sdk/mcp"
)

// TestStatusRender guards the status line: the degradations first, in their order, the bundle
// expiry and the tmux deaths of the restart window, which age out; then the sessions most recently
// seen first, by name for the same time, at most statusSessions of them and a count of the others,
// in coarse times; the waits in flight; nothing for what is not there. Once stopping, the step of
// the stop alone.
func TestStatusRender(t *testing.T) {
	now := time.Now()
	s := newStatus(slog.New(slog.DiscardHandler))
	s.serving = "serving tenant prod on :7281"
	if line := s.render(now); line != "serving tenant prod on :7281" {
		t.Errorf("expected the serving part alone, got %q", line)
	}
	for session, ago := range map[string]time.Duration{
		"alice": 59 * time.Second, "bob": 25*time.Minute + 59*time.Second, "carol": 3 * time.Hour,
		"dave": 50 * time.Hour, "erin": 59 * time.Second, "frank": 60 * time.Hour, "grace": 70 * time.Hour,
	} {
		s.see(session, now.Add(-ago))
	}
	s.waitStarted()
	sessions := "last seen: alice now, erin now, bob 25m ago, carol 3h ago, dave 2d ago, +2 more"
	if line, expected := s.render(now), "serving tenant prod on :7281 · "+sessions+" · 1 wait in flight"; line != expected {
		t.Errorf("expected\n%q\ngot\n%q", expected, line)
	}
	s.waitStarted()
	if line := s.render(now); !strings.HasSuffix(line, "+2 more · 2 waits in flight") {
		t.Errorf("expected 2 waits in flight, got %q", line)
	}
	s.waitEnded()
	s.waitEnded()
	if line := s.render(now); !strings.HasSuffix(line, "+2 more") {
		t.Errorf("expected no wait in flight, got %q", line)
	}

	s.degrade(pasteOverridden, runningAsRoot)
	s.expires(now.Add(100*24*time.Hour + time.Hour))
	s.died(now.Add(-6*time.Minute), 5*time.Minute)
	s.died(now.Add(-4*time.Minute), 5*time.Minute)
	expected := "serving tenant prod on :7281 · running as root, bracketed paste overridden, bundle expires in 100 days, " +
		"tmux server died once in 5m · " + sessions
	if line := s.render(now); line != expected {
		t.Errorf("expected\n%q\ngot\n%q", expected, line)
	}
	s.died(now, 5*time.Minute)
	if line := s.render(now); !strings.Contains(line, "tmux server died 2 times in 5m") {
		t.Errorf("expected 2 deaths, got %q", line)
	}
	if line := s.render(now.Add(10 * time.Minute)); strings.Contains(line, "died") {
		t.Errorf("expected the deaths aged out, got %q", line)
	}
	if line := s.render(now.Add(101 * 24 * time.Hour)); !strings.Contains(line, "bundle expired") {
		t.Errorf("expected the bundle expired, got %q", line)
	}

	s.stop("closing the door")
	if line := s.render(now); line != "stopping: closing the door" {
		t.Errorf("expected the step of the stop alone, got %q", line)
	}
}

// TestDegradationLabels guards that every degradation has a label for the status.
func TestDegradationLabels(t *testing.T) {
	for d := range degradations {
		if d.String() == "" {
			t.Errorf("degradation %d has no label", d)
		}
	}
}

// TestStatusTell guards what systemd is told: nothing before ratd serves, systemd waiting for
// READY=1; then the line when it changed only, which the coarse times make seldom; a failure logged
// once, the line being retried at the next render.
func TestStatusTell(t *testing.T) {
	missing := filepath.Join(t.TempDir(), "missing")
	t.Setenv("NOTIFY_SOCKET", missing)
	logs := &logBuffer{}
	now := time.Now()
	s := newStatus(slog.New(slog.NewTextHandler(logs, nil)))
	s.notifier = takeNotifySocket()
	tell := func(at time.Time) {
		s.mu.Lock()
		defer s.mu.Unlock()
		s.tell(at)
	}
	s.degrade(runningAsRoot)
	if out := logs.String(); out != "" {
		t.Errorf("expected nothing told before serving, got:\n%s", out)
	}
	s.serving = "serving"
	tell(now)
	tell(now)
	if count := strings.Count(logs.String(), "failed to tell systemd the status of ratd"); count != 1 {
		t.Errorf("expected the failure logged once, got %d times:\n%s", count, logs)
	}

	next := listenNotify(t)
	s.notifier = takeNotifySocket()
	tell(now)
	if state := next(time.Second, ""); state != "STATUS=serving · running as root" {
		t.Errorf("expected the status retried, got %q", state)
	}
	s.see("alice", now)
	tell(now.Add(10 * time.Second))
	tell(now.Add(20 * time.Second))
	if state := next(time.Second, ""); state != "STATUS=serving · running as root · last seen: alice now" {
		t.Errorf("expected the status with alice, got %q", state)
	}
	if state := next(100*time.Millisecond, ""); state != "" {
		t.Errorf("expected nothing told of an unchanged status, got %q", state)
	}
	tell(now.Add(time.Minute))
	if state := next(time.Second, ""); state != "STATUS=serving · running as root · last seen: alice 1m ago" {
		t.Errorf("expected the status with alice seen a minute ago, got %q", state)
	}
	s.stop("closing the door")
	if state := next(time.Second, ""); state != "STATUS=stopping: closing the door" {
		t.Errorf("expected the stop told at once, got %q", state)
	}
}

// TestStatusCalls guards where the status learns of calls: every request of a session, and each
// call of wait_window while it waits.
func TestStatusCalls(t *testing.T) {
	requireTmux(t)
	testHome(t)
	controller, err := tmux.New("test-ratd-status")
	if err != nil {
		t.Fatal(err)
	}
	if err = controller.StartServer(context.Background()); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = controller.StopServer(context.Background()) })
	d, err := newDaemon(slog.New(slog.DiscardHandler), &mtls.Side{Tenant: "t"}, controller, terminalsHold, defaultReadBudget)
	if err != nil {
		t.Fatal(err)
	}
	session := connectTo(t, d)
	line := func() string {
		d.status.mu.Lock()
		defer d.status.mu.Unlock()
		return d.status.render(time.Now())
	}
	if !strings.Contains(line(), "last seen: "+toolsSession+" now") {
		t.Errorf("expected %s seen once connected, got %q", toolsSession, line())
	}
	expectTool(t, session, "wait_window", map[string]any{"window": "main"}, false, "main: bash is at its prompt")
	expectTool(t, session, "send_text", map[string]any{"window": "main", "text": "sleep 30", "enter": true}, false)
	waited := make(chan error, 1)
	go func() {
		_, err := session.CallTool(context.Background(), &mcp.CallToolParams{Name: "wait_window",
			Arguments: map[string]any{"window": "main", "max_seconds": 1}})
		waited <- err
	}()
	eventually(t, "wait in flight", func() bool { return strings.HasSuffix(line(), " · 1 wait in flight") })
	if err = <-waited; err != nil {
		t.Fatal(err)
	}
	if l := line(); strings.Contains(l, "wait") {
		t.Errorf("expected no wait in flight once it returned, got %q", l)
	}
}
