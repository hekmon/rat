package main

import (
	"bytes"
	"context"
	"crypto/rand"
	"errors"
	"io"
	"io/fs"
	"log/slog"
	"net/http"
	"os"
	"os/user"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"
	"sync/atomic"
	"syscall"
	"testing"
	"time"

	"github.com/hekmon/rat/connect"
	"github.com/hekmon/rat/mtls"
)

// transferRatd serves a ratd for transfer tests, with a fresh home, and returns it with an HTTP
// client of alice built as rat builds it (connect.HTTPClient: HTTP/1.1, waiting for the go-ahead
// of ratd before sending a body when asked to), and the home.
func transferRatd(t *testing.T) (*servedRatd, *http.Client, string) {
	t.Helper()
	home := testHome(t)
	bundle := newBundle(t, "t")
	r := serveRatd(t, bundle, slog.LevelInfo)
	side, err := mtls.Load(mtls.ClientDir(bundle, "alice"))
	if err != nil {
		t.Fatal(err)
	}
	client, err := connect.HTTPClient(side)
	if err != nil {
		t.Fatal(err)
	}
	client.Timeout = 10 * time.Second
	return r, client, home
}

// fileBody is the body of an upload: content, counting what the HTTP client took of it. With
// failAt, it fails once that many bytes are taken, as rat does when its file changed; atEnd is
// called once the content is all taken, before its end is told.
type fileBody struct {
	content []byte
	failAt  int
	atEnd   func()
	taken   atomic.Int64
}

func (b *fileBody) Read(p []byte) (int, error) {
	taken := int(b.taken.Load())
	if b.failAt > 0 && taken >= b.failAt {
		return 0, errors.New("the file changed while it was read")
	}
	if taken == len(b.content) {
		if b.atEnd != nil {
			b.atEnd()
			b.atEnd = nil
		}
		return 0, io.EOF
	}
	end := len(b.content)
	if b.failAt > 0 {
		end = min(end, b.failAt)
	}
	n := copy(p, b.content[taken:end])
	b.taken.Add(int64(n))
	return n, nil
}

// newUpload returns the request uploading body to path on the ratd at addr, as rat sends it:
// chunked, waiting for the go-ahead of ratd, with the permission bits mode and overwrite.
func newUpload(t *testing.T, addr, path string, body io.Reader, mode string, overwrite bool) *http.Request {
	t.Helper()
	req, err := http.NewRequest(http.MethodPost, "https://"+addr+connect.UploadPath, body)
	if err != nil {
		t.Fatal(err)
	}
	req.ContentLength = -1
	req.Header.Set("Content-Type", connect.OctetStream)
	req.Header.Set("Expect", "100-continue")
	req.Header.Set(connect.HeaderPath, connect.EncodePath(path))
	req.Header.Set(connect.HeaderMode, mode)
	req.Header.Set(connect.HeaderOverwrite, strconv.FormatBool(overwrite))
	return req
}

// newDownload returns the request downloading path from the ratd at addr.
func newDownload(t *testing.T, addr, path string) *http.Request {
	t.Helper()
	req, err := http.NewRequest(http.MethodGet, "https://"+addr+connect.DownloadPath, nil)
	if err != nil {
		t.Fatal(err)
	}
	req.Header.Set(connect.HeaderPath, connect.EncodePath(path))
	return req
}

// sendTransfer sends req with client, and returns the answer, its body read whole (its trailers
// then read too), and the error of the request.
func sendTransfer(client *http.Client, req *http.Request) (*http.Response, string, error) {
	resp, err := client.Do(req)
	if err != nil {
		return nil, "", err
	}
	defer resp.Body.Close()
	body, err := io.ReadAll(resp.Body)
	return resp, string(body), err
}

// expectRefused fails the test unless resp answers status, with text in its body, and the version
// of ratd.
func expectRefused(t *testing.T, what string, resp *http.Response, body string, err error, status int, text string) {
	t.Helper()
	switch {
	case err != nil:
		t.Errorf("%s: %v", what, err)
	case resp.StatusCode != status || !strings.Contains(body, text):
		t.Errorf("%s: expected %d telling %q, got %s: %q", what, status, text, resp.Status, body)
	case resp.Header.Get(connect.HeaderVersion) == "":
		t.Errorf("%s: expected the version of ratd in the answer", what)
	}
}

// expectNoTemporary fails the test if a temporary file of an upload remains under root.
func expectNoTemporary(t *testing.T, root string) {
	t.Helper()
	_ = filepath.WalkDir(root, func(path string, entry fs.DirEntry, err error) error {
		if err == nil && strings.Contains(entry.Name(), ".rat-") {
			t.Errorf("temporary file left: %s", path)
		}
		return nil
	})
}

// randomContent returns size random bytes: binary, NUL bytes and invalid UTF-8 included.
func randomContent(t *testing.T, size int) []byte {
	t.Helper()
	content := make([]byte, size)
	if _, err := rand.Read(content); err != nil {
		t.Fatal(err)
	}
	return content
}

// TestUpload guards uploads: a new file created with exactly the bytes sent, binary included, its
// permission bits those sent minus the umask, told back, its missing directories created and
// named; an existing file refused without overwrite, and replaced with it, keeping its own mode,
// told back; the version of ratd in every answer; no temporary file left; and one log line per
// upload telling the path and the size, never the content.
func TestUpload(t *testing.T) {
	r, client, home := transferRatd(t)
	content := append(randomContent(t, 300<<10), "secret"...)
	path := filepath.Join(home, "dir", "sub", "app.bin")
	resp, body, err := sendTransfer(client, newUpload(t, r.addr, "~/dir/sub/app.bin", &fileBody{content: content}, "0754", false))
	if err != nil || resp.StatusCode != http.StatusCreated {
		t.Fatalf("expected 201, got %v, %v: %s", resp, err, body)
	}
	created := "Missing directories created: " + filepath.Join(home, "dir") + ", " + filepath.Join(home, "dir", "sub") +
		". Check the path if you did not mean them."
	mode := 0o754 &^ umask()
	if body != created || resp.Header.Get(connect.HeaderMode) != connect.FormatMode(mode) ||
		resp.Header.Get(connect.HeaderVersion) == "" {
		t.Errorf("expected the directories created, the mode %v and the version, got %q, %v", mode, body, resp.Header)
	}
	expectFile(t, path, string(content), mode)

	resp, body, err = sendTransfer(client, newUpload(t, r.addr, path, &fileBody{content: []byte("other")}, "0644", false))
	expectRefused(t, "existing file", resp, body, err, http.StatusPreconditionFailed, path+" exists: set overwrite to replace it.")
	expectFile(t, path, string(content), mode)

	if err = os.Chmod(path, 0o600); err != nil {
		t.Fatal(err)
	}
	resp, body, err = sendTransfer(client, newUpload(t, r.addr, path, &fileBody{content: []byte("replaced")}, "0755", true))
	if err != nil || resp.StatusCode != http.StatusNoContent || body != "" || resp.Header.Get(connect.HeaderMode) != "0600" {
		t.Errorf("expected 204 telling the mode kept, 0600, got %v, %v: %q", resp, err, body)
	}
	expectFile(t, path, "replaced", 0o600)
	expectNoTemporary(t, home)

	logged := "path=" + path + " bytes=8 replaced=true directories_created=0 status=204"
	eventually(t, "the replacing upload logged", func() bool { return strings.Contains(r.logs.String(), logged) })
	out := r.logs.String()
	if !strings.Contains(out, "msg=transfer session=alice remote=127.0.0.1:") ||
		!strings.Contains(out, "direction=upload outcome=ok") ||
		!strings.Contains(out, "path="+path+" bytes="+strconv.Itoa(len(content))+" replaced=false directories_created=2 status=201") {
		t.Errorf("expected each upload logged with its session, address, path and size:\n%s", out)
	}
	if strings.Contains(out, "secret") {
		t.Errorf("the content was logged:\n%s", out)
	}
}

// TestUploadLongName guards an upload to a file whose name is longer than its temporary name takes,
// a character across byte 200 (see package tempname): created, no temporary file left, rather than
// refused on macOS, whose APFS takes UTF-8 names only (EILSEQ).
func TestUploadLongName(t *testing.T) {
	r, client, home := transferRatd(t)
	path := filepath.Join(home, strings.Repeat("a", 199)+"é.bin")
	resp, body, err := sendTransfer(client, newUpload(t, r.addr, path, &fileBody{content: []byte("data")}, "0644", false))
	if err != nil || resp.StatusCode != http.StatusCreated {
		t.Fatalf("expected 201, got %v, %v: %s", resp, err, body)
	}
	expectFile(t, path, "data", 0o644&^umask())
	expectNoTemporary(t, home)
}

// TestUploadNameNotUTF8 guards an upload to a name that is not UTF-8, which APFS refuses (EILSEQ):
// refused 400, a mistake of the client, before any byte is sent, no temporary file left. Linux
// takes any byte in a name but / and NUL: the test runs on macOS alone.
func TestUploadNameNotUTF8(t *testing.T) {
	if runtime.GOOS != "darwin" {
		t.Skip("only APFS refuses a name that is not UTF-8")
	}
	r, client, home := transferRatd(t)
	path := filepath.Join(home, "\xffname")
	body := &fileBody{content: bytes.Repeat([]byte("x"), 1<<20)}
	resp, text, err := sendTransfer(client, newUpload(t, r.addr, path, body, "0644", false))
	expectRefused(t, "name not UTF-8", resp, text, err, http.StatusBadRequest,
		"Writing "+path+" failed: illegal byte sequence.")
	if taken := body.taken.Load(); taken != 0 {
		t.Errorf("expected the refusal before any byte was sent, %d were", taken)
	}
	expectNoTemporary(t, home)
}

// TestUploadRefused guards what ratd refuses from the headers of an upload alone, telling why,
// before the client sends any byte: a missing, undecodable or relative path, permission bits or an
// overwrite it can not read, a body declared other than bytes, an existing file without
// overwrite, a directory, a FIFO, a symbolic link (telling where it leads, even nowhere), a path
// under a file, a denied permission naming the user, and a name too long, though its temporary
// file, named to fit, could take the bytes, even under a directory to create, which it names. The
// file existing stays as it was, and no temporary file is left. The endpoints answer their method
// only.
func TestUploadRefused(t *testing.T) {
	r, client, home := transferRatd(t)
	file := filepath.Join(home, "file")
	if err := os.WriteFile(file, []byte("kept"), 0o600); err != nil {
		t.Fatal(err)
	}
	fifo := filepath.Join(home, "fifo")
	if err := syscall.Mkfifo(fifo, 0o600); err != nil {
		t.Fatal(err)
	}
	link, dangling := filepath.Join(home, "link"), filepath.Join(home, "dangling")
	if err := os.Symlink(file, link); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink("missing", dangling); err != nil {
		t.Fatal(err)
	}
	locked := filepath.Join(home, "locked")
	if err := os.Mkdir(locked, 0o500); err != nil {
		t.Fatal(err)
	}
	// longer than any file system takes, unlike its temporary name, cut to fit (see package
	// tempname), and under a directory to create, which a look at the path stops at
	tooLong := filepath.Join(home, strings.Repeat("a", 300))
	tooLongUnder := filepath.Join(home, "new", strings.Repeat("a", 300))
	me, err := user.Current()
	if err != nil {
		t.Fatal(err)
	}
	header := func(name, value string) func(*http.Request) {
		return func(req *http.Request) { req.Header.Set(name, value) }
	}
	for _, tc := range []struct {
		what      string
		path      string
		overwrite bool
		change    func(*http.Request)
		status    int
		text      string
	}{
		{"relative path", "file", false, nil, http.StatusBadRequest,
			`Path "file" is relative: give an absolute path, or one starting with ~/`},
		{"no path", "~/new", false, func(req *http.Request) { req.Header.Del(connect.HeaderPath) }, http.StatusBadRequest,
			"Rat-Path is missing"},
		{"path not encoded", "~/new", false, header(connect.HeaderPath, "/x%zz"), http.StatusBadRequest,
			`invalid header Rat-Path "/x%zz": not a percent-encoded path.`},
		{"mode not octal", "~/new", false, header(connect.HeaderMode, "rwx"), http.StatusBadRequest,
			`invalid header Rat-Mode "rwx": expected permission bits in octal, such as 0644.`},
		{"mode beyond permission bits", "~/new", false, header(connect.HeaderMode, "4755"), http.StatusBadRequest,
			`invalid header Rat-Mode "4755"`},
		{"overwrite not a boolean", "~/new", false, header(connect.HeaderOverwrite, "yes"), http.StatusBadRequest,
			`invalid header Rat-Overwrite "yes": expected true or false.`},
		{"a form", "~/new", false, header("Content-Type", "application/x-www-form-urlencoded"),
			http.StatusUnsupportedMediaType,
			`Content-Type "application/x-www-form-urlencoded": files travel as application/octet-stream.`},
		{"existing file", file, false, nil, http.StatusPreconditionFailed, file + " exists: set overwrite to replace it."},
		{"directory", "~", true, nil, http.StatusConflict, home + " is a directory, not a file."},
		{"FIFO", fifo, true, nil, http.StatusConflict, fifo + " is not a regular file (a FIFO): transfers only copy regular files."},
		{"symbolic link", link, true, nil, http.StatusConflict,
			link + " is a symbolic link to " + file + ", which transfers do not follow: use that path instead."},
		{"dangling link", dangling, true, nil, http.StatusConflict, dangling + " is a symbolic link to " +
			filepath.Join(home, "missing") + ", which does not exist: transfers do not follow links, upload to that path " +
			"to create the file."},
		{"under a file", file + "/a/b.txt", false, nil, http.StatusConflict,
			file + " is not a directory: " + file + "/a can not be created in it."},
		{"permission denied", locked + "/a.txt", false, nil, http.StatusForbidden,
			"Permission denied: " + locked + "/a.txt can not be written by " + me.Username + ", the user the terminals run as."},
		{"name too long", tooLong, false, nil, http.StatusBadRequest,
			"Writing " + tooLong + " failed: file name too long."},
		{"name too long under a directory to create", tooLongUnder, false, nil, http.StatusBadRequest,
			"Writing " + tooLongUnder + " failed: file name too long. Missing directories created: " +
				filepath.Join(home, "new") + ". Check the path if you did not mean them."},
	} {
		// no permission is denied to root
		if tc.status == http.StatusForbidden && os.Geteuid() == 0 {
			continue
		}
		body := &fileBody{content: bytes.Repeat([]byte("x"), 1<<20)}
		req := newUpload(t, r.addr, tc.path, body, "0644", tc.overwrite)
		if tc.change != nil {
			tc.change(req)
		}
		resp, text, err := sendTransfer(client, req)
		expectRefused(t, tc.what, resp, text, err, tc.status, tc.text)
		if taken := body.taken.Load(); taken != 0 {
			t.Errorf("%s: expected the refusal before any byte was sent, %d were", tc.what, taken)
		}
	}
	expectFile(t, file, "kept", 0o600)
	expectNoTemporary(t, home)

	for _, wrong := range []struct{ method, path string }{
		{http.MethodGet, connect.UploadPath}, {http.MethodPost, connect.DownloadPath},
	} {
		req, err := http.NewRequest(wrong.method, "https://"+r.addr+wrong.path, nil)
		if err != nil {
			t.Fatal(err)
		}
		if resp, _, err := sendTransfer(client, req); err != nil || resp.StatusCode != http.StatusMethodNotAllowed {
			t.Errorf("%s %s: expected 405, got %v, %v", wrong.method, wrong.path, resp, err)
		}
	}
}

// TestUploadCut guards an upload whose body is cut before its end (rat failing it, its file
// having changed, or the connection lost): nothing is written, the file it was to replace stays as
// it was, no temporary file is left, and the upload is logged as failed.
func TestUploadCut(t *testing.T) {
	r, client, home := transferRatd(t)
	path := filepath.Join(home, "app.bin")
	if err := os.WriteFile(path, []byte("previous"), 0o600); err != nil {
		t.Fatal(err)
	}
	body := &fileBody{content: bytes.Repeat([]byte("x"), 1<<20), failAt: 256 << 10}
	if _, _, err := sendTransfer(client, newUpload(t, r.addr, path, body, "0644", true)); err == nil {
		t.Fatal("expected the upload to fail")
	}
	eventually(t, "the cut upload logged", func() bool {
		return strings.Contains(r.logs.String(), "direction=upload outcome=error")
	})
	if !strings.Contains(r.logs.String(), "path="+path+" bytes=") {
		t.Errorf("expected the path of the cut upload logged:\n%s", r.logs)
	}
	expectFile(t, path, "previous", 0o600)
	expectNoTemporary(t, home)
}

// TestUploadAppeared guards a file appearing at the path of an upload while its bytes are sent:
// without overwrite, it is not replaced (412), where a rename would have; with overwrite, it is,
// keeping its mode.
func TestUploadAppeared(t *testing.T) {
	r, client, home := transferRatd(t)
	path := filepath.Join(home, "app.bin")
	appear := func() {
		if err := os.WriteFile(path, []byte("appeared"), 0o600); err != nil {
			t.Error(err)
		}
	}
	resp, body, err := sendTransfer(client, newUpload(t, r.addr, path, &fileBody{content: []byte("uploaded"), atEnd: appear},
		"0644", false))
	expectRefused(t, "appeared without overwrite", resp, body, err, http.StatusPreconditionFailed,
		path+" exists: set overwrite to replace it.")
	expectFile(t, path, "appeared", 0o600)
	if err = os.Remove(path); err != nil {
		t.Fatal(err)
	}
	resp, _, err = sendTransfer(client, newUpload(t, r.addr, path, &fileBody{content: []byte("uploaded"), atEnd: appear},
		"0644", true))
	if err != nil || resp.StatusCode != http.StatusNoContent || resp.Header.Get(connect.HeaderMode) != "0600" {
		t.Errorf("appeared with overwrite: expected 204 keeping its mode, got %v, %v", resp, err)
	}
	expectFile(t, path, "uploaded", 0o600)
	expectNoTemporary(t, home)
}

// TestUploadWithoutHardLinks guards an upload to a file system without hard links, whatever the
// error its link gives (FAT, some network ones, FUSE without link as sshfs): the file is renamed
// into place instead.
func TestUploadWithoutHardLinks(t *testing.T) {
	var linkErr atomic.Value
	// set before ratd serves, and restored once it stopped (cleanups run in reverse order)
	link := linkFile
	linkFile = func(oldname, newname string) error {
		return &os.LinkError{Op: "link", Old: oldname, New: newname, Err: linkErr.Load().(error)}
	}
	t.Cleanup(func() { linkFile = link })
	r, client, home := transferRatd(t)
	// ENOTSUP and EOPNOTSUPP are one on Linux: each file is named after its rank
	for i, errno := range []syscall.Errno{syscall.EPERM, syscall.EMLINK, syscall.ENOTSUP, syscall.EOPNOTSUPP,
		syscall.ENOSYS} {
		linkErr.Store(error(errno))
		path := filepath.Join(home, "app-"+strconv.Itoa(i)+".bin")
		resp, body, err := sendTransfer(client, newUpload(t, r.addr, path, &fileBody{content: []byte("renamed")}, "0644",
			false))
		if err != nil || resp.StatusCode != http.StatusCreated {
			t.Errorf("link failing with %v: expected 201, got %v, %v: %s", errno, resp, err, body)
			continue
		}
		expectFile(t, path, "renamed", 0o644&^umask())
	}
	expectNoTemporary(t, home)
}

// TestUploadMovedAtOnce guards an upload whose file is moved as soon as it is placed, as a watcher
// of a spool directory does: ratd still answers 201 with the mode it wrote the file with
// (Rat-Mode), which it set, rather than look at the path again, where the file is gone.
func TestUploadMovedAtOnce(t *testing.T) {
	// set before ratd serves, and restored once it stopped (cleanups run in reverse order)
	link := linkFile
	linkFile = func(oldname, newname string) error {
		if err := os.Link(oldname, newname); err != nil {
			return err
		}
		return os.Rename(newname, newname+".spooled")
	}
	t.Cleanup(func() { linkFile = link })
	r, client, home := transferRatd(t)
	path := filepath.Join(home, "app.bin")
	resp, _, err := sendTransfer(client, newUpload(t, r.addr, path, &fileBody{content: []byte("spooled")}, "0640", false))
	mode := connect.FormatMode(0o640 &^ umask())
	if err != nil || resp.StatusCode != http.StatusCreated || resp.Header.Get(connect.HeaderMode) != mode {
		t.Errorf("expected 201 telling the mode %s, got %v, %v", mode, resp, err)
	}
	expectFile(t, path+".spooled", "spooled", 0o640&^umask())
	expectNoTemporary(t, home)
}

// TestUploadWriteFailed guards an upload failing to write (no space left, here over the file size
// limit of the process, which Go makes a failed write rather than a signal): 507 telling why, the
// file it was to replace kept as it was, and no temporary file left.
func TestUploadWriteFailed(t *testing.T) {
	r, client, home := transferRatd(t)
	path := filepath.Join(home, "app.bin")
	if err := os.WriteFile(path, []byte("previous"), 0o600); err != nil {
		t.Fatal(err)
	}
	var limit syscall.Rlimit
	if err := syscall.Getrlimit(syscall.RLIMIT_FSIZE, &limit); err != nil {
		t.Fatal(err)
	}
	small := limit
	small.Cur = 4
	if err := syscall.Setrlimit(syscall.RLIMIT_FSIZE, &small); err != nil {
		t.Fatal(err)
	}
	resp, body, err := sendTransfer(client, newUpload(t, r.addr, path, &fileBody{content: bytes.Repeat([]byte("x"), 64<<10)},
		"0644", true))
	if err := syscall.Setrlimit(syscall.RLIMIT_FSIZE, &limit); err != nil {
		t.Fatal(err)
	}
	expectRefused(t, "write failed", resp, body, err, http.StatusInsufficientStorage, "Writing "+path+" failed: file too large.")
	expectFile(t, path, "previous", 0o600)
	expectNoTemporary(t, home)
}

// TestDownload guards downloads: exactly the bytes of the file, binary included, its permission
// bits and size told in headers, the body chunked and complete, without the Rat-Error trailer;
// HEAD telling the headers alone; and one log line per download telling the path and the size.
func TestDownload(t *testing.T) {
	r, client, home := transferRatd(t)
	content := randomContent(t, 1<<20)
	path := filepath.Join(home, "app.bin")
	if err := os.WriteFile(path, content, 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(path, 0o750); err != nil {
		t.Fatal(err)
	}
	resp, body, err := sendTransfer(client, newDownload(t, r.addr, "~/app.bin"))
	if err != nil || resp.StatusCode != http.StatusOK || body != string(content) {
		t.Fatalf("expected 200 with the content, got %v, %v (%d bytes)", resp, err, len(body))
	}
	header := resp.Header
	if header.Get("Content-Type") != connect.OctetStream || header.Get(connect.HeaderMode) != "0750" ||
		header.Get(connect.HeaderSize) != strconv.Itoa(len(content)) || header.Get(connect.HeaderVersion) == "" {
		t.Errorf("expected the type, mode, size and version, got %v", header)
	}
	if len(resp.TransferEncoding) != 1 || resp.TransferEncoding[0] != "chunked" || resp.Trailer.Get(connect.HeaderError) != "" {
		t.Errorf("expected a chunked body without Rat-Error, got %v, trailers %v", resp.TransferEncoding, resp.Trailer)
	}

	req := newDownload(t, r.addr, path)
	req.Method = http.MethodHead
	resp, body, err = sendTransfer(client, req)
	if err != nil || resp.StatusCode != http.StatusOK || body != "" || resp.Header.Get(connect.HeaderSize) != strconv.Itoa(len(content)) {
		t.Errorf("HEAD: expected 200 with the size and no body, got %v, %v: %d bytes", resp, err, len(body))
	}
	logged := "direction=download outcome=ok"
	eventually(t, "the downloads logged", func() bool { return strings.Count(r.logs.String(), logged) == 2 })
	if !strings.Contains(r.logs.String(), "path="+path+" status=200 bytes="+strconv.Itoa(len(content))) {
		t.Errorf("expected the path and size of the download logged:\n%s", r.logs)
	}
}

// TestDownloadRefused guards what ratd refuses to download, telling why: a relative path, a
// missing file, a directory (pointing to an archive), a FIFO (without blocking on it), a device, a
// symbolic link (telling where it leads, even nowhere), a denied permission naming the user, and a
// name too long, a mistake of the client (400) rather than of ratd.
func TestDownloadRefused(t *testing.T) {
	r, client, home := transferRatd(t)
	file := filepath.Join(home, "file")
	if err := os.WriteFile(file, []byte("x"), 0o600); err != nil {
		t.Fatal(err)
	}
	fifo := filepath.Join(home, "fifo")
	if err := syscall.Mkfifo(fifo, 0o600); err != nil {
		t.Fatal(err)
	}
	link, dangling := filepath.Join(home, "link"), filepath.Join(home, "dangling")
	if err := os.Symlink(file, link); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink("missing", dangling); err != nil {
		t.Fatal(err)
	}
	locked := filepath.Join(home, "locked")
	if err := os.WriteFile(locked, []byte("x"), 0o000); err != nil {
		t.Fatal(err)
	}
	me, err := user.Current()
	if err != nil {
		t.Fatal(err)
	}
	missing := filepath.Join(home, "missing")
	tooLong := filepath.Join(home, strings.Repeat("a", 300))
	for _, tc := range []struct {
		what, path string
		status     int
		text       string
	}{
		{"relative path", "file", http.StatusBadRequest, `Path "file" is relative`},
		{"missing file", missing, http.StatusNotFound, "Reading " + missing + " failed: no such file or directory."},
		{"directory", "~", http.StatusConflict, home + " is a directory, not a file: transfers copy one file, archive it first (tar)."},
		{"FIFO", fifo, http.StatusConflict, fifo + " is not a regular file (a FIFO): transfers only copy regular files."},
		{"device", "/dev/null", http.StatusConflict, "/dev/null is not a regular file (a device)"},
		{"symbolic link", link, http.StatusConflict,
			link + " is a symbolic link to " + file + ", which transfers do not follow: use that path instead."},
		{"dangling link", dangling, http.StatusConflict,
			dangling + " is a symbolic link to " + missing + ", which does not exist: there is no file to download."},
		{"permission denied", locked, http.StatusForbidden,
			"Permission denied: " + locked + " can not be read by " + me.Username + ", the user the terminals run as."},
		{"name too long", tooLong, http.StatusBadRequest, "Reading " + tooLong + " failed: file name too long."},
	} {
		// no permission is denied to root
		if tc.status == http.StatusForbidden && os.Geteuid() == 0 {
			continue
		}
		resp, body, err := sendTransfer(client, newDownload(t, r.addr, tc.path))
		expectRefused(t, tc.what, resp, body, err, tc.status, tc.text)
	}
}

// TestDownloadLooksFirst guards that a download looks at its path before opening it: a FIFO is
// refused without being opened, by GET and HEAD alike, so that a command waiting to write into it
// (tar -cf - . > fifo) keeps waiting. Opened first, without blocking, the FIFO released the
// command, whose next write then failed with a broken pipe, the refusal having closed it.
func TestDownloadLooksFirst(t *testing.T) {
	r, client, home := transferRatd(t)
	fifo := filepath.Join(home, "fifo")
	if err := syscall.Mkfifo(fifo, 0o600); err != nil {
		t.Fatal(err)
	}
	opened := make(chan error, 1)
	go func() {
		writer, err := os.OpenFile(fifo, os.O_WRONLY, 0)
		if err == nil {
			writer.Close()
		}
		opened <- err
	}()
	// time for the command to wait in open, the case guarded
	time.Sleep(100 * time.Millisecond)
	for _, method := range []string{http.MethodGet, http.MethodHead} {
		req := newDownload(t, r.addr, fifo)
		req.Method = method
		resp, _, err := sendTransfer(client, req)
		if err != nil || resp.StatusCode != http.StatusConflict {
			t.Errorf("%s: expected 409, got %v, %v", method, resp, err)
		}
	}
	select {
	case err := <-opened:
		t.Errorf("the download opened the FIFO, releasing the command waiting to write into it (open: %v)", err)
	case <-time.After(200 * time.Millisecond):
		// a reader of our own releases it, for the goroutine to end
		reader, err := os.OpenFile(fifo, os.O_RDONLY|syscall.O_NONBLOCK, 0)
		if err != nil {
			t.Fatal(err)
		}
		defer reader.Close()
		if err := <-opened; err != nil {
			t.Error(err)
		}
	}
}

// TestDownloadChanged guards a file changing while it is downloaded (a log still written, a build
// output): grown, shrunk, or rewritten in place with its size kept, the download ends with the
// Rat-Error trailer telling it, which makes it fail, and is logged as failed. A grown file is sent
// as it was opened, up to its size then.
func TestDownloadChanged(t *testing.T) {
	var change atomic.Value
	// set before ratd serves, and restored once it stopped (cleanups run in reverse order)
	opened := downloadOpened
	downloadOpened = func(path string) { change.Load().(func(string))(path) }
	t.Cleanup(func() { downloadOpened = opened })
	r, client, home := transferRatd(t)
	path := filepath.Join(home, "app.log")
	content := bytes.Repeat([]byte("line\n"), 64<<10)
	for _, tc := range []struct {
		what   string
		change func(string)
	}{
		{"grown", func(path string) {
			file, err := os.OpenFile(path, os.O_WRONLY|os.O_APPEND, 0)
			if err == nil {
				_, err = file.WriteString("more\n")
				_ = file.Close()
			}
			if err != nil {
				t.Error(err)
			}
		}},
		{"shrunk", func(path string) {
			if err := os.Truncate(path, 10); err != nil {
				t.Error(err)
			}
		}},
		{"rewritten", func(path string) {
			// past a tick of the clock the system stamps times with
			time.Sleep(50 * time.Millisecond)
			file, err := os.OpenFile(path, os.O_WRONLY, 0)
			if err == nil {
				_, err = file.WriteAt([]byte("LINE\n"), 0)
				_ = file.Close()
			}
			if err != nil {
				t.Error(err)
			}
		}},
	} {
		if err := os.WriteFile(path, content, 0o600); err != nil {
			t.Fatal(err)
		}
		change.Store(tc.change)
		resp, body, err := sendTransfer(client, newDownload(t, r.addr, path))
		if err != nil || resp.StatusCode != http.StatusOK {
			t.Fatalf("%s: expected 200, got %v, %v", tc.what, resp, err)
		}
		if failure := resp.Trailer.Get(connect.HeaderError); failure != path+" changed while it was read: it may still "+
			"be written. Download it again once it is complete." {
			t.Errorf("%s: expected Rat-Error telling the file changed, got %q", tc.what, failure)
		}
		if tc.what == "grown" && body != string(content) {
			t.Errorf("grown: expected the file as opened, got %d bytes", len(body))
		}
	}
	eventually(t, "the changed downloads logged", func() bool {
		return strings.Count(r.logs.String(), `direction=download outcome=error`) == 3
	})
	if !strings.Contains(r.logs.String(), `error="file changed while read"`) {
		t.Errorf("expected the change logged:\n%s", r.logs)
	}
}

// TestDownloadLookFailed guards a download whose look at its file once read fails (fstat, a disk
// failing): told as a read failing, with the reason of the system, rather than as a change the
// file may not have, at each of the three looks: a file read whole and one ending before its size
// in the trailer, one showing no size yet giving bytes before anything is sent.
func TestDownloadLookFailed(t *testing.T) {
	var change atomic.Value
	// set before ratd serves, and restored once it stopped (cleanups run in reverse order)
	opened, stat := downloadOpened, statOnceRead
	downloadOpened = func(path string) { change.Load().(func(string))(path) }
	statOnceRead = func(file *os.File) (fs.FileInfo, error) {
		return nil, &fs.PathError{Op: "fstat", Path: file.Name(), Err: syscall.EIO}
	}
	t.Cleanup(func() { downloadOpened, statOnceRead = opened, stat })
	r, client, home := transferRatd(t)
	path := filepath.Join(home, "app.log")
	failure := "Reading " + path + " failed: input/output error."
	content := bytes.Repeat([]byte("line\n"), 1<<10)
	for _, tc := range []struct {
		what    string
		content []byte
		change  func(string)
	}{
		{"read whole", content, func(string) {}},
		{"ending before its size", content, func(path string) {
			if err := os.Truncate(path, 10); err != nil {
				t.Error(err)
			}
		}},
		{"showing no size", nil, func(path string) {
			if err := os.WriteFile(path, content, 0o600); err != nil {
				t.Error(err)
			}
		}},
	} {
		if err := os.WriteFile(path, tc.content, 0o600); err != nil {
			t.Fatal(err)
		}
		change.Store(tc.change)
		resp, body, err := sendTransfer(client, newDownload(t, r.addr, path))
		if tc.content == nil {
			expectRefused(t, tc.what, resp, body, err, http.StatusInternalServerError, failure)
			continue
		}
		if err != nil || resp.StatusCode != http.StatusOK {
			t.Fatalf("%s: expected 200, got %v, %v", tc.what, resp, err)
		}
		if got := resp.Trailer.Get(connect.HeaderError); got != failure {
			t.Errorf("%s: expected Rat-Error telling the read failing, got %q", tc.what, got)
		}
	}
}

// TestDownloadNoTrueSize guards a file whose size is not that of its content, one the system makes
// as it is read, which a download, sending the size a file had when opened, sent empty told
// complete (/proc, showing 0) or failed as changed (/sys, showing a page): refused, telling to read
// it with read_file or to copy it first, before anything is sent (409) for one showing 0, in the
// trailer for one ending before its size. An empty file stays sent empty, and one filled once
// opened is told changed. The files of the system are Linux's: those cases run there alone.
func TestDownloadNoTrueSize(t *testing.T) {
	var change atomic.Value
	change.Store(func(string) {})
	// set before ratd serves, and restored once it stopped (cleanups run in reverse order)
	opened := downloadOpened
	downloadOpened = func(path string) { change.Load().(func(string))(path) }
	t.Cleanup(func() { downloadOpened = opened })
	r, client, home := transferRatd(t)
	told := func(path string) string {
		return path + " tells a size that is not that of its content: a file the system makes as it is read, as " +
			"in /proc and /sys. Read it with read_file if it is text, or copy it in a terminal (cat " + path +
			" > ~/copy) and download the copy."
	}
	empty := filepath.Join(home, "empty.log")
	t.Run("empty", func(t *testing.T) {
		if err := os.WriteFile(empty, nil, 0o600); err != nil {
			t.Fatal(err)
		}
		resp, body, err := sendTransfer(client, newDownload(t, r.addr, empty))
		if err != nil || resp.StatusCode != http.StatusOK || body != "" || resp.Trailer.Get(connect.HeaderError) != "" {
			t.Errorf("expected the empty file sent empty, got %v, %q, %v", resp, body, err)
		}
	})
	t.Run("filled once opened", func(t *testing.T) {
		if err := os.WriteFile(empty, nil, 0o600); err != nil {
			t.Fatal(err)
		}
		change.Store(func(path string) {
			if err := os.WriteFile(path, []byte("filled\n"), 0o600); err != nil {
				t.Error(err)
			}
		})
		defer change.Store(func(string) {})
		resp, body, err := sendTransfer(client, newDownload(t, r.addr, empty))
		expectRefused(t, "filled once opened", resp, body, err, http.StatusConflict, empty+" changed while it was read")
	})
	t.Run("showing no size", func(t *testing.T) {
		if runtime.GOOS != "linux" {
			t.Skip("/proc is Linux's")
		}
		resp, body, err := sendTransfer(client, newDownload(t, r.addr, "/proc/version"))
		expectRefused(t, "/proc/version", resp, body, err, http.StatusConflict, told("/proc/version"))
	})
	t.Run("showing a page", func(t *testing.T) {
		if runtime.GOOS != "linux" {
			t.Skip("/sys is Linux's")
		}
		// 4096 bytes told, a line of 18 held
		const attribute = "/sys/class/net/lo/address"
		resp, _, err := sendTransfer(client, newDownload(t, r.addr, attribute))
		if err != nil || resp.StatusCode != http.StatusOK || resp.Trailer.Get(connect.HeaderError) != told(attribute) {
			t.Errorf("expected Rat-Error telling its size is not that of its content, got %v, %v", resp, err)
		}
	})
}

// growingFile is a file growing as it is read, as a log still written: once read to its end, a
// line is appended to it.
type growingFile struct {
	*os.File
}

func (f growingFile) Read(p []byte) (int, error) {
	n, err := f.File.Read(p)
	if err == io.EOF {
		appended, openErr := os.OpenFile(f.Name(), os.O_WRONLY|os.O_APPEND, 0)
		if openErr != nil {
			return n, openErr
		}
		_, _ = appended.WriteString("more\n")
		_ = appended.Close()
	}
	return n, err
}

// TestTransferClient guards that the client of package connect, which rat uses, and ratd agree on
// the contract: an upload created, with its mode and the directories created told, then refused
// without overwrite with the sentence of ratd; a download of what was uploaded, with its mode and
// size; a download refused; and an upload whose file changed while read, which ratd does not
// write.
func TestTransferClient(t *testing.T) {
	r, client, home := transferRatd(t)
	ctx := context.Background()
	content := randomContent(t, 512<<10)
	local := filepath.Join(t.TempDir(), "app.bin")
	if err := os.WriteFile(local, content, 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(local, 0o640); err != nil {
		t.Fatal(err)
	}
	upload := func(path string, overwrite bool) (*connect.Uploaded, error) {
		file, err := os.Open(local)
		if err != nil {
			t.Fatal(err)
		}
		defer file.Close()
		return connect.Upload(ctx, nil, client, r.addr, file, path, overwrite)
	}
	remote := filepath.Join(home, "sub", "app.bin")
	mode := 0o640 &^ umask()
	up, err := upload("~/sub/app.bin", false)
	if err != nil || *up != (connect.Uploaded{Mode: mode, Sent: int64(len(content)), Note: "Missing directories created: " +
		filepath.Join(home, "sub") + ". Check the path if you did not mean them."}) {
		t.Fatalf("expected the file created, got %+v, %v", up, err)
	}
	expectFile(t, remote, string(content), mode)
	var refused *connect.Refusal
	if _, err = upload("~/sub/app.bin", false); !errors.As(err, &refused) ||
		refused.Status != http.StatusPreconditionFailed || refused.Text != remote+" exists: set overwrite to replace it." {
		t.Errorf("without overwrite: expected the refusal of ratd, got %v", err)
	}

	download, err := connect.StartDownload(ctx, client, r.addr, "~/sub/app.bin")
	if err != nil {
		t.Fatal(err)
	}
	var written bytes.Buffer
	n, err := download.WriteTo(&written)
	_ = download.Close()
	if err != nil || download.Mode != mode || download.Size != int64(len(content)) || n != download.Size ||
		!bytes.Equal(written.Bytes(), content) {
		t.Errorf("expected the file uploaded, with its mode and size, got %v, %d, %d bytes, %v", download.Mode,
			download.Size, n, err)
	}
	missing := filepath.Join(home, "missing")
	if _, err = connect.StartDownload(ctx, client, r.addr, missing); !errors.As(err, &refused) ||
		refused.Status != http.StatusNotFound || refused.Text != "Reading "+missing+" failed: no such file or directory." {
		t.Errorf("a missing file: expected the refusal of ratd, got %v", err)
	}

	file, err := os.Open(local)
	if err != nil {
		t.Fatal(err)
	}
	defer file.Close()
	grown := filepath.Join(home, "grown.bin")
	if _, err = connect.Upload(ctx, nil, client, r.addr, growingFile{file}, grown, false); !errors.Is(err, connect.ErrChanged) {
		t.Errorf("a file growing: expected ErrChanged, got %v", err)
	}
	eventually(t, "the cut upload logged", func() bool {
		return strings.Contains(r.logs.String(), "direction=upload outcome=error duration=") &&
			strings.Contains(r.logs.String(), "path="+grown)
	})
	if _, err = os.Lstat(grown); !errors.Is(err, fs.ErrNotExist) {
		t.Errorf("expected nothing written for a file changing while uploaded, got %v", err)
	}
	expectNoTemporary(t, home)
}

// blockingBody is the body of an upload stalling: some bytes, then nothing until unblocked.
type blockingBody struct {
	first   []byte
	unblock chan struct{}
}

func (b *blockingBody) Read(p []byte) (int, error) {
	if len(b.first) > 0 {
		n := copy(p, b.first)
		b.first = b.first[n:]
		return n, nil
	}
	<-b.unblock
	return 0, errors.New("unblocked")
}

// TestTransferStop guards the kill switch for transfers: one in flight is cut as soon as ratd
// stops, rather than holding the door open for the grace of the shutdown, an upload leaving no
// temporary file, and both are logged as cut by the stop.
func TestTransferStop(t *testing.T) {
	t.Run("upload", func(t *testing.T) {
		r, client, home := transferRatd(t)
		body := &blockingBody{first: bytes.Repeat([]byte("x"), 64<<10), unblock: make(chan struct{})}
		defer close(body.unblock)
		req := newUpload(t, r.addr, "~/app.bin", body, "0644", false)
		go func() { _, _, _ = sendTransfer(client, req) }()
		eventually(t, "the upload writing its temporary file", func() bool {
			matches, _ := filepath.Glob(filepath.Join(home, ".app.bin.rat-*"))
			return len(matches) == 1
		})
		expectQuickStop(t, r)
		expectNoTemporary(t, home)
		if !strings.Contains(r.logs.String(), `direction=upload outcome=error`) ||
			!strings.Contains(r.logs.String(), `error="cut: ratd stopping"`) {
			t.Errorf("expected the upload logged as cut by the stop:\n%s", r.logs)
		}
	})
	t.Run("download", func(t *testing.T) {
		r, client, home := transferRatd(t)
		path := filepath.Join(home, "large.bin")
		// sparse, read as zeros: far more than the buffers of the connection hold
		if err := os.WriteFile(path, nil, 0o600); err != nil {
			t.Fatal(err)
		}
		if err := os.Truncate(path, 256<<20); err != nil {
			t.Fatal(err)
		}
		resp, err := client.Do(newDownload(t, r.addr, path))
		if err != nil {
			t.Fatal(err)
		}
		// not read: ratd blocks writing once the buffers are full
		defer resp.Body.Close()
		expectQuickStop(t, r)
		if !strings.Contains(r.logs.String(), `direction=download outcome=error`) ||
			!strings.Contains(r.logs.String(), `error="cut: ratd stopping"`) {
			t.Errorf("expected the download logged as cut by the stop:\n%s", r.logs)
		}
	})
}

// expectQuickStop stops r, which has a transfer in flight, and fails the test unless it stops well
// within the grace of the shutdown.
func expectQuickStop(t *testing.T, r *servedRatd) {
	t.Helper()
	eventually(t, "a transfer in flight", func() bool {
		r.d.status.mu.Lock()
		defer r.d.status.mu.Unlock()
		return r.d.status.transfers == 1
	})
	start := time.Now()
	if err := r.stop(); err != nil {
		t.Fatal(err)
	}
	if took := time.Since(start); took > shutdownGrace/2 {
		t.Errorf("expected the transfer cut at once, the stop took %s", took)
	}
}
