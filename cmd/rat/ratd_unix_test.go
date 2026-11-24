//go:build unix

package main

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io/fs"
	"log/slog"
	"net/http"
	"os"
	"os/exec"
	"os/user"
	"path/filepath"
	"regexp"
	"strings"
	"sync"
	"syscall"
	"testing"
	"time"

	"github.com/hekmon/rat/connect"
	"github.com/hekmon/rat/mtls"
)

// The tests of rat's transfers against ratd itself, rather than the fake ratd of transfers_test.go,
// whose answers are written by hand after ratd's. What ratd does by itself, successes and refusals,
// is tested against it, its texts written nowhere else; what a test must provoke (a cut, a stall,
// an answer lost, an old ratd, a failure once the body came) against the fake, whose answers on the
// way TestRealRatdAnswersAsFake compares with ratd's. ratd is a main package, which rat can not
// import: it is built from this tree and runs as a process of its own, as it is deployed, rat
// staying in the test with its hooks. ratd needs Unix, and tmux. No build tag keeps these tests out
// of go test ./...: a run testing less without saying so would pass for one that tested, and
// -skip RealRatd is the explicit choice to.

// ratdBuilt is ratd built from this tree, once for the tests of the package (see buildRatd), in a
// directory TestMain removes once they ran: a build for each test would load the machine for the
// tests of tmux running alongside, and macOS assesses each new binary on its first run.
var ratdBuilt struct {
	once   sync.Once
	dir    string
	binary string
	err    error
}

func TestMain(m *testing.M) {
	code := m.Run()
	if ratdBuilt.dir != "" {
		_ = os.RemoveAll(ratdBuilt.dir)
	}
	os.Exit(code)
}

// buildRatd builds ratd from this tree the first time a test asks for it, and returns the path of
// its binary. A test that can not have it built (no go command) fails, as one lacking tmux does: a
// run that tested nothing would pass for one that tested.
func buildRatd(t *testing.T) string {
	t.Helper()
	ratdBuilt.once.Do(func() {
		dir, err := os.MkdirTemp("", "rat-test-ratd-")
		if err != nil {
			ratdBuilt.err = err
			return
		}
		ratdBuilt.dir = dir
		binary := filepath.Join(dir, "ratd")
		out, err := exec.Command("go", "build", "-o", binary, "github.com/hekmon/rat/cmd/ratd").CombinedOutput()
		if err != nil {
			ratdBuilt.err = fmt.Errorf("%w\n%s", err, out)
			return
		}
		ratdBuilt.binary = binary
	})
	if ratdBuilt.err != nil {
		t.Fatalf("building ratd: %v", ratdBuilt.err)
	}
	return ratdBuilt.binary
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

// TestRealRatdPaths guards what ratd does by itself with the remote path of a transfer: the missing
// directories of an upload created and named, which rat adds to its text, and ~/ taken as the home
// of ratd, both ways.
func TestRealRatdPaths(t *testing.T) {
	tt := startRatdTransfers(t, "test-rat-paths")
	content := randomFile(t, filepath.Join(tt.local, "app.bin"), 300<<10, 0o644)
	deep := filepath.Join(tt.remote, "new", "dir", "app.bin")
	tt.expect(t, uploadTool, map[string]any{"local_path": "app.bin", "remote_path": deep}, false,
		"Uploaded app.bin to "+deep+" (300 KiB): created. Missing directories created: "+
			filepath.Join(tt.remote, "new")+", "+filepath.Join(tt.remote, "new", "dir")+". Check the path if you "+
			"did not mean them.")
	expectContent(t, deep, content, 0o644&^umask())
	tt.expect(t, uploadTool, map[string]any{"local_path": "app.bin", "remote_path": "~/files/home.bin"}, false,
		"Uploaded app.bin to ~/files/home.bin (300 KiB): created.")
	expectContent(t, filepath.Join(tt.remote, "home.bin"), content, 0o644&^umask())
	tt.expect(t, downloadTool, map[string]any{"remote_path": "~/files/home.bin", "local_path": "back.bin"}, false,
		"Downloaded ~/files/home.bin to back.bin (300 KiB): created.")
	expectContent(t, filepath.Join(tt.local, "back.bin"), content, 0o644&^umask())
}

// TestRealRatdRefusals guards the refusals of ratd itself, relayed by rat in ratd's words: what a
// transfer does not write nor read (a symbolic link, a directory, a FIFO), a path under a file, a
// relative path; and what ratd's user may not do: read a file of no permission, or replace a file
// whose directory refuses a new one, rat told to set in_place, which then writes it. Nothing is
// written.
func TestRealRatdRefusals(t *testing.T) {
	tt := startRatdTransfers(t, "test-rat-refusals")
	content := randomFile(t, filepath.Join(tt.local, "app.bin"), 64<<10, 0o644)
	file := filepath.Join(tt.remote, "app.bin")
	randomFile(t, file, 64<<10, 0o644)
	link := filepath.Join(tt.remote, "link")
	if err := os.Symlink(file, link); err != nil {
		t.Fatal(err)
	}
	dir := filepath.Join(tt.remote, "dir")
	if err := os.Mkdir(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	fifo := filepath.Join(tt.remote, "fifo")
	if err := syscall.Mkfifo(fifo, 0o644); err != nil {
		t.Fatal(err)
	}
	linkText := link + " is a symbolic link to " + file + ", which transfers do not follow: use that path instead."
	fifoText := fifo + " is not a regular file (a FIFO): transfers only copy regular files."
	relativeText := `Path "app.bin" is relative: give an absolute path, or one starting with ~/ (list_windows shows ` +
		"the directories of your windows)."
	for _, tc := range []struct {
		what, tool, remote, text string
	}{
		{"upload to a symbolic link", uploadTool, link, linkText},
		{"download of a symbolic link", downloadTool, link, linkText},
		{"upload to a directory", uploadTool, dir, dir + " is a directory, not a file."},
		{"download of a directory", downloadTool, dir,
			dir + " is a directory, not a file: transfers copy one file, archive it first (tar)."},
		{"upload to a FIFO", uploadTool, fifo, fifoText},
		{"download of a FIFO", downloadTool, fifo, fifoText},
		{"upload under a file", uploadTool, filepath.Join(file, "a", "x.bin"),
			file + " is not a directory: " + filepath.Join(file, "a") + " can not be created in it."},
		{"upload to a relative path", uploadTool, "app.bin", relativeText},
		{"download of a relative path", downloadTool, "app.bin", relativeText},
	} {
		args := map[string]any{"local_path": "down.bin", "remote_path": tc.remote}
		if tc.tool == uploadTool {
			args = map[string]any{"local_path": "app.bin", "remote_path": tc.remote, "overwrite": true}
		}
		if text, isError := tt.call(t, tc.tool, args); !isError || text != tc.text {
			t.Errorf("%s: expected the refusal %q, got %v: %q", tc.what, tc.text, isError, text)
		}
	}
	// root reads and writes anything
	if os.Geteuid() != 0 {
		me, err := user.Current()
		if err != nil {
			t.Fatal(err)
		}
		secret := filepath.Join(tt.remote, "secret.bin")
		randomFile(t, secret, 1<<10, 0)
		tt.expect(t, downloadTool, map[string]any{"remote_path": secret, "local_path": "down.bin"}, true,
			"Permission denied: "+secret+" can not be read by "+me.Username+", the user the terminals run as.")
		locked := filepath.Join(tt.remote, "locked")
		if err := os.Mkdir(locked, 0o755); err != nil {
			t.Fatal(err)
		}
		conf := filepath.Join(locked, "app.yml")
		randomFile(t, conf, 1<<10, 0o644)
		if err := os.Chmod(locked, 0o555); err != nil {
			t.Fatal(err)
		}
		t.Cleanup(func() { _ = os.Chmod(locked, 0o755) })
		args := map[string]any{"local_path": "app.bin", "remote_path": conf, "overwrite": true}
		tt.expect(t, uploadTool, args, true, "Permission denied: an upload puts a new file in place of "+conf+
			", which "+locked+" refuses to "+me.Username+", the user the terminals run as: set in_place to write "+
			"into "+conf+" itself, which a copy cut or canceled leaves partly written.")
		args["in_place"] = true
		tt.expect(t, uploadTool, args, false, "Uploaded app.bin to "+conf+" (64 KiB): replaced.")
		expectContent(t, conf, content, 0o644)
	}
	if _, err := os.Lstat(filepath.Join(tt.local, "down.bin")); err == nil {
		t.Error("expected no file downloaded")
	}
	expectNoTemporary(t, tt.remote)
}

// answersOf runs through tt the calls whose answers the tests of the fake go through, on their way
// to what they provoke: an upload created, refused as existing, replaced (its mode kept, a hard
// link left with the old content), written in place (a hard link following), replacing a file of a
// setuid bit, put in place and in place; a download created, replaced, replacing a file of a setuid
// bit, missing. It returns their results and the files they leave, a line each, its
// directories named <local> and <remote>: alike from one ratd to the other when they answer alike.
// The files hold their name repeated, unlike randomFile's, for their content to be told alike too.
func answersOf(t *testing.T, tt *transferTest) []string {
	t.Helper()
	contents := map[string][]byte{}
	source := func(name string, mode fs.FileMode) {
		content := bytes.Repeat([]byte(name), (300<<10)/len(name)+1)[:300<<10]
		path := filepath.Join(tt.local, name)
		if err := os.WriteFile(path, content, mode); err != nil {
			t.Fatal(err)
		}
		if err := os.Chmod(path, mode); err != nil {
			t.Fatal(err)
		}
		contents[name] = content
	}
	state := func(path string) string {
		info, err := os.Stat(path)
		if err != nil {
			return filepath.Base(path) + " missing"
		}
		data, err := os.ReadFile(path)
		if err != nil {
			t.Fatal(err)
		}
		holds := "other content"
		for name, content := range contents {
			if bytes.Equal(data, content) {
				holds = "the content of " + name
			}
		}
		// the whole mode, for the special bits of a file replaced to show
		return fmt.Sprintf("%s %v, %s", filepath.Base(path), info.Mode(), holds)
	}
	var lines []string
	call := func(what, tool string, args map[string]any, files ...string) {
		text, isError := tt.call(t, tool, args)
		line := fmt.Sprintf("%s: error %v, %q", what, isError, text)
		for _, file := range files {
			line += "; " + state(file)
		}
		line = strings.ReplaceAll(line, tt.remote, "<remote>")
		lines = append(lines, strings.ReplaceAll(line, tt.local, "<local>"))
	}
	chmod := func(path string, mode fs.FileMode) {
		if err := os.Chmod(path, mode); err != nil {
			t.Fatal(err)
		}
	}
	source("app.bin", 0o750)
	source("app2.bin", 0o644)
	remote := filepath.Join(tt.remote, "app.bin")
	call("upload, created", uploadTool, map[string]any{"local_path": "app.bin", "remote_path": remote}, remote)
	call("upload, existing", uploadTool, map[string]any{"local_path": "app2.bin", "remote_path": remote}, remote)
	chmod(remote, 0o600)
	call("upload, replaced", uploadTool, map[string]any{"local_path": "app2.bin", "remote_path": remote,
		"overwrite": true}, remote)
	link := filepath.Join(tt.remote, "app.link")
	if err := os.Link(remote, link); err != nil {
		t.Fatal(err)
	}
	call("upload in place, a hard link", uploadTool, map[string]any{"local_path": "app.bin", "remote_path": remote,
		"overwrite": true, "in_place": true}, remote, link)
	call("upload replaced, a hard link", uploadTool, map[string]any{"local_path": "app2.bin", "remote_path": remote,
		"overwrite": true}, remote, link)
	chmod(remote, fs.ModeSetuid|0o755)
	call("upload replaced, setuid", uploadTool, map[string]any{"local_path": "app.bin", "remote_path": remote,
		"overwrite": true}, remote)
	chmod(remote, fs.ModeSetuid|0o755)
	call("upload in place, setuid", uploadTool, map[string]any{"local_path": "app2.bin", "remote_path": remote,
		"overwrite": true, "in_place": true}, remote)
	back := filepath.Join(tt.local, "back.bin")
	call("download, created", downloadTool, map[string]any{"remote_path": remote, "local_path": "back.bin"}, back)
	chmod(back, 0o640)
	call("download, replaced", downloadTool, map[string]any{"remote_path": remote, "local_path": "back.bin",
		"overwrite": true}, back)
	chmod(back, fs.ModeSetuid|0o755)
	call("download replaced, setuid", downloadTool, map[string]any{"remote_path": remote, "local_path": "back.bin",
		"overwrite": true}, back)
	call("download, missing", downloadTool, map[string]any{"remote_path": filepath.Join(tt.remote, "missing.bin"),
		"local_path": "missing.bin"}, filepath.Join(tt.local, "missing.bin"))
	return lines
}

// TestRealRatdAnswersAsFake guards the fake ratd of rat's tests (fakeTransfers), written by hand
// after ratd, against ratd itself: the calls the tests of the fake go through (see answersOf) give
// the same results against both, and leave the same files. A change of ratd the fake does not
// follow fails it, rather than leave those tests passing against answers ratd no longer gives.
func TestRealRatdAnswersAsFake(t *testing.T) {
	fake := answersOf(t, startTransfers(t))
	real := answersOf(t, startRatdTransfers(t, "test-rat-asfake"))
	if !strings.Contains(real[0], "error false") {
		t.Errorf("expected the first upload to ratd to succeed: %s", real[0])
	}
	for i := range fake {
		if fake[i] != real[i] {
			t.Errorf("the fake answers otherwise than ratd:\nfake: %s\nratd: %s", fake[i], real[i])
		}
	}
}

// unreadable is a file whose reads fail once after of its bytes were read, as a file of rat failing
// to read: Stat tells the regular file it is, of its size. Past bytes read, the read failing waits
// a little first, as a disk failing after a while would, for these bytes to reach ratd.
type unreadable struct {
	*os.File
	after, read int64
}

func (u *unreadable) Read(p []byte) (int, error) {
	if u.read >= u.after {
		if u.after > 0 {
			time.Sleep(100 * time.Millisecond)
		}
		return 0, errors.New("input/output error")
	}
	n, err := u.File.Read(p[:min(int64(len(p)), u.after-u.read)])
	u.read += int64(n)
	return n, err
}

// TestRealRatdEmptiesInPlaceAsFake guards when the fake ratd empties a file written in place, and
// writes into it, against ratd itself: as the body comes. An upload in place whose body fails
// before its first byte, its source failing at its first read, leaves the file as it was against
// both; one whose body fails once 64 KiB of it were read leaves the beginning of the new content
// against both, where a fake writing only once the body came whole would leave the old one. The
// calls of TestRealRatdAnswersAsFake write in place whole, which would tell neither. Each upload has
// a deadline: the go-ahead awaited with no limit, a side holding it would hang the test rather than
// fail it.
func TestRealRatdEmptiesInPlaceAsFake(t *testing.T) {
	old, content := bytes.Repeat([]byte("o"), 1<<20), bytes.Repeat([]byte("n"), 1<<20)
	// left returns what an upload in place whose source fails once after of its bytes were read
	// leaves, through the ratd at addr serving the tenant of bundle
	left := func(t *testing.T, bundle, addr string, after int64) string {
		t.Helper()
		side, err := mtls.Load(mtls.ClientDir(bundle, "alice"))
		if err != nil {
			t.Fatal(err)
		}
		client, err := connect.HTTPClient(side)
		if err != nil {
			t.Fatal(err)
		}
		dir, err := filepath.EvalSymlinks(t.TempDir())
		if err != nil {
			t.Fatal(err)
		}
		target, source := filepath.Join(dir, "app.conf"), filepath.Join(dir, "source.bin")
		if err := os.WriteFile(target, old, 0o644); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(source, content, 0o644); err != nil {
			t.Fatal(err)
		}
		file, err := os.Open(source)
		if err != nil {
			t.Fatal(err)
		}
		defer file.Close()
		ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
		defer cancel()
		_, err = connect.Upload(ctx, nil, client, addr, &unreadable{File: file, after: after}, target,
			connect.UploadOptions{Overwrite: true, InPlace: true})
		// once the side wrote what it received, which it may still be doing as the client returns
		beginning := func(data []byte) bool {
			return len(data) > 0 && int64(len(data)) <= after && bytes.Equal(data, content[:len(data)])
		}
		data, readErr := os.ReadFile(target)
		for deadline := time.Now().Add(5 * time.Second); after > 0 && !beginning(data) && time.Now().Before(deadline); {
			time.Sleep(20 * time.Millisecond)
			data, readErr = os.ReadFile(target)
		}
		holds := "other content"
		switch {
		case readErr != nil:
			holds = readErr.Error()
		case bytes.Equal(data, old):
			holds = "the old content"
		case beginning(data):
			holds = "a beginning of the new content"
		}
		return fmt.Sprintf("read failing %v; the file holding %s", errors.Is(err, connect.ErrRead), holds)
	}
	bundle := newBundle(t)
	fake := &fakeTransfers{}
	mux := http.NewServeMux()
	mux.HandleFunc("POST "+connect.UploadPath, fake.upload)
	fakeAddr := fakeRatd(t, bundle, mux, nil)
	ratdBundle := filepath.Join(t.TempDir(), "bundle")
	if _, err := mtls.Generate(ratdBundle, "test-rat-emptied", []string{"alice"}); err != nil {
		t.Fatal(err)
	}
	home, err := filepath.EvalSymlinks(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	ratdAddr := startRealRatd(t, buildRatd(t), ratdBundle, home)
	for _, tc := range []struct {
		after int64
		want  string
	}{
		{0, "read failing true; the file holding the old content"},
		{64 << 10, "read failing true; the file holding a beginning of the new content"},
	} {
		fakeLeft, ratdLeft := left(t, bundle, fakeAddr, tc.after), left(t, ratdBundle, ratdAddr, tc.after)
		if fakeLeft != tc.want || ratdLeft != tc.want {
			t.Errorf("a body failing after %d bytes: expected %q against both:\nfake: %s\nratd: %s", tc.after,
				tc.want, fakeLeft, ratdLeft)
		}
	}
}
