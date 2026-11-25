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
