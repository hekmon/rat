package main

import (
	"errors"
	"fmt"
	"io"
	"io/fs"
	"net/http"
	"os"
	"sync"
	"sync/atomic"
	"syscall"
	"time"

	"github.com/hekmon/rat/connect"
	"github.com/hekmon/rat/internal/version"
	"github.com/hekmon/rat/mtls"
	"golang.org/x/sys/unix"
)

// A portal is a transfer with a named pipe at its remote end, where a file transfer has a file
// (see Portals in the README, and package connect for the wire). Its model: it opens, ratd making
// its pipe at the path asked, where nothing may be; the answer's headers tell it ready; a command
// of the agent, in a terminal, uses the pipe; it closes, ratd packing up: the pipe deleted first,
// then its end closed. ratd adds nothing to that model: no time limit, no heartbeat, no look for
// a command at the pipe, nor at the client while nothing is asked of it.
//
// One request is the portal, served by downloadPipe for a download, where a command writes into
// the pipe and what it writes is the body, and by uploadPipe for an upload, where the body is what
// ratd writes into the pipe for a command to read. Either way the portal waits in the plain
// blocking open of its end, which returns once a command opens the other one: until then nothing
// flows, nothing is read, and no byte is written into a pipe nobody reads (rejected: taking its
// end at once, then looking for a command, which loses the end of a stream). The pump then copies
// what comes, until the end of the source, the portal closing by itself: the command writing
// closed its end, or the body of the upload ended, all of it written into the pipe.
//
// Three events end it otherwise, while it waits in the open or while it copies: the link lost (the
// client gone, or a write or a read of the body failing), a close asked (DELETE, which a cancel
// sends), ratd stopping. The pipe is deleted first, for a command coming later to be told there is
// no such file rather than left waiting at a pipe nobody serves, then its end closed: a command
// writing gets a broken pipe from the system at its next write; a command reading sees the end of
// its input, which a close asked means (Q2), the answer telling it the stream was not whole.
//
// Two of them must not end the input of a command reading (P5): the link lost, which nobody asked
// for, and ratd stopping, whose terminals stop after the door, the command with them (Q5). The
// portal is then held instead (see hold): its name deleted, its pipe kept open by the end of its
// pump, and known by its path in the registry, another portal there refused, until a close asked,
// or the release once the terminals stopped, packs it up; the request returns meanwhile. A link
// lost is seen as the connection tells it, when the portal has something to do with it: an upload
// reads its body only once a command opened the pipe, so a client gone before is seen then. What
// it sent before leaving is read all the same, from the connection: a body cut midway is seen as
// cut, the portal held, its command seeing no end; a body the client had sent whole, its file
// small enough for the buffers of the connection, is read whole, the command served all of it and
// then the end of its input, as a portal done, which the client, gone, is not told.
//
// Leaving the wait of a download takes the other end of the pipe, opened by ratd itself
// (otherEnd): open for reading and writing without blocking, one descriptor holding both ends, it
// releases an open waiting for the other side, whichever of the two ran first, with no race on
// whether the open had begun, and a byte written on it releases a read waiting for data, a byte
// never copied, the pump looking at closing, set before, after every read. An upload needs it only
// for its open: past it, its pump writes into the pipe without blocking, waiting for room a
// bounded time, and leaves by itself once closing is set (see writePipe), nothing read out of the
// pipe, which keeps what the command is to read next. Closing the file of the pump would not do:
// on macOS, where Go keeps a FIFO out of its poller (kqueue misses the close of its last writer),
// a read or a write waiting holds the descriptor, and Close returns while it waits on (measured on
// macOS with Go 1.27, as poll(2) missing the close of the writer is); Linux releases it, but one
// mechanism serves both. The other end is opened by the name of the pipe, once checked to be the
// pipe ratd made: a name deleted by someone else (rm in a terminal), or another file in its place,
// can not be reached. The pump is then left to end by itself (abandon), closing its own end once
// its command closes or reads, the request returning meanwhile: a stop is never delayed, at the
// price of a goroutine and a descriptor left while that command holds the pipe, or forever for an
// open that waits for a command at a pipe nobody can name any more.

// portals are the portals open, by path: a close finds one by its path and session, and a pipe
// made where a portal still holds a path (its own pipe deleted by someone else, or a portal held)
// is refused, the portal being known by its path until closed.
type portals struct {
	mu   sync.Mutex
	open map[string]*portal
	// releasing is closed by release, once the terminals stopped: the held portals pack up
	releasing chan struct{}
	released  bool
	// held counts the held portals not packed up yet, which release waits for
	held sync.WaitGroup
}

// add registers p, unless a portal holds its path already, which it returns.
func (ps *portals) add(p *portal) (holder *portal) {
	ps.mu.Lock()
	defer ps.mu.Unlock()
	if ps.open == nil {
		ps.open = map[string]*portal{}
	}
	if holder := ps.open[p.path]; holder != nil {
		return holder
	}
	ps.open[p.path] = p
	return nil
}

// remove forgets p, once it packed up.
func (ps *portals) remove(p *portal) {
	ps.mu.Lock()
	defer ps.mu.Unlock()
	if ps.open[p.path] == p {
		delete(ps.open, p.path)
	}
}

// find returns the portal open at path for session, nil without one. The path alone names it,
// whichever way it goes (P8): one path holds one pipe, and an agent coming back to a portal closes
// it with either tool. A portal of another session is none, a client reaching its own session alone.
func (ps *portals) find(path, session string) *portal {
	ps.mu.Lock()
	defer ps.mu.Unlock()
	if p := ps.open[path]; p != nil && p.session == session {
		return p
	}
	return nil
}

// hold keeps p, held, until a close is asked or release is called, then packs it up with pack, in
// a goroutine of its own: the request of p returns meanwhile. Held once released (ratd stopping,
// handlers still running past the grace), it packs up at once.
func (ps *portals) hold(p *portal, pack func()) {
	ps.mu.Lock()
	if ps.released {
		ps.mu.Unlock()
		pack()
		return
	}
	if ps.releasing == nil {
		ps.releasing = make(chan struct{})
	}
	releasing := ps.releasing
	ps.held.Add(1)
	ps.mu.Unlock()
	go func() {
		defer ps.held.Done()
		select {
		case <-p.closeAsked:
		case <-releasing:
		}
		pack()
	}()
}

// release packs up the held portals, and returns once they did. ratd calls it once its terminals
// stopped (see run), and the commands reading with them: a command reading the pipe of a portal
// held while ratd stops is killed with its terminal, never handed the end of its input before (Q5).
func (ps *portals) release() {
	ps.mu.Lock()
	if !ps.released && ps.releasing != nil {
		close(ps.releasing)
	}
	ps.released = true
	ps.mu.Unlock()
	ps.held.Wait()
}

// portal is a portal open, served by one request, or held once it returned.
type portal struct {
	way           direction
	path, session string
	// made is the pipe as ratd made it, told apart from what may take its path later
	made fs.FileInfo
	// unpin lets go of what keeps made the pipe of the portal alone, once it packed up (see pinPipe)
	unpin func()
	// closeAsked is closed once a close is asked (askClose)
	closeAsked chan struct{}
	askOnce    sync.Once
	// packed is closed once the portal packed up, the facts below final
	packed chan struct{}
	// opened tells a command opened the pipe; moved counts the bytes copied, read from the pipe or
	// written into it: what the command saw, or will see before the end of its input, for the
	// answer to a close
	opened atomic.Bool
	moved  atomic.Int64
	// ending is how the portal ended, once packed
	ending portalEnding
	// mu guards abandoned and the body, which the pump reads or writes under it
	mu sync.Mutex
	// closing tells the portal packs up: from then on the pump touches neither the body nor what
	// it reads, nor writes into the pipe. Set under mu, for the pump to look at it with the body;
	// looked at without it by a write of an upload waiting for room (see writePipe).
	closing atomic.Bool
	// abandoned tells the request no longer waits for the pump, which closes its own end
	abandoned bool
}

// newPortal returns a portal of way at path for session, not registered yet.
func newPortal(way direction, path, session string) *portal {
	return &portal{way: way, path: path, session: session, closeAsked: make(chan struct{}),
		packed: make(chan struct{}), unpin: func() {}, ending: endedOtherwise}
}

// portalEnding is how a portal ended.
type portalEnding int

const (
	// endedWhole: the end of its source, all of it delivered (a download: its command closed the
	// pipe; an upload: its body ended, all of it written into the pipe)
	endedWhole portalEnding = iota
	// endedClosed: a close was asked
	endedClosed
	// endedStopped: the command reading the pipe of an upload left before the end
	endedStopped
	// endedOtherwise: cut, failed or ratd stopping
	endedOtherwise
)

// finish ends p, packed up: what pinned its pipe is let go, and those waiting for it are told.
func (p *portal) finish() {
	p.unpin()
	close(p.packed)
}

// askClose asks p to close; closeText tells, once it packed up, what its command saw.
func (p *portal) askClose() {
	p.askOnce.Do(func() { close(p.closeAsked) })
}

// pumpEnd is how the pump of a portal ended: the end it opened, nil if none, and why. linkErr is
// the body failing, a write of a download or a read of an upload, the link lost; otherwise
// pipeErr is the open of the pipe or an operation on it failing (a read, or a write, a broken pipe
// telling the command reading left), nil at the end of the source; left tells it left because the
// portal closes.
type pumpEnd struct {
	file    *os.File
	pipeErr error
	linkErr error
	left    bool
	// notMade tells the open found another file than the pipe ratd made at its path
	notMade bool
}

// endsClosing is called with the path of a portal as its ends are about to be closed, once the
// request or the hold no longer needs them: a hook for tests, which look then for its name, gone
// first (P9).
var endsClosing = func(string) {}

// pumpOpening is called with the path of a portal as its pump is about to open its end: a hook
// for tests, which hold it there, the portal then closing before the open begins.
var pumpOpening = func(string) {}

// openEnd opens the end of p for flag, blocking, as a command opening the pipe: it returns once a
// command opens the other end. What it opened is then checked to be the pipe ratd made: a file or
// a link someone else put at its path meanwhile is not the portal's, and is left untouched, opening
// a file changing nothing of it. Another pipe put there is opened as any command would open it, the
// one thing that reaches it: a command waiting at its other end is let in, and sees it closed at
// once. It returns the end opened, or how the pump ends.
func (p *portal) openEnd(flag int) (*os.File, *pumpEnd) {
	pumpOpening(p.path)
	file, err := os.OpenFile(p.path, flag, 0)
	if err != nil {
		return nil, &pumpEnd{pipeErr: err}
	}
	// what was opened may not be the pipe made (deleted, another file put there)
	if info, err := file.Stat(); err != nil || !os.SameFile(info, p.made) {
		return nil, &pumpEnd{file: file, pipeErr: err, notMade: err == nil}
	}
	p.mu.Lock()
	defer p.mu.Unlock()
	if p.closing.Load() {
		return nil, &pumpEnd{file: file, left: true}
	}
	p.opened.Store(true)
	return file, nil
}

// pump opens the end of p, a download, waiting for a command to open the other, then copies what
// comes into body, flushed as it comes, a small write sent at once, and hands how it ended to the
// request (hand). Every read is followed by a look at closing, under the lock the body is written
// under: once the portal closes, nothing more is written, and the byte otherEnd wrote to release a
// read is never copied.
func (p *portal) pump(body io.Writer, flush func() error, ended chan<- pumpEnd) {
	file, failed := p.openEnd(os.O_RDONLY)
	if failed != nil {
		p.hand(ended, *failed)
		return
	}
	buf := make([]byte, 32<<10)
	for {
		n, err := file.Read(buf)
		p.mu.Lock()
		if p.closing.Load() {
			p.mu.Unlock()
			p.hand(ended, pumpEnd{file: file, left: true})
			return
		}
		if n > 0 {
			_, writeErr := body.Write(buf[:n])
			if writeErr == nil {
				writeErr = flush()
			}
			if writeErr != nil {
				p.mu.Unlock()
				p.hand(ended, pumpEnd{file: file, linkErr: writeErr})
				return
			}
			p.moved.Add(int64(n))
		}
		p.mu.Unlock()
		switch {
		case errors.Is(err, io.EOF):
			p.hand(ended, pumpEnd{file: file})
			return
		case err != nil:
			p.hand(ended, pumpEnd{file: file, pipeErr: err})
			return
		}
	}
}

// pumpInto opens the end of p, an upload, waiting for a command to open the other, then copies
// body into the pipe, and hands how it ended to the request (hand). The body is read under the
// lock, with a look at closing before each read, and written into the pipe out of it, a write
// waiting for room as long as the command reads slowly (see writePipe): once the portal closes,
// the body is read no more, a close taking the lock once out of a read (see stopBodyRead), and
// nothing more is written into the pipe. The body is read only once a command opened the pipe:
// until then, it waits, sent by the client as far as the connection holds it.
func (p *portal) pumpInto(body io.Reader, ended chan<- pumpEnd) {
	file, failed := p.openEnd(os.O_WRONLY)
	if failed != nil {
		p.hand(ended, *failed)
		return
	}
	raw, err := file.SyscallConn()
	if err == nil {
		controlErr := raw.Control(func(fd uintptr) { err = unix.SetNonblock(int(fd), true) })
		if err == nil {
			err = controlErr
		}
	}
	if err != nil {
		p.hand(ended, pumpEnd{file: file, pipeErr: &fs.PathError{Op: "write", Path: p.path, Err: err}})
		return
	}
	buf := make([]byte, 32<<10)
	for {
		p.mu.Lock()
		if p.closing.Load() {
			p.mu.Unlock()
			p.hand(ended, pumpEnd{file: file, left: true})
			return
		}
		n, err := body.Read(buf)
		p.mu.Unlock()
		if n > 0 {
			left, writeErr := p.writePipe(raw, buf[:n])
			switch {
			case writeErr != nil:
				p.hand(ended, pumpEnd{file: file, pipeErr: writeErr})
				return
			case left:
				p.hand(ended, pumpEnd{file: file, left: true})
				return
			}
		}
		switch {
		case errors.Is(err, io.EOF):
			p.hand(ended, pumpEnd{file: file})
			return
		case err != nil:
			p.hand(ended, pumpEnd{file: file, linkErr: err})
			return
		}
	}
}

// roomWait bounds a wait of an upload for room in its pipe: the pump looks at closing that often
// at least while the command reading does not read.
const roomWait = 20 * time.Millisecond

// writePipe writes data into the pipe of p, an upload, through raw, its end, non-blocking, counting
// the bytes written (moved), and returns once all of it was, or once the portal closes, left then
// true, or with the error of a write. The pipe full, it waits for room, roomWait at most before
// looking at closing again: a close never has to release a write, and never touches the pipe. So
// what the pipe holds when the portal closes is the stream's own, the bytes after those the
// command read, which it reads then, then the end of its input: a beginning of the stream (P3),
// counted exactly. Rejected: a blocking write, released by a close reading the pipe through its
// other end, which took the oldest bytes out of it, those the command was to read next, the write
// then landing behind them, a hole in the stream; and closing the end under the write, which macOS
// does not release (see the doc of the portals).
//
// The wait is poll(2), measured on 2026-11-25 with a FIFO, its end non-blocking: on Linux it tells
// room once a write can take some (a page), and the command reading leaving (POLLERR), the next
// write then failing with a broken pipe; on macOS it tells room once a single byte is free, where
// a write takes PIPE_BUF bytes (512) or fails, and nothing of the command leaving, the next write
// failing with a broken pipe all the same. On macOS, then, a wait may return at once with no room
// for a write: a millisecond is waited before the next one, rather than turning on the spot.
func (p *portal) writePipe(raw syscall.RawConn, data []byte) (left bool, err error) {
	roomTold := false
	for len(data) > 0 {
		var n int
		var writeErr error
		if err := raw.Control(func(fd uintptr) { n, writeErr = unix.Write(int(fd), data) }); err != nil {
			return false, &fs.PathError{Op: "write", Path: p.path, Err: err}
		}
		if n > 0 {
			p.moved.Add(int64(n))
			data = data[n:]
			roomTold = false
			continue
		}
		switch {
		case writeErr == nil, errors.Is(writeErr, unix.EINTR):
			continue
		case !errors.Is(writeErr, unix.EAGAIN):
			return false, &fs.PathError{Op: "write", Path: p.path, Err: writeErr}
		case p.closing.Load():
			return true, nil
		case roomTold:
			// macOS: room told for less than a write takes
			time.Sleep(time.Millisecond)
		}
		var ready int
		_ = raw.Control(func(fd uintptr) {
			ready, _ = unix.Poll([]unix.PollFd{{Fd: int32(fd), Events: unix.POLLOUT}}, int(roomWait/time.Millisecond))
		})
		roomTold = ready > 0
	}
	return false, nil
}

// hand hands end to the request of p, the pump returning, unless the request abandoned it: the
// pump then closes its own end, nobody else holding it.
func (p *portal) hand(ended chan<- pumpEnd, end pumpEnd) {
	p.mu.Lock()
	defer p.mu.Unlock()
	if p.abandoned {
		if end.file != nil {
			_ = end.file.Close()
		}
		return
	}
	// buffered: the request reads it, or drains it as it abandons the pump
	ended <- end
}

// abandon tells the pump of p the request waits for it no more, and returns how it ended if it had
// already, false otherwise: the pump then closes its own end.
func (p *portal) abandon(ended chan pumpEnd) (pumpEnd, bool) {
	p.mu.Lock()
	p.abandoned = true
	p.mu.Unlock()
	select {
	case end := <-ended:
		return end, true
	default:
		return pumpEnd{}, false
	}
}

// leave packs p, a download, up while its pump waits, in the open of its end, in a read, or in a
// write of the body, and returns how the pump ended, once it did; or, abandoning a pump it can not
// reach (see otherEnd), false. The name goes first, then the ends are closed, the end of the pump
// by the caller. link tells the link is the cause: no trailer will be read, and a write of the
// body may stay cut.
func (d *daemon) leave(p *portal, controller *http.ResponseController, ended chan pumpEnd, link bool) (pumpEnd,
	bool) {
	// Out of a write of the body first, which a client not reading holds: the lock is the pump's
	// while it writes. Once closing is set, the pump writes nothing more, and the deadline is lifted
	// for the trailer to go, unless ratd stops (see cutOnStop, which may have set it before).
	_ = controller.SetWriteDeadline(time.Now())
	p.mu.Lock()
	p.closing.Store(true)
	p.mu.Unlock()
	if !link {
		_ = controller.SetWriteDeadline(time.Time{})
		select {
		case <-d.stopping:
			_ = controller.SetWriteDeadline(time.Now())
		default:
		}
	}
	other, reached := p.otherEnd()
	d.removePipe(p)
	if !reached {
		d.logger.Warn("the pipe of a portal could not be reached to end its wait: left to its command", "path",
			p.path)
		return p.abandon(ended)
	}
	// A read waiting for data takes this byte, and leaves (see pump); an open, or a pipe full (the
	// pump not waiting in a read then), leaves it in the pipe, deleted with it.
	_, _ = unix.Write(other, []byte{0})
	end := <-ended
	endsClosing(p.path)
	_ = unix.Close(other)
	return end, true
}

// stopBodyRead sets closing on p, an upload, its pump out of a read of the body, which it reads
// under the lock: a read waiting for the client (a link stalled, a client reading its file slowly)
// is cut by a read deadline first, then lifted, for the rest of the body to be read once the
// handler returns (net/http reads up to 256 KiB of it, then closes the connection gracefully,
// sending the answer first), unless ratd stops (see cutOnStop). Once closing is set, the pump reads
// the body no more: it never touches it once the request returned. A deadline is set only when the
// lock is taken, the pump then possibly in a read: a read cut makes the body fail for good, the
// connection then closed at once once the handler returns, which may lose the trailer.
func (d *daemon) stopBodyRead(p *portal, controller *http.ResponseController) {
	if !p.mu.TryLock() {
		_ = controller.SetReadDeadline(time.Now())
		p.mu.Lock()
		_ = controller.SetReadDeadline(time.Time{})
		select {
		case <-d.stopping:
			_ = controller.SetReadDeadline(time.Now())
		default:
		}
	}
	p.closing.Store(true)
	p.mu.Unlock()
}

// otherEnd opens the other end of the pipe of p, for reading and writing without blocking, and
// tells whether it reached it: only the pipe ratd made, looked at by its path before it is opened,
// another pipe put there not being opened even for an instant (it would let a command waiting at
// it in, then end its input), and checked once opened, the path possibly taken in between.
func (p *portal) otherEnd() (int, bool) {
	if info, err := os.Lstat(p.path); err != nil || !os.SameFile(info, p.made) {
		return -1, false
	}
	fd, err := unix.Open(p.path, unix.O_RDWR|unix.O_NONBLOCK|unix.O_CLOEXEC, 0)
	if err != nil {
		return -1, false
	}
	var opened unix.Stat_t
	made, ok := p.made.Sys().(*syscall.Stat_t)
	if unix.Fstat(fd, &opened) != nil || !ok || uint64(opened.Dev) != uint64(made.Dev) ||
		uint64(opened.Ino) != uint64(made.Ino) {
		_ = unix.Close(fd)
		return -1, false
	}
	return fd, true
}

// removePipe deletes the pipe of p, if its path still names it: another file put there is not
// ratd's to delete.
func (d *daemon) removePipe(p *portal) {
	info, err := os.Lstat(p.path)
	if err != nil || !os.SameFile(info, p.made) {
		return
	}
	if err := os.Remove(p.path); err != nil && !errors.Is(err, fs.ErrNotExist) {
		d.logger.Warn("failed to delete the pipe of a portal", "path", p.path, "error", err)
	}
}

// stoppingText tells the portal at path ended by ratd stopping, in the trailer of its answer. It
// tells no portal closed: one a command reads is held until the terminals stopped (Q5), after
// this answer.
func stoppingText(path string) string {
	return fmt.Sprintf("ratd is stopping: the portal at %s ends with it, its pipe deleted.", path)
}

// errPipeTaken is the error of pinPipe finding, at the path of a pipe just made, another file than
// that pipe.
var errPipeTaken = errors.New("not the pipe made")

// errPortalClosed is the error of a portal closed as asked.
var errPortalClosed = errors.New("portal closed as asked")

// errLinkLost is the error of a portal whose client left.
var errLinkLost = errors.New("cut: the client left")

// openPortal opens the portal of way req asks, its pipe made at the path it names, where nothing
// may be, and returns it, registered: the caller removes it once packed. Every answer echoes
// Rat-Pipe, for rat to tell a ratd older than the portals. No directory is created for a pipe:
// an opening that fails leaves nothing, its refusal answered and returned.
func (d *daemon) openPortal(w http.ResponseWriter, req *http.Request, way direction) (*portal, []any, *result) {
	path, refused := transferPath(req, way)
	if refused != nil {
		r := refuse(w, http.StatusBadRequest, *refused)
		return nil, nil, &r
	}
	attrs := []any{"path", path, "pipe", true}
	p := newPortal(way, path, mtls.PeerName(*req.TLS))
	// Registered before the pipe is made: a portal whose pipe someone else deleted still holds its
	// path, until closed. A close finding it before its pipe is made waits for it to pack up.
	if holder := d.portals.add(p); holder != nil {
		text := fmt.Sprintf("A portal is open at %s: close it with cancel before opening another there.", path)
		if holder.session != p.session {
			// its cancel would find none: a session closes its own portals alone
			text = fmt.Sprintf("%s is held by a portal of another session, which alone can close it: use another "+
				"path.", path)
		}
		r := refuse(w, http.StatusConflict, result{err: fs.ErrExist, attrs: attrs, text: text})
		return nil, nil, &r
	}
	failed := func(status int, r result) (*portal, []any, *result) {
		d.portals.remove(p)
		p.finish()
		r = refuse(w, status, r)
		return nil, nil, &r
	}
	// mkfifo creates the pipe or fails where anything is, atomically: no look before it.
	if err := unix.Mkfifo(path, 0o600); err != nil {
		err = &fs.PathError{Op: "mkfifo", Path: path, Err: err}
		if errors.Is(err, fs.ErrExist) {
			return failed(http.StatusConflict, result{err: err, attrs: attrs, text: fmt.Sprintf("%s exists: a portal "+
				"makes its pipe itself, at a path where nothing is. If it is the pipe of a portal still open, close it "+
				"with cancel.", path)})
		}
		return failed(transferStatus(err), withAttrs(d.fileFailure(err, "Making the pipe "+path, written, false), attrs))
	}
	made, err := os.Lstat(path)
	if err != nil {
		return failed(transferStatus(err), withAttrs(d.fileFailure(err, "Making the pipe "+path, written, false), attrs))
	}
	p.made = made
	unpin, err := pinPipe(path, made)
	switch {
	case errors.Is(err, errPipeTaken):
		// not removed: what is there now is not the pipe made
		return failed(http.StatusConflict, result{err: err, attrs: attrs, text: fmt.Sprintf("%s was taken by "+
			"something else as the portal made its pipe there: nothing was opened.", path)})
	case err != nil:
		// the pin itself failing (no descriptor left): the pipe made is removed, an opening that
		// fails leaving nothing, and the reason of the system told
		d.removePipe(p)
		return failed(transferStatus(err), withAttrs(d.fileFailure(err, "Making the pipe "+path, written, false), attrs))
	}
	p.unpin = unpin
	return p, attrs, nil
}

// downloadPipe serves a download from a pipe (connect.HeaderPipe): it makes a named pipe at the
// path req names, answers its headers at once, telling the portal ready, then waits for a command
// to open the pipe for writing, and sends what it writes as the body, flushed as it comes. The
// body ends cleanly once the command closed its end, the end of the source, all of it sent: the
// pipe is deleted, then its end closed. A close asked, ratd stopping, or the link lost end it
// otherwise, packing up alike, the first two telling why in the Rat-Error trailer, as a read of
// the pipe failing is told (see the doc of the portals). HEAD is refused, a portal opening by GET
// alone. The answer carries no Rat-Size nor Rat-Mode: a pipe has no size, and a file made
// from one takes 0600 on the side of the client (P7), whatever the bits of the pipe.
func (d *daemon) downloadPipe(w http.ResponseWriter, req *http.Request) result {
	w.Header().Set(connect.HeaderPipe, "true")
	if req.Method == http.MethodHead {
		return refuse(w, http.StatusBadRequest, result{err: connect.ErrHeader,
			text: "HEAD with Rat-Pipe: a portal is opened by GET alone, its headers telling it ready."})
	}
	p, attrs, refused := d.openPortal(w, req, download)
	if refused != nil {
		return *refused
	}
	path := p.path
	defer d.portals.remove(p)
	defer p.finish()
	header := w.Header()
	header.Set("Content-Type", connect.OctetStream)
	header.Set("Trailer", connect.HeaderError)
	w.WriteHeader(http.StatusOK)
	attrs = append(attrs, "status", http.StatusOK)
	controller := http.NewResponseController(w)
	// ready: the pipe exists, nothing flowed
	if err := controller.Flush(); err != nil {
		d.removePipe(p)
		return result{outcome: failed, err: d.cutError(err), attrs: append(attrs, "bytes", 0, "opened", false)}
	}
	ended := make(chan pumpEnd, 1)
	go p.pump(w, controller.Flush, ended)
	var end pumpEnd
	var cause error
	reached := true
	select {
	case end = <-ended:
	case <-req.Context().Done():
		cause = errLinkLost
	case <-p.closeAsked:
		cause = errPortalClosed
	case <-d.stopping:
		cause = errStopping
	}
	if cause != nil {
		end, reached = d.leave(p, controller, ended, cause == errLinkLost)
	} else {
		d.removePipe(p)
	}
	if reached && end.file != nil {
		endsClosing(p.path)
		_ = end.file.Close()
	}
	attrs = append(attrs, "bytes", p.moved.Load(), "opened", p.opened.Load())
	var failure result
	switch {
	case cause == errPortalClosed:
		p.ending = endedClosed
		failure = result{err: cause, text: fmt.Sprintf("The portal at %s was closed, as asked: its pipe is deleted.",
			path)}
	case cause == errStopping:
		failure = result{err: cause, text: stoppingText(path)}
	case cause != nil:
		return result{outcome: failed, err: d.cutError(cause), attrs: attrs}
	case end.linkErr != nil:
		// the client left, or ratd stops (see cutOnStop): nobody reads a trailer
		return result{outcome: failed, err: d.cutError(end.linkErr), attrs: attrs}
	case end.notMade:
		failure = result{err: errors.New("pipe replaced"), text: fmt.Sprintf("%s is no longer the pipe of the portal: "+
			"something took its place before a command opened it. Nothing was copied.", path)}
	case end.pipeErr != nil:
		failure = d.fileFailure(end.pipeErr, "Reading the pipe "+path, read, false)
	default:
		p.ending = endedWhole
		return result{attrs: attrs}
	}
	header.Set(connect.HeaderError, connect.EncodeError(failure.text))
	failure.outcome, failure.attrs = failed, append(failure.attrs, attrs...)
	return failure
}

// uploadPipe serves an upload into a pipe (connect.HeaderPipe): it makes a named pipe at the path
// req names, answers its headers at once, before reading any byte of the body, telling the portal
// ready, then waits for a command to open the pipe for reading, and writes the body into it as it
// comes. Its outcome is told after, in the Rat-Error trailer, the status being sent by then: none
// once the body ended cleanly and all of it was written into the pipe, the end of the source, the
// pipe then deleted and its end closed, the command seeing the end of its input; the command
// leaving before the end (a write failing with a broken pipe), a close asked, ratd stopping, a
// write of the pipe failing, otherwise. A body cut (the client gone, or failing its body on purpose, its file having
// changed) once a command opened the pipe, and ratd stopping then, hold the portal (see hold): the
// command sees no end. Rat-Overwrite, Rat-In-Place and Rat-Mode mean nothing for a pipe, made where
// nothing is with its own bits: they are not read.
func (d *daemon) uploadPipe(w http.ResponseWriter, req *http.Request) result {
	w.Header().Set(connect.HeaderPipe, "true")
	if refused := octetStreamRefusal(req); refused != nil {
		return refuse(w, http.StatusUnsupportedMediaType, *refused)
	}
	p, attrs, refused := d.openPortal(w, req, upload)
	if refused != nil {
		return *refused
	}
	path := p.path
	held := false
	defer func() {
		if !held {
			d.portals.remove(p)
			p.finish()
		}
	}()
	controller := http.NewResponseController(w)
	// The answer starts before the body is read: the headers say ready, the body waits for a
	// command, as a download says ready and its body waits. Go's HTTP/1 server does not let a
	// handler answer while the body is unread, unless both directions are enabled: answering, it
	// disposes of the body first (measured with Go 1.27 over TLS: a body the client holds until the
	// headers came, the flush of the headers waits for it, and the exchange hangs; a body sent at
	// once is closed under the handler, whose reads fail, "invalid Read on closed Body"). With it,
	// over TLS, the headers reach the client in milliseconds, no byte of the body taken by then,
	// and 8 MiB sent afterwards arrive whole, the trailer telling the outcome. Rejected: an interim answer
	// saying ready (100 Continue or 102), the final one at the end, the common path, which gives an
	// upload and a download two ways to say ready. The limit (Q13): a proxy between rat and ratd
	// may hold this answer until the body ended; a portal is promised on a direct connection.
	if err := controller.EnableFullDuplex(); err != nil {
		d.removePipe(p)
		return refuse(w, http.StatusInternalServerError, result{err: err, attrs: attrs,
			text: "The portal can not be opened: this connection can not answer before its body is read."})
	}
	w.Header().Set("Trailer", connect.HeaderError)
	w.WriteHeader(http.StatusOK)
	attrs = append(attrs, "status", http.StatusOK)
	// ready: the pipe exists, nothing flowed, no byte of the body read
	if err := controller.Flush(); err != nil {
		d.removePipe(p)
		return result{outcome: failed, err: d.cutError(err), attrs: append(attrs, "bytes", 0, "opened", false)}
	}
	ended := make(chan pumpEnd, 1)
	go p.pumpInto(req.Body, ended)
	var end pumpEnd
	var cause error
	// The context of the request is not looked at: the body unread, net/http does not read the
	// connection, and sees the client gone only as the pump reads the body, which tells it.
	select {
	case end = <-ended:
	case <-p.closeAsked:
		cause = errPortalClosed
	case <-d.stopping:
		cause = errStopping
	}
	reached := true
	if cause != nil {
		// closing set, opened is final: the pump sets it only while closing is not
		d.stopBodyRead(p, controller)
		opened := p.opened.Load()
		other := -1
		if !opened {
			// the pump may wait in the open of its end, which only the other end releases
			other, reached = p.otherEnd()
		}
		d.removePipe(p)
		if reached {
			// past the open, the pump leaves within roomWait (see writePipe)
			end = <-ended
		} else {
			d.logger.Warn("the pipe of a portal could not be reached to end its wait: left to its command", "path",
				p.path)
			end, reached = p.abandon(ended)
		}
		if other >= 0 {
			endsClosing(p.path)
			_ = unix.Close(other)
		}
		if cause == errStopping && opened {
			// Q5: the command reading stops with its terminal, after the door; its input is not
			// ended before, the end of the pump kept open
			held = true
			d.hold(p, end.file)
			w.Header().Set(connect.HeaderError, connect.EncodeError(stoppingText(path)))
			return result{outcome: failed, err: cause, attrs: append(attrs, "bytes", p.moved.Load(), "opened", true,
				"held", true)}
		}
	} else if end.linkErr != nil {
		// The body cut: the client gone, or failing its body on purpose. It is read only once a
		// command opened the pipe, whose input the end of the pump keeps open (P5).
		d.removePipe(p)
		held = true
		d.hold(p, end.file)
		return result{outcome: failed, err: d.cutError(end.linkErr), attrs: append(attrs, "bytes", p.moved.Load(),
			"opened", p.opened.Load(), "held", true)}
	} else {
		d.removePipe(p)
	}
	if reached && end.file != nil {
		endsClosing(p.path)
		_ = end.file.Close()
	}
	attrs = append(attrs, "bytes", p.moved.Load(), "opened", p.opened.Load())
	var failure result
	switch {
	case cause == errPortalClosed:
		p.ending = endedClosed
		failure = result{err: cause, text: closeText(p)}
	case cause == errStopping:
		// The answer is cut by the stop (see cutOnStop), most often: told in the trailer all the
		// same, an answer ending cleanly without one being a portal done, which a client reading it
		// before the cut would take a stop for.
		failure = result{err: cause, text: stoppingText(path)}
	case end.notMade:
		failure = result{err: errors.New("pipe replaced"), text: fmt.Sprintf("%s is no longer the pipe of the portal: "+
			"something took its place before a command opened it. Nothing was written.", path)}
	case errors.Is(end.pipeErr, syscall.EPIPE):
		p.ending = endedStopped
		failure = result{err: end.pipeErr, text: fmt.Sprintf("The command reading %s left before the end of the "+
			"stream, after %s were written into the pipe: read why in its terminal. The pipe is deleted.", path,
			sizeText(int(p.moved.Load())))}
	case end.pipeErr != nil:
		failure = d.fileFailure(end.pipeErr, "Writing the pipe "+path, written, false)
	default:
		p.ending = endedWhole
		return result{attrs: attrs}
	}
	w.Header().Set(connect.HeaderError, connect.EncodeError(failure.text))
	failure.outcome, failure.attrs = failed, append(failure.attrs, attrs...)
	return failure
}

// hold keeps p, an upload whose request returns, held (see the doc of the portals): its name
// deleted already, its pipe kept open by file, the end of its pump, which returned, until a close
// asked or the release packs it up: file is closed, the command reading then reading what the pipe
// still holds, then the end of its input.
func (d *daemon) hold(p *portal, file *os.File) {
	d.portals.hold(p, func() {
		select {
		case <-p.closeAsked:
			p.ending = endedClosed
		default:
		}
		if file != nil {
			endsClosing(p.path)
			_ = file.Close()
		}
		d.portals.remove(p)
		p.finish()
		d.logger.Info("held portal packed up", "path", p.path, "session", p.session, "closed", p.ending == endedClosed,
			"bytes", p.moved.Load())
	})
}

// closePortal closes the portal the request names by its path (connect.HeaderPath), for the session
// of its client certificate, and answers once it packed up, telling what its command saw: 200 when
// it closed one, 404 with none there for that session, nothing to close. It serves the endpoint of
// way, and closes the portal at the path whichever way that one goes, the path alone naming it (see
// find). The path is read as the transfer's (see transferPath). Every answer carries the version of ratd, the 404 of none
// told apart from the one of a ratd older than the transfers; and each close is logged in one line.
func (d *daemon) closePortal(way direction) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, req *http.Request) {
		w.Header().Set(connect.HeaderVersion, version.Module())
		var session string
		if req.TLS != nil {
			session = mtls.PeerName(*req.TLS)
		}
		if session == "" {
			http.Error(w, "A client certificate naming the session is required.", http.StatusBadRequest)
			return
		}
		d.status.see(session, time.Now())
		path, refused := transferPath(req, way)
		attrs := []any{"session", session, "remote", req.RemoteAddr, "direction", way}
		if refused != nil {
			http.Error(w, refused.text, http.StatusBadRequest)
			d.logger.Info("portal close", append(attrs, "outcome", "error", "error", refused.err)...)
			return
		}
		attrs = append(attrs, "path", path)
		p := d.portals.find(path, session)
		if p == nil {
			http.Error(w, fmt.Sprintf("No portal is open at %s: nothing to close.", path), http.StatusNotFound)
			d.logger.Info("portal close", append(attrs, "outcome", "message", "closed", false)...)
			return
		}
		p.askClose()
		select {
		case <-p.packed:
		case <-req.Context().Done():
			// the client left: the portal packs up all the same, nobody reads the answer
			d.logger.Info("portal close", append(attrs, "outcome", "error", "error", errLinkLost)...)
			return
		}
		w.Header().Set("Content-Type", "text/plain; charset=utf-8")
		_, _ = io.WriteString(w, closeText(p))
		d.logger.Info("portal close", append(attrs, "outcome", "ok", "closed", true, "bytes", p.moved.Load(),
			"opened", p.opened.Load())...)
	})
}

// closeText tells what the command at the pipe of p, packed up, saw: for a download, a broken pipe
// at its next write after the bytes copied; for an upload, the end of its input after the bytes it
// saw, a stream that was not whole; or nothing, none having opened it; or that the portal had
// ended before the close took it, the end of its source or cut, or the command reading left.
func closeText(p *portal) string {
	moved := sizeText(int(p.moved.Load()))
	if p.way == upload {
		switch {
		case p.ending == endedWhole:
			return fmt.Sprintf("The portal at %s had ended: all of it was written into the pipe, after %s, and the "+
				"command reading it saw the end of its input. The pipe is deleted.", p.path, moved)
		case p.ending == endedStopped:
			return fmt.Sprintf("The portal at %s had ended: the command reading it left before the end of the stream, "+
				"after %s were written into the pipe. The pipe is deleted.", p.path, moved)
		case p.ending != endedClosed:
			return fmt.Sprintf("The portal at %s had ended before the close, cut after %s: the pipe is deleted.", p.path,
				moved)
		case !p.opened.Load():
			return fmt.Sprintf("Closed the portal at %s: the pipe is deleted. No command had opened it.", p.path)
		default:
			return fmt.Sprintf("Closed the portal at %s: the pipe is deleted. A command still reading it saw the end of "+
				"its input after %s, which is not the whole stream: stop it if it still runs, and do not trust what it "+
				"made of them.", p.path, sizeText(int(p.moved.Load())))
		}
	}
	switch {
	case p.ending == endedWhole:
		return fmt.Sprintf("The portal at %s had ended: the command writing into it closed the pipe after %s, all of "+
			"it sent. The pipe is deleted.", p.path, moved)
	case p.ending != endedClosed:
		return fmt.Sprintf("The portal at %s had ended before the close, cut after %s: the pipe is deleted.", p.path,
			moved)
	case !p.opened.Load():
		return fmt.Sprintf("Closed the portal at %s: the pipe is deleted. No command had opened it.", p.path)
	default:
		return fmt.Sprintf("Closed the portal at %s after %s: the pipe is deleted, and the command writing into it "+
			"gets a broken pipe at its next write.", p.path, moved)
	}
}
