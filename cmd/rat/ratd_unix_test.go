//go:build unix

package main

import (
	"log/slog"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"syscall"
	"testing"
	"time"

	"github.com/hekmon/rat/mtls"
)

// The tests of rat's transfers against ratd itself, rather than the fake ratd of transfers_test.go,
// whose answers are written by hand after ratd's. ratd is a main package, which rat can not import:
// it is built from this tree and runs as a process of its own, as it is deployed, rat staying in
// the test with its hooks. ratd needs Unix, and tmux.

// buildRatd builds ratd from this tree, and returns the path of its binary. A test that can not
// build it (no go command) fails, as one lacking tmux does: a run that tested nothing would pass
// for one that tested.
func buildRatd(t *testing.T) string {
	t.Helper()
	binary := filepath.Join(t.TempDir(), "ratd")
	out, err := exec.Command("go", "build", "-o", binary, "github.com/hekmon/rat/cmd/ratd").CombinedOutput()
	if err != nil {
		t.Fatalf("building ratd: %v\n%s", err, out)
	}
	return binary
}

// servingLine is the line ratd logs once it serves, telling the address it listens on.
var servingLine = regexp.MustCompile(`msg=serving .*address=(\S+)`)

// startRealRatd runs binary, ratd, for the tenant of bundle on a loopback port, with home as its
// home, until the test ends, and returns its address. It is stopped as systemd stops it (SIGTERM),
// and must then exit, cleanly; its logs are told if the test failed.
func startRealRatd(t *testing.T, binary, bundle, home string) string {
	t.Helper()
	logs := &logBuffer{}
	cmd := exec.Command(binary, "--bundle", mtls.ServerDir(bundle), "--listen", "127.0.0.1:0")
	cmd.Env = append(os.Environ(), "HOME="+home)
	cmd.Stdout, cmd.Stderr = logs, logs
	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}
	exited := make(chan error, 1)
	go func() { exited <- cmd.Wait() }()
	t.Cleanup(func() {
		_ = cmd.Process.Signal(syscall.SIGTERM)
		select {
		case err := <-exited:
			if err != nil {
				t.Errorf("ratd stopped with %v", err)
			}
		case <-time.After(10 * time.Second):
			_ = cmd.Process.Kill()
			t.Error("ratd did not stop")
		}
		if t.Failed() {
			t.Logf("the logs of ratd:\n%s", logs)
		}
	})
	// generous: on macOS, the first run of a binary just built waits for its assessment
	// (Gatekeeper), which the load of a full test run slows
	deadline := time.Now().Add(30 * time.Second)
	for {
		if m := servingLine.FindStringSubmatch(logs.String()); m != nil {
			return m[1]
		}
		select {
		case err := <-exited:
			// for the cleanup to find it too
			exited <- err
			t.Fatalf("ratd exited before serving: %v", err)
		default:
		}
		if time.Now().After(deadline) {
			t.Fatal("ratd did not serve in time")
		}
		time.Sleep(20 * time.Millisecond)
	}
}

// startRatdTransfers runs ratd, built from this tree, for tenant, and rat offering the transfers
// with a fresh transfers directory, and a harness connected to it, as startTransfers does with the
// fake: no fake then, the remote directory one in the home of ratd, its links resolved. tenant is
// a tmux socket, named test-rat-*: one of its own for each test.
func startRatdTransfers(t *testing.T, tenant string) *transferTest {
	t.Helper()
	local, err := filepath.EvalSymlinks(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	home, err := filepath.EvalSymlinks(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	// a directory of its own: ratd may write in its home (the history of a terminal)
	remote := filepath.Join(home, "files")
	if err := os.Mkdir(remote, 0o755); err != nil {
		t.Fatal(err)
	}
	bundle := filepath.Join(t.TempDir(), "bundle")
	if _, err := mtls.Generate(bundle, tenant, []string{"alice"}); err != nil {
		t.Fatal(err)
	}
	addr := startRealRatd(t, buildRatd(t), bundle, home)
	p := startRatTransfers(t, bundle, addr, slog.LevelInfo, local)
	return &transferTest{rat: p, session: connectHarness(t, p, ""), local: local, remote: remote, addr: addr}
}

// TestRealRatdRoundTrip guards rat's transfers against ratd itself: a file uploaded, then
// downloaded back, arrives whole both ways, with its permissions minus the umask.
func TestRealRatdRoundTrip(t *testing.T) {
	tt := startRatdTransfers(t, "test-rat-roundtrip")
	content := randomFile(t, filepath.Join(tt.local, "app.bin"), 300<<10, 0o750)
	remote := filepath.Join(tt.remote, "app.bin")
	tt.expect(t, uploadTool, map[string]any{"local_path": "app.bin", "remote_path": remote}, false,
		"Uploaded app.bin to "+remote+" (300 KiB): created.")
	expectContent(t, remote, content, 0o750&^umask())
	tt.expect(t, downloadTool, map[string]any{"remote_path": remote, "local_path": "back.bin"}, false,
		"Downloaded "+remote+" to back.bin (300 KiB): created.")
	expectContent(t, filepath.Join(tt.local, "back.bin"), content, 0o750&^umask())
}
