package main

import (
	"bytes"
	"context"
	"errors"
	"io"
	"io/fs"
	"net/http"
	"net/http/httptrace"
	"net/textproto"
	"os"
	"path/filepath"
	"strings"
	"syscall"
	"testing"
	"time"

	"github.com/hekmon/rat/connect"
)

// withPipes returns req telling that its client handles pipes, as rat does, and telling processing
// once ratd answers 102 (unless nil).
func withPipes(req *http.Request, processing chan<- struct{}) *http.Request {
	req.Header.Set(connect.HeaderFeatures, connect.FeaturePipes)
	if processing == nil {
		return req
	}
	return req.WithContext(httptrace.WithClientTrace(req.Context(), &httptrace.ClientTrace{
		Got1xxResponse: func(code int, _ textproto.MIMEHeader) error {
			if code == http.StatusProcessing {
				processing <- struct{}{}
			}
			return nil
		},
	}))
}

// makePipe makes the named pipe name in home, as an agent does (mkfifo), and returns its path.
func makePipe(t *testing.T, home, name string) string {
	t.Helper()
	path := filepath.Join(home, name)
	if err := syscall.Mkfifo(path, 0o600); err != nil {
		t.Fatal(err)
	}
	return path
}

// answer is what a transfer request sent in a goroutine got.
type answer struct {
	resp *http.Response
	body string
	err  error
}

// sendAsync sends req with client in a goroutine: a transfer through a pipe waits for its command.
func sendAsync(client *http.Client, req *http.Request) <-chan answer {
	answers := make(chan answer, 1)
	go func() {
		resp, body, err := sendTransfer(client, req)
		answers <- answer{resp, body, err}
	}()
	return answers
}

// await returns what c gives, failing the test after 5 seconds without, telling what it waited for.
func await[T any](t *testing.T, c <-chan T, what string) T {
	t.Helper()
	select {
	case v := <-c:
		return v
	case <-time.After(5 * time.Second):
		t.Fatalf("%s: nothing after 5s", what)
		var zero T
		return zero
	}
}

// openCommand opens the other end of the pipe at path as a command does, waiting for it, with flag
// (O_RDONLY for the command of an upload, O_WRONLY for that of a download), and gives the file,
// closed once the test ends, or nil when the open failed.
func openCommand(t *testing.T, path string, flag int) <-chan *os.File {
	files := make(chan *os.File, 1)
	go func() {
		f, err := os.OpenFile(path, flag, 0)
		if err != nil {
			files <- nil
			return
		}
		t.Cleanup(func() { f.Close() })
		files <- f
	}()
	return files
}

// awaitCommand returns the file of a command opening a pipe (see openCommand).
func awaitCommand(t *testing.T, files <-chan *os.File) *os.File {
	t.Helper()
	f := await(t, files, "the command opening the pipe")
	if f == nil {
		t.Fatal("the command failed to open the pipe")
	}
	return f
}

// drainPipe reads what the pipe at f holds now, without waiting, as a command reads before waiting
// for more: it returns how many bytes, and whether the end of the data came (no writer left).
func drainPipe(t *testing.T, f *os.File) (n int, ended bool) {
	t.Helper()
	conn, err := f.SyscallConn()
	if err != nil {
		t.Fatal(err)
	}
	buf := make([]byte, 64<<10)
	for {
		var got int
		var rerr error
		if err := conn.Control(func(fd uintptr) {
			if rerr = syscall.SetNonblock(int(fd), true); rerr == nil {
				got, rerr = syscall.Read(int(fd), buf)
			}
		}); err != nil {
			t.Fatal(err)
		}
		switch {
		case rerr == nil && got > 0:
			n += got
		case rerr == nil:
			return n, true
		case errors.Is(rerr, syscall.EAGAIN):
			return n, false
		case !errors.Is(rerr, syscall.EINTR):
			t.Fatal(rerr)
		}
	}
}

// writeNow writes a byte into the pipe at f without waiting, as a command does: EPIPE once no
// reader is left.
func writeNow(t *testing.T, f *os.File) error {
	t.Helper()
	conn, err := f.SyscallConn()
	if err != nil {
		t.Fatal(err)
	}
	var werr error
	if err := conn.Control(func(fd uintptr) {
		if werr = syscall.SetNonblock(int(fd), true); werr == nil {
			_, werr = syscall.Write(int(fd), []byte("x"))
		}
	}); err != nil {
		t.Fatal(err)
	}
	return werr
}

// stallingBody is the body of an upload stalling after first until ctx is done, as rat's, whose
// reads stop with its transfer: Go's client returns from a request only once its body returned.
type stallingBody struct {
	first []byte
	ctx   context.Context
}

func (b *stallingBody) Read(p []byte) (int, error) {
	if len(b.first) > 0 {
		n := copy(p, b.first)
		b.first = b.first[n:]
		return n, nil
	}
	<-b.ctx.Done()
	return 0, b.ctx.Err()
}

// newClose returns the request closing the pipe at path held by a transfer to endpoint.
func newClose(t *testing.T, addr, endpoint, path string) *http.Request {
	t.Helper()
	req, err := http.NewRequest(http.MethodDelete, "https://"+addr+endpoint, nil)
	if err != nil {
		t.Fatal(err)
	}
	req.Header.Set(connect.HeaderPath, connect.EncodePath(path))
	return req
}

// expectClosed fails the test unless closing the pipe at path held by a transfer to endpoint
// answers 204.
func expectClosed(t *testing.T, r *servedRatd, client *http.Client, endpoint, path string) {
	t.Helper()
	resp, body, err := sendTransfer(client, newClose(t, r.addr, endpoint, path))
	if err != nil || resp.StatusCode != http.StatusNoContent {
		t.Fatalf("closing %s: expected 204, got %v, %v: %q", path, resp, err, body)
	}
}

// pipesHeld returns how many pipes ratd holds, as its status counts them.
func pipesHeld(r *servedRatd) int {
	r.d.status.mu.Lock()
	defer r.d.status.mu.Unlock()
	return r.d.status.pipes
}

// pipeTransfersLeft returns how many transfers through pipes ratd knows, in flight or held.
func pipeTransfersLeft(r *servedRatd) int {
	r.d.pipes.mu.Lock()
	defer r.d.pipes.mu.Unlock()
	return len(r.d.pipes.byPath)
}

// TestDownloadPipe guards a download through a named pipe, started before its command as the
// recipe of rat has it: 102 at once, as ratd waits for the command; once the command wrote and
// closed its end, 200 with the mode of the pipe and no size, every byte, no Rat-Error, the
// features of ratd in the answer; nothing left of the transfer, the pipe kept, and one log line.
func TestDownloadPipe(t *testing.T) {
	r, client, home := transferRatd(t)
	path := makePipe(t, home, "data.pipe")
	content := randomContent(t, 1<<20)
	processing := make(chan struct{}, 1)
	answers := sendAsync(client, withPipes(newDownload(t, r.addr, "~/data.pipe"), processing))
	await(t, processing, "the 102 of ratd waiting for the command")
	writer := awaitCommand(t, openCommand(t, path, os.O_WRONLY))
	go func() {
		_, _ = writer.Write(content)
		writer.Close()
	}()
	a := await(t, answers, "the download")
	if a.err != nil || a.resp.StatusCode != http.StatusOK || a.body != string(content) {
		t.Fatalf("expected 200 with the content, got %v, %v (%d bytes)", a.resp, a.err, len(a.body))
	}
	header := a.resp.Header
	if header.Get(connect.HeaderSize) != "" || header.Get(connect.HeaderMode) != "0600" ||
		!connect.HasFeature(header.Get(connect.HeaderFeatures), connect.FeaturePipes) {
		t.Errorf("expected the mode, no size, and the features of ratd, got %v", header)
	}
	if a.resp.Trailer.Get(connect.HeaderError) != "" {
		t.Errorf("expected no Rat-Error, got %v", a.resp.Trailer)
	}
	eventually(t, "the download logged", func() bool {
		return strings.Contains(r.logs.String(), "direction=download outcome=ok")
	})
	if !strings.Contains(r.logs.String(), "pipe=closed") {
		t.Errorf("expected the pipe closed logged:\n%s", r.logs)
	}
	if left := pipeTransfersLeft(r); left != 0 {
		t.Errorf("expected no transfer left, got %d", left)
	}
	if info, err := os.Lstat(path); err != nil || info.Mode()&fs.ModeNamedPipe == 0 {
		t.Errorf("expected the pipe kept, never removed: %v, %v", info, err)
	}
}

// TestUploadPipe guards an upload through a named pipe: 102 as ratd waits for the command, no byte
// of the body sent before the command opened the pipe (ratd's go-ahead coming then), every byte
// read by the command, then the end of its input once the body ended, and 200; overwrite and mode
// not applying to a pipe.
func TestUploadPipe(t *testing.T) {
	r, client, home := transferRatd(t)
	path := makePipe(t, home, "data.pipe")
	content := randomContent(t, 1<<20)
	body := &fileBody{content: content}
	processing := make(chan struct{}, 1)
	answers := sendAsync(client, withPipes(newUpload(t, r.addr, "~/data.pipe", body, "0755", false), processing))
	await(t, processing, "the 102 of ratd waiting for the command")
	if taken := body.taken.Load(); taken != 0 {
		t.Errorf("expected no byte sent before the command opened the pipe, %d were", taken)
	}
	reader := awaitCommand(t, openCommand(t, path, os.O_RDONLY))
	read := make(chan []byte, 1)
	go func() {
		got, _ := io.ReadAll(reader)
		read <- got
	}()
	if got := await(t, read, "the command reading to the end of its input"); !bytes.Equal(got, content) {
		t.Errorf("expected the command to read the %d bytes sent, it read %d", len(content), len(got))
	}
	a := await(t, answers, "the upload")
	if a.err != nil || a.resp.StatusCode != http.StatusOK {
		t.Fatalf("expected 200, got %v, %v: %q", a.resp, a.err, a.body)
	}
	eventually(t, "the upload logged", func() bool {
		return strings.Contains(r.logs.String(), "direction=upload outcome=ok")
	})
	if !strings.Contains(r.logs.String(), "pipe=closed") {
		t.Errorf("expected the pipe closed logged:\n%s", r.logs)
	}
	if left := pipeTransfersLeft(r); left != 0 {
		t.Errorf("expected no transfer left, got %d", left)
	}
}

// TestPipeNeedsFeature guards a client not telling it handles pipes, as rat before them: a named
// pipe stays what is not a regular file, refused (409), both ways, ratd telling its features.
func TestPipeNeedsFeature(t *testing.T) {
	r, client, home := transferRatd(t)
	path := makePipe(t, home, "data.pipe")
	refused := path + " is not a regular file (a FIFO): transfers only copy regular files."
	resp, body, err := sendTransfer(client, newDownload(t, r.addr, path))
	expectRefused(t, "download", resp, body, err, http.StatusConflict, refused)
	resp, body, err = sendTransfer(client, newUpload(t, r.addr, path, bytes.NewReader([]byte("x")), "0644", true))
	expectRefused(t, "upload", resp, body, err, http.StatusConflict, refused)
	if resp != nil && !connect.HasFeature(resp.Header.Get(connect.HeaderFeatures), connect.FeaturePipes) {
		t.Errorf("expected the features of ratd in the answer, got %v", resp.Header)
	}
}

// TestPipeBusy guards one transfer at a time through a pipe: another one is refused (409), either
// way, while the first waits for its command. Closing the first (DELETE) ends its wait, it answering
// so, and leaves the pipe as it was, no end of it open.
func TestPipeBusy(t *testing.T) {
	r, client, home := transferRatd(t)
	path := makePipe(t, home, "data.pipe")
	processing := make(chan struct{}, 1)
	first := sendAsync(client, withPipes(newDownload(t, r.addr, path), processing))
	await(t, processing, "the 102 of the first transfer")
	busy := path + " is already in a transfer: cancel that one first."
	resp, body, err := sendTransfer(client, withPipes(newDownload(t, r.addr, path), nil))
	expectRefused(t, "a second download", resp, body, err, http.StatusConflict, busy)
	upload := withPipes(newUpload(t, r.addr, path, bytes.NewReader([]byte("x")), "0644", false), nil)
	resp, body, err = sendTransfer(client, upload)
	expectRefused(t, "an upload meanwhile", resp, body, err, http.StatusConflict, busy)

	expectClosed(t, r, client, connect.DownloadPath, path)
	a := await(t, first, "the first transfer, closed")
	expectRefused(t, "the first transfer", a.resp, a.body, a.err, http.StatusConflict,
		"given up before a command opened the pipe")
	// no end left open: a command writing into the pipe waits for a reader
	if f, err := os.OpenFile(path, os.O_WRONLY|syscall.O_NONBLOCK, 0); !errors.Is(err, syscall.ENXIO) {
		if f != nil {
			f.Close()
		}
		t.Errorf("expected no reader left (ENXIO), got %v", err)
	}
	if left := pipeTransfersLeft(r); left != 0 {
		t.Errorf("expected no transfer left, got %d", left)
	}
}

// TestUploadPipeHeld guards an upload through a pipe cut before its end, its client gone: the pipe
// is held, its command waiting for more input rather than seeing the end of it, the status telling
// it. Closing it (DELETE) then gives the command the end of its input, and logs it.
func TestUploadPipeHeld(t *testing.T) {
	r, client, home := transferRatd(t)
	path := makePipe(t, home, "data.pipe")
	readers := openCommand(t, path, os.O_RDONLY)
	ctx, cut := context.WithCancel(context.Background())
	body := &stallingBody{first: bytes.Repeat([]byte("x"), 1000), ctx: ctx}
	answers := sendAsync(client, withPipes(newUpload(t, r.addr, path, body, "0644", false), nil).WithContext(ctx))
	reader := awaitCommand(t, readers)
	if _, err := io.ReadFull(reader, make([]byte, 1000)); err != nil {
		t.Fatal(err)
	}
	cut()
	if a := await(t, answers, "the upload cut"); a.err == nil {
		t.Errorf("expected the upload cut, got %v: %q", a.resp, a.body)
	}
	eventually(t, "the pipe held", func() bool { return pipesHeld(r) == 1 })
	if n, ended := drainPipe(t, reader); n != 0 || ended {
		t.Errorf("held: expected the command waiting for more input, it got %d bytes, ended %v", n, ended)
	}
	r.d.status.mu.Lock()
	line := r.d.status.render(time.Now())
	r.d.status.mu.Unlock()
	if !strings.HasSuffix(line, "1 pipe held") {
		t.Errorf("expected the status to tell the pipe held, got %q", line)
	}

	expectClosed(t, r, client, connect.UploadPath, path)
	if _, ended := drainPipe(t, reader); !ended {
		t.Error("expected the end of the input once the pipe closed")
	}
	if held, left := pipesHeld(r), pipeTransfersLeft(r); held != 0 || left != 0 {
		t.Errorf("expected nothing held nor left, got %d held, %d left", held, left)
	}
	logs := r.logs.String()
	if !strings.Contains(logs, "pipe=held") || !strings.Contains(logs, "pipe closed") ||
		!strings.Contains(logs, "path="+path+" held=") {
		t.Errorf("expected the transfer logged as held, then the pipe closed:\n%s", logs)
	}
}

// TestUploadPipeReaderGone guards the command of an upload's pipe closing its end before the body
// ended: the upload fails (409), telling where the command stopped, and the pipe is not held.
func TestUploadPipeReaderGone(t *testing.T) {
	r, client, home := transferRatd(t)
	path := makePipe(t, home, "data.pipe")
	readers := openCommand(t, path, os.O_RDONLY)
	// more than the pipe takes, within what the connection takes
	content := randomContent(t, 100<<10)
	answers := sendAsync(client, withPipes(newUpload(t, r.addr, path, bytes.NewReader(content), "0644", false), nil))
	reader := awaitCommand(t, readers)
	if _, err := io.ReadFull(reader, make([]byte, 1024)); err != nil {
		t.Fatal(err)
	}
	reader.Close()
	a := await(t, answers, "the upload")
	expectRefused(t, "the upload", a.resp, a.body, a.err, http.StatusConflict, "The command reading "+path+" stopped after ")
	if held, left := pipesHeld(r), pipeTransfersLeft(r); held != 0 || left != 0 {
		t.Errorf("expected nothing held nor left, got %d held, %d left", held, left)
	}
}

// TestDownloadPipeHeld guards a download through a pipe cut before its end, its client gone: the
// pipe is held, its command not told its reader left; closing it (DELETE) gives the command a
// broken pipe.
func TestDownloadPipeHeld(t *testing.T) {
	r, client, home := transferRatd(t)
	path := makePipe(t, home, "data.pipe")
	writers := openCommand(t, path, os.O_WRONLY)
	ctx, cut := context.WithCancel(context.Background())
	req := withPipes(newDownload(t, r.addr, path), nil).WithContext(ctx)
	responses := make(chan *http.Response, 1)
	go func() {
		resp, err := client.Do(req)
		if err != nil {
			resp = nil
		}
		responses <- resp
	}()
	writer := awaitCommand(t, writers)
	if _, err := writer.Write(bytes.Repeat([]byte("x"), 1000)); err != nil {
		t.Fatal(err)
	}
	resp := await(t, responses, "the answer of the download")
	if resp == nil {
		t.Fatal("the download failed")
	}
	if _, err := io.ReadFull(resp.Body, make([]byte, 1000)); err != nil {
		t.Fatal(err)
	}
	cut()
	resp.Body.Close()
	eventually(t, "the pipe held", func() bool { return pipesHeld(r) == 1 })
	if err := writeNow(t, writer); err != nil {
		t.Errorf("held: expected the command not told its reader left, got %v", err)
	}
	expectClosed(t, r, client, connect.DownloadPath, path)
	if err := writeNow(t, writer); !errors.Is(err, syscall.EPIPE) {
		t.Errorf("expected a broken pipe once the pipe closed, got %v", err)
	}
}

// TestClosePipeNone guards closing a pipe no transfer holds: 404, telling so, with the version of
// ratd, so that a client tells it from an older ratd.
func TestClosePipeNone(t *testing.T) {
	r, client, home := transferRatd(t)
	path := makePipe(t, home, "data.pipe")
	resp, body, err := sendTransfer(client, newClose(t, r.addr, connect.UploadPath, path))
	expectRefused(t, "closing nothing", resp, body, err, http.StatusNotFound, "No transfer of "+path+" to close")
}

// TestHeadPipe guards HEAD on a named pipe, for a client handling pipes: 200 with the mode and no
// size, the pipe never opened, a command waiting to write into it still waiting.
func TestHeadPipe(t *testing.T) {
	r, client, home := transferRatd(t)
	path := makePipe(t, home, "data.pipe")
	opened := make(chan error, 1)
	go func() {
		writer, err := os.OpenFile(path, os.O_WRONLY, 0)
		if err == nil {
			writer.Close()
		}
		opened <- err
	}()
	// time for the command to wait in open, the case guarded
	time.Sleep(100 * time.Millisecond)
	req := withPipes(newDownload(t, r.addr, path), nil)
	req.Method = http.MethodHead
	resp, _, err := sendTransfer(client, req)
	if err != nil || resp.StatusCode != http.StatusOK || resp.Header.Get(connect.HeaderSize) != "" ||
		resp.Header.Get(connect.HeaderMode) != "0600" {
		t.Errorf("expected 200 with the mode and no size, got %v, %v", resp, err)
	}
	select {
	case err := <-opened:
		t.Errorf("HEAD opened the pipe, releasing the command waiting to write into it (open: %v)", err)
	case <-time.After(200 * time.Millisecond):
		reader, err := os.OpenFile(path, os.O_RDONLY|syscall.O_NONBLOCK, 0)
		if err != nil {
			t.Fatal(err)
		}
		defer reader.Close()
		<-opened
	}
}

// TestPipeStop guards the kill switch for transfers through pipes: stopping ratd ends at once a
// transfer waiting for its command, leaving nothing open, and one streaming, whose pipe is held
// rather than closed, its command waiting instead of seeing the end of its input, until ratd
// closes what it holds once its terminals stopped (see run).
func TestPipeStop(t *testing.T) {
	r, client, home := transferRatd(t)
	waiting := makePipe(t, home, "waiting.pipe")
	processing := make(chan struct{}, 1)
	sendAsync(client, withPipes(newDownload(t, r.addr, waiting), processing))
	await(t, processing, "the 102 of the transfer waiting")
	streaming := makePipe(t, home, "streaming.pipe")
	readers := openCommand(t, streaming, os.O_RDONLY)
	body := &blockingBody{first: bytes.Repeat([]byte("x"), 1000), unblock: make(chan struct{})}
	defer close(body.unblock)
	sendAsync(client, withPipes(newUpload(t, r.addr, streaming, body, "0644", false), nil))
	reader := awaitCommand(t, readers)
	if _, err := io.ReadFull(reader, make([]byte, 1000)); err != nil {
		t.Fatal(err)
	}
	start := time.Now()
	if err := r.stop(); err != nil {
		t.Fatal(err)
	}
	if took := time.Since(start); took > shutdownGrace/2 {
		t.Errorf("expected the transfers cut at once, the stop took %s", took)
	}
	if held := pipesHeld(r); held != 1 {
		t.Errorf("expected the pipe streaming held, got %d held", held)
	}
	if _, ended := drainPipe(t, reader); ended {
		t.Error("expected the command waiting for more input until ratd exits")
	}
	if f, err := os.OpenFile(waiting, os.O_WRONLY|syscall.O_NONBLOCK, 0); !errors.Is(err, syscall.ENXIO) {
		if f != nil {
			f.Close()
		}
		t.Errorf("expected nothing left open of the pipe waiting (ENXIO), got %v", err)
	}
	r.d.pipes.closeAll()
	if _, ended := drainPipe(t, reader); !ended {
		t.Error("expected the end of the input once ratd closed what it held")
	}
	if held := pipesHeld(r); held != 0 {
		t.Errorf("expected nothing held, got %d", held)
	}
}
