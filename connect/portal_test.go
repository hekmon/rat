package connect

import (
	"bytes"
	"context"
	"errors"
	"io"
	"net/http"
	"testing"
	"time"
)

// TestPipeDownload guards a download from a pipe as ratd sends it: a GET asking Rat-Pipe, its
// answer echoing it, with no size nor mode, the download a pipe of mode 0666 and no size; the body
// ending cleanly is the download done, whatever its length; the Rat-Error trailer fails it with
// ratd's sentence, and a body cut short with a TransferError of kind ErrCut. An answer without the
// echo, from a ratd older than the portals, fails with ErrNoPortals, a file it sends as a refusal
// it writes, while a 404 without the version still tells a ratd older than the transfers.
func TestPipeDownload(t *testing.T) {
	server, client := fakeRatd(t, func(w http.ResponseWriter, r *http.Request) {
		path, _ := DecodePath(r.Header.Get(HeaderPath))
		if r.Header.Get(HeaderPipe) != "true" {
			t.Errorf("%s: expected Rat-Pipe asked, got %v", path, r.Header)
		}
		switch path {
		case "/old":
			http.NotFound(w, r)
			return
		case "/file":
			// a ratd serving the transfers without the portals sends the file there
			w.Header().Set(HeaderVersion, "v0")
			w.Header().Set(HeaderMode, "0644")
			w.Header().Set(HeaderSize, "4")
			_, _ = w.Write([]byte("file"))
			return
		case "/missing":
			w.Header().Set(HeaderVersion, "v0")
			http.Error(w, "Reading /missing failed: no such file or directory.", http.StatusNotFound)
			return
		case "/exists":
			w.Header().Set(HeaderVersion, "v1")
			w.Header().Set(HeaderPipe, "true")
			http.Error(w, "/exists exists.", http.StatusConflict)
			return
		}
		w.Header().Set(HeaderVersion, "v1")
		w.Header().Set(HeaderPipe, "true")
		w.Header().Set("Trailer", HeaderError)
		_, _ = w.Write(bytes.Repeat([]byte("x"), 500))
		switch path {
		case "/closed":
			w.Header().Set(HeaderError, "The portal at /closed was closed, as asked: its pipe is deleted.")
		case "/cut":
			http.NewResponseController(w).Flush()
			panic(http.ErrAbortHandler)
		}
	})
	ctx := context.Background()
	options := DownloadOptions{Pipe: true}
	for path, expected := range map[string]error{"/old": ErrNoTransfers, "/file": ErrNoPortals, "/missing": ErrNoPortals} {
		if _, err := StartDownload(ctx, client, server, path, options); !errors.Is(err, expected) {
			t.Errorf("%s: expected %v, got %v", path, expected, err)
		}
	}
	var refused *Refusal
	if _, err := StartDownload(ctx, client, server, "/exists", options); !errors.As(err, &refused) ||
		refused.Status != http.StatusConflict || refused.Text != "/exists exists." {
		t.Errorf("expected the refusal of ratd, got %v", err)
	}
	d, err := StartDownload(ctx, client, server, "/whole", options)
	if err != nil {
		t.Fatal(err)
	}
	if !d.Pipe || d.Mode != 0o666 || d.Size != -1 {
		t.Errorf("expected a pipe of mode 0666 and no size, got %v, %v, %d", d.Pipe, d.Mode, d.Size)
	}
	if n, err := d.WriteTo(io.Discard); n != 500 || err != nil {
		t.Errorf("expected the 500 bytes, done, got %d, %v", n, err)
	}
	_ = d.Close()
	var failed *TransferError
	for path, expected := range map[string]func(error) bool{
		"/closed": func(err error) bool {
			return errors.As(err, &refused) && refused.Status == http.StatusOK &&
				refused.Text == "The portal at /closed was closed, as asked: its pipe is deleted."
		},
		"/cut": func(err error) bool {
			return errors.Is(err, ErrCut) && errors.As(err, &failed) && errors.Is(failed.Reason, io.ErrUnexpectedEOF)
		},
	} {
		d, err := StartDownload(ctx, client, server, path, options)
		if err != nil {
			t.Fatalf("%s: %v", path, err)
		}
		if _, err = d.WriteTo(io.Discard); !expected(err) {
			t.Errorf("%s: unexpected error %v", path, err)
		}
		_ = d.Close()
	}
}

// TestClosePortal guards the close of a portal: a DELETE of the endpoint naming the path, download
// or upload; a 200
// closed one, its sentence returned; a 404 with the version of ratd closed none, its sentence
// returned too; a 405, the mux of a ratd older than the portals, fails with ErrNoPortals, a 404
// without the version with ErrNoTransfers, and another refusal is a Refusal.
func TestClosePortal(t *testing.T) {
	server, client := fakeRatd(t, func(w http.ResponseWriter, r *http.Request) {
		path, _ := DecodePath(r.Header.Get(HeaderPath))
		if r.Method != http.MethodDelete || (r.URL.Path != DownloadPath && r.URL.Path != UploadPath) {
			t.Errorf("unexpected request: %s %s", r.Method, r.URL.Path)
		}
		switch path {
		case "/old":
			http.NotFound(w, r)
		case "/older":
			http.Error(w, "Method Not Allowed", http.StatusMethodNotAllowed)
		case "/none":
			w.Header().Set(HeaderVersion, "v1")
			http.Error(w, "No portal is open at /none: nothing to close.", http.StatusNotFound)
		case "/relative":
			w.Header().Set(HeaderVersion, "v1")
			http.Error(w, "relative", http.StatusBadRequest)
		default:
			w.Header().Set(HeaderVersion, "v1")
			_, _ = io.WriteString(w, "Closed the portal at /p: the pipe is deleted.")
		}
	})
	ctx := context.Background()
	if text, closed, err := ClosePortal(ctx, client, server, DownloadPath, "/p"); err != nil || !closed ||
		text != "Closed the portal at /p: the pipe is deleted." {
		t.Errorf("expected the portal closed, got %q, %v, %v", text, closed, err)
	}
	if text, closed, err := ClosePortal(ctx, client, server, UploadPath, "/p"); err != nil || !closed ||
		text != "Closed the portal at /p: the pipe is deleted." {
		t.Errorf("upload: expected the portal closed, got %q, %v, %v", text, closed, err)
	}
	if text, closed, err := ClosePortal(ctx, client, server, DownloadPath, "/none"); err != nil || closed ||
		text != "No portal is open at /none: nothing to close." {
		t.Errorf("expected none closed, got %q, %v, %v", text, closed, err)
	}
	if _, _, err := ClosePortal(ctx, client, server, DownloadPath, "/older"); !errors.Is(err, ErrNoPortals) {
		t.Errorf("a 405: expected ErrNoPortals, got %v", err)
	}
	if _, _, err := ClosePortal(ctx, client, server, DownloadPath, "/old"); !errors.Is(err, ErrNoTransfers) {
		t.Errorf("a 404 without the version: expected ErrNoTransfers, got %v", err)
	}
	var refused *Refusal
	if _, _, err := ClosePortal(ctx, client, server, DownloadPath, "/relative"); !errors.As(err, &refused) ||
		refused.Status != http.StatusBadRequest {
		t.Errorf("expected a refusal, got %v", err)
	}
}

// gatedFile is a file to upload whose reads wait until its gate is opened: a test tells what
// happens before any byte of it is sent.
type gatedFile struct {
	*testFile
	gate chan struct{}
}

func (f *gatedFile) Read(p []byte) (int, error) {
	<-f.gate
	return f.testFile.Read(p)
}

// pipeAnswer answers an upload into a pipe as ratd does: both directions enabled, Rat-Pipe echoed,
// the Rat-Error trailer announced, headers flushed before any byte of the body is read.
func pipeAnswer(t *testing.T, w http.ResponseWriter) {
	t.Helper()
	controller := http.NewResponseController(w)
	if err := controller.EnableFullDuplex(); err != nil {
		t.Error(err)
	}
	w.Header().Set(HeaderVersion, "v1")
	w.Header().Set(HeaderPipe, "true")
	w.Header().Set("Trailer", HeaderError)
	w.WriteHeader(http.StatusOK)
	if err := controller.Flush(); err != nil {
		t.Error(err)
	}
}

// TestPipeUpload guards an upload into a pipe as ratd answers it: a POST of UploadPath asking
// Rat-Pipe, chunked, with the path encoded, without Expect (whose go-ahead a final answer cancels)
// nor Rat-Mode (which an older ratd requires before reading a body), its file not read before ratd
// answered; StartPipeUpload returning at the headers, the file unread then, and Wait at the end of
// the answer: done once it ends cleanly, the file sent whole; the Rat-Error trailer a Refusal with
// ratd's sentence; the answer cut a TransferError of kind ErrCut; an answer ending cleanly before the
// file was sent whole a cut, not done. An answer without the echo, from a ratd older than the
// portals, fails with ErrNoPortals, a 404 without the version with ErrNoTransfers, and another
// refusal is a Refusal.
func TestPipeUpload(t *testing.T) {
	content := bytes.Repeat([]byte("data\n"), 200_000)
	// answered tells the fake the client got the answer to /cut: cut before, the client may return
	// the failure of its write rather than the answer
	answered := make(chan struct{})
	server, client := fakeRatd(t, func(w http.ResponseWriter, r *http.Request) {
		path, _ := DecodePath(r.Header.Get(HeaderPath))
		if r.Method != http.MethodPost || r.URL.Path != UploadPath || r.Header.Get(HeaderPipe) != "true" ||
			r.Header.Get("Expect") != "" || r.Header.Get(HeaderMode) != "" || r.Header.Get("Content-Type") != OctetStream ||
			len(r.TransferEncoding) != 1 || r.TransferEncoding[0] != "chunked" {
			t.Errorf("%s: unexpected request %s %s %v %v", path, r.Method, r.URL.Path, r.TransferEncoding, r.Header)
		}
		switch path {
		case "/old":
			http.NotFound(w, r)
			return
		case "/older":
			w.Header().Set(HeaderVersion, "v0")
			http.Error(w, "Rat-Mode \"\": expected permission bits in octal, such as 0644.", http.StatusBadRequest)
			return
		case "/exists":
			w.Header().Set(HeaderVersion, "v1")
			w.Header().Set(HeaderPipe, "true")
			http.Error(w, "/exists exists.", http.StatusConflict)
			return
		}
		pipeAnswer(t, w)
		switch path {
		case "/whole":
			if body, err := io.ReadAll(r.Body); err != nil || !bytes.Equal(body, content) {
				t.Errorf("expected the file whole, got %d bytes, %v", len(body), err)
			}
		case "/stopped":
			_, _ = io.ReadFull(r.Body, make([]byte, 1000))
			w.Header().Set(HeaderError, EncodeError("The command reading /stopped left before the end."))
		case "/short":
			_, _ = io.ReadFull(r.Body, make([]byte, 1000))
		case "/cut":
			<-answered
			_, _ = io.ReadFull(r.Body, make([]byte, 1000))
			panic(http.ErrAbortHandler)
		}
	})
	ctx := context.Background()
	for path, expected := range map[string]error{"/old": ErrNoTransfers, "/older": ErrNoPortals} {
		if _, err := StartPipeUpload(ctx, client, server, &testFile{content: content, mode: 0o644}, path); !errors.Is(err,
			expected) {
			t.Errorf("%s: expected %v, got %v", path, expected, err)
		}
	}
	var refused *Refusal
	if _, err := StartPipeUpload(ctx, client, server, &testFile{content: content, mode: 0o644}, "/exists"); !errors.As(err,
		&refused) || refused.Status != http.StatusConflict || refused.Text != "/exists exists." {
		t.Errorf("expected the refusal of ratd, got %v", err)
	}
	file := &gatedFile{testFile: &testFile{content: content, mode: 0o644}, gate: make(chan struct{})}
	up, err := StartPipeUpload(ctx, client, server, file, "/whole")
	if err != nil {
		t.Fatal(err)
	}
	if file.taken() != 0 || up.Sent() != 0 {
		t.Errorf("expected nothing read of the file at the answer, got %d", file.taken())
	}
	close(file.gate)
	if n, err := up.Wait(); n != int64(len(content)) || err != nil {
		t.Errorf("expected the file sent whole, done, got %d, %v", n, err)
	}
	_ = up.Close()
	var failed *TransferError
	for path, expected := range map[string]func(error) bool{
		"/stopped": func(err error) bool {
			return errors.As(err, &refused) && refused.Status == http.StatusOK &&
				refused.Text == "The command reading /stopped left before the end."
		},
		"/short": func(err error) bool { return errors.Is(err, ErrCut) },
		"/cut": func(err error) bool {
			return errors.Is(err, ErrCut) && errors.As(err, &failed)
		},
	} {
		// far more than the buffers of the connection hold: the answer ends before the file was sent
		up, err := StartPipeUpload(ctx, client, server, &testFile{size: 256 << 20, mode: 0o644}, path)
		if err != nil {
			t.Fatalf("%s: %v", path, err)
		}
		if path == "/cut" {
			close(answered)
		}
		if _, err = up.Wait(); !expected(err) {
			t.Errorf("%s: unexpected error %v", path, err)
		}
		_ = up.Close()
	}
}

// TestPipeUploadFailed guards the body of an upload into a pipe failing as a file upload's does:
// the file changing (growing once read), or failing to read, cuts the body, ratd seeing it cut, and
// Wait fails with ErrChanged, or a TransferError of kind ErrRead; the context cancelled cuts it
// whatever was sent, Wait failing with its cause.
func TestPipeUploadFailed(t *testing.T) {
	cut := make(chan error, 3)
	server, client := fakeRatd(t, func(w http.ResponseWriter, r *http.Request) {
		pipeAnswer(t, w)
		_, err := io.Copy(io.Discard, r.Body)
		cut <- err
	})
	content := bytes.Repeat([]byte("x"), 64<<10)
	reading := errors.New("input/output error")
	for _, tc := range []struct {
		what     string
		file     *testFile
		expected func(error) bool
	}{
		{"changed", &testFile{content: content, mode: 0o644, grows: true}, func(err error) bool {
			return errors.Is(err, ErrChanged)
		}},
		{"read failing", &testFile{content: content, mode: 0o644, fails: reading}, func(err error) bool {
			var failed *TransferError
			return errors.Is(err, ErrRead) && errors.As(err, &failed) && failed.Reason == reading
		}},
	} {
		up, err := StartPipeUpload(context.Background(), client, server, tc.file, "/x")
		if err == nil {
			_, err = up.Wait()
			_ = up.Close()
		}
		if !tc.expected(err) {
			t.Errorf("%s: unexpected error %v", tc.what, err)
		}
		if err := <-cut; err == nil {
			t.Errorf("%s: expected ratd to see the body cut", tc.what)
		}
	}
	ctx, cancel := context.WithCancel(context.Background())
	file := &gatedFile{testFile: &testFile{content: content, mode: 0o644}, gate: make(chan struct{})}
	up, err := StartPipeUpload(ctx, client, server, file, "/x")
	if err != nil {
		t.Fatal(err)
	}
	defer up.Close()
	cancel()
	// the cancel takes its course in a goroutine of its own: the file read once it did
	for deadline := time.Now().Add(10 * time.Second); up.body.failure() == nil && time.Now().Before(deadline); {
		time.Sleep(time.Millisecond)
	}
	close(file.gate)
	if _, err := up.Wait(); !errors.Is(err, context.Canceled) {
		t.Errorf("cancelled: expected context.Canceled, got %v", err)
	}
	if err := <-cut; err == nil {
		t.Error("cancelled: expected ratd to see the body cut")
	}
}
