package fifo

import (
	"context"
	"errors"
	"io"
	"io/fs"
	"os"
	"syscall"
	"time"
)

// Way is how a transfer uses the pipe it opens: it reads it, or writes into it.
type Way int

const (
	// Read is the end of a transfer reading what a command writes into the pipe.
	Read Way = iota
	// Write is the end of a transfer writing what a command reads from the pipe.
	Write
)

func (w Way) String() string {
	if w == Write {
		return "write"
	}
	return "read"
}

// Opener opens the named pipe with flag: os.OpenFile on its path, or the OpenFile of the os.Root
// the pipe is in. Open passes O_RDONLY or O_WRONLY, and O_RDWR to end its own wait.
type Opener func(flag int) (*os.File, error)

// ErrNotFIFO is the error of Open when what it opened is not a named pipe: the path was replaced
// since its caller looked at it.
var ErrNotFIFO = errors.New("not a named pipe")

// tick is how long a read or a write waits for the pipe at most before checking again whether
// its transfer gave up.
const tick = 250 * time.Millisecond

// Pipe is an end of a named pipe, opened by Open, read or written through Reader or Writer, never
// blocking.
type Pipe struct {
	file *os.File
	conn syscall.RawConn
}

// Reader returns the pipe as an io.Reader, for a transfer reading it: a read waits for data,
// returns io.EOF once the command writing into the pipe closed its end, and the error of ctx once
// ctx is done, the pipe left open.
func (p *Pipe) Reader(ctx context.Context) io.Reader { return reader{p, ctx} }

// Writer returns the pipe as an io.Writer, for a transfer writing into it: a write returns once
// the pipe took all of it, with an error wrapping syscall.EPIPE once the command reading the pipe
// closed its end, and with the error of ctx once ctx is done, the pipe left open. On error, it
// returns what the pipe took.
func (p *Pipe) Writer(ctx context.Context) io.Writer { return writer{p, ctx} }

// Stat describes the pipe opened, for its caller to check that it is the one it looked at
// (os.SameFile).
func (p *Pipe) Stat() (fs.FileInfo, error) { return p.file.Stat() }

// Close closes this end of the pipe: the command at the other end then sees the end of the data
// (reading it) or a broken pipe (writing into it). A read or a write under way returns within a
// tick, with an error wrapping os.ErrClosed.
func (p *Pipe) Close() error { return p.file.Close() }

// reader and writer bind a pipe to the context of the transfer reading or writing it, for io.Copy.
type reader struct {
	p   *Pipe
	ctx context.Context
}

func (r reader) Read(b []byte) (int, error) { return r.p.read(r.ctx, b) }

type writer struct {
	p   *Pipe
	ctx context.Context
}

func (w writer) Write(b []byte) (int, error) { return w.p.write(w.ctx, b) }
