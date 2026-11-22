package main

import (
	"bytes"
	"context"
	"crypto/rand"
	"io"
	"io/fs"
	"log/slog"
	"math"
	"net/http"
	"os"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"syscall"
	"testing"
	"time"

	"github.com/hekmon/rat/cmd/ratd/tools"
	"github.com/hekmon/rat/connect"
	"github.com/hekmon/rat/internal/flags"
	"github.com/hekmon/rat/mtls"

	"github.com/modelcontextprotocol/go-sdk/mcp"
)

// fakeChunk is the size of the chunks the fake transfers move at a time.
const fakeChunk = 32 << 10

// fakeTransfers serves the file transfers as ratd does, at paths of the machine of the test, with
// what a test needs of a ratd: older than the transfers, slow (a pause between chunks), stalling
// after its first chunk until stall is closed, failing a download in its trailer, cutting a
// transfer after its first chunk, or losing the answer to an upload it received whole
// (dropAnswer).
type fakeTransfers struct {
	// mu guards the fields below
	mu         sync.Mutex
	requests   int
	old        bool
	pause      time.Duration
	stall      chan struct{}
	failWith   string
	cut        bool
	dropAnswer bool
}

// take counts a transfer request, and returns how to answer it.
func (f *fakeTransfers) take() (old bool, pause time.Duration, stall chan struct{}, failWith string, cut bool) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.requests++
	return f.old, f.pause, f.stall, f.failWith, f.cut
}

// count returns the transfer requests received.
func (f *fakeTransfers) count() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.requests
}

// set changes how the next requests are answered.
func (f *fakeTransfers) set(change func(*fakeTransfers)) {
	f.mu.Lock()
	defer f.mu.Unlock()
	change(f)
}

// answerLost tells whether the fake loses its answer to an upload it received whole (see
// dropAnswer), hanging up, as a link going down then.
func (f *fakeTransfers) answerLost(w http.ResponseWriter) bool {
	f.mu.Lock()
	drop := f.dropAnswer
	f.mu.Unlock()
	if drop {
		hangUp(w)
	}
	return drop
}

// hangUp closes the connection of the request w answers, with no answer: the connection lost.
func hangUp(w http.ResponseWriter) {
	if conn, _, err := http.NewResponseController(w).Hijack(); err == nil {
		_ = conn.Close()
	}
}

func (f *fakeTransfers) upload(w http.ResponseWriter, r *http.Request) {
	old, pause, stall, _, cut := f.take()
	if old {
		http.NotFound(w, r)
		return
	}
	w.Header().Set(connect.HeaderVersion, "test")
	path, _ := connect.DecodePath(r.Header.Get(connect.HeaderPath))
	overwrite, _ := connect.ParseOverwrite(r.Header.Get(connect.HeaderOverwrite))
	mode, _ := connect.ParseMode(r.Header.Get(connect.HeaderMode))
	info, statErr := os.Stat(path)
	if statErr == nil && !overwrite {
		http.Error(w, path+" exists: set overwrite to replace it.", http.StatusPreconditionFailed)
		return
	}
	var received bytes.Buffer
	for first := true; ; first = false {
		_, err := io.CopyN(&received, r.Body, fakeChunk)
		if err == io.EOF {
			break
		}
		if err != nil {
			// cut: nothing written
			return
		}
		if first && cut {
			// the connection lost in the middle of the body, as when ratd is killed
			hangUp(w)
			return
		}
		if first && stall != nil {
			<-stall
		}
		time.Sleep(pause)
	}
	replaced := statErr == nil
	if replaced {
		mode = info.Mode().Perm()
	}
	if err := os.WriteFile(path, received.Bytes(), mode); err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	if f.answerLost(w) {
		return
	}
	written, _ := os.Stat(path)
	w.Header().Set(connect.HeaderMode, connect.FormatMode(written.Mode()))
	if replaced {
		w.WriteHeader(http.StatusNoContent)
		return
	}
	w.WriteHeader(http.StatusCreated)
}

func (f *fakeTransfers) download(w http.ResponseWriter, r *http.Request) {
	old, pause, stall, failWith, cut := f.take()
	if old {
		http.NotFound(w, r)
		return
	}
	w.Header().Set(connect.HeaderVersion, "test")
	path, _ := connect.DecodePath(r.Header.Get(connect.HeaderPath))
	data, err := os.ReadFile(path)
	if err != nil {
		http.Error(w, "Reading "+path+" failed: no such file or directory.", http.StatusNotFound)
		return
	}
	info, _ := os.Stat(path)
	w.Header().Set("Content-Type", connect.OctetStream)
	w.Header().Set(connect.HeaderMode, connect.FormatMode(info.Mode()))
	w.Header().Set(connect.HeaderSize, strconv.Itoa(len(data)))
	w.Header().Set("Trailer", connect.HeaderError)
	w.WriteHeader(http.StatusOK)
	for off := 0; off < len(data); off += fakeChunk {
		if _, err := w.Write(data[off:min(off+fakeChunk, len(data))]); err != nil {
			return
		}
		_ = http.NewResponseController(w).Flush()
		if off == 0 && cut {
			// the connection closed without the end of the body, as when ratd is killed
			panic(http.ErrAbortHandler)
		}
		if off == 0 && stall != nil {
			<-stall
		}
		time.Sleep(pause)
	}
	if failWith != "" {
		w.Header().Set(connect.HeaderError, failWith)
	}
}

// transferTest is rat offering the transfers to a fake ratd, and a harness connected to it.
type transferTest struct {
	rat     *ratProcess
	session *mcp.ClientSession
	fake    *fakeTransfers
	// local is the transfers directory of rat, remote a directory of the machine of the fake ratd
	local, remote string
	addr          string
}

// startTransfers starts a fake ratd serving MCP and the transfers, and rat offering them with a
// fresh transfers directory, its links resolved, and a harness connected to it.
func startTransfers(t *testing.T) *transferTest {
	t.Helper()
	local, err := filepath.EvalSymlinks(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	return startTransfersAt(t, local)
}

// startTransfersAt is startTransfers with local as the transfers directory.
func startTransfersAt(t *testing.T, local string) *transferTest {
	t.Helper()
	bundle := newBundle(t)
	fake := &fakeTransfers{}
	mux := http.NewServeMux()
	mux.Handle("/mcp", newMCPServer().handler)
	mux.HandleFunc("POST "+connect.UploadPath, fake.upload)
	mux.HandleFunc("GET "+connect.DownloadPath, fake.download)
	addr := fakeRatd(t, bundle, mux, nil)
	remote, err := filepath.EvalSymlinks(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	p := startRatTransfers(t, bundle, addr, slog.LevelInfo, local)
	return &transferTest{rat: p, session: connectHarness(t, p, ""), fake: fake, local: local, remote: remote, addr: addr}
}

// call calls tool with args, and returns the text of its result and whether it is an error.
func (tt *transferTest) call(t *testing.T, tool string, args map[string]any) (string, bool) {
	t.Helper()
	return tt.callContext(t, context.Background(), tool, args)
}

func (tt *transferTest) callContext(t *testing.T, ctx context.Context, tool string, args map[string]any) (string, bool) {
	t.Helper()
	result, err := tt.session.CallTool(ctx, &mcp.CallToolParams{Name: tool, Arguments: args})
	if err != nil {
		t.Fatalf("%s %v: %v\n%s", tool, args, err, tt.rat.logs)
	}
	var text strings.Builder
	for _, content := range result.Content {
		if c, ok := content.(*mcp.TextContent); ok {
			text.WriteString(c.Text)
		}
	}
	return text.String(), result.IsError
}

// expect calls tool with args, and fails the test unless its result is the one expected, an error
// or not, and starts with prefix.
func (tt *transferTest) expect(t *testing.T, tool string, args map[string]any, isError bool, prefix string) string {
	t.Helper()
	text, gotError := tt.call(t, tool, args)
	if gotError != isError || !strings.HasPrefix(text, prefix) {
		t.Errorf("%s %v: expected IsError %v and %q, got %v: %q", tool, args, isError, prefix, gotError, text)
	}
	return text
}

// stopRat stops rat, as a harness leaving does, and waits for it to exit.
func (tt *transferTest) stopRat(t *testing.T) {
	t.Helper()
	_ = tt.rat.stdin.Close()
	select {
	case err := <-tt.rat.done:
		// for the end of the test to find it too
		tt.rat.done <- err
	case <-time.After(5 * time.Second):
		t.Fatal("rat did not stop")
	}
}

// randomFile writes size random bytes to path, with mode, and returns them.
func randomFile(t *testing.T, path string, size int, mode fs.FileMode) []byte {
	t.Helper()
	content := make([]byte, size)
	if _, err := rand.Read(content); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, content, mode); err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(path, mode); err != nil {
		t.Fatal(err)
	}
	return content
}

// expectContent fails the test unless the file at path holds content, with the permission bits
// mode.
func expectContent(t *testing.T, path string, content []byte, mode fs.FileMode) {
	t.Helper()
	data, err := os.ReadFile(path)
	if err != nil || !bytes.Equal(data, content) {
		t.Errorf("%s: expected %d bytes, got %d, %v", path, len(content), len(data), err)
	}
	if info, err := os.Stat(path); err != nil || info.Mode().Perm() != mode {
		t.Errorf("%s: expected the mode %v, got %v, %v", path, mode, info, err)
	}
}

// expectNoTemporary fails the test if a temporary file of a transfer remains under root.
func expectNoTemporary(t *testing.T, root string) {
	t.Helper()
	_ = filepath.WalkDir(root, func(path string, entry fs.DirEntry, err error) error {
		if err == nil && strings.Contains(entry.Name(), ".rat-") {
			t.Errorf("temporary file left: %s", path)
		}
		return nil
	})
}

// shortWait sets transferWait to wait for the test, until it ends.
func shortWait(t *testing.T, wait time.Duration) {
	t.Helper()
	saved := transferWait
	transferWait = wait
	t.Cleanup(func() { transferWait = saved })
}

// TestTransferTools guards the tools rat offers: none of its own without a transfers directory;
// with one, upload_file and download_file next to the tools of ratd, their descriptions naming the
// directory, their paths required; and a transfers directory that is not one fails the start.
func TestTransferTools(t *testing.T) {
	bundle := newBundle(t)
	addr := fakeRatd(t, bundle, newMCPServer().handler, nil)
	names := func(session *mcp.ClientSession) map[string]*mcp.Tool {
		list, err := session.ListTools(context.Background(), nil)
		if err != nil {
			t.Fatal(err)
		}
		tools := map[string]*mcp.Tool{}
		for _, tool := range list.Tools {
			tools[tool.Name] = tool
		}
		return tools
	}
	relayOnly := names(connectHarness(t, startRat(t, bundle, addr, slog.LevelInfo), ""))
	if _, ok := relayOnly[uploadTool]; ok || len(relayOnly) != 2 {
		t.Errorf("without a transfers directory: expected the tools of ratd alone, got %v", relayOnly)
	}
	dir, err := filepath.EvalSymlinks(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	p := startRatTransfers(t, bundle, addr, slog.LevelInfo, dir)
	tools := names(connectHarness(t, p, ""))
	for _, name := range []string{"echo", "block", uploadTool, downloadTool} {
		if _, ok := tools[name]; !ok {
			t.Errorf("expected %s listed, got %v", name, tools)
		}
	}
	for _, name := range []string{uploadTool, downloadTool} {
		tool := tools[name]
		if tool == nil {
			continue
		}
		if !strings.Contains(tool.Description, "local_path is relative to "+dir+",") {
			t.Errorf("%s: expected the directory named, got %q", name, tool.Description)
		}
		schema, _ := tool.InputSchema.(map[string]any)
		if required, _ := schema["required"].([]any); len(required) != 2 {
			t.Errorf("%s: expected the two paths required, got %v", name, schema)
		}
	}
	if !strings.Contains(p.logs.String(), "transfers="+dir) {
		t.Errorf("expected the transfers directory logged:\n%s", p.logs)
	}
	err = run(context.Background(), slog.New(slog.DiscardHandler), flags.Target{Server: addr,
		Bundle: mtls.ClientDir(bundle, "alice")}, filepath.Join(dir, "missing"), strings.NewReader(""), io.Discard)
	if err == nil || !strings.Contains(err.Error(), "transfers directory") {
		t.Errorf("a missing transfers directory: expected the start to fail, got %v", err)
	}
}

// newHintsServer returns an MCP server as ratd serves it, whose instructions and tools write_file
// and read_file hold the generic copy hints of ratd.
func newHintsServer() http.Handler {
	server := mcp.NewServer(&mcp.Implementation{Name: "fake", Version: "1"},
		&mcp.ServerOptions{Instructions: "Terminals. " + tools.InstructionsCopyHint})
	for name, hint := range map[string]string{tools.WriteFile: tools.WriteFileCopyHint, tools.ReadFile: tools.ReadFileCopyHint} {
		mcp.AddTool(server, &mcp.Tool{Name: name, Description: "A file tool. " + hint},
			func(context.Context, *mcp.CallToolRequest, struct{}) (*mcp.CallToolResult, any, error) {
				return &mcp.CallToolResult{}, nil, nil
			})
	}
	return mcp.NewStreamableHTTPHandler(func(*http.Request) *mcp.Server { return server }, &mcp.StreamableHTTPOptions{
		Stateless: true, JSONResponse: true, DisableLocalhostProtection: true,
	})
}

// TestCopyHintsReplaced guards that rat offering the transfers puts the sentences naming its tools
// in place of the generic copy hints of ratd: in the instructions, from initialize or from
// server/discover (protocol 2026-07-28 on), and in the descriptions of write_file and read_file;
// and that rat without a transfers directory leaves them as they are.
func TestCopyHintsReplaced(t *testing.T) {
	bundle := newBundle(t)
	addr := fakeRatd(t, bundle, newHintsServer(), nil)
	direct := map[string]string{}
	for _, hint := range tools.CopyHints() {
		direct[hint.Generic] = hint.Direct
	}
	for _, tc := range []struct {
		name, version, dir string
	}{
		{"transfers, server/discover", "", t.TempDir()},
		{"transfers, initialize", "2025-11-25", t.TempDir()},
		{"relay only", "", ""},
	} {
		t.Run(tc.name, func(t *testing.T) {
			session := connectHarness(t, startRatTransfers(t, bundle, addr, slog.LevelInfo, tc.dir), tc.version)
			list, err := session.ListTools(context.Background(), nil)
			if err != nil {
				t.Fatal(err)
			}
			texts := map[string]string{"instructions": session.InitializeResult().Instructions}
			for _, tool := range list.Tools {
				texts[tool.Name] = tool.Description
			}
			for name, generic := range map[string]string{tools.WriteFile: tools.WriteFileCopyHint,
				tools.ReadFile: tools.ReadFileCopyHint, "instructions": tools.InstructionsCopyHint} {
				expected, replaced := direct[generic], generic
				if tc.dir == "" {
					expected, replaced = generic, direct[generic]
				}
				if !strings.Contains(texts[name], expected) || strings.Contains(texts[name], replaced) {
					t.Errorf("%s: expected %q, got %q", name, expected, texts[name])
				}
			}
		})
	}
}

// TestUploadFile guards uploads: a file of the transfers directory copied whole, binary included,
// created; refused by ratd without overwrite, its sentence relayed; replaced with it, its mode
// kept by ratd and told when not the one of the file; and logged with its paths and size.
func TestUploadFile(t *testing.T) {
	tt := startTransfers(t)
	content := randomFile(t, filepath.Join(tt.local, "sub", "app.bin"), 300<<10, 0o750)
	remote := filepath.Join(tt.remote, "app.bin")
	args := map[string]any{"local_path": "sub/app.bin", "remote_path": remote}
	tt.expect(t, uploadTool, args, false, "Uploaded sub/app.bin to "+remote+" (300 KiB): created.")
	expectContent(t, remote, content, 0o750&^umask())
	tt.expect(t, uploadTool, args, true, remote+" exists: set overwrite to replace it.")

	if err := os.Chmod(remote, 0o600); err != nil {
		t.Fatal(err)
	}
	args["overwrite"] = true
	tt.expect(t, uploadTool, args, false, "Uploaded sub/app.bin to "+remote+" (300 KiB): replaced. It kept its "+
		"permissions (0600), not those of your file (0750): change them in a terminal (chmod) if it needs yours.")
	expectContent(t, remote, content, 0o600)
	if !strings.Contains(tt.rat.logs.String(), "msg=transfer direction=upload_file local=sub/app.bin remote="+remote+
		" outcome=ok bytes="+strconv.Itoa(len(content))) {
		t.Errorf("expected the upload logged with its paths and size:\n%s", tt.rat.logs)
	}
}

// TestTransferModesNotTold guards a replaced file keeping permissions other than those of its
// source left untold where rat tells no modes (modesTold, false on Windows, whose files show 0666
// or 0444 whatever they are), both ways: otherwise, almost every replace would tell it.
func TestTransferModesNotTold(t *testing.T) {
	saved := modesTold
	modesTold = false
	t.Cleanup(func() { modesTold = saved })
	tt := startTransfers(t)
	// as a file shows on Windows
	randomFile(t, filepath.Join(tt.local, "app.bin"), 1<<10, 0o666)
	remote := filepath.Join(tt.remote, "app.bin")
	randomFile(t, remote, 1<<10, 0o644)
	args := map[string]any{"local_path": "app.bin", "remote_path": remote, "overwrite": true}
	for _, tool := range []string{uploadTool, downloadTool} {
		text, isError := tt.call(t, tool, args)
		if isError || !strings.HasSuffix(text, "(1 KiB): replaced.") {
			t.Errorf("%s: expected the file replaced, its permissions untold, got %q", tool, text)
		}
	}
}

// TestUploadFileRefused guards what rat refuses of an upload, from the local side alone and before
// any request: paths missing, a local path out of the transfers directory, a file missing, a
// directory, and a symbolic link, telling where it leads within the directory, or that it leads
// out of it.
func TestUploadFileRefused(t *testing.T) {
	tt := startTransfers(t)
	randomFile(t, filepath.Join(tt.local, "sub", "app.bin"), 10, 0o644)
	if err := os.Symlink("sub/app.bin", filepath.Join(tt.local, "link.bin")); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink("/etc/hosts", filepath.Join(tt.local, "out.bin")); err != nil {
		t.Fatal(err)
	}
	remote := filepath.Join(tt.remote, "app.bin")
	for _, tc := range []struct {
		local, text string
	}{
		{"", "local_path and remote_path are both required."},
		{"../app.bin", `local_path "../app.bin" is not within ` + tt.local + ": give a path relative to it, without going up (..)."},
		{"/etc/hosts", `local_path "/etc/hosts" is not within ` + tt.local},
		{"nope.bin", "Reading nope.bin failed: no such file or directory."},
		{"sub", "sub is a directory, not a file."},
		{"link.bin", "link.bin is a symbolic link to sub/app.bin, which transfers do not follow: use that path instead."},
		{"out.bin", "out.bin is a symbolic link leading out of " + tt.local + ", which transfers do not follow."},
	} {
		tt.expect(t, uploadTool, map[string]any{"local_path": tc.local, "remote_path": remote}, true, tc.text)
	}
	if requests := tt.fake.count(); requests != 0 {
		t.Errorf("expected no request to ratd, got %d", requests)
	}
}

// TestUploadFileAnswerLost guards an upload whose connection is lost once all of it was sent,
// before ratd answered, as a link going down then: told so, the remote file possibly written,
// rather than ratd unreachable.
func TestUploadFileAnswerLost(t *testing.T) {
	tt := startTransfers(t)
	tt.fake.set(func(f *fakeTransfers) { f.dropAnswer = true })
	randomFile(t, filepath.Join(tt.local, "up.bin"), 1<<10, 0o644)
	remote := filepath.Join(tt.remote, "up.bin")
	text := tt.expect(t, uploadTool, map[string]any{"local_path": "up.bin", "remote_path": remote}, true,
		"The answer of ratd to the copy of up.bin to "+remote+" was lost once all of it was sent (")
	if !strings.HasSuffix(text, "): "+remote+" may be written or not. Check it before using it, or call upload_file "+
		"again with overwrite.") {
		t.Errorf("expected the remote file told possibly written, got %q", text)
	}
}

// TestUploadCancelTooLateAnswerLost guards a cancel coming once all of an upload was sent, too late
// to cut it, ratd then writing the file and its answer lost, as a link going down then: told too
// late, the remote file possibly written, rather than as it was.
func TestUploadCancelTooLateAnswerLost(t *testing.T) {
	shortWait(t, time.Second)
	tt := startTransfers(t)
	stall := make(chan struct{})
	release := sync.OnceFunc(func() { close(stall) })
	t.Cleanup(release)
	// the fake reads a first chunk, then stalls, the rest in the buffers of the connection; once
	// released, it writes the file, then hangs up
	tt.fake.set(func(f *fakeTransfers) { f.stall, f.dropAnswer = stall, true })
	randomFile(t, filepath.Join(tt.local, "up.bin"), 64<<10, 0o644)
	remote := filepath.Join(tt.remote, "up.bin")
	args := map[string]any{"local_path": "up.bin", "remote_path": remote}
	tt.expect(t, uploadTool, args, false, "Still copying up.bin to "+remote+": 64 KiB of 64 KiB")
	// sent whole: the cancel comes too late, ratd answering meanwhile, its answer lost
	go func() {
		time.Sleep(200 * time.Millisecond)
		release()
	}()
	args["cancel"] = true
	text := tt.expect(t, uploadTool, args, true, "Too late to cancel: The answer of ratd to the copy of up.bin to "+
		remote+" was lost once all of it was sent (")
	if !strings.HasSuffix(text, "): "+remote+" may be written or not. Check it before using it, or call upload_file "+
		"again with overwrite.") {
		t.Errorf("expected the remote file told possibly written, got %q", text)
	}
	if _, err := os.Lstat(remote); err != nil {
		t.Errorf("expected the fake to write the file: %v", err)
	}
}

// TestUploadFileReadFailed guards an upload whose local file fails to read (a disk failing, a file
// system gone): told as the system tells it, a local failure, rather than ratd unreachable, ratd
// being fine, the remote file as it was. The file failing is /proc/self/mem, whose reads fail at
// its start (Linux), rat running in the process of the test: the test needs it.
func TestUploadFileReadFailed(t *testing.T) {
	if _, err := os.Stat("/proc/self/mem"); err != nil {
		t.Skip("needs /proc/self/mem, a file whose reads fail (Linux)")
	}
	self, err := filepath.EvalSymlinks("/proc/self")
	if err != nil {
		t.Fatal(err)
	}
	tt := startTransfersAt(t, self)
	remote := filepath.Join(tt.remote, "mem")
	tt.expect(t, uploadTool, map[string]any{"local_path": "mem", "remote_path": remote}, true,
		"Reading mem failed: input/output error. "+remote+" is as it was.")
	if _, err := os.Lstat(remote); err == nil {
		t.Error("expected no remote file written")
	}
}

// TestUploadCancelTooLateRefused guards a cancel coming once all of an upload was sent, too late to
// cut it, ratd then failing the upload (the fake failing to write into a directory that does not
// exist, 500): told too late, with the sentence of ratd, rather than canceled, its reason lost.
func TestUploadCancelTooLateRefused(t *testing.T) {
	shortWait(t, time.Second)
	tt := startTransfers(t)
	stall := make(chan struct{})
	release := sync.OnceFunc(func() { close(stall) })
	t.Cleanup(release)
	// the fake reads a first chunk, then stalls, the rest in the buffers of the connection; once
	// released, it fails to write the file
	tt.fake.set(func(f *fakeTransfers) { f.stall = stall })
	randomFile(t, filepath.Join(tt.local, "up.bin"), 64<<10, 0o644)
	remote := filepath.Join(tt.remote, "missing", "up.bin")
	args := map[string]any{"local_path": "up.bin", "remote_path": remote}
	tt.expect(t, uploadTool, args, false, "Still copying up.bin to "+remote+": 64 KiB of 64 KiB")
	// sent whole: the cancel comes too late, ratd failing the upload meanwhile
	go func() {
		time.Sleep(200 * time.Millisecond)
		release()
	}()
	args["cancel"] = true
	text := tt.expect(t, uploadTool, args, true, "Too late to cancel: ")
	if !strings.HasSuffix(text, "missing/up.bin: no such file or directory") {
		t.Errorf("expected the sentence of ratd, got %q", text)
	}
	if _, err := os.Lstat(remote); err == nil {
		t.Error("expected no remote file written")
	}
}

// TestUploadFileCut guards an upload whose connection is lost midway, once ratd gave its go-ahead:
// told cut, with the reason, rather than ratd unreachable, the remote file as it was.
func TestUploadFileCut(t *testing.T) {
	tt := startTransfers(t)
	tt.fake.set(func(f *fakeTransfers) { f.cut = true })
	// zeros, sparse: far more than the buffers of the connection hold, the upload under way
	local := filepath.Join(tt.local, "up.bin")
	if err := os.WriteFile(local, nil, 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.Truncate(local, 64<<20); err != nil {
		t.Fatal(err)
	}
	remote := filepath.Join(tt.remote, "up.bin")
	text := tt.expect(t, uploadTool, map[string]any{"local_path": "up.bin", "remote_path": remote}, true,
		"The copy of up.bin to "+remote+" was cut: ")
	if !strings.Contains(text, ". "+remote+" is as it was.") {
		t.Errorf("expected the remote file told as it was, got %q", text)
	}
	if _, err := os.Lstat(remote); err == nil {
		t.Error("expected no remote file written")
	}
}

// TestDownloadFile guards downloads: a remote file copied whole into the transfers directory,
// binary included, created with its mode minus the umask, the directories created named; an
// existing file refused without overwrite before any request; replaced with it, keeping its own
// mode, told when not the one of the remote file; and no temporary file left.
func TestDownloadFile(t *testing.T) {
	tt := startTransfers(t)
	remote := filepath.Join(tt.remote, "app.bin")
	content := randomFile(t, remote, 300<<10, 0o750)
	local := filepath.Join(tt.local, "new", "dir", "app.bin")
	args := map[string]any{"remote_path": remote, "local_path": "new/dir/app.bin"}
	tt.expect(t, downloadTool, args, false, "Downloaded "+remote+" to new/dir/app.bin (300 KiB): created. Missing "+
		"directories created in "+tt.local+": new, new/dir. Check the path if you did not mean them.")
	expectContent(t, local, content, 0o750&^umask())

	requests := tt.fake.count()
	tt.expect(t, downloadTool, args, true, "new/dir/app.bin exists: set overwrite to replace it.")
	if tt.fake.count() != requests {
		t.Error("expected the existing file refused before any request")
	}
	if err := os.Chmod(local, 0o600); err != nil {
		t.Fatal(err)
	}
	args["overwrite"] = true
	tt.expect(t, downloadTool, args, false, "Downloaded "+remote+" to new/dir/app.bin (300 KiB): replaced. It kept "+
		"its permissions (0600), not those of the remote file (0750).")
	expectContent(t, local, content, 0o600)
	expectNoTemporary(t, tt.local)
}

// TestDownloadFileLongName guards a download to a local file whose name is longer than its
// temporary name takes, a character across byte 200 (see package tempname): downloaded, no
// temporary file left, rather than refused on macOS, whose APFS takes UTF-8 names only (EILSEQ).
func TestDownloadFileLongName(t *testing.T) {
	tt := startTransfers(t)
	remote := filepath.Join(tt.remote, "data.bin")
	content := randomFile(t, remote, 1<<10, 0o644)
	name := strings.Repeat("a", 199) + "é.bin"
	tt.expect(t, downloadTool, map[string]any{"remote_path": remote, "local_path": name}, false, "Downloaded ")
	expectContent(t, filepath.Join(tt.local, name), content, 0o644&^umask())
	expectNoTemporary(t, tt.local)
}

// TestDownloadFileWithoutHardLinks guards a download to a file system without hard links, whatever
// the error its link gives (FAT, some network ones, FUSE without link as sshfs): the file is
// renamed into place instead. Windows gives other errors (see link_windows.go).
func TestDownloadFileWithoutHardLinks(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("the errors of a link without hard links are those of Unix")
	}
	var linkErr atomic.Value
	link := linkLocal
	linkLocal = func(root *os.Root, oldname, newname string) error {
		return &os.LinkError{Op: "linkat", Old: oldname, New: newname, Err: linkErr.Load().(error)}
	}
	t.Cleanup(func() { linkLocal = link })
	tt := startTransfers(t)
	remote := filepath.Join(tt.remote, "app.bin")
	content := randomFile(t, remote, 1<<10, 0o644)
	// ENOTSUP and EOPNOTSUPP are one on Linux: each file is named after its rank
	for i, errno := range []syscall.Errno{syscall.EPERM, syscall.EMLINK, syscall.ENOTSUP, syscall.EOPNOTSUPP,
		syscall.ENOSYS} {
		linkErr.Store(error(errno))
		local := "app-" + strconv.Itoa(i) + ".bin"
		tt.expect(t, downloadTool, map[string]any{"remote_path": remote, "local_path": local}, false, "Downloaded ")
		expectContent(t, filepath.Join(tt.local, local), content, 0o644&^umask())
	}
	expectNoTemporary(t, tt.local)
}

// TestDownloadFileNameTooLong guards a download to a name longer than any file system takes,
// though its temporary file, named to fit, could take the bytes: refused before any byte is
// written, in the transfers directory before any request, and under a directory to create once
// it is created, a look before the request stopping at it, the directory then named; a directory
// to create too long names those created before it. No temporary file is left.
func TestDownloadFileNameTooLong(t *testing.T) {
	tt := startTransfers(t)
	remote := filepath.Join(tt.remote, "data.bin")
	randomFile(t, remote, 1<<20, 0o644)
	// the fake stalls after its first chunk: a download taking the bytes would wait there, its
	// call telling its progress instead
	stall := make(chan struct{})
	release := sync.OnceFunc(func() { close(stall) })
	defer release()
	tt.fake.set(func(f *fakeTransfers) { f.stall = stall })
	name := strings.Repeat("a", 300)
	tt.expect(t, downloadTool, map[string]any{"remote_path": remote, "local_path": name}, true,
		"Writing "+name+" failed: file name too long.")
	if n := tt.fake.count(); n != 0 {
		t.Errorf("a name too long in the transfers directory: expected no request, %d were made", n)
	}
	under := filepath.Join("new", name)
	tt.expect(t, downloadTool, map[string]any{"remote_path": remote, "local_path": under}, true,
		"Writing "+under+" failed: file name too long. Missing directories created in "+tt.local+": new. Check the "+
			"path if you did not mean them.")
	// a directory to create too long, under one created before it
	dir := filepath.Join("made", name, "f.bin")
	tt.expect(t, downloadTool, map[string]any{"remote_path": remote, "local_path": dir}, true,
		"Writing "+dir+" failed: file name too long. Missing directories created in "+tt.local+": made. Check the "+
			"path if you did not mean them.")
	expectNoTemporary(t, tt.local)
}

// TestDownloadFileFailed guards downloads failing: refused by ratd (its sentence relayed), failed
// by its trailer (a file changed while it read it), cut midway (the reason told, rather than
// "<nil>", which errors.Unwrap gave of the cut connect tells, and the directories it created
// named, left), and from a ratd older than the transfers, whoever runs it to be told; no file nor
// temporary file left.
func TestDownloadFileFailed(t *testing.T) {
	tt := startTransfers(t)
	missing := filepath.Join(tt.remote, "missing")
	tt.expect(t, downloadTool, map[string]any{"remote_path": missing, "local_path": "a.bin"}, true,
		"Reading "+missing+" failed: no such file or directory.")
	remote := filepath.Join(tt.remote, "app.log")
	randomFile(t, remote, 100<<10, 0o644)
	failure := remote + " changed while it was read: it may still be written. Download it again once it is complete."
	tt.fake.set(func(f *fakeTransfers) { f.failWith = failure })
	tt.expect(t, downloadTool, map[string]any{"remote_path": remote, "local_path": "a.bin"}, true, failure)
	tt.fake.set(func(f *fakeTransfers) { f.failWith, f.cut = "", true })
	tt.expect(t, downloadTool, map[string]any{"remote_path": remote, "local_path": "a.bin"}, true,
		"The copy of "+remote+" to a.bin was cut: unexpected EOF. a.bin is as it was.")
	tt.expect(t, downloadTool, map[string]any{"remote_path": remote, "local_path": "new/a.bin"}, true,
		"The copy of "+remote+" to new/a.bin was cut: unexpected EOF. new/a.bin is as it was. Missing directories "+
			"created in "+tt.local+": new. Check the path if you did not mean them.")
	tt.fake.set(func(f *fakeTransfers) { f.cut, f.old = false, true })
	tt.expect(t, downloadTool, map[string]any{"remote_path": remote, "local_path": "a.bin"}, true,
		"The ratd at "+tt.addr+" is older than this rat and serves no file transfers: whoever runs it must upgrade it.")
	for _, name := range []string{"a.bin", "new/a.bin"} {
		if _, err := os.Lstat(filepath.Join(tt.local, name)); err == nil {
			t.Errorf("expected no file written at %s", name)
		}
	}
	expectNoTemporary(t, tt.local)
}

// TestTransferLong guards a transfer longer than a call: the call returns once it waited, telling
// the progress (bytes of the size, rate, time left), the copy going on; the same call again, once
// the copy ended, tells how it ended and since when, once: the next call starts anew.
func TestTransferLong(t *testing.T) {
	shortWait(t, 300*time.Millisecond)
	tt := startTransfers(t)
	remote := filepath.Join(tt.remote, "big.bin")
	content := randomFile(t, remote, 2<<20, 0o644)
	// 64 chunks of 32 KiB: about 1.6 seconds
	tt.fake.set(func(f *fakeTransfers) { f.pause = 25 * time.Millisecond })
	args := map[string]any{"remote_path": remote, "local_path": "big.bin"}
	text := tt.expect(t, downloadTool, args, false, "Still copying "+remote+" to big.bin: ")
	if !strings.Contains(text, " of 2 MiB, ") || !strings.Contains(text, "/s, ") || !strings.Contains(text, " left. "+
		"The copy goes on while you do something else: call download_file again with the same paths for the rest, "+
		"or with cancel to stop it.") {
		t.Errorf("expected the progress, rate and time left, got %q", text)
	}
	time.Sleep(2 * time.Second)
	text = tt.expect(t, downloadTool, args, false, "Downloaded "+remote+" to big.bin (2 MiB): created. It ended ")
	if !strings.HasSuffix(text, " ago.") {
		t.Errorf("expected when the copy ended, got %q", text)
	}
	expectContent(t, filepath.Join(tt.local, "big.bin"), content, 0o644&^umask())
	tt.expect(t, downloadTool, args, true, "big.bin exists: set overwrite to replace it.")
}

// TestTransferRate guards the rate a progress tells: over about rateWindow for a transfer that kept
// moving, since its first bytes for one younger, nothing before any byte moved, and nothing for one
// that moved nothing for longer than rateWindow: neither its average since its start, nor the
// bytes counted after its last sample over the time since, which count leaves up to a second of
// (a transfer moving at the start of a wait, then stalled, was told a rate and a time left).
func TestTransferRate(t *testing.T) {
	now := time.Now()
	ago := func(seconds int) time.Time { return now.Add(-time.Duration(seconds) * time.Second) }
	for _, tc := range []struct {
		what      string
		started   time.Time
		samples   []sample
		lastMoved time.Time
		moved     int64
		rate      float64
	}{
		{"kept moving", ago(120), []sample{{ago(60), 10 << 20}, {ago(40), 20 << 20}, {ago(25), 30 << 20},
			{ago(5), 40 << 20}}, ago(4), 42 << 20, float64(12<<20) / 25},
		{"younger", ago(12), []sample{{ago(10), 1 << 20}, {ago(1), 12 << 20}}, ago(1), 12 << 20, float64(11<<20) / 10},
		{"nothing moved", ago(10), nil, time.Time{}, 0, 0},
	} {
		j := &job{started: tc.started, samples: tc.samples, lastMoved: tc.lastMoved}
		j.moved.Store(tc.moved)
		if got := j.rate(now); math.Abs(got-tc.rate) > 1e-6 {
			t.Errorf("%s: expected %.0f B/s, got %.0f", tc.what, tc.rate, got)
		}
	}
	// stalled, its bytes counted by count: a sample for the first chunk, the next ones after it
	stalled := &job{started: time.Now()}
	for range 100 {
		stalled.count(32 << 10)
	}
	const since = 40 * time.Second
	stalled.started = stalled.started.Add(-since)
	stalled.lastMoved = stalled.lastMoved.Add(-since)
	for i := range stalled.samples {
		stalled.samples[i].at = stalled.samples[i].at.Add(-since)
	}
	if got := stalled.rate(time.Now()); got != 0 {
		t.Errorf("moved nothing for %s: expected 0 B/s, got %.0f", since, got)
	}
}

// TestTransferCanceledCallKeepsJob guards a call that the harness cancels as its job ends, waiting
// for it or canceling it: the call tells nothing, its text dropped, and keeps the job for the next
// call to tell how it ended, rather than forget it untold, select picking at random between the
// end of the job and the cancel of the call.
func TestTransferCanceledCallKeepsJob(t *testing.T) {
	q := transferRequest{tool: uploadTool, local: "up.bin", remote: "/r/up.bin"}
	for _, way := range []string{"waiting", "canceling"} {
		for range 20 {
			j := &job{request: q, cancel: func() {}, done: make(chan struct{}), text: "Uploaded up.bin to /r/up.bin."}
			close(j.done)
			tr := &transfers{jobs: map[string]*job{q.key(): j}}
			ctx, cancel := context.WithCancel(context.Background())
			cancel()
			var text string
			if way == "waiting" {
				text, _ = tr.await(ctx, j, time.Now())
			} else {
				text, _ = tr.cancel(ctx, q)
			}
			if text != "" || tr.jobs[q.key()] != j {
				t.Fatalf("%s, the call canceled as its job ended: expected nothing told and the job kept, got %q, "+
					"kept %v", way, text, tr.jobs[q.key()] == j)
			}
		}
	}
}

// TestTransferCancel guards a transfer stalling, then canceled: the call tells nothing moved; a
// cancel stops it, leaving the destination as it was, with no temporary file; a transfer to the
// same destination from another source is refused while it runs, and a cancel naming another
// source, or no transfer under way, cancels nothing.
func TestTransferCancel(t *testing.T) {
	shortWait(t, 300*time.Millisecond)
	tt := startTransfers(t)
	stall := make(chan struct{})
	t.Cleanup(func() { close(stall) })
	tt.fake.set(func(f *fakeTransfers) { f.stall = stall })
	remote := filepath.Join(tt.remote, "big.bin")
	randomFile(t, remote, 1<<20, 0o644)
	args := map[string]any{"remote_path": remote, "local_path": "big.bin"}
	tt.expect(t, downloadTool, args, false, "Still copying "+remote+" to big.bin: 32 KiB of 1 MiB")
	tt.expect(t, downloadTool, args, false, "Still copying "+remote+" to big.bin: 32 KiB of 1 MiB, and nothing moved")
	tt.expect(t, downloadTool, map[string]any{"remote_path": filepath.Join(tt.remote, "other.bin"), "local_path": "big.bin"},
		true, "A transfer to big.bin from "+remote+" is under way: wait for it with its paths, or stop it with cancel.")
	other := filepath.Join(tt.remote, "other.bin")
	tt.expect(t, downloadTool, map[string]any{"remote_path": other, "local_path": "big.bin", "cancel": true}, true,
		"The transfer to big.bin under way is from "+remote+", not "+other+": nothing canceled.")
	tt.expect(t, downloadTool, map[string]any{"remote_path": remote, "local_path": "other.bin", "cancel": true}, false,
		"No transfer to other.bin is under way: nothing to cancel.")
	args["cancel"] = true
	tt.expect(t, downloadTool, args, false, "Canceled the copy of "+remote+" to big.bin after 32 KiB of 1 MiB: big.bin "+
		"is as it was.")
	if _, err := os.Lstat(filepath.Join(tt.local, "big.bin")); err == nil {
		t.Error("expected no file written")
	}
	expectNoTemporary(t, tt.local)

	// zeros, sparse: far more than the buffers of the connection hold, the upload staying under way
	if err := os.WriteFile(filepath.Join(tt.local, "up.bin"), nil, 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.Truncate(filepath.Join(tt.local, "up.bin"), 64<<20); err != nil {
		t.Fatal(err)
	}
	upload := map[string]any{"local_path": "up.bin", "remote_path": filepath.Join(tt.remote, "up.bin")}
	tt.expect(t, uploadTool, upload, false, "Still copying up.bin to "+filepath.Join(tt.remote, "up.bin")+": ")
	upload["cancel"] = true
	tt.expect(t, uploadTool, upload, false, "Canceled the copy of up.bin to "+filepath.Join(tt.remote, "up.bin")+" after ")
	if _, err := os.Lstat(filepath.Join(tt.remote, "up.bin")); err == nil {
		t.Error("expected no remote file written")
	}
	if !strings.Contains(tt.rat.logs.String(), "outcome=canceled") {
		t.Errorf("expected the cancels logged:\n%s", tt.rat.logs)
	}
}

// TestTransferCallCanceled guards a call canceled by the harness: only its wait ends, the transfer
// going on, which the same call tells once ended.
func TestTransferCallCanceled(t *testing.T) {
	tt := startTransfers(t)
	remote := filepath.Join(tt.remote, "big.bin")
	randomFile(t, remote, 1<<20, 0o644)
	// 32 chunks of 32 KiB: about 0.6 seconds
	tt.fake.set(func(f *fakeTransfers) { f.pause = 20 * time.Millisecond })
	args := map[string]any{"remote_path": remote, "local_path": "big.bin"}
	ctx, cancel := context.WithTimeout(context.Background(), 100*time.Millisecond)
	defer cancel()
	if _, err := tt.session.CallTool(ctx, &mcp.CallToolParams{Name: downloadTool, Arguments: args}); err == nil {
		t.Fatal("expected the call canceled")
	}
	time.Sleep(time.Second)
	tt.expect(t, downloadTool, args, false, "Downloaded "+remote+" to big.bin (1 MiB): created. It ended ")
}

// TestTransferRatStopsUploadSent guards rat stopping while an upload whose file was sent whole
// waits for the answer of ratd, slow to give it, the agent having canceled it meanwhile or not (a
// cancel waits for that answer, telling the transfer still ending): rat cuts it and exits, as for
// any transfer under way, rather than wait for an answer nobody reads any more.
func TestTransferRatStopsUploadSent(t *testing.T) {
	for _, canceled := range []bool{false, true} {
		name := "untouched"
		if canceled {
			name = "canceled"
		}
		t.Run(name, func(t *testing.T) {
			shortWait(t, 300*time.Millisecond)
			tt := startTransfers(t)
			stall := make(chan struct{})
			t.Cleanup(func() { close(stall) })
			// the fake reads a first chunk, then stalls: the rest of the file in the buffers of the
			// connection, the file sent whole
			tt.fake.set(func(f *fakeTransfers) { f.stall = stall })
			randomFile(t, filepath.Join(tt.local, "up.bin"), 64<<10, 0o644)
			remote := filepath.Join(tt.remote, "up.bin")
			args := map[string]any{"local_path": "up.bin", "remote_path": remote}
			tt.expect(t, uploadTool, args, false, "Still copying up.bin to ")
			if canceled {
				args["cancel"] = true
				tt.expect(t, uploadTool, args, true, "The transfer to "+remote+" is still ending: ")
			}
			tt.stopRat(t)
		})
	}
}

// TestTransferRatStops guards rat stopping with a transfer under way: the transfer is cut, and its
// temporary file removed before rat exits.
func TestTransferRatStops(t *testing.T) {
	tt := startTransfers(t)
	stall := make(chan struct{})
	t.Cleanup(func() { close(stall) })
	tt.fake.set(func(f *fakeTransfers) { f.stall = stall })
	remote := filepath.Join(tt.remote, "big.bin")
	randomFile(t, remote, 1<<20, 0o644)
	ctx, cancel := context.WithTimeout(context.Background(), 300*time.Millisecond)
	defer cancel()
	_, _ = tt.session.CallTool(ctx, &mcp.CallToolParams{Name: downloadTool,
		Arguments: map[string]any{"remote_path": remote, "local_path": "big.bin"}})
	matches, _ := filepath.Glob(filepath.Join(tt.local, ".big.bin.rat-*"))
	if len(matches) != 1 {
		t.Fatalf("expected the temporary file of the download, got %v", matches)
	}
	tt.stopRat(t)
	expectNoTemporary(t, tt.local)
}
