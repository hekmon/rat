package main

import (
	"bytes"
	"context"
	"errors"
	"io"
	"io/fs"
	"log/slog"
	"net/http"
	"os"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"
	"syscall"
	"testing"
	"time"

	"github.com/hekmon/rat/connect"
	"github.com/hekmon/rat/mtls"
	"golang.org/x/sys/unix"
)

// portalTest is a ratd serving portals, with a client of alice and one of bob, two sessions of its
// tenant, neither with a timeout: a portal waits as long as it is not used, each test bounding its
// own waits.
type portalTest struct {
	r          *servedRatd
	alice, bob *http.Client
	home       string
}

// startPortals serves a ratd for portal tests, with a fresh home.
func startPortals(t *testing.T) *portalTest {
	t.Helper()
	home := testHome(t)
	bundle := filepath.Join(t.TempDir(), "bundle")
	if _, err := mtls.Generate(bundle, "t", []string{"alice", "bob"}); err != nil {
		t.Fatal(err)
	}
	r := serveRatd(t, bundle, slog.LevelInfo)
	client := func(name string) *http.Client {
		side, err := mtls.Load(mtls.ClientDir(bundle, name))
		if err != nil {
			t.Fatal(err)
		}
		c, err := connect.HTTPClient(side)
		if err != nil {
			t.Fatal(err)
		}
		return c
	}
	return &portalTest{r: r, alice: client("alice"), bob: client("bob"), home: home}
}

// open opens a portal at path through client, and returns the answer, its headers read, its body
// still to come, and the function cutting the link. The test fails unless the answer comes within a
// bound: ratd answers before any command.
func (pt *portalTest) open(t *testing.T, client *http.Client, path string) (*http.Response, context.CancelFunc) {
	t.Helper()
	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)
	req := newDownload(t, pt.r.addr, path).WithContext(ctx)
	req.Header.Set(connect.HeaderPipe, "true")
	answered := make(chan struct{})
	timer := time.AfterFunc(30*time.Second, func() {
		select {
		case <-answered:
		default:
			cancel()
		}
	})
	defer timer.Stop()
	resp, err := client.Do(req)
	close(answered)
	if err != nil {
		t.Fatalf("opening a portal at %s: %v", path, err)
	}
	t.Cleanup(func() { _ = resp.Body.Close() })
	return resp, cancel
}

// closePortal asks ratd, through client, to close the portal at path, and returns its answer and
// sentence.
func (pt *portalTest) closePortal(t *testing.T, client *http.Client, path string) (*http.Response, string) {
	t.Helper()
	req := newDownload(t, pt.r.addr, path)
	req.Method = http.MethodDelete
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	resp, body, err := sendTransfer(client, req.WithContext(ctx))
	if err != nil {
		t.Fatalf("closing the portal at %s: %v", path, err)
	}
	return resp, body
}

// readBody reads the rest of body within a bound, and returns it with the error of the read.
func readBody(t *testing.T, body io.Reader) ([]byte, error) {
	t.Helper()
	type read struct {
		data []byte
		err  error
	}
	done := make(chan read, 1)
	go func() {
		data, err := io.ReadAll(body)
		done <- read{data, err}
	}()
	select {
	case r := <-done:
		return r.data, r.err
	case <-time.After(30 * time.Second):
		t.Fatal("the body of the portal never ended")
		return nil, nil
	}
}

// openWriter opens the pipe at path for writing, as a command redirecting its output into it does:
// it returns once ratd opened its end.
func openWriter(t *testing.T, path string) *os.File {
	t.Helper()
	opened := make(chan *os.File, 1)
	go func() {
		w, err := os.OpenFile(path, os.O_WRONLY, 0)
		if err != nil {
			t.Error(err)
		}
		opened <- w
	}()
	select {
	case w := <-opened:
		if w == nil {
			t.FailNow()
		}
		t.Cleanup(func() { _ = w.Close() })
		return w
	case <-time.After(30 * time.Second):
		t.Fatal("the pipe was never opened by ratd")
		return nil
	}
}

// expectPipe fails the test unless path is a FIFO of the permission bits 0600, and returns it.
func expectPipe(t *testing.T, path string) fs.FileInfo {
	t.Helper()
	info, err := os.Lstat(path)
	if err != nil || info.Mode().Type() != fs.ModeNamedPipe || info.Mode().Perm() != 0o600 {
		t.Fatalf("%s: expected a FIFO of mode 0600, got %v, %v", path, info, err)
	}
	return info
}

// expectPacked fails the test unless the portal of the pipe made, as looked at, packed up: its name
// deleted, no pump left, and no descriptor of the pipe open in the process but those of except, the
// test's own.
func expectPacked(t *testing.T, path string, made fs.FileInfo, except ...*os.File) {
	t.Helper()
	eventually(t, "the pipe deleted", func() bool {
		_, err := os.Lstat(path)
		return errors.Is(err, fs.ErrNotExist)
	})
	eventually(t, "no pump left", func() bool {
		buf := make([]byte, 1<<20)
		// the package is named by its import path in a stack, not main
		return !strings.Contains(string(buf[:runtime.Stack(buf, true)]), ".(*portal).pump")
	})
	eventually(t, "no descriptor of the pipe left", func() bool { return len(pipeDescriptors(t, made, except)) == 0 })
}

// pipeDescriptors returns the descriptors of the process open on the pipe made, but those of
// except.
func pipeDescriptors(t *testing.T, made fs.FileInfo, except []*os.File) []int {
	t.Helper()
	entries, err := os.ReadDir("/dev/fd")
	if err != nil {
		t.Fatal(err)
	}
	stat := made.Sys().(*syscall.Stat_t)
	var found []int
	for _, entry := range entries {
		fd, err := strconv.Atoi(entry.Name())
		if err != nil {
			continue
		}
		skip := false
		for _, f := range except {
			skip = skip || int(f.Fd()) == fd
		}
		var st unix.Stat_t
		if skip || unix.Fstat(fd, &st) != nil {
			continue
		}
		if uint64(st.Dev) == uint64(stat.Dev) && uint64(st.Ino) == uint64(stat.Ino) {
			found = append(found, fd)
		}
	}
	return found
}

// expectBrokenPipe fails the test unless writes into w come to fail with a broken pipe, ratd having
// closed its end: the system tells the command writing.
func expectBrokenPipe(t *testing.T, w *os.File) {
	t.Helper()
	var err error
	eventually(t, "a write failing with a broken pipe", func() bool {
		_, err = w.Write([]byte("more"))
		return err != nil
	})
	if !errors.Is(err, syscall.EPIPE) {
		t.Errorf("expected a broken pipe, got %v", err)
	}
}

// TestPortalDownload guards a download from a pipe: ratd answers its headers before any command,
// the pipe existing then (a FIFO of mode 0600), echoing Rat-Pipe, with no size nor mode, and its
// version; a small write reaches the client without waiting for more; what a command writes
// arrives whole and in order; the command closing its end ends the body cleanly, without Rat-Error,
// the pipe deleted, no pump nor descriptor left; and the portal is logged in one line.
func TestPortalDownload(t *testing.T) {
	pt := startPortals(t)
	path := filepath.Join(pt.home, "out.pipe")
	resp, _ := pt.open(t, pt.alice, "~/out.pipe")
	if resp.StatusCode != http.StatusOK || resp.Header.Get(connect.HeaderPipe) != "true" ||
		resp.Header.Get(connect.HeaderSize) != "" || resp.Header.Get(connect.HeaderMode) != "" ||
		resp.Header.Get(connect.HeaderVersion) == "" || resp.Header.Get("Content-Type") != connect.OctetStream {
		t.Fatalf("expected 200, Rat-Pipe, no size nor mode, got %s %v", resp.Status, resp.Header)
	}
	made := expectPipe(t, path)
	w := openWriter(t, path)
	if _, err := w.Write([]byte("x")); err != nil {
		t.Fatal(err)
	}
	first := make([]byte, 1)
	if _, err := io.ReadFull(resp.Body, first); err != nil || first[0] != 'x' {
		t.Fatalf("expected the first byte at once, got %q, %v", first, err)
	}
	content := randomContent(t, 1<<20)
	// written as the body is read: more than the buffers on the way hold
	go func() {
		for off := 0; off < len(content); off += 10_000 {
			if _, err := w.Write(content[off:min(off+10_000, len(content))]); err != nil {
				t.Error(err)
				break
			}
		}
		_ = w.Close()
	}()
	data, err := readBody(t, resp.Body)
	if err != nil || !bytes.Equal(data, content) || resp.Trailer.Get(connect.HeaderError) != "" {
		t.Errorf("expected what was written, whole, and no Rat-Error, got %d bytes, %v, %v", len(data), err,
			resp.Trailer)
	}
	expectPacked(t, path, made)
	eventually(t, "the portal logged", func() bool {
		return strings.Contains(pt.r.logs.String(), "direction=download outcome=ok")
	})
	if !strings.Contains(pt.r.logs.String(), "path="+path+" pipe=true status=200 bytes="+strconv.Itoa(len(content)+1)+
		" opened=true") {
		t.Errorf("expected the path, the bytes and the open logged:\n%s", pt.r.logs)
	}
}

// TestPortalRefused guards what a portal refuses to open, touching nothing: a path where anything
// is (a file, a directory, a FIFO, a symbolic link), a missing directory, which is not created, a
// relative path, and HEAD; each answer echoing Rat-Pipe, for rat to tell a ratd that opens
// portals.
func TestPortalRefused(t *testing.T) {
	pt := startPortals(t)
	file := filepath.Join(pt.home, "file")
	if err := os.WriteFile(file, []byte("kept"), 0o644); err != nil {
		t.Fatal(err)
	}
	dir := filepath.Join(pt.home, "dir")
	if err := os.Mkdir(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	fifo := filepath.Join(pt.home, "fifo")
	if err := unix.Mkfifo(fifo, 0o644); err != nil {
		t.Fatal(err)
	}
	link := filepath.Join(pt.home, "link")
	if err := os.Symlink(filepath.Join(pt.home, "nowhere"), link); err != nil {
		t.Fatal(err)
	}
	exists := func(path string) string {
		return path + " exists: a portal makes its pipe itself, at a path where nothing is. If it is the pipe of a " +
			"portal still open, close it with cancel."
	}
	missing := filepath.Join(pt.home, "missing", "out.pipe")
	for _, tc := range []struct {
		what, path, method string
		status             int
		text               string
	}{
		{"a file", file, http.MethodGet, http.StatusConflict, exists(file)},
		{"a directory", dir, http.MethodGet, http.StatusConflict, exists(dir)},
		{"a FIFO", fifo, http.MethodGet, http.StatusConflict, exists(fifo)},
		{"a symbolic link", link, http.MethodGet, http.StatusConflict, exists(link)},
		{"a missing directory", missing, http.MethodGet, http.StatusNotFound,
			"Making the pipe " + missing + " failed: no such file or directory."},
		{"a relative path", "out.pipe", http.MethodGet, http.StatusBadRequest, `Path "out.pipe" is relative`},
		{"HEAD", filepath.Join(pt.home, "head.pipe"), http.MethodHead, http.StatusBadRequest, ""},
	} {
		req := newDownload(t, pt.r.addr, tc.path)
		req.Method = tc.method
		req.Header.Set(connect.HeaderPipe, "true")
		resp, body, err := sendTransfer(pt.alice, req)
		expectRefused(t, tc.what, resp, body, err, tc.status, tc.text)
		if err == nil && resp.Header.Get(connect.HeaderPipe) != "true" {
			t.Errorf("%s: expected Rat-Pipe echoed, got %v", tc.what, resp.Header)
		}
	}
	expectFile(t, file, "kept", 0o644)
	if info, err := os.Lstat(dir); err != nil || !info.IsDir() {
		t.Errorf("expected the directory kept, got %v, %v", info, err)
	}
	if info, err := os.Lstat(fifo); err != nil || info.Mode().Type() != fs.ModeNamedPipe || info.Mode().Perm() != 0o644&^umask() {
		t.Errorf("expected the FIFO kept, got %v, %v", info, err)
	}
	if info, err := os.Lstat(link); err != nil || info.Mode().Type() != fs.ModeSymlink {
		t.Errorf("expected the link kept, got %v, %v", info, err)
	}
	for _, path := range []string{filepath.Join(pt.home, "missing"), filepath.Join(pt.home, "nowhere"),
		filepath.Join(pt.home, "head.pipe")} {
		if _, err := os.Lstat(path); !errors.Is(err, fs.ErrNotExist) {
			t.Errorf("expected nothing made at %s, got %v", path, err)
		}
	}
}

// TestPortalLinkLost guards the link lost, the client leaving: while the portal waits in the open,
// and while it copies. Either way, the pipe is deleted and nothing is left (no pump, no descriptor),
// and the command writing, once it opened the pipe, gets a broken pipe at its next write.
func TestPortalLinkLost(t *testing.T) {
	t.Run("waiting", func(t *testing.T) {
		pt := startPortals(t)
		path := filepath.Join(pt.home, "out.pipe")
		_, cut := pt.open(t, pt.alice, path)
		made := expectPipe(t, path)
		cut()
		expectPacked(t, path, made)
		eventually(t, "the portal logged cut", func() bool {
			return strings.Contains(pt.r.logs.String(), `error="cut: the client left"`)
		})
	})
	t.Run("copying", func(t *testing.T) {
		pt := startPortals(t)
		path := filepath.Join(pt.home, "out.pipe")
		resp, cut := pt.open(t, pt.alice, path)
		made := expectPipe(t, path)
		w := openWriter(t, path)
		if _, err := w.Write([]byte("begin")); err != nil {
			t.Fatal(err)
		}
		if _, err := io.ReadFull(resp.Body, make([]byte, 5)); err != nil {
			t.Fatal(err)
		}
		cut()
		expectBrokenPipe(t, w)
		expectPacked(t, path, made, w)
	})
}

// TestPortalClose guards a close asked (DELETE), while the portal waits in the open and while it
// copies: ratd answers once it packed up, telling what the command saw (none had opened the pipe;
// or the bytes copied, and the broken pipe its next write gets), the body of the portal ending with
// the Rat-Error trailer telling it closed, the pipe deleted, nothing left. A close with no portal
// there, or one of another session, finds none: 404 with the version of ratd, the portal of the
// other session kept; and that session, opening a portal there, is told the path is held by another
// session, not to cancel.
func TestPortalClose(t *testing.T) {
	t.Run("waiting", func(t *testing.T) {
		pt := startPortals(t)
		path := filepath.Join(pt.home, "out.pipe")
		resp, _ := pt.open(t, pt.alice, path)
		made := expectPipe(t, path)
		closed, text := pt.closePortal(t, pt.bob, path)
		expectRefused(t, "a close from another session", closed, text, nil, http.StatusNotFound,
			"No portal is open at "+path+": nothing to close.")
		// that session opening one there is not told to cancel, which it can not
		req := newDownload(t, pt.r.addr, path)
		req.Header.Set(connect.HeaderPipe, "true")
		again, body, err := sendTransfer(pt.bob, req)
		expectRefused(t, "a portal where another session holds one", again, body, err, http.StatusConflict,
			path+" is held by a portal of another session, which alone can close it: use another path.")
		expectPipe(t, path)
		closed, text = pt.closePortal(t, pt.alice, path)
		if closed.StatusCode != http.StatusOK ||
			text != "Closed the portal at "+path+": the pipe is deleted. No command had opened it." {
			t.Errorf("expected the portal closed, unused, got %s: %q", closed.Status, text)
		}
		data, err := readBody(t, resp.Body)
		if err != nil || len(data) != 0 || connect.DecodeError(resp.Trailer.Get(connect.HeaderError)) !=
			"The portal at "+path+" was closed, as asked: its pipe is deleted." {
			t.Errorf("expected the body ended by the trailer telling the close, got %q, %v, %v", data, err, resp.Trailer)
		}
		expectPacked(t, path, made)
		closed, text = pt.closePortal(t, pt.alice, path)
		expectRefused(t, "a close once closed", closed, text, nil, http.StatusNotFound,
			"No portal is open at "+path+": nothing to close.")
	})
	t.Run("copying", func(t *testing.T) {
		pt := startPortals(t)
		path := filepath.Join(pt.home, "out.pipe")
		resp, _ := pt.open(t, pt.alice, path)
		made := expectPipe(t, path)
		w := openWriter(t, path)
		if _, err := w.Write([]byte("begin")); err != nil {
			t.Fatal(err)
		}
		if _, err := io.ReadFull(resp.Body, make([]byte, 5)); err != nil {
			t.Fatal(err)
		}
		closed, text := pt.closePortal(t, pt.alice, "~/out.pipe")
		if closed.StatusCode != http.StatusOK || text != "Closed the portal at "+path+" after 5 B: the pipe is "+
			"deleted, and the command writing into it gets a broken pipe at its next write." {
			t.Errorf("expected the portal closed after 5 B, got %s: %q", closed.Status, text)
		}
		data, err := readBody(t, resp.Body)
		if err != nil || len(data) != 0 || connect.DecodeError(resp.Trailer.Get(connect.HeaderError)) !=
			"The portal at "+path+" was closed, as asked: its pipe is deleted." {
			t.Errorf("expected the body ended by the trailer telling the close, got %q, %v, %v", data, err, resp.Trailer)
		}
		expectBrokenPipe(t, w)
		expectPacked(t, path, made, w)
		eventually(t, "the close logged", func() bool {
			return strings.Contains(pt.r.logs.String(), "msg=\"portal close\"") &&
				strings.Contains(pt.r.logs.String(), "outcome=ok closed=true bytes=5 opened=true")
		})
	})
}

// TestPortalStop guards the kill switch for portals: ratd stopping while a portal waits in the
// open, and while it copies, is not delayed by it, and the pipe is deleted, nothing left.
func TestPortalStop(t *testing.T) {
	t.Run("waiting", func(t *testing.T) {
		pt := startPortals(t)
		path := filepath.Join(pt.home, "out.pipe")
		pt.open(t, pt.alice, path)
		made := expectPipe(t, path)
		expectQuickStop(t, pt.r)
		expectPacked(t, path, made)
	})
	t.Run("copying", func(t *testing.T) {
		pt := startPortals(t)
		path := filepath.Join(pt.home, "out.pipe")
		resp, _ := pt.open(t, pt.alice, path)
		made := expectPipe(t, path)
		w := openWriter(t, path)
		if _, err := w.Write([]byte("begin")); err != nil {
			t.Fatal(err)
		}
		if _, err := io.ReadFull(resp.Body, make([]byte, 5)); err != nil {
			t.Fatal(err)
		}
		expectQuickStop(t, pt.r)
		expectBrokenPipe(t, w)
		expectPacked(t, path, made, w)
	})
}

// TestPortalPipeDeletedByOther guards a portal whose pipe someone else deleted (rm in a terminal):
// the path stays the portal's until it is closed, another portal there refused meanwhile; the close
// answers at once, though ratd can no longer reach the pipe to end its wait, the pump left ending
// by itself (here, its open finding no pipe) with nothing left.
func TestPortalPipeDeletedByOther(t *testing.T) {
	pt := startPortals(t)
	path := filepath.Join(pt.home, "out.pipe")
	hold, entered := make(chan struct{}), make(chan struct{})
	pumpOpening = func(string) {
		close(entered)
		<-hold
	}
	// restored once the pump took the hook, for the race detector to see it read before
	t.Cleanup(func() {
		<-entered
		pumpOpening = func(string) {}
	})
	resp, _ := pt.open(t, pt.alice, path)
	made := expectPipe(t, path)
	if err := os.Remove(path); err != nil {
		t.Fatal(err)
	}
	req := newDownload(t, pt.r.addr, path)
	req.Header.Set(connect.HeaderPipe, "true")
	again, body, err := sendTransfer(pt.alice, req)
	expectRefused(t, "a portal where one is open", again, body, err, http.StatusConflict,
		"A portal is open at "+path+": close it with cancel before opening another there.")
	closed, text := pt.closePortal(t, pt.alice, path)
	if closed.StatusCode != http.StatusOK || !strings.HasPrefix(text, "Closed the portal at "+path) {
		t.Errorf("expected the portal closed, got %s: %q", closed.Status, text)
	}
	if _, err := readBody(t, resp.Body); err != nil {
		t.Errorf("expected the body ended, got %v", err)
	}
	close(hold)
	expectPacked(t, path, made)
	if _, err := os.Lstat(path); !errors.Is(err, fs.ErrNotExist) {
		t.Errorf("expected nothing made again at %s, got %v", path, err)
	}
}
