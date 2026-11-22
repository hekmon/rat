package main

import (
	"bytes"
	"context"
	"crypto/rand"
	"io"
	"io/fs"
	"log/slog"
	"net/http"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
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
// after its first chunk until stall is closed, or failing a download in its trailer.
type fakeTransfers struct {
	// mu guards the fields below
	mu       sync.Mutex
	requests int
	old      bool
	pause    time.Duration
	stall    chan struct{}
	failWith string
}

// take counts a transfer request, and returns how to answer it.
func (f *fakeTransfers) take() (old bool, pause time.Duration, stall chan struct{}, failWith string) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.requests++
	return f.old, f.pause, f.stall, f.failWith
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

func (f *fakeTransfers) upload(w http.ResponseWriter, r *http.Request) {
	old, pause, stall, _ := f.take()
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
	written, _ := os.Stat(path)
	w.Header().Set(connect.HeaderMode, connect.FormatMode(written.Mode()))
	if replaced {
		w.WriteHeader(http.StatusNoContent)
		return
	}
	w.WriteHeader(http.StatusCreated)
}

func (f *fakeTransfers) download(w http.ResponseWriter, r *http.Request) {
	old, pause, stall, failWith := f.take()
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
	bundle := newBundle(t)
	fake := &fakeTransfers{}
	mux := http.NewServeMux()
	mux.Handle("/mcp", newMCPServer().handler)
	mux.HandleFunc("POST "+connect.UploadPath, fake.upload)
	mux.HandleFunc("GET "+connect.DownloadPath, fake.download)
	addr := fakeRatd(t, bundle, mux, nil)
	local, err := filepath.EvalSymlinks(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
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

// TestDownloadFileFailed guards downloads failing: refused by ratd (its sentence relayed), failed
// by its trailer (a file changed while it read it), and from a ratd older than the transfers,
// whoever runs it to be told; no file nor temporary file left.
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
	tt.fake.set(func(f *fakeTransfers) { f.failWith, f.old = "", true })
	tt.expect(t, downloadTool, map[string]any{"remote_path": remote, "local_path": "a.bin"}, true,
		"The ratd at "+tt.addr+" is older than this rat and serves no file transfers: whoever runs it must upgrade it.")
	if _, err := os.Lstat(filepath.Join(tt.local, "a.bin")); err == nil {
		t.Error("expected no file written")
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
	_ = tt.rat.stdin.Close()
	select {
	case err := <-tt.rat.done:
		// for the end of the test to find it too
		tt.rat.done <- err
	case <-time.After(5 * time.Second):
		t.Fatal("rat did not stop")
	}
	expectNoTemporary(t, tt.local)
}
