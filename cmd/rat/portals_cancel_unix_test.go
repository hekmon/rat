//go:build unix

package main

import (
	"context"
	"encoding/json"
	"net/http"
	"path/filepath"
	"strings"
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
