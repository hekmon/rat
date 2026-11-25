package main

import (
	"bytes"
	"context"
	"crypto/sha256"
	"errors"
	"io"
	"io/fs"
	"net/http"
	"os"
	"path/filepath"
	"runtime"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/hekmon/rat/connect"
	"golang.org/x/sys/unix"
)

// The tests of the portals of an upload: ratd writes the body of the request into a pipe it makes,
// for a command to read, the test reading it as a command in a terminal would.

// openUpload opens a portal at path through client, an upload whose body is the returned writer,
// nothing written into it yet: the test fails unless ratd answers within a bound, which it does
// before reading any byte of the body. The answer's headers are read, its end still to come.
func (pt *portalTest) openUpload(t *testing.T, client *http.Client, path string) (*http.Response, *io.PipeWriter) {
	t.Helper()
	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)
	body, send := io.Pipe()
	t.Cleanup(func() { _ = send.CloseWithError(errors.New("the test ended")) })
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, "https://"+pt.r.addr+connect.UploadPath, body)
	if err != nil {
		t.Fatal(err)
	}
	req.ContentLength = -1
	req.Header.Set("Content-Type", connect.OctetStream)
	req.Header.Set(connect.HeaderPath, connect.EncodePath(path))
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
	return resp, send
}

// sendBody writes content into send, the body of an upload, then ends it cleanly, in the
// background; the returned channel tells the error of the writes.
func sendBody(send *io.PipeWriter, content []byte) <-chan error {
	done := make(chan error, 1)
	go func() {
		_, err := send.Write(content)
		if err == nil {
			err = send.Close()
		}
		done <- err
	}()
	return done
}

// patterned returns n bytes of which no part of 32 bytes or more repeats: a hole in a stream, or a
// part of it in the wrong place, shows in what a command read of it.
func patterned(n int) []byte {
	out := make([]byte, 0, n+sha256.Size)
	sum := sha256.Sum256([]byte("portal"))
	for len(out) < n {
		out = append(out, sum[:]...)
		sum = sha256.Sum256(sum[:])
	}
	return out[:n]
}

// commonPrefix returns the length of the beginning a and b share.
func commonPrefix(a, b []byte) int {
	n := 0
	for n < len(a) && n < len(b) && a[n] == b[n] {
		n++
	}
	return n
}

// sendForever writes into send, the body of an upload, until a write fails, the request having
// ended, in the background.
func sendForever(send *io.PipeWriter) {
	go func() {
		chunk := bytes.Repeat([]byte("y"), 32<<10)
		for {
			if _, err := send.Write(chunk); err != nil {
				return
			}
		}
	}()
}

// fifoReader reads a pipe without blocking, as a command reading it would, the test looking at
// what it gets: data, the end of its input, or a wait for more.
type fifoReader struct {
	t  *testing.T
	fd int
	// got is what it read so far
	got []byte
}

// openReader opens the pipe at path for reading, without blocking: a command reading it, which
// releases the open of ratd's end.
func openReader(t *testing.T, path string) *fifoReader {
	t.Helper()
	fd, err := unix.Open(path, unix.O_RDONLY|unix.O_NONBLOCK|unix.O_CLOEXEC, 0)
	if err != nil {
		t.Fatal(err)
	}
	r := &fifoReader{t: t, fd: fd}
	t.Cleanup(r.close)
	return r
}

// read reads what the pipe holds now, and tells whether its input ended (no writer left), rather
// than waiting for more. Before ratd opened its end, a pipe without a writer reads as ended.
func (r *fifoReader) read() (ended bool) {
	buf := make([]byte, 64<<10)
	for {
		n, err := unix.Read(r.fd, buf)
		switch {
		case n > 0:
			r.got = append(r.got, buf[:n]...)
		case errors.Is(err, unix.EINTR):
		case errors.Is(err, unix.EAGAIN):
			return false
		case err != nil:
			r.t.Fatal(err)
		default:
			return true
		}
	}
}

// close closes the reader, a command leaving: once.
func (r *fifoReader) close() {
	if r.fd >= 0 {
		_ = unix.Close(r.fd)
		r.fd = -1
	}
}

// readUntil reads until n bytes came, ratd having opened its end (see waitOpened), waiting for no
// end before.
func (r *fifoReader) readUntil(n int) {
	r.t.Helper()
	eventually(r.t, "the bytes read from the pipe", func() bool {
		if r.read() && len(r.got) < n {
			r.t.Fatalf("the input ended after %d bytes, %d expected", len(r.got), n)
		}
		return len(r.got) >= n
	})
}

// waitOpened waits until the pump of the portal at path opened its end, a command having opened
// the other: from then on, a reader seeing the end of its input saw it from ratd.
func (pt *portalTest) waitOpened(t *testing.T, path string) {
	t.Helper()
	eventually(t, "ratd opening its end of the pipe", func() bool {
		p := pt.portalAt(path)
		return p != nil && p.opened.Load()
	})
}

// expectNoEnd fails the test unless the reader sees no end: it waits for more, a writer being
// held.
func (r *fifoReader) expectNoEnd() {
	r.t.Helper()
	if r.read() {
		r.t.Fatalf("expected no end of input, got the end after %d bytes", len(r.got))
	}
}

// expectEnd fails the test unless the reader comes to see the end of its input, reading what is
// left.
func (r *fifoReader) expectEnd() {
	r.t.Helper()
	eventually(r.t, "the end of the input", r.read)
}

// portalAt returns the portal ratd holds at path, nil without one.
func (pt *portalTest) portalAt(path string) *portal {
	pt.r.d.portals.mu.Lock()
	defer pt.r.d.portals.mu.Unlock()
	return pt.r.d.portals.open[path]
}

// pumpAt tells whether the pump of an upload is in at, a call its stack shows: .(*portal).writePipe(,
// a write of the pipe, or its wait for room; net/http.(*body).Read, a read of the body.
func pumpAt(at string) bool {
	buf := make([]byte, 1<<20)
	for _, stack := range strings.Split(string(buf[:runtime.Stack(buf, true)]), "\n\n") {
		if strings.Contains(stack, ".(*portal).pumpInto(") && strings.Contains(stack, at) {
			return true
		}
	}
	return false
}

// waitWriteBlocked waits until the pump of the portal at path waits in a write of its pipe, full,
// nobody reading it: in a write, and nothing more written since the look before.
func (pt *portalTest) waitWriteBlocked(t *testing.T, path string) {
	t.Helper()
	last := int64(-1)
	eventually(t, "the pump waiting in a write of the pipe", func() bool {
		p := pt.portalAt(path)
		moved := p.moved.Load()
		blocked := pumpAt(".(*portal).writePipe(") && moved == last
		last = moved
		return blocked
	})
}

// expectTrailer reads the rest of the answer to an upload into a pipe, and fails the test unless
// it ends cleanly with text in its Rat-Error trailer, none for an empty text.
func expectTrailer(t *testing.T, resp *http.Response, text string) {
	t.Helper()
	data, err := readBody(t, resp.Body)
	got := connect.DecodeError(resp.Trailer.Get(connect.HeaderError))
	if err != nil || len(data) != 0 || got != text {
		t.Errorf("expected the answer ended with Rat-Error %q, got %q, %v, %q", text, data, err, got)
	}
}

// TestPortalUpload guards an upload into a pipe: ratd answers its headers before reading any byte
// of the body (none sent by then), the pipe existing (a FIFO of mode 0600), echoing Rat-Pipe, with
// the Rat-Error trailer announced and its version; once a command opened the pipe for reading, the
// body reaches it whole and in order; the body ending cleanly ends the input of the command and the
// answer, without Rat-Error, the pipe deleted, no pump nor descriptor left; and the portal is logged
// in one line.
func TestPortalUpload(t *testing.T) {
	pt := startPortals(t)
	path := filepath.Join(pt.home, "in.pipe")
	resp, send := pt.openUpload(t, pt.alice, "~/in.pipe")
	_, announced := resp.Trailer[connect.HeaderError]
	if resp.StatusCode != http.StatusOK || resp.Header.Get(connect.HeaderPipe) != "true" || !announced ||
		resp.Header.Get(connect.HeaderVersion) == "" {
		t.Fatalf("expected 200, Rat-Pipe and the trailer announced, got %s %v", resp.Status, resp.Header)
	}
	made := expectPipe(t, path)
	content := randomContent(t, 4<<20)
	read := make(chan []byte, 1)
	go func() {
		r, err := os.Open(path)
		if err != nil {
			t.Error(err)
			read <- nil
			return
		}
		defer r.Close()
		data, err := io.ReadAll(r)
		if err != nil {
			t.Error(err)
		}
		read <- data
	}()
	sent := sendBody(send, content)
	select {
	case data := <-read:
		if !bytes.Equal(data, content) {
			t.Errorf("expected the body whole and in order, then the end of the input, got %d bytes", len(data))
		}
	case <-time.After(30 * time.Second):
		t.Fatal("the input of the command never ended")
	}
	if err := <-sent; err != nil {
		t.Error(err)
	}
	expectTrailer(t, resp, "")
	expectPacked(t, path, made)
	eventually(t, "the portal logged", func() bool {
		return strings.Contains(pt.r.logs.String(), "direction=upload outcome=ok")
	})
	if !strings.Contains(pt.r.logs.String(), "path="+path+" pipe=true status=200 bytes=4194304 opened=true") {
		t.Errorf("expected the path, the bytes and the open logged:\n%s", pt.r.logs)
	}
}

// zeros returns a reader of size zero bytes: a body far larger than the buffers of a connection.
func zeros(size int64) io.Reader {
	return io.LimitReader(zeroReader{}, size)
}

type zeroReader struct{}

func (zeroReader) Read(p []byte) (int, error) {
	clear(p)
	return len(p), nil
}

// sparseFile returns a file of size bytes, read as zeros, at path, opened.
func sparseFile(t *testing.T, path string, size int64) *os.File {
	t.Helper()
	if err := os.WriteFile(path, nil, 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.Truncate(path, size); err != nil {
		t.Fatal(err)
	}
	file, err := os.Open(path)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = file.Close() })
	return file
}

// TestPortalUploadRefused guards what an upload into a pipe refuses, as a download from one does:
// a path where something is, touching it, and a body that is not bytes; each answer echoing
// Rat-Pipe. A refusal comes as the client sends its body, no go-ahead being waited for: rat, sending
// a file far larger than the buffers of the connection, gets the refusal all the same, rather than
// the failure of its write as ratd closes the connection.
func TestPortalUploadRefused(t *testing.T) {
	pt := startPortals(t)
	file := filepath.Join(pt.home, "file")
	if err := os.WriteFile(file, []byte("kept"), 0o644); err != nil {
		t.Fatal(err)
	}
	for _, tc := range []struct {
		what, path, contentType string
		status                  int
		text                    string
	}{
		{"a file", file, connect.OctetStream, http.StatusConflict, file + " exists: a portal makes its pipe itself"},
		{"a form", filepath.Join(pt.home, "form.pipe"), "application/x-www-form-urlencoded",
			http.StatusUnsupportedMediaType, "files travel as application/octet-stream"},
	} {
		req := newUpload(t, pt.r.addr, tc.path, strings.NewReader("data"), "0644", false)
		req.Header.Del("Expect")
		req.Header.Set("Content-Type", tc.contentType)
		req.Header.Set(connect.HeaderPipe, "true")
		resp, body, err := sendTransfer(pt.alice, req)
		expectRefused(t, tc.what, resp, body, err, tc.status, tc.text)
		if err == nil && resp.Header.Get(connect.HeaderPipe) != "true" {
			t.Errorf("%s: expected Rat-Pipe echoed, got %v", tc.what, resp.Header)
		}
	}
	expectFile(t, file, "kept", 0o644)
	if _, err := os.Lstat(filepath.Join(pt.home, "form.pipe")); err == nil {
		t.Error("expected no pipe made for a form")
	}
	large := sparseFile(t, filepath.Join(t.TempDir(), "large"), 1<<30)
	var refused *connect.Refusal
	if _, err := connect.StartPipeUpload(context.Background(), pt.alice, pt.r.addr, large, file); !errors.As(err,
		&refused) || refused.Status != http.StatusConflict {
		t.Errorf("a large file: expected the refusal of ratd, got %v", err)
	}
	expectFile(t, file, "kept", 0o644)
}

// TestPortalUploadOlderRatd guards what a ratd older than the portals does with an upload into a
// pipe: it reads no Rat-Pipe, and takes the request, sent at once without a go-ahead to wait for,
// for a file upload, which it refuses before reading its body, the request carrying no Rat-Mode:
// nothing is written at the path, the answer echoing no Rat-Pipe (connect.ErrNoPortals). Sent
// with Rat-Mode, the body would be written there as a file.
func TestPortalUploadOlderRatd(t *testing.T) {
	pt := startPortals(t)
	path := filepath.Join(pt.home, "in.pipe")
	req := newUpload(t, pt.r.addr, path, zeros(1<<30), "", false)
	// as connect sends it, but Rat-Pipe, which an older ratd does not read; a body far larger than
	// the buffers of the connection, whose refusal comes all the same
	for _, name := range []string{"Expect", connect.HeaderMode, connect.HeaderOverwrite} {
		req.Header.Del(name)
	}
	resp, body, err := sendTransfer(pt.alice, req)
	expectRefused(t, "an upload without Rat-Mode", resp, body, err, http.StatusBadRequest, connect.HeaderMode)
	if err == nil && resp.Header.Get(connect.HeaderPipe) != "" {
		t.Errorf("expected no Rat-Pipe echoed, got %v", resp.Header)
	}
	if _, err := os.Lstat(path); err == nil {
		t.Error("expected nothing written at the path")
	}
	expectNoTemporary(t, pt.home)
}

// TestPortalUploadStopped guards the command reading leaving before the end, the client still
// sending: ratd's next write fails with a broken pipe, and the answer, which the client reads while
// it sends, ends with the Rat-Error trailer telling it, rather than a connection cut; the pipe is
// deleted, nothing left.
func TestPortalUploadStopped(t *testing.T) {
	pt := startPortals(t)
	path := filepath.Join(pt.home, "in.pipe")
	resp, send := pt.openUpload(t, pt.alice, path)
	made := expectPipe(t, path)
	r := openReader(t, path)
	pt.waitOpened(t, path)
	sendForever(send)
	r.readUntil(1)
	r.close()
	data, err := readBody(t, resp.Body)
	text := connect.DecodeError(resp.Trailer.Get(connect.HeaderError))
	if err != nil || len(data) != 0 || !strings.HasPrefix(text, "The command reading "+path+" left before the end of "+
		"the stream, after ") || !strings.HasSuffix(text, " were written into the pipe: read why in its terminal. "+
		"The pipe is deleted.") {
		t.Errorf("expected the answer ended by the trailer telling the command left, got %q, %v, %q", data, err, text)
	}
	expectPacked(t, path, made)
}

// TestPortalUploadBroken guards a body cut once a command opened the pipe, as the client fails it
// when its file changed: the portal is held, its name deleted at once, the command reading seeing
// no end of its input; another portal at the path is refused meanwhile, the portal known by its
// path; a close, its request long gone, gives the command the end of its input, telling the bytes
// it saw, which are not the whole stream; nothing is left.
func TestPortalUploadBroken(t *testing.T) {
	pt := startPortals(t)
	path := filepath.Join(pt.home, "in.pipe")
	resp, send := pt.openUpload(t, pt.alice, path)
	made := expectPipe(t, path)
	r := openReader(t, path)
	pt.waitOpened(t, path)
	if _, err := send.Write([]byte("begin")); err != nil {
		t.Fatal(err)
	}
	r.readUntil(5)
	_ = send.CloseWithError(errors.New("the file changed"))
	if _, err := readBody(t, resp.Body); err == nil {
		t.Error("expected the answer cut")
	}
	eventually(t, "the pipe deleted", func() bool {
		_, err := os.Lstat(path)
		return errors.Is(err, os.ErrNotExist)
	})
	eventually(t, "the request returned, the portal held", func() bool {
		return strings.Contains(pt.r.logs.String(), "held=true")
	})
	r.expectNoEnd()
	again, _ := pt.openUploadRefused(t, path)
	expectRefused(t, "a portal where one is held", again.resp, again.body, again.err, http.StatusConflict,
		"A portal is open at "+path+": close it with cancel before opening another there.")
	r.expectNoEnd()
	closed, text := pt.closeUpload(t, pt.alice, path)
	if closed.StatusCode != http.StatusOK || text != "Closed the portal at "+path+": the pipe is deleted. A command "+
		"still reading it saw the end of its input after 5 B, which is not the whole stream: stop it if it still "+
		"runs, and do not trust what it made of them." {
		t.Errorf("expected the held portal closed, the end of the input told, got %s: %q", closed.Status, text)
	}
	r.expectEnd()
	if string(r.got) != "begin" {
		t.Errorf("expected the beginning of the stream alone, got %q", r.got)
	}
	r.close()
	expectPacked(t, path, made)
	closed, text = pt.closeUpload(t, pt.alice, path)
	expectRefused(t, "a close once closed", closed, text, nil, http.StatusNotFound,
		"No portal is open at "+path+": nothing to close.")
}

// TestPortalUploadCutWithoutCommand guards a body cut while no command opened the pipe: ratd reads
// no body before one does, and sees the cut once a command opened it, which then sees no end, the
// portal held, until a close gives it the end of its input after nothing.
func TestPortalUploadCutWithoutCommand(t *testing.T) {
	pt := startPortals(t)
	path := filepath.Join(pt.home, "in.pipe")
	resp, send := pt.openUpload(t, pt.alice, path)
	made := expectPipe(t, path)
	_ = send.CloseWithError(errors.New("the file changed"))
	if _, err := readBody(t, resp.Body); err == nil {
		t.Error("expected the answer cut")
	}
	// the portal waits in the open, as unused
	expectPipe(t, path)
	r := openReader(t, path)
	pt.waitOpened(t, path)
	eventually(t, "the portal held", func() bool {
		return strings.Contains(pt.r.logs.String(), "held=true")
	})
	r.expectNoEnd()
	closed, text := pt.closeUpload(t, pt.alice, path)
	if closed.StatusCode != http.StatusOK || !strings.Contains(text, "saw the end of its input after 0 B") {
		t.Errorf("expected the held portal closed after 0 B, got %s: %q", closed.Status, text)
	}
	r.expectEnd()
	r.close()
	expectPacked(t, path, made)
}

// refusedOpen is the answer to an opening refused, its body read.
type refusedOpen struct {
	resp *http.Response
	body string
	err  error
}

// openUploadRefused asks for a portal at path, an upload, expecting it refused.
func (pt *portalTest) openUploadRefused(t *testing.T, path string) (refusedOpen, bool) {
	t.Helper()
	req := newUpload(t, pt.r.addr, path, strings.NewReader("data"), "0644", false)
	req.Header.Del("Expect")
	req.Header.Set(connect.HeaderPipe, "true")
	resp, body, err := sendTransfer(pt.alice, req)
	return refusedOpen{resp, body, err}, err == nil
}

// closeUpload asks ratd, through client, to close the portal of an upload at path.
func (pt *portalTest) closeUpload(t *testing.T, client *http.Client, path string) (*http.Response, string) {
	t.Helper()
	req := newDownload(t, pt.r.addr, path)
	req.Method = http.MethodDelete
	req.URL.Path = connect.UploadPath
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	resp, body, err := sendTransfer(client, req.WithContext(ctx))
	if err != nil {
		t.Fatalf("closing the portal at %s: %v", path, err)
	}
	return resp, body
}

// TestPortalUploadClose guards a close asked (DELETE /upload): while the portal waits in the open,
// telling no command opened it; and while a command reads slowly, the pump waiting for room in the
// pipe full, which leaves it, telling the bytes the command saw, which it then reads, then the end
// of its input: a beginning of the stream (P3), no hole in it, nothing taken out of the pipe by the
// close (rejected: a close releasing a blocked write by reading the pipe, which took the bytes the
// command was to read next). Either way the answer of the upload ends with the same sentence in
// its trailer, the pipe is deleted, nothing left. The path alone names the portal (P8): a close by
// the endpoint of a download closes it as well; one from another session finds none.
func TestPortalUploadClose(t *testing.T) {
	t.Run("waiting", func(t *testing.T) {
		pt := startPortals(t)
		path := filepath.Join(pt.home, "in.pipe")
		resp, _ := pt.openUpload(t, pt.alice, path)
		made := expectPipe(t, path)
		closed, text := pt.closeUpload(t, pt.bob, path)
		expectRefused(t, "a close from another session", closed, text, nil, http.StatusNotFound,
			"No portal is open at "+path+": nothing to close.")
		// by the endpoint of a download: the path alone names the portal
		closed, text = pt.closePortal(t, pt.alice, path)
		expected := "Closed the portal at " + path + ": the pipe is deleted. No command had opened it."
		if closed.StatusCode != http.StatusOK || text != expected {
			t.Errorf("expected the portal closed, unused, got %s: %q", closed.Status, text)
		}
		expectTrailer(t, resp, expected)
		expectPacked(t, path, made)
	})
	t.Run("copying", func(t *testing.T) {
		pt := startPortals(t)
		path := filepath.Join(pt.home, "in.pipe")
		resp, send := pt.openUpload(t, pt.alice, path)
		made := expectPipe(t, path)
		r := openReader(t, path)
		pt.waitOpened(t, path)
		content := patterned(16 << 20)
		sendBody(send, content)
		// the command reads, then stops reading: the pipe fills
		r.readUntil(100)
		pt.waitWriteBlocked(t, path)
		p := pt.portalAt(path)
		closed, text := pt.closeUpload(t, pt.alice, path)
		r.expectEnd()
		expected := "Closed the portal at " + path + ": the pipe is deleted. A command still reading it saw the end " +
			"of its input after " + sizeText(len(r.got)) + ", which is not the whole stream: stop it if it still " +
			"runs, and do not trust what it made of them."
		if closed.StatusCode != http.StatusOK || text != expected {
			t.Errorf("expected the portal closed, telling the %d bytes read, got %s: %q", len(r.got), closed.Status,
				text)
		}
		// P3: the beginning of the stream, no hole in it, and not all of it
		if len(r.got) >= len(content) || !bytes.Equal(r.got, content[:len(r.got)]) {
			t.Errorf("expected the command to read a beginning of the stream, got %d bytes, the stream's up to byte %d",
				len(r.got), commonPrefix(r.got, content))
		}
		// exactly, beyond the rounding of the sentence
		if moved := p.moved.Load(); moved != int64(len(r.got)) {
			t.Errorf("expected the bytes the command saw counted exactly, got %d of %d read", moved, len(r.got))
		}
		expectTrailer(t, resp, expected)
		r.close()
		expectPacked(t, path, made)
	})
}

// TestPortalUploadStop guards the kill switch for the portals of an upload: ratd stopping is not
// delayed by one, waiting in the open, its pump waiting in a write of the pipe full, or in a read
// of the body. Waiting, the pipe is deleted and nothing left. With a command reading, its input is
// not ended by the stop (Q5): the name deleted, the portal held until the terminals stopped
// (release), the command then seeing the end of its input, nothing left.
func TestPortalUploadStop(t *testing.T) {
	t.Run("waiting", func(t *testing.T) {
		pt := startPortals(t)
		path := filepath.Join(pt.home, "in.pipe")
		pt.openUpload(t, pt.alice, path)
		made := expectPipe(t, path)
		expectQuickStop(t, pt.r)
		expectPacked(t, path, made)
	})
	for _, phase := range []string{"writing", "reading"} {
		t.Run(phase, func(t *testing.T) {
			pt := startPortals(t)
			path := filepath.Join(pt.home, "in.pipe")
			_, send := pt.openUpload(t, pt.alice, path)
			made := expectPipe(t, path)
			r := openReader(t, path)
			pt.waitOpened(t, path)
			if phase == "writing" {
				sendForever(send)
				pt.waitWriteBlocked(t, path)
			} else {
				if _, err := send.Write([]byte("begin")); err != nil {
					t.Fatal(err)
				}
				r.readUntil(5)
				eventually(t, "the pump waiting in a read of the body", func() bool {
					return pumpAt("net/http.(*body).Read(")
				})
			}
			expectQuickStop(t, pt.r)
			eventually(t, "the pipe deleted", func() bool {
				_, err := os.Lstat(path)
				return errors.Is(err, os.ErrNotExist)
			})
			r.read()
			r.expectNoEnd()
			pt.r.d.portals.release()
			r.expectEnd()
			r.close()
			expectPacked(t, path, made)
		})
	}
}

// TestPortalUploadRun guards the order in which ratd stopping packs up the portal of an upload a
// command reads (Q5): the door closed, the request returned at once, the terminals stopped (and a
// command reading with them), and only then the pipe's end closed, the command seeing the end of
// its input once run returned, nothing left.
func TestPortalUploadRun(t *testing.T) {
	requireTmux(t)
	bundle := newBundle(t, "test-ratd-portalrun")
	addr, logs, stop := runRatd(t, bundle)
	client := httpClient(t, bundle, "alice")
	pt := &portalTest{r: &servedRatd{addr: addr, logs: logs}}
	path := filepath.Join(t.TempDir(), "in.pipe")
	_, send := pt.openUpload(t, client, path)
	made := expectPipe(t, path)
	r := openReader(t, path)
	if _, err := send.Write([]byte("begin")); err != nil {
		t.Fatal(err)
	}
	eventually(t, "the bytes read from the pipe", func() bool {
		r.read()
		return len(r.got) == 5
	})
	if err := stop(); err != nil {
		t.Fatal(err)
	}
	r.expectEnd()
	stopped, packed := strings.Index(logs.String(), "msg=\"terminals stopped\""),
		strings.Index(logs.String(), "msg=\"held portal packed up\"")
	if stopped < 0 || packed < stopped {
		t.Errorf("expected the portal packed up once the terminals stopped:\n%s", logs)
	}
	r.close()
	expectPacked(t, path, made)
}

// TestPortalNameFirst guards the order in which a portal packs up (P9): its name deleted before
// any of its ends is closed, for a download closed while it copies, and for an upload closed while
// its pump waits for room, and ended by its source: a command told by the end of its pipe never
// finds the name to open again.
func TestPortalNameFirst(t *testing.T) {
	var mu sync.Mutex
	var looked, found []string
	hook := endsClosing
	endsClosing = func(path string) {
		_, err := os.Lstat(path)
		mu.Lock()
		defer mu.Unlock()
		looked = append(looked, path)
		if !errors.Is(err, fs.ErrNotExist) {
			found = append(found, path)
		}
	}
	// restored once ratd stopped (cleanups run in reverse order)
	t.Cleanup(func() { endsClosing = hook })
	pt := startPortals(t)
	download := filepath.Join(pt.home, "out.pipe")
	resp, _ := pt.open(t, pt.alice, download)
	w := openWriter(t, download)
	if _, err := w.Write([]byte("begin")); err != nil {
		t.Fatal(err)
	}
	if _, err := io.ReadFull(resp.Body, make([]byte, 5)); err != nil {
		t.Fatal(err)
	}
	if closed, text := pt.closePortal(t, pt.alice, download); closed.StatusCode != http.StatusOK {
		t.Fatalf("expected the download closed, got %s: %q", closed.Status, text)
	}
	expectBrokenPipe(t, w)
	upload := filepath.Join(pt.home, "in.pipe")
	_, send := pt.openUpload(t, pt.alice, upload)
	r := openReader(t, upload)
	pt.waitOpened(t, upload)
	sendForever(send)
	r.readUntil(1)
	pt.waitWriteBlocked(t, upload)
	if closed, text := pt.closeUpload(t, pt.alice, upload); closed.StatusCode != http.StatusOK {
		t.Fatalf("expected the upload closed, got %s: %q", closed.Status, text)
	}
	r.expectEnd()
	whole := filepath.Join(pt.home, "whole.pipe")
	resp, send = pt.openUpload(t, pt.alice, whole)
	r = openReader(t, whole)
	pt.waitOpened(t, whole)
	sendBody(send, []byte("all"))
	r.expectEnd()
	expectTrailer(t, resp, "")
	mu.Lock()
	defer mu.Unlock()
	for _, path := range []string{download, upload, whole} {
		if !slices.Contains(looked, path) {
			t.Errorf("%s: expected its ends closed, never seen", path)
		}
	}
	if len(found) > 0 {
		t.Errorf("expected the names deleted before the ends were closed, found at %v", found)
	}
}

// TestPortalUploadPipeFailing guards a pipe failing otherwise than by a broken pipe, here its open,
// the pipe deleted by someone else (rm in a terminal) before ratd opened its end: told in the
// trailer as a failure, in the system's words, never as a portal done; nothing left.
func TestPortalUploadPipeFailing(t *testing.T) {
	entered, hold := make(chan struct{}), make(chan struct{})
	pumpOpening = func(string) {
		close(entered)
		<-hold
	}
	// restored once the pump took the hook, for the race detector to see it read before
	t.Cleanup(func() {
		<-entered
		pumpOpening = func(string) {}
	})
	pt := startPortals(t)
	path := filepath.Join(pt.home, "in.pipe")
	resp, send := pt.openUpload(t, pt.alice, path)
	made := expectPipe(t, path)
	<-entered
	if err := os.Remove(path); err != nil {
		t.Fatal(err)
	}
	close(hold)
	sendBody(send, []byte("data"))
	expectTrailer(t, resp, "Writing the pipe "+path+" failed: no such file or directory.")
	expectPacked(t, path, made)
}
