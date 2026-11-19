package main

import (
	"context"
	"errors"
	"fmt"
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
// in coarse times; the waits, then the file transfers, in flight; nothing for what is not there.
// Once stopping, the step of the stop alone.
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
	s.transferStarted()
	if line := s.render(now); !strings.HasSuffix(line, "+2 more · 1 transfer in flight") {
		t.Errorf("expected 1 transfer in flight, got %q", line)
	}
	s.waitStarted()
	s.transferStarted()
	if line := s.render(now); !strings.HasSuffix(line, "+2 more · 1 wait in flight · 2 transfers in flight") {
		t.Errorf("expected 1 wait and 2 transfers in flight, got %q", line)
	}
	s.waitEnded()
	s.transferEnded()
	s.transferEnded()
	if line := s.render(now); !strings.HasSuffix(line, "+2 more") {
		t.Errorf("expected nothing in flight, got %q", line)
	}

	s.degrade(pasteOverridden, runningAsRoot)
	s.expires(now.Add(100*24*time.Hour + time.Hour))
	s.died(now.Add(-6*time.Minute), 5*time.Minute)
	s.died(now.Add(-4*time.Minute), 5*time.Minute)
	expected := "serving tenant prod on :7281 · running as root, bracketed paste overridden, bundle expires in 100d, " +
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
	// the last day in hours, rather than 0 days
	if line := s.render(now.Add(100 * 24 * time.Hour)); !strings.Contains(line, "bundle expires in 1h") {
		t.Errorf("expected the last hour told, got %q", line)
	}
	if line := s.render(now.Add(101 * 24 * time.Hour)); !strings.Contains(line, "bundle expired") {
		t.Errorf("expected the bundle expired, got %q", line)
	}

	s.stop("closing the door")
	if line := s.render(now); line != "stopping: closing the door" {
		t.Errorf("expected the step of the stop alone, got %q", line)
	}
}

// recordingSource records the events status registers, for the test to tell them.
type recordingSource struct {
	crl   mtls.CRLEvents
	watch mtls.WatchEvents
}

func (r *recordingSource) OnCRL(events mtls.CRLEvents)     { r.crl = events }
func (r *recordingSource) OnWatch(events mtls.WatchEvents) { r.watch = events }

// TestWatchStatus guards that the status follows what happens to the CRL and to the files of the
// side in use, as ratd registers them: a fetch failing, with when the CRL held goes stale or none
// held, and succeeding again (the CRL held or a newer one) clearing it; a renewal, the files
// refused for a new CA pending a restart or another reason, and restored.
func TestWatchStatus(t *testing.T) {
	now := time.Now()
	s := newStatus(slog.New(slog.DiscardHandler))
	s.serving = "serving"
	var source recordingSource
	watchStatus(&source, s)
	if source.crl.Succeeded == nil || source.crl.Failed == nil || source.watch.Reloaded == nil || source.watch.Refused == nil {
		t.Fatal("expected the status to be told of the CRL and of the files")
	}
	line := func() string {
		s.mu.Lock()
		defer s.mu.Unlock()
		return s.render(now)
	}
	source.crl.Failed(errors.New("the PKI is down"), &mtls.CRL{NextUpdate: now.Add(3*time.Hour + time.Minute)})
	if l := line(); !strings.Contains(l, "CRL not renewed, stale in 3h") {
		t.Errorf("fetch failing: expected when the CRL held goes stale, got %q", l)
	}
	// the same CRL fetched again clears it: what failed was reaching the PKI
	source.crl.Succeeded(nil)
	if l := line(); strings.Contains(l, "CRL") {
		t.Errorf("fetch succeeding again: expected nothing of the CRL, got %q", l)
	}
	source.crl.Failed(errors.New("the PKI is down"), nil)
	if l := line(); !strings.Contains(l, "no valid CRL, every client refused") {
		t.Errorf("no CRL held: expected every client refused, got %q", l)
	}
	source.crl.Succeeded(nil)
	if l := line(); strings.Contains(l, "CRL") {
		t.Errorf("first CRL fetched: expected nothing of the CRL, got %q", l)
	}
	source.watch.Reloaded(nil, true)
	if l := line(); !strings.Contains(l, "server key changed now") {
		t.Errorf("renewal with a new key: expected it shown, got %q", l)
	}
	source.watch.Refused(fmt.Errorf("%w: ca.crt holds another CA", mtls.ErrCAChanged))
	if l := line(); !strings.Contains(l, "CA changed, restart pending") {
		t.Errorf("new CA: expected a restart pending, got %q", l)
	}
	source.watch.Refused(errors.New("a file missing"))
	if l := line(); !strings.Contains(l, "bundle files refused") {
		t.Errorf("files refused: expected them shown, got %q", l)
	}
	source.watch.Refused(nil)
	if l := line(); strings.Contains(l, "bundle files") || strings.Contains(l, "CA changed") {
		t.Errorf("files restored: expected nothing of them, got %q", l)
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

// TestStatusCRL guards what the status tells of the CRL of the CA: a renewal failing, with when
// the CRL held goes stale, every client refused once it did or without one held, and nothing
// once a fetch succeeds again, the CRL held or a newer one.
func TestStatusCRL(t *testing.T) {
	now := time.Now()
	s := newStatus(slog.New(slog.DiscardHandler))
	s.serving = "serving"
	s.crlFailed(&mtls.CRL{NextUpdate: now.Add(3*time.Hour + time.Minute)})
	if line := s.render(now); !strings.Contains(line, "CRL not renewed, stale in 3h") {
		t.Errorf("renewal failing: expected when the CRL goes stale, got %q", line)
	}
	if line := s.render(now.Add(4 * time.Hour)); !strings.Contains(line, "no valid CRL, every client refused") {
		t.Errorf("CRL stale: expected every client refused, got %q", line)
	}
	s.crlSucceeded()
	if line := s.render(now); strings.Contains(line, "CRL") {
		t.Errorf("recovered: expected nothing of the CRL, got %q", line)
	}
	s.crlFailed(nil)
	if line := s.render(now); !strings.Contains(line, "no valid CRL, every client refused") {
		t.Errorf("no CRL held: expected every client refused, got %q", line)
	}
}

// TestStatusBundleFiles guards what the status tells of the files of the server directory
// changing: a new CA pending a restart, other files refused, nothing once restored, and a renewal
// with a new key for a day.
func TestStatusBundleFiles(t *testing.T) {
	now := time.Now()
	s := newStatus(slog.New(slog.DiscardHandler))
	s.serving = "serving"
	for _, tc := range []struct {
		err      error
		expected string
	}{
		{fmt.Errorf("%w: ca.crt holds another CA", mtls.ErrCAChanged), "CA changed, restart pending"},
		{fmt.Errorf("%w: another client", mtls.ErrIdentityChanged), "bundle files refused"},
		{nil, ""},
	} {
		s.refused(tc.err)
		if line := s.render(now); (tc.expected == "" && line != "serving") || !strings.Contains(line, tc.expected) {
			t.Errorf("refused %v: expected %q, got %q", tc.err, tc.expected, line)
		}
	}
	s.renewed(now, true)
	if line := s.render(now.Add(2 * time.Hour)); !strings.Contains(line, "server key changed 2h ago") {
		t.Errorf("new key: expected it told, got %q", line)
	}
	if line := s.render(now.Add(25 * time.Hour)); strings.Contains(line, "key") {
		t.Errorf("new key a day ago: expected nothing of it, got %q", line)
	}
}

// TestLeftText guards the coarse delays the status tells: days beyond two, hours beyond one,
// minutes beyond one, and under a minute rather than 0m, read as none left.
func TestLeftText(t *testing.T) {
	for left, expected := range map[time.Duration]string{
		3 * 24 * time.Hour: "3d", 47 * time.Hour: "47h", 90 * time.Minute: "1h", 59 * time.Minute: "59m",
		time.Minute: "1m", 59 * time.Second: "<1m", time.Second: "<1m",
	} {
		if text := leftText(left); text != expected {
			t.Errorf("%s: expected %q, got %q", left, expected, text)
		}
	}
}
