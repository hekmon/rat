//go:build unix

package fifo

import (
	"context"
	"errors"
	"io"
	"io/fs"
	"os"
	"syscall"
	"time"
	"unsafe"

	"golang.org/x/sys/unix"
)

// Open opens the end of a named pipe with open, once a command opened the other end: as a command
// opening a pipe, it waits until then, reading the pipe (Read) or writing into it (Write). ctx done
// ends the wait with the error of ctx: Open opens the pipe read and write itself, which ends its
// own open, then closes both, leaving the pipe as it was. Opening it read and write takes both
// permissions on the pipe: without them, the open is left to end when a command opens the other
// end, its file closed then. What open opens must be a named pipe, ErrNotFIFO otherwise.
func Open(ctx context.Context, open Opener, way Way) (*Pipe, error) {
	flag := os.O_RDONLY
	if way == Write {
		flag = os.O_WRONLY
	}
	done := make(chan opened, 1)
	go func() {
		file, err := open(flag)
		done <- opened{file, err}
	}()
	select {
	case o := <-done:
		if o.err != nil {
			return nil, o.err
		}
		return newPipe(o.file)
	case <-ctx.Done():
	}
	// Read and write, the open never waits, and the pipe then has both ends: the open under way
	// returns, measured on Linux and macOS.
	release, err := open(os.O_RDWR)
	if err != nil {
		go closeOpened(done)
		return nil, ctx.Err()
	}
	defer release.Close()
	select {
	case o := <-done:
		if o.file != nil {
			o.file.Close()
		}
	case <-time.After(time.Second):
		// another pipe at the path by now, which the release opened
		go closeOpened(done)
	}
	return nil, ctx.Err()
}

// opened is what an open waiting for the other end of a pipe returned.
type opened struct {
	file *os.File
	err  error
}

// closeOpened closes the file of an open given up, once it returns.
func closeOpened(done <-chan opened) {
	if o := <-done; o.file != nil {
		o.file.Close()
	}
}

// newPipe returns the pipe file is an end of, never blocking from then on, or ErrNotFIFO, file
// closed on failure.
func newPipe(file *os.File) (*Pipe, error) {
	info, err := file.Stat()
	if err == nil && info.Mode()&fs.ModeNamedPipe == 0 {
		err = &fs.PathError{Op: "open", Path: file.Name(), Err: ErrNotFIFO}
	}
	var conn syscall.RawConn
	if err == nil {
		conn, err = file.SyscallConn()
	}
	if err == nil {
		// Go does so already on Linux, where it polls pipes, not on macOS
		if cerr := conn.Control(func(fd uintptr) { err = unix.SetNonblock(int(fd), true) }); cerr != nil {
			err = cerr
		}
	}
	if err != nil {
		file.Close()
		return nil, err
	}
	return &Pipe{file: file, conn: conn}, nil
}

// read reads into b what the pipe holds, waiting for some (see Reader).
func (p *Pipe) read(ctx context.Context, b []byte) (int, error) {
	if len(b) == 0 {
		return 0, nil
	}
	for {
		if err := ctx.Err(); err != nil {
			return 0, err
		}
		var n int
		var err error
		if cerr := p.conn.Control(func(fd uintptr) { n, err = unix.Read(int(fd), b) }); cerr != nil {
			return 0, p.closed("read")
		}
		switch {
		case err == nil && n > 0:
			return n, nil
		case err == nil:
			// no writer left: the command closed its end
			return 0, io.EOF
		case errors.Is(err, unix.EINTR):
		case errors.Is(err, unix.EAGAIN):
			if p.wait(false) != nil {
				return 0, p.closed("read")
			}
		default:
			return 0, &fs.PathError{Op: "read", Path: p.file.Name(), Err: err}
		}
	}
}

// write writes b into the pipe, waiting for room as long as it takes (see Writer).
func (p *Pipe) write(ctx context.Context, b []byte) (int, error) {
	written := 0
	for written < len(b) {
		if err := ctx.Err(); err != nil {
			return written, err
		}
		var n int
		var err error
		if cerr := p.conn.Control(func(fd uintptr) { n, err = unix.Write(int(fd), b[written:]) }); cerr != nil {
			return written, p.closed("write")
		}
		if n > 0 {
			written += n
		}
		switch {
		case err == nil, errors.Is(err, unix.EINTR):
		case errors.Is(err, unix.EAGAIN):
			if p.wait(true) != nil {
				return written, p.closed("write")
			}
		default:
			// EPIPE: the command reading the pipe closed its end
			return written, &fs.PathError{Op: "write", Path: p.file.Name(), Err: err}
		}
	}
	return written, nil
}

// closed is the error of a read or a write (op) of the pipe once closed: its raw connection then
// fails with an error of its own ("use of closed file"), where reads and writes of a file tell
// os.ErrClosed.
func (p *Pipe) closed(op string) error {
	return &fs.PathError{Op: op, Path: p.file.Name(), Err: os.ErrClosed}
}

// fdSetSize is how many descriptors select can watch: 1024, on Linux and macOS alike.
const fdSetSize = int(unsafe.Sizeof(unix.FdSet{})) * 8

// wait waits until the pipe can be read, or written when write, a tick at most, for its caller to
// check whether its transfer gave up before trying again. It fails once the pipe is closed. With
// select, which tells at once on both systems (see the package documentation); with poll beyond
// the descriptors select can watch, which may let a wait on macOS run its whole tick.
func (p *Pipe) wait(write bool) error {
	return p.conn.Control(func(fd uintptr) {
		// errors, EINTR included, left to the caller trying again
		if int(fd) < fdSetSize {
			var set unix.FdSet
			set.Set(int(fd))
			timeout := unix.NsecToTimeval(tick.Nanoseconds())
			if write {
				_, _ = unix.Select(int(fd)+1, nil, &set, nil, &timeout)
			} else {
				_, _ = unix.Select(int(fd)+1, &set, nil, nil, &timeout)
			}
			return
		}
		events := int16(unix.POLLIN)
		if write {
			events = unix.POLLOUT
		}
		_, _ = unix.Poll([]unix.PollFd{{Fd: int32(fd), Events: events}}, int(tick.Milliseconds()))
	})
}
