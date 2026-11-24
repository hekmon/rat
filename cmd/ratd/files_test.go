package main

import (
	"context"
	"errors"
	"io/fs"
	"os"
	"os/user"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
	"testing"
	"time"

	"github.com/hekmon/rat/cmd/ratd/tools"
	"github.com/hekmon/rat/tmux"

	"github.com/modelcontextprotocol/go-sdk/mcp"
)

// connectFiles returns an MCP client connected in memory to the tools of session alice, with a
// fresh home, and the logs of ratd. No tmux server runs: file tools do not touch terminals.
func connectFiles(t *testing.T) (*mcp.ClientSession, *logBuffer, string) {
	t.Helper()
	home := testHome(t)
	controller, err := tmux.New("test-ratd-files")
	if err != nil {
		t.Fatal(err)
	}
	session, logs := connectDaemon(t, controller, terminalsHold)
	return session, logs, home
}

// expectFile fails the test if the file at path does not hold content, with mode.
func expectFile(t *testing.T, path, content string, mode fs.FileMode) {
	t.Helper()
	data, err := os.ReadFile(path)
	if err != nil || string(data) != content {
		t.Errorf("%s: expected %q, got %q, %v", path, content, data, err)
	}
	if info, err := os.Stat(path); err != nil || info.Mode().Perm() != mode {
		t.Errorf("%s: expected mode %v, got %v, %v", path, mode, info.Mode().Perm(), err)
	}
}

// umask returns the umask of the process, which the modes of new files and directories go through.
func umask() fs.FileMode {
	mask := syscall.Umask(0)
	syscall.Umask(mask)
	return fs.FileMode(mask)
}

// TestWriteFile guards writing files: created with the exact content, nothing added, and a mode
// going through the umask; replaced in place, keeping its mode, with its former size told; missing
// directories created and named; a symbolic link followed, writing its target; and the log line
// telling the path and the size, never the content.
func TestWriteFile(t *testing.T) {
	session, logs, home := connectFiles(t)
	path := filepath.Join(home, "notes.txt")
	expectTool(t, session, "write_file", map[string]any{"path": "~/notes.txt", "content": "secret\nno end"}, false,
		"Created "+path+" (13 B).")
	expectFile(t, path, "secret\nno end", 0o644&^umask())
	if err := os.Chmod(path, 0o600); err != nil {
		t.Fatal(err)
	}
	if text := expectTool(t, session, "write_file", map[string]any{"path": path, "content": ""}, false,
		"Replaced "+path+" (0 B, 13 B before)."); strings.Contains(text, "directories") {
		t.Errorf("expected no directory created, got %q", text)
	}
	expectFile(t, path, "", 0o600)

	nested := filepath.Join(home, "prj", "nwe", "sub", "a.txt")
	expectTool(t, session, "write_file", map[string]any{"path": "~/prj/../prj/nwe/sub/a.txt", "content": "a"}, false,
		"Created "+nested+" (1 B). Missing directories created: "+filepath.Join(home, "prj")+", "+
			filepath.Join(home, "prj", "nwe")+", "+filepath.Join(home, "prj", "nwe", "sub")+
			". Check the path if you did not mean them.")
	if info, err := os.Stat(filepath.Dir(nested)); err != nil || info.Mode().Perm() != 0o755&^umask() {
		t.Errorf("expected the directories with mode %v, got %v, %v", 0o755&^umask(), info, err)
	}

	link := filepath.Join(home, "link")
	if err := os.Symlink(path, link); err != nil {
		t.Fatal(err)
	}
	expectTool(t, session, "write_file", map[string]any{"path": link, "content": "through"}, false, "Replaced "+link)
	expectFile(t, path, "through", 0o600)
	if info, err := os.Lstat(link); err != nil || info.Mode()&fs.ModeSymlink == 0 {
		t.Errorf("expected the link kept, got %v, %v", info, err)
	}

	out := logs.String()
	if !strings.Contains(out, "tool=write_file outcome=ok") || !strings.Contains(out, "path="+path+" bytes=13") ||
		!strings.Contains(out, "created=true directories_created=0") {
		t.Errorf("expected the path and size to be logged:\n%s", out)
	}
	if strings.Contains(out, "secret") {
		t.Errorf("the content was logged:\n%s", out)
	}
}

// TestCopyHints guards the texts pointing to a copy by path, which keeps a file out of the context
// of the agent: the descriptions of write_file and read_file, telling what they do with that
// context, and the instructions hold the generic copy hints, once each, which rat finds to put the
// sentences naming its tools in their place.
func TestCopyHints(t *testing.T) {
	session, _, _ := connectFiles(t)
	list, err := session.ListTools(context.Background(), nil)
	if err != nil {
		t.Fatal(err)
	}
	texts := map[string]string{"instructions": session.InitializeResult().Instructions}
	for _, tool := range list.Tools {
		texts[tool.Name] = tool.Description
	}
	for name, hint := range map[string]string{
		tools.WriteFile: tools.WriteFileCopyHint, tools.ReadFile: tools.ReadFileCopyHint,
		"instructions": tools.InstructionsCopyHint,
	} {
		if strings.Count(texts[name], hint) != 1 {
			t.Errorf("%s: expected the copy hint %q once, got %q", name, hint, texts[name])
		}
	}
	if !strings.Contains(texts[tools.WriteFile], "from your context") || !strings.Contains(texts[tools.ReadFile],
		"into your context") {
		t.Errorf("expected write_file and read_file to tell what they do with the context of the agent, got %q and %q",
			texts[tools.WriteFile], texts[tools.ReadFile])
	}
}

// TestPathRefusalNoHome guards a path starting with ~ when the home directory of ratd's user is
// unknown (HOME unset): told that ~ can not be resolved, with why, rather than told relative.
func TestPathRefusalNoHome(t *testing.T) {
	t.Setenv("HOME", "")
	_, err := absolutePath("~/notes.txt")
	if err == nil {
		t.Fatal("expected ~/notes.txt refused without a home directory")
	}
	text := pathRefusal("~/notes.txt", err)
	if !strings.HasPrefix(text, `Path "~/notes.txt" starts with ~, which can not be resolved: `) ||
		!strings.HasSuffix(text, ". Give an absolute path.") {
		t.Errorf("expected ~ told unresolved, got %q", text)
	}
}

// TestWriteFileRefused guards what write_file does not write, telling why: a relative path, a
// path holding a NUL byte (no directory created for it), a path naming a directory (a final
// slash), a name too long under a directory it
// created (which it names), a directory, a FIFO (without blocking on it), a device, a path under a
// file, a denied permission naming the user, and a write failing midway, leaving the file partial
// (here over the file size limit of the process, which Go makes a failed write rather than a
// signal).
func TestWriteFileRefused(t *testing.T) {
	session, _, home := connectFiles(t)
	expectTool(t, session, "write_file", map[string]any{"path": "notes.txt", "content": "x"}, true,
		`Path "notes.txt" is relative: give an absolute path, or one starting with ~/`)
	nul := filepath.Join(home, "nul", "a\x00b")
	expectTool(t, session, "write_file", map[string]any{"path": nul, "content": "x"}, true,
		"Path "+strconv.Quote(nul)+" holds a NUL byte, which no file name can hold.")
	if _, err := os.Stat(filepath.Dir(nul)); err == nil {
		t.Error("NUL in path: expected no directory created for it")
	}
	// a path naming a directory, which cleaning would make the name of a file
	expectTool(t, session, "write_file", map[string]any{"path": "~/notes/", "content": "x"}, true,
		`Path "~/notes/" names a directory: give the name of the file to write in it.`)
	if _, err := os.Lstat(filepath.Join(home, "notes")); err == nil {
		t.Error("naming a directory: expected no file written in its place")
	}
	// a name too long under a directory to create: the directory, left, named
	tooLong := filepath.Join(home, "new", strings.Repeat("a", 300))
	expectTool(t, session, "write_file", map[string]any{"path": tooLong, "content": "x"}, true,
		"Writing "+tooLong+" failed: file name too long. Missing directories created: "+filepath.Join(home, "new")+
			". Check the path if you did not mean them.")
	expectTool(t, session, "write_file", map[string]any{"path": "~", "content": "x"}, true, home+" is a directory, not a file.")

	fifo := filepath.Join(home, "fifo")
	if err := syscall.Mkfifo(fifo, 0o600); err != nil {
		t.Fatal(err)
	}
	expectTool(t, session, "write_file", map[string]any{"path": fifo, "content": "x"}, true,
		fifo+" is not a regular file (a FIFO): write_file only writes regular files.")
	expectTool(t, session, "write_file", map[string]any{"path": "/dev/null", "content": "x"}, true,
		"/dev/null is not a regular file (a device)")

	file := filepath.Join(home, "file")
	if err := os.WriteFile(file, []byte("kept"), 0o600); err != nil {
		t.Fatal(err)
	}
	expectTool(t, session, "write_file", map[string]any{"path": file + "/a/b.txt", "content": "x"}, true,
		file+" is not a directory: "+file+"/a can not be created in it.")
	expectFile(t, file, "kept", 0o600)

	locked := filepath.Join(home, "locked")
	if err := os.Mkdir(locked, 0o500); err != nil {
		t.Fatal(err)
	}
	me, err := user.Current()
	if err != nil {
		t.Fatal(err)
	}
	// no permission is denied to root
	if os.Geteuid() != 0 {
		expectTool(t, session, "write_file", map[string]any{"path": locked + "/a.txt", "content": "x"}, true,
			"Permission denied: "+locked+"/a.txt can not be written by "+me.Username+", the user the terminals run as.")
	}

	var limit syscall.Rlimit
	if err = syscall.Getrlimit(syscall.RLIMIT_FSIZE, &limit); err != nil {
		t.Fatal(err)
	}
	small := limit
	small.Cur = 4
	if err = syscall.Setrlimit(syscall.RLIMIT_FSIZE, &small); err != nil {
		t.Fatal(err)
	}
	text, isError := callTool(t, session, "write_file", map[string]any{"path": file, "content": "longer than the limit"})
	if err = syscall.Setrlimit(syscall.RLIMIT_FSIZE, &limit); err != nil {
		t.Fatal(err)
	}
	if !isError || text != "Writing "+file+" failed: file too large. The file may be partial: write it again once fixed." {
		t.Errorf("expected a partial write, got %v: %s", isError, text)
	}
}

// TestOpenRegular guards opening, which may not open the file checked before: a FIFO without a
// reader fails at once rather than blocking, and one with a reader opens, then is refused.
func TestOpenRegular(t *testing.T) {
	fifo := filepath.Join(t.TempDir(), "fifo")
	if err := syscall.Mkfifo(fifo, 0o600); err != nil {
		t.Fatal(err)
	}
	opened := make(chan error, 1)
	go func() {
		_, _, err := openRegular(fifo)
		opened <- err
	}()
	select {
	case err := <-opened:
		if err == nil {
			t.Error("FIFO without a reader: expected an error")
		}
	case <-time.After(2 * time.Second):
		t.Fatal("opening a FIFO without a reader blocked")
	}
	reader, err := os.OpenFile(fifo, os.O_RDONLY|syscall.O_NONBLOCK, 0)
	if err != nil {
		t.Fatal(err)
	}
	defer reader.Close()
	var notRegular *notRegularError
	if _, _, err = openRegular(fifo); !errors.As(err, &notRegular) {
		t.Errorf("expected a notRegularError, got %v", err)
	}
}

// TestWriteFileTimeout guards a file system not answering (a stale NFS mount): the call returns at
// the end of its time, telling the file may still be written, and ratd logs a warning.
func TestWriteFileTimeout(t *testing.T) {
	session, logs, home := connectFiles(t)
	timeout, writer := toolTimeout, fileWriter
	toolTimeout = 200 * time.Millisecond
	// The call returns without waiting for the goroutine writing the file, which reads fileWriter
	// on its own: restoring the hook is ordered after that read by called, or it races.
	stuck, called := make(chan struct{}), make(chan struct{})
	fileWriter = func(*daemon, string, string) result {
		close(called)
		<-stuck
		return result{}
	}
	t.Cleanup(func() {
		close(stuck)
		<-called
		toolTimeout, fileWriter = timeout, writer
	})
	path := filepath.Join(home, "x")
	expectTool(t, session, "write_file", map[string]any{"path": "~/x", "content": "x"}, true,
		"The file system did not answer in time: "+path+" may still be written, partially or not. Check it before "+
			"writing again.")
	if !strings.Contains(logs.String(), "level=WARN msg=tool session=alice remote="+toolsRemote+" tool=write_file outcome=error") {
		t.Errorf("expected a warning:\n%s", logs)
	}
}

// TestWriteFileWaitsForWriteEnding guards write_file into a file a write in place is still writing
// into (G7: an upload canceled, going on with the bytes already sent): it waits for that write to
// end, its last bytes written, then the file holds exactly its content, rather than torn, the late
// bytes of the other left after it. The lock of the file is dropped once both ended.
func TestWriteFileWaitsForWriteEnding(t *testing.T) {
	waiting := waitingLocks(t)
	d := &daemon{user: "u"}
	path := filepath.Join(t.TempDir(), "app.conf")
	if err := os.WriteFile(path, []byte("old"), 0o644); err != nil {
		t.Fatal(err)
	}
	info, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}
	// the write still going on, holding the lock of the file
	release, err := d.inPlace.lock(path, info, nil)
	if err != nil {
		t.Fatal(err)
	}
	written := make(chan result, 1)
	go func() { written <- d.writeFileNow(path, "new") }()
	expectWaiting(t, waiting, path)
	// an answer coming now is kept for the end: the late bytes below then tear the file
	var r result
	answered := false
	select {
	case r = <-written:
		answered = true
		t.Error("expected write_file waiting for the write under way, it wrote meanwhile")
	case <-time.After(500 * time.Millisecond):
	}
	// the write ending: its last bytes, written where it was
	late, err := os.OpenFile(path, os.O_WRONLY|os.O_APPEND, 0)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := late.WriteString(" and late bytes"); err != nil {
		t.Fatal(err)
	}
	if err := late.Close(); err != nil {
		t.Fatal(err)
	}
	release()
	if !answered {
		r = <-written
	}
	if r.outcome == failed || !strings.HasPrefix(r.text, "Replaced "+path+" (3 B,") {
		t.Errorf("expected the file replaced once the write ended, got %v: %s", r.err, r.text)
	}
	expectFile(t, path, "new", 0o644)
	if n := len(d.inPlace.files); n != 0 {
		t.Errorf("expected no lock left once the writes ended, got %d", n)
	}
}
