//go:build unix

package fifo

import (
	"bytes"
	"context"
	"crypto/rand"
	"errors"
	"io"
	"os"
	"path/filepath"
	"syscall"
	"testing"
	"time"

	"golang.org/x/sys/unix"
)

// newFIFO makes a named pipe for a test, and returns its path and an opener of it.
func newFIFO(t testing.TB) (string, Opener) {
	t.Helper()
	path := filepath.Join(t.TempDir(), "data.pipe")
	if err := unix.Mkfifo(path, 0o600); err != nil {
		t.Fatal(err)
	}
	return path, func(flag int) (*os.File, error) { return os.OpenFile(path, flag, 0) }
}

// openResult is what Open returned, run in a goroutine.
type openResult struct {
	p   *Pipe
	err error
}

// openAsync runs Open in a goroutine, as its wait for a command blocks.
func openAsync(ctx context.Context, open Opener, way Way) <-chan openResult {
	done := make(chan openResult, 1)
	go func() {
		p, err := Open(ctx, open, way)
		done <- openResult{p, err}
	}()
	return done
}

// otherEnd is the flag a command opens the other end of a pipe with, for a transfer's way.
func otherEnd(way Way) int {
	if way == Write {
		return os.O_RDONLY
	}
	return os.O_WRONLY
}

// openBoth opens the end way of the pipe at path with Open, and the other end as a command does
// (os.OpenFile, blocking until the pipe has both ends), both closed once the test ends.
func openBoth(t *testing.T, path string, open Opener, way Way) (*Pipe, *os.File) {
	t.Helper()
	done := openAsync(context.Background(), open, way)
	cmd, err := os.OpenFile(path, otherEnd(way), 0)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { cmd.Close() })
	o := receive(t, done, "Open once the command opened the other end")
	if o.err != nil {
		t.Fatal(o.err)
	}
	t.Cleanup(func() { o.p.Close() })
	return o.p, cmd
}

// receive returns what c gives, failing the test after 5 seconds without, telling what.
func receive[T any](t *testing.T, c <-chan T, what string) T {
	t.Helper()
	select {
	case v := <-c:
		return v
	case <-time.After(5 * time.Second):
		t.Fatalf("%s: nothing after 5s", what)
		var zero T
		return zero
	}
}

// expectRead fails the test unless r gives want next.
func expectRead(t *testing.T, r io.Reader, want string) {
	t.Helper()
	got := make([]byte, len(want))
	if _, err := io.ReadFull(r, got); err != nil || string(got) != want {
		t.Fatalf("expected to read %q, got %q, %v", want, got, err)
	}
}

// drain reads what the pipe at f holds now, without waiting, as a command reads before waiting
// for more: it returns how many bytes, and whether the end of the data came (no writer left).
func drain(t *testing.T, f *os.File) (n int, ended bool) {
	t.Helper()
	conn, err := f.SyscallConn()
	if err != nil {
		t.Fatal(err)
	}
	buf := make([]byte, 64<<10)
	for {
		var got int
		var rerr error
		if err := conn.Control(func(fd uintptr) {
			if rerr = unix.SetNonblock(int(fd), true); rerr == nil {
				got, rerr = unix.Read(int(fd), buf)
			}
		}); err != nil {
			t.Fatal(err)
		}
		switch {
		case rerr == nil && got > 0:
			n += got
		case rerr == nil:
			return n, true
		case errors.Is(rerr, unix.EAGAIN):
			return n, false
		case !errors.Is(rerr, unix.EINTR):
			t.Fatal(rerr)
		}
	}
}

// TestOpenWaits guards that opening an end of a pipe waits for a command to open the other end,
// for reading it as for writing into it, and that bytes then go through.
func TestOpenWaits(t *testing.T) {
	for _, way := range []Way{Read, Write} {
		t.Run(way.String(), func(t *testing.T) {
			path, open := newFIFO(t)
			done := openAsync(context.Background(), open, way)
			select {
			case o := <-done:
				if o.p != nil {
					o.p.Close()
				}
				t.Fatalf("returned before any command opened the other end: %v", o.err)
			case <-time.After(200 * time.Millisecond):
			}
			cmd, err := os.OpenFile(path, otherEnd(way), 0)
			if err != nil {
				t.Fatal(err)
			}
			defer cmd.Close()
			o := receive(t, done, "Open once the command opened the other end")
			if o.err != nil {
				t.Fatal(o.err)
			}
			p := o.p
			defer p.Close()
			if way == Read {
				if _, err := cmd.Write([]byte("x")); err != nil {
					t.Fatal(err)
				}
				expectRead(t, p.Reader(context.Background()), "x")
				return
			}
			if _, err := p.Writer(context.Background()).Write([]byte("x")); err != nil {
				t.Fatal(err)
			}
			expectRead(t, cmd, "x")
		})
	}
}

// TestOpenGivenUp guards giving up the wait for a command: Open returns the error of its context
// at once, and leaves the pipe as it was, no end of it open: a command opening it later waits for
// its own other end.
func TestOpenGivenUp(t *testing.T) {
	for _, way := range []Way{Read, Write} {
		t.Run(way.String(), func(t *testing.T) {
			path, open := newFIFO(t)
			ctx, cancel := context.WithCancel(context.Background())
			done := openAsync(ctx, open, way)
			time.Sleep(100 * time.Millisecond)
			start := time.Now()
			cancel()
			if o := receive(t, done, "Open given up"); !errors.Is(o.err, context.Canceled) {
				if o.p != nil {
					o.p.Close()
				}
				t.Fatalf("expected context.Canceled, got %v", o.err)
			}
			if elapsed := time.Since(start); elapsed > tick {
				t.Errorf("Open took %v to give up", elapsed)
			}
			if way == Read {
				// no reader left: a writer can not open without waiting for one
				f, err := os.OpenFile(path, os.O_WRONLY|syscall.O_NONBLOCK, 0)
				if err == nil {
					f.Close()
				}
				if !errors.Is(err, syscall.ENXIO) {
					t.Errorf("expected no reader left (ENXIO), got %v", err)
				}
				return
			}
			// no writer left: a reader sees the end of the data at once, rather than waiting
			f, err := os.OpenFile(path, os.O_RDONLY|syscall.O_NONBLOCK, 0)
			if err != nil {
				t.Fatal(err)
			}
			defer f.Close()
			if n, ended := drain(t, f); n != 0 || !ended {
				t.Errorf("expected no writer left (the end of the data), got %d bytes, ended %v", n, ended)
			}
		})
	}
}

// TestOpenNotFIFO guards that Open refuses what is not a named pipe, the path having been replaced
// since its caller looked at it.
func TestOpenNotFIFO(t *testing.T) {
	path := filepath.Join(t.TempDir(), "file")
	if err := os.WriteFile(path, nil, 0o600); err != nil {
		t.Fatal(err)
	}
	open := func(flag int) (*os.File, error) { return os.OpenFile(path, flag, 0) }
	if p, err := Open(context.Background(), open, Read); !errors.Is(err, ErrNotFIFO) {
		if p != nil {
			p.Close()
		}
		t.Errorf("expected ErrNotFIFO, got %v", err)
	}
}

// TestRead guards reading a pipe: every byte the command writes, in order, whatever the capacity
// of the pipe, and without waiting a tick at a time while the command is blocked in write(2), as
// poll would on macOS (1 MiB took 17 s); then io.EOF as soon as the command closed its end, even
// while a read waits for data, which poll misses on macOS too.
func TestRead(t *testing.T) {
	content := make([]byte, 1<<20)
	rand.Read(content)
	path, open := newFIFO(t)
	p, cmd := openBoth(t, path, open, Read)
	go func() {
		cmd.Write(content)
		cmd.Close()
	}()
	start := time.Now()
	if got, err := io.ReadAll(p.Reader(context.Background())); err != nil || !bytes.Equal(got, content) {
		t.Fatalf("expected the %d bytes written, got %d bytes, %v", len(content), len(got), err)
	}
	if elapsed := time.Since(start); elapsed > 2*time.Second {
		t.Errorf("reading 1 MiB took %v", elapsed)
	}

	path, open = newFIFO(t)
	silent, writer := openBoth(t, path, open, Read)
	errs := make(chan error, 1)
	go func() {
		_, err := silent.Reader(context.Background()).Read(make([]byte, 8))
		errs <- err
	}()
	time.Sleep(100 * time.Millisecond)
	start = time.Now()
	writer.Close()
	if err := receive(t, errs, "the read once the command left"); err != io.EOF {
		t.Fatalf("expected io.EOF, got %v", err)
	}
	if elapsed := time.Since(start); elapsed > tick/2 {
		t.Errorf("the end of the data seen %v after the command left", elapsed)
	}
}

// TestReadGivenUp guards giving up a read waiting for data: it returns the error of its context
// within a tick, and leaves the pipe open, held: the command writing into it is not told its
// reader left, and what it writes next is read by the next read.
func TestReadGivenUp(t *testing.T) {
	path, open := newFIFO(t)
	p, cmd := openBoth(t, path, open, Read)
	ctx, cancel := context.WithCancel(context.Background())
	errs := make(chan error, 1)
	go func() {
		_, err := p.Reader(ctx).Read(make([]byte, 8))
		errs <- err
	}()
	time.Sleep(100 * time.Millisecond)
	start := time.Now()
	cancel()
	if err := receive(t, errs, "the read given up"); !errors.Is(err, context.Canceled) {
		t.Fatalf("expected context.Canceled, got %v", err)
	}
	if elapsed := time.Since(start); elapsed > tick+100*time.Millisecond {
		t.Errorf("the read took %v to give up", elapsed)
	}
	if _, err := cmd.Write([]byte("x")); err != nil {
		t.Fatalf("the command writing was told its reader left: %v", err)
	}
	expectRead(t, p.Reader(context.Background()), "x")
}

// TestWrite guards writing into a pipe: every byte, in order, whatever the capacity of the pipe
// (8 KiB on macOS, 64 KiB on Linux), the command reading at its own pace, then the end of the
// data for the command once the pipe is closed.
func TestWrite(t *testing.T) {
	content := make([]byte, 1<<20)
	rand.Read(content)
	path, open := newFIFO(t)
	p, cmd := openBoth(t, path, open, Write)
	read := make(chan []byte, 1)
	go func() {
		got, _ := io.ReadAll(cmd)
		read <- got
	}()
	if n, err := p.Writer(context.Background()).Write(content); n != len(content) || err != nil {
		t.Fatalf("expected the %d bytes written, got %d, %v", len(content), n, err)
	}
	p.Close()
	if got := receive(t, read, "the read of the command"); !bytes.Equal(got, content) {
		t.Errorf("expected the %d bytes written, the command read %d", len(content), len(got))
	}
}

// TestWriteReaderGone guards a command that stops reading: the write fails with EPIPE, and the
// process goes on.
func TestWriteReaderGone(t *testing.T) {
	path, open := newFIFO(t)
	p, cmd := openBoth(t, path, open, Write)
	go func() {
		cmd.Read(make([]byte, 1024))
		cmd.Close()
	}()
	if _, err := p.Writer(context.Background()).Write(make([]byte, 1<<20)); !errors.Is(err, syscall.EPIPE) {
		t.Errorf("expected EPIPE, got %v", err)
	}
}

// TestWriteGivenUp guards giving up a write waiting for room in a full pipe: it returns the error
// of its context and what the pipe took, and leaves the pipe open, held: the command reading it
// gets those bytes, then would wait for more rather than see the end of its data, which comes once
// the pipe is closed.
func TestWriteGivenUp(t *testing.T) {
	path, open := newFIFO(t)
	p, cmd := openBoth(t, path, open, Write)
	ctx, cancel := context.WithCancel(context.Background())
	type wrote struct {
		n   int
		err error
	}
	done := make(chan wrote, 1)
	go func() {
		n, err := p.Writer(ctx).Write(make([]byte, 1<<20))
		done <- wrote{n, err}
	}()
	time.Sleep(100 * time.Millisecond)
	cancel()
	w := receive(t, done, "the write given up")
	if !errors.Is(w.err, context.Canceled) || w.n == 0 || w.n == 1<<20 {
		t.Fatalf("expected part of the bytes taken and context.Canceled, got %d, %v", w.n, w.err)
	}
	if n, ended := drain(t, cmd); n != w.n || ended {
		t.Errorf("held: expected the %d bytes taken, then no end; the command got %d, ended %v", w.n, n, ended)
	}
	p.Close()
	if _, ended := drain(t, cmd); !ended {
		t.Error("expected the end of the data once the pipe is closed")
	}
}

// TestCloseDuringRead guards closing a pipe while a read of it waits for data: the read returns
// an error wrapping os.ErrClosed within a tick.
func TestCloseDuringRead(t *testing.T) {
	path, open := newFIFO(t)
	p, _ := openBoth(t, path, open, Read)
	errs := make(chan error, 1)
	go func() {
		_, err := p.Reader(context.Background()).Read(make([]byte, 8))
		errs <- err
	}()
	time.Sleep(100 * time.Millisecond)
	start := time.Now()
	p.Close()
	if err := receive(t, errs, "the read of the pipe closed"); !errors.Is(err, os.ErrClosed) {
		t.Errorf("expected os.ErrClosed, got %v", err)
	}
	if elapsed := time.Since(start); elapsed > 2*tick {
		t.Errorf("the read took %v to return", elapsed)
	}
}

// TestThroughRoot guards opening a pipe through an os.Root, as rat opens those of its transfers
// directory: the open waits for the command as os.OpenFile does, and giving it up ends it alike.
func TestThroughRoot(t *testing.T) {
	dir := t.TempDir()
	if err := unix.Mkfifo(filepath.Join(dir, "data.pipe"), 0o600); err != nil {
		t.Fatal(err)
	}
	root, err := os.OpenRoot(dir)
	if err != nil {
		t.Fatal(err)
	}
	defer root.Close()
	open := func(flag int) (*os.File, error) { return root.OpenFile("data.pipe", flag, 0) }
	ctx, cancel := context.WithTimeout(context.Background(), 100*time.Millisecond)
	defer cancel()
	if p, err := Open(ctx, open, Write); !errors.Is(err, context.DeadlineExceeded) {
		if p != nil {
			p.Close()
		}
		t.Fatalf("expected the wait given up, got %v", err)
	}
	p, cmd := openBoth(t, filepath.Join(dir, "data.pipe"), open, Read)
	if _, err := cmd.Write([]byte("x")); err != nil {
		t.Fatal(err)
	}
	expectRead(t, p.Reader(context.Background()), "x")
}

// BenchmarkStream measures what a pipe carries from a Writer to a Reader of this package, 32 KiB
// at a time, as io.Copy writes.
func BenchmarkStream(b *testing.B) {
	_, open := newFIFO(b)
	reading := openAsync(context.Background(), open, Read)
	w, err := Open(context.Background(), open, Write)
	if err != nil {
		b.Fatal(err)
	}
	o := <-reading
	if o.err != nil {
		b.Fatal(o.err)
	}
	r := o.p
	copied := make(chan struct{})
	go func() {
		io.Copy(io.Discard, r.Reader(context.Background()))
		close(copied)
	}()
	chunk := make([]byte, 32<<10)
	writer := w.Writer(context.Background())
	b.SetBytes(int64(len(chunk)))
	for b.Loop() {
		if _, err := writer.Write(chunk); err != nil {
			b.Fatal(err)
		}
	}
	w.Close()
	<-copied
	r.Close()
}
