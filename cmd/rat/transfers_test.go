package main

import (
	"bytes"
	"context"
	"crypto/rand"
	"errors"
	"io"
	"io/fs"
	"log/slog"
	"math"
	"net/http"
	"os"
	"path/filepath"
	"runtime"
	"slices"
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
// after its first chunk until stall is closed, failing with the sentence failWith (a download in
// its trailer, an upload once received whole, writing nothing but what it wrote in place), cutting
// a transfer after its first chunk, losing the answer to an upload it received whole (dropAnswer),
// answering an upload written without the mode of the file (noMode), putting a new file in place
// of the one an upload in place replaces, as ratd does of a file appearing at a path it found empty
// (newFile), or calling meanwhile as a
// download comes, before answering it, as another transfer acting in between. It is for what a test must provoke: what ratd does by itself, successes and refusals,
// is tested against ratd (see ratd_unix_test.go), and the answers the tests of the fake go through
// are those of ratd, which TestRealRatdAnswersAsFake checks, TestRealRatdEmptiesInPlaceAsFake
// when a file written in place is emptied.
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
	noMode     bool
	newFile    bool
	meanwhile  func()
	// inPlace is the in place of each upload received
	inPlace []bool
}

// take counts a transfer request, and returns how to answer it.
func (f *fakeTransfers) take() (old bool, pause time.Duration, stall chan struct{}, failWith string, cut bool) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.requests++
	return f.old, f.pause, f.stall, f.failWith, f.cut
}

// inPlaces returns the in place of each upload received.
func (f *fakeTransfers) inPlaces() []bool {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]bool(nil), f.inPlace...)
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
	old, pause, stall, failWith, cut := f.take()
	if old {
		http.NotFound(w, r)
		return
	}
	w.Header().Set(connect.HeaderVersion, "test")
	path, _ := connect.DecodePath(r.Header.Get(connect.HeaderPath))
	overwrite, _ := connect.ParseOverwrite(r.Header.Get(connect.HeaderOverwrite))
	inPlace, _ := connect.ParseInPlace(r.Header.Get(connect.HeaderInPlace))
	f.mu.Lock()
	f.inPlace = append(f.inPlace, inPlace)
	f.mu.Unlock()
	mode, _ := connect.ParseMode(r.Header.Get(connect.HeaderMode))
	info, statErr := os.Stat(path)
	if statErr == nil && !overwrite {
		http.Error(w, path+" exists: set overwrite to replace it.", http.StatusPreconditionFailed)
		return
	}
	var received bytes.Buffer
	var into io.Writer = &received
	// in place, as ratd writes a file replaced: emptied once the body comes, then written into, a
	// cut leaving it partly written
	f.mu.Lock()
	newFile := f.newFile
	f.mu.Unlock()
	intoFile := inPlace && statErr == nil && !newFile
	var inPlaceFile *emptiedOnWrite
	if intoFile {
		file, err := os.OpenFile(path, os.O_WRONLY, 0)
		if err != nil {
			http.Error(w, err.Error(), http.StatusInternalServerError)
			return
		}
		defer file.Close()
		inPlaceFile = &emptiedOnWrite{file: file}
		into = inPlaceFile
	}
	for first := true; ; first = false {
		_, err := io.CopyN(into, r.Body, fakeChunk)
		if err == io.EOF {
			break
		}
		if err != nil {
			// cut: nothing written, but what was written in place
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
	if intoFile {
		// an empty body, which writes nothing: the file emptied all the same
		if err := inPlaceFile.empty(); err != nil {
			http.Error(w, err.Error(), http.StatusInternalServerError)
			return
		}
	}
	if failWith != "" {
		// as ratd failing to write a file received whole (a disk full)
		http.Error(w, failWith, http.StatusInsufficientStorage)
		return
	}
	replaced := statErr == nil
	if replaced {
		mode = info.Mode().Perm()
	}
	if !intoFile {
		// as ratd writes a file not in place: a new file put in place of the one replaced, whose
		// hard links keep their content, rather than written into
		tmp := filepath.Join(filepath.Dir(path), ".fake-"+filepath.Base(path))
		err := os.WriteFile(tmp, received.Bytes(), mode)
		if err == nil && replaced {
			// the mode of the file replaced, which the umask takes nothing from
			err = os.Chmod(tmp, mode)
		}
		if err == nil {
			err = os.Rename(tmp, path)
		}
		if err != nil {
			http.Error(w, err.Error(), http.StatusInternalServerError)
			return
		}
	}
	if f.answerLost(w) {
		return
	}
	f.mu.Lock()
	noMode := f.noMode
	f.mu.Unlock()
	if !noMode {
		written, _ := os.Stat(path)
		w.Header().Set(connect.HeaderMode, connect.FormatMode(written.Mode()))
	}
	if replaced {
		// as ratd tells the setuid or setgid bit of the file replaced, from the look before the bytes
		if note := connect.SpecialBitsNote(info.Mode()); note != "" {
			w.Header().Set(connect.HeaderNote, connect.EncodeError(note))
		}
		if intoFile {
			// as ratd tells a file written into
			w.Header().Set(connect.HeaderInPlace, "true")
		}
		w.WriteHeader(http.StatusNoContent)
		return
	}
	w.WriteHeader(http.StatusCreated)
}

// emptiedOnWrite writes into file, emptying it before its first write, as ratd writes a file in
// place: until the body comes, the file is left as it was.
type emptiedOnWrite struct {
	file    *os.File
	emptied bool
}

func (w *emptiedOnWrite) Write(p []byte) (int, error) {
	if err := w.empty(); err != nil {
		return 0, err
	}
	return w.file.Write(p)
}

// empty empties the file, once.
func (w *emptiedOnWrite) empty() error {
	if w.emptied {
		return nil
	}
	if err := w.file.Truncate(0); err != nil {
		return err
	}
	w.emptied = true
	return nil
}

func (f *fakeTransfers) download(w http.ResponseWriter, r *http.Request) {
	old, pause, stall, failWith, cut := f.take()
	if old {
		http.NotFound(w, r)
		return
	}
	f.mu.Lock()
	meanwhile := f.meanwhile
	f.mu.Unlock()
	if meanwhile != nil {
		meanwhile()
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
		w.Header().Set(connect.HeaderError, connect.EncodeError(failWith))
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
	// bundle is the bundle of the ratd, for a test to be another client of it
	bundle string
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
	return &transferTest{rat: p, session: connectHarness(t, p, ""), fake: fake, local: local, remote: remote, addr: addr,
		bundle: bundle}
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
// directory, that a copy under way is joined as it was asked, canceled to change it, and what a
// cancel leaves (a file written in place partly written, upload_file alone
// writing in place), in_place telling what each choice costs (room for a second copy, or a file
// partly written) and warning against writing into the file uploaded itself, their paths
// required; both telling their portals (remote_pipe), upload_file how to stop one midway (the
// command reading first, then cancel); and a transfers directory that is not one fails the start.
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
		if !strings.Contains(tool.Description, "local_path is a path under "+dir+", relative to it or absolute:") {
			t.Errorf("%s: expected the directory named, got %q", name, tool.Description)
		}
		if !strings.Contains(tool.Description, "the copy keeping the parameters it started with: to change them, "+
			"cancel it, then call again.") {
			t.Errorf("%s: expected a copy under way told joined as asked, got %q", name, tool.Description)
		}
		schema, _ := tool.InputSchema.(map[string]any)
		if required, _ := schema["required"].([]any); len(required) != 2 {
			t.Errorf("%s: expected the two paths required, got %v", name, schema)
		}
		left := "cancel stops it, leaving the destination as it was, unless"
		if name == uploadTool {
			left = "cancel stops it, leaving the destination as it was (written in place, partly written), unless"
			properties, _ := schema["properties"].(map[string]any)
			inPlace, _ := properties["in_place"].(map[string]any)
			description, _ := inPlace["description"].(string)
			for _, told := range []string{"the disk must hold both meanwhile", "no room needed for a second copy",
				"Never into the very file you upload, when the terminals run on your machine"} {
				if !strings.Contains(description, told) {
					t.Errorf("expected in_place telling %q, got %q", told, description)
				}
			}
		}
		if !strings.Contains(tool.Description, left) {
			t.Errorf("%s: expected what a cancel leaves told, got %q", name, tool.Description)
		}
	}
	if tool := tools[downloadTool]; tool != nil && !strings.Contains(tool.Description, "With remote_pipe, the source is "+
		"a portal rather than a file") {
		t.Errorf("expected download_file telling its portals, got %q", tool.Description)
	}
	if tool := tools[uploadTool]; tool != nil && (!strings.Contains(tool.Description, "With remote_pipe, the "+
		"destination is a portal rather than a file") || !strings.Contains(tool.Description, "stop the command reading "+
		"first, then cancel")) {
		t.Errorf("expected upload_file telling its portals, and how to stop one midway, got %q", tool.Description)
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
	tt.expect(t, uploadTool, args, false, "Uploaded sub/app.bin to "+remote+" (300 KiB): replaced by a new file. It kept its "+
		"permissions (0600), not those of your file (0750): change them in a terminal (chmod) if it needs yours.")
	expectContent(t, remote, content, 0o600)
	if !strings.Contains(tt.rat.logs.String(), "msg=transfer direction=upload_file local=sub/app.bin remote="+remote+
		" outcome=ok bytes="+strconv.Itoa(len(content))) {
		t.Errorf("expected the upload logged with its paths and size:\n%s", tt.rat.logs)
	}
}

// modeOf returns the mode a look found, or why it failed, for a test to tell what it got.
func modeOf(info fs.FileInfo, err error) any {
	if err != nil {
		return err
	}
	return info.Mode()
}

// TestTransferSpecialBits guards a transfer replacing a file of a setuid bit (G6, L2): a download
// puts a file of the nine permission bits of the file replaced only in its place, its setuid bit
// never put back, which would then be for the user of rat, and tells it, for the agent to set it
// again; an upload relays what ratd tells of the file it replaced (Rat-Note), put in place or
// written in place. setuid rather than setgid: a chmod may drop setgid silently, for a group the
// user is not in.
func TestTransferSpecialBits(t *testing.T) {
	tt := startTransfers(t)
	note := "The file replaced had the setuid bit, which a file written loses: set it again if it needs it " +
		"(chmod u+s)."
	setuid := func(path string) {
		t.Helper()
		if err := os.Chmod(path, fs.ModeSetuid|0o755); err != nil {
			t.Fatal(err)
		}
		if info, err := os.Stat(path); err != nil || info.Mode()&fs.ModeSetuid == 0 {
			t.Fatalf("the setuid bit was not set: %v", modeOf(info, err))
		}
	}
	remote := filepath.Join(tt.remote, "tool")
	content := randomFile(t, remote, 1<<10, 0o750)
	local := filepath.Join(tt.local, "tool")
	randomFile(t, local, 1<<10, 0o755)
	setuid(local)
	size := sizeText(1 << 10)
	tt.expect(t, downloadTool, map[string]any{"remote_path": remote, "local_path": "tool", "overwrite": true}, false,
		"Downloaded "+remote+" to tool ("+size+"): replaced. It kept its permissions (0755), not those of the remote "+
			"file (0750). "+note)
	expectContent(t, local, content, 0o755)
	if info, err := os.Lstat(local); err != nil || info.Mode() != 0o755 {
		t.Errorf("expected exactly 0755, no setuid bit, got %v", modeOf(info, err))
	}

	randomFile(t, filepath.Join(tt.local, "new"), 1<<10, 0o700)
	for inPlace, how := range map[bool]string{false: "replaced by a new file", true: "written in place"} {
		setuid(remote)
		tt.expect(t, uploadTool, map[string]any{"local_path": "new", "remote_path": remote, "overwrite": true,
			"in_place": inPlace}, false, "Uploaded new to "+remote+" ("+size+"): "+how+". It kept its permissions "+
			"(0755), not those of your file (0700): change them in a terminal (chmod) if it needs yours. "+note)
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
	for tool, replaced := range map[string]string{uploadTool: "(1 KiB): replaced by a new file.",
		downloadTool: "(1 KiB): replaced."} {
		text, isError := tt.call(t, tool, args)
		if isError || !strings.HasSuffix(text, replaced) {
			t.Errorf("%s: expected the file replaced, its permissions untold, got %q", tool, text)
		}
	}
}

// TestUploadFileRefused guards what rat refuses of an upload, from the local side alone and before
// any request: paths missing, a local path out of the transfers directory, a file missing, a
// directory, a symbolic link, telling the path to use when it leads to a file under the directory,
// a link alone otherwise, a NUL byte in either path, and a remote path naming a directory (a final
// slash).
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
		{"../app.bin", `local_path "../app.bin" is not within ` + tt.local + ": give a path within it, relative to it or absolute."},
		{"/etc/hosts", `local_path "/etc/hosts" is not within ` + tt.local},
		// absolute: under the directory by its whole names once cleaned, or refused
		{tt.local + "x/app.bin", "local_path " + strconv.Quote(tt.local+"x/app.bin") + " is not within " + tt.local},
		{tt.local + "/sub/../../app.bin", "local_path " + strconv.Quote(tt.local+"/sub/../../app.bin") + " is not within " + tt.local},
		{tt.local + "/nope.bin", "Reading nope.bin failed: no such file or directory."},
		{tt.local + "/sub", "sub is a directory, not a file."},
		{"nope.bin", "Reading nope.bin failed: no such file or directory."},
		{"sub", "sub is a directory, not a file."},
		{"link.bin", "link.bin is a symbolic link to sub/app.bin, which transfers do not follow: use that path instead."},
		{"out.bin", "out.bin is a symbolic link, which transfers do not follow."},
		{"a\x00b.bin", `local_path "a\x00b.bin" holds a NUL byte, which no file name can hold.`},
	} {
		tt.expect(t, uploadTool, map[string]any{"local_path": tc.local, "remote_path": remote}, true, tc.text)
	}
	tt.expect(t, uploadTool, map[string]any{"local_path": "sub/app.bin", "remote_path": remote + "\x00b"}, true,
		"remote_path "+strconv.Quote(remote+"\x00b")+" holds a NUL byte, which no file name can hold.")
	tt.expect(t, uploadTool, map[string]any{"local_path": "sub/app.bin", "remote_path": tt.remote + "/backups/"}, true,
		"remote_path "+strconv.Quote(tt.remote+"/backups/")+" names a directory: give the name of the file to write in it.")
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

// TestUploadCancelTooLateWritten guards a cancel coming once all of an upload was sent, too late to
// cut it, ratd then writing the file and answering: the copy told done behind "Too late to
// cancel", where an upload failing tells its failure first (TestUploadCancelTooLateRefused). The
// progress of the upload read whole still offers the cancel: the bytes read do not tell the end of
// the body, which connect decides (a link stalled on the last chunk leaves it untold).
func TestUploadCancelTooLateWritten(t *testing.T) {
	shortWait(t, time.Second)
	tt := startTransfers(t)
	stall := make(chan struct{})
	release := sync.OnceFunc(func() { close(stall) })
	t.Cleanup(release)
	// the fake reads a first chunk, then stalls, the rest in the buffers of the connection
	tt.fake.set(func(f *fakeTransfers) { f.stall = stall })
	randomFile(t, filepath.Join(tt.local, "up.bin"), 64<<10, 0o644)
	remote := filepath.Join(tt.remote, "up.bin")
	args := map[string]any{"local_path": "up.bin", "remote_path": remote}
	text := tt.expect(t, uploadTool, args, false, "Still copying up.bin to "+remote+": 64 KiB of 64 KiB")
	if !strings.HasSuffix(text, "or with cancel to stop it.") {
		t.Errorf("expected the progress of an upload read whole to offer the cancel, got %q", text)
	}
	go func() {
		time.Sleep(200 * time.Millisecond)
		release()
	}()
	args["cancel"] = true
	tt.expect(t, uploadTool, args, false, "Too late to cancel: Uploaded up.bin to "+remote+" (64 KiB): created.")
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
	text := tt.expect(t, uploadTool, args, true, "The answer of ratd to the copy of up.bin to "+remote+" was lost "+
		"once all of it was sent (")
	if !strings.HasSuffix(text, "): "+remote+" may be written or not. Check it before using it, or call upload_file "+
		"again with overwrite. The cancel came too late to stop it.") {
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

// TestUploadStopAfterCancelTooLate guards an upload sent whole, canceled too late (its cancel waiting
// for the answer of ratd), then cut by rat stopping, which cuts it whatever was sent: logged as
// failing, ratd possibly writing it, rather than as canceled, its destination as it was.
func TestUploadStopAfterCancelTooLate(t *testing.T) {
	shortWait(t, 300*time.Millisecond)
	tt := startTransfers(t)
	stall := make(chan struct{})
	t.Cleanup(func() { close(stall) })
	// the fake reads a first chunk, then stalls, the rest in the buffers of the connection
	tt.fake.set(func(f *fakeTransfers) { f.stall = stall })
	randomFile(t, filepath.Join(tt.local, "up.bin"), 64<<10, 0o644)
	remote := filepath.Join(tt.remote, "up.bin")
	args := map[string]any{"local_path": "up.bin", "remote_path": remote}
	tt.expect(t, uploadTool, args, false, "Still copying up.bin to ")
	args["cancel"] = true
	tt.expect(t, uploadTool, args, true, "The transfer to "+remote+" is still ending: ")
	tt.stopRat(t)
	if logs := tt.rat.logs.String(); strings.Contains(logs, "outcome=canceled") ||
		!strings.Contains(logs, "outcome=error") {
		t.Errorf("expected the upload logged failing, not canceled:\n%s", logs)
	}
}

// TestUploadModeNotTold guards an upload replacing a file, ratd answering it written without the
// mode of the file (noMode): told replaced, nothing said of permissions rat does not know, rather
// than permissions 0000 kept, or the upload failed.
func TestUploadModeNotTold(t *testing.T) {
	tt := startTransfers(t)
	tt.fake.set(func(f *fakeTransfers) { f.noMode = true })
	randomFile(t, filepath.Join(tt.local, "up.bin"), 1<<10, 0o644)
	remote := filepath.Join(tt.remote, "app.bin")
	randomFile(t, remote, 10, 0o640)
	told := "Uploaded up.bin to " + remote + " (1 KiB): replaced by a new file."
	if text := tt.expect(t, uploadTool, map[string]any{"local_path": "up.bin", "remote_path": remote,
		"overwrite": true}, false, told); text != told {
		t.Errorf("expected %q alone, got %q", told, text)
	}
}

// TestUploadInPlaceNewFile guards an upload with in_place and overwrite whose file ratd replaced by
// a new one (newFile), as it does of a file appearing at a path it found empty: told replaced by a
// new file, from the answer of ratd, rather than written in place, from what was asked, the agent
// then believing the hard links of the file to follow.
func TestUploadInPlaceNewFile(t *testing.T) {
	tt := startTransfers(t)
	tt.fake.set(func(f *fakeTransfers) { f.newFile = true })
	randomFile(t, filepath.Join(tt.local, "up.bin"), 1<<10, 0o644)
	remote := filepath.Join(tt.remote, "app.bin")
	randomFile(t, remote, 10, 0o640)
	tt.expect(t, uploadTool, map[string]any{"local_path": "up.bin", "remote_path": remote, "overwrite": true,
		"in_place": true}, false, "Uploaded up.bin to "+remote+" (1 KiB): replaced by a new file.")
}

// TestTransferAbsoluteLocalPath guards a local path given absolute, under the transfers directory,
// as an agent gives back a full path rat told it: the same file as its relative path, uploaded and
// downloaded as it, told by its relative name, one job with it (a download to it then finds the
// file the relative path wrote), and one naming a directory refused as its relative form is. The
// contract of a local path is its prefix.
func TestTransferAbsoluteLocalPath(t *testing.T) {
	tt := startTransfers(t)
	content := randomFile(t, filepath.Join(tt.local, "sub", "up.bin"), 1<<10, 0o644)
	remote := filepath.Join(tt.remote, "up.bin")
	tt.expect(t, uploadTool, map[string]any{"local_path": filepath.Join(tt.local, "sub", "up.bin"), "remote_path": remote},
		false, "Uploaded "+filepath.Join("sub", "up.bin")+" to "+remote+" (1 KiB): created.")
	expectContent(t, remote, content, 0o644)
	down := map[string]any{"remote_path": remote, "local_path": "down.bin"}
	tt.expect(t, downloadTool, down, false, "Downloaded "+remote+" to down.bin (1 KiB): created.")
	down["local_path"] = filepath.Join(tt.local, "down.bin")
	tt.expect(t, downloadTool, down, true, "down.bin exists: set overwrite to replace it.")
	// one rule for both: joined when relative, cleaned, then under the directory or not
	down["local_path"] = filepath.Join("..", filepath.Base(tt.local), "down.bin")
	tt.expect(t, downloadTool, down, true, "down.bin exists: set overwrite to replace it.")
	down["local_path"] = tt.local + "/logs/"
	tt.expect(t, downloadTool, down, true, "local_path "+strconv.Quote(tt.local+"/logs/")+" names a directory: give the "+
		"name of the file to write in it.")
}

// TestTransferThroughLinkedDirectory guards that the contract of a local path is its prefix, not a
// confinement: a directory of the transfers directory that is a symbolic link to a directory
// outside is followed, as the system follows it, for an upload from it and a download into it, the
// file landing where the link leads, the missing directories under it created there; a symbolic
// link at the end of a path is still refused, leading under the directory (the path to use told),
// outside, nowhere, or reached through the linked directory; a path going up, or absolute
// elsewhere, the very file the link reaches included, is still refused before any request.
func TestTransferThroughLinkedDirectory(t *testing.T) {
	tt := startTransfers(t)
	outside, err := filepath.EvalSymlinks(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(outside, filepath.Join(tt.local, "ext")); err != nil {
		t.Fatal(err)
	}
	content := randomFile(t, filepath.Join(outside, "up.bin"), 1<<10, 0o644)
	remote := filepath.Join(tt.remote, "up.bin")
	tt.expect(t, uploadTool, map[string]any{"local_path": "ext/up.bin", "remote_path": remote}, false,
		"Uploaded ext/up.bin to "+remote+" (1 KiB): created.")
	expectContent(t, remote, content, 0o644)
	tt.expect(t, downloadTool, map[string]any{"remote_path": remote, "local_path": "ext/down.bin"}, false,
		"Downloaded "+remote+" to ext/down.bin (1 KiB): created.")
	expectContent(t, filepath.Join(outside, "down.bin"), content, 0o644&^umask())
	tt.expect(t, downloadTool, map[string]any{"remote_path": remote, "local_path": "ext/new/down.bin"}, false,
		"Downloaded "+remote+" to ext/new/down.bin (1 KiB): created. Missing directories created in "+tt.local+
			": ext/new. Check the path if you did not mean them.")
	expectContent(t, filepath.Join(outside, "new", "down.bin"), content, 0o644&^umask())
	expectNoTemporary(t, outside)

	randomFile(t, filepath.Join(tt.local, "sub", "app.bin"), 10, 0o644)
	for name, target := range map[string]string{"in.bin": "sub/app.bin", "out.bin": filepath.Join(outside, "up.bin"),
		"gone.bin": "missing.bin", filepath.Join("ext", "link.bin"): "up.bin"} {
		if err := os.Symlink(target, filepath.Join(tt.local, name)); err != nil {
			t.Fatal(err)
		}
	}
	requests := tt.fake.count()
	for _, tc := range []struct {
		local, text string
	}{
		{"in.bin", "in.bin is a symbolic link to sub/app.bin, which transfers do not follow: use that path instead."},
		{"out.bin", "out.bin is a symbolic link, which transfers do not follow."},
		{"gone.bin", "gone.bin is a symbolic link, which transfers do not follow."},
		{"ext/link.bin", "ext/link.bin is a symbolic link, which transfers do not follow."},
		{"../up.bin", `local_path "../up.bin" is not within ` + tt.local},
		{"ext/../../up.bin", `local_path "ext/../../up.bin" is not within ` + tt.local},
		{filepath.Join(outside, "up.bin"), "local_path " + strconv.Quote(filepath.Join(outside, "up.bin")) +
			" is not within " + tt.local},
	} {
		tt.expect(t, uploadTool, map[string]any{"local_path": tc.local, "remote_path": remote, "overwrite": true}, true,
			tc.text)
		tt.expect(t, downloadTool, map[string]any{"remote_path": remote, "local_path": tc.local, "overwrite": true}, true,
			tc.text)
	}
	if n := tt.fake.count(); n != requests {
		t.Errorf("expected the refusals made before any request, %d made", n-requests)
	}
	expectContent(t, filepath.Join(outside, "up.bin"), content, 0o644)
}

// TestUploadFileInPlace guards an upload with in_place and overwrite: sent with Rat-In-Place, ratd
// (the fake, as ratd does) writing into the file replaced, its hard link following, told written in
// place; a call with the same paths, in_place left out, joining the job of its paths, as any call
// with them joins it; a cancel telling the file, written in
// place, may be partly written, rather than as it was. Without overwrite, in_place asks nothing.
func TestUploadFileInPlace(t *testing.T) {
	shortWait(t, 300*time.Millisecond)
	tt := startTransfers(t)
	remote := filepath.Join(tt.remote, "app.bin")
	randomFile(t, remote, 10, 0o640)
	link := filepath.Join(tt.remote, "app.link")
	if err := os.Link(remote, link); err != nil {
		t.Fatal(err)
	}
	content := randomFile(t, filepath.Join(tt.local, "up.bin"), 1<<10, 0o644)
	tt.expect(t, uploadTool, map[string]any{"local_path": "up.bin", "remote_path": remote, "overwrite": true,
		"in_place": true}, false, "Uploaded up.bin to "+remote+" (1 KiB): written in place.")
	expectContent(t, link, content, 0o640)
	tt.expect(t, uploadTool, map[string]any{"local_path": "up.bin", "remote_path": remote, "in_place": true}, true,
		remote+" exists: set overwrite to replace it.")
	// stalled after its first chunk, the rest of the file far more than the buffers of the connection
	stall := make(chan struct{})
	t.Cleanup(func() { close(stall) })
	tt.fake.set(func(f *fakeTransfers) { f.stall = stall })
	if err := os.WriteFile(filepath.Join(tt.local, "big.bin"), nil, 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.Truncate(filepath.Join(tt.local, "big.bin"), 64<<20); err != nil {
		t.Fatal(err)
	}
	big := map[string]any{"local_path": "big.bin", "remote_path": remote, "overwrite": true, "in_place": true}
	tt.expect(t, uploadTool, big, false, "Still copying big.bin to "+remote+": ")
	tt.expect(t, uploadTool, map[string]any{"local_path": "big.bin", "remote_path": remote, "overwrite": true}, false,
		"Still copying big.bin to "+remote+": ")
	big["cancel"] = true
	text := tt.expect(t, uploadTool, big, false, "Canceled the copy of big.bin to "+remote+" after ")
	if !strings.HasSuffix(text, ": "+remote+", written in place, may be partly written.") {
		t.Errorf("expected the cancel to tell the file may be partly written, got %q", text)
	}
	if got := tt.fake.inPlaces(); len(got) != 3 || !got[0] || !got[1] || !got[2] {
		t.Errorf("expected Rat-In-Place true for each upload, got %v", got)
	}
}

// TestTransferLeftInPlace guards what an upload in place with overwrite tells it left of its
// destination when it fails before its end, rat not knowing whether ratd wrote into it: partly
// written maybe, for a cut, a read failing, a change, and an answer lost once all of it was sent,
// which ratd may not have received whole, the old content gone all the same, told to call again
// with overwrite and in_place, rather than with overwrite, read as without in_place. Without in
// place, or without overwrite, a missing file being created all or nothing: as it was, or written
// or not for an answer lost. The reason of the answer lost loses its final period, as Windows ends
// its messages with one.
func TestTransferLeftInPlace(t *testing.T) {
	tr := &transfers{ctx: context.Background()}
	for _, q := range []transferRequest{
		{tool: uploadTool, local: "up.bin", remote: "/r/up.bin", overwrite: true, inPlace: true},
		{tool: uploadTool, local: "up.bin", remote: "/r/up.bin", overwrite: true},
		{tool: uploadTool, local: "up.bin", remote: "/r/up.bin", inPlace: true},
	} {
		left := "/r/up.bin is as it was."
		lost := "/r/up.bin may be written or not."
		again := "overwrite"
		if q.inPlace && q.overwrite {
			left = "/r/up.bin, written in place, may be partly written."
			lost = left
			again = "overwrite and in_place"
		}
		j := &job{request: q}
		for err, told := range map[error]string{
			&connect.TransferError{Kind: connect.ErrCut, Reason: errors.New("broken pipe")}: "The copy of up.bin to " +
				"/r/up.bin was cut: broken pipe. " + left,
			&connect.TransferError{Kind: connect.ErrRead, Reason: errors.New("input/output error")}: "Reading up.bin " +
				"failed: input/output error. " + left,
			connect.ErrChanged: "up.bin changed while it was read: it may still be written. Upload it again once it " +
				"is complete. " + left,
			&connect.TransferError{Kind: connect.ErrAnswerLost, Reason: errors.New("wsarecv: An existing connection " +
				"was forcibly closed by the remote host.")}: "The answer of ratd to the copy of up.bin to /r/up.bin " +
				"was lost once all of it was sent (wsarecv: An existing connection was forcibly closed by the remote " +
				"host): " + lost + " Check it before using it, or call upload_file again with " + again + ".",
		} {
			if got, _, _ := tr.failureText(j, err); got != told {
				t.Errorf("overwrite %v, in place %v, %v: expected %q, got %q", q.overwrite, q.inPlace, err, told, got)
			}
		}
	}
}

// TestUploadCancelTooLateRefused guards a cancel coming once all of an upload was sent, too late to
// cut it, ratd then failing the upload (failWith, a disk full: 507): told with the sentence of ratd
// first, then the cancel too late, rather than canceled, its reason lost, or behind "Too late to
// cancel", which reads as a copy done.
func TestUploadCancelTooLateRefused(t *testing.T) {
	shortWait(t, time.Second)
	tt := startTransfers(t)
	stall := make(chan struct{})
	release := sync.OnceFunc(func() { close(stall) })
	t.Cleanup(release)
	randomFile(t, filepath.Join(tt.local, "up.bin"), 64<<10, 0o644)
	remote := filepath.Join(tt.remote, "up.bin")
	failure := "Writing " + remote + " failed: no space left on device."
	// the fake reads a first chunk, then stalls, the rest in the buffers of the connection; once
	// released, it fails the upload
	tt.fake.set(func(f *fakeTransfers) { f.stall, f.failWith = stall, failure })
	args := map[string]any{"local_path": "up.bin", "remote_path": remote}
	tt.expect(t, uploadTool, args, false, "Still copying up.bin to "+remote+": 64 KiB of 64 KiB")
	// sent whole: the cancel comes too late, ratd failing the upload meanwhile
	go func() {
		time.Sleep(200 * time.Millisecond)
		release()
	}()
	args["cancel"] = true
	tt.expect(t, uploadTool, args, true, failure+" The cancel came too late to stop it.")
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
// existing file refused without overwrite before any request, as a path under a file or naming a
// directory; replaced with it, keeping its own mode, told when not the one of the remote file;
// and no temporary file left.
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
	// under a file: refused before any request too, rather than once ratd answered
	tt.expect(t, downloadTool, map[string]any{"remote_path": remote, "local_path": "new/dir/app.bin/x.bin"}, true,
		"Writing new/dir/app.bin/x.bin failed: not a directory.")
	if tt.fake.count() != requests {
		t.Error("expected a path under a file refused before any request")
	}
	// a local path naming a directory, which cleaning would make the name of a file: refused before
	// any request
	tt.expect(t, downloadTool, map[string]any{"remote_path": remote, "local_path": "logs/"}, true,
		`local_path "logs/" names a directory: give the name of the file to write in it.`)
	if _, err := os.Lstat(filepath.Join(tt.local, "logs")); err == nil || tt.fake.count() != requests {
		t.Error("expected a local path naming a directory refused before any request, no file written")
	}
	// a NUL byte under a directory missing, where a look tells the file missing: refused before any
	// request, and before the directory is created for it
	tt.expect(t, downloadTool, map[string]any{"remote_path": remote, "local_path": "missing/a\x00b"}, true,
		`local_path "missing/a\x00b" holds a NUL byte, which no file name can hold.`)
	if tt.fake.count() != requests {
		t.Error("expected a NUL byte refused before any request")
	}
	if _, err := os.Lstat(filepath.Join(tt.local, "missing")); err == nil {
		t.Error("expected no directory created for a path holding a NUL byte")
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

// TestDownloadFileAppearedAtLink guards a local file appearing at the destination of a download as
// its temporary file is linked there, after the last look: the link fails, where a rename would
// replace the file, and the file found then is not replaced, kept as it appeared.
func TestDownloadFileAppearedAtLink(t *testing.T) {
	// set before rat starts, and restored once it stopped (cleanups run in reverse order)
	link := linkLocal
	linkLocal = func(oldname, newname string) error {
		if err := os.WriteFile(newname, []byte("appeared"), 0o600); err != nil {
			t.Error(err)
		}
		return link(oldname, newname)
	}
	t.Cleanup(func() { linkLocal = link })
	tt := startTransfers(t)
	remote := filepath.Join(tt.remote, "app.bin")
	randomFile(t, remote, 1<<10, 0o644)
	tt.expect(t, downloadTool, map[string]any{"remote_path": remote, "local_path": "x.bin"}, true,
		"x.bin exists: set overwrite to replace it.")
	expectContent(t, filepath.Join(tt.local, "x.bin"), []byte("appeared"), 0o600)
	expectNoTemporary(t, tt.local)
}

// TestUploadFileReplacedBeforeOpen guards a local file replaced between the look of an upload and
// its open (a file renamed over it): refused, telling it changed, before any request, rather than
// sending another file than the one looked at.
func TestUploadFileReplacedBeforeOpen(t *testing.T) {
	// set before rat starts, and restored once it stopped (cleanups run in reverse order)
	hook := uploadLooked
	var replace func(string)
	uploadLooked = func(local string) { replace(local) }
	t.Cleanup(func() { uploadLooked = hook })
	tt := startTransfers(t)
	randomFile(t, filepath.Join(tt.local, "up.bin"), 1<<10, 0o644)
	randomFile(t, filepath.Join(tt.local, "other.bin"), 1<<10, 0o644)
	replace = func(string) {
		if err := os.Rename(filepath.Join(tt.local, "other.bin"), filepath.Join(tt.local, "up.bin")); err != nil {
			t.Error(err)
		}
	}
	tt.expect(t, uploadTool, map[string]any{"local_path": "up.bin", "remote_path": filepath.Join(tt.remote, "up.bin")},
		true, "up.bin changed while it was opened: upload it again.")
	if n := tt.fake.count(); n != 0 {
		t.Errorf("expected no request, %d made", n)
	}
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
	linkLocal = func(oldname, newname string) error {
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

// TestDownloadFilePlacedReplyLost guards a download put in place by a link or a rename that the
// system made and failed all the same, as on NFS when the reply is lost and the request sent again
// fails (link(2), BUGS): told downloaded, created or replaced, rather than told that the file
// exists, or that writing it failed.
func TestDownloadFilePlacedReplyLost(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("the errors of a link without hard links are those of Unix")
	}
	// set before rat starts, and restored once it stopped (cleanups run in reverse order)
	link, rename := linkLocal, renameLocal
	linkLocal = func(oldname, newname string) error {
		if filepath.Base(newname) == "unlinked.bin" {
			// a file system without hard links: renamed instead
			return &os.LinkError{Op: "linkat", Old: oldname, New: newname, Err: syscall.EPERM}
		}
		if err := link(oldname, newname); err != nil {
			return err
		}
		return &os.LinkError{Op: "linkat", Old: oldname, New: newname, Err: syscall.EEXIST}
	}
	renameLocal = func(oldname, newname string) error {
		if err := rename(oldname, newname); err != nil {
			return err
		}
		return &os.LinkError{Op: "renameat", Old: oldname, New: newname, Err: syscall.ENOENT}
	}
	t.Cleanup(func() { linkLocal, renameLocal = link, rename })
	tt := startTransfers(t)
	remote := filepath.Join(tt.remote, "app.bin")
	content := randomFile(t, remote, 1<<10, 0o644)
	for _, download := range []struct {
		local     string
		overwrite bool
		told      string
	}{{"x.bin", false, "created"}, {"x.bin", true, "replaced"}, {"unlinked.bin", false, "created"}} {
		tt.expect(t, downloadTool, map[string]any{"remote_path": remote, "local_path": download.local,
			"overwrite": download.overwrite}, false, "Downloaded "+remote+" to "+download.local+" (1 KiB): "+
			download.told+".")
		expectContent(t, filepath.Join(tt.local, download.local), content, 0o644&^umask())
	}
	expectNoTemporary(t, tt.local)
}

// TestDownloadFileReadOnlyKept guards a download with overwrite to a read-only file, which a rename
// can not replace on Windows: told read-only, with what to do, rather than access denied, the file
// kept as it was; a writable file whose rename is denied all the same told as the system tells it.
// Its rename refuses as Windows does, readOnlyKept set as there.
func TestDownloadFileReadOnlyKept(t *testing.T) {
	// set before rat starts, and restored once it stopped (cleanups run in reverse order)
	kept, rename := readOnlyKept, renameLocal
	readOnlyKept = true
	renameLocal = func(oldname, newname string) error {
		if info, err := os.Lstat(newname); err == nil && (info.Mode().Perm()&0o200 == 0 ||
			filepath.Base(newname) == "denied.bin") {
			return &os.LinkError{Op: "rename", Old: oldname, New: newname, Err: fs.ErrPermission}
		}
		return rename(oldname, newname)
	}
	t.Cleanup(func() { readOnlyKept, renameLocal = kept, rename })
	tt := startTransfers(t)
	remote := filepath.Join(tt.remote, "app.bin")
	randomFile(t, remote, 1<<10, 0o644)
	local := filepath.Join(tt.local, "x.bin")
	previous := randomFile(t, local, 10, 0o444)
	tt.expect(t, downloadTool, map[string]any{"remote_path": remote, "local_path": "x.bin", "overwrite": true}, true,
		"x.bin is read-only, which Windows keeps from being replaced: clear its read-only attribute, or download to "+
			"another name.")
	expectContent(t, local, previous, 0o444)
	randomFile(t, filepath.Join(tt.local, "denied.bin"), 10, 0o644)
	tt.expect(t, downloadTool, map[string]any{"remote_path": remote, "local_path": "denied.bin", "overwrite": true},
		true, "Writing denied.bin failed: permission denied.")
	expectNoTemporary(t, tt.local)
}

// TestDownloadFileReadOnlyDenied guards a download with overwrite to a read-only file whose rename is
// denied where a rename replaces read-only files (Unix, the directory deciding): told as the system
// tells it, rather than as a read-only file Windows keeps.
func TestDownloadFileReadOnlyDenied(t *testing.T) {
	// set before rat starts, and restored once it stopped (cleanups run in reverse order)
	kept, rename := readOnlyKept, renameLocal
	readOnlyKept = false
	renameLocal = func(oldname, newname string) error {
		return &os.LinkError{Op: "rename", Old: oldname, New: newname, Err: fs.ErrPermission}
	}
	t.Cleanup(func() { readOnlyKept, renameLocal = kept, rename })
	tt := startTransfers(t)
	remote := filepath.Join(tt.remote, "app.bin")
	randomFile(t, remote, 1<<10, 0o644)
	randomFile(t, filepath.Join(tt.local, "x.bin"), 10, 0o444)
	tt.expect(t, downloadTool, map[string]any{"remote_path": remote, "local_path": "x.bin", "overwrite": true}, true,
		"Writing x.bin failed: permission denied.")
	expectNoTemporary(t, tt.local)
}

// TestDownloadFileNameTooLong guards a download to a name longer than any file system takes,
// though its temporary file, named to fit, could take the bytes: refused before any byte is
// written, in the transfers directory before any request, and under a directory to create once
// it is created, a look before the request stopping at it, the directory then named, or once
// another transfer created it meanwhile; a directory to create too long names those created
// before it. No temporary file is left.
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
	// the directory missing before the request, created by another transfer before the answer
	tt.fake.set(func(f *fakeTransfers) {
		f.meanwhile = func() { _ = os.Mkdir(filepath.Join(tt.local, "meanwhile"), 0o755) }
	})
	other := filepath.Join("meanwhile", name)
	tt.expect(t, downloadTool, map[string]any{"remote_path": remote, "local_path": other}, true,
		"Writing "+other+" failed: file name too long.")
	expectNoTemporary(t, tt.local)
}

// TestDownloadFileFailed guards downloads failing: refused by ratd (its sentence relayed), failed
// by its trailer (a file changed while it read it, its sentence told whole even when its path holds
// what a header can not carry), cut midway (the reason told, rather than
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
	// a name holding what a header can not carry: the sentence of ratd told whole
	escaped := filepath.Join(tt.remote, "app\x1b.log")
	randomFile(t, escaped, 100<<10, 0o644)
	failure = escaped + " changed while it was read: it may still be written. Download it again once it is complete."
	tt.fake.set(func(f *fakeTransfers) { f.failWith = failure })
	tt.expect(t, downloadTool, map[string]any{"remote_path": escaped, "local_path": "a.bin"}, true, failure)
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

// TestTransferProgressSlow guards the progress of a transfer crawling: under 1 B/s, told 0 B/s and
// no time left, rather than a time left next to 0 B/s; at a few bytes a second with a large file
// left, a time left in hours, rather than its duration overflowing, told less than a minute or
// negative.
func TestTransferProgressSlow(t *testing.T) {
	now := time.Now()
	q := transferRequest{tool: downloadTool, local: "big.bin", remote: "/r/big.bin"}
	for _, tc := range []struct {
		what  string
		moved int64
		size  int64
		told  string
	}{
		// 3 bytes in the last 30 seconds: 0.1 B/s
		{"under 1 B/s", 3, 10 << 30, ", 0 B/s. "},
		// 300 bytes in the last 30 seconds: 10 B/s, 1 PiB left, millions of years
		{"10 B/s, 1 PiB left", 300, 1 << 50, " hours left. "},
	} {
		j := &job{request: q, started: now.Add(-2 * time.Minute), lastMoved: now.Add(-time.Second),
			samples: []sample{{at: now.Add(-rateWindow), bytes: 100 << 20}}}
		j.moved.Store(100<<20 + tc.moved)
		j.size.Store(tc.size)
		text := (&transfers{}).progressText(j, 100<<20)
		if !strings.Contains(text, tc.told) || strings.Contains(text, "less than a minute") ||
			strings.Contains(text, " -") {
			t.Errorf("%s: expected %q, got %q", tc.what, tc.told, text)
		}
	}
}

// TestAgoText guards how long ago a transfer ended, as a call finding it ended tells it: in the
// singular for one unit, rather than "1 minutes ago".
func TestAgoText(t *testing.T) {
	for d, told := range map[time.Duration]string{
		500 * time.Millisecond:  "under a second",
		time.Second:             "1 second",
		1900 * time.Millisecond: "1 second",
		2 * time.Second:         "2 seconds",
		time.Minute:             "1 minute",
		90 * time.Second:        "1 minute",
		2 * time.Minute:         "2 minutes",
		time.Hour:               "1 hour",
		3 * time.Hour:           "3 hours",
	} {
		if got := agoText(d); got != told {
			t.Errorf("%v: expected %q, got %q", d, told, got)
		}
	}
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

// TestCleanRemote guards the cleaning of a remote path, as ratd reads it: absolute or after ~/, its
// spellings made one (a doubled slash, after ~/ too, a dot, a parent), a parent above ~ kept, which
// rat can not resolve; a relative path, which ratd refuses, left as it is, rather than cleaned into
// one starting with ~/, which ratd would read as its home.
func TestCleanRemote(t *testing.T) {
	for remote, clean := range map[string]string{
		"/a//b/./c/../d": "/a/b/d", "/../x": "/x", "~/a//b/./c/../d": "~/a/b/d", "~//x": "~/x", "~///x": "~/x",
		"~/../x": "~/../x", "~/a/../../x": "~/../x", "~": "~", "./~/x": "./~/x", "a/../~/x": "a/../~/x",
		"x//y": "x//y", "~user/x": "~user/x",
	} {
		if got := cleanRemote(remote); got != clean {
			t.Errorf("%q: expected %q, got %q", remote, clean, got)
		}
	}
}

// TestTransferRemoteSpellings guards a remote path spelled two ways (a doubled slash, a dot, a
// parent): one transfer, joined by a call with the other spelling and canceled by it, rather than
// two writing one file, a cancel finding none under way.
func TestTransferRemoteSpellings(t *testing.T) {
	shortWait(t, 300*time.Millisecond)
	tt := startTransfers(t)
	stall := make(chan struct{})
	t.Cleanup(func() { close(stall) })
	tt.fake.set(func(f *fakeTransfers) { f.stall = stall })
	// zeros, sparse: far more than the buffers of the connection hold, the upload staying under way
	if err := os.WriteFile(filepath.Join(tt.local, "up.bin"), nil, 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.Truncate(filepath.Join(tt.local, "up.bin"), 64<<20); err != nil {
		t.Fatal(err)
	}
	remote := filepath.Join(tt.remote, "up.bin")
	other := tt.remote + "//./sub/../up.bin"
	tt.expect(t, uploadTool, map[string]any{"local_path": "up.bin", "remote_path": remote}, false,
		"Still copying up.bin to "+remote+": ")
	tt.expect(t, uploadTool, map[string]any{"local_path": "up.bin", "remote_path": other}, false,
		"Still copying up.bin to "+remote+": ")
	if n := tt.fake.count(); n != 1 {
		t.Errorf("expected one transfer, %d requests made", n)
	}
	tt.expect(t, uploadTool, map[string]any{"local_path": "up.bin", "remote_path": other, "cancel": true}, false,
		"Canceled the copy of up.bin to "+remote+" after ")
}

// holdFirst sets preparing, until the test ends, to hold the first job of each destination named,
// by its base name, as a file system not answering would, until the function returned for that
// name is called. Set before rat starts, restored once it stopped (cleanups run in reverse order):
// the caller registers the functions returned as cleanups once rat started, rat waiting for its
// transfers as it stops.
func holdFirst(t *testing.T, names ...string) map[string]func() {
	t.Helper()
	var mu sync.Mutex
	holds := map[string]chan struct{}{}
	releases := map[string]func(){}
	for _, name := range names {
		hold := make(chan struct{})
		holds[name] = hold
		releases[name] = sync.OnceFunc(func() { close(hold) })
	}
	hook := preparing
	preparing = func(q transferRequest) {
		mu.Lock()
		hold := holds[filepath.Base(q.destination())]
		delete(holds, filepath.Base(q.destination()))
		mu.Unlock()
		if hold != nil {
			<-hold
		}
	}
	t.Cleanup(func() { preparing = hook })
	return releases
}

// untilTold calls tool with args until its answer is no longer the progress of a copy, and returns
// it.
func (tt *transferTest) untilTold(t *testing.T, tool string, args map[string]any) (string, bool) {
	t.Helper()
	for deadline := time.Now().Add(10 * time.Second); time.Now().Before(deadline); {
		if text, isError := tt.call(t, tool, args); !strings.HasPrefix(text, "Still copying") {
			return text, isError
		}
	}
	t.Fatalf("%s %v: still copying 10 seconds on", tool, args)
	return "", false
}

// untilLogged waits for rat to log the end of the transfer of tool from local to remote, its
// outcome set: ended, and not told yet by any call.
func (tt *transferTest) untilLogged(t *testing.T, tool, local, remote string) {
	t.Helper()
	ended := "msg=transfer direction=" + tool + " local=" + local + " remote=" + remote + " outcome="
	for deadline := time.Now().Add(10 * time.Second); time.Now().Before(deadline); time.Sleep(10 * time.Millisecond) {
		if strings.Contains(tt.rat.logs.String(), ended) {
			return
		}
	}
	t.Fatalf("the transfer of %s to %s did not end 10 seconds on:\n%s", local, remote, tt.rat.logs)
}

// TestTransferJoinedWhateverOptions guards a call with the paths of a running job asking more than
// that job (overwrite, overwrite and in_place): joined, told its progress, rather than refused or a
// second copy started; told how the job ended, once, what the job was asked deciding (a file
// created, sent without in_place); the same call again then starting the copy it asks.
func TestTransferJoinedWhateverOptions(t *testing.T) {
	shortWait(t, 300*time.Millisecond)
	releases := holdFirst(t, "up.bin")
	tt := startTransfers(t)
	for _, release := range releases {
		t.Cleanup(release)
	}
	randomFile(t, filepath.Join(tt.local, "up.bin"), 1<<10, 0o644)
	remote := filepath.Join(tt.remote, "up.bin")
	tt.expect(t, uploadTool, map[string]any{"local_path": "up.bin", "remote_path": remote}, false,
		"Still copying up.bin to "+remote+": 0 B")
	overwrite := map[string]any{"local_path": "up.bin", "remote_path": remote, "overwrite": true}
	inPlace := map[string]any{"local_path": "up.bin", "remote_path": remote, "overwrite": true, "in_place": true}
	for _, call := range []map[string]any{overwrite, inPlace} {
		tt.expect(t, uploadTool, call, false, "Still copying up.bin to "+remote+": 0 B")
	}
	releases["up.bin"]()
	if text, isError := tt.untilTold(t, uploadTool, inPlace); isError ||
		!strings.HasPrefix(text, "Uploaded up.bin to "+remote+" (1 KiB): created.") {
		t.Errorf("expected the job told, its file created, got %v: %q", isError, text)
	}
	if got := tt.fake.inPlaces(); !slices.Equal(got, []bool{false}) {
		t.Errorf("expected one upload, without in_place, got the in place of %v", got)
	}
	tt.expect(t, uploadTool, inPlace, false, "Uploaded up.bin to "+remote+" (1 KiB): written in place.")
	if got := tt.fake.inPlaces(); !slices.Equal(got, []bool{false, true}) {
		t.Errorf("expected the same call again to upload in place, got the in place of %v", got)
	}
}

// TestTransferJoinedTellsHowReplaced guards the answer of an upload that replaced its file, told to
// a call joining it with other options: how that upload replaced it, a new file put in place of the
// old one, whose hard link keeps the old content, or the file written in place, its hard link
// following, rather than "replaced" alone, which a call asking the other way would read as its own.
func TestTransferJoinedTellsHowReplaced(t *testing.T) {
	shortWait(t, 300*time.Millisecond)
	releases := holdFirst(t, "new.bin", "into.bin")
	tt := startTransfers(t)
	for _, release := range releases {
		t.Cleanup(release)
	}
	content := randomFile(t, filepath.Join(tt.local, "up.bin"), 1<<10, 0o644)
	for _, replaced := range []struct {
		name            string
		inPlace, joined bool
		how             string
	}{{"new.bin", false, true, "replaced by a new file"}, {"into.bin", true, false, "written in place"}} {
		remote := filepath.Join(tt.remote, replaced.name)
		old := randomFile(t, remote, 10, 0o644)
		link := remote + ".link"
		if err := os.Link(remote, link); err != nil {
			t.Fatal(err)
		}
		tt.expect(t, uploadTool, map[string]any{"local_path": "up.bin", "remote_path": remote, "overwrite": true,
			"in_place": replaced.inPlace}, false, "Still copying up.bin to "+remote+": 0 B")
		joining := map[string]any{"local_path": "up.bin", "remote_path": remote, "overwrite": true,
			"in_place": replaced.joined}
		tt.expect(t, uploadTool, joining, false, "Still copying up.bin to "+remote+": 0 B")
		releases[replaced.name]()
		told := "Uploaded up.bin to " + remote + " (1 KiB): " + replaced.how + "."
		if text, isError := tt.untilTold(t, uploadTool, joining); isError || !strings.HasPrefix(text, told) {
			t.Errorf("expected %q, got %v: %q", told, isError, text)
		}
		if replaced.inPlace {
			expectContent(t, link, content, 0o644)
		} else {
			expectContent(t, link, old, 0o644)
		}
	}
	if got := tt.fake.inPlaces(); !slices.Equal(got, []bool{false, true}) {
		t.Errorf("expected each upload sent as its job was asked, the calls joining them, got the in place of %v", got)
	}
}

// TestTransferEndedToldWhateverOptions guards a job that ended without writing its file, its outcome
// not told yet, met by a call with its paths asking more (overwrite, overwrite and in_place): told
// that outcome, once, whatever the call asked (its file existing, refused without overwrite; its
// answer lost; canceled), rather than forgotten untold for the copy asked to start; the same call
// again then starts the copy it asks.
func TestTransferEndedToldWhateverOptions(t *testing.T) {
	shortWait(t, 300*time.Millisecond)
	releases := holdFirst(t, "refused.bin", "lost.bin", "canceled.bin")
	// the download to canceled.bin canceled once its file came whole, long after its call
	hookDownloaded := downloaded
	downloaded = func(j *job) {
		if j.request.local == "canceled.bin" && !j.request.overwrite {
			j.askCancel()
		}
	}
	t.Cleanup(func() { downloaded = hookDownloaded })
	tt := startTransfers(t)
	for _, release := range releases {
		t.Cleanup(release)
	}
	randomFile(t, filepath.Join(tt.local, "up.bin"), 1<<10, 0o644)

	// started, held past the wait of its call, released, and ended untold
	endedUntold := func(tool, name string, args map[string]any) {
		t.Helper()
		remote := args["remote_path"].(string)
		tt.expect(t, tool, args, false, "Still copying ")
		releases[name]()
		tt.untilLogged(t, tool, args["local_path"].(string), remote)
	}

	refused := filepath.Join(tt.remote, "refused.bin")
	randomFile(t, refused, 10, 0o644)
	endedUntold(uploadTool, "refused.bin", map[string]any{"local_path": "up.bin", "remote_path": refused})
	inPlace := map[string]any{"local_path": "up.bin", "remote_path": refused, "overwrite": true, "in_place": true}
	tt.expect(t, uploadTool, inPlace, true, refused+" exists: set overwrite to replace it. It ended ")
	tt.expect(t, uploadTool, inPlace, false, "Uploaded up.bin to "+refused+" (1 KiB): written in place.")

	lost := filepath.Join(tt.remote, "lost.bin")
	randomFile(t, lost, 10, 0o644)
	tt.fake.set(func(f *fakeTransfers) { f.dropAnswer = true })
	endedUntold(uploadTool, "lost.bin", map[string]any{"local_path": "up.bin", "remote_path": lost, "overwrite": true})
	tt.fake.set(func(f *fakeTransfers) { f.dropAnswer = false })
	inPlace = map[string]any{"local_path": "up.bin", "remote_path": lost, "overwrite": true, "in_place": true}
	text := tt.expect(t, uploadTool, inPlace, true, "The answer of ratd to the copy of up.bin to "+lost+" was lost "+
		"once all of it was sent (")
	if !strings.Contains(text, ": "+lost+" may be written or not. Check it before using it, or call upload_file "+
		"again with overwrite. It ended ") {
		t.Errorf("expected the answer lost told as the job was asked, without in_place, got %q", text)
	}
	tt.expect(t, uploadTool, inPlace, false, "Uploaded up.bin to "+lost+" (1 KiB): written in place.")

	remote := filepath.Join(tt.remote, "data.bin")
	randomFile(t, remote, 1<<10, 0o644)
	endedUntold(downloadTool, "canceled.bin", map[string]any{"remote_path": remote, "local_path": "canceled.bin"})
	overwrite := map[string]any{"remote_path": remote, "local_path": "canceled.bin", "overwrite": true}
	tt.expect(t, downloadTool, overwrite, false, "Canceled the copy of "+remote+" to canceled.bin after 1 KiB of "+
		"1 KiB: canceled.bin is as it was. It ended ")
	tt.expect(t, downloadTool, overwrite, false, "Downloaded "+remote+" to canceled.bin (1 KiB): created.")
	if got := tt.fake.inPlaces(); !slices.Equal(got, []bool{false, true, false, true}) {
		t.Errorf("expected each job sent as asked, the same call again in place, got the in place of %v", got)
	}
}

// TestTransferCancelWhateverOptions guards a cancel with the paths of a running job, asking an
// option the job was not asked (in_place, the job asked overwrite): the job canceled, told as it was asked (its
// destination as it was, not written in place), rather than refused as another copy; the same paths
// again then start the copy with the options they ask.
func TestTransferCancelWhateverOptions(t *testing.T) {
	shortWait(t, 300*time.Millisecond)
	tt := startTransfers(t)
	stall := make(chan struct{})
	t.Cleanup(func() { close(stall) })
	tt.fake.set(func(f *fakeTransfers) { f.stall = stall })
	// zeros, sparse: far more than the buffers of the connection hold, the upload staying under way
	if err := os.WriteFile(filepath.Join(tt.local, "up.bin"), nil, 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.Truncate(filepath.Join(tt.local, "up.bin"), 64<<20); err != nil {
		t.Fatal(err)
	}
	remote := filepath.Join(tt.remote, "up.bin")
	randomFile(t, remote, 10, 0o644)
	tt.expect(t, uploadTool, map[string]any{"local_path": "up.bin", "remote_path": remote, "overwrite": true}, false,
		"Still copying up.bin to "+remote+": ")
	text := tt.expect(t, uploadTool, map[string]any{"local_path": "up.bin", "remote_path": remote, "overwrite": true,
		"in_place": true, "cancel": true}, false, "Canceled the copy of up.bin to "+remote+" after ")
	if !strings.HasSuffix(text, ": "+remote+" is as it was.") {
		t.Errorf("expected the cancel told as the job was asked, not in place, got %q", text)
	}
	tt.fake.set(func(f *fakeTransfers) { f.stall = nil })
	content := randomFile(t, filepath.Join(tt.local, "up.bin"), 1<<10, 0o644)
	tt.expect(t, uploadTool, map[string]any{"local_path": "up.bin", "remote_path": remote, "overwrite": true,
		"in_place": true}, false, "Uploaded up.bin to "+remote+" (1 KiB): written in place.")
	expectContent(t, remote, content, 0o644)
	if got := tt.fake.inPlaces(); !slices.Equal(got, []bool{false, true}) {
		t.Errorf("expected the job canceled without in_place, the copy after it in place, got the in place of %v", got)
	}
}

// TestTransferOverwriteLeftOut guards a transfer with overwrite called again with its paths,
// overwrite left out, as its progress tells to call again with the same paths: joined while it
// runs, its progress told, and once it ended, told how, rather than refused, or told to copy again.
func TestTransferOverwriteLeftOut(t *testing.T) {
	shortWait(t, 300*time.Millisecond)
	tt := startTransfers(t)
	stall := make(chan struct{})
	release := sync.OnceFunc(func() { close(stall) })
	t.Cleanup(release)
	tt.fake.set(func(f *fakeTransfers) { f.stall = stall })
	remote := filepath.Join(tt.remote, "big.bin")
	randomFile(t, remote, 1<<20, 0o644)
	samePaths := map[string]any{"remote_path": remote, "local_path": "big.bin"}
	tt.expect(t, downloadTool, map[string]any{"remote_path": remote, "local_path": "big.bin", "overwrite": true}, false,
		"Still copying "+remote+" to big.bin: ")
	tt.expect(t, downloadTool, samePaths, false, "Still copying "+remote+" to big.bin: ")
	release()
	var text string
	var isError bool
	for deadline := time.Now().Add(10 * time.Second); time.Now().Before(deadline); time.Sleep(20 * time.Millisecond) {
		if text, isError = tt.call(t, downloadTool, samePaths); !strings.HasPrefix(text, "Still copying") {
			break
		}
	}
	if isError || !strings.HasPrefix(text, "Downloaded "+remote+" to big.bin (1 MiB): created.") {
		t.Errorf("expected the transfer told ended, got %v: %q", isError, text)
	}
	if n := tt.fake.count(); n != 1 {
		t.Errorf("expected one transfer, %d requests made", n)
	}
}

// TestTransferCancelDuringPrepare guards a cancel coming as a transfer checks its local side, which
// then refuses it (a file existing without overwrite): told the refusal alone, before any request,
// rather than "the cancel came too late", nothing having run for it to stop.
func TestTransferCancelDuringPrepare(t *testing.T) {
	shortWait(t, 3*time.Second)
	held := make(chan struct{})
	release := sync.OnceFunc(func() { close(held) })
	entered := make(chan struct{}, 1)
	// set before rat starts, and restored once it stopped (cleanups run in reverse order)
	hook := preparing
	preparing = func(q transferRequest) {
		if q.local == "exists.bin" {
			select {
			case entered <- struct{}{}:
			default:
			}
			<-held
		}
	}
	t.Cleanup(func() { preparing = hook })
	tt := startTransfers(t)
	// released before rat stops, which waits for its transfers
	t.Cleanup(release)
	remote := filepath.Join(tt.remote, "data.bin")
	randomFile(t, remote, 1<<10, 0o644)
	randomFile(t, filepath.Join(tt.local, "exists.bin"), 10, 0o644)
	args := map[string]any{"remote_path": remote, "local_path": "exists.bin"}
	go func() {
		_, _ = tt.session.CallTool(context.Background(), &mcp.CallToolParams{Name: downloadTool, Arguments: args})
	}()
	<-entered
	// the cancel asked while the local side is held, released then
	go func() {
		time.Sleep(300 * time.Millisecond)
		release()
	}()
	text, isError := tt.call(t, downloadTool, map[string]any{"remote_path": remote, "local_path": "exists.bin",
		"cancel": true})
	if !isError || text != "exists.bin exists: set overwrite to replace it." {
		t.Errorf("expected the refusal of the local side alone, got %v: %q", isError, text)
	}
	if n := tt.fake.count(); n != 0 {
		t.Errorf("expected no request, %d made", n)
	}
}

// TestTransferPrepareHeld guards a transfer whose local side does not answer (a stale NFS mount in
// the transfers directory): it holds that transfer alone, its call returning once it waited,
// telling its progress, while a call for other paths goes on, rather than every transfer call
// held behind it; once the local side answers, the transfer goes on.
func TestTransferPrepareHeld(t *testing.T) {
	shortWait(t, 300*time.Millisecond)
	held := make(chan struct{})
	release := sync.OnceFunc(func() { close(held) })
	entered := make(chan struct{}, 1)
	// set before rat starts, and restored once it stopped (cleanups run in reverse order)
	hook := preparing
	preparing = func(q transferRequest) {
		if q.local == "held.bin" {
			select {
			case entered <- struct{}{}:
			default:
			}
			<-held
		}
	}
	t.Cleanup(func() { preparing = hook })
	tt := startTransfers(t)
	// released before rat stops, which waits for its transfers
	t.Cleanup(release)
	remote := filepath.Join(tt.remote, "data.bin")
	randomFile(t, remote, 1<<10, 0o644)
	heldArgs := map[string]any{"remote_path": remote, "local_path": "held.bin"}
	done := make(chan string, 1)
	go func() {
		result, err := tt.session.CallTool(context.Background(), &mcp.CallToolParams{Name: downloadTool,
			Arguments: heldArgs})
		switch {
		case err != nil:
			done <- err.Error()
		case len(result.Content) == 0:
			done <- "no content"
		default:
			text, _ := result.Content[0].(*mcp.TextContent)
			done <- text.Text
		}
	}()
	<-entered
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	if text, isError := tt.callContext(t, ctx, downloadTool, map[string]any{"remote_path": remote,
		"local_path": "free.bin"}); isError || !strings.HasPrefix(text, "Downloaded ") {
		t.Errorf("expected a call for other paths to go on, got %q", text)
	}
	select {
	case text := <-done:
		if !strings.HasPrefix(text, "Still copying "+remote+" to held.bin: 0 B") {
			t.Errorf("expected the held call to tell its progress once it waited, got %q", text)
		}
	case <-time.After(5 * time.Second):
		t.Error("the held call did not return once it waited")
	}
	release()
	tt.expect(t, downloadTool, heldArgs, false, "Downloaded "+remote+" to held.bin (1 KiB): created.")
}

// TestTransferEndedOtherSource guards a transfer that ended, its outcome not told yet, met by a call
// to its destination from another source, or by a cancel naming one (an agent after a context
// compaction): told how it ended, and forgotten, the next call starting the transfer asked, rather
// than told under way, which it is while it runs.
func TestTransferEndedOtherSource(t *testing.T) {
	shortWait(t, 300*time.Millisecond)
	tt := startTransfers(t)
	a := filepath.Join(tt.remote, "a.bin")
	randomFile(t, a, 1<<20, 0o644)
	b := filepath.Join(tt.remote, "b.bin")
	randomFile(t, b, 1<<10, 0o644)
	fromA := map[string]any{"remote_path": a, "local_path": "x.bin", "overwrite": true}
	fromB := map[string]any{"remote_path": b, "local_path": "x.bin", "overwrite": true}
	cancelB := map[string]any{"remote_path": b, "local_path": "x.bin", "cancel": true}
	// asked, until a call no longer tells the download from a.bin under way: once it ended, the
	// call tells how, which it never does without the fix
	untilEnded := func(args map[string]any) string {
		t.Helper()
		for deadline := time.Now().Add(10 * time.Second); time.Now().Before(deadline); {
			if text, _ := tt.call(t, downloadTool, args); !strings.Contains(text, "under way") {
				return text
			}
			time.Sleep(20 * time.Millisecond)
		}
		t.Fatalf("%v: still told under way 10 seconds on", args)
		return ""
	}
	for _, other := range []map[string]any{fromB, cancelB} {
		stall := make(chan struct{})
		release := sync.OnceFunc(func() { close(stall) })
		tt.fake.set(func(f *fakeTransfers) { f.stall = stall })
		tt.expect(t, downloadTool, fromA, false, "Still copying ")
		release()
		text := untilEnded(other)
		ended := "The transfer to x.bin from " + a + " ended "
		if !strings.HasPrefix(text, ended) || !strings.Contains(text, ": Downloaded "+a+" to x.bin (1 MiB): replaced.") &&
			!strings.Contains(text, ": Downloaded "+a+" to x.bin (1 MiB): created.") {
			t.Errorf("%v: expected how the download from a.bin ended, got %q", other, text)
		}
		// forgotten: told once
		if other["cancel"] == true {
			tt.expect(t, downloadTool, cancelB, false, "No transfer to x.bin is under way: nothing to cancel.")
		} else {
			tt.expect(t, downloadTool, fromB, false, "Downloaded "+b+" to x.bin (1 KiB): replaced.")
		}
	}
}

// TestTransferCanceledCallKeepsJob guards a call that the harness cancels as its job ends, waiting
// for it or canceling it, or as it finds the job of another source ended: whatever text it is
// handed, select picking at random between the end of the job and the cancel of the call, the job
// is kept until told is called, which the caller does only as it writes the text, rather than
// forgotten by the call itself, its outcome lost with a text dropped.
func TestTransferCanceledCallKeepsJob(t *testing.T) {
	q := transferRequest{tool: uploadTool, local: "up.bin", remote: "/r/up.bin"}
	other := transferRequest{tool: uploadTool, local: "other.bin", remote: "/r/up.bin"}
	for _, way := range []string{"waiting", "canceling", "another source", "canceling another source"} {
		for range 20 {
			j := &job{request: q, cancel: func() {}, done: make(chan struct{}), text: "Uploaded up.bin to /r/up.bin."}
			close(j.done)
			tr := &transfers{jobs: map[string]*job{q.key(): j}}
			ctx, cancel := context.WithCancel(context.Background())
			cancel()
			var text string
			var told func()
			switch way {
			case "waiting":
				text, _, told = tr.await(ctx, j, time.Now())
			case "canceling":
				text, _, told = tr.cancel(ctx, q, time.Now())
			case "another source":
				var ended *job
				_, text, ended = tr.join(other)
				told = tr.forgetting(ended)
			default:
				text, _, told = tr.cancel(ctx, other, time.Now())
			}
			if tr.jobs[q.key()] != j {
				t.Fatalf("%s, the call canceled as its job ended: expected the job kept until told, forgotten "+
					"(text %q)", way, text)
			}
			told()
			if kept := tr.jobs[q.key()] == j; kept != (text == "") {
				t.Fatalf("%s: expected the job forgotten once its outcome is told, and only then, got text %q, "+
					"kept %v", way, text, kept)
			}
		}
	}
}

// TestTransferCanceledAsAnswered guards a call the harness cancels once its transfer ended and rat
// holds its answer, not yet written (transferAnswering): nothing is written for it, and the job is
// kept, the same call again telling how it ended, rather than forgotten with the text dropped, the
// call again then finding the file it just copied and telling it exists.
func TestTransferCanceledAsAnswered(t *testing.T) {
	tt := startTransfers(t)
	remote := filepath.Join(tt.remote, "data.bin")
	randomFile(t, remote, 1<<10, 0o644)
	args := map[string]any{"remote_path": remote, "local_path": "data.bin"}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	hook := transferAnswering
	t.Cleanup(func() { transferAnswering = hook })
	var once sync.Once
	transferAnswering = func(c *call) {
		once.Do(func() {
			// the harness gives up: rat reads its cancellation while it holds the answer
			cancel()
			for deadline := time.Now().Add(5 * time.Second); !c.canceled(); time.Sleep(time.Millisecond) {
				if time.Now().After(deadline) {
					t.Error("expected rat to read the cancellation of the call")
					return
				}
			}
		})
	}
	if _, err := tt.session.CallTool(ctx, &mcp.CallToolParams{Name: downloadTool, Arguments: args}); err == nil {
		t.Fatal("expected the call canceled")
	}
	tt.expect(t, downloadTool, args, false, "Downloaded "+remote+" to data.bin (1 KiB): created. It ended ")
}

// TestTransferCancelEnded guards a cancel of a transfer that ended before the call, its outcome not
// told yet: told how it ended, too late or before any request, with how long ago, as a call waiting
// for it tells, rather than as a transfer the cancel just missed; the same for a cancel asking
// options the transfer was not asked (overwrite and in_place), its paths alone finding it.
func TestTransferCancelEnded(t *testing.T) {
	q := transferRequest{tool: uploadTool, local: "up.bin", remote: "/r/up.bin"}
	for _, ended := range []struct {
		text             string
		isError, refused bool
		told             string
	}{
		{"Uploaded up.bin to /r/up.bin (1 KiB): created.", false, false,
			"Too late to cancel: Uploaded up.bin to /r/up.bin (1 KiB): created. It ended 2 minutes ago."},
		{"The connection to ratd was lost.", true, false,
			"The connection to ratd was lost. The cancel came too late to stop it. It ended 2 minutes ago."},
		{"Reading up.bin failed: permission denied.", true, true,
			"Reading up.bin failed: permission denied. It ended 2 minutes ago."},
	} {
		j := &job{request: q, cancel: func() {}, done: make(chan struct{}), text: ended.text, isError: ended.isError,
			refused: ended.refused, ended: time.Now().Add(-2*time.Minute - time.Second)}
		close(j.done)
		tr := &transfers{jobs: map[string]*job{q.key(): j}}
		canceling := q
		canceling.cancel, canceling.overwrite, canceling.inPlace = true, true, true
		if text, isError, _ := tr.cancel(context.Background(), canceling, time.Now()); text != ended.told ||
			isError != ended.isError {
			t.Errorf("expected %v: %q, got %v: %q", ended.isError, ended.told, isError, text)
		}
	}
}

// TestTransferReasonEndsOnce guards the reason of the system ending a sentence of rat with one
// period, though Windows ends its messages with one ("Access is denied."): a local file failing,
// with a path, a link or no error of its own, and a copy cut.
func TestTransferReasonEndsOnce(t *testing.T) {
	tr := &transfers{ctx: context.Background()}
	denied := errors.New("Access is denied.")
	for _, err := range []error{&fs.PathError{Op: "open", Path: "x.bin", Err: denied},
		&os.LinkError{Op: "rename", Old: ".x.bin.rat-1", New: "x.bin", Err: denied}, denied} {
		if got := tr.localFailure(err, "Writing", "x.bin"); got != "Writing x.bin failed: Access is denied." {
			t.Errorf("%T: expected one period, got %q", err, got)
		}
	}
	j := &job{request: transferRequest{tool: uploadTool, local: "up.bin", remote: "/r/up.bin"}}
	cut := &connect.TransferError{Kind: connect.ErrCut,
		Reason: errors.New("wsarecv: An existing connection was forcibly closed by the remote host.")}
	if got, _, _ := tr.failureText(j, cut); got != "The copy of up.bin to /r/up.bin was cut: wsarecv: An existing "+
		"connection was forcibly closed by the remote host. /r/up.bin is as it was." {
		t.Errorf("a copy cut: expected one period, got %q", got)
	}
}

// TestDownloadFileCanceledWhole guards a download canceled once its file came whole, before it is
// put in place, the last moment a cancel is in time: canceled, the destination as it was, the
// directories it created named, left as they are.
func TestDownloadFileCanceledWhole(t *testing.T) {
	// set before rat starts, and restored once it stopped (cleanups run in reverse order)
	hook := downloaded
	downloaded = func(j *job) { j.askCancel() }
	t.Cleanup(func() { downloaded = hook })
	tt := startTransfers(t)
	remote := filepath.Join(tt.remote, "s.bin")
	randomFile(t, remote, 1<<10, 0o644)
	tt.expect(t, downloadTool, map[string]any{"remote_path": remote, "local_path": "whole/s.bin"}, false,
		"Canceled the copy of "+remote+" to whole/s.bin after 1 KiB of 1 KiB: whole/s.bin is as it was. Missing "+
			"directories created in "+tt.local+": whole. Check the path if you did not mean them.")
	if _, err := os.Lstat(filepath.Join(tt.local, "whole", "s.bin")); err == nil {
		t.Error("expected no file put in place")
	}
	expectNoTemporary(t, tt.local)
}

// TestTransferCancel guards a transfer stalling, then canceled: the call tells nothing moved; a
// cancel stops it, leaving the destination as it was, with no temporary file, naming the
// directories it created; a transfer to the
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
	// canceled once it created its directories, which it names, left as they are
	inNew := map[string]any{"remote_path": remote, "local_path": "new/big.bin"}
	tt.expect(t, downloadTool, inNew, false, "Still copying "+remote+" to new/big.bin: 32 KiB of 1 MiB")
	inNew["cancel"] = true
	tt.expect(t, downloadTool, inNew, false, "Canceled the copy of "+remote+" to new/big.bin after 32 KiB of 1 MiB: "+
		"new/big.bin is as it was. Missing directories created in "+tt.local+": new. Check the path if you did not "+
		"mean them.")
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
