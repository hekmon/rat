package main

import (
	"log/slog"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/hekmon/rat/connect"
)

// startPortalFake starts a fake ratd serving MCP and downloads with download, for what a test of
// a portal must provoke, and rat offering the transfers, with a harness connected to it.
func startPortalFake(t *testing.T, download http.HandlerFunc) *transferTest {
	t.Helper()
	local, err := filepath.EvalSymlinks(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	bundle := newBundle(t)
	mux := http.NewServeMux()
	mux.Handle("/mcp", newMCPServer().handler)
	mux.HandleFunc("GET "+connect.DownloadPath, download)
	addr := fakeRatd(t, bundle, mux, nil)
	p := startRatTransfers(t, bundle, addr, slog.LevelInfo, local)
	return &transferTest{rat: p, session: connectHarness(t, p, ""), local: local, remote: "/remote", addr: addr,
		bundle: bundle}
}

// TestPortalCut guards a portal cut once ready, the connection closed midway as when ratd is
// killed: the call following it tells it cut, the local file untouched and no temporary file left,
// ratd deleting its pipe as it sees the cut, the agent to open it again.
func TestPortalCut(t *testing.T) {
	release := make(chan struct{})
	tt := startPortalFake(t, func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set(connect.HeaderVersion, "test")
		w.Header().Set(connect.HeaderPipe, "true")
		w.Header().Set("Trailer", connect.HeaderError)
		w.WriteHeader(http.StatusOK)
		_ = http.NewResponseController(w).Flush()
		<-release
		_, _ = w.Write([]byte("begin"))
		_ = http.NewResponseController(w).Flush()
		panic(http.ErrAbortHandler)
	})
	args := map[string]any{"remote_path": "/remote/out.pipe", "local_path": "out.tar", "remote_pipe": true}
	tt.expect(t, downloadTool, args, false, "The portal is open: /remote/out.pipe is a pipe")
	close(release)
	text := tt.expect(t, downloadTool, args, true, "The portal from /remote/out.pipe to out.tar was cut after ")
	if !strings.Contains(text, "out.tar is as it was. ratd deletes the pipe on the machine of the terminals as it "+
		"sees the cut, the command writing into it getting a broken pipe: open the portal again once the cause is "+
		"fixed.") {
		t.Errorf("expected the cut told, the local file as it was, got %q", text)
	}
	if _, err := os.Lstat(filepath.Join(tt.local, "out.tar")); err == nil {
		t.Error("expected no local file")
	}
	expectNoTemporary(t, tt.local)
}

// TestPortalOlderRatd guards a portal asked of a ratd older than the portals, which answers
// without echoing Rat-Pipe, here a refusal of a missing file: told older, for whoever runs it to
// upgrade it, rather than relay a refusal telling nothing of a pipe; no local file made.
func TestPortalOlderRatd(t *testing.T) {
	tt := startPortalFake(t, func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set(connect.HeaderVersion, "old")
		http.Error(w, "Reading /remote/out.pipe failed: no such file or directory.", http.StatusNotFound)
	})
	tt.expect(t, downloadTool, map[string]any{"remote_path": "/remote/out.pipe", "local_path": "out.tar",
		"remote_pipe": true}, true, "The ratd at "+tt.addr+" is older than this rat and opens no portal: whoever runs "+
		"it must upgrade it.")
	if _, err := os.Lstat(filepath.Join(tt.local, "out.tar")); err == nil {
		t.Error("expected no local file")
	}
}

// TestPortalUploadOptionsRefused guards overwrite and in_place with remote_pipe on upload_file:
// refused before any request, with a sentence, a pipe being made where nothing is.
func TestPortalUploadOptionsRefused(t *testing.T) {
	tt := startPortalFake(t, func(w http.ResponseWriter, r *http.Request) {
		t.Errorf("unexpected request: %s %s", r.Method, r.URL.Path)
	})
	randomFile(t, filepath.Join(tt.local, "in.bin"), 10, 0o644)
	for _, option := range []string{"overwrite", "in_place"} {
		tt.expect(t, uploadTool, map[string]any{"local_path": "in.bin", "remote_path": "/remote/in.pipe",
			"remote_pipe": true, option: true}, true, "overwrite and in_place mean nothing with remote_pipe: rat makes "+
			"the pipe at remote_path, where nothing may exist. Call upload_file again without them.")
	}
}

// TestPortalCancelWaitsBounded guards a cancel of a portal ready while ratd does not answer the
// close (a link swallowing packets, as rat sees it): the call waits transferWait at most (P12),
// telling the portal still ending, rather than for ratd's answer; once ratd answers, the next call
// tells the portal closed, with ratd's words.
func TestPortalCancelWaitsBounded(t *testing.T) {
	shortWait(t, 300*time.Millisecond)
	local, err := filepath.EvalSymlinks(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	answer := make(chan struct{})
	bundle := newBundle(t)
	mux := http.NewServeMux()
	mux.Handle("/mcp", newMCPServer().handler)
	mux.HandleFunc("GET "+connect.DownloadPath, func(w http.ResponseWriter, r *http.Request) {
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
	// answered before the fake stops, which waits for its handlers (cleanups run in reverse order)
	t.Cleanup(func() {
		select {
		case <-answer:
		default:
			close(answer)
		}
	})
	p := startRatTransfers(t, bundle, addr, slog.LevelInfo, local)
	tt := &transferTest{rat: p, session: connectHarness(t, p, ""), local: local, remote: "/remote", addr: addr,
		bundle: bundle}
	args := map[string]any{"remote_path": "/remote/out.pipe", "local_path": "out.tar", "remote_pipe": true}
	tt.expect(t, downloadTool, args, false, "The portal is open: ")
	args["cancel"] = true
	type called struct {
		text    string
		isError bool
	}
	got := make(chan called, 1)
	go func() {
		text, isError := tt.call(t, downloadTool, args)
		got <- called{text, isError}
	}()
	select {
	case c := <-got:
		if !c.isError || c.text != "The transfer to out.tar is still ending: call download_file again with the same "+
			"paths to know how it ended." {
			t.Errorf("expected the portal told still ending, got %v: %q", c.isError, c.text)
		}
	case <-time.After(20 * time.Second):
		// released, for the test to end
		close(answer)
		t.Fatal("the cancel waited for ratd's answer to the close")
	}
	close(answer)
	delete(args, "cancel")
	shortWait(t, 20*time.Second)
	tt.expect(t, downloadTool, args, false, "Closed the portal from /remote/out.pipe to out.tar after 0 B: out.tar is "+
		"as it was. Closed the portal at /remote/out.pipe: the pipe is deleted. No command had opened it.")
	expectNoTemporary(t, tt.local)
}

// TestPortalCancelNotTold guards a cancel of a portal ready that ratd could not be told to close
// (the request of the close failing): the outcome is an error, telling rat's side cut, the local
// file as it was, and ratd's side possibly still open, to cancel again; the job told, that cancel
// asks ratd again, rat holding no job of the path.
func TestPortalCancelNotTold(t *testing.T) {
	local, err := filepath.EvalSymlinks(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	bundle := newBundle(t)
	mux := http.NewServeMux()
	mux.Handle("/mcp", newMCPServer().handler)
	mux.HandleFunc("GET "+connect.DownloadPath, func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set(connect.HeaderVersion, "test")
		w.Header().Set(connect.HeaderPipe, "true")
		w.Header().Set("Trailer", connect.HeaderError)
		w.WriteHeader(http.StatusOK)
		_ = http.NewResponseController(w).Flush()
		<-r.Context().Done()
	})
	var deletes atomic.Int32
	mux.HandleFunc("DELETE "+connect.DownloadPath, func(w http.ResponseWriter, r *http.Request) {
		deletes.Add(1)
		panic(http.ErrAbortHandler)
	})
	addr := fakeRatd(t, bundle, mux, nil)
	p := startRatTransfers(t, bundle, addr, slog.LevelInfo, local)
	tt := &transferTest{rat: p, session: connectHarness(t, p, ""), local: local, remote: "/remote", addr: addr,
		bundle: bundle}
	args := map[string]any{"remote_path": "/remote/out.pipe", "local_path": "out.tar", "remote_pipe": true}
	tt.expect(t, downloadTool, args, false, "The portal is open: ")
	args["cancel"] = true
	text := tt.expect(t, downloadTool, args, true, "Cut the portal from /remote/out.pipe to out.tar on this side "+
		"after 0 B: out.tar is as it was. ")
	notTold := "Its side of the portal at /remote/out.pipe could not be told to close, and may still be open: call " +
		"download_file again with cancel."
	if !strings.HasSuffix(text, notTold) {
		t.Errorf("expected ratd's side told possibly open, got %q", text)
	}
	before := deletes.Load()
	if text := tt.expect(t, downloadTool, args, true, ""); !strings.HasSuffix(text, notTold) ||
		deletes.Load() == before {
		t.Errorf("expected ratd asked again, without a job, got %q", text)
	}
	expectNoTemporary(t, tt.local)
}
