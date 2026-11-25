package main

import (
	"io"
	"io/fs"
	"net/http"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"

	"golang.org/x/sys/unix"
)

// holdPump holds the pump of the next portal as it is about to open its end, and returns entered,
// closed once it is there, and release, which lets it go on: what takes the path of a pipe meanwhile
// is there before ratd opens anything.
func holdPump(t *testing.T) (entered <-chan struct{}, release func()) {
	t.Helper()
	in, hold := make(chan struct{}), make(chan struct{})
	pumpOpening = func(string) {
		close(in)
		<-hold
	}
	// restored once the pump took the hook, for the race detector to see it read before
	t.Cleanup(func() {
		<-in
		pumpOpening = func(string) {}
	})
	return in, func() { close(hold) }
}

// takePath puts a regular file holding content in place of the pipe at path, as a command in a
// terminal would (rm, then a redirection).
func takePath(t *testing.T, path, content string) {
	t.Helper()
	if err := os.Remove(path); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}
}

// expectLeftAlone fails the test unless the file at path, put there in place of the pipe made, is
// as it was put, content and all, and nothing of the portal is left: no pump, and no descriptor of
// its pipe.
func expectLeftAlone(t *testing.T, path, content string, made fs.FileInfo) {
	t.Helper()
	eventually(t, "no pump left", func() bool {
		buf := make([]byte, 1<<20)
		return !strings.Contains(string(buf[:runtime.Stack(buf, true)]), ".(*portal).pump")
	})
	eventually(t, "no descriptor of the pipe left", func() bool { return len(pipeDescriptors(t, made, nil)) == 0 })
	got, err := os.ReadFile(path)
	if err != nil || string(got) != content {
		t.Errorf("expected the file put at %s left as it was (%q), got %q, %v", path, content, got, err)
	}
}

// TestPortalPathTakenBeforeOpen guards the pipes being the portal's (P1) the other way round: a
// file someone else put at the path of a portal, its pipe removed, before ratd opened its end, is
// not the portal's. ratd neither sends its content as the stream of a download, nor writes the
// body of an upload into it, nor deletes it: the answer ends telling the path is no longer the pipe
// of the portal, and the file is left as it was put.
func TestPortalPathTakenBeforeOpen(t *testing.T) {
	const theirs = "not the portal's\n"
	t.Run("download", func(t *testing.T) {
		entered, release := holdPump(t)
		pt := startPortals(t)
		path := filepath.Join(pt.home, "out.pipe")
		resp, _ := pt.open(t, pt.alice, path)
		made := expectPipe(t, path)
		<-entered
		takePath(t, path, theirs)
		release()
		data, err := readBody(t, resp.Body)
		told := resp.Trailer.Get("Rat-Error")
		if err != nil || len(data) != 0 || !strings.Contains(told, "is no longer the pipe of the portal") {
			t.Errorf("expected nothing sent and the path told taken, got %q, %v, %q", data, err, told)
		}
		expectLeftAlone(t, path, theirs, made)
	})
	t.Run("upload", func(t *testing.T) {
		entered, release := holdPump(t)
		pt := startPortals(t)
		path := filepath.Join(pt.home, "in.pipe")
		resp, send := pt.openUpload(t, pt.alice, path)
		made := expectPipe(t, path)
		<-entered
		takePath(t, path, theirs)
		release()
		sendBody(send, []byte("the body of the upload"))
		expectTrailer(t, resp, path+" is no longer the pipe of the portal: something took its place before a command "+
			"opened it. Nothing was written.")
		expectLeftAlone(t, path, theirs, made)
	})
}

// TestPortalPathTakenClosed guards a close, and a stop of ratd, of a portal whose path someone
// else took, a file or a symbolic link to one put where its pipe was: ratd, which leaves a wait
// through the other end of its pipe, opened by its name, and deletes that name, does neither to
// what is not the pipe it made: no byte is written into the file, nor through the link, and
// neither is deleted. The close answers all the same.
func TestPortalPathTakenClosed(t *testing.T) {
	const theirs = "not the portal's\n"
	for _, way := range []string{"download", "upload"} {
		for _, taken := range []string{"file", "link"} {
			for _, by := range []string{"close", "stop"} {
				t.Run(way+" "+taken+" "+by, func(t *testing.T) {
					entered, release := holdPump(t)
					pt := startPortals(t)
					path := filepath.Join(pt.home, "portal.pipe")
					if way == "download" {
						pt.open(t, pt.alice, path)
					} else {
						pt.openUpload(t, pt.alice, path)
					}
					made := expectPipe(t, path)
					<-entered
					file := path
					if taken == "link" {
						file = filepath.Join(pt.home, "theirs")
						if err := os.WriteFile(file, []byte(theirs), 0o644); err != nil {
							t.Fatal(err)
						}
						if err := os.Remove(path); err != nil {
							t.Fatal(err)
						}
						if err := os.Symlink(file, path); err != nil {
							t.Fatal(err)
						}
					} else {
						takePath(t, path, theirs)
					}
					if by == "stop" {
						expectQuickStop(t, pt.r)
					} else {
						closePortal := pt.closePortal
						if way == "upload" {
							closePortal = pt.closeUpload
						}
						if closed, text := closePortal(t, pt.alice, path); closed.StatusCode != http.StatusOK {
							t.Errorf("expected the portal closed, got %s: %q", closed.Status, text)
						}
					}
					release()
					expectLeftAlone(t, file, theirs, made)
					if info, err := os.Lstat(path); err != nil || (taken == "link") != (info.Mode()&fs.ModeSymlink != 0) {
						t.Errorf("expected what took %s left there, got %v, %v", path, info, err)
					}
				})
			}
		}
	}
}

// TestPortalPathTakenByPipe guards a close of a portal whose path another pipe took, a command
// waiting to read it: ratd does not open it, even for an instant, which would let that command
// into a pipe nobody writes, the end of its input following at once. The command still waits once
// the portal closed, and reads what is then written for it.
func TestPortalPathTakenByPipe(t *testing.T) {
	entered, release := holdPump(t)
	pt := startPortals(t)
	path := filepath.Join(pt.home, "out.pipe")
	pt.open(t, pt.alice, path)
	made := expectPipe(t, path)
	<-entered
	if err := os.Remove(path); err != nil {
		t.Fatal(err)
	}
	if err := unix.Mkfifo(path, 0o600); err != nil {
		t.Fatal(err)
	}
	// the command: it waits in the open of its pipe, then reads to the end of its input
	read := make(chan string, 1)
	go func() {
		file, err := os.OpenFile(path, os.O_RDONLY, 0)
		if err != nil {
			read <- err.Error()
			return
		}
		defer file.Close()
		data, _ := io.ReadAll(file)
		read <- string(data)
	}()
	if closed, text := pt.closePortal(t, pt.alice, path); closed.StatusCode != http.StatusOK {
		t.Errorf("expected the portal closed, got %s: %q", closed.Status, text)
	}
	// The pump, let go, opens what is at its path, as the command does, and leaves it at once, not
	// being its pipe: the writer stays until it did, the open of the pump waiting for one.
	release()
	w, err := os.OpenFile(path, os.O_WRONLY, 0)
	if err != nil {
		t.Fatal(err)
	}
	eventually(t, "no pump left", func() bool {
		buf := make([]byte, 1<<20)
		return !strings.Contains(string(buf[:runtime.Stack(buf, true)]), ".(*portal).pump")
	})
	if _, err := w.WriteString("theirs"); err != nil {
		t.Fatal(err)
	}
	_ = w.Close()
	if got := <-read; got != "theirs" {
		t.Errorf("expected the command to read what was written for it, then the end, got %q", got)
	}
	if err := os.Remove(path); err != nil {
		t.Errorf("expected the pipe of the command left at %s, got %v", path, err)
	}
	eventually(t, "no descriptor of the portal's pipe left", func() bool { return len(pipeDescriptors(t, made, nil)) == 0 })
}
