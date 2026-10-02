package main

import (
	"context"
	"errors"
	"log/slog"
	"net"
	"net/http"
	"path/filepath"
	"testing"
	"time"

	"github.com/hekmon/rat/connect"
	"github.com/hekmon/rat/mtls"
)

// listenNotify plays the service manager: it listens on a socket named in NOTIFY_SOCKET, and
// returns a function returning the next state ratd sends, or "" if none comes within wait.
func listenNotify(t *testing.T) func(wait time.Duration) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "notify")
	conn, err := net.ListenUnixgram("unixgram", &net.UnixAddr{Name: path, Net: "unixgram"})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = conn.Close() })
	t.Setenv("NOTIFY_SOCKET", path)
	return func(wait time.Duration) string {
		t.Helper()
		if err := conn.SetReadDeadline(time.Now().Add(wait)); err != nil {
			t.Fatal(err)
		}
		buf := make([]byte, 256)
		n, err := conn.Read(buf)
		if err != nil {
			return ""
		}
		return string(buf[:n])
	}
}

// TestRunNotifiesSystemd guards what ratd tells systemd: nothing when its startup fails, its last
// step included (listening), so that systemctl start reports the failure; ready once it serves,
// so that systemctl start returns on a ratd answering; stopping as soon as it is signaled, before
// the door closes.
func TestRunNotifiesSystemd(t *testing.T) {
	requireTmux(t)
	next := listenNotify(t)
	bundle := newBundle(t, "test-ratd-notify")

	err := run(context.Background(), slog.New(slog.DiscardHandler), mtls.ServerDir(bundle), defaultReadBudget,
		func() (net.Listener, error) { return nil, errors.New("address in use") })
	if err == nil {
		t.Fatal("expected the startup to fail")
	}
	if state := next(100 * time.Millisecond); state != "" {
		t.Errorf("expected nothing told of a failed startup, got %q", state)
	}

	addr, logs, stop := runRatd(t, bundle)
	if state := next(15 * time.Second); state != "READY=1" {
		t.Fatalf("expected READY=1, got %q\n%s", state, logs)
	}
	if resp, err := post(t, httpClient(t, bundle, "alice"), addr, addr, connect.Path); err != nil || resp.StatusCode != http.StatusOK {
		t.Fatalf("expected ratd to serve once ready, got %v, %v\n%s", resp, err, logs)
	}
	if err = stop(); err != nil {
		t.Fatal(err)
	}
	if state := next(time.Second); state != "STOPPING=1" {
		t.Errorf("expected STOPPING=1, got %q\n%s", state, logs)
	}
}
