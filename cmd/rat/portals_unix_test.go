//go:build unix

package main

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"log/slog"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"syscall"
	"testing"
	"time"

	"github.com/hekmon/rat/connect"
	"github.com/hekmon/rat/mtls"
	"golang.org/x/sys/unix"
)

// The tests of the portals of rat against ratd itself (see ratd_unix_test.go): a download from a
// pipe ratd makes, the test writing into it as a command in a terminal would.

// waitUntil fails the test unless condition comes true within a bound, looked at again and again.
func waitUntil(t *testing.T, what string, condition func() bool) {
	t.Helper()
	for deadline := time.Now().Add(20 * time.Second); time.Now().Before(deadline); time.Sleep(20 * time.Millisecond) {
		if condition() {
			return
		}
	}
	t.Fatalf("%s: never happened", what)
}

// expectNoPipe fails the test unless nothing is at path, eventually: ratd deletes its pipe as it
// packs up, which may end after the answer rat got.
func expectNoPipe(t *testing.T, path string) {
	t.Helper()
	waitUntil(t, "the pipe deleted at "+path, func() bool {
		_, err := os.Lstat(path)
		return errors.Is(err, fs.ErrNotExist)
	})
}

// expectFIFO fails the test unless path is a FIFO.
func expectFIFO(t *testing.T, path string) {
	t.Helper()
	if info, err := os.Lstat(path); err != nil || info.Mode().Type() != fs.ModeNamedPipe {
		t.Fatalf("expected a pipe at %s, got %v, %v", path, info, err)
	}
}

// writeInto writes content into the pipe at path and closes it, as a command redirected into it
// does, in the background; the returned channel tells the error of the writes.
func writeInto(path string, content []byte) <-chan error {
	done := make(chan error, 1)
	go func() {
		w, err := os.OpenFile(path, os.O_WRONLY, 0)
		if err != nil {
			done <- err
			return
		}
		_, err = w.Write(content)
		if closeErr := w.Close(); err == nil {
			err = closeErr
		}
		done <- err
	}()
	return done
}

// TestRealRatdPortal guards a download from a remote pipe: the first call answers ready at once,
// with nothing written, the pipe made on ratd's side; a command writing into it, then the same call,
// tells it done, the local file exact, of mode 0666 minus the umask, the pipe deleted, no temporary
// file left.
func TestRealRatdPortal(t *testing.T) {
	tt := startRatdTransfers(t, "test-rat-portal")
	pipe := filepath.Join(tt.remote, "out.pipe")
	args := map[string]any{"remote_path": pipe, "local_path": "out.tar", "remote_pipe": true}
	tt.expect(t, downloadTool, args, false, "The portal is open: "+pipe+" is a pipe on the machine of the "+
		"terminals, and what a command writes into it is copied to out.tar. Start that command in a terminal")
	expectFIFO(t, pipe)
	content := randomFile(t, filepath.Join(t.TempDir(), "src"), 2<<20, 0o644)
	written := writeInto(pipe, content)
	tt.expect(t, downloadTool, args, false, "Downloaded what was written into "+pipe+" to out.tar (2 MiB): created. "+
		"The command writing closed its end of the pipe: its exit status tells whether that was all.")
	if err := <-written; err != nil {
		t.Error(err)
	}
	expectContent(t, filepath.Join(tt.local, "out.tar"), content, 0o666&^umask())
	expectNoPipe(t, pipe)
	expectNoTemporary(t, tt.local)
}

// TestRealRatdPortalRefused guards a portal refused: a local file existing without overwrite before
// any request, no pipe made on ratd's side; a remote path where something is, in ratd's words,
// touching nothing.
func TestRealRatdPortalRefused(t *testing.T) {
	tt := startRatdTransfers(t, "test-rat-portalno")
	pipe := filepath.Join(tt.remote, "out.pipe")
	randomFile(t, filepath.Join(tt.local, "out.tar"), 10, 0o644)
	tt.expect(t, downloadTool, map[string]any{"remote_path": pipe, "local_path": "out.tar", "remote_pipe": true}, true,
		"out.tar exists: set overwrite to replace it.")
	if _, err := os.Lstat(pipe); !errors.Is(err, fs.ErrNotExist) {
		t.Errorf("expected no pipe made, got %v", err)
	}
	file := filepath.Join(tt.remote, "file")
	randomFile(t, file, 10, 0o644)
	tt.expect(t, downloadTool, map[string]any{"remote_path": file, "local_path": "new.tar", "remote_pipe": true}, true,
		file+" exists: a portal makes its pipe itself, at a path where nothing is.")
	if info, err := os.Lstat(file); err != nil || !info.Mode().IsRegular() {
		t.Errorf("expected the file kept, got %v, %v", info, err)
	}
	if _, err := os.Lstat(filepath.Join(tt.local, "new.tar")); err == nil {
		t.Error("expected no local file")
	}
}

// TestRealRatdPortalCancel guards a cancel of a portal rat holds, once bytes came through: ratd
// closes its side, the pipe deleted and the command writing told by a broken pipe, the answer
// telling the bytes and ratd's words; the local file untouched, no temporary file left.
func TestRealRatdPortalCancel(t *testing.T) {
	tt := startRatdTransfers(t, "test-rat-portalcancel")
	shortWait(t, 2*time.Second)
	pipe := filepath.Join(tt.remote, "out.pipe")
	args := map[string]any{"remote_path": pipe, "local_path": "out.tar", "remote_pipe": true}
	tt.expect(t, downloadTool, args, false, "The portal is open: ")
	w, err := os.OpenFile(pipe, os.O_WRONLY, 0)
	if err != nil {
		t.Fatal(err)
	}
	defer w.Close()
	if _, err := w.Write([]byte("begin")); err != nil {
		t.Fatal(err)
	}
	// the bytes reach rat's temporary file
	waitUntil(t, "the bytes came through", func() bool {
		text, _ := tt.call(t, downloadTool, args)
		return strings.Contains(text, ": 5 B, ")
	})
	args["cancel"] = true
	tt.expect(t, downloadTool, args, false, "Closed the portal from "+pipe+" to out.tar after 5 B: out.tar is as it "+
		"was. Closed the portal at "+pipe+" after 5 B: the pipe is deleted, and the command writing into it gets a "+
		"broken pipe at its next write.")
	if _, err := w.Write([]byte("more")); !errors.Is(err, syscall.EPIPE) {
		t.Errorf("expected a broken pipe, got %v", err)
	}
	expectNoPipe(t, pipe)
	if _, err := os.Lstat(filepath.Join(tt.local, "out.tar")); err == nil {
		t.Error("expected no local file")
	}
	expectNoTemporary(t, tt.local)
}

// TestRealRatdPortalCancelWithoutJob guards a cancel of a portal rat holds no job of (rat started
// again, another rat of the session): with remote_pipe, rat asks ratd to close the portal at the
// path all the same, and tells its answer; ratd holding none, that there is nothing to close.
func TestRealRatdPortalCancelWithoutJob(t *testing.T) {
	tt := startRatdTransfers(t, "test-rat-portalnojob")
	pipe := filepath.Join(tt.remote, "out.pipe")
	args := map[string]any{"remote_path": pipe, "local_path": "out.tar", "remote_pipe": true, "cancel": true}
	tt.expect(t, downloadTool, args, false, "No portal is open at "+pipe+": nothing to close.")
	// a portal opened by another client of the session, as a rat gone would have left it
	client := aliceClient(t, tt)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	download, err := connect.StartDownload(ctx, client, tt.addr, pipe, connect.DownloadOptions{Pipe: true})
	if err != nil {
		t.Fatal(err)
	}
	defer download.Close()
	expectFIFO(t, pipe)
	tt.expect(t, downloadTool, args, false, "Closed the portal at "+pipe+": the pipe is deleted. No command had "+
		"opened it.")
	expectNoPipe(t, pipe)
	var refused *connect.Refusal
	if _, err := download.WriteTo(&strings.Builder{}); !errors.As(err, &refused) || refused.Status != http.StatusOK {
		t.Errorf("expected the portal told closed in its trailer, got %v", err)
	}
}

// TestRealRatdPortalProgress guards the progress of a portal open and unused: nothing came
// through yet, rat not claiming to know whether a command is at the pipe.
func TestRealRatdPortalProgress(t *testing.T) {
	tt := startRatdTransfers(t, "test-rat-portalwait")
	shortWait(t, 200*time.Millisecond)
	pipe := filepath.Join(tt.remote, "out.pipe")
	args := map[string]any{"remote_path": pipe, "local_path": "out.tar", "remote_pipe": true}
	tt.expect(t, downloadTool, args, false, "The portal is open: ")
	tt.expect(t, downloadTool, args, false, "The portal from "+pipe+" to out.tar is open, and nothing came through "+
		"yet. Call download_file again with the same paths to keep following it, or with cancel to close it.")
	args["cancel"] = true
	tt.expect(t, downloadTool, args, false, "Closed the portal from "+pipe+" to out.tar after 0 B: out.tar is as it "+
		"was. Closed the portal at "+pipe+": the pipe is deleted. No command had opened it.")
	expectNoPipe(t, pipe)
}

// aliceClient returns an HTTP client of alice for the ratd of tt, as rat builds it.
func aliceClient(t *testing.T, tt *transferTest) *http.Client {
	t.Helper()
	side, err := mtls.Load(mtls.ClientDir(tt.bundle, "alice"))
	if err != nil {
		t.Fatal(err)
	}
	client, err := connect.HTTPClient(side)
	if err != nil {
		t.Fatal(err)
	}
	return client
}

// The tests of the portals of an upload against ratd itself: rat sends a local file, ratd writes
// it into a pipe it makes, the test reading it as a command in a terminal would.

// gatedFile is a local file to upload whose reads wait until its gate is opened: a test tells what
// happens before any byte of it is sent.
type gatedFile struct {
	*os.File
	gate chan struct{}
}

func (f gatedFile) Read(p []byte) (int, error) {
	<-f.gate
	return f.File.Read(p)
}

// readPipe opens the pipe at path for reading, as a command reading it (tar -x < PIPE), in the
// background, reads first bytes of it, then, once more is closed, the rest; the returned channel
// tells what it read once the input ended. Without first, it reads it whole.
func readPipe(t *testing.T, path string, first int, more <-chan struct{}) (<-chan []byte, <-chan struct{}) {
	t.Helper()
	read, firstRead := make(chan []byte, 1), make(chan struct{})
	go func() {
		r, err := os.Open(path)
		if err != nil {
			t.Error(err)
			close(firstRead)
			read <- nil
			return
		}
		defer r.Close()
		data := make([]byte, first)
		if _, err := io.ReadFull(r, data); err != nil {
			t.Error(err)
		}
		close(firstRead)
		if more != nil {
			<-more
		}
		rest, err := io.ReadAll(r)
		if err != nil {
			t.Error(err)
		}
		read <- append(data, rest...)
	}()
	return read, firstRead
}

// received waits for what readPipe read, within a bound.
func received(t *testing.T, read <-chan []byte) []byte {
	t.Helper()
	select {
	case data := <-read:
		return data
	case <-time.After(60 * time.Second):
		t.Fatal("the input of the command never ended")
		return nil
	}
}

// TestRealRatdPipeUploadReady guards the answer before the body on the real pair, rat's client
// against ratd over mutual TLS: the portal is answered ready while not one byte of the file was
// read, let alone sent or read by ratd; then the file, sent once a command opened the pipe, reaches
// it whole, and the answer ends done, the pipe deleted.
func TestRealRatdPipeUploadReady(t *testing.T) {
	tt := startRatdTransfers(t, "test-rat-pipeready")
	pipe := filepath.Join(tt.remote, "in.pipe")
	path := filepath.Join(t.TempDir(), "in.bin")
	content := randomFile(t, path, 8<<20, 0o644)
	file, err := os.Open(path)
	if err != nil {
		t.Fatal(err)
	}
	defer file.Close()
	src := gatedFile{File: file, gate: make(chan struct{})}
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()
	up, err := connect.StartPipeUpload(ctx, aliceClient(t, tt), tt.addr, src, pipe)
	if err != nil {
		t.Fatal(err)
	}
	defer up.Close()
	if offset, err := file.Seek(0, io.SeekCurrent); err != nil || offset != 0 || up.Sent() != 0 {
		t.Fatalf("expected ready with nothing of the file read, got %d, %v", offset, err)
	}
	expectFIFO(t, pipe)
	read, _ := readPipe(t, pipe, 0, nil)
	close(src.gate)
	if n, err := up.Wait(); err != nil || n != int64(len(content)) {
		t.Errorf("expected the portal done, the file sent whole, got %d, %v", n, err)
	}
	if data := received(t, read); !bytes.Equal(data, content) {
		t.Errorf("expected the file whole and in order, got %d bytes", len(data))
	}
	expectNoPipe(t, pipe)
}

// TestRealRatdPipeUpload guards an upload into a remote pipe through rat: the first call answers
// ready at once, the pipe made on ratd's side; a command reading it, then the same call, tells it
// delivered, the command having read the file whole, then the end of its input, the pipe deleted.
func TestRealRatdPipeUpload(t *testing.T) {
	tt := startRatdTransfers(t, "test-rat-pipeup")
	pipe := filepath.Join(tt.remote, "in.pipe")
	content := randomFile(t, filepath.Join(tt.local, "in.bin"), 2<<20, 0o644)
	args := map[string]any{"local_path": "in.bin", "remote_path": pipe, "remote_pipe": true}
	tt.expect(t, uploadTool, args, false, "The portal is open: "+pipe+" is a pipe on the machine of the terminals, "+
		"and what a command reads from it is in.bin. Start that command in a terminal (for example: tar -x < "+pipe+
		"), then call upload_file again with the same paths to follow it, or with cancel to close it.")
	expectFIFO(t, pipe)
	read, _ := readPipe(t, pipe, 0, nil)
	tt.expect(t, uploadTool, args, false, "Delivered in.bin into "+pipe+" (2 MiB): all of it was written into the "+
		"pipe, and the command reading it saw the end of its input; the pipe is deleted. Its exit status tells what "+
		"it made of it.")
	if data := received(t, read); !bytes.Equal(data, content) {
		t.Errorf("expected the file whole and in order, got %d bytes", len(data))
	}
	expectNoPipe(t, pipe)
}

// TestRealRatdPipeUploadStopped guards the command reading leaving before the end while rat still
// sends: the call tells it, in ratd's words from the trailer, which reaches rat rather than a
// connection cut, the pipe deleted.
func TestRealRatdPipeUploadStopped(t *testing.T) {
	tt := startRatdTransfers(t, "test-rat-pipestop")
	pipe := filepath.Join(tt.remote, "in.pipe")
	randomFile(t, filepath.Join(tt.local, "in.bin"), 64<<20, 0o644)
	args := map[string]any{"local_path": "in.bin", "remote_path": pipe, "remote_pipe": true}
	tt.expect(t, uploadTool, args, false, "The portal is open: ")
	r, err := os.Open(pipe)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := io.ReadFull(r, make([]byte, 64<<10)); err != nil {
		t.Fatal(err)
	}
	_ = r.Close()
	text := tt.expect(t, uploadTool, args, true, "The portal from in.bin to "+pipe+" ended after ")
	if !strings.Contains(text, " of 64 MiB sent: The command reading "+pipe+" left before the end of the stream, "+
		"after ") || !strings.HasSuffix(text, " were written into the pipe: read why in its terminal. The pipe is "+
		"deleted.") {
		t.Errorf("expected the command told leaving, in ratd's words, got %q", text)
	}
	expectNoPipe(t, pipe)
}

// TestRealRatdPipeUploadCancel guards a cancel of a portal rat holds, once a command read part of
// the file and stopped reading: ratd closes its side, the pipe deleted, the answer telling what rat
// sent and, in ratd's words, the bytes the command saw before the end of its input, which it then
// gets, the stream not whole: a beginning of the file (P3).
func TestRealRatdPipeUploadCancel(t *testing.T) {
	tt := startRatdTransfers(t, "test-rat-pipecancel")
	shortWait(t, time.Second)
	pipe := filepath.Join(tt.remote, "in.pipe")
	content := randomFile(t, filepath.Join(tt.local, "in.bin"), 64<<20, 0o644)
	args := map[string]any{"local_path": "in.bin", "remote_path": pipe, "remote_pipe": true}
	tt.expect(t, uploadTool, args, false, "The portal is open: ")
	more := make(chan struct{})
	read, firstRead := readPipe(t, pipe, 64<<10, more)
	<-firstRead
	tt.expect(t, uploadTool, args, false, "")
	args["cancel"] = true
	text := tt.expect(t, uploadTool, args, false, "Closed the portal from in.bin to "+pipe+" after ")
	close(more)
	data := received(t, read)
	if !strings.Contains(text, " of 64 MiB sent. Closed the portal at "+pipe+": the pipe is deleted. A command still "+
		"reading it saw the end of its input after "+sizeText(int64(len(data)))+", which is not the whole stream: stop "+
		"it if it still runs, and do not trust what it made of them.") {
		t.Errorf("expected the close told, with the %d bytes the command saw, got %q", len(data), text)
	}
	if len(data) >= len(content) || !bytes.Equal(data, content[:len(data)]) {
		t.Errorf("expected a beginning of the file, got %d bytes", len(data))
	}
	expectNoPipe(t, pipe)
}

// TestRealRatdPipeUploadCancelWithoutJob guards a cancel of an upload portal rat holds no job of
// (rat started again, another rat of the session): with remote_pipe, rat asks ratd to close the
// portal at the path all the same, and tells its answer; ratd holding none, that there is nothing
// to close; the portal of the other client then ends with the close told in its trailer.
func TestRealRatdPipeUploadCancelWithoutJob(t *testing.T) {
	tt := startRatdTransfers(t, "test-rat-pipenojob")
	pipe := filepath.Join(tt.remote, "in.pipe")
	path := filepath.Join(tt.local, "in.bin")
	randomFile(t, path, 1<<20, 0o644)
	args := map[string]any{"local_path": "in.bin", "remote_path": pipe, "remote_pipe": true, "cancel": true}
	tt.expect(t, uploadTool, args, false, "No portal is open at "+pipe+": nothing to close.")
	file, err := os.Open(path)
	if err != nil {
		t.Fatal(err)
	}
	defer file.Close()
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	up, err := connect.StartPipeUpload(ctx, aliceClient(t, tt), tt.addr, file, pipe)
	if err != nil {
		t.Fatal(err)
	}
	defer up.Close()
	expectFIFO(t, pipe)
	tt.expect(t, uploadTool, args, false, "Closed the portal at "+pipe+": the pipe is deleted. No command had opened "+
		"it.")
	expectNoPipe(t, pipe)
	var refused *connect.Refusal
	if _, err := up.Wait(); !errors.As(err, &refused) || refused.Status != http.StatusOK ||
		refused.Text != "Closed the portal at "+pipe+": the pipe is deleted. No command had opened it." {
		t.Errorf("expected the portal told closed in its trailer, got %v", err)
	}
}

// TestRealRatdPipeUploadChanged guards a local file changing while rat sends it into a pipe a
// command reads: rat cuts the portal, and tells it broken, the command reading having seen no end
// of its input, which ratd keeps open, its pipe deleted; the agent told to stop the command, then
// cancel, which closes the portal held, the command then seeing the end of its input after the
// bytes ratd tells.
func TestRealRatdPipeUploadChanged(t *testing.T) {
	tt := startRatdTransfers(t, "test-rat-pipechanged")
	pipe := filepath.Join(tt.remote, "in.pipe")
	path := filepath.Join(tt.local, "in.bin")
	randomFile(t, path, 64<<20, 0o644)
	args := map[string]any{"local_path": "in.bin", "remote_path": pipe, "remote_pipe": true}
	tt.expect(t, uploadTool, args, false, "The portal is open: ")
	// grown past its size when opened: rat sees it as it reads that far, long after the buffers of
	// the connection filled
	appended, err := os.OpenFile(path, os.O_WRONLY|os.O_APPEND, 0)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := appended.Write([]byte("more")); err != nil {
		t.Fatal(err)
	}
	_ = appended.Close()
	// a second reader, which reads nothing until the cut: it tells whether a writer is left
	look, err := unix.Open(pipe, unix.O_RDONLY|unix.O_NONBLOCK|unix.O_CLOEXEC, 0)
	if err != nil {
		t.Fatal(err)
	}
	defer unix.Close(look)
	read, _ := readPipe(t, pipe, 0, nil)
	tt.expect(t, uploadTool, args, true, "in.bin changed while it was read: it may still be written. The portal "+
		"from in.bin to "+pipe+" is cut after 64 MiB of 64 MiB sent. If a command was reading "+pipe+" it still "+
		"waits, it has seen no end: stop it in its terminal, then call upload_file again with cancel to close the "+
		"portal.")
	expectNoPipe(t, pipe)
	waitUntil(t, "the second reader waiting for more, a writer held", func() bool {
		n, err := unix.Read(look, make([]byte, 64<<10))
		if n == 0 && err == nil {
			t.Fatal("expected no end of input, the portal held, got the end")
		}
		return errors.Is(err, unix.EAGAIN)
	})
	select {
	case data := <-read:
		t.Fatalf("expected the command reading to see no end, it got it after %d bytes", len(data))
	default:
	}
	args["cancel"] = true
	text := tt.expect(t, uploadTool, args, false, "Closed the portal at "+pipe+": the pipe is deleted. A command "+
		"still reading it saw the end of its input after ")
	data := received(t, read)
	if !strings.Contains(text, " after "+sizeText(int64(len(data)))+", which is not the whole stream") {
		t.Errorf("expected the %d bytes the command saw told, got %q", len(data), text)
	}
}

// inProcess returns the transfers of a rat of alice for the ratd of tt, between dir and it, called
// by the test as the relay calls them: a test then holds a call's context, and its jobs.
func inProcess(t *testing.T, tt *transferTest, dir string) *transfers {
	t.Helper()
	tr, err := newTransfers(slog.New(slog.NewTextHandler(io.Discard, nil)), aliceClient(t, tt), tt.addr, dir,
		func(err error) string { return "ratd not reached: " + err.Error() + "." })
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(tr.close)
	return tr
}

// callTold calls tool with args through tr, its answer written as the relay writes it: told is
// called.
func callTold(t *testing.T, tr *transfers, ctx context.Context, tool string, args map[string]any) (string, bool) {
	t.Helper()
	raw, err := json.Marshal(args)
	if err != nil {
		t.Fatal(err)
	}
	text, isError, told := tr.call(ctx, tool, raw)
	told()
	return text, isError
}

// onlyJob returns the job tr holds, the test having started one alone.
func onlyJob(t *testing.T, tr *transfers) *job {
	t.Helper()
	tr.jobsMu.Lock()
	defer tr.jobsMu.Unlock()
	for _, j := range tr.jobs {
		return j
	}
	t.Fatal("expected a job")
	return nil
}

// TestRealRatdPipeUploadCancelDropped guards a cancel whose call the harness cancels as rat runs it,
// its answer dropped: the close is the job's, not the call's. ratd closes its side all the same, the
// command reading then getting a beginning of the file and the end of its input, the pipe deleted,
// another portal there opening; the next call with the same paths tells the outcome the job kept,
// with ratd's words, the bytes the command saw, which are those it read.
func TestRealRatdPipeUploadCancelDropped(t *testing.T) {
	tt := startRatdTransfers(t, "test-rat-pipedropped")
	tr := inProcess(t, tt, tt.local)
	pipe := filepath.Join(tt.remote, "in.pipe")
	content := randomFile(t, filepath.Join(tt.local, "in.bin"), 64<<20, 0o644)
	args := map[string]any{"local_path": "in.bin", "remote_path": pipe, "remote_pipe": true}
	if text, _ := callTold(t, tr, context.Background(), uploadTool, args); !strings.HasPrefix(text, "The portal is open: ") {
		t.Fatalf("expected the portal ready, got %q", text)
	}
	more := make(chan struct{})
	read, firstRead := readPipe(t, pipe, 64<<10, more)
	<-firstRead
	canceled, cancel := context.WithCancel(context.Background())
	cancel()
	cancelArgs := map[string]any{"local_path": "in.bin", "remote_path": pipe, "remote_pipe": true, "cancel": true}
	raw, err := json.Marshal(cancelArgs)
	if err != nil {
		t.Fatal(err)
	}
	// dropped: told not called
	_, _, _ = tr.call(canceled, uploadTool, raw)
	text, isError := callTold(t, tr, context.Background(), uploadTool, args)
	close(more)
	data := received(t, read)
	if isError || !strings.HasPrefix(text, "Closed the portal from in.bin to "+pipe+" after ") ||
		!strings.Contains(text, " of 64 MiB sent. Closed the portal at "+pipe+": the pipe is deleted. A command still "+
			"reading it saw the end of its input after "+sizeText(int64(len(data)))+", which is not the whole stream") {
		t.Errorf("expected the close told, in ratd's words with the %d bytes the command read, got %v: %q", len(data),
			isError, text)
	}
	if len(data) >= len(content) || !bytes.Equal(data, content[:len(data)]) {
		t.Errorf("expected a beginning of the file, got %d bytes", len(data))
	}
	expectNoPipe(t, pipe)
	file, err := os.Open(filepath.Join(tt.local, "in.bin"))
	if err != nil {
		t.Fatal(err)
	}
	defer file.Close()
	up, err := connect.StartPipeUpload(context.Background(), aliceClient(t, tt), tt.addr, file, pipe)
	if err != nil {
		t.Fatalf("expected another portal opening at the path, ratd's closed, got %v", err)
	}
	_ = up.Close()
}

// TestRealRatdPipeCancelRacesRefusal guards a cancel asked while a portal opens, the opening then
// refused by ratd, something being at the remote path: the outcome is the refusal, in ratd's
// words, not a close, and the cancel tells it as it is; the file there kept.
func TestRealRatdPipeCancelRacesRefusal(t *testing.T) {
	entered, hold := make(chan struct{}), make(chan struct{})
	hook := preparing
	preparing = func(transferRequest) {
		close(entered)
		<-hold
	}
	t.Cleanup(func() { preparing = hook })
	tt := startRatdTransfers(t, "test-rat-piperace")
	tr := inProcess(t, tt, tt.local)
	pipe := filepath.Join(tt.remote, "in.pipe")
	kept := randomFile(t, pipe, 10, 0o644)
	randomFile(t, filepath.Join(tt.local, "in.bin"), 1<<20, 0o644)
	args := map[string]any{"local_path": "in.bin", "remote_path": pipe, "remote_pipe": true}
	raw, err := json.Marshal(args)
	if err != nil {
		t.Fatal(err)
	}
	// the call starting the job, whose answer is dropped
	go func() { _, _, _ = tr.call(context.Background(), uploadTool, raw) }()
	<-entered
	j := onlyJob(t, tr)
	args["cancel"] = true
	answer := make(chan [2]any, 1)
	go func() {
		text, isError := callTold(t, tr, context.Background(), uploadTool, args)
		answer <- [2]any{text, isError}
	}()
	waitUntil(t, "the cancel asked", j.cancelWasAsked)
	close(hold)
	got := <-answer
	if text := got[0].(string); !got[1].(bool) || !strings.HasPrefix(text, pipe+" exists: a portal makes its pipe "+
		"itself") || strings.Contains(text, "Closed") {
		t.Errorf("expected the refusal told as it is, got %v: %q", got[1], text)
	}
	expectContent(t, pipe, kept, 0o644)
}

// TestRealRatdPortalReadyAfterTemporary guards when a download from a pipe is ready (P2): once its
// temporary file exists. Creating it failing, the portal is never told ready, its outcome is the
// failure, and ratd deletes its pipe as it sees the link cut.
func TestRealRatdPortalReadyAfterTemporary(t *testing.T) {
	if os.Geteuid() == 0 {
		t.Skip("root writes in a directory without the write bit: creating the temporary file can not fail here")
	}
	tt := startRatdTransfers(t, "test-rat-portaltmp")
	dir, err := filepath.EvalSymlinks(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	tr := inProcess(t, tt, dir)
	if err := os.Chmod(dir, 0o555); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.Chmod(dir, 0o755) })
	pipe := filepath.Join(tt.remote, "out.pipe")
	raw, err := json.Marshal(map[string]any{"remote_path": pipe, "local_path": "out.tar", "remote_pipe": true})
	if err != nil {
		t.Fatal(err)
	}
	text, isError, told := tr.call(context.Background(), downloadTool, raw)
	j := onlyJob(t, tr)
	told()
	if !isError || text != "Writing out.tar failed: permission denied." {
		t.Errorf("expected the failure to create the temporary file, got %v: %q", isError, text)
	}
	if isReady(j) {
		t.Error("expected the portal never ready, its temporary file never created")
	}
	expectNoPipe(t, pipe)
}

// TestRealRatdPortalEndedBeforeReadyTold guards a portal that ended before its ready was told, the
// call that would have told it dropped: the next call tells its outcome, not that it is open. Run
// several times: such a call finds both the ready and the end, which it may look at in either order.
func TestRealRatdPortalEndedBeforeReadyTold(t *testing.T) {
	tt := startRatdTransfers(t, "test-rat-portalended")
	tr := inProcess(t, tt, tt.local)
	for i := range 8 {
		pipe := filepath.Join(tt.remote, fmt.Sprintf("out%d.pipe", i))
		local := fmt.Sprintf("out%d.tar", i)
		args := map[string]any{"remote_path": pipe, "local_path": local, "remote_pipe": true}
		raw, err := json.Marshal(args)
		if err != nil {
			t.Fatal(err)
		}
		canceled, cancel := context.WithCancel(context.Background())
		cancel()
		// dropped: told not called
		_, _, _ = tr.call(canceled, downloadTool, raw)
		j := onlyJob(t, tr)
		<-j.ready
		if err := <-writeInto(pipe, []byte("data")); err != nil {
			t.Fatal(err)
		}
		<-j.done
		if text, isError := callTold(t, tr, context.Background(), downloadTool, args); isError ||
			!strings.HasPrefix(text, "Downloaded what was written into "+pipe+" to "+local+" (4 B): created.") {
			t.Fatalf("expected the outcome told, got %v: %q", isError, text)
		}
	}
}

// TestRealRatdPortalCanceledWhole guards a download from a pipe canceled once all of it came, before
// it is put in place, the last moment a cancel is in time: closed, the local file not made, no
// temporary file left.
func TestRealRatdPortalCanceledWhole(t *testing.T) {
	// set before rat starts, and restored once it stopped (cleanups run in reverse order)
	hook := downloaded
	downloaded = func(j *job) { j.markCancel() }
	t.Cleanup(func() { downloaded = hook })
	tt := startRatdTransfers(t, "test-rat-portalwhole")
	pipe := filepath.Join(tt.remote, "out.pipe")
	args := map[string]any{"remote_path": pipe, "local_path": "out.tar", "remote_pipe": true}
	tt.expect(t, downloadTool, args, false, "The portal is open: ")
	written := writeInto(pipe, []byte("data"))
	tt.expect(t, downloadTool, args, false, "Closed the portal from "+pipe+" to out.tar after 4 B: out.tar is as it "+
		"was.")
	if err := <-written; err != nil {
		t.Error(err)
	}
	if _, err := os.Lstat(filepath.Join(tt.local, "out.tar")); err == nil {
		t.Error("expected no file put in place")
	}
	expectNoTemporary(t, tt.local)
	expectNoPipe(t, pipe)
}

// tcpRelay relays TCP connections to a server, until cut: the link between rat and ratd, which a
// test loses with both alive.
type tcpRelay struct {
	listener net.Listener
	mu       sync.Mutex
	conns    []net.Conn
}

// startRelay relays to addr until the test ends.
func startRelay(t *testing.T, addr string) *tcpRelay {
	t.Helper()
	l, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	r := &tcpRelay{listener: l}
	t.Cleanup(func() {
		_ = l.Close()
		r.cut()
	})
	go func() {
		for {
			in, err := l.Accept()
			if err != nil {
				return
			}
			out, err := net.Dial("tcp", addr)
			if err != nil {
				_ = in.Close()
				continue
			}
			r.mu.Lock()
			r.conns = append(r.conns, in, out)
			r.mu.Unlock()
			go func() { _, _ = io.Copy(out, in); _ = out.Close() }()
			go func() { _, _ = io.Copy(in, out); _ = in.Close() }()
		}
	}()
	return r
}

// cut closes every connection relayed, each side seeing the other gone.
func (r *tcpRelay) cut() {
	r.mu.Lock()
	defer r.mu.Unlock()
	for _, c := range r.conns {
		_ = c.Close()
	}
	r.conns = nil
}

// TestRealRatdPipeUploadLinkLost guards the link lost while a command reads the pipe of an upload,
// rat and ratd both alive: rat tells the portal broken, the command reading still waiting, having
// seen no end, and the agent to stop it, then cancel; ratd, seeing the cut as the command reads on,
// keeps its input open, its pipe deleted; the cancel closes it, the command then getting a
// beginning of the file and the end of its input.
func TestRealRatdPipeUploadLinkLost(t *testing.T) {
	tt := startRatdTransfers(t, "test-rat-pipelost")
	relay := startRelay(t, tt.addr)
	direct := tt.addr
	tt.addr = relay.listener.Addr().String()
	cut := inProcess(t, tt, tt.local)
	tt.addr = direct
	pipe := filepath.Join(tt.remote, "in.pipe")
	content := randomFile(t, filepath.Join(tt.local, "in.bin"), 64<<20, 0o644)
	args := map[string]any{"local_path": "in.bin", "remote_path": pipe, "remote_pipe": true}
	if text, _ := callTold(t, cut, context.Background(), uploadTool, args); !strings.HasPrefix(text, "The portal is open: ") {
		t.Fatalf("expected the portal ready, got %q", text)
	}
	more := make(chan struct{})
	read, firstRead := readPipe(t, pipe, 64<<10, more)
	<-firstRead
	relay.cut()
	text, isError := callTold(t, cut, context.Background(), uploadTool, args)
	if !isError || !strings.HasPrefix(text, "The portal from in.bin to "+pipe+" was cut after ") ||
		!strings.Contains(text, "If a command was reading "+pipe+" it still waits, it has seen no end: stop it in its "+
			"terminal, then call upload_file again with cancel to close the portal.") {
		t.Errorf("expected the portal told broken, the command reading waiting, got %v: %q", isError, text)
	}
	// ratd sees the cut as it reads the body, once the command reads on: it holds the portal then,
	// its pipe deleted
	close(more)
	expectNoPipe(t, pipe)
	select {
	case data := <-read:
		t.Fatalf("expected the command reading to see no end, it got it after %d bytes", len(data))
	default:
	}
	args["cancel"] = true
	text = tt.expect(t, uploadTool, args, false, "Closed the portal at "+pipe+": the pipe is deleted. A command still "+
		"reading it saw the end of its input after ")
	data := received(t, read)
	if !strings.Contains(text, " after "+sizeText(int64(len(data)))+", which is not the whole stream") {
		t.Errorf("expected the %d bytes the command saw told, got %q", len(data), text)
	}
	if len(data) >= len(content) || !bytes.Equal(data, content[:len(data)]) {
		t.Errorf("expected a beginning of the file, got %d bytes", len(data))
	}
}
