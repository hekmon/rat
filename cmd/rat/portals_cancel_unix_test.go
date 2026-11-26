//go:build unix

package main

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/hekmon/rat/connect"
)

// slowCloseFake starts a fake ratd whose portal download answers ready once open is closed, then
// waits; whose DELETE answers once answer is closed. It returns in-process transfers of alice.
func slowCloseFake(t *testing.T, open, answer chan struct{}, cut chan struct{}) *transfers {
	t.Helper()
	local, err := filepath.EvalSymlinks(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	bundle := newBundle(t)
	mux := http.NewServeMux()
	mux.HandleFunc("GET "+connect.DownloadPath, func(w http.ResponseWriter, r *http.Request) {
		select {
		case <-open:
		case <-r.Context().Done():
			close(cut)
			return
		}
		w.Header().Set(connect.HeaderVersion, "test")
		w.Header().Set(connect.HeaderPipe, "true")
		w.Header().Set("Trailer", connect.HeaderError)
		w.WriteHeader(http.StatusOK)
		_ = http.NewResponseController(w).Flush()
		<-r.Context().Done()
	})
	mux.HandleFunc("DELETE "+connect.DownloadPath, func(w http.ResponseWriter, r *http.Request) {
		select {
		case <-answer:
		case <-r.Context().Done():
			return
		}
		w.Header().Set(connect.HeaderVersion, "test")
		http.Error(w, "Closed the portal at /remote/out.pipe: the pipe is deleted. No command had opened it.",
			http.StatusOK)
	})
	addr := fakeRatd(t, bundle, mux, nil)
	release := func(c chan struct{}) {
		select {
		case <-c:
		default:
			close(c)
		}
	}
	t.Cleanup(func() { release(answer); release(open) })
	tt := &transferTest{local: local, remote: "/remote", addr: addr, bundle: bundle}
	return inProcess(t, tt, local)
}

// dropped makes a call of tool with args whose context is done already, as a harness giving up on
// it: its answer is never told.
func dropped(t *testing.T, tr *transfers, tool string, args map[string]any) {
	t.Helper()
	raw, err := json.Marshal(args)
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	_, _, _ = tr.call(ctx, tool, raw)
}

// TestPortalNotToldReadyOnceCancelAsked checks that a portal whose close was asked is never told
// ready: its opening call dropped before the ready was told, then a cancel that ratd is slow to
// answer, told still ending, a plain call with the same paths is told still ending too, not that the
// portal is open and waits for a command, which the job is about to close. A second cancel meanwhile
// is answered as the first, and rat stopping is not delayed by the close in flight.
func TestPortalNotToldReadyOnceCancelAsked(t *testing.T) {
	shortWait(t, 300*time.Millisecond)
	open, answer, cut := make(chan struct{}), make(chan struct{}), make(chan struct{})
	close(open)
	tr := slowCloseFake(t, open, answer, cut)
	args := map[string]any{"remote_path": "/remote/out.pipe", "local_path": "out.tar", "remote_pipe": true}
	dropped(t, tr, downloadTool, args)
	j := onlyJob(t, tr)
	<-j.ready
	cancelArgs := map[string]any{"remote_path": "/remote/out.pipe", "local_path": "out.tar", "remote_pipe": true,
		"cancel": true}
	for range 2 {
		if text, _ := callTold(t, tr, context.Background(), downloadTool, cancelArgs); !strings.Contains(text, "still ending") {
			t.Fatalf("a cancel whose close ratd has not answered told %q, expected still ending", text)
		}
	}
	text, isError := callTold(t, tr, context.Background(), downloadTool, args)
	if !isError || !strings.Contains(text, "still ending") {
		t.Errorf("a portal being closed told %q (error %v) to a plain call, expected still ending", text, isError)
	}
	stopped := make(chan struct{})
	go func() { tr.close(); close(stopped) }()
	select {
	case <-stopped:
	case <-time.After(30 * time.Second):
		t.Fatal("rat stopping waited for the close ratd does not answer")
	}
}

// TestPortalCancelGivesUpOpening checks a cancel asked while ratd does not answer the opening of a
// portal: the job waits for the opening transferWait, then cuts its request and ends, telling the
// opening was not answered, cut on this side, and to cancel again for what ratd may hold, rather
// than "still ending" for as long as the connection lasts; that cancel, the job told and forgotten,
// asks ratd. Without a cancel, the opening is not cut.
func TestPortalCancelGivesUpOpening(t *testing.T) {
	shortWait(t, 300*time.Millisecond)
	open, answer, cut := make(chan struct{}), make(chan struct{}), make(chan struct{})
	close(answer)
	tr := slowCloseFake(t, open, answer, cut)
	args := map[string]any{"remote_path": "/remote/out.pipe", "local_path": "out.tar", "remote_pipe": true}
	if text, _ := callTold(t, tr, context.Background(), downloadTool, args); !strings.HasPrefix(text, "Still opening the portal") {
		t.Fatalf("expected the portal told opening still, got %q", text)
	}
	select {
	case <-cut:
		t.Fatal("the opening was cut without a cancel asked")
	default:
	}
	cancelArgs := map[string]any{"remote_path": "/remote/out.pipe", "local_path": "out.tar", "remote_pipe": true,
		"cancel": true}
	var text string
	var isError bool
	// the cancel and the job wait the same time: the first cancel may tell it still ending
	for range 3 {
		text, isError = callTold(t, tr, context.Background(), downloadTool, cancelArgs)
		if !strings.Contains(text, "still ending") {
			break
		}
	}
	if !isError || !strings.HasPrefix(text, "The opening of the portal from /remote/out.pipe to out.tar was not "+
		"answered by ratd, and is cut on this side, as asked. Its side of the portal at /remote/out.pipe could not be "+
		"told to close, and may still be open: call download_file again with cancel.") {
		t.Errorf("expected the opening told given up, got %v: %q", isError, text)
	}
	select {
	case <-cut:
	case <-time.After(30 * time.Second):
		t.Fatal("the request of the opening was not cut")
	}
	// told, the job is forgotten: the cancel asks ratd
	text, _ = callTold(t, tr, context.Background(), downloadTool, cancelArgs)
	if !strings.HasPrefix(text, "Closed the portal at /remote/out.pipe") {
		t.Errorf("expected the next cancel to ask ratd, got %q", text)
	}
}

// eofTransport is the transport of a rat under test, calling atEOF once the body of an answer to a
// GET, a download, is read to its end, before that read returns: a test acts at the moment rat has
// its stream whole, ahead of what rat does next.
type eofTransport struct {
	next  http.RoundTripper
	atEOF func()
}

func (e eofTransport) RoundTrip(r *http.Request) (*http.Response, error) {
	resp, err := e.next.RoundTrip(r)
	if err == nil && r.Method == http.MethodGet {
		resp.Body = &eofBody{ReadCloser: resp.Body, atEOF: e.atEOF}
	}
	return resp, err
}

// eofBody is a body calling atEOF once a read of it returns io.EOF.
type eofBody struct {
	io.ReadCloser
	once  sync.Once
	atEOF func()
}

func (b *eofBody) Read(p []byte) (int, error) {
	n, err := b.ReadCloser.Read(p)
	if err == io.EOF {
		b.once.Do(b.atEOF)
	}
	return n, err
}

// eofFake starts a fake ratd serving mux, and returns in-process transfers of alice for it whose
// downloads call atEOF once their stream came whole (see eofTransport).
func eofFake(t *testing.T, mux *http.ServeMux, atEOF func()) *transfers {
	t.Helper()
	local, err := filepath.EvalSymlinks(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	bundle := newBundle(t)
	addr := fakeRatd(t, bundle, mux, nil)
	tt := &transferTest{local: local, remote: "/remote", addr: addr, bundle: bundle}
	client := aliceClient(t, tt)
	client.Transport = eofTransport{next: client.Transport, atEOF: atEOF}
	tr, err := newTransfers(slog.New(slog.NewTextHandler(io.Discard, nil)), client, addr, local,
		func(err error) string { return "ratd not reached: " + err.Error() + "." })
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(tr.close)
	return tr
}

// portalHeaders writes the headers of a portal download ready, as ratd answers it.
func portalHeaders(w http.ResponseWriter) {
	w.Header().Set(connect.HeaderVersion, "test")
	w.Header().Set(connect.HeaderPipe, "true")
	w.Header().Set("Trailer", connect.HeaderError)
	w.WriteHeader(http.StatusOK)
}

// TestPortalCloseAskedAsStreamEnds guards a close asked as the stream of a download portal ends by
// itself, whole: ratd asked, and answering, as rat reads the end of the stream. The close wins, the
// stream whole or not: the local file is not put in place, no temporary file is left, and the
// answer is the portal closed with what ratd said of its pipe (P11), not a cancel told without it.
func TestPortalCloseAskedAsStreamEnds(t *testing.T) {
	finish, eof := make(chan struct{}), make(chan struct{})
	var finishOnce sync.Once
	byRatd := "Closed the portal at /remote/out.pipe after 4 B: the pipe is deleted, and the command writing into " +
		"it had closed its end."
	mux := http.NewServeMux()
	mux.HandleFunc("GET "+connect.DownloadPath, func(w http.ResponseWriter, r *http.Request) {
		portalHeaders(w)
		_ = http.NewResponseController(w).Flush()
		select {
		case <-finish:
		case <-r.Context().Done():
			return
		}
		// the command writing closed its end: the stream ends whole, no trailer telling otherwise
		_, _ = w.Write([]byte("data"))
	})
	mux.HandleFunc("DELETE "+connect.DownloadPath, func(w http.ResponseWriter, r *http.Request) {
		// the close asked: the stream ends meanwhile, read whole by rat before ratd answers
		finishOnce.Do(func() { close(finish) })
		select {
		case <-eof:
		case <-r.Context().Done():
			return
		}
		w.Header().Set(connect.HeaderVersion, "test")
		http.Error(w, byRatd, http.StatusOK)
	})
	tr := eofFake(t, mux, func() { close(eof) })
	t.Cleanup(func() { finishOnce.Do(func() { close(finish) }) })
	args := map[string]any{"remote_path": "/remote/out.pipe", "local_path": "out.tar", "remote_pipe": true}
	if text, _ := callTold(t, tr, context.Background(), downloadTool, args); !strings.HasPrefix(text, "The portal is open: ") {
		t.Fatalf("expected the portal ready, got %q", text)
	}
	args["cancel"] = true
	text, isError := callTold(t, tr, context.Background(), downloadTool, args)
	if want := "Closed the portal from /remote/out.pipe to out.tar after 4 B: out.tar is as it was. " + byRatd; isError ||
		text != want {
		t.Errorf("expected the close told with ratd's words, got %v: %q, expected %q", isError, text, want)
	}
	if _, err := os.Lstat(filepath.Join(tr.dir, "out.tar")); err == nil {
		t.Error("expected no file put in place")
	}
	expectNoTemporary(t, tr.dir)
}

// TestCancelWithoutJobBounded guards a cancel finding no job of rat at its paths while ratd does not
// answer the close it asks (P12): the call returns once transferWait passed, told that ratd's side
// could not be told to close and to cancel again, rather than waiting for ratd's answer.
func TestCancelWithoutJobBounded(t *testing.T) {
	shortWait(t, 500*time.Millisecond)
	open, answer, cut := make(chan struct{}), make(chan struct{}), make(chan struct{})
	tr := slowCloseFake(t, open, answer, cut)
	args := map[string]any{"remote_path": "/remote/out.pipe", "local_path": "out.tar", "remote_pipe": true,
		"cancel": true}
	type called struct {
		text    string
		isError bool
	}
	got := make(chan called, 1)
	go func() {
		text, isError := callTold(t, tr, context.Background(), downloadTool, args)
		got <- called{text, isError}
	}()
	select {
	case c := <-got:
		if !c.isError || !strings.HasPrefix(c.text, "No transfer to out.tar is under way on this side. ratd not "+
			"reached: ") || !strings.HasSuffix(c.text, "Its side of the portal at /remote/out.pipe could not be told "+
			"to close, and may still be open: call download_file again with cancel.") {
			t.Errorf("expected ratd's side told possibly open, got %v: %q", c.isError, c.text)
		}
	case <-time.After(30 * time.Second):
		// released, for the call and the test to end
		close(answer)
		t.Fatal("the cancel waited for ratd's answer to the close")
	}
}

// TestPortalCloseAskedAsRequestEnds guards a cancel marked just before the request of a download
// portal ends by itself, its stream whole: the close is the job's all the same, ratd asked and its
// answer told (see closeOnCancel), even when the watch of the close finds the cancel and the end of
// the request there at once. That moment is a race inside rat, provoked here: with a single
// processor, and the stream read whole without waiting, the watch first runs once the job waits for
// it, finding both. Run many times, select picking either of the two at random.
func TestPortalCloseAskedAsRequestEnds(t *testing.T) {
	previous := runtime.GOMAXPROCS(1)
	t.Cleanup(func() { runtime.GOMAXPROCS(previous) })
	var deletes atomic.Int32
	byRatd := "Closed the portal after 4 B: the pipe is deleted, and the command writing into it had closed its end."
	mux := http.NewServeMux()
	mux.HandleFunc("GET "+connect.DownloadPath, func(w http.ResponseWriter, r *http.Request) {
		// the stream whole with its headers, for rat to read it without waiting
		portalHeaders(w)
		_, _ = w.Write([]byte("data"))
	})
	mux.HandleFunc("DELETE "+connect.DownloadPath, func(w http.ResponseWriter, r *http.Request) {
		deletes.Add(1)
		w.Header().Set(connect.HeaderVersion, "test")
		http.Error(w, byRatd, http.StatusOK)
	})
	var tr *transfers
	tr = eofFake(t, mux, func() {
		// in the job, its stream whole: the cancel marked before it looks
		tr.jobsMu.Lock()
		defer tr.jobsMu.Unlock()
		for _, j := range tr.jobs {
			j.markCancel()
		}
	})
	for i := range 64 {
		local := fmt.Sprintf("out%d.tar", i)
		args := map[string]any{"remote_path": fmt.Sprintf("/remote/out%d.pipe", i), "local_path": local,
			"remote_pipe": true}
		before := deletes.Load()
		text, isError := callTold(t, tr, context.Background(), downloadTool, args)
		if strings.HasPrefix(text, "The portal is open: ") {
			text, isError = callTold(t, tr, context.Background(), downloadTool, args)
		}
		want := fmt.Sprintf("Closed the portal from /remote/out%d.pipe to %s after 4 B: %s is as it was. %s", i, local,
			local, byRatd)
		if isError || text != want || deletes.Load() == before {
			t.Fatalf("run %d: expected ratd asked to close and its answer told, got %v: %q (ratd asked %d times "+
				"before, %d now)", i, isError, text, before, deletes.Load())
		}
		if _, err := os.Lstat(filepath.Join(tr.dir, local)); err == nil {
			t.Fatalf("run %d: expected no file put in place", i)
		}
	}
	expectNoTemporary(t, tr.dir)
}
